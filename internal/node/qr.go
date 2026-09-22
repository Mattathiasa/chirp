package node

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"rsc.io/qr"
)

// Invite describes how to reach this device without discovery: the URI to
// share, and the addresses it could be reached on.
type Invite struct {
	URI         string   `json:"uri"`
	Host        string   `json:"host"`
	Port        string   `json:"port"`
	Fingerprint string   `json:"fingerprint"`
	Addresses   []string `json:"addresses"` // every usable address, best first
	SVG         string   `json:"svg"`       // the URI as a scannable QR code
}

// Invite builds the invite for this node, preferring a private LAN address:
// Chirp only works on the local network, so a loopback or link-local address
// in a QR code would be useless to the person scanning it.
func (n *Node) Invite() (Invite, error) {
	port := strconv.Itoa(n.Port())
	addrs := localAddresses()
	host := "127.0.0.1"
	if len(addrs) > 0 {
		host = addrs[0]
	}
	uri := FormatInvite(host, port, n.myFP)
	svg, err := QRSVG(uri, 8)
	if err != nil {
		return Invite{}, err
	}
	return Invite{
		URI:         uri,
		Host:        host,
		Port:        port,
		Fingerprint: n.myFP,
		Addresses:   addrs,
		SVG:         svg,
	}, nil
}

// localAddresses returns the machine's usable IPv4 addresses, private ones
// first, skipping loopback and interfaces that are down.
func localAddresses() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var private, other []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		as, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range as {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if ip.IsPrivate() {
				private = append(private, ip.String())
			} else {
				other = append(other, ip.String())
			}
		}
	}
	return append(private, other...)
}

// QRSVG renders text as a QR code in SVG. It is drawn as one path of square
// modules rather than an image so it stays crisp at any size and needs neither
// an external host nor a data: URI, which keeps the CSP strict.
func QRSVG(text string, scale int) (string, error) {
	if scale <= 0 {
		scale = 8
	}
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", fmt.Errorf("node: encode qr: %w", err)
	}
	const quiet = 4 // the standard quiet zone, in modules
	dim := code.Size + quiet*2
	px := dim * scale

	var path strings.Builder
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if !code.Black(x, y) {
				continue
			}
			fmt.Fprintf(&path, "M%d %dh%dv%dh-%dz",
				(x+quiet)*scale, (y+quiet)*scale, scale, scale, scale)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" role="img" aria-label="Invite QR code" shape-rendering="crispEdges">`, px, px, px, px)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/>`, px, px)
	fmt.Fprintf(&b, `<path d="%s" fill="#0C0C14"/>`, path.String())
	b.WriteString(`</svg>`)
	return b.String(), nil
}
