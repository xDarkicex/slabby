//go:build windows

package slabby

import (
	"testing"
	"unsafe"
)

// TestWindowsMmapRoundTrip verifies the basic VirtualAlloc/VirtualFree path:
// allocate a page, write a pattern into it, read it back, free it.
func TestWindowsMmapRoundTrip(t *testing.T) {
	const size = 65536

	ptr, err := windowsMmap(size)
	if err != nil {
		t.Fatalf("windowsMmap failed: %v", err)
	}
	if ptr == nil {
		t.Fatal("windowsMmap returned nil pointer")
	}

	data := unsafe.Slice((*byte)(ptr), size)

	// Write a non-zero pattern.
	for i := range data {
		data[i] = byte(i % 251)
	}
	// Read it back and confirm.
	for i := range data {
		if got := data[i]; got != byte(i%251) {
			t.Fatalf("mismatch at %d: got %d, want %d", i, got, byte(i%251))
		}
	}

	if err := windowsMunmap(ptr, size); err != nil {
		t.Fatalf("windowsMunmap failed: %v", err)
	}
}

// TestWindowsMmapAllocatorAllocDealloc exercises the MmapAllocator wrapper
// end-to-end on Windows, which is the path libraVDB and other consumers use.
func TestWindowsMmapAllocatorAllocDealloc(t *testing.T) {
	alloc, err := NewMmapAllocator()
	if err != nil {
		t.Fatalf("NewMmapAllocator failed: %v", err)
	}
	defer alloc.Close()

	ptr, err := alloc.Allocate(16384)
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}
	if ptr == nil {
		t.Fatal("Allocate returned nil")
	}

	// Touch the region to confirm it's committed and writable.
	data := unsafe.Slice((*byte)(ptr), 16384)
	data[0] = 0xAB
	data[16383] = 0xCD
	if data[0] != 0xAB || data[16383] != 0xCD {
		t.Fatalf("unexpected readback: %x %x", data[0], data[16383])
	}

	if err := alloc.Deallocate(ptr); err != nil {
		t.Fatalf("Deallocate failed: %v", err)
	}
}
