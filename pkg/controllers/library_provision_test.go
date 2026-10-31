package controllers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	dwclient "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned"
)

// The Library page provisions a group member's User on first sign-in
// (XERK-2540); everyone else keeps the 403.

const testProvisionGroup = "k8x-games"

// provisioningServer is f's page with provisioning on, sharing its User cache.
func (f *libraryFixture) provisioningServer(t *testing.T, pods ...*corev1.Pod) *LibraryServer {
	t.Helper()
	s := f.server(t, pods...)
	s.Provisioner = NewProvisioner(libraryTestNS, s.users,
		f.dw.DirewolfV1alpha1().Users(libraryTestNS),
		f.k8s.CoreV1().PersistentVolumeClaims(libraryTestNS),
		&ProvisionerOptions{
			Group: testProvisionGroup, GroupsHeader: DefaultLibraryGroupsHeader, GroupsSeparator: DefaultLibraryGroupsSeparator,
			Home:          ClaimTemplate{Size: resource.MustParse("100Gi")},
			Games:         ClaimTemplate{Size: resource.MustParse("1Ti")},
			SharedHomePVC: f.library.HomePVC, SharedGamesPVC: f.library.GamesPVC,
		})
	return s
}

// provisionWrites counts User and PVC writes (anything but get/list/watch).
func (f *libraryFixture) provisionWrites() int {
	n := 0
	for _, a := range append(f.k8s.Actions(), f.dw.Actions()...) {
		switch a.GetVerb() {
		case "get", "list", "watch":
			continue
		}
		if r := a.GetResource().Resource; r == "users" || r == "persistentvolumeclaims" {
			n++
		}
	}
	return n
}

func (f *libraryFixture) podNames(t *testing.T) []string {
	t.Helper()
	pods, err := f.k8s.CoreV1().Pods(libraryTestNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(pods.Items))
	for _, p := range pods.Items {
		names = append(names, p.Name)
	}
	return names
}

func signIn(username, groups string, visit bool) http.Header {
	h := http.Header{DefaultLibraryUserHeader: {username}, DefaultLibraryGroupsHeader: {groups}}
	if visit {
		h.Set("Sec-Fetch-User", "?1")
	}
	return h
}

func TestLibraryServerProvisionsMember(t *testing.T) {
	f := newLibraryFixture(t, nil)
	s := f.provisioningServer(t)
	rec := serveLibrary(s, "10.1.0.5:5555", signIn("Kim", "staff|"+testProvisionGroup, true))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Setting up your account") ||
		!strings.Contains(rec.Body.String(), `http-equiv="refresh"`) {
		t.Fatalf("HTTP %d %q, want the refreshing setup page", rec.Code, rec.Body.String())
	}
	user, err := f.dw.DirewolfV1alpha1().Users(libraryTestNS).Get(context.Background(), "kim", metav1.GetOptions{})
	if err != nil || !user.IsFor("Kim") || user.Spec.Library == nil {
		t.Fatalf("user kim = %+v, err %v; want one provisioned for Kim", user, err)
	}
	for _, claim := range []string{"direwolf-home-kim", "direwolf-games-kim"} {
		if _, getErr := f.k8s.CoreV1().PersistentVolumeClaims(libraryTestNS).Get(context.Background(), claim, metav1.GetOptions{}); getErr != nil {
			t.Errorf("claim %s: %v", claim, getErr)
		}
	}
	if got := f.podNames(t); len(got) != 1 || got[0] != "direwolf-library-kim" {
		t.Errorf("pods = %v, want only direwolf-library-kim", got)
	}

	// The setup page's refresh, before the User reaches the cache: same page,
	// nothing more written.
	writes := f.provisionWrites()
	rec = serveLibrary(s, "10.1.0.5:5555", signIn("Kim", testProvisionGroup, false))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Setting up your account") {
		t.Errorf("refresh: HTTP %d %q, want the setup page", rec.Code, rec.Body.String())
	}
	if got := f.provisionWrites(); got != writes {
		t.Errorf("refresh made %d more User/PVC writes, want none", got-writes)
	}

	// Once cached, the normal starting page.
	pod, err := f.k8s.CoreV1().Pods(libraryTestNS).Get(context.Background(), "direwolf-library-kim", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rec = serveLibrary(f.provisioningServer(t, pod), "10.1.0.5:5555", signIn("Kim", testProvisionGroup, false))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Starting the Library") {
		t.Errorf("cached: HTTP %d %q, want the starting page", rec.Code, rec.Body.String())
	}
	if got := f.provisionWrites(); got != writes {
		t.Errorf("cached visit made %d more User/PVC writes, want none", got-writes)
	}
}

// Without the group, an unpaired name keeps the 403 and nothing is created,
// whatever else the request carries.
func TestLibraryServerProvisionRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, peer string
		header     http.Header
		off        bool
	}{
		{name: "non-member", peer: "10.1.0.5:5555", header: signIn("Kim", "staff|k8x-games-admins", true)},
		{name: "no groups header", peer: "10.1.0.5:5555", header: http.Header{"Sec-Fetch-User": {"?1"}, DefaultLibraryUserHeader: {"Kim"}}},
		{name: "untrusted peer", peer: "10.2.0.5:5555", header: signIn("Kim", testProvisionGroup, true)},
		{name: "no proxy secret", peer: "10.1.0.5:5555", header: func() http.Header {
			h := signIn("Kim", testProvisionGroup, true)
			h.Set(LibraryProxySecretHeader, "")
			return h
		}()},
		{name: "provisioning off", peer: "10.1.0.5:5555", header: signIn("Kim", testProvisionGroup, true), off: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLibraryFixture(t, nil)
			s := f.provisioningServer(t)
			if tc.off {
				s.Provisioner = nil
			}
			rec := serveLibrary(s, tc.peer, tc.header)
			if rec.Code != http.StatusForbidden {
				t.Errorf("HTTP %d, want 403", rec.Code)
			}
			if n := f.provisionWrites(); n != 0 || len(f.podNames(t)) != 0 {
				t.Errorf("%d User/PVC writes, pods %v; want none", n, f.podNames(t))
			}
		})
	}
}

// A User already present takes the normal path: no User or PVC writes.
func TestLibraryServerAlreadyProvisioned(t *testing.T) {
	f := newLibraryFixture(t, nil, provisionedUser("kim", "Kim"))
	f.dw.ClearActions()
	rec := serveLibrary(f.provisioningServer(t), "10.1.0.5:5555", signIn("Kim", testProvisionGroup, true))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Starting the Library") {
		t.Errorf("HTTP %d %q, want the starting page", rec.Code, rec.Body.String())
	}
	if n := f.provisionWrites(); n != 0 {
		t.Errorf("%d User/PVC writes, want none", n)
	}
}

func TestLibraryServerProvisionErrors(t *testing.T) {
	t.Run("refused username", func(t *testing.T) {
		f := newLibraryFixture(t, nil)
		rec := serveLibrary(f.provisioningServer(t), "10.1.0.5:5555", signIn("first last", testProvisionGroup, true))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "can&#39;t be used") {
			t.Errorf("HTTP %d %q, want the refused page", rec.Code, rec.Body.String())
		}
		if n := f.provisionWrites(); n != 0 || len(f.podNames(t)) != 0 {
			t.Errorf("%d writes, pods %v; want none", n, f.podNames(t))
		}
	})
	t.Run("API failure", func(t *testing.T) {
		f := newLibraryFixture(t, nil)
		f.k8s.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("etcd unavailable")
		})
		rec := serveLibrary(f.provisioningServer(t), "10.1.0.5:5555", signIn("Kim", testProvisionGroup, true))
		body := rec.Body.String()
		if rec.Code != http.StatusInternalServerError || !strings.Contains(body, "Setting up your account failed") ||
			!strings.Contains(body, `http-equiv="refresh"`) || strings.Contains(body, "etcd") {
			t.Errorf("HTTP %d %q, want a refreshing error page without the cause", rec.Code, body)
		}
		if _, err := f.dw.DirewolfV1alpha1().Users(libraryTestNS).Get(context.Background(), "kim", metav1.GetOptions{}); err == nil {
			t.Error("User created over failed claims")
		}
		if got := f.podNames(t); len(got) != 0 {
			t.Errorf("pods = %v, want none", got)
		}
	})
}

// A request the page refuses anyway never provisions first: the start path
// takes only a same-site POST.
func TestLibraryServerStartCheckBeforeProvisioning(t *testing.T) {
	for name, tc := range map[string]struct {
		method string
		site   string
	}{
		"GET":             {method: http.MethodGet},
		"cross-site POST": {method: http.MethodPost, site: "cross-site"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLibraryFixture(t, nil)
			h := signIn("Kim", testProvisionGroup, true)
			if tc.site != "" {
				h.Set("Sec-Fetch-Site", tc.site)
			}
			rec := serveLibraryRequest(f.provisioningServer(t), tc.method, libraryStartPath, "10.1.0.5:5555", h)
			if rec.Code != http.StatusForbidden || f.provisionWrites() != 0 || len(f.podNames(t)) != 0 {
				t.Errorf("HTTP %d, %d writes, pods %v; want 403 and nothing", rec.Code, f.provisionWrites(), f.podNames(t))
			}
		})
	}
}

// A tab's script reload (no user gesture) never creates a User: an admin's
// delete must stick while the member's tab is open. The stopped page's Start
// button (a same-site POST) does provision.
func TestLibraryServerReloadDoesNotProvision(t *testing.T) {
	f := newLibraryFixture(t, nil)
	s := f.provisioningServer(t)
	rec := serveLibrary(s, "10.1.0.5:5555", signIn("Kim", testProvisionGroup, false))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Start the Library") {
		t.Errorf("HTTP %d %q, want the stopped page", rec.Code, rec.Body.String())
	}
	if n := f.provisionWrites(); n != 0 || len(f.podNames(t)) != 0 {
		t.Fatalf("%d User/PVC writes, pods %v; want none", n, f.podNames(t))
	}
	rec = serveLibraryRequest(s, http.MethodPost, libraryStartPath, "10.1.0.5:5555", signIn("Kim", testProvisionGroup, false))
	if rec.Code != http.StatusSeeOther {
		t.Errorf("Start: HTTP %d, want 303", rec.Code)
	}
	if got := f.podNames(t); len(got) != 1 || got[0] != "direwolf-library-kim" {
		t.Errorf("Start: pods = %v, want direwolf-library-kim", got)
	}
}

// Only a provisioned User skips the user-gesture rule while uncached: a
// hand-written one the cache hasn't seen yet gets the ordinary stopped page,
// not a started shared Library.
func TestLibraryServerUncachedHandWrittenUserNeedsGesture(t *testing.T) {
	f := newLibraryFixture(t, nil)
	s := f.provisioningServer(t) // cache built before the User exists
	if _, err := f.dw.DirewolfV1alpha1().Users(libraryTestNS).Create(context.Background(), libraryUser("kim", false), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rec := serveLibrary(s, "10.1.0.5:5555", signIn("kim", testProvisionGroup, false))
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "Setting up") || len(f.podNames(t)) != 0 {
		t.Errorf("HTTP %d %q, pods %v; want the stopped page and no pod", rec.Code, rec.Body.String(), f.podNames(t))
	}
}

// A hung API server costs one bounded request and a refreshing error page.
func TestLibraryServerProvisionTimeout(t *testing.T) {
	// Gives up after 10s itself, so a lost bound fails the took check below
	// rather than hanging the suite.
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer hung.Close()
	client, err := dwclient.NewForConfig(&rest.Config{Host: hung.URL})
	if err != nil {
		t.Fatal(err)
	}
	old := libraryProvisionTimeout
	libraryProvisionTimeout = 200 * time.Millisecond
	defer func() { libraryProvisionTimeout = old }()

	f := newLibraryFixture(t, nil)
	s := f.provisioningServer(t)
	s.Provisioner.userAPI = client.DirewolfV1alpha1().Users(libraryTestNS)
	begin := time.Now()
	rec := serveLibrary(s, "10.1.0.5:5555", signIn("Kim", testProvisionGroup, true))
	if took := time.Since(begin); took > 5*time.Second {
		t.Errorf("request took %v, want about the provisioning timeout", took)
	}
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `http-equiv="refresh"`) ||
		strings.Contains(rec.Body.String(), hung.URL) {
		t.Errorf("HTTP %d %q, want a refreshing error page without the cause", rec.Code, rec.Body.String())
	}
}

// An allowlisted user with no groups at all is provisioned (XERK-2542), as is
// a member listed on a later groups header line (oauth2-proxy sends one per
// group); the header names are the server's configured ones.
func TestLibraryServerProvisionsAllowlistedAndMultiLine(t *testing.T) {
	for name, tc := range map[string]struct {
		username string
		header   http.Header
		want     string // User created, or "" for a refusal
	}{
		"allowlisted, no groups": {"Kim", http.Header{"Remote-User": {"Kim"}}, "kim"},
		"allowlist is exact":     {"KIM", http.Header{"Remote-User": {"KIM"}}, ""},
		"member on a later line": {"Sam", http.Header{"Remote-User": {"Sam"}, "Remote-Groups": {"staff", testProvisionGroup}}, "sam"},
		"neither":                {"Lee", http.Header{"Remote-User": {"Lee"}, "Remote-Groups": {"staff"}}, ""},
		"default header ignored": {"Kim", http.Header{DefaultLibraryUserHeader: {"Kim"}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLibraryFixture(t, nil)
			s := f.provisioningServer(t)
			s.userHeader = "Remote-User"
			s.Provisioner.GroupsHeader, s.Provisioner.GroupsSeparator = "Remote-Groups", ","
			s.Provisioner.AllowedUsers = []string{"Kim"}
			tc.header.Set("Sec-Fetch-User", "?1")
			rec := serveLibrary(s, "10.1.0.5:5555", tc.header)
			users, err := f.dw.DirewolfV1alpha1().Users(libraryTestNS).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(users.Items) != 0 {
					t.Errorf("HTTP %d, Users %v; want none created", rec.Code, users.Items)
				}
				return
			}
			if len(users.Items) != 1 || users.Items[0].Name != tc.want || !users.Items[0].IsFor(tc.username) {
				t.Errorf("HTTP %d, Users %v; want only %s", rec.Code, users.Items, tc.want)
			}
		})
	}
}
