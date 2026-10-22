package inputbridge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"k8s.io/klog/v2"
)

// Sweep removes the leftover Session directories in root, the sessions host
// directory (XERK-2178). A bridge removes its own on a clean exit, but a
// SIGKILLed one leaves it with its nodes still bind-mounted, and an App pod
// starting after its Session ended has kubelet recreate it.
//
// The bridge can't list Sessions, so the operator passes live: every Session
// UID it knew of when it built this bridge's pod. A directory is swept only if
// its name is a UID not in live and it was last changed before cutoff, a time
// before that pod was built: a Session created since isn't in live, but its
// directory is newer than cutoff.
func Sweep(root string, live map[string]bool, cutoff time.Time) error {
	return sweep(root, live, cutoff, unmount)
}

func sweep(root string, live map[string]bool, cutoff time.Time, unmount func(string) error) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read %s: %w", root, err)
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || live[e.Name()] || !isUID(e.Name()) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		fi, err := e.Info()
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			continue
		}
		if !fi.ModTime().Before(cutoff) {
			continue
		}
		// The only mounts a bridge makes are its nodes in input/, which
		// Clear undoes; a mount point would not unlink.
		s := &Server{InputDir: filepath.Join(dir, "input"), unmount: unmount}
		if err := s.clearNodes(); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
			continue
		}
		klog.Infof("input-bridge: removed leftover session directory %s", dir)
	}
	return errors.Join(errs...)
}

// isUID reports whether name is a Session UID in Kubernetes' form, so the
// sweep leaves anything else an admin keeps in root alone.
func isUID(name string) bool {
	u, err := uuid.Parse(name)
	return err == nil && u.String() == name
}
