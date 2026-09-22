package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Mattathiasa/chirp/internal/app"
	"github.com/Mattathiasa/chirp/internal/discovery"
	"github.com/Mattathiasa/chirp/internal/node"
)

func newApp(t *testing.T, hub *discovery.Hub) *app.App {
	t.Helper()
	a, err := app.Open(app.Config{
		DataDir: t.TempDir(), ListenAddr: "127.0.0.1:0",
		NewDisc: func() discovery.Discovery { return hub.New() },
		Tune: func(c *node.Config) {
			c.DialTick = 50 * time.Millisecond
			c.PingEvery = 100 * time.Millisecond
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

type client struct {
	t   *testing.T
	srv *httptest.Server
}

func newClient(t *testing.T, a *app.App) *client {
	srv := httptest.NewServer(New(a).Handler())
	t.Cleanup(srv.Close)
	return &client{t, srv}
}

func (c *client) do(method, path string, body any, hdr map[string]string) (*http.Response, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, rd)
	if method != "GET" {
		req.Header.Set("X-Chirp", "1")
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestSetupFlowAndGuards(t *testing.T) {
	c := newClient(t, newApp(t, discovery.NewHub()))

	if r, b := c.do("GET", "/api/peers", nil, nil); r.StatusCode != 409 {
		t.Fatalf("before setup: %d %s", r.StatusCode, b)
	}
	if r, _ := c.do("POST", "/api/setup", map[string]string{"name": "Alex"}, map[string]string{"X-Chirp": ""}); r.StatusCode != 403 {
		t.Fatalf("missing X-Chirp accepted: %d", r.StatusCode)
	}
	if r, _ := c.do("POST", "/api/setup", map[string]string{"name": "Alex"}, map[string]string{"Origin": "https://evil.example"}); r.StatusCode != 403 {
		t.Fatalf("foreign origin accepted: %d", r.StatusCode)
	}
	if r, _ := c.do("POST", "/api/setup", map[string]string{"name": "  "}, nil); r.StatusCode != 400 {
		t.Fatalf("blank name: %d", r.StatusCode)
	}
	r, b := c.do("POST", "/api/setup", map[string]string{"name": "Alex"}, nil)
	if r.StatusCode != 200 || !strings.Contains(string(b), `"setup":false`) {
		t.Fatalf("setup: %d %s", r.StatusCode, b)
	}
	if r, _ := c.do("POST", "/api/setup", map[string]string{"name": "Again"}, nil); r.StatusCode != 400 {
		t.Fatalf("second setup: %d", r.StatusCode)
	}
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options"} {
		if r.Header.Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
}

func TestHostHeaderAllowList(t *testing.T) {
	c := newClient(t, newApp(t, discovery.NewHub()))
	req, _ := http.NewRequest("GET", c.srv.URL+"/api/state", nil)
	req.Host = "evil.example:7777" // DNS rebinding
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("rebinding host accepted: %d", resp.StatusCode)
	}
}

func TestStaticUIServed(t *testing.T) {
	c := newClient(t, newApp(t, discovery.NewHub()))
	for _, p := range []string{"/", "/app.js", "/style.css"} {
		if r, b := c.do("GET", p, nil, nil); r.StatusCode != 200 || len(b) < 100 {
			t.Fatalf("%s: %d (%d bytes)", p, r.StatusCode, len(b))
		}
	}
}

func TestEndToEndChatOverAPI(t *testing.T) {
	hub := discovery.NewHub()
	ca, cb := newClient(t, newApp(t, hub)), newClient(t, newApp(t, hub))
	ca.do("POST", "/api/setup", map[string]string{"name": "Alex"}, nil)
	cb.do("POST", "/api/setup", map[string]string{"name": "Sam"}, nil)

	waitPeer := func(c *client, name string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			_, b := c.do("GET", "/api/peers", nil, nil)
			var ps []node.PeerView
			json.Unmarshal(b, &ps)
			for _, p := range ps {
				if p.Name == name && p.Online {
					return
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatalf("%s never came online", name)
	}
	waitPeer(ca, "Sam")
	waitPeer(cb, "Alex")

	// Open Sam's SSE stream first so we can observe the inbound message event.
	resp, err := http.Get(cb.srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") && strings.Contains(sc.Text(), `"type":"message"`) {
				got <- sc.Text()
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)

	r, b := ca.do("POST", "/api/peers/Sam/messages", map[string]string{"body": "hi <b>Sam</b>"}, nil)
	if r.StatusCode != 201 {
		t.Fatalf("send: %d %s", r.StatusCode, b)
	}
	select {
	case line := <-got:
		var ev node.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil || ev.Message == nil || ev.Message.Body != "hi <b>Sam</b>" {
			t.Fatalf("unexpected event %s (%v)", line, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no SSE message event")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, b := ca.do("GET", "/api/peers/Sam/messages", nil, nil)
		if strings.Contains(string(b), `"status":"delivered"`) {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	_, b = ca.do("GET", "/api/peers/Sam/messages", nil, nil)
	if !strings.Contains(string(b), `"status":"delivered"`) {
		t.Fatalf("never delivered: %s", b)
	}
	if r, _ := ca.do("POST", "/api/peers/Nobody/messages", map[string]string{"body": "x"}, nil); r.StatusCode != 404 {
		t.Fatalf("unknown peer: %d", r.StatusCode)
	}
	if r, _ := ca.do("POST", "/api/peers/Sam/messages", map[string]string{"body": ""}, nil); r.StatusCode != 400 {
		t.Fatalf("empty body: %d", r.StatusCode)
	}
	if r, _ := ca.do("POST", "/api/peers/Sam/messages", map[string]any{"body": "x", "extra": 1}, nil); r.StatusCode != 400 {
		t.Fatalf("unknown field: %d", r.StatusCode)
	}
}

func TestBackupEndpointRoundTrip(t *testing.T) {
	a := newApp(t, discovery.NewHub())
	c := newClient(t, a)
	c.do("POST", "/api/setup", map[string]string{"name": "Alex"}, nil)
	if r, _ := c.do("POST", "/api/backup", map[string]string{"passphrase": "short"}, nil); r.StatusCode != 400 {
		t.Fatalf("short passphrase: %d", r.StatusCode)
	}
	r, data := c.do("POST", "/api/backup", map[string]string{"passphrase": "a long enough passphrase"}, nil)
	if r.StatusCode != 200 || !strings.Contains(r.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("backup: %d", r.StatusCode)
	}
	// Restore into a brand new app.
	c2 := newClient(t, newApp(t, discovery.NewHub()))
	if r, _ := c2.do("POST", "/api/restore", map[string]string{"backup": string(data), "passphrase": "wrong wrong wrong"}, nil); r.StatusCode != 401 {
		t.Fatalf("wrong passphrase: %d", r.StatusCode)
	}
	r, b := c2.do("POST", "/api/restore", map[string]string{"backup": string(data), "passphrase": "a long enough passphrase"}, nil)
	if r.StatusCode != 200 {
		t.Fatalf("restore: %d %s", r.StatusCode, b)
	}
	_, s1 := c.do("GET", "/api/me", nil, nil)
	_, s2 := c2.do("GET", "/api/me", nil, nil)
	var m1, m2 node.Me
	json.Unmarshal(s1, &m1)
	json.Unmarshal(s2, &m2)
	if m1.FP != m2.FP || m1.Name != m2.Name {
		t.Fatalf("restored identity differs: %s vs %s", m1.FP, m2.FP)
	}
}

// The UI self-hosts its fonts. Under "default-src 'none'" a missing font-src
// directive makes the browser refuse every @font-face, silently, so the
// directive is pinned here along with the rest of the policy.
func TestFontsAreServableUnderTheCSP(t *testing.T) {
	c := newClient(t, newApp(t, discovery.NewHub()))

	r, b := c.do("GET", "/fonts/HankenGrotesk-400.woff2", nil, nil)
	if r.StatusCode != 200 {
		t.Fatalf("font fetch: %d", r.StatusCode)
	}
	if len(b) == 0 {
		t.Fatal("font body is empty")
	}
	if ct := r.Header.Get("Content-Type"); ct != "font/woff2" {
		t.Errorf("Content-Type = %q, want font/woff2", ct)
	}

	csp := r.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("CSP lost its default-src: %q", csp)
	}
	if !strings.Contains(csp, "font-src 'self'") {
		t.Fatalf("CSP has no font-src, so every self-hosted font is blocked: %q", csp)
	}
	// The policy must stay strict in every other respect.
	for _, forbidden := range []string{"unsafe-inline", "unsafe-eval", "http://", "https://", "*"} {
		if strings.Contains(csp, forbidden) {
			t.Errorf("CSP relaxed with %q: %s", forbidden, csp)
		}
	}
}

// Every @font-face URL in the stylesheet must resolve to a file that ships.
func TestEveryFontFaceURLResolves(t *testing.T) {
	c := newClient(t, newApp(t, discovery.NewHub()))

	r, css := c.do("GET", "/style.css", nil, nil)
	if r.StatusCode != 200 {
		t.Fatalf("style.css: %d", r.StatusCode)
	}
	refs := regexp.MustCompile(`url\('([^']+\.woff2?)'\)`).FindAllStringSubmatch(string(css), -1)
	if len(refs) == 0 {
		t.Fatal("no @font-face urls found in style.css")
	}
	for _, m := range refs {
		if rr, body := c.do("GET", m[1], nil, nil); rr.StatusCode != 200 || len(body) == 0 {
			t.Errorf("%s -> %d (%d bytes)", m[1], rr.StatusCode, len(body))
		}
	}
	t.Logf("checked %d font files", len(refs))
}

// These node methods existed with no route at all, so the UI could not reach
// reactions, typing, read receipts, deleting a message, or search.
func TestMessageActionRoutesExist(t *testing.T) {
	hub := discovery.NewHub()
	c := newClient(t, newApp(t, hub))
	if r, b := c.do("POST", "/api/setup", map[string]string{"name": "Alex"}, nil); r.StatusCode != 200 {
		t.Fatalf("setup: %d %s", r.StatusCode, b)
	}

	// Unknown peer is a 404, not a 404-because-there-is-no-such-route.
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/peers/Nobody/react", map[string]string{"emoji": "🎉", "target": "aabbccdd11223344aabbccdd11223344"}},
		{"POST", "/api/peers/Nobody/typing", nil},
		{"POST", "/api/peers/Nobody/read", map[string]string{"target": "aabbccdd11223344aabbccdd11223344"}},
	} {
		r, b := c.do(tc.method, tc.path, tc.body, nil)
		if r.StatusCode == 405 || (r.StatusCode == 404 && strings.Contains(string(b), "page not found")) {
			t.Errorf("%s %s is not routed: %d %s", tc.method, tc.path, r.StatusCode, b)
		}
	}

	// Search answers, and an empty query is not an error.
	r, b := c.do("GET", "/api/search?q=", nil, nil)
	if r.StatusCode != 200 || string(bytes.TrimSpace(b)) != "[]" {
		t.Errorf("empty search: %d %s", r.StatusCode, b)
	}
	if r, b := c.do("GET", "/api/search?q=hello", nil, nil); r.StatusCode != 200 {
		t.Errorf("search: %d %s", r.StatusCode, b)
	}

	// Delete-for-everyone needs to know which peer to ask.
	if r, _ := c.do("DELETE", "/api/messages/aabbccdd11223344aabbccdd11223344?everyone=1", nil, nil); r.StatusCode != 400 {
		t.Errorf("delete for everyone with no peer: %d", r.StatusCode)
	}

	// Mutating routes stay behind the CSRF header.
	for _, p := range []string{"/api/peers/Nobody/react", "/api/peers/Nobody/typing", "/api/messages/x"} {
		m := "POST"
		if strings.HasPrefix(p, "/api/messages") {
			m = "DELETE"
		}
		if r, _ := c.do(m, p, nil, map[string]string{"X-Chirp": ""}); r.StatusCode != 403 {
			t.Errorf("%s %s accepted without X-Chirp: %d", m, p, r.StatusCode)
		}
	}
}
