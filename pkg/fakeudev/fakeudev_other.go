//go:build !linux

package fakeudev

import "errors"

func mknodChar(path string, major, minor uint32) error {
	return errors.New("device nodes are only supported on linux")
}
