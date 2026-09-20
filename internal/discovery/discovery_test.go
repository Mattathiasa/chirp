package discovery

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
)

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
		return Event{}
	}
}

func TestMemoryHub(t *testing.T) {
	h := NewHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := h.New(), h.New()
	chA, _ := a.Browse(ctx)
	chB, _ := b.Browse(ctx)
	actxA, cancelA := context.WithCancel(ctx)
	a.Announce(actxA, Announcement{Name: "Alex", FP: "aa", Port: 1111})
	if e := recv(t, chB); !e.Up || e.Peer.Name != "Alex" || e.Peer.Addrs[0] != "127.0.0.1:1111" {
		t.Fatalf("%+v", e)
	}
	select {
	case e := <-chA:
		t.Fatalf("saw own announcement: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
	// Late browser still learns about existing peers.
	c := h.New()
	chC, _ := c.Browse(ctx)
	if e := recv(t, chC); e.Peer.Name != "Alex" {
		t.Fatalf("%+v", e)
	}
	cancelA()
	if e := recv(t, chB); e.Up {
		t.Fatalf("expected down: %+v", e)
	}
}

func TestFromEntryRejectsBadInput(t *testing.T) {
	fp := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	mk := func(txt []string, port int) *zeroconf.ServiceEntry {
		e := zeroconf.NewServiceEntry("x", Service, "local.")
		e.Text, e.Port, e.AddrIPv4 = txt, port, []net.IP{net.ParseIP("192.168.1.9")}
		return e
	}
	if p, ok := fromEntry(mk([]string{"v=1", "name=Sam", "fp=" + fp}, 4000)); !ok || p.Addrs[0] != "192.168.1.9:4000" {
		t.Fatalf("good entry rejected: %+v %v", p, ok)
	}
	for _, bad := range []*zeroconf.ServiceEntry{
		mk([]string{"v=2", "name=Sam", "fp=" + fp}, 4000),
		mk([]string{"v=1", "fp=" + fp}, 4000),
		mk([]string{"v=1", "name=Sam", "fp=short"}, 4000),
		mk([]string{"v=1", "name=Sam", "fp=" + fp}, 0),
		mk([]string{"v=1", "name=Sam", "fp=" + fp}, 70000),
	} {
		if _, ok := fromEntry(bad); ok {
			t.Errorf("accepted %+v", bad)
		}
	}
}
