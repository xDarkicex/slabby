//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd

package slabby

import (
	"errors"
	"unsafe"
)

// ErrPlatformUnsupported is returned when mmap is not supported on the platform
var ErrPlatformUnsupported = errors.New("slabby: mmap is not supported on this platform (Linux, macOS, and BSD only)")

// unixMmap returns an error on unsupported platforms
func unixMmap(length int) (unsafe.Pointer, error) {
	return nil, ErrPlatformUnsupported
}

// unixMunmap returns an error on unsupported platforms
func unixMunmap(addr unsafe.Pointer, length int) error {
	return ErrPlatformUnsupported
}

// unixMadvise returns an error on unsupported platforms
func unixMadvise(addr unsafe.Pointer, length int, advice int) error {
	return ErrPlatformUnsupported
}
