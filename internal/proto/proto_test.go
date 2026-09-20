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
	}
	for _, e := range good {
		b, err := Encode(e)
		if err != nil {
			t.Fatalf("%+v: %v", e, err)
		}
		got, err := Decode(b)
		if err != nil || got != e {
			t.Fatalf("roundtrip %+v -> %+v (%v)", e, got, err)
		}
	}
	bad := []Envelope{
		{T: TypeMsg, ID: id},
		{T: TypeMsg, ID: "zz", Body: "x"},
		{T: TypeMsg, ID: id, Body: strings.Repeat("x", MaxBody+1)},
		{T: TypeMsg, ID: id, Body: "\xff\xfe"},
		{T: TypeAck, ID: id, Body: "x"},
		{T: TypePing, ID: id},
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
	for _, e := range []Envelope{{T: TypeMsg, ID: id, Body: "hello"}, {T: TypeAck, ID: id}, {T: TypePing}} {
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
		if err != nil || e2 != e {
			t.Fatalf("unstable: %+v vs %+v (%v)", e, e2, err)
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
