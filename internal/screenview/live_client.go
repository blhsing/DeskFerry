package screenview

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// FrameUpdate describes one presented frame.
type FrameUpdate struct {
	Seq      uint64
	Width    int
	Height   int
	Dirty    []image.Rectangle
	Keyframe bool
	Lossless bool
	Settled  bool
	Bytes    int
	Legacy   bool
}

// LiveClientOptions configures a Home-side screen session.
type LiveClientOptions struct {
	// ScreenKey enables the live session; without it the client asks for the
	// legacy read-only stream.
	ScreenKey string
	// Client is "viewer" or "agent"; Name is shown on the Work PC's banner.
	Client string
	Name   string
	// LegacyIntervalMS is the legacy stream interval when live mode is not
	// available. Zero uses 1000 ms.
	LegacyIntervalMS int
	OnFrame          func(FrameUpdate)
	OnStatus         func(Status)
	OnCursor         func(CursorState)
	// OnEncoded receives every frame as it arrives, still encoded, before it
	// is drawn. Legacy frames arrive as PNG rects. Rect data must not be
	// modified.
	OnEncoded func(EncodedEvent)
}

// Encoded event kinds.
const (
	EncodedFrameStart = 1
	EncodedRects      = 2
	EncodedFrameEnd   = 3
)

// EncodedEvent is one part of a frame as the Work agent sent it.
type EncodedEvent struct {
	Kind     int
	Seq      uint64
	Width    int
	Height   int
	Keyframe bool
	Lossless bool
	Rects    []EncodedRect
}

// EncodeKeyframe encodes a whole framebuffer losslessly as rects, for clients
// that need to resynchronize a viewer.
func EncodeKeyframe(img *image.RGBA) ([]EncodedRect, error) {
	bounds := img.Bounds()
	if bounds.Min != (image.Point{}) {
		img = toRGBA(img)
	}
	grid := newTileGrid(bounds.Dx(), bounds.Dy())
	grid.markAll()
	rects, _, err := grid.encodeTiles(img, func(int) bool { return true }, encodeMode{Lossless: true})
	return rects, err
}

// LiveClient is a running Home-side screen session.
type LiveClient struct {
	opts   LiveClientOptions
	rw     io.ReadWriteCloser
	conn   *recordConn
	legacy bool
	hello  Hello

	fbMu     sync.RWMutex
	fb       *image.RGBA
	seq      uint64
	lossless bool

	stateMu sync.Mutex
	status  Status
	cursor  CursorState
	shapes  map[uint32]*image.RGBA

	nextID  atomic.Uint64
	pending sync.Map // id -> chan Message

	done    chan struct{}
	err     error
	errOnce sync.Once

	bytesIn atomic.Uint64
}

// StartLive sends the screen request on rw and starts the session. It returns
// after the Work agent answered; the session then runs until Close or until
// the connection ends.
func StartLive(ctx context.Context, rw io.ReadWriteCloser, opts LiveClientOptions) (*LiveClient, error) {
	client := &LiveClient{opts: opts, rw: rw, done: make(chan struct{}), shapes: make(map[uint32]*image.RGBA)}
	stop := context.AfterFunc(ctx, func() { _ = rw.Close() })
	defer stop()
	interval := opts.LegacyIntervalMS
	if interval == 0 {
		interval = DefaultIntervalMS
	}
	request := Request{Mode: ModeStream, IntervalMS: interval, TileSize: DefaultTileSize}
	var handshake *clientHandshake
	if opts.ScreenKey != "" {
		var err error
		handshake, err = newClientHandshake(opts.ScreenKey)
		if err != nil {
			return nil, err
		}
		request.Protocol = ProtocolLive
		request.Client = opts.Client
		request.Name = opts.Name
		request.ClientHello = base64.RawURLEncoding.EncodeToString(handshake.hello)
		request.Features = []string{FeatureScaledJPEG}
		if request.IntervalMS < MinIntervalMS {
			request.IntervalMS = MinIntervalMS
		}
	}
	if err := WriteRequest(rw, request); err != nil {
		return nil, fmt.Errorf("send screen request: %w", err)
	}
	frame, payloads, err := ReadFrame(rw)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	switch frame.Type {
	case FrameHello:
		if handshake == nil || frame.Live == nil {
			return nil, errors.New("unexpected live screen hello")
		}
		serverHello, err := base64.RawURLEncoding.DecodeString(frame.Live.ServerHello)
		if err != nil {
			return nil, errors.New("invalid live screen hello")
		}
		keys, finish, err := handshake.finish(serverHello)
		if err != nil {
			return nil, err
		}
		client.hello = *frame.Live
		client.conn, err = newRecordConn(rw, rw, keys.c2s, keys.s2c)
		if err != nil {
			return nil, err
		}
		if err := client.conn.WriteRecord(recFinish, finish); err != nil {
			return nil, err
		}
		client.status = Status{Width: frame.Live.Width, Height: frame.Live.Height, ControlAllowed: frame.Live.ControlAllowed}
		go client.liveLoop()
		go client.pingLoop()
	case FrameError:
		return nil, parseReplyError(frame.Error)
	default:
		client.legacy = true
		if err := client.applyLegacy(frame, payloads); err != nil {
			return nil, err
		}
		go client.legacyLoop()
	}
	return client, nil
}

// Legacy reports that the Work agent only offers the read-only legacy stream.
func (c *LiveClient) Legacy() bool { return c.legacy }

// Hello returns the Work agent's live hello.
func (c *LiveClient) Hello() Hello { return c.hello }

// Done is closed when the session ends.
func (c *LiveClient) Done() <-chan struct{} { return c.done }

// Err returns why the session ended.
func (c *LiveClient) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

// Close ends the session.
func (c *LiveClient) Close() error {
	c.finish(errors.New("screen session closed"))
	return nil
}

// BytesReceived counts the secure-record and legacy bytes received.
func (c *LiveClient) BytesReceived() uint64 { return c.bytesIn.Load() }

func (c *LiveClient) finish(err error) {
	c.errOnce.Do(func() {
		c.err = err
		_ = c.rw.Close()
		close(c.done)
		c.pending.Range(func(key, value any) bool {
			select {
			case value.(chan Message) <- Message{Type: "reply", Error: "failed: " + err.Error()}:
			default:
			}
			return true
		})
	})
}

// WithFramebuffer calls fn with the current framebuffer under a read lock.
// fn must not keep the image.
func (c *LiveClient) WithFramebuffer(fn func(fb *image.RGBA, seq uint64, lossless bool)) {
	c.fbMu.RLock()
	defer c.fbMu.RUnlock()
	fn(c.fb, c.seq, c.lossless)
}

// Snapshot returns a copy of the framebuffer.
func (c *LiveClient) Snapshot() (*image.RGBA, uint64, bool) {
	c.fbMu.RLock()
	defer c.fbMu.RUnlock()
	if c.fb == nil {
		return nil, c.seq, false
	}
	out := image.NewRGBA(c.fb.Bounds())
	copy(out.Pix, c.fb.Pix)
	return out, c.seq, c.lossless
}

// Status returns the last status the Work agent pushed.
func (c *LiveClient) Status() Status {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.status
}

// Cursor returns the Work PC's pointer.
func (c *LiveClient) Cursor() CursorState {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.cursor
}

// Call sends a command and waits for its reply.
func (c *LiveClient) Call(ctx context.Context, command Command) (json.RawMessage, error) {
	if c.legacy {
		return nil, commandError("unsupported", "the Work agent only offers the read-only legacy screen stream; upgrade DeskFerry on the Work PC")
	}
	command.ID = c.nextID.Add(1)
	reply := make(chan Message, 1)
	c.pending.Store(command.ID, reply)
	defer c.pending.Delete(command.ID)
	if err := c.send(command); err != nil {
		return nil, err
	}
	select {
	case message := <-reply:
		if !message.OK {
			return message.Result, parseReplyError(message.Error)
		}
		return message.Result, nil
	case <-ctx.Done():
		return nil, commandError("timeout", "%v", ctx.Err())
	case <-c.done:
		return nil, commandError("failed", "%v", c.err)
	}
}

// Send sends a command without waiting for a reply.
func (c *LiveClient) Send(command Command) error {
	if c.legacy {
		return commandError("unsupported", "read-only legacy screen stream")
	}
	command.ID = 0
	return c.send(command)
}

func (c *LiveClient) send(command Command) error {
	data, err := json.Marshal(command)
	if err != nil {
		return err
	}
	return c.conn.WriteRecord(recJSON, data)
}

// Input injects events and waits until the Work PC has applied them.
func (c *LiveClient) Input(ctx context.Context, events ...InputEvent) error {
	_, err := c.Call(ctx, Command{Cmd: "input", Events: events})
	return err
}

// SendInput injects events without waiting, for interactive viewers.
func (c *LiveClient) SendInput(events ...InputEvent) error {
	return c.Send(Command{Cmd: "input", Events: events})
}

// SetControl turns control on or off for this session.
func (c *LiveClient) SetControl(ctx context.Context, enable, yieldToHuman bool) error {
	_, err := c.Call(ctx, Command{Cmd: "control", Enable: enable, YieldToHuman: yieldToHuman})
	if err == nil {
		c.stateMu.Lock()
		c.status.ControlActive = enable
		c.stateMu.Unlock()
	}
	return err
}

// Settle waits until the screen has been quiet for quiet and every change has
// arrived losslessly, or until timeout.
func (c *LiveClient) Settle(ctx context.Context, quiet, timeout time.Duration) (SettleResult, error) {
	var result SettleResult
	callCtx, cancel := context.WithTimeout(ctx, timeout+10*time.Second)
	defer cancel()
	raw, err := c.Call(callCtx, Command{Cmd: "settle", QuietMS: int(quiet / time.Millisecond), TimeoutMS: int(timeout / time.Millisecond)})
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(raw, &result)
	return result, err
}

func (c *LiveClient) pingLoop() {
	ticker := time.NewTicker(livePingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case now := <-ticker.C:
			if err := c.conn.WriteRecord(recPing, encodeSeq(uint64(now.UnixNano()))); err != nil {
				c.finish(err)
				return
			}
		}
	}
}

func (c *LiveClient) liveLoop() {
	var current FrameUpdate
	inFrame := false
	for {
		recordType, payload, err := c.conn.ReadRecord()
		if err != nil {
			c.finish(err)
			return
		}
		c.bytesIn.Add(uint64(len(payload) + 21))
		switch recordType {
		case recFrameStart:
			seq, width, height, flags, err := decodeFrameStart(payload)
			if err != nil {
				c.finish(err)
				return
			}
			c.fbMu.Lock()
			if c.fb == nil || c.fb.Bounds().Dx() != width || c.fb.Bounds().Dy() != height || flags&frameFlagKey != 0 {
				c.fb = image.NewRGBA(image.Rect(0, 0, width, height))
				for i := 3; i < len(c.fb.Pix); i += 4 {
					c.fb.Pix[i] = 255
				}
			}
			c.fbMu.Unlock()
			current = FrameUpdate{Seq: seq, Width: width, Height: height, Keyframe: flags&frameFlagKey != 0}
			inFrame = true
			if c.opts.OnEncoded != nil {
				c.opts.OnEncoded(EncodedEvent{Kind: EncodedFrameStart, Seq: seq, Width: width, Height: height, Keyframe: current.Keyframe})
			}
		case recRects:
			if !inFrame {
				c.finish(errors.New("screen rects outside a frame"))
				return
			}
			rects, err := unpackRects(payload)
			if err != nil {
				c.finish(err)
				return
			}
			if c.opts.OnEncoded != nil {
				c.opts.OnEncoded(EncodedEvent{Kind: EncodedRects, Seq: current.Seq, Rects: rects})
			}
			dirty, err := c.drawRects(rects)
			if err != nil {
				c.finish(err)
				return
			}
			current.Dirty = append(current.Dirty, dirty...)
			current.Bytes += len(payload)
		case recFrameEnd:
			seq, flags, err := decodeFrameEnd(payload)
			if err != nil || !inFrame || seq != current.Seq {
				c.finish(errors.New("invalid screen frame end"))
				return
			}
			inFrame = false
			current.Lossless = flags&frameEndLossless != 0
			current.Settled = flags&frameEndSettled != 0
			c.fbMu.Lock()
			c.seq = seq
			c.lossless = current.Lossless
			c.fbMu.Unlock()
			if err := c.conn.WriteRecord(recAck, encodeSeq(seq)); err != nil {
				c.finish(err)
				return
			}
			if c.opts.OnEncoded != nil {
				c.opts.OnEncoded(EncodedEvent{Kind: EncodedFrameEnd, Seq: seq, Lossless: current.Lossless})
			}
			if c.opts.OnFrame != nil {
				c.opts.OnFrame(current)
			}
		case recCursor:
			record, err := decodeCursor(payload)
			if err != nil {
				continue
			}
			c.stateMu.Lock()
			if len(record.PNG) > 0 {
				if decoded, err := png.Decode(bytes.NewReader(record.PNG)); err == nil {
					c.shapes[record.ShapeID] = toRGBA(decoded)
				}
			}
			c.cursor = CursorState{X: record.X, Y: record.Y, Visible: record.Visible, ShapeID: record.ShapeID, HotX: record.HotX, HotY: record.HotY, Shape: c.shapes[record.ShapeID]}
			cursor := c.cursor
			c.stateMu.Unlock()
			if c.opts.OnCursor != nil {
				c.opts.OnCursor(cursor)
			}
		case recJSON:
			var message Message
			if err := json.Unmarshal(payload, &message); err != nil {
				continue
			}
			switch message.Type {
			case "reply":
				if value, ok := c.pending.Load(message.ID); ok {
					value.(chan Message) <- message
				}
			case "status":
				if message.Status != nil {
					c.stateMu.Lock()
					c.status = *message.Status
					c.stateMu.Unlock()
					if c.opts.OnStatus != nil {
						c.opts.OnStatus(*message.Status)
					}
				}
			}
		case recPing:
			_ = c.conn.WriteRecord(recPong, payload)
		}
	}
}

type decodedRect struct {
	rect  EncodedRect
	image image.Image
	err   error
}

func (c *LiveClient) drawRects(rects []EncodedRect) ([]image.Rectangle, error) {
	decoded := make([]decodedRect, len(rects))
	var wg sync.WaitGroup
	workers := max(1, min(runtime.NumCPU(), len(rects)))
	next := make(chan int)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range next {
				decoded[index] = decodeRect(rects[index])
			}
		}()
	}
	for index := range rects {
		next <- index
	}
	close(next)
	wg.Wait()
	dirty := make([]image.Rectangle, 0, len(rects))
	c.fbMu.Lock()
	defer c.fbMu.Unlock()
	if c.fb == nil {
		return nil, errors.New("screen rects before a frame")
	}
	bounds := c.fb.Bounds()
	for _, item := range decoded {
		if item.err != nil {
			return nil, item.err
		}
		target := image.Rect(item.rect.X, item.rect.Y, item.rect.X+item.rect.W, item.rect.Y+item.rect.H)
		if !target.In(bounds) {
			return nil, errors.New("screen rect outside the framebuffer")
		}
		if item.image == nil {
			fill := item.rect.Data
			draw.Draw(c.fb, target, &image.Uniform{C: color.RGBA{R: fill[0], G: fill[1], B: fill[2], A: 255}}, image.Point{}, draw.Src)
		} else {
			if item.image.Bounds().Dx() != item.rect.W || item.image.Bounds().Dy() != item.rect.H {
				return nil, errors.New("screen rect size does not match its image")
			}
			draw.Draw(c.fb, target, item.image, item.image.Bounds().Min, draw.Src)
		}
		dirty = append(dirty, target)
	}
	return dirty, nil
}

func decodeRect(rect EncodedRect) decodedRect {
	result := decodedRect{rect: rect}
	switch rect.Enc {
	case EncSolid:
		if len(rect.Data) != 4 {
			result.err = errors.New("invalid solid rect")
		}
	case EncPNG:
		result.image, result.err = png.Decode(bytes.NewReader(rect.Data))
	case EncJPEG:
		result.image, result.err = jpeg.Decode(bytes.NewReader(rect.Data))
	case EncJPEGScaled:
		var small image.Image
		small, result.err = jpeg.Decode(bytes.NewReader(rect.Data))
		if result.err == nil {
			bounds := small.Bounds()
			if bounds.Dx() <= 0 || bounds.Dy() <= 0 || bounds.Dx() > rect.W || bounds.Dy() > rect.H {
				result.err = errors.New("invalid scaled screen rect")
			} else {
				result.image = upscaleBilinear(toRGBA(small), rect.W, rect.H)
			}
		}
	default:
		result.err = fmt.Errorf("unsupported rect encoding %d", rect.Enc)
	}
	return result
}

func toRGBA(source image.Image) *image.RGBA {
	if rgba, ok := source.(*image.RGBA); ok {
		return rgba
	}
	bounds := source.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(out, out.Bounds(), source, bounds.Min, draw.Src)
	return out
}

func (c *LiveClient) legacyLoop() {
	for {
		frame, payloads, err := ReadFrame(c.rw)
		if err != nil {
			c.finish(err)
			return
		}
		if err := c.applyLegacy(frame, payloads); err != nil {
			c.finish(err)
			return
		}
	}
}

func (c *LiveClient) applyLegacy(frame Frame, payloads [][]byte) error {
	total := 0
	for _, payload := range payloads {
		total += len(payload)
	}
	c.bytesIn.Add(uint64(total))
	c.fbMu.Lock()
	canvas, err := ApplyFrame(c.fb, frame, payloads)
	if err != nil {
		c.fbMu.Unlock()
		return err
	}
	c.fb = canvas
	c.seq = frame.Seq
	c.lossless = true
	dirty := make([]image.Rectangle, 0, len(frame.Rects))
	for _, rect := range frame.Rects {
		dirty = append(dirty, image.Rect(rect.X, rect.Y, rect.X+rect.Width, rect.Y+rect.Height))
	}
	c.fbMu.Unlock()
	c.stateMu.Lock()
	c.status.Width, c.status.Height = frame.Width, frame.Height
	c.stateMu.Unlock()
	if c.opts.OnEncoded != nil {
		key := frame.Type == FrameFull
		c.opts.OnEncoded(EncodedEvent{Kind: EncodedFrameStart, Seq: frame.Seq, Width: frame.Width, Height: frame.Height, Keyframe: key})
		rects := make([]EncodedRect, 0, len(frame.Rects))
		for index, rect := range frame.Rects {
			rects = append(rects, EncodedRect{X: rect.X, Y: rect.Y, W: rect.Width, H: rect.Height, Enc: EncPNG, Data: payloads[index]})
		}
		c.opts.OnEncoded(EncodedEvent{Kind: EncodedRects, Seq: frame.Seq, Rects: rects})
		c.opts.OnEncoded(EncodedEvent{Kind: EncodedFrameEnd, Seq: frame.Seq, Lossless: true})
	}
	if c.opts.OnFrame != nil {
		c.opts.OnFrame(FrameUpdate{Seq: frame.Seq, Width: frame.Width, Height: frame.Height, Dirty: dirty, Keyframe: frame.Type == FrameFull, Lossless: true, Bytes: total, Legacy: true})
	}
	return nil
}

// upscaleBilinear scales src up to width x height.
func upscaleBilinear(src *image.RGBA, width, height int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	xRatio := float64(sw) / float64(width)
	yRatio := float64(sh) / float64(height)
	for y := 0; y < height; y++ {
		fy := (float64(y)+0.5)*yRatio - 0.5
		y0 := int(fy)
		if fy < 0 {
			fy, y0 = 0, 0
		}
		y1 := min(y0+1, sh-1)
		wy := fy - float64(y0)
		for x := 0; x < width; x++ {
			fx := (float64(x)+0.5)*xRatio - 0.5
			x0 := int(fx)
			if fx < 0 {
				fx, x0 = 0, 0
			}
			x1 := min(x0+1, sw-1)
			wx := fx - float64(x0)
			p00 := src.PixOffset(x0, y0)
			p01 := src.PixOffset(x1, y0)
			p10 := src.PixOffset(x0, y1)
			p11 := src.PixOffset(x1, y1)
			offset := out.PixOffset(x, y)
			for channel := 0; channel < 3; channel++ {
				top := float64(src.Pix[p00+channel])*(1-wx) + float64(src.Pix[p01+channel])*wx
				bottom := float64(src.Pix[p10+channel])*(1-wx) + float64(src.Pix[p11+channel])*wx
				out.Pix[offset+channel] = uint8(top*(1-wy) + bottom*wy + 0.5)
			}
			out.Pix[offset+3] = 255
		}
	}
	return out
}
