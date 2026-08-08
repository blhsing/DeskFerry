package screenview

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Receive sends a screen request and reconstructs each full image delivered by
// the Work agent. The callback is invoked synchronously, so the image remains
// stable until the callback returns.
func Receive(rw io.ReadWriter, request Request, onFrame func(Frame, *image.RGBA) error) error {
	if onFrame == nil {
		return errors.New("screen frame callback is required")
	}
	if err := request.Normalize(); err != nil {
		return err
	}
	if err := WriteRequest(rw, request); err != nil {
		return fmt.Errorf("send screen request: %w", err)
	}
	var canvas *image.RGBA
	for {
		frame, payloads, err := ReadFrame(rw)
		if err != nil {
			return err
		}
		canvas, err = ApplyFrame(canvas, frame, payloads)
		if err != nil {
			return err
		}
		if err := onFrame(frame, canvas); err != nil {
			return err
		}
		if request.Mode == ModeSingle {
			return nil
		}
	}
}

// CLIOptions describes file-oriented screenshot capture for Home-agent command
// lines. Exactly one of Screenshot or StreamDirectory must be set.
type CLIOptions struct {
	Screenshot      string
	StreamDirectory string
	Interval        time.Duration
	Count           int
	Stdout          io.Writer
	Status          io.Writer
	now             func() time.Time
}

func (o CLIOptions) Active() bool {
	return strings.TrimSpace(o.Screenshot) != "" || strings.TrimSpace(o.StreamDirectory) != ""
}

// Validate checks CLI screenshot arguments without opening a relay session.
func (o CLIOptions) Validate() error {
	_, err := o.request()
	return err
}

func (o CLIOptions) request() (Request, error) {
	screenshot := strings.TrimSpace(o.Screenshot)
	streamDirectory := strings.TrimSpace(o.StreamDirectory)
	if screenshot == "" && streamDirectory == "" {
		return Request{}, errors.New("use -screenshot or -screenshot-stream")
	}
	if screenshot != "" && streamDirectory != "" {
		return Request{}, errors.New("use either -screenshot or -screenshot-stream, not both")
	}
	if o.Count < 0 {
		return Request{}, errors.New("screen count cannot be negative")
	}
	if screenshot != "" && o.Count != 0 {
		return Request{}, errors.New("-screen-count can only be used with -screenshot-stream")
	}
	if streamDirectory == "-" {
		return Request{}, errors.New("-screenshot-stream requires a directory; use -screenshot - for one PNG on standard output")
	}
	interval := o.Interval
	if interval == 0 {
		interval = time.Duration(DefaultIntervalMS) * time.Millisecond
	}
	if interval%time.Millisecond != 0 {
		return Request{}, errors.New("screen interval must use whole milliseconds")
	}
	request := Request{Mode: ModeSingle, IntervalMS: int(interval / time.Millisecond), TileSize: DefaultTileSize}
	if streamDirectory != "" {
		request.Mode = ModeStream
	}
	if err := request.Normalize(); err != nil {
		return Request{}, err
	}
	return request, nil
}

// RunCLI receives authenticated screen frames and writes complete PNG images.
// Closing the context closes the connection so an idle stream exits promptly.
func RunCLI(ctx context.Context, conn io.ReadWriteCloser, options CLIOptions) error {
	request, err := options.request()
	if err != nil {
		return err
	}
	if options.Stdout == nil {
		options.Stdout = os.Stdout
	}
	if options.Status == nil {
		options.Status = os.Stderr
	}
	if options.now == nil {
		options.now = time.Now
	}

	streamDirectory := strings.TrimSpace(options.StreamDirectory)
	if streamDirectory != "" {
		if err := os.MkdirAll(streamDirectory, 0700); err != nil {
			return fmt.Errorf("create screenshot stream directory: %w", err)
		}
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	written := 0
	err = Receive(conn, request, func(frame Frame, canvas *image.RGBA) error {
		written++
		if request.Mode == ModeSingle && strings.TrimSpace(options.Screenshot) == "-" {
			if err := png.Encode(options.Stdout, canvas); err != nil {
				return fmt.Errorf("write screenshot to standard output: %w", err)
			}
			return nil
		}

		path := strings.TrimSpace(options.Screenshot)
		if request.Mode == ModeStream {
			name := fmt.Sprintf("DeskFerry-%s-%06d.png", options.now().Format("20060102-150405.000"), frame.Seq)
			path = filepath.Join(streamDirectory, name)
		}
		if err := writePNGAtomically(path, canvas); err != nil {
			return err
		}
		absolute, absoluteErr := filepath.Abs(path)
		if absoluteErr == nil {
			path = absolute
		}
		_, _ = fmt.Fprintln(options.Status, path)
		if request.Mode == ModeStream && options.Count > 0 && written >= options.Count {
			return errScreenCountReached
		}
		return nil
	})
	if errors.Is(err, errScreenCountReached) {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

var errScreenCountReached = errors.New("requested screenshot count reached")

func writePNGAtomically(path string, source image.Image) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("screenshot path is empty")
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".deskferry-screenshot-*.tmp")
	if err != nil {
		return fmt.Errorf("create screenshot %q: %w", path, err)
	}
	temporaryPath := temporary.Name()
	ok := false
	defer func() {
		_ = temporary.Close()
		if !ok {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0600); err != nil {
		return fmt.Errorf("protect screenshot %q: %w", path, err)
	}
	if err := png.Encode(temporary, source); err != nil {
		return fmt.Errorf("encode screenshot %q: %w", path, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close screenshot %q: %w", path, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("replace screenshot %q: %w", path, err)
		}
		if retryErr := os.Rename(temporaryPath, path); retryErr != nil {
			return fmt.Errorf("save screenshot %q: %w", path, retryErr)
		}
	}
	ok = true
	return nil
}
