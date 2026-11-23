//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"os"
	"sync"
	"time"

	"nhooyr.io/websocket"

	"deskferry/internal/screenview"
	"deskferry/internal/tunnel"
)

// The macOS viewer page talks to this backend over one local WebSocket; see
// "macOS Home viewer socket" in docs/screen-live-protocol.md.

const (
	pageFrameStart  = 0x01
	pageRect        = 0x02
	pageFrameEnd    = 0x03
	maxPageUnacked  = 3
	pageSendBacklog = 4096
)

type pageMessage struct {
	binary bool
	data   []byte
}

type macScreenSocket struct {
	controller *macUIController
	ws         *websocket.Conn
	send       chan pageMessage
	done       chan struct{}

	mu        sync.Mutex
	client    *screenview.LiveClient
	cancel    context.CancelFunc
	phase     string
	status    string
	unacked   int
	dropping  bool
	dropFrame bool
	needKey   bool
	sentShape map[uint32]bool
	frames    int
	lastBytes uint64
	lastStats time.Time
	stats     map[string]float64
}

func (c *macUIController) serveScreenSocket(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(8 << 20)
	socket := &macScreenSocket{
		controller: c,
		ws:         ws,
		send:       make(chan pageMessage, pageSendBacklog),
		done:       make(chan struct{}),
		phase:      "idle",
		status:     "Ready",
		sentShape:  make(map[uint32]bool),
		stats:      map[string]float64{},
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go socket.writeLoop(ctx)
	go socket.statsLoop(ctx)
	socket.pushState()
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			break
		}
		var message struct {
			Type   string                  `json:"type"`
			Seq    uint64                  `json:"seq"`
			Enable bool                    `json:"enable"`
			Events []screenview.InputEvent `json:"events"`
			Text   string                  `json:"text"`
		}
		if json.Unmarshal(data, &message) != nil {
			continue
		}
		socket.handle(ctx, message.Type, message.Seq, message.Enable, message.Events, message.Text)
	}
	socket.stop("stopped", "Screen stopped")
	close(socket.done)
	_ = ws.Close(websocket.StatusNormalClosure, "")
}

func (s *macScreenSocket) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case message := <-s.send:
			kind := websocket.MessageText
			if message.binary {
				kind = websocket.MessageBinary
			}
			writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := s.ws.Write(writeCtx, kind, message.data)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (s *macScreenSocket) statsLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.mu.Lock()
			client := s.client
			if client == nil {
				s.mu.Unlock()
				continue
			}
			elapsed := now.Sub(s.lastStats).Seconds()
			received := client.BytesReceived()
			if elapsed > 0 && !s.lastStats.IsZero() {
				s.stats = map[string]float64{
					"fps":  float64(s.frames) / elapsed,
					"kbps": float64(received-s.lastBytes) * 8 / 1000 / elapsed,
				}
			}
			s.frames, s.lastBytes, s.lastStats = 0, received, now
			s.mu.Unlock()
			s.pushState()
		}
	}
}

func (s *macScreenSocket) queue(message pageMessage) {
	select {
	case s.send <- message:
	default:
		// The page fell far behind; resynchronize it with a keyframe.
		s.mu.Lock()
		s.dropping = true
		s.needKey = true
		s.mu.Unlock()
	}
}

func (s *macScreenSocket) queueJSON(value any) {
	data, err := json.Marshal(value)
	if err == nil {
		s.queue(pageMessage{data: data})
	}
}

func (s *macScreenSocket) pushState() {
	s.mu.Lock()
	state := map[string]any{"type": "state", "phase": s.phase, "status": s.status, "stats": s.stats}
	client := s.client
	s.mu.Unlock()
	if client != nil {
		status := client.Status()
		state["control_allowed"] = status.ControlAllowed && !client.Legacy()
		state["control_active"] = status.ControlActive
		state["width"], state["height"] = status.Width, status.Height
		state["desktop"], state["locked"], state["foreground_title"] = status.Desktop, status.Locked, status.ForegroundTitle
	}
	s.queueJSON(state)
}

func (s *macScreenSocket) setPhase(phase, status string) {
	s.mu.Lock()
	s.phase, s.status = phase, status
	s.mu.Unlock()
	s.pushState()
}

func (s *macScreenSocket) fail(err error) {
	s.queueJSON(map[string]string{"type": "error", "error": err.Error()})
}

func (s *macScreenSocket) currentClient() *screenview.LiveClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (s *macScreenSocket) handle(ctx context.Context, kind string, seq uint64, enable bool, events []screenview.InputEvent, text string) {
	client := s.currentClient()
	call := func(command screenview.Command, after func(json.RawMessage)) {
		if client == nil {
			s.fail(errors.New("the screen is not connected"))
			return
		}
		go func() {
			callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			raw, err := client.Call(callCtx, command)
			if err != nil {
				s.fail(err)
				return
			}
			if after != nil {
				after(raw)
			}
		}()
	}
	switch kind {
	case "start":
		s.start(ctx)
	case "stop":
		s.stop("stopped", "Screen stopped")
	case "ack":
		s.ack()
	case "control":
		if client == nil {
			s.fail(errors.New("the screen is not connected"))
			return
		}
		go func() {
			callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := client.SetControl(callCtx, enable, false); err != nil {
				s.fail(err)
			}
			s.pushState()
		}()
	case "input":
		if client != nil && len(events) > 0 {
			if err := client.SendInput(events...); err != nil {
				s.fail(err)
			}
		}
	case "sas":
		call(screenview.Command{Cmd: "sas"}, nil)
	case "refresh":
		call(screenview.Command{Cmd: "refresh", Keyframe: true}, nil)
	case "clipboard_get":
		call(screenview.Command{Cmd: "clipboard_get"}, func(raw json.RawMessage) {
			var result struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(raw, &result)
			s.queueJSON(map[string]string{"type": "clipboard", "text": result.Text})
		})
	case "clipboard_set":
		call(screenview.Command{Cmd: "clipboard_set", Text: text}, nil)
	}
}

func (s *macScreenSocket) start(parent context.Context) {
	s.stop("connecting", "Connecting to the Work screen service")
	s.controller.mu.Lock()
	cfg := s.controller.cfg
	s.controller.mu.Unlock()
	if roomProof(cfg, cfg.primaryRelayAddress()) == "" {
		s.setPhase("error", "Save a room password for this profile before viewing its screen")
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	s.cancel = cancel
	s.unacked, s.dropping, s.needKey = 0, false, false
	s.sentShape = make(map[uint32]bool)
	s.mu.Unlock()
	go func() {
		conn, relayAddr, err := dialRelayService(ctx, cfg, tunnel.ServiceScreen)
		if err != nil {
			if ctx.Err() == nil {
				s.setPhase("error", "Screen connection failed: "+err.Error())
			}
			return
		}
		s.setPhase("connecting", "Connected through "+relayAddr+"; waiting for the Work PC")
		hostname, _ := os.Hostname()
		client, err := screenview.StartLive(ctx, conn, screenview.LiveClientOptions{
			ScreenKey: cfg.screenKey(),
			Client:    "viewer",
			Name:      hostname,
			OnEncoded: s.forward,
			OnStatus:  func(screenview.Status) { s.pushState() },
			OnCursor:  s.forwardCursor,
		})
		if err != nil {
			_ = conn.Close()
			if ctx.Err() == nil {
				s.setPhase("error", "Screen session failed: "+err.Error())
			}
			return
		}
		s.mu.Lock()
		if ctx.Err() != nil {
			s.mu.Unlock()
			_ = client.Close()
			return
		}
		s.client = client
		s.needKey = true
		s.mu.Unlock()
		s.controller.setScreenClient(client)
		switch {
		case client.Legacy():
			s.setPhase("legacy", "Read-only: the Work PC runs an older DeskFerry")
		case cfg.screenKey() == "":
			s.setPhase("legacy", "Read-only: save the room password again to enable live view and control")
		case client.Hello().ControlAllowed:
			s.setPhase("live", "Live through "+relayAddr)
		default:
			s.setPhase("live", "Live, view only: remote control is turned off on the Work PC")
		}
		s.resync()
		select {
		case <-client.Done():
			if ctx.Err() == nil {
				reason := "the connection closed"
				if err := client.Err(); err != nil {
					reason = err.Error()
				}
				s.setPhase("error", "Screen session ended: "+reason)
			}
		case <-ctx.Done():
		}
		_ = client.Close()
	}()
}

func (s *macScreenSocket) stop(phase, status string) {
	s.mu.Lock()
	cancel, client := s.cancel, s.client
	s.cancel, s.client = nil, nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if client != nil {
		_ = client.Close()
		s.controller.setScreenClient(nil)
	}
	s.setPhase(phase, status)
}

// forward relays encoded frames to the page unless it is behind.
func (s *macScreenSocket) forward(event screenview.EncodedEvent) {
	s.mu.Lock()
	if event.Kind == screenview.EncodedFrameStart {
		s.frames++
		if s.unacked >= maxPageUnacked {
			s.dropping = true
		}
		// A frame is forwarded whole or not at all.
		s.dropFrame = s.dropping
	}
	dropping := s.dropFrame
	if !dropping && event.Kind == screenview.EncodedFrameEnd {
		s.unacked++
	}
	s.mu.Unlock()
	if dropping {
		return
	}
	for _, message := range encodePageEvent(event) {
		s.queue(message)
	}
}

func encodePageEvent(event screenview.EncodedEvent) []pageMessage {
	switch event.Kind {
	case screenview.EncodedFrameStart:
		data := make([]byte, 14)
		data[0] = pageFrameStart
		binary.BigEndian.PutUint32(data[1:], uint32(event.Seq))
		binary.BigEndian.PutUint32(data[5:], uint32(event.Width))
		binary.BigEndian.PutUint32(data[9:], uint32(event.Height))
		if event.Keyframe {
			data[13] = 1
		}
		return []pageMessage{{binary: true, data: data}}
	case screenview.EncodedRects:
		messages := make([]pageMessage, 0, len(event.Rects))
		for _, rect := range event.Rects {
			data := make([]byte, 10+len(rect.Data))
			data[0] = pageRect
			binary.BigEndian.PutUint16(data[1:], uint16(rect.X))
			binary.BigEndian.PutUint16(data[3:], uint16(rect.Y))
			binary.BigEndian.PutUint16(data[5:], uint16(rect.W))
			binary.BigEndian.PutUint16(data[7:], uint16(rect.H))
			data[9] = rect.Enc
			copy(data[10:], rect.Data)
			messages = append(messages, pageMessage{binary: true, data: data})
		}
		return messages
	case screenview.EncodedFrameEnd:
		data := make([]byte, 6)
		data[0] = pageFrameEnd
		binary.BigEndian.PutUint32(data[1:], uint32(event.Seq))
		if event.Lossless {
			data[5] = 1
		}
		return []pageMessage{{binary: true, data: data}}
	}
	return nil
}

func (s *macScreenSocket) ack() {
	s.mu.Lock()
	if s.unacked > 0 {
		s.unacked--
	}
	resync := s.unacked == 0 && (s.dropping || s.needKey)
	s.mu.Unlock()
	if resync {
		s.resync()
	}
}

// resync sends the whole framebuffer as a lossless keyframe and resumes
// forwarding. Frames forwarded later apply on top of it.
func (s *macScreenSocket) resync() {
	client := s.currentClient()
	if client == nil {
		return
	}
	frame, seq, lossless := client.Snapshot()
	if frame == nil {
		return
	}
	rects, err := screenview.EncodeKeyframe(frame)
	if err != nil {
		s.fail(err)
		return
	}
	s.mu.Lock()
	s.dropping, s.needKey = false, false
	s.unacked++
	s.mu.Unlock()
	bounds := frame.Bounds()
	for _, event := range []screenview.EncodedEvent{
		{Kind: screenview.EncodedFrameStart, Seq: seq, Width: bounds.Dx(), Height: bounds.Dy(), Keyframe: true},
		{Kind: screenview.EncodedRects, Seq: seq, Rects: rects},
		{Kind: screenview.EncodedFrameEnd, Seq: seq, Lossless: lossless},
	} {
		for _, message := range encodePageEvent(event) {
			s.queue(message)
		}
	}
}

func (s *macScreenSocket) forwardCursor(cursor screenview.CursorState) {
	message := map[string]any{"type": "cursor", "x": cursor.X, "y": cursor.Y, "visible": cursor.Visible, "hot_x": cursor.HotX, "hot_y": cursor.HotY, "shape_id": cursor.ShapeID}
	s.mu.Lock()
	sendShape := cursor.Shape != nil && cursor.ShapeID != 0 && !s.sentShape[cursor.ShapeID]
	if sendShape {
		s.sentShape[cursor.ShapeID] = true
	}
	s.mu.Unlock()
	if sendShape {
		var output bytes.Buffer
		if err := png.Encode(&output, cursor.Shape); err == nil {
			message["shape"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(output.Bytes())
		}
	}
	s.queueJSON(message)
}

func (c *macUIController) setScreenClient(client *screenview.LiveClient) {
	c.mu.Lock()
	c.screenClient = client
	c.mu.Unlock()
}

func (c *macUIController) serveScreenPNG(w http.ResponseWriter) {
	c.mu.Lock()
	client := c.screenClient
	c.mu.Unlock()
	if client == nil {
		http.Error(w, "no screenshot is available", http.StatusNotFound)
		return
	}
	frame, _, _ := client.Snapshot()
	if frame == nil {
		http.Error(w, "no screenshot is available", http.StatusNotFound)
		return
	}
	var output bytes.Buffer
	if err := png.Encode(&output, frame); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=DeskFerry-%s.png", time.Now().Format("20060102-150405")))
	_, _ = w.Write(output.Bytes())
}
