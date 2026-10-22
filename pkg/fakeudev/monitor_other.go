//go:build !linux

package fakeudev

import "errors"

// Announcer is Linux-only.
type Announcer struct{}

// NewAnnouncer fails off Linux.
func NewAnnouncer() (*Announcer, error) { return nil, errors.New("udev announcements need Linux") }

// Announce does nothing off Linux.
func (*Announcer) Announce(map[string]string) error { return nil }

// Close does nothing off Linux.
func (*Announcer) Close() error { return nil }
