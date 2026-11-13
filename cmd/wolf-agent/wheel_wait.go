package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/klog/v2"
)

// wheelWaitCommand runs this binary as a native VR pod's wheel-wait init
// container (XERK-2852): it holds the App until the wheel helper's
// input-bridge has reset the pod's udev directory. libudev subscribes a
// monitor to udev events only if /run/udev/control exists when it is made,
// so a game started first would never hear of the wheel.
const wheelWaitCommand = "wheel-wait"

func runWheelWait(args []string) {
	fs := flag.NewFlagSet(wheelWaitCommand, flag.ExitOnError)
	file := fs.String("file", "/run/udev/control", "File the bridge creates once the directory is set up")
	timeout := fs.Duration("timeout", 5*time.Minute, "How long to wait before failing (the Session then ends)")
	klog.InitFlags(fs)
	_ = fs.Parse(args)
	if !waitForFile(*file, *timeout, 200*time.Millisecond) {
		klog.Fatalf("wheel-wait: %s did not appear within %s: the wheel helper pod never set up", *file, *timeout)
	}
	klog.Infof("wheel-wait: %s is there", *file)
}

// waitForFile reports whether path exists (lstat) within timeout.
func waitForFile(path string, timeout, every time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Lstat(path); err == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(every)
	}
}
