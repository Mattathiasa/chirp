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
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Mattathiasa/chirp/internal/app"
	"github.com/Mattathiasa/chirp/internal/identity"
	"github.com/Mattathiasa/chirp/internal/node"
	"github.com/Mattathiasa/chirp/internal/proto"
	"github.com/Mattathiasa/chirp/internal/store"
	"github.com/Mattathiasa/chirp/internal/web"
)

const (
	maxBody       = int64(64 << 10)
	maxFileUpload = int64(proto.MaxFileSize) + (1 << 20)
)

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
	m.HandleFunc("POST /api/me/rename", s.needNode(s.rename))
	m.HandleFunc("GET /api/peers", s.needNode(s.peers))
	m.HandleFunc("GET /api/peers/{name}/messages", s.needNode(s.messages))
	m.HandleFunc("POST /api/peers/{name}/messages", s.needNode(s.send))
	m.HandleFunc("POST /api/peers/{name}/verify", s.needNode(s.verify))
	m.HandleFunc("POST /api/peers/{name}/accept-key", s.needNode(s.acceptKey))
	m.HandleFunc("POST /api/peers/{name}/retry", s.needNode(s.retry))
	m.HandleFunc("POST /api/peers/{name}/react", s.needNode(s.react))
	m.HandleFunc("POST /api/peers/{name}/typing", s.needNode(s.typing))
	m.HandleFunc("POST /api/peers/{name}/read", s.needNode(s.readReceipt))
	m.HandleFunc("DELETE /api/messages/{id}", s.needNode(s.deleteMessage))
	m.HandleFunc("GET /api/search", s.needNode(s.search))
	m.HandleFunc("DELETE /api/peers/{name}", s.needNode(s.forget))
	m.HandleFunc("POST /api/dial", s.needNode(s.dial))
	m.HandleFunc("POST /api/invite/parse", s.needNode(s.parseInvite))
	m.HandleFunc("GET /api/outbox", s.needNode(s.outbox))
	m.HandleFunc("DELETE /api/outbox/{id}", s.needNode(s.deleteOutbox))
	m.HandleFunc("GET /api/diagnostics", s.needNode(s.diagnostics))
	m.HandleFunc("GET /api/settings", s.needNode(s.getSettings))
	m.HandleFunc("PUT /api/settings", s.needNode(s.putSettings))
	m.HandleFunc("POST /api/messages/clear", s.needNode(s.clearMessages))
	m.HandleFunc("POST /api/backup", s.needNode(s.backup))
	m.HandleFunc("GET /api/events", s.needNode(s.events))

	// File routes.
	m.HandleFunc("POST /api/peers/{name}/file", s.needNode(s.sendFile))
	m.HandleFunc("GET /api/files", s.needNode(s.files))
	m.HandleFunc("GET /api/files/{id}", s.needNode(s.getFile))
	m.HandleFunc("GET /api/files/{id}/data", s.needNode(s.fileData))
	m.HandleFunc("DELETE /api/files/{id}", s.needNode(s.deleteFile))

	// Room routes.
	m.HandleFunc("GET /api/rooms", s.needNode(s.rooms))
	m.HandleFunc("POST /api/rooms", s.needNode(s.createRoom))
	m.HandleFunc("GET /api/rooms/{id}", s.needNode(s.getRoom))
	m.HandleFunc("GET /api/rooms/{id}/messages", s.needNode(s.roomMessages))
	m.HandleFunc("POST /api/rooms/{id}/messages", s.needNode(s.sendRoomMessage))
	m.HandleFunc("DELETE /api/rooms/{id}", s.needNode(s.deleteRoom))
	m.HandleFunc("POST /api/rooms/{id}/members", s.needNode(s.addRoomMember))
	m.HandleFunc("DELETE /api/rooms/{id}/members/{name}", s.needNode(s.removeRoomMember))
	m.HandleFunc("POST /api/rooms/{id}/rename", s.needNode(s.renameRoom))
	m.HandleFunc("POST /api/rooms/{id}/messages/{msgId}/react", s.needNode(s.reactRoomMessage))
	m.HandleFunc("POST /api/rooms/{id}/accept", s.needNode(s.acceptRoom))
	m.HandleFunc("POST /api/rooms/{id}/decline", s.needNode(s.declineRoom))

	// Go's built-in table has no entry for woff2, and a host without a system
	// mime database would otherwise serve the fonts as octet-stream.
	_ = mime.AddExtensionType(".woff2", "font/woff2")
	_ = mime.AddExtensionType(".woff", "font/woff")

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
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
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
			limit := maxBody
			if isFileUpload(r) {
				limit = maxFileUpload
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
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

// isFileUpload reports whether the request uploads a file (larger body limit).
func isFileUpload(r *http.Request) bool {
	return strings.Contains(r.URL.Path, "/file") && r.Method == http.MethodPost
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

func (s *Server) rename(w http.ResponseWriter, r *http.Request, n *node.Node) {
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := n.Rename(in.Name); err != nil {
		fail(w, err)
		return
	}
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
		Body    string `json:"body"`
		ReplyTo string `json:"replyTo"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	m, err := n.SendReply(r.PathValue("name"), in.Body, in.ReplyTo)
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

func (s *Server) dial(w http.ResponseWriter, r *http.Request, n *node.Node) {
	var in struct {
		Host string `json:"host"`
		Port string `json:"port"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := n.Dial(in.Host, in.Port); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) parseInvite(w http.ResponseWriter, r *http.Request, n *node.Node) {
	var in struct {
		URI string `json:"uri"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	host, port, fp, err := node.ParseInvite(in.URI)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"host": host, "port": port, "fingerprint": fp})
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

// ---- room handlers ----

func (s *Server) rooms(w http.ResponseWriter, r *http.Request, n *node.Node) {
	rooms, err := n.Rooms()
	if err != nil {
		fail(w, err)
		return
	}
	if rooms == nil {
		rooms = []node.RoomView{}
	}
	writeJSON(w, 200, rooms)
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request, n *node.Node) {
	var in struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	room, err := n.CreateRoom(in.Name, in.Members)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, room)
}

func (s *Server) getRoom(w http.ResponseWriter, r *http.Request, n *node.Node) {
	room, err := n.GetRoom(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, room)
}

func (s *Server) roomMessages(w http.ResponseWriter, r *http.Request, n *node.Node) {
	msgs, err := n.RoomMessages(r.PathValue("id"), 300)
	if err != nil {
		fail(w, err)
		return
	}
	if msgs == nil {
		msgs = []store.RoomMessage{}
	}
	writeJSON(w, 200, msgs)
}

func (s *Server) sendRoomMessage(w http.ResponseWriter, r *http.Request, n *node.Node) {
	var in struct {
		Body string `json:"body"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	msg, err := n.SendRoomMessage(r.PathValue("id"), in.Body)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, msg)
}

func (s *Server) deleteRoom(w http.ResponseWriter, r *http.Request, n *node.Node) {
	if err := n.LeaveRoom(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) addRoomMember(w http.ResponseWriter, r *http.Request, n *node.Node) {
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := n.AddRoomMember(r.PathValue("id"), in.Name); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) removeRoomMember(w http.ResponseWriter, r *http.Request, n *node.Node) {
	if err := n.RemoveRoomMember(r.PathValue("id"), r.PathValue("name")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// react toggles an emoji on a one-to-one message. Reactions are ephemeral for
// one-to-one chats: they are relayed to the peer and surfaced as an event, not
// stored. Room reactions, which do persist, go through the room routes.
func (s *Server) react(w http.ResponseWriter, r *http.Request, n *node.Node) {
	var in struct {
		Emoji  string `json:"emoji"`
		Target string `json:"target"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := n.SendReaction(r.PathValue("name"), in.Emoji, in.Target); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) typing(w http.ResponseWriter, r *http.Request, n *node.Node) {
	if err := n.SendTyping(r.PathValue("name")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) readReceipt(w http.ResponseWriter, r *http.Request, n *node.Node) {
	var in struct {
		Target string `json:"target"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := n.SendReadReceipt(r.PathValue("name"), in.Target); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// deleteMessage removes a message here. With ?everyone=1 it also asks the peer
// to drop their copy, which is a request, not a guarantee: they may be offline,
// may be running something else, and already have the words on their screen.
func (s *Server) deleteMessage(w http.ResponseWriter, r *http.Request, n *node.Node) {
	id := r.PathValue("id")
	if r.URL.Query().Get("everyone") == "1" {
		peer := r.URL.Query().Get("peer")
		if peer == "" {
			writeErr(w, 400, "peer is required to delete for everyone")
			return
		}
		if err := n.DeleteMessageForEveryone(peer, id); err != nil {
			fail(w, err)
			return
		}
	}
	if err := n.DeleteMessage(id); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request, n *node.Node) {
	q := r.URL.Query().Get("q")
	if strings.TrimSpace(q) == "" {
		writeJSON(w, 200, []store.SearchResult{})
		return
	}
	res, err := n.Search(q, 50)
	if err != nil {
		fail(w, err)
		return
	}
	if res == nil {
		res = []store.SearchResult{}
	}
	writeJSON(w, 200, res)
}

func (s *Server) acceptRoom(w http.ResponseWriter, r *http.Request, n *node.Node) {
	if err := n.AcceptRoomInvite(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) declineRoom(w http.ResponseWriter, r *http.Request, n *node.Node) {
	if err := n.DeclineRoomInvite(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) reactRoomMessage(w http.ResponseWriter, r *http.Request, n *node.Node) {
	var in struct {
		Emoji string `json:"emoji"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := n.SendRoomReaction(r.PathValue("id"), r.PathValue("msgId"), in.Emoji); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) renameRoom(w http.ResponseWriter, r *http.Request, n *node.Node) {
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := n.RenameRoom(r.PathValue("id"), in.Name); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- file handlers ----

// sendFile accepts a multipart upload and queues it for the named peer.
func (s *Server) sendFile(w http.ResponseWriter, r *http.Request, n *node.Node) {
	if err := r.ParseMultipartForm(maxFileUpload); err != nil {
		writeErr(w, http.StatusBadRequest, "could not parse upload: "+err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "no file in upload: "+err.Error())
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, proto.MaxFileSize+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read upload: "+err.Error())
		return
	}
	name := sanitizeFilename(header.Filename)
	id, err := n.SendFile(r.PathValue("name"), name, data)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "name": name})
}

func sanitizeFilename(name string) string {
	if name == "" {
		return "file"
	}
	// Strip directory components.
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.LastIndexByte(name, '\\'); i >= 0 {
		name = name[i+1:]
	}
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}

func (s *Server) files(w http.ResponseWriter, r *http.Request, n *node.Node) {
	fs, err := n.Files()
	if err != nil {
		fail(w, err)
		return
	}
	if fs == nil {
		fs = []store.File{}
	}
	writeJSON(w, 200, fs)
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request, n *node.Node) {
	f, err := n.FileMeta(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, f)
}

func (s *Server) fileData(w http.ResponseWriter, r *http.Request, n *node.Node) {
	data, err := n.FileData(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	f, _ := n.FileMeta(r.PathValue("id"))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	if f != nil && f.Name != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, sanitizeFilename(f.Name)))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request, n *node.Node) {
	if err := n.DeleteFile(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
