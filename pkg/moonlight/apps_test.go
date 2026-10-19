package moonlight

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

func appServer(t *testing.T, apps ...*v1alpha1types.App) *RESTServer {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, a := range apps {
		if err := indexer.Add(a); err != nil {
			t.Fatal(err)
		}
	}
	return &RESTServer{AppLister: generic.NewLister[*v1alpha1types.App](indexer).Namespaced(testNamespace)}
}

func testApp(name string, id int, hidden bool, asset []byte) *v1alpha1types.App {
	return &v1alpha1types.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       v1alpha1types.AppSpec{Title: name, ID: id, Hidden: hidden, AppAssetWebP: asset},
	}
}

// Hidden Apps (e.g. the catalogue's base Apps) are not listed, and can't be
// launched or looked up by ID either.
func TestHiddenAppsAreNotListed(t *testing.T) {
	s := appServer(t, testApp("shown", 1, false, nil), testApp("steam-base", 2, true, nil))

	rec := httptest.NewRecorder()
	s.appListHandler(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/applist", http.NoBody))
	body := rec.Body.String()
	if !strings.Contains(body, "<AppTitle>shown</AppTitle>") || strings.Contains(body, "steam-base") {
		t.Errorf("applist = %s, want only the shown App", body)
	}
	if strings.Contains(body, "Hidden") || strings.Contains(body, "GPU") {
		t.Errorf("applist leaks App fields Moonlight doesn't know: %s", body)
	}
	if _, err := s.getAppByID("2"); err == nil {
		t.Error("getAppByID found the hidden App")
	}
	if _, err := s.getAppByID("1"); err != nil {
		t.Errorf("getAppByID(1): %v", err)
	}
}

// The catalogue stores the box art it fetches (JPEG from Steam's CDN) as is;
// Moonlight gets it as PNG like a WebP asset.

// A JPEG cover is served as PNG, and one larger than box art needs is scaled
// down to fit 600x800 (XERK-1343: Epic's 1200x1600 covers timed Moonlight out).
func TestAppAssetServesJPEGAsPNG(t *testing.T) {
	for _, tc := range []struct{ w, h, wantW, wantH int }{
		{6, 9, 6, 9},
		{1200, 1600, 600, 800},
		{2560, 1440, 600, 337},
		{500, 1600, 250, 800},
		{4096, 10, 600, 1},
		{4096, 1, 600, 1},
	} {
		var jpg bytes.Buffer
		if err := jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, tc.w, tc.h)), nil); err != nil {
			t.Fatal(err)
		}
		s := appServer(t, testApp("game", 7, false, jpg.Bytes()))

		rec := httptest.NewRecorder()
		s.appAssetHandler(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/appasset?appid=7", http.NoBody))
		img, err := png.Decode(rec.Body)
		if err != nil {
			t.Fatalf("%dx%d: appasset is not a PNG: %v", tc.w, tc.h, err)
		}
		if b := img.Bounds(); b.Dx() != tc.wantW || b.Dy() != tc.wantH {
			t.Errorf("%dx%d: appasset is %v, want %dx%d", tc.w, tc.h, b, tc.wantW, tc.wantH)
		}
	}
}

// An asset that does not decode is an error, not a 200 with an empty body.
func TestAppAssetUndecodableIsAnError(t *testing.T) {
	s := appServer(t, testApp("game", 7, false, []byte("garbage")))

	rec := httptest.NewRecorder()
	s.appAssetHandler(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/appasset?appid=7", http.NoBody))
	if rec.Code == http.StatusOK {
		t.Errorf("undecodable appasset answered %d, want an error status", rec.Code)
	}
}

// A scaled cover is the picture scaled, not a blank of the right size, and
// its Content-Length really reaches the client (it is lost if set after
// WriteHeader, which a ResponseRecorder does not show).
func TestAppAssetScaledOverTheWire(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1200, 1600))
	for y := range 1600 {
		for x := range 1200 {
			if x < 600 {
				src.Set(x, y, color.RGBA{R: 255, A: 255})
			} else {
				src.Set(x, y, color.RGBA{B: 255, A: 255})
			}
		}
	}
	var pngSrc bytes.Buffer
	if err := png.Encode(&pngSrc, src); err != nil {
		t.Fatal(err)
	}
	s := appServer(t, testApp("game", 7, false, pngSrc.Bytes()))
	srv := httptest.NewServer(http.HandlerFunc(s.appAssetHandler))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/appasset?appid=7", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ContentLength <= 0 {
		t.Errorf("Content-Length = %d, want the PNG's size", resp.ContentLength)
	}
	img, err := png.Decode(resp.Body)
	if err != nil {
		t.Fatalf("appasset is not a PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 600 || b.Dy() != 800 {
		t.Fatalf("appasset is %v, want 600x800", b)
	}
	if r, _, b, _ := img.At(10, 400).RGBA(); r>>8 < 250 || b>>8 > 5 {
		t.Errorf("left edge is not red: r=%d b=%d", r>>8, b>>8)
	}
	if r, _, b, _ := img.At(590, 400).RGBA(); b>>8 < 250 || r>>8 > 5 {
		t.Errorf("right edge is not blue: r=%d b=%d", r>>8, b>>8)
	}
}

// exifJPEG encodes img as a JPEG carrying EXIF orientation o, in the given
// TIFF byte order ("II" or "MM").
func exifJPEG(t *testing.T, img image.Image, o uint16, order string) []byte {
	t.Helper()
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	var bo binary.AppendByteOrder = binary.BigEndian
	if order == "II" {
		bo = binary.LittleEndian
	}
	tiff := []byte(order)
	tiff = bo.AppendUint16(tiff, 42)
	tiff = bo.AppendUint32(tiff, 8)
	tiff = bo.AppendUint16(tiff, 1)      // one IFD0 entry
	tiff = bo.AppendUint16(tiff, 0x0112) // Orientation
	tiff = bo.AppendUint16(tiff, 3)      // SHORT
	tiff = bo.AppendUint32(tiff, 1)
	tiff = bo.AppendUint16(tiff, o)
	tiff = append(tiff, 0, 0, 0, 0, 0, 0) // value padding, next IFD = 0
	app1 := append([]byte("Exif\x00\x00"), tiff...)
	out := binary.BigEndian.AppendUint16([]byte{0xFF, 0xD8, 0xFF, 0xE1}, uint16(len(app1)+2)) //nolint:gosec // a few dozen bytes
	out = append(out, app1...)
	return append(out, jpg.Bytes()[2:]...)
}

// A JPEG cover is turned upright per its EXIF orientation before it is
// scaled: a 1600x1200 photo tagged "rotate 90" is a 600x800 portrait cover.
func TestAppAssetHonoursJPEGOrientation(t *testing.T) {
	// cover is w x h, its top-left quadrant red and the rest blue.
	cover := func(w, h int) image.Image {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := range h {
			for x := range w {
				c := color.RGBA{B: 255, A: 255}
				if x < w/2 && y < h/2 {
					c = color.RGBA{R: 255, A: 255}
				}
				img.SetRGBA(x, y, c)
			}
		}
		return img
	}
	isRed := func(c color.Color) bool { r, _, b, _ := c.RGBA(); return r>>8 > 200 && b>>8 < 50 }
	small, large := cover(160, 120), cover(1600, 1200)
	for _, tc := range []struct {
		name         string
		src          image.Image
		o            uint16
		order        string
		wantW, wantH int
		wantRed      string // the corner left red once upright
	}{
		{"upright", small, 1, "MM", 160, 120, "top-left"},
		{"mirrored", small, 2, "II", 160, 120, "top-right"},
		{"rotated 180", small, 3, "MM", 160, 120, "bottom-right"},
		{"flipped", small, 4, "II", 160, 120, "bottom-left"},
		{"transposed", small, 5, "MM", 120, 160, "top-left"},
		{"rotate 90 cw", small, 6, "MM", 120, 160, "top-right"},
		{"transversed", small, 7, "II", 120, 160, "bottom-right"},
		{"rotate 90 ccw", small, 8, "MM", 120, 160, "bottom-left"},
		{"out of range", small, 9, "MM", 160, 120, "top-left"},
		// XERK-1867: a 1600x1200 photo tagged "rotate 90" was served 600x450.
		{"rotate 90 cw, scaled", large, 6, "II", 600, 800, "top-right"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := appServer(t, testApp("game", 7, false, exifJPEG(t, tc.src, tc.o, tc.order)))
			rec := httptest.NewRecorder()
			s.appAssetHandler(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/appasset?appid=7", http.NoBody))
			img, err := png.Decode(rec.Body)
			if err != nil {
				t.Fatalf("appasset is not a PNG: %v", err)
			}
			b := img.Bounds()
			if b.Dx() != tc.wantW || b.Dy() != tc.wantH {
				t.Fatalf("appasset is %v, want %dx%d", b, tc.wantW, tc.wantH)
			}
			var red []string
			for name, p := range map[string]image.Point{
				"top-left": {5, 5}, "top-right": {b.Dx() - 6, 5},
				"bottom-right": {b.Dx() - 6, b.Dy() - 6}, "bottom-left": {5, b.Dy() - 6},
			} {
				if isRed(img.At(p.X, p.Y)) {
					red = append(red, name)
				}
			}
			if len(red) != 1 || red[0] != tc.wantRed {
				t.Errorf("red corners = %v, want only %s", red, tc.wantRed)
			}
		})
	}
}

// Truncated or bogus EXIF never fails the cover; it is served as stored.
func TestJPEGOrientationMalformed(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":            nil,
		"not jpeg":         []byte("garbage"),
		"truncated app1":   {0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x40, 'E', 'x', 'i', 'f', 0, 0},
		"bad byte order":   {0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x10, 'E', 'x', 'i', 'f', 0, 0, 'X', 'X', 0, 42, 0, 0, 0, 8},
		"ifd out of range": {0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x10, 'E', 'x', 'i', 'f', 0, 0, 'M', 'M', 0, 42, 0xFF, 0xFF, 0xFF, 0xF0},
	} {
		if o := jpegOrientation(data); o != 1 {
			t.Errorf("%s: orientation = %d, want 1", name, o)
		}
	}
}
