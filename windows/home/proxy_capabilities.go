//go:build windows

package homewindows

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"deskferry/internal/tunnel"
)

type savedProxyCapabilities struct {
	HTTPStreamPreferred  []string `json:"http_stream_preferred,omitempty"`
	CONNECTUnsupported   []string `json:"connect_unsupported,omitempty"`
	LegacyHTTPStreamOnly []string `json:"http_stream_only,omitempty"`
}

var proxyCapabilitiesState struct {
	sync.Mutex
	signature string
}

func loadProxyCapabilities() error {
	path, err := proxyCapabilitiesPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		proxyCapabilitiesState.Lock()
		proxyCapabilitiesState.signature = ""
		proxyCapabilitiesState.Unlock()
		return nil
	}
	if err != nil {
		return fmt.Errorf("read proxy capabilities: %w", err)
	}
	var saved savedProxyCapabilities
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("decode proxy capabilities: %w", err)
	}
	for _, proxySpec := range append(saved.HTTPStreamPreferred, saved.LegacyHTTPStreamOnly...) {
		tunnel.MarkProxyHTTPStreamPreferred(proxySpec)
	}
	for _, proxySpec := range saved.CONNECTUnsupported {
		tunnel.MarkProxyCONNECTUnsupported(proxySpec)
	}
	_, signature := currentProxyCapabilityState()
	if len(saved.LegacyHTTPStreamOnly) > 0 {
		// v0.11.3 conflated a failed WebSocket upgrade with rejected CONNECT.
		// Force a rewrite using the split capability model after migration.
		signature = "legacy:" + signature
	}
	proxyCapabilitiesState.Lock()
	proxyCapabilitiesState.signature = signature
	proxyCapabilitiesState.Unlock()
	return nil
}

func persistProxyCapabilitiesIfChanged() error {
	saved, signature := currentProxyCapabilityState()
	proxyCapabilitiesState.Lock()
	defer proxyCapabilitiesState.Unlock()
	if signature == proxyCapabilitiesState.signature {
		return nil
	}
	path, err := proxyCapabilitiesPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create proxy capability directory: %w", err)
	}
	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return fmt.Errorf("encode proxy capabilities: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("write proxy capabilities: %w", err)
	}
	proxyCapabilitiesState.signature = signature
	return nil
}

func currentProxyCapabilityState() (savedProxyCapabilities, string) {
	preferred := tunnel.ProxyHTTPStreamPreferredKeys()
	connectUnsupported := tunnel.ProxyCONNECTUnsupportedKeys()
	sort.Strings(preferred)
	sort.Strings(connectUnsupported)
	saved := savedProxyCapabilities{
		HTTPStreamPreferred: preferred,
		CONNECTUnsupported:  connectUnsupported,
	}
	signature := "http-stream-preferred:\n" + strings.Join(preferred, "\n") +
		"\nconnect-unsupported:\n" + strings.Join(connectUnsupported, "\n")
	return saved, signature
}

func proxyCapabilitiesPath() (string, error) {
	settings, err := settingsPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(settings), "proxy-capabilities.json"), nil
}
