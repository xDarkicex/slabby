//go:build windows

package slabby

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsMmap allocates memory using VirtualAlloc on Windows.
//
// Unlike POSIX mmap, Windows does not have a single anonymous-mapping primitive.
// VirtualAlloc with MEM_COMMIT|MEM_RESERVE is the equivalent: the OS reserves a
// range of virtual addresses and commits (zero-initializes) the pages up front.
// Page granularity is enforced by VirtualAlloc, so callers must pass a size
// that is already a multiple of the system page size.
func windowsMmap(length int) (unsafe.Pointer, error) {
	addr, err := windows.VirtualAlloc(
		0,                            // lpAddress: let the OS choose
		uintptr(length),              // dwSize
		windows.MEM_COMMIT|windows.MEM_RESERVE, // commit+reserve in one call
		windows.PAGE_READWRITE,       // flProtect
	)
	if err != nil {
		return nil, fmt.Errorf("slabby: VirtualAlloc failed: %w", err)
	}
	return unsafe.Pointer(addr), nil
}

// windowsMunmap releases a region previously returned by windowsMmap.
// MEM_RELEASE requires the size argument to be zero and frees the entire
// reservation that was originally committed.
func windowsMunmap(addr unsafe.Pointer, length int) error {
	if err := windows.VirtualFree(uintptr(addr), 0, windows.MEM_RELEASE); err != nil {
		return fmt.Errorf("slabby: VirtualFree failed: %w", err)
	}
	return nil
}

// windowsMadvise hints the OS about memory usage. Windows has no direct
// equivalent to MADV_FREE/MADV_DONTNEED: VirtualAlloc already backs pages
// lazily and the working set manager reclaims unused pages on demand. The
// nearby munmap call will release the reservation, so this is a no-op.
func windowsMadvise(addr unsafe.Pointer, length int, advice int) error {
	return nil
}

// getMmapOS returns platform-specific mmap functions
func getMmapOS() mmapOS {
	return mmapOS{
		mmap:    windowsMmap,
		munmap:  windowsMunmap,
		madvise: windowsMadvise,
	}
}
