package controllers

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	sigsyaml "sigs.k8s.io/yaml"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// vrExampleFixture is a ports fixture whose App (the informer's copy, which
// the controller reads back) carries examples/vr.yaml's spec, with the
// operator flags it needs.
func vrExampleFixture(t *testing.T) (*portsFixture, *v1alpha1types.App) {
	t.Helper()
	b, err := os.ReadFile("../../examples/vr.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var vr v1alpha1types.App
	if err := sigsyaml.UnmarshalStrict(b, &vr); err != nil {
		t.Fatal(err)
	}
	f := newPortsFixture(t)
	app := withVR(t, f)
	app.Spec = vr.Spec
	return f, app
}

// examples/vr.yaml builds: in the App pod, with the VR port, and every
// stateMount on the per-User PVC under mounts/<name>, outside state/.
func TestVRExampleBuildsWithStateMounts(t *testing.T) {
	f, app := vrExampleFixture(t)
	sess := f.session("alex-1", f.user)
	if err := f.sc.allocatePorts(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	wolf, appPod, err := f.sc.buildPods(sess, testWolfPodUID)
	if err != nil {
		t.Fatalf("buildPods: %v", err)
	}
	if appPod == nil || appPod.Spec.HostNetwork {
		t.Fatal("VR example not in an App pod off the host network")
	}
	if len(app.Spec.StateMounts) != 4 {
		t.Fatalf("example has %d stateMounts, want 4", len(app.Spec.StateMounts))
	}
	v := volumeNamed(&appPod.Spec, "wolf-data")
	if v == nil || v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != f.sc.pvcName(sess) {
		t.Fatalf("App pod wolf-data = %+v, want the per-User PVC %s", v, f.sc.pvcName(sess))
	}
	for _, c := range appPod.Spec.Containers {
		if isWolfPodContainer(c.Name) || c.Name == v1alpha1types.UdevRelayContainerName || c.Name == v1alpha1types.VRAgentContainerName {
			continue
		}
		for _, m := range app.Spec.StateMounts {
			var got *corev1.VolumeMount
			for i := range c.VolumeMounts {
				if c.VolumeMounts[i].MountPath == path.Join(stateMountsRoot, m.Name) {
					got = &c.VolumeMounts[i]
				}
			}
			if got == nil || got.Name != "wolf-data" || got.SubPath != path.Join("mounts", m.Name) || got.ReadOnly {
				t.Errorf("container %s stateMount %s = %+v, want wolf-data subPath mounts/%s", c.Name, m.Name, got, m.Name)
			}
		}
	}
	// None reaches the Wolf pod: its containers mount wolf-data whole.
	for _, c := range wolf.Spec.Containers {
		for _, m := range c.VolumeMounts {
			if strings.HasPrefix(m.SubPath, "mounts/") {
				t.Errorf("Wolf pod container %s mounts stateMount %s", c.Name, m.SubPath)
			}
		}
	}
}

// The example stays within the App confinement rules (vr.md): no NET_ADMIN,
// SYS_ADMIN, privilege, hostPath, host namespaces or operator volumes.
func TestVRExampleAsksForNothingForbidden(t *testing.T) {
	_, app := vrExampleFixture(t)
	spec := app.Spec.Template.Spec
	if spec.HostNetwork || spec.HostPID || spec.HostIPC || (spec.ShareProcessNamespace != nil && *spec.ShareProcessNamespace) {
		t.Error("example asks for a host namespace or a shared process namespace")
	}
	for _, v := range spec.Volumes {
		if v.HostPath != nil {
			t.Errorf("volume %s is a hostPath", v.Name)
		}
	}
	for _, c := range spec.Containers {
		if sc := c.SecurityContext; sc != nil {
			if sc.Privileged != nil && *sc.Privileged {
				t.Errorf("container %s is privileged", c.Name)
			}
			if sc.Capabilities != nil {
				for _, capName := range sc.Capabilities.Add {
					if capName == "NET_ADMIN" || capName == "SYS_ADMIN" {
						t.Errorf("container %s adds %s", c.Name, capName)
					}
				}
			}
		}
	}
	if err := validateNoOperatorVolumeMounts(&spec); err != nil {
		t.Error(err)
	}
}

// No other mount may sit at, above or under the stateMounts' root: links in
// that volume would redirect them (a game's link to /etc in the shared home,
// XERK-2713), or links in the User's own directories would redirect it.
func TestStateMountRootCollisionIsInvalidSpec(t *testing.T) {
	for _, p := range []string{"/srv", "srv/direwolf", "/srv/direwolf/", "/srv/direwolf/state", "/srv/direwolf/state/wivrn", "/srv/direwolf/state/wivrn/keys", "/srv/direwolf/state/other"} {
		f, app := vrExampleFixture(t)
		c := &app.Spec.Template.Spec.Containers[0]
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "games", MountPath: p})
		sess := f.session("alex-1", f.user)
		if err := f.sc.allocatePorts(context.Background(), sess); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.sc.buildPods(sess, testWolfPodUID); !errors.Is(err, errInvalidSpec) || !strings.Contains(err.Error(), stateMountsRoot) && !strings.Contains(err.Error(), "mounts two volumes") {
			t.Errorf("App mount at %s: buildPods err = %v, want errInvalidSpec about %s", p, err, stateMountsRoot)
		}
	}
	// Beside the root is fine, as is any of it in an App without stateMounts.
	f, app := vrExampleFixture(t)
	c := &app.Spec.Template.Spec.Containers[0]
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "games", MountPath: "/srv/other"})
	sess := f.session("alex-1", f.user)
	if err := f.sc.allocatePorts(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.sc.buildPods(sess, testWolfPodUID); err != nil {
		t.Errorf("App mount at /srv/other: buildPods err = %v", err)
	}
	f, app = vrExampleFixture(t)
	app.Spec.StateMounts = nil
	c = &app.Spec.Template.Spec.Containers[0]
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "games", MountPath: "/srv"})
	sess = f.session("alex-1", f.user)
	if err := f.sc.allocatePorts(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.sc.buildPods(sess, testWolfPodUID); err != nil {
		t.Errorf("App without stateMounts mounting /srv: buildPods err = %v", err)
	}
}

// vrExampleSeedScript is the Python the example's command runs as PUID to
// seed and set the per-User AC config.
func vrExampleSeedScript(t *testing.T) string {
	t.Helper()
	_, app := vrExampleFixture(t)
	cmd := strings.Join(app.Spec.Template.Spec.Containers[0].Command, "\n")
	_, rest, ok := strings.Cut(cmd, "<<'PY'\n")
	script, _, ok2 := strings.Cut(rest, "\nPY\n")
	if !ok || !ok2 {
		t.Fatal("no PY heredoc in examples/vr.yaml's command")
	}
	return script
}

func runSeed(t *testing.T, script, shared, cfg string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "python3", "-I", "-", shared, cfg)
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed: %v: %s", err, out)
	}
}

// The VR copy of AC's cfg starts as the shared one, in VR mode and windowed,
// with every other line kept; the shared (flat) one is never touched.
func TestVRExampleSeedsACConfigInVRMode(t *testing.T) {
	script := vrExampleSeedScript(t)
	dir := t.TempDir()
	shared, cfg := filepath.Join(dir, "shared", "cfg"), filepath.Join(dir, "user", "cfg")
	if err := os.MkdirAll(filepath.Join(shared, "extension"), 0o755); err != nil {
		t.Fatal(err)
	}
	flat := "[VIDEO]\r\nWIDTH=1920\r\nFULLSCREEN=1\r\n\r\n[CAMERA]\r\nMODE=DEFAULT\r\n[ASSETTO]\r\nX=1\r\n"
	for name, body := range map[string]string{"video.ini": flat, "controls.ini": "[A]\r\nB=1\r\n", "extension/csp.ini": "c"} {
		if err := os.WriteFile(filepath.Join(shared, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	want := "[VIDEO]\r\nWIDTH=1920\r\nFULLSCREEN=0\r\n\r\n[CAMERA]\r\nMODE=OPENVR\r\n[ASSETTO]\r\nX=1\r\n"
	for range 2 { // and again: a second start changes nothing
		runSeed(t, script, shared, cfg)
		if got, _ := os.ReadFile(filepath.Join(cfg, "video.ini")); string(got) != want {
			t.Errorf("VR video.ini = %q, want %q", got, want)
		}
	}
	for name, body := range map[string]string{"controls.ini": "[A]\r\nB=1\r\n", "extension/csp.ini": "c"} {
		if got, _ := os.ReadFile(filepath.Join(cfg, name)); string(got) != body {
			t.Errorf("VR %s = %q, want the shared copy", name, got)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(shared, "video.ini")); string(got) != flat {
		t.Errorf("shared video.ini changed to %q", got)
	}
	// Once seeded, the User's own copy is kept: the shared one is not re-copied.
	if err := os.WriteFile(filepath.Join(cfg, "controls.ini"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	runSeed(t, script, shared, cfg)
	if got, _ := os.ReadFile(filepath.Join(cfg, "controls.ini")); string(got) != "mine" {
		t.Errorf("User's controls.ini replaced: %q", got)
	}
}

// With no shared cfg (AC never configured flat) the VR copy still gets both keys.
func TestVRExampleSeedsACConfigFromNothing(t *testing.T) {
	script := vrExampleSeedScript(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "cfg")
	runSeed(t, script, filepath.Join(dir, "missing"), cfg)
	got, _ := os.ReadFile(filepath.Join(cfg, "video.ini"))
	if want := "[VIDEO]\r\nFULLSCREEN=0\r\n[CAMERA]\r\nMODE=OPENVR\r\n"; string(got) != want {
		t.Errorf("video.ini = %q, want %q", got, want)
	}
}

// The seed copies no link out of the shared prefix and follows none in the
// User's cfg; odd encodings neither end the session nor duplicate sections.
func TestVRExampleSeedHandlesLinksAndEncodings(t *testing.T) {
	script := vrExampleSeedScript(t)
	dir := t.TempDir()
	shared, cfg := filepath.Join(dir, "shared", "cfg"), filepath.Join(dir, "user", "cfg")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "evil"), filepath.Join(shared, "extension")); err != nil {
		t.Fatal(err)
	}
	// A BOM and a Latin-1 byte.
	if err := os.WriteFile(filepath.Join(shared, "video.ini"), []byte("\ufeff[VIDEO]\r\nFULLSCREEN=1\r\nNAME=caf\xe9\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	runSeed(t, script, shared, cfg)
	if _, err := os.Lstat(filepath.Join(cfg, "extension")); !os.IsNotExist(err) {
		t.Errorf("shared link copied into the User's cfg: %v", err)
	}
	want := "[VIDEO]\r\nFULLSCREEN=0\r\nNAME=caf\xe9\r\n[CAMERA]\r\nMODE=OPENVR\r\n"
	if got, _ := os.ReadFile(filepath.Join(cfg, "video.ini")); string(got) != want {
		t.Errorf("video.ini = %q, want %q", got, want)
	}
	// A linked video.ini (or temp file) is replaced, its target left alone.
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"video.ini", "video.ini.direwolf-tmp"} {
		_ = os.Remove(filepath.Join(cfg, name))
		if err := os.Symlink(target, filepath.Join(cfg, name)); err != nil {
			t.Fatal(err)
		}
	}
	runSeed(t, script, shared, cfg)
	if got, _ := os.ReadFile(target); string(got) != "x" {
		t.Errorf("link target written: %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(cfg, "video.ini")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("video.ini not replaced by a file: %v %v", fi, err)
	}
}

// vrExampleLinkScript is the Python the example's command runs as PUID to
// turn AC's Documents folder in the shared prefix into a link to the User's
// mount (XERK-2713).
func vrExampleLinkScript(t *testing.T) string {
	t.Helper()
	_, app := vrExampleFixture(t)
	cmd := strings.Join(app.Spec.Template.Spec.Containers[0].Command, "\n")
	_, rest, ok := strings.Cut(cmd, "<<'LINK'\n")
	script, _, ok2 := strings.Cut(rest, "\nLINK\n")
	if !ok || !ok2 {
		t.Fatal("no LINK heredoc in examples/vr.yaml's command")
	}
	if !strings.Contains(cmd, `"$docs" `+linkTarget+` <<'LINK'`) {
		t.Fatalf("examples/vr.yaml no longer links AC's Documents to %s", linkTarget)
	}
	return script
}

// linkTarget is where the example links AC's Documents: the image links it on.
const linkTarget = "/srv/direwolf/ac-documents"

func runLink(t *testing.T, script, docs string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "python3", "-I", "-", docs, linkTarget)
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The shared folder moves to "Assetto Corsa.flat" (contents kept) and a link
// to the User's mount takes its place; a planted link is replaced, never
// followed; what the script can't place safely ends the session.
func TestVRExampleLinksACDocumentsToState(t *testing.T) {
	script := vrExampleLinkScript(t)
	dir := t.TempDir()
	docs, target := filepath.Join(dir, "Documents", "Assetto Corsa"), linkTarget
	if err := os.MkdirAll(filepath.Join(docs, "cfg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "cfg", "video.ini"), []byte("flat"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 { // and again, with the link in place
		if out, err := runLink(t, script, docs); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if got, err := os.Readlink(docs); err != nil || got != target {
			t.Fatalf("Documents/Assetto Corsa -> %q (%v), want %s", got, err, target)
		}
		if got, _ := os.ReadFile(filepath.Join(docs+".flat", "cfg", "video.ini")); string(got) != "flat" {
			t.Errorf("flat video.ini = %q, want the shared copy kept", got)
		}
	}
	// A game's link elsewhere is replaced, its target untouched.
	if err := os.Remove(docs); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", docs); err != nil {
		t.Fatal(err)
	}
	if out, err := runLink(t, script, docs); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got, _ := os.Readlink(docs); got != target {
		t.Errorf("planted link not replaced: -> %q", got)
	}
	// No prefix (ac-setup makes it first): no Documents folder, no link.
	if out, err := runLink(t, script, filepath.Join(dir, "missing", "Assetto Corsa")); err == nil || !strings.Contains(out, "in Assetto Corsa's prefix") {
		t.Errorf("missing prefix: err %v, output %q; want a refusal", err, out)
	}
	// A folder and a flat copy both: neither is the script's to drop.
	both := filepath.Join(dir, "both", "Assetto Corsa")
	for _, d := range []string{both, both + ".flat"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := runLink(t, script, both); err == nil || !strings.Contains(out, "move one aside") {
		t.Errorf("both folders: err %v, output %q; want a refusal", err, out)
	}
	// No flat copy yet (AC never wrote one): an empty one is made for flat sessions.
	fresh := filepath.Join(dir, "fresh", "Assetto Corsa")
	if err := os.MkdirAll(filepath.Dir(fresh), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runLink(t, script, fresh); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if fi, err := os.Lstat(fresh + ".flat"); err != nil || !fi.IsDir() {
		t.Errorf("no flat folder made: %v %v", fi, err)
	}
}

// AC's paths agree across the example and the steam image (XERK-2666): the
// command reads the shared prefix's flat Documents through a read-only mount
// of the home it is in, and the steam image's flat link names the same
// folder. A path changed in one place alone leaves the VR copy seeded from
// nothing, or flat sessions with a dangling Documents link, and no other test
// fails.
func TestVRExampleACPathsAgreeWithSteamImage(t *testing.T) {
	_, app := vrExampleFixture(t)
	c := app.Spec.Template.Spec.Containers[0]
	cmd := strings.Join(c.Command, "\n")
	var apps, docs string
	for line := range strings.SplitSeq(cmd, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "apps="); ok {
			apps = v
		}
		if v, ok := strings.CutPrefix(line, "docs="); ok {
			docs = strings.ReplaceAll(strings.Replace(v, "$apps", apps, 1), `\ `, " ")
		}
	}
	const home, seed = "/home/retro", "/run/direwolf-vr/home"
	if apps == "" || !strings.HasPrefix(docs, home+"/") {
		t.Fatalf("apps=%q docs=%q; want AC's Documents in the home at %s", apps, docs, home)
	}
	if !strings.Contains(cmd, `"`+seed+`/${docs#`+home+`/}.flat/cfg"`) {
		t.Errorf("the seed no longer reads %s/<docs below the home>.flat/cfg", seed)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = m
	}
	if m, ok := mounts[home]; !ok || m.ReadOnly {
		t.Errorf("the home is not mounted read-write at %s: %+v", home, m)
	}
	if m, ok := mounts[seed]; !ok || !m.ReadOnly || m.Name != mounts[home].Name || m.SubPath != "" {
		t.Errorf("%s is not a read-only whole mount of the home's volume: %+v", seed, m)
	}
	b, err := os.ReadFile("../../images/gow-nvidia32/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "ln -s '"+docs+".flat' \\\n      /srv/direwolf/ac-documents") {
		t.Errorf("the steam image's /srv/direwolf/ac-documents link does not lead to %s.flat", docs)
	}
}

// The example runs the image's Assetto Corsa setup (XERK-2726) as PUID, after
// the stateMount checks and before the Documents link and the entrypoint, at
// a CSP version the image pins.
func TestVRExampleRunsACSetup(t *testing.T) {
	_, app := vrExampleFixture(t)
	c := app.Spec.Template.Spec.Containers[0]
	cmd := strings.Join(c.Command, "\n")
	setup := strings.Index(cmd, `gosu "$PUID:$PGID" /opt/direwolf/vr/ac-setup`+"\n")
	if setup < 0 || setup < strings.Index(cmd, "mountpoint -q") ||
		setup > strings.Index(cmd, "<<'LINK'") || setup > strings.Index(cmd, "exec /entrypoint.sh") {
		t.Errorf("the command does not run ac-setup as PUID between the stateMount checks and the Documents link")
	}
	b, err := os.ReadFile("../../images/gow-nvidia32/vr/ac-csp-versions.json")
	if err != nil {
		t.Fatal(err)
	}
	var pins map[string]any
	if err := sigsyaml.Unmarshal(b, &pins); err != nil {
		t.Fatal(err)
	}
	var csp string
	for _, e := range c.Env {
		if e.Name == "DIREWOLF_AC_CSP" {
			csp = e.Value
		}
	}
	if _, ok := pins[csp]; !ok {
		t.Errorf("DIREWOLF_AC_CSP=%q is not a version the image pins", csp)
	}
}
