package controllers

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// A Session whose App does not exist (deleted, or a typo in a hand-made
// Session) ends with a Warning Event instead of retrying forever (XERK-2191).
func TestMissingAppEndsSession(t *testing.T) {
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := f.session("alex-1", f.user)
		s.CreationTimestamp = metav1.Now() // not yet reaped as unstarted
		s.Spec.GameReference.Name = "missing"
		return s
	}, nil)
	recorder := record.NewFakeRecorder(10)
	f.sc.Recorder = recorder
	if err := f.sc.Reconcile(portsTestNS, sess.Name, sess); err != nil {
		t.Fatalf("Reconcile = %v, want the Session ended", err)
	}
	if f.sessionExists(t, sess.Name) {
		t.Error("a Session whose App does not exist was kept")
	}
	if len(recorder.Events) != 1 || !strings.HasPrefix(<-recorder.Events, "Warning AppNotFound ") {
		t.Error("no Warning AppNotFound Event for the ended Session")
	}
}

// An App the API server has but our cache has not seen yet keeps the Session,
// as does an API server error confirming the App is gone.
func TestMissingAppConfirmsWithAPIServer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		create  bool  // the App exists on the API server, not yet in the cache
		getErr  error // from the live App GET
		wantErr bool
	}{
		{"app not yet cached", true, nil, false},
		{"api error", false, apierrors.NewServiceUnavailable("apiserver restarting"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPortsFixture(t)
			s := f.session("alex-1", f.user)
			s.Spec.GameReference.Name = "fresh"
			if tc.create {
				app := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: portsTestNS}}
				f.dw.PrependReactor("get", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, app, nil
				})
			}
			if tc.getErr != nil {
				f.dw.PrependReactor("get", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tc.getErr
				})
			}
			reason, err := f.sc.appGoneReason(context.Background(), s)
			if (err != nil) != tc.wantErr {
				t.Errorf("appGoneReason err = %v, want error=%v", err, tc.wantErr)
			}
			if reason != "" {
				t.Errorf("ended a Session whose App may exist: %q", reason)
			}
		})
	}
}

// A cached App needs no API call.
func TestAppGoneReasonKeepsCachedApp(t *testing.T) {
	f := newPortsFixture(t)
	reason, err := f.sc.appGoneReason(context.Background(), f.session("alex-1", f.user))
	if err != nil || reason != "" {
		t.Errorf("appGoneReason = %q, %v for a cached App", reason, err)
	}
}
