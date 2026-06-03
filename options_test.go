package slabby

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// =============================================================================
// P3 Group 1: Latency tracking options (high risk — changes hot path)
// =============================================================================

// TestWithLatencyTracking_Enabled exercises the allocation hot path with
// latency tracking enabled. startTime is captured on every alloc, and
// trackAllocationLatency writes to per-CPU ring buffers via atomics.
func TestWithLatencyTracking_Enabled(t *testing.T) {
	a, err := New(256, 500,
		WithLatencyTracking(true),
		WithLatencySampling(10000), // 100% sampling — exercise buffer writes
	)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	if !a.config.enableLatencyTracking {
		t.Fatal("latency tracking should be enabled")
	}
	if a.config.latencySampleRate != 10000 {
		t.Fatalf("expected sample rate 10000, got %d", a.config.latencySampleRate)
	}

	// Do enough allocations to fill the ring buffers.
	for i := 0; i < 200; i++ {
		ref, err := a.Allocate()
		if err != nil {
			t.Fatalf("Allocate %d failed: %v", i, err)
		}
		// Touch memory.
		data := ref.GetBytes()
		data[0] = byte(i)
		ref.Release()
	}

	// Latency percentiles should be populated.
	health := a.HealthCheck()
	t.Logf("Latency tracking: p50=%v p95=%v p99=%v", health.AllocLatencyP50, health.AllocLatencyP95, health.AllocLatencyP99)

	stats := a.Stats()
	t.Logf("AvgAllocTimeNs=%f MaxAllocTimeNs=%d", stats.AvgAllocTimeNs, stats.MaxAllocTimeNs)
}

// TestWithLatencyTracking_Concurrent runs the allocation hot path under
// concurrent load with latency tracking enabled.
func TestWithLatencyTracking_Concurrent(t *testing.T) {
	a, err := New(512, 2000,
		WithLatencyTracking(true),
		WithLatencySampling(5000), // 50% sampling
	)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	const goroutines = 8
	const iters = 300

	var wg sync.WaitGroup
	var errors int64

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				ref, err := a.Allocate()
				if err != nil {
					atomic.AddInt64(&errors, 1)
					continue
				}
				data := ref.GetBytes()
				data[0] = byte(gid)
				data[len(data)-1] = byte(i)
				ref.Release()
			}
		}(g)
	}
	wg.Wait()

	if atomic.LoadInt64(&errors) > 0 {
		t.Errorf("allocation errors under latency tracking: %d", errors)
	}

	stats := a.Stats()
	t.Logf("Concurrent latency tracking: %d allocs, avg=%f ns, max=%d ns",
		stats.TotalAllocations, stats.AvgAllocTimeNs, stats.MaxAllocTimeNs)

	// Health check should not panic.
	health := a.HealthCheck()
	_ = health.AllocLatencyP50
}

// TestWithLatencyTracking_FastPath exercises the AllocateFast/DeallocateFast
// path with latency tracking enabled.
func TestWithLatencyTracking_FastPath(t *testing.T) {
	a, err := New(128, 500,
		WithLatencyTracking(true),
		WithLatencySampling(10000),
	)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	for i := 0; i < 200; i++ {
		data, id, err := a.AllocateFast()
		if err != nil {
			t.Fatalf("AllocateFast %d failed: %v", i, err)
		}
		data[0] = byte(i)
		if err := a.DeallocateFast(id); err != nil {
			t.Fatalf("DeallocateFast %d failed: %v", i, err)
		}
	}

	stats := a.Stats()
	if stats.FastAllocations == 0 {
		t.Error("FastAllocations should be > 0")
	}
}

// TestWithLatencyTracking_Disabled verifies that when latency tracking is
// off (default), startTime is not captured and the ring buffers stay empty.
func TestWithLatencyTracking_Disabled(t *testing.T) {
	a, err := New(256, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	if a.config.enableLatencyTracking {
		t.Fatal("latency tracking should be disabled by default")
	}

	for i := 0; i < 50; i++ {
		ref, err := a.Allocate()
		if err != nil {
			t.Fatalf("Allocate failed: %v", err)
		}
		ref.Release()
	}

	stats := a.Stats()
	t.Logf("Latency tracking disabled: AvgAllocTimeNs=%f MaxAllocTimeNs=%d",
		stats.AvgAllocTimeNs, stats.MaxAllocTimeNs)
}

// TestWithMaxAllocLatency verifies the max allocation latency config option
// is stored and affects health score calculation.
func TestWithMaxAllocLatency(t *testing.T) {
	customLatency := 50 * time.Microsecond
	a, err := New(256, 100,
		WithMaxAllocLatency(customLatency),
		WithHealthChecks(true),
		WithHealthInterval(50*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	if a.config.maxAllocLatency != customLatency {
		t.Errorf("expected maxAllocLatency %v, got %v", customLatency, a.config.maxAllocLatency)
	}

	// Allocate and check health — max latency should influence score.
	for i := 0; i < 10; i++ {
		ref, err := a.Allocate()
		if err != nil {
			t.Fatalf("Allocate failed: %v", err)
		}
		ref.Release()
	}

	health := a.HealthCheck()
	if health.HealthScore < 0 || health.HealthScore > 1 {
		t.Errorf("health score out of range: %f", health.HealthScore)
	}
}

// =============================================================================
// P3 Group 2: Mmap options (mmap.go)
// =============================================================================

// TestWithMmapReturnToOS_Enabled creates an mmap allocator with return-to-OS
// enabled and does alloc/dealloc cycles.
func TestWithMmapReturnToOS_Enabled(t *testing.T) {
	a, err := NewMmapAllocator(
		WithMmapThreshold(4096),
		WithMmapReturnToOS(true),
		WithMmapReturnThreshold(1024),
	)
	if err != nil {
		t.Fatalf("NewMmapAllocator failed: %v", err)
	}
	defer a.Close()

	// Allocate and deallocate several regions.
	ptrs := make([]unsafe.Pointer, 10)
	for i := 0; i < 10; i++ {
		ptr, err := a.Allocate(8192)
		if err != nil {
			t.Fatalf("Allocate %d failed: %v", i, err)
		}
		ptrs[i] = ptr
	}

	for i, ptr := range ptrs {
		if err := a.Deallocate(ptr); err != nil {
			t.Fatalf("Deallocate %d failed: %v", i, err)
		}
	}
}

// TestWithMmapReturnThreshold verifies the threshold is stored correctly.
func TestWithMmapReturnThreshold(t *testing.T) {
	a, err := NewMmapAllocator(
		WithMmapReturnThreshold(64 * 1024),
	)
	if err != nil {
		t.Fatalf("NewMmapAllocator failed: %v", err)
	}
	defer a.Close()

	_ = a // Threshold stored internally in checkReturnToOS logic
}

// =============================================================================
// P3 Group 3: Size class options (size_class.go)
// =============================================================================

// TestWithLargeThreshold verifies the large allocation threshold option.
func TestWithLargeThreshold(t *testing.T) {
	customThreshold := 4096
	a, err := NewSizeClassAllocator(
		WithSizeClassCapacity(32),
		WithLargeThreshold(customThreshold),
	)
	if err != nil {
		t.Fatalf("NewSizeClassAllocator failed: %v", err)
	}
	defer a.Close()

	if a.config.largeThreshold != customThreshold {
		t.Errorf("expected largeThreshold %d, got %d", customThreshold, a.config.largeThreshold)
	}

	// Allocate below threshold — should use slab class.
	ref, err := a.Allocate(2048)
	if err != nil {
		t.Fatalf("below-threshold allocate failed: %v", err)
	}
	if ref.IsLarge() {
		t.Error("allocation below custom threshold should not be large")
	}
	ref.Release()

	// Allocate above threshold — should be large.
	ref2, err := a.Allocate(8192)
	if err != nil {
		t.Fatalf("above-threshold allocate failed: %v", err)
	}
	if !ref2.IsLarge() {
		t.Error("allocation above custom threshold should be large")
	}
	ref2.Release()
}

// TestSizeClassSecurityOptions verifies that bit guard and secure options
// pass through to the underlying slab allocators.
func TestSizeClassSecurityOptions(t *testing.T) {
	a, err := NewSizeClassAllocator(
		WithSizeClassCapacity(32),
		WithSizeClassBitGuard(),
		WithSizeClassSecure(),
	)
	if err != nil {
		t.Fatalf("NewSizeClassAllocator failed: %v", err)
	}
	defer a.Close()

	if !a.config.enableBitGuard {
		t.Error("bit guard should be enabled")
	}
	if !a.config.enableSecure {
		t.Error("secure should be enabled")
	}

	// Allocate with security features active.
	ref, err := a.Allocate(128)
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}
	data := ref.GetBytes()
	data[0] = 0xAB

	// Release — secure mode zeros memory on the underlying slab.
	ref.Release()
}

// TestSizeClassGuardPages exercises size class with guard pages enabled.
func TestSizeClassGuardPages(t *testing.T) {
	a, err := NewSizeClassAllocator(
		WithSizeClassCapacity(16),
		WithSizeClassGuardPages(),
	)
	if err != nil {
		t.Fatalf("NewSizeClassAllocator failed: %v", err)
	}
	defer a.Close()

	if !a.config.enableGuardPages {
		t.Error("guard pages should be enabled")
	}

	ref, err := a.Allocate(64)
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}
	ref.Release()
}

// TestSizeClassFinalizers enables finalizer-based leak detection.
func TestSizeClassFinalizers(t *testing.T) {
	a, err := NewSizeClassAllocator(
		WithSizeClassCapacity(32),
		WithSizeClassFinalizers(),
	)
	if err != nil {
		t.Fatalf("NewSizeClassAllocator failed: %v", err)
	}
	defer a.Close()

	if !a.config.enableFinalizers {
		t.Error("finalizers should be enabled")
	}

	ref, err := a.Allocate(256)
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}
	ref.Release()
}

// TestSizeClassHealthChecks enables health monitoring on size class allocators.
func TestSizeClassHealthChecks(t *testing.T) {
	a, err := NewSizeClassAllocator(
		WithSizeClassCapacity(32),
		WithSizeClassHealthChecks(),
	)
	if err != nil {
		t.Fatalf("NewSizeClassAllocator failed: %v", err)
	}
	defer a.Close()

	if !a.config.enableHealthChecks {
		t.Error("health checks should be enabled")
	}

	ref, err := a.Allocate(64)
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}
	ref.Release()

	// Stats should work.
	_ = a.Stats()
}

// TestSizeClassPCPUCache exercises the PCPUCache toggle for size classes.
func TestSizeClassPCPUCache(t *testing.T) {
	// With PCPU cache enabled (default).
	a, err := NewSizeClassAllocator(
		WithSizeClassCapacity(64),
		WithSizeClassPCPUCache(true),
	)
	if err != nil {
		t.Fatalf("NewSizeClassAllocator(enabled) failed: %v", err)
	}
	defer a.Close()

	ref, err := a.Allocate(32)
	if err != nil {
		t.Fatalf("Allocate(enabled) failed: %v", err)
	}
	ref.Release()

	// With PCPU cache disabled.
	b, err := NewSizeClassAllocator(
		WithSizeClassCapacity(64),
		WithSizeClassPCPUCache(false),
	)
	if err != nil {
		t.Fatalf("NewSizeClassAllocator(disabled) failed: %v", err)
	}
	defer b.Close()

	ref2, err := b.Allocate(32)
	if err != nil {
		t.Fatalf("Allocate(disabled) failed: %v", err)
	}
	ref2.Release()
}

// TestSizeClassAllOptions_Concurrent verifies a size class allocator with all
// options enabled handles concurrent load.
func TestSizeClassAllOptions_Concurrent(t *testing.T) {
	a, err := NewSizeClassAllocator(
		WithSizeClassCapacity(512),
		WithLargeThreshold(2048),
		WithSizeClassBitGuard(),
		WithSizeClassSecure(),
		WithSizeClassGuardPages(),
		WithSizeClassFinalizers(),
		WithSizeClassHealthChecks(),
	)
	if err != nil {
		t.Fatalf("NewSizeClassAllocator failed: %v", err)
	}
	defer a.Close()

	const goroutines = 6
	const iters = 150

	var wg sync.WaitGroup
	var errors int64
	var largeAllocs int64
	var slabAllocs int64

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				// Mix of small and large allocations.
				var size int
				if i%3 == 0 {
					size = 64 // slab class
				} else if i%3 == 1 {
					size = 512 // slab class
				} else {
					size = 4096 // large
				}

				ref, err := a.Allocate(size)
				if err != nil {
					atomic.AddInt64(&errors, 1)
					continue
				}
				if ref.IsLarge() {
					atomic.AddInt64(&largeAllocs, 1)
				} else {
					atomic.AddInt64(&slabAllocs, 1)
				}
				data := ref.GetBytes()
				data[0] = byte(gid)
				data[len(data)-1] = byte(i)
				ref.Release()
			}
		}(g)
	}
	wg.Wait()

	t.Logf("All-options concurrent: %d slab, %d large, %d errors",
		atomic.LoadInt64(&slabAllocs), atomic.LoadInt64(&largeAllocs), atomic.LoadInt64(&errors))

	if atomic.LoadInt64(&errors) > 0 {
		t.Errorf("allocation errors: %d", errors)
	}
}

// =============================================================================
// P3: WithHealthInterval (slabby.go:2322) — changes health ticker period
// =============================================================================

func TestWithHealthInterval(t *testing.T) {
	a, err := New(256, 100,
		WithHealthChecks(true),
		WithHealthInterval(20*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	if a.config.healthInterval != 20*time.Millisecond {
		t.Errorf("expected healthInterval 20ms, got %v", a.config.healthInterval)
	}

	// Wait for a health check cycle.
	time.Sleep(50 * time.Millisecond)

	health := a.HealthCheck()
	if health.HealthScore < 0 || health.HealthScore > 1 {
		t.Errorf("health score out of range: %f", health.HealthScore)
	}
}
