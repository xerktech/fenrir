package controllers

import (
	"context"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

func TestCleanHeadsetName(t *testing.T) {
	got := CleanHeadsetName("Quest\u202e\u2028\n3\x00\xff" + strings.Repeat("é", 200))
	// Cut to the CRD's 128 characters, not bytes (XERK-2747).
	if !strings.HasPrefix(got, "Quest3é") || utf8.RuneCountInString(got) != 128 || !utf8.ValidString(got) {
		t.Errorf("CleanHeadsetName = %q (%d characters)", got, utf8.RuneCountInString(got))
	}
}

func TestIsWiVRnKey(t *testing.T) {
	for key, want := range map[string]bool{
		testHeadsetKeyA: true, "MEIw": true, "MEI=": true, "": false, "AAA": false, "AAAAA": false,
		strings.Repeat("A", 1024): true, strings.Repeat("A", 1028): false, "AA=A": false,
	} {
		if got := IsWiVRnKey(key); got != want {
			t.Errorf("IsWiVRnKey(%.20q) = %v, want %v", key, got, want)
		}
	}
}

// A Moonlight Session's split App pod of a VR App runs no vr-agent and gets
// no bus dir (XERK-2811): it was there only to follow PIN pairings, which
// the image no longer opens.
func TestSplitVRAppPodRunsNoVRAgent(t *testing.T) {
	f := newPortsFixture(t)
	withVR(t, f)
	sess := f.session("alex-1", f.user)
	if err := f.sc.allocatePorts(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	wolf, app, err := f.sc.buildPods(sess, testWolfPodUID)
	if err != nil {
		t.Fatal(err)
	}
	for _, pod := range [][]string{containerNames(&wolf.Spec), containerNames(&app.Spec)} {
		if slices.Contains(pod, v1alpha1types.VRAgentContainerName) {
			t.Errorf("containers %v run vr-agent", pod)
		}
	}
	for _, v := range []string{vrBusVolume, vrAgentOwnTokenVolume, vrAgentTokenVolume} {
		if volumeNamed(&app.Spec, v) != nil {
			t.Errorf("App pod has volume %s", v)
		}
	}
	// Native mode keeps its readiness gate.
	if c := f.sc.vrAgentContainer(vrAgentUID, vrAgentUID); c.ReadinessProbe == nil {
		t.Error("native vr-agent lost its readiness probe")
	}
}
