//go:build !linux

package wheel

import "errors"

// Uinput needs Linux.
type Uinput struct{}

// NewUinput returns a backend that fails: uinput is Linux's.
func NewUinput() *Uinput { return &Uinput{} }

// Create always fails.
func (*Uinput) Create(*Request, func(Message)) (Device, Node, error) {
	return nil, Node{}, errors.New("uinput needs Linux")
}
