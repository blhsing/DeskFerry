//go:build windows

package workconfigurator

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	workActivityInitialLines = 100
	workActivityPollInterval = time.Second
)

type workActivityFollower struct {
	dir     string
	path    string
	offset  int64
	partial string
}

func newWorkActivityFollower(dir string) *workActivityFollower {
	return &workActivityFollower{dir: dir}
}

func workActivityLogDir() string {
	base := strings.TrimSpace(os.Getenv("ProgramData"))
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "DeskFerry")
}

func (f *workActivityFollower) initial(maxLines int) ([]string, error) {
	path, err := latestWorkActivityLog(f.dir)
	if err != nil || path == "" {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f.path = path
	f.offset = int64(len(data))
	lines, partial := completeLogLines(string(data))
	f.partial = partial
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines, nil
}

func (f *workActivityFollower) poll() ([]string, error) {
	path, err := latestWorkActivityLog(f.dir)
	if err != nil || path == "" {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(path, f.path) || info.Size() < f.offset {
		f.path = path
		f.offset = 0
		f.partial = ""
	}
	if info.Size() == f.offset {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	f.offset += int64(len(data))
	lines, partial := completeLogLines(f.partial + string(data))
	f.partial = partial
	return lines, nil
}

func latestWorkActivityLog(dir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "work-agent-*.log"))
	if err != nil {
		return "", err
	}
	latest := ""
	var latestMod time.Time
	for _, path := range matches {
		info, statErr := os.Stat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return "", statErr
		}
		if latest == "" || info.ModTime().After(latestMod) || info.ModTime().Equal(latestMod) && path > latest {
			latest = path
			latestMod = info.ModTime()
		}
	}
	return latest, nil
}

func completeLogLines(text string) (lines []string, partial string) {
	parts := strings.Split(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		partial = parts[len(parts)-1]
		parts = parts[:len(parts)-1]
	}
	for _, line := range parts {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines, partial
}

func formatWorkActivityLine(line string) string {
	line = strings.TrimSpace(line)
	parts := strings.SplitN(line, " ", 3)
	if len(parts) == 3 && len(parts[1]) >= len("15:04:05") {
		if _, err := time.Parse("2006/01/02 15:04:05", parts[0]+" "+parts[1][:len("15:04:05")]); err == nil {
			return parts[1][:len("15:04:05")] + "  " + parts[2]
		}
	}
	return line
}

func (a *app) startWorkActivityFollower() {
	if a.mw == nil || a.log == nil || a.activityStop != nil {
		return
	}
	stop := make(chan struct{})
	a.activityStop = stop
	follower := newWorkActivityFollower(workActivityLogDir())
	go func() {
		lastError := ""
		publish := func(lines []string, err error) {
			if err != nil {
				message := err.Error()
				if message != lastError {
					a.appendLog("Work service activity unavailable: %v", err)
					lastError = message
				}
				return
			}
			lastError = ""
			a.appendWorkActivityLines(lines)
		}
		lines, err := follower.initial(workActivityInitialLines)
		publish(lines, err)
		ticker := time.NewTicker(workActivityPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				lines, err := follower.poll()
				publish(lines, err)
			}
		}
	}()
}

func (a *app) stopWorkActivityFollower() {
	if a.activityStop == nil {
		return
	}
	close(a.activityStop)
	a.activityStop = nil
}

func (a *app) appendWorkActivityLines(lines []string) {
	if len(lines) == 0 || a.mw == nil || a.log == nil {
		return
	}
	formatted := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = formatWorkActivityLine(line); line != "" {
			formatted = append(formatted, line)
		}
	}
	if len(formatted) == 0 {
		return
	}
	a.mw.Synchronize(func() {
		if a.log == nil {
			return
		}
		a.log.AppendText(strings.Join(formatted, "\r\n") + "\r\n")
	})
}
