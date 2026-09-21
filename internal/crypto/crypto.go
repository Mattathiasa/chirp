// Package crypto provides encryption at rest for the Chirp database. Message
// bodies, peer keys, and the identity private key are encrypted using an
// envelope format: magic + version + nonce + ciphertext.
//
// The encryption key is derived via HKDF from the identity private key with a
// domain-specific context string, or from an optional user passphrase via
// Argon2id. When both are present, the two 32-byte keys are XORed.
package crypto

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	magic    = "CHIRPENC"
	version  = 1
	keyLen   = 32
	nonceLen = 12 // chacha20poly1305 standard nonce
)

// Envelope is the on-disk format for encrypted values.
// Bytes: magic(8) + version(1) + nonce(12) + ciphertext.
// The ciphertext includes the 16-byte Poly1305 tag.

// DeriveKeyFromIdentity derives a 32-byte key from the identity private key
// using HKDF-SHA256 with a domain-specific context.
func DeriveKeyFromIdentity(privKey []byte) []byte {
	h := hkdf.New(sha256.New, privKey, nil, []byte("chirp/at-rest/v1"))
	key := make([]byte, keyLen)
	if _, err := h.Read(key); err != nil {
		panic("crypto: hkdf: " + err.Error())
	}
	return key
}

// DeriveKeyFromPassphrase derives a 32-byte key from a passphrase using
// Argon2id with random salt. The salt is returned so it can be stored.
func DeriveKeyFromPassphrase(passphrase string) (key, salt []byte) {
	salt = make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic("crypto: rand: " + err.Error())
	}
	key = argon2.IDKey([]byte(passphrase), salt, 3, 64*1024, 4, keyLen)
	return key, salt
}

// DeriveKeyFromPassphraseSalt derives a 32-byte key from a passphrase and
// a previously-stored salt using Argon2id.
func DeriveKeyFromPassphraseSalt(passphrase string, salt []byte) []byte {
	return argon2.IDKey([]byte(passphrase), salt, 3, 64*1024, 4, keyLen)
}

// CombineKeys XORs two 32-byte keys.
func CombineKeys(a, b []byte) []byte {
	out := make([]byte, keyLen)
	for i := range out {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// Encrypt encrypts plaintext with the given key using ChaCha20-Poly1305 and
// returns the envelope bytes.
func Encrypt(key, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: aead: %w", err)
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: rand: %w", err)
	}
	ct := aead.Seal(nil, nonce, plaintext, nil)
	var buf bytes.Buffer
	buf.WriteString(magic)
	buf.WriteByte(version)
	buf.Write(nonce)
	buf.Write(ct)
	return buf.Bytes(), nil
}

// Decrypt decrypts an envelope produced by Encrypt. Returns nil, nil for nil
// or empty input (migration from unencrypted stores).
func Decrypt(key, data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if !bytes.HasPrefix(data, []byte(magic)) {
		return nil, errors.New("crypto: not an encrypted envelope")
	}
	if len(data) < len(magic)+1+nonceLen+16 {
		return nil, errors.New("crypto: envelope too short")
	}
	off := len(magic)
	v := data[off]
	off++
	if v != version {
		return nil, fmt.Errorf("crypto: unknown version %d", v)
	}
	nonce := data[off : off+nonceLen]
	off += nonceLen
	ct := data[off:]
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: aead: %w", err)
	}
	pt, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, errors.New("crypto: decryption failed")
	}
	return pt, nil
}

// IsEncrypted returns true if data starts with the envelope magic.
func IsEncrypted(data []byte) bool {
	return bytes.HasPrefix(data, []byte(magic))
}
