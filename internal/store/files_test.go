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
	if _, err := s.BeginReceive(id, "Sam", "notes.txt", hash, int64(len(data))); err != nil {
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
	if _, err := s.BeginReceive(id32(3), "Sam", "short.bin", sum(data), int64(len(data))); err != nil {
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

// An interrupted transfer picks up where it stopped instead of starting over.
func TestResumeContinuesFromThePartial(t *testing.T) {
	s, _, _ := openEncrypted(t)
	data := bytes.Repeat([]byte("resume me "), 3000) // 30 KB
	hash := sum(data)
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"

	// First attempt: the header, then part of the body, then the line drops.
	start, err := s.BeginReceive(id, "Sam", "big.bin", hash, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if start != 0 {
		t.Fatalf("a fresh transfer resumed at %d, want 0", start)
	}
	const cut = 12_000
	if _, err := s.WriteChunk(id, 0, data[:cut]); err != nil {
		t.Fatal(err)
	}

	// Second attempt: the same header must report the bytes already held.
	resume, err := s.BeginReceive(id, "Sam", "big.bin", hash, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if resume != cut {
		t.Fatalf("resumed at %d, want %d", resume, cut)
	}

	// Send only the remainder, as a real sender would.
	if _, err := s.WriteChunk(id, resume, data[resume:]); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteFile(id); err != nil {
		t.Fatal(err)
	}
	got, err := s.FileData(id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("resumed file does not match the original")
	}
}

// A partial for a different file must never be resumed onto: the id could be
// reused, or the sender could have changed the file. Both start again.
func TestResumeRefusesAMismatchedPartial(t *testing.T) {
	s, _, _ := openEncrypted(t)
	first := bytes.Repeat([]byte("one"), 2000)
	second := bytes.Repeat([]byte("two"), 2000)
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa2"

	if _, err := s.BeginReceive(id, "Sam", "a.bin", sum(first), int64(len(first))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteChunk(id, 0, first[:3000]); err != nil {
		t.Fatal(err)
	}

	// Same id, different content: the hash no longer matches the partial.
	resume, err := s.BeginReceive(id, "Sam", "b.bin", sum(second), int64(len(second)))
	if err != nil {
		t.Fatal(err)
	}
	if resume != 0 {
		t.Fatalf("resumed a mismatched partial at %d, want 0", resume)
	}
	if _, err := s.WriteChunk(id, 0, second); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteFile(id); err != nil {
		t.Fatal(err)
	}
	got, _ := s.FileData(id)
	if !bytes.Equal(got, second) {
		t.Fatal("the second file was contaminated by the first partial")
	}
}

// Received is a high-water mark, so a chunk resent after a resume must not
// advance it twice and declare the file complete early.
func TestResentChunksDoNotInflateProgress(t *testing.T) {
	s, _, _ := openEncrypted(t)
	data := bytes.Repeat([]byte("x"), 900)
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa3"
	if _, err := s.BeginReceive(id, "Sam", "d.bin", sum(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // the same opening chunk, three times over
		if _, err := s.WriteChunk(id, 0, data[:300]); err != nil {
			t.Fatal(err)
		}
	}
	f, err := s.GetFile(id)
	if err != nil {
		t.Fatal(err)
	}
	if f.Received != 300 {
		t.Fatalf("three copies of one chunk advanced progress to %d, want 300", f.Received)
	}
	if err := s.CompleteFile(id); err == nil {
		t.Fatal("a file that is only a third here was accepted as complete")
	}
}

// A chunk that starts past the high-water mark would leave a hole, so it is
// refused rather than silently producing a file that fails its hash at the end.
func TestChunkLeavingAGapIsRefused(t *testing.T) {
	s, _, _ := openEncrypted(t)
	data := bytes.Repeat([]byte("y"), 900)
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa4"
	if _, err := s.BeginReceive(id, "Sam", "e.bin", sum(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteChunk(id, 0, data[:100]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteChunk(id, 500, data[500:600]); err == nil {
		t.Fatal("a chunk starting beyond the received bytes was accepted")
	}
	f, _ := s.GetFile(id)
	if f.Received != 100 {
		t.Fatalf("a refused chunk moved progress to %d", f.Received)
	}
}
