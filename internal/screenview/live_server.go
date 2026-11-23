package screenview

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/png"
	"io"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// LiveSource captures the shared display.
type LiveSource interface {
	// Next waits up to timeout for the display to change and returns the
	// current frame, which stays valid until the next call. changed is false
	// when the frame is identical to the previous call's.
	Next(timeout time.Duration) (frame *image.RGBA, changed bool, err error)
	// Cursor returns the pointer; Shape may be nil when unchanged is fine.
	Cursor() CursorState
	// Method names the capture method for status, for example "dxgi".
	Method() string
	Close()
}

// LiveHost injects input and answers the Work PC's state queries.
type LiveHost interface {
	Inject(events []InputEvent) error
	ReleaseAll()
	// LastHumanInput is the time of the last input that DeskFerry did not
	// inject, or zero when none was seen.
	LastHumanInput() time.Time
	ClipboardGet() (string, error)
	ClipboardSet(text string) error
	Desktop() (name string, locked bool, foreground string)
	UITree(command Command) ([]UINode, error)
	UIFind(command Command) ([]UINode, error)
	SAS() error
}

// SessionState is shown on the Work PC's sharing banner.
type SessionState struct {
	ClientName    string
	ClientKind    string
	ControlActive bool
	Live          bool
	Ended         bool
}

// LiveServerOptions configures one live session on the Work side.
type LiveServerOptions struct {
	ScreenKey      string
	ControlAllowed bool
	Version        string
	Source         LiveSource
	Host           LiveHost
	// OnState is called whenever the banner should change.
	OnState func(SessionState)
	// Control receives banner requests: false stops control for the session.
	Control <-chan bool
	// VideoEncoder opens an H.264 encoder; nil disables video.
	VideoEncoder VideoEncoderFactory
	Logf         func(format string, args ...any)
}

const (
	defaultMaxFPS     = 60
	refineDelay       = 250 * time.Millisecond
	humanYieldWindow  = 1500 * time.Millisecond
	livePingInterval  = 10 * time.Second
	liveIdleTimeout   = 60 * time.Second
	minWindowBytes    = 192 << 10
	maxWindowBytes    = 32 << 20
	maxFramesInFlight = 16
	// pipelinedFrames is how many typical frames may always be in flight.
	pipelinedFrames = 3
	// maxQueuedFrames lets one frame be encoded while another is written.
	maxQueuedFrames      = 2
	statusPushInterval   = 500 * time.Millisecond
	refineBudgetFraction = 2
)

type inflightFrame struct {
	seq    uint64
	bytes  int
	sentAt time.Time
	lossy  int
}

type settleWaiter struct {
	id       uint64
	quiet    time.Duration
	deadline time.Time
	changed  []image.Rectangle
}

type liveServer struct {
	opts    LiveServerOptions
	conn    *recordConn
	request Request
	logf    func(string, ...any)

	controlActive atomic.Bool
	yieldToHuman  atomic.Bool
	lastReceived  atomic.Int64

	commands chan Command
	inputs   chan Command
	outbox   *recordQueue

	// frames carries encoded frames, and replies that must follow them, to
	// the sender goroutine, so capturing and encoding the next frame overlap
	// with sending this one.
	frames chan frameJob
	perf   framePerf
	video  *videoStream
	// videoFailures counts encoder failures in this session.
	videoFailures int
	// ackedBytes counts acknowledged frame bytes; throughput is the
	// delivered rate (bytes per second) measured while changes were waiting.
	ackedBytes atomic.Int64
	throughput float64
	tpStart    time.Time
	queued     atomic.Int32
	sendErr    atomic.Value
	frameBytes float64 // moving average of sent frame sizes

	mu        sync.Mutex
	inflight  []inflightFrame
	minRTT    time.Duration
	srtt      time.Duration
	bandwidth float64 // bytes per second
	acked     chan struct{}

	rate         *fpsController
	fixedQuality int
	maxFPS       int
}

// ServeLive runs a live session after the request has been read. rw carries
// the rest of the session.
func ServeLive(ctx context.Context, rw io.ReadWriter, request Request, opts LiveServerOptions) error {
	logf := opts.Logf
	if logf == nil {
		logf = log.Printf
	}
	writeHelloError := func(err error) error {
		_ = WriteFrame(rw, Frame{Type: FrameError, Error: err.Error()}, nil)
		return err
	}
	clientHello, err := base64.RawURLEncoding.DecodeString(request.ClientHello)
	if err != nil {
		return writeHelloError(commandError("bad_request", "invalid client hello"))
	}
	serverHello, keys, expectedFinish, err := serverHandshake(opts.ScreenKey, clientHello)
	if err != nil {
		return writeHelloError(commandError("failed", "%v", err))
	}
	frame, _, err := opts.Source.Next(0)
	if err != nil {
		return writeHelloError(err)
	}
	bounds := frame.Bounds()
	hello := Hello{
		ServerHello:    base64.RawURLEncoding.EncodeToString(serverHello),
		Version:        opts.Version,
		Width:          bounds.Dx(),
		Height:         bounds.Dy(),
		ControlAllowed: opts.ControlAllowed,
		Capabilities:   liveCapabilities(opts.VideoEncoder != nil),
	}
	if err := WriteFrame(rw, Frame{Type: FrameHello, Live: &hello}, nil); err != nil {
		return err
	}
	conn, err := newRecordConn(rw, rw, keys.s2c, keys.c2s)
	if err != nil {
		return err
	}
	recordType, payload, err := conn.ReadRecord()
	if err != nil {
		return fmt.Errorf("read live screen finish: %w", err)
	}
	if recordType != recFinish || !bytes.Equal(payload, expectedFinish) {
		return ErrRoomPasswordMismatch
	}
	server := &liveServer{
		opts:     opts,
		conn:     conn,
		request:  request,
		logf:     logf,
		commands: make(chan Command, 64),
		inputs:   make(chan Command, 256),
		acked:    make(chan struct{}, 1),
		rate:     newFPSController(request.Features, opts.VideoEncoder != nil, logf),
		maxFPS:   defaultMaxFPS,
	}
	if request.MinFPS != nil {
		server.rate.setTarget(*request.MinFPS)
	}
	server.outbox = newRecordQueue(conn)
	server.frames = make(chan frameJob, 8)
	defer server.outbox.stop()
	server.lastReceived.Store(time.Now().UnixNano())
	server.notifyState(false)
	defer server.notifyState(true)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	readErr := make(chan error, 1)
	go func() { readErr <- server.readLoop(ctx) }()
	go server.inputLoop(ctx)
	go server.frameSender(ctx)
	loopErr := make(chan error, 1)
	go func() { loopErr <- server.frameLoop(ctx, frame) }()
	select {
	case err = <-readErr:
	case err = <-loopErr:
	case <-ctx.Done():
		err = ctx.Err()
	}
	cancel()
	server.stopVideo()
	opts.Host.ReleaseAll()
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (s *liveServer) notifyState(ended bool) {
	if s.opts.OnState == nil {
		return
	}
	s.opts.OnState(SessionState{
		ClientName:    s.request.Name,
		ClientKind:    s.request.Client,
		ControlActive: s.controlActive.Load(),
		Live:          true,
		Ended:         ended,
	})
}

func (s *liveServer) readLoop(ctx context.Context) error {
	for {
		recordType, payload, err := s.conn.ReadRecord()
		if err != nil {
			return err
		}
		s.lastReceived.Store(time.Now().UnixNano())
		switch recordType {
		case recAck:
			if len(payload) == 8 {
				s.handleAck(decodeUint64(payload))
			}
		case recPing:
			s.outbox.send(recPong, payload)
		case recPong:
		case recJSON:
			var command Command
			if err := json.Unmarshal(payload, &command); err != nil {
				s.reply(0, nil, commandError("bad_request", "invalid command JSON"))
				continue
			}
			s.dispatch(ctx, command)
		}
	}
}

func decodeUint64(payload []byte) uint64 {
	var value uint64
	for _, b := range payload[:8] {
		value = value<<8 | uint64(b)
	}
	return value
}

func (s *liveServer) dispatch(ctx context.Context, command Command) {
	switch command.Cmd {
	case "input":
		select {
		case s.inputs <- command:
		case <-ctx.Done():
		}
	case "control":
		if command.Enable && !s.opts.ControlAllowed {
			s.reply(command.ID, nil, commandError("control_not_allowed", "remote control is turned off in this Work PC's DeskFerry Work Services settings"))
			return
		}
		s.yieldToHuman.Store(command.YieldToHuman)
		if !command.Enable {
			select {
			case s.inputs <- Command{Cmd: "input", Events: []InputEvent{{T: "release_all"}}}:
			default:
				s.opts.Host.ReleaseAll()
			}
		}
		s.controlActive.Store(command.Enable)
		s.notifyState(false)
		s.logf("screen control enabled=%t client=%s name=%q yield_to_human=%t", command.Enable, s.request.Client, s.request.Name, command.YieldToHuman)
		s.reply(command.ID, map[string]bool{"control_active": command.Enable}, nil)
		s.pushStatusSoon()
	case "refresh", "settle", "quality", "status":
		select {
		case s.commands <- command:
		case <-ctx.Done():
		}
	case "clipboard_get":
		go func() {
			text, err := s.opts.Host.ClipboardGet()
			s.reply(command.ID, map[string]string{"text": text}, err)
		}()
	case "clipboard_set":
		if !s.controlActive.Load() {
			s.reply(command.ID, nil, commandError("control_not_enabled", "enable control before setting the clipboard"))
			return
		}
		go func() { s.reply(command.ID, struct{}{}, s.opts.Host.ClipboardSet(command.Text)) }()
	case "ui_tree", "ui_find":
		go func() {
			var nodes []UINode
			var err error
			if command.Cmd == "ui_tree" {
				nodes, err = s.opts.Host.UITree(command)
			} else {
				nodes, err = s.opts.Host.UIFind(command)
			}
			if nodes == nil {
				nodes = []UINode{}
			}
			s.reply(command.ID, map[string][]UINode{"nodes": nodes}, err)
		}()
	case "sas":
		if !s.controlActive.Load() {
			s.reply(command.ID, nil, commandError("control_not_enabled", "enable control before sending Ctrl+Alt+Del"))
			return
		}
		go func() { s.reply(command.ID, struct{}{}, s.opts.Host.SAS()) }()
	default:
		s.reply(command.ID, nil, commandError("unsupported", "unknown command %q", command.Cmd))
	}
}

func (s *liveServer) inputLoop(ctx context.Context) {
	for {
		var command Command
		select {
		case <-ctx.Done():
			return
		case command = <-s.inputs:
		}
		releaseOnly := len(command.Events) == 1 && command.Events[0].T == "release_all"
		if !releaseOnly {
			if !s.opts.ControlAllowed {
				s.reply(command.ID, nil, commandError("control_not_allowed", "remote control is turned off on this Work PC"))
				continue
			}
			if !s.controlActive.Load() {
				s.reply(command.ID, nil, commandError("control_not_enabled", "enable control before sending input"))
				continue
			}
			if s.yieldToHuman.Load() && s.humanActive() {
				s.reply(command.ID, map[string]bool{"human_active": true}, commandError("human_active", "someone is using this Work PC's mouse or keyboard; input paused"))
				continue
			}
		}
		if releaseOnly {
			s.opts.Host.ReleaseAll()
			s.reply(command.ID, map[string]bool{"human_active": s.humanActive()}, nil)
			continue
		}
		err := s.opts.Host.Inject(command.Events)
		s.reply(command.ID, map[string]bool{"human_active": s.humanActive()}, err)
	}
}

func (s *liveServer) humanActive() bool {
	last := s.opts.Host.LastHumanInput()
	return !last.IsZero() && time.Since(last) < humanYieldWindow
}

func (s *liveServer) reply(id uint64, result any, err error) {
	if id == 0 && err == nil {
		return
	}
	message := Message{Type: "reply", ID: id, OK: err == nil}
	if err != nil {
		var commandErr *CommandError
		if !errors.As(err, &commandErr) {
			err = &CommandError{Code: "failed", Message: err.Error()}
		}
		message.Error = err.Error()
	}
	if result != nil {
		if data, marshalErr := json.Marshal(result); marshalErr == nil {
			message.Result = data
		}
	}
	s.sendJSON(message)
}

func (s *liveServer) sendJSON(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	s.outbox.send(recJSON, data)
}

func (s *liveServer) pushStatusSoon() {
	select {
	case s.commands <- Command{Cmd: "status-push"}:
	default:
	}
}

func (s *liveServer) handleAck(seq uint64) {
	now := time.Now()
	s.mu.Lock()
	keep := s.inflight[:0]
	for _, frame := range s.inflight {
		if frame.seq > seq {
			keep = append(keep, frame)
			continue
		}
		s.rate.delivered()
		s.ackedBytes.Add(int64(frame.bytes))
		if frame.seq != seq {
			continue
		}
		sample := now.Sub(frame.sentAt)
		if s.minRTT == 0 || sample < s.minRTT {
			s.minRTT = sample
		}
		if s.srtt == 0 {
			s.srtt = sample
		} else {
			s.srtt = (s.srtt*7 + sample) / 8
		}
		// The time beyond the minimum round trip is roughly the frame's
		// transfer time; only large frames give a usable rate.
		if frame.bytes >= 16<<10 {
			transfer := sample - s.minRTT*9/10
			if transfer < time.Millisecond {
				transfer = time.Millisecond
			}
			rate := float64(frame.bytes) / transfer.Seconds()
			if s.bandwidth == 0 {
				s.bandwidth = rate
			} else if rate > s.bandwidth {
				s.bandwidth = s.bandwidth*0.5 + rate*0.5
			} else {
				s.bandwidth = s.bandwidth*0.85 + rate*0.15
			}
		}
	}
	s.inflight = keep
	s.mu.Unlock()
	select {
	case s.acked <- struct{}{}:
	default:
	}
}

// window returns the unacknowledged-byte limit and the bytes in flight.
func (s *liveServer) window() (int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inflight := 0
	for _, frame := range s.inflight {
		inflight += frame.bytes
	}
	bandwidth := s.bandwidth
	if bandwidth == 0 {
		bandwidth = 2 << 20
	}
	rtt := s.minRTT
	if rtt < 20*time.Millisecond {
		rtt = 20 * time.Millisecond
	}
	limit := int(bandwidth * rtt.Seconds() * 2)
	// Always allow a few typical frames in flight. With a single frame in
	// flight every frame waits a full round trip plus the viewer's decode,
	// and that wait also makes the link look slower than it is.
	limit = max(limit, int(s.frameBytes*pipelinedFrames))
	limit = max(minWindowBytes, min(maxWindowBytes, limit))
	return limit, inflight, len(s.inflight)
}

func (s *liveServer) bandwidthEstimate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bandwidth == 0 {
		return 2 << 20
	}
	return s.bandwidth
}

type cursorTracker struct {
	last      CursorState
	sentShape uint32
	haveLast  bool
}

func cursorShapePNG(shape *image.RGBA) []byte {
	if shape == nil {
		return nil
	}
	var output bytes.Buffer
	if err := png.Encode(&output, shape); err != nil {
		return nil
	}
	return output.Bytes()
}

// CursorShapeID hashes a cursor image so unchanged shapes are not resent.
func CursorShapeID(shape *image.RGBA, hotX, hotY int) uint32 {
	if shape == nil {
		return 0
	}
	hash := fnv.New32a()
	hash.Write(shape.Pix)
	hash.Write([]byte{byte(hotX), byte(hotY), byte(shape.Bounds().Dx()), byte(shape.Bounds().Dy())})
	value := hash.Sum32()
	if value == 0 {
		value = 1
	}
	return value
}

func (s *liveServer) frameLoop(ctx context.Context, first *image.RGBA) error {
	bounds := first.Bounds()
	reference := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	copy(reference.Pix, first.Pix)
	grid := newTileGrid(bounds.Dx(), bounds.Dy())
	grid.markAll()
	keyframe := true
	var seq uint64
	var lastSend time.Time
	lastChange := time.Now()
	var waiters []settleWaiter
	cursor := cursorTracker{}
	lastStatus := ""
	lastStatusAt := time.Time{}
	lastPing := time.Now()
	lastIteration := time.Now()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Process queued commands without blocking.
	drain:
		for {
			select {
			case command := <-s.commands:
				switch command.Cmd {
				case "refresh":
					if command.Keyframe {
						grid.markAll()
						keyframe = true
					} else {
						for index, lossy := range grid.lossy {
							if lossy {
								grid.dirty[index] = true
								grid.lossy[index] = false
							}
						}
					}
					s.reply(command.ID, struct{}{}, nil)
				case "settle":
					quiet := time.Duration(command.QuietMS) * time.Millisecond
					if quiet <= 0 {
						quiet = 400 * time.Millisecond
					}
					timeout := time.Duration(command.TimeoutMS) * time.Millisecond
					if timeout <= 0 {
						timeout = 5 * time.Second
					}
					waiters = append(waiters, settleWaiter{id: command.ID, quiet: quiet, deadline: time.Now().Add(timeout)})
				case "quality":
					if command.MaxFPS > 0 {
						s.maxFPS = max(1, min(120, command.MaxFPS))
					}
					if command.MinFPS != nil {
						s.rate.setTarget(*command.MinFPS)
						lastStatusAt = time.Time{}
					}
					if command.JPEGQuality == 0 {
						s.fixedQuality = 0
					} else {
						s.fixedQuality = max(20, min(95, command.JPEGQuality))
					}
					s.reply(command.ID, struct{}{}, nil)
				case "status":
					status := s.status(grid)
					s.reply(command.ID, status, nil)
				case "status-push":
					lastStatusAt = time.Time{}
				}
			default:
				break drain
			}
		}

		pending := grid.anyDirty()
		lossyPending := grid.anyLossy()
		timeout := 50 * time.Millisecond
		if pending || len(waiters) > 0 || lossyPending {
			timeout = time.Second / time.Duration(max(1, s.maxFPS))
		}
		captureStarted := time.Now()
		frame, changed, err := s.opts.Source.Next(timeout)
		if err != nil {
			return err
		}
		captureTime := time.Since(captureStarted)
		diffStarted := time.Now()
		if fb := frame.Bounds(); fb.Dx() != grid.width || fb.Dy() != grid.height {
			reference = image.NewRGBA(image.Rect(0, 0, fb.Dx(), fb.Dy()))
			copy(reference.Pix, frame.Pix)
			grid = newTileGrid(fb.Dx(), fb.Dy())
			grid.markAll()
			keyframe = true
			changed = true
		} else if changed {
			if grid.diffInto(reference, frame, time.Now()) == 0 {
				changed = false
			}
		} else {
			grid.markCalm(time.Now())
		}
		now := time.Now()
		if changed {
			s.perf.add(&s.perf.capture, captureTime)
			s.perf.add(&s.perf.diff, now.Sub(diffStarted))
		}
		s.rate.totalTiles = len(grid.lastChange)
		s.rate.observe(now, grid.anyDirty(), changed, now.Sub(lastIteration), func() int {
			_, count := grid.motionBounds(now)
			return count
		})
		s.measureThroughput(now, grid.anyDirty())
		lastIteration = now
		if changed {
			lastChange = now
			if len(waiters) > 0 {
				rects := grid.dirtyRects()
				for index := range waiters {
					waiters[index].changed = append(waiters[index].changed, rects...)
				}
			}
		}

		s.sendCursor(&cursor)

		limit, inflight, frames := s.window()
		minInterval := time.Second / time.Duration(max(1, s.maxFPS))
		if err := s.senderError(); err != nil {
			return err
		}
		canSend := inflight < limit && frames < maxFramesInFlight && now.Sub(lastSend) >= minInterval && s.queued.Load() < maxQueuedFrames
		if canSend && (grid.anyDirty() || keyframe) {
			level := s.rate.current()
			mode := encodeMode{Quality: level.Quality, MotionLossy: level.MotionLossy, MotionScale: level.MotionScale, Now: now, Lossless: s.rate.lossless()}
			if s.fixedQuality > 0 {
				mode.Quality = s.fixedQuality
			}
			encodeStarted := time.Now()
			if keyframe {
				s.stopVideo()
			}
			var videoRects []EncodedRect
			if level.Video && !mode.Lossless {
				videoRects = s.encodeVideo(grid, reference, now)
			} else {
				s.stopVideo()
			}
			rects, _, err := grid.encodeTiles(reference, func(index int) bool { return grid.dirty[index] }, mode)
			if err != nil {
				return err
			}
			rects = append(videoRects, rects...)
			s.perf.add(&s.perf.encode, time.Since(encodeStarted))
			seq++
			flags := byte(0)
			if !grid.anyLossy() {
				flags |= frameEndLossless
			}
			if err := s.sendFrame(seq, grid, rects, keyframe, flags); err != nil {
				return err
			}
			keyframe = false
			lastSend = now
		} else if canSend && grid.anyLossy() && now.Sub(lastChange) >= refineDelay {
			budget := max(64<<10, limit/refineBudgetFraction)
			selected := make(map[int]bool)
			estimated := 0
			for index, lossy := range grid.lossy {
				if !lossy {
					continue
				}
				selected[index] = true
				estimated += 12 << 10
				if estimated >= budget {
					break
				}
			}
			rects, _, err := grid.encodeTiles(reference, func(index int) bool { return selected[index] }, encodeMode{Lossless: true})
			if err != nil {
				return err
			}
			seq++
			flags := byte(0)
			if !grid.anyLossy() {
				flags |= frameEndLossless
			}
			if err := s.sendFrame(seq, grid, rects, false, flags); err != nil {
				return err
			}
			lastSend = now
		}

		// Answer settle waiters once every change is sent losslessly and the
		// screen has been quiet long enough.
		if len(waiters) > 0 {
			kept := waiters[:0]
			quietFor := now.Sub(lastChange)
			clean := !grid.anyDirty() && !grid.anyLossy()
			for _, waiter := range waiters {
				settled := clean && quietFor >= waiter.quiet
				if settled || now.After(waiter.deadline) {
					s.replyAfterFrames(waiter.id, SettleResult{Seq: seq, Settled: settled, Changed: mergeRects(waiter.changed)})
					continue
				}
				kept = append(kept, waiter)
			}
			waiters = kept
		}

		if now.Sub(lastStatusAt) >= statusPushInterval {
			lastStatusAt = now
			status := s.status(grid)
			key, _ := json.Marshal(struct {
				Status
				HumanActive bool
			}{Status: statusWithoutVolatile(status), HumanActive: s.humanActive()})
			if string(key) != lastStatus {
				lastStatus = string(key)
				s.sendJSON(Message{Type: "status", Status: &status})
			}
		}
		if now.Sub(lastPing) >= livePingInterval {
			lastPing = now
			_ = s.conn.WriteRecord(recPing, encodeSeq(uint64(now.UnixNano())))
		}
		if since := time.Since(time.Unix(0, s.lastReceived.Load())); since > liveIdleTimeout {
			return fmt.Errorf("live screen client silent for %s", since.Round(time.Second))
		}
		s.perf.sample(!canSend && (grid.anyDirty() || keyframe), inflight >= limit || frames >= maxFramesInFlight)
		if !canSend && (grid.anyDirty() || keyframe) {
			// Wait briefly for an ack instead of spinning on capture.
			select {
			case <-s.acked:
			case <-time.After(minInterval):
			case <-ctx.Done():
			}
		}
		select {
		case enable := <-s.opts.Control:
			if !enable && s.controlActive.Load() {
				s.controlActive.Store(false)
				s.opts.Host.ReleaseAll()
				s.notifyState(false)
				lastStatusAt = time.Time{}
				s.logf("screen control stopped from the Work PC banner")
			}
		default:
		}
	}
}

func statusWithoutVolatile(status Status) Status {
	status.HumanInputMSAgo = 0
	status.Cursor = [2]int{}
	return status
}

func (s *liveServer) status(grid *tileGrid) Status {
	desktop, locked, foreground := s.opts.Host.Desktop()
	cursor := s.opts.Source.Cursor()
	human := int64(-1)
	if last := s.opts.Host.LastHumanInput(); !last.IsZero() {
		human = time.Since(last).Milliseconds()
	}
	return Status{
		Width:           grid.width,
		Height:          grid.height,
		ControlAllowed:  s.opts.ControlAllowed,
		ControlActive:   s.controlActive.Load(),
		YieldToHuman:    s.yieldToHuman.Load(),
		HumanInputMSAgo: human,
		Desktop:         desktop,
		Locked:          locked,
		ForegroundTitle: foreground,
		Cursor:          [2]int{cursor.X, cursor.Y},
		Capture:         s.opts.Source.Method(),
		Compression:     s.rate.describe(s.rate.level),
		Perf:            s.perf.String(s.rate.lastFPS),
	}
}

func (s *liveServer) sendCursor(tracker *cursorTracker) {
	cursor := s.opts.Source.Cursor()
	if tracker.haveLast && cursor.X == tracker.last.X && cursor.Y == tracker.last.Y &&
		cursor.Visible == tracker.last.Visible && cursor.ShapeID == tracker.last.ShapeID {
		return
	}
	var shape []byte
	if cursor.ShapeID != 0 && cursor.ShapeID != tracker.sentShape && cursor.Shape != nil {
		shape = cursorShapePNG(cursor.Shape)
		if shape != nil {
			tracker.sentShape = cursor.ShapeID
		}
	}
	if err := s.conn.WriteRecord(recCursor, encodeCursor(cursor, shape)); err == nil {
		tracker.last = cursor
		tracker.haveLast = true
	}
}

// frameJob is an encoded frame, or a JSON reply that must follow the frames
// queued before it.
type frameJob struct {
	seq           uint64
	width, height int
	rects         []EncodedRect
	keyframe      bool
	endFlags      byte
	message       []byte
}

// sendFrame queues an encoded frame for the sender goroutine.
func (s *liveServer) sendFrame(seq uint64, grid *tileGrid, rects []EncodedRect, keyframe bool, endFlags byte) error {
	if err := s.senderError(); err != nil {
		return err
	}
	s.queued.Add(1)
	s.frames <- frameJob{seq: seq, width: grid.width, height: grid.height, rects: rects, keyframe: keyframe, endFlags: endFlags}
	return nil
}

// replyAfterFrames sends a reply after every frame queued so far.
func (s *liveServer) replyAfterFrames(id uint64, result any) {
	message := Message{Type: "reply", ID: id, OK: true}
	if data, err := json.Marshal(result); err == nil {
		message.Result = data
	}
	data, err := json.Marshal(message)
	if err != nil {
		return
	}
	s.frames <- frameJob{message: data}
}

func (s *liveServer) senderError() error {
	if err, ok := s.sendErr.Load().(error); ok {
		return err
	}
	return nil
}

func (s *liveServer) frameSender(ctx context.Context) {
	for {
		var job frameJob
		select {
		case <-ctx.Done():
			return
		case job = <-s.frames:
		}
		if s.senderError() != nil {
			if job.message == nil {
				s.queued.Add(-1)
			}
			continue
		}
		if job.message != nil {
			if err := s.conn.WriteRecord(recJSON, job.message); err != nil {
				s.sendErr.Store(err)
			}
			continue
		}
		size, err := s.writeFrame(job)
		if err != nil {
			s.sendErr.Store(err)
			s.queued.Add(-1)
			continue
		}
		s.mu.Lock()
		s.inflight = append(s.inflight, inflightFrame{seq: job.seq, bytes: size, sentAt: time.Now()})
		if s.frameBytes == 0 {
			s.frameBytes = float64(size)
		} else {
			s.frameBytes = s.frameBytes*0.8 + float64(size)*0.2
		}
		s.mu.Unlock()
		s.queued.Add(-1)
	}
}

func (s *liveServer) writeFrame(job frameJob) (int, error) {
	startFlags := byte(0)
	if job.keyframe {
		startFlags |= frameFlagKey
	}
	before := s.conn.BytesWritten()
	if err := s.conn.WriteRecord(recFrameStart, encodeFrameStart(job.seq, job.width, job.height, startFlags)); err != nil {
		return 0, err
	}
	for _, record := range packRects(job.rects) {
		if err := s.conn.WriteRecord(recRects, record); err != nil {
			return 0, err
		}
	}
	if err := s.conn.WriteRecord(recFrameEnd, encodeFrameEnd(job.seq, job.endFlags)); err != nil {
		return 0, err
	}
	return int(s.conn.BytesWritten() - before), nil
}

// framePerf keeps moving averages of the frame loop's stages.
type framePerf struct {
	mu                   sync.Mutex
	capture, diff        float64
	encode, video        float64
	videoBytes           float64
	samples, held, acked float64
}

func (p *framePerf) add(field *float64, value time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ms := float64(value) / float64(time.Millisecond)
	if *field == 0 {
		*field = ms
	} else {
		*field = *field*0.9 + ms*0.1
	}
}

// sample records whether changes were held back, and whether by acks.
func (p *framePerf) sample(held, byAcks bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.samples = p.samples*0.98 + 1
	value, acked := 0.0, 0.0
	if held {
		value = 1
		if byAcks {
			acked = 1
		}
	}
	p.held = p.held*0.98 + value
	p.acked = p.acked*0.98 + acked
}

// heldFraction is how often changes recently waited to be sent.
func (p *framePerf) heldFraction() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.samples == 0 {
		return 0
	}
	return p.held / p.samples
}

func (p *framePerf) String(fps float64) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.samples == 0 {
		return ""
	}
	video := ""
	if p.video > 0 {
		video = fmt.Sprintf(", video encode %.0f ms for %.0f KiB", p.video, p.videoBytes/1024)
	}
	return fmt.Sprintf("%.0f fps acked; capture %.0f ms, diff %.0f ms, encode %.0f ms%s; changes held %.0f%% of loops (%.0f%% waiting for acks); %d cores",
		fps, p.capture, p.diff, p.encode, video, 100*p.held/p.samples, 100*p.acked/p.samples, runtime.NumCPU())
}

func hasFeature(features []string, name string) bool {
	for _, feature := range features {
		if feature == name {
			return true
		}
	}
	return false
}

func mergeRects(rects []image.Rectangle) [][4]int {
	if len(rects) == 0 {
		return nil
	}
	merged := make([]image.Rectangle, 0, len(rects))
	for _, rect := range rects {
		joined := false
		for index, existing := range merged {
			if existing.Overlaps(rect) || adjacent(existing, rect) {
				merged[index] = existing.Union(rect)
				joined = true
				break
			}
		}
		if !joined {
			merged = append(merged, rect)
		}
	}
	if len(merged) > 32 {
		union := merged[0]
		for _, rect := range merged[1:] {
			union = union.Union(rect)
		}
		merged = []image.Rectangle{union}
	}
	out := make([][4]int, 0, len(merged))
	for _, rect := range merged {
		out = append(out, [4]int{rect.Min.X, rect.Min.Y, rect.Dx(), rect.Dy()})
	}
	return out
}

func adjacent(a, b image.Rectangle) bool {
	return a.Inset(-1).Overlaps(b)
}
