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

	flushMu sync.Mutex // serialises outbox flushes on this link

	// Inbound file reception state.
	rxFileID   string
	rxFileOff  int64
	rxFileHash string
	rxFileSize int64
	rxFileName string

	// Outbound transfers waiting to learn where to resume from.
	ackMu   sync.Mutex
	fileAck map[string]chan int64
}

// speaks reports whether the peer advertised a capability in its handshake.
func (l *link) speaks(cap string) bool {
	for _, c := range l.conn.RemoteCaps {
		if c == cap {
			return true
		}
	}
	return false
}

// awaitFileAck registers interest in the resume offset for a transfer. It must
// be called before the header goes out, or the answer can arrive first.
func (l *link) awaitFileAck(id string) chan int64 {
	l.ackMu.Lock()
	defer l.ackMu.Unlock()
	if l.fileAck == nil {
		l.fileAck = make(map[string]chan int64)
	}
	ch := make(chan int64, 1)
	l.fileAck[id] = ch
	return ch
}

func (l *link) cancelFileAck(id string) {
	l.ackMu.Lock()
	defer l.ackMu.Unlock()
	delete(l.fileAck, id)
}

func (l *link) deliverFileAck(id string, offset int64) {
	l.ackMu.Lock()
	ch := l.fileAck[id]
	l.ackMu.Unlock()
	if ch == nil {
		return // nothing waiting; a late or unsolicited ack is harmless
	}
	select {
	case ch <- offset:
	default: // already answered
	}
}

// progressOf is a 0..1 fraction that does not divide by zero.
func progressOf(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(done) / float64(total)
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
			if e.Room != "" {
				l.n.handleRoomReaction(l, e)
				break
			}
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
			// BeginReceive keeps a matching partial and tells us where it got
			// to, so an interrupted transfer picks up instead of starting over.
			resume, err := l.n.cfg.Store.BeginReceive(e.ID, l.peer, e.Src, e.Hash, e.Size)
			if err != nil {
				l.n.logf("begin receive %s: %v", e.ID, err)
				l.conn.Close()
				return
			}
			l.rxFileID = e.ID
			l.rxFileOff = resume
			l.rxFileHash = e.Hash
			l.rxFileSize = e.Size
			l.rxFileName = e.Src
			if resume > 0 {
				l.n.logf("resuming %s from %d of %d bytes", e.Src, resume, e.Size)
			}
			// Only answer a sender that asked for one: an older peer does not
			// know this type, and an unknown type is fatal to the session.
			if l.speaks(proto.CapResume) {
				if err := l.send(proto.Envelope{T: proto.TypeFileAck, ID: e.ID, Offset: resume}); err != nil {
					l.n.logf("fileack to %s: %v", l.peer, err)
					return
				}
			}
			l.n.emit(Event{Type: "file", Peer: l.peer, FileID: e.ID, FileSrc: e.Src, FileSize: e.Size, FileHash: e.Hash, Progress: progressOf(resume, e.Size)})
		case proto.TypeFileAck:
			l.deliverFileAck(e.ID, e.Offset)
		case proto.TypeChunk:
			if e.ID != l.rxFileID {
				continue // chunk for an unknown/older transfer; ignore
			}
			f, err := l.n.cfg.Store.WriteChunk(e.ID, e.Offset, e.Chunk)
			if err != nil {
				// A gap means the stream is not what we think it is. Drop the
				// session rather than assemble a file we would only discover
				// was wrong at the final hash check.
				l.n.logf("write chunk %s: %v", e.ID, err)
				l.conn.Close()
				return
			}
			l.rxFileOff = f.Received
			progress := progressOf(l.rxFileOff, l.rxFileSize)
			l.n.emit(Event{Type: "fileProgress", Peer: l.peer, FileID: e.ID, FileHash: l.rxFileHash, Progress: progress})
			if l.rxFileOff >= l.rxFileSize {
				if err := l.n.cfg.Store.CompleteFile(e.ID); err != nil {
					l.n.logf("complete file %s: %v", e.ID, err)
				} else {
					l.n.emit(Event{Type: "fileComplete", Peer: l.peer, FileID: e.ID, FileHash: l.rxFileHash, FileSrc: l.rxFileName})
				}
				l.rxFileID = ""
				l.rxFileOff = 0
				l.rxFileHash = ""
				l.rxFileSize = 0
				l.rxFileName = ""
			}
		case proto.TypeRoomMsg:
			l.n.handleRoomMsg(l, e)
		case proto.TypeRoomAck:
			l.n.handleRoomAck(l, e)
		case proto.TypeRoomEvent:
			l.n.handleRoomEvent(l, e)
		}
	}
}
