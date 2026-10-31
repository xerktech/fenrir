package controllers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
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
