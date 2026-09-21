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
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/flynn/noise"

	"github.com/Mattathiasa/chirp/internal/crypto"
	"github.com/Mattathiasa/chirp/internal/identity"
)

var (
	bMeta   = []byte("meta")
	bPeers  = []byte("peers")
	bMsgs   = []byte("msgs")   // key: peerKey 0x00 seq(8)
	bIdx    = []byte("msgidx") // key: message id -> msgs key
	bOutbox = []byte("outbox") // key: message id -> msgs key (pending outbound)
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
	db     *bolt.DB
	encKey []byte // nil = plaintext
}

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
		for _, b := range [][]byte{bMeta, bPeers, bMsgs, bIdx, bOutbox} {
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
	return &Store{db: db, encKey: encKey}, nil
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
