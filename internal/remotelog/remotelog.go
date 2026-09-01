package remotelog

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"deskferry/internal/tunnel"
	"nhooyr.io/websocket"
)

const (
	maxQueuedBytes = 1 << 20
	maxQueuedLines = 2000
	maxLineBytes   = 8192
	maxBatchLines  = 100
)

type Target struct {
	RelayAddr    string
	Proxy        string
	RoomPassword string
	RoomProof    string
}

type line struct {
	seq  uint64
	text string
}

// Hub is an io.Writer that retains a bounded process-local backlog and uploads
// acknowledged batches to every configured relay room.
type Hub struct {
	mu        sync.Mutex
	component string
	instance  string
	lines     []line
	bytes     int
	next      uint64
	notify    chan struct{}
	targets   map[string]struct{}
}

func New(component string) *Hub {
	return &Hub{component: cleanHeader(component), next: 1, notify: make(chan struct{}, 1), targets: make(map[string]struct{})}
}

func (h *Hub) SetInstance(instance string) {
	h.mu.Lock()
	h.instance = cleanHeader(instance)
	h.mu.Unlock()
}

func (h *Hub) Write(p []byte) (int, error) {
	original := len(p)
	for _, value := range strings.Split(strings.ReplaceAll(string(p), "\r\n", "\n"), "\n") {
		value = strings.TrimSuffix(value, "\r")
		if value == "" {
			continue
		}
		if len(value) > maxLineBytes {
			value = value[:maxLineBytes]
		}
		h.mu.Lock()
		h.lines = append(h.lines, line{seq: h.next, text: value})
		h.next++
		h.bytes += len(value)
		for len(h.lines) > maxQueuedLines || h.bytes > maxQueuedBytes {
			h.bytes -= len(h.lines[0].text)
			h.lines = h.lines[1:]
		}
		h.mu.Unlock()
	}
	select {
	case h.notify <- struct{}{}:
	default:
	}
	return original, nil
}

func (h *Hub) AddTarget(ctx context.Context, target Target) {
	key := strings.TrimSpace(target.RelayAddr) + "\x00" + target.RoomProof + "\x00" + target.RoomPassword
	if key == "" {
		return
	}
	h.mu.Lock()
	if _, exists := h.targets[key]; exists {
		h.mu.Unlock()
		return
	}
	h.targets[key] = struct{}{}
	next := h.next
	if len(h.lines) > 0 {
		next = h.lines[0].seq
	}
	h.mu.Unlock()
	go h.runTarget(ctx, target, next)
}

// StartTarget starts an independently managed uploader. It is intended for
// clients whose selected relay profile can change at runtime.
func (h *Hub) StartTarget(ctx context.Context, target Target) {
	if strings.TrimSpace(target.RelayAddr) == "" {
		return
	}
	h.mu.Lock()
	next := h.next
	if len(h.lines) > 0 {
		next = h.lines[0].seq
	}
	h.mu.Unlock()
	go h.runTarget(ctx, target, next)
}

func (h *Hub) runTarget(ctx context.Context, target Target, next uint64) {
	backoff := time.Second
	for ctx.Err() == nil {
		headers := http.Header{}
		h.mu.Lock()
		headers.Set(tunnel.HeaderLogComponent, h.component)
		headers.Set(tunnel.HeaderLogInstance, h.instance)
		h.mu.Unlock()
		if target.RoomProof != "" {
			headers.Set(tunnel.HeaderRoomProof, target.RoomProof)
		} else {
			tunnel.AddRoomPasswordHeader(headers, target.RelayAddr, "", target.RoomPassword)
		}
		dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		conn, err := tunnel.DialMessageConnWithHeaders(dialCtx, target.RelayAddr, target.Proxy, tunnel.RoleDiagnosticLog, "", headers)
		cancel()
		if err != nil {
			if !wait(ctx, backoff, h.notify) {
				return
			}
			backoff = grow(backoff)
			continue
		}
		backoff = time.Second
		next = h.upload(ctx, conn, next)
		tunnel.CloseMessageConn(conn)
		if !wait(ctx, backoff, h.notify) {
			return
		}
	}
}

type batch struct {
	Entries []string `json:"entries"`
}
type acknowledgement struct {
	Accepted int `json:"accepted"`
}

func (h *Hub) upload(ctx context.Context, conn tunnel.MessageConn, next uint64) uint64 {
	for ctx.Err() == nil {
		entries, first := h.batch(next)
		if len(entries) == 0 {
			if !wait(ctx, 30*time.Second, h.notify) {
				return next
			}
			continue
		}
		payload, _ := json.Marshal(batch{Entries: entries})
		writeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := conn.Write(writeCtx, websocket.MessageText, payload)
		cancel()
		if err != nil {
			return next
		}
		readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		typ, response, err := conn.Read(readCtx)
		cancel()
		if err != nil || typ != websocket.MessageText {
			return next
		}
		var ack acknowledgement
		if json.Unmarshal(response, &ack) != nil || ack.Accepted != len(entries) {
			return next
		}
		next = first + uint64(ack.Accepted)
	}
	return next
}

func (h *Hub) batch(next uint64) ([]string, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.lines) == 0 {
		return nil, next
	}
	if next < h.lines[0].seq {
		next = h.lines[0].seq
	}
	entries := make([]string, 0, maxBatchLines)
	first := next
	for _, item := range h.lines {
		if item.seq < next {
			continue
		}
		if len(entries) == 0 {
			first = item.seq
		}
		entries = append(entries, item.text)
		if len(entries) == maxBatchLines {
			break
		}
	}
	return entries, first
}

func wait(ctx context.Context, duration time.Duration, notify <-chan struct{}) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-notify:
		return true
	case <-timer.C:
		return true
	}
}
func grow(value time.Duration) time.Duration {
	value *= 2
	if value > 30*time.Second {
		return 30 * time.Second
	}
	return value
}
func cleanHeader(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, strings.TrimSpace(value))
}

var _ io.Writer = (*Hub)(nil)
