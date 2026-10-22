//go:build linux

package inputbridge

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

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

// mountsUnder lists the mount points strictly under dir in this mount
// namespace, from /proc/self/mountinfo: the kernel's paths, so no symlink in
// dir is followed to find them.
func mountsUnder(dir string) ([]string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("read mountinfo: %w", err)
	}
	defer f.Close()
	return parseMountsUnder(f, dir)
}

func parseMountsUnder(r io.Reader, dir string) ([]string, error) {
	var points []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		if p := unescapeMountPath(fields[4]); strings.HasPrefix(p, dir+"/") {
			points = append(points, p)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read mountinfo: %w", err)
	}
	return points, nil
}

// unescapeMountPath undoes mountinfo's octal escapes (\040 for a space).
func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
