package main

import (
	"context"
	"encoding/json"
	"errors"
	"fckcensor/internal/replacements"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
)

type catalogTrack struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Artist    string `json:"artist"`
	Cover     string `json:"cover,omitempty"`
	Available bool   `json:"available"`
}
type musicTrack struct {
	ID        json.Number `json:"id"`
	Title     string      `json:"title"`
	Available bool        `json:"available"`
	Cover     string      `json:"coverUri"`
	Artists   []struct {
		Name string `json:"name"`
	} `json:"artists"`
}
type recoveryService struct {
	mu        sync.RWMutex
	playerMu  sync.Mutex
	service   *service
	station   *stationClient
	tracks    map[string]catalogTrack
	overrides map[string]catalogTrack
	loading   bool
	lastError string
}

func newRecovery(s *service) *recoveryService {
	return &recoveryService{service: s, station: newStationClient(), tracks: map[string]catalogTrack{}, loading: true}
}

func (c *stationClient) metadata(ctx context.Context, ids []string) ([]catalogTrack, error) {
	if err := c.authorize(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	token := c.oauth
	c.mu.Unlock()
	form := url.Values{"track-ids": {strings.Join(ids, ",")}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.music.yandex.net/tracks", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "OAuth "+token)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("music metadata HTTP %d", response.StatusCode)
	}
	var reply struct {
		Result []musicTrack `json:"result"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&reply); err != nil {
		return nil, err
	}
	result := make([]catalogTrack, 0, len(reply.Result))
	for _, track := range reply.Result {
		var artists []string
		for _, artist := range track.Artists {
			artists = append(artists, artist.Name)
		}
		cover := strings.ReplaceAll(track.Cover, "%%", "400x400")
		if cover != "" && !strings.HasPrefix(cover, "https://") {
			cover = "https://" + cover
		}
		result = append(result, catalogTrack{track.ID.String(), track.Title, strings.Join(artists, ", "), cover, track.Available})
	}
	return result, nil
}

func normalized(value string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(value) {
		if r == 'ё' {
			r = 'е'
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	text := strings.TrimSpace(b.String())
	numbers := map[string]string{"ноль": "0", "один": "1", "два": "2", "три": "3", "четыре": "4", "пять": "5", "шесть": "6", "семь": "7", "восемь": "8", "девять": "9", "десять": "10", "одиннадцать": "11", "двенадцать": "12", "тринадцать": "13", "четырнадцать": "14", "пятнадцать": "15", "шестнадцать": "16", "семнадцать": "17", "восемнадцать": "18", "девятнадцать": "19", "двадцать": "20"}
	words := strings.Fields(text)
	for i, word := range words {
		if digit, ok := numbers[word]; ok {
			words[i] = digit
		}
	}
	return strings.Join(words, " ")
}

var fillerWords = map[string]bool{"песню": true, "песня": true, "трек": true, "композицию": true, "от": true, "группы": true, "исполнителя": true, "пожалуйста": true, "в": true, "на": true, "станции": true}

func matchDeleted(query string, tracks []catalogTrack) (catalogTrack, bool) {
	q := normalized(query)
	q = strings.TrimSpace(strings.TrimPrefix(q, "алиса "))
	playing := false
	for _, prefix := range []string{"включи ", "поставь ", "сыграй "} {
		if strings.HasPrefix(q, prefix) {
			q = strings.TrimPrefix(q, prefix)
			playing = true
			break
		}
	}
	if !playing {
		return catalogTrack{}, false
	}
	best := 0
	var match catalogTrack
	ambiguous := false
	for _, track := range tracks {
		if track.Available {
			continue
		}
		title := normalized(track.Title)
		artist := normalized(track.Artist)
		if title == "" || !strings.Contains(" "+q+" ", " "+title+" ") {
			continue
		}
		wrongVersion := false
		for _, modifier := range []string{"ремикс", "remix", "slowed", "speed", "live", "instrumental", "караоке"} {
			if strings.Contains(" "+q+" ", " "+modifier+" ") && !strings.Contains(" "+title+" ", " "+modifier+" ") {
				wrongVersion = true
				break
			}
		}
		if wrongVersion {
			continue
		}
		artistMatch := artist != "" && strings.Contains(" "+q+" ", " "+artist+" ")
		rest := strings.Replace(" "+q+" ", " "+title+" ", " ", 1)
		if artistMatch {
			rest = strings.Replace(rest, " "+artist+" ", " ", 1)
		}
		extra := false
		for _, word := range strings.Fields(rest) {
			if !fillerWords[word] {
				extra = true
				break
			}
		}
		if extra {
			continue
		}
		words := len(strings.Fields(title))
		if words < 2 && !artistMatch {
			continue
		}
		score := words * 100
		if artistMatch {
			score += 50
		}
		if score > best {
			best = score
			match = track
			ambiguous = false
		} else if score == best && track.ID != match.ID {
			ambiguous = true
		}
	}
	return match, best > 0 && !ambiguous
}
func (r *recoveryService) snapshot() []catalogTrack {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]catalogTrack, 0, len(r.tracks))
	for _, track := range r.tracks {
		out = append(out, track)
	}
	return out
}
func (r *recoveryService) loadCache() {
	data, err := os.ReadFile(filepath.Join(stateDir, "catalog.json"))
	if err != nil {
		return
	}
	var tracks []catalogTrack
	if json.Unmarshal(data, &tracks) != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, track := range tracks {
		if _, ok := r.service.source(track.ID); ok {
			r.tracks[track.ID] = track
		}
	}
}
func (r *recoveryService) saveCache() {
	data, err := json.Marshal(r.snapshot())
	if err != nil {
		return
	}
	path := filepath.Join(stateDir, "catalog.json")
	if os.WriteFile(path+".tmp", data, 0600) == nil {
		os.Rename(path+".tmp", path)
	}
}
func (r *recoveryService) catalog(ctx context.Context) {
	ids := r.service.ids()
	sort.Strings(ids)
	var lastErr error
	for start := 0; start < len(ids); start += 100 {
		if ctx.Err() != nil {
			return
		}
		end := start + 100
		if end > len(ids) {
			end = len(ids)
		}
		tracks, err := r.station.metadata(ctx, ids[start:end])
		if err != nil {
			lastErr = err
			break
		}
		r.mu.Lock()
		for _, track := range tracks {
			track = r.applyOverride(track)
			r.tracks[track.ID] = track
		}
		r.mu.Unlock()
	}
	r.mu.Lock()
	r.loading = false
	if lastErr != nil {
		r.lastError = lastErr.Error()
	} else {
		r.lastError = ""
	}
	r.mu.Unlock()
	r.saveCache()
}
func trackID(value string) (string, error) {
	if replacementsNumeric(value) {
		return value, nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host != "music.yandex.ru" && u.Host != "music.yandex.com" {
		return "", errors.New("expected a numeric track ID or Yandex Music link")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "track" && replacementsNumeric(parts[i+1]) {
			return parts[i+1], nil
		}
	}
	return "", errors.New("link has no track ID")
}
func replacementsNumeric(value string) bool {
	if value == "" || len(value) > 20 {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
func (r *recoveryService) lookup(ctx context.Context, id string) (catalogTrack, error) {
	if _, ok := r.service.source(id); !ok {
		return catalogTrack{}, errors.New("no replacement audio in FckCensor database")
	}
	r.mu.RLock()
	track, found := r.tracks[id]
	r.mu.RUnlock()
	if found {
		return track, nil
	}
	tracks, err := r.station.metadata(ctx, []string{id})
	if err != nil {
		return track, err
	}
	for _, track := range tracks {
		if track.ID == id {
			r.mu.Lock()
			track = r.applyOverride(track)
			r.tracks[id] = track
			r.mu.Unlock()
			return track, nil
		}
	}
	return track, errors.New("track metadata unavailable")
}
func (r *recoveryService) play(ctx context.Context, id string, voice bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	track, err := r.lookup(ctx, id)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if voice {
		cancelDialog()
	}
	if _, err = r.service.prepare(id); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.playerMu.Lock()
	defer r.playerMu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	cancelDialog()
	return r.station.play(ctx, track)
}

func (r *recoveryService) applyOverride(track catalogTrack) catalogTrack {
	value := r.overrides[track.ID]
	if value.Title != "" {
		track.Title = value.Title
	}
	if value.Artist != "" {
		track.Artist = value.Artist
	}
	if value.Cover != "" {
		track.Cover = value.Cover
	}
	return track
}
func (r *recoveryService) loadOverrides(ctx context.Context) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, replacements.Source, nil)
	if err != nil {
		return
	}
	response, err := r.service.client.Do(request)
	if err != nil {
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return
	}
	var doc struct {
		Sources []struct {
			Tracks map[string]struct {
				Title   string `json:"title"`
				Cover   string `json:"coverUri"`
				Artists []struct {
					Name string `json:"name"`
				} `json:"artists"`
			} `json:"tracks"`
		} `json:"sources"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&doc) != nil {
		return
	}
	values := map[string]catalogTrack{}
	for _, source := range doc.Sources {
		for id, value := range source.Tracks {
			if _, known := r.service.source(id); !known {
				continue
			}
			entry := values[id]
			entry.ID = id
			if entry.Title == "" {
				entry.Title = value.Title
			}
			if entry.Cover == "" && value.Cover != "" {
				entry.Cover = strings.ReplaceAll(value.Cover, "%%", "400x400")
				if !strings.HasPrefix(entry.Cover, "https://") {
					entry.Cover = "https://" + entry.Cover
				}
			}
			if entry.Artist == "" {
				var names []string
				for _, a := range value.Artists {
					if a.Name != "" {
						names = append(names, a.Name)
					}
				}
				entry.Artist = strings.Join(names, ", ")
			}
			values[id] = entry
		}
	}
	r.mu.Lock()
	r.overrides = values
	for id, value := range values {
		if track, found := r.tracks[id]; found {
			r.tracks[id] = r.applyOverride(track)
		} else if value.Title != "" {
			r.tracks[id] = value
		}
	}
	r.mu.Unlock()
}

func (r *recoveryService) refresh(ctx context.Context) {
	r.loadOverrides(ctx)
	r.catalog(ctx)
}
