package node

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mattathiasa/chirp/internal/proto"
	"github.com/Mattathiasa/chirp/internal/store"
)

// Room view for the API layer.
type RoomView struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Members   []RoomMemberView   `json:"members"`
	CreatedBy string             `json:"createdBy"`
	CreatedAt time.Time          `json:"createdAt"`
	LastMsg   *store.RoomMessage `json:"lastMsg,omitempty"`
	Unread    int                `json:"unread"`
}

// RoomMemberView is one member in a room.
type RoomMemberView struct {
	Name   string `json:"name"`
	Online bool   `json:"online"`
}

// CreateRoom creates a new room with the given name and member list.
// The creator is automatically included. Members must be pinned peers.
func (n *Node) CreateRoom(name string, memberNames []string) (*store.Room, error) {
	if name == "" {
		return nil, errors.New("node: room name required")
	}
	if len(memberNames) == 0 {
		return nil, errors.New("node: at least one member required")
	}
	if len(memberNames) > store.MaxRoomMembers-1 { // -1 for creator
		return nil, fmt.Errorf("node: too many members (max %d)", store.MaxRoomMembers-1)
	}

	// Validate all members are pinned peers.
	for _, m := range memberNames {
		if _, err := n.cfg.Store.GetPeer(m); err != nil {
			return nil, fmt.Errorf("node: member %q is not a pinned peer", m)
		}
	}

	// Build member list (sorted, unique, includes creator).
	members := dedupeMembers(append([]string{n.id.Name}, memberNames...))

	roomID, err := proto.NewID(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("node: generate room id: %w", err)
	}

	room := store.Room{
		ID:        roomID,
		Name:      name,
		Members:   members,
		CreatedBy: n.id.Name,
		CreatedAt: time.Now(),
	}

	if err := n.cfg.Store.CreateRoom(room); err != nil {
		return nil, err
	}

	// Send room-create events to all members (best effort, they may be offline).
	for _, m := range members {
		if m == n.id.Name {
			continue
		}
		n.sendRoomEvent(m, roomID, "create", n.id.Name, name, members)
	}

	n.emit(Event{Type: "room"})
	return &room, nil
}

// Rooms returns all rooms the user is a member of.
func (n *Node) Rooms() ([]RoomView, error) {
	rooms, err := n.cfg.Store.ListRooms()
	if err != nil {
		return nil, err
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	var out []RoomView
	for _, r := range rooms {
		rv := RoomView{
			ID:        r.ID,
			Name:      r.Name,
			CreatedBy: r.CreatedBy,
			CreatedAt: r.CreatedAt,
			Members:   make([]RoomMemberView, 0, len(r.Members)),
		}
		for _, m := range r.Members {
			rv.Members = append(rv.Members, RoomMemberView{
				Name:   m,
				Online: n.live[strings.ToLower(m)] != nil,
			})
		}
		// Get last message.
		msgs, err := n.cfg.Store.RoomMessages(r.ID, 1)
		if err == nil && len(msgs) > 0 {
			rv.LastMsg = &msgs[len(msgs)-1]
		}
		out = append(out, rv)
	}
	return out, nil
}

// GetRoom returns a single room by ID.
func (n *Node) GetRoom(id string) (*RoomView, error) {
	r, err := n.cfg.Store.GetRoom(id)
	if err != nil {
		return nil, err
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	rv := &RoomView{
		ID:        r.ID,
		Name:      r.Name,
		CreatedBy: r.CreatedBy,
		CreatedAt: r.CreatedAt,
		Members:   make([]RoomMemberView, 0, len(r.Members)),
	}
	for _, m := range r.Members {
		rv.Members = append(rv.Members, RoomMemberView{
			Name:   m,
			Online: n.live[strings.ToLower(m)] != nil,
		})
	}
	msgs, err := n.cfg.Store.RoomMessages(r.ID, 1)
	if err == nil && len(msgs) > 0 {
		rv.LastMsg = &msgs[len(msgs)-1]
	}
	return rv, nil
}

// RoomMessages returns messages for a room.
func (n *Node) RoomMessages(roomID string, limit int) ([]store.RoomMessage, error) {
	return n.cfg.Store.RoomMessages(roomID, limit)
}

// SendRoomMessage sends a message to all members of a room.
func (n *Node) SendRoomMessage(roomID, body string) (*store.RoomMessage, error) {
	if body == "" {
		return nil, ErrBadBody
	}
	if len(body) > proto.MaxBody {
		return nil, ErrBadBody
	}

	r, err := n.cfg.Store.GetRoom(roomID)
	if err != nil {
		n.logf("SendRoomMessage: get room: %v", err)
		return nil, err
	}

	// Verify sender is a member.
	if !isMember(r.Members, n.id.Name) {
		return nil, errors.New("node: not a member of this room")
	}

	msgID, err := proto.NewID(rand.Reader)
	if err != nil {
		return nil, err
	}

	// Get per-sender sequence number.
	senderSeq, err := n.nextRoomSenderSeq(roomID, n.id.Name)
	if err != nil {
		return nil, err
	}

	m := store.RoomMessage{
		ID:        msgID,
		Room:      roomID,
		Sender:    n.id.Name,
		Dir:       store.DirOut,
		Body:      body,
		TS:        time.Now(),
		Status:    store.StatusQueued,
		SenderSeq: senderSeq,
	}

	stored, dup, err := n.cfg.Store.AddRoomMessage(m)
	if err != nil {
		return nil, err
	}
	if dup {
		return &stored, nil
	}

	// Add to outbox for each member (except self).
	for _, mb := range r.Members {
		if mb == n.id.Name {
			continue
		}
		if err := n.cfg.Store.AddRoomMemberToOutbox(roomID, mb, msgID); err != nil {
			n.logf("room outbox: %v", err)
		}
	}

	n.emit(Event{Type: "roomMessage", Room: roomID, RoomMessage: &stored})

	n.logf("SendRoomMessage: stored msg %s in room %s, members=%v", msgID, roomID, r.Members)

	// Flush to all connected members.
	for _, mb := range r.Members {
		if mb == n.id.Name {
			continue
		}
		n.logf("room flush for %s (room %s)", mb, roomID)
		n.flushRoomMember(mb, roomID)
	}

	return &stored, nil
}

// AddRoomMember adds a member to a room. Only the creator can do this.
func (n *Node) AddRoomMember(roomID, memberName string) error {
	r, err := n.cfg.Store.GetRoom(roomID)
	if err != nil {
		return err
	}
	if r.CreatedBy != n.id.Name {
		return errors.New("node: only the creator can add members")
	}
	if isMember(r.Members, memberName) {
		return errors.New("node: already a member")
	}
	if len(r.Members) >= store.MaxRoomMembers {
		return fmt.Errorf("node: room is full (%d members)", store.MaxRoomMembers)
	}
	if _, err := n.cfg.Store.GetPeer(memberName); err != nil {
		return fmt.Errorf("node: %q is not a pinned peer", memberName)
	}

	r.Members = append(r.Members, memberName)
	sortStrings(r.Members)
	if err := n.cfg.Store.UpdateRoom(*r); err != nil {
		return err
	}

	// Send a create event to the new member so they receive the room state
	// (a join event for a room they don't have would otherwise be dropped).
	n.sendRoomEvent(memberName, roomID, "create", n.id.Name, r.Name, r.Members)
	// Tell existing members about the new member.
	for _, mb := range r.Members {
		if mb == n.id.Name || mb == memberName {
			continue
		}
		n.sendRoomEvent(mb, roomID, "join", memberName, "", nil)
	}

	n.emit(Event{Type: "room"})
	return nil
}

// RemoveRoomMember removes a member from a room. Only the creator can do this.
func (n *Node) RemoveRoomMember(roomID, memberName string) error {
	r, err := n.cfg.Store.GetRoom(roomID)
	if err != nil {
		return err
	}
	if r.CreatedBy != n.id.Name {
		return errors.New("node: only the creator can remove members")
	}
	if memberName == n.id.Name {
		return errors.New("node: cannot remove yourself; use leave instead")
	}
	if !isMember(r.Members, memberName) {
		return errors.New("node: not a member")
	}

	// Remove from member list.
	newMembers := make([]string, 0, len(r.Members)-1)
	for _, m := range r.Members {
		if m != memberName {
			newMembers = append(newMembers, m)
		}
	}
	r.Members = newMembers
	if err := n.cfg.Store.UpdateRoom(*r); err != nil {
		return err
	}

	// Send remove event to the removed member and all remaining members.
	n.sendRoomEvent(memberName, roomID, "remove", memberName, "", nil)
	for _, mb := range r.Members {
		if mb == n.id.Name || mb == memberName {
			continue
		}
		n.sendRoomEvent(mb, roomID, "remove", memberName, "", nil)
	}

	n.emit(Event{Type: "room"})
	return nil
}

// LeaveRoom removes the current user from a room.
func (n *Node) LeaveRoom(roomID string) error {
	r, err := n.cfg.Store.GetRoom(roomID)
	if err != nil {
		return err
	}
	if !isMember(r.Members, n.id.Name) {
		return errors.New("node: not a member")
	}

	// If creator is leaving and there are other members, transfer ownership.
	if r.CreatedBy == n.id.Name && len(r.Members) > 1 {
		// Transfer to the first other member (sorted order).
		for _, m := range r.Members {
			if m != n.id.Name {
				r.CreatedBy = m
				break
			}
		}
	}

	// Remove self from member list.
	newMembers := make([]string, 0, len(r.Members)-1)
	for _, m := range r.Members {
		if m != n.id.Name {
			newMembers = append(newMembers, m)
		}
	}
	r.Members = newMembers

	// If no members left, delete the room.
	if len(r.Members) == 0 {
		return n.cfg.Store.DeleteRoom(roomID)
	}

	if err := n.cfg.Store.UpdateRoom(*r); err != nil {
		return err
	}

	// Send leave event to remaining members.
	for _, mb := range r.Members {
		n.sendRoomEvent(mb, roomID, "leave", n.id.Name, "", nil)
	}

	n.emit(Event{Type: "room"})
	return nil
}

// RenameRoom renames a room. Only the creator can do this.
func (n *Node) RenameRoom(roomID, newName string) error {
	if newName == "" {
		return errors.New("node: new name required")
	}
	r, err := n.cfg.Store.GetRoom(roomID)
	if err != nil {
		return err
	}
	if r.CreatedBy != n.id.Name {
		return errors.New("node: only the creator can rename the room")
	}

	r.Name = newName
	if err := n.cfg.Store.UpdateRoom(*r); err != nil {
		return err
	}

	// Send rename event to all members.
	for _, mb := range r.Members {
		if mb == n.id.Name {
			continue
		}
		n.sendRoomEvent(mb, roomID, "rename", n.id.Name, newName, nil)
	}

	n.emit(Event{Type: "room"})
	return nil
}

// flushRoomMember sends pending room messages to a specific member.
func (n *Node) flushRoomMember(member, roomID string) {
	n.mu.Lock()
	l := n.live[strings.ToLower(member)]
	n.mu.Unlock()
	if l == nil {
		return // offline; messages will be queued in outbox
	}

	pending, err := n.cfg.Store.RoomPending(roomID, member)
	if err != nil {
		n.logf("room pending: %v", err)
		return
	}
	if len(pending) == 0 {
		return
	}

	l.flushMu.Lock()
	defer l.flushMu.Unlock()

	now := time.Now()
	for _, m := range pending {
		// Apply backoff.
		if m.Attempts > 0 {
			backoff := n.cfg.RetryBase * (1 << uint(m.Attempts-1))
			if backoff > n.cfg.RetryMax {
				backoff = n.cfg.RetryMax
			}
			if now.Sub(m.LastTry) < backoff {
				continue
			}
		}
		n.cfg.Store.RoomRecordAttempt(m.ID, now) //nolint:errcheck

		e := proto.Envelope{
			T:    proto.TypeRoomMsg,
			ID:   m.ID,
			Room: roomID,
			TS:   m.TS.UnixMilli(),
			Body: m.Body,
		}
		if err := l.send(e); err != nil {
			n.logf("room send to %s: %v", member, err)
			return
		}
	}
}

// sendRoomEvent sends a membership change event envelope to a peer.
func (n *Node) sendRoomEvent(member, roomID, eventType, actor, name string, members []string) {
	n.mu.Lock()
	l := n.live[strings.ToLower(member)]
	n.mu.Unlock()
	if l == nil {
		return
	}

	e := proto.Envelope{
		T:           proto.TypeRoomEvent,
		Room:        roomID,
		RoomEvent:   eventType,
		RoomActor:   actor,
		RoomName:    name,
		RoomMembers: members,
		TS:          time.Now().UnixMilli(),
	}
	if err := l.send(e); err != nil {
		n.logf("room event to %s: %v", member, err)
	}
}

// handleRoomMsg processes an incoming room message from a peer.
//
// Security: l.peer is the Noise-authenticated identity of the sender. A room
// message is only accepted for a room we are in, from a peer who is currently
// in it. Without both checks any pinned peer could write rows for room ids we
// have never seen, and a removed member could keep posting into the room they
// were removed from - removal would only stop us sending to them, not stop
// them talking to us.
func (n *Node) handleRoomMsg(l *link, e proto.Envelope) {
	r, err := n.cfg.Store.GetRoom(e.Room)
	if err != nil {
		n.logf("room msg dropped: unknown room %s from %s", e.Room, l.peer)
		return
	}
	if !isMember(r.Members, l.peer) {
		n.logf("room msg dropped: %q is not a member of %s", l.peer, e.Room)
		return
	}
	if !isMember(r.Members, n.id.Name) {
		n.logf("room msg dropped: we are not a member of %s", e.Room)
		return
	}
	m := store.RoomMessage{
		ID:     e.ID,
		Room:   e.Room,
		Sender: l.peer,
		Dir:    store.DirIn,
		Body:   e.Body,
		TS:     time.UnixMilli(e.TS),
		Status: store.StatusReceived,
	}

	stored, dup, err := n.cfg.Store.AddRoomMessage(m)
	if err != nil {
		n.logf("room msg store: %v", err)
		return
	}

	// Send ack back.
	ack := proto.Envelope{
		T:    proto.TypeRoomAck,
		ID:   e.ID,
		Room: e.Room,
	}
	if err := l.send(ack); err != nil {
		n.logf("room ack: %v", err)
	}

	if !dup {
		n.emit(Event{Type: "roomMessage", Room: e.Room, RoomMessage: &stored})
	}
}

// handleRoomAck processes an incoming room ack from a peer. Only a current
// member of a room we hold can move a message's delivery state.
func (n *Node) handleRoomAck(l *link, e proto.Envelope) {
	r, err := n.cfg.Store.GetRoom(e.Room)
	if err != nil || !isMember(r.Members, l.peer) {
		n.logf("room ack dropped from %q for room %s", l.peer, e.Room)
		return
	}
	_, changed, err := n.cfg.Store.MarkRoomDelivered(e.Room, l.peer, e.ID)
	if err != nil {
		n.logf("room ack store: %v", err)
		return
	}
	if changed {
		n.emit(Event{Type: "roomStatus", Room: e.Room, Target: e.ID})
	}
}

// HandleRoomEvent processes an incoming room membership event.
//
// Security: every event arrives over an authenticated Noise session, so l.peer
// is the one true identity of the sender. The envelope's RoomActor field is
// untrusted input and is never used for authorisation or for deciding who
// changed. Only the room creator may add, remove or rename; any member may
// leave. Forged events (wrong actor, non-creator acting) are silently dropped.
func (n *Node) handleRoomEvent(l *link, e proto.Envelope) {
	actor := l.peer // Noise-authenticated identity of the sender
	switch e.RoomEvent {
	case "create":
		members := e.RoomMembers
		if len(members) == 0 {
			members = []string{actor, n.id.Name}
		}
		members = dedupeMembers(members)
		if !isMember(members, n.id.Name) {
			n.logf("room create rejected: %q did not include us in %v", actor, members)
			return
		}
		if len(members) > store.MaxRoomMembers {
			n.logf("room create rejected: %d members exceeds the cap", len(members))
			return
		}

		existing, err := n.cfg.Store.GetRoom(e.Room)
		if err == nil {
			// The room is already here. A create event may only ever be a
			// refresh from the peer who already owns it: applying one from
			// anybody else would let any member seize the room, rewrite its
			// membership, and thereby unlock rename, add and remove.
			if actor != existing.CreatedBy {
				n.logf("room create rejected: %q is not the creator of %s", actor, e.Room)
				return
			}
			existing.Name = e.RoomName
			existing.Members = members
			n.cfg.Store.UpdateRoom(*existing) //nolint:errcheck
		} else {
			// First sight of this room. The peer that told us about it is its
			// creator by definition; it cannot nominate somebody else.
			room := store.Room{
				ID:        e.Room,
				Name:      e.RoomName,
				Members:   members,
				CreatedBy: actor,
				CreatedAt: time.UnixMilli(e.TS),
			}
			if err := n.cfg.Store.CreateRoom(room); err != nil {
				n.logf("room create: %v", err)
				return
			}
		}
		n.emit(Event{Type: "room"})

	case "join":
		r, err := n.cfg.Store.GetRoom(e.Room)
		if err != nil {
			return
		}
		if actor != r.CreatedBy {
			n.logf("room join rejected: %q is not the creator", actor)
			return
		}
		if isMember(r.Members, e.RoomActor) {
			return
		}
		r.Members = append(r.Members, e.RoomActor)
		sortStrings(r.Members)
		n.cfg.Store.UpdateRoom(*r) //nolint:errcheck
		n.emit(Event{Type: "room"})

	case "remove":
		r, err := n.cfg.Store.GetRoom(e.Room)
		if err != nil {
			return
		}
		if actor != r.CreatedBy {
			n.logf("room remove rejected: %q is not the creator", actor)
			return
		}
		if e.RoomActor == n.id.Name {
			n.cfg.Store.DeleteRoom(e.Room) //nolint:errcheck
		} else if isMember(r.Members, e.RoomActor) {
			r.Members = removePeer(r.Members, e.RoomActor)
			n.cfg.Store.UpdateRoom(*r) //nolint:errcheck
		}
		n.emit(Event{Type: "room"})

	case "leave":
		r, err := n.cfg.Store.GetRoom(e.Room)
		if err != nil {
			return
		}
		if actor == n.id.Name {
			n.cfg.Store.DeleteRoom(e.Room) //nolint:errcheck
		} else if isMember(r.Members, actor) {
			r.Members = removePeer(r.Members, actor)
			n.cfg.Store.UpdateRoom(*r) //nolint:errcheck
			n.emit(Event{Type: "room"})
		}

	case "rename":
		r, err := n.cfg.Store.GetRoom(e.Room)
		if err != nil {
			return
		}
		if actor != r.CreatedBy {
			n.logf("room rename rejected: %q is not the creator", actor)
			return
		}
		r.Name = e.RoomName
		n.cfg.Store.UpdateRoom(*r) //nolint:errcheck
		n.emit(Event{Type: "room"})
	}
}

// nextRoomSenderSeq returns the next per-sender sequence number for a room.
func (n *Node) nextRoomSenderSeq(roomID, sender string) (uint64, error) {
	msgs, err := n.cfg.Store.RoomMessages(roomID, 1000)
	if err != nil {
		return 0, err
	}
	var maxSeq uint64
	for _, m := range msgs {
		if m.Sender == sender && m.SenderSeq > maxSeq {
			maxSeq = m.SenderSeq
		}
	}
	return maxSeq + 1, nil
}

func isMember(members []string, name string) bool {
	for _, m := range members {
		if strings.EqualFold(m, name) {
			return true
		}
	}
	return false
}

func removePeer(members []string, name string) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		if !strings.EqualFold(m, name) {
			out = append(out, m)
		}
	}
	return out
}

func dedupeMembers(members []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, m := range members {
		lower := strings.ToLower(m)
		if !seen[lower] {
			seen[lower] = true
			out = append(out, m)
		}
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
