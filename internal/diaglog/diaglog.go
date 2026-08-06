package diaglog

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	DefaultRetentionDays = 7
	maxLogBytes          = 8 * 1024 * 1024
)

// Enable adds daily, size-limited diagnostic logs alongside stderr. Files
// older than retentionDays calendar days are removed automatically.
func Enable(component string, systemWide bool, retentionDays int, additional ...io.Writer) (string, error) {
	if retentionDays < 1 || retentionDays > 3650 {
		return "", fmt.Errorf("log retention days must be between 1 and 3650")
	}
	base := ""
	if systemWide {
		base = os.Getenv("ProgramData")
	}
	if base == "" {
		var err error
		base, err = os.UserConfigDir()
		if err != nil {
			return "", err
		}
	}
	dir := filepath.Join(base, "DeskFerry")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create diagnostic log directory: %w", err)
	}
	w, err := newDailyWriter(dir, component, retentionDays, maxLogBytes, time.Now)
	if err != nil {
		return "", err
	}
	// Windows services can have an invalid stderr handle. Keep best-effort
	// stderr last so it cannot prevent local persistence or relay queueing.
	writers := []io.Writer{w}
	writers = append(writers, additional...)
	writers = append(writers, os.Stderr)
	log.SetOutput(io.MultiWriter(writers...))
	return w.path, nil
}

type dailyWriter struct {
	mu            sync.Mutex
	dir           string
	component     string
	retentionDays int
	maxSize       int64
	now           func() time.Time
	date          string
	path          string
	file          *os.File
	size          int64
}

func newDailyWriter(dir, component string, retentionDays int, maxSize int64, now func() time.Time) (*dailyWriter, error) {
	w := &dailyWriter{dir: dir, component: component, retentionDays: retentionDays, maxSize: maxSize, now: now}
	if err := w.openForDate(now()); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *dailyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if today := w.now(); today.Format("2006-01-02") != w.date {
		if err := w.openForDate(today); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *dailyWriter) openForDate(now time.Time) error {
	if w.file != nil {
		_ = w.file.Close()
	}
	w.date = now.Format("2006-01-02")
	w.path = filepath.Join(w.dir, fmt.Sprintf("%s-%s.log", w.component, w.date))
	if err := prune(w.dir, w.component, now, w.retentionDays); err != nil {
		return err
	}
	return w.open()
}

func (w *dailyWriter) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open diagnostic log: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("stat diagnostic log: %w", err)
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *dailyWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	rotated := w.path + ".old"
	_ = os.Remove(rotated)
	if err := os.Rename(w.path, rotated); err != nil && !os.IsNotExist(err) {
		_ = w.open()
		return fmt.Errorf("rotate diagnostic log: %w", err)
	}
	w.size = 0
	return w.open()
}

func prune(dir, component string, now time.Time, retentionDays int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read diagnostic log directory: %w", err)
	}
	cutoff := beginningOfDay(now).AddDate(0, 0, -(retentionDays - 1))
	prefix := component + "-"
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		var fileDate time.Time
		if strings.HasPrefix(name, prefix) && len(name) >= len(prefix)+10 {
			fileDate, _ = time.ParseInLocation("2006-01-02", name[len(prefix):len(prefix)+10], now.Location())
		} else if name == component+".log" || name == component+".log.old" {
			info, statErr := entry.Info()
			if statErr != nil {
				return statErr
			}
			fileDate = beginningOfDay(info.ModTime().In(now.Location()))
		}
		if !fileDate.IsZero() && fileDate.Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove expired diagnostic log %s: %w", name, err)
			}
		}
	}
	return nil
}

func beginningOfDay(value time.Time) time.Time {
	year, month, day := value.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, value.Location())
}
