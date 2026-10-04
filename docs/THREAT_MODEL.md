# Threat model

This document says what Chirp protects, against whom, and — just as
importantly — what it does not protect. It is the reasoning behind the
controls listed in [SECURITY.md](../SECURITY.md); the wire-level details are
in [PROTOCOL.md](PROTOCOL.md) and the room-specific analysis in
[ARCHITECTURE.md](ARCHITECTURE.md) ("Rooms: threat model").

## What exists to protect (assets)

1. **The identity key.** A Curve25519 keypair generated on first run. Whoever
   holds it *is* the user: every later connection authenticates against it.
2. **Message content**, one-to-one and in rooms, in transit and at rest.
3. **File content** transferred between peers.
4. **The trust state**: which public key is pinned to which name, and which
   pins the user has verified out of band.
5. **The local database**: pins, history, outbox, settings, received files.
6. **The daemon's loopback HTTP API**, which can read and write all of the
   above through a browser the user happens to have open.

## Trust boundaries

```
 remote peer ──TCP/LAN──▶ node ──▶ session (Noise XX) ──▶ store ──▶ disk
                            ▲
 mDNS records ──▶ discovery │
                            │
 browser tab ──loopback HTTP──▶ api ──▶ app ──▶ node
     (any web page on the machine can *reach* loopback)
```

Everything crossing a boundary from the left is hostile by default:
mDNS TXT records, handshake messages, frame payloads, envelope JSON, file
chunks, and every HTTP request. Nothing is trusted because of where it came
from; authentication is a property the Noise session establishes per link, or
it does not exist.

## Adversaries

### A. A passive network observer

Someone capturing packets on the LAN (open Wi-Fi, mirrored switch port).

* Sees: who is present (mDNS advertisements carry name and fingerprint), when
  connections form, connection sizes and timing.
* Cannot: read message or file content, or alter it undetected. All payload
  traffic is inside Noise XX sessions with ChaCha20-Poly1305; a tampered frame
  fails its AEAD check and drops the link.

### B. An active network attacker

Someone who can inject, drop, redirect and replay traffic.

* Can: try to impersonate a peer during or after a handshake, replay old
  frames, flood connections, block discovery.
* Cannot: complete a handshake as someone else without their key; XX
  authenticates both static keys, and the store pins the first key seen per
  name (TOFU), so a later key change is a loud UI event that blocks sending
  until reviewed.
* Replays: deduplication is by message ID after persistence, so a replayed
  `msg` is stored once and displayed once; `ack` bookkeeping is idempotent.
* Discovery: mDNS records are validated (name length, address sanity,
  fingerprint shape) before they reach the UI, and discovery can always be
  replaced by explicit addresses or an invite code.

### C. A malicious local-network peer

Another Chirp (or Chirp-shaped) client on the network. This is adversary B
running our protocol on purpose.

* Can: pick any display name, including one someone else uses; send garbage
  in every optional field; invite itself to rooms; send oversized or
  pathological input.
* Cannot: make two people with the same name merge — the collision surfaces
  as a key change; forge a message "from" another member — the sender of a
  message is the identity authenticated on the link, never a JSON field;
  force a room join — invitations stay pending until accepted.
* Half-open and stalling connections are capped and timed out (30 s idle,
  bounded concurrent handshakes) so one peer cannot exhaust the daemon.

### D. A hostile web page

Any page the user visits in the same browser. The daemon listens on
`127.0.0.1`, which every web page can attempt to reach.

* Can: send requests to `http://127.0.0.1:7777/...` from the user's browser.
* Cannot: do anything with them, because writes require the custom `X-Chirp`
  header (which a cross-origin form or simple request cannot set), foreign
  origins are refused, the `Host` header must be the loopback host (defeats
  DNS rebinding), and the API serves a strict CSP with `innerHTML` never used
  on peer-controlled text. A page that fails all of these sees errors, not
  data.

### E. Someone with the device, after the fact

Someone who copies the data directory, or borrows the unlocked machine.

* Copy of the database file: message bodies, pinned keys, room names and
  received files are encrypted under a key derived from the identity key.
  With a passphrase set (`EnablePassphrase`), the identity key itself is
  encrypted under an Argon2id key, and the file yields nothing without the
  passphrase.
* Without a passphrase set: the identity key sits next to the data (mode
  `0600`), so a file copy can be read by anyone who can derive that key. At
  rest protection defends a copied file, not a compromised user account.
* A running, unlocked daemon: game over. The API is running with the keys in
  memory; nothing in this document protects against that. Locking a desktop
  is the control here, not Chirp.

### F. Chirp's own developers

Out of the model by construction: there is no server, no telemetry, no
update channel that executes code, and the API never sends data off the
machine. The check this claim against the code is the code; network calls are
limited to mDNS and direct peer TCP.

## What is explicitly not defended

Stated plainly so nobody has to reverse-engineer the guarantees:

* **First contact.** TOFU means whoever was present when a name first
  appeared could be holding it. Verification (fingerprint or six words, in
  person) is the fix, and the UI says so wherever a pin is unverified.
* **Metadata.** Presence, timing, sizes and room membership are observable
  on the LAN. There is no cover traffic and no padding discipline.
* **Ex-members of rooms** keep everything they already received; removal
  stops the flow, it does not reach backwards (see ARCHITECTURE.md).
* **No global room order.** Concurrent senders are ordered by arrival, not by
  clock or by consensus.
* **Compromised endpoint.** Malware running as the user, or a malicious
  browser extension with loopback access through the page's own origin, is
  outside anything an application can defend.
* **Forward secrecy after key compromise.** Noise gives per-session forward
  secrecy; a stolen identity key impersonates future sessions and decrypts
  the local store (unless a passphrase protects it).
* **Availability.** Any LAN adversary can jam discovery or flood the TCP
  port. Chirp fails closed and clearly, it does not fight back.

## Security-relevant invariants worth re-checking after changes

1. No plaintext peer-controlled data is ever decrypted with a key that was
   not authenticated by the Noise session (or the store's at-rest key).
2. Authorization decisions read the *authenticated* identity on the link,
   never an envelope field.
3. A key change on a pinned name blocks sending until reviewed; accepting a
   new key resets verification.
4. Every value written to the store is either plaintext-because-no-key-yet
   or encrypted; the identity private key is never encrypted under a key
   derived from itself.
5. The HTTP API refuses every write that lacks `X-Chirp`, every non-loopback
   `Host`, and every foreign origin.
