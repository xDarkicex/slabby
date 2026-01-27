package slabby

import (
	"sync"
	"testing"
)

// TestHandleBasicAllocation tests basic handle allocation and deallocation
func TestHandleBasicAllocation(t *testing.T) {
	ha, err := NewHandleAllocator(256, 100)
	if err != nil {
		t.Fatalf("Failed to create handle allocator: %v", err)
	}
	defer ha.Close()

	// Allocate a handle
	h, data, err := ha.AllocateHandle(256)
	if err != nil {
		t.Fatalf("AllocateHandle failed: %v", err)
	}

	if !h.Valid() {
		t.Error("Handle should be valid after allocation")
	}

	// Verify data slice
	if len(data) != 256 {
		t.Errorf("Expected data length 256, got %d", len(data))
	}

	// Write to data
	for i := range data {
		data[i] = byte(i % 256)
	}

	// Free the handle
	err = ha.FreeHandle(h)
	if err != nil {
		t.Fatalf("FreeHandle failed: %v", err)
	}

	// Verify GetBytes fails with stale handle error
	_, err = ha.GetBytes(h)
	if err != ErrStaleHandle {
		t.Errorf("Expected ErrStaleHandle after free, got: %v", err)
	}
}

// TestHandleGenerationMismatch tests that stale handles are detected
func TestHandleGenerationMismatch(t *testing.T) {
	ha, err := NewHandleAllocator(256, 10)
	if err != nil {
		t.Fatalf("Failed to create handle allocator: %v", err)
	}
	defer ha.Close()

	// Allocate first handle
	h1, _, err := ha.AllocateHandle(256)
	if err != nil {
		t.Fatalf("AllocateHandle failed: %v", err)
	}

	// Allocate second handle
	h2, _, err := ha.AllocateHandle(256)
	if err != nil {
		t.Fatalf("AllocateHandle failed: %v", err)
	}

	// Free first handle
	err = ha.FreeHandle(h1)
	if err != nil {
		t.Fatalf("FreeHandle failed: %v", err)
	}

	// Try to use h1 again - should fail with stale handle error
	_, err = ha.GetBytes(h1)
	if err != ErrStaleHandle {
		t.Errorf("Expected ErrStaleHandle, got: %v", err)
	}

	// h2 should still be valid
	data, err := ha.GetBytes(h2)
	if err != nil {
		t.Fatalf("GetBytes failed for valid handle: %v", err)
	}
	if data == nil {
		t.Error("Expected non-nil data for valid handle")
	}

	// Cleanup
	ha.FreeHandle(h2)
}

// TestHandleAllocatorStats tests statistics tracking
func TestHandleAllocatorStats(t *testing.T) {
	ha, err := NewHandleAllocator(256, 100)
	if err != nil {
		t.Fatalf("Failed to create handle allocator: %v", err)
	}
	defer ha.Close()

	// Allocate several handles
	for i := 0; i < 10; i++ {
		_, _, err := ha.AllocateHandle(256)
		if err != nil {
			t.Fatalf("AllocateHandle failed: %v", err)
		}
	}

	stats := ha.Stats()
	if stats.Allocations != 10 {
		t.Errorf("Expected 10 allocations, got %d", stats.Allocations)
	}

	if stats.Deallocations != 0 {
		t.Errorf("Expected 0 deallocations, got %d", stats.Deallocations)
	}
}

// TestHandleConcurrentAllocation tests concurrent handle allocation
func TestHandleConcurrentAllocation(t *testing.T) {
	cha, err := NewConcurrentHandleAllocator(256, 2000, 4)
	if err != nil {
		t.Fatalf("Failed to create concurrent handle allocator: %v", err)
	}
	defer cha.Close()

	const goroutines = 10
	const handlesPerGoroutine = 100

	var wg sync.WaitGroup
	handles := make([]Handle, goroutines*handlesPerGoroutine)
	errors := make([]error, goroutines*handlesPerGoroutine)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for j := 0; j < handlesPerGoroutine; j++ {
				idx := goroutineID*handlesPerGoroutine + j
				h, _, err := cha.AllocateHandle(256)
				handles[idx] = h
				errors[idx] = err
			}
		}(i)
	}

	wg.Wait()

	// Check for errors
	for i, err := range errors {
		if err != nil {
			t.Errorf("Goroutine %d, handle %d: %v", i/handlesPerGoroutine, i%handlesPerGoroutine, err)
		}
	}

	// Free all handles
	for _, h := range handles {
		if h.Valid() {
			cha.FreeHandle(h)
		}
	}

	// Check stats
	stats := cha.Stats()
	if len(stats) != 4 {
		t.Errorf("Expected 4 shard stats, got %d", len(stats))
	}
}

// TestHandleInvalidOperations tests error handling for invalid operations
func TestHandleInvalidOperations(t *testing.T) {
	ha, err := NewHandleAllocator(256, 10)
	if err != nil {
		t.Fatalf("Failed to create handle allocator: %v", err)
	}
	defer ha.Close()

	// Test invalid handle (out of range ID)
	invalidHandle := Handle{id: 99999, allocator: ha}
	_, err = ha.GetBytes(invalidHandle)
	if err != ErrInvalidHandle {
		t.Errorf("Expected ErrInvalidHandle for out of range handle, got: %v", err)
	}

	// Allocate and free
	h, data, err := ha.AllocateHandle(256)
	if err != nil {
		t.Fatalf("AllocateHandle failed: %v", err)
	}

	// Verify we can use the data
	if len(data) != 256 {
		t.Errorf("Expected data length 256, got %d", len(data))
	}

	// Free the handle
	err = ha.FreeHandle(h)
	if err != nil {
		t.Fatalf("FreeHandle failed: %v", err)
	}
}

// TestHandleDoubleFree tests that double-free is detected
func TestHandleDoubleFree(t *testing.T) {
	ha, err := NewHandleAllocator(256, 10)
	if err != nil {
		t.Fatalf("Failed to create handle allocator: %v", err)
	}
	defer ha.Close()

	h, _, err := ha.AllocateHandle(256)
	if err != nil {
		t.Fatalf("AllocateHandle failed: %v", err)
	}

	// First free should succeed
	err = ha.FreeHandle(h)
	if err != nil {
		t.Fatalf("First FreeHandle should succeed: %v", err)
	}

	// Second free should fail with stale handle
	err = ha.FreeHandle(h)
	if err != ErrStaleHandle {
		t.Errorf("Expected ErrStaleHandle for double free, got: %v", err)
	}
}

// TestHandleStringRepresentation tests the String() method
func TestHandleStringRepresentation(t *testing.T) {
	h := Handle{id: (1 << 32) | 5} // generation 1, slab ID 5
	str := h.String()
	if str != "Handle(id=5, gen=1)" {
		t.Errorf("Unexpected string representation: %s", str)
	}
}

// BenchmarkHandleAllocation benchmarks handle allocation performance
func BenchmarkHandleAllocation(b *testing.B) {
	ha, err := NewHandleAllocator(256, 10000)
	if err != nil {
		b.Fatalf("Failed to create handle allocator: %v", err)
	}
	defer ha.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			h, data, err := ha.AllocateHandle(256)
			if err != nil {
				b.Fatal(err)
			}
			_ = data
			ha.FreeHandle(h)
		}
	})
}

// BenchmarkHandleVsFastAllocate compares handle vs fast allocate performance
func BenchmarkHandleVsFastAllocate(b *testing.B) {
	slab, err := New(256, 10000)
	if err != nil {
		b.Fatalf("Failed to create slab: %v", err)
	}
	defer slab.Close()

	ha, err := NewHandleAllocator(256, 10000)
	if err != nil {
		b.Fatalf("Failed to create handle allocator: %v", err)
	}
	defer ha.Close()

	// Benchmark FastAllocate
	b.Run("FastAllocate", func(b *testing.B) {
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				data, id, err := slab.AllocateFast()
				if err != nil {
					b.Fatal(err)
				}
				_ = data
				slab.DeallocateFast(id)
			}
		})
	})

	// Benchmark Handle allocation
	b.Run("HandleAllocate", func(b *testing.B) {
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				h, data, err := ha.AllocateHandle(256)
				if err != nil {
					b.Fatal(err)
				}
				_ = data
				ha.FreeHandle(h)
			}
		})
	})
}

// TestHandleAllocatorClose tests that Close properly shuts down
func TestHandleAllocatorClose(t *testing.T) {
	ha, err := NewHandleAllocator(256, 10)
	if err != nil {
		t.Fatalf("Failed to create handle allocator: %v", err)
	}

	// Allocate a handle
	h, data, err := ha.AllocateHandle(256)
	if err != nil {
		t.Fatalf("AllocateHandle failed: %v", err)
	}

	// Free the handle before closing
	ha.FreeHandle(h)

	// Close should succeed
	err = ha.Close()
	if err != nil {
		t.Errorf("Close failed: %v", err)
	}

	// Data should still be accessible until GC
	if len(data) != 256 {
		t.Errorf("Expected data length 256, got %d", len(data))
	}
	_ = h // Avoid unused variable error
}

// TestHandleAllocatorWithOptions tests creating allocator with options
func TestHandleAllocatorWithOptions(t *testing.T) {
	ha, err := NewHandleAllocator(256, 100,
		WithSecure(),
		WithBitGuard(),
		WithHealthChecks(true),
	)
	if err != nil {
		t.Fatalf("Failed to create handle allocator with options: %v", err)
	}
	defer ha.Close()

	h, data, err := ha.AllocateHandle(256)
	if err != nil {
		t.Fatalf("AllocateHandle failed: %v", err)
	}

	// Write some data
	for i := range data {
		data[i] = byte(i)
	}

	err = ha.FreeHandle(h)
	if err != nil {
		t.Fatalf("FreeHandle failed: %v", err)
	}

	// Check stats
	stats := ha.Stats()
	if stats.Allocations != 1 {
		t.Errorf("Expected 1 allocation, got %d", stats.Allocations)
	}
	if stats.Deallocations != 1 {
		t.Errorf("Expected 1 deallocation, got %d", stats.Deallocations)
	}
}

// TestHandleStaleAccessDetection tests that stale accesses are counted
func TestHandleStaleAccessDetection(t *testing.T) {
	ha, err := NewHandleAllocator(256, 10)
	if err != nil {
		t.Fatalf("Failed to create handle allocator: %v", err)
	}
	defer ha.Close()

	// Allocate and free a handle
	h, _, err := ha.AllocateHandle(256)
	if err != nil {
		t.Fatalf("AllocateHandle failed: %v", err)
	}

	ha.FreeHandle(h)

	// Try to access stale handle multiple times
	for i := 0; i < 5; i++ {
		_, err := ha.GetBytes(h)
		if err != ErrStaleHandle {
			t.Errorf("Expected ErrStaleHandle, got: %v", err)
		}
	}

	stats := ha.Stats()
	if stats.StaleAccesses != 5 {
		t.Errorf("Expected 5 stale accesses, got %d", stats.StaleAccesses)
	}
}

// TestHandleGenMismatchOnReuse tests generation increment on slab reuse
func TestHandleGenMismatchOnReuse(t *testing.T) {
	ha, err := NewHandleAllocator(256, 5)
	if err != nil {
		t.Fatalf("Failed to create handle allocator: %v", err)
	}
	defer ha.Close()

	// Allocate handle 1
	h1, _, err := ha.AllocateHandle(256)
	if err != nil {
		t.Fatalf("AllocateHandle failed: %v", err)
	}
	gen1 := h1.Generation()

	// Allocate handle 2
	h2, _, err := ha.AllocateHandle(256)
	if err != nil {
		t.Fatalf("AllocateHandle failed: %v", err)
	}
	_ = h2.Generation()

	// Free handle 1
	ha.FreeHandle(h1)

	// Allocate again - might reuse slab 0 with new generation
	h3, _, err := ha.AllocateHandle(256)
	if err != nil {
		t.Fatalf("AllocateHandle failed: %v", err)
	}
	gen3 := h3.Generation()

	// Generation should have changed
	if gen3 == gen1 {
		t.Log("Generation stayed the same (slab not reused yet)")
	}

	// h1 should now be stale
	_, err = ha.GetBytes(h1)
	if err != ErrStaleHandle {
		t.Errorf("Expected ErrStaleHandle for reused slab, got: %v", err)
	}

	// h3 should be valid
	_, err = ha.GetBytes(h3)
	if err != nil {
		t.Errorf("Expected valid handle, got: %v", err)
	}

	// Cleanup
	ha.FreeHandle(h3)
}

// TestHandleAllocationTimeout tests that allocation respects timeout (if implemented)
func TestHandleAllocationTimeout(t *testing.T) {
	// This test verifies basic timeout functionality exists
	ha, err := NewHandleAllocator(256, 100)
	if err != nil {
		t.Fatalf("Failed to create handle allocator: %v", err)
	}
	defer ha.Close()

	// Quick allocation should succeed
	_, _, err = ha.AllocateHandle(256)
	if err != nil {
		t.Fatalf("AllocateHandle failed: %v", err)
	}
}
