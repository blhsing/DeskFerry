//go:build windows

package main

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
	HTTPStreamOnly []string `json:"http_stream_only,omitempty"`
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
	for _, proxySpec := range saved.HTTPStreamOnly {
		tunnel.MarkProxyHTTPStreamOnly(proxySpec)
	}
	_, signature := currentProxyCapabilityState()
	proxyCapabilitiesState.Lock()
	proxyCapabilitiesState.signature = signature
	proxyCapabilitiesState.Unlock()
	return nil
}

func persistProxyCapabilitiesIfChanged() error {
	keys, signature := currentProxyCapabilityState()
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
	data, err := json.MarshalIndent(savedProxyCapabilities{HTTPStreamOnly: keys}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode proxy capabilities: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("write proxy capabilities: %w", err)
	}
	proxyCapabilitiesState.signature = signature
	return nil
}

func currentProxyCapabilityState() ([]string, string) {
	keys := tunnel.ProxyHTTPStreamOnlyKeys()
	sort.Strings(keys)
	return keys, strings.Join(keys, "\n")
}

func proxyCapabilitiesPath() (string, error) {
	settings, err := settingsPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(settings), "proxy-capabilities.json"), nil
}
