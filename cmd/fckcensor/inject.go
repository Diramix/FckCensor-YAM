package main

import (
	"bytes"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

type armRegisters [18]uint32

func ptrace(request uintptr, pid int, address, data uintptr) error {
	_, _, e := syscall.Syscall6(syscall.SYS_PTRACE, request, uintptr(pid), address, data, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

func registers(pid int, request uintptr, r *armRegisters) error {
	iov := syscall.Iovec{Base: (*byte)(unsafe.Pointer(r)), Len: uint64(unsafe.Sizeof(*r))}
	if err := ptrace(request, pid, 1, uintptr(unsafe.Pointer(&iov))); err != nil {
		return err
	}
	if iov.Len != 72 {
		return fmt.Errorf("expected ARM32 registers, received %d bytes", iov.Len)
	}
	return nil
}

func stopped(pid int, timeout time.Duration) (syscall.WaitStatus, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var status syscall.WaitStatus
		n, err := syscall.Wait4(pid, &status, syscall.WNOHANG|0x40000000, nil)
		if err != nil {
			return status, err
		}
		if n == pid {
			return status, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0, errors.New("remote call timed out")
}

func remoteSymbol(pid int, library, symbol string) (uint32, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return 0, err
	}
	var filePath string
	var base uint64
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || !strings.Contains(f[5], library) || f[2] != "00000000" {
			continue
		}
		base, err = strconv.ParseUint(strings.Split(f[0], "-")[0], 16, 32)
		if err != nil {
			return 0, err
		}
		filePath = f[5]
		break
	}
	if filePath == "" {
		return 0, fmt.Errorf("library %s is not mapped", library)
	}
	file, err := elf.Open(filePath)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	symbols, err := file.DynamicSymbols()
	if err != nil {
		return 0, err
	}
	for _, s := range symbols {
		if s.Name == symbol && s.Section != elf.SHN_UNDEF {
			return uint32(base + s.Value), nil
		}
	}
	return 0, fmt.Errorf("symbol %s missing", symbol)
}

func inject(pid int, path string) (result error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if !strings.HasPrefix(path, "/") || strings.IndexByte(path, 0) >= 0 || len(path) > 1024 {
		return errors.New("invalid library path")
	}
	libraryFile, err := elf.Open(path)
	if err != nil {
		return err
	}
	supported := libraryFile.Class == elf.ELFCLASS32 && libraryFile.Machine == elf.EM_ARM && libraryFile.Type == elf.ET_DYN
	libraryFile.Close()
	if !supported {
		return errors.New("hook must be an ARM32 shared library")
	}
	symbol, err := remoteSymbol(pid, "libdl-", "dlopen")
	if err != nil {
		return err
	}
	if err = ptrace(16, pid, 0, 0); err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	attached := true
	var saved armRegisters
	restored := false
	defer func() {
		if attached {
			if !restored && saved[15] != 0 {
				registers(pid, 0x4205, &saved)
			}
			if err := ptrace(17, pid, 0, 0); result == nil && err != nil {
				result = err
			}
		}
	}()
	status, err := stopped(pid, 3*time.Second)
	if err != nil {
		return err
	}
	if !status.Stopped() {
		return fmt.Errorf("unexpected attach state: %v", status)
	}
	if err = registers(pid, 0x4204, &saved); err != nil {
		return err
	}
	if saved[13] < 0x10000 {
		return errors.New("invalid remote stack")
	}
	mem, err := os.OpenFile(fmt.Sprintf("/proc/%d/mem", pid), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer mem.Close()
	address := int64((saved[13] - 1536) &^ 7)
	original := make([]byte, len(path)+1)
	if _, err = mem.ReadAt(original, address); err != nil {
		return err
	}
	defer mem.WriteAt(original, address)
	if _, err = mem.WriteAt(append([]byte(path), 0), address); err != nil {
		return err
	}
	call := saved
	call[0] = uint32(address)
	call[1] = 2
	call[2] = 0
	call[3] = 0
	call[13] = (saved[13] - 4096) &^ 7
	call[14] = 0
	call[15] = symbol &^ 1
	call[16] &= ^uint32(0x20)
	if symbol&1 != 0 {
		call[16] |= 0x20
	}
	call[17] = 0xffffffff
	if err = registers(pid, 0x4205, &call); err != nil {
		return err
	}
	if err = ptrace(7, pid, 0, 0); err != nil {
		return err
	}
	deadline := time.Now().Add(8 * time.Second)
	var after armRegisters
	for {
		status, err = stopped(pid, time.Until(deadline))
		if err != nil {
			syscall.Kill(pid, syscall.SIGSTOP)
			if _, stopErr := stopped(pid, 3*time.Second); stopErr != nil {
				return fmt.Errorf("%v; could not stop target: %v", err, stopErr)
			}
			return err
		}
		if !status.Stopped() {
			attached = false
			return fmt.Errorf("target exited during load: %v", status)
		}
		if err = registers(pid, 0x4204, &after); err != nil {
			return err
		}
		if status.StopSignal() == syscall.SIGSEGV && after[15] == 0 {
			break
		}
		sig := status.StopSignal()
		switch sig {
		case syscall.SIGCHLD, syscall.SIGWINCH, syscall.SIGURG, syscall.SIGCONT:
			if err = ptrace(7, pid, 0, uintptr(sig)); err != nil {
				return err
			}
		case syscall.SIGSTOP:
			if err = ptrace(7, pid, 0, 0); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected remote signal during load: %v at %x", sig, after[15])
		}
	}
	if err = registers(pid, 0x4205, &saved); err != nil {
		return err
	}
	restored = true
	if status.StopSignal() != syscall.SIGSEGV || after[15] != 0 {
		return fmt.Errorf("unexpected remote return: signal=%v pc=%x", status.StopSignal(), after[15])
	}
	if after[0] == 0 {
		return errors.New("remote dlopen returned null")
	}
	return nil
}

func loadedHookState(maps []byte, installed bool) (bool, error) {
	if bytes.Contains(maps, []byte("libstation_uncensor.so")) {
		return false, errors.New("legacy station-uncensor hook remains in mediad; restart the native player once before starting fckcensor")
	}
	if !bytes.Contains(maps, []byte("libfckcensor.so")) {
		return false, nil
	}
	if !installed {
		return false, errors.New("fckcensor library is already loaded but inactive; restart the native player once; see hook.log")
	}
	return true, nil
}

func hookPresent(pid int) (bool, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return false, err
	}
	_, markerErr := os.Stat(fmt.Sprintf("%s/installed-%d", stateDir, pid))
	return loadedHookState(data, markerErr == nil)
}

func mediaPID() (int, error) {
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	for _, d := range dirs {
		pid, e := strconv.Atoi(d.Name())
		if e != nil {
			continue
		}
		cmd, _ := os.ReadFile("/proc/" + d.Name() + "/cmdline")
		if bytes.Contains(cmd, []byte("\x00--mediad\x00")) {
			return pid, nil
		}
	}
	return 0, errors.New("native mediad process not found")
}

func restartMedia(ctx context.Context) error {
	old, err := mediaPID()
	if err == nil {
		if err = syscall.Kill(old, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("restart native player: %w", err)
		}
		log.Printf("restarting native player %d to discard preloaded original pipelines", old)
	}
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if pid, err := mediaPID(); err == nil && pid != old {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("native player did not restart within 20 seconds")
		case <-ticker.C:
		}
	}
}
