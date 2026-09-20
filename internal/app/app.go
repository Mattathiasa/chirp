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

	"github.com/mattathias/chirp/internal/discovery"
	"github.com/mattathias/chirp/internal/identity"
	"github.com/mattathias/chirp/internal/node"
	"github.com/mattathias/chirp/internal/store"
)

// Config configures an App.
type Config struct {
	DataDir    string
	ListenAddr string // TCP listen address for peers, default 0.0.0.0:0
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

	mu   sync.Mutex
	n    *node.Node
	ctx  context.Context
	stop context.CancelFunc
}

// Open opens the database and, if an identity exists, starts the node.
func Open(cfg Config) (*App, error) {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "chirp.db"))
	if err != nil {
		return nil, err
	}
	ctx, stop := context.WithCancel(context.Background())
	a := &App{cfg: cfg, St: st, ctx: ctx, stop: stop}
	id, err := st.LoadIdentity()
	if err != nil {
		st.Close()
		return nil, err
	}
	if id != nil {
		if err := a.startNode(id); err != nil {
			st.Close()
			return nil, err
		}
	}
	return a, nil
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

// Setup generates a new identity and starts the node. It fails if one exists.
func (a *App) Setup(name string) error {
	if a.Node() != nil {
		return errors.New("app: already set up")
	}
	id, err := identity.Generate(name)
	if err != nil {
		return err
	}
	if err := a.St.SaveIdentity(id); err != nil {
		return err
	}
	return a.startNode(id)
}

// Restore imports an encrypted backup as the identity and starts the node.
func (a *App) Restore(data []byte, passphrase string) error {
	if a.Node() != nil {
		return errors.New("app: already set up")
	}
	id, err := identity.ImportBackup(data, passphrase)
	if err != nil {
		return err
	}
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

// Close stops the node and closes the database.
func (a *App) Close() {
	a.stop()
	if n := a.Node(); n != nil {
		n.Stop()
	}
	a.St.Close()
}
