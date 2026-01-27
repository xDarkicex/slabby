//go:build linux || darwin || freebsd || netbsd || openbsd

package slabby

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// unixMmap allocates memory using mmap on Unix systems
// This is the real mmap implementation using golang.org/x/sys/unix
func unixMmap(length int) (unsafe.Pointer, error) {
	// mmap arguments:
	// -1: file descriptor (anonymous mapping, no file backing)
	// 0: offset (no offset for anonymous mapping)
	// length: size to allocate
	// PROT_READ | PROT_WRITE: readable and writable memory
	// MAP_PRIVATE | MAP_ANONYMOUS: private mapping, not file-backed

	data, err := unix.Mmap(
		-1,                                  // fd: -1 for anonymous mapping
		0,                                   // offset: 0
		length,                              // length: bytes to allocate
		unix.PROT_READ|unix.PROT_WRITE,      // prot: read+write
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS, // flags: private, anonymous
	)

	if err != nil {
		return nil, fmt.Errorf("slabby: mmap failed: %w", err)
	}

	// Return pointer to the first byte
	// This is safe because data is managed by the OS, not Go's GC
	return unsafe.Pointer(&data[0]), nil
}

// unixMunmap deallocates memory using munmap
func unixMunmap(addr unsafe.Pointer, length int) error {
	// Convert pointer back to slice for munmap
	data := unsafe.Slice((*byte)(addr), length)

	err := unix.Munmap(data)
	if err != nil {
		return fmt.Errorf("slabby: munmap failed: %w", err)
	}

	return nil
}

// unixMadvise gives advice to kernel about memory usage
func unixMadvise(addr unsafe.Pointer, length int, advice int) error {
	// Map our constants to unix constants
	var unixAdvice int

	switch advice {
	case madvNormal:
		unixAdvice = unix.MADV_NORMAL
	case madvDontNeed:
		// Platform-specific handling
		if runtime.GOOS == "linux" {
			unixAdvice = unix.MADV_DONTNEED // Linux: immediate free
		} else {
			// macOS/BSD: use MADV_FREE (lazy free)
			unixAdvice = unix.MADV_FREE
		}
	case madvFree:
		unixAdvice = unix.MADV_FREE
	default:
		unixAdvice = unix.MADV_NORMAL
	}

	// Convert pointer to slice for madvise
	data := unsafe.Slice((*byte)(addr), length)

	// madvise is best-effort, log but don't fail
	if err := unix.Madvise(data, unixAdvice); err != nil {
		// madvise failure is non-fatal - memory just won't be returned to OS
		return fmt.Errorf("slabby: madvise failed: %w", err)
	}

	return nil
}
