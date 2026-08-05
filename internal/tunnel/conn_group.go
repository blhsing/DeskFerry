package tunnel

import (
	"context"
	"net"
	"sync"
)

// ConnGroup tracks local connections so they can all be closed when their
// listener stops. Connections remain independent: RDP clients routinely open
// short negotiation and retry sockets, and one of those must not terminate an
// already established desktop session.
type ConnGroup struct {
	mu      sync.Mutex
	nextID  uint64
	entries map[uint64]*groupConn
	closed  bool
}

type groupConn struct {
	conn   net.Conn
	cancel context.CancelFunc
}

// Begin registers conn until the returned release function is called. Release
// is idempotent and never affects another connection in the group.
func (group *ConnGroup) Begin(parent context.Context, conn net.Conn) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)

	group.mu.Lock()
	group.nextID++
	id := group.nextID
	if group.entries == nil {
		group.entries = make(map[uint64]*groupConn)
	}
	closed := group.closed
	if !closed {
		group.entries[id] = &groupConn{conn: conn, cancel: cancel}
	}
	group.mu.Unlock()

	if closed {
		cancel()
		_ = conn.Close()
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			group.mu.Lock()
			delete(group.entries, id)
			group.mu.Unlock()
		})
	}
	return ctx, release
}

// Close cancels and closes every registered connection.
func (group *ConnGroup) Close() {
	group.mu.Lock()
	if group.closed {
		group.mu.Unlock()
		return
	}
	group.closed = true
	entries := group.entries
	group.entries = nil
	group.mu.Unlock()
	for _, entry := range entries {
		entry.cancel()
		_ = entry.conn.Close()
	}
}
