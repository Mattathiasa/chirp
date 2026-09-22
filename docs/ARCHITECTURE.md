# Architecture

```
                     browser  ──  http://127.0.0.1:7777  (loopback only)
                        │  REST + Server-Sent Events
              ┌─────────▼──────────┐
              │ internal/api       │  Host allow-list, X-Chirp header, CSP,
              │ + internal/web     │  embedded UI (go:embed)
              └─────────┬──────────┘
              ┌─────────▼──────────┐
              │ internal/app       │  first-run setup, restore, backup, lifecycle
              └─────────┬──────────┘
              ┌─────────▼──────────┐        ┌───────────────────┐
              │ internal/node      │◄──────►│ internal/store    │  bbolt: identity,
              │ dial / accept /    │        │ pins, messages,   │  pins, messages,
              │ outbox / events    │        │ outbox, settings  │  outbox
              └───┬────────────┬───┘        └───────────────────┘
      ┌───────────▼───┐   ┌────▼──────────────┐
      │ discovery     │   │ session           │  Noise XX over net.Conn
      │  mDNS │ memory│   │  └─ proto (frames)│  length-prefixed JSON
      └───────────────┘   └───────────────────┘
                internal/identity: keys, fingerprint, words, encrypted backup
```

## Package map

| Package | Responsibility | Depends on |
| --- | --- | --- |
| `identity` | X25519 keypair, SHA-256 fingerprint, six-word string, identicon seed, scrypt + ChaCha20-Poly1305 key backup | noise |
| `proto` | frame codec, envelope validation. Pure functions, fuzzed | none |
| `session` | Noise XX handshake, encrypted `Send`/`Recv`. No trust decisions | identity, proto |
| `store` | persistence and the invariants that need transactions: TOFU `Observe`, dedup, outbox, retention | identity |
| `discovery` | `Discovery` interface, mDNS implementation, in-memory hub for tests | none |
| `node` | the engine: who dials, session lifecycle, outbox flushing, events, views | all of the above |
| `app` | process lifecycle around an optional node (so the UI can run first-run setup) | node, store |
| `api` | HTTP + SSE, security middleware | app, node |
| `web` | embedded static UI | none |

`cmd/chirpd` is the daemon. `cmd/demo` runs the same engine against scripted in-process peers.

## Decisions worth defending

**One dialer per pair (lower fingerprint dials).** The alternative is both sides dial and you reconcile duplicates, which needs a tie-break protocol and still races. Cost: if the designated dialer cannot reach the other (asymmetric firewall), nothing connects even though the reverse direction might work.

**TCP first, UDP later.** The original brief asked for UDP. On a LAN, TCP already gives ordering, retransmission and congestion control, and Noise needs an ordered stream to keep nonce counters in sync. Reliable UDP would mean re-implementing exactly that, and the interesting part of this project is the trust and delivery model, not a retransmit window. `session.Conn` wraps a `net.Conn`, so a datagram transport is a contained future change.

**Discovery behind an interface.** The engine never imports zeroconf. Tests and `cmd/demo` use an in-memory hub, so the whole node runs, with real TCP and real Noise, in a test with no multicast. The real mDNS path has one opt-in integration test (`CHIRP_MDNS_TEST=1`).

**The store owns the invariants.** `Observe` (pin or flag) and `AddMessage` (dedup) are single bbolt transactions, so two sessions racing cannot pin two keys for one name or store a message twice.

**Acks mean "persisted".** The receiver stores, then acks. A crash between the two causes a retransmit, which dedup absorbs. The opposite order would lose messages.

**Events are lossy on purpose.** Subscribers get a buffered channel and lose events if they fall behind, rather than stalling the engine. The UI refetches state when the SSE stream reconnects and polls every 10 s as a backstop.

**The API is a browser attack surface.** The daemon has no login because it binds to loopback, but any web page can still send requests to `127.0.0.1`. Defences: Host allow-list (DNS rebinding), mandatory `X-Chirp` header on mutating requests (cross-origin pages cannot set it without a preflight, and no preflight is ever answered), Origin check, 64 KiB body cap, strict CSP (no inline script or style attributes; the UI uses CSSOM), `Cache-Control: no-store`. Peer-controlled text is only ever inserted with `textContent`.

## Concurrency model

* One goroutine per session reads; writes go through a mutex in `session.Conn`.
* Per link, `flushMu` serialises outbox flushes so a message is not sent twice concurrently.
* `Node.mu` guards the `nearby`, `live`, subscriber and log maps. It is never held across I/O.
* All goroutines start through `Node.goRun`, so `Stop()` waits for every one of them. Tests run under `-race`.

## Known limits

* No mobile client yet (Flutter, next phase).
* Display-name renaming is not supported: the name is the pinning handle.
* Messages are stored unencrypted at rest.
* Pruning removes dedup entries with the message; a very late retransmit of a pruned message would reappear. Retention windows are far longer than any retry window.
* Real Wi-Fi behaviour (client isolation, multicast filtering, firewalls) cannot be simulated. The Network screen and README explain the usual causes.

## Rooms: threat model

Rooms use **fan-out of one pairwise-encrypted copy per member**. There is no shared group key. Each message is encrypted separately to each member's Noise session, so forward secrecy properties come from the pairwise sessions. The cost is O(n) bandwidth per message, which is acceptable for LAN sizes (capped at 32 members).

### What an ex-member retains

When a member is removed from a room (or leaves), they retain all messages they received while they were a member. There is no cryptographic erasure: the messages are encrypted to their session key and they can decrypt them forever. This is inherent to the fan-out design and is documented in the UI.

### What a malicious creator can do

The creator is the only admin in v1. A malicious creator can:
- Add any pinned peer to the room without their consent (they receive a "create" event).
- Remove any member at any time.
- Rename the room at any time.
- Transfer ownership by leaving (ownership goes to the first remaining member in sorted order).

A malicious creator **cannot**:
- Read messages from members who have not joined (no shared key).
- Forge messages as another member (each message is signed by the sender's Noise session).
- Decrypt messages sent to other members (pairwise encryption).

### Why unverified members show a warning

Unverified members (pinned but not verified out-of-band) could be impersonated by an active attacker who has compromised the TOFU pin. The UI shows a warning badge on rooms containing unverified members so the user can make an informed trust decision before sharing sensitive content.
