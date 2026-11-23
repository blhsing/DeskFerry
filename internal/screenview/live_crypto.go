package screenview

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"deskferry/internal/tunnel"
)

const (
	clientHelloSize = 65 + 32
	serverHelloSize = 65 + 32 + 32
	maxRecordCipher = 2 << 20
	maxRecordPlain  = 1 << 20
)

// ErrRoomPasswordMismatch reports that the Work agent proved a different room
// password than the one this client derived its screen key from.
var ErrRoomPasswordMismatch = errors.New("room password mismatch: re-enter the room password for this destination")

// ScreenKeyFromProof derives the live screen pre-shared key from a room
// proof, the credential Home apps already save, so no extra secret has to be
// entered. Anyone who holds the proof (which the relays see when they pair a
// room) can derive this key too: the encryption protects against parties who
// cannot authenticate to the room, not against the relays.
func ScreenKeyFromProof(proof string) string {
	proof = strings.TrimSpace(proof)
	if proof == "" {
		return ""
	}
	key, err := hkdf.Key(sha256.New, []byte(proof), []byte("DeskFerry screen key v2\x00"), "DeskFerry live screen PSK", 32)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(key)
}

// DeriveScreenKey derives the screen key from a room name and password.
func DeriveScreenKey(room, password string) string {
	if password == "" {
		return ""
	}
	return ScreenKeyFromProof(tunnel.RoomPasswordProof("", strings.TrimSpace(room), password))
}

// DeriveScreenKeyForRelay derives the screen key for the room named by a relay
// room URL.
func DeriveScreenKeyForRelay(relayAddr, password string) string {
	if password == "" {
		return ""
	}
	return ScreenKeyFromProof(tunnel.RoomPasswordProof(relayAddr, "", password))
}

func decodeScreenKey(value string) ([]byte, error) {
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(key) != 32 {
		return nil, errors.New("invalid screen key")
	}
	return key, nil
}

type clientHandshake struct {
	psk   []byte
	priv  *ecdh.PrivateKey
	hello []byte
}

func newClientHandshake(screenKey string) (*clientHandshake, error) {
	psk, err := decodeScreenKey(screenKey)
	if err != nil {
		return nil, err
	}
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	hello := make([]byte, 0, clientHelloSize)
	hello = append(hello, priv.PublicKey().Bytes()...)
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	hello = append(hello, nonce...)
	return &clientHandshake{psk: psk, priv: priv, hello: hello}, nil
}

func handshakeTranscript(clientHello, serverPubNonce []byte) []byte {
	h := sha256.New()
	h.Write([]byte("DeskFerry live v2\x00"))
	h.Write(clientHello)
	h.Write(serverPubNonce)
	return h.Sum(nil)
}

func handshakeMAC(psk []byte, label string, transcript []byte) []byte {
	mac := hmac.New(sha256.New, psk)
	mac.Write([]byte(label))
	mac.Write(transcript)
	return mac.Sum(nil)
}

type sessionKeys struct {
	c2s []byte
	s2c []byte
}

func deriveSessionKeys(shared, psk, transcript []byte) (sessionKeys, error) {
	ikm := append(append([]byte(nil), shared...), psk...)
	prk, err := hkdf.Extract(sha256.New, ikm, transcript)
	if err != nil {
		return sessionKeys{}, err
	}
	c2s, err := hkdf.Expand(sha256.New, prk, "DeskFerry c2s", 32)
	if err != nil {
		return sessionKeys{}, err
	}
	s2c, err := hkdf.Expand(sha256.New, prk, "DeskFerry s2c", 32)
	if err != nil {
		return sessionKeys{}, err
	}
	return sessionKeys{c2s: c2s, s2c: s2c}, nil
}

// finish verifies the server hello and returns the session keys and the
// client finish MAC.
func (h *clientHandshake) finish(serverHello []byte) (sessionKeys, []byte, error) {
	if len(serverHello) != serverHelloSize {
		return sessionKeys{}, nil, errors.New("invalid live screen server hello")
	}
	pubNonce := serverHello[:97]
	transcript := handshakeTranscript(h.hello, pubNonce)
	if !hmac.Equal(serverHello[97:], handshakeMAC(h.psk, "server finished\x00", transcript)) {
		return sessionKeys{}, nil, ErrRoomPasswordMismatch
	}
	serverPub, err := ecdh.P256().NewPublicKey(serverHello[:65])
	if err != nil {
		return sessionKeys{}, nil, fmt.Errorf("invalid live screen server key: %w", err)
	}
	shared, err := h.priv.ECDH(serverPub)
	if err != nil {
		return sessionKeys{}, nil, err
	}
	keys, err := deriveSessionKeys(shared, h.psk, transcript)
	if err != nil {
		return sessionKeys{}, nil, err
	}
	return keys, handshakeMAC(h.psk, "client finished\x00", transcript), nil
}

// serverHandshake answers a client hello. It returns the server hello, the
// session keys, and the client finish MAC the server must receive.
func serverHandshake(screenKey string, clientHello []byte) ([]byte, sessionKeys, []byte, error) {
	psk, err := decodeScreenKey(screenKey)
	if err != nil {
		return nil, sessionKeys{}, nil, err
	}
	if len(clientHello) != clientHelloSize {
		return nil, sessionKeys{}, nil, errors.New("invalid live screen client hello")
	}
	clientPub, err := ecdh.P256().NewPublicKey(clientHello[:65])
	if err != nil {
		return nil, sessionKeys{}, nil, fmt.Errorf("invalid live screen client key: %w", err)
	}
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, sessionKeys{}, nil, err
	}
	pubNonce := append([]byte(nil), priv.PublicKey().Bytes()...)
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, sessionKeys{}, nil, err
	}
	pubNonce = append(pubNonce, nonce...)
	transcript := handshakeTranscript(clientHello, pubNonce)
	shared, err := priv.ECDH(clientPub)
	if err != nil {
		return nil, sessionKeys{}, nil, err
	}
	keys, err := deriveSessionKeys(shared, psk, transcript)
	if err != nil {
		return nil, sessionKeys{}, nil, err
	}
	hello := append(pubNonce, handshakeMAC(psk, "server finished\x00", transcript)...)
	return hello, keys, handshakeMAC(psk, "client finished\x00", transcript), nil
}

// recordConn carries AES-256-GCM records with per-direction sequence nonces.
type recordConn struct {
	r io.Reader
	w io.Writer

	readMu  sync.Mutex
	recv    cipher.AEAD
	recvSeq uint64
	header  [4]byte

	writeMu sync.Mutex
	send    cipher.AEAD
	sendSeq uint64
	written uint64
}

func newRecordConn(r io.Reader, w io.Writer, sendKey, recvKey []byte) (*recordConn, error) {
	send, err := newGCM(sendKey)
	if err != nil {
		return nil, err
	}
	recv, err := newGCM(recvKey)
	if err != nil {
		return nil, err
	}
	return &recordConn{r: r, w: w, send: send, recv: recv}, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func recordNonce(seq uint64) []byte {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], seq)
	return nonce
}

// WriteRecord encrypts and writes one record with a single Write call so a
// message-oriented transport keeps records whole where it can.
func (c *recordConn) WriteRecord(recordType byte, payload []byte) error {
	if len(payload)+1 > maxRecordPlain {
		return errors.New("live screen record is too large")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	plain := make([]byte, 0, len(payload)+1)
	plain = append(plain, recordType)
	plain = append(plain, payload...)
	out := make([]byte, 4, 4+len(plain)+c.send.Overhead())
	out = c.send.Seal(out, recordNonce(c.sendSeq), plain, nil)
	binary.BigEndian.PutUint32(out[:4], uint32(len(out)-4))
	c.sendSeq++
	if _, err := c.w.Write(out); err != nil {
		return err
	}
	c.written += uint64(len(out))
	return nil
}

// outgoingRecord is a record waiting for a recordQueue.
type outgoingRecord struct {
	recordType byte
	payload    []byte
}

// recordQueue writes records from its own goroutine, so read loops never
// block on a write: a peer that is itself blocked writing to us would
// otherwise deadlock both sides.
type recordQueue struct {
	conn    *recordConn
	records chan outgoingRecord
	failed  chan struct{}
	once    sync.Once
	err     error
}

func newRecordQueue(conn *recordConn) *recordQueue {
	queue := &recordQueue{conn: conn, records: make(chan outgoingRecord, 4096), failed: make(chan struct{})}
	go func() {
		for record := range queue.records {
			if err := conn.WriteRecord(record.recordType, record.payload); err != nil {
				queue.once.Do(func() {
					queue.err = err
					close(queue.failed)
				})
				for range queue.records {
				}
				return
			}
		}
	}()
	return queue
}

// send queues a record; it reports false once the connection has failed.
func (q *recordQueue) send(recordType byte, payload []byte) bool {
	select {
	case <-q.failed:
		return false
	default:
	}
	select {
	case q.records <- outgoingRecord{recordType: recordType, payload: payload}:
		return true
	case <-q.failed:
		return false
	}
}

// BytesWritten returns the ciphertext bytes written so far.
func (c *recordConn) BytesWritten() uint64 {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.written
}

func (c *recordConn) ReadRecord() (byte, []byte, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if _, err := io.ReadFull(c.r, c.header[:]); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(c.header[:])
	if size < uint32(c.recv.Overhead()+1) || size > maxRecordCipher {
		return 0, nil, fmt.Errorf("invalid live screen record size %d", size)
	}
	sealed := make([]byte, size)
	if _, err := io.ReadFull(c.r, sealed); err != nil {
		return 0, nil, err
	}
	plain, err := c.recv.Open(sealed[:0], recordNonce(c.recvSeq), sealed, nil)
	if err != nil {
		return 0, nil, errors.New("live screen record failed authentication")
	}
	c.recvSeq++
	return plain[0], plain[1:], nil
}
