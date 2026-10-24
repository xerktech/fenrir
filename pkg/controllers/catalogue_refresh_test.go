package controllers

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	dwfake "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
	generatedinformers "games-on-whales.github.io/direwolf/pkg/generated/informers/externalversions"
)

func (f *catalogueFixture) update(t *testing.T, app *v1alpha1types.App) {
	t.Helper()
	if _, err := f.dw.DirewolfV1alpha1().Apps(libraryTestNS).Update(context.Background(), app, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A base App change reaches its catalogue Apps with no Library scan
// (XERK-1963), keeping each game's own settings, launch and cover.
func TestCatalogueRefreshFollowsBaseApp(t *testing.T) {
	f := newCatalogueFixture(t, baseApp("steam-base", nil), baseApp("heroic-base", nil))
	f.scan(t)

	dota := f.app(t, "steam-570")
	dota.Spec.Hidden = true
	dota.Spec.GPU = &v1alpha1types.AppGPU{}
	f.update(t, dota)
	art := dota.Spec.AppAssetWebP
	for _, name := range []string{"steam-base", "heroic-base"} {
		base := f.app(t, name)
		base.Spec.Template.Spec.Containers[0].Image = "new:2"
		f.update(t, base)
	}
	fetchedBefore := len(f.fetched)
	if err := f.catalogue.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	dota = f.app(t, "steam-570")
	if img := dota.Spec.Template.Spec.Containers[0].Image; img != "new:2" {
		t.Errorf("steam-570 image = %q, want the base App's new:2", img)
	}
	if !dota.Spec.Hidden || dota.Spec.GPU == nil || dota.Spec.GPU.Memory != nil {
		t.Errorf("per-game settings not kept: hidden=%v gpu=%+v", dota.Spec.Hidden, dota.Spec.GPU)
	}
	if dota.Spec.Title != "Dota 2" || dota.Spec.ID != catalogueMoonlightID("steam", "570") || !bytes.Equal(dota.Spec.AppAssetWebP, art) {
		t.Errorf("steam-570 title=%q id=%d, or its cover was replaced", dota.Spec.Title, dota.Spec.ID)
	}
	if env := dota.Spec.Template.Spec.Containers[0].Env; env[len(env)-1].Value != "steam -applaunch 570" {
		t.Errorf("steam-570 env = %+v", env)
	}
	hades := f.app(t, catalogueAppName(v1alpha1types.CatalogueStoreEpic, "Quail"))
	if img := hades.Spec.Template.Spec.Containers[0].Image; img != "new:2" {
		t.Errorf("Hades (hashed name) image = %q, want new:2", img)
	}
	if again := f.fetched[fetchedBefore:]; len(again) != 0 {
		t.Errorf("Refresh fetched art: %v", again)
	}
}

// A refresh never trusts an ID annotation that a scan would have refused:
// it goes into the launch command's shell line.
func TestCatalogueRefreshRejectsForgedID(t *testing.T) {
	forged := func(name, store, id string) *v1alpha1types.App {
		app := baseApp(name, nil)
		app.Labels = map[string]string{v1alpha1types.CatalogueLabel: store}
		app.Annotations = map[string]string{catalogueIDAnnotation: id}
		return app
	}
	f := newCatalogueFixture(t, baseApp("steam-base", nil), baseApp("heroic-base", nil),
		forged("steam-570", "steam", "570; reboot"),
		forged("steam-1", "steam", "570"),
		forged("epic-x", "epic", "x'"),
		// Named as a scan would name them, so only the ID checks catch these.
		forged(catalogueAppName("steam", "1;reboot"), "steam", "1;reboot"),
		forged(catalogueAppName("epic", "x'$(id)"), "epic", "x'$(id)"),
		forged("steam-2", "steam", "2"))
	if err := f.catalogue.Refresh(context.Background()); err == nil {
		t.Error("Refresh reported no error for forged catalogue IDs")
	}
	for _, name := range []string{"steam-570", "steam-1", "epic-x",
		catalogueAppName("steam", "1;reboot"), catalogueAppName("epic", "x'$(id)")} {
		if env := f.app(t, name).Spec.Template.Spec.Containers[0].Env; env[0].Value != "stale" {
			t.Errorf("%s was refreshed: env %+v", name, env)
		}
	}
	if env := f.app(t, "steam-2").Spec.Template.Spec.Containers[0].Env; env[len(env)-1].Value != "steam -applaunch 2" {
		t.Errorf("valid steam-2 not refreshed: env %+v", env)
	}
}

// A RomM App's launch line is only known to RomM, so a refresh keeps it.
func TestCatalogueRefreshKeepsRomMLaunch(t *testing.T) {
	app := baseApp("romm-5", nil)
	app.Labels = map[string]string{v1alpha1types.CatalogueLabel: v1alpha1types.CatalogueStoreRomM}
	app.Annotations = map[string]string{catalogueIDAnnotation: "5"}
	app.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: LaunchCommandEnv, Value: "retroarch 'x.sfc'"}}
	base := baseApp("retroarch", nil)
	base.Spec.Template.Spec.Containers[0].Image = "retroarch:2"
	dw := dwfake.NewSimpleClientset(base, app)
	c := NewCatalogue(dw.DirewolfV1alpha1().Apps(libraryTestNS), nil, "", CatalogueOptions{RomMBaseApp: "retroarch"})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got, err := dw.DirewolfV1alpha1().Apps(libraryTestNS).Get(context.Background(), "romm-5", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctr := got.Spec.Template.Spec.Containers[0]
	if ctr.Image != "retroarch:2" || ctr.Env[len(ctr.Env)-1].Value != "retroarch 'x.sfc'" {
		t.Errorf("romm-5 image %q env %+v", ctr.Image, ctr.Env)
	}
}

// Refresh lists in pages (each App carries its cover) and refreshes the
// Apps of every page.
func TestCatalogueRefreshPagesList(t *testing.T) {
	f := newCatalogueFixture(t, baseApp("steam-base", nil), baseApp("heroic-base", nil))
	f.scan(t)
	for _, name := range []string{"steam-base", "heroic-base"} {
		base := f.app(t, name)
		base.Spec.Template.Spec.Containers[0].Image = "new:2"
		f.update(t, base)
	}

	pages := 0
	// One App per page: an API server may return fewer than the limit.
	f.dw.PrependReactor("list", "apps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		list, ok := action.(k8stesting.ListActionImpl)
		if !ok {
			return false, nil, nil
		}
		if list.ListOptions.Limit != catalogueRefreshPageSize {
			t.Errorf("listed with limit %d, want %d", list.ListOptions.Limit, catalogueRefreshPageSize)
		}
		gv := schema.GroupVersion(v1alpha1types.GroupVersion)
		obj, err := f.dw.Tracker().List(gv.WithResource("apps"), gv.WithKind("App"), libraryTestNS)
		if err != nil {
			return true, nil, fmt.Errorf("listing the tracker: %w", err)
		}
		var items []v1alpha1types.App
		for _, app := range obj.(*v1alpha1types.AppList).Items { //nolint:forcetypeassert // the tracker's own list
			if list.GetListRestrictions().Labels.Matches(labels.Set(app.Labels)) {
				items = append(items, app)
			}
		}
		i, _ := strconv.Atoi(list.ListOptions.Continue) // "" is the first page
		page := &v1alpha1types.AppList{Items: items[i : i+1]}
		if i+1 < len(items) {
			page.Continue = strconv.Itoa(i + 1)
		}
		pages++
		return true, page, nil
	})
	if err := f.catalogue.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if pages < 2 {
		t.Fatalf("Refresh listed %d pages, want one per App", pages)
	}
	for _, name := range []string{"steam-570", catalogueAppName(v1alpha1types.CatalogueStoreEpic, "Quail")} {
		if img := f.app(t, name).Spec.Template.Spec.Containers[0].Image; img != "new:2" {
			t.Errorf("%s image = %q, want new:2", name, img)
		}
	}
}

// BaseAppChanges signals on a base App's spec change only: not on another
// App, a metadata-only change, or a resync.
func TestBaseAppChanges(t *testing.T) {
	ctx := t.Context()
	dw := dwfake.NewSimpleClientset(baseApp("steam-base", nil), baseApp("firefox", nil))
	factory := generatedinformers.NewSharedInformerFactory(dw, 0)
	informer := factory.Direwolf().V1alpha1().Apps().Informer()
	changed, err := BaseAppChanges(informer, "steam-base", "")
	if err != nil {
		t.Fatal(err)
	}
	factory.Start(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), informer.HasSynced)
	apps := dw.DirewolfV1alpha1().Apps(libraryTestNS)

	expect := func(want bool, what string) {
		t.Helper()
		select {
		case <-changed:
			if !want {
				t.Errorf("signalled on %s", what)
			}
		case <-time.After(300 * time.Millisecond):
			if want {
				t.Errorf("no signal on %s", what)
			}
		}
	}
	expect(true, "the base App's initial add")
	edit := func(name string, mutate func(*v1alpha1types.App)) {
		t.Helper()
		app, err := apps.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		mutate(app)
		if _, err := apps.Update(ctx, app, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	edit("firefox", func(a *v1alpha1types.App) { a.Spec.Template.Spec.Containers[0].Image = "ff:2" })
	expect(false, "another App's change")
	edit("steam-base", func(a *v1alpha1types.App) { a.Labels = map[string]string{"x": "y"} })
	expect(false, "a metadata-only change")
	edit("steam-base", func(a *v1alpha1types.App) { a.Spec.Template.Spec.Containers[0].Image = "steam:2" })
	expect(true, "the base App's spec change")
}

// RunRefresh refreshes at start and again on each signal.
func TestCatalogueRunRefreshOnSignal(t *testing.T) {
	f := newCatalogueFixture(t, baseApp("steam-base", nil), baseApp("heroic-base", nil))
	f.scan(t)
	// Each refresh lists the catalogue Apps after reading the bases.
	var lists atomic.Int32
	f.dw.PrependReactor("list", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
		lists.Add(1)
		return false, nil, nil
	})
	setImage := func(image string) {
		t.Helper()
		base := f.app(t, "steam-base")
		base.Spec.Template.Spec.Containers[0].Image = image
		f.update(t, base)
	}
	waitFor := func(what string, done func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !done(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	imageIs := func(image string) func() bool {
		return func() bool { return f.app(t, "steam-570").Spec.Template.Spec.Containers[0].Image == image }
	}

	setImage("steam:2") // changed while no refresh ran
	changed := make(chan struct{})
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	go func() { f.catalogue.RunRefresh(ctx, changed); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("RunRefresh did not return when its context ended")
		}
	}()
	waitFor("the start-up refresh", imageIs("steam:2"))
	waitFor("the start-up refresh's list", func() bool { return lists.Load() == 1 })

	// The start-up refresh has read the bases, so only a signal brings steam:3.
	setImage("steam:3")
	select {
	case changed <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("RunRefresh did not take the change signal")
	}
	waitFor("a refresh on the change signal", imageIs("steam:3"))
}

// A Nexus App's ID goes into its launch line, so a refresh accepts only what
// a scan would: 16 lowercase hex digits, matching the App's name.
func TestCatalogueGameOfNexusID(t *testing.T) {
	app := func(name, id string) *v1alpha1types.App {
		a := baseApp(name, nil)
		a.Labels = map[string]string{v1alpha1types.CatalogueLabel: v1alpha1types.CatalogueStoreNexus}
		a.Annotations = map[string]string{catalogueIDAnnotation: id}
		return a
	}
	if _, err := catalogueGameOf(app("nexus-3f2a9c1e7b4d5a60", "3f2a9c1e7b4d5a60")); err != nil {
		t.Errorf("valid Nexus App rejected: %v", err)
	}
	for _, tc := range []struct{ name, id string }{
		{catalogueAppName("nexus", "3f2a;reboot"), "3f2a;reboot"},
		{catalogueAppName("nexus", "3F2A9C1E7B4D5A60"), "3F2A9C1E7B4D5A60"},
		{"nexus-0000000000000000", "3f2a9c1e7b4d5a60"},
	} {
		if _, err := catalogueGameOf(app(tc.name, tc.id)); err == nil {
			t.Errorf("forged Nexus App %s (ID %q) accepted", tc.name, tc.id)
		}
	}
}
