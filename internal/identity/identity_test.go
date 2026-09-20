package identity

import (
	"errors"
	"strings"
	"testing"
)

func TestWordListIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i, w := range wordList {
		if w == "" || w != strings.ToLower(w) {
			t.Fatalf("word %d %q invalid", i, w)
		}
		if seen[w] {
			t.Fatalf("duplicate word %q", w)
		}
		seen[w] = true
	}
}

func TestFingerprintAndWordsStable(t *testing.T) {
	id, err := Generate("Alex")
	if err != nil {
		t.Fatal(err)
	}
	fp := Fingerprint(id.Key.Public)
	if len(fp) != 64 {
		t.Fatalf("fp length %d", len(fp))
	}
	if Fingerprint(id.Key.Public) != fp {
		t.Fatal("fingerprint not deterministic")
	}
	if Words(id.Key.Public) != Words(id.Key.Public) {
		t.Fatal("words not deterministic")
	}
	other, _ := Generate("Sam")
	if Words(other.Key.Public) == Words(id.Key.Public) {
		t.Fatal("two keys produced the same words")
	}
	if g := FingerprintGroups(fp); len(g) != 16 {
		t.Fatalf("groups %d", len(g))
	}
}

func TestValidateName(t *testing.T) {
	for _, bad := range []string{"", "   ", "a\nb", strings.Repeat("x", MaxNameLen+1)} {
		if _, err := ValidateName(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
	if n, err := ValidateName("  Dana R.  "); err != nil || n != "Dana R." {
		t.Fatalf("got %q %v", n, err)
	}
}

func TestBackupRoundTrip(t *testing.T) {
	id, _ := Generate("Alex")
	data, err := ExportBackup(id, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ImportBackup(data, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Alex" || string(got.Key.Public) != string(id.Key.Public) || string(got.Key.Private) != string(id.Key.Private) {
		t.Fatal("restored identity differs")
	}
	if _, err := ImportBackup(data, "wrong passphrase!!"); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("want ErrBadPassphrase, got %v", err)
	}
	if _, err := ExportBackup(id, "short"); err == nil {
		t.Fatal("short passphrase accepted")
	}
}

func TestBackupTamper(t *testing.T) {
	id, _ := Generate("Alex")
	data, _ := ExportBackup(id, "correct horse battery")
	bad := strings.Replace(string(data), `"n": 32768`, `"n": 16384`, 1)
	if _, err := ImportBackup([]byte(bad), "correct horse battery"); err == nil {
		t.Fatal("tampered KDF params accepted")
	}
	if _, err := ImportBackup([]byte(`{"v":1,"kdf":"scrypt","n":1073741824,"r":8,"p":1,"salt":"AAAAAAAAAAAAAAAAAAAAAA=="}`), "x"); err == nil {
		t.Fatal("hostile KDF cost accepted")
	}
}
