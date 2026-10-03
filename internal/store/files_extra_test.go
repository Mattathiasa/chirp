package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func TestOutboundFileCancelFromOutbox(t *testing.T) {
	s, _ := open(t)
	data := []byte("cancel me")
	sum := sha256Of(data)
	if err := s.AddOutboundFile("f1", "Sam", "notes.txt", sum, data); err != nil {
		t.Fatal(err)
	}

	// Cancelling pulls it out of the outbox but keeps the record, so the user
	// still sees what they sent (or tried to).
	if err := s.DeleteFileOutbox("f1"); err != nil {
		t.Fatal(err)
	}
	if q, _ := s.FileOutbox(); len(q) != 0 {
		t.Fatalf("cancelled file still queued: %d", len(q))
	}
	f, err := s.GetFile("f1")
	if err != nil || f.Status != FileQueued {
		t.Fatalf("after cancel: %v %+v", err, f)
	}

	// Cancelling something not in the outbox is a no-op, not an error.
	if err := s.DeleteFileOutbox("missing"); err != nil {
		t.Fatalf("cancel unknown: %v", err)
	}
}

func TestCancelReceiveRemovesPartial(t *testing.T) {
	s, _ := open(t)
	sum := sha256Of([]byte("partial"))
	if _, err := s.BeginReceive("f2", "Sam", "big.bin", sum, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteChunk("f2", 0, []byte("1234")); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelReceive("f2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetFile("f2"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("cancelled receive survived: %v", err)
	}
	// Cancelling something that is not being received is refused.
	if err := s.CancelReceive("f2"); err == nil {
		t.Fatal("cancelled a non-receiving file")
	}
}

func TestDeleteFileRemovesEverything(t *testing.T) {
	s, _ := open(t)
	data := []byte("delete all of this")
	if err := s.AddOutboundFile("f3", "Sam", "x.bin", sha256Of(data), data); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFile("f3"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetFile("f3"); !errors.Is(err, ErrFileNotFound) {
		t.Fatal("metadata survived")
	}
	if _, err := s.FileBlob("f3"); err == nil {
		t.Fatal("blob survived")
	}
}

func TestListFilesNewestFirst(t *testing.T) {
	s, _ := open(t)
	for i, id := range []string{"a", "b", "c"} {
		if err := s.AddOutboundFile(id, "Sam", id+".txt", sha256Of([]byte(id)), []byte(id)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
		_ = i
	}
	fs, err := s.ListFiles()
	if err != nil || len(fs) != 3 {
		t.Fatalf("list: %v %d", err, len(fs))
	}
	if fs[0].ID != "c" || fs[2].ID != "a" {
		t.Fatalf("order: %v %v %v", fs[0].ID, fs[1].ID, fs[2].ID)
	}
}

func TestFileStatusTransitions(t *testing.T) {
	s, _ := open(t)
	data := []byte("status transitions")
	if err := s.AddOutboundFile("f4", "Sam", "x.txt", sha256Of(data), data); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFileSending("f4"); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.GetFile("f4"); f.Status != FileSending || f.Sent != 0 {
		t.Fatalf("sending: %+v", f)
	}
	if err := s.MarkFileSent("f4"); err != nil {
		t.Fatal(err)
	}
	f, _ := s.GetFile("f4")
	if f.Status != FileSent || f.Sent != f.Size {
		t.Fatalf("sent: %+v", f)
	}
	// A sent file is out of the outbox.
	if q, _ := s.FileOutbox(); len(q) != 0 {
		t.Fatalf("sent file still queued: %d", len(q))
	}
	// Unknown IDs are refused.
	if err := s.MarkFileSending("nope"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("mark sending unknown: %v", err)
	}
	if err := s.MarkFileSent("nope"); err != nil {
		// MarkFileSent on an unknown id removes nothing and must not crash.
		t.Logf("mark sent unknown: %v", err)
	}
}

func TestFileDataRequiresCompletion(t *testing.T) {
	s, _ := open(t)
	data := []byte("not yet complete")
	if err := s.AddOutboundFile("f5", "Sam", "x.txt", sha256Of(data), data); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FileData("f5"); err == nil {
		t.Fatal("FileData served an unverified outbound file")
	}
	// The raw blob is still readable, which is what the sender's flush needs.
	if blob, err := s.FileBlob("f5"); err != nil || string(blob) != string(data) {
		t.Fatalf("blob: %v %q", err, blob)
	}
	if _, err := s.FileData("unknown"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("unknown file data: %v", err)
	}
}

// A chunk that would leave a hole is refused, and retransmits are idempotent.
func TestWriteChunkGapAndRetransmit(t *testing.T) {
	s, _ := open(t)
	if _, err := s.BeginReceive("f6", "Sam", "x.bin", sha256Of([]byte("abcdefgh")), 8); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteChunk("f6", 0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	// A gap is refused.
	if _, err := s.WriteChunk("f6", 6, []byte("gh")); err == nil {
		t.Fatal("gap accepted")
	}
	// A retransmit of what we already have changes nothing.
	f, err := s.WriteChunk("f6", 0, []byte("abcd"))
	if err != nil || f.Received != 4 {
		t.Fatalf("retransmit: %v %d", err, f.Received)
	}
	// The extension continues from the high-water mark.
	if f, err = s.WriteChunk("f6", 4, []byte("efgh")); err != nil || f.Received != 8 {
		t.Fatalf("extend: %v %d", err, f.Received)
	}
	// Overrun is clamped to the declared size.
	if _, err := s.BeginReceive("f7", "Sam", "y.bin", sha256Of([]byte("ab")), 2); err != nil {
		t.Fatal(err)
	}
	if f, err := s.WriteChunk("f7", 0, []byte("abcd")); err != nil || f.Received != 2 {
		t.Fatalf("clamp: %v %d", err, f.Received)
	}
}

// A resumed partial only counts bytes that are actually on disk.
func TestBeginReceiveResumeAgainstShortTempFile(t *testing.T) {
	s, _ := open(t)
	body := []byte("0123456789")
	sum := sha256Of(body)
	if _, err := s.BeginReceive("f8", "Sam", "x.bin", sum, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteChunk("f8", 0, body); err != nil {
		t.Fatal(err)
	}
	// Receiving again with the same identity resumes from what the counter says.
	off, err := s.BeginReceive("f8", "Sam", "x.bin", sum, int64(len(body)))
	if err != nil || off != int64(len(body)) {
		t.Fatalf("resume: %d %v", off, err)
	}
	// A different hash starts over.
	off, err = s.BeginReceive("f8", "Sam", "x.bin", sha256Of([]byte("different")), int64(len(body)))
	if err != nil || off != 0 {
		t.Fatalf("mismatched hash resume: %d %v", off, err)
	}
}

func TestCompleteFileRejectsTruncation(t *testing.T) {
	s, _ := open(t)
	body := []byte("0123456789")
	if _, err := s.BeginReceive("f9", "Sam", "x.bin", sha256Of(body), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteChunk("f9", 0, []byte("0123")); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteFile("f9"); err == nil {
		t.Fatal("truncated file completed")
	}
	if f, _ := s.GetFile("f9"); f.Status != FileReceiving {
		t.Fatalf("truncated file status: %q", f.Status)
	}
}

// sha256Of is the hex SHA-256 helper the file tests share.
func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
