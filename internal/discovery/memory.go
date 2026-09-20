package discovery

import (
	"context"
	"fmt"
	"sync"
)

// Hub connects any number of in-memory Discovery instances, standing in for a
// LAN in tests. Announcements are delivered to every browser, including
// browsers that start later.
type Hub struct {
	mu       sync.Mutex
	next     int
	announce map[int]Announcement
	browsers map[int]chan Event
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{announce: map[int]Announcement{}, browsers: map[int]chan Event{}}
}

// New returns a Discovery attached to the hub. Every announced service is
// reported as reachable at 127.0.0.1:<port>.
func (h *Hub) New() Discovery { return &mem{hub: h} }

type mem struct {
	hub *Hub
	id  int
}

func toPeer(a Announcement) Peer {
	return Peer{Name: a.Name, FP: a.FP, Addrs: []string{fmt.Sprintf("127.0.0.1:%d", a.Port)}}
}

func (m *mem) Announce(ctx context.Context, a Announcement) error {
	h := m.hub
	h.mu.Lock()
	h.next++
	id := h.next
	h.announce[id] = a
	for bid, ch := range h.browsers {
		if bid != m.id {
			trySend(ch, Event{Up: true, Peer: toPeer(a)})
		}
	}
	h.mu.Unlock()
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		delete(h.announce, id)
		for bid, ch := range h.browsers {
			if bid != m.id {
				trySend(ch, Event{Up: false, Peer: toPeer(a)})
			}
		}
		h.mu.Unlock()
	}()
	return nil
}

func (m *mem) Browse(ctx context.Context) (<-chan Event, error) {
	h := m.hub
	ch := make(chan Event, 64)
	h.mu.Lock()
	h.next++
	m.id = h.next
	h.browsers[m.id] = ch
	for _, a := range h.announce {
		trySend(ch, Event{Up: true, Peer: toPeer(a)})
	}
	h.mu.Unlock()
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		delete(h.browsers, m.id)
		close(ch)
		h.mu.Unlock()
	}()
	return ch, nil
}

func trySend(ch chan Event, e Event) {
	select {
	case ch <- e:
	default: // slow consumer: drop, like a lossy multicast network
	}
}
