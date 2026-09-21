package crypto

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	plaintext := []byte("hello chirp encrypted world")
	ct, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ct, plaintext) {
		t.Fatal("ciphertext equals plaintext")
	}
	if !IsEncrypted(ct) {
		t.Fatal("IsEncrypted returned false")
	}
	pt, err := Decrypt(key, ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("round-trip mismatch: %q vs %q", pt, plaintext)
	}
}

func TestDecryptWrongKey(t *testing.T) {
	k1 := make([]byte, 32)
	k2 := make([]byte, 32)
	rand.Read(k1)
	rand.Read(k2)
	ct, err := Encrypt(k1, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(k2, ct); err == nil {
		t.Fatal("wrong key should fail")
	}
}

func TestDecryptTampered(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	ct, _ := Encrypt(key, []byte("secret"))
	ct[len(ct)-1] ^= 0xff
	if _, err := Decrypt(key, ct); err == nil {
		t.Fatal("tampered ciphertext should fail")
	}
}

func TestDecryptNilEmpty(t *testing.T) {
	pt, err := Decrypt(make([]byte, 32), nil)
	if err != nil || pt != nil {
		t.Fatalf("nil input: %v %v", pt, err)
	}
	pt, err = Decrypt(make([]byte, 32), []byte{})
	if err != nil || pt != nil {
		t.Fatalf("empty input: %v %v", pt, err)
	}
}

func TestIsEncrypted(t *testing.T) {
	if IsEncrypted(nil) {
		t.Fatal("nil reported as encrypted")
	}
	if IsEncrypted([]byte("plaintext")) {
		t.Fatal("plaintext reported as encrypted")
	}
	key := make([]byte, 32)
	rand.Read(key)
	ct, _ := Encrypt(key, []byte("x"))
	if !IsEncrypted(ct) {
		t.Fatal("ciphertext not detected")
	}
}

func TestDeriveKeyFromIdentityDeterministic(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	k1 := DeriveKeyFromIdentity(key)
	k2 := DeriveKeyFromIdentity(key)
	if !bytes.Equal(k1, k2) {
		t.Fatal("HKDF derivation not deterministic")
	}
}

func TestDeriveKeyFromPassphraseDeterministic(t *testing.T) {
	k1, s1 := DeriveKeyFromPassphrase("test passphrase here")
	k2 := DeriveKeyFromPassphraseSalt("test passphrase here", s1)
	if !bytes.Equal(k1, k2) {
		t.Fatal("Argon2 derivation not deterministic")
	}
}

func TestCombineKeysXOR(t *testing.T) {
	a := make([]byte, 32)
	b := make([]byte, 32)
	for i := range a {
		a[i] = byte(i)
		b[i] = byte(i + 10)
	}
	c := CombineKeys(a, b)
	if bytes.Equal(c, a) || bytes.Equal(c, b) {
		t.Fatal("XOR should produce different result")
	}
	// XOR again should recover original
	d := CombineKeys(c, b)
	if !bytes.Equal(d, a) {
		t.Fatal("double XOR should recover original")
	}
}

func TestEnvelopeSize(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	ct, _ := Encrypt(key, []byte("x"))
	// magic(8) + version(1) + nonce(12) + ciphertext(1+16 = 17 for "x") = 38
	if len(ct) != len(magic)+1+nonceLen+1+16 {
		t.Fatalf("unexpected envelope size: %d", len(ct))
	}
}
