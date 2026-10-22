package main

import (
	"context"
	"flag"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"games-on-whales.github.io/direwolf/pkg/fakeudev"
	"games-on-whales.github.io/direwolf/pkg/inputbridge"
)

// udevRelayCommand runs this binary as the App pod's udev-relay sidecar: it
// broadcasts the udev events the input-bridge streams into the App pod's own
// network namespace, where the node's udevd can't be heard (XERK-1920).
const udevRelayCommand = "udev-relay"

// runUdevRelay relays until SIGTERM. It never exits on its own: any exited
// container ends the Session. A lost stream is redialled; the bridge resends
// every announced device on connect.
func runUdevRelay(args []string) {
	fs := flag.NewFlagSet(udevRelayCommand, flag.ExitOnError)
	socket := fs.String("events-socket", "", "The input-bridge's udev event socket (required)")
	klog.InitFlags(fs)
	_ = fs.Parse(args)
	if *socket == "" {
		klog.Fatal("--events-socket is required")
	}
	announcer, err := fakeudev.NewAnnouncer()
	if err != nil {
		klog.Fatal(err)
	}
	defer announcer.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	for ctx.Err() == nil {
		err := inputbridge.Subscribe(ctx, *socket, func(ev map[string]string) error {
			if err := announcer.Announce(ev); err != nil {
				// One lost event, not a lost stream.
				klog.Errorf("udev-relay: %s %s: %v", ev["ACTION"], ev["DEVPATH"], err)
				return nil
			}
			klog.InfoS("udev-relay", "action", ev["ACTION"], "devpath", ev["DEVPATH"], "devname", ev["DEVNAME"])
			return nil
		})
		if err != nil && ctx.Err() == nil {
			klog.Warningf("udev-relay: %v; redialling", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}
