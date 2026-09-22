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
* Message bodies, pinned peers, room names and file contents are encrypted at rest with a key derived from the identity key via HKDF. That key lives in the same directory, so this defends a copied database or an unencrypted backup, not a reader of the user's own account. A passphrase mode would fix that and is not implemented.
* File transfer is not resumable. An interrupted transfer restarts from zero; the receiver truncates any partial and verifies the whole file against the sender's SHA-256 before it can be read.
* Pruning removes dedup entries with the message; a very late retransmit of a pruned message would reappear. Retention windows are far longer than any retry window.
* Real Wi-Fi behaviour (client isolation, multicast filtering, firewalls) cannot be simulated. The Network screen and README explain the usual causes.

## Rooms: threat model

Rooms use **fan-out of one pairwise-encrypted copy per member**. There is no shared group key. Each message is encrypted separately to each member's Noise session, so forward secrecy properties come from the pairwise sessions. The cost is O(n) bandwidth per message, which is acceptable for LAN sizes (capped at 32 members).

### What authorises a membership change

Membership events are **not signed**. They arrive over an authenticated Noise session, so the sender's identity is the pinned name on the link, and that identity — never the `roomActor` field, which is attacker-controlled — is what the receiver authorises against. This is strictly weaker than signed events in one respect: it is hop-by-hop, so a member learns of a change only from a peer entitled to make it, and there is nothing to relay through a third party. Given creator-only administration and a direct session to every member, there is nothing a signature would add here, and a signing key would be one more thing to get wrong.

Concretely: only the creator may add, remove or rename; any member may announce their own departure; and a `create` event for a room that already exists is accepted only from the peer that already owns it. Without that last rule any member could re-`create` a room, name themselves its creator on every other device, and thereby acquire the right to rename it and remove everyone else.

`roommsg`, `roomack` and reactions are accepted only for a room we hold and only from a current member, so a removed member is rejected on receipt rather than merely dropped from the send list.

### What an ex-member retains

A member who is removed, or who leaves, keeps every message they received while they were in the room. There is no cryptographic erasure: those messages were encrypted to their own session and they can decrypt them forever. This is inherent to fan-out and no group-key scheme without a rekey-on-removal step would do better. Removal stops the flow; it does not reach backwards.

### What a malicious creator can do

The creator is the only admin. A malicious creator can:
- Invite any pinned peer. They **cannot** put someone in a room: an invitation is pending until the invitee accepts it, and a pending room stores nothing, sends nothing and cannot be opened.
- Remove any member at any time, and rename the room at any time.
- Transfer ownership by leaving; it goes to the first remaining member in sorted order.
- See everything any member sends to the room, which is true of every member.

A malicious creator **cannot**:
- Make anyone join. Declining deletes the room locally and tells the room you are out.
- Forge a message as another member: the sender of a `roommsg` is the authenticated identity on the link, not a field in the envelope.
- Read what members send to each other outside the room, or decrypt another member's copy of a room message.

### Ordering

Each sender stamps its own durable per-room counter on `seq`, so within one sender's stream the order is gapless and survives restarts. **There is no global order across senders.** Two members sending at the same instant have no defined relative order, and each receiver falls back to its own arrival time for display. Device clocks are not synchronised, so the sender's timestamp cannot be used for ordering either. This is a deliberate limit: a total order would need either a coordinator, which there is none of, or vector clocks, which are not worth their cost at LAN sizes.

### Why unverified members are called out

An unverified member is pinned but never checked out of band, so an active attacker who was present at first contact could be holding that name. Everyone in a room sees everything sent to it, so the invitation card names exactly which members are unverified before you accept, and the members panel keeps flagging them afterwards.
