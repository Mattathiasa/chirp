// Package discovery finds Chirp peers on the local network. The node depends
// only on the Discovery interface, so tests run against an in-memory
// implementation and production uses mDNS.
package discovery

import "context"

// Service is the DNS-SD service type Chirp advertises.
const Service = "_chirp._tcp"

// Announcement is what we publish about ourselves.
type Announcement struct {
	Name string // display name (also the pinning handle)
	FP   string // hex SHA-256 of our public key; advisory only, verified in the handshake
	Port int
}

// Peer is a discovered service.
type Peer struct {
	Name  string
	FP    string
	Addrs []string // host:port candidates
}

// Event reports a peer appearing or going away.
type Event struct {
	Up   bool
	Peer Peer
}

// Discovery announces this node and reports others.
type Discovery interface {
	// Announce publishes a until ctx is cancelled.
	Announce(ctx context.Context, a Announcement) error
	// Browse streams peer events until ctx is cancelled. The channel is closed then.
	Browse(ctx context.Context) (<-chan Event, error)
}
