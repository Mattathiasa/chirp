// Package api serves the local HTTP + SSE interface and the embedded web UI.
//
// Threat model: the daemon binds to loopback, but any web page in the user's
// browser can still make requests to 127.0.0.1. Three defences:
//  1. Host allow-list, which defeats DNS rebinding.
//  2. Mutating requests must carry a custom X-Chirp header. Cross-origin pages
//     cannot set it without a CORS preflight, and we never answer preflights.
//  3. If an Origin header is present on a mutating request it must be our own.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/Mattathiasa/chirp/internal/app"
	"github.com/Mattathiasa/chirp/internal/identity"
	"github.com/Mattathiasa/chirp/internal/node"
	"github.com/Mattathiasa/chirp/internal/store"
	"github.com/Mattathiasa/chirp/internal/web"
)

const maxBody = 64 << 10

// Server is the HTTP handler set.
type Server struct {
	app *app.App
	mux *http.ServeMux
}

// New wires routes.
func New(a *app.App) *Server {
	s := &Server{app: a, mux: http.NewServeMux()}
	m := s.mux

	m.HandleFunc("GET /api/state", s.state)
	m.HandleFunc("POST /api/setup", s.setup)
	m.HandleFunc("POST /api/restore", s.restore)

	m.HandleFunc("GET /api/me", s.needNode(s.me))
	m.HandleFunc("GET /api/peers", s.needNode(s.peers))
	m.HandleFunc("GET /api/peers/{name}/messages", s.needNode(s.messages))
	m.HandleFunc("POST /api/peers/{name}/messages", s.needNode(s.send))
	m.HandleFunc("POST /api/peers/{name}/verify", s.needNode(s.verify))
	m.HandleFunc("POST /api/peers/{name}/accept-key", s.needNode(s.acceptKey))
	m.HandleFunc("POST /api/peers/{name}/retry", s.needNode(s.retry))
	m.HandleFunc("DELETE /api/peers/{name}", s.needNode(s.forget))
	m.HandleFunc("GET /api/outbox", s.needNode(s.outbox))
	m.HandleFunc("DELETE /api/outbox/{id}", s.needNode(s.deleteOutbox))
	m.HandleFunc("GET /api/diagnostics", s.needNode(s.diagnostics))
	m.HandleFunc("GET /api/settings", s.needNode(s.getSettings))
	m.HandleFunc("PUT /api/settings", s.needNode(s.putSettings))
	m.HandleFunc("POST /api/messages/clear", s.needNode(s.clearMessages))
	m.HandleFunc("POST /api/backup", s.needNode(s.backup))
	m.HandleFunc("GET /api/events", s.needNode(s.events))

	static, _ := fs.Sub(web.Files, "static")
	m.Handle("GET /", http.FileServerFS(static))
	return s
}

// Handler returns the hardened handler.
func (s *Server) Handler() http.Handler { return secure(s.mux) }

// ---- middleware ----

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		switch host {
		case "127.0.0.1", "localhost", "[::1]", "::1":
		default:
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Chirp") != "1" {
				http.Error(w, "missing X-Chirp header", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request refused", http.StatusForbidden)
					return
				}
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) needNode(f func(http.ResponseWriter, *http.Request, *node.Node)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n := s.app.Node()
		if n == nil {
			writeErr(w, http.StatusConflict, "setup required")
			return
		}
		f(w, r, n)
	}
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, node.ErrUnknownPeer), errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, node.ErrKeyChanged):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, node.ErrBadBody):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, identity.ErrBadPassphrase):
		writeErr(w, http.StatusUnauthorized, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

// ---- handlers ----

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	n := s.app.Node()
	if n == nil {
		writeJSON(w, 200, map[string]any{"setup": true})
		return
	}
	writeJSON(w, 200, map[string]any{"setup": false, "me": n.Me()})
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := s.app.Setup(in.Name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.state(w, r)
}

func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Backup     string `json:"backup"`
		Passphrase string `json:"passphrase"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := s.app.Restore([]byte(in.Backup), in.Passphrase); err != nil {
		if errors.Is(err, identity.ErrBadPassphrase) {
			fail(w, err)
		} else {
			writeErr(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	s.state(w, r)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, n *node.Node) {
	writeJSON(w, 200, n.Me())
}

func (s *Server) peers(w http.ResponseWriter, r *http.Request, n *node.Node) {
	ps, err := n.Peers()
	if err != nil {
		fail(w, err)
		return
	}
	if ps == nil {
		ps = []node.PeerView{}
	}
	writeJSON(w, 200, ps)
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request, n *node.Node) {
	ms, err := n.Messages(r.PathValue("name"), 300)
	if err != nil {
		fail(w, err)
		return
	}
	if ms == nil {
		ms = []store.Message{}
	}
	writeJSON(w, 200, ms)
}

func (s *Server) send(w http.ResponseWriter, r *http.Request, n *node.Node) {
	var in struct {
		Body string `json:"body"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	m, err := n.Send(r.PathValue("name"), in.Body)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) verify(w http.ResponseWriter, r *http.Request, n *node.Node) {
	var in struct {
		Verified bool `json:"verified"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := n.Verify(r.PathValue("name"), in.Verified); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) acceptKey(w http.ResponseWriter, r *http.Request, n *node.Node) {
	if err := n.AcceptKey(r.PathValue("name")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fail(w, err)
		} else {
			writeErr(w, http.StatusConflict, err.Error())
		}
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request, n *node.Node) {
	n.RetryNow(r.PathValue("name"))
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) forget(w http.ResponseWriter, r *http.Request, n *node.Node) {
	if err := n.Forget(r.PathValue("name")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) outbox(w http.ResponseWriter, r *http.Request, n *node.Node) {
	ms, err := n.Outbox()
	if err != nil {
		fail(w, err)
		return
	}
	if ms == nil {
		ms = []store.Message{}
	}
	writeJSON(w, 200, ms)
}

func (s *Server) deleteOutbox(w http.ResponseWriter, r *http.Request, n *node.Node) {
	if err := n.DeleteFromOutbox(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request, n *node.Node) {
	writeJSON(w, 200, n.Diagnostics())
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request, n *node.Node) {
	st, err := s.app.St.Settings()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request, n *node.Node) {
	var in store.Settings
	if !readJSON(w, r, &in) {
		return
	}
	if err := s.app.St.SaveSettings(in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, 200, in)
}

func (s *Server) clearMessages(w http.ResponseWriter, r *http.Request, n *node.Node) {
	if err := s.app.St.DeleteAllMessages(); err != nil {
		fail(w, err)
		return
	}
	n.Emit(node.Event{Type: "peers"})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) backup(w http.ResponseWriter, r *http.Request, n *node.Node) {
	var in struct {
		Passphrase string `json:"passphrase"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	data, err := s.app.Backup(in.Passphrase)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="chirp-key-backup.json"`)
	_, _ = w.Write(data)
}

// events streams node events as Server-Sent Events.
func (s *Server) events(w http.ResponseWriter, r *http.Request, n *node.Node) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	ch, cancel := n.Subscribe()
	defer cancel()
	fmt.Fprint(w, "retry: 2000\n\n")
	fl.Flush()
	hb := time.NewTicker(15 * time.Second)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-hb.C:
			if _, err := io.WriteString(w, ": hb\n\n"); err != nil {
				return
			}
			fl.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(e)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
