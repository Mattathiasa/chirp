// Package store persists identity, pinned peers, messages and the outbox in a
// single bbolt file. bbolt gives us serialisable transactions and crash safety,
// which is what the at-least-once delivery story leans on.
package store

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/flynn/noise"

	"github.com/Mattathiasa/chirp/internal/crypto"
	"github.com/Mattathiasa/chirp/internal/identity"
)

var (
	bMeta       = []byte("meta")
	bPeers      = []byte("peers")
	bMsgs       = []byte("msgs")       // key: peerKey 0x00 seq(8)
	bIdx        = []byte("msgidx")     // key: message id -> msgs key
	bOutbox     = []byte("outbox")     // key: message id -> msgs key (pending outbound)
	bRooms      = []byte("rooms")      // key: roomID -> JSON Room
	bRoomMsgs   = []byte("roommsgs")   // key: roomID 0x00 seq(8) -> JSON RoomMessage
	bRoomIdx    = []byte("roomidx")    // key: message id -> roommsgs key
	bRoomOutbox = []byte("roomoutbox") // key: roomID 0x00 memberName -> JSON {msgID}
	bRoomSeq    = []byte("roomseq")    // key: roomID 0x00 sender -> uint64 per-sender counter
	bFiles      = []byte("files")      // key: fileID -> JSON File
	bFileOutbox = []byte("fileoutbox") // key: fileID -> fileID (pending outbound)
)

// Message directions and statuses.
const (
	DirIn  = "in"
	DirOut = "out"

	StatusQueued    = "queued"    // written locally, not yet delivered
	StatusDelivered = "delivered" // peer acked
	StatusReceived  = "received"  // inbound
)

// ErrNotFound is returned for missing peers and messages.
var ErrNotFound = errors.New("store: not found")

// Peer is a pinned contact. The lower-cased Name is the pinning handle.
type Peer struct {
	Name       string    `json:"name"`
	Key        []byte    `json:"key"`
	FP         string    `json:"fp"`
	FirstSeen  time.Time `json:"firstSeen"`
	LastSeen   time.Time `json:"lastSeen"`
	Verified   bool      `json:"verified"`
	PendingKey []byte    `json:"pendingKey,omitempty"` // a different key claimed this name
	PendingAt  time.Time `json:"pendingAt,omitempty"`
}

// Message is one chat message in either direction.
type Message struct {
	ID       string    `json:"id"`
	Peer     string    `json:"peer"` // display name as pinned
	Dir      string    `json:"dir"`
	Body     string    `json:"body"`
	TS       time.Time `json:"ts"` // local time written or received
	Status   string    `json:"status"`
	Attempts int       `json:"attempts"`
	LastTry  time.Time `json:"lastTry,omitempty"`
	Seq      uint64    `json:"seq"`
	ReplyTo  string    `json:"replyTo,omitempty"` // message ID being quoted
}

// Settings are user-tunable persisted options.
type Settings struct {
	RetentionDays int  `json:"retentionDays"` // 0 = keep forever
	Notify        bool `json:"notify"`        // browser notifications while the tab is hidden
	Previews      bool `json:"previews"`      // show message text in notifications and the peer list
}

// DefaultSettings are used until the user changes something.
func DefaultSettings() Settings {
	return Settings{RetentionDays: 0, Notify: true, Previews: false}
}

// Store wraps a bbolt DB with optional encryption at rest.
type Store struct {
	db       *bolt.DB
	encKey   []byte // nil = plaintext
	filesDir string // on-disk encrypted file storage
}

// SetKey sets the encryption key after the store is opened, typically once the
// identity has been loaded (the key is derived from it). Must be called before
// concurrent use. Existing plaintext values remain readable; new writes are
// encrypted.
func (s *Store) SetKey(key []byte) { s.encKey = key }

// Key returns the current encryption key (nil if unencrypted).
func (s *Store) Key() []byte { return s.encKey }

// Open opens or creates the database at path without encryption.
func Open(path string) (*Store, error) {
	return openStore(path, nil)
}

// OpenWithKey opens the database with an encryption key. Message bodies,
// peer keys, and the identity private key are encrypted at rest.
func OpenWithKey(path string, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, errors.New("store: encryption key must be 32 bytes")
	}
	return openStore(path, key)
}

func openStore(path string, encKey []byte) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bMeta, bPeers, bMsgs, bIdx, bOutbox, bRooms, bRoomMsgs, bRoomIdx, bRoomOutbox, bRoomSeq, bFiles, bFileOutbox} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	filesDir := filepath.Join(filepath.Dir(path), "files")
	_ = os.MkdirAll(filesDir, 0o700)
	return &Store{db: db, encKey: encKey, filesDir: filesDir}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// ---- encryption helpers ----

func (s *Store) encrypt(plaintext []byte) ([]byte, error) {
	if s.encKey == nil || len(plaintext) == 0 {
		return plaintext, nil
	}
	return crypto.Encrypt(s.encKey, plaintext)
}

func (s *Store) decrypt(data []byte) ([]byte, error) {
	if s.encKey == nil || !crypto.IsEncrypted(data) {
		return data, nil
	}
	return crypto.Decrypt(s.encKey, data)
}

func (s *Store) encryptString(s2 string) (string, error) {
	if s.encKey == nil || s2 == "" {
		return s2, nil
	}
	ct, err := crypto.Encrypt(s.encKey, []byte(s2))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (s *Store) decryptString(s2 string) (string, error) {
	if s.encKey == nil || s2 == "" {
		return s2, nil
	}
	raw, err := base64.StdEncoding.DecodeString(s2)
	if err != nil {
		return "", fmt.Errorf("store: base64 decode: %w", err)
	}
	if !crypto.IsEncrypted(raw) {
		return s2, nil
	}
	pt, err := crypto.Decrypt(s.encKey, raw)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

func peerKey(name string) []byte { return []byte(strings.ToLower(strings.TrimSpace(name))) }

// ---- identity & settings ----

type idRecord struct {
	Name string `json:"name"`
	Priv []byte `json:"priv"`
	Pub  []byte `json:"pub"`
}

// LoadIdentity returns nil, nil when no identity exists yet.
func (s *Store) LoadIdentity() (*identity.Identity, error) {
	var rec idRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bMeta).Get([]byte("identity"))
		if v == nil {
			return ErrNotFound
		}
		return json.Unmarshal(v, &rec)
	})
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	priv, err := s.decrypt(rec.Priv)
	if err != nil {
		return nil, fmt.Errorf("store: decrypt identity key: %w", err)
	}
	rec.Priv = priv
	return &identity.Identity{Name: rec.Name, Key: noise.DHKey{Private: rec.Priv, Public: rec.Pub}}, nil
}

// SaveIdentity stores the identity. It refuses to overwrite an existing one:
// replacing a key is destructive and must be an explicit reset.
func (s *Store) SaveIdentity(id *identity.Identity) error {
	priv, err := s.encrypt(id.Key.Private)
	if err != nil {
		return fmt.Errorf("store: encrypt identity key: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bMeta)
		if b.Get([]byte("identity")) != nil {
			return errors.New("store: identity already exists")
		}
		v, _ := json.Marshal(idRecord{Name: id.Name, Priv: priv, Pub: id.Key.Public})
		return b.Put([]byte("identity"), v)
	})
}

// RenameIdentity changes the display name, keeping the key.
func (s *Store) RenameIdentity(name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bMeta)
		v := b.Get([]byte("identity"))
		if v == nil {
			return ErrNotFound
		}
		var rec idRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			return err
		}
		rec.Name = name
		nv, _ := json.Marshal(rec)
		return b.Put([]byte("identity"), nv)
	})
}

// Settings returns persisted settings or defaults.
func (s *Store) Settings() (Settings, error) {
	st := DefaultSettings()
	err := s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bMeta).Get([]byte("settings")); v != nil {
			return json.Unmarshal(v, &st)
		}
		return nil
	})
	return st, err
}

// SaveSettings persists settings.
func (s *Store) SaveSettings(st Settings) error {
	if st.RetentionDays < 0 || st.RetentionDays > 3650 {
		return errors.New("store: retention out of range")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		v, _ := json.Marshal(st)
		return tx.Bucket(bMeta).Put([]byte("settings"), v)
	})
}

// ---- peers ----

func getPeer(tx *bolt.Tx, name string) (*Peer, error) {
	v := tx.Bucket(bPeers).Get(peerKey(name))
	if v == nil {
		return nil, ErrNotFound
	}
	var p Peer
	if err := json.Unmarshal(v, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func putPeer(tx *bolt.Tx, p *Peer) error {
	v, _ := json.Marshal(p)
	return tx.Bucket(bPeers).Put(peerKey(p.Name), v)
}

// GetPeer returns the pinned peer by name. The Key field is decrypted.
func (s *Store) GetPeer(name string) (*Peer, error) {
	var p *Peer
	err := s.db.View(func(tx *bolt.Tx) (err error) { p, err = getPeer(tx, name); return })
	if err != nil {
		return nil, err
	}
	plain, derr := s.decrypt(p.Key)
	if derr != nil {
		return nil, fmt.Errorf("store: decrypt peer key: %w", derr)
	}
	p.Key = plain
	return p, nil
}

// ListPeers returns all pinned peers ordered by name. Keys are decrypted.
func (s *Store) ListPeers() ([]Peer, error) {
	var out []Peer
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bPeers).ForEach(func(_, v []byte) error {
			var p Peer
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			plain, derr := s.decrypt(p.Key)
			if derr != nil {
				return fmt.Errorf("store: decrypt peer key: %w", derr)
			}
			p.Key = plain
			if len(p.PendingKey) > 0 {
				pk, derr := s.decrypt(p.PendingKey)
				if derr != nil {
					return fmt.Errorf("store: decrypt pending key: %w", derr)
				}
				p.PendingKey = pk
			}
			out = append(out, p)
			return nil
		})
	})
	return out, err
}

// Observe records a successful authenticated handshake with (name, key) and
// applies TOFU: an unknown name is pinned, a known name with the same key is
// refreshed, and a known name with a DIFFERENT key is flagged (never replaced).
// It returns the resulting peer and whether the key matches the pin.
func (s *Store) Observe(name string, key []byte, now time.Time) (*Peer, bool, error) {
	encKey, err := s.encrypt(key)
	if err != nil {
		return nil, false, fmt.Errorf("store: encrypt peer key: %w", err)
	}
	var (
		peer *Peer
		ok   bool
	)
	err = s.db.Update(func(tx *bolt.Tx) error {
		p, err := getPeer(tx, name)
		switch {
		case errors.Is(err, ErrNotFound):
			p = &Peer{Name: name, Key: append([]byte(nil), encKey...), FP: identity.Fingerprint(key), FirstSeen: now, LastSeen: now}
			ok = true
		case err != nil:
			return err
		default:
			storedKey, derr := s.decrypt(p.Key)
			if derr != nil {
				return fmt.Errorf("store: decrypt peer key: %w", derr)
			}
			if bytes.Equal(storedKey, key) {
				p.LastSeen = now
				p.PendingKey, p.PendingAt = nil, time.Time{}
				ok = true
			} else {
				p.PendingKey = append([]byte(nil), encKey...)
				p.PendingAt = now
				ok = false
			}
		}
		peer = p
		return putPeer(tx, p)
	})
	return peer, ok, err
}

// SetVerified marks the pinned key as verified out of band.
func (s *Store) SetVerified(name string, v bool) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		p, err := getPeer(tx, name)
		if err != nil {
			return err
		}
		p.Verified = v
		return putPeer(tx, p)
	})
}

// AcceptPendingKey re-pins a peer to the key that most recently claimed its
// name. Verification is reset: the new key has not been checked by anyone.
func (s *Store) AcceptPendingKey(name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		p, err := getPeer(tx, name)
		if err != nil {
			return err
		}
		if p.PendingKey == nil {
			return errors.New("store: no pending key")
		}
		plainKey, derr := s.decrypt(p.PendingKey)
		if derr != nil {
			return fmt.Errorf("store: decrypt pending key: %w", derr)
		}
		p.Key, p.FP = p.PendingKey, identity.Fingerprint(plainKey)
		p.PendingKey, p.PendingAt, p.Verified = nil, time.Time{}, false
		return putPeer(tx, p)
	})
}

// ForgetPeer deletes the pin and all messages with that peer.
func (s *Store) ForgetPeer(name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if _, err := getPeer(tx, name); err != nil {
			return err
		}
		if err := deleteMessages(tx, func(m *Message) bool { return strings.EqualFold(m.Peer, name) }); err != nil {
			return err
		}
		return tx.Bucket(bPeers).Delete(peerKey(name))
	})
}

// ---- messages ----

func msgKey(peer string, seq uint64) []byte {
	k := append(peerKey(peer), 0)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], seq)
	return append(k, b[:]...)
}

// AddMessage stores m. If a message with the same ID exists it returns
// dup=true and stores nothing: this is the receiver-side dedup that turns
// at-least-once delivery into exactly-once display.
func (s *Store) AddMessage(m Message) (stored Message, dup bool, err error) {
	encBody, cerr := s.encryptString(m.Body)
	if cerr != nil {
		return Message{}, false, fmt.Errorf("store: encrypt body: %w", cerr)
	}
	m.Body = encBody
	err = s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bIdx).Get([]byte(m.ID)) != nil {
			dup = true
			return nil
		}
		seq, err := tx.Bucket(bMsgs).NextSequence()
		if err != nil {
			return err
		}
		m.Seq = seq
		k := msgKey(m.Peer, seq)
		v, _ := json.Marshal(m)
		if err := tx.Bucket(bMsgs).Put(k, v); err != nil {
			return err
		}
		if err := tx.Bucket(bIdx).Put([]byte(m.ID), k); err != nil {
			return err
		}
		if m.Dir == DirOut && m.Status != StatusDelivered {
			if err := tx.Bucket(bOutbox).Put([]byte(m.ID), k); err != nil {
				return err
			}
		}
		stored = m
		return nil
	})
	if err == nil {
		if derr := s.decryptMessage(&stored); derr != nil {
			return Message{}, false, derr
		}
	}
	return
}

func loadByID(tx *bolt.Tx, id string) (*Message, []byte, error) {
	k := tx.Bucket(bIdx).Get([]byte(id))
	if k == nil {
		return nil, nil, ErrNotFound
	}
	v := tx.Bucket(bMsgs).Get(k)
	if v == nil {
		return nil, nil, ErrNotFound
	}
	var m Message
	if err := json.Unmarshal(v, &m); err != nil {
		return nil, nil, err
	}
	return &m, append([]byte(nil), k...), nil
}

func (s *Store) decryptMessage(m *Message) error {
	body, err := s.decryptString(m.Body)
	if err != nil {
		return fmt.Errorf("store: decrypt body: %w", err)
	}
	m.Body = body
	return nil
}

// MarkDelivered flips an outbound message to delivered. The peer must match so
// one peer cannot ack another peer's messages. Returns the updated message and
// whether anything changed (duplicate acks are harmless).
func (s *Store) MarkDelivered(peer, id string) (Message, bool, error) {
	var (
		out     Message
		changed bool
	)
	err := s.db.Update(func(tx *bolt.Tx) error {
		m, k, err := loadByID(tx, id)
		if err != nil {
			return err
		}
		if m.Dir != DirOut || !strings.EqualFold(m.Peer, peer) {
			return ErrNotFound
		}
		out = *m
		if m.Status == StatusDelivered {
			return nil
		}
		m.Status = StatusDelivered
		v, _ := json.Marshal(m)
		if err := tx.Bucket(bMsgs).Put(k, v); err != nil {
			return err
		}
		if err := tx.Bucket(bOutbox).Delete([]byte(id)); err != nil {
			return err
		}
		out, changed = *m, true
		return nil
	})
	if err == nil {
		if derr := s.decryptMessage(&out); derr != nil {
			return Message{}, false, derr
		}
	}
	return out, changed, err
}

// RecordAttempt bumps the attempt counter for an outbound message.
func (s *Store) RecordAttempt(id string, now time.Time) (Message, error) {
	var out Message
	err := s.db.Update(func(tx *bolt.Tx) error {
		m, k, err := loadByID(tx, id)
		if err != nil {
			return err
		}
		m.Attempts++
		m.LastTry = now
		v, _ := json.Marshal(m)
		out = *m
		return tx.Bucket(bMsgs).Put(k, v)
	})
	if err == nil {
		if derr := s.decryptMessage(&out); derr != nil {
			return Message{}, derr
		}
	}
	return out, err
}

// Messages returns up to limit most recent messages with a peer, oldest first.
func (s *Store) Messages(peer string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var out []Message
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bMsgs).Cursor()
		prefix := append(peerKey(peer), 0)
		upper := append(append([]byte(nil), prefix...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
		k, v := c.Seek(upper) // first key >= upper; real keys never equal it
		if k == nil {
			k, v = c.Last()
		} else {
			k, v = c.Prev()
		}
		for ; k != nil && bytes.HasPrefix(k, prefix) && len(out) < limit; k, v = c.Prev() {
			var m Message
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			if derr := s.decryptMessage(&m); derr != nil {
				return derr
			}
			out = append(out, m)
		}
		return nil
	})
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, err
}

// Pending returns undelivered outbound messages, oldest first. peer == "" means all peers.
func (s *Store) Pending(peer string) ([]Message, error) {
	var out []Message
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bOutbox).ForEach(func(_, k []byte) error {
			v := tx.Bucket(bMsgs).Get(k)
			if v == nil {
				return nil
			}
			var m Message
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			if derr := s.decryptMessage(&m); derr != nil {
				return derr
			}
			if peer == "" || strings.EqualFold(m.Peer, peer) {
				out = append(out, m)
			}
			return nil
		})
	})
	sortBySeq(out)
	return out, err
}

// DeleteFromOutbox removes an undelivered outbound message entirely.
func (s *Store) DeleteFromOutbox(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		m, k, err := loadByID(tx, id)
		if err != nil {
			return err
		}
		if m.Dir != DirOut || m.Status == StatusDelivered {
			return ErrNotFound
		}
		tx.Bucket(bOutbox).Delete([]byte(id)) //nolint:errcheck
		tx.Bucket(bIdx).Delete([]byte(id))    //nolint:errcheck
		return tx.Bucket(bMsgs).Delete(k)
	})
}

// DeleteAllMessages removes every message but keeps identity and pins.
func (s *Store) DeleteAllMessages() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return deleteMessages(tx, func(*Message) bool { return true })
	})
}

// Prune deletes delivered/received messages older than cutoff. Undelivered
// outbound messages are never pruned: they are the user's unsent intent.
func (s *Store) Prune(cutoff time.Time) (int, error) {
	n := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		return deleteMessages(tx, func(m *Message) bool {
			if m.Dir == DirOut && m.Status != StatusDelivered {
				return false
			}
			if m.TS.Before(cutoff) {
				n++
				return true
			}
			return false
		})
	})
	return n, err
}

// DeleteMessage removes a single message by ID (delete-for-me or delete-for-everyone).
func (s *Store) DeleteMessage(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		m, k, err := loadByID(tx, id)
		if err != nil {
			return err
		}
		tx.Bucket(bIdx).Delete([]byte(m.ID))    //nolint:errcheck
		tx.Bucket(bOutbox).Delete([]byte(m.ID)) //nolint:errcheck
		return tx.Bucket(bMsgs).Delete(k)
	})
}

// SearchResult is one result from full-text search.
type SearchResult struct {
	Message Message `json:"message"`
	Snippet string  `json:"snippet"`
}

// Search performs case-insensitive full-text search over decrypted messages.
func (s *Store) Search(query string, limit int) ([]SearchResult, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query = strings.ToLower(query)
	if query == "" {
		return nil, nil
	}
	var out []SearchResult
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bMsgs).ForEach(func(_, v []byte) error {
			if len(out) >= limit {
				return nil
			}
			var m Message
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			if derr := s.decryptMessage(&m); derr != nil {
				return derr
			}
			body := strings.ToLower(m.Body)
			if strings.Contains(body, query) {
				snippet := m.Body
				if idx := strings.Index(strings.ToLower(snippet), query); idx >= 0 {
					start := idx - 20
					if start < 0 {
						start = 0
					}
					end := idx + len(query) + 40
					if end > len(snippet) {
						end = len(snippet)
					}
					snippet = snippet[start:end]
					if start > 0 {
						snippet = "..." + snippet
					}
					if end < len(m.Body) {
						snippet = snippet + "..."
					}
				}
				out = append(out, SearchResult{Message: m, Snippet: snippet})
			}
			return nil
		})
	})
	return out, err
}

func deleteMessages(tx *bolt.Tx, pred func(*Message) bool) error {
	b := tx.Bucket(bMsgs)
	var keys [][]byte
	var ids []string
	err := b.ForEach(func(k, v []byte) error {
		var m Message
		if err := json.Unmarshal(v, &m); err != nil {
			return err
		}
		if pred(&m) {
			keys = append(keys, append([]byte(nil), k...))
			ids = append(ids, m.ID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i, k := range keys {
		if err := b.Delete(k); err != nil {
			return err
		}
		tx.Bucket(bIdx).Delete([]byte(ids[i]))    //nolint:errcheck
		tx.Bucket(bOutbox).Delete([]byte(ids[i])) //nolint:errcheck
	}
	return nil
}

func sortBySeq(m []Message) {
	for i := 1; i < len(m); i++ {
		for j := i; j > 0 && m[j].Seq < m[j-1].Seq; j-- {
			m[j], m[j-1] = m[j-1], m[j]
		}
	}
}

// ---- rooms ----

const MaxRoomMembers = 32

// Room is a group chat. Membership changes arrive as events over an
// authenticated session; see node.handleRoomEvent for what authorises one.
type Room struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Members   []string  `json:"members"` // display names (sorted)
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
}

// RoomMessage is one message in a room.
type RoomMessage struct {
	ID        string    `json:"id"`
	Room      string    `json:"room"`   // room ID
	Sender    string    `json:"sender"` // display name of sender
	Dir       string    `json:"dir"`    // "in" or "out"
	Body      string    `json:"body"`
	TS        time.Time `json:"ts"`     // local time written or received
	Status    string    `json:"status"` // "queued" | "delivered" | "received"
	Attempts  int       `json:"attempts"`
	LastTry   time.Time `json:"lastTry,omitempty"`
	Seq       uint64    `json:"seq"`
	ReplyTo   string    `json:"replyTo,omitempty"`
	SenderSeq uint64    `json:"senderSeq,omitempty"` // per-sender sequence number
}

// roomMsgKey builds a bbolt key for room messages.
func roomMsgKey(roomID string, seq uint64) []byte {
	k := append([]byte(strings.ToLower(roomID)), 0)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], seq)
	return append(k, b[:]...)
}

// roomOutboxKey builds a per-message outbox key for room messages.
func roomOutboxKey(roomID, member, msgID string) []byte {
	return []byte(strings.ToLower(roomID) + ":" + strings.ToLower(member) + ":" + strings.ToLower(msgID))
}

func (s *Store) encryptRoom(r *Room) (*Room, error) {
	name, err := s.encryptString(r.Name)
	if err != nil {
		return nil, err
	}
	r.Name = name
	return r, nil
}

func (s *Store) decryptRoom(r *Room) error {
	name, err := s.decryptString(r.Name)
	if err != nil {
		return err
	}
	r.Name = name
	return nil
}

func (s *Store) decryptRoomMessage(m *RoomMessage) error {
	body, err := s.decryptString(m.Body)
	if err != nil {
		return err
	}
	m.Body = body
	return nil
}

// CreateRoom creates a new room with the given members.
func (s *Store) CreateRoom(r Room) error {
	if len(r.Members) > MaxRoomMembers {
		return fmt.Errorf("store: room has %d members, max is %d", len(r.Members), MaxRoomMembers)
	}
	enc, err := s.encryptRoom(&r)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		v, _ := json.Marshal(enc)
		return tx.Bucket(bRooms).Put([]byte(r.ID), v)
	})
}

// GetRoom returns a room by ID. Members are decrypted.
func (s *Store) GetRoom(id string) (*Room, error) {
	var r Room
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bRooms).Get([]byte(id))
		if v == nil {
			return ErrNotFound
		}
		return json.Unmarshal(v, &r)
	})
	if err != nil {
		return nil, err
	}
	if err := s.decryptRoom(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListRooms returns all rooms the user is a member of.
func (s *Store) ListRooms() ([]Room, error) {
	var out []Room
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bRooms).ForEach(func(_, v []byte) error {
			var r Room
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			if err := s.decryptRoom(&r); err != nil {
				return err
			}
			out = append(out, r)
			return nil
		})
	})
	return out, err
}

// UpdateRoom updates a room's fields in place.
func (s *Store) UpdateRoom(r Room) error {
	enc, err := s.encryptRoom(&r)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		v, _ := json.Marshal(enc)
		return tx.Bucket(bRooms).Put([]byte(r.ID), v)
	})
}

// DeleteRoom removes a room and all its messages.
func (s *Store) DeleteRoom(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		// Delete all room messages.
		prefix := append([]byte(strings.ToLower(id)), 0)
		c := tx.Bucket(bRoomMsgs).Cursor()
		for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			// Get the message ID from the value to clean up the index.
			v := tx.Bucket(bRoomMsgs).Get(k)
			if v != nil {
				var m RoomMessage
				if json.Unmarshal(v, &m) == nil {
					tx.Bucket(bRoomIdx).Delete([]byte(m.ID)) //nolint:errcheck
				}
			}
			c.Delete() //nolint:errcheck
		}
		// Delete outbox entries for this room.
		oprefix := []byte(strings.ToLower(id) + ":")
		oc := tx.Bucket(bRoomOutbox).Cursor()
		for k, _ := oc.Seek(oprefix); k != nil && bytes.HasPrefix(k, oprefix); k, _ = oc.Next() {
			oc.Delete() //nolint:errcheck
		}
		return tx.Bucket(bRooms).Delete([]byte(id))
	})
}

// AddRoomMessage stores a room message. Returns dup=true if the ID already exists.
func (s *Store) AddRoomMessage(m RoomMessage) (RoomMessage, bool, error) {
	encBody, cerr := s.encryptString(m.Body)
	if cerr != nil {
		return RoomMessage{}, false, fmt.Errorf("store: encrypt body: %w", cerr)
	}
	m.Body = encBody
	var stored RoomMessage
	dup := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bRoomIdx).Get([]byte(m.ID)) != nil {
			dup = true
			return nil
		}
		seq, err := tx.Bucket(bRoomMsgs).NextSequence()
		if err != nil {
			return err
		}
		m.Seq = seq
		k := roomMsgKey(m.Room, seq)
		v, _ := json.Marshal(m)
		if err := tx.Bucket(bRoomMsgs).Put(k, v); err != nil {
			return err
		}
		if err := tx.Bucket(bRoomIdx).Put([]byte(m.ID), k); err != nil {
			return err
		}
		stored = m
		return nil
	})
	if err == nil {
		if derr := s.decryptRoomMessage(&stored); derr != nil {
			return RoomMessage{}, false, derr
		}
	}
	return stored, dup, err
}

// RoomMessages returns up to limit most recent messages in a room, oldest first.
func (s *Store) RoomMessages(roomID string, limit int) ([]RoomMessage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var out []RoomMessage
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bRoomMsgs).Cursor()
		prefix := append([]byte(strings.ToLower(roomID)), 0)
		upper := append(append([]byte(nil), prefix...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
		k, v := c.Seek(upper)
		if k == nil {
			k, v = c.Last()
		} else {
			k, v = c.Prev()
		}
		for ; k != nil && bytes.HasPrefix(k, prefix) && len(out) < limit; k, v = c.Prev() {
			var m RoomMessage
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			if derr := s.decryptRoomMessage(&m); derr != nil {
				return derr
			}
			out = append(out, m)
		}
		return nil
	})
	// Reverse to oldest-first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, err
}

// RoomPending returns undelivered outbound room messages for a given member.
func (s *Store) RoomPending(roomID, member string) ([]RoomMessage, error) {
	var out []RoomMessage
	err := s.db.View(func(tx *bolt.Tx) error {
		prefix := []byte(strings.ToLower(roomID) + ":" + strings.ToLower(member) + ":")
		return tx.Bucket(bRoomOutbox).ForEach(func(k, v []byte) error {
			if !bytes.HasPrefix(k, prefix) {
				return nil
			}
			msgID := string(v)
			idxKey := tx.Bucket(bRoomIdx).Get([]byte(msgID))
			if idxKey == nil {
				return nil
			}
			mv := tx.Bucket(bRoomMsgs).Get(idxKey)
			if mv == nil {
				return nil
			}
			var m RoomMessage
			if err := json.Unmarshal(mv, &m); err != nil {
				return err
			}
			if derr := s.decryptRoomMessage(&m); derr != nil {
				return derr
			}
			out = append(out, m)
			return nil
		})
	})
	sortByRoomSeq(out)
	return out, err
}

// MarkRoomDelivered marks a room message as delivered for a specific member.
// If all members have been marked delivered, the message status becomes "delivered".
func (s *Store) MarkRoomDelivered(roomID, member, msgID string) (RoomMessage, bool, error) {
	var out RoomMessage
	changed := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		k := tx.Bucket(bRoomIdx).Get([]byte(msgID))
		if k == nil {
			return ErrNotFound
		}
		v := tx.Bucket(bRoomMsgs).Get(k)
		if v == nil {
			return ErrNotFound
		}
		var m RoomMessage
		if err := json.Unmarshal(v, &m); err != nil {
			return err
		}
		out = m
		if m.Status == StatusDelivered {
			return nil
		}
		// Remove this member from the outbox.
		tx.Bucket(bRoomOutbox).Delete(roomOutboxKey(roomID, member, msgID)) //nolint:errcheck

		// Check if all members have been delivered by looking at remaining outbox entries.
		r, rerr := getRoom(tx, roomID)
		if rerr != nil {
			return rerr
		}
		allDelivered := true
		for _, mb := range r.Members {
			if mb == m.Sender {
				continue // sender doesn't need to receive their own message
			}
			pfx := []byte(strings.ToLower(roomID) + ":" + strings.ToLower(mb) + ":")
			c := tx.Bucket(bRoomOutbox).Cursor()
			if k, _ := c.Seek(pfx); k != nil && bytes.HasPrefix(k, pfx) {
				allDelivered = false
				break
			}
		}
		if allDelivered {
			m.Status = StatusDelivered
			nv, _ := json.Marshal(m)
			if err := tx.Bucket(bRoomMsgs).Put(k, nv); err != nil {
				return err
			}
			changed = true
		}
		out = m
		return nil
	})
	if err == nil {
		if derr := s.decryptRoomMessage(&out); derr != nil {
			return RoomMessage{}, false, derr
		}
	}
	return out, changed, err
}

// RoomRecordAttempt bumps the attempt counter for a room outbox message.
func (s *Store) RoomRecordAttempt(msgID string, now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		k := tx.Bucket(bRoomIdx).Get([]byte(msgID))
		if k == nil {
			return ErrNotFound
		}
		v := tx.Bucket(bRoomMsgs).Get(k)
		if v == nil {
			return ErrNotFound
		}
		var m RoomMessage
		if err := json.Unmarshal(v, &m); err != nil {
			return err
		}
		m.Attempts++
		m.LastTry = now
		nv, _ := json.Marshal(m)
		return tx.Bucket(bRoomMsgs).Put(k, nv)
	})
}

// NextRoomSenderSeq returns and consumes the next per-sender sequence number
// for a room. The counter is durable, so it keeps climbing across restarts.
//
// It used to be derived by scanning the last 1000 messages for the highest
// value, which silently restarted the sequence once a room passed 1000
// messages and produced colliding numbers.
func (s *Store) NextRoomSenderSeq(roomID, sender string) (uint64, error) {
	var next uint64
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bRoomSeq)
		k := roomSeqKey(roomID, sender)
		if v := b.Get(k); len(v) == 8 {
			next = binary.BigEndian.Uint64(v)
		}
		next++
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], next)
		return b.Put(k, buf[:])
	})
	return next, err
}

func roomSeqKey(roomID, sender string) []byte {
	k := append([]byte(strings.ToLower(roomID)), 0)
	return append(k, []byte(strings.ToLower(sender))...)
}

// AddRoomMemberToOutbox adds a pending delivery entry for a member.
func (s *Store) AddRoomMemberToOutbox(roomID, member, msgID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bRoomOutbox).Put(roomOutboxKey(roomID, member, msgID), []byte(msgID))
	})
}

// GetRoomWithTx returns a room within an existing transaction (for internal use).
func getRoom(tx *bolt.Tx, id string) (*Room, error) {
	v := tx.Bucket(bRooms).Get([]byte(id))
	if v == nil {
		return nil, ErrNotFound
	}
	var r Room
	if err := json.Unmarshal(v, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func sortByRoomSeq(m []RoomMessage) {
	for i := 1; i < len(m); i++ {
		for j := i; j > 0 && m[j].Seq < m[j-1].Seq; j-- {
			m[j], m[j-1] = m[j-1], m[j]
		}
	}
}
