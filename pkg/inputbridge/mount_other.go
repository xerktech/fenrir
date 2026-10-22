//go:build !linux

package inputbridge

import "errors"

var errUnsupported = errors.New("input-bridge needs Linux")

func bindSelf(string) error { return errUnsupported }

func unmount(string) error { return nil }

func mountsUnder(string) ([]string, error) { return nil, errUnsupported }
