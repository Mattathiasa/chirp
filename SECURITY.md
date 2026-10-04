# Security policy

Chirp handles identity keys, end-to-end encrypted traffic and an on-disk
database, so reports are taken seriously even though this is a hobby-grade,
unaudited project. This file explains how to report a problem and what to
expect.

## Supported versions

There are no releases yet. The supported configuration is the `main` branch
built from source with a current Go toolchain, exactly as `make build` builds
it. If you are running anything else (an older checkout, a patched build),
reproduce the issue on `main` first.

## Reporting a vulnerability

Please do **not** open a public issue for anything that could expose a user.

Use GitHub's private vulnerability reporting on this repository, or email
**mattathiasabraham@gmail.com** with `[chirp security]` in the subject. If you
use email, encrypt to this key if you can; plain email is fine for anything
that only affects a local network you control.

What helps most:

* What you did, what you expected, what happened.
* The exact build (`git rev-parse HEAD`) and OS.
* A minimal reproduction: a script, a crafted frame, a packet capture.
* Your assessment of impact, and any fix or mitigation you would suggest.

If the report involves the wire protocol, include the raw bytes or a
`docs/testvectors.json`-style vector so the failure can be replayed against
the fuzz corpus.

## What to expect

* Acknowledgement within about a week.
* A fix on `main`, usually within days for anything that exposes another
  user's key or messages, slower for hardening.
* Public disclosure once a fix is available: you are credited in the commit
  message unless you ask not to be. There is no bug bounty and no advisory
  infrastructure beyond this repository.

## Scope

**In scope**

* The Noise XX handshake and session layer (`internal/session`, `internal/proto`),
  including frame parsing, envelope validation and replay handling.
* The node engine: peer identity pinning (TOFU), key-change handling, room
  fan-out, file transfer integrity (`internal/node`).
* The on-disk store: encryption at rest, the passphrase lock, backup/restore
  (`internal/store`, `internal/app`).
* The loopback HTTP API and embedded web UI: DNS-rebinding, CSRF, CSP, and
  any way a remote web page can reach the daemon (`internal/api`, `internal/web`).
* The mDNS discovery layer's parsing of untrusted records (`internal/discovery`).

**Out of scope**

* A compromised host: anyone who can run code as your user, or read your user
  account's files, owns the identity and the unlocked database. Encryption at
  rest defends a copied file, not a compromised account.
* Traffic analysis, metadata inference, and the fact that local-network peers
  can see who is present. These are inherent to serverless LAN messaging and
  documented in the threat model.
* The demo and soak tooling (`cmd/demo`, `cmd/soak`), which exist for testing.
* Vulnerabilities in dependencies: report them upstream, but you are welcome
  to file an issue so the dependency can be bumped.

## Hardening that already exists

For orientation when writing a report; details live in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) and
[docs/THREAT_MODEL.md](docs/THREAT_MODEL.md):

* Every peer connection is a Noise `XX_25519_ChaChaPoly_SHA256` session with
  both keys authenticated; frames are strictly length-checked and envelopes
  strictly validated, with both decoders under continuous fuzzing.
* Identity keys are pinned on first contact; a key change blocks sending
  until the user reviews it.
* The HTTP API binds to loopback, checks the `Host` header, requires a custom
  header on every write, rejects foreign origins, and serves a strict CSP.
* The database is encrypted with a key derived from the identity key; the
  identity itself can additionally be encrypted under an Argon2id-derived
  passphrase (`EnablePassphrase`), after which the file is unreadable without
  it.

## Known limitations

These are design limitations, not vulnerabilities, and are spelled out in the
README and the threat model:

* First contact is trust-on-first-use: someone present when a name is first
  seen can impersonate it until you verify in person.
* Without a passphrase, the identity private key sits in the data directory
  (mode `0600`) next to the data it protects.
* The software has had no outside security review.
