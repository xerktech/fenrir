package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAppVisibleTo(t *testing.T) {
	app := func(labels map[string]string) *App { return &App{ObjectMeta: metav1.ObjectMeta{Labels: labels}} }
	for _, tc := range []struct {
		name       string
		app        *App
		shared     bool // visible to a User of the shared library
		sam, other bool // to sam (own library), to another own-library User
	}{
		{"hand-written", app(nil), true, true, true},
		{"shared Steam game", app(map[string]string{CatalogueLabel: CatalogueStoreSteam}), true, false, false},
		{"shared Battle.net game", app(map[string]string{CatalogueLabel: CatalogueStoreBattlenet}), true, false, false},
		{"RomM ROM", app(map[string]string{CatalogueLabel: CatalogueStoreRomM}), true, true, true},
		{"sam's game", app(map[string]string{CatalogueLabel: CatalogueStoreEpic, LibraryOwnerLabel: "sam"}), false, true, false},
	} {
		for owner, want := range map[string]bool{"": tc.shared, "sam": tc.sam, "kim": tc.other} {
			if got := AppVisibleTo(tc.app, owner); got != want {
				t.Errorf("%s: AppVisibleTo(%q) = %v, want %v", tc.name, owner, got, want)
			}
		}
	}
}

func TestLibraryPodNameFor(t *testing.T) {
	if LibraryPodNameFor("") != LibraryPodName || LibraryPodNameFor("sam") != "direwolf-library-sam" {
		t.Errorf("LibraryPodNameFor = %q, %q", LibraryPodNameFor(""), LibraryPodNameFor("sam"))
	}
	if (&User{ObjectMeta: metav1.ObjectMeta{Name: "sam"}}).LibraryOwner() != "" {
		t.Error("a User without spec.library owns a library")
	}
	own := &User{ObjectMeta: metav1.ObjectMeta{Name: "sam"}, Spec: UserSpec{Library: &UserLibrary{}}}
	if own.LibraryOwner() != "sam" {
		t.Error("a User with spec.library does not own theirs")
	}
}
