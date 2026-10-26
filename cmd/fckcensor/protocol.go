package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

type protoField struct {
	Number  int
	Wire    int
	Bytes   []byte
	Integer uint64
}

func protoFields(data []byte) ([]protoField, error) {
	var fields []protoField
	for len(data) > 0 {
		key, n := binary.Uvarint(data)
		if n <= 0 || key>>3 == 0 {
			return nil, errors.New("invalid protobuf field")
		}
		data = data[n:]
		f := protoField{Number: int(key >> 3), Wire: int(key & 7)}
		switch f.Wire {
		case 0:
			v, n := binary.Uvarint(data)
			if n <= 0 {
				return nil, errors.New("invalid protobuf integer")
			}
			f.Integer = v
			data = data[n:]
		case 1:
			if len(data) < 8 {
				return nil, io.ErrUnexpectedEOF
			}
			data = data[8:]
		case 5:
			if len(data) < 4 {
				return nil, io.ErrUnexpectedEOF
			}
			data = data[4:]
		case 2:
			size, n := binary.Uvarint(data)
			if n <= 0 || size > uint64(len(data)-n) {
				return nil, io.ErrUnexpectedEOF
			}
			data = data[n:]
			f.Bytes = data[:size]
			data = data[size:]
		default:
			return nil, errors.New("unsupported protobuf wire type")
		}
		fields = append(fields, f)
	}
	return fields, nil
}
func protoBytes(fields []protoField, number int) []byte {
	for _, f := range fields {
		if f.Number == number && f.Wire == 2 {
			return f.Bytes
		}
	}
	return nil
}
func protoInteger(fields []protoField, number int) uint64 {
	for _, f := range fields {
		if f.Number == number && f.Wire == 0 {
			return f.Integer
		}
	}
	return 0
}
func putVarint(value uint64) []byte {
	out := make([]byte, 10)
	n := binary.PutUvarint(out, value)
	return out[:n]
}
func putBytes(number int, value []byte) []byte {
	out := putVarint(uint64(number<<3 | 2))
	out = append(out, putVarint(uint64(len(value)))...)
	return append(out, value...)
}
func putInteger(number int, value uint64) []byte {
	return append(putVarint(uint64(number<<3)), putVarint(value)...)
}
func normalizeMessage(data []byte) ([]protoField, error) {
	f, err := protoFields(data)
	if err != nil {
		return nil, err
	}
	body := protoBytes(f, 252)
	kind := protoInteger(f, 253)
	if body != nil && kind != 0 {
		return []protoField{{Number: int(kind), Wire: 2, Bytes: body}}, nil
	}
	return f, nil
}
func writeFrame(w io.Writer, data []byte) error {
	if len(data) > 10<<20 {
		return errors.New("IPC frame too large")
	}
	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, uint32(len(data)))
	_, err := io.Copy(w, bytes.NewReader(append(header, data...)))
	return err
}
func readFrame(r io.Reader) ([]byte, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}
	size := binary.LittleEndian.Uint32(head[:])
	if size > 10<<20 {
		return nil, errors.New("IPC frame too large")
	}
	data := make([]byte, size)
	_, err := io.ReadFull(r, data)
	return data, err
}
func transcript(data []byte) (string, error) {
	fields, err := normalizeMessage(data)
	if err != nil {
		return "", err
	}
	stateBytes := protoBytes(fields, 3)
	if stateBytes == nil {
		return "", errors.New("not a speech state")
	}
	state, err := protoFields(stateBytes)
	if err != nil {
		return "", err
	}
	requestBytes := protoBytes(state, 15)
	if requestBytes != nil {
		request, err := protoFields(requestBytes)
		if err != nil {
			return "", err
		}
		if protoInteger(request, 2) == 0 {
			return "", errors.New("partial recognition")
		}
	}
	source, _ := protoFields(protoBytes(state, 10))
	if string(protoBytes(source, 6)) == "command" {
		req, _ := protoFields(protoBytes(state, 15))
		if protoInteger(req, 2) != 0 {
			return string(protoBytes(req, 1)), nil
		}
	}
	return string(protoBytes(state, 2)), nil
}
func cancelDialog() error {
	method := putInteger(1, 3)
	remoting := append(putBytes(7, []byte("AliceCapability")), putBytes(8, method)...)
	message := append(putBytes(252, remoting), putInteger(253, 210)...)
	c, err := net.DialTimeout("tcp4", "127.0.0.1:9875", time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetWriteDeadline(time.Now().Add(time.Second))
	return writeFrame(c, message)
}
func nativeTokens(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	unique := map[string]bool{}
	var tokens []string
	var walk func([]byte, int)
	walk = func(data []byte, depth int) {
		if depth > 5 {
			return
		}
		fields, err := protoFields(data)
		if err != nil {
			return
		}
		for _, f := range fields {
			if f.Wire != 2 {
				continue
			}
			if bytes.HasPrefix(f.Bytes, []byte("y0_")) && len(f.Bytes) >= 40 && len(f.Bytes) <= 4096 {
				value := string(f.Bytes)
				if !unique[value] {
					unique[value] = true
					tokens = append(tokens, value)
				}
			} else {
				walk(f.Bytes, depth+1)
			}
		}
	}
	walk(data, 0)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("native account has no supported OAuth credential")
	}
	return tokens, nil
}
