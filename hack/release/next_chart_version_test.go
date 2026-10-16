// Package release tests the chart release versioning script builder.yml runs.
package release

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// nextVersion runs next-chart-version.sh in a fresh repo carrying tags.
func nextVersion(t *testing.T, kind string, tags ...string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "x")
	for _, tag := range tags {
		git("tag", tag)
	}
	script, err := filepath.Abs("next-chart-version.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), script, kind)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func TestNextChartVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		tags []string
		want string
	}{
		{"first release", "patch", nil, "0.1.0"},
		{"first minor", "minor", nil, "0.1.0"},
		{"first major", "major", nil, "1.0.0"},
		{"patch", "patch", []string{"v0.1.0", "v0.1.1"}, "0.1.2"},
		// Numeric, not lexical: v0.1.10 is newer than v0.1.9.
		{"patch past nine", "patch", []string{"v0.1.9", "v0.1.10", "v0.1.2"}, "0.1.11"},
		{"minor resets patch", "minor", []string{"v0.1.7"}, "0.2.0"},
		{"major resets minor and patch", "major", []string{"v0.9.3"}, "1.0.0"},
		{"patch after a minor", "patch", []string{"v0.1.7", "v0.2.0"}, "0.2.1"},
		// Only exact vMAJOR.MINOR.PATCH tags count.
		{"ignores other tags", "patch", []string{"v0.1.0", "v9.9.9-rc1", "v01.2.3", "release-5", "v1.2"}, "0.1.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nextVersion(t, tc.kind, tc.tags...)
			if err != nil || got != tc.want {
				t.Errorf("next %s after %v = %q, %v; want %q", tc.kind, tc.tags, got, err, tc.want)
			}
		})
	}
}

func TestNextChartVersionRejectsUnknownKind(t *testing.T) {
	if got, err := nextVersion(t, "snapshot"); err == nil {
		t.Errorf("next snapshot = %q, want an error", got)
	}
}
