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
