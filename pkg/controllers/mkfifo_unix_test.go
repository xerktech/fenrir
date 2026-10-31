//go:build unix

package controllers

import (
	"fmt"
	"syscall"
)

func mkfifo(path string) error {
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		return fmt.Errorf("mkfifo %s: %w", path, err)
	}
	return nil
}
