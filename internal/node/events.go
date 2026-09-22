package node

import (
	"fmt"
	"time"

	"github.com/Mattathiasa/chirp/internal/store"
)

// Event is pushed to subscribers (the SSE endpoint).
type Event struct {
	Type    string         `json:"type"` // "peers" | "message" | "status" | "outbox" | "reaction" | "typing" | "read" | "file" | "fileProgress" | "fileComplete" | "me" | "fileOutbox"
	Peer    string         `json:"peer,omitempty"`
	Message *store.Message `json:"message,omitempty"`

	// Reaction fields
	Emoji  string `json:"emoji,omitempty"`
	Target string `json:"target,omitempty"` // message ID being reacted to

	// File transfer fields
	FileID   string  `json:"fileId,omitempty"`
	FileSrc  string  `json:"fileSrc,omitempty"`
	FileSize int64   `json:"fileSize,omitempty"`
	FileHash string  `json:"fileHash,omitempty"`
	Progress float64 `json:"progress,omitempty"`
}

// Subscribe returns a channel of events and a function to unsubscribe. Slow
// subscribers lose events rather than blocking the engine; clients resync by
// refetching state after reconnecting.
func (n *Node) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	n.mu.Lock()
	id := n.nextSub
	n.nextSub++
	n.subs[id] = ch
	n.mu.Unlock()
	return ch, func() {
		n.mu.Lock()
		if _, ok := n.subs[id]; ok {
			delete(n.subs, id)
			close(ch)
		}
		n.mu.Unlock()
	}
}

func (n *Node) emit(e Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, ch := range n.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// LogEntry is one line of the diagnostics log.
type LogEntry struct {
	At  time.Time `json:"at"`
	Msg string    `json:"msg"`
}

const logCap = 200

func (n *Node) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	n.cfg.Logf("%s", msg)
	n.mu.Lock()
	n.log = append(n.log, LogEntry{At: time.Now(), Msg: msg})
	if len(n.log) > logCap {
		n.log = n.log[len(n.log)-logCap:]
	}
	n.mu.Unlock()
}

// Emit lets the API layer publish an event (for example after a bulk delete).
func (n *Node) Emit(e Event) { n.emit(e) }
