package slabby

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleProperty_BasicLifecycle tests basic handle allocation and deallocation
func TestHandleProperty_BasicLifecycle(t *testing.T) {
	allocator, err := NewHandleAllocator(512, 100)
	require.NoError(t, err)
	defer allocator.Close()

	handles := make([]Handle, 0, 50)
	for i := 0; i < 50; i++ {
		handle, data, err := allocator.AllocateHandle(512)
		require.NoError(t, err)
		assert.NotNil(t, data)
		assert.True(t, handle.Valid())
		handles = append(handles, handle)
	}

	for _, handle := range handles {
		err := allocator.FreeHandle(handle)
		assert.NoError(t, err)
	}

	stats := allocator.Stats()
	assert.Equal(t, uint64(50), stats.Allocations)
	assert.Equal(t, uint64(50), stats.Deallocations)
}

// TestHandleProperty_ConcurrentAccess tests concurrent handle operations
func TestHandleProperty_ConcurrentAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping concurrent test in short mode")
	}

	allocator, err := NewHandleAllocator(1024, 200)
	require.NoError(t, err)
	defer allocator.Close()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var allocs, frees int32

	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				handle, _, err := allocator.AllocateHandle(512)
				if err == nil {
					atomic.AddInt32(&allocs, 1)
					mu.Lock()
					err = allocator.FreeHandle(handle)
					if err == nil {
						atomic.AddInt32(&frees, 1)
					}
					mu.Unlock()
				}
			}
		}()
	}

	wg.Wait()

	_ = allocator.Stats()
	assert.Equal(t, allocs, frees, "Allocs and frees should match")
}

// TestHandleProperty_GenerationCounting tests that generation counting prevents use-after-free
func TestHandleProperty_GenerationCounting(t *testing.T) {
	allocator, err := NewHandleAllocator(512, 50)
	require.NoError(t, err)
	defer allocator.Close()

	handle1, data1, err := allocator.AllocateHandle(512)
	require.NoError(t, err)
	for i := range data1 {
		data1[i] = byte(i % 256)
	}

	err = allocator.FreeHandle(handle1)
	require.NoError(t, err)

	// Freeing a stale handle should fail
	err = allocator.FreeHandle(handle1)
	assert.Error(t, err)
	assert.Equal(t, ErrStaleHandle, err)

	// Allocate again - should get a new handle
	handle2, _, err := allocator.AllocateHandle(512)
	require.NoError(t, err)

	// Verify stale handle is rejected
	_, err = allocator.GetBytes(handle1)
	assert.Error(t, err)

	allocator.FreeHandle(handle2)
}

// TestHandleProperty_StaleAccessDetection tests stale handle detection
func TestHandleProperty_StaleAccessDetection(t *testing.T) {
	allocator, err := NewHandleAllocator(512, 50)
	require.NoError(t, err)
	defer allocator.Close()

	handles := make([]Handle, 5)
	for i := 0; i < 5; i++ {
		handles[i], _, err = allocator.AllocateHandle(512)
		require.NoError(t, err)
	}

	for i := 0; i < 3; i++ {
		err = allocator.FreeHandle(handles[i])
		require.NoError(t, err)
	}

	staleCount := 0
	for i := 0; i < 3; i++ {
		_, err = allocator.GetBytes(handles[i])
		if err == ErrStaleHandle {
			staleCount++
		}
	}
	assert.Equal(t, 3, staleCount)

	newHandles := make([]Handle, 5)
	for i := 0; i < 5; i++ {
		newHandles[i], _, err = allocator.AllocateHandle(512)
		require.NoError(t, err)
	}

	for _, h := range newHandles {
		allocator.FreeHandle(h)
	}
}

// TestHandleProperty_ConcurrentFreeDetection tests concurrent free detection
func TestHandleProperty_ConcurrentFreeDetection(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}

	allocator, err := NewHandleAllocator(512, 100)
	require.NoError(t, err)
	defer allocator.Close()

	handles := make([]Handle, 20)
	for i := 0; i < 20; i++ {
		handles[i], _, err = allocator.AllocateHandle(512)
		require.NoError(t, err)
	}

	var wg sync.WaitGroup
	freeErrors := make([]error, len(handles))

	for i, handle := range handles {
		wg.Add(1)
		go func(idx int, h Handle) {
			defer wg.Done()
			freeErrors[idx] = allocator.FreeHandle(h)
		}(i, handle)
	}

	wg.Wait()

	successCount := 0
	staleCount := 0
	for _, err := range freeErrors {
		if err == nil {
			successCount++
		} else if err == ErrStaleHandle {
			staleCount++
		}
	}

	assert.Equal(t, len(handles), successCount+staleCount)
}

// TestHandleProperty_WithOptions tests handles with various allocator options
func TestHandleProperty_WithOptions(t *testing.T) {
	configs := []struct {
		name string
		opts []AllocatorOption
	}{
		{"Secure", []AllocatorOption{WithSecure()}},
		{"BitGuard", []AllocatorOption{WithBitGuard()}},
		{"GuardPages", []AllocatorOption{WithGuardPages()}},
		{"HeapFallback", []AllocatorOption{WithHeapFallback()}},
	}

	for _, tc := range configs {
		t.Run(tc.name, func(t *testing.T) {
			allocator, err := NewHandleAllocator(512, 50, tc.opts...)
			require.NoError(t, err)
			defer allocator.Close()

			for i := 0; i < 20; i++ {
				handle, data, err := allocator.AllocateHandle(512)
				require.NoError(t, err)
				assert.NotNil(t, data)
				assert.True(t, handle.Valid())
				err = allocator.FreeHandle(handle)
				assert.NoError(t, err)
			}

			stats := allocator.Stats()
			assert.Equal(t, uint64(20), stats.Allocations)
			assert.Equal(t, uint64(20), stats.Deallocations)
		})
	}
}

// TestHandleProperty_Stress tests handles under high stress
func TestHandleProperty_Stress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	allocator, err := NewHandleAllocator(256, 500)
	require.NoError(t, err)
	defer allocator.Close()

	violations := 0

	for iteration := 0; iteration < 3; iteration++ {
		var wg sync.WaitGroup
		var mu sync.Mutex
		var allocs, frees int32

		for g := 0; g < 10; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 200; i++ {
					handle, _, err := allocator.AllocateHandle(256)
					if err == nil {
						atomic.AddInt32(&allocs, 1)
						mu.Lock()
						err = allocator.FreeHandle(handle)
						if err == nil {
							atomic.AddInt32(&frees, 1)
						}
						mu.Unlock()
					}
				}
			}()
		}

		wg.Wait()

		if allocs != frees {
			violations++
			t.Logf("Iteration %d: alloc=%d free=%d mismatch", iteration+1, allocs, frees)
		}

		allocator.slab.Reset()
	}

	assert.LessOrEqual(t, violations, 1)
}

// TestHandleProperty_ConcurrentHandleAllocator tests ConcurrentHandleAllocator
func TestHandleProperty_ConcurrentHandleAllocator(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping concurrent test in short mode")
	}

	allocator, err := NewConcurrentHandleAllocator(512, 200, 4)
	require.NoError(t, err)
	defer allocator.Close()

	handles := make([]Handle, 0, 100)
	for i := 0; i < 100; i++ {
		handle, data, err := allocator.AllocateHandle(512)
		require.NoError(t, err)
		assert.NotNil(t, data)
		assert.True(t, handle.Valid())
		handles = append(handles, handle)
	}

	for _, handle := range handles {
		err := allocator.FreeHandle(handle)
		assert.NoError(t, err)
	}

	stats := allocator.Stats()
	assert.Equal(t, 4, len(stats), "Should have 4 shards")
	
	// Sum up allocations and deallocations across all shards
	var totalAllocs, totalDeallocs uint64
	for _, stat := range stats {
		totalAllocs += stat.Allocations
		totalDeallocs += stat.Deallocations
	}
	
	assert.Equal(t, uint64(100), totalAllocs, "Total allocations should be 100")
	assert.Equal(t, uint64(100), totalDeallocs, "Total deallocations should be 100")
}
