package node

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// InviteURI is the chirp://add/ format for manually connecting to a peer.
// Format: chirp://add/<host>:<port>/<fingerprint-prefix>
// The fingerprint prefix is at least 8 hex chars for basic verification.
const (
	InvitePrefix = "chirp://add/"
	MinFPLen     = 8
)

// ParseInvite parses a chirp://add/ URI and returns host, port, and the
// fingerprint prefix. Strict validation: no empty fields, valid port,
// valid hex fingerprint prefix.
func ParseInvite(uri string) (host, port, fpPrefix string, err error) {
	if !strings.HasPrefix(uri, InvitePrefix) {
		return "", "", "", errors.New("invite: must start with chirp://add/")
	}
	rest := uri[len(InvitePrefix):]
	if rest == "" {
		return "", "", "", errors.New("invite: empty address")
	}

	// Split host:port/fp
	hostPort, fp, hasFP := strings.Cut(rest, "/")
	if !hasFP || fp == "" {
		return "", "", "", errors.New("invite: missing fingerprint prefix")
	}
	if len(fp) < MinFPLen {
		return "", "", "", fmt.Errorf("invite: fingerprint prefix must be at least %d chars", MinFPLen)
	}
	for _, c := range fp {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", "", "", errors.New("invite: fingerprint must be lowercase hex")
		}
	}

	host, port, err = net.SplitHostPort(hostPort)
	if err != nil {
		return "", "", "", fmt.Errorf("invite: bad host:port: %w", err)
	}
	if host == "" {
		return "", "", "", errors.New("invite: empty host")
	}
	if port == "" {
		return "", "", "", errors.New("invite: empty port")
	}

	return host, port, fp, nil
}

// FormatInvite creates a chirp://add/ URI from host, port, and fingerprint.
func FormatInvite(host, port, fingerprint string) string {
	fp := fingerprint
	if len(fp) > MaxPreviewFP {
		fp = fp[:MaxPreviewFP]
	}
	return fmt.Sprintf("%s%s:%s/%s", InvitePrefix, host, port, fp)
}

const MaxPreviewFP = 16
