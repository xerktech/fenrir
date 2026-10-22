//go:build linux

package fakeudev

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Announcer sends libudev monitor messages to the caller's network namespace.
// Sending to a netlink multicast group needs CAP_NET_ADMIN over that
// namespace; in a user-namespaced pod's own namespace that capability reaches
// nothing else (measured on talos04: EPERM without it).
type Announcer struct{ fd int }

// NewAnnouncer opens the netlink socket.
func NewAnnouncer() (*Announcer, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	return &Announcer{fd: fd}, nil
}

// Announce broadcasts props as a udev event (MonitorMessage).
func (a *Announcer) Announce(props map[string]string) error {
	err := unix.Sendto(a.fd, MonitorMessage(props), 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: MonitorGroupUdev})
	if err != nil {
		return fmt.Errorf("send udev event: %w", err)
	}
	return nil
}

// Close closes the socket.
func (a *Announcer) Close() error {
	if err := unix.Close(a.fd); err != nil {
		return fmt.Errorf("close netlink socket: %w", err)
	}
	return nil
}
