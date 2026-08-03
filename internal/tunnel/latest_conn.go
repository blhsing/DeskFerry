package tunnel

import (
	"context"
	"net"
	"sync"
)

// LatestConnGroup keeps only the newest local connection active. RDP clients
// can leave retry sockets alive while authentication is in progress; allowing
// an older retry to connect later can replace the session the user just opened.
type LatestConnGroup struct {
	mu      sync.Mutex
	nextID  uint64
	current *latestConn
}

type latestConn struct {
	id     uint64
	conn   net.Conn
	cancel context.CancelFunc
}

// Begin registers conn as current and closes any connection it supersedes.
// The returned release function is safe to call after a newer Begin call.
func (group *LatestConnGroup) Begin(parent context.Context, conn net.Conn) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancel(parent)

	group.mu.Lock()
	group.nextID++
	entry := &latestConn{id: group.nextID, conn: conn, cancel: cancel}
	previous := group.current
	group.current = entry
	group.mu.Unlock()

	if previous != nil {
		previous.cancel()
		_ = previous.conn.Close()
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			group.mu.Lock()
			if group.current != nil && group.current.id == entry.id {
				group.current = nil
			}
			group.mu.Unlock()
		})
	}
	return ctx, release, previous != nil
}

// Close cancels and closes the currently active connection, if any.
func (group *LatestConnGroup) Close() {
	group.mu.Lock()
	current := group.current
	group.current = nil
	group.mu.Unlock()
	if current != nil {
		current.cancel()
		_ = current.conn.Close()
	}
}
