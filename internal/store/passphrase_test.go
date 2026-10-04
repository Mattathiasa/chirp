package store

import (
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mattathiasa/chirp/internal/identity"
	bolt "go.etcd.io/bbolt"
)

// A passphrase-protected database refuses to open without the passphrase,
// reads fine with it, and keeps the identity private key out of the raw file.
func TestPassphraseProtectsDatabase(t *testing.T) {
	s, p := open(t)
	id, err := identity.Generate("Alex")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIdentity(id); err != nil {
		t.Fatal(err)
	}
	if err := s.EnablePassphrase("correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if !s.PassphraseSet() {
		t.Fatal("PassphraseSet is false after EnablePassphrase")
	}
	// Data written after enabling is encrypted under the identity key and
	// readable after an unlock.
	if _, dup, err := s.AddMessage(Message{ID: id32(1), Peer: "Sam", Dir: DirIn, Body: "passphrase secret body", TS: time.Now(), Status: StatusReceived}); err != nil || dup {
		t.Fatal(err, dup)
	}
	s.Close()

	// The identity private key must be gone from the raw file in both raw and
	// base64 form. bbolt keeps freed pages around, so compact into a fresh
	// file first: that is what a copy of the database looks like after use.
	raw, err := compactScan(t, p)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(id.Key.Private)
	if strings.Contains(string(raw), b64) {
		t.Fatal("base64 private key found in the database file")
	}
	if strings.Contains(string(raw), string(id.Key.Private)) {
		t.Fatal("raw private key found in the database file")
	}

	// Opening without a passphrase is refused, with and without a key.
	if _, err := Open(p); !errors.Is(err, ErrPassphraseRequired) {
		t.Fatalf("Open without passphrase: %v", err)
	}
	wk := make([]byte, 32)
	if _, err := OpenWithKey(p, wk); !errors.Is(err, ErrPassphraseRequired) {
		t.Fatalf("OpenWithKey on a protected database: %v", err)
	}

	// An empty passphrase is a refusal, not a wrong-password error.
	if _, err := OpenWithPassphrase(p, ""); !errors.Is(err, ErrPassphraseRequired) {
		t.Fatalf("OpenWithPassphrase with empty passphrase: %v", err)
	}

	// A wrong passphrase fails on the identity decrypt.
	s2, err := OpenWithPassphrase(p, "not the right passphrase")
	if err == nil {
		s2.Close()
		t.Fatal("opened a protected database with a wrong passphrase")
	}

	// Message bodies were written under the identity key, so a copy of the
	// database does not contain them either.
	if strings.Contains(string(raw), "passphrase secret body") {
		t.Fatal("message body found in the database file")
	}

	// The right passphrase unlocks everything.
	s3, err := OpenWithPassphrase(p, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	got, err := s3.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Alex" {
		t.Fatalf("identity name: %q", got.Name)
	}
	ms, err := s3.Messages("Sam", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Body != "passphrase secret body" {
		t.Fatalf("messages after unlock: %+v", ms)
	}
}

// EnablePassphrase rejects bad input instead of locking the database.
func TestEnablePassphraseValidation(t *testing.T) {
	s, _ := open(t)
	defer s.Close()

	// Too short.
	if err := s.EnablePassphrase("short"); err == nil {
		t.Fatal("accepted a short passphrase")
	}
	// No identity yet.
	if err := s.EnablePassphrase("a long enough passphrase"); err == nil {
		t.Fatal("accepted a passphrase with no identity to protect")
	}

	id, _ := identity.Generate("Alex")
	if err := s.SaveIdentity(id); err != nil {
		t.Fatal(err)
	}
	if err := s.EnablePassphrase("a long enough passphrase"); err != nil {
		t.Fatal(err)
	}
	// A second passphrase is refused.
	if err := s.EnablePassphrase("another long passphrase"); err == nil {
		t.Fatal("accepted a second passphrase")
	}
}

// compactScan compacts the database at path into a fresh file and returns its
// raw bytes, approximating a copy of the database after normal use.
func compactScan(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	dstPath := path + "-compact"
	dst, err := bolt.Open(dstPath, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	src, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second, ReadOnly: true})
	if err != nil {
		dst.Close()
		return nil, err
	}
	defer src.Close()
	if err := bolt.Compact(dst, src, 0); err != nil {
		dst.Close()
		return nil, err
	}
	if err := dst.Close(); err != nil {
		return nil, err
	}
	t.Cleanup(func() { os.Remove(dstPath) })
	return os.ReadFile(dstPath)
}
