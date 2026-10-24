package library

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const xodusNexus = "root/usr/bin/xodus-nexus"

// The Library and the nexus session image must start Nexus the same way: the
// keyring they share, and where games install, are set up by this script.
func TestXodusNexusWrapperMatchesSessionImage(t *testing.T) {
	library, err := os.ReadFile(xodusNexus)
	if err != nil {
		t.Fatal(err)
	}
	session, err := os.ReadFile("../gow-nvidia32/root/usr/bin/xodus-nexus")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(library, session) {
		t.Error("images/library and images/gow-nvidia32 ship different xodus-nexus wrappers")
	}
}

// runXodusNexus runs the wrapper with Nexus, the session bus check, the
// keyring daemon and bwrap stubbed, and returns the stubs' log.
func runXodusNexus(t *testing.T, home, games string, secretsOwned bool, args ...string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	writeStub(t, filepath.Join(dir, "AppRun"), `echo "nexus $*" >>"$LOG"; echo "keyring ${GNOME_KEYRING_CONTROL:-unset}" >>"$LOG"`)
	owned := "false"
	if secretsOwned {
		owned = "true"
	}
	writeStub(t, filepath.Join(dir, "dbus-send"), `echo "   boolean `+owned+`"`)
	writeStub(t, filepath.Join(dir, "gnome-keyring-daemon"), `echo "gnome-keyring-daemon $*" >>"$LOG"; echo "GNOME_KEYRING_CONTROL=/run/keyring"`)
	writeStub(t, filepath.Join(dir, "bwrap"), `exit 1`)
	src, err := os.ReadFile(xodusNexus)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.ReplaceAll(string(src), "/opt/xodus-nexus/AppRun", filepath.Join(dir, "AppRun"))
	if patched == string(src) {
		t.Fatal("xodus-nexus no longer runs /opt/xodus-nexus/AppRun")
	}
	script := filepath.Join(dir, "xodus-nexus")
	if err := os.WriteFile(script, []byte(patched), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = []string{
		"PATH=" + dir + ":/usr/bin:/bin",
		"HOME=" + home,
		"LOG=" + log,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/nonexistent",
		"XODUS_NEXUS_GAMES=" + games,
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("xodus-nexus: %v\n%s", err, out)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A first start unlocks the keyring before Nexus runs, links the install
// directory to the games volume, and passes --launch-game through.
func TestXodusNexusStartsKeyringAndLinksGames(t *testing.T) {
	home, games := t.TempDir(), filepath.Join(t.TempDir(), "xodus-nexus")
	if err := os.Mkdir(games, 0o755); err != nil {
		t.Fatal(err)
	}
	log := runXodusNexus(t, home, games, false, "--launch-game", "3f2a9c1e7b4d5a60")
	for _, want := range []string{
		"gnome-keyring-daemon --unlock --components=secrets",
		"gnome-keyring-daemon --start --components=secrets",
		"nexus --no-sandbox --launch-game 3f2a9c1e7b4d5a60",
		"keyring /run/keyring",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if target, err := os.Readlink(filepath.Join(home, "Games", "Xodus Nexus")); err != nil || target != games {
		t.Errorf("~/Games/Xodus Nexus -> %q (%v), want %q", target, err, games)
	}
}

// A keyring already on the bus is reused, and an install directory the user
// already has is never replaced.
func TestXodusNexusKeepsRunningKeyringAndExistingInstallDir(t *testing.T) {
	home, games := t.TempDir(), filepath.Join(t.TempDir(), "xodus-nexus")
	if err := os.Mkdir(games, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(home, "Games", "Xodus Nexus")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	log := runXodusNexus(t, home, games, true)
	if strings.Contains(log, "gnome-keyring-daemon") {
		t.Errorf("started a second keyring:\n%s", log)
	}
	if fi, err := os.Lstat(existing); err != nil || !fi.IsDir() {
		t.Errorf("existing install directory replaced: %v %v", fi, err)
	}
}
