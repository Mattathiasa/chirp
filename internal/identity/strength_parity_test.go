package identity

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The setup screen scores a passphrase in the browser so the meter reacts as
// you type, without sending the passphrase anywhere. That makes two
// implementations of one rule, which is exactly the kind of pair that drifts.
// This runs the JavaScript one against this one over a spread of inputs.
func TestPassphraseStrengthMatchesTheBrowserMeter(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the browser meter cannot be cross-checked here")
	}

	const appJS = "../web/static/app.js"
	src, err := os.ReadFile(appJS)
	if err != nil {
		t.Fatal(err)
	}
	fn := regexp.MustCompile(`(?s)function passphraseStrength\(pw\) \{.*?\n\}`).Find(src)
	if fn == nil {
		t.Fatalf("passphraseStrength is no longer in %s; update this test or the meter", appJS)
	}

	cases := []string{
		"", "a", "short", "elevenchars", "twelvechars1", "lowercaseonlylongenough",
		"UPPERANDlower", "Tr0ub4dor&3", "Tr0ub4dor&3-horse-battery",
		"correct horse battery staple", "0123456789012345678901",
		"!@#$%^&*()!@#$%^&*()", "MiXeD123!@#abcDEF", "  spaces  and  more  ",
		"ünïcödé-påsswörd-123", strings.Repeat("x", 64),
		// 11 characters but 15 bytes: byte-counting and character-counting
		// land on opposite sides of the 12-character threshold.
		"ünïcödé-123",
		"åäöÅÄÖ", "日本語のパスワード", "🔑🔑🔑🔑🔑🔑🔑🔑🔑🔑🔑🔑",
	}

	var script strings.Builder
	script.Write(fn)
	script.WriteString("\nconst cases = ")
	script.WriteString(jsStringArray(cases))
	script.WriteString(";\nconsole.log(cases.map(passphraseStrength).join(','));\n")

	out, err := exec.Command(node, "-e", script.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("running the browser meter: %v\n%s", err, out)
	}
	got := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(got) != len(cases) {
		t.Fatalf("expected %d scores, got %q", len(cases), out)
	}
	for i, pw := range cases {
		js, err := strconv.Atoi(got[i])
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if want := PassphraseStrength(pw); js != want {
			t.Errorf("%q: browser says %d, Go says %d", pw, js, want)
		}
	}
}

func jsStringArray(ss []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, s := range ss {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Quote(s))
	}
	b.WriteByte(']')
	return b.String()
}
