package moonlight

import (
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"

	"rsc.io/qr"
)

// vrPairingLink is what the pairing page's QR code carries (XERK-2812): the
// front door's address and the full SHA-256 of its SPKI. The Quest app pins
// that key before its first /pair, so it never has to ask the user to compare
// fingerprints (XERK-2813): only this page, behind Authentik, can show it. The
// Quest app parses it in client/fenrir.cpp parse_pairing_link: keep the two in
// step. TestVRPairingLink holds its vectors.
func vrPairingLink(address string, port int, key [32]byte) string {
	return "fenrir-vr://" + net.JoinHostPort(address, strconv.Itoa(port)) + "?key=" + hex.EncodeToString(key[:])
}

// vrQRQuietZone is the light border a QR code needs around it, in modules.
const vrQRQuietZone = 4

// vrQRCode renders text as a QR code for inline SVG: the viewBox side and one
// path drawing every dark module, quiet zone included in the side. Inline,
// because the page's CSP allows no images; a path, so the template escapes it
// as an ordinary attribute.
func vrQRCode(text string) (size int, path string, err error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return 0, "", err
	}
	var b strings.Builder
	for y := range code.Size {
		for x := range code.Size {
			if code.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+vrQRQuietZone, y+vrQRQuietZone)
			}
		}
	}
	return code.Size + 2*vrQRQuietZone, b.String(), nil
}
