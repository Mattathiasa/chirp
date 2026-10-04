// Package app owns process-level lifecycle. These tests run the lifecycle for
// real: a database on disk, a node on a loopback port, an in-memory discovery
// hub, and goroutine-leak detection so Close() is held to the same standard as
// the node package.
package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mattathiasa/chirp/internal/discovery"
	"github.com/Mattathiasa/chirp/internal/store"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func testConfig(dir string) Config {
	return Config{
		DataDir:    dir,
		ListenAddr: "127.0.0.1:0",
		NewDisc:    func() discovery.Discovery { return discovery.NewHub().New() },
	}
}

// Setup creates an identity, starts the node, and everything survives a close
// and reopen: the identity comes back from disk and the node starts again.
func TestOpenSetupCloseReopen(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(testConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	if a.Node() != nil {
		t.Fatal("a fresh app must not have a running node")
	}
	if _, err := a.Backup("long enough passphrase"); err == nil {
		t.Fatal("Backup before Setup must fail with ErrNeedsSetup")
	}

	if err := a.Setup("Alex"); err != nil {
		t.Fatal(err)
	}
	n := a.Node()
	if n == nil {
		t.Fatal("Setup did not start a node")
	}
	if n.Identity().Name != "Alex" {
		t.Fatalf("identity name = %q", n.Identity().Name)
	}
	// A second Setup is refused: replacing a key must be an explicit reset.
	if err := a.Setup("Bob"); err == nil {
		t.Fatal("second Setup succeeded")
	}
	a.Close()

	// Reopen: the identity persists and the node starts by itself.
	a2, err := Open(testConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	n2 := a2.Node()
	if n2 == nil {
		t.Fatal("node did not restart after reopen")
	}
	if n2.Identity().Name != "Alex" {
		t.Fatalf("name after reopen = %q", n2.Identity().Name)
	}
}

// The same identity can be restored from an encrypted backup, and a wrong
// passphrase is refused before anything is written.
func TestRestoreRoundTrip(t *testing.T) {
	src := t.TempDir()
	a, err := Open(testConfig(src))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Setup("Original"); err != nil {
		t.Fatal(err)
	}
	data, err := a.Backup("a safe passphrase")
	if err != nil {
		t.Fatal(err)
	}
	a.Close()

	// Restoring onto a set-up app is refused.
	if err := a.Restore(data, "a safe passphrase"); err == nil {
		t.Fatal("Restore onto a running app succeeded")
	}

	dst := t.TempDir()
	a2, err := Open(testConfig(dst))
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	if err := a2.Restore(data, "a safe passphrase"); err != nil {
		t.Fatal(err)
	}
	if n := a2.Node(); n == nil || n.Identity().Name != "Original" {
		t.Fatalf("restored node: %v", n)
	}

	// A wrong passphrase must not create an identity.
	dst2 := t.TempDir()
	a3, err := Open(testConfig(dst2))
	if err != nil {
		t.Fatal(err)
	}
	defer a3.Close()
	if err := a3.Restore(data, "wrong passphrase"); err == nil {
		t.Fatal("Restore accepted a wrong passphrase")
	}
	if a3.Node() != nil {
		t.Fatal("a refused restore still started a node")
	}
	if id, _ := a3.St.LoadIdentity(); id != nil {
		t.Fatal("a refused restore still wrote an identity")
	}
}

// The private key is encrypted at rest with a key derived from the identity,
// so the private half must never be readable in the raw database file.
func TestPrivateKeyIsEncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(testConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Setup("Secret Keeper"); err != nil {
		t.Fatal(err)
	}
	priv := a.Node().Identity().Key.Private
	a.Close()

	raw, err := os.ReadFile(filepath.Join(dir, "chirp.db"))
	if err != nil {
		t.Fatal(err)
	}
	if contains(raw, priv) {
		t.Fatal("the private key is in the database file in plaintext")
	}
}

// Open on a path that cannot be created fails with a usable error.
func TestOpenFailsOnBadDataDir(t *testing.T) {
	taken := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(taken, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(testConfig(filepath.Join(taken, "sub"))); err == nil {
		t.Fatal("Open succeeded on an unwritable path")
	}
}

// Restore with a corrupt backup is refused cleanly.
func TestRestoreRejectsGarbage(t *testing.T) {
	a, err := Open(testConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Restore([]byte("not a backup at all"), "a safe passphrase"); err == nil {
		t.Fatal("garbage backup accepted")
	}
}

// A passphrase locks the database: opening without it yields a shell app that
// runs nothing, the wrong passphrase is refused, and the right one restores
// the identity and the data.
func TestPassphraseLocksAndUnlocks(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(testConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Setup("Locked Out"); err != nil {
		t.Fatal(err)
	}
	if _, dup, err := a.St.AddMessage(store.Message{ID: "00112233445566778899aabbccddeeff", Peer: "Sam", Dir: store.DirIn, Body: "locked treasure", TS: time.Now(), Status: store.StatusReceived}); err != nil || dup {
		t.Fatal(err, dup)
	}
	pass := "a long and safe passphrase"
	if err := a.EnablePassphrase(pass); err != nil {
		t.Fatal(err)
	}
	a.Close()

	// Reopening without the passphrase returns a locked shell, not an error.
	locked, err := Open(testConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !locked.NeedsPassphrase() {
		t.Fatal("Open without a passphrase did not lock")
	}
	if !errors.Is(locked.OpenErr(), store.ErrPassphraseRequired) {
		t.Fatalf("OpenErr: %v", locked.OpenErr())
	}
	if locked.Node() != nil || locked.St != nil {
		t.Fatal("the shell runs a node or holds a store")
	}
	locked.Close() // must be safe on a shell

	// The wrong passphrase is refused.
	if _, err := Unlock(testConfig(dir), "not the passphrase"); err == nil {
		t.Fatal("the wrong passphrase opened the database")
	}

	// The right passphrase restores identity and data, both via Unlock and a
	// plain Open with Config.Passphrase.
	a2, err := Unlock(testConfig(dir), pass)
	if err != nil {
		t.Fatal(err)
	}
	if n := a2.Node(); n == nil || n.Identity().Name != "Locked Out" {
		t.Fatalf("unlocked node: %v", a2.Node())
	}
	ms, err := a2.St.Messages("Sam", 10)
	if err != nil || len(ms) != 1 || ms[0].Body != "locked treasure" {
		t.Fatalf("messages after unlock: %v %+v", err, ms)
	}
	a2.Close()

	cfg := testConfig(dir)
	cfg.Passphrase = pass
	a3, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n := a3.Node(); n == nil || n.Identity().Name != "Locked Out" {
		t.Fatalf("open with passphrase: %v", a3.Node())
	}
	a3.Close()
}

func contains(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
