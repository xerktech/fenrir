package controllers

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// canMknod reports whether this process may create a char device node.
// Unprivileged and some sandboxed pods refuse it with EPERM even as root.
func canMknod(t *testing.T) bool {
	t.Helper()
	// /dev/null's numbers (1:3): any char device would do.
	const devNull = 1<<8 | 3
	return unix.Mknod(filepath.Join(t.TempDir(), "probe"), unix.S_IFCHR|0o600, devNull) == nil
}
