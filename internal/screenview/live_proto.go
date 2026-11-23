package screenview

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
)

// ProtocolLive is the screen protocol number of the live session.
const ProtocolLive = 2

// Secure record types. See docs/screen-live-protocol.md.
const (
	recFinish     byte = 0x01
	recAck        byte = 0x02
	recFrameStart byte = 0x10
	recRects      byte = 0x11
	recFrameEnd   byte = 0x12
	recCursor     byte = 0x13
	recJSON       byte = 0x20
	recPing       byte = 0x7E
	recPong       byte = 0x7F
)

// Rect encodings.
const (
	EncSolid byte = 0
	EncPNG   byte = 1
	EncJPEG  byte = 2
	// EncJPEGScaled is a JPEG of the rect at reduced resolution; the client
	// scales it up to the rect's size.
	EncJPEGScaled byte = 3
)

// FeatureScaledJPEG is the request feature for EncJPEGScaled rects.
const FeatureScaledJPEG = "scaled-jpeg"

const (
	frameFlagKey      byte = 1
	frameEndLossless  byte = 1
	frameEndSettled   byte = 2
	rectHeaderSize         = 13
	maxRectsPerRecord      = 4096
	targetRecordBytes      = 256 << 10
)

// Hello is the Work agent's answer to a protocol 2 request.
type Hello struct {
	ServerHello    string   `json:"server_hello"`
	Version        string   `json:"version,omitempty"`
	Width          int      `json:"width"`
	Height         int      `json:"height"`
	ControlAllowed bool     `json:"control_allowed"`
	Capabilities   []string `json:"capabilities,omitempty"`
}

// EncodedRect is one encoded framebuffer region.
type EncodedRect struct {
	X, Y, W, H int
	Enc        byte
	Data       []byte
}

// InputEvent is one injected input event.
type InputEvent struct {
	T    string `json:"t"`
	X    *int   `json:"x,omitempty"`
	Y    *int   `json:"y,omitempty"`
	B    string `json:"b,omitempty"`
	Down bool   `json:"down,omitempty"`
	DX   int    `json:"dx,omitempty"`
	DY   int    `json:"dy,omitempty"`
	VK   int    `json:"vk,omitempty"`
	Scan int    `json:"scan,omitempty"`
	Ext  bool   `json:"ext,omitempty"`
	C    string `json:"c,omitempty"`
	S    string `json:"s,omitempty"`
}

// Point returns the event coordinates when both are present.
func (e InputEvent) Point() (int, int, bool) {
	if e.X == nil || e.Y == nil {
		return 0, 0, false
	}
	return *e.X, *e.Y, true
}

// At returns a pointer to v, for InputEvent coordinates.
func At(v int) *int { return &v }

// Command is a client-to-Work JSON command.
type Command struct {
	ID           uint64       `json:"id,omitempty"`
	Cmd          string       `json:"cmd"`
	Enable       bool         `json:"enable,omitempty"`
	YieldToHuman bool         `json:"yield_to_human,omitempty"`
	Events       []InputEvent `json:"events,omitempty"`
	Keyframe     bool         `json:"keyframe,omitempty"`
	QuietMS      int          `json:"quiet_ms,omitempty"`
	TimeoutMS    int          `json:"timeout_ms,omitempty"`
	Text         string       `json:"text,omitempty"`
	Depth        int          `json:"depth,omitempty"`
	MaxNodes     int          `json:"max_nodes,omitempty"`
	Max          int          `json:"max,omitempty"`
	Scope        string       `json:"scope,omitempty"`
	MaxFPS       int          `json:"max_fps,omitempty"`
	JPEGQuality  int          `json:"jpeg_quality,omitempty"`
}

// Status describes the shared screen and this session's control state.
type Status struct {
	Width           int    `json:"width"`
	Height          int    `json:"height"`
	ControlAllowed  bool   `json:"control_allowed"`
	ControlActive   bool   `json:"control_active"`
	YieldToHuman    bool   `json:"yield_to_human"`
	HumanInputMSAgo int64  `json:"human_input_ms_ago"`
	Desktop         string `json:"desktop,omitempty"`
	Locked          bool   `json:"locked"`
	ForegroundTitle string `json:"foreground_title,omitempty"`
	Cursor          [2]int `json:"cursor"`
	Capture         string `json:"capture,omitempty"`
	// Compression describes the current compression level.
	Compression string `json:"compression,omitempty"`
	Viewers     int    `json:"viewers,omitempty"`
}

// Message is a Work-to-client JSON message.
type Message struct {
	Type   string          `json:"type"`
	ID     uint64          `json:"id,omitempty"`
	OK     bool            `json:"ok,omitempty"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	*Status
}

// UINode is one UI Automation element.
type UINode struct {
	Name         string `json:"name,omitempty"`
	Role         string `json:"role,omitempty"`
	Value        string `json:"value,omitempty"`
	AutomationID string `json:"automation_id,omitempty"`
	Class        string `json:"class,omitempty"`
	Enabled      bool   `json:"enabled"`
	Focused      bool   `json:"focused,omitempty"`
	Rect         [4]int `json:"rect"`
	Depth        int    `json:"depth"`
}

// SettleResult answers a settle command.
type SettleResult struct {
	Seq     uint64   `json:"seq"`
	Settled bool     `json:"settled"`
	Changed [][4]int `json:"changed,omitempty"`
}

// CommandError is an error reply with a machine-readable code.
type CommandError struct {
	Code    string
	Message string
}

func (e *CommandError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

func commandError(code, format string, args ...any) error {
	return &CommandError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrorCode returns the code of a command error, or "failed".
func ErrorCode(err error) string {
	var commandErr *CommandError
	if errors.As(err, &commandErr) {
		return commandErr.Code
	}
	return "failed"
}

func parseReplyError(text string) error {
	for index := 0; index < len(text); index++ {
		if text[index] == ':' {
			return &CommandError{Code: text[:index], Message: trimSpace(text[index+1:])}
		}
	}
	return &CommandError{Code: text}
}

func trimSpace(value string) string {
	start, end := 0, len(value)
	for start < end && (value[start] == ' ' || value[start] == '\t') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\t') {
		end--
	}
	return value[start:end]
}

func encodeFrameStart(seq uint64, width, height int, flags byte) []byte {
	out := make([]byte, 17)
	binary.BigEndian.PutUint64(out[0:], seq)
	binary.BigEndian.PutUint32(out[8:], uint32(width))
	binary.BigEndian.PutUint32(out[12:], uint32(height))
	out[16] = flags
	return out
}

func decodeFrameStart(payload []byte) (uint64, int, int, byte, error) {
	if len(payload) != 17 {
		return 0, 0, 0, 0, errors.New("invalid frame start record")
	}
	width := int(binary.BigEndian.Uint32(payload[8:]))
	height := int(binary.BigEndian.Uint32(payload[12:]))
	if width <= 0 || height <= 0 || width > 16384 || height > 16384 {
		return 0, 0, 0, 0, errors.New("invalid frame size")
	}
	return binary.BigEndian.Uint64(payload), width, height, payload[16], nil
}

func encodeFrameEnd(seq uint64, flags byte) []byte {
	out := make([]byte, 9)
	binary.BigEndian.PutUint64(out, seq)
	out[8] = flags
	return out
}

func decodeFrameEnd(payload []byte) (uint64, byte, error) {
	if len(payload) != 9 {
		return 0, 0, errors.New("invalid frame end record")
	}
	return binary.BigEndian.Uint64(payload), payload[8], nil
}

func encodeSeq(seq uint64) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, seq)
	return out
}

// packRects groups encoded rects into record payloads no larger than about
// targetRecordBytes, except that a single large rect gets a record of its own.
func packRects(rects []EncodedRect) [][]byte {
	var records [][]byte
	var current []byte
	count := 0
	flush := func() {
		if count == 0 {
			return
		}
		binary.BigEndian.PutUint16(current[:2], uint16(count))
		records = append(records, current)
		current = nil
		count = 0
	}
	for _, rect := range rects {
		size := rectHeaderSize + len(rect.Data)
		if count > 0 && (len(current)+size > targetRecordBytes || count == maxRectsPerRecord) {
			flush()
		}
		if current == nil {
			current = make([]byte, 2, min(targetRecordBytes, 2+size)+64)
		}
		var header [rectHeaderSize]byte
		binary.BigEndian.PutUint16(header[0:], uint16(rect.X))
		binary.BigEndian.PutUint16(header[2:], uint16(rect.Y))
		binary.BigEndian.PutUint16(header[4:], uint16(rect.W))
		binary.BigEndian.PutUint16(header[6:], uint16(rect.H))
		header[8] = rect.Enc
		binary.BigEndian.PutUint32(header[9:], uint32(len(rect.Data)))
		current = append(current, header[:]...)
		current = append(current, rect.Data...)
		count++
	}
	flush()
	return records
}

func unpackRects(payload []byte) ([]EncodedRect, error) {
	if len(payload) < 2 {
		return nil, errors.New("invalid rects record")
	}
	count := int(binary.BigEndian.Uint16(payload))
	payload = payload[2:]
	rects := make([]EncodedRect, 0, count)
	for index := 0; index < count; index++ {
		if len(payload) < rectHeaderSize {
			return nil, errors.New("truncated rects record")
		}
		rect := EncodedRect{
			X:   int(binary.BigEndian.Uint16(payload[0:])),
			Y:   int(binary.BigEndian.Uint16(payload[2:])),
			W:   int(binary.BigEndian.Uint16(payload[4:])),
			H:   int(binary.BigEndian.Uint16(payload[6:])),
			Enc: payload[8],
		}
		size := int(binary.BigEndian.Uint32(payload[9:]))
		payload = payload[rectHeaderSize:]
		if size > len(payload) || rect.W <= 0 || rect.H <= 0 {
			return nil, errors.New("invalid rect in rects record")
		}
		rect.Data = payload[:size]
		payload = payload[size:]
		rects = append(rects, rect)
	}
	return rects, nil
}

// CursorState is the Work PC's pointer.
type CursorState struct {
	X, Y    int
	Visible bool
	ShapeID uint32
	HotX    int
	HotY    int
	// Shape is the RGBA cursor image; nil when unknown.
	Shape *image.RGBA
}

func encodeCursor(cursor CursorState, shapePNG []byte) []byte {
	out := make([]byte, 21, 21+len(shapePNG))
	binary.BigEndian.PutUint32(out[0:], uint32(int32(cursor.X)))
	binary.BigEndian.PutUint32(out[4:], uint32(int32(cursor.Y)))
	if cursor.Visible {
		out[8] = 1
	}
	binary.BigEndian.PutUint32(out[9:], cursor.ShapeID)
	binary.BigEndian.PutUint16(out[13:], uint16(cursor.HotX))
	binary.BigEndian.PutUint16(out[15:], uint16(cursor.HotY))
	binary.BigEndian.PutUint32(out[17:], uint32(len(shapePNG)))
	return append(out, shapePNG...)
}

type cursorRecord struct {
	X, Y       int
	Visible    bool
	ShapeID    uint32
	HotX, HotY int
	PNG        []byte
}

func decodeCursor(payload []byte) (cursorRecord, error) {
	if len(payload) < 21 {
		return cursorRecord{}, errors.New("invalid cursor record")
	}
	record := cursorRecord{
		X:       int(int32(binary.BigEndian.Uint32(payload[0:]))),
		Y:       int(int32(binary.BigEndian.Uint32(payload[4:]))),
		Visible: payload[8] != 0,
		ShapeID: binary.BigEndian.Uint32(payload[9:]),
		HotX:    int(binary.BigEndian.Uint16(payload[13:])),
		HotY:    int(binary.BigEndian.Uint16(payload[15:])),
	}
	size := int(binary.BigEndian.Uint32(payload[17:]))
	if size != len(payload)-21 {
		return cursorRecord{}, errors.New("invalid cursor shape length")
	}
	record.PNG = payload[21:]
	return record, nil
}

// readLine reads one '\n'-terminated line a byte at a time, so no bytes after
// the line are consumed from the underlying stream.
func readLine(r io.Reader, limit int) ([]byte, error) {
	if br, ok := r.(*bufio.Reader); ok {
		line, err := br.ReadSlice('\n')
		if err != nil {
			return nil, err
		}
		return append([]byte(nil), line...), nil
	}
	var line []byte
	var one [1]byte
	for len(line) < limit {
		if _, err := io.ReadFull(r, one[:]); err != nil {
			return nil, err
		}
		line = append(line, one[0])
		if one[0] == '\n' {
			return line, nil
		}
	}
	return nil, errors.New("line is too long")
}
