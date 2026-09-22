package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/Mattathiasa/chirp/internal/discovery"
	"github.com/Mattathiasa/chirp/internal/store"
)

// A file sent over a live session arrives byte-identical and hash-verified.
func TestFileTransferBetweenNodes(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	// Several chunks' worth, so the chunking path is actually exercised.
	data := bytes.Repeat([]byte("chirp payload block "), 12000)
	want := sha256.Sum256(data)

	id, err := alice.n.SendFile("Bob", "report.bin", data)
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "bob completes the file", func() bool {
		f, err := bob.st.GetFile(id)
		return err == nil && f.Status == store.FileComplete
	})

	got, err := bob.st.FileData(id)
	if err != nil {
		t.Fatal(err)
	}
	if h := sha256.Sum256(got); h != want {
		t.Fatal("received file does not match the sender's hash")
	}
	if !bytes.Equal(got, data) {
		t.Fatal("received file bytes differ")
	}

	f, err := bob.st.GetFile(id)
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "report.bin" || f.Size != int64(len(data)) || f.Hash != hex.EncodeToString(want[:]) {
		t.Fatalf("metadata mismatch: %+v", f)
	}

	// The sender's copy leaves the outbox once it has gone out.
	waitFor(t, "alice's file outbox drains", func() bool {
		q, err := alice.st.FileOutbox()
		return err == nil && len(q) == 0
	})
}

// Sending to a peer that was never pinned is refused rather than queued.
func TestSendFileToUnknownPeer(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	if _, err := alice.n.SendFile("Nobody", "x.txt", []byte("hi")); err == nil {
		t.Fatal("expected an error sending a file to an unpinned peer")
	}
}

// An empty file and one over the cap are both rejected before anything is stored.
func TestSendFileSizeBounds(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	newRig(t, hub, "Bob")
	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") })

	if _, err := alice.n.SendFile("Bob", "empty.txt", nil); err == nil {
		t.Fatal("expected an error for an empty file")
	}
	q, _ := alice.st.FileOutbox()
	if len(q) != 0 {
		t.Fatalf("a rejected file was queued anyway: %v", q)
	}
}
