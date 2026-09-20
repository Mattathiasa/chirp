package node

import (
	"context"
	"sync"
	"time"

	"github.com/mattathias/chirp/internal/proto"
	"github.com/mattathias/chirp/internal/session"
	"github.com/mattathias/chirp/internal/store"
)

// link is one live authenticated session with a pinned peer.
type link struct {
	n     *Node
	conn  *session.Conn
	peer  string // pinned display name
	since time.Time

	flushMu sync.Mutex // serialises outbox flushes on this link
}

func newLink(n *Node, c *session.Conn, peer string) *link {
	return &link{n: n, conn: c, peer: peer, since: time.Now()}
}

func (l *link) send(e proto.Envelope) error {
	_ = l.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return l.conn.Send(e)
}

func (l *link) pingLoop(ctx context.Context) {
	t := time.NewTicker(l.n.cfg.PingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.send(proto.Envelope{T: proto.TypePing}); err != nil {
				l.conn.Close()
				return
			}
		}
	}
}

// readLoop handles inbound envelopes until the session dies. Silence for
// DeadAfter is treated as death: TCP alone can take minutes to notice a peer
// that vanished from Wi-Fi.
func (l *link) readLoop(ctx context.Context) {
	done := make(chan struct{})
	defer close(done)
	go func() { // unblock Recv on shutdown; exits with the session so none pile up
		select {
		case <-ctx.Done():
			l.conn.Close()
		case <-done:
		}
	}()
	for {
		_ = l.conn.SetReadDeadline(time.Now().Add(l.n.cfg.DeadAfter))
		e, err := l.conn.Recv()
		if err != nil {
			return
		}
		switch e.T {
		case proto.TypePing:
		case proto.TypeMsg:
			m, dup, err := l.n.cfg.Store.AddMessage(store.Message{
				ID: e.ID, Peer: l.peer, Dir: store.DirIn, Body: e.Body,
				TS: time.Now(), Status: store.StatusReceived,
			})
			if err != nil {
				l.n.logf("store inbound: %v", err)
				return // do not ack what we could not persist
			}
			// Always ack, even duplicates: the sender may have missed our first ack.
			if err := l.send(proto.Envelope{T: proto.TypeAck, ID: e.ID}); err != nil {
				return
			}
			if !dup {
				l.n.emit(Event{Type: "message", Peer: l.peer, Message: &m})
			}
		case proto.TypeAck:
			m, changed, err := l.n.cfg.Store.MarkDelivered(l.peer, e.ID)
			if err == nil && changed {
				l.n.emit(Event{Type: "status", Peer: l.peer, Message: &m})
			}
		}
	}
}
