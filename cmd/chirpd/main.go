// Command chirpd is the Chirp daemon: it discovers peers on the local network
// with mDNS, talks to them over Noise-encrypted TCP, and serves a web UI on
// loopback.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Mattathiasa/chirp/internal/api"
	"github.com/Mattathiasa/chirp/internal/app"
	"github.com/Mattathiasa/chirp/internal/discovery"
)

// version is set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Fatalf("chirpd: %v", err)
	}
}

func run() error {
	def, err := os.UserConfigDir()
	if err != nil {
		def = "."
	}
	var (
		dataDir = flag.String("data", filepath.Join(def, "chirp"), "directory for the database (holds your private key; keep it private)")
		httpAt  = flag.String("http", "127.0.0.1:7777", "web UI address; must be a loopback address")
		listen  = flag.String("listen", "0.0.0.0:0", "TCP address for peer connections")
		name    = flag.String("name", "", "display name; creates an identity non-interactively on first run")
		showVer = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("chirpd", version)
		return nil
	}
	if err := requireLoopback(*httpAt); err != nil {
		return err
	}

	a, err := app.Open(app.Config{
		DataDir:    *dataDir,
		ListenAddr: *listen,
		NewDisc:    func() discovery.Discovery { return &discovery.MDNS{} },
		Logf:       log.Printf,
	})
	if err != nil {
		return err
	}
	defer a.Close()

	if a.Node() == nil && *name != "" {
		if err := a.Setup(*name); err != nil {
			return err
		}
	}

	srv := &http.Server{
		Addr:              *httpAt,
		Handler:           api.New(a).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// No WriteTimeout: the SSE stream is long-lived.
	}
	ln, err := net.Listen("tcp", *httpAt)
	if err != nil {
		return err
	}
	log.Printf("chirpd %s: open http://%s in your browser", version, ln.Addr())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	log.Print("shutting down")
	srv.Close() // SSE streams never go idle, so a graceful Shutdown would hang
	return nil
}

func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bad --http address: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("--http must be a loopback address (got %q): the UI has no login, so it must not be reachable from the network", host)
	}
	return nil
}
