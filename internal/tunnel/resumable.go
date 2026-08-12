package tunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

const (
	resumableFrameData = byte(1)
	resumableFrameAck  = byte(2)
	resumableHeaderLen = 9
	resumableChunkSize = 64 * 1024
	resumableMaxBuffer = 8 * 1024 * 1024
	resumableWindow    = 5 * time.Minute
)

type ResumableWebSocketOptions struct {
	RelayAddr string
	Proxy     string
	Token     string
	SessionID string
	Side      string
	RoomProof string
	Service   string
}

// NewResumableWebSocketConn exposes a reliable byte stream over replaceable
// WebSockets. Sequence acknowledgements and a bounded replay buffer hide
// transient proxy disconnects from the local TCP peer.
func NewResumableWebSocketConn(ctx context.Context, initial *websocket.Conn, opts ResumableWebSocketOptions) net.Conn {
	childCtx, cancel := context.WithCancel(ctx)
	c := &resumableWebSocketConn{
		ctx:        childCtx,
		cancel:     cancel,
		opts:       opts,
		lost:       make(chan struct{}, 1),
		localAddr:  resumableAddr("deskferry-local"),
		remoteAddr: resumableAddr(opts.RelayAddr),
	}
	c.cond = sync.NewCond(&c.mu)
	go c.connectionLoop(initial)
	return c
}

type resumableWebSocketConn struct {
	ctx    context.Context
	cancel context.CancelFunc
	opts   ResumableWebSocketOptions

	mu          sync.Mutex
	cond        *sync.Cond
	ws          *websocket.Conn
	generation  uint64
	closed      bool
	terminalErr error
	lostAt      time.Time

	recvBuffer []byte
	recvOffset uint64
	sendBuffer []byte
	sendBase   uint64
	sendEnd    uint64

	writeMu sync.Mutex
	lost    chan struct{}

	localAddr  net.Addr
	remoteAddr net.Addr
}

func (c *resumableWebSocketConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.recvBuffer) == 0 && !c.closed {
		c.cond.Wait()
	}
	if len(c.recvBuffer) == 0 {
		if c.terminalErr != nil {
			return 0, c.terminalErr
		}
		return 0, io.EOF
	}
	n := copy(p, c.recvBuffer)
	copy(c.recvBuffer, c.recvBuffer[n:])
	c.recvBuffer = c.recvBuffer[:len(c.recvBuffer)-n]
	c.cond.Broadcast()
	return n, nil
}

func (c *resumableWebSocketConn) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		size := len(p)
		if size > resumableChunkSize {
			size = resumableChunkSize
		}
		chunk := append([]byte(nil), p[:size]...)
		offset, err := c.queueSend(chunk)
		if err != nil {
			return written, err
		}
		if err := c.sendDataUntilAccepted(offset, chunk); err != nil {
			return written, err
		}
		written += size
		p = p[size:]
	}
	return written, nil
}

func (c *resumableWebSocketConn) queueSend(payload []byte) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.sendBuffer)+len(payload) > resumableMaxBuffer && !c.closed {
		c.cond.Wait()
	}
	if c.closed {
		return 0, c.connectionErrorLocked()
	}
	offset := c.sendEnd
	c.sendBuffer = append(c.sendBuffer, payload...)
	c.sendEnd += uint64(len(payload))
	return offset, nil
}

func (c *resumableWebSocketConn) sendDataUntilAccepted(offset uint64, payload []byte) error {
	frame := makeFrame(resumableFrameData, offset, payload)
	for {
		ws, generation, err := c.waitTransport()
		if err != nil {
			return err
		}
		if err := c.writeFrame(ws, frame); err == nil {
			return nil
		}
		c.dropTransport(ws, generation)
	}
}

func (c *resumableWebSocketConn) waitTransport() (*websocket.Conn, uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.ws == nil && !c.closed {
		c.cond.Wait()
	}
	if c.closed {
		return nil, 0, c.connectionErrorLocked()
	}
	return c.ws, c.generation, nil
}

func (c *resumableWebSocketConn) connectionErrorLocked() error {
	if c.terminalErr != nil {
		return c.terminalErr
	}
	return net.ErrClosed
}

func (c *resumableWebSocketConn) writeFrame(ws *websocket.Conn, frame []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	writeCtx, cancel := context.WithTimeout(c.ctx, 20*time.Second)
	defer cancel()
	return ws.Write(writeCtx, websocket.MessageBinary, frame)
}

func (c *resumableWebSocketConn) connectionLoop(initial *websocket.Conn) {
	defer func() {
		if err := c.ctx.Err(); err != nil {
			c.setTerminal(err)
		}
	}()
	ws := initial
	backoff := 250 * time.Millisecond
	for {
		if ws != nil {
			c.drainLostSignal()
			if err := c.attachTransport(ws); err == nil {
				backoff = 250 * time.Millisecond
				select {
				case <-c.ctx.Done():
					return
				case <-c.lost:
				}
			} else {
				CloseWebSocket(ws)
			}
			ws = nil
		}
		if c.ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		if c.lostAt.IsZero() {
			c.lostAt = time.Now()
		}
		lostAt := c.lostAt
		c.mu.Unlock()
		if time.Since(lostAt) >= resumableWindow {
			c.setTerminal(fmt.Errorf("relay session %s could not resume within %s", c.opts.SessionID, resumableWindow))
			return
		}

		remaining := resumableWindow - time.Since(lostAt)
		dialTimeout := 20 * time.Second
		if remaining < dialTimeout {
			dialTimeout = remaining
		}
		dialCtx, cancelDial := context.WithTimeout(c.ctx, dialTimeout)
		candidate, err := c.dialResume(dialCtx)
		cancelDial()
		if err == nil {
			// Once the proxy has accepted the WebSocket, keep this attachment at
			// the relay until its peer arrives. Re-dialing on a short timer would
			// otherwise leave stale resume sockets queued at the relay.
			resumeCtx, cancelResume := context.WithTimeout(c.ctx, remaining)
			err = AwaitWebSocketResume(resumeCtx, candidate, c.opts.SessionID)
			cancelResume()
			if err != nil {
				CloseWebSocket(candidate)
			}
		}
		if err == nil {
			ws = candidate
			continue
		}
		if IsTerminalSessionError(err) {
			c.setTerminal(fmt.Errorf("relay session %s is closed: %w", c.opts.SessionID, err))
			return
		}
		timer := time.NewTimer(backoff)
		select {
		case <-c.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 5*time.Second {
			backoff *= 2
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
		}
	}
}

func (c *resumableWebSocketConn) drainLostSignal() {
	for {
		select {
		case <-c.lost:
		default:
			return
		}
	}
}

func (c *resumableWebSocketConn) dialResume(ctx context.Context) (*websocket.Conn, error) {
	headers := http.Header{}
	headers.Set(HeaderSessionID, c.opts.SessionID)
	headers.Set(HeaderSessionSide, c.opts.Side)
	if c.opts.RoomProof != "" {
		headers.Set(HeaderRoomProof, c.opts.RoomProof)
	}
	AddServiceHeader(headers, c.opts.Service)
	ws, err := DialWebSocketWithHeaders(ctx, c.opts.RelayAddr, c.opts.Proxy, RoleResume, c.opts.Token, headers)
	if err != nil {
		return nil, err
	}
	return ws, nil
}

func (c *resumableWebSocketConn) attachTransport(ws *websocket.Conn) error {
	c.writeMu.Lock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.writeMu.Unlock()
		return net.ErrClosed
	}
	c.generation++
	generation := c.generation
	c.ws = ws
	c.lostAt = time.Time{}
	recvOffset := c.recvOffset
	sendBase := c.sendBase
	replay := append([]byte(nil), c.sendBuffer...)
	c.mu.Unlock()

	if err := writeFrameWithTimeout(c.ctx, ws, makeFrame(resumableFrameAck, recvOffset, nil)); err != nil {
		c.writeMu.Unlock()
		c.dropTransport(ws, generation)
		return err
	}
	for len(replay) > 0 {
		size := len(replay)
		if size > resumableChunkSize {
			size = resumableChunkSize
		}
		if err := writeFrameWithTimeout(c.ctx, ws, makeFrame(resumableFrameData, sendBase, replay[:size])); err != nil {
			c.writeMu.Unlock()
			c.dropTransport(ws, generation)
			return err
		}
		sendBase += uint64(size)
		replay = replay[size:]
	}
	c.writeMu.Unlock()

	c.mu.Lock()
	c.cond.Broadcast()
	c.mu.Unlock()
	go c.readTransport(ws, generation)
	return nil
}

func writeFrameWithTimeout(ctx context.Context, ws *websocket.Conn, frame []byte) error {
	writeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return ws.Write(writeCtx, websocket.MessageBinary, frame)
}

func (c *resumableWebSocketConn) readTransport(ws *websocket.Conn, generation uint64) {
	for {
		typ, payload, err := ws.Read(c.ctx)
		if err != nil {
			if isLogicalSessionClose(err) {
				c.setTerminal(io.EOF)
			} else {
				c.dropTransport(ws, generation)
			}
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		frameType, offset, data, err := parseFrame(payload)
		if err != nil {
			c.dropTransport(ws, generation)
			return
		}
		switch frameType {
		case resumableFrameAck:
			if !c.applyAck(offset) {
				c.dropTransport(ws, generation)
				return
			}
		case resumableFrameData:
			ack, ok := c.applyData(offset, data)
			if !ok {
				c.dropTransport(ws, generation)
				return
			}
			if err := c.writeFrame(ws, makeFrame(resumableFrameAck, ack, nil)); err != nil {
				c.dropTransport(ws, generation)
				return
			}
		default:
			c.dropTransport(ws, generation)
			return
		}
	}
}

func isLogicalSessionClose(err error) bool {
	var closeErr websocket.CloseError
	return errors.As(err, &closeErr) && closeErr.Code == websocket.StatusNormalClosure && closeErr.Reason == "session closed"
}

func (c *resumableWebSocketConn) applyAck(offset uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if offset < c.sendBase || offset > c.sendEnd {
		return false
	}
	drop := int(offset - c.sendBase)
	copy(c.sendBuffer, c.sendBuffer[drop:])
	c.sendBuffer = c.sendBuffer[:len(c.sendBuffer)-drop]
	c.sendBase = offset
	c.cond.Broadcast()
	return true
}

func (c *resumableWebSocketConn) applyData(offset uint64, data []byte) (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	end := offset + uint64(len(data))
	if end < offset {
		return c.recvOffset, false
	}
	if offset > c.recvOffset {
		return c.recvOffset, false
	}
	if end <= c.recvOffset {
		return c.recvOffset, true
	}
	data = data[c.recvOffset-offset:]
	for len(c.recvBuffer)+len(data) > resumableMaxBuffer && !c.closed {
		c.cond.Wait()
	}
	if c.closed {
		return c.recvOffset, false
	}
	c.recvBuffer = append(c.recvBuffer, data...)
	c.recvOffset += uint64(len(data))
	c.cond.Broadcast()
	return c.recvOffset, true
}

func (c *resumableWebSocketConn) dropTransport(ws *websocket.Conn, generation uint64) {
	c.mu.Lock()
	if c.ws != ws || c.generation != generation || c.closed {
		c.mu.Unlock()
		return
	}
	c.ws = nil
	if c.lostAt.IsZero() {
		c.lostAt = time.Now()
	}
	c.cond.Broadcast()
	c.mu.Unlock()
	_ = ws.CloseNow()
	select {
	case c.lost <- struct{}{}:
	default:
	}
}

func (c *resumableWebSocketConn) setTerminal(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.terminalErr = err
	ws := c.ws
	c.ws = nil
	c.cond.Broadcast()
	c.mu.Unlock()
	c.cancel()
	if ws != nil {
		_ = ws.CloseNow()
	}
}

func (c *resumableWebSocketConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	ws := c.ws
	c.ws = nil
	c.cond.Broadcast()
	c.mu.Unlock()
	if ws != nil {
		_ = ws.Close(websocket.StatusNormalClosure, "session closed")
	}
	c.cancel()
	return nil
}

func (c *resumableWebSocketConn) LocalAddr() net.Addr              { return c.localAddr }
func (c *resumableWebSocketConn) RemoteAddr() net.Addr             { return c.remoteAddr }
func (c *resumableWebSocketConn) SetDeadline(time.Time) error      { return nil }
func (c *resumableWebSocketConn) SetReadDeadline(time.Time) error  { return nil }
func (c *resumableWebSocketConn) SetWriteDeadline(time.Time) error { return nil }

type resumableAddr string

func (a resumableAddr) Network() string { return "deskferry-resumable" }
func (a resumableAddr) String() string  { return string(a) }

func makeFrame(frameType byte, offset uint64, payload []byte) []byte {
	frame := make([]byte, resumableHeaderLen+len(payload))
	frame[0] = frameType
	binary.BigEndian.PutUint64(frame[1:resumableHeaderLen], offset)
	copy(frame[resumableHeaderLen:], payload)
	return frame
}

func parseFrame(frame []byte) (byte, uint64, []byte, error) {
	if len(frame) < resumableHeaderLen {
		return 0, 0, nil, errors.New("resumable frame is too short")
	}
	frameType := frame[0]
	offset := binary.BigEndian.Uint64(frame[1:resumableHeaderLen])
	payload := frame[resumableHeaderLen:]
	if frameType == resumableFrameAck && len(payload) != 0 {
		return 0, 0, nil, errors.New("resumable acknowledgement has a payload")
	}
	if frameType == resumableFrameData && len(payload) > resumableChunkSize {
		return 0, 0, nil, errors.New("resumable data frame exceeds the chunk limit")
	}
	return frameType, offset, payload, nil
}
