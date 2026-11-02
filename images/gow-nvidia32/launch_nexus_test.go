package gownvidia32

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const launchNexus = "root/opt/direwolf/launch-nexus"

// runLaunchNexus runs the script with sh and a launch command that prints the
// cache path the game would inherit.
func runLaunchNexus(t *testing.T, home string, env ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", launchNexus)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("launch-nexus: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestLaunchNexusKeepsAPerGameShaderCache(t *testing.T) {
	home := t.TempDir()
	got := runLaunchNexus(t, home,
		`DIREWOLF_LAUNCH_COMMAND=printf %s "$VKD3D_SHADER_CACHE_PATH" # xodus-nexus --launch-game 96b3e5b429ea88ce`)
	want := filepath.Join(home, ".cache", "vkd3d-proton", "96b3e5b429ea88ce")
	if got != want {
		t.Fatalf("VKD3D_SHADER_CACHE_PATH = %q, want %q", got, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("cache directory not created: %v", err)
	}
}

func TestLaunchNexusLeavesOtherLaunchesAlone(t *testing.T) {
	home := t.TempDir()
	// The Nexus launcher itself, and a game ID that would leave the cache root.
	for _, launch := range []string{`printf %s "$VKD3D_SHADER_CACHE_PATH"`,
		`printf %s "$VKD3D_SHADER_CACHE_PATH" # --launch-game ../escape`} {
		if got := runLaunchNexus(t, home, "DIREWOLF_LAUNCH_COMMAND="+launch); got != "" {
			t.Errorf("%q: VKD3D_SHADER_CACHE_PATH = %q, want unset", launch, got)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".cache")); !os.IsNotExist(err) {
		t.Errorf("created a cache directory: %v", err)
	}
}

func TestLaunchNexusKeepsAnExplicitShaderCache(t *testing.T) {
	got := runLaunchNexus(t, t.TempDir(), "VKD3D_SHADER_CACHE_PATH=/user/cache",
		`DIREWOLF_LAUNCH_COMMAND=printf %s "$VKD3D_SHADER_CACHE_PATH" # --launch-game 96b3e5b429ea88ce`)
	if got != "/user/cache" {
		t.Fatalf("VKD3D_SHADER_CACHE_PATH = %q, want the caller's", got)
	}
}

// A launch that fails ends the session under sway (GoW only kills sway after a
// success), keeping its exit status; a successful one leaves that to GoW.
func TestLaunchNexusEndsTheSessionWhenTheLaunchFails(t *testing.T) {
	for _, tc := range []struct {
		name, launch, sway string
		code               int
		killed             bool
	}{
		{"failed under sway", "exit 3", "yes", 3, true},
		{"succeeded under sway", "exit 0", "yes", 0, false},
		{"failed outside sway", "exit 3", "", 3, false},
	} {
		bin := t.TempDir()
		log := filepath.Join(bin, "log")
		if err := os.WriteFile(filepath.Join(bin, "killall"), []byte("#!/bin/sh\necho \"killall $*\" >>\""+log+"\"\n"), 0o755); err != nil { //nolint:gosec // test stub must be executable
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), "sh", launchNexus)
		cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
			"DIREWOLF_LAUNCH_COMMAND=" + tc.launch, "SWAY_STOP_ON_APP_EXIT=" + tc.sway}
		err := cmd.Run()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		calls, _ := os.ReadFile(log)
		if code != tc.code || (string(calls) == "killall sway\n") != tc.killed {
			t.Errorf("%s: exit %d, calls %q; want exit %d, sway killed %v", tc.name, code, calls, tc.code, tc.killed)
		}
	}
}

// The driver trims its cache in the session's first OpenGL process (sway), so
// the setting must be in the game-session image's environment (nexus and
// battlenet build on it), not only the game's.
func TestGameSessionImageKeepsTheNVIDIAShaderCache(t *testing.T) {
	if !strings.Contains(dockerfileStage(t, "heroic AS game-session"), "\nENV __GL_SHADER_DISK_CACHE_SKIP_CLEANUP=1\n") {
		t.Fatal("the game-session stage must set ENV __GL_SHADER_DISK_CACHE_SKIP_CLEANUP=1")
	}
	for _, target := range []string{"nexus", "battlenet"} {
		dockerfileStage(t, "game-session AS "+target)
	}
}
