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
	// Version is the current protocol version.
	Version = 2
	// MaxFrame bounds one frame on the wire. Noise transport messages are
	// limited to 65535 bytes including the 16-byte tag.
	MaxFrame = 65535
	// MaxBody bounds one chat message body in bytes.
	MaxBody = 4096
	// MaxFileSize is the default maximum file transfer size (100 MB).
	MaxFileSize = 100 << 20
	// IDLen is the byte length of a message ID.
	IDLen = 16
	// ChunkSize is the size of each file transfer chunk. A chunk travels as
	// base64 inside a JSON envelope, which costs 4 bytes per 3, and the
	// envelope is then Noise-encrypted (+16 bytes) and written into a frame
	// bounded by MaxFrame. 32 KB leaves comfortable headroom; 64 KB does not
	// fit and made every chunk unsendable.
	ChunkSize = 32 << 10 // 32 KB
)

// Capabilities advertised in the Hello payload.
const (
	CapFiles     = "files"     // file transfer support
	CapReactions = "reactions" // reaction support
	CapReceipts  = "receipts"  // read receipt support
	CapTyping    = "typing"    // typing indicator support
	CapRooms     = "rooms"     // group chat support (roommsg/roomack/roomevent)
	CapResume    = "resume"    // resumable file transfer (fileack)
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
	TypeMsg     Type = "msg"
	TypeAck     Type = "ack"
	TypePing    Type = "ping"
	TypeReact   Type = "react"
	TypeDel     Type = "del"    // delete-for-me
	TypeDelAll  Type = "delall" // delete-for-everyone (best effort)
	TypeTyping  Type = "typing"
	TypeRead    Type = "read"
	TypeFile    Type = "file"    // file metadata
	TypeChunk   Type = "chunk"   // file data chunk
	TypeFileAck Type = "fileack" // receiver: "I hold this many bytes, resume there"

	// Room types (Phase 3).
	TypeRoomMsg   Type = "roommsg"   // message to a room
	TypeRoomAck   Type = "roomack"   // ack for room message
	TypeRoomEvent Type = "roomevent" // membership change (join/leave/remove/rename)
)

// Envelope is the decrypted application frame.
type Envelope struct {
	T       Type   `json:"t"`
	ID      string `json:"id,omitempty"`      // 32 hex chars
	TS      int64  `json:"ts,omitempty"`      // sender's unix millis
	Body    string `json:"body,omitempty"`    // msg only
	ReplyTo string `json:"replyTo,omitempty"` // reply-to: message ID being quoted
	Emoji   string `json:"emoji,omitempty"`   // react: emoji string
	Target  string `json:"target,omitempty"`  // del/delall/read: target message ID
	Src     string `json:"src,omitempty"`     // file: original filename
	Size    int64  `json:"size,omitempty"`    // file: total size in bytes
	Hash    string `json:"hash,omitempty"`    // file: SHA-256 hex of entire file
	Offset  int64  `json:"offset,omitempty"`  // chunk: byte offset in file
	Chunk   []byte `json:"chunk,omitempty"`   // chunk: file data (raw bytes, sent as base64 in JSON)

	// Room fields.
	Room        string   `json:"room,omitempty"`        // room ID for room messages
	Seq         uint64   `json:"seq,omitempty"`         // roommsg: sender's per-room sequence number
	RoomEvent   string   `json:"roomEvent,omitempty"`   // room event type: "join", "leave", "remove", "rename", "create"
	RoomName    string   `json:"roomName,omitempty"`    // room rename: new name
	RoomActor   string   `json:"roomActor,omitempty"`   // room event: who performed the action
	RoomMembers []string `json:"roomMembers,omitempty"` // room create: full member list
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
		if e.ReplyTo != "" {
			if err := checkID(e.ReplyTo); err != nil {
				return fmt.Errorf("proto: bad replyTo: %w", err)
			}
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
	case TypeReact:
		// react, del, delall and read all point at another message, and they
		// all carry that pointer in target.
		if err := checkID(e.Target); err != nil {
			return fmt.Errorf("proto: bad react target: %w", err)
		}
		if e.Emoji == "" {
			return errors.New("proto: react must have emoji")
		}
		if utf8.RuneCountInString(e.Emoji) > 32 {
			return errors.New("proto: emoji too long")
		}
		if !utf8.ValidString(e.Emoji) {
			return errors.New("proto: emoji is not valid UTF-8")
		}
		// A reaction to a room message names the room; a one-to-one reaction
		// leaves it empty.
		if e.Room != "" {
			if err := checkID(e.Room); err != nil {
				return fmt.Errorf("proto: bad react room: %w", err)
			}
		}
	case TypeDel:
		if err := checkID(e.Target); err != nil {
			return fmt.Errorf("proto: bad del target: %w", err)
		}
	case TypeDelAll:
		if err := checkID(e.Target); err != nil {
			return fmt.Errorf("proto: bad delall target: %w", err)
		}
	case TypeTyping:
		// typing is ephemeral, no ID needed
	case TypeRead:
		if e.Target == "" {
			return errors.New("proto: read must have target")
		}
		if err := checkID(e.Target); err != nil {
			return fmt.Errorf("proto: bad read target: %w", err)
		}
	case TypeFile:
		if err := checkID(e.ID); err != nil {
			return err
		}
		if e.Src == "" {
			return errors.New("proto: file must have src")
		}
		if e.Size <= 0 || e.Size > MaxFileSize {
			return errors.New("proto: file size out of range")
		}
		if e.Hash == "" || len(e.Hash) != 64 {
			return errors.New("proto: file must have 64-char SHA-256 hash")
		}
		if _, err := hex.DecodeString(e.Hash); err != nil {
			return errors.New("proto: file hash is not hex")
		}
	case TypeChunk:
		if err := checkID(e.ID); err != nil {
			return err
		}
		if e.Offset < 0 || e.Offset > MaxFileSize {
			return errors.New("proto: chunk offset out of range")
		}
		if len(e.Chunk) == 0 || len(e.Chunk) > ChunkSize+1024 { // base64 overhead
			return errors.New("proto: chunk size out of range")
		}
	case TypeFileAck:
		if err := checkID(e.ID); err != nil {
			return err
		}
		// Offset is how many contiguous bytes the receiver already holds, so
		// zero is both legal and the common case: start from the beginning.
		if e.Offset < 0 || e.Offset > MaxFileSize {
			return errors.New("proto: fileack offset out of range")
		}
		if len(e.Chunk) != 0 || e.Body != "" {
			return errors.New("proto: fileack must carry no data")
		}
	case TypeRoomMsg:
		if err := checkID(e.ID); err != nil {
			return err
		}
		if e.Room == "" {
			return errors.New("proto: roommsg must have room")
		}
		if err := checkID(e.Room); err != nil {
			return fmt.Errorf("proto: bad room id: %w", err)
		}
		if e.Body == "" {
			return errors.New("proto: roommsg must have body")
		}
		if len(e.Body) > MaxBody {
			return errors.New("proto: body too long")
		}
		if !utf8.ValidString(e.Body) {
			return errors.New("proto: body is not valid UTF-8")
		}
		if e.ReplyTo != "" {
			if err := checkID(e.ReplyTo); err != nil {
				return fmt.Errorf("proto: bad replyTo: %w", err)
			}
		}
		// seq is optional: a peer speaking the earlier v2 wire format never
		// sends one, and its absence simply means "ordering unknown".
	case TypeRoomAck:
		if err := checkID(e.ID); err != nil {
			return err
		}
		if e.Room == "" {
			return errors.New("proto: roomack must have room")
		}
		if err := checkID(e.Room); err != nil {
			return fmt.Errorf("proto: bad room id: %w", err)
		}
	case TypeRoomEvent:
		if e.Room == "" {
			return errors.New("proto: roomevent must have room")
		}
		if err := checkID(e.Room); err != nil {
			return fmt.Errorf("proto: bad room id: %w", err)
		}
		validEvents := map[string]bool{"join": true, "leave": true, "remove": true, "rename": true, "create": true}
		if !validEvents[e.RoomEvent] {
			return fmt.Errorf("proto: unknown room event %q", e.RoomEvent)
		}
		if e.RoomActor == "" {
			return errors.New("proto: roomevent must have roomActor")
		}
	default:
		return fmt.Errorf("proto: unknown type %q", e.T)
	}
	if e.Seq != 0 && e.T != TypeRoomMsg {
		return fmt.Errorf("proto: %s must not carry seq", e.T)
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
// ephemeral exchange). It carries the display name, protocol version, and
// capabilities for v2+ negotiation.
type Hello struct {
	V    int      `json:"v"`
	Name string   `json:"name"`
	Caps []string `json:"caps,omitempty"` // v2+ capabilities
}

// HasCap checks if the Hello contains a capability.
func (h Hello) HasCap(cap string) bool {
	for _, c := range h.Caps {
		if c == cap {
			return true
		}
	}
	return false
}

// RemoteVersion returns the negotiated version: min(local, remote).
func RemoteVersion(local, remote int) int {
	if remote < local {
		return remote
	}
	return local
}

// Now returns unix millis; a variable so tests can pin it.
var Now = func() int64 { return time.Now().UnixMilli() }
