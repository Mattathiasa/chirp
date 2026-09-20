package node

import (
	"sort"
	"strings"
	"time"

	"github.com/Mattathiasa/chirp/internal/identity"
	"github.com/Mattathiasa/chirp/internal/store"
)

// Trust states shown in the UI.
const (
	TrustUnknown  = "unknown"  // seen on the network, never handshaken
	TrustNew      = "new"      // pinned on first contact, not verified
	TrustVerified = "verified" // user confirmed the fingerprint out of band
	TrustChanged  = "changed"  // a different key claimed a pinned name
)

// Me describes this device.
type Me struct {
	Name      string   `json:"name"`
	FP        string   `json:"fp"`
	FPGroups  []string `json:"fpGroups"`
	Words     []string `json:"words"`
	Identicon []byte   `json:"identicon"`
	Port      int      `json:"port"`
}

// Me returns our own identity summary.
func (n *Node) Me() Me {
	w := identity.Words(n.id.Key.Public)
	return Me{
		Name: n.id.Name, FP: n.myFP, FPGroups: identity.FingerprintGroups(n.myFP),
		Words: w[:], Identicon: identity.IdenticonSeed(n.id.Key.Public), Port: n.Port(),
	}
}

// PeerView is everything the UI needs to draw one contact.
type PeerView struct {
	Name      string         `json:"name"`
	FP        string         `json:"fp"`
	FPGroups  []string       `json:"fpGroups,omitempty"`
	Words     []string       `json:"words,omitempty"`
	Identicon []byte         `json:"identicon,omitempty"`
	Trust     string         `json:"trust"`
	Online    bool           `json:"online"` // live authenticated session
	Nearby    bool           `json:"nearby"` // currently advertised on the LAN
	Addr      string         `json:"addr,omitempty"`
	FirstSeen time.Time      `json:"firstSeen,omitempty"`
	LastSeen  time.Time      `json:"lastSeen,omitempty"`
	Queued    int            `json:"queued"`
	Last      *store.Message `json:"last,omitempty"`

	// Populated only when Trust == "changed".
	NewFP       string   `json:"newFp,omitempty"`
	NewFPGroups []string `json:"newFpGroups,omitempty"`
	NewWords    []string `json:"newWords,omitempty"`
}

// Peers lists pinned peers plus anyone nearby who we have not met yet.
func (n *Node) Peers() ([]PeerView, error) {
	pinned, err := n.cfg.Store.ListPeers()
	if err != nil {
		return nil, err
	}
	pend, err := n.cfg.Store.Pending("")
	if err != nil {
		return nil, err
	}
	queued := map[string]int{}
	for _, m := range pend {
		queued[strings.ToLower(m.Peer)]++
	}

	n.mu.Lock()
	nearbyByName := map[string]*nearby{}
	for _, nb := range n.nearby {
		if nb.up {
			nearbyByName[strings.ToLower(nb.peer.Name)] = nb
		}
	}
	liveByName := map[string]*link{}
	for k, l := range n.live {
		liveByName[k] = l
	}
	n.mu.Unlock()

	var out []PeerView
	seen := map[string]bool{}
	for _, p := range pinned {
		k := strings.ToLower(p.Name)
		seen[k] = true
		w := identity.Words(p.Key)
		v := PeerView{
			Name: p.Name, FP: p.FP, FPGroups: identity.FingerprintGroups(p.FP), Words: w[:],
			Identicon: identity.IdenticonSeed(p.Key), FirstSeen: p.FirstSeen, LastSeen: p.LastSeen,
			Trust: TrustNew, Queued: queued[k],
		}
		if p.Verified {
			v.Trust = TrustVerified
		}
		if p.PendingKey != nil {
			v.Trust = TrustChanged
			v.NewFP = identity.Fingerprint(p.PendingKey)
			v.NewFPGroups = identity.FingerprintGroups(v.NewFP)
			nw := identity.Words(p.PendingKey)
			v.NewWords = nw[:]
		}
		if l := liveByName[k]; l != nil {
			v.Online, v.Addr = true, l.conn.RemoteAddr().String()
		}
		_, v.Nearby = nearbyByName[k]
		if ms, err := n.cfg.Store.Messages(p.Name, 1); err == nil && len(ms) == 1 {
			v.Last = &ms[0]
		}
		out = append(out, v)
	}
	for k, nb := range nearbyByName {
		if seen[k] {
			continue
		}
		out = append(out, PeerView{
			Name: nb.peer.Name, FP: nb.peer.FP, FPGroups: identity.FingerprintGroups(nb.peer.FP),
			Trust: TrustUnknown, Nearby: true, LastSeen: nb.seen,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// Verify marks a peer's pinned key as verified (or clears it).
func (n *Node) Verify(name string, v bool) error {
	if err := n.cfg.Store.SetVerified(name, v); err != nil {
		return err
	}
	n.emit(Event{Type: "peers", Peer: name})
	return nil
}

// AcceptKey re-pins a peer to the key that most recently claimed its name.
// The peer's live session, if any, is unaffected (none can exist while changed);
// the dialer will connect on its next tick.
func (n *Node) AcceptKey(name string) error {
	if err := n.cfg.Store.AcceptPendingKey(name); err != nil {
		return err
	}
	n.emit(Event{Type: "peers", Peer: name})
	return nil
}

// Forget removes a peer and all messages with it, and drops any live session.
func (n *Node) Forget(name string) error {
	if l := n.linkFor(name); l != nil {
		l.conn.Close()
	}
	if err := n.cfg.Store.ForgetPeer(name); err != nil {
		return err
	}
	n.emit(Event{Type: "peers", Peer: name})
	return nil
}

// Messages returns recent messages with a peer.
func (n *Node) Messages(peer string, limit int) ([]store.Message, error) {
	return n.cfg.Store.Messages(peer, limit)
}

// Outbox returns every undelivered message.
func (n *Node) Outbox() ([]store.Message, error) { return n.cfg.Store.Pending("") }

// Session is a live link for diagnostics.
type Session struct {
	Peer  string    `json:"peer"`
	Addr  string    `json:"addr"`
	Since time.Time `json:"since"`
}

// Diag is the network diagnostics view.
type Diag struct {
	Listen   string     `json:"listen"`
	Uptime   int64      `json:"uptimeSec"`
	Nearby   int        `json:"nearby"`
	Sessions []Session  `json:"sessions"`
	Log      []LogEntry `json:"log"`
}

// Diagnostics reports engine state, newest log entries last.
func (n *Node) Diagnostics() Diag {
	n.mu.Lock()
	defer n.mu.Unlock()
	d := Diag{Listen: n.ln.Addr().String(), Uptime: int64(time.Since(n.started).Seconds())}
	for _, nb := range n.nearby {
		if nb.up {
			d.Nearby++
		}
	}
	for _, l := range n.live {
		d.Sessions = append(d.Sessions, Session{Peer: l.peer, Addr: l.conn.RemoteAddr().String(), Since: l.since})
	}
	d.Log = append([]LogEntry(nil), n.log...)
	return d
}
