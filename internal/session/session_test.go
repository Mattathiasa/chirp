package session

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"testing"

	"github.com/Mattathiasa/chirp/internal/identity"
	"github.com/Mattathiasa/chirp/internal/proto"
)

func pair(t *testing.T) (*Conn, *Conn, *identity.Identity, *identity.Identity) {
	t.Helper()
	a, _ := identity.Generate("Alex")
	b, _ := identity.Generate("Sam")
	ca, cb := net.Pipe()
	type res struct {
		c   *Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := Responder(cb, b)
		ch <- res{c, err}
	}()
	ic, err := Initiator(ca, a)
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { ic.Close(); r.c.Close() })
	return ic, r.c, a, b
}

func TestHandshakeAuthenticatesBothKeys(t *testing.T) {
	ic, rc, a, b := pair(t)
	if !bytes.Equal(ic.RemoteKey, b.Key.Public) || ic.RemoteName != "Sam" {
		t.Fatal("initiator saw wrong responder")
	}
	if !bytes.Equal(rc.RemoteKey, a.Key.Public) || rc.RemoteName != "Alex" {
		t.Fatal("responder saw wrong initiator")
	}
}

func TestBidirectionalMessages(t *testing.T) {
	ic, rc, _, _ := pair(t)
	id, _ := proto.NewID(rand.Reader)
	go func() {
		ic.Send(proto.Envelope{T: proto.TypeMsg, ID: id, Body: "hello"})
		ic.Send(proto.Envelope{T: proto.TypePing})
	}()
	e, err := rc.Recv()
	if err != nil || e.Body != "hello" || e.ID != id {
		t.Fatalf("got %+v %v", e, err)
	}
	if e, err = rc.Recv(); err != nil || e.T != proto.TypePing {
		t.Fatalf("got %+v %v", e, err)
	}
	go rc.Send(proto.Envelope{T: proto.TypeAck, ID: id})
	if e, err = ic.Recv(); err != nil || e.T != proto.TypeAck {
		t.Fatalf("got %+v %v", e, err)
	}
}

// corruptConn flips the last bit of the Nth Write call.
type corruptConn struct {
	net.Conn
	n, at int
}

func (c *corruptConn) Write(p []byte) (int, error) {
	c.n++
	if c.n == c.at {
		q := append([]byte(nil), p...)
		q[len(q)-1] ^= 1
		p = q
	}
	return c.Conn.Write(p)
}

func TestTamperedFrameIsRejected(t *testing.T) {
	a, _ := identity.Generate("Alex")
	b, _ := identity.Generate("Sam")
	ca, cb := net.Pipe()
	// Initiator writes: msg1, msg3 (handshake), then data1, data2. Corrupt data2.
	ch := make(chan *Conn, 1)
	go func() {
		rc, err := Responder(cb, b)
		if err != nil {
			ch <- nil
			return
		}
		ch <- rc
	}()
	ic, err := Initiator(&corruptConn{Conn: ca, at: 4}, a)
	if err != nil {
		t.Fatal(err)
	}
	rc := <-ch
	if rc == nil {
		t.Fatal("responder handshake failed")
	}
	defer ic.Close()
	defer rc.Close()
	id, _ := proto.NewID(rand.Reader)
	go func() {
		ic.Send(proto.Envelope{T: proto.TypeMsg, ID: id, Body: "first"})
		ic.Send(proto.Envelope{T: proto.TypeMsg, ID: id, Body: "second"})
	}()
	if e, err := rc.Recv(); err != nil || e.Body != "first" {
		t.Fatalf("first frame should pass: %v", err)
	}
	if _, err := rc.Recv(); err == nil {
		t.Fatal("corrupted frame accepted")
	}
}

func TestVersionMismatchFailsHandshake(t *testing.T) {
	a, _ := identity.Generate("Alex")
	b, _ := identity.Generate("Sam")
	ca, cb := net.Pipe()
	defer ca.Close()
	defer cb.Close()
	go func() { handshake(cb, b, false, []byte("chirp/999")); cb.Close() }()
	if _, err := Initiator(ca, a); err == nil {
		t.Fatal("handshake with different prologue succeeded")
	}
}

func TestGarbageHandshakeFails(t *testing.T) {
	b, _ := identity.Generate("Sam")
	ca, cb := net.Pipe()
	defer ca.Close()
	go func() {
		proto.WriteFrame(ca, bytes.Repeat([]byte{0xAA}, 10))
		io.Copy(io.Discard, ca)
	}()
	if _, err := Responder(cb, b); err == nil {
		t.Fatal("garbage accepted")
	}
}
