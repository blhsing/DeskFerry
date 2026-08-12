package tunnel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestResumableWebSocketConnReplaysUnacknowledgedData(t *testing.T) {
	const sessionID = "0123456789abcdef0123456789abcdef"
	serverErrors := make(chan error, 2)
	var connections atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		index := connections.Add(1)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if index == 1 {
			if err := ws.Write(ctx, websocket.MessageText, []byte("start "+sessionID)); err != nil {
				serverErrors <- err
				return
			}
			if _, _, err := ws.Read(ctx); err != nil { // initial receive acknowledgement
				serverErrors <- err
				return
			}
			_, frame, err := ws.Read(ctx)
			if err != nil {
				serverErrors <- err
				return
			}
			frameType, offset, payload, err := parseFrame(frame)
			if err != nil || frameType != resumableFrameData || offset != 0 || string(payload) != "hello" {
				serverErrors <- formatTestFrameError(frameType, offset, payload, err)
				return
			}
			// A hosting layer may use a normal close code for a transport loss.
			// Without DeskFerry's logical-close marker, the stream must resume.
			_ = ws.Close(websocket.StatusNormalClosure, "")
			return
		}

		if got := r.Header.Get(HeaderSessionID); got != sessionID {
			serverErrors <- &testError{"resume session header", got, sessionID}
			return
		}
		if got := r.Header.Get(HeaderSessionSide); got != "client" {
			serverErrors <- &testError{"resume side header", got, "client"}
			return
		}
		if err := ws.Write(ctx, websocket.MessageText, []byte("resume "+sessionID)); err != nil {
			serverErrors <- err
			return
		}
		if _, _, err := ws.Read(ctx); err != nil { // receive acknowledgement after reattach
			serverErrors <- err
			return
		}
		_, frame, err := ws.Read(ctx)
		if err != nil {
			serverErrors <- err
			return
		}
		frameType, offset, payload, err := parseFrame(frame)
		if err != nil || frameType != resumableFrameData || offset != 0 || string(payload) != "hello" {
			serverErrors <- formatTestFrameError(frameType, offset, payload, err)
			return
		}
		if err := ws.Write(ctx, websocket.MessageBinary, makeFrame(resumableFrameAck, 5, nil)); err != nil {
			serverErrors <- err
			return
		}
		if err := ws.Write(ctx, websocket.MessageBinary, makeFrame(resumableFrameData, 0, []byte("world"))); err != nil {
			serverErrors <- err
			return
		}
		_, _, _ = ws.Read(ctx) // client acknowledgement or normal close
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	headers := http.Header{}
	headers.Set(HeaderResumable, "1")
	initial, err := DialWebSocketWithHeaders(ctx, server.URL+"/relay/unit", "direct", RoleClient, "", headers)
	if err != nil {
		t.Fatal(err)
	}
	gotSession, err := AwaitWebSocketStartSession(ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	if gotSession != sessionID {
		t.Fatalf("session ID = %q, want %q", gotSession, sessionID)
	}
	conn := NewResumableWebSocketConn(ctx, initial, ResumableWebSocketOptions{
		RelayAddr: server.URL + "/relay/unit",
		Proxy:     "direct",
		SessionID: sessionID,
		Side:      "client",
	})
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	received := make([]byte, len("world"))
	if _, err := io.ReadFull(conn, received); err != nil {
		t.Fatal(err)
	}
	if string(received) != "world" {
		t.Fatalf("received %q, want world", received)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-serverErrors:
		t.Fatal(err)
	default:
	}
}

func TestLogicalSessionCloseRequiresExplicitReason(t *testing.T) {
	if isLogicalSessionClose(websocket.CloseError{Code: websocket.StatusNormalClosure}) {
		t.Fatal("normal transport close without marker ended the logical session")
	}
	if !isLogicalSessionClose(websocket.CloseError{Code: websocket.StatusNormalClosure, Reason: "session closed"}) {
		t.Fatal("explicit logical session close was not recognized")
	}
}

type testError struct {
	field string
	got   string
	want  string
}

func (e *testError) Error() string {
	return e.field + " = " + e.got + ", want " + e.want
}

func formatTestFrameError(frameType byte, offset uint64, payload []byte, err error) error {
	if err != nil {
		return err
	}
	return &testError{
		field: "frame",
		got:   string([]byte{frameType}) + "/" + string(payload),
		want:  string([]byte{resumableFrameData}) + "/hello at offset 0",
	}
}

func TestParseFrameRejectsOversizedDataAndAckPayload(t *testing.T) {
	if _, _, _, err := parseFrame(makeFrame(resumableFrameData, 0, make([]byte, resumableChunkSize+1))); err == nil {
		t.Fatal("oversized data frame was accepted")
	}
	if _, _, _, err := parseFrame(makeFrame(resumableFrameAck, 0, []byte{1})); err == nil {
		t.Fatal("acknowledgement payload was accepted")
	}
}
