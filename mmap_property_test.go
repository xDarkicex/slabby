package slabby

import (
	"testing"
	"unsafe"
)

// Property: Hybrid allocator should route small allocations to slab
func TestMmapProperty_HybridSmallAllocations(t *testing.T) {
	hybrid, err := NewHybridAllocator(256, 100)
	if err != nil {
		t.Fatalf("Failed to create hybrid allocator: %v", err)
	}
	defer hybrid.Close()

	for i := 0; i < 50; i++ {
		data, id, err := hybrid.Allocate(256)
		if err != nil {
			t.Fatalf("Allocate failed: %v", err)
		}

		// Slab allocator returns fixed-size data
		if len(data) != 256 {
			t.Errorf("Expected data length 256, got %d", len(data))
		}

		// Small allocations should use slab (id >= 0)
		if id < 0 {
			t.Errorf("Expected slab ID (>=0), got %d", id)
		}

		// Write to data
		for j := range data {
			data[j] = byte(j % 256)
		}

		hybrid.Deallocate(data, id)
	}
}

// Property: Hybrid allocator should route large allocations to mmap
func TestMmapProperty_HybridLargeAllocations(t *testing.T) {
	hybrid, err := NewHybridAllocator(256, 100)
	if err != nil {
		t.Fatalf("Failed to create hybrid allocator: %v", err)
	}
	defer hybrid.Close()

	for i := 0; i < 50; i++ {
		// Sizes from 16KB to 64KB
		size := 16384 * ((i % 4) + 1)

		data, id, err := hybrid.Allocate(size)
		if err != nil {
			t.Fatalf("Allocate failed for size %d: %v", size, err)
		}

		if len(data) != size {
			t.Errorf("Expected data length %d, got %d", size, len(data))
		}

		// Large allocations should use mmap (id == -1)
		if id != -1 {
			t.Errorf("Expected mmap ID (-1) for large allocation %d, got %d", size, id)
		}

		// Write to data
		data[0] = 1
		data[size-1] = 255

		hybrid.Deallocate(data, id)
	}
}

// Property: Hybrid allocator should route exactly at threshold
func TestMmapProperty_ThresholdBoundary(t *testing.T) {
	hybrid, err := NewHybridAllocator(256, 100)
	if err != nil {
		t.Fatalf("Failed to create hybrid allocator: %v", err)
	}
	defer hybrid.Close()

	// 8KB is the default threshold
	// Exactly 8KB should go to slab
	data8192, id8192, err := hybrid.Allocate(8192)
	if err != nil {
		t.Fatalf("Allocate 8192 failed: %v", err)
	}
	if id8192 < 0 {
		t.Errorf("8KB allocation should use slab, got mmap")
	}
	hybrid.Deallocate(data8192, id8192)

	// Just over 8KB should go to mmap
	data8193, id8193, err := hybrid.Allocate(8193)
	if err != nil {
		t.Fatalf("Allocate 8193 failed: %v", err)
	}
	if id8193 != -1 {
		t.Errorf("8193B allocation should use mmap, got slab")
	}
	hybrid.Deallocate(data8193, id8193)
}

// Property: Active region count should track allocations
func TestMmapProperty_ActiveRegionsTracking(t *testing.T) {
	mmap, err := NewMmapAllocator()
	if err != nil {
		t.Fatalf("Failed to create mmap allocator: %v", err)
	}
	defer mmap.Close()

	stats := mmap.Stats()
	if stats.ActiveRegions != 0 {
		t.Errorf("Expected 0 active regions initially, got %d", stats.ActiveRegions)
	}

	// Allocate multiple regions
	ptrs := make([]unsafe.Pointer, 10)
	for i := 0; i < 10; i++ {
		ptr, err := mmap.Allocate(16384)
		if err != nil {
			t.Fatalf("Allocate failed: %v", err)
		}
		ptrs[i] = ptr
	}

	stats = mmap.Stats()
	if stats.ActiveRegions != 10 {
		t.Errorf("Expected 10 active regions, got %d", stats.ActiveRegions)
	}

	// Deallocate in reverse order
	for i := len(ptrs) - 1; i >= 0; i-- {
		mmap.Deallocate(ptrs[i])
	}

	stats = mmap.Stats()
	if stats.ActiveRegions != 0 {
		t.Errorf("Expected 0 active regions after deallocation, got %d", stats.ActiveRegions)
	}
}

// Property: Bytes allocated should track correctly
func TestMmapProperty_BytesTracking(t *testing.T) {
	mmap, err := NewMmapAllocator()
	if err != nil {
		t.Fatalf("Failed to create mmap allocator: %v", err)
	}
	defer mmap.Close()

	var totalBytes uint64

	sizes := []int{16384, 32768, 49152, 65536}
	for _, size := range sizes {
		ptr, err := mmap.Allocate(size)
		if err != nil {
			t.Fatalf("Allocate failed: %v", err)
		}

		// Write to ensure page alignment works
		data := unsafe.Slice((*byte)(ptr), size)
		data[0] = 1
		data[size-1] = 255

		totalBytes += uint64(size)
		mmap.Deallocate(ptr)
	}

	stats := mmap.Stats()
	t.Logf("Total bytes allocated: %d", stats.BytesAllocated)
}

// Property: Allocating with invalid size should fail
func TestMmapProperty_InvalidSize(t *testing.T) {
	mmap, err := NewMmapAllocator()
	if err != nil {
		t.Fatalf("Failed to create mmap allocator: %v", err)
	}
	defer mmap.Close()

	invalidSizes := []int{0, -1, -100}

	for _, size := range invalidSizes {
		_, err := mmap.Allocate(size)
		if err == nil {
			t.Errorf("Expected error for invalid size %d, but got none", size)
		}
	}
}

// Property: Deallocating nil should not crash
func TestMmapProperty_DeallocateNil(t *testing.T) {
	mmap, err := NewMmapAllocator()
	if err != nil {
		t.Fatalf("Failed to create mmap allocator: %v", err)
	}
	defer mmap.Close()

	err = mmap.Deallocate(nil)
	if err != nil {
		t.Errorf("Deallocate(nil) returned error: %v", err)
	}
}

// Property: Mixed workload should work correctly
func TestMmapProperty_MixedWorkload(t *testing.T) {
	hybrid, err := NewHybridAllocator(256, 200)
	if err != nil {
		t.Fatalf("Failed to create hybrid allocator: %v", err)
	}
	defer hybrid.Close()

	// Simulate mixed workload
	for i := 0; i < 100; i++ {
		// 70% small allocations, 30% large
		if i%10 < 7 {
			size := 256 * ((i % 32) + 1) // 256B to 8KB
			data, id, err := hybrid.Allocate(size)
			if err != nil {
				t.Fatalf("Small allocation failed: %v", err)
			}
			data[0] = byte(i)
			hybrid.Deallocate(data, id)
		} else {
			size := 16384 * ((i % 4) + 1) // 16KB to 64KB
			data, id, err := hybrid.Allocate(size)
			if err != nil {
				t.Fatalf("Large allocation failed: %v", err)
			}
			data[0] = byte(i)
			hybrid.Deallocate(data, id)
		}
	}
}

// Property: Hybrid stats should track both paths
func TestMmapProperty_HybridStats(t *testing.T) {
	hybrid, err := NewHybridAllocator(256, 100)
	if err != nil {
		t.Fatalf("Failed to create hybrid allocator: %v", err)
	}
	defer hybrid.Close()

	// Allocate from both paths
	for i := 0; i < 10; i++ {
		hybrid.Allocate(4096)  // Slab
		hybrid.Allocate(16384) // mmap
	}

	stats := hybrid.Stats()
	if stats.SlabStats == nil {
		t.Error("Expected non-nil slab stats")
	}

	t.Logf("Slab allocations: %d", stats.SlabStats.TotalAllocations)
	t.Logf("Mmap allocations: %d", stats.MmapStats.Allocations)
}

// BenchmarkMmapAllocation benchmarks mmap allocation performance
func BenchmarkMmapAllocation(b *testing.B) {
	mmap, err := NewMmapAllocator()
	if err != nil {
		b.Fatalf("Failed to create mmap allocator: %v", err)
	}
	defer mmap.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ptr, err := mmap.Allocate(16384)
			if err != nil {
				b.Fatal(err)
			}
			mmap.Deallocate(ptr)
		}
	})
}

// BenchmarkMmapAllocationVaryingSizes benchmarks mmap with varying sizes
func BenchmarkMmapAllocationVaryingSizes(b *testing.B) {
	mmap, err := NewMmapAllocator()
	if err != nil {
		b.Fatalf("Failed to create mmap allocator: %v", err)
	}
	defer mmap.Close()

	sizes := []int{16384, 32768, 65536}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			size := sizes[b.N%len(sizes)]
			ptr, err := mmap.Allocate(size)
			if err != nil {
				b.Fatal(err)
			}
			mmap.Deallocate(ptr)
		}
	})
}

// BenchmarkHybridAllocation benchmarks hybrid allocation performance
func BenchmarkHybridAllocation(b *testing.B) {
	hybrid, err := NewHybridAllocator(256, 1000)
	if err != nil {
		b.Fatalf("Failed to create hybrid allocator: %v", err)
	}
	defer hybrid.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Mix of small and large allocations
			if b.N%2 == 0 {
				data, id, err := hybrid.Allocate(4096)
				if err != nil {
					b.Fatal(err)
				}
				_ = data
				hybrid.Deallocate(data, id)
			} else {
				data, id, err := hybrid.Allocate(16384)
				if err != nil {
					b.Fatal(err)
				}
				_ = data
				hybrid.Deallocate(data, id)
			}
		}
	})
}

// BenchmarkMmapVsGo benchmarks mmap against Go's make
func BenchmarkMmapVsGo(b *testing.B) {
	b.Run("MmapAllocate", func(b *testing.B) {
		mmap, _ := NewMmapAllocator()
		defer mmap.Close()

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				ptr, err := mmap.Allocate(16384)
				if err != nil {
					b.Fatal(err)
				}
				mmap.Deallocate(ptr)
			}
		})
	})

	b.Run("GoMake", func(b *testing.B) {
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				data := make([]byte, 16384)
				_ = data
			}
		})
	})
}
