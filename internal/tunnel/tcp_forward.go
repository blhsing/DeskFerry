package tunnel

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"unicode"
)

// TCP port forwards let a Home app listen on a loopback port and carry each
// accepted connection through the relay to a target host and port that the
// Work agent's owner configured. Each Work forward is identified by a forward
// port number and advertised as its own protocol-v2 service, "tcp-<forward
// port>". The relay only pairs the service; the target stays in the Work
// agent's configuration, so a Home client cannot choose arbitrary targets.

const tcpForwardServicePrefix = "tcp-"

// TCPForward maps a Work forward port number to a target reachable from the
// Work agent.
type TCPForward struct {
	ForwardPort int
	Target      string
}

// Service returns the protocol-v2 service name for the forward.
func (f TCPForward) Service() string {
	return TCPForwardService(f.ForwardPort)
}

// String formats the forward as "forwardPort=host:port".
func (f TCPForward) String() string {
	return strconv.Itoa(f.ForwardPort) + "=" + f.Target
}

// TCPForwardService returns the service name advertised for a forward port.
func TCPForwardService(port int) string {
	return tcpForwardServicePrefix + strconv.Itoa(port)
}

// ParseTCPForwardService reports the forward port of a "tcp-<port>" service.
func ParseTCPForwardService(service string) (int, bool) {
	service = strings.ToLower(strings.TrimSpace(service))
	if !strings.HasPrefix(service, tcpForwardServicePrefix) {
		return 0, false
	}
	digits := strings.TrimPrefix(service, tcpForwardServicePrefix)
	if digits == "" || len(digits) > 5 || strings.TrimLeft(digits, "0123456789") != "" || digits[0] == '0' {
		return 0, false
	}
	port, err := strconv.Atoi(digits)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

// IsKnownService reports whether service is a built-in DeskFerry service or a
// well-formed TCP forward service.
func IsKnownService(service string) bool {
	switch service {
	case ServiceRDP, ServiceWinRM, ServiceSMB, ServiceScreen:
		return true
	}
	_, ok := ParseTCPForwardService(service)
	return ok
}

// ParseTCPForwards parses a list of "forwardPort=host:port" entries separated by
// semicolons, commas, or newlines. Relay ports must be unique.
func ParseTCPForwards(value string) ([]TCPForward, error) {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ';' || r == ',' || r == '\r' || r == '\n'
	})
	out := make([]TCPForward, 0, len(parts))
	seen := make(map[int]bool, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		forward, err := ParseTCPForward(part)
		if err != nil {
			return nil, err
		}
		if seen[forward.ForwardPort] {
			return nil, fmt.Errorf("forward port %d is forwarded more than once", forward.ForwardPort)
		}
		seen[forward.ForwardPort] = true
		out = append(out, forward)
	}
	return out, nil
}

// ParseTCPForward parses one "forwardPort=host:port" entry.
func ParseTCPForward(value string) (TCPForward, error) {
	left, right, ok := strings.Cut(strings.TrimSpace(value), "=")
	if !ok {
		return TCPForward{}, fmt.Errorf("TCP forward %q must use forwardPort=host:port", value)
	}
	port, err := ParseTCPPort(left)
	if err != nil {
		return TCPForward{}, fmt.Errorf("TCP forward %q has an invalid forward port: %w", value, err)
	}
	target, err := NormalizeTCPTarget(right)
	if err != nil {
		return TCPForward{}, fmt.Errorf("TCP forward %q has an invalid target: %w", value, err)
	}
	return TCPForward{ForwardPort: port, Target: target}, nil
}

// ParseTCPPort parses a TCP port number between 1 and 65535.
func ParseTCPPort(value string) (int, error) {
	port, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("port must be a number from 1 to 65535")
	}
	return port, nil
}

// NormalizeTCPTarget validates a host:port target and returns it trimmed.
func NormalizeTCPTarget(value string) (string, error) {
	value = strings.TrimSpace(value)
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", fmt.Errorf("target must be host:port")
	}
	if strings.TrimSpace(host) == "" || strings.ContainsAny(host, " \t;,=") {
		return "", fmt.Errorf("target host is invalid")
	}
	if _, err := ParseTCPPort(port); err != nil {
		return "", fmt.Errorf("target %w", err)
	}
	return net.JoinHostPort(host, port), nil
}

// FormatTCPForwards formats forwards as a semicolon-separated list.
func FormatTCPForwards(forwards []TCPForward) string {
	parts := make([]string, 0, len(forwards))
	for _, forward := range forwards {
		parts = append(parts, forward.String())
	}
	return strings.Join(parts, ";")
}

// TCPForwardListener is a Home-side loopback listener that carries each
// accepted connection to a Work agent forward.
type TCPForwardListener struct {
	LocalPort   int
	ForwardPort int
}

// Service returns the Work agent forward service the listener requests.
func (l TCPForwardListener) Service() string {
	return TCPForwardService(l.ForwardPort)
}

// String formats the listener as "localPort=forwardPort".
func (l TCPForwardListener) String() string {
	return strconv.Itoa(l.LocalPort) + "=" + strconv.Itoa(l.ForwardPort)
}

// ParseTCPForwardListeners parses Home-side listeners written as
// "localPort=forwardPort" or a bare "forwardPort" (listen on the same local
// port), separated by semicolons, commas, whitespace, or newlines. Local ports
// must be unique.
func ParseTCPForwardListeners(value string) ([]TCPForwardListener, error) {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ';' || r == ',' || unicode.IsSpace(r)
	})
	out := make([]TCPForwardListener, 0, len(parts))
	seen := make(map[int]bool, len(parts))
	for _, part := range parts {
		local, forward, hasForward := strings.Cut(part, "=")
		localPort, err := ParseTCPPort(local)
		if err != nil {
			return nil, fmt.Errorf("TCP forward listener %q has an invalid local port: %w", part, err)
		}
		forwardPort := localPort
		if hasForward {
			if forwardPort, err = ParseTCPPort(forward); err != nil {
				return nil, fmt.Errorf("TCP forward listener %q has an invalid forward port: %w", part, err)
			}
		}
		if seen[localPort] {
			return nil, fmt.Errorf("local port %d is used by more than one TCP forward", localPort)
		}
		seen[localPort] = true
		out = append(out, TCPForwardListener{LocalPort: localPort, ForwardPort: forwardPort})
	}
	return out, nil
}

// FormatTCPForwardListeners formats listeners as a semicolon-separated list.
func FormatTCPForwardListeners(listeners []TCPForwardListener) string {
	parts := make([]string, 0, len(listeners))
	for _, listener := range listeners {
		parts = append(parts, listener.String())
	}
	return strings.Join(parts, ";")
}
