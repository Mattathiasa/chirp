package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/mattathias/chirp/internal/identity"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "chirp.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, p
}

func id32(i int) string { return fmt.Sprintf("%032x", i) }

func TestIdentityPersists(t *testing.T) {
	s, p := open(t)
	if got, err := s.LoadIdentity(); err != nil || got != nil {
		t.Fatalf("fresh store: %v %v", got, err)
	}
	id, _ := identity.Generate("Alex")
	if err := s.SaveIdentity(id); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIdentity(id); err == nil {
		t.Fatal("overwrote existing identity")
	}
	s.Close()
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, _ := s2.LoadIdentity()
	if got == nil || got.Name != "Alex" || string(got.Key.Private) != string(id.Key.Private) {
		t.Fatal("identity lost across reopen")
	}
}

func TestTOFU(t *testing.T) {
	s, _ := open(t)
	a, _ := identity.Generate("Sam")
	b, _ := identity.Generate("Sam")
	now := time.Now()
	p, ok, err := s.Observe("Sam", a.Key.Public, now)
	if err != nil || !ok || p.Verified {
		t.Fatalf("first contact: %v %v %+v", err, ok, p)
	}
	if _, ok, _ = s.Observe("sam", a.Key.Public, now); !ok {
		t.Fatal("same key under case-different name must match")
	}
	s.SetVerified("Sam", true)
	p, ok, _ = s.Observe("Sam", b.Key.Public, now)
	if ok || p.PendingKey == nil {
		t.Fatal("changed key not flagged")
	}
	got, _ := s.GetPeer("Sam")
	if string(got.Key) != string(a.Key.Public) || !got.Verified {
		t.Fatal("pin was replaced by an unverified key")
	}
	if err := s.AcceptPendingKey("Sam"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetPeer("Sam")
	if string(got.Key) != string(b.Key.Public) || got.Verified || got.PendingKey != nil {
		t.Fatal("accept did not re-pin and reset verification")
	}
	if err := s.AcceptPendingKey("Sam"); err == nil {
		t.Fatal("accept with nothing pending succeeded")
	}
}

func TestMessagesDedupAndOrder(t *testing.T) {
	s, _ := open(t)
	now := time.Now()
	for i := 1; i <= 5; i++ {
		_, dup, err := s.AddMessage(Message{ID: id32(i), Peer: "Sam", Dir: DirIn, Body: fmt.Sprint(i), TS: now, Status: StatusReceived})
		if err != nil || dup {
			t.Fatal(err, dup)
		}
	}
	s.AddMessage(Message{ID: id32(99), Peer: "Samuel", Dir: DirIn, Body: "other", TS: now})
	if _, dup, _ := s.AddMessage(Message{ID: id32(3), Peer: "Sam", Dir: DirIn, Body: "again"}); !dup {
		t.Fatal("duplicate id stored")
	}
	ms, _ := s.Messages("Sam", 3)
	if len(ms) != 3 || ms[0].Body != "3" || ms[2].Body != "5" {
		t.Fatalf("got %+v", ms)
	}
	ms, _ = s.Messages("Samuel", 10)
	if len(ms) != 1 || ms[0].Body != "other" {
		t.Fatalf("prefix leak: %+v", ms)
	}
}

func TestOutboxLifecycle(t *testing.T) {
	s, _ := open(t)
	now := time.Now()
	s.AddMessage(Message{ID: id32(1), Peer: "Sam", Dir: DirOut, Body: "a", TS: now, Status: StatusQueued})
	s.AddMessage(Message{ID: id32(2), Peer: "Dana", Dir: DirOut, Body: "b", TS: now, Status: StatusQueued})
	if p, _ := s.Pending(""); len(p) != 2 {
		t.Fatalf("pending %d", len(p))
	}
	if _, err := s.RecordAttempt(id32(1), now); err != nil {
		t.Fatal(err)
	}
	// Dana cannot ack Sam's message.
	if _, _, err := s.MarkDelivered("Dana", id32(1)); err == nil {
		t.Fatal("cross-peer ack accepted")
	}
	m, changed, err := s.MarkDelivered("Sam", id32(1))
	if err != nil || !changed || m.Status != StatusDelivered || m.Attempts != 1 {
		t.Fatalf("%+v %v %v", m, changed, err)
	}
	if _, changed, _ := s.MarkDelivered("Sam", id32(1)); changed {
		t.Fatal("duplicate ack reported a change")
	}
	if p, _ := s.Pending(""); len(p) != 1 || p[0].Peer != "Dana" {
		t.Fatalf("pending %+v", p)
	}
	if err := s.DeleteFromOutbox(id32(2)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFromOutbox(id32(1)); err == nil {
		t.Fatal("deleted a delivered message from the outbox")
	}
}

func TestPruneKeepsUnsent(t *testing.T) {
	s, _ := open(t)
	old := time.Now().Add(-48 * time.Hour)
	s.AddMessage(Message{ID: id32(1), Peer: "Sam", Dir: DirIn, Body: "old", TS: old})
	s.AddMessage(Message{ID: id32(2), Peer: "Sam", Dir: DirOut, Body: "unsent", TS: old, Status: StatusQueued})
	n, err := s.Prune(time.Now().Add(-24 * time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d %v", n, err)
	}
	ms, _ := s.Messages("Sam", 10)
	if len(ms) != 1 || ms[0].Body != "unsent" {
		t.Fatalf("%+v", ms)
	}
}

func TestForgetPeer(t *testing.T) {
	s, _ := open(t)
	k, _ := identity.Generate("x")
	s.Observe("Sam", k.Key.Public, time.Now())
	s.AddMessage(Message{ID: id32(1), Peer: "Sam", Dir: DirOut, Body: "a", TS: time.Now(), Status: StatusQueued})
	if err := s.ForgetPeer("Sam"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPeer("Sam"); err != ErrNotFound {
		t.Fatal("peer survived")
	}
	if p, _ := s.Pending(""); len(p) != 0 {
		t.Fatal("outbox survived forget")
	}
}

func TestSettingsRange(t *testing.T) {
	s, _ := open(t)
	if err := s.SaveSettings(Settings{RetentionDays: -1}); err == nil {
		t.Fatal("negative retention accepted")
	}
	st := DefaultSettings()
	st.RetentionDays = 7
	if err := s.SaveSettings(st); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Settings(); got.RetentionDays != 7 {
		t.Fatal("settings not persisted")
	}
}
