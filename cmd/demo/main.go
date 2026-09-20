// Command demo runs the real Chirp engine and web UI against scripted bot
// peers that live in this process and talk over loopback TCP with real Noise
// sessions. Discovery is in-memory, so it works on networks where multicast
// is blocked (conference Wi-Fi, CI, a laptop with no LAN).
//
// The bots exist to show every state the UI has: a chatty verified-looking
// contact, first contact, a peer that drops offline so messages queue, and a
// peer whose key changes mid-session.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
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
	"github.com/Mattathiasa/chirp/internal/identity"
	"github.com/Mattathiasa/chirp/internal/node"
	"github.com/Mattathiasa/chirp/internal/store"
)

var botNames = map[string]bool{"Alex Rivera": true, "Sam Okafor": true, "Priya N.": true, "Dana Kim": true}

var replies = []string{
	"Sounds good, see you at 3.",
	"Can you also bring the HDMI adapter?",
	"Ha, yes. Working on it now.",
	"Nice. That went through without any server at all.",
	"Got it.",
}

func main() {
	httpAt := flag.String("http", "127.0.0.1:7777", "web UI address (loopback only)")
	name := flag.String("name", "You", "your display name in the demo")
	flag.Parse()
	if host, _, err := net.SplitHostPort(*httpAt); err != nil || !net.ParseIP(host).IsLoopback() {
		log.Fatal("--http must be a loopback ip:port")
	}

	tmp, err := os.MkdirTemp("", "chirp-demo-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	hub := discovery.NewHub()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.Open(app.Config{
		DataDir: filepath.Join(tmp, "you"), ListenAddr: "127.0.0.1:0",
		NewDisc: func() discovery.Discovery { return hub.New() },
	})
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()
	if err := a.Setup(*name); err != nil {
		log.Fatal(err)
	}

	go bot(ctx, hub, filepath.Join(tmp, "alex"), "Alex Rivera", 0, true)
	go bot(ctx, hub, filepath.Join(tmp, "sam"), "Sam Okafor", 6*time.Second, true)
	go flaky(ctx, hub, filepath.Join(tmp, "priya"), "Priya N.")
	go impostor(ctx, hub, filepath.Join(tmp, "dana"), "Dana Kim")

	srv := &http.Server{Addr: *httpAt, Handler: api.New(a).Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	fmt.Printf("Chirp demo: open http://%s\n  Alex and Sam chat back. Priya drops offline every minute (watch the outbox).\n  Dana's key changes after ~45s (watch the trust warning).\n", *httpAt)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func newBot(hub *discovery.Hub, dir, name string, id *identity.Identity) (*node.Node, *store.Store, error) {
	_ = os.MkdirAll(dir, 0o700)
	st, err := store.Open(filepath.Join(dir, "bot.db"))
	if err != nil {
		return nil, nil, err
	}
	if id == nil {
		if id, err = identity.Generate(name); err != nil {
			return nil, nil, err
		}
	}
	n := node.New(node.Config{Store: st, ID: id, Disc: hub.New(), ListenAddr: "127.0.0.1:0"})
	return n, st, nil
}

// autoReply answers every inbound message after a short human-ish delay.
func autoReply(ctx context.Context, n *node.Node) {
	ev, cancel := n.Subscribe()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ev:
			if !ok {
				return
			}
			if e.Type == "message" && e.Message != nil && e.Message.Dir == "in" {
				peer := e.Peer
				go func() {
					time.Sleep(time.Duration(800+rand.Intn(1500)) * time.Millisecond)
					_, _ = n.Send(peer, replies[rand.Intn(len(replies))])
				}()
			}
		}
	}
}

func bot(ctx context.Context, hub *discovery.Hub, dir, name string, delay time.Duration, greet bool) {
	time.Sleep(delay)
	n, st, err := newBot(hub, dir, name, nil)
	if err != nil {
		log.Print(err)
		return
	}
	defer st.Close()
	if err := n.Start(ctx); err != nil {
		log.Print(err)
		return
	}
	defer n.Stop()
	if greet {
		go func() {
			for i := 0; i < 60; i++ { // greet the first person who connects
				time.Sleep(500 * time.Millisecond)
				ps, _ := n.Peers()
				for _, p := range ps {
					if p.Online && !botNames[p.Name] {
						_, _ = n.Send(p.Name, "Hey, are you on the same Wi-Fi? Just testing Chirp.")
						return
					}
				}
			}
		}()
	}
	autoReply(ctx, n)
}

func flaky(ctx context.Context, hub *discovery.Hub, dir, name string) {
	_ = os.MkdirAll(dir, 0o700)
	st, err := store.Open(filepath.Join(dir, "bot.db"))
	if err != nil {
		log.Print(err)
		return
	}
	defer st.Close()
	id, _ := identity.Generate(name)
	for ctx.Err() == nil {
		n := node.New(node.Config{Store: st, ID: id, Disc: hub.New(), ListenAddr: "127.0.0.1:0"})
		if err := n.Start(ctx); err != nil {
			log.Print(err)
			return
		}
		rctx, cancel := context.WithCancel(ctx)
		go autoReply(rctx, n)
		select {
		case <-ctx.Done():
		case <-time.After(25 * time.Second):
		}
		cancel()
		n.Stop()
		select { // offline window
		case <-ctx.Done():
		case <-time.After(35 * time.Second):
		}
	}
}

func impostor(ctx context.Context, hub *discovery.Hub, dir, name string) {
	n, st, err := newBot(hub, dir, name, nil)
	if err != nil {
		log.Print(err)
		return
	}
	if err := n.Start(ctx); err != nil {
		log.Print(err)
		return
	}
	select {
	case <-ctx.Done():
		n.Stop()
		st.Close()
		return
	case <-time.After(45 * time.Second):
	}
	n.Stop()
	st.Close()
	// Same name, brand new key, fresh database: exactly what a reinstall (or an attacker) looks like.
	n2, st2, err := newBot(hub, dir+"-2", name, nil)
	if err != nil {
		return
	}
	defer st2.Close()
	if err := n2.Start(ctx); err != nil {
		return
	}
	defer n2.Stop()
	<-ctx.Done()
}
