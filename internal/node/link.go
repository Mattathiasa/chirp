package node

import (
	"context"
	"sync"
	"time"

	"github.com/Mattathiasa/chirp/internal/proto"
	"github.com/Mattathiasa/chirp/internal/session"
	"github.com/Mattathiasa/chirp/internal/store"
)

// link is one live authenticated session with a pinned peer.
type link struct {
	n     *Node
	conn  *session.Conn
	peer  string // pinned display name
	since time.Time

	flushMu  sync.Mutex // serialises outbox flushes on this link
	fileHash string     // hash of in-progress file transfer
	fileSize int64
	fileName string
	fileOff  int64
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
	// Rate limiting: max 100 messages per second per peer.
	var msgCount int
	var rateStart time.Time
	const maxMsgPerSec = 100
	for {
		_ = l.conn.SetReadDeadline(time.Now().Add(l.n.cfg.DeadAfter))
		e, err := l.conn.Recv()
		if err != nil {
			return
		}
		switch e.T {
		case proto.TypePing:
		case proto.TypeMsg:
			now := time.Now()
			if rateStart.IsZero() || now.Sub(rateStart) > time.Second {
				rateStart = now
				msgCount = 0
			}
			msgCount++
			if msgCount > maxMsgPerSec {
				l.n.logf("rate limit: %q exceeded %d msg/s", l.peer, maxMsgPerSec)
				l.conn.Close()
				return
			}
			m, dup, err := l.n.cfg.Store.AddMessage(store.Message{
				ID: e.ID, Peer: l.peer, Dir: store.DirIn, Body: e.Body,
				TS: now, Status: store.StatusReceived, ReplyTo: e.ReplyTo,
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
		case proto.TypeReact:
			l.n.emit(Event{Type: "reaction", Peer: l.peer, Emoji: e.Emoji, Target: e.Target})
		case proto.TypeDel:
			l.n.cfg.Store.DeleteMessage(e.Target)
			l.n.emit(Event{Type: "peers", Peer: l.peer})
		case proto.TypeDelAll:
			l.n.cfg.Store.DeleteMessage(e.Target)
			l.n.emit(Event{Type: "peers", Peer: l.peer})
		case proto.TypeTyping:
			l.n.emit(Event{Type: "typing", Peer: l.peer})
		case proto.TypeRead:
			l.n.emit(Event{Type: "read", Peer: l.peer, Target: e.Target})
		case proto.TypeFile:
			l.fileHash = e.Hash
			l.fileSize = e.Size
			l.fileName = e.Src
			l.fileOff = 0
			l.n.emit(Event{Type: "file", Peer: l.peer, FileSrc: e.Src, FileSize: e.Size, FileHash: e.Hash})
		case proto.TypeChunk:
			l.fileOff += int64(len(e.Chunk))
			progress := float64(l.fileOff) / float64(l.fileSize)
			l.n.emit(Event{Type: "fileProgress", Peer: l.peer, FileHash: l.fileHash, Progress: progress})
			if l.fileOff >= l.fileSize {
				l.n.emit(Event{Type: "fileComplete", Peer: l.peer, FileHash: l.fileHash, FileSrc: l.fileName})
				l.fileHash = ""
				l.fileSize = 0
				l.fileName = ""
				l.fileOff = 0
			}
		}
	}
}
