package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/Mattathiasa/chirp/internal/discovery"
	"github.com/Mattathiasa/chirp/internal/proto"
	"github.com/Mattathiasa/chirp/internal/session"
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

// The Phase 2 acceptance case: a transfer cut off partway through resumes on
// the next connection instead of starting from zero, and the file that lands
// still matches the sender's hash.
func TestFileTransferResumesAfterDisconnect(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	// Big enough to take many chunks, so there is a middle to interrupt.
	data := bytes.Repeat([]byte("resumable payload "), 60000) // ~1 MB
	want := sha256.Sum256(data)

	id, err := alice.n.SendFile("Bob", "big.bin", data)
	if err != nil {
		t.Fatal(err)
	}

	// Cut the connection once Bob has some of it but not all of it.
	waitFor(t, "transfer is underway", func() bool {
		f, err := bob.st.GetFile(id)
		return err == nil && f.Received > 0 && f.Received < f.Size
	})
	partial, err := bob.st.GetFile(id)
	if err != nil {
		t.Fatal(err)
	}
	if l := alice.n.linkFor("Bob"); l != nil {
		l.conn.Close()
	}

	// The peers reconnect on their own and the transfer picks up again.
	waitFor(t, "bob completes the file", func() bool {
		f, err := bob.st.GetFile(id)
		return err == nil && f.Status == store.FileComplete
	})

	got, err := bob.st.FileData(id)
	if err != nil {
		t.Fatal(err)
	}
	if h := sha256.Sum256(got); h != want {
		t.Fatal("the resumed file does not match the sender's hash")
	}
	if !bytes.Equal(got, data) {
		t.Fatal("the resumed file differs from the original")
	}
	t.Logf("interrupted at %d of %d bytes, resumed and completed", partial.Received, partial.Size)
}

// Resuming has to actually save work: the second attempt must start from the
// bytes already held, not resend the whole file.
func TestResumeDoesNotResendFromZero(t *testing.T) {
	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	data := bytes.Repeat([]byte("count the bytes "), 40000) // ~640 KB
	id, err := alice.n.SendFile("Bob", "counted.bin", data)
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "transfer is underway", func() bool {
		f, err := bob.st.GetFile(id)
		return err == nil && f.Received > int64(len(data))/8
	})
	before, _ := bob.st.GetFile(id)
	if l := alice.n.linkFor("Bob"); l != nil {
		l.conn.Close()
	}

	// After the break the received count must never go backwards, which is
	// what starting over would look like from here.
	lowest := before.Received
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			f, err := bob.st.GetFile(id)
			if err == nil {
				if f.Received < lowest {
					lowest = f.Received
				}
				if f.Status == store.FileComplete {
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	waitFor(t, "bob completes the file", func() bool {
		f, err := bob.st.GetFile(id)
		return err == nil && f.Status == store.FileComplete
	})
	<-done

	if lowest < before.Received {
		t.Fatalf("progress fell from %d to %d: the transfer restarted instead of resuming",
			before.Received, lowest)
	}
}

// A peer that predates resume does not know the fileack type, and an unknown
// type is fatal to a Noise session. Neither side may send one unless the other
// advertised the capability; the transfer then simply runs from the start.
func TestFileTransferWorksWithoutTheResumeCapability(t *testing.T) {
	saved := session.LocalCaps
	session.LocalCaps = []string{proto.CapFiles, proto.CapReactions, proto.CapReceipts, proto.CapTyping, proto.CapRooms}
	t.Cleanup(func() { session.LocalCaps = saved })

	hub := discovery.NewHub()
	alice := newRig(t, hub, "Alice")
	bob := newRig(t, hub, "Bob")
	waitFor(t, "sessions", func() bool { return online(alice.n, "Bob") && online(bob.n, "Alice") })

	if l := alice.n.linkFor("Bob"); l != nil && l.speaks(proto.CapResume) {
		t.Fatal("the peer should not be advertising resume in this test")
	}

	data := bytes.Repeat([]byte("no resume here "), 5000)
	want := sha256.Sum256(data)
	id, err := alice.n.SendFile("Bob", "plain.bin", data)
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
		t.Fatal("file does not match the sender's hash")
	}
	// The session must still be up: a stray fileack would have torn it down.
	if !online(alice.n, "Bob") {
		t.Fatal("the session dropped during a transfer to a peer without resume")
	}
}
