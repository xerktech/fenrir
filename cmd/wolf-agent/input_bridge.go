package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"games-on-whales.github.io/direwolf/pkg/inputbridge"
)

// inputBridgeCommand runs this binary as the session's input-bridge sidecar
// (pkg/inputbridge) instead of wolf-agent.
const inputBridgeCommand = "input-bridge"

// runInputBridge serves wolf-agent's device node requests until SIGTERM, then
// withdraws every node and removes the session's host directory: the runtime
// leaves a hostPath behind, and a mount left in it would stay on the node.
func runInputBridge(args []string) {
	fs := flag.NewFlagSet(inputBridgeCommand, flag.ExitOnError)
	socket := fs.String("socket", "", "Unix socket wolf-agent sends requests to (required)")
	eventsSocket := fs.String("events-socket", "",
		"Unix socket the App pod's udev-relay reads udev events from; in a directory only it mounts")
	sessionDir := fs.String("session-dir", "", "The session's host directory, mounted Bidirectional; nodes go in its input/ (required)")
	liveSessions := fs.String("live-sessions", "",
		"Comma-separated UIDs of the Sessions the operator knew of; their sibling directories are never swept")
	sweepBefore := fs.String("sweep-before", "",
		"RFC 3339 time; sibling directories of no live Session last changed before it are removed (empty: no sweep)")
	klog.InitFlags(fs)
	_ = fs.Parse(args)
	if *socket == "" || *sessionDir == "" {
		klog.Fatal("--socket and --session-dir are required")
	}

	udevDir := filepath.Join(*sessionDir, "udev")
	server := inputbridge.NewServer(filepath.Join(*sessionDir, "input"), filepath.Join(udevDir, "data"), *sessionDir)
	for _, dir := range []string{server.InputDir, udevDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			klog.Fatalf("creating %s: %v", dir, err)
		}
		// The App pod mounts both read-only, but can remount them
		// writable: only their owner keeps it out (LockedOwner).
		if err := inputbridge.LockDir(dir); err != nil {
			klog.Fatal(err)
		}
	}
	// Whatever the App put there before the lock, or a crashed bridge of
	// this pod left.
	if err := server.Clear(); err != nil {
		klog.Fatalf("clearing %s: %v", server.InputDir, err)
	}
	if err := resetUdevDir(udevDir, *sessionDir); err != nil {
		klog.Fatal(err)
	}
	if *sweepBefore != "" {
		live, cutoff, err := sweepArgs(*sessionDir, *liveSessions, *sweepBefore)
		if err != nil {
			klog.Fatal(err)
		}
		// In the background: it is housekeeping the session never waits on.
		go func() {
			if err := inputbridge.Sweep(filepath.Dir(*sessionDir), live, cutoff); err != nil {
				klog.Errorf("input-bridge: sweeping leftover session directories: %v", err)
			}
		}()
	}
	_ = os.Remove(*socket)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	l, err := (&net.ListenConfig{}).Listen(ctx, "unix", *socket)
	if err != nil {
		klog.Fatalf("listening on %s: %v", *socket, err)
	}
	if *eventsSocket != "" {
		if err := os.MkdirAll(filepath.Dir(*eventsSocket), 0o755); err != nil {
			klog.Fatalf("creating %s: %v", filepath.Dir(*eventsSocket), err)
		}
		_ = os.Remove(*eventsSocket)
		el, err := (&net.ListenConfig{}).Listen(ctx, "unix", *eventsSocket)
		if err != nil {
			klog.Fatalf("listening on %s: %v", *eventsSocket, err)
		}
		go func() {
			if err := server.ServeEvents(ctx, el); err != nil {
				klog.Errorf("input-bridge: events: %v", err)
			}
		}()
	}
	klog.Infof("input-bridge: serving %s on %s", server.InputDir, *socket)
	if err := server.Serve(ctx, l); err != nil {
		klog.Errorf("input-bridge: %v", err)
	}
	if err := server.Clear(); err != nil {
		klog.Errorf("input-bridge: clearing %s: %v", server.InputDir, err)
		return
	}
	if err := os.RemoveAll(*sessionDir); err != nil {
		klog.Errorf("input-bridge: removing %s: %v", *sessionDir, err)
	}
}

// sweepArgs parses the sweep's flags. The bridge's own directory is always
// live, whatever the operator listed.
func sweepArgs(sessionDir, liveSessions, sweepBefore string) (map[string]bool, time.Time, error) {
	cutoff, err := time.Parse(time.RFC3339, sweepBefore)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("--sweep-before: %w", err)
	}
	live := map[string]bool{filepath.Base(sessionDir): true}
	for uid := range strings.SplitSeq(liveSessions, ",") {
		live[uid] = true
	}
	return live, cutoff, nil
}

// resetUdevDir empties the locked udev directory and recreates what libudev
// expects in it: the control file (see udevControlPath in the operator) and
// the data directory, itself locked, where the bridge writes udev entries.
func resetUdevDir(dir, tmpDir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		if err = os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("clearing %s: %w", dir, err)
		}
	}
	// libudev only checks it exists.
	if err = inputbridge.WriteLockedFile(tmpDir, filepath.Join(dir, "control"), nil); err != nil {
		return fmt.Errorf("creating udev control: %w", err)
	}
	// Staged and locked like control: made in place, the App could create
	// in it before the lock.
	staged, err := os.MkdirTemp(tmpDir, ".locked-*")
	if err != nil {
		return fmt.Errorf("staging udev data: %w", err)
	}
	if err = inputbridge.LockDir(staged); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("locking udev data: %w", err)
	}
	if err = os.Rename(staged, filepath.Join(dir, "data")); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("creating udev data: %w", err)
	}
	return nil
}
