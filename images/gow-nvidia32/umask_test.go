package gownvidia32

import (
	"os"
	"strings"
	"testing"
)

// GoW's base images set UMASK=000, so without the override every session
// target leaves world-writable files in the shared home and /games (XERK-2351).
// nexus inherits heroic's ENV, so it must only not reset it.
func TestSessionImagesUseUmask022(t *testing.T) {
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"steam": true, "heroic": true}
	for stage := range strings.SplitSeq(string(dockerfile), "\nFROM ") {
		header, _, _ := strings.Cut(stage, "\n")
		_, name, _ := strings.Cut(header, " AS ")
		// Any other mention could reset it: a second key on an ENV line, the
		// legacy "ENV UMASK 000", a continuation, a RUN editing GoW's hook.
		for line := range strings.SplitSeq(stage, "\n") {
			line = strings.TrimSpace(line)
			if strings.Contains(strings.ToUpper(line), "UMASK") &&
				!strings.HasPrefix(line, "#") && line != "ENV UMASK=022" {
				t.Errorf("stage %s has %q; only ENV UMASK=022 may mention UMASK", name, line)
			}
		}
		if want[name] {
			if !strings.Contains(stage, "\nENV UMASK=022\n") {
				t.Errorf("stage %s must set ENV UMASK=022", name)
			}
			delete(want, name)
		}
	}
	for name := range want {
		t.Errorf("no %s stage in the Dockerfile", name)
	}
	if !strings.Contains(string(dockerfile), "\nFROM heroic AS nexus\n") {
		t.Error("nexus no longer builds on heroic, so it no longer inherits its UMASK")
	}
}
