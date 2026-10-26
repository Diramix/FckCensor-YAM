package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

type localDevice struct {
	ID       string `json:"id"`
	Platform string `json:"platform"`
	Glagol   struct {
		Security struct {
			Certificate string `json:"server_certificate"`
		} `json:"security"`
	} `json:"glagol"`
}
type stationClient struct {
	mu      sync.Mutex
	http    *http.Client
	device  localDevice
	oauth   string
	token   string
	tokenAt time.Time
}

func newStationClient() *stationClient {
	return &stationClient{http: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(r *http.Request, via []*http.Request) error { return errors.New("credential redirect rejected") }}}
}
func (c *stationClient) authorize(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Since(c.tokenAt) < 10*time.Minute {
		return nil
	}
	b, err := os.ReadFile("/data/quasar/devices.json")
	if err != nil {
		return err
	}
	var devices struct {
		Devices []localDevice `json:"devices"`
	}
	if err = json.Unmarshal(b, &devices); err != nil {
		return err
	}
	if len(devices.Devices) == 0 {
		return errors.New("local station identity missing")
	}
	c.device = devices.Devices[0]
	tokens, err := nativeTokens("/data/quasar/account_storage.dat")
	if err != nil {
		return err
	}
	query := url.Values{"device_id": {c.device.ID}, "platform": {c.device.Platform}}
	for _, token := range tokens {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://quasar.yandex.net/glagol/token?"+query.Encode(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "OAuth "+token)
		response, err := c.http.Do(req)
		if err != nil {
			continue
		}
		var result struct {
			Status string `json:"status"`
			Token  string `json:"token"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result)
		response.Body.Close()
		if response.StatusCode == 200 && decodeErr == nil && result.Status == "ok" && result.Token != "" {
			c.oauth = token
			c.token = result.Token
			c.tokenAt = time.Now()
			return nil
		}
	}
	return errors.New("native station account could not authorize local playback")
}

type socketClient struct {
	connection net.Conn
	reader     *bufio.Reader
}

func (c *stationClient) connect(ctx context.Context) (*socketClient, string, error) {
	if err := c.authorize(ctx); err != nil {
		return nil, "", err
	}
	c.mu.Lock()
	device, token := c.device, c.token
	c.mu.Unlock()
	pinned, _ := pem.Decode([]byte(device.Glagol.Security.Certificate))
	if pinned == nil {
		return nil, "", errors.New("station TLS certificate missing")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(s tls.ConnectionState) error {
		if len(s.PeerCertificates) == 0 || !bytes.Equal(s.PeerCertificates[0].Raw, pinned.Bytes) {
			return errors.New("local station certificate mismatch")
		}
		return nil
	}}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: config}
	conn, err := dialer.DialContext(ctx, "tcp4", "127.0.0.1:1961")
	if err != nil {
		return nil, "", err
	}
	conn.SetDeadline(time.Now().Add(12 * time.Second))
	keyBytes := make([]byte, 16)
	if _, err = rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, "", err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	request := "GET / HTTP/1.1\r\nHost: 127.0.0.1:1961\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\nOrigin: http://yandex.ru\r\n\r\n"
	if _, err = io.WriteString(conn, request); err != nil {
		conn.Close()
		return nil, "", err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		conn.Close()
		return nil, "", err
	}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if response.StatusCode != 101 || response.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		conn.Close()
		return nil, "", errors.New("station WebSocket handshake rejected")
	}
	return &socketClient{conn, reader}, token, nil
}
func (s *socketClient) write(opcode byte, data []byte) error {
	if len(data) > 1<<20 {
		return errors.New("WebSocket message too large")
	}
	header := []byte{0x80 | opcode}
	n := len(data)
	if n < 126 {
		header = append(header, 0x80|byte(n))
	} else if n <= 65535 {
		header = append(header, 0xfe, byte(n>>8), byte(n))
	} else {
		header = append(header, 0xff)
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, uint64(n))
		header = append(header, b...)
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	header = append(header, mask...)
	payload := append([]byte(nil), data...)
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	_, err := io.Copy(s.connection, bytes.NewReader(append(header, payload...)))
	return err
}
func (s *socketClient) read() ([]byte, error) {
	var out []byte
	for {
		var head [2]byte
		if _, err := io.ReadFull(s.reader, head[:]); err != nil {
			return nil, err
		}
		if head[0]&0x70 != 0 || head[1]&0x80 != 0 {
			return nil, errors.New("invalid WebSocket frame")
		}
		length := uint64(head[1] & 127)
		if length == 126 {
			var b [2]byte
			if _, err := io.ReadFull(s.reader, b[:]); err != nil {
				return nil, err
			}
			length = uint64(binary.BigEndian.Uint16(b[:]))
		} else if length == 127 {
			var b [8]byte
			if _, err := io.ReadFull(s.reader, b[:]); err != nil {
				return nil, err
			}
			length = binary.BigEndian.Uint64(b[:])
		}
		if length > 1<<20 || uint64(len(out))+length > 1<<20 {
			return nil, errors.New("WebSocket frame too large")
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(s.reader, body); err != nil {
			return nil, err
		}
		switch head[0] & 15 {
		case 8:
			return nil, io.EOF
		case 9:
			if err := s.write(10, body); err != nil {
				return nil, err
			}
			continue
		case 10:
			continue
		case 0, 1:
			out = append(out, body...)
		default:
			return nil, errors.New("unsupported WebSocket opcode")
		}
		if head[0]&0x80 != 0 {
			return out, nil
		}
	}
}
func (c *stationClient) command(ctx context.Context, payload any) error {
	socket, token, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer socket.connection.Close()
	random := make([]byte, 16)
	if _, err = rand.Read(random); err != nil {
		return err
	}
	id := fmt.Sprintf("%x", random)
	message, err := json.Marshal(map[string]any{"conversationToken": token, "id": id, "payload": payload, "sentTime": time.Now().UnixMilli()})
	if err != nil {
		return err
	}
	if err = socket.write(1, message); err != nil {
		return err
	}
	for i := 0; i < 64; i++ {
		data, err := socket.read()
		if err != nil {
			return err
		}
		var response struct {
			RequestID string `json:"requestId"`
			Status    string `json:"status"`
		}
		if json.Unmarshal(data, &response) == nil && response.RequestID == id {
			if response.Status != "SUCCESS" {
				return fmt.Errorf("station command returned %s", response.Status)
			}
			return nil
		}
	}
	return errors.New("station did not acknowledge command")
}
func (c *stationClient) play(ctx context.Context, track catalogTrack) error {
	payload := map[string]any{"stream": map[string]any{"id": track.ID, "url": "http://127.0.0.1:18777/audio/" + track.ID + ".mp3", "format": "MP3", "type": "Track"}, "metadata": map[string]any{"title": track.Title, "subtitle": track.Artist, "art_image_url": track.Cover}}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	command := append(putBytes(1, []byte("audio_play")), putBytes(2, body)...)
	return c.command(ctx, map[string]any{"command": "externalCommandBypass", "data": base64.StdEncoding.EncodeToString(command)})
}
