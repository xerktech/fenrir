package moonlight

import (
	"bytes"
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
