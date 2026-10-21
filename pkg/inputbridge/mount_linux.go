//go:build linux

package inputbridge

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// bindSelf bind-mounts path onto itself, so the node is a mount of its own:
// one that propagates, without the App pod volume's nodev. Only the node's
// root writes the directory (LockDir), so path is the node createNode just
// made, which Server.create checked.
func bindSelf(path string) error {
	if err := unix.Mount(path, path, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	return nil
}

// unmount detaches the mount at path, if any. EINVAL (not a mount point) and
// ENOENT are not errors.
func unmount(path string) error {
	err := unix.Unmount(path, unix.MNT_DETACH|unix.UMOUNT_NOFOLLOW)
	if err == nil || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
		return nil
	}
	return fmt.Errorf("unmount %s: %w", path, err)
}
