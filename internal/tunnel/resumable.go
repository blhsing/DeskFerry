package tunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

const (
	resumableFrameData       = byte(1)
	resumableFrameAck        = byte(2)
	resumableFramePing       = byte(3)
	resumableFramePong       = byte(4)
	resumableHeaderLen       = 9
	resumableChunkSize       = 64 * 1024
	resumableMaxBuffer       = 8 * 1024 * 1024
	resumableWindow          = 5 * time.Minute
	defaultHeartbeatInterval = 5 * time.Second
	defaultHeartbeatTimeout  = 15 * time.Second
	ackStallThreshold        = 3 * time.Second
	rdpAckRecoveryThreshold  = 5 * time.Second
	defaultAckRecovery       = 10 * time.Second
)

type ResumableWebSocketOptions struct {
	RelayAddr string
	Proxy     string
	Token     string
	SessionID string
	Side      string
	RoomProof string
	Service   string
	Heartbeat bool
	// Logf receives abnormal transport and data-progress diagnostics only.
	Logf func(string, ...any)

	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration
}

// NewResumableWebSocketConn exposes a reliable byte stream over replaceable
// WebSockets. Sequence acknowledgements and a bounded replay buffer hide
// transient proxy disconnects from the local TCP peer.
func NewResumableWebSocketConn(ctx context.Context, initial MessageConn, opts ResumableWebSocketOptions) net.Conn {
	childCtx, cancel := context.WithCancel(ctx)
	c := &resumableWebSocketConn{
		ctx:        childCtx,
		cancel:     cancel,
		opts:       opts,
		lost:       make(chan struct{}, 1),
		localAddr:  resumableAddr("deskferry-local"),
		remoteAddr: resumableAddr(opts.RelayAddr),
		heartbeats: make(map[uint64]*heartbeatState),
	}
	if c.opts.heartbeatInterval <= 0 {
		c.opts.heartbeatInterval = defaultHeartbeatInterval
	}
	if c.opts.heartbeatTimeout <= 0 {
		c.opts.heartbeatTimeout = defaultHeartbeatTimeout
	}
	c.cond = sync.NewCond(&c.mu)
	go c.connectionLoop(initial)
	go c.watchAckProgress()
	return c
}

type resumableWebSocketConn struct {
	ctx    context.Context
	cancel context.CancelFunc
	opts   ResumableWebSocketOptions

	mu          sync.Mutex
	cond        *sync.Cond
	ws          MessageConn
	generation  uint64
	closed      bool
	terminalErr error
	lostAt      time.Time

	recvBuffer      []byte
	recvOffset      uint64
	sendBuffer      []byte
	sendBase        uint64
	sendEnd         uint64
	lastAckProgress time.Time
	ackStallLogged  bool
	ackStallAt      time.Time

	writeMu        sync.Mutex
	lost           chan struct{}
	heartbeats     map[uint64]*heartbeatState
	heartbeatNonce uint64

	localAddr  net.Addr
	remoteAddr net.Addr
}

type heartbeatState struct {
	ack      chan uint64
	lost     chan struct{}
	stopOnce sync.Once
}

func newHeartbeatState() *heartbeatState {
	return &heartbeatState{ack: make(chan uint64, 1), lost: make(chan struct{})}
}

func (state *heartbeatState) stop() {
	state.stopOnce.Do(func() { close(state.lost) })
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
	if c.sendEnd == c.sendBase {
		c.lastAckProgress = time.Now()
	}
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
		} else {
			c.dropTransport(ws, generation, fmt.Sprintf("data write failed: %v", err))
		}
	}
}

func (c *resumableWebSocketConn) waitTransport() (MessageConn, uint64, error) {
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

func (c *resumableWebSocketConn) writeFrame(ws MessageConn, frame []byte) error {
	lockStarted := time.Now()
	c.writeMu.Lock()
	lockWait := time.Since(lockStarted)
	writeCtx, cancel := context.WithTimeout(c.ctx, 20*time.Second)
	writeStarted := time.Now()
	err := ws.Write(writeCtx, websocket.MessageBinary, frame)
	writeDuration := time.Since(writeStarted)
	c.writeMu.Unlock()
	cancel()
	if lockWait >= ackStallThreshold {
		c.diagnostic("slow transport write lock protocol=%s frame_type=%d bytes=%d wait=%s", MessageConnProtocol(ws), frame[0], len(frame), lockWait.Round(time.Millisecond))
	}
	if writeDuration >= ackStallThreshold {
		c.diagnostic("slow transport write protocol=%s frame_type=%d bytes=%d duration=%s error=%v", MessageConnProtocol(ws), frame[0], len(frame), writeDuration.Round(time.Millisecond), err)
	}
	return err
}

func (c *resumableWebSocketConn) connectionLoop(initial MessageConn) {
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
				c.diagnostic("transport attach failed: %v", err)
				CloseMessageConn(ws)
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
			// Reaching the relay proves the path is healthy again. If this
			// provisional attachment closes before its peer arrives, retry
			// promptly instead of retaining backoff accumulated while the
			// relay process was unavailable.
			backoff = 250 * time.Millisecond
			// Once the proxy has accepted the WebSocket, keep this attachment at
			// the relay until its peer arrives. Re-dialing on a short timer would
			// otherwise leave stale resume sockets queued at the relay.
			resumeCtx, cancelResume := context.WithTimeout(c.ctx, remaining)
			err = AwaitWebSocketResume(resumeCtx, candidate, c.opts.SessionID)
			cancelResume()
			if err != nil {
				CloseMessageConn(candidate)
			}
		}
		if err == nil {
			ws = candidate
			continue
		}
		c.diagnostic("resume attempt failed after=%s error=%v", time.Since(lostAt).Round(time.Millisecond), err)
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

func (c *resumableWebSocketConn) dialResume(ctx context.Context) (MessageConn, error) {
	headers := http.Header{}
	headers.Set(HeaderSessionID, c.opts.SessionID)
	headers.Set(HeaderSessionSide, c.opts.Side)
	if c.opts.RoomProof != "" {
		headers.Set(HeaderRoomProof, c.opts.RoomProof)
	}
	AddServiceHeader(headers, c.opts.Service)
	ws, err := DialMessageConnWithHeaders(ctx, c.opts.RelayAddr, c.opts.Proxy, RoleResume, c.opts.Token, headers)
	if err != nil {
		return nil, err
	}
	return ws, nil
}

func (c *resumableWebSocketConn) attachTransport(ws MessageConn) error {
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
	lostDuration := time.Since(c.lostAt)
	c.lostAt = time.Time{}
	if c.sendEnd > c.sendBase {
		c.lastAckProgress = time.Now()
	}
	recvOffset := c.recvOffset
	sendBase := c.sendBase
	replay := append([]byte(nil), c.sendBuffer...)
	replayBytes := len(replay)
	c.mu.Unlock()

	if err := writeFrameWithTimeout(c.ctx, ws, makeFrame(resumableFrameAck, recvOffset, nil)); err != nil {
		c.writeMu.Unlock()
		c.dropTransport(ws, generation, fmt.Sprintf("resume acknowledgement write failed: %v", err))
		return err
	}
	for len(replay) > 0 {
		size := len(replay)
		if size > resumableChunkSize {
			size = resumableChunkSize
		}
		if err := writeFrameWithTimeout(c.ctx, ws, makeFrame(resumableFrameData, sendBase, replay[:size])); err != nil {
			c.writeMu.Unlock()
			c.dropTransport(ws, generation, fmt.Sprintf("replay write failed: %v", err))
			return err
		}
		sendBase += uint64(size)
		replay = replay[size:]
	}
	c.writeMu.Unlock()

	var heartbeat *heartbeatState
	c.mu.Lock()
	if c.opts.Heartbeat && c.ws == ws && c.generation == generation && !c.closed {
		heartbeat = newHeartbeatState()
		c.heartbeats[generation] = heartbeat
	}
	c.cond.Broadcast()
	c.mu.Unlock()
	go c.readTransport(ws, generation)
	if generation > 1 {
		c.diagnostic("transport resumed generation=%d protocol=%s after=%s replay_bytes=%d", generation, MessageConnProtocol(ws), lostDuration.Round(time.Millisecond), replayBytes)
	}
	if heartbeat != nil {
		go c.heartbeatTransport(ws, generation, heartbeat)
	}
	return nil
}

func (c *resumableWebSocketConn) heartbeatTransport(ws MessageConn, generation uint64, state *heartbeatState) {
	defer func() {
		c.mu.Lock()
		if c.heartbeats[generation] == state {
			delete(c.heartbeats, generation)
		}
		c.mu.Unlock()
	}()
	timer := time.NewTimer(c.opts.heartbeatInterval)
	defer timer.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-state.lost:
			return
		case <-timer.C:
		}
		if !c.isCurrentTransport(ws, generation) {
			return
		}
		c.mu.Lock()
		c.heartbeatNonce++
		nonce := c.heartbeatNonce
		c.mu.Unlock()
		if err := c.writeFrame(ws, makeFrame(resumableFramePing, nonce, nil)); err != nil {
			c.dropTransport(ws, generation, fmt.Sprintf("heartbeat ping write failed: %v", err))
			return
		}
		pingAt := time.Now()
		timeout := time.NewTimer(c.opts.heartbeatTimeout)
		acknowledged := false
		for !acknowledged {
			select {
			case <-c.ctx.Done():
				timeout.Stop()
				return
			case <-state.lost:
				timeout.Stop()
				return
			case ack := <-state.ack:
				acknowledged = ack == nonce
			case <-timeout.C:
				c.dropTransport(ws, generation, fmt.Sprintf("heartbeat timed out after %s", c.opts.heartbeatTimeout))
				return
			}
		}
		if elapsed := time.Since(pingAt); elapsed >= ackStallThreshold {
			c.diagnostic("slow heartbeat generation=%d round_trip=%s", generation, elapsed.Round(time.Millisecond))
		}
		if !timeout.Stop() {
			select {
			case <-timeout.C:
			default:
			}
		}
		timer.Reset(c.opts.heartbeatInterval)
	}
}

func (c *resumableWebSocketConn) isCurrentTransport(ws MessageConn, generation uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && c.ws == ws && c.generation == generation
}

func writeFrameWithTimeout(ctx context.Context, ws MessageConn, frame []byte) error {
	writeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return ws.Write(writeCtx, websocket.MessageBinary, frame)
}

func (c *resumableWebSocketConn) readTransport(ws MessageConn, generation uint64) {
	for {
		typ, payload, err := ws.Read(c.ctx)
		if err != nil {
			if isLogicalSessionClose(err) {
				c.setTerminal(io.EOF)
			} else {
				c.dropTransport(ws, generation, fmt.Sprintf("transport read failed: %v", err))
			}
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		frameType, offset, data, err := parseFrame(payload)
		if err != nil {
			c.dropTransport(ws, generation, fmt.Sprintf("invalid resumable frame: %v", err))
			return
		}
		switch frameType {
		case resumableFrameAck:
			if !c.applyAck(offset) {
				c.dropTransport(ws, generation, fmt.Sprintf("invalid acknowledgement offset=%d", offset))
				return
			}
		case resumableFrameData:
			ack, ok := c.applyData(offset, data)
			if !ok {
				c.dropTransport(ws, generation, fmt.Sprintf("invalid data offset=%d", offset))
				return
			}
			if err := c.writeFrame(ws, makeFrame(resumableFrameAck, ack, nil)); err != nil {
				c.dropTransport(ws, generation, fmt.Sprintf("acknowledgement write failed: %v", err))
				return
			}
		case resumableFramePing:
			if err := c.writeFrame(ws, makeFrame(resumableFramePong, offset, nil)); err != nil {
				c.dropTransport(ws, generation, fmt.Sprintf("heartbeat pong write failed: %v", err))
				return
			}
		case resumableFramePong:
			c.signalHeartbeatAck(generation, offset)
		default:
			c.dropTransport(ws, generation, fmt.Sprintf("unexpected frame type=%d", frameType))
			return
		}
	}
}

func (c *resumableWebSocketConn) signalHeartbeatAck(generation, nonce uint64) {
	c.mu.Lock()
	state := c.heartbeats[generation]
	c.mu.Unlock()
	if state == nil {
		return
	}
	select {
	case state.ack <- nonce:
	default:
		select {
		case <-state.ack:
		default:
		}
		select {
		case state.ack <- nonce:
		default:
		}
	}
}

func isLogicalSessionClose(err error) bool {
	var closeErr websocket.CloseError
	return errors.As(err, &closeErr) && closeErr.Code == websocket.StatusNormalClosure && closeErr.Reason == "session closed"
}

func (c *resumableWebSocketConn) applyAck(offset uint64) bool {
	c.mu.Lock()
	if offset < c.sendBase || offset > c.sendEnd {
		c.mu.Unlock()
		return false
	}
	advanced := offset > c.sendBase
	stalled := c.ackStallLogged
	stallAt := c.ackStallAt
	drop := int(offset - c.sendBase)
	copy(c.sendBuffer, c.sendBuffer[drop:])
	c.sendBuffer = c.sendBuffer[:len(c.sendBuffer)-drop]
	c.sendBase = offset
	if advanced {
		c.lastAckProgress = time.Now()
		c.ackStallLogged = false
	}
	pending := c.sendEnd - c.sendBase
	c.cond.Broadcast()
	c.mu.Unlock()
	if advanced && stalled {
		c.diagnostic("acknowledgement progress resumed after=%s pending_bytes=%d", time.Since(stallAt).Round(time.Millisecond), pending)
	}
	return true
}

func (c *resumableWebSocketConn) watchAckProgress() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
		}
		c.checkAckProgress(time.Now())
	}
}

func (c *resumableWebSocketConn) checkAckProgress(now time.Time) {
	c.mu.Lock()
	pending := c.sendEnd - c.sendBase
	elapsed := now.Sub(c.lastAckProgress)
	if pending == 0 || c.ws == nil || elapsed < ackStallThreshold {
		c.mu.Unlock()
		return
	}
	ws := c.ws
	generation := c.generation
	protocol := MessageConnProtocol(c.ws)
	shouldLog := !c.ackStallLogged
	if shouldLog {
		c.ackStallLogged = true
		c.ackStallAt = now
	}
	recoveryThreshold := defaultAckRecovery
	if c.opts.Service == ServiceRDP {
		recoveryThreshold = rdpAckRecoveryThreshold
	}
	shouldRecover := elapsed >= recoveryThreshold
	c.mu.Unlock()
	if shouldLog {
		c.diagnostic("acknowledgements stalled generation=%d protocol=%s pending_bytes=%d no_progress=%s", generation, protocol, pending, elapsed.Round(time.Millisecond))
	}
	if shouldRecover {
		c.dropTransport(ws, generation, fmt.Sprintf("data acknowledgements made no progress for %s", elapsed.Round(time.Millisecond)))
	}
}

func (c *resumableWebSocketConn) diagnostic(format string, args ...any) {
	if c.opts.Logf == nil {
		return
	}
	message := fmt.Sprintf(format, args...)
	for _, secret := range []string{c.opts.Token, c.opts.RoomProof} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	if strings.Contains(c.opts.Proxy, "@") {
		message = strings.ReplaceAll(message, c.opts.Proxy, ProxySpecForLog(c.opts.Proxy))
	}
	c.opts.Logf("resumable session=%s side=%s service=%s relay=%s: %s",
		c.opts.SessionID, c.opts.Side, c.opts.Service, c.opts.RelayAddr, message)
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

func (c *resumableWebSocketConn) dropTransport(ws MessageConn, generation uint64, reason string) {
	c.mu.Lock()
	if c.ws != ws || c.generation != generation || c.closed {
		c.mu.Unlock()
		return
	}
	c.ws = nil
	heartbeat := c.heartbeats[generation]
	if c.lostAt.IsZero() {
		c.lostAt = time.Now()
	}
	c.cond.Broadcast()
	pending := c.sendEnd - c.sendBase
	c.mu.Unlock()
	c.diagnostic("transport lost generation=%d protocol=%s pending_bytes=%d reason=%s", generation, MessageConnProtocol(ws), pending, reason)
	if heartbeat != nil {
		heartbeat.stop()
	}
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
	if (frameType == resumableFrameAck || frameType == resumableFramePing || frameType == resumableFramePong) && len(payload) != 0 {
		return 0, 0, nil, errors.New("resumable control frame has a payload")
	}
	if frameType == resumableFrameData && len(payload) > resumableChunkSize {
		return 0, 0, nil, errors.New("resumable data frame exceeds the chunk limit")
	}
	return frameType, offset, payload, nil
}
