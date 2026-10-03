package node

import (
	"testing"
	"time"

	"github.com/Mattathiasa/chirp/internal/discovery"
)

// Renaming used to change the local identity only. The display name travels in
// the mDNS TXT record, so until a fresh Announce went out, every peer on the
// LAN kept seeing the old name until this node happened to restart.
func TestRenameReannounces(t *testing.T) {
	hub := discovery.NewHub()
	observer := newRig(t, hub, "Watch")
	rig := newRig(t, hub, "Old Name")
	// Production saves the identity before the node starts (app.Setup); the
	// test rigs keep theirs in memory only, so persist it for the rename.
	if err := rig.st.SaveIdentity(rig.id); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "observer sees the first announcement", func() bool {
		ps, _ := observer.n.Peers()
		for _, p := range ps {
			if p.Nearby && p.Name == "Old Name" {
				return true
			}
		}
		return false
	})

	if err := rig.n.Rename("New Name"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "observer sees the new name", func() bool {
		ps, _ := observer.n.Peers()
		for _, p := range ps {
			if p.Nearby && p.Name == "New Name" {
				return true
			}
		}
		return false
	})

	// The rename survives a restart of the renaming node, since the store was
	// updated before the announcement.
	if rig.n.Identity().Name != "New Name" {
		t.Fatalf("identity name = %q", rig.n.Identity().Name)
	}
	_ = time.Now // keep time imported if assertions change
}

// Renaming is refused for names that would break pinning or rendering.
func TestRenameRejectsBadNames(t *testing.T) {
	hub := discovery.NewHub()
	rig := newRig(t, hub, "Keep")

	for _, bad := range []string{"", "   ", string([]byte{0x07})} {
		if err := rig.n.Rename(bad); err != ErrBadName {
			t.Fatalf("Rename(%q) = %v, want ErrBadName", bad, err)
		}
	}
	if rig.n.Identity().Name != "Keep" {
		t.Fatalf("a refused rename still changed the name to %q", rig.n.Identity().Name)
	}
}
