package tunnel

import (
	"io"
	"net"
	"sync"
	"time"
)

type closeWriter interface {
	CloseWrite() error
}

type closeReader interface {
	CloseRead() error
}

type CopyResult struct {
	Bytes         int64
	CopyErr       error
	CloseWriteErr error
	CloseReadErr  error
}

type PipeResult struct {
	AToB CopyResult
	BToA CopyResult
	// FirstCompleted records the source side whose copy loop ended first. An
	// orderly EOF is still an end signal, so error presence alone cannot
	// reliably identify which endpoint initiated a shutdown.
	FirstCompleted string
	ACloseErr      error
	BCloseErr      error
	Duration       time.Duration
}

// EndInitiator identifies which source side ended first, including orderly
// EOFs. It makes a local RDP socket closure visibly different from a relay
// transport failure in agent diagnostics.
func (result PipeResult) EndInitiator(aName, bName string) string {
	switch result.FirstCompleted {
	case "a":
		return aName
	case "b":
		return bName
	}
	switch {
	case result.AToB.CopyErr != nil && result.BToA.CopyErr == nil:
		return aName
	case result.BToA.CopyErr != nil && result.AToB.CopyErr == nil:
		return bName
	case result.AToB.CopyErr != nil && result.BToA.CopyErr != nil:
		return "both"
	default:
		return "clean_or_unknown"
	}
}

func Pipe(a, b net.Conn) {
	_ = PipeWithResult(a, b)
}

// PipeWithResult copies both directions until each side ends and returns the
// errors and byte counts that explain why the stream stopped. AToB describes
// bytes read from a and written to b; BToA describes the reverse direction.
func PipeWithResult(a, b net.Conn) PipeResult {
	started := time.Now()
	var result PipeResult
	var wg sync.WaitGroup
	completed := make(chan string, 2)
	wg.Add(2)
	go copyHalf(&wg, b, a, &result.AToB, completed, "a")
	go copyHalf(&wg, a, b, &result.BToA, completed, "b")
	result.FirstCompleted = <-completed
	wg.Wait()
	result.ACloseErr = a.Close()
	result.BCloseErr = b.Close()
	result.Duration = time.Since(started)
	return result
}

func copyHalf(wg *sync.WaitGroup, dst, src net.Conn, result *CopyResult, completed chan<- string, source string) {
	defer wg.Done()
	result.Bytes, result.CopyErr = io.Copy(dst, src)
	completed <- source
	if cw, ok := dst.(closeWriter); ok {
		result.CloseWriteErr = cw.CloseWrite()
	} else {
		result.CloseWriteErr = dst.Close()
	}
	if cr, ok := src.(closeReader); ok {
		result.CloseReadErr = cr.CloseRead()
	}
}
