package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func (r *recoveryService) run(ctx context.Context) {
	r.loadCache()
	go func() {
		r.loadOverrides(ctx)
		r.catalog(ctx)
		r.mu.RLock()
		total, err := len(r.tracks), r.lastError
		r.mu.RUnlock()
		if err != "" {
			log.Printf("recovery catalogue: %s", err)
		} else {
			removed := 0
			for _, t := range r.snapshot() {
				if !t.Available {
					removed++
				}
			}
			log.Printf("recovery catalogue ready: %d tracks, %d unavailable", total, removed)
		}
	}()
	go r.voice(ctx)
}
func (r *recoveryService) voice(ctx context.Context) {
	messages := make(chan string, 1)
	go func() {
		for ctx.Err() == nil {
			connection, err := net.DialTimeout("tcp4", "127.0.0.1:9875", 2*time.Second)
			if err == nil {
				done := make(chan struct{})
				go func() {
					select {
					case <-ctx.Done():
						connection.Close()
					case <-done:
					}
				}()
				first := true
				previous := ""
				for {
					data, err := readFrame(connection)
					if err != nil {
						break
					}
					text, err := transcript(data)
					if err != nil {
						continue
					}
					value := normalized(text)
					if first {
						first = false
						previous = value
						continue
					}
					if value == previous {
						continue
					}
					previous = value
					if value == "" {
						continue
					}
					select {
					case messages <- text:
					default:
						select {
						case <-messages:
						default:
						}
						select {
						case messages <- text:
						default:
						}
					}
				}
				close(done)
				connection.Close()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	var pending string
	var cancel context.CancelFunc
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case text := <-messages:
			if cancel != nil {
				cancel()
				cancel = nil
			}
			pending = text
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(500 * time.Millisecond)
		case <-timer.C:
			track, ok := matchDeleted(pending, r.snapshot())
			if !ok {
				continue
			}
			job, stop := context.WithTimeout(ctx, 80*time.Second)
			cancel = stop
			go func(id string) {
				if err := r.play(job, id, true); err != nil {
					log.Printf("recovery track %s: %v", id, err)
				} else {
					log.Printf("recovered track %s through native player", id)
				}
			}(track.ID)
		}
	}
}
func (s *service) route(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/audio/") {
		s.audio(w, r)
		return
	}
	if s.recovery == nil {
		http.Error(w, "Recovery is starting", 503)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/status":
		s.recovery.mu.RLock()
		state := map[string]any{"catalogue_tracks": len(s.recovery.tracks), "loading": s.recovery.loading, "error": s.recovery.lastError}
		s.recovery.mu.RUnlock()
		json.NewEncoder(w).Encode(state)
	case "/api/catalog":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		q := normalized(r.URL.Query().Get("q"))
		tracks := []catalogTrack{}
		for _, track := range s.recovery.snapshot() {
			if track.Available && r.URL.Query().Get("all") != "true" {
				continue
			}
			if q == "" || strings.Contains(normalized(track.Title+" "+track.Artist), q) {
				tracks = append(tracks, track)
				if len(tracks) >= 50 {
					break
				}
			}
		}
		json.NewEncoder(w).Encode(tracks)
	case "/api/play":
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var request struct {
			Track string `json:"track"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&request) != nil {
			http.Error(w, "Invalid request", 400)
			return
		}
		id, err := trackID(request.Track)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err = s.recovery.play(r.Context(), id, false); err != nil {
			log.Printf("recovery track %s: %v", id, err)
			http.Error(w, err.Error(), 502)
			return
		}
		log.Printf("recovered track %s through native player", id)
		json.NewEncoder(w).Encode(map[string]string{"status": "playing", "track": id})
	default:
		http.NotFound(w, r)
	}
}
func recoveryCommand(mode, track, query string) error {
	client := &http.Client{Timeout: 80 * time.Second}
	var response *http.Response
	var err error
	if mode == "play" {
		body, _ := json.Marshal(map[string]string{"track": track})
		response, err = client.Post("http://127.0.0.1:18777/api/play", "application/json", strings.NewReader(string(body)))
	} else {
		path := "/api/status"
		if mode == "search" {
			path = "/api/catalog?q=" + url.QueryEscape(query)
		}
		response, err = client.Get("http://127.0.0.1:18777" + path)
	}
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != 200 {
		return fmt.Errorf("%s", strings.TrimSpace(string(data)))
	}
	_, err = os.Stdout.Write(data)
	return err
}
