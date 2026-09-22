package node

import (
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Mattathiasa/chirp/internal/discovery"
	"github.com/Mattathiasa/chirp/internal/proto"
	"github.com/Mattathiasa/chirp/internal/store"
)

// acceptAll answers every outstanding room invitation on a node. Most tests
// care about what happens after the invitation is accepted, not about the
// consent step itself, which TestRoomInvite* cover directly.
func acceptAll(t *testing.T, n *Node) {
	t.Helper()
	rooms, err := n.Rooms()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rooms {
		if r.State == store.RoomPending {
			if err := n.AcceptRoomInvite(r.ID); err != nil {
				t.Fatalf("accept %s: %v", r.ID, err)
			}
		}
	}
}

// seesRoom waits until a node has been offered a room, then accepts it.
func joinRoom(t *testing.T, n *Node, want int) {
	t.Helper()
	waitFor(t, "room invitation arrives", func() bool {
		rooms, _ := n.Rooms()
		return len(rooms) >= want
	})
	acceptAll(t, n)
}

func TestTwoNodeRoom(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")

	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	room, err := alice.n.CreateRoom("Test Room", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}
	if room.ID == "" {
		t.Fatal("room ID empty")
	}

	waitFor(t, "bob sees room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1
	})
	acceptAll(t, bob.n)
	acceptAll(t, bob.n)

	msg, err := alice.n.SendRoomMessage(room.ID, "Hello room!")
	if err != nil {
		t.Fatal(err)
	}
	if msg.Body != "Hello room!" {
		t.Fatalf("message body: %q", msg.Body)
	}

	waitFor(t, "bob gets message", func() bool {
		msgs, _ := bob.n.RoomMessages(room.ID, 10)
		return len(msgs) == 1 && msgs[0].Body == "Hello room!"
	})

	_, err = bob.n.SendRoomMessage(room.ID, "Hi from Bob!")
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "alice gets bob's message", func() bool {
		msgs, _ := alice.n.RoomMessages(room.ID, 10)
		return len(msgs) == 2 && msgs[1].Body == "Hi from Bob!"
	})
}

func TestThreeNodeRoom(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	carol := newRig(t, hub, "Carol")

	waitFor(t, "alice-bob", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })
	waitFor(t, "alice-carol", func() bool { return online(alice.n, "Carol") && online(carol.n, "Alice") })
	waitFor(t, "bob-carol", func() bool { return online(bob.n, "Carol") && online(carol.n, "Bob") })

	room, err := alice.n.CreateRoom("Test Room", []string{"Bob", "Carol"})
	if err != nil {
		t.Fatal(err)
	}
	if room.ID == "" {
		t.Fatal("room ID empty")
	}
	if room.Name != "Test Room" {
		t.Fatalf("room name: %q", room.Name)
	}
	if len(room.Members) != 3 {
		t.Fatalf("members: %v", room.Members)
	}

	waitFor(t, "bob sees room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1
	})
	acceptAll(t, bob.n)
	acceptAll(t, bob.n)
	waitFor(t, "carol sees room", func() bool {
		rooms, _ := carol.n.Rooms()
		return len(rooms) == 1
	})
	acceptAll(t, carol.n)
	acceptAll(t, carol.n)

	// Wait for session links to settle after room-create events propagate.
	time.Sleep(time.Second)

	msg, err := alice.n.SendRoomMessage(room.ID, "Hello room!")
	if err != nil {
		t.Fatal(err)
	}
	if msg.Body != "Hello room!" {
		t.Fatalf("message body: %q", msg.Body)
	}
	if msg.Sender != "Alice" {
		t.Fatalf("sender: %q", msg.Sender)
	}

	waitFor(t, "bob gets message", func() bool {
		msgs, _ := bob.n.RoomMessages(room.ID, 10)
		return len(msgs) == 1 && msgs[0].Body == "Hello room!"
	})
	waitFor(t, "carol gets message", func() bool {
		msgs, _ := carol.n.RoomMessages(room.ID, 10)
		return len(msgs) == 1 && msgs[0].Body == "Hello room!"
	})

	_, err = bob.n.SendRoomMessage(room.ID, "Hi from Bob!")
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "alice gets bob's message", func() bool {
		msgs, _ := alice.n.RoomMessages(room.ID, 10)
		return len(msgs) == 2 && msgs[1].Body == "Hi from Bob!"
	})
	waitFor(t, "carol gets bob's message", func() bool {
		msgs, _ := carol.n.RoomMessages(room.ID, 10)
		return len(msgs) == 2 && msgs[1].Body == "Hi from Bob!"
	})

	msgs, _ := alice.n.RoomMessages(room.ID, 10)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Sender != "Alice" || msgs[1].Sender != "Bob" {
		t.Fatalf("unexpected order: %v", msgs)
	}
}

func TestRoomMemberRemoved(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")

	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	room, err := alice.n.CreateRoom("Private", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "bob sees room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1
	})
	acceptAll(t, bob.n)
	acceptAll(t, bob.n)

	alice.n.SendRoomMessage(room.ID, "before removal") //nolint:errcheck
	waitFor(t, "bob gets message", func() bool {
		msgs, _ := bob.n.RoomMessages(room.ID, 10)
		return len(msgs) == 1
	})

	if err := alice.n.RemoveRoomMember(room.ID, "Bob"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "bob loses room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 0
	})

	alice.n.SendRoomMessage(room.ID, "after removal") //nolint:errcheck
	time.Sleep(200 * time.Millisecond)
	// Bob's room was deleted when he was removed, so he has no messages.
	msgs, _ := bob.n.RoomMessages(room.ID, 10)
	if len(msgs) != 0 {
		t.Fatalf("bob should have 0 messages after removal, got %d", len(msgs))
	}
}

func TestRoomLeave(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")

	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	room, err := alice.n.CreateRoom("Temporary", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "bob sees room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1
	})
	acceptAll(t, bob.n)
	acceptAll(t, bob.n)

	if err := bob.n.LeaveRoom(room.ID); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "alice sees bob left", func() bool {
		rooms, _ := alice.n.Rooms()
		if len(rooms) != 1 {
			return false
		}
		return len(rooms[0].Members) == 1
	})
}

func TestRoomRename(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")

	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	room, err := alice.n.CreateRoom("Original", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "bob sees room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1 && rooms[0].Name == "Original"
	})
	acceptAll(t, bob.n)

	if err := alice.n.RenameRoom(room.ID, "Renamed"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "bob sees rename", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1 && rooms[0].Name == "Renamed"
	})
}

func TestRoomOfflineReceivesOnRejoin(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")

	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	room, err := alice.n.CreateRoom("Temp", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "bob sees room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1
	})
	acceptAll(t, bob.n)
	acceptAll(t, bob.n)

	alice.n.SendRoomMessage(room.ID, "msg1") //nolint:errcheck
	waitFor(t, "bob gets msg1", func() bool {
		msgs, _ := bob.n.RoomMessages(room.ID, 10)
		return len(msgs) == 1
	})

	bobKey := bob.id
	bobStore := bob.st
	bob.n.Stop()
	waitFor(t, "alice sees bob offline", func() bool { return !online(alice.n, "Bob") })

	alice.n.SendRoomMessage(room.ID, "msg2") //nolint:errcheck
	alice.n.SendRoomMessage(room.ID, "msg3") //nolint:errcheck
	time.Sleep(200 * time.Millisecond)

	pending, _ := alice.st.RoomPending(room.ID, "Bob")
	if len(pending) < 2 {
		t.Fatalf("expected at least 2 pending messages, got %d", len(pending))
	}

	bob2 := startNode(t, hub, bobStore, bobKey)
	_ = bob2
	waitFor(t, "bob back online", func() bool { return online(alice.n, "Bob") })

	waitFor(t, "bob gets all messages", func() bool {
		msgs, _ := bobStore.RoomMessages(room.ID, 10)
		return len(msgs) == 3
	})
}

func TestRoomNotMemberCannotSend(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	carol := newRig(t, hub, "Carol")

	waitFor(t, "all online", func() bool {
		return online(alice.n, "Bob") && online(alice.n, "Carol") &&
			online(bob.n, "Alice") && online(carol.n, "Alice")
	})

	room, err := alice.n.CreateRoom("No Carol", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "bob sees room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1
	})
	acceptAll(t, bob.n)
	acceptAll(t, bob.n)

	if _, err := carol.n.SendRoomMessage(room.ID, "I'm not in this room"); err == nil {
		t.Fatal("expected error for non-member sending")
	}
}

func TestRoomDeliveryStatus(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")

	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	room, err := alice.n.CreateRoom("Status Test", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "bob sees room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1
	})
	acceptAll(t, bob.n)
	acceptAll(t, bob.n)

	if _, err := alice.n.SendRoomMessage(room.ID, "check status"); err != nil {
		t.Fatal(err)
	}

	msgs, _ := alice.n.RoomMessages(room.ID, 10)
	if len(msgs) != 1 || msgs[0].Status != store.StatusQueued {
		t.Fatalf("initial status: %v", msgs)
	}

	waitFor(t, "delivered", func() bool {
		msgs, _ := alice.n.RoomMessages(room.ID, 10)
		return len(msgs) == 1 && msgs[0].Status == store.StatusDelivered
	})
}

// TestRoomEventForgedByNonCreator verifies that a non-creator peer cannot forge
// membership events. Events arrive over an authenticated Noise session (l.peer
// is trusted) but the envelope's RoomActor/RoomName fields are not.
func TestRoomEventForgedByNonCreator(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")

	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	room, err := alice.n.CreateRoom("Original", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob sees room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1
	})
	acceptAll(t, bob.n)
	acceptAll(t, bob.n)

	// Bob opens a fresh authenticated session to Alice and forges events.
	sc := rawSession(t, alice.n.ln.Addr().String(), bob.id)

	// 1. Forged rename: Bob claims to rename the room.
	sc.Send(proto.Envelope{
		T: proto.TypeRoomEvent, Room: room.ID, RoomEvent: "rename",
		RoomName: "Hacked", RoomActor: "Bob", TS: time.Now().UnixMilli(),
	})
	// 2. Forged remove: Bob claims to remove Alice (the creator).
	sc.Send(proto.Envelope{
		T: proto.TypeRoomEvent, Room: room.ID, RoomEvent: "remove",
		RoomActor: "Alice", TS: time.Now().UnixMilli(),
	})
	// 3. Forged join: Bob claims to add Carol.
	sc.Send(proto.Envelope{
		T: proto.TypeRoomEvent, Room: room.ID, RoomEvent: "join",
		RoomActor: "Carol", TS: time.Now().UnixMilli(),
	})
	time.Sleep(300 * time.Millisecond)
	sc.Close()

	// The room name must be unchanged.
	rooms, _ := alice.n.Rooms()
	if len(rooms) != 1 || rooms[0].Name != "Original" {
		t.Fatalf("room modified by non-creator: %+v", rooms)
	}
	// Members must be unchanged: Alice and Bob only.
	rs, _ := alice.st.GetRoom(room.ID)
	if len(rs.Members) != 2 || !isMember(rs.Members, "Alice") || !isMember(rs.Members, "Bob") {
		t.Fatalf("members modified by forged events: %v", rs.Members)
	}
}

// The "create" event is how a room first appears on a member's device, so it
// carries an untrusted name, member list and creator. Applied blindly to a
// room that already exists, it lets any member seize the room: send a create
// for someone else's room and you become its creator everywhere, which then
// unlocks rename, add and remove.
func TestRoomCreateEventCannotSeizeAnExistingRoom(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")

	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	room, err := alice.n.CreateRoom("Alice's room", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob sees room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1
	})
	acceptAll(t, bob.n)
	acceptAll(t, bob.n)

	// Bob re-sends "create" for Alice's room, naming himself the creator and
	// rewriting the membership.
	sc := rawSession(t, alice.n.ln.Addr().String(), bob.id)
	sc.Send(proto.Envelope{ //nolint:errcheck
		T: proto.TypeRoomEvent, Room: room.ID, RoomEvent: "create",
		RoomActor: "Bob", RoomName: "Bob's room", RoomMembers: []string{"Bob"},
		TS: time.Now().UnixMilli(),
	})
	time.Sleep(300 * time.Millisecond)
	sc.Close()

	got, err := alice.st.GetRoom(room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedBy != "Alice" {
		t.Fatalf("a member seized the room: createdBy = %q, want Alice", got.CreatedBy)
	}
	if got.Name != "Alice's room" {
		t.Fatalf("room renamed by a forged create: %q", got.Name)
	}
	if !isMember(got.Members, "Alice") || !isMember(got.Members, "Bob") || len(got.Members) != 2 {
		t.Fatalf("membership rewritten by a forged create: %v", got.Members)
	}
}

// A room message must come from somebody who is actually in that room. Without
// the check, any pinned peer can post into any room id, and a removed member
// keeps posting into the room they were removed from.
func TestRoomMessageFromNonMemberIsRejected(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	carol := newRig(t, hub, "Carol")

	waitFor(t, "all online", func() bool {
		return online(alice.n, "Bob") && online(alice.n, "Carol")
	})

	room, err := alice.n.CreateRoom("No Carol", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob sees room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1
	})
	acceptAll(t, bob.n)
	acceptAll(t, bob.n)

	// Carol is not a member, but she has an authenticated session with Alice.
	sc := rawSession(t, alice.n.ln.Addr().String(), carol.id)
	id, _ := proto.NewID(rand.Reader)
	sc.Send(proto.Envelope{ //nolint:errcheck
		T: proto.TypeRoomMsg, ID: id, Room: room.ID,
		Body: "I am not in this room", TS: time.Now().UnixMilli(),
	})
	time.Sleep(300 * time.Millisecond)
	sc.Close()

	msgs, _ := alice.n.RoomMessages(room.ID, 10)
	for _, m := range msgs {
		if m.Sender == "Carol" {
			t.Fatalf("stored a room message from a non-member: %+v", m)
		}
	}
	if len(msgs) != 0 {
		t.Fatalf("expected no messages, got %d", len(msgs))
	}
}

// A message for a room id we have never heard of must not create phantom rows.
func TestRoomMessageForUnknownRoomIsRejected(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") })

	sc := rawSession(t, alice.n.ln.Addr().String(), bob.id)
	ghost, _ := proto.NewID(rand.Reader)
	id, _ := proto.NewID(rand.Reader)
	sc.Send(proto.Envelope{ //nolint:errcheck
		T: proto.TypeRoomMsg, ID: id, Room: ghost,
		Body: "room that does not exist", TS: time.Now().UnixMilli(),
	})
	time.Sleep(300 * time.Millisecond)
	sc.Close()

	if msgs, _ := alice.n.RoomMessages(ghost, 10); len(msgs) != 0 {
		t.Fatalf("stored %d messages for an unknown room", len(msgs))
	}
	if rooms, _ := alice.n.Rooms(); len(rooms) != 0 {
		t.Fatalf("a room message conjured a room into existence: %v", rooms)
	}
}

// Removal has to bite on the receiving side too: the remover must stop
// accepting the removed member's messages, not merely stop sending to them.
func TestRemovedMemberCannotKeepPosting(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	room, err := alice.n.CreateRoom("Private", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob sees room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1
	})
	acceptAll(t, bob.n)
	acceptAll(t, bob.n)
	if err := alice.n.RemoveRoomMember(room.ID, "Bob"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice drops bob", func() bool {
		r, err := alice.st.GetRoom(room.ID)
		return err == nil && !isMember(r.Members, "Bob")
	})

	// Bob, now an ex-member, posts anyway over a fresh authenticated session.
	sc := rawSession(t, alice.n.ln.Addr().String(), bob.id)
	id, _ := proto.NewID(rand.Reader)
	sc.Send(proto.Envelope{ //nolint:errcheck
		T: proto.TypeRoomMsg, ID: id, Room: room.ID,
		Body: "still here", TS: time.Now().UnixMilli(),
	})
	time.Sleep(300 * time.Millisecond)
	sc.Close()

	msgs, _ := alice.n.RoomMessages(room.ID, 10)
	for _, m := range msgs {
		if m.Body == "still here" {
			t.Fatal("a removed member's message was accepted")
		}
	}
}

// The per-sender sequence is the only ordering a receiver has within one
// sender's stream, so it has to survive the wire. It used to be computed on
// send and then thrown away, leaving every received message at senderSeq 0.
func TestRoomSenderSeqReachesTheReceiver(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	room, err := alice.n.CreateRoom("Ordered", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob sees room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1
	})
	acceptAll(t, bob.n)
	acceptAll(t, bob.n)

	const n = 5
	for i := 0; i < n; i++ {
		if _, err := alice.n.SendRoomMessage(room.ID, fmt.Sprintf("message %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "bob gets them all", func() bool {
		msgs, _ := bob.n.RoomMessages(room.ID, 20)
		return len(msgs) == n
	})

	// Both sides must agree, and the numbers must be 1..n with no gaps.
	for _, side := range []struct {
		who string
		n   *Node
	}{{"alice", alice.n}, {"bob", bob.n}} {
		msgs, _ := side.n.RoomMessages(room.ID, 20)
		var seqs []uint64
		for _, m := range msgs {
			if m.Sender != "Alice" {
				continue
			}
			if m.SenderSeq == 0 {
				t.Fatalf("%s: message %q has no sender sequence", side.who, m.Body)
			}
			seqs = append(seqs, m.SenderSeq)
		}
		if len(seqs) != n {
			t.Fatalf("%s: %d sequenced messages, want %d", side.who, len(seqs), n)
		}
		for i, got := range seqs {
			if got != uint64(i+1) {
				t.Fatalf("%s: sequence %v is not 1..%d", side.who, seqs, n)
			}
		}
	}
}

// Each sender numbers its own stream, so two senders both start at 1 and
// neither one's numbering is disturbed by the other.
func TestRoomSeqIsPerSender(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	room, err := alice.n.CreateRoom("Interleaved", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob sees room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1
	})
	acceptAll(t, bob.n)
	acceptAll(t, bob.n)

	for i := 0; i < 3; i++ {
		alice.n.SendRoomMessage(room.ID, fmt.Sprintf("a%d", i)) //nolint:errcheck
		bob.n.SendRoomMessage(room.ID, fmt.Sprintf("b%d", i))   //nolint:errcheck
	}
	waitFor(t, "alice has all six", func() bool {
		msgs, _ := alice.n.RoomMessages(room.ID, 20)
		return len(msgs) == 6
	})

	msgs, _ := alice.n.RoomMessages(room.ID, 20)
	per := map[string][]uint64{}
	for _, m := range msgs {
		per[m.Sender] = append(per[m.Sender], m.SenderSeq)
	}
	for who, seqs := range per {
		if len(seqs) != 3 {
			t.Fatalf("%s sent %d messages, want 3", who, len(seqs))
		}
		for i, got := range seqs {
			if got != uint64(i+1) {
				t.Fatalf("%s's sequence is %v, want [1 2 3]", who, seqs)
			}
		}
	}
}

// The counter is durable: it used to be recovered by scanning the last 1000
// messages, so a restart (or a room past 1000 messages) restarted it at 1.
func TestRoomSeqSurvivesRestart(t *testing.T) {
	st := newStore(t)
	roomID, _ := proto.NewID(rand.Reader)
	for i := uint64(1); i <= 3; i++ {
		got, err := st.NextRoomSenderSeq(roomID, "Alice")
		if err != nil {
			t.Fatal(err)
		}
		if got != i {
			t.Fatalf("seq %d, want %d", got, i)
		}
	}
	// A different sender in the same room is numbered independently.
	if got, _ := st.NextRoomSenderSeq(roomID, "Bob"); got != 1 {
		t.Fatalf("Bob's first seq is %d, want 1", got)
	}
	if got, _ := st.NextRoomSenderSeq(roomID, "Alice"); got != 4 {
		t.Fatalf("Alice's seq after Bob's is %d, want 4", got)
	}
}

// A reaction sent into a room reaches every other member and is persisted, so
// it survives a reload rather than living only in an SSE event.
func TestRoomReactionReachesEveryMember(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	carol := newRig(t, hub, "Carol")

	waitFor(t, "all online", func() bool {
		return online(alice.n, "Bob") && online(alice.n, "Carol") &&
			online(bob.n, "Carol") && online(carol.n, "Bob")
	})

	room, err := alice.n.CreateRoom("Reactions", []string{"Bob", "Carol"})
	if err != nil {
		t.Fatal(err)
	}
	joinRoom(t, bob.n, 1)
	joinRoom(t, carol.n, 1)
	time.Sleep(500 * time.Millisecond)

	msg, err := alice.n.SendRoomMessage(room.ID, "ship it")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "everyone has the message", func() bool {
		bm, _ := bob.n.RoomMessages(room.ID, 10)
		cm, _ := carol.n.RoomMessages(room.ID, 10)
		return len(bm) == 1 && len(cm) == 1
	})

	if err := bob.n.SendRoomReaction(room.ID, msg.ID, "🎉"); err != nil {
		t.Fatal(err)
	}

	hasReaction := func(n *Node, who, emoji string) func() bool {
		return func() bool {
			msgs, _ := n.RoomMessages(room.ID, 10)
			for _, m := range msgs {
				for _, r := range m.Reactions {
					if r.Sender == who && r.Emoji == emoji {
						return true
					}
				}
			}
			return false
		}
	}
	waitFor(t, "alice sees bob's reaction", hasReaction(alice.n, "Bob", "🎉"))
	waitFor(t, "carol sees bob's reaction", hasReaction(carol.n, "Bob", "🎉"))
	if !hasReaction(bob.n, "Bob", "🎉")() {
		t.Fatal("bob does not see his own reaction")
	}

	// Sending the same emoji again toggles it back off everywhere.
	if err := bob.n.SendRoomReaction(room.ID, msg.ID, "🎉"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice sees it removed", func() bool { return !hasReaction(alice.n, "Bob", "🎉")() })
	waitFor(t, "carol sees it removed", func() bool { return !hasReaction(carol.n, "Bob", "🎉")() })
}

// A reaction, like a message, is only honoured from a current member.
func TestRoomReactionFromNonMemberIsRejected(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	newRig(t, hub, "Bob")
	carol := newRig(t, hub, "Carol")
	waitFor(t, "all online", func() bool { return online(alice.n, "Bob") && online(alice.n, "Carol") })

	room, err := alice.n.CreateRoom("No Carol", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := alice.n.SendRoomMessage(room.ID, "members only")
	if err != nil {
		t.Fatal(err)
	}

	sc := rawSession(t, alice.n.ln.Addr().String(), carol.id)
	sc.Send(proto.Envelope{T: proto.TypeReact, Room: room.ID, Target: msg.ID, Emoji: "👀"}) //nolint:errcheck
	time.Sleep(300 * time.Millisecond)
	sc.Close()

	msgs, _ := alice.n.RoomMessages(room.ID, 10)
	for _, m := range msgs {
		if len(m.Reactions) != 0 {
			t.Fatalf("a non-member's reaction was stored: %+v", m.Reactions)
		}
	}
}

// Being added to a room is an invitation, not a fait accompli. Until it is
// accepted the room does nothing: no messages are stored and nothing is sent.
func TestRoomInviteMustBeAcceptedBeforeAnythingHappens(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	room, err := alice.n.CreateRoom("Invite me", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob is offered the room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1
	})

	rooms, _ := bob.n.Rooms()
	if rooms[0].State != store.RoomPending {
		t.Fatalf("invitation landed as %q, want %q", rooms[0].State, store.RoomPending)
	}
	// Alice's own room is joined outright; she is the one who made it.
	ar, _ := alice.n.Rooms()
	if ar[0].State != store.RoomJoined {
		t.Fatalf("creator's room is %q, want %q", ar[0].State, store.RoomJoined)
	}

	// Bob cannot send into a room he has not accepted.
	if _, err := bob.n.SendRoomMessage(room.ID, "hello?"); !errors.Is(err, ErrRoomPending) {
		t.Fatalf("send while pending: %v, want ErrRoomPending", err)
	}

	// Anything Alice says while the invitation is unanswered is held, not lost.
	if _, err := alice.n.SendRoomMessage(room.ID, "said before you accepted"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if msgs, _ := bob.n.RoomMessages(room.ID, 10); len(msgs) != 0 {
		t.Fatalf("a pending room stored %d messages", len(msgs))
	}

	if err := bob.n.AcceptRoomInvite(room.ID); err != nil {
		t.Fatal(err)
	}

	// The sender's outbox keeps retrying, so the backlog lands on acceptance.
	waitFor(t, "backlog arrives after accepting", func() bool {
		msgs, _ := bob.n.RoomMessages(room.ID, 10)
		return len(msgs) == 1 && msgs[0].Body == "said before you accepted"
	})
	waitFor(t, "alice sees it delivered", func() bool {
		msgs, _ := alice.n.RoomMessages(room.ID, 10)
		return len(msgs) == 1 && msgs[0].Status == store.StatusDelivered
	})

	// And Bob can now take part.
	if _, err := bob.n.SendRoomMessage(room.ID, "accepted"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice gets bob's reply", func() bool {
		msgs, _ := alice.n.RoomMessages(room.ID, 10)
		return len(msgs) == 2
	})
}

// Declining removes the room locally and tells the others we are out.
func TestRoomInviteDecline(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	room, err := alice.n.CreateRoom("No thanks", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob is offered the room", func() bool {
		rooms, _ := bob.n.Rooms()
		return len(rooms) == 1
	})

	if err := bob.n.DeclineRoomInvite(room.ID); err != nil {
		t.Fatal(err)
	}
	if rooms, _ := bob.n.Rooms(); len(rooms) != 0 {
		t.Fatalf("declined room is still here: %v", rooms)
	}
	waitFor(t, "alice drops bob from the room", func() bool {
		r, err := alice.st.GetRoom(room.ID)
		return err == nil && !isMember(r.Members, "Bob")
	})

	// Declining a room that was already joined is refused; leaving is the
	// operation for that.
	if err := alice.n.DeclineRoomInvite(room.ID); err == nil {
		t.Fatal("declined an already-joined room")
	}
}

// Accepting announces the arrival, so the creator can tell a live member from
// one who has not answered.
func TestRoomAcceptIsAnnounced(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	room, err := alice.n.CreateRoom("Announce", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}
	joinRoom(t, bob.n, 1)

	// Bob announcing himself must not disturb Alice's membership list.
	time.Sleep(300 * time.Millisecond)
	r, err := alice.st.GetRoom(room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Members) != 2 || !isMember(r.Members, "Bob") || r.CreatedBy != "Alice" {
		t.Fatalf("membership disturbed by a join announcement: %+v", r)
	}
}

// n.live only holds sessions to other people, so a naive lookup reported the
// reader as offline in their own room's member list.
func TestRoomMemberListShowsUsAsOnline(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	room, err := alice.n.CreateRoom("Presence", []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}
	joinRoom(t, bob.n, 1)

	for _, side := range []struct {
		who  string
		n    *Node
		self string
	}{{"alice", alice.n, "Alice"}, {"bob", bob.n, "Bob"}} {
		rv, err := side.n.GetRoom(room.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range rv.Members {
			if m.Name == side.self && !m.Online {
				t.Errorf("%s sees themselves as offline in their own room", side.who)
			}
		}
	}

	// And the count in the header follows from the same list.
	rooms, _ := alice.n.Rooms()
	onlineCount := 0
	for _, m := range rooms[0].Members {
		if m.Online {
			onlineCount++
		}
	}
	if onlineCount != 2 {
		t.Fatalf("2 members are connected but the room reports %d online", onlineCount)
	}
}
