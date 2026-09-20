// Package identity owns the long-term X25519 keypair, the fingerprint and
// "verify with words" derivations, and the encrypted key backup format.
package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/flynn/noise"
)

// Identity is this device's long-term identity.
type Identity struct {
	Name string
	Key  noise.DHKey // Private + Public, X25519
}

// MaxNameLen is the longest display name in runes.
const MaxNameLen = 32

// ValidateName enforces a conservative display-name policy. Names are used as
// the pinning handle, so they are trimmed and must not contain control chars.
func ValidateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name is empty")
	}
	if utf8.RuneCountInString(name) > MaxNameLen {
		return "", fmt.Errorf("name is longer than %d characters", MaxNameLen)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("name contains control characters")
		}
	}
	return name, nil
}

// Generate creates a fresh identity.
func Generate(name string) (*Identity, error) {
	name, err := ValidateName(name)
	if err != nil {
		return nil, err
	}
	kp, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Identity{Name: name, Key: kp}, nil
}

// Fingerprint is the SHA-256 of the public key, lowercase hex (64 chars).
func Fingerprint(pub []byte) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// FingerprintGroups formats a fingerprint as 16 groups of 4 hex chars for
// humans to compare aloud.
func FingerprintGroups(fp string) []string {
	var out []string
	for i := 0; i+4 <= len(fp); i += 4 {
		out = append(out, fp[i:i+4])
	}
	return out
}

// Words derives a six-word short authentication string from a public key.
// 48 bits: enough to stop a casual swap, NOT a substitute for comparing the
// full fingerprint against a determined attacker who can grind keys.
func Words(pub []byte) [6]string {
	h := sha256.Sum256(append([]byte("chirp/words/v1"), pub...))
	var w [6]string
	for i := range w {
		w[i] = wordList[h[i]]
	}
	return w
}

// Colors returns 8 bytes for rendering an identicon: the UI maps them to a
// 5x5 mirrored grid and a hue.
func IdenticonSeed(pub []byte) []byte {
	h := sha256.Sum256(append([]byte("chirp/identicon/v1"), pub...))
	return h[:8]
}
