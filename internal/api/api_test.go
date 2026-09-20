package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
