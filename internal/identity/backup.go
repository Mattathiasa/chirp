package identity

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/flynn/noise"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/scrypt"
)

const (
	backupVersion = 1
	scryptN       = 1 << 15
	scryptR       = 8
	scryptP       = 1
	// MinPassphraseLen is enforced on export, counted in characters.
	MinPassphraseLen = 12
)

// ErrBadPassphrase is returned when a backup fails to decrypt. A wrong
// passphrase and a corrupted file are indistinguishable by design.
var ErrBadPassphrase = errors.New("wrong passphrase or corrupted backup")

// PassphraseStrength returns a score from 0 (terrible) to 5 (excellent)
// based on length and character diversity.
//
// Length is counted in characters, not bytes. Counting bytes scored
// "日本語のパスワード" - nine characters - as though it were twenty, because
// it occupies twenty-seven bytes, and disagreed with the meter in the browser
// for every passphrase that is not pure ASCII.
func PassphraseStrength(pw string) int {
	n := utf8.RuneCountInString(pw)
	if n == 0 {
		return 0
	}
	score := 0
	// length
	switch {
	case n >= 20:
		score += 3
	case n >= 16:
		score += 2
	case n >= 12:
		score += 1
	}
	// character classes
	hasLower, hasUpper, hasDigit, hasSpecial := false, false, false, false
	for _, r := range pw {
		switch {
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= '0' && r <= '9':
			hasDigit = true
		default:
			hasSpecial = true
		}
	}
	classes := 0
	if hasLower {
		classes++
	}
	if hasUpper {
		classes++
	}
	if hasDigit {
		classes++
	}
	if hasSpecial {
		classes++
	}
	score += classes - 1 // 0-3 bonus for classes
	if score > 5 {
		score = 5
	}
	return score
}

type backupFile struct {
	V    int    `json:"v"`
	KDF  string `json:"kdf"`
	N    int    `json:"n"`
	R    int    `json:"r"`
	P    int    `json:"p"`
	Salt []byte `json:"salt"`
	Non  []byte `json:"nonce"`
	Data []byte `json:"data"`
}

type backupPlain struct {
	Name string `json:"name"`
	Priv []byte `json:"priv"`
	Pub  []byte `json:"pub"`
}

// ExportBackup encrypts the identity with a scrypt-derived key and
// ChaCha20-Poly1305. The KDF parameters are stored in the file so they can be
// raised later without breaking old backups.
func ExportBackup(id *Identity, passphrase string) ([]byte, error) {
	if utf8.RuneCountInString(passphrase) < MinPassphraseLen {
		return nil, fmt.Errorf("passphrase must be at least %d characters", MinPassphraseLen)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key, err := scrypt.Key([]byte(passphrase), salt, scryptN, scryptR, scryptP, 32)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	plain, _ := json.Marshal(backupPlain{Name: id.Name, Priv: id.Key.Private, Pub: id.Key.Public})
	hdr := backupFile{V: backupVersion, KDF: "scrypt", N: scryptN, R: scryptR, P: scryptP, Salt: salt, Non: nonce}
	ad, _ := json.Marshal(struct {
		V    int    `json:"v"`
		KDF  string `json:"kdf"`
		N    int    `json:"n"`
		R    int    `json:"r"`
		P    int    `json:"p"`
		Salt []byte `json:"salt"`
	}{hdr.V, hdr.KDF, hdr.N, hdr.R, hdr.P, hdr.Salt})
	hdr.Data = aead.Seal(nil, nonce, plain, ad)
	return json.MarshalIndent(hdr, "", "  ")
}

// ImportBackup decrypts a backup produced by ExportBackup and checks that the
// stored public key matches the private key.
func ImportBackup(data []byte, passphrase string) (*Identity, error) {
	var f backupFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, errors.New("not a chirp backup file")
	}
	if f.V != backupVersion || f.KDF != "scrypt" {
		return nil, errors.New("unsupported backup version")
	}
	// Bound attacker-supplied KDF params so a hostile file cannot exhaust memory.
	if f.N < 1<<14 || f.N > 1<<20 || f.N&(f.N-1) != 0 || f.R != scryptR || f.P < 1 || f.P > 4 || len(f.Salt) != 16 {
		return nil, errors.New("unsupported backup parameters")
	}
	key, err := scrypt.Key([]byte(passphrase), f.Salt, f.N, f.R, f.P, 32)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	if len(f.Non) != aead.NonceSize() {
		return nil, errors.New("corrupted backup")
	}
	ad, _ := json.Marshal(struct {
		V    int    `json:"v"`
		KDF  string `json:"kdf"`
		N    int    `json:"n"`
		R    int    `json:"r"`
		P    int    `json:"p"`
		Salt []byte `json:"salt"`
	}{f.V, f.KDF, f.N, f.R, f.P, f.Salt})
	plain, err := aead.Open(nil, f.Non, f.Data, ad)
	if err != nil {
		return nil, ErrBadPassphrase
	}
	var p backupPlain
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, errors.New("corrupted backup")
	}
	if len(p.Priv) != 32 || len(p.Pub) != 32 {
		return nil, errors.New("corrupted backup")
	}
	// Re-derive the public key rather than trusting the stored one.
	kp, err := noise.DH25519.GenerateKeypair(&fixedReader{p.Priv})
	if err != nil {
		return nil, err
	}
	if string(kp.Public) != string(p.Pub) {
		return nil, errors.New("backup key pair does not match")
	}
	name, err := ValidateName(p.Name)
	if err != nil {
		return nil, err
	}
	return &Identity{Name: name, Key: kp}, nil
}

type fixedReader struct{ b []byte }

func (r *fixedReader) Read(p []byte) (int, error) { return copy(p, r.b), nil }
