package controllers

import (
	"bytes"
	"encoding/binary"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

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
	want := []catalogueGame{{Store: v1alpha1types.CatalogueStoreBattlenet, ID: "wow", Title: "World of Warcraft", ArtURL: []string{battlenetProducts["wow"].art}}}
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
		"installed":              {pbBytes(1, pbInstall("wow", true)), []string{"wow"}},
		"not installed":          {pbBytes(1, pbInstall("wow", false)), nil},
		"no cached state":        {pbBytes(1, pbBytes(1, []byte("wow"))), nil},
		"only the client":        {slices.Concat(pbBytes(1, pbInstall("agent", true)), pbBytes(1, pbInstall("battle.net", true))), nil},
		"unknown product":        {pbBytes(1, pbInstall("s2", true)), nil},
		"unusable uid":           {pbBytes(1, pbInstall("wow; reboot", true)), nil},
		"listed twice, one done": {slices.Concat(pbBytes(1, pbInstall("wow", false)), pbBytes(1, pbInstall("wow", true))), []string{"wow"}},
		"empty product_install":  {slices.Concat(pbBytes(1, pbInstall("wow", true)), pbBytes(2)), []string{"wow"}},
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
	matches := regexp.MustCompile(`(?m)^\s+([a-z0-9_]+)\) code=`).FindAllSubmatch(script, -1)
	launchable := make([]string, 0, len(matches))
	for _, m := range matches {
		launchable = append(launchable, string(m[1]))
	}
	catalogued := slices.Sorted(maps.Keys(battlenetProducts))
	slices.Sort(launchable)
	if len(launchable) == 0 || !slices.Equal(catalogued, launchable) {
		t.Errorf("battlenetProducts %v, launch-battlenet plays %v; want the same", catalogued, launchable)
	}
	for uid, p := range battlenetProducts {
		if !battlenetUID.MatchString(uid) || catalogueAppName(v1alpha1types.CatalogueStoreBattlenet, uid) != "battlenet-"+uid {
			t.Errorf("uid %q is not safe in a shell line and an App name", uid)
		}
		if u, err := url.Parse(p.art); err != nil || checkArtURL(u) != nil {
			t.Errorf("%s art %s is not on artHosts", uid, p.art)
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
	if !slices.Contains(f.fetched, battlenetProducts["wow"].art) {
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
