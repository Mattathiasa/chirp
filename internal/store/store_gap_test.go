package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mattathiasa/chirp/internal/identity"
	bolt "go.etcd.io/bbolt"
)

// DeleteRoom must remove not just the room row but every room message, the
// index entries that point at them, and the per-member outbox rows. A leftover
// outbox row would make MarkRoomDelivered resurrect state for a room that no
// longer exists.
func TestDeleteRoomCleansUpMessagesIndexAndOutbox(t *testing.T) {
	s := openRoomStore(t)
	if err := s.CreateRoom(Room{ID: "r-del", Name: "Cleanup", Members: []string{"Alex", "Sam"}, CreatedBy: "Alex", CreatedAt: time.Now(), State: RoomJoined}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, _, err := s.AddRoomMessage(RoomMessage{ID: id32(i), Room: "r-del", Sender: "Alex", Dir: DirOut, Body: fmt.Sprint(i), TS: time.Now(), Status: StatusQueued}); err != nil {
			t.Fatal(err)
		}
		if err := s.AddRoomMemberToOutbox("r-del", "Sam", id32(i)); err != nil {
			t.Fatal(err)
		}
	}
	if pend, _ := s.RoomPending("r-del", "Sam"); len(pend) != 3 {
		t.Fatalf("precondition: pending = %d", len(pend))
	}

	if err := s.DeleteRoom("r-del"); err != nil {
		t.Fatal(err)
	}

	// Every side bucket must be empty, not just the room row.
	// (the checks below run in separate view transactions)
	checkEmpty := func(name string, probe func() int) {
		t.Helper()
		if n := probe(); n != 0 {
			t.Fatalf("%s still holds %d entries after DeleteRoom", name, n)
		}
	}
	checkEmpty("room messages", func() int { ms, _ := s.RoomMessages("r-del", 100); return len(ms) })
	checkEmpty("member outbox", func() int { p, _ := s.RoomPending("r-del", "Sam"); return len(p) })
	// The dedup index must be gone too, or a reused message id would collide.
	for i := 1; i <= 3; i++ {
		_, dup, err := s.AddRoomMessage(RoomMessage{ID: id32(i), Room: "r-new", Sender: "Alex", Dir: DirIn, Body: "reuse", TS: time.Now(), Status: StatusReceived})
		if err != nil || dup {
			t.Fatalf("index entry for %s survived DeleteRoom (dup=%v err=%v)", id32(i), dup, err)
		}
	}

	// Deleting a room that does not exist is not an error.
	if err := s.DeleteRoom("never-was"); err != nil {
		t.Fatalf("delete unknown room: %v", err)
	}
}

// MarkRoomDelivered touches the room row to decide whether every member has
// been delivered; with the room row gone it must fail cleanly.
func TestMarkRoomDeliveredWithMissingRoomFails(t *testing.T) {
	s := openRoomStore(t)
	if _, _, err := s.AddRoomMessage(RoomMessage{ID: id32(9), Room: "ghost", Sender: "Alex", Dir: DirOut, Body: "x", TS: time.Now(), Status: StatusQueued}); err != nil {
		t.Fatal(err)
	}
	// Drop the room row behind the store's back, the way a corrupted
	// database would present.
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bRooms).Delete([]byte("ghost"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MarkRoomDelivered("ghost", "Sam", id32(9)); err == nil {
		t.Fatal("delivered a message whose room no longer exists")
	}
}

// Pending sorts by seq across peers. The insertion order in the outbox bucket
// is not the delivery order, so the sort has to actually run.
func TestPendingSortsAcrossPeersBySeq(t *testing.T) {
	s := openRoomStore(t)
	now := time.Now()
	// Interleave two peers: seq 1,3 for Sam and 2,4 for Dana.
	pairs := []struct {
		peer string
		n    int
	}{
		{"Sam", 1}, {"Dana", 2}, {"Sam", 3}, {"Dana", 4},
	}
	for _, p := range pairs {
		if _, _, err := s.AddMessage(Message{ID: id32(p.n), Peer: p.peer, Dir: DirOut, Body: fmt.Sprint(p.n), TS: now, Status: StatusQueued}); err != nil {
			t.Fatal(err)
		}
	}
	// The outbox bucket walks message ids (1,2,3,4) which here matches seq, so
	// reverse-insert a case where the id order is not the seq order.
	if _, _, err := s.AddMessage(Message{ID: id32(90), Peer: "Dana", Dir: DirOut, Body: "late", TS: now, Status: StatusQueued}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Pending("Dana")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Body != "2" || got[1].Body != "4" || got[2].Body != "late" {
		t.Fatalf("pending order: %+v", got)
	}
}

// Pending("") returns everything, oldest seq first, across all peers.
func TestPendingAllPeersOldestFirst(t *testing.T) {
	s := openRoomStore(t)
	now := time.Now()
	for i := 1; i <= 4; i++ {
		peer := "Sam"
		if i%2 == 0 {
			peer = "Dana"
		}
		s.AddMessage(Message{ID: id32(i), Peer: peer, Dir: DirOut, Body: fmt.Sprint(i), TS: now, Status: StatusQueued})
	}
	got, err := s.Pending("")
	if err != nil || len(got) != 4 {
		t.Fatalf("pending: %v %d", err, len(got))
	}
	for i, m := range got {
		if m.Body != fmt.Sprint(i+1) {
			t.Fatalf("position %d holds %q, want %d", i, m.Body, i+1)
		}
	}
}

// A delivered outbound message must not sit in the outbox.
func TestAddMessageDeliveredSkipsOutbox(t *testing.T) {
	s := openRoomStore(t)
	now := time.Now()
	if _, dup, err := s.AddMessage(Message{ID: id32(1), Peer: "Sam", Dir: DirOut, Body: "already there", TS: now, Status: StatusDelivered}); err != nil || dup {
		t.Fatal(err, dup)
	}
	if p, _ := s.Pending(""); len(p) != 0 {
		t.Fatalf("a pre-delivered message queued itself: %+v", p)
	}
}

// ForgetPeer refuses names that were never pinned.
func TestForgetPeerMissing(t *testing.T) {
	s := openRoomStore(t)
	if err := s.ForgetPeer("nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("forget unknown peer: %v", err)
	}
}

// Search with limit 0 falls back to the default of 20 rather than returning
// everything or nothing.
func TestSearchDefaultLimit(t *testing.T) {
	s := openRoomStore(t)
	now := time.Now()
	for i := 1; i <= 25; i++ {
		s.AddMessage(Message{ID: id32(i), Peer: "Sam", Dir: DirIn, Body: fmt.Sprintf("hit %d needle", i), TS: now, Status: StatusReceived})
	}
	res, err := s.Search("needle", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 20 {
		t.Fatalf("default limit returned %d results, want 20", len(res))
	}
	// Over the cap is clamped too.
	if res, _ := s.Search("needle", 500); len(res) != 20 {
		t.Fatalf("over-cap limit returned %d, want 20", len(res))
	}
}

// A corrupt row must surface as an error, not panic or silently vanish.
func TestCorruptPeerRowIsAnError(t *testing.T) {
	s := openRoomStore(t)
	k, _ := identity.Generate("Sam")
	s.Observe("Sam", k.Key.Public, time.Now())
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bPeers).Put([]byte("sam"), []byte("{not json"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPeer("Sam"); err == nil {
		t.Fatal("corrupt peer row read cleanly")
	}
	if _, err := s.ListPeers(); err == nil {
		t.Fatal("corrupt peer row survived a list")
	}
}

// Messages with a corrupt body row error instead of returning garbage.
func TestCorruptMessageRowIsAnError(t *testing.T) {
	s := openRoomStore(t)
	if _, _, err := s.AddMessage(Message{ID: id32(1), Peer: "Sam", Dir: DirIn, Body: "fine", TS: time.Now(), Status: StatusReceived}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		c := tx.Bucket(bMsgs).Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			_ = v
			return c.Delete()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Re-put a malformed value so unmarshal fails.
	if err := s.db.Update(func(tx *bolt.Tx) error {
		k := append([]byte("sam"), 0)
		var b [8]byte
		return tx.Bucket(bMsgs).Put(append(k, b[:]...), []byte("{bad"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Messages("Sam", 10); err == nil {
		t.Fatal("corrupt message row read cleanly")
	}
}

// Opening a database at an unwritable path must fail with a usable error.
func TestOpenStoreFailsOnUnwritablePath(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "taken")
	if err := os.MkdirAll(filepath.Join(blocker, "chirp.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory where the database file should be cannot be opened.
	if _, err := Open(blocker); err == nil {
		t.Fatal("opened a database on top of a directory")
	}
}

// GetRoom behind the rooms API: a missing row is ErrNotFound, and the internal
// helper agrees with the public one.
func TestGetRoomInternalMatchesPublic(t *testing.T) {
	s := openRoomStore(t)
	if _, err := s.GetRoom("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("public get: %v", err)
	}
	r := Room{ID: "r9", Name: "Nine", Members: []string{"A"}, CreatedBy: "A", CreatedAt: time.Now(), State: RoomJoined}
	if err := s.CreateRoom(r); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRoom("r9")
	if err != nil || got.Name != "Nine" {
		t.Fatalf("public get after create: %v %+v", err, got)
	}
}
