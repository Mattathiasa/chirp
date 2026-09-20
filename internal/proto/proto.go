// Package proto defines Chirp's wire format: length-prefixed frames carrying
// small JSON envelopes. Everything here is transport-agnostic and is fed
// attacker-controlled bytes, so it is strict and fuzzed.
package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"
)

const (
	// Version is the protocol version announced in the handshake payload.
	Version = 1
	// MaxFrame bounds one frame on the wire. Noise transport messages are
	// limited to 65535 bytes including the 16-byte tag.
	MaxFrame = 65535
	// MaxBody bounds one chat message body in bytes.
	MaxBody = 4096
	// IDLen is the byte length of a message ID.
	IDLen = 16
)

// Frame errors.
var (
	ErrFrameTooLarge = errors.New("proto: frame too large")
	ErrEmptyFrame    = errors.New("proto: empty frame")
)

// WriteFrame writes a 2-byte big-endian length followed by payload.
func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) == 0 {
		return ErrEmptyFrame
	}
	if len(payload) > MaxFrame {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 2+len(payload))
	binary.BigEndian.PutUint16(buf, uint16(len(payload)))
	copy(buf[2:], payload)
	_, err := w.Write(buf) // single write so frames from one writer never interleave
	return err
}

// ReadFrame reads one frame. Zero-length frames are rejected.
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n == 0 {
		return nil, ErrEmptyFrame
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// Type identifies an envelope.
type Type string

const (
	TypeMsg  Type = "msg"
	TypeAck  Type = "ack"
	TypePing Type = "ping"
)

// Envelope is the decrypted application frame.
type Envelope struct {
	T    Type   `json:"t"`
	ID   string `json:"id,omitempty"`   // 32 hex chars, for msg and ack
	TS   int64  `json:"ts,omitempty"`   // sender's unix millis, informational only
	Body string `json:"body,omitempty"` // msg only, valid UTF-8, <= MaxBody bytes
}

// NewID returns a random message ID from r.
func NewID(r io.Reader) (string, error) {
	var b [IDLen]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Encode validates and serialises an envelope.
func Encode(e Envelope) ([]byte, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(e)
}

// Decode parses and validates an envelope. Unknown fields are rejected so a
// future version bump is explicit rather than silently ignored.
func Decode(b []byte) (Envelope, error) {
	var e Envelope
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return Envelope{}, fmt.Errorf("proto: %w", err)
	}
	if dec.More() {
		return Envelope{}, errors.New("proto: trailing data")
	}
	if err := e.validate(); err != nil {
		return Envelope{}, err
	}
	return e, nil
}

func (e Envelope) validate() error {
	switch e.T {
	case TypeMsg:
		if err := checkID(e.ID); err != nil {
			return err
		}
		if e.Body == "" {
			return errors.New("proto: empty body")
		}
		if len(e.Body) > MaxBody {
			return errors.New("proto: body too long")
		}
		if !utf8.ValidString(e.Body) {
			return errors.New("proto: body is not valid UTF-8")
		}
	case TypeAck:
		if err := checkID(e.ID); err != nil {
			return err
		}
		if e.Body != "" {
			return errors.New("proto: ack must not carry a body")
		}
	case TypePing:
		if e.ID != "" || e.Body != "" {
			return errors.New("proto: ping must be empty")
		}
	default:
		return fmt.Errorf("proto: unknown type %q", e.T)
	}
	return nil
}

func checkID(id string) error {
	if len(id) != IDLen*2 {
		return errors.New("proto: bad id length")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return errors.New("proto: id is not hex")
	}
	return nil
}

// Hello is the Noise handshake payload each side sends (encrypted, after the
// ephemeral exchange). It carries the display name and protocol version.
type Hello struct {
	V    int    `json:"v"`
	Name string `json:"name"`
}

// Now returns unix millis; a variable so tests can pin it.
var Now = func() int64 { return time.Now().UnixMilli() }
