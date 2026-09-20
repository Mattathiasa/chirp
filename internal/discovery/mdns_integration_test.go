package discovery

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Real multicast needs a network that permits it, so this only runs on request:
//
//	CHIRP_MDNS_TEST=1 go test ./internal/discovery -run MDNS -v
func TestMDNSRoundTrip(t *testing.T) {
	if os.Getenv("CHIRP_MDNS_TEST") == "" {
		t.Skip("set CHIRP_MDNS_TEST=1 to run against real multicast")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fp := strings.Repeat("ab", 32)
	if err := (&MDNS{}).Announce(ctx, Announcement{Name: "Alex", FP: fp, Port: 4242}); err != nil {
		t.Fatal(err)
	}
	ch, err := (&MDNS{}).Browse(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case e := <-ch:
			if e.Up && e.Peer.FP == fp {
				t.Logf("found %+v", e.Peer)
				return
			}
		case <-ctx.Done():
			t.Fatal("service never appeared: multicast blocked here?")
		}
	}
}
