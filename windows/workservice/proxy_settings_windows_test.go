//go:build windows

package workservice

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"deskferry/internal/uiproxy"
)

func TestProxyWatcherHandlesCreationReplacementAndCancellation(t *testing.T) {
	dir := t.TempDir()
	path := uiproxy.Path(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := make(chan struct{}, 1)
	finished := make(chan error, 1)
	go func() { finished <- watchProxySettings(ctx, path, changes) }()
	waitChange := func() {
		t.Helper()
		select {
		case <-changes:
		case err := <-finished:
			t.Fatalf("watcher stopped: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("no native settings change notification")
		}
	}
	waitChange()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"proxy":"direct"}`), 0600); err != nil {
		t.Fatal(err)
	}
	waitChange()
	for len(changes) > 0 {
		<-changes
	}
	stage := filepath.Join(filepath.Dir(path), "new-settings.json")
	if err := os.WriteFile(stage, []byte(`{"proxy":"http://192.9.200.22:9090"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stage, path); err != nil {
		t.Fatal(err)
	}
	waitChange()
	if proxy, found, err := uiproxy.Read(path); err != nil || !found || proxy != "http://192.9.200.22:9090" {
		t.Fatalf("proxy=%q found=%t error=%v", proxy, found, err)
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("native watcher did not cancel")
	}
}

func TestProxyChangePreservesSessionsAndConcurrencyLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan string)
	type started struct {
		proxy            string
		control, session context.Context
		limiter          chan struct{}
	}
	starts := make(chan started, 2)
	finished := make(chan error, 1)
	go func() {
		finished <- runProxyUpdates(ctx, config{ConcurrencyLimit: 1}, updates, func(control, session context.Context, cfg config, limiter chan struct{}) error {
			starts <- started{cfg.Proxy, control, session, limiter}
			<-control.Done()
			return nil
		})
	}()
	receive := func() started {
		t.Helper()
		select {
		case value := <-starts:
			return value
		case <-time.After(5 * time.Second):
			t.Fatal("proxy controls did not start")
			return started{}
		}
	}
	updates <- "http://old:3128"
	first := receive()
	first.limiter <- struct{}{} // An established session occupies the only slot.
	updates <- "http://192.9.200.22:9090"
	second := receive()
	if second.proxy != "http://192.9.200.22:9090" {
		t.Fatalf("new control proxy = %q", second.proxy)
	}
	if first.control.Err() == nil {
		t.Fatal("old control was not cancelled")
	}
	if first.session.Err() != nil || second.session != first.session {
		t.Fatal("proxy change cancelled established data sessions")
	}
	if first.limiter != second.limiter {
		t.Fatal("proxy change reset the session concurrency limit")
	}
	select {
	case second.limiter <- struct{}{}:
		t.Fatal("proxy change allowed excess concurrent sessions")
	default:
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("controls did not stop")
	}
	if first.session.Err() == nil {
		t.Fatal("service shutdown did not cancel data sessions")
	}
}
