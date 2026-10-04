// Package app owns process-level lifecycle: the database, the first-run
// setup flow, and the (optionally not-yet-existing) running node. The HTTP API
// talks to App so it can serve a setup screen before an identity exists.
package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/Mattathiasa/chirp/internal/crypto"
	"github.com/Mattathiasa/chirp/internal/discovery"
	"github.com/Mattathiasa/chirp/internal/identity"
	"github.com/Mattathiasa/chirp/internal/node"
	"github.com/Mattathiasa/chirp/internal/store"
)

// Config configures an App.
type Config struct {
	DataDir    string
	ListenAddr string // TCP listen address for peers, default 0.0.0.0:0
	// Passphrase unlocks a database that was protected with one. Empty means
	// "no passphrase given", which fails for a protected database.
	Passphrase string
	NewDisc    func() discovery.Discovery
	Logf       func(string, ...any)
	Tune       func(*node.Config) // test hook
}

// ErrNeedsSetup is returned by operations that require an identity.
var ErrNeedsSetup = errors.New("app: no identity yet")

// App is the process-level singleton.
type App struct {
	cfg Config
	St  *store.Store

	// openErr records why the node is not running when the database could not
	// be opened fully (a locked database, a corrupt one). The API layer reads
	// it to tell first run and locked apart.
	openErr error

	mu   sync.Mutex
	n    *node.Node
	ctx  context.Context
	stop context.CancelFunc
}

// Open opens the database and, if an identity exists, starts the node.
//
// When the database is passphrase-protected and cfg.Passphrase is empty, a
// shell App is returned with openErr set to store.ErrPassphraseRequired: the
// API can then serve an unlock prompt. Call Unlock (or Open again with the
// passphrase) and Replace the shell.
func Open(cfg Config) (*App, error) {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	a := &App{cfg: cfg}
	ctx, stop := context.WithCancel(context.Background())
	a.ctx, a.stop = ctx, stop

	st, err := store.OpenWithPassphrase(filepath.Join(cfg.DataDir, "chirp.db"), cfg.Passphrase)
	if err != nil {
		a.stop()
		if errors.Is(err, store.ErrPassphraseRequired) {
			a.openErr = err
			return a, nil // shell: locked, waiting for a passphrase
		}
		return nil, err
	}
	a.St = st
	id, err := st.LoadIdentity()
	if err != nil {
		st.Close()
		return nil, err
	}
	if id != nil {
		if st.Key() == nil {
			st.SetKey(crypto.DeriveKeyFromIdentity(id.Key.Private))
		}
		if err := a.startNode(id); err != nil {
			st.Close()
			return nil, err
		}
	}
	return a, nil
}

// OpenErr reports why the app is a locked shell, if it is one.
func (a *App) OpenErr() error { return a.openErr }

// Cfg returns the configuration this app was opened with.
func (a *App) Cfg() Config { return a.cfg }

// Replace swaps the running store and node for a newly unlocked app's.
func (a *App) Replace(next *App) {
	a.mu.Lock()
	oldStop := a.stop
	a.St = next.St
	a.n = next.n
	a.ctx = next.ctx
	a.stop = next.stop
	a.cfg = next.cfg
	a.openErr = nil
	a.mu.Unlock()
	// The old app's node was nil (it was a shell), so there is nothing to
	// stop beyond the placeholder context.
	if oldStop != nil {
		oldStop()
	}
}

func (a *App) startNode(id *identity.Identity) error {
	nc := node.Config{Store: a.St, ID: id, Disc: a.cfg.NewDisc(), ListenAddr: a.cfg.ListenAddr, Logf: a.cfg.Logf}
	if a.cfg.Tune != nil {
		a.cfg.Tune(&nc)
	}
	n := node.New(nc)
	if err := n.Start(a.ctx); err != nil {
		return err
	}
	a.mu.Lock()
	a.n = n
	a.mu.Unlock()
	return nil
}

// Node returns the running node, or nil before setup.
func (a *App) Node() *node.Node {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}

// NeedsPassphrase reports whether this database waits to be unlocked.
func (a *App) NeedsPassphrase() bool { return a.openErr != nil }

// Unlock opens a passphrase-protected database with the given passphrase. It
// is the path a UI takes when Open returned store.ErrPassphraseRequired.
func Unlock(cfg Config, passphrase string) (*App, error) {
	c := cfg
	c.Passphrase = passphrase
	return Open(c)
}

// Setup generates a new identity and starts the node. It fails if one exists.
//
// The key must be set before the identity is saved, or the private key lands
// on disk unencrypted: this was the case once, and it is pinned by a test.
func (a *App) Setup(name string) error {
	if a.Node() != nil {
		return errors.New("app: already set up")
	}
	id, err := identity.Generate(name)
	if err != nil {
		return err
	}
	a.St.SetKey(crypto.DeriveKeyFromIdentity(id.Key.Private))
	if err := a.St.SaveIdentity(id); err != nil {
		return err
	}
	return a.startNode(id)
}

// Restore imports an encrypted backup as the identity and starts the node.
// The key is likewise set before anything is written.
func (a *App) Restore(data []byte, passphrase string) error {
	if a.Node() != nil {
		return errors.New("app: already set up")
	}
	id, err := identity.ImportBackup(data, passphrase)
	if err != nil {
		return err
	}
	a.St.SetKey(crypto.DeriveKeyFromIdentity(id.Key.Private))
	if err := a.St.SaveIdentity(id); err != nil {
		return err
	}
	return a.startNode(id)
}

// Backup exports the identity encrypted under passphrase.
func (a *App) Backup(passphrase string) ([]byte, error) {
	n := a.Node()
	if n == nil {
		return nil, ErrNeedsSetup
	}
	return identity.ExportBackup(n.Identity(), passphrase)
}

// EnablePassphrase turns on passphrase protection for this database. Once it
// returns, opening the data directory without the passphrase fails, and a
// copy of the database file no longer yields the identity or the data.
// Requires a running node, because the mode only makes sense after setup.
func (a *App) EnablePassphrase(passphrase string) error {
	if a.Node() == nil {
		return ErrNeedsSetup
	}
	return a.St.EnablePassphrase(passphrase)
}

// Close stops the node and closes the database. It is safe on a locked shell.
func (a *App) Close() {
	a.stop()
	if n := a.Node(); n != nil {
		n.Stop()
	}
	if a.St != nil {
		a.St.Close()
	}
}
