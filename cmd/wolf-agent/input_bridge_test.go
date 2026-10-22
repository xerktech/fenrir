package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// control and data are made in tmpDir, outside the App's mounts, and only
// renamed into dir: with no tmpDir to stage in, nothing is created in dir.
func TestResetUdevDirStagesOutsideTheApp(t *testing.T) {
	dir := t.TempDir()
	if err := resetUdevDir(dir, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("reset without a staging directory succeeded")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("made %v in place", entries)
	}
}

// The bridge never sweeps its own directory, even if the operator left it
// out of --live-sessions (XERK-2178).
func TestSweepArgsKeepOwnDir(t *testing.T) {
	live, cutoff, err := sweepArgs("/sessions/own", []string{"a,b", "c"}, "2026-10-21T11:50:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if !live["own"] || !live["a"] || !live["b"] || !live["c"] || len(live) != 4 {
		t.Errorf("live = %v", live)
	}
	if cutoff.Format(time.RFC3339) != "2026-10-21T11:50:00Z" {
		t.Errorf("cutoff = %v", cutoff)
	}
	if _, _, err := sweepArgs("/sessions/own", nil, "-0001-11-30T00:00:00Z"); err == nil {
		t.Error("bad --sweep-before accepted")
	}
}
