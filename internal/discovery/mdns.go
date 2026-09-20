package discovery

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/grandcat/zeroconf"
)

// MDNS is the production Discovery, backed by multicast DNS.
type MDNS struct {
	// Interfaces restricts multicast to specific interfaces; nil means all
	// multicast-capable ones.
	Interfaces []net.Interface
}

// Announce registers the service. The DNS-SD instance name is the fingerprint
// prefix, which is unique per key, so display-name collisions between two
// people cannot break registration. The display name travels in the TXT record.
func (m *MDNS) Announce(ctx context.Context, a Announcement) error {
	if len(a.FP) < 16 {
		return fmt.Errorf("mdns: fingerprint too short")
	}
	txt := []string{"v=1", "name=" + a.Name, "fp=" + a.FP}
	srv, err := zeroconf.Register(a.FP[:16], Service, "local.", a.Port, txt, m.Interfaces)
	if err != nil {
		return fmt.Errorf("mdns: register: %w", err)
	}
	go func() {
		<-ctx.Done()
		srv.Shutdown()
	}()
	return nil
}

// Browse streams discovered peers. Entries with a zero TTL are goodbye packets.
func (m *MDNS) Browse(ctx context.Context) (<-chan Event, error) {
	res, err := zeroconf.NewResolver(zeroconf.SelectIfaces(m.Interfaces))
	if err != nil {
		// Hosts with IPv6 disabled (some containers, hardened laptops) cannot
		// join the IPv6 multicast group. Chirp only needs IPv4 to work.
		res, err = zeroconf.NewResolver(zeroconf.SelectIfaces(m.Interfaces), zeroconf.SelectIPTraffic(zeroconf.IPv4))
	}
	if err != nil {
		return nil, fmt.Errorf("mdns: resolver: %w", err)
	}
	entries := make(chan *zeroconf.ServiceEntry, 32)
	out := make(chan Event, 32)
	if err := res.Browse(ctx, Service, "local.", entries); err != nil {
		return nil, fmt.Errorf("mdns: browse: %w", err)
	}
	go func() {
		defer close(out)
		for e := range entries {
			p, ok := fromEntry(e)
			if !ok {
				continue
			}
			select {
			case out <- Event{Up: e.TTL > 0, Peer: p}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// fromEntry parses a service entry. Everything in it is untrusted network input.
func fromEntry(e *zeroconf.ServiceEntry) (Peer, bool) {
	txt := map[string]string{}
	for _, kv := range e.Text {
		if k, v, ok := strings.Cut(kv, "="); ok {
			txt[k] = v
		}
	}
	if txt["v"] != "1" || txt["name"] == "" || len(txt["fp"]) != 64 || e.Port <= 0 || e.Port > 65535 {
		return Peer{}, false
	}
	p := Peer{Name: txt["name"], FP: txt["fp"]}
	port := strconv.Itoa(e.Port)
	for _, ip := range e.AddrIPv4 {
		p.Addrs = append(p.Addrs, net.JoinHostPort(ip.String(), port))
	}
	for _, ip := range e.AddrIPv6 {
		if ip.IsLinkLocalUnicast() {
			continue // needs a zone; skip rather than guess the interface
		}
		p.Addrs = append(p.Addrs, net.JoinHostPort(ip.String(), port))
	}
	return p, len(p.Addrs) > 0
}
