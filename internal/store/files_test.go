package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// receive drives a whole inbound transfer through the store the way link.go
// does: header, chunks, then completion.
func receive(t *testing.T, s *Store, id string, data []byte, hash string, chunk int) error {
	t.Helper()
	if err := s.BeginReceive(id, "Sam", "notes.txt", hash, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	for off := 0; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		if _, err := s.WriteChunk(id, int64(off), data[off:end]); err != nil {
			t.Fatal(err)
		}
	}
	return s.CompleteFile(id)
}

func TestInboundFileRoundTrip(t *testing.T) {
	s, _, _ := openEncrypted(t)
	data := bytes.Repeat([]byte("chirp file payload "), 5000)
	if err := receive(t, s, id32(1), data, sum(data), 4096); err != nil {
		t.Fatal(err)
	}
	f, err := s.GetFile(id32(1))
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != FileComplete {
		t.Fatalf("status = %q, want %q", f.Status, FileComplete)
	}
	got, err := s.FileData(id32(1))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("file data round-trip mismatch")
	}
}

// A chunk corrupted in flight must be rejected. The transfer must not be
// readable and must not be reported as complete.
func TestCorruptedChunkIsRejected(t *testing.T) {
	s, _, _ := openEncrypted(t)
	data := bytes.Repeat([]byte("honest bytes "), 400)
	honest := sum(data)

	corrupt := append([]byte(nil), data...)
	corrupt[17] ^= 0xFF

	err := receive(t, s, id32(2), corrupt, honest, 512)
	if err == nil {
		t.Fatal("CompleteFile accepted a file whose bytes did not match the hash")
	}
	f, err := s.GetFile(id32(2))
	if err != nil {
		t.Fatal(err)
	}
	if f.Status == FileComplete {
		t.Fatal("a hash-mismatched transfer was marked complete")
	}
	if f.Status != FileFailed {
		t.Fatalf("status = %q, want %q", f.Status, FileFailed)
	}
	if _, err := s.FileData(id32(2)); err == nil {
		t.Fatal("corrupted file data is readable")
	}
}

// A transfer that stops short of the advertised size is not complete.
func TestTruncatedTransferIsRejected(t *testing.T) {
	s, _, _ := openEncrypted(t)
	data := bytes.Repeat([]byte("x"), 1000)
	if err := s.BeginReceive(id32(3), "Sam", "short.bin", sum(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteChunk(id32(3), 0, data[:400]); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteFile(id32(3)); err == nil {
		t.Fatal("CompleteFile accepted a truncated transfer")
	}
}

// File bodies live on disk, so they get the same at-rest treatment as messages.
func TestFileDataEncryptedOnDisk(t *testing.T) {
	s, p, _ := openEncrypted(t)
	secret := []byte("the quick brown fox jumps over a secret")
	if err := receive(t, s, id32(4), secret, sum(secret), 16); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(filepath.Dir(p), "files", id32(4), "data.enc"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, secret) {
		t.Fatal("file contents are on disk in plaintext")
	}
}

func TestOutboundFileQueuesAndSends(t *testing.T) {
	s, _, _ := openEncrypted(t)
	data := []byte("outbound payload")
	if err := s.AddOutboundFile(id32(5), "Sam", "out.txt", sum(data), data); err != nil {
		t.Fatal(err)
	}
	q, err := s.FileOutbox()
	if err != nil || len(q) != 1 || q[0].Status != FileQueued {
		t.Fatalf("outbox: %v %v", err, q)
	}
	blob, err := s.FileBlob(id32(5))
	if err != nil || !bytes.Equal(blob, data) {
		t.Fatalf("blob: %v", err)
	}
	if err := s.MarkFileSending(id32(5)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFileSent(id32(5)); err != nil {
		t.Fatal(err)
	}
	q, _ = s.FileOutbox()
	if len(q) != 0 {
		t.Fatalf("a sent file is still in the outbox: %v", q)
	}
}
