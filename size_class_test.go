package slabby

import (
	"sync"
	"sync/atomic"
	"testing"
)

// =============================================================================
// P1: SizeClassAllocator.DeallocateFast (size_class.go:364)
// =============================================================================

// TestDeallocateFast_RoundTrip verifies that a slab allocated via the underlying
// Slabby.AllocateFast can be freed via SizeClassAllocator.DeallocateFast.
func TestDeallocateFast_RoundTrip(t *testing.T) {
	a, err := NewSizeClassAllocator(WithSizeClassCapacity(64))
	if err != nil {
		t.Fatalf("NewSizeClassAllocator failed: %v", err)
	}
	defer a.Close()

	classIdx := a.SizeToClass(64)
	if classIdx < 0 {
		t.Fatal("expected valid class index for 64-byte allocation")
	}

	// Use underlying Slabby fast path to get both data and slab ID.
	slab := a.allocators[classIdx]
	data, slabID, err := slab.AllocateFast()
	if err != nil {
		t.Fatalf("AllocateFast failed: %v", err)
	}

	// Write a pattern to prove we own the memory.
	for i := range data {
		data[i] = byte(i % 256)
	}

	// Free through SizeClassAllocator.DeallocateFast.
	if err := a.DeallocateFast(classIdx, slabID); err != nil {
		t.Fatalf("DeallocateFast failed: %v", err)
	}
}

// TestDeallocateFast_InvalidClassIdx verifies error handling for bad inputs.
func TestDeallocateFast_InvalidClassIdx(t *testing.T) {
	a, err := NewSizeClassAllocator(WithSizeClassCapacity(64))
	if err != nil {
		t.Fatalf("NewSizeClassAllocator failed: %v", err)
	}
	defer a.Close()

	// Negative class index.
	if err := a.DeallocateFast(-1, 0); err == nil {
		t.Error("expected error for negative class index")
	}

	// Class index beyond the size class table. DeallocateFast refuses these
	// (large allocations must use DeallocateLarge).
	if err := a.DeallocateFast(SizeClassCount+10, 0); err == nil {
		t.Error("expected error for large class index")
	}
}

// TestDeallocateFast_DoubleFree verifies that freeing the same slab twice
// returns an error on the second attempt.
func TestDeallocateFast_DoubleFree(t *testing.T) {
	a, err := NewSizeClassAllocator(WithSizeClassCapacity(64))
	if err != nil {
		t.Fatalf("NewSizeClassAllocator failed: %v", err)
	}
	defer a.Close()

	classIdx := a.SizeToClass(128)
	slab := a.allocators[classIdx]
	_, slabID, err := slab.AllocateFast()
	if err != nil {
		t.Fatalf("AllocateFast failed: %v", err)
	}

	// First free should succeed.
	if err := a.DeallocateFast(classIdx, slabID); err != nil {
		t.Fatalf("first DeallocateFast failed: %v", err)
	}

	// Second free on same ID should fail (double deallocation).
	if err := a.DeallocateFast(classIdx, slabID); err != ErrDoubleDeallocation {
		t.Errorf("expected ErrDoubleDeallocation, got %v", err)
	}
}

// TestDeallocateFast_Concurrent exercises DeallocateFast under concurrent
// allocate/deallocate pressure from multiple goroutines.
func TestDeallocateFast_Concurrent(t *testing.T) {
	a, err := NewSizeClassAllocator(WithSizeClassCapacity(1024))
	if err != nil {
		t.Fatalf("NewSizeClassAllocator failed: %v", err)
	}
	defer a.Close()

	classIdx := a.SizeToClass(256)
	slab := a.allocators[classIdx]

	const goroutines = 8
	const iters = 500

	var wg sync.WaitGroup
	var allocErrors int64
	var freeErrors int64
	var ops int64

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				data, slabID, err := slab.AllocateFast()
				if err != nil {
					atomic.AddInt64(&allocErrors, 1)
					continue
				}
				// Touch the memory to catch any races.
				data[0] = byte(i)
				data[len(data)-1] = byte(i >> 8)

				if err := a.DeallocateFast(classIdx, slabID); err != nil {
					atomic.AddInt64(&freeErrors, 1)
				} else {
					atomic.AddInt64(&ops, 1)
				}
			}
		}()
	}

	wg.Wait()

	t.Logf("Concurrent DeallocateFast: %d ops, %d alloc errors, %d free errors",
		atomic.LoadInt64(&ops),
		atomic.LoadInt64(&allocErrors),
		atomic.LoadInt64(&freeErrors))

	if freeErrors > 0 {
		t.Errorf("unexpected free errors under concurrency: %d", freeErrors)
	}
}

// =============================================================================
// P1: SizeClassAllocator.AllocateFast (size_class.go:335)
// =============================================================================

// TestAllocateFast_SlabSized verifies AllocateFast returns correct data and class
// index for allocations that fit within a slab-backed size class.
func TestAllocateFast_SlabSized(t *testing.T) {
	a, err := NewSizeClassAllocator(WithSizeClassCapacity(64))
	if err != nil {
		t.Fatalf("NewSizeClassAllocator failed: %v", err)
	}
	defer a.Close()

	data, classIdx, err := a.AllocateFast(64)
	if err != nil {
		t.Fatalf("AllocateFast failed: %v", err)
	}
	if classIdx < 0 {
		t.Errorf("expected non-negative class index, got %d", classIdx)
	}
	if len(data) < 64 {
		t.Errorf("expected at least 64 bytes, got %d", len(data))
	}

	// Write and verify.
	for i := 0; i < 64; i++ {
		data[i] = byte(i)
	}
	for i := 0; i < 64; i++ {
		if data[i] != byte(i) {
			t.Errorf("data corruption at offset %d: expected %d, got %d", i, i, data[i])
			break
		}
	}
}

// TestAllocateFast_Large verifies AllocateFast for sizes above the slab threshold.
func TestAllocateFast_Large(t *testing.T) {
	a, err := NewSizeClassAllocator(WithSizeClassCapacity(64))
	if err != nil {
		t.Fatalf("NewSizeClassAllocator failed: %v", err)
	}
	defer a.Close()

	// 64KB is above the default 8KB threshold.
	data, classIdx, err := a.AllocateFast(64 * 1024)
	if err != nil {
		t.Fatalf("AllocateFast (large) failed: %v", err)
	}
	if classIdx >= 0 {
		t.Logf("large allocation returned classIdx=%d (implementation detail)", classIdx)
	}
	if len(data) != 64*1024 {
		t.Errorf("expected %d bytes, got %d", 64*1024, len(data))
	}

	data[0] = 0xAB
	data[len(data)-1] = 0xCD
}

// TestAllocateFast_InvalidSize verifies error handling for invalid sizes.
func TestAllocateFast_InvalidSize(t *testing.T) {
	a, err := NewSizeClassAllocator(WithSizeClassCapacity(64))
	if err != nil {
		t.Fatalf("NewSizeClassAllocator failed: %v", err)
	}
	defer a.Close()

	_, _, err = a.AllocateFast(0)
	if err == nil {
		t.Error("expected error for zero-size allocation")
	}
	_, _, err = a.AllocateFast(-1)
	if err == nil {
		t.Error("expected error for negative-size allocation")
	}
}

// TestAllocateFast_Concurrent runs AllocateFast under concurrent load.
func TestAllocateFast_Concurrent(t *testing.T) {
	a, err := NewSizeClassAllocator(WithSizeClassCapacity(2048))
	if err != nil {
		t.Fatalf("NewSizeClassAllocator failed: %v", err)
	}
	defer a.Close()

	const goroutines = 8
	const iters = 500

	var wg sync.WaitGroup
	var allocErrors int64
	var ops int64

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				data, classIdx, err := a.AllocateFast(128)
				if err != nil {
					atomic.AddInt64(&allocErrors, 1)
					continue
				}
				atomic.AddInt64(&ops, 1)

				// Touch memory.
				data[0] = byte(gid)
				data[len(data)-1] = byte(i)

				// Free via underlying allocator since AllocateFast doesn't
				// return the slab ID we need for DeallocateFast.
				if classIdx >= 0 && classIdx < SizeClassCount {
					slab := a.allocators[classIdx]
					// We can't free via DeallocateFast without the slab ID.
					// This tests the allocation path only.
					_ = slab
				}
			}
		}(g)
	}

	wg.Wait()

	t.Logf("Concurrent AllocateFast: %d ops, %d errors",
		atomic.LoadInt64(&ops), atomic.LoadInt64(&allocErrors))
}

// TestDeallocateFast_ExhaustAndRefill verifies that slabs freed via
// DeallocateFast can be re-allocated, proving the free list round-trip works.
func TestDeallocateFast_ExhaustAndRefill(t *testing.T) {
	a, err := NewSizeClassAllocator(WithSizeClassCapacity(16))
	if err != nil {
		t.Fatalf("NewSizeClassAllocator failed: %v", err)
	}
	defer a.Close()

	classIdx := a.SizeToClass(32)
	slab := a.allocators[classIdx]

	// Allocate all slabs.
	type slot struct {
		data   []byte
		slabID int32
	}
	slots := make([]slot, 0, 16)
	for i := 0; i < 16; i++ {
		data, slabID, err := slab.AllocateFast()
		if err != nil {
			t.Fatalf("AllocateFast %d failed: %v", i, err)
		}
		slots = append(slots, slot{data, slabID})
	}

	// Free half.
	for i := 0; i < 8; i++ {
		if err := a.DeallocateFast(classIdx, slots[i].slabID); err != nil {
			t.Fatalf("DeallocateFast %d failed: %v", i, err)
		}
	}

	// Re-allocate — should succeed using freed slabs.
	for i := 0; i < 8; i++ {
		data, slabID, err := slab.AllocateFast()
		if err != nil {
			t.Fatalf("re-allocate %d failed: %v", i, err)
		}
		// Touch to prove ownership.
		data[0] = 0xFF
		// Free remaining original slots + new ones at end.
		slots = append(slots, slot{data, slabID})
	}

	// Free everything.
	for i := 8; i < len(slots); i++ {
		if err := a.DeallocateFast(classIdx, slots[i].slabID); err != nil {
			t.Errorf("final DeallocateFast %d failed: %v", i, err)
		}
	}
}

// =============================================================================
// P1: ConcurrentHandleAllocator.GetBytes (handle.go:288)
// =============================================================================

// TestConcurrentGetBytes_FreeHandleRace verifies that GetBytes called
// concurrently with FreeHandle correctly detects stale handles via
// generation mismatch.
func TestConcurrentGetBytes_FreeHandleRace(t *testing.T) {
	cha, err := NewConcurrentHandleAllocator(256, 2000, 4)
	if err != nil {
		t.Fatalf("NewConcurrentHandleAllocator failed: %v", err)
	}
	defer cha.Close()

	const goroutines = 4
	const iters = 500

	type held struct {
		h    Handle
		data []byte
	}
	heldRefs := make([]held, 0, goroutines*iters)

	// Phase 1: allocate handles from all goroutines.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allocErrors int64

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				h, data, err := cha.AllocateHandle(256)
				if err != nil {
					atomic.AddInt64(&allocErrors, 1)
					continue
				}
				data[0] = byte(gid)
				data[len(data)-1] = byte(i)
				mu.Lock()
				heldRefs = append(heldRefs, held{h, data})
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()

	if len(heldRefs) == 0 {
		t.Fatal("no handles allocated")
	}
	t.Logf("Phase 1: allocated %d handles, %d errors", len(heldRefs), allocErrors)

	// Phase 2: concurrently free handles AND call GetBytes on random handles
	// to exercise the generation-checking path under contention.
	var getBytesErrors int64
	var staleDetected int64
	var validGets int64

	wg.Add(2)
	// Goroutine A: free all handles rapidly.
	go func() {
		defer wg.Done()
		for _, h := range heldRefs {
			cha.FreeHandle(h.h)
		}
	}()

	// Goroutine B: hammer GetBytes on random handles while frees are happening.
	go func() {
		defer wg.Done()
		for i := 0; i < len(heldRefs)*2; i++ {
			idx := int(uint32(i*0x9e3779b9) % uint32(len(heldRefs)))
			_, err := cha.GetBytes(heldRefs[idx].h)
			if err == nil {
				atomic.AddInt64(&validGets, 1)
			} else if err == ErrStaleHandle {
				atomic.AddInt64(&staleDetected, 1)
			} else {
				atomic.AddInt64(&getBytesErrors, 1)
			}
		}
	}()
	wg.Wait()

	t.Logf("Phase 2: GetBytes — %d valid, %d stale, %d other errors",
		atomic.LoadInt64(&validGets),
		atomic.LoadInt64(&staleDetected),
		atomic.LoadInt64(&getBytesErrors))

	// After all frees, every handle should be stale.
	for _, h := range heldRefs {
		_, err := cha.GetBytes(h.h)
		if err != ErrStaleHandle {
			t.Errorf("expected ErrStaleHandle after mass free, got %v", err)
			break
		}
	}
}
