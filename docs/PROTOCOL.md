# Chirp wire protocol, version 2

Everything below is implemented in `internal/proto` and `internal/session`.

## Discovery (mDNS / DNS-SD)

Service type `_chirp._tcp` on `local.`. One instance per device.

| Field | Value |
| --- | --- |
| Instance name | first 16 hex chars of the key fingerprint (unique per key, so two people called "Alex" never collide) |
| Port | the TCP port the daemon listens on |
| TXT `v` | `1` |
| TXT `name` | display name |
| TXT `fp` | 64 hex chars, SHA-256 of the static public key |

TXT data is **advisory and unauthenticated**. It only decides who to dial. Identity is proven by the handshake, never by what an mDNS packet claims. Entries with a bad version, missing name, wrong-length fingerprint or out-of-range port are dropped (`fromEntry`, unit tested). IPv6 link-local addresses are skipped because they need an interface zone.

## Who dials

For every pair, exactly one side dials: the device whose fingerprint (as a lowercase hex string) sorts **lower** connects to the higher one. Both sides browse, so both learn of each other and only one opens a socket. This removes duplicate-connection races without a tie-break protocol. If two sessions for the same peer do exist (for example after a restart), the newest replaces the older.

## Transport

TCP. Every message on the wire, handshake and data alike, is one frame:

```
+---------------+----------------------+
| length (u16)  | payload (1..65535 B) |
+---------------+----------------------+
```

Big-endian length. Zero-length frames are rejected. 65535 is the Noise message limit, so a frame never has to be split.

## Handshake: Noise_XX_25519_ChaChaPoly_SHA256

Prologue: the ASCII string `chirp/2`. A peer speaking another version fails the handshake (the transcript hashes differ) instead of misparsing frames. Neither side needs to know the other's key in advance, and both static keys are encrypted from a passive observer.

```
-> e
<- e, ee, s, es          payload: Hello
-> s, se                 payload: Hello
```

`Hello` is JSON: `{"v":2,"name":"Alex","caps":["files","reactions","receipts","typing"]}`. It is sent inside the encrypted handshake payload, so the display name is authenticated by the same transcript as the key. The handshake has a 5 second deadline. The responder caps concurrent unauthenticated handshakes at 32 and closes the excess.

Version negotiation: `negotiated = min(local, remote)`. A v2 peer connecting to a v1 peer downgrades to text-only; the v1 peer simply ignores the `caps` field. This ensures backward compatibility.

After the handshake each direction has its own `CipherState`. Noise nonces are implicit counters, so **any decrypt failure ends the session**: a dropped or reordered frame cannot be recovered and must not be papered over.

## Application frames

Each transport frame carries one ChaCha20-Poly1305 ciphertext of one JSON envelope (`internal/proto.Envelope`):

| `t` | Fields | Meaning |
| --- | --- | --- |
| `msg` | `id` (32 hex), `ts` (sender millis), `body` (1..4096 bytes, valid UTF-8), `replyTo` (optional, message ID) | a chat message, optionally replying to another |
| `ack` | `id` | the receiver has **persisted** message `id` |
| `ping` | none | keepalive, sent every 10 s |
| `react` | `target` (32 hex, message ID), `emoji` (1..32 runes) | emoji reaction to message `target` |
| `del` | `target` (32 hex, message ID) | delete-for-me (local only) |
| `delall` | `target` (32 hex, message ID) | delete-for-everyone (best effort, peer deletes locally) |
| `typing` | none | ephemeral typing indicator, not persisted |
| `read` | `target` (32 hex, message ID) | read receipt for message `target` |
| `file` | `id` (32 hex), `src` (filename), `size` (bytes, ≤100 MB), `hash` (64 hex SHA-256) | file transfer metadata |
| `chunk` | `id` (32 hex), `offset` (byte offset), `chunk` (≤32 KB, base64 in JSON) | file data chunk |
| `fileack` | `id` (32 hex), `offset` (contiguous bytes already held) | receiver: resume the transfer from `offset` |
| `roommsg` | `id` (32 hex), `room` (32 hex), `ts`, `body` (1..4096 bytes), `seq` (optional, sender's per-room counter), `replyTo` (optional) | a message to a room |
| `roomack` | `id` (32 hex), `room` (32 hex) | the receiver has **persisted** room message `id` |
| `roomevent` | `room` (32 hex), `roomEvent` (`create`/`join`/`leave`/`remove`/`rename`), `roomActor`, `roomName` (rename/create), `roomMembers` (create) | a membership change |

### Resuming a transfer

A transfer that is cut off partway through picks up where it stopped:

1. The sender sends `file` with the id, name, size and SHA-256.
2. The receiver looks for a partial under that id. It reuses one only when the
   id, hash and size all match and the bytes are still on disk; anything else
   starts from zero, because resuming onto the wrong bytes produces a file that
   fails its hash only after everything has been transferred twice.
3. The receiver replies `fileack` with the number of contiguous bytes it holds.
4. The sender sends `chunk`s from that offset.

`fileack` is gated on the `resume` capability in both directions. Neither side
sends one unless the other advertised it, because an unknown envelope type is
fatal to the session rather than ignored. Against a peer without the
capability the sender skips the round trip and transmits from the beginning.

The receiver's byte count is a **contiguous high-water mark**, not a running
total. A chunk that arrives below it is a retransmit and does not advance it
twice; a chunk that starts above it would leave a hole and is refused, so a
short file can never be assembled and then presented as complete. The counter
is written only after the bytes reach disk, so it can lag a crash but never
lead one, and the smaller of it and the file's real length is what resumes.

A sender that advertised resume and gets no `fileack` within ten seconds leaves
the transfer in its outbox for the retry loop rather than pushing the whole
file at a peer that may be wedged.

### Why chunks are 32 KB

A chunk is carried as base64 inside the JSON envelope, which costs four bytes
for every three, and the envelope is then Noise-encrypted (+16 bytes for the
tag) and written into a frame whose length prefix is a `uint16`. The budget is
therefore 65535 bytes for the encrypted envelope, not for the chunk. 32 KB of
payload encodes to about 43 KB, which fits; 64 KB encodes to about 87 KB, which
does not. `TestMaxChunkEnvelopeFitsInAFrame` pins this.

### Rooms

A room is **fan-out of one pairwise-encrypted copy per member**. There is no
shared group key: the sender emits one `roommsg` per member over that member's
own Noise session. Cost is O(n) per message, and membership is capped at 32.

**What authorises a membership event.** `roomevent` arrives over an
authenticated session, so the sender's identity is the pinned name on the link.
That identity — never the `roomActor` field, which is attacker-controlled — is
what the receiver authorises against:

| Event | Accepted from |
| --- | --- |
| `create` | any peer, for a room we do not have and that lists us as a member; for a room we already have, only from its current creator, and only to refresh name and membership |
| `join`, `remove`, `rename` | the room's creator only |
| `leave` | the leaving peer, about themselves only |

`roommsg` and `roomack` are accepted only for a room we hold, from a peer who
is a current member of it. A removed member is therefore rejected on receipt,
not merely dropped from the send list.

**Ordering.** Each sender keeps a durable per-room counter and stamps it on
`seq`. Within one sender's stream, `seq` is a gapless total order that survives
restarts. **There is no global order across senders**: two members sending at
the same moment produce no defined relative order, and each receiver falls back
to its own arrival time for display. Clock skew between devices means sender
`ts` cannot be used for ordering either. `seq` is optional on the wire, and a
missing one simply means the sender's ordering is unknown.

### Capabilities

The Hello `caps` field advertises supported features. v2 peers always advertise all caps; v1 peers omit the field. Recipients check `conn.RemoteCaps` before using v2-only features.

| Cap | Feature |
| --- | --- |
| `files` | File transfer (chunked, SHA-256 verified end to end) |
| `reactions` | Emoji reactions |
| `receipts` | Read receipts |
| `typing` | Typing indicators |
| `rooms` | Group chat (`roommsg`, `roomack`, `roomevent`) |
| `resume` | Resumable file transfer (`fileack`) |

### Backward compatibility

A v2 peer connecting to a v1 peer negotiates version 1.

Unknown envelope types are **not** silently dropped. `Decode` sets
`DisallowUnknownFields` and rejects an unrecognised `t`, and any decode error is
fatal for the session because the Noise nonce is an implicit counter that cannot
resynchronise. Sending an envelope type a peer does not know therefore tears the
session down.

So a newer feature must be gated on the peer advertising its capability, not
merely on the version number, and adding a field to an existing envelope type is
equally breaking. Room traffic checks for the `rooms` capability before anything
room-shaped goes out; the `seq` field rides along inside that capability.

If nothing is read for 30 s the session is declared dead. TCP alone can take minutes to notice a device that vanished from Wi-Fi.

## Test vectors

`docs/testvectors.json` carries an encoded example of every envelope type, a
set of inputs that must be rejected, and the frame constants. Every `json`
string in it is produced by the Go encoder and checked against it by
`TestProtocolTestVectorsMatchTheEncoder`, so the file cannot drift from the
implementation: a second implementation can be compared to it byte for byte.

Regenerate after a deliberate wire change:

```sh
UPDATE_VECTORS=1 go test ./internal/proto
```

## Delivery semantics

**At-least-once on the wire, exactly-once on screen.**

1. The sender writes the message to its outbox (bbolt) *before* trying the network, status `queued`.
2. On every new session, and on a backoff timer while a session is up, it (re)sends everything unacked, oldest first.
3. The receiver stores the message first, then sends `ack`. If the store fails, it does **not** ack and drops the session.
4. The sender marks the message `delivered` only on `ack`, and only if the ack comes from the peer the message was addressed to.
5. The receiver deduplicates by `id` (a separate index), so a retransmit is stored once. Duplicates are still acked, because the sender may have missed the first ack.

Ordering: one TCP stream per peer, flushed oldest first under a per-link lock, so messages arrive in send order. This is tested with 50 messages.

## Trust: TOFU keyed by name

The pinning handle is the case-insensitive display name.

| Situation | Result |
| --- | --- |
| unseen name | pin the key, state `new` (unverified) |
| known name, same key | refresh `lastSeen` |
| known name, **different key** | keep the old pin, record the new key as pending, **close the session**, block sending, state `changed` |
| user accepts pending key | re-pin to the new key and reset `verified` to false |

Users can mark a pin `verified` after comparing the 64-hex fingerprint (shown as 16 groups of 4) or the six-word string out of band.

### What this does not protect against

* First contact is trust-on-first-use. An attacker present at first contact wins.
* Names are the handle. Two different people who both call themselves "Dana" collide, and the second one shows up as a key change. That is the safe failure, but it is confusing.
* The six words are 48 bits derived from one public key. They stop casual mistakes, not an attacker who can grind keys. Compare the full fingerprint for anything that matters.
* No forward secrecy for stored messages: they sit in a plaintext bbolt file, protected only by OS file permissions (`0600`).

## Encryption at rest

Message bodies, peer keys, and the identity private key are optionally encrypted on disk using ChaCha20-Poly1305 AEAD.

### Envelope format

```
+------------+---------+-------+------------+
| CHIRPENC   | version | nonce | ciphertext |
| (8 bytes)  | (1)     | (12)  | (variable) |
+------------+---------+-------+------------+
```

The ciphertext includes a 16-byte Poly1305 authentication tag.

### Key derivation

- **Identity-derived**: HKDF-SHA256 from the identity private key with context `chirp/at-rest/v1`. Always available.
- **Passphrase-derived**: Argon2id (t=3, m=64MiB, p=4) from a user passphrase with random 16-byte salt.
- **Combined**: When both are present, the two 32-byte keys are XORed.

### Storage

Encrypted values are base64-encoded before JSON marshaling to preserve binary round-trip through bbolt and JSON.

### Migration

Existing unencrypted databases continue to work: `IsEncrypted()` checks the magic prefix. To encrypt an existing database, re-open it with a key and the store will encrypt on the next write.

## Add-by-address (manual connection)

When mDNS fails (wrong network, VPN, firewall), users can connect manually.

### Invite URI format

```
chirp://add/<host>:<port>/<fingerprint-prefix>
```

- `<host>`: IPv4 address, IPv6 address in brackets, or hostname
- `<port>`: TCP port number
- `<fingerprint-prefix>`: at least 8 lowercase hex characters of the SHA-256 fingerprint

Examples:
- `chirp://add/192.168.1.5:9001/aabbccdd`
- `chirp://add/[fd00::1]:9001/aabbccddeeff0011`

### API

- `POST /api/dial` with `{"host":"...","port":"..."}`: connects to a peer at the given address
- `POST /api/invite/parse` with `{"uri":"..."}`: validates and parses an invite URI
