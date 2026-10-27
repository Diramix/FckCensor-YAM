package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fckcensor/internal/replacements"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const stateDir = "/tmp/fckcensor"
const maxAudio = 24 << 20
const maxCache = 48 << 20
const refreshInterval = 60 * time.Minute

type pendingDownload struct {
	done chan struct{}
	path string
	err  error
}

type service struct {
	mu           sync.Mutex
	index        map[string]string
	indexMu      sync.RWMutex
	client       *http.Client
	pending      map[string]*pendingDownload
	reserved     int64
	remote       map[string]remoteAudio
	streamClient *http.Client
	recovery     *recoveryService
}

func mediaHeader(data []byte) bool {
	if len(data) < 12 {
		return false
	}
	return string(data[:3]) == "ID3" || data[0] == 0xff && data[1]&0xe0 == 0xe0 || string(data[:4]) == "fLaC" || string(data[:4]) == "OggS" || string(data[4:8]) == "ftyp"
}

func newClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) > 3 || r.URL.Scheme != "https" || r.URL.Host != "raw.githubusercontent.com" {
			return errors.New("redirect rejected")
		}
		return nil
	}}
}

func fetchList(client *http.Client) (map[string]string, []byte, error) {
	return fetchListContext(context.Background(), client)
}

func fetchListContext(ctx context.Context, client *http.Client) (map[string]string, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, replacements.Source, nil)
	if err != nil {
		return nil, nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, nil, fmt.Errorf("replacement list HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > 4<<20 {
		return nil, nil, errors.New("replacement list too large")
	}
	index, err := replacements.Build(data)
	return index, data, err
}

func waitForList(ctx context.Context, client *http.Client, delay time.Duration) (map[string]string, []byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		index, data, err := fetchListContext(ctx, client)
		if err == nil {
			return index, data, nil
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		log.Printf("replacement list unavailable: %v; retrying in %s", err, delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil, ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, 10*time.Second)
	}
}

func fetchIndex(client *http.Client) (map[string]string, error) {
	index, _, err := fetchList(client)
	return index, err
}

func (s *service) source(id string) (string, bool) {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	value, found := s.index[id]
	return value, found
}

func (s *service) ids() []string {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	ids := make([]string, 0, len(s.index))
	for id := range s.index {
		ids = append(ids, id)
	}
	return ids
}

func (s *service) refreshLoop(ctx context.Context, current []byte) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		index, data, err := fetchListContext(ctx, s.client)
		if err != nil {
			log.Printf("replacement list refresh failed: %v", err)
			continue
		}
		if bytes.Equal(data, current) {
			continue
		}
		path := filepath.Join(stateDir, "list_v2.json")
		if os.WriteFile(path+".tmp", data, 0600) == nil {
			os.Rename(path+".tmp", path)
		}
		s.indexMu.Lock()
		s.index = index
		s.indexMu.Unlock()
		current = data
		log.Printf("replacement list updated: %d tracks", len(index))
		if s.recovery != nil {
			s.recovery.refresh(ctx)
		}
	}
}

func (s *service) prepare(id string) (string, error) {
	source, found := s.source(id)
	if !found {
		return "", os.ErrNotExist
	}
	path := filepath.Join(stateDir, "cache", id+".audio")
	s.mu.Lock()
	if _, found := s.remote[id]; found {
		s.mu.Unlock()
		return "stream:" + id, nil
	}
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		now := time.Now()
		os.Chtimes(path, now, now)
		s.mu.Unlock()
		return path, nil
	}
	if flight := s.pending[id]; flight != nil {
		s.mu.Unlock()
		<-flight.done
		return flight.path, flight.err
	}
	if s.pending == nil {
		s.pending = map[string]*pendingDownload{}
	}
	flight := &pendingDownload{done: make(chan struct{})}
	s.pending[id] = flight
	s.mu.Unlock()
	result, err := s.download(source, path)
	s.mu.Lock()
	flight.path = result
	flight.err = err
	delete(s.pending, id)
	close(flight.done)
	s.mu.Unlock()
	if err == nil {
		if strings.HasPrefix(result, "stream:") {
			log.Printf("prepared streaming replacement track %s", id)
		} else {
			log.Printf("cached replacement track %s", id)
		}
	}
	return result, err
}

func (s *service) download(source, path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 28*time.Second)
	defer cancel()
	var response *http.Response
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if requestErr != nil {
			return "", requestErr
		}
		response, err = s.client.Do(request)
		if err == nil && response.StatusCode < 500 {
			break
		}
		if response != nil {
			response.Body.Close()
			response = nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	if err != nil {
		return "", err
	}
	if response == nil {
		return "", errors.New("audio server unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return "", fmt.Errorf("audio HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxAudio {
		return s.rememberRemote(response, source, path)
	}
	needed := int64(maxAudio)
	if response.ContentLength > 0 {
		needed = response.ContentLength
	}
	s.mu.Lock()
	err = s.cacheRoom(s.reserved + needed)
	if err == nil {
		s.reserved += needed
	}
	s.mu.Unlock()
	if err != nil {
		if response.ContentLength > 0 {
			return s.rememberRemote(response, source, path)
		}
		return "", err
	}
	defer func() { s.mu.Lock(); s.reserved -= needed; s.mu.Unlock() }()
	temp, err := os.CreateTemp(filepath.Join(stateDir, "cache"), "pending-")
	if err != nil {
		return "", err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	n, err := io.Copy(temp, io.LimitReader(response.Body, maxAudio+1))
	if err != nil {
		return "", err
	}
	if n > maxAudio {
		return "", errors.New("audio exceeds cache file limit")
	}
	header := make([]byte, 12)
	if _, err = temp.ReadAt(header, 0); err != nil || !mediaHeader(header) {
		return "", errors.New("replacement is not recognized audio")
	}
	if err = temp.Close(); err != nil {
		return "", err
	}
	if err = os.Rename(temp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

func (s *service) cacheRoom(needed int64) error {
	entries, err := os.ReadDir(filepath.Join(stateDir, "cache"))
	if err != nil {
		return err
	}
	type cached struct {
		path string
		size int64
		at   time.Time
	}
	var files []cached
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || strings.HasPrefix(entry.Name(), "pending-") {
			continue
		}
		total += info.Size()
		if strings.HasSuffix(entry.Name(), ".audio") {
			files = append(files, cached{filepath.Join(stateDir, "cache", entry.Name()), info.Size(), info.ModTime()})
		}
	}
	for total+needed > maxCache && len(files) > 0 {
		oldest := 0
		for i := range files {
			if files[i].at.Before(files[oldest].at) {
				oldest = i
			}
		}
		if time.Since(files[oldest].at) < 5*time.Minute {
			break
		}
		if err := os.Remove(files[oldest].path); err != nil {
			return err
		}
		total -= files[oldest].size
		files = append(files[:oldest], files[oldest+1:]...)
	}
	if total+needed > maxCache {
		return errors.New("RAM cache budget exhausted")
	}
	return nil
}

func (s *service) prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, _ := os.ReadDir(filepath.Join(stateDir, "cache"))
	type item struct {
		path     string
		size     int64
		modified time.Time
	}
	var files []item
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || !strings.HasSuffix(entry.Name(), ".audio") {
			continue
		}
		files = append(files, item{filepath.Join(stateDir, "cache", entry.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	for total > maxCache && len(files) > 1 {
		oldest := 0
		for i := range files {
			if files[i].modified.Before(files[oldest].modified) {
				oldest = i
			}
		}
		if time.Since(files[oldest].modified) < 5*time.Minute {
			break
		}
		if os.Remove(files[oldest].path) == nil {
			total -= files[oldest].size
		}
		files = append(files[:oldest], files[oldest+1:]...)
	}
}

func (s *service) control(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(34 * time.Second))
	id, err := bufio.NewReader(io.LimitReader(conn, 64)).ReadString('\n')
	if err != nil {
		return
	}
	id = strings.TrimSpace(id)
	if !replacements.Numeric.MatchString(id) {
		io.WriteString(conn, "MISS\n")
		return
	}
	if _, err = s.prepare(id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("skipped track %s: no audio replacement in database", id)
		} else {
			log.Printf("skipped track %s: %v", id, err)
		}
		io.WriteString(conn, "MISS\n")
		return
	}
	io.WriteString(conn, "OK\n")
}

func (s *service) audio(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/audio/"), ".mp3")
	if r.Method != "GET" && r.Method != "HEAD" {
		w.WriteHeader(405)
		return
	}
	if !replacements.Numeric.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	_, known := s.source(id)
	if !known {
		http.NotFound(w, r)
		return
	}
	if s.remoteContent(w, r, id) {
		return
	}
	file, err := os.Open(filepath.Join(stateDir, "cache", id+".audio"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	now := time.Now()
	os.Chtimes(filepath.Join(stateDir, "cache", id+".audio"), now, now)
	info, err := file.Stat()
	if err != nil {
		w.WriteHeader(500)
		return
	}
	if r.Method == http.MethodGet {
		log.Printf("serving replacement track %s (%d bytes)", id, info.Size())
	}
	http.ServeContent(w, r, id, info.ModTime(), file)
}

func ensureState() error {
	if info, err := os.Lstat(stateDir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("unsafe state directory")
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "cache"), 0700); err != nil {
		return err
	}
	return os.Chmod(stateDir, 0700)
}

func serve(library string, attach bool, resetPlayer bool) error {
	if err := ensureState(); err != nil {
		return err
	}
	if !filepath.IsAbs(library) {
		return errors.New("hook library path must be absolute")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	lock, err := os.OpenFile(filepath.Join(stateDir, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another instance is running")
	}
	client := newClient()
	index, list, err := waitForList(ctx, client, time.Second)
	if err != nil {
		return err
	}
	log.Printf("loaded %d replacement tracks", len(index))
	s := &service{index: index, client: client, pending: map[string]*pendingDownload{}, streamClient: streamingClient()}
	s.recovery = newRecovery(s)
	socketPath := filepath.Join(stateDir, "control.sock")
	os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socketPath)
	tcp, err := net.Listen("tcp4", "127.0.0.1:18777")
	if err != nil {
		return err
	}
	defer tcp.Close()
	server := &http.Server{Handler: http.HandlerFunc(s.route), ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 20 * time.Second}
	go server.Serve(tcp)
	defer server.Close()
	if attach && resetPlayer {
		if err := restartMedia(ctx); err != nil {
			return err
		}
	}
	if err = os.WriteFile(filepath.Join(stateDir, "enabled"), []byte("1\n"), 0600); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(stateDir, "enabled"))
	go func() { <-ctx.Done(); listener.Close(); server.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go s.control(conn)
		}
	}()
	go s.recovery.run(ctx)
	go s.refreshLoop(ctx, list)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	lastFailure := time.Time{}
	for {
		pid, err := mediaPID()
		present := false
		if attach && err == nil {
			var stateErr error
			present, stateErr = hookPresent(pid)
			if stateErr != nil {
				return stateErr
			}
		}
		if attach && err == nil && !present {
			if err = inject(pid, library); err != nil {
				if time.Since(lastFailure) > time.Minute {
					log.Printf("hook injection failed: %v", err)
					lastFailure = time.Now()
				}
			} else {
				if _, err := os.Stat(filepath.Join(stateDir, fmt.Sprintf("installed-%d", pid))); err != nil {
					return errors.New("hook loaded but GStreamer ABI was not accepted; see hook.log")
				}
				log.Printf("hook loaded into mediad %d", pid)
			}
		}
		s.prune()
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func main() {
	attach := flag.Bool("attach", true, "attach the hook to native mediad")
	resetPlayer := flag.Bool("reset-player", true, "restart native player once at service start to discard preloaded original pipelines")
	mode := flag.String("mode", "serve", "serve, inject, or resolve")
	pid := flag.Int("pid", 0, "target mediad process for inject")
	library := flag.String("library", "", "absolute path to ARM32 hook library")
	track := flag.String("track", "", "track ID or Yandex Music link")
	query := flag.String("query", "", "search restored track catalogue")
	flag.Parse()
	var err error
	switch *mode {
	case "play", "search", "status":
		err = recoveryCommand(*mode, *track, *query)
	case "serve":
		err = serve(*library, *attach, *resetPlayer)
	case "inject":
		if *pid == 0 {
			*pid, err = mediaPID()
		}
		if err == nil {
			err = inject(*pid, *library)
		}
	case "prepare":
		var conn net.Conn
		conn, err = net.DialTimeout("unix", filepath.Join(stateDir, "control.sock"), time.Second)
		if err == nil {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(35 * time.Second))
			fmt.Fprintln(conn, *track)
			var response string
			response, err = bufio.NewReader(conn).ReadString('\n')
			if err == nil && response != "OK\n" {
				err = errors.New("replacement unavailable")
			}
		}
	case "resolve":
		var index map[string]string
		index, err = fetchIndex(newClient())
		if err == nil {
			if !replacements.Numeric.MatchString(*track) {
				err = errors.New("invalid track ID")
			} else if value, ok := index[*track]; ok {
				u, _ := url.Parse(value)
				fmt.Println(u.String())
			} else {
				err = os.ErrNotExist
			}
		}
	default:
		err = fmt.Errorf("unknown mode %s", *mode)
	}
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
