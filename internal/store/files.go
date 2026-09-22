package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/Mattathiasa/chirp/internal/crypto"
)

// File statuses.
const (
	FileQueued    = "queued"    // outbound, not yet sent
	FileSending   = "sending"   // outbound, in progress
	FileSent      = "sent"      // outbound fully sent
	FileReceiving = "receiving" // inbound, in progress
	FileComplete  = "complete"  // inbound fully received and hash-verified
	FileFailed    = "failed"    // inbound, rejected because the hash did not match
)

// File is one stored or transferring file.
type File struct {
	ID       string    `json:"id"`
	Peer     string    `json:"peer"` // display name of the other party
	Dir      string    `json:"dir"`  // "in" or "out"
	Name     string    `json:"name"` // original filename
	Size     int64     `json:"size"` // total bytes
	Hash     string    `json:"hash"` // SHA-256 hex of the whole file
	Status   string    `json:"status"`
	Received int64     `json:"received,omitempty"` // bytes received (inbound)
	Sent     int64     `json:"sent,omitempty"`     // bytes sent (outbound)
	TS       time.Time `json:"ts"`
}

// ErrFileNotFound is returned when a file ID is unknown.
var ErrFileNotFound = errors.New("store: file not found")

// writeFileData writes data to disk, encrypted if a key is present.
func (s *Store) writeFileData(path string, data []byte) error {
	if s.encKey == nil {
		return os.WriteFile(path, data, 0o600)
	}
	ct, err := crypto.Encrypt(s.encKey, data)
	if err != nil {
		return err
	}
	return os.WriteFile(path, ct, 0o600)
}

// readFileData reads and decrypts a file from disk.
func (s *Store) readFileData(path string) ([]byte, error) {
	if s.encKey == nil {
		return os.ReadFile(path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return crypto.Decrypt(s.encKey, data)
}

func fileDir(filesDir, id string) string {
	return filepath.Join(filesDir, id)
}

// AddOutboundFile stores file data and metadata for an outgoing transfer.
func (s *Store) AddOutboundFile(id, peer, name, hash string, data []byte) error {
	dir := fileDir(s.filesDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("store: create file dir: %w", err)
	}
	if err := s.writeFileData(filepath.Join(dir, "data.enc"), data); err != nil {
		return fmt.Errorf("store: write file data: %w", err)
	}
	f := File{
		ID:     id,
		Peer:   peer,
		Dir:    DirOut,
		Name:   name,
		Size:   int64(len(data)),
		Hash:   hash,
		Status: FileQueued,
		TS:     time.Now(),
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		v, _ := json.Marshal(f)
		if err := tx.Bucket(bFiles).Put([]byte(id), v); err != nil {
			return err
		}
		return tx.Bucket(bFileOutbox).Put([]byte(id), []byte(id))
	})
}

// BeginReceive prepares to receive a transfer and reports how many contiguous
// bytes are already held, so the sender can resume from there.
//
// A partial is only reused when the header matches it exactly: same id, same
// SHA-256 and same size, still in the receiving state, with the bytes still on
// disk. Anything else - a different file under a reused id, a partial left in
// a failed state, a temp file that vanished - starts again from zero, because
// resuming onto the wrong bytes would produce a file that fails its hash at
// the very end after transferring everything twice.
func (s *Store) BeginReceive(id, peer, name, hash string, size int64) (int64, error) {
	dir := fileDir(s.filesDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, fmt.Errorf("store: create receive dir: %w", err)
	}
	tmp := filepath.Join(dir, "data.tmp")

	resume := int64(0)
	if prev, err := s.GetFile(id); err == nil &&
		prev.Status == FileReceiving && prev.Hash == hash && prev.Size == size {
		if st, serr := os.Stat(tmp); serr == nil {
			// The database counter is only advanced after the bytes are on
			// disk, so it can lag a crash but never lead it. Trust the smaller.
			resume = prev.Received
			if st.Size() < resume {
				resume = st.Size()
			}
			if resume < 0 || resume > size {
				resume = 0
			}
		}
	}

	if resume == 0 {
		if err := os.WriteFile(tmp, nil, 0o600); err != nil {
			return 0, fmt.Errorf("store: init temp file: %w", err)
		}
	}

	f := File{
		ID:       id,
		Peer:     peer,
		Dir:      DirIn,
		Name:     name,
		Size:     size,
		Hash:     hash,
		Status:   FileReceiving,
		Received: resume,
		TS:       time.Now(),
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		v, _ := json.Marshal(f)
		return tx.Bucket(bFiles).Put([]byte(id), v)
	})
	return resume, err
}

// WriteChunk writes a received chunk at the given offset and updates progress.
func (s *Store) WriteChunk(id string, offset int64, data []byte) (File, error) {
	dir := fileDir(s.filesDir, id)
	tmp := filepath.Join(dir, "data.tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return File{}, fmt.Errorf("store: open temp file: %w", err)
	}
	_, werr := f.WriteAt(data, offset)
	cerr := f.Close()
	if werr != nil {
		return File{}, fmt.Errorf("store: write chunk: %w", werr)
	}
	if cerr != nil {
		return File{}, cerr
	}
	var file File
	err = s.db.Update(func(tx *bolt.Tx) error {
		v := tx.Bucket(bFiles).Get([]byte(id))
		if v == nil {
			return ErrFileNotFound
		}
		if err := json.Unmarshal(v, &file); err != nil {
			return err
		}
		// Received is a contiguous high-water mark, not a running total. A
		// chunk that lands before it is a retransmit and must not advance it
		// twice; a chunk that starts beyond it would leave a hole, so the
		// file would be short however many bytes arrived afterwards.
		if offset > file.Received {
			return fmt.Errorf("store: chunk at %d leaves a gap after %d", offset, file.Received)
		}
		if end := offset + int64(len(data)); end > file.Received {
			file.Received = end
		}
		if file.Received > file.Size {
			file.Received = file.Size
		}
		nv, _ := json.Marshal(file)
		return tx.Bucket(bFiles).Put([]byte(id), nv)
	})
	return file, err
}

// ErrFileHashMismatch means the received bytes did not hash to what the sender
// advertised. The transfer is marked failed and its data discarded.
var ErrFileHashMismatch = errors.New("store: file hash mismatch")

// CompleteFile verifies the received bytes against the advertised SHA-256 and,
// only if they match, encrypts them and marks the transfer complete.
//
// Verification happens before anything is marked complete: a file that fails
// the hash check must never be readable, and must never be reported to the UI
// as a finished transfer.
func (s *Store) CompleteFile(id string) error {
	var file File
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bFiles).Get([]byte(id))
		if v == nil {
			return ErrFileNotFound
		}
		if err := json.Unmarshal(v, &file); err != nil {
			return err
		}
		if file.Status != FileReceiving {
			return errors.New("store: file not in receiving state")
		}
		if file.Received != file.Size {
			return errors.New("store: file truncated")
		}
		return nil
	})
	if err != nil {
		return err
	}

	dir := fileDir(s.filesDir, id)
	tmp := filepath.Join(dir, "data.tmp")
	data, err := os.ReadFile(tmp)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != file.Hash {
		s.markFileFailed(id)
		_ = os.Remove(tmp)
		return ErrFileHashMismatch
	}
	if err := s.writeFileData(filepath.Join(dir, "data.enc"), data); err != nil {
		return err
	}
	if err := s.setFileStatus(id, FileComplete); err != nil {
		return err
	}
	_ = os.Remove(tmp)
	return nil
}

func (s *Store) markFileFailed(id string) {
	_ = s.setFileStatus(id, FileFailed)
}

func (s *Store) setFileStatus(id, status string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		v := tx.Bucket(bFiles).Get([]byte(id))
		if v == nil {
			return ErrFileNotFound
		}
		var f File
		if err := json.Unmarshal(v, &f); err != nil {
			return err
		}
		f.Status = status
		nv, _ := json.Marshal(f)
		return tx.Bucket(bFiles).Put([]byte(id), nv)
	})
}

// CancelReceive removes an in-progress reception.
func (s *Store) CancelReceive(id string) error {
	var file File
	err := s.db.Update(func(tx *bolt.Tx) error {
		v := tx.Bucket(bFiles).Get([]byte(id))
		if v == nil {
			return ErrFileNotFound
		}
		_ = json.Unmarshal(v, &file)
		if file.Status != FileReceiving {
			return errors.New("store: file not in receiving state")
		}
		return tx.Bucket(bFiles).Delete([]byte(id))
	})
	if err != nil {
		return err
	}
	_ = os.RemoveAll(fileDir(s.filesDir, id))
	return nil
}

// FileOutbox returns all pending outbound files.
func (s *Store) FileOutbox() ([]File, error) {
	var out []File
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bFileOutbox).ForEach(func(k, _ []byte) error {
			v := tx.Bucket(bFiles).Get(k)
			if v == nil {
				return nil
			}
			var f File
			if err := json.Unmarshal(v, &f); err != nil {
				return err
			}
			out = append(out, f)
			return nil
		})
	})
	return out, err
}

// MarkFileSent removes a file from the outbox and marks it sent.
func (s *Store) MarkFileSent(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bFileOutbox).Delete([]byte(id)); err != nil {
			return err
		}
		v := tx.Bucket(bFiles).Get([]byte(id))
		if v == nil {
			return nil
		}
		var f File
		if err := json.Unmarshal(v, &f); err != nil {
			return err
		}
		f.Status = FileSent
		f.Sent = f.Size
		nv, _ := json.Marshal(f)
		return tx.Bucket(bFiles).Put([]byte(id), nv)
	})
}

// DeleteFileOutbox removes an outbound file (user cancelled sending).
func (s *Store) DeleteFileOutbox(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bFileOutbox).Delete([]byte(id)); err != nil {
			return err
		}
		v := tx.Bucket(bFiles).Get([]byte(id))
		if v == nil {
			return nil
		}
		var f File
		if err := json.Unmarshal(v, &f); err != nil {
			return err
		}
		f.Status = FileQueued
		nv, _ := json.Marshal(f)
		return tx.Bucket(bFiles).Put([]byte(id), nv)
	})
}

// GetFile returns metadata for a file by ID.
func (s *Store) GetFile(id string) (*File, error) {
	var f File
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bFiles).Get([]byte(id))
		if v == nil {
			return ErrFileNotFound
		}
		return json.Unmarshal(v, &f)
	})
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// ListFiles returns all stored files, newest first.
func (s *Store) ListFiles() ([]File, error) {
	var out []File
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bFiles).Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var f File
			if err := json.Unmarshal(v, &f); err != nil {
				return err
			}
			out = append(out, f)
		}
		return nil
	})
	return out, err
}

// FileData returns the decrypted data for a completed inbound file.
func (s *Store) FileData(id string) ([]byte, error) {
	f, err := s.GetFile(id)
	if err != nil {
		return nil, err
	}
	if f.Status != FileComplete {
		return nil, errors.New("store: file not complete")
	}
	return s.FileBlob(id)
}

// FileBlob reads and decrypts the raw file data, regardless of status.
func (s *Store) FileBlob(id string) ([]byte, error) {
	return s.readFileData(filepath.Join(fileDir(s.filesDir, id), "data.enc"))
}

// MarkFileSending updates a queued outbound file to sending.
func (s *Store) MarkFileSending(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		v := tx.Bucket(bFiles).Get([]byte(id))
		if v == nil {
			return ErrFileNotFound
		}
		var f File
		if err := json.Unmarshal(v, &f); err != nil {
			return err
		}
		if f.Status == FileQueued {
			f.Status = FileSending
		}
		f.Sent = 0
		nv, _ := json.Marshal(f)
		return tx.Bucket(bFiles).Put([]byte(id), nv)
	})
}

// DeleteFile removes a file's metadata and data.
func (s *Store) DeleteFile(id string) error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bFileOutbox).Delete([]byte(id)); err != nil {
			return err
		}
		return tx.Bucket(bFiles).Delete([]byte(id))
	})
	if err != nil {
		return err
	}
	_ = os.RemoveAll(fileDir(s.filesDir, id))
	return nil
}
