//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !windows

package slabby

import (
	"errors"
	"unsafe"
)

// ErrPlatformUnsupported is returned when mmap is not supported on the platform
var ErrPlatformUnsupported = errors.New("slabby: mmap is not supported on this platform (Linux, macOS, BSD, and Windows only)")

// unsupportedMmap returns an error on platforms without a real mapping primitive
func unsupportedMmap(length int) (unsafe.Pointer, error) {
	return nil, ErrPlatformUnsupported
}

// unsupportedMunmap returns an error on unsupported platforms
func unsupportedMunmap(addr unsafe.Pointer, length int) error {
	return ErrPlatformUnsupported
}

// unsupportedMadvise returns an error on unsupported platforms
func unsupportedMadvise(addr unsafe.Pointer, length int, advice int) error {
	return ErrPlatformUnsupported
}

// getMmapOS returns platform-specific mmap functions
func getMmapOS() mmapOS {
	return mmapOS{
		mmap:    unsupportedMmap,
		munmap:  unsupportedMunmap,
		madvise: unsupportedMadvise,
	}
}
