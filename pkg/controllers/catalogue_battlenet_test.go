package controllers

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// The fixture product.db (testdata/catalogue/home/.local/share/battlenet/...)
// lists agent, battle.net, wow (installed), wow_classic (installed, not
// launchable) and fenris (not installed), each with settings and version
// fields the parser skips, plus active_installs and two unknown top-level
// fields (fixed32, varint).

// Protobuf encoding helpers for hand-built product.db files.
func pbKey(num, wire uint64) []byte { return binary.AppendUvarint(nil, num<<3|wire) }

func pbVarint(num, v uint64) []byte { return binary.AppendUvarint(pbKey(num, 0), v) }

func pbBytes(num uint64, b ...[]byte) []byte {
	body := bytes.Join(b, nil)
	return append(binary.AppendUvarint(pbKey(num, 2), uint64(len(body))), body...)
}

// pbInstall is a ProductInstall: uid, and installed in
// cached_product_state.base_product_state.
func pbInstall(uid string, installed bool) []byte {
	var flag uint64
	if installed {
		flag = 1
	}
	return slices.Concat(pbBytes(1, []byte(uid)), pbBytes(4, pbBytes(1, pbVarint(1, flag))))
}

func parseProductDB(db []byte) ([]catalogueGame, error) {
	return parseBattlenet(map[string][]byte{battlenetProductDB: db})
}

// The fixture's product.db through the real scan script: only the installed,
// launchable product gets a game, titled and with art from battlenetProducts.
func TestCatalogueScanBattlenet(t *testing.T) {
	var got []catalogueGame
	for _, g := range scanFixture(t, "testdata/catalogue") {
		if g.Store == v1alpha1types.CatalogueStoreBattlenet {
			got = append(got, g)
		}
	}
	want := []catalogueGame{{Store: v1alpha1types.CatalogueStoreBattlenet, ID: "wow", Title: "World of Warcraft", ArtURL: battlenetProducts["wow"].art}}
	if !slices.EqualFunc(got, want, func(a, b catalogueGame) bool {
		return a.ID == b.ID && a.Title == b.Title && slices.Equal(a.ArtURL, b.ArtURL)
	}) {
		t.Errorf("Battle.net games = %+v, want %+v", got, want)
	}
}

func TestParseBattlenet(t *testing.T) {
	if games, err := parseBattlenet(map[string][]byte{}); err != nil || games != nil {
		t.Errorf("no product.db: %v, %v; want no games, no error", games, err)
	}
	for name, tc := range map[string]struct {
		db   []byte
		want []string
	}{
		"installed":                {pbBytes(1, pbInstall("wow", true)), []string{"wow"}},
		"WoW Forever":              {pbBytes(1, pbInstall("wowf", true)), []string{"wowf"}},
		"not installed":            {pbBytes(1, pbInstall("wow", false)), nil},
		"no cached state":          {pbBytes(1, pbBytes(1, []byte("wow"))), nil},
		"only the client":          {slices.Concat(pbBytes(1, pbInstall("agent", true)), pbBytes(1, pbInstall("battle.net", true))), nil},
		"unknown product":          {pbBytes(1, pbInstall("s2", true)), nil},
		"unusable uid":             {pbBytes(1, pbInstall("wow; reboot", true)), nil},
		"listed twice, done last":  {slices.Concat(pbBytes(1, pbInstall("wow", false)), pbBytes(1, pbInstall("wow", true))), []string{"wow"}},
		"listed twice, done first": {slices.Concat(pbBytes(1, pbInstall("wow", true)), pbBytes(1, pbInstall("wow", false))), []string{"wow"}},
		"empty product_install":    {slices.Concat(pbBytes(1, pbInstall("wow", true)), pbBytes(2)), []string{"wow"}},
	} {
		games, err := parseProductDB(tc.db)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var ids []string
		for _, g := range games {
			ids = append(ids, g.ID)
		}
		if !slices.Equal(ids, tc.want) {
			t.Errorf("%s: games %v, want %v", name, ids, tc.want)
		}
	}
}

// A product.db that is empty, torn or not what the parser expects fails the
// scan: read as fewer games, it would delete their Apps.
func TestParseBattlenetRejects(t *testing.T) {
	wow := pbBytes(1, pbInstall("wow", true))
	for name, db := range map[string][]byte{
		"empty":                       {},
		"truncated":                   wow[:len(wow)-1],
		"length past the end":         append(pbKey(1, 2), 0x7f, 'x'),
		"huge length":                 append(pbKey(1, 2), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01),
		"bad varint":                  append(pbKey(9, 0), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01),
		"cut fixed64":                 append(pbKey(9, 1), 1, 2, 3),
		"group":                       pbKey(9, 3),
		"field number 0":              pbVarint(0, 1),
		"field number past 2^29-1":    pbVarint(1<<29, 1),
		"product_install as a varint": pbVarint(1, 1),
		"uid as a varint":             pbBytes(1, pbVarint(1, 7)),
		"no uid":                      pbBytes(1, pbBytes(4, pbBytes(1, pbVarint(1, 1)))),
		"installed as bytes":          pbBytes(1, pbBytes(1, []byte("wow")), pbBytes(4, pbBytes(1, pbBytes(1, []byte{1})))),
		"cached state as a varint":    pbBytes(1, pbBytes(1, []byte("wow")), pbVarint(4, 1)),
		"torn nested message":         pbBytes(1, pbBytes(1, []byte("wow")), pbBytes(4, append(pbKey(1, 2), 5))),
	} {
		if games, err := parseProductDB(db); err == nil {
			t.Errorf("%s: parsed as %+v, want an error", name, games)
		} else if strings.Contains(err.Error(), "wow") {
			t.Errorf("%s: error %q quotes the file", name, err)
		}
	}
}

// The real scan script fails on a product.db over the store cap, and on one
// it cannot read, rather than reading either as no games.
func TestCatalogueScanBattlenetHostileFiles(t *testing.T) {
	const rel = "home/.local/share/battlenet/pfx/drive_c/ProgramData/Battle.net/Agent/product.db"
	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{"games/.keep": "", rel: strings.Repeat("\x00", catalogueMaxStoreBytes+1)})
	if err := scanFixtureErr(t, dir); err == nil {
		t.Error("over-cap product.db: scan succeeded")
	}
	dir = t.TempDir()
	writeFixture(t, dir, map[string]string{"games/.keep": "", rel: ""})
	if err := os.Symlink("missing", dir+"/"+rel+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir+"/"+rel+".tmp", dir+"/"+rel); err != nil {
		t.Fatal(err)
	}
	if err := scanFixtureErr(t, dir); err == nil {
		t.Error("dangling product.db symlink: scan succeeded")
	}
}

// Every catalogued product is one launch-battlenet plays, and the reverse:
// an App launch-battlenet refuses only ends its session.
func TestBattlenetProductsLaunchable(t *testing.T) {
	script, err := os.ReadFile("../../images/gow-nvidia32/root/opt/direwolf/launch-battlenet")
	if err != nil {
		t.Fatal(err)
	}
	// Any pattern before ") code=" is captured whole, so `a|b)` or `"x")`
	// shows up as a mismatch rather than going unseen.
	matches := regexp.MustCompile(`(?m)^\s+(\S+)\) code=`).FindAllSubmatch(script, -1)
	launchable := make([]string, 0, len(matches))
	for _, m := range matches {
		launchable = append(launchable, string(m[1]))
	}
	catalogued := slices.Sorted(maps.Keys(battlenetProducts))
	slices.Sort(launchable)
	if len(launchable) == 0 || !slices.Equal(catalogued, launchable) {
		t.Errorf("battlenetProducts %v, launch-battlenet plays %v; want the same", catalogued, launchable)
	}
	// Two products sharing a cover show as two identical Moonlight tiles.
	coverOf := map[string]string{}
	for uid, p := range battlenetProducts {
		for _, a := range p.art {
			if other, ok := coverOf[a]; ok {
				t.Errorf("%s and %s share art %s; give each its own", other, uid, a)
			}
			coverOf[a] = uid
		}
		if !battlenetUID.MatchString(uid) || catalogueAppName(v1alpha1types.CatalogueStoreBattlenet, uid) != "battlenet-"+uid {
			t.Errorf("uid %q is not safe in a shell line and an App name", uid)
		}
		if len(p.art) == 0 {
			t.Errorf("%s has no art", uid)
		}
		for _, a := range append(slices.Clip(p.art), p.logo) {
			if a == "" {
				continue
			}
			if u, err := url.Parse(a); err != nil || checkArtURL(u) != nil {
				t.Errorf("%s art %s is not on artHosts", uid, a)
			}
		}
	}
}

// With a Battle.net base App, an installed WoW becomes a Moonlight App whose
// container names the product in BATTLENET_PRODUCT, with Blizzard's cover.
func TestCatalogueSyncBattlenetGame(t *testing.T) {
	base := baseApp("battlenet-base", nil)
	base.Spec.RequireAppPod = true
	f := newCatalogueFixture(t, baseApp("steam-base", nil), baseApp("heroic-base", nil), base)
	f.catalogue.BattlenetBaseApp = "battlenet-base"
	f.scan(t)

	app := f.app(t, "battlenet-wow")
	if app.Labels[v1alpha1types.CatalogueLabel] != v1alpha1types.CatalogueStoreBattlenet || app.Spec.Title != "World of Warcraft" || app.Spec.Hidden || !app.Spec.RequireAppPod {
		t.Errorf("battlenet-wow labels %v title %q hidden %v requireAppPod %v", app.Labels, app.Spec.Title, app.Spec.Hidden, app.Spec.RequireAppPod)
	}
	env := map[string]string{}
	for _, e := range app.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env[battlenetProductEnv] != "wow" || env[LaunchCommandEnv] != "/opt/direwolf/launch-battlenet wow" || env["KEEP"] != "1" {
		t.Errorf("battlenet-wow env = %v", env)
	}
	if len(app.Spec.AppAssetWebP) == 0 || bytes.Equal(app.Spec.AppAssetWebP, []byte("base-art")) {
		t.Error("battlenet-wow has no cover of its own")
	}
	// The box art with the game's logo comes first; Blizzard's textless key
	// art is only the fallback (XERK-2625).
	if u, err := url.Parse(battlenetProducts["wow"].art[0]); err != nil || u.Host != "upload.wikimedia.org" {
		t.Errorf("WoW's first cover is %s, want the box art on upload.wikimedia.org", battlenetProducts["wow"].art[0])
	}
	if !slices.Contains(f.fetched, battlenetProducts["wow"].art[0]) {
		t.Errorf("WoW cover not fetched; fetched %v", f.fetched)
	}
	if _, err := f.dw.DirewolfV1alpha1().Apps(libraryTestNS).Get(t.Context(), "battlenet-wow-classic", metav1.GetOptions{}); err == nil {
		t.Error("an App was made for wow_classic, which launch-battlenet does not play")
	}
}

// A Battle.net App's ID goes into BATTLENET_PRODUCT and its launch line, so
// a refresh accepts only a catalogued product.
func TestCatalogueGameOfBattlenetID(t *testing.T) {
	app := func(name, id string) *v1alpha1types.App {
		return &v1alpha1types.App{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Labels:      map[string]string{v1alpha1types.CatalogueLabel: v1alpha1types.CatalogueStoreBattlenet},
				Annotations: map[string]string{catalogueIDAnnotation: id},
			},
			Spec: v1alpha1types.AppSpec{Template: &corev1.PodTemplateSpec{}},
		}
	}
	if _, err := catalogueGameOf(app("battlenet-wow", "wow")); err != nil {
		t.Errorf("valid Battle.net App rejected: %v", err)
	}
	for _, tc := range []struct{ name, id string }{
		{"battlenet-s2", "s2"},
		{catalogueAppName("battlenet", "wow;reboot"), "wow;reboot"},
		{"battlenet-wow", "WOW"},
	} {
		if _, err := catalogueGameOf(app(tc.name, tc.id)); err == nil {
			t.Errorf("forged Battle.net App %s (ID %q) accepted", tc.name, tc.id)
		}
	}
}

// solidPNG is a w x h PNG of one colour, for covers and logos.
func solidPNG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A product with a logo gets it drawn across the top of its art, at the size
// moonlight-proxy serves; the annotation names both, so a logo added to a
// game whose art is already fetched draws it on the next scan, and a cover
// already drawn is not fetched again.
func TestCatalogueArtDrawsBattlenetLogo(t *testing.T) {
	const artURL, logoURL = "https://upload.wikimedia.org/art.png", "https://upload.wikimedia.org/logo.png"
	red, blue := color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255}
	files := map[string][]byte{artURL: solidPNG(t, 960, 1880, blue), logoURL: solidPNG(t, 880, 720, red)}
	var fetched []string
	c := &Catalogue{now: func() time.Time { return libraryTestNow }, artFailed: map[string]time.Time{}}
	c.FetchArt = func(_ context.Context, u string) ([]byte, error) {
		fetched = append(fetched, u)
		if d, ok := files[u]; ok {
			return d, nil
		}
		return nil, errors.New("404")
	}
	game := &catalogueGame{Title: "Forever", ArtURL: []string{artURL}, Logo: logoURL}

	// Fetched before the logo was added: drawn again.
	art, from := c.art(t.Context(), game, artURL)
	if from != artURL+" "+logoURL || art == nil {
		t.Fatalf("art from %q (%d bytes), want %q", from, len(art), artURL+" "+logoURL)
	}
	img, err := jpeg.Decode(bytes.NewReader(art))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() > 600 || b.Dy() > 800 || b.Dx() < 400 {
		t.Errorf("cover is %v, want 960x1880 fitted in 600x800", b)
	}
	near := func(c color.Color, want color.RGBA) bool {
		r, g, b, _ := c.RGBA()
		d := func(x uint32, y uint8) bool { return int(x>>8)-int(y) < 40 && int(y)-int(x>>8) < 40 }
		return d(r, want.R) && d(g, want.G) && d(b, want.B)
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	// 85% of the width: 7.5% to 92.5%.
	for _, p := range []struct {
		x, y int
		want color.RGBA
	}{{w / 2, h / 10, red}, {w * 10 / 100, h / 10, red}, {w * 90 / 100, h / 10, red}, {w * 4 / 100, h / 10, blue}, {w * 96 / 100, h / 10, blue}, {w / 2, h * 9 / 10, blue}} {
		if !near(img.At(p.x, p.y), p.want) {
			t.Errorf("cover at %d,%d is %v, want %v: logo not drawn across 85%% of the top", p.x, p.y, img.At(p.x, p.y), p.want)
		}
	}

	// Already drawn: nothing fetched.
	fetched = nil
	if art, _ := c.art(t.Context(), game, from); art != nil || len(fetched) != 0 {
		t.Errorf("drawn cover fetched again: %v", fetched)
	}

	// A logo that fails leaves the App's cover alone rather than storing
	// textless art as if the logo were on it.
	// and blames the logo, not the art (fetched once, retried with it).
	delete(files, logoURL)
	c.artFailed = map[string]time.Time{}
	game.ArtURL = []string{artURL, "https://upload.wikimedia.org/art2.png"}
	files["https://upload.wikimedia.org/art2.png"] = files[artURL]
	fetched = nil
	if art, from := c.art(t.Context(), game, ""); art != nil {
		t.Errorf("cover stored from %q without its logo", from)
	}
	if _, ok := c.artFailed[logoURL]; !ok || len(c.artFailed) != 1 || !slices.Equal(fetched, []string{artURL, logoURL}) {
		t.Errorf("logo down: failed %v, fetched %v; want only the logo failed, fetched once", c.artFailed, fetched)
	}
	fetched = nil
	if art, _ := c.art(t.Context(), game, ""); art != nil || len(fetched) != 0 {
		t.Errorf("fetched %v while the logo is down", fetched)
	}

	// A logo far taller than wide stays within the cover.
	files[logoURL] = solidPNG(t, 1, 4096, red)
	c.artFailed = map[string]time.Time{}
	if art, _ := c.art(t.Context(), game, ""); art == nil {
		t.Error("no cover with a tall logo")
	}
}
