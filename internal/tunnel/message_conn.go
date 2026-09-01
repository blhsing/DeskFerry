package tunnel

import (
	"context"
	"net"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// MessageConn is the message-oriented transport used by DeskFerry's relay
// protocol. A native WebSocket satisfies this interface. HTTPStreamConn also
// satisfies it, which lets callers use the same control and data protocols
// when a forward proxy rejects CONNECT and WebSocket upgrade traffic.
type MessageConn interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	Close(websocket.StatusCode, string) error
	CloseNow() error
}

// MessageNetConn exposes binary messages as a byte stream. It is deliberately
// independent of websocket.NetConn so HTTP-stream fallback connections and
// native WebSockets have identical net.Conn behavior.
func MessageNetConn(ctx context.Context, conn MessageConn) net.Conn {
	return &messageNetConn{
		ctx:        ctx,
		conn:       conn,
		localAddr:  messageAddr("deskferry-local"),
		remoteAddr: messageAddr("deskferry-relay"),
	}
}

type messageNetConn struct {
	ctx  context.Context
	conn MessageConn

	readMu  sync.Mutex
	writeMu sync.Mutex
	buffer  []byte

	deadlineMu    sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time

	localAddr  net.Addr
	remoteAddr net.Addr
}

func (c *messageNetConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for len(c.buffer) == 0 {
		ctx, cancel := c.operationContext(c.readDeadlineValue())
		typ, payload, err := c.conn.Read(ctx)
		cancel()
		if err != nil {
			return 0, err
		}
		if typ != websocket.MessageBinary {
			continue
		}
		c.buffer = payload
	}
	n := copy(p, c.buffer)
	c.buffer = c.buffer[n:]
	return n, nil
}

func (c *messageNetConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := c.operationContext(c.writeDeadlineValue())
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageBinary, append([]byte(nil), p...)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *messageNetConn) Close() error {
	return c.conn.Close(websocket.StatusNormalClosure, "session closed")
}

func (c *messageNetConn) LocalAddr() net.Addr  { return c.localAddr }
func (c *messageNetConn) RemoteAddr() net.Addr { return c.remoteAddr }

func (c *messageNetConn) SetDeadline(deadline time.Time) error {
	c.deadlineMu.Lock()
	c.readDeadline = deadline
	c.writeDeadline = deadline
	c.deadlineMu.Unlock()
	return nil
}

func (c *messageNetConn) SetReadDeadline(deadline time.Time) error {
	c.deadlineMu.Lock()
	c.readDeadline = deadline
	c.deadlineMu.Unlock()
	return nil
}

func (c *messageNetConn) SetWriteDeadline(deadline time.Time) error {
	c.deadlineMu.Lock()
	c.writeDeadline = deadline
	c.deadlineMu.Unlock()
	return nil
}

func (c *messageNetConn) readDeadlineValue() time.Time {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	return c.readDeadline
}

func (c *messageNetConn) writeDeadlineValue() time.Time {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	return c.writeDeadline
}

func (c *messageNetConn) operationContext(deadline time.Time) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return context.WithCancel(c.ctx)
	}
	return context.WithDeadline(c.ctx, deadline)
}

type messageAddr string

func (a messageAddr) Network() string { return "deskferry" }
func (a messageAddr) String() string  { return string(a) }
