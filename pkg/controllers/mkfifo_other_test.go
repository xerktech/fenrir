//go:build !unix

package controllers

import "errors"

// mkfifo: no FIFOs here; callers skip the case.
func mkfifo(string) error { return errors.ErrUnsupported }
