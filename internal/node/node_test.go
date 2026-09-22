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

// react, del and delall all point at another message. The sender and the
// receiver have to agree on which field carries that pointer: react built an
// envelope that would not encode at all, and delall encoded fine but the
// receiver read an empty field and deleted nothing.
func TestMessagePointerEnvelopesRoundTrip(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	m, err := alice.n.Send("Bob", "delete me")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob receives", func() bool {
		ms, _ := bob.st.Messages("Alice", 10)
		return len(ms) == 1
	})

	// A reaction must at minimum encode and reach the peer.
	if err := alice.n.SendReaction("Bob", "🎉", m.ID); err != nil {
		t.Fatalf("SendReaction: %v", err)
	}

	// delall must actually remove the message on the far side.
	if err := alice.n.DeleteMessageForEveryone("Bob", m.ID); err != nil {
		t.Fatalf("DeleteMessageForEveryone: %v", err)
	}
	waitFor(t, "bob drops the message", func() bool {
		ms, _ := bob.st.Messages("Alice", 10)
		return len(ms) == 0
	})
}

// reply-to existed in the store, the envelope and the docs, but nothing ever
// filled it in or put it on the wire, so every quote was lost in transit.
func TestReplyToSurvivesTheWire(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	first, err := alice.n.Send("Bob", "what time?")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob has the first", func() bool {
		ms, _ := bob.st.Messages("Alice", 10)
		return len(ms) == 1
	})

	reply, err := alice.n.SendReply("Bob", "half past", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reply.ReplyTo != first.ID {
		t.Fatalf("sender did not record the quote: %q", reply.ReplyTo)
	}
	waitFor(t, "bob has the reply", func() bool {
		ms, _ := bob.st.Messages("Alice", 10)
		return len(ms) == 2
	})
	ms, _ := bob.st.Messages("Alice", 10)
	if ms[1].ReplyTo != first.ID {
		t.Fatalf("quote lost in transit: %q, want %q", ms[1].ReplyTo, first.ID)
	}
}

// Typing and read receipts are privacy settings, off by default, and enforced
// in the node so the local API cannot be used to leak them either way.
func TestTypingAndReceiptsRespectTheSettings(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	seen := make(chan string, 8)
	evs, stop := bob.n.Subscribe()
	defer stop()
	go func() {
		for e := range evs {
			if e.Type == "typing" || e.Type == "read" {
				seen <- e.Type
			}
		}
	}()

	m, _ := alice.n.Send("Bob", "hello")
	waitFor(t, "bob receives", func() bool {
		ms, _ := bob.st.Messages("Alice", 10)
		return len(ms) == 1
	})

	// Off by default: nothing goes out.
	if err := alice.n.SendTyping("Bob"); err != nil {
		t.Fatal(err)
	}
	if err := alice.n.SendReadReceipt("Bob", m.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-seen:
		t.Fatalf("%s was sent while the setting was off", got)
	case <-time.After(400 * time.Millisecond):
	}

	// Turned on, they are delivered.
	st, _ := alice.st.Settings()
	st.Typing, st.Receipts = true, true
	if err := alice.st.SaveSettings(st); err != nil {
		t.Fatal(err)
	}
	if err := alice.n.SendTyping("Bob"); err != nil {
		t.Fatal(err)
	}
	if err := alice.n.SendReadReceipt("Bob", m.ID); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for len(got) < 2 {
		select {
		case k := <-seen:
			got[k] = true
		case <-deadline:
			t.Fatalf("only saw %v after enabling both settings", got)
		}
	}
}
