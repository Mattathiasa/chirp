package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Mattathiasa/chirp/internal/identity"
	bolt "go.etcd.io/bbolt"
)

// ---- rooms ----

func openRoomStore(t *testing.T) *Store {
	t.Helper()
	s, _ := open(t)
	return s
}

func TestRoomLifecycle(t *testing.T) {
	s := openRoomStore(t)

	r := Room{
		ID: "room-1", Name: "Weekend", Members: []string{"Alex", "Sam"},
		CreatedBy: "Alex", CreatedAt: time.Now(), State: RoomJoined,
	}
	if err := s.CreateRoom(r); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRoom("room-1")
	if err != nil || got.Name != "Weekend" || len(got.Members) != 2 {
		t.Fatalf("get: %v %+v", err, got)
	}
	if got.Pending() {
		t.Fatal("joined room reported pending")
	}

	// Update.
	got.Name = "Long Weekend"
	if err := s.UpdateRoom(*got); err != nil {
		t.Fatal(err)
	}
	if r2, _ := s.GetRoom("room-1"); r2.Name != "Long Weekend" {
		t.Fatalf("update lost: %+v", r2)
	}

	// List.
	rooms, err := s.ListRooms()
	if err != nil || len(rooms) != 1 || rooms[0].Name != "Long Weekend" {
		t.Fatalf("list: %v %+v", err, rooms)
	}

	// Missing room.
	if _, err := s.GetRoom("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing room: %v", err)
	}

	// Delete removes the room and its messages.
	if err := s.DeleteRoom("room-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRoom("room-1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("room survived delete")
	}
	if ms, _ := s.RoomMessages("room-1", 10); len(ms) != 0 {
		t.Fatal("messages survived delete")
	}
}

// Rows written before rooms had a state field are read as joined, not pending.
func TestRoomWithoutStateReadsAsJoined(t *testing.T) {
	s := openRoomStore(t)
	if err := s.CreateRoom(Room{ID: "r", Name: "Old", Members: []string{"A"}, CreatedBy: "A", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r, _ := s.GetRoom("r")
	if r.Pending() {
		t.Fatal("legacy row read as pending")
	}
}

// A pending room is an unanswered invitation.
func TestRoomPendingState(t *testing.T) {
	s := openRoomStore(t)
	if err := s.CreateRoom(Room{ID: "r", Name: "Invited", Members: []string{"A", "B"}, CreatedBy: "A", CreatedAt: time.Now(), State: RoomPending}); err != nil {
		t.Fatal(err)
	}
	r, _ := s.GetRoom("r")
	if !r.Pending() {
		t.Fatal("pending state lost")
	}
}

func TestRoomMemberCap(t *testing.T) {
	s := openRoomStore(t)
	tooBig := Room{ID: "big", Name: "Big", CreatedBy: "A", CreatedAt: time.Now()}
	for i := 0; i < MaxRoomMembers+1; i++ {
		tooBig.Members = append(tooBig.Members, fmt.Sprintf("m%d", i))
	}
	if err := s.CreateRoom(tooBig); err == nil {
		t.Fatal("over-cap room accepted")
	}
}

// ---- room messages ----

func TestRoomMessageDedupOrderAndDelivery(t *testing.T) {
	s := openRoomStore(t)
	if err := s.CreateRoom(Room{ID: "r", Name: "N", Members: []string{"Alex", "Sam", "Dana"}, CreatedBy: "Alex", CreatedAt: time.Now(), State: RoomJoined}); err != nil {
		t.Fatal(err)
	}

	m1 := RoomMessage{ID: id32(1), Room: "r", Sender: "Alex", Dir: DirOut, Body: "first", TS: time.Now(), Status: StatusQueued}
	stored, dup, err := s.AddRoomMessage(m1)
	if err != nil || dup {
		t.Fatalf("add: %v %v", err, dup)
	}
	if stored.Seq == 0 {
		t.Fatal("no seq assigned")
	}
	// Same ID again is a duplicate and stores nothing.
	if _, dup, _ := s.AddRoomMessage(m1); !dup {
		t.Fatal("duplicate room message stored")
	}
	_, _, _ = s.AddRoomMessage(RoomMessage{ID: id32(2), Room: "r", Sender: "Alex", Dir: DirOut, Body: "second", TS: time.Now(), Status: StatusQueued})
	_, _, _ = s.AddRoomMessage(RoomMessage{ID: id32(3), Room: "r", Sender: "Alex", Dir: DirOut, Body: "third", TS: time.Now(), Status: StatusQueued})

	ms, err := s.RoomMessages("r", 0)
	if err != nil || len(ms) != 3 {
		t.Fatalf("messages: %v %d", err, len(ms))
	}
	if ms[0].Body != "first" || ms[2].Body != "third" {
		t.Fatalf("out of order: %+v", ms)
	}

	// Per-member outbox drives delivery state.
	for _, mb := range []string{"Sam", "Dana"} {
		if err := s.AddRoomMemberToOutbox("r", mb, id32(1)); err != nil {
			t.Fatal(err)
		}
	}
	pend, err := s.RoomPending("r", "sam")
	if err != nil || len(pend) != 1 || pend[0].Body != "first" {
		t.Fatalf("pending for sam: %v %+v", err, pend)
	}
	if pend, _ := s.RoomPending("r", "nobody"); len(pend) != 0 {
		t.Fatal("pending for a non-member")
	}

	// One ack leaves the other member pending.
	if _, changed, err := s.MarkRoomDelivered("r", "Sam", id32(1)); err != nil || changed {
		t.Fatalf("first ack: %v %v", changed, err)
	}
	if got, _ := s.GetRoomMessageStatus(id32(1)); got != StatusQueued {
		t.Fatalf("status after one ack: %q", got)
	}
	// Final ack flips the message to delivered.
	m, changed, err := s.MarkRoomDelivered("r", "Dana", id32(1))
	if err != nil || !changed || m.Status != StatusDelivered {
		t.Fatalf("final ack: %v %v %+v", changed, err, m)
	}
	// A duplicate ack changes nothing.
	if _, changed, _ := s.MarkRoomDelivered("r", "Dana", id32(1)); changed {
		t.Fatal("duplicate ack reported a change")
	}
	// Unknown message.
	if _, _, err := s.MarkRoomDelivered("r", "Sam", "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

// GetRoomMessageStatus is a small helper used by the tests above.
func (s *Store) GetRoomMessageStatus(id string) (string, error) {
	var st string
	err := s.db.View(func(tx *bolt.Tx) error {
		k := tx.Bucket(bRoomIdx).Get([]byte(id))
		if k == nil {
			return ErrNotFound
		}
		var m RoomMessage
		if err := json.Unmarshal(tx.Bucket(bRoomMsgs).Get(k), &m); err != nil {
			return err
		}
		st = m.Status
		return nil
	})
	return st, err
}

func TestRoomSenderSeqIsDurableAndMonotonic(t *testing.T) {
	s := openRoomStore(t)
	for i := uint64(1); i <= 3; i++ {
		n, err := s.NextRoomSenderSeq("r", "Alex")
		if err != nil || n != i {
			t.Fatalf("seq %d: %d %v", i, n, err)
		}
	}
	// Different senders count independently.
	if n, _ := s.NextRoomSenderSeq("r", "Sam"); n != 1 {
		t.Fatalf("sam seq: %d", n)
	}
	// Different rooms count independently.
	if n, _ := s.NextRoomSenderSeq("r2", "Alex"); n != 1 {
		t.Fatalf("other room seq: %d", n)
	}
}

func TestRoomRecordAttempt(t *testing.T) {
	s := openRoomStore(t)
	_, _, err := s.AddRoomMessage(RoomMessage{ID: id32(1), Room: "r", Sender: "Alex", Dir: DirOut, Body: "x", TS: time.Now(), Status: StatusQueued})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 1; i <= 3; i++ {
		if err := s.RoomRecordAttempt(id32(1), now); err != nil {
			t.Fatal(err)
		}
		ms, _ := s.RoomMessages("r", 10)
		if ms[0].Attempts != i {
			t.Fatalf("attempts = %d, want %d", ms[0].Attempts, i)
		}
	}
	if err := s.RoomRecordAttempt("unknown", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestToggleRoomReaction(t *testing.T) {
	s := openRoomStore(t)
	_, _, err := s.AddRoomMessage(RoomMessage{ID: id32(1), Room: "r", Sender: "Alex", Dir: DirIn, Body: "x", TS: time.Now(), Status: StatusReceived})
	if err != nil {
		t.Fatal(err)
	}
	m, changed, err := s.ToggleRoomReaction("r", id32(1), "Sam", "👍")
	if err != nil || !changed || len(m.Reactions) != 1 {
		t.Fatalf("add: %v %v %+v", changed, err, m)
	}
	// Same sender, same emoji toggles it off.
	if _, changed, _ = s.ToggleRoomReaction("r", id32(1), "sam", "👍"); !changed {
		t.Fatal("remove reported no change")
	}
	m, _, _ = s.ToggleRoomReaction("r", id32(1), "Sam", "👍")
	// Case-insensitive sender, different emoji coexists.
	if _, changed, _ = s.ToggleRoomReaction("r", id32(1), "SAM", "🎉"); !changed {
		t.Fatal("second reaction reported no change")
	}
	if len(m.Reactions) != 1 {
		t.Fatalf("reactions = %+v", m.Reactions)
	}

	// Wrong room is refused.
	if _, _, err := s.ToggleRoomReaction("other", id32(1), "Sam", "👍"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong room: %v", err)
	}
	// Unknown message is refused.
	if _, _, err := s.ToggleRoomReaction("r", "unknown", "Sam", "👍"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown msg: %v", err)
	}
}

// Reactions are capped so a peer cannot grow a row without limit.
func TestRoomReactionCap(t *testing.T) {
	s := openRoomStore(t)
	_, _, _ = s.AddRoomMessage(RoomMessage{ID: id32(1), Room: "r", Sender: "Alex", Dir: DirIn, Body: "x", TS: time.Now(), Status: StatusReceived})
	for i := 0; i < MaxRoomReactions; i++ {
		if _, _, err := s.ToggleRoomReaction("r", id32(1), fmt.Sprintf("m%d", i), "👍"); err != nil {
			t.Fatalf("reaction %d: %v", i, err)
		}
	}
	if _, _, err := s.ToggleRoomReaction("r", id32(1), "overflow", "👍"); err == nil {
		t.Fatal("over-cap reaction accepted")
	}
}

// Room messages with encrypted-at-rest bodies round-trip.
func TestEncryptedRoomMessages(t *testing.T) {
	s, _, _ := openEncrypted(t)
	body := "room secret"
	_, dup, err := s.AddRoomMessage(RoomMessage{ID: id32(1), Room: "r", Sender: "Alex", Dir: DirIn, Body: body, TS: time.Now(), Status: StatusReceived})
	if err != nil || dup {
		t.Fatal(err, dup)
	}
	ms, _ := s.RoomMessages("r", 10)
	if len(ms) != 1 || ms[0].Body != body {
		t.Fatalf("roundtrip: %+v", ms)
	}
	// Room names are encrypted too.
	if err := s.CreateRoom(Room{ID: "r2", Name: "secret room name", Members: []string{"A"}, CreatedBy: "A", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRoom("r2"); r.Name != "secret room name" {
		t.Fatalf("room name: %q", r.Name)
	}
}

// ---- message-level gaps ----

func TestDeleteMessage(t *testing.T) {
	s := openRoomStore(t)
	now := time.Now()
	s.AddMessage(Message{ID: id32(1), Peer: "Sam", Dir: DirIn, Body: "a", TS: now, Status: StatusReceived})
	s.AddMessage(Message{ID: id32(2), Peer: "Sam", Dir: DirOut, Body: "b", TS: now, Status: StatusQueued})

	if err := s.DeleteMessage(id32(1)); err != nil {
		t.Fatal(err)
	}
	if ms, _ := s.Messages("Sam", 10); len(ms) != 1 || ms[0].Body != "b" {
		t.Fatalf("after delete: %+v", ms)
	}
	// The deleted outbound message left the outbox too.
	if p, _ := s.Pending("Sam"); len(p) != 1 {
		t.Fatalf("pending after delete: %d", len(p))
	}
	if err := s.DeleteMessage("unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestSearch(t *testing.T) {
	s := openRoomStore(t)
	now := time.Now()
	s.AddMessage(Message{ID: id32(1), Peer: "Sam", Dir: DirIn, Body: "the quick brown fox", TS: now, Status: StatusReceived})
	s.AddMessage(Message{ID: id32(2), Peer: "Sam", Dir: DirIn, Body: "jumps over the lazy dog", TS: now, Status: StatusReceived})
	s.AddMessage(Message{ID: id32(3), Peer: "Dana", Dir: DirIn, Body: "nothing relevant here", TS: now, Status: StatusReceived})

	res, err := s.Search("QUICK", 10)
	if err != nil || len(res) != 1 || res[0].Message.ID != id32(1) {
		t.Fatalf("search: %v %+v", err, res)
	}
	if res[0].Snippet == "" {
		t.Fatal("no snippet")
	}
	// Many hits respect the limit.
	res, _ = s.Search("the", 2)
	if len(res) != 2 {
		t.Fatalf("limit: %d", len(res))
	}
	// A hit at the start and end of a body gets ellipses on the right sides.
	res, _ = s.Search("lazy", 10)
	if len(res) != 1 || res[0].Snippet == "" {
		t.Fatal("snippet empty")
	}
	// No hits.
	if res, _ := s.Search("zzz", 10); len(res) != 0 {
		t.Fatalf("no-hit search returned %d", len(res))
	}
}

func TestSearchSnippetEdges(t *testing.T) {
	s := openRoomStore(t)
	s.AddMessage(Message{ID: id32(1), Peer: "Sam", Dir: DirIn, Body: "start middle end", TS: time.Now(), Status: StatusReceived})
	// Match at the very start: no leading ellipsis.
	res, _ := s.Search("start", 10)
	if len(res) != 1 || res[0].Snippet[:5] != "start" {
		t.Fatalf("start snippet: %+v", res)
	}
	// Match at the very end: no trailing ellipsis.
	res, _ = s.Search("end", 10)
	if len(res) != 1 || res[0].Snippet[len(res[0].Snippet)-3:] != "end" {
		t.Fatalf("end snippet: %+v", res)
	}
}

func TestRenameIdentity(t *testing.T) {
	s, _ := open(t)
	id, _ := identity.Generate("Alex")
	if err := s.SaveIdentity(id); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameIdentity("New Name"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LoadIdentity()
	if got.Name != "New Name" || string(got.Key.Private) != string(id.Key.Private) {
		t.Fatalf("rename: %+v", got)
	}
	// Renaming with no identity fails.
	s2, _ := open(t)
	if err := s2.RenameIdentity("X"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty store rename: %v", err)
	}
}

func TestListPeersDecryptsPendingKeys(t *testing.T) {
	s, _, _ := openEncrypted(t)
	a, _ := identity.Generate("Sam")
	b, _ := identity.Generate("Other")
	now := time.Now()
	s.Observe("Sam", a.Key.Public, now)
	s.Observe("Other", b.Key.Public, now)
	// A changed key puts a pending key in the row.
	c, _ := identity.Generate("Sam")
	s.Observe("Sam", c.Key.Public, now)

	ps, err := s.ListPeers()
	if err != nil || len(ps) != 2 {
		t.Fatalf("list: %v %d", err, len(ps))
	}
	for _, p := range ps {
		if len(p.Key) != 32 {
			t.Fatalf("peer %q key not decrypted: %x", p.Name, p.Key)
		}
		if p.PendingKey != nil && len(p.PendingKey) != 32 {
			t.Fatalf("peer %q pending key not decrypted", p.Name)
		}
	}
}

func TestEncryptStringRoundTrip(t *testing.T) {
	s, _, _ := openEncrypted(t)
	enc, err := s.encryptString("hello")
	if err != nil {
		t.Fatal(err)
	}
	if enc == "hello" {
		t.Fatal("encryptString was a no-op")
	}
	got, err := s.decryptString(enc)
	if err != nil || got != "hello" {
		t.Fatalf("decrypt: %q %v", got, err)
	}
	// Garbage base64 fails cleanly.
	if _, err := s.decryptString("!!!not base64!!!"); err == nil {
		t.Fatal("bad base64 accepted")
	}
	// Empty strings pass through unencrypted.
	if enc, _ := s.encryptString(""); enc != "" {
		t.Fatal("empty string encrypted")
	}
}
