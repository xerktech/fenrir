package gownvidia32

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const gamePatchesHook = "root/etc/cont-init.d/31-xodus-nexus-game-patches.sh"

// sourceGamePatchesHook sources the hook as GOW's entrypoint does (root,
// `set -e`), with gosu and timeout stubbed and the script swapped for one
// running body, and reports the output and whether the entrypoint carried on.
func sourceGamePatchesHook(t *testing.T, body string) (string, bool) {
	t.Helper()
	bin := t.TempDir()
	for name, stub := range map[string]string{
		"gosu":    "#!/bin/sh\nshift\nexec \"$@\"\n",
		"timeout": "#!/bin/sh\nshift\nexec \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o700); err != nil { //nolint:gosec // an executable stub
			t.Fatal(err)
		}
	}
	script := filepath.Join(bin, "xodus-nexus-game-patches")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil { //nolint:gosec // an executable stub
		t.Fatal(err)
	}
	src, err := os.ReadFile(gamePatchesHook)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.ReplaceAll(string(src), "/usr/local/bin/xodus-nexus-game-patches", script)
	if patched == string(src) {
		t.Fatal("hook no longer runs /usr/local/bin/xodus-nexus-game-patches")
	}
	h := filepath.Join(t.TempDir(), "hook.sh")
	if err = os.WriteFile(h, []byte(patched), 0o600); err != nil { //nolint:gosec // h is under t.TempDir
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c",
		`set -e; gow_log() { echo "LOG: $*"; }; source "$1"; echo STARTED`, "entrypoint", h)
	cmd.Env = append(os.Environ(), "UNAME=retro", "PATH="+bin+":"+os.Getenv("PATH"))
	b, _ := cmd.CombinedOutput()
	return string(b), strings.Contains(string(b), "STARTED")
}

// The session starts whether the patch step succeeds, fails or times out
// (timeout exits 124): it only logs.
func TestGamePatchesHookNeverStopsTheSession(t *testing.T) {
	for test, body := range map[string]string{"succeeds": "echo patched", "fails": "exit 1", "times out": "exit 124"} {
		t.Run(test, func(t *testing.T) {
			out, started := sourceGamePatchesHook(t, body)
			if !started {
				t.Errorf("the entrypoint stopped:\n%s", out)
			}
			if test != "succeeds" && !strings.Contains(out, "LOG: Could not refresh") {
				t.Errorf("no log line for a failure:\n%s", out)
			}
		})
	}
}
