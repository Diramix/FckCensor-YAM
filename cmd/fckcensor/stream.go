package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

type remoteAudio struct {
	URL  string
	Size int64
	MIME string
}

func (s *service) rememberRemote(response *http.Response, source, path string) (string, error) {
	header := make([]byte, 12)
	if _, err := io.ReadFull(response.Body, header); err != nil {
		return "", err
	}
	if !mediaHeader(header) {
		return "", errors.New("replacement is not recognized audio")
	}
	mime := "audio/mpeg"
	if string(header[:4]) == "fLaC" {
		mime = "audio/flac"
	} else if string(header[:4]) == "OggS" {
		mime = "audio/ogg"
	} else if string(header[4:8]) == "ftyp" {
		mime = "audio/mp4"
	}
	id := strings.TrimSuffix(filepath.Base(path), ".audio")
	s.mu.Lock()
	if s.remote == nil {
		s.remote = map[string]remoteAudio{}
	}
	s.remote[id] = remoteAudio{source, response.ContentLength, mime}
	s.mu.Unlock()
	return "stream:" + id, nil
}

type remoteReader struct {
	ctx    context.Context
	client *http.Client
	entry  remoteAudio
	pos    int64
	body   io.ReadCloser
}

func (r *remoteReader) Read(p []byte) (int, error) {
	if r.pos >= r.entry.Size {
		return 0, io.EOF
	}
	if r.body == nil {
		req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.entry.URL, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", r.pos))
		response, err := r.client.Do(req)
		if err != nil {
			return 0, err
		}
		if response.StatusCode != 200 && response.StatusCode != 206 {
			response.Body.Close()
			return 0, fmt.Errorf("audio HTTP %d", response.StatusCode)
		}
		if response.StatusCode == 206 && !strings.HasPrefix(response.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", r.pos)) {
			response.Body.Close()
			return 0, errors.New("invalid upstream range")
		}
		if response.StatusCode == 200 && r.pos > 0 {
			if _, err = io.CopyN(io.Discard, response.Body, r.pos); err != nil {
				response.Body.Close()
				return 0, err
			}
		}
		r.body = response.Body
	}
	if int64(len(p)) > r.entry.Size-r.pos {
		p = p[:r.entry.Size-r.pos]
	}
	n, err := r.body.Read(p)
	r.pos += int64(n)
	return n, err
}
func (r *remoteReader) Seek(offset int64, whence int) (int64, error) {
	next := offset
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		next += r.pos
	case io.SeekEnd:
		next += r.entry.Size
	default:
		return r.pos, errors.New("invalid seek")
	}
	if next < 0 {
		return r.pos, errors.New("negative seek")
	}
	if next != r.pos && r.body != nil {
		r.body.Close()
		r.body = nil
	}
	r.pos = next
	return next, nil
}
func (r *remoteReader) Close() error {
	if r.body != nil {
		return r.body.Close()
	}
	return nil
}

func streamingClient() *http.Client {
	c := newClient()
	c.Timeout = 0
	c.Transport = &http.Transport{Proxy: nil, ResponseHeaderTimeout: 15 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConns: 8}
	return c
}

func (s *service) remoteContent(w http.ResponseWriter, r *http.Request, id string) bool {
	s.mu.Lock()
	entry, found := s.remote[id]
	client := s.streamClient
	s.mu.Unlock()
	if !found {
		return false
	}
	if client == nil {
		client = streamingClient()
	}
	reader := &remoteReader{ctx: r.Context(), client: client, entry: entry}
	defer reader.Close()
	w.Header().Set("Content-Type", entry.MIME)
	if r.Method == http.MethodGet {
		log.Printf("serving streamed replacement track %s (%d bytes)", id, entry.Size)
	}
	http.ServeContent(w, r, id, time.Time{}, reader)
	return true
}
