//go:build !linux

package controllers

import "testing"

// canMknod: fakeudev creates device nodes only on Linux.
func canMknod(*testing.T) bool { return false }
