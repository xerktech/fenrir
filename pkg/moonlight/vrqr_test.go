package moonlight

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestVRPairingLink(t *testing.T) {
	// Fixed vectors, for the headset app's parse_pairing_link (test_pairing_link).
	var key [32]byte
	for i := range key {
		key[i] = byte(i)
	}
	const hexKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	for _, c := range []struct {
		address string
		port    int
		want    string
	}{
		{"fenrir.local", 47990, "fenrir-vr://fenrir.local:47990?key=" + hexKey},
		{"fd00::1", 443, "fenrir-vr://[fd00::1]:443?key=" + hexKey},
	} {
		if got := vrPairingLink(c.address, c.port, key); got != c.want {
			t.Errorf("vrPairingLink(%q, %d) = %q, want %q", c.address, c.port, got, c.want)
		}
	}
}

func TestVRQRCode(t *testing.T) {
	size, path, err := vrQRCode(vrPairingLink("fenrir.local", 47990, sha256.Sum256(nil)))
	if err != nil {
		t.Fatal(err)
	}
	// 99 bytes at level M is version 6, 41 modules, plus the quiet zone.
	if size != 41+2*vrQRQuietZone {
		t.Errorf("size %d", size)
	}
	// A camera needs the light border: no module in it, and the code starts right
	// after it (the finder pattern's corner is always dark).
	if vrQRQuietZone < 4 {
		t.Errorf("quiet zone %d modules, QR codes need 4", vrQRQuietZone)
	}
	if !strings.HasPrefix(path, fmt.Sprintf("M%d %dh1v1h-1z", vrQRQuietZone, vrQRQuietZone)) {
		t.Errorf("code does not start after the quiet zone: %.40s", path)
	}
	for m := range strings.SplitSeq(strings.TrimSuffix(path, "z"), "z") {
		var x, y int
		if _, err := fmt.Sscanf(m, "M%d %dh1v1h-1", &x, &y); err != nil {
			t.Fatalf("module %q: %s", m, err)
		}
		if x < vrQRQuietZone || y < vrQRQuietZone || x >= size-vrQRQuietZone || y >= size-vrQRQuietZone {
			t.Errorf("module %d,%d in the quiet zone", x, y)
		}
	}
}

// The page draws the QR code of the key the headset sees on the wire.
func TestVRPairingPageShowsQR(t *testing.T) {
	f := newVRDoorFixture(t, "alice")
	h := f.newHeadset(t)
	r := h.do(t, f, http.MethodGet, VRAppsPath, nil)
	if r.tls == nil || len(r.tls.PeerCertificates) == 0 {
		t.Fatal("no server certificate")
	}
	key := sha256.Sum256(r.tls.PeerCertificates[0].RawSubjectPublicKeyInfo)
	size, path, err := vrQRCode(vrPairingLink("127.0.0.1", 1, key))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/pin/", nil)
	req.RemoteAddr = trustedPeer
	req.Header.Set(PinProxySecretHeader, proxySecret)
	req.Header.Set(DefaultPinUserHeader, "alice")
	rec := httptest.NewRecorder()
	f.pinPage.ServeHTTP(rec, req)
	body := rec.Body.String()
	viewBox := `viewBox="0 0 ` + strconv.Itoa(size) + " " + strconv.Itoa(size) + `"`
	if rec.Code != http.StatusOK || !strings.Contains(body, `d="`+path+`"`) || !strings.Contains(body, viewBox) {
		t.Errorf("page %d lacks the QR code of the wire key:\n%s", rec.Code, body)
	}
}
