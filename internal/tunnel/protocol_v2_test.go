package tunnel

import (
	"errors"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestValidateSessionOffer(t *testing.T) {
	now := time.Now().UTC()
	offer := ControlMessage{
		Type:            MessageSessionOffer,
		SessionID:       "0123456789abcdef0123456789abcdef",
		Room:            "workdesk",
		Service:         ServiceWinRM,
		AgentID:         "agent-1",
		CreatedAt:       now,
		ExpiresAt:       now.Add(8 * time.Second),
		ProtocolVersion: ProtocolVersion2,
		Resumable:       true,
	}
	if err := ValidateSessionOffer(offer, "workdesk", "agent-1", now); err != nil {
		t.Fatal(err)
	}
	offer.ExpiresAt = now.Add(-time.Millisecond)
	if err := ValidateSessionOffer(offer, "workdesk", "agent-1", now); err == nil {
		t.Fatal("expired session offer was accepted")
	}
}

func TestTypedSessionResultsClassifyFallback(t *testing.T) {
	for _, result := range []string{MessageNoAgent, MessageBusy, MessageTimeout} {
		err := &SessionResultError{Result: result}
		if !err.RetryNextRelay() {
			t.Fatalf("%s should select the next relay", result)
		}
	}
	for _, result := range []string{MessageAuthFailed, MessageServiceDisabled, MessageInvalidRequest} {
		err := &SessionResultError{Result: result}
		if err.RetryNextRelay() {
			t.Fatalf("%s should be terminal", result)
		}
	}
}

func TestUnknownSessionCloseIsTerminal(t *testing.T) {
	err := websocket.CloseError{Code: websocket.StatusPolicyViolation, Reason: "unknown resumable session"}
	if !IsTerminalSessionError(err) {
		t.Fatal("unknown-session policy close was not terminal")
	}
	if IsTerminalSessionError(errors.New("temporary network reset")) {
		t.Fatal("temporary transport error was terminal")
	}
}
