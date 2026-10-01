package controllers

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	dwfake "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
)

// fixtureExec runs the Library's exec commands on this machine, with the
// pod's /config (home) and /games mounts mapped onto testdata/catalogue.
func fixtureExec(t *testing.T, dir string) PodExecutor {
	t.Helper()
	home, games := filepath.Join(dir, "home"), filepath.Join(dir, "games")
	return func(ctx context.Context, _, pod, container string, command []string) (string, error) {
		if pod != LibraryPodName || container != libraryContainer {
			return "", errors.New("exec into the wrong container")
		}
		args := slices.Clone(command)
		for i, a := range args {
			switch {
			case a == libraryHome || strings.HasPrefix(a, libraryHome+"/"):
				args[i] = home + strings.TrimPrefix(a, libraryHome)
			case a == "/games":
				args[i] = games
			}
		}
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // the Library's own scan command, on fixtures
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return stdout.String(), errors.New(err.Error() + ": " + stderr.String())
		}
		return stdout.String(), nil
	}
}

func scanFixture(t *testing.T, dir string) []catalogueGame {
	t.Helper()
	out, err := fixtureExec(t, dir)(context.Background(), libraryTestNS, LibraryPodName, libraryContainer, catalogueScanCommand("/games"))
	if err != nil {
		t.Fatalf("scan command: %v", err)
	}
	files, err := readTar([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	games, err := parseInstalls(files)
	if err != nil {
		t.Fatal(err)
	}
	return games
}

// The real scan script over fixture Steam libraries and Heroic stores: only
// fully installed games, no Steam tools, no DLC, no appName that isn't safe
// to put in a launch command.
func TestCatalogueScanFixtures(t *testing.T) {
	games := scanFixture(t, "testdata/catalogue")

	got := map[string]catalogueGame{}
	for _, g := range games {
		got[g.Store+"/"+g.ID] = g
	}
	want := map[string]string{
		"steam/570":      "Dota 2",
		"steam/1245620":  `ELDEN RING "Shadow" Edition`, // StateFlags 6: installed, update pending
		"epic/Quail":     "Hades",
		"gog/1207658924": "Unreal Tournament 2004",
	}
	if len(got) != len(want) {
		t.Errorf("scanned %d games, want %d: %+v", len(got), len(want), games)
	}
	for key, title := range want {
		if got[key].Title != title {
			t.Errorf("%s: title %q, want %q", key, got[key].Title, title)
		}
	}
	if art := got["epic/Quail"].ArtURL; len(art) == 0 || art[0] != "https://cdn1.epicgames.com/quail/hades-tall.jpg" {
		t.Errorf("Hades art = %v, want the tall cover first", art)
	}
	if art := got["steam/570"].ArtURL; len(art) == 0 || !strings.HasSuffix(art[0], "/570/library_600x900.jpg") {
		t.Errorf("Dota 2 art = %v, want the 600x900 library cover first", art)
	}
}

// An empty Library (no Steam, no Heroic yet) is no games, not an error.
func TestCatalogueScanEmptyLibrary(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"home", "games"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if games := scanFixture(t, dir); len(games) != 0 {
		t.Errorf("games = %+v, want none", games)
	}
}

// A store that doesn't parse (e.g. read mid-write) fails the scan, so the
// sync never sees it as every game uninstalled.
func TestCatalogueScanCorruptStoreFails(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"/config/.config/heroic/gog_store/installed.json", `{"installed": [`},
		{"/games/steamapps/appmanifest_1.acf", `"AppState" { "appid" "1" `},
		{"/games/steamapps/appmanifest_2.acf", `"AppState" { "appid" "abc" "name" "x" "StateFlags" "4" }`},
	} {
		if _, err := parseInstalls(map[string][]byte{tc.name: []byte(tc.data)}); err == nil {
			t.Errorf("%s: parsed %q without error", tc.name, tc.data)
		}
	}
}

func TestParseVDF(t *testing.T) {
	got, err := parseVDF([]byte("// c\n\"A\" { \"K\" \"v \\\"q\\\" \\\\\" Bare { \"x\" \"1\" } }"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": map[string]any{"k": `v "q" \`, "bare": map[string]any{"x": "1"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseVDF = %#v, want %#v", got, want)
	}
	for _, bad := range []string{`"a" {`, `}`, `"a"`, `"a" "b`, `{ }`} {
		if _, err := parseVDF([]byte(bad)); err == nil {
			t.Errorf("parseVDF(%q) succeeded", bad)
		}
	}
}

func TestCatalogueAppName(t *testing.T) {
	seen := map[string]string{}
	for _, tc := range []struct{ store, id, want string }{
		{"steam", "570", "steam-570"},
		{"gog", "1207658924", "gog-1207658924"},
		{"epic", "quail", "epic-quail"},
		{"epic", "Quail", ""}, // case changes: hashed, distinct from "quail"
		{"epic", "a.b_c", ""},
		{"epic", strings.Repeat("x", 128), ""},
		{"epic", "...", ""},
	} {
		got := catalogueAppName(tc.store, tc.id)
		if tc.want != "" && got != tc.want {
			t.Errorf("catalogueAppName(%q, %q) = %q, want %q", tc.store, tc.id, got, tc.want)
		}
		if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 || len(got) > 63 {
			t.Errorf("catalogueAppName(%q, %q) = %q is not a valid name: %v", tc.store, tc.id, got, errs)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%q and %q both map to %q", prev, tc.id, got)
		}
		seen[got] = tc.id
	}
}

func TestCatalogueMoonlightID(t *testing.T) {
	a, b := catalogueMoonlightID("steam", "570"), catalogueMoonlightID("gog", "570")
	if a == b {
		t.Error("the same ID in two stores got one Moonlight ID")
	}
	for _, id := range []int{a, b} {
		if id < 1<<30 || id > 1<<31-1 {
			t.Errorf("Moonlight ID %d outside 2^30..2^31-1", id)
		}
	}
	if catalogueMoonlightID("steam", "570") != a {
		t.Error("Moonlight ID is not stable")
	}
}

func TestLaunchCommand(t *testing.T) {
	for _, tc := range []struct {
		game catalogueGame
		want string
	}{
		{catalogueGame{Store: v1alpha1types.CatalogueStoreSteam, ID: "570"}, "steam -applaunch 570"},
		{catalogueGame{Store: v1alpha1types.CatalogueStoreEpic, ID: "Quail"}, "heroic 'heroic://launch?appName=Quail&runner=legendary'"},
		{catalogueGame{Store: v1alpha1types.CatalogueStoreGOG, ID: "1207658924"}, "heroic 'heroic://launch?appName=1207658924&runner=gog'"},
	} {
		if got := launchCommand(tc.game); got != tc.want {
			t.Errorf("launchCommand(%+v) = %q, want %q", tc.game, got, tc.want)
		}
	}
}

func TestCheckArtURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://cdn.cloudflare.steamstatic.com/steam/apps/570/library_600x900.jpg": true,
		"https://cdn1.epicgames.com/x.jpg":                                          true,
		"https://images.gog-statics.com/x.jpg":                                      true,
		"https://images.gog-statics.com:443/x.jpg":                                  true,
		"http://cdn.cloudflare.steamstatic.com/x.jpg":                               false,
		"https://steamstatic.com.evil.example/x.jpg":                                false,
		"https://evilsteamstatic.com/x.jpg":                                         false,
		"https://user:pw@cdn1.epicgames.com/x.jpg":                                  false,
		"https://cdn1.epicgames.com:8443/x.jpg":                                     false,
		"https://10.0.0.1/x.jpg":                                                    false,
		"https://kubernetes.default.svc/api":                                        false,
		"fallback":                                                                  false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := checkArtURL(u) == nil; got != ok {
			t.Errorf("checkArtURL(%s) allowed = %v, want %v", raw, got, ok)
		}
	}
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 6, 9))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fetchArt against a local TLS server standing in for every allowed host.
func TestFetchArt(t *testing.T) {
	pngData := testPNG(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.png":
			_, _ = w.Write(pngData)
		case "/big.png":
			_, _ = w.Write(append(slices.Clone(pngData), make([]byte, catalogueMaxArtBytes)...))
		case "/html":
			_, _ = w.Write([]byte("<html>not an image</html>"))
		case "/redirect-out":
			http.Redirect(w, r, "https://169.254.169.254/latest/meta-data", http.StatusFound)
		case "/redirect-in":
			http.Redirect(w, r, "https://cdn1.epicgames.com/ok.png", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	base, ok := srv.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("test server client has no *http.Transport")
	}
	transport := base.Clone()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	transport.TLSClientConfig.InsecureSkipVerify = true // the test server stands in for every host
	client := &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		return checkArtURL(req.URL)
	}}

	for path, wantOK := range map[string]bool{
		"/ok.png":       true,
		"/redirect-in":  true,
		"/big.png":      false,
		"/html":         false,
		"/missing":      false,
		"/redirect-out": false,
	} {
		data, err := fetchArt(context.Background(), client, "https://cdn1.epicgames.com"+path)
		if (err == nil) != wantOK {
			t.Errorf("fetchArt(%s): err = %v, want ok=%v", path, err, wantOK)
		}
		if wantOK && !bytes.Equal(data, pngData) {
			t.Errorf("fetchArt(%s) returned %d bytes, want the PNG", path, len(data))
		}
	}
	if _, err := fetchArt(context.Background(), client, "https://"+srv.Listener.Addr().String()+"/ok.png"); err == nil {
		t.Error("fetchArt fetched from a host that is not allowed")
	}
}

func baseApp(name string, gpu *v1alpha1types.AppGPU) *v1alpha1types.App {
	return &v1alpha1types.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: libraryTestNS},
		Spec: v1alpha1types.AppSpec{
			Title:        name,
			ID:           2,
			Hidden:       true,
			AppAssetWebP: []byte("base-art"),
			GPU:          gpu,
			Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:  "app",
				Image: "steam:1",
				Env:   []corev1.EnvVar{{Name: LaunchCommandEnv, Value: "stale"}, {Name: "KEEP", Value: "1"}},
			}}}},
		},
	}
}

type catalogueFixture struct {
	dw        *dwfake.Clientset
	catalogue *Catalogue
	fetched   []string
}

func newCatalogueFixture(t *testing.T, objs ...runtime.Object) *catalogueFixture {
	t.Helper()
	f := &catalogueFixture{dw: dwfake.NewSimpleClientset(objs...)}
	f.catalogue = NewCatalogue(f.dw.DirewolfV1alpha1().Apps(libraryTestNS), fixtureExec(t, "testdata/catalogue"), "/games",
		CatalogueOptions{SteamBaseApp: "steam-base", HeroicBaseApp: "heroic-base"})
	f.catalogue.now = func() time.Time { return libraryTestNow }
	pngData := testPNG(t)
	f.catalogue.FetchArt = func(_ context.Context, u string) ([]byte, error) {
		f.fetched = append(f.fetched, u)
		if strings.Contains(u, "header.jpg") || strings.Contains(u, "wide") {
			return nil, errors.New("404")
		}
		return pngData, nil
	}
	return f
}

func (f *catalogueFixture) app(t *testing.T, name string) *v1alpha1types.App {
	t.Helper()
	app, err := f.dw.DirewolfV1alpha1().Apps(libraryTestNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("App %s: %v", name, err)
	}
	return app
}

func (f *catalogueFixture) scan(t *testing.T) {
	t.Helper()
	if err := f.catalogue.Scan(context.Background(), libraryPod(0, true)); err != nil {
		t.Fatalf("Scan: %v", err)
	}
}

func TestCatalogueSync(t *testing.T) {
	eightGi := resource.MustParse("8Gi")
	handMade := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{Name: "firefox", Namespace: libraryTestNS}}
	uninstalled := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{
		Name: "steam-999", Namespace: libraryTestNS, UID: "u999",
		Labels: map[string]string{v1alpha1types.CatalogueLabel: v1alpha1types.CatalogueStoreSteam},
	}}
	f := newCatalogueFixture(t,
		baseApp("steam-base", &v1alpha1types.AppGPU{Memory: &eightGi}),
		baseApp("heroic-base", nil),
		handMade, uninstalled)
	f.scan(t)

	apps, err := f.dw.DirewolfV1alpha1().Apps(libraryTestNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(apps.Items))
	for _, a := range apps.Items {
		names = append(names, a.Name)
	}
	slices.Sort(names)
	want := []string{catalogueAppName(v1alpha1types.CatalogueStoreEpic, "Quail"), "firefox", "gog-1207658924", "heroic-base", "steam-1245620", "steam-570", "steam-base"}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("Apps = %v, want %v (steam-999 uninstalled, hand-made firefox kept)", names, want)
	}

	dota := f.app(t, "steam-570")
	if dota.Labels[v1alpha1types.CatalogueLabel] != "steam" || dota.Annotations[catalogueIDAnnotation] != "570" {
		t.Errorf("steam-570 labels %v annotations %v", dota.Labels, dota.Annotations)
	}
	if dota.Spec.Title != "Dota 2" || dota.Spec.ID != catalogueMoonlightID("steam", "570") || dota.Spec.Hidden {
		t.Errorf("steam-570 spec title=%q id=%d hidden=%v", dota.Spec.Title, dota.Spec.ID, dota.Spec.Hidden)
	}
	if dota.Spec.GPU == nil || dota.Spec.GPU.Memory.Cmp(eightGi) != 0 {
		t.Errorf("steam-570 gpu = %+v, want the base App's 8Gi", dota.Spec.GPU)
	}
	env := dota.Spec.Template.Spec.Containers[0].Env
	wantEnv := []corev1.EnvVar{{Name: "KEEP", Value: "1"}, {Name: LaunchCommandEnv, Value: "steam -applaunch 570"}}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Errorf("steam-570 env = %+v, want %+v", env, wantEnv)
	}
	if len(dota.Spec.AppAssetWebP) == 0 || bytes.Equal(dota.Spec.AppAssetWebP, []byte("base-art")) {
		t.Error("steam-570 has no box art of its own")
	}
	ut := f.app(t, "gog-1207658924")
	if ut.Annotations[catalogueArtAnnotation] != "https://images.gog-statics.com/ut2004.jpg" {
		t.Errorf("UT2004 art from %q, want the GOG cover", ut.Annotations[catalogueArtAnnotation])
	}
	if hero := f.app(t, catalogueAppName("epic", "Quail")); !strings.Contains(hero.Spec.Template.Spec.Containers[0].Env[1].Value, "runner=legendary") {
		t.Errorf("Hades launch = %+v", hero.Spec.Template.Spec.Containers[0].Env)
	}

	// The user hides one game and gives it the whole card; the base App
	// moves to a new image.
	dota.Spec.Hidden = true
	dota.Spec.GPU = &v1alpha1types.AppGPU{}
	if _, err := f.dw.DirewolfV1alpha1().Apps(libraryTestNS).Update(context.Background(), dota, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	base := f.app(t, "steam-base")
	base.Spec.Template.Spec.Containers[0].Image = "steam:2"
	if _, err := f.dw.DirewolfV1alpha1().Apps(libraryTestNS).Update(context.Background(), base, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	fetchedBefore := len(f.fetched)
	f.scan(t)

	dota = f.app(t, "steam-570")
	if !dota.Spec.Hidden || dota.Spec.GPU == nil || dota.Spec.GPU.Memory != nil {
		t.Errorf("per-game settings not kept: hidden=%v gpu=%+v", dota.Spec.Hidden, dota.Spec.GPU)
	}
	if img := dota.Spec.Template.Spec.Containers[0].Image; img != "steam:2" {
		t.Errorf("image = %q, want the base App's new steam:2", img)
	}
	// Art comes once; failed URLs are not retried within catalogueArtRetry.
	if again := f.fetched[fetchedBefore:]; len(again) != 0 {
		t.Errorf("second scan fetched art again: %v", again)
	}
}

// A launcher whose base App is missing is not synced: its Apps stay, rather
// than being deleted as uninstalled.
func TestCatalogueSyncMissingBaseKeepsApps(t *testing.T) {
	heroicApp := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{
		Name: "gog-1", Namespace: libraryTestNS,
		Labels: map[string]string{v1alpha1types.CatalogueLabel: v1alpha1types.CatalogueStoreGOG},
	}}
	f := newCatalogueFixture(t, baseApp("steam-base", nil), heroicApp)
	if err := f.catalogue.Scan(context.Background(), libraryPod(0, true)); err == nil {
		t.Error("Scan with a missing base App reported no error")
	}
	f.app(t, "gog-1")
	f.app(t, "steam-570")
}

// The Library controller scans while the Library runs and once more as it
// stops (after Steam has shut down), and a failing scan never blocks it.
func TestLibraryScansCatalogue(t *testing.T) {
	pod := libraryPod(2*DefaultLibraryIdleTimeout, true)
	f := newLibraryFixture(t, []runtime.Object{pod})
	cf := newCatalogueFixture(t, baseApp("steam-base", nil), baseApp("heroic-base", nil))
	scans := 0
	fixture := fixtureExec(t, "testdata/catalogue")
	cf.catalogue.Exec = func(ctx context.Context, ns, p, ctr string, command []string) (string, error) {
		scans++
		if f.exec.shutdowns == 0 {
			return "", errors.New("scanned before steam -shutdown")
		}
		return fixture(ctx, ns, p, ctr, command)
	}
	f.library.Catalogue = cf.catalogue

	if err := f.library.stop(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if scans != 1 || f.podExists(t) {
		t.Errorf("stop: %d scans, pod exists %v; want 1 scan after shutdown and the pod gone", scans, f.podExists(t))
	}
	cf.app(t, "steam-570")

	// A periodic scan is rate limited.
	cf.catalogue.lastScan = libraryTestNow.Add(-time.Minute)
	if cf.catalogue.ScanDue() {
		t.Error("scan due a minute after the last one")
	}
	cf.catalogue.lastScan = libraryTestNow.Add(-catalogueScanInterval)
	if !cf.catalogue.ScanDue() {
		t.Error("scan not due after catalogueScanInterval")
	}
}

// App.spec.gpu becomes a per-Session ResourceClaim the pod references by
// name, on the game container and Wolf (NVENC on the same card).
func TestSessionPodGPUFromAppSpec(t *testing.T) {
	for _, tc := range []struct {
		gpu          string
		wantCapacity string
	}{
		{"gpu: {memory: 8Gi}", "8Gi"},
		{"gpu: {}", ""},
	} {
		raw, err := os.ReadFile("../../examples/steam.yaml")
		if err != nil {
			t.Fatal(err)
		}
		appPath := filepath.Join(t.TempDir(), "app.yaml")
		withGPU := strings.Replace(string(raw), "\nspec:\n", "\nspec:\n  "+tc.gpu+"\n", 1)
		if err = os.WriteFile(appPath, []byte(withGPU), 0o600); err != nil { //nolint:gosec // t.TempDir()
			t.Fatal(err)
		}
		_, k8s, sess, pod := reconcileFixtures(t, "../../examples/user.yaml", appPath)

		want := corev1.PodResourceClaim{Name: appGPUClaim, ResourceClaimName: new(gpuClaimName(sess.Name))}
		if !slices.ContainsFunc(pod.Spec.ResourceClaims, func(c corev1.PodResourceClaim) bool { return reflect.DeepEqual(c, want) }) {
			t.Errorf("%s: pod resourceClaims = %+v, want %+v", tc.gpu, pod.Spec.ResourceClaims, want)
		}
		for _, ctr := range pod.Spec.Containers {
			has := slices.Contains(ctr.Resources.Claims, corev1.ResourceClaim{Name: appGPUClaim})
			if wantClaim := ctr.Name == "app" || ctr.Name == "wolf"; has != wantClaim {
				t.Errorf("%s: container %s claims GPU = %v, want %v", tc.gpu, ctr.Name, has, wantClaim)
			}
		}

		claim, err := k8s.ResourceV1().ResourceClaims(sess.Namespace).Get(context.Background(), gpuClaimName(sess.Name), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("%s: %v", tc.gpu, err)
		}
		if !metav1.IsControlledBy(claim, sess) {
			t.Errorf("%s: claim owners = %+v, want the Session", tc.gpu, claim.OwnerReferences)
		}
		req := claim.Spec.Devices.Requests[0].Exactly
		if req.DeviceClassName != defaultGPUDeviceClass {
			t.Errorf("%s: device class %q", tc.gpu, req.DeviceClassName)
		}
		switch {
		case tc.wantCapacity == "" && req.Capacity != nil:
			t.Errorf("%s: whole card wanted, claim requests %+v", tc.gpu, req.Capacity)
		case tc.wantCapacity != "":
			got := req.Capacity.Requests[gpuClaimMemoryCapacity]
			if got.Cmp(resource.MustParse(tc.wantCapacity)) != 0 {
				t.Errorf("%s: claim memory = %s, want %s", tc.gpu, got.String(), tc.wantCapacity)
			}
		}
	}
}

// A claim left by an earlier Session of the same name is waited out, never
// adopted.
func TestGPUClaimFromEarlierSessionIsNotReused(t *testing.T) {
	sc, k8s, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml")
	app, err := sc.AppInformer.Namespaced(sess.Namespace).Get(sess.Spec.GameReference.Name)
	if err != nil {
		t.Fatal(err)
	}
	app = app.DeepCopy()
	app.Spec.GPU = &v1alpha1types.AppGPU{}
	if err := sc.AppInformer.GetIndexer().Update(app); err != nil {
		t.Fatal(err)
	}
	old := sess.DeepCopy()
	old.UID = "earlier"
	if _, err := k8s.ResourceV1().ResourceClaims(sess.Namespace).Create(context.Background(), buildGPUClaim(app, old), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := sc.reconcileGPUClaim(context.Background(), sess); err == nil {
		t.Error("reconcileGPUClaim adopted an earlier Session's claim")
	}
	if err := k8s.ResourceV1().ResourceClaims(sess.Namespace).Delete(context.Background(), gpuClaimName(sess.Name), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := sc.reconcileGPUClaim(context.Background(), sess); err != nil {
		t.Errorf("reconcileGPUClaim after GC: %v", err)
	}
	if _, err := k8s.ResourceV1().ResourceClaims(sess.Namespace).Get(context.Background(), gpuClaimName(sess.Name), metav1.GetOptions{}); apierrors.IsNotFound(err) {
		t.Error("no claim created after the earlier one was gone")
	}
}
