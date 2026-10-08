//go:build windows

package workservice

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"unsafe"

	"deskferry/internal/tunnel"
	"deskferry/internal/uiproxy"
	"golang.org/x/sys/windows"
)

var (
	proxyKernel      = windows.NewLazySystemDLL("kernel32.dll")
	findProxyChange  = proxyKernel.NewProc("FindFirstChangeNotificationW")
	nextProxyChange  = proxyKernel.NewProc("FindNextChangeNotification")
	closeProxyChange = proxyKernel.NewProc("FindCloseChangeNotification")
)

// Match the interactive identity used for integrated proxy authentication:
// console first, then other active sessions, then disconnected sessions.
func proxySettingsPath() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	if user.User.Sid.String() != "S-1-5-18" {
		base, err := windows.KnownFolderPath(windows.FOLDERID_RoamingAppData, 0)
		return uiproxy.Path(base), err
	}
	var info *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &info, &count); err != nil {
		return "", err
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(info)))
	sessions := unsafe.Slice(info, int(count))
	console := windows.WTSGetActiveConsoleSessionId()
	for _, state := range []uint32{windows.WTSActive, windows.WTSDisconnected} {
		for _, preferConsole := range []bool{true, false} {
			for _, session := range sessions {
				if session.State != state || (session.SessionID == console) != preferConsole {
					continue
				}
				var token windows.Token
				if err := windows.WTSQueryUserToken(session.SessionID, &token); err != nil {
					continue
				}
				base, err := token.KnownFolderPath(windows.FOLDERID_RoamingAppData, 0)
				token.Close()
				if err == nil {
					return uiproxy.Path(base), nil
				}
			}
		}
	}
	return "", fmt.Errorf("no logged-on Windows user settings are available")
}

// Directory notifications also catch atomic replacement of the settings file.
// Cancellation wakes the native wait; there is no periodic configuration poll.
func watchProxySettings(ctx context.Context, path string, changed chan<- struct{}) error {
	dir := filepath.Dir(path)
	for {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return fmt.Errorf("no settings directory available")
		}
		dir = parent
	}
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	handle, _, callErr := findProxyChange.Call(uintptr(unsafe.Pointer(name)), 1, windows.FILE_NOTIFY_CHANGE_LAST_WRITE|windows.FILE_NOTIFY_CHANGE_FILE_NAME|windows.FILE_NOTIFY_CHANGE_DIR_NAME)
	if windows.Handle(handle) == windows.InvalidHandle {
		return callErr
	}
	defer closeProxyChange.Call(handle)
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(stop)
	done := make(chan struct{})
	wakeFinished := make(chan struct{})
	defer func() { close(done); <-wakeFinished }()
	go func() {
		defer close(wakeFinished)
		select {
		case <-ctx.Done():
			windows.SetEvent(stop)
		case <-done:
		}
	}()
	// Subscribe before reading the snapshot to close the startup race.
	select {
	case changed <- struct{}{}:
	default:
	}
	for {
		event, err := windows.WaitForMultipleObjects([]windows.Handle{stop, windows.Handle(handle)}, false, windows.INFINITE)
		if err != nil {
			return err
		}
		if event == windows.WAIT_OBJECT_0 {
			return nil
		}
		if result, _, err := nextProxyChange.Call(handle); result == 0 {
			return err
		}
		select {
		case changed <- struct{}{}:
		default:
		}
	}
}

func followUIProxy(ctx context.Context, fallback string, sessions <-chan struct{}, updates chan<- string) {
	changes := make(chan struct{}, 1)
	var path string
	var stopWatch context.CancelFunc
	defer func() {
		if stopWatch != nil {
			stopWatch()
		}
	}()
	selectUser := func() {
		if stopWatch != nil {
			stopWatch()
		}
		var err error
		path, err = proxySettingsPath()
		if err != nil {
			log.Printf("UI proxy settings unavailable; using configured fallback: %v", err)
			path = ""
		}
		if path != "" {
			var watchCtx context.Context
			watchCtx, stopWatch = context.WithCancel(ctx)
			watchPath := path
			go func() {
				if err := watchProxySettings(watchCtx, watchPath, changes); err != nil && watchCtx.Err() == nil {
					log.Printf("watch UI proxy settings: %v", err)
				}
			}()
		}
	}
	last := ""
	haveSnapshot := false
	publish := func() {
		proxy := fallback
		if path != "" {
			value, found, err := uiproxy.Read(path)
			if err != nil {
				log.Printf("read UI proxy settings: %v", err)
				return
			}
			if found {
				proxy = value
			}
		}
		if haveSnapshot && proxy == last {
			return
		}
		select {
		case updates <- proxy:
			last = proxy
			haveSnapshot = true
		case <-ctx.Done():
		}
	}
	selectUser()
	publish()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sessions:
			selectUser()
			publish()
		case <-changes:
			publish()
		}
	}
}

func runFollowingUIProxy(ctx context.Context, cfg config, sessions <-chan struct{}) error {
	updates := make(chan string)
	go followUIProxy(ctx, cfg.Proxy, sessions, updates)
	return runProxyUpdates(ctx, cfg, updates, runWebSocketPoolsWithSessions)
}

func runProxyUpdates(ctx context.Context, cfg config, updates <-chan string, runPools func(context.Context, context.Context, config, chan struct{}) error) error {
	limiter := make(chan struct{}, cfg.ConcurrencyLimit)
	var cancel context.CancelFunc
	var finished chan error
	defer func() {
		if cancel != nil {
			cancel()
			<-finished
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case proxy := <-updates:
			if cancel != nil {
				cancel()
				<-finished
			}
			cfg.Proxy = proxy
			log.Printf("Work service following UI proxy=%s; refreshing relay controls", tunnel.ProxySpecForLog(proxy))
			controlCtx, stop := context.WithCancel(ctx)
			cancel = stop
			finished = make(chan error, 1)
			runCfg := cfg
			result := finished
			go func() { result <- runPools(controlCtx, ctx, runCfg, limiter) }()
		}
	}
}
