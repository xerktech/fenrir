package gownvidia32

import (
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

func TestLaunchNexusKeepsTheNVIDIAShaderCache(t *testing.T) {
	launch := `DIREWOLF_LAUNCH_COMMAND=printf %s "$__GL_SHADER_DISK_CACHE_SKIP_CLEANUP"`
	if got := runLaunchNexus(t, t.TempDir(), launch); got != "1" {
		t.Fatalf("__GL_SHADER_DISK_CACHE_SKIP_CLEANUP = %q, want 1", got)
	}
	if got := runLaunchNexus(t, t.TempDir(), "__GL_SHADER_DISK_CACHE_SKIP_CLEANUP=0", launch); got != "0" {
		t.Fatalf("__GL_SHADER_DISK_CACHE_SKIP_CLEANUP = %q, want the caller's", got)
	}
}
