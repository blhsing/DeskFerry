package homenetwork

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	DefaultAlias            = "deskferry-work"
	DefaultInterfaceName    = "DeskFerry"
	DefaultInterfaceAddress = "198.18.0.1/30"
	DefaultRemoteAddress    = "198.18.0.2"
	DefaultSOCKSAddress     = "127.0.0.1:10839"
)

var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

type Config struct {
	RelayAddrs       []string `json:"relay_addrs"`
	Proxy            string   `json:"proxy,omitempty"`
	RoomProof        string   `json:"room_proof"`
	Alias            string   `json:"alias"`
	InterfaceName    string   `json:"interface_name"`
	InterfaceAddress string   `json:"interface_address"`
	RemoteAddress    string   `json:"remote_address"`
	SOCKSAddress     string   `json:"socks_address"`
	Tun2SocksPath    string   `json:"tun2socks_path"`
}

func (c Config) WithDefaults(baseDir string) Config {
	c.RelayAddrs = cleanRelayURLs(c.RelayAddrs)
	c.Proxy = strings.TrimSpace(c.Proxy)
	c.RoomProof = strings.TrimSpace(c.RoomProof)
	c.Alias = strings.TrimSpace(c.Alias)
	if c.Alias == "" {
		c.Alias = DefaultAlias
	}
	c.InterfaceName = strings.TrimSpace(c.InterfaceName)
	if c.InterfaceName == "" {
		c.InterfaceName = DefaultInterfaceName
	}
	c.InterfaceAddress = strings.TrimSpace(c.InterfaceAddress)
	if c.InterfaceAddress == "" {
		c.InterfaceAddress = DefaultInterfaceAddress
	}
	c.RemoteAddress = strings.TrimSpace(c.RemoteAddress)
	if c.RemoteAddress == "" {
		c.RemoteAddress = DefaultRemoteAddress
	}
	c.SOCKSAddress = strings.TrimSpace(c.SOCKSAddress)
	if c.SOCKSAddress == "" {
		c.SOCKSAddress = DefaultSOCKSAddress
	}
	c.Tun2SocksPath = strings.TrimSpace(c.Tun2SocksPath)
	if c.Tun2SocksPath == "" && baseDir != "" {
		c.Tun2SocksPath = filepath.Join(baseDir, "tun2socks.exe")
	}
	return c
}

func (c Config) Validate() error {
	if len(c.RelayAddrs) == 0 {
		return errors.New("at least one relay room URL is required")
	}
	var room string
	for _, relayAddr := range c.RelayAddrs {
		u, err := url.Parse(relayAddr)
		if err != nil || u.Host == "" {
			return fmt.Errorf("invalid relay URL %q", relayAddr)
		}
		if u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "wss" && u.Scheme != "ws" {
			return fmt.Errorf("relay URL %q must use http, https, ws, or wss", relayAddr)
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 2 || !strings.EqualFold(parts[0], "relay") || strings.TrimSpace(parts[1]) == "" {
			return fmt.Errorf("relay URL %q must name a room under /relay/<room>", relayAddr)
		}
		if room == "" {
			room = strings.ToLower(parts[1])
		} else if !strings.EqualFold(room, parts[1]) {
			return errors.New("all relay URLs must use the same room name")
		}
	}
	if c.RoomProof == "" {
		return errors.New("SMB file access requires a room password")
	}
	if !aliasPattern.MatchString(c.Alias) {
		return errors.New("work computer alias must be a single DNS label containing only letters, numbers, and hyphens")
	}
	interfaceIP, subnet, err := net.ParseCIDR(c.InterfaceAddress)
	if err != nil || interfaceIP.To4() == nil {
		return errors.New("virtual adapter address must be an IPv4 CIDR address")
	}
	remoteIP := net.ParseIP(c.RemoteAddress)
	if remoteIP == nil || remoteIP.To4() == nil {
		return errors.New("work computer virtual address must be an IPv4 address")
	}
	if !subnet.Contains(remoteIP) || interfaceIP.Equal(remoteIP) {
		return errors.New("work computer virtual address must be a different address in the virtual adapter subnet")
	}
	host, port, err := net.SplitHostPort(c.SOCKSAddress)
	if err != nil || port == "" || (host != "127.0.0.1" && host != "::1" && !strings.EqualFold(host, "localhost")) {
		return errors.New("the internal SOCKS listener must be a loopback host:port")
	}
	if c.Tun2SocksPath == "" {
		return errors.New("tun2socks executable path is required")
	}
	return nil
}

func cleanRelayURLs(values []string) []string {
	seen := make(map[string]bool)
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		for _, item := range strings.FieldsFunc(value, func(r rune) bool { return r == ';' || r == '\r' || r == '\n' }) {
			item = strings.TrimSpace(item)
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			cleaned = append(cleaned, item)
		}
	}
	return cleaned
}
