package proto

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	for _, p := range [][]byte{[]byte("a"), bytes.Repeat([]byte{7}, MaxFrame)} {
		buf.Reset()
		if err := WriteFrame(&buf, p); err != nil {
			t.Fatal(err)
		}
		got, err := ReadFrame(&buf)
		if err != nil || !bytes.Equal(got, p) {
			t.Fatalf("roundtrip failed: %v", err)
		}
	}
}

func TestFrameLimits(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, nil); err != ErrEmptyFrame {
		t.Fatalf("got %v", err)
	}
	if err := WriteFrame(&buf, make([]byte, MaxFrame+1)); err != ErrFrameTooLarge {
		t.Fatalf("got %v", err)
	}
	// zero-length on the wire
	if _, err := ReadFrame(bytes.NewReader([]byte{0, 0})); err != ErrEmptyFrame {
		t.Fatalf("got %v", err)
	}
	// truncated body
	if _, err := ReadFrame(bytes.NewReader([]byte{0, 5, 1, 2})); err == nil {
		t.Fatal("truncated frame accepted")
	}
}

func TestEnvelope(t *testing.T) {
	id, _ := NewID(rand.Reader)
	good := []Envelope{
		{T: TypeMsg, ID: id, TS: 1, Body: "hi"},
		{T: TypeAck, ID: id},
		{T: TypePing},
		{T: TypeReact, Target: id, Emoji: "👍"},
		{T: TypeDel, Target: id},
		{T: TypeDelAll, Target: id},
		{T: TypeTyping},
		{T: TypeRead, Target: id},
	}
	for _, e := range good {
		b, err := Encode(e)
		if err != nil {
			t.Fatalf("%+v: %v", e, err)
		}
		got, err := Decode(b)
		if err != nil {
			t.Fatalf("decode %+v: %v", e, err)
		}
		if got.T != e.T || got.ID != e.ID || got.Body != e.Body || got.Emoji != e.Emoji || got.Target != e.Target {
			t.Fatalf("roundtrip %+v -> %+v", e, got)
		}
	}
	bad := []Envelope{
		{T: TypeMsg, ID: id},
		{T: TypeMsg, ID: "zz", Body: "x"},
		{T: TypeMsg, ID: id, Body: strings.Repeat("x", MaxBody+1)},
		{T: TypeMsg, ID: id, Body: "\xff\xfe"},
		{T: TypeAck, ID: id, Body: "x"},
		{T: TypePing, ID: id},
		{T: TypeReact, ID: id},
		{T: TypeReact, ID: id, Emoji: strings.Repeat("😀", 33)},
		{T: TypeRead},
		{T: TypeFile, ID: id},
		{T: "nope"},
	}
	for _, e := range bad {
		if _, err := Encode(e); err == nil {
			t.Errorf("accepted %+v", e)
		}
	}
	for _, raw := range []string{`{"t":"ping","x":1}`, `{"t":"ping"} {"t":"ping"}`, `[]`, ``} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func FuzzDecode(f *testing.F) {
	id, _ := NewID(rand.Reader)
	for _, e := range []Envelope{
		{T: TypeMsg, ID: id, Body: "hello"},
		{T: TypeAck, ID: id},
		{T: TypePing},
		{T: TypeReact, ID: id, Emoji: "👍"},
		{T: TypeDel, ID: id},
		{T: TypeRead, Target: id},
	} {
		b, _ := Encode(e)
		f.Add(b)
	}
	f.Add([]byte(`{"t":"msg"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		e, err := Decode(data)
		if err != nil {
			return
		}
		// Anything Decode accepts must re-encode and decode to the same value.
		b, err := Encode(e)
		if err != nil {
			t.Fatalf("decoded but cannot encode: %v", err)
		}
		e2, err := Decode(b)
		if err != nil {
			t.Fatalf("re-decode failed: %v", err)
		}
		if e2.T != e.T || e2.ID != e.ID || e2.Body != e.Body || e2.Emoji != e.Emoji || e2.Target != e.Target {
			t.Fatalf("unstable: %+v vs %+v", e, e2)
		}
	})
}

func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{0, 3, 'a', 'b', 'c'})
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ReadFrame(bytes.NewReader(data))
		if err == nil && (len(p) == 0 || len(p) > MaxFrame) {
			t.Fatalf("bad frame length %d", len(p))
		}
	})
}

// A full-size chunk must survive base64 expansion, the JSON envelope, the
// Noise tag and still fit in one frame. Getting this wrong makes every file
// transfer fail with "frame too large", so it is pinned by a test.
func TestMaxChunkEnvelopeFitsInAFrame(t *testing.T) {
	const noiseTag = 16
	e := Envelope{
		T:      TypeChunk,
		ID:     "ffffffffffffffffffffffffffffffff",
		Offset: MaxFileSize - ChunkSize,
		Chunk:  bytes.Repeat([]byte{0xFF}, ChunkSize),
	}
	b, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(b)+noiseTag > MaxFrame {
		t.Fatalf("a %d-byte chunk encodes to %d bytes, which with the Noise tag exceeds MaxFrame (%d)",
			ChunkSize, len(b), MaxFrame)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Chunk) != ChunkSize {
		t.Fatalf("chunk round-trip length %d, want %d", len(got.Chunk), ChunkSize)
	}
}

// Each of these envelope types points at another message. The field that
// carries the pointer must be the one the sender fills in; validating a
// different field than the code uses made react unencodable and delall a
// silent no-op on the receiving side.
func TestMessagePointerTypesValidateOnTarget(t *testing.T) {
	const target = "ffffffffffffffffffffffffffffffff"
	for _, tc := range []struct {
		name string
		ok   Envelope
		bad  Envelope
	}{
		{"react", Envelope{T: TypeReact, Target: target, Emoji: "🎉"}, Envelope{T: TypeReact, ID: target, Emoji: "🎉"}},
		{"del", Envelope{T: TypeDel, Target: target}, Envelope{T: TypeDel, ID: target}},
		{"delall", Envelope{T: TypeDelAll, Target: target}, Envelope{T: TypeDelAll, ID: target}},
		{"read", Envelope{T: TypeRead, Target: target}, Envelope{T: TypeRead, ID: target}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Encode(tc.ok)
			if err != nil {
				t.Fatalf("a well-formed %s did not encode: %v", tc.name, err)
			}
			got, err := Decode(b)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Target != target {
				t.Fatalf("target lost in transit: %q", got.Target)
			}
			if _, err := Encode(tc.bad); err == nil {
				t.Fatalf("%s accepted with the pointer in id instead of target", tc.name)
			}
		})
	}
}

// Emoji length is a rune count, not a byte count: one emoji is several bytes.
func TestReactEmojiLengthIsCountedInRunes(t *testing.T) {
	const target = "ffffffffffffffffffffffffffffffff"
	if _, err := Encode(Envelope{T: TypeReact, Target: target, Emoji: strings.Repeat("🎉", 10)}); err != nil {
		t.Fatalf("10 emoji rejected: %v", err)
	}
	if _, err := Encode(Envelope{T: TypeReact, Target: target, Emoji: strings.Repeat("a", 33)}); err == nil {
		t.Fatal("33 runes accepted")
	}
}
