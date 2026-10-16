// Package release tests the chart release versioning script builder.yml runs.
package release

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo is a scratch git repository for the script to read.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "-q")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit adds a commit and returns its hash.
func (r *repo) commit() string {
	r.t.Helper()
	r.git("commit", "-q", "--allow-empty", "-m", "x")
	return r.git("rev-parse", "HEAD")
}

// next runs next-chart-version.sh in dir.
func next(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	script, err := filepath.Abs("next-chart-version.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), script, args...)
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
		// 0.0.x is the old SNAPSHOT line: never released into.
		{"floor past a stray 0.0.x tag", "patch", []string{"v0.0.1", "v0.0.7"}, "0.1.0"},
		{"patch", "patch", []string{"v0.1.0", "v0.1.1"}, "0.1.2"},
		// Numeric, not lexical: v0.1.10 is newer than v0.1.9.
		{"patch past nine", "patch", []string{"v0.1.9", "v0.1.10", "v0.1.2"}, "0.1.11"},
		{"minor resets patch", "minor", []string{"v0.1.7"}, "0.2.0"},
		{"major resets minor and patch", "major", []string{"v0.9.3"}, "1.0.0"},
		{"patch after a minor", "patch", []string{"v0.1.7", "v0.2.0"}, "0.2.1"},
		// Only exact vMAJOR.MINOR.PATCH tags of sane size count.
		{"ignores other tags", "patch", []string{"v0.1.0", "v9.9.9-rc1", "v01.2.3", "release-5", "v1.2", "v0.1.9223372036854775807"}, "0.1.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepo(t)
			r.commit()
			for _, tag := range tc.tags {
				r.git("tag", tag)
			}
			r.commit() // the commit being released, past every tag
			got, err := next(t, r.dir, tc.kind)
			if err != nil || got != tc.want {
				t.Errorf("next %s after %v = %q, %v; want %q", tc.kind, tc.tags, got, err, tc.want)
			}
		})
	}
}

// Re-running an old build must not ship its older code as the newest
// version: ArgoCD deploys the highest one.
func TestNextChartVersionRefusesOlderCommit(t *testing.T) {
	r := newRepo(t)
	a := r.commit()
	r.git("tag", "v0.1.0", a)
	b := r.commit()
	r.git("tag", "v0.1.1", b)

	if got, err := next(t, r.dir, "patch", a); err == nil {
		t.Errorf("released %s, older than v0.1.1, as %s", a, got)
	}
	// Nothing new to ship, but a minor/major of the released tip is fine.
	if got, err := next(t, r.dir, "patch", b); err == nil {
		t.Errorf("re-released v0.1.1's commit as %s", got)
	}
	if got, err := next(t, r.dir, "minor", b); err != nil || got != "0.2.0" {
		t.Errorf("minor of the released tip = %q, %v; want 0.2.0", got, err)
	}
	c := r.commit()
	if got, err := next(t, r.dir, "patch", c); err != nil || got != "0.1.2" {
		t.Errorf("patch of a new commit = %q, %v; want 0.1.2", got, err)
	}
}

func TestNextChartVersionFailsClosed(t *testing.T) {
	if got, err := next(t, t.TempDir(), "patch"); err == nil {
		t.Errorf("outside a git repo = %q, want an error, not a first release", got)
	}
	r := newRepo(t)
	r.commit()
	if got, err := next(t, r.dir, "snapshot"); err == nil {
		t.Errorf("next snapshot = %q, want an error", got)
	}
}
