// Package session runs a Noise XX handshake over a stream connection and
// then exposes an authenticated, encrypted, framed channel of proto envelopes.
//
// XX gives mutual authentication of static keys without any prior knowledge
// of them, and hides both static keys from passive observers. Trust in a key
// is decided one layer up (TOFU pinning in the node), never here.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/flynn/noise"

	"github.com/Mattathiasa/chirp/internal/identity"
	"github.com/Mattathiasa/chirp/internal/proto"
)

var suite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)

// prologue binds the handshake to this protocol and version, so a peer speaking
// a different version fails the handshake instead of misparsing frames.
var prologue = []byte(fmt.Sprintf("chirp/%d", proto.Version))

// HandshakeTimeout bounds the whole handshake.
const HandshakeTimeout = 5 * time.Second

// Conn is an established secure session.
type Conn struct {
	raw net.Conn

	// RemoteKey is the peer's authenticated static public key.
	RemoteKey []byte
	// RemoteName is the display name the peer claimed. It is NOT authenticated
	// beyond being signed-by-key; treat it as a label.
	RemoteName string

	wmu sync.Mutex
	enc *noise.CipherState
	dec *noise.CipherState // only touched by the single reader
}

// Initiator performs the handshake as the dialer.
func Initiator(c net.Conn, id *identity.Identity) (*Conn, error) {
	return handshake(c, id, true, prologue)
}

// Responder performs the handshake as the listener.
func Responder(c net.Conn, id *identity.Identity) (*Conn, error) {
	return handshake(c, id, false, prologue)
}

func handshake(c net.Conn, id *identity.Identity, initiator bool, prologue []byte) (*Conn, error) {
	_ = c.SetDeadline(time.Now().Add(HandshakeTimeout))
	defer c.SetDeadline(time.Time{}) //nolint:errcheck

	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   suite,
		Pattern:       noise.HandshakeXX,
		Initiator:     initiator,
		StaticKeypair: id.Key,
		Prologue:      prologue,
	})
	if err != nil {
		return nil, err
	}
	hello, _ := json.Marshal(proto.Hello{V: proto.Version, Name: id.Name})

	var (
		peerHello       []byte
		csWrite, csRead *noise.CipherState
		cs0, cs1        *noise.CipherState
		msg             []byte
	)
	read := func() ([]byte, error) {
		m, err := proto.ReadFrame(c)
		if err != nil {
			return nil, err
		}
		p, a, b, err := hs.ReadMessage(nil, m)
		if err != nil {
			return nil, fmt.Errorf("session: handshake: %w", err)
		}
		cs0, cs1 = a, b
		return p, nil
	}
	write := func(payload []byte) error {
		m, a, b, err := hs.WriteMessage(nil, payload)
		if err != nil {
			return err
		}
		cs0, cs1 = a, b
		msg = m
		return proto.WriteFrame(c, msg)
	}

	if initiator {
		// -> e
		if err := write(nil); err != nil {
			return nil, err
		}
		// <- e, ee, s, es  (+ responder hello)
		if peerHello, err = read(); err != nil {
			return nil, err
		}
		// -> s, se  (+ initiator hello)
		if err := write(hello); err != nil {
			return nil, err
		}
		csWrite, csRead = cs0, cs1
	} else {
		if _, err = read(); err != nil {
			return nil, err
		}
		if err := write(hello); err != nil {
			return nil, err
		}
		if peerHello, err = read(); err != nil {
			return nil, err
		}
		csWrite, csRead = cs1, cs0
	}
	if csWrite == nil || csRead == nil {
		return nil, errors.New("session: handshake incomplete")
	}

	var h proto.Hello
	if err := json.Unmarshal(peerHello, &h); err != nil {
		return nil, errors.New("session: bad hello")
	}
	if h.V != proto.Version {
		return nil, fmt.Errorf("session: unsupported protocol version %d", h.V)
	}
	name, err := identity.ValidateName(h.Name)
	if err != nil {
		return nil, fmt.Errorf("session: bad peer name: %w", err)
	}
	rs := hs.PeerStatic()
	if len(rs) != 32 {
		return nil, errors.New("session: missing peer static key")
	}
	return &Conn{raw: c, RemoteKey: append([]byte(nil), rs...), RemoteName: name, enc: csWrite, dec: csRead}, nil
}

// Send encrypts and writes one envelope. Safe for concurrent use.
func (c *Conn) Send(e proto.Envelope) error {
	plain, err := proto.Encode(e)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	ct, err := c.enc.Encrypt(nil, nil, plain)
	if err != nil {
		return err
	}
	return proto.WriteFrame(c.raw, ct)
}

// Recv reads and decrypts one envelope. Must be called from a single goroutine.
// Any error (including authentication failure) is fatal for the session:
// Noise nonces are implicit counters, so a dropped or reordered frame can never
// be recovered.
func (c *Conn) Recv() (proto.Envelope, error) {
	ct, err := proto.ReadFrame(c.raw)
	if err != nil {
		return proto.Envelope{}, err
	}
	plain, err := c.dec.Decrypt(nil, nil, ct)
	if err != nil {
		return proto.Envelope{}, errors.New("session: authentication failed")
	}
	return proto.Decode(plain)
}

// SetReadDeadline exposes the underlying deadline for keepalive handling.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.raw.SetReadDeadline(t) }

// SetWriteDeadline bounds a single Send.
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.raw.SetWriteDeadline(t) }

// RemoteAddr is the peer's network address.
func (c *Conn) RemoteAddr() net.Addr { return c.raw.RemoteAddr() }

// Close closes the underlying connection.
func (c *Conn) Close() error { return c.raw.Close() }
