// Package node is the Chirp engine: it announces us, dials peers it discovers,
// accepts inbound sessions, applies TOFU pinning, runs the outbox, and fans
// events out to whoever is watching (the HTTP API).
//
// Connection rule: for every pair, exactly one side dials. The side whose
// fingerprint sorts lower is the dialer. Both sides browse, so both learn of
// each other; only one connects. This removes duplicate-connection races
// without a tie-break protocol.
package node

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Mattathiasa/chirp/internal/discovery"
	"github.com/Mattathiasa/chirp/internal/identity"
	"github.com/Mattathiasa/chirp/internal/proto"
	"github.com/Mattathiasa/chirp/internal/session"
	"github.com/Mattathiasa/chirp/internal/store"
)

// Errors surfaced to callers.
var (
	ErrUnknownPeer = errors.New("node: unknown peer")
	ErrKeyChanged  = errors.New("node: peer key changed; review it before sending")
	ErrBadBody     = errors.New("node: message must be 1-4096 bytes of valid UTF-8")
)

// Config configures a Node.
type Config struct {
	Store      *store.Store
	ID         *identity.Identity
	Disc       discovery.Discovery
	ListenAddr string // default "0.0.0.0:0"
	Logf       func(format string, args ...any)

	// Timing knobs; zero values pick production defaults. Tests shrink them.
	PingEvery time.Duration // default 10s
	DeadAfter time.Duration // default 30s of silence
	RetryBase time.Duration // outbox resend base, default 2s
	RetryMax  time.Duration // default 30s
	DialTick  time.Duration // default 1s
}

func (c *Config) defaults() {
	if c.ListenAddr == "" {
		c.ListenAddr = "0.0.0.0:0"
	}
	if c.PingEvery == 0 {
		c.PingEvery = 10 * time.Second
	}
	if c.DeadAfter == 0 {
		c.DeadAfter = 30 * time.Second
	}
	if c.RetryBase == 0 {
		c.RetryBase = 2 * time.Second
	}
	if c.RetryMax == 0 {
		c.RetryMax = 30 * time.Second
	}
	if c.DialTick == 0 {
		c.DialTick = time.Second
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
}

// Node is a running Chirp engine.
type Node struct {
	cfg  Config
	id   *identity.Identity
	myFP string

	ln     net.Listener
	cancel context.CancelFunc
	wg     sync.WaitGroup
	hsSem  chan struct{} // bounds concurrent unauthenticated handshakes

	mu      sync.Mutex
	nearby  map[string]*nearby // by advertised fingerprint
	live    map[string]*link   // by lower-cased peer name
	subs    map[int]chan Event
	nextSub int
	log     []LogEntry
	started time.Time
}

type nearby struct {
	peer     discovery.Peer
	up       bool
	seen     time.Time
	fails    int
	nextDial time.Time
	dialing  bool
}

// New builds a Node. Call Start to run it.
func New(cfg Config) *Node {
	cfg.defaults()
	return &Node{
		cfg:    cfg,
		id:     cfg.ID,
		myFP:   identity.Fingerprint(cfg.ID.Key.Public),
		hsSem:  make(chan struct{}, 32),
		nearby: map[string]*nearby{},
		live:   map[string]*link{},
		subs:   map[int]chan Event{},
	}
}

// Start listens, announces and begins browsing. It returns once the listener
// is up; work continues in the background until ctx is cancelled or Stop.
func (n *Node) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", n.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("node: listen: %w", err)
	}
	n.ln = ln
	n.started = time.Now()
	ctx, n.cancel = context.WithCancel(ctx)

	events, err := n.cfg.Disc.Browse(ctx)
	if err != nil {
		ln.Close()
		return err
	}
	if err := n.cfg.Disc.Announce(ctx, discovery.Announcement{Name: n.id.Name, FP: n.myFP, Port: n.Port()}); err != nil {
		ln.Close()
		return err
	}
	n.logf("listening on %s, announcing as %q (%s…)", ln.Addr(), n.id.Name, n.myFP[:8])

	n.goRun(func() { n.acceptLoop(ctx) })
	n.goRun(func() { n.browseLoop(ctx, events) })
	n.goRun(func() { n.dialLoop(ctx) })
	n.goRun(func() { n.retentionLoop(ctx) })
	n.goRun(func() { n.outboxLoop(ctx) })
	return nil
}

// Stop shuts everything down and waits for goroutines.
func (n *Node) Stop() {
	if n.cancel != nil {
		n.cancel()
	}
	if n.ln != nil {
		n.ln.Close()
	}
	n.mu.Lock()
	for _, l := range n.live {
		l.conn.Close()
	}
	n.mu.Unlock()
	n.wg.Wait()
}

func (n *Node) goRun(f func()) {
	n.wg.Add(1)
	go func() { defer n.wg.Done(); f() }()
}

// Port is the TCP port we listen on.
func (n *Node) Port() int { return n.ln.Addr().(*net.TCPAddr).Port }

// Identity returns who we are.
func (n *Node) Identity() *identity.Identity { return n.id }

// ---- accept / dial ----

func (n *Node) acceptLoop(ctx context.Context) {
	for {
		c, err := n.ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				n.logf("accept: %v", err)
			}
			return
		}
		select {
		case n.hsSem <- struct{}{}:
		default:
			c.Close() // too many half-open handshakes: shed load
			continue
		}
		n.goRun(func() {
			n.serve(ctx, c, false, func() { <-n.hsSem })
		})
	}
}

func (n *Node) browseLoop(ctx context.Context, ev <-chan discovery.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ev:
			if !ok {
				return
			}
			if e.Peer.FP == n.myFP {
				continue // our own announcement
			}
			n.mu.Lock()
			nb := n.nearby[e.Peer.FP]
			if nb == nil {
				nb = &nearby{}
				n.nearby[e.Peer.FP] = nb
			}
			nb.peer, nb.up, nb.seen = e.Peer, e.Up, time.Now()
			n.mu.Unlock()
			if e.Up {
				n.logf("mdns: found %q at %s", e.Peer.Name, strings.Join(e.Peer.Addrs, ","))
			} else {
				n.logf("mdns: %q went away", e.Peer.Name)
			}
			n.emit(Event{Type: "peers"})
		}
	}
}

func (n *Node) dialLoop(ctx context.Context) {
	t := time.NewTicker(n.cfg.DialTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		var todo []*nearby
		n.mu.Lock()
		for fp, nb := range n.nearby {
			if !nb.up || nb.dialing || now.Before(nb.nextDial) || n.myFP >= fp {
				continue // we only dial peers whose fingerprint sorts above ours
			}
			if _, ok := n.live[strings.ToLower(nb.peer.Name)]; ok {
				continue
			}
			nb.dialing = true
			todo = append(todo, nb)
		}
		n.mu.Unlock()
		for _, nb := range todo {
			nb := nb
			n.goRun(func() { n.dial(ctx, nb) })
		}
	}
}

func (n *Node) dial(ctx context.Context, nb *nearby) {
	n.mu.Lock()
	p := nb.peer
	n.mu.Unlock()

	var conn net.Conn
	var err error
	d := net.Dialer{Timeout: 3 * time.Second}
	for _, a := range p.Addrs {
		if conn, err = d.DialContext(ctx, "tcp", a); err == nil {
			break
		}
	}
	if conn == nil {
		n.dialDone(nb, false)
		n.logf("dial %q failed: %v", p.Name, err)
		return
	}
	established := n.serve(ctx, conn, true, nil)
	n.dialDone(nb, established)
}

func (n *Node) dialDone(nb *nearby, ok bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	nb.dialing = false
	if ok {
		nb.fails = 0
		nb.nextDial = time.Now().Add(n.cfg.DialTick)
		return
	}
	nb.fails++
	nb.nextDial = time.Now().Add(backoff(nb.fails, time.Second, 30*time.Second))
}

func backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt > 16 {
		attempt = 16
	}
	d := base << uint(attempt-1)
	if attempt < 1 || d > max || d <= 0 {
		return max
	}
	return d
}

// ---- session lifecycle ----

// serve runs one connection from handshake to close. It reports whether a
// session was established (handshake ok and key acceptable).
func (n *Node) serve(ctx context.Context, c net.Conn, initiator bool, hsDone func()) (established bool) {
	release := func() {
		if hsDone != nil {
			hsDone()
			hsDone = nil
		}
	}
	defer release()

	var sc *session.Conn
	var err error
	if initiator {
		sc, err = session.Initiator(c, n.id)
	} else {
		sc, err = session.Responder(c, n.id)
	}
	release() // handshake finished (or failed): free the slot for others
	if err != nil {
		c.Close()
		n.logf("handshake with %s failed: %v", c.RemoteAddr(), err)
		return false
	}
	if string(sc.RemoteKey) == string(n.id.Key.Public) {
		sc.Close()
		return false // talked to ourselves
	}

	peer, ok, err := n.cfg.Store.Observe(sc.RemoteName, sc.RemoteKey, time.Now())
	if err != nil {
		sc.Close()
		n.logf("store: %v", err)
		return false
	}
	if !ok {
		sc.Close()
		n.logf("REFUSED %q: key %s… does not match pinned %s…", sc.RemoteName,
			identity.Fingerprint(sc.RemoteKey)[:8], peer.FP[:8])
		n.emit(Event{Type: "peers", Peer: peer.Name})
		return false
	}

	l := newLink(n, sc, peer.Name)
	n.mu.Lock()
	key := strings.ToLower(peer.Name)
	if old := n.live[key]; old != nil {
		old.conn.Close() // newest session wins
	}
	n.live[key] = l
	n.mu.Unlock()
	n.logf("session up with %q (%s…) from %s", peer.Name, peer.FP[:8], sc.RemoteAddr())
	n.emit(Event{Type: "peers", Peer: peer.Name})

	n.goRun(func() { l.pingLoop(ctx) })
	n.goRun(func() { n.flush(peer.Name, true) })
	l.readLoop(ctx)

	sc.Close()
	n.mu.Lock()
	if n.live[key] == l {
		delete(n.live, key)
	}
	n.mu.Unlock()
	n.logf("session down with %q", peer.Name)
	n.emit(Event{Type: "peers", Peer: peer.Name})
	return true
}

func (n *Node) linkFor(name string) *link {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.live[strings.ToLower(name)]
}

// ---- sending and the outbox ----

// Send queues a message for a pinned peer and tries to deliver it now.
func (n *Node) Send(peer, body string) (store.Message, error) {
	if body == "" || len(body) > proto.MaxBody || !utf8.ValidString(body) {
		return store.Message{}, ErrBadBody
	}
	p, err := n.cfg.Store.GetPeer(peer)
	if errors.Is(err, store.ErrNotFound) {
		return store.Message{}, ErrUnknownPeer
	} else if err != nil {
		return store.Message{}, err
	}
	if p.PendingKey != nil {
		return store.Message{}, ErrKeyChanged
	}
	id, err := proto.NewID(rand.Reader)
	if err != nil {
		return store.Message{}, err
	}
	m, _, err := n.cfg.Store.AddMessage(store.Message{
		ID: id, Peer: p.Name, Dir: store.DirOut, Body: body, TS: time.Now(), Status: store.StatusQueued,
	})
	if err != nil {
		return store.Message{}, err
	}
	n.emit(Event{Type: "message", Peer: p.Name, Message: &m})
	n.goRun(func() { n.flush(p.Name, false) })
	return m, nil
}

// flush sends pending messages to peer if a session is up. force=true ignores
// backoff (used when a session has just been established).
func (n *Node) flush(peer string, force bool) {
	l := n.linkFor(peer)
	if l == nil {
		return
	}
	l.flushMu.Lock()
	defer l.flushMu.Unlock()
	pend, err := n.cfg.Store.Pending(peer)
	if err != nil {
		n.logf("outbox: %v", err)
		return
	}
	now := time.Now()
	for _, m := range pend {
		if !force && m.Attempts > 0 && now.Sub(m.LastTry) < backoff(m.Attempts, n.cfg.RetryBase, n.cfg.RetryMax) {
			continue
		}
		m2, err := n.cfg.Store.RecordAttempt(m.ID, now)
		if err != nil {
			continue
		}
		if err := l.send(proto.Envelope{T: proto.TypeMsg, ID: m.ID, TS: m.TS.UnixMilli(), Body: m.Body}); err != nil {
			n.logf("send to %q failed: %v", peer, err)
			l.conn.Close()
			return
		}
		n.emit(Event{Type: "status", Peer: peer, Message: &m2})
	}
}

func (n *Node) outboxLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n.mu.Lock()
		names := make([]string, 0, len(n.live))
		for _, l := range n.live {
			names = append(names, l.peer)
		}
		n.mu.Unlock()
		for _, name := range names {
			n.flush(name, false)
		}
	}
}

// RetryNow forces an immediate resend attempt for a peer.
func (n *Node) RetryNow(peer string) { n.goRun(func() { n.flush(peer, true) }) }

// DeleteFromOutbox drops an undelivered message.
func (n *Node) DeleteFromOutbox(id string) error {
	if err := n.cfg.Store.DeleteFromOutbox(id); err != nil {
		return err
	}
	n.emit(Event{Type: "outbox"})
	return nil
}

func (n *Node) retentionLoop(ctx context.Context) {
	run := func() {
		st, err := n.cfg.Store.Settings()
		if err != nil || st.RetentionDays == 0 {
			return
		}
		if k, err := n.cfg.Store.Prune(time.Now().AddDate(0, 0, -st.RetentionDays)); err == nil && k > 0 {
			n.logf("retention: pruned %d messages", k)
			n.emit(Event{Type: "peers"})
		}
	}
	run()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
