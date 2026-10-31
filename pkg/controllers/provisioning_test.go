package controllers

import (
	"context"
	stderrors "errors"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	dwfake "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

type provisionFixture struct {
	k8s   *k8sfake.Clientset
	dw    *dwfake.Clientset
	cache cache.Indexer
	p     *Provisioner
}

// newProvisionFixture seeds both the API and the User cache with users.
func newProvisionFixture(t *testing.T, claims []runtime.Object, users ...*v1alpha1types.User) *provisionFixture {
	t.Helper()
	f := &provisionFixture{
		k8s:   k8sfake.NewClientset(claims...),
		dw:    dwfake.NewSimpleClientset(),
		cache: cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
	}
	for _, u := range users {
		if _, err := f.dw.DirewolfV1alpha1().Users(libraryTestNS).Create(context.Background(), u, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := f.cache.Add(u); err != nil {
			t.Fatal(err)
		}
	}
	class := "local-path"
	f.p = NewProvisioner(libraryTestNS,
		generic.NewLister[*v1alpha1types.User](f.cache).Namespaced(libraryTestNS),
		f.dw.DirewolfV1alpha1().Users(libraryTestNS),
		f.k8s.CoreV1().PersistentVolumeClaims(libraryTestNS),
		&ProvisionerOptions{
			Group: "k8x-games", GroupsHeader: DefaultLibraryGroupsHeader, GroupsSeparator: DefaultLibraryGroupsSeparator,
			UserTemplate: v1alpha1types.UserSpec{Resources: &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8")},
			}},
			Home:          ClaimTemplate{StorageClassName: class, Size: resource.MustParse("100Gi")},
			Games:         ClaimTemplate{StorageClassName: class, Size: resource.MustParse("1Ti")},
			SharedHomePVC: "steam-home", SharedGamesPVC: "games",
		})
	return f
}

func (f *provisionFixture) claim(t *testing.T, name string) *corev1.PersistentVolumeClaim {
	t.Helper()
	c, err := f.k8s.CoreV1().PersistentVolumeClaims(libraryTestNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("claim %s: %v", name, err)
	}
	return c
}

func (f *provisionFixture) claimCount(t *testing.T) int {
	t.Helper()
	l, err := f.k8s.CoreV1().PersistentVolumeClaims(libraryTestNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return len(l.Items)
}

func provisionedUser(name, username string) *v1alpha1types.User {
	u := libraryUser(name, false)
	u.Labels = map[string]string{v1alpha1types.ProvisionedLabel: v1alpha1types.ProvisionedLabelValue}
	u.Annotations = map[string]string{v1alpha1types.UsernameAnnotation: username}
	u.Spec.Library = &v1alpha1types.UserLibrary{HomeClaimName: "direwolf-home-" + name, GamesClaimName: "direwolf-games-" + name}
	return u
}

func TestProvisionerEnsureCreatesUserAndClaims(t *testing.T) {
	f := newProvisionFixture(t, nil)
	user, err := f.p.Ensure(context.Background(), "Kim")
	if err != nil {
		t.Fatal(err)
	}
	if user.Name != "kim" || user.Annotations[v1alpha1types.UsernameAnnotation] != "Kim" ||
		user.Labels[v1alpha1types.ProvisionedLabel] != "true" {
		t.Errorf("user %s labels %v annotations %v", user.Name, user.Labels, user.Annotations)
	}
	if l := user.Spec.Library; l == nil || l.HomeClaimName != "direwolf-home-kim" || l.GamesClaimName != "direwolf-games-kim" {
		t.Errorf("library = %+v", user.Spec.Library)
	}
	if user.Spec.Resources == nil || !user.Spec.Resources.Limits.Cpu().Equal(resource.MustParse("8")) {
		t.Errorf("resources = %+v, want the template's", user.Spec.Resources)
	}
	if f.p.UserTemplate.Library != nil {
		t.Error("Ensure wrote into the template")
	}
	for name, size := range map[string]string{"direwolf-home-kim": "100Gi", "direwolf-games-kim": "1Ti"} {
		c := f.claim(t, name)
		if c.Labels[v1alpha1types.ProvisionedLabel] != "true" || c.Annotations[v1alpha1types.UsernameAnnotation] != "Kim" {
			t.Errorf("%s labels %v annotations %v", name, c.Labels, c.Annotations)
		}
		// Never owned by the User: deleting it must not delete a games volume.
		if len(c.OwnerReferences) != 0 {
			t.Errorf("%s ownerReferences = %v, want none", name, c.OwnerReferences)
		}
		if c.Spec.StorageClassName == nil || *c.Spec.StorageClassName != "local-path" ||
			!c.Spec.Resources.Requests.Storage().Equal(resource.MustParse(size)) ||
			len(c.Spec.AccessModes) != 1 || c.Spec.AccessModes[0] != corev1.ReadWriteOnce {
			t.Errorf("%s spec = %+v", name, c.Spec)
		}
	}
	// The Library starts on the generated claims.
	if err := libraryClaimsConflict(LibraryRef{Owner: "kim", HomePVC: "direwolf-home-kim", GamesPVC: "direwolf-games-kim"},
		"steam-home", "games", []v1alpha1types.User{*user}); err != nil {
		t.Errorf("libraryClaimsConflict on a provisioned User: %v", err)
	}
	// The page finds it by the raw username, and only by it.
	get := func(n string) (*v1alpha1types.User, error) {
		return f.dw.DirewolfV1alpha1().Users(libraryTestNS).Get(context.Background(), n, metav1.GetOptions{})
	}
	if u, err := v1alpha1types.LookupUser(get, "Kim"); err != nil || u.Name != "kim" {
		t.Errorf("LookupUser(Kim) = %v, %v", u, err)
	}
	if _, err := v1alpha1types.LookupUser(get, "kim"); err == nil {
		t.Error("LookupUser(kim) found Kim's User")
	}
}

func TestProvisionerEnsureStorageClassDefault(t *testing.T) {
	f := newProvisionFixture(t, nil)
	f.p.Home.StorageClassName = ""
	if _, err := f.p.Ensure(context.Background(), "kim"); err != nil {
		t.Fatal(err)
	}
	if c := f.claim(t, "direwolf-home-kim"); c.Spec.StorageClassName != nil {
		t.Errorf("storageClassName = %q, want unset (the cluster default)", *c.Spec.StorageClassName)
	}
}

// Two first requests race: both succeed, on one User and one pair of claims.
func TestProvisionerEnsureConcurrent(t *testing.T) {
	f := newProvisionFixture(t, nil)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { _, errs[i] = f.p.Ensure(context.Background(), "kim") })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	users, err := f.dw.DirewolfV1alpha1().Users(libraryTestNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(users.Items) != 1 || f.claimCount(t) != 2 {
		t.Errorf("%d users, %d claims; want 1 and 2", len(users.Items), f.claimCount(t))
	}
}

// A Create lost to a request the cache hasn't seen yet reads the winner's User.
func TestProvisionerEnsureAlreadyExists(t *testing.T) {
	f := newProvisionFixture(t, []runtime.Object{})
	won := provisionedUser("kim", "kim")
	if _, err := f.dw.DirewolfV1alpha1().Users(libraryTestNS).Create(context.Background(), won, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	user, err := f.p.Ensure(context.Background(), "kim")
	if err != nil || user.Name != "kim" {
		t.Fatalf("Ensure = %v, %v", user, err)
	}
	// Claims left by the winner (or an earlier failed attempt) are adopted.
	if _, err := f.p.Ensure(context.Background(), "kim"); err != nil {
		t.Fatal(err)
	}
}

// A User the cache hasn't seen yet is found live, before any claim is made.
func TestProvisionerEnsureStaleCache(t *testing.T) {
	f := newProvisionFixture(t, nil)
	if _, err := f.dw.DirewolfV1alpha1().Users(libraryTestNS).Create(context.Background(), libraryUser("hank", false), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	user, err := f.p.Ensure(context.Background(), "hank")
	if err != nil || user.Spec.Library != nil {
		t.Fatalf("Ensure = %+v, %v; want the hand-written User", user, err)
	}
	if n := f.claimCount(t); n != 0 {
		t.Errorf("%d claims created for an existing User", n)
	}
}

// Hand-written Users stay authoritative: returned as they are, never changed.
func TestProvisionerEnsureLeavesHandWrittenUser(t *testing.T) {
	for _, own := range []bool{false, true} {
		hand := libraryUser("sam", own)
		f := newProvisionFixture(t, nil, hand)
		user, err := f.p.Ensure(context.Background(), "sam")
		if err != nil {
			t.Fatal(err)
		}
		if (user.Spec.Library != nil) != own || user.Spec.Resources != nil || user.Labels != nil {
			t.Errorf("own=%v: Ensure returned %+v", own, user)
		}
		live, err := f.dw.DirewolfV1alpha1().Users(libraryTestNS).Get(context.Background(), "sam", metav1.GetOptions{})
		if err != nil || (live.Spec.Library != nil) != own || live.Labels != nil {
			t.Errorf("own=%v: live user %+v, %v", own, live, err)
		}
		if n := f.claimCount(t); n != 0 {
			t.Errorf("own=%v: %d claims created", own, n)
		}
	}
}

func TestProvisionerEnsureRefusesInvalidUsername(t *testing.T) {
	for _, name := range []string{"", "kim@example.com", "kim_doe", "kim.doe", "-kim", "kim-", "ki m",
		"a234567890123456789012345678901234567890x"} {
		f := newProvisionFixture(t, nil)
		if _, err := f.p.Ensure(context.Background(), name); !stderrors.Is(err, errProvisionRefused) {
			t.Errorf("Ensure(%q) = %v, want refused", name, err)
		}
		if n := f.claimCount(t); n != 0 {
			t.Errorf("Ensure(%q) created %d claims", name, n)
		}
	}
}

// Two usernames on one name: the second is refused, never handed the first's
// User or volumes.
func TestProvisionerEnsureRefusesNameCollision(t *testing.T) {
	t.Run("provisioned User", func(t *testing.T) {
		f := newProvisionFixture(t, nil, provisionedUser("kim", "Kim"))
		if _, err := f.p.Ensure(context.Background(), "kim"); !stderrors.Is(err, errProvisionRefused) {
			t.Errorf("Ensure(kim) = %v, want refused", err)
		}
	})
	t.Run("hand-written User", func(t *testing.T) {
		f := newProvisionFixture(t, nil, libraryUser("kim", true))
		if _, err := f.p.Ensure(context.Background(), "KIM"); !stderrors.Is(err, errProvisionRefused) {
			t.Errorf("Ensure(KIM) = %v, want refused", err)
		}
	})
	t.Run("claims of a racing username", func(t *testing.T) {
		f := newProvisionFixture(t, nil)
		if _, err := f.p.Ensure(context.Background(), "Kim"); err != nil {
			t.Fatal(err)
		}
		// The cache (empty here) hasn't seen Kim's User; the claims still
		// tell the usernames apart.
		if _, err := f.p.Ensure(context.Background(), "kim"); !stderrors.Is(err, errProvisionRefused) {
			t.Errorf("Ensure(kim) = %v, want refused", err)
		}
	})
	t.Run("claims left by another username", func(t *testing.T) {
		// A failed "Bob" attempt left its claims and no User.
		left := make([]runtime.Object, 0, 2)
		for _, name := range []string{"direwolf-home-bob", "direwolf-games-bob"} {
			left = append(left, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: libraryTestNS,
				Labels:      map[string]string{v1alpha1types.ProvisionedLabel: v1alpha1types.ProvisionedLabelValue},
				Annotations: map[string]string{v1alpha1types.UsernameAnnotation: "Bob"},
			}})
		}
		f := newProvisionFixture(t, left)
		if _, err := f.p.Ensure(context.Background(), "bob"); !stderrors.Is(err, errProvisionRefused) {
			t.Errorf("Ensure(bob) = %v, want refused", err)
		}
		if _, err := f.dw.DirewolfV1alpha1().Users(libraryTestNS).Get(context.Background(), "bob", metav1.GetOptions{}); err == nil {
			t.Error("bob got Bob's claims")
		}
		if _, err := f.p.Ensure(context.Background(), "Bob"); err != nil {
			t.Errorf("Ensure(Bob) = %v, want its claims adopted", err)
		}
	})
	t.Run("hand-written claim", func(t *testing.T) {
		hand := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "direwolf-games-kim", Namespace: libraryTestNS}}
		f := newProvisionFixture(t, []runtime.Object{hand})
		if _, err := f.p.Ensure(context.Background(), "kim"); !stderrors.Is(err, errProvisionRefused) {
			t.Errorf("Ensure(kim) = %v, want refused", err)
		}
		if _, err := f.dw.DirewolfV1alpha1().Users(libraryTestNS).Get(context.Background(), "kim", metav1.GetOptions{}); err == nil {
			t.Error("a User was created on someone else's claim")
		}
		// Refused before anything is created.
		if n := f.claimCount(t); n != 1 {
			t.Errorf("%d claims, want only the hand-written one", n)
		}
	})
}

// A generated claim another library already uses is refused before anything
// is created: the Library would refuse it (libraryClaimsConflict).
func TestProvisionerEnsureRefusesTakenClaims(t *testing.T) {
	t.Run("shared library's claim", func(t *testing.T) {
		f := newProvisionFixture(t, nil)
		f.p.SharedGamesPVC = "direwolf-games-kim"
		if _, err := f.p.Ensure(context.Background(), "kim"); !stderrors.Is(err, errLibraryClaims) {
			t.Errorf("Ensure = %v, want a claims conflict", err)
		}
	})
	t.Run("another User's claim", func(t *testing.T) {
		other := libraryUser("sam", true)
		other.Spec.Library.HomeClaimName = "direwolf-home-kim"
		f := newProvisionFixture(t, nil, other)
		if _, err := f.p.Ensure(context.Background(), "kim"); !stderrors.Is(err, errLibraryClaims) {
			t.Errorf("Ensure = %v, want a claims conflict", err)
		}
		if n := f.claimCount(t); n != 0 {
			t.Errorf("%d claims created", n)
		}
	})
}

func TestProvisionerInGroup(t *testing.T) {
	f := newProvisionFixture(t, nil)
	for groups, want := range map[string]bool{
		"k8x-games":                true,
		"admins|k8x-games":         true,
		"admins | k8x-games":       true,
		"admins":                   false,
		"k8x-games-old":            false,
		"":                         false,
		"admins,k8x-games":         false,
		"authentik Admins|k8x-gam": false,
	} {
		if got := f.p.InGroup(groups); got != want {
			t.Errorf("InGroup(%q) = %v, want %v", groups, got, want)
		}
	}
	f.p.Group = ""
	if f.p.InGroup("") {
		t.Error("InGroup with provisioning off")
	}
}

func TestParseProvisionUserTemplate(t *testing.T) {
	spec, err := ParseProvisionUserTemplate(`
resources:
  limits: {cpu: "8", memory: 16Gi}
volumes:
  - name: wolf-nvidia-icd
    configMap: {name: nvidia-icd}
sidecarPolicies:
  wolf:
    env: [{name: GBM_BACKENDS_PATH, value: /usr/lib/gbm}]
`)
	if err != nil || spec.Resources == nil || len(spec.Volumes) != 1 || spec.SidecarPolicies.Wolf == nil {
		t.Fatalf("YAML = %+v, %v", spec, err)
	}
	if _, err := ParseProvisionUserTemplate(`{"resources":{"limits":{"cpu":"8"}}}`); err != nil {
		t.Errorf("JSON: %v", err)
	}
	// Empty documents around the one that counts are fine.
	for _, ok := range []string{"# c\n---\nresources: {limits: {cpu: \"8\"}}\n", "---\nresources: {limits: {cpu: \"8\"}}\n---\n"} {
		if spec, err := ParseProvisionUserTemplate(ok); err != nil || spec.Resources == nil {
			t.Errorf("ParseProvisionUserTemplate(%q) = %+v, %v", ok, spec, err)
		}
	}
	if spec, err := ParseProvisionUserTemplate(""); err != nil || spec.Resources != nil {
		t.Errorf("empty = %+v, %v", spec, err)
	}
	for _, bad := range []string{
		`{"library":{"homeClaimName":"a","gamesClaimName":"b"}}`,
		`{"resource":{}}`, // typo: would silently drop the limits
		`[`,
		"resources: {}\n---\nlibrary: {homeClaimName: a, gamesClaimName: b}\n",
		"sidecarPolicies: {}\n...\nresources: {limits: {cpu: \"8\"}}\n",
		// Two documents, neither setting library: refused for being two.
		"resources: {limits: {cpu: \"8\"}}\n---\nsidecarPolicies: {}\n",
		// A "!!null" tag on a mapping does not make it empty.
		"--- !!null\nresources: {limits: {cpu: \"8\"}}\n",
		"--- !!null\nresources: {limits: {cpu: \"8\"}}\n---\nsidecarPolicies: {}\n",
		"--- !!null\nlibrary: {homeClaimName: a, gamesClaimName: b}\n",
		"resources: {}\n...\n---\nlibrary: {homeClaimName: a, gamesClaimName: b}\n",
	} {
		if _, err := ParseProvisionUserTemplate(bad); err == nil {
			t.Errorf("ParseProvisionUserTemplate(%s) accepted", bad)
		}
	}
}
