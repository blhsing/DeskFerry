package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

const (
	ProtocolVersion2 = 2

	HeaderProtocolVersion = "X-DeskFerry-Protocol"
	HeaderAgentInstance   = "X-DeskFerry-Agent-Instance"
	HeaderAgentServices   = "X-DeskFerry-Agent-Services"
	HeaderConcurrency     = "X-DeskFerry-Concurrency"

	MessageControlReady       = "control-ready"
	MessageSessionOffer       = "session-offer"
	MessageAccept             = "accept"
	MessageBusy               = "busy"
	MessageServiceDisabled    = "service-disabled"
	MessageUnsupportedVersion = "unsupported-version"
	MessageSessionReady       = "session-ready"
	MessageSessionClosed      = "session-closed"
	MessageNoAgent            = "no-agent"
	MessageAuthFailed         = "authentication-failed"
	MessageInvalidRequest     = "invalid-request"
	MessageTimeout            = "timeout"
)

// ControlMessage is the protocol-v2 control-channel envelope. Fields that do
// not apply to a particular Type are omitted on the wire.
type ControlMessage struct {
	Type            string    `json:"type"`
	SessionID       string    `json:"session_id,omitempty"`
	Room            string    `json:"room,omitempty"`
	Service         string    `json:"service,omitempty"`
	AgentID         string    `json:"agent_id,omitempty"`
	CreatedAt       time.Time `json:"created_at,omitempty"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	ProtocolVersion int       `json:"protocol_version,omitempty"`
	Resumable       bool      `json:"resumable,omitempty"`
	Reason          string    `json:"reason,omitempty"`
}

type SessionResultError struct {
	Result    string
	SessionID string
	Reason    string
}

func (e *SessionResultError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("relay session rejected: %s (%s)", e.Result, e.Reason)
	}
	return "relay session rejected: " + e.Result
}

func (e *SessionResultError) RetryNextRelay() bool {
	switch e.Result {
	case MessageNoAgent, MessageBusy, MessageTimeout:
		return true
	default:
		return false
	}
}

func AddProtocolV2Header(headers http.Header) {
	headers.Set(HeaderProtocolVersion, "2")
}

func WriteControlMessage(ctx context.Context, c *websocket.Conn, message ControlMessage) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, payload)
}

func ReadControlMessage(ctx context.Context, c *websocket.Conn) (ControlMessage, error) {
	for {
		typ, payload, err := c.Read(ctx)
		if err != nil {
			return ControlMessage{}, err
		}
		if typ != websocket.MessageText {
			continue
		}
		var message ControlMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			return ControlMessage{}, fmt.Errorf("decode relay control message: %w", err)
		}
		message.Type = strings.ToLower(strings.TrimSpace(message.Type))
		message.SessionID = strings.ToLower(strings.TrimSpace(message.SessionID))
		message.Service = strings.ToLower(strings.TrimSpace(message.Service))
		return message, nil
	}
}

func AwaitControlReady(ctx context.Context, c *websocket.Conn) error {
	message, err := ReadControlMessage(ctx, c)
	if err != nil {
		return fmt.Errorf("wait for control channel: %w", err)
	}
	if message.Type != MessageControlReady || message.ProtocolVersion != ProtocolVersion2 {
		return fmt.Errorf("relay returned unexpected control response %q", message.Type)
	}
	return nil
}

func AwaitSessionReady(ctx context.Context, c *websocket.Conn) (string, error) {
	sessionID, _, err := AwaitSessionReadyCompatible(ctx, c)
	return sessionID, err
}

// AwaitSessionReadyCompatible accepts a protocol-v2 result or the legacy
// "start" control frame. It allows upgraded Home clients to operate through a
// relay that is still paired with rollback-mode work-agent slots.
func AwaitSessionReadyCompatible(ctx context.Context, c *websocket.Conn) (string, bool, error) {
	return awaitSessionReadyCompatibleService(ctx, c, "")
}

// AwaitSessionReadyCompatibleService additionally requires a protocol-v2
// relay to echo the requested service. New services use this to avoid being
// silently mapped to RDP by an older relay.
func AwaitSessionReadyCompatibleService(ctx context.Context, c *websocket.Conn, expectedService string) (string, bool, error) {
	return awaitSessionReadyCompatibleService(ctx, c, strings.ToLower(strings.TrimSpace(expectedService)))
}

func awaitSessionReadyCompatibleService(ctx context.Context, c *websocket.Conn, expectedService string) (string, bool, error) {
	for {
		typ, payload, err := c.Read(ctx)
		if err != nil {
			return "", false, fmt.Errorf("wait for session result: %w", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		fields := strings.Fields(string(payload))
		if len(fields) > 0 && fields[0] == webSocketStartMessage {
			if expectedService != "" {
				return "", false, fmt.Errorf("relay does not confirm support for service %q", expectedService)
			}
			if len(fields) > 1 {
				return cleanProtocolSessionID(fields[1]), false, nil
			}
			return "", false, nil
		}
		var message ControlMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			return "", true, fmt.Errorf("decode relay session result: %w", err)
		}
		message.Type = strings.ToLower(strings.TrimSpace(message.Type))
		message.SessionID = strings.ToLower(strings.TrimSpace(message.SessionID))
		message.Service = strings.ToLower(strings.TrimSpace(message.Service))
		if message.Type == MessageSessionReady && expectedService != "" && message.Service != expectedService {
			return "", true, fmt.Errorf("relay confirmed service %q instead of %q", message.Service, expectedService)
		}
		return sessionResult(message)
	}
}

func sessionResult(message ControlMessage) (string, bool, error) {
	if message.Type == MessageSessionReady && cleanProtocolSessionID(message.SessionID) != "" {
		return message.SessionID, true, nil
	}
	if message.Type == "" {
		message.Type = MessageInvalidRequest
	}
	return "", true, &SessionResultError{Result: message.Type, SessionID: message.SessionID, Reason: message.Reason}
}

func ValidateSessionOffer(message ControlMessage, room, agentID string, now time.Time) error {
	if message.Type != MessageSessionOffer {
		return fmt.Errorf("unexpected control message %q", message.Type)
	}
	if cleanProtocolSessionID(message.SessionID) == "" {
		return errors.New("session offer has an invalid session ID")
	}
	if message.ProtocolVersion != ProtocolVersion2 {
		return fmt.Errorf("unsupported protocol version %d", message.ProtocolVersion)
	}
	if strings.ToLower(strings.TrimSpace(message.Room)) != strings.ToLower(strings.TrimSpace(room)) {
		return errors.New("session offer room does not match the control channel")
	}
	if message.AgentID != "" && message.AgentID != agentID {
		return errors.New("session offer agent identity does not match")
	}
	if message.Service != ServiceRDP && message.Service != ServiceWinRM && message.Service != ServiceSMB && message.Service != ServiceScreen {
		return fmt.Errorf("session offer has unsupported service %q", message.Service)
	}
	if message.ExpiresAt.IsZero() || !now.Before(message.ExpiresAt) {
		return errors.New("session offer expired")
	}
	if message.CreatedAt.After(now.Add(30 * time.Second)) {
		return errors.New("session offer creation time is in the future")
	}
	return nil
}

func cleanProtocolSessionID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) != 32 {
		return ""
	}
	for _, ch := range value {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return ""
		}
	}
	return strings.ToLower(value)
}

// IsTerminalSessionError identifies failures that cannot be repaired by
// reconnecting the same session ID. This prevents authentication and unknown-
// session rejection storms after the relay has already discarded a session.
func IsTerminalSessionError(err error) bool {
	if err == nil {
		return false
	}
	status := websocket.CloseStatus(err)
	if status == websocket.StatusPolicyViolation || status == websocket.StatusUnsupportedData {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "unknown resumable session") ||
		strings.Contains(text, "authentication failed") ||
		strings.Contains(text, "room authentication failed") ||
		strings.Contains(text, "http 401") || strings.Contains(text, "http 403")
}
