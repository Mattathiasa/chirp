package node

import "testing"

func TestParseInviteValid(t *testing.T) {
	tests := []struct {
		uri  string
		host string
		port string
		fp   string
	}{
		{"chirp://add/192.168.1.5:9001/aabbccdd", "192.168.1.5", "9001", "aabbccdd"},
		{"chirp://add/10.0.0.1:8080/aabbccddeeff0011", "10.0.0.1", "8080", "aabbccddeeff0011"},
		{"chirp://add/[::1]:9001/aabbccdd", "::1", "9001", "aabbccdd"},
	}
	for _, tc := range tests {
		host, port, fp, err := ParseInvite(tc.uri)
		if err != nil {
			t.Errorf("ParseInvite(%q): %v", tc.uri, err)
			continue
		}
		if host != tc.host || port != tc.port || fp != tc.fp {
			t.Errorf("ParseInvite(%q) = (%q, %q, %q), want (%q, %q, %q)",
				tc.uri, host, port, fp, tc.host, tc.port, tc.fp)
		}
	}
}

func TestParseInviteInvalid(t *testing.T) {
	bad := []string{
		"",
		"http://example.com",
		"chirp://add/",
		"chirp://add/host:port",
		"chirp://add/host:port/abc",
		"chirp://add/host:port/GHIJ",
		"chirp://add/:9001/aabbccdd",
		"chirp://add/host:/aabbccdd",
	}
	for _, uri := range bad {
		if _, _, _, err := ParseInvite(uri); err == nil {
			t.Errorf("ParseInvite(%q) should fail", uri)
		}
	}
}

func TestFormatInvite(t *testing.T) {
	uri := FormatInvite("192.168.1.5", "9001", "aabbccddeeff00112233445566778899")
	if uri != "chirp://add/192.168.1.5:9001/aabbccddeeff0011" {
		t.Errorf("FormatInvite = %q", uri)
	}
}
