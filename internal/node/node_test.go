package node

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Mattathiasa/chirp/internal/discovery"
	"github.com/Mattathiasa/chirp/internal/identity"
	"github.com/Mattathiasa/chirp/internal/proto"
	"github.com/Mattathiasa/chirp/internal/session"
	"github.com/Mattathiasa/chirp/internal/store"
)

type rig struct {
	n  *Node
	st *store.Store
	id *identity.Identity
}

func fast(c *Config) {
	c.PingEvery = 100 * time.Millisecond
	c.DeadAfter = time.Second
	c.RetryBase = 100 * time.Millisecond
	c.RetryMax = 300 * time.Millisecond
	c.DialTick = 50 * time.Millisecond
	c.ListenAddr = "127.0.0.1:0"
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func startNode(t *testing.T, hub *discovery.Hub, st *store.Store, id *identity.Identity) *Node {
	t.Helper()
	cfg := Config{Store: st, ID: id, Disc: hub.New(), Logf: t.Logf}
	fast(&cfg)
	n := New(cfg)
	if err := n.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Stop)
	return n
}

func newRig(t *testing.T, hub *discovery.Hub, name string) *rig {
	id, err := identity.Generate(name)
	if err != nil {
		t.Fatal(err)
	}
	st := newStore(t)
	return &rig{n: startNode(t, hub, st, id), st: st, id: id}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func online(n *Node, name string) bool {
	ps, _ := n.Peers()
	for _, p := range ps {
		if p.Name == name && p.Online {
			return true
		}
	}
	return false
}

func status(st *store.Store, peer, id string) string {
	ms, _ := st.Messages(peer, 100)
	for _, m := range ms {
		if m.ID == id {
			return m.Status
		}
	}
	return ""
}

func TestTwoNodesDeliver(t *testing.T) {
	hub := discovery.NewHub()
	a, b := newRig(t, hub, "Alex"), newRig(t, hub, "Sam")
	waitFor(t, "sessions", func() bool { return online(a.n, "Sam") && online(b.n, "Alex") })

	m, err := a.n.Send("Sam", "hello over noise")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "delivery ack", func() bool { return status(a.st, "Sam", m.ID) == store.StatusDelivered })
	ms, _ := b.st.Messages("Alex", 10)
	if len(ms) != 1 || ms[0].Body != "hello over noise" || ms[0].Dir != store.DirIn {
		t.Fatalf("receiver has %+v", ms)
	}

	// and the other direction
	m2, _ := b.n.Send("Alex", "hi back")
	waitFor(t, "reverse delivery", func() bool { return status(b.st, "Alex", m2.ID) == store.StatusDelivered })

	// First contact is pinned but unverified on both sides.
	ps, _ := a.n.Peers()
	if ps[0].Trust != TrustNew {
		t.Fatalf("trust %q", ps[0].Trust)
	}
	a.n.Verify("Sam", true)
	ps, _ = a.n.Peers()
	if ps[0].Trust != TrustVerified {
		t.Fatalf("trust %q", ps[0].Trust)
	}
}

func TestOutboxSurvivesPeerRestart(t *testing.T) {
	hub := discovery.NewHub()
	a := newRig(t, hub, "Alex")
	bid, _ := identity.Generate("Priya")
	bst := newStore(t)
	cfg := Config{Store: bst, ID: bid, Disc: hub.New(), Logf: t.Logf}
	fast(&cfg)
	b := New(cfg)
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first session", func() bool { return online(a.n, "Priya") })
	b.Stop()
	waitFor(t, "offline", func() bool { return !online(a.n, "Priya") })

	m, err := a.n.Send("Priya", "are you around?")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if s := status(a.st, "Priya", m.ID); s != store.StatusQueued {
		t.Fatalf("status while offline: %q", s)
	}
	if out, _ := a.n.Outbox(); len(out) != 1 {
		t.Fatalf("outbox %d", len(out))
	}

	// Priya comes back with the same key and store, on a new port.
	b2 := startNode(t, hub, bst, bid)
	_ = b2
	waitFor(t, "delivery after restart", func() bool { return status(a.st, "Priya", m.ID) == store.StatusDelivered })
	if ms, _ := bst.Messages("Alex", 10); len(ms) != 1 {
		t.Fatalf("receiver has %d messages", len(ms))
	}
}

func rawSession(t *testing.T, addr string, id *identity.Identity) *session.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := session.Initiator(c, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sc.Close() })
	return sc
}

func TestReceiverDedupsRedelivery(t *testing.T) {
	hub := discovery.NewHub()
	b := newRig(t, hub, "Sam")
	eve, _ := identity.Generate("Eve")
	sc := rawSession(t, b.n.ln.Addr().String(), eve)
	id, _ := proto.NewID(rand.Reader)
	for i := 0; i < 3; i++ {
		if err := sc.Send(proto.Envelope{T: proto.TypeMsg, ID: id, Body: "once"}); err != nil {
			t.Fatal(err)
		}
	}
	acks := 0
	sc.SetReadDeadline(time.Now().Add(2 * time.Second))
	for acks < 3 {
		e, err := sc.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if e.T == proto.TypeAck && e.ID == id {
			acks++
		}
	}
	if ms, _ := b.st.Messages("Eve", 10); len(ms) != 1 {
		t.Fatalf("stored %d copies", len(ms))
	}
}

func TestKeyChangeBlocksChat(t *testing.T) {
	hub := discovery.NewHub()
	a := newRig(t, hub, "Alex")
	real, _ := identity.Generate("Dana")
	sc := rawSession(t, a.n.ln.Addr().String(), real)
	waitFor(t, "pinned", func() bool { p, _ := a.st.GetPeer("Dana"); return p != nil })
	sc.Close()
	waitFor(t, "session closed", func() bool { return !online(a.n, "Dana") })
	a.n.Verify("Dana", true)

	imp, _ := identity.Generate("Dana")
	sc2 := rawSession(t, a.n.ln.Addr().String(), imp)
	sc2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := sc2.Recv(); err == nil {
		t.Fatal("impostor session stayed open")
	}
	ps, _ := a.n.Peers()
	var dana PeerView
	for _, p := range ps {
		if p.Name == "Dana" {
			dana = p
		}
	}
	if dana.Trust != TrustChanged || dana.NewFP != identity.Fingerprint(imp.Key.Public) || dana.FP != identity.Fingerprint(real.Key.Public) {
		t.Fatalf("view: %+v", dana)
	}
	if _, err := a.n.Send("Dana", "hi"); err != ErrKeyChanged {
		t.Fatalf("send while changed: %v", err)
	}
	if err := a.n.AcceptKey("Dana"); err != nil {
		t.Fatal(err)
	}
	sc3 := rawSession(t, a.n.ln.Addr().String(), imp)
	_ = sc3
	waitFor(t, "accepted key online", func() bool { return online(a.n, "Dana") })
	ps, _ = a.n.Peers()
	for _, p := range ps {
		if p.Name == "Dana" && p.Trust != TrustNew {
			t.Fatalf("accepted key must not inherit verification, got %q", p.Trust)
		}
	}
}

func TestSendValidation(t *testing.T) {
	hub := discovery.NewHub()
	a := newRig(t, hub, "Alex")
	if _, err := a.n.Send("Nobody", "x"); err != ErrUnknownPeer {
		t.Fatalf("got %v", err)
	}
	a.st.Observe("Sam", make([]byte, 32), time.Now())
	for _, body := range []string{"", string(make([]byte, proto.MaxBody+1)), "\xff"} {
		if _, err := a.n.Send("Sam", body); err != ErrBadBody {
			t.Fatalf("body %q: %v", body, err)
		}
	}
}

func TestDeadPeerIsDetected(t *testing.T) {
	hub := discovery.NewHub()
	a := newRig(t, hub, "Alex")
	quiet, _ := identity.Generate("Mute")
	sc := rawSession(t, a.n.ln.Addr().String(), quiet) // never pings
	_ = sc
	waitFor(t, "online", func() bool { return online(a.n, "Mute") })
	waitFor(t, "declared dead after silence", func() bool { return !online(a.n, "Mute") })
}

func TestManyMessagesInOrder(t *testing.T) {
	hub := discovery.NewHub()
	a, b := newRig(t, hub, "Alex"), newRig(t, hub, "Sam")
	waitFor(t, "sessions", func() bool { return online(a.n, "Sam") && online(b.n, "Alex") })
	var last string
	for i := 0; i < 50; i++ {
		m, err := a.n.Send("Sam", fmt.Sprintf("msg %d", i))
		if err != nil {
			t.Fatal(err)
		}
		last = m.ID
	}
	waitFor(t, "all delivered", func() bool { return status(a.st, "Sam", last) == store.StatusDelivered })
	waitFor(t, "receiver has all", func() bool { ms, _ := b.st.Messages("Alex", 100); return len(ms) == 50 })
	ms, _ := b.st.Messages("Alex", 100)
	for i, m := range ms {
		if m.Body != fmt.Sprintf("msg %d", i) {
			t.Fatalf("out of order at %d: %q", i, m.Body)
		}
	}
}

// Reconnecting many times must not leave goroutines behind.
func TestNoGoroutineLeakAcrossReconnects(t *testing.T) {
	hub := discovery.NewHub()
	a := newRig(t, hub, "Alex")
	id, _ := identity.Generate("Flap")
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		sc := rawSession(t, a.n.ln.Addr().String(), id)
		waitFor(t, "online", func() bool { return online(a.n, "Flap") })
		sc.Close()
		waitFor(t, "offline", func() bool { return !online(a.n, "Flap") })
	}
	time.Sleep(300 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+8 {
		t.Fatalf("goroutines grew from %d to %d over 20 reconnects", before, after)
	}
}
