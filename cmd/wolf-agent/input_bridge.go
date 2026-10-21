package main

import (
	"context"
	"flag"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

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
	sessionDir := fs.String("session-dir", "", "The session's host directory, mounted Bidirectional; nodes go in its input/ (required)")
	klog.InitFlags(fs)
	_ = fs.Parse(args)
	if *socket == "" || *sessionDir == "" {
		klog.Fatal("--socket and --session-dir are required")
	}

	server := inputbridge.NewServer(filepath.Join(*sessionDir, "input"))
	if err := os.MkdirAll(server.InputDir, 0o755); err != nil {
		klog.Fatalf("creating %s: %v", server.InputDir, err)
	}
	// Left over from a crashed bridge of this pod, if any.
	if err := server.Clear(); err != nil {
		klog.Fatalf("clearing %s: %v", server.InputDir, err)
	}
	_ = os.Remove(*socket)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	l, err := (&net.ListenConfig{}).Listen(ctx, "unix", *socket)
	if err != nil {
		klog.Fatalf("listening on %s: %v", *socket, err)
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
