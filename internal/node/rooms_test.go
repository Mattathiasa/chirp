package node

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/Mattathiasa/chirp/internal/discovery"
	"github.com/Mattathiasa/chirp/internal/proto"
	"github.com/Mattathiasa/chirp/internal/store"
)

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
	waitFor(t, "carol sees room", func() bool {
		rooms, _ := carol.n.Rooms()
		return len(rooms) == 1
	})

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
