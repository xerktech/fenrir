package inputbridge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	return sweep(root, live, cutoff, mountsUnder, unmount)
}

func sweep(root string, live map[string]bool, cutoff time.Time,
	mounts func(dir string) ([]string, error), unmount func(string) error,
) error {
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
		if err := removeSessionDir(dir, mounts, unmount); err != nil {
			errs = append(errs, err)
			continue
		}
		klog.Infof("input-bridge: removed leftover session directory %s", dir)
	}
	return errors.Join(errs...)
}

// RemoveSessionDir unmounts everything at or under dir, then removes it. The
// bridge's own directory on exit goes this way too: its runtime/ may carry a
// mount besides the nodes Clear undoes.
func RemoveSessionDir(dir string) error {
	// Clean: mount points are matched by string.
	return removeSessionDir(filepath.Clean(dir), mountsUnder, unmount)
}

func removeSessionDir(dir string, mounts func(string) ([]string, error), unmount func(string) error) error {
	if err := unmountAll(dir, mounts, unmount); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove %s: %w", dir, err)
	}
	return nil
}

// unmountAll detaches every mount at or under dir, deepest first, and fails unless
// none is left. Not only the bridge's node mounts in input/: on talos04 a
// leftover directory held a tmpfs on runtime/. RemoveAll crosses mounts, so
// one left would have its filesystem's contents deleted.
func unmountAll(dir string, mounts func(string) ([]string, error), unmount func(string) error) error {
	points, err := mounts(dir)
	if err != nil {
		return err
	}
	slices.SortFunc(points, func(a, b string) int { return len(b) - len(a) })
	for _, p := range points {
		if uerr := unmount(p); uerr != nil {
			return uerr
		}
	}
	if points, err = mounts(dir); err != nil {
		return err
	} else if len(points) > 0 {
		return fmt.Errorf("%s: still mounted at %v", dir, points)
	}
	return nil
}

// isUID reports whether name is a Session UID in Kubernetes' form, so the
// sweep leaves anything else an admin keeps in root alone.
func isUID(name string) bool {
	u, err := uuid.Parse(name)
	return err == nil && u.String() == name
}
