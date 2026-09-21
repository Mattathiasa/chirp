# Testing Chirp

## Automated tests

```bash
make race          # unit + integration tests with -race
make fuzz          # 30-second fuzz of the wire-format decoder
make vet fmt       # vet + gofmt check
make vuln          # govulncheck
```

CI runs all of the above plus a cross-platform build matrix.

## Two-process loopback test

Two chirpd instances on the same machine, different data dirs and HTTP ports, over loopback.

```bash
# Build
make build

# Terminal A: Alice
bin/chirpd --data /tmp/chirp-alice --http 127.0.0.1:7777 --listen 127.0.0.1:9001 --name Alice

# Terminal B: Bob
bin/chirpd --data /tmp/chirp-bob   --http 127.0.0.1:7778 --listen 127.0.0.1:9002 --name Bob
```

Both instances should discover each other via mDNS on loopback and establish a Noise XX session. Send a message from Alice's UI (http://127.0.0.1:7777) to Bob and confirm it appears in Bob's UI (http://127.0.0.1:7778).

**Expected result**: peers appear in the Peers panel, messages round-trip with delivery confirmation.

## Manual two-machine test script

### Prerequisites

- Two devices on the same Wi-Fi network
- Chirp installed on both (or built from source)

### Test: same Wi-Fi

1. Start chirpd on both devices with different names
2. Wait 5-10 seconds for mDNS discovery
3. Confirm both peers appear in the Nearby panel
4. Send a message from A to B, then B to A
5. Verify delivery checkmarks appear

**Pass/Fail**: ___

### Test: guest-network isolation

1. Connect device A to the main Wi-Fi, device B to the guest network
2. Start chirpd on both
3. Wait 30 seconds

**Expected**: peers do NOT discover each other (guest networks typically isolate clients).

**Pass/Fail**: ___

### Test: VPN

1. Connect both devices to the same VPN
2. Start chirpd on both

**Expected**: depends on VPN configuration. If the VPN supports multicast (rare), discovery works. Otherwise, use add-by-address.

**Pass/Fail**: ___  Notes: ___

### Test: macOS firewall prompt

1. On a fresh macOS install (or after resetting firewall), start chirpd
2. macOS should prompt for local network permission

**Expected**: user grants permission, mDNS works.

**Pass/Fail**: ___

### Test: Windows Defender firewall

1. Start chirpd on Windows for the first time
2. Windows Defender Firewall prompts for network access

**Expected**: user grants access on Private network, mDNS works.

**Pass/Fail**: ___

### Test: Linux firewalld/ufw

1. On a system with firewalld or ufw, start chirpd
2. mDNS requires multicast (UDP 5353) and TCP for the peer connection

```bash
# firewalld
sudo firewall-cmd --add-service=mdns --permanent
sudo firewall-cmd --reload

# ufw
sudo ufw allow 5353/udp
sudo ufw allow 9000:9100/tcp   # adjust for your listen range
```

**Pass/Fail**: ___  Notes: ___

### Test: add-by-address (fallback)

If mDNS fails (wrong network, VPN, firewall):

1. On device A, note the listen address from diagnostics (or the log)
2. On device B, use the Add by Address UI to enter `host:port`
3. Confirm the peer appears and messaging works

**Pass/Fail**: ___

## Demo mode

`make demo` runs four scripted bot peers against each other in-memory, no network required. Useful for seeing the UI without real devices.
