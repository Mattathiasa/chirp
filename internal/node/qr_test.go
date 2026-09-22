package node

import (
	"strings"
	"testing"

	"rsc.io/qr"
)

// The QR is drawn as SVG path data rather than an image so it needs no
// external host and no data: URI, which is what keeps the CSP strict.
func TestQRSVGIsSelfContained(t *testing.T) {
	const uri = "chirp://add/192.168.1.40:47120/aabbccdd11223344"
	svg, err := QRSVG(uri, 6)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") {
		t.Fatalf("not an svg document: %.60s", svg)
	}
	// The xmlns declaration is a namespace identifier, not a fetched URL, so
	// take it out before looking for anything that would actually be loaded.
	body := strings.Replace(svg, `xmlns="http://www.w3.org/2000/svg"`, "", 1)
	for _, bad := range []string{"http://", "https://", "data:", "<script", "xlink:href", "<image", "url("} {
		if strings.Contains(body, bad) {
			t.Errorf("svg contains %q, which the CSP would block or which is unsafe", bad)
		}
	}
	if !strings.Contains(svg, `role="img"`) || !strings.Contains(svg, "aria-label") {
		t.Error("svg is not labelled for a screen reader")
	}
	// It must actually encode the URI: decode the module grid back.
	code, err := qr.Encode(uri, qr.M)
	if err != nil {
		t.Fatal(err)
	}
	black := 0
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if code.Black(x, y) {
				black++
			}
		}
	}
	if got := strings.Count(svg, "M"); got != black {
		t.Fatalf("svg draws %d modules, the code has %d black ones", got, black)
	}
}

func TestQRSVGRejectsUnencodableInput(t *testing.T) {
	if _, err := QRSVG(strings.Repeat("x", 10000), 4); err == nil {
		t.Fatal("expected an error for input too large for a QR code")
	}
}
