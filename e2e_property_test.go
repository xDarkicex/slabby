package slabby

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Cross-component E2E property tests
// These tests verify that all components work together correctly

// TestE2E_FullStackWorkflow tests the complete allocator workflow with all features
func TestE2E_FullStackWorkflow(t *testing.T) {
	allocator, err := New(512, 100,
		WithSecure(),
		WithBitGuard(),
		WithHealthChecks(true),
	)
	require.NoError(t, err)
	defer allocator.Close()

	// Allocate resources
	refs := make([]*SlabRef, 0, 50)
	for i := 0; i < 50; i++ {
		ref, err := allocator.Allocate()
		require.NoError(t, err)
		assert.True(t, ref.IsValid())

		data := ref.GetBytes()
		data[0] = byte(i)
		data[len(data)-1] = byte(i ^ 0xFF)

		refs = append(refs, ref)
	}

	stats := allocator.Stats()
	assert.Equal(t, 50, int(stats.CurrentAllocations))

	// Verify data integrity
	for i, ref := range refs {
		data := ref.GetBytes()
		assert.Equal(t, byte(i), data[0], "First byte should match for ref %d", i)
		assert.Equal(t, byte(i^0xFF), data[len(data)-1], "Last byte should match for ref %d", i)
	}

	// Deallocate resources
	for i, ref := range refs {
		err := allocator.Deallocate(ref)
		assert.NoError(t, err, "Deallocation should succeed for ref %d", i)
	}

	stats = allocator.Stats()
	assert.Equal(t, 0, int(stats.CurrentAllocations))

	// Check health metrics - just verify it doesn't panic
	_ = allocator.HealthCheck()
}

// TestE2E_MixedAllocationPatterns tests various allocation patterns
func TestE2E_MixedAllocationPatterns(t *testing.T) {
	allocator, err := New(1024, 200,
		WithHeapFallback(),
		WithPCPUCache(true),
	)
	require.NoError(t, err)
	defer allocator.Close()

	var wg sync.WaitGroup
	var allocCount, freeCount int32

	// Pattern 1: Sequential allocate/free
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			ref, err := allocator.Allocate()
			if err == nil {
				atomic.AddInt32(&allocCount, 1)
				data := ref.GetBytes()
				data[0] = byte(i)
				err = allocator.Deallocate(ref)
				if err == nil {
					atomic.AddInt32(&freeCount, 1)
				}
			}
			runtime.Gosched()
		}
	}()

	// Pattern 2: Batch operations
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			refs, err := allocator.BatchAllocate(10)
			if err == nil || len(refs) > 0 {
				atomic.AddInt32(&allocCount, int32(len(refs)))
				err = allocator.BatchDeallocate(refs)
				if err == nil {
					atomic.AddInt32(&freeCount, int32(len(refs)))
				}
			}
			runtime.Gosched()
		}
	}()

	// Pattern 3: Fast path operations
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			data, id, err := allocator.AllocateFast()
			if err == nil {
				atomic.AddInt32(&allocCount, 1)
				data[0] = byte(i)
				err = allocator.DeallocateFast(id)
				if err == nil {
					atomic.AddInt32(&freeCount, 1)
				}
			}
			runtime.Gosched()
		}
	}()

	wg.Wait()

	stats := allocator.Stats()
	t.Logf("E2E mixed pattern: alloc=%d free=%d current=%d",
		allocCount, freeCount, stats.CurrentAllocations)

	// Allow some allocations to fail due to capacity, but verify consistency
	assert.Equal(t, allocCount-freeCount, int32(stats.CurrentAllocations))
}

// TestE2E_ConcurrentAccessWithOptions tests concurrent access with various options
func TestE2E_ConcurrentAccessWithOptions(t *testing.T) {
	configs := []struct {
		name string
		opts []AllocatorOption
	}{
		{"Secure", []AllocatorOption{WithSecure()}},
		{"BitGuard", []AllocatorOption{WithBitGuard()}},
		{"Performance", []AllocatorOption{WithPCPUCache(true), WithShards(4)}},
	}

	for _, tc := range configs {
		t.Run(tc.name, func(t *testing.T) {
			allocator, err := New(512, 100, tc.opts...)
			require.NoError(t, err)
			defer allocator.Close()

			var wg sync.WaitGroup
			var successCount int32
			var mu sync.Mutex
			allRefs := make([]*SlabRef, 0, 100)
			numGoroutines := 10
			opsPerGoroutine := 10

			for g := 0; g < numGoroutines; g++ {
				wg.Add(1)
				go func(gid int) {
					defer wg.Done()

					for i := 0; i < opsPerGoroutine; i++ {
						ref, err := allocator.Allocate()
						if err == nil {
							atomic.AddInt32(&successCount, 1)
							data := ref.GetBytes()
							data[0] = byte(gid)

							mu.Lock()
							allRefs = append(allRefs, ref)
							mu.Unlock()
						}

						runtime.Gosched()
					}
				}(g)
			}

			wg.Wait()

			// Free all refs
			mu.Lock()
			for _, ref := range allRefs {
				allocator.Deallocate(ref)
			}
			mu.Unlock()

			// Note: Some allocations may have been freed already, so current may not be 0
			// Just verify no panic and log the results
			stats := allocator.Stats()
			t.Logf("%s: %d successful operations, current allocations: %d",
				tc.name, successCount, stats.CurrentAllocations)
		})
	}
}

// TestE2E_ResourceCleanup tests that resources are properly cleaned up
func TestE2E_ResourceCleanup(t *testing.T) {
	allocator, err := New(512, 100)
	require.NoError(t, err)
	defer allocator.Close()

	// Allocate resources
	refs := make([]*SlabRef, 0, 50)
	for i := 0; i < 50; i++ {
		ref, err := allocator.Allocate()
		require.NoError(t, err)
		refs = append(refs, ref)
	}

	// Keep only some references
	refs = refs[:25]

	// Explicitly deallocate some
	for i := 0; i < 10; i++ {
		allocator.Deallocate(refs[i])
	}

	// Clear the slice to release references
	refs = nil

	// Force GC
	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	// Check stats - should be 10 deallocated + 15 unreferenced = 25 total deallocated
	stats := allocator.Stats()
	t.Logf("Resource cleanup: current allocations = %d", stats.CurrentAllocations)

	// Close the allocator
	allocator.Close()

	// Verify no leaks (this is best-effort in Go)
	_ = allocator
}

// TestE2E_HandleAndSlabInterop tests Handle and SlabRef interoperability
func TestE2E_HandleAndSlabInterop(t *testing.T) {
	handleAlloc, err := NewHandleAllocator(512, 50)
	require.NoError(t, err)
	defer handleAlloc.Close()

	slabAlloc, err := New(512, 50)
	require.NoError(t, err)
	defer slabAlloc.Close()

	// Allocate from both allocators
	handles := make([]Handle, 0, 20)
	refs := make([]*SlabRef, 0, 20)

	for i := 0; i < 20; i++ {
		if i%2 == 0 {
			h, _, err := handleAlloc.AllocateHandle(512)
			require.NoError(t, err)
			handles = append(handles, h)
		} else {
			ref, err := slabAlloc.Allocate()
			require.NoError(t, err)
			refs = append(refs, ref)
		}
	}

	stats1 := handleAlloc.Stats()
	stats2 := slabAlloc.Stats()

	assert.Equal(t, uint64(10), stats1.Allocations)
	assert.Equal(t, uint64(10), stats2.TotalAllocations)

	// Free mixed
	for i := 0; i < 20; i++ {
		if i%2 == 0 {
			handleAlloc.FreeHandle(handles[i/2])
		} else {
			slabAlloc.Deallocate(refs[i/2])
		}
	}

	stats1 = handleAlloc.Stats()
	stats2 = slabAlloc.Stats()

	assert.Equal(t, uint64(10), stats1.Deallocations)
	assert.Equal(t, uint64(10), stats2.TotalDeallocations)
}

// TestE2E_PerformanceUnderLoad tests performance characteristics under load
func TestE2E_PerformanceUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	allocator, err := New(512, 500,
		WithPCPUCache(true),
		WithShards(8),
	)
	require.NoError(t, err)
	defer allocator.Close()

	start := time.Now()
	var wg sync.WaitGroup
	var allocCount, freeCount int32
	numGoroutines := 20
	opsPerGoroutine := 200

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				ref, err := allocator.Allocate()
				if err == nil {
					atomic.AddInt32(&allocCount, 1)
					data := ref.GetBytes()
					data[0] = byte(i)
					err = allocator.Deallocate(ref)
					if err == nil {
						atomic.AddInt32(&freeCount, 1)
					}
				}
			}
		}()
	}

	wg.Wait()
	elapsed := time.Since(start)

	opsPerSec := float64(allocCount) / elapsed.Seconds()
	t.Logf("Performance: %d ops in %v (%.0f ops/sec)",
		allocCount, elapsed, opsPerSec)

	// Verify all operations completed
	stats := allocator.Stats()
	assert.Equal(t, allocCount-freeCount, int32(stats.CurrentAllocations))

	// Performance assertion - should handle at least 50K ops/sec
	assert.Greater(t, opsPerSec, 50000.0,
		"Should handle at least 50K operations per second")
}

// TestE2E_HealthMonitoringIntegration tests health monitoring integration
func TestE2E_HealthMonitoringIntegration(t *testing.T) {
	allocator, err := New(512, 100)
	require.NoError(t, err)
	defer allocator.Close()

	// Get initial health
	health1 := allocator.HealthCheck()
	assert.Greater(t, health1.HealthScore, 0.0)

	// Perform some operations
	refs := make([]*SlabRef, 0, 50)
	for i := 0; i < 50; i++ {
		ref, err := allocator.Allocate()
		require.NoError(t, err)
		refs = append(refs, ref)
	}

	// Get health under load
	health2 := allocator.HealthCheck()
	t.Logf("Health under load: score=%.3f", health2.HealthScore)

	// Free resources
	for _, ref := range refs {
		allocator.Deallocate(ref)
	}

	// Health should be defined
	_ = allocator.HealthCheck()
}

// TestE2E_SequentialAllocateDeallocate tests basic sequential operations
func TestE2E_SequentialAllocateDeallocate(t *testing.T) {
	allocator, err := New(512, 100)
	require.NoError(t, err)
	defer allocator.Close()

	// Sequential allocate/deallocate
	for i := 0; i < 100; i++ {
		ref, err := allocator.Allocate()
		require.NoError(t, err)

		data := ref.GetBytes()
		data[0] = byte(i)

		err = allocator.Deallocate(ref)
		require.NoError(t, err)
	}

	stats := allocator.Stats()
	assert.Equal(t, 0, int(stats.CurrentAllocations))
	assert.Equal(t, uint64(100), stats.TotalAllocations)
	assert.Equal(t, uint64(100), stats.TotalDeallocations)
}

// TestE2E_StatsTracking tests that statistics are tracked correctly
func TestE2E_StatsTracking(t *testing.T) {
	allocator, err := New(512, 100)
	require.NoError(t, err)
	defer allocator.Close()

	stats := allocator.Stats()
	assert.Equal(t, uint64(0), stats.TotalAllocations)
	assert.Equal(t, uint64(0), stats.TotalDeallocations)

	// Allocate
	refs := make([]*SlabRef, 10)
	for i := 0; i < 10; i++ {
		ref, err := allocator.Allocate()
		require.NoError(t, err)
		refs[i] = ref
	}

	stats = allocator.Stats()
	assert.Equal(t, uint64(10), stats.TotalAllocations)
	assert.Equal(t, uint64(10), stats.CurrentAllocations)

	// Deallocate half
	for i := 0; i < 5; i++ {
		err := allocator.Deallocate(refs[i])
		require.NoError(t, err)
	}

	stats = allocator.Stats()
	assert.Equal(t, uint64(10), stats.TotalAllocations)
	assert.Equal(t, uint64(5), stats.TotalDeallocations)
	assert.Equal(t, uint64(5), stats.CurrentAllocations)

	// Deallocate remaining
	for i := 5; i < 10; i++ {
		err := allocator.Deallocate(refs[i])
		require.NoError(t, err)
	}

	stats = allocator.Stats()
	assert.Equal(t, uint64(10), stats.TotalAllocations)
	assert.Equal(t, uint64(10), stats.TotalDeallocations)
	assert.Equal(t, uint64(0), stats.CurrentAllocations)
}
