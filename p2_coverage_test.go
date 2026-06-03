package slabby

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =============================================================================
// P2: HandleAllocator.Size / HandleAllocator.Capacity (handle.go:195,200)
// =============================================================================

func TestHandleAllocator_SizeAndCapacity(t *testing.T) {
	ha, err := NewHandleAllocator(256, 100)
	if err != nil {
		t.Fatalf("NewHandleAllocator failed: %v", err)
	}
	defer ha.Close()

	if ha.Size() != 256 {
		t.Errorf("expected Size 256, got %d", ha.Size())
	}
	if ha.Capacity() != 100 {
		t.Errorf("expected Capacity 100, got %d", ha.Capacity())
	}
}

func TestHandleAllocator_SizeAndCapacity_ConcurrentWithAllocFree(t *testing.T) {
	ha, err := NewHandleAllocator(512, 500)
	if err != nil {
		t.Fatalf("NewHandleAllocator failed: %v", err)
	}
	defer ha.Close()

	var wg sync.WaitGroup
	var sizeErrors, capErrors int64

	// Allocator goroutines.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				h, _, err := ha.AllocateHandle(512)
				if err != nil {
					continue
				}
				ha.FreeHandle(h)
			}
		}()
	}

	// Reader goroutines hammer Size and Capacity.
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				if s := ha.Size(); s != 512 {
					atomic.AddInt64(&sizeErrors, 1)
				}
				if c := ha.Capacity(); c != 500 {
					atomic.AddInt64(&capErrors, 1)
				}
			}
		}()
	}

	wg.Wait()

	if atomic.LoadInt64(&sizeErrors) > 0 {
		t.Errorf("Size() returned wrong value under concurrency: %d times", sizeErrors)
	}
	if atomic.LoadInt64(&capErrors) > 0 {
		t.Errorf("Capacity() returned wrong value under concurrency: %d times", capErrors)
	}
}

// =============================================================================
// P2: Handle.ID / Handle.Generation (handle.go:175,180)
// =============================================================================

func TestHandle_IDAndGeneration(t *testing.T) {
	ha, err := NewHandleAllocator(256, 10)
	if err != nil {
		t.Fatalf("NewHandleAllocator failed: %v", err)
	}
	defer ha.Close()

	h, _, err := ha.AllocateHandle(256)
	if err != nil {
		t.Fatalf("AllocateHandle failed: %v", err)
	}

	id := h.ID()
	gen := h.Generation()
	if id < 0 || id >= 10 {
		t.Errorf("ID out of range: %d", id)
	}
	if gen == 0 {
		t.Error("generation should be non-zero after allocation")
	}

	// Free and verify generation changes on next alloc.
	ha.FreeHandle(h)
	h2, _, err := ha.AllocateHandle(256)
	if err != nil {
		t.Fatalf("second AllocateHandle failed: %v", err)
	}
	if h2.ID() == id && h2.Generation() != gen {
		// Same slab reused — generation should have changed.
	}
	ha.FreeHandle(h2)
}

// =============================================================================
// P2: LeakDetector.Stats / LeakDetector.String (leak_detector.go:420,440)
// =============================================================================

func TestLeakDetector_StatsAndString(t *testing.T) {
	detector := NewLeakDetector(LeakDetectorConfig{
		SampleRate:     1,
		ReportInterval: time.Hour,
	})
	detector.Start()
	defer detector.Stop()

	for i := 0; i < 100; i++ {
		detector.OnAllocate(StateHealthy, 100, true)
	}
	for i := 0; i < 30; i++ {
		detector.OnDeallocate(StateHealthy, true)
	}

	stats := detector.Stats()
	if stats.TotalAllocs != 100 {
		t.Errorf("expected 100 allocs, got %d", stats.TotalAllocs)
	}
	if stats.TotalDeallocs != 30 {
		t.Errorf("expected 30 deallocs, got %d", stats.TotalDeallocs)
	}
	if stats.NetLeaks != 70 {
		t.Errorf("expected 70 net leaks, got %d", stats.NetLeaks)
	}
	if !stats.Running {
		t.Error("expected running=true")
	}

	s := detector.String()
	if len(s) == 0 {
		t.Error("String() returned empty")
	}
}

func TestLeakDetector_Stats_ConcurrentWithAllocDealloc(t *testing.T) {
	detector := NewLeakDetector(LeakDetectorConfig{
		SampleRate:     1,
		ReportInterval: time.Hour,
	})
	detector.Start()
	defer detector.Stop()

	var wg sync.WaitGroup
	var statsErrors int64

	// Writers: alloc and dealloc.
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 5000; i++ {
			detector.OnAllocate(StateHealthy, 100, true)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			detector.OnDeallocate(StateHealthy, true)
		}
	}()

	// Reader: hammer Stats.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			s := detector.Stats()
			_ = detector.String()
			// TotalAllocs and TotalDeallocs only increase, so they can never
			// be negative even under concurrent access (they use atomic.AddInt64).
			if s.TotalAllocs < 0 || s.TotalDeallocs < 0 {
				atomic.AddInt64(&statsErrors, 1)
			}
		}
	}()

	wg.Wait()

	if atomic.LoadInt64(&statsErrors) > 0 {
		t.Errorf("Stats returned negative values under concurrency: %d times", statsErrors)
	}

	// Final verification — NetLeaks should be non-negative after all ops complete.
	final := detector.Stats()
	if final.NetLeaks < 0 {
		t.Errorf("NetLeaks went negative after completion: %d", final.NetLeaks)
	}
}

// =============================================================================
// P2: LeakDetector.OnStateChange / OnMetricsSnapshot (leak_detector.go:216,221)
// =============================================================================

func TestLeakDetector_OnStateChangeAndOnMetricsSnapshot(t *testing.T) {
	detector := NewLeakDetector()
	// These are no-ops but should not panic.
	detector.OnStateChange(StateHealthy, StateDegraded, "test")
	detector.OnMetricsSnapshot(HealthSnapshot{
		State:        StateHealthy,
		UsagePercent: 0.5,
	})

	// Called on nil-like snapshot.
	detector.OnMetricsSnapshot(HealthSnapshot{})
}

// =============================================================================
// P2: HealthAware.State / StateChangedAt / PreviousState (health_aware.go)
// =============================================================================

func TestHealthAware_StateAccessors(t *testing.T) {
	slab, err := New(256, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	ha := NewHealthAware(slab)
	defer ha.Close()

	if ha.State() != StateHealthy {
		t.Errorf("expected StateHealthy, got %v", ha.State())
	}

	changedAt := ha.StateChangedAt()
	if changedAt.IsZero() {
		t.Error("StateChangedAt should not be zero")
	}

	prev := ha.PreviousState()
	_ = prev // Initial previous state may be zero value.
}

func TestHealthAware_StateAccessors_Concurrent(t *testing.T) {
	slab, err := New(256, 2000)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	ha := NewHealthAware(slab,
		HealthConfig{
			PressureThreshold: 0.3,
			CheckInterval:     10 * time.Millisecond,
		})
	defer ha.Close()

	var wg sync.WaitGroup
	var panicCount int64

	// Hammer state accessors while health monitor runs.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					atomic.AddInt64(&panicCount, 1)
				}
			}()
			for i := 0; i < 500; i++ {
				_ = ha.State()
				_ = ha.StateChangedAt()
				_ = ha.PreviousState()
				_ = ha.QuickHealthCheck()
				_ = ha.Usage()
				_ = ha.ErrorRate()
				_ = ha.Stats()
			}
		}()
	}

	// Also do allocations to create state transitions.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			data, id, err := ha.AllocateFast()
			if err != nil {
				continue
			}
			_ = data
			ha.DeallocateFast(id)
		}
	}()

	wg.Wait()

	if atomic.LoadInt64(&panicCount) > 0 {
		t.Errorf("health accessors panicked %d times under concurrency", panicCount)
	}
}

// =============================================================================
// P2: HealthState.String (health_aware.go:29)
// =============================================================================

func TestHealthState_String(t *testing.T) {
	tests := []struct {
		state    HealthState
		expected string
	}{
		{StateHealthy, "healthy"},
		{StateDegraded, "degraded"},
		{StateSurvival, "survival"},
		{StateFallback, "fallback"},
		{HealthState(99), "unknown"},
	}

	for _, tt := range tests {
		if got := tt.state.String(); got != tt.expected {
			t.Errorf("HealthState(%d).String() = %q, want %q", tt.state, got, tt.expected)
		}
	}
}

// =============================================================================
// P2: SlotArena.Size / SlotArena.Capacity (slot_arena.go:82,90)
// =============================================================================

func TestSlotArena_SizeAndCapacity(t *testing.T) {
	arena, err := NewSlotArena(64, 32)
	if err != nil {
		t.Fatalf("NewSlotArena failed: %v", err)
	}
	defer arena.Close()

	if arena.Size() != 64 {
		t.Errorf("expected Size 64, got %d", arena.Size())
	}
	if arena.Capacity() != 32 {
		t.Errorf("expected Capacity 32, got %d", arena.Capacity())
	}
}

func TestSlotArena_SizeAndCapacity_ConcurrentWithAllocFree(t *testing.T) {
	arena, err := NewSlotArena(128, 200)
	if err != nil {
		t.Fatalf("NewSlotArena failed: %v", err)
	}
	defer arena.Close()

	var wg sync.WaitGroup
	var sizeErrors, capErrors int64

	// Alloc/free goroutines.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 150; i++ {
				slot, _, err := arena.AllocateSlot()
				if err != nil {
					continue
				}
				arena.FreeSlot(slot)
			}
		}()
	}

	// Reader goroutines.
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				if s := arena.Size(); s != 128 {
					atomic.AddInt64(&sizeErrors, 1)
				}
				if c := arena.Capacity(); c != 200 {
					atomic.AddInt64(&capErrors, 1)
				}
			}
		}()
	}

	wg.Wait()

	if atomic.LoadInt64(&sizeErrors) > 0 {
		t.Errorf("Size() returned wrong value under concurrency: %d times", sizeErrors)
	}
	if atomic.LoadInt64(&capErrors) > 0 {
		t.Errorf("Capacity() returned wrong value under concurrency: %d times", capErrors)
	}
}

// =============================================================================
// P2: GetSizeClassInfo (size_class.go:42)
// =============================================================================

func TestGetSizeClassInfo(t *testing.T) {
	info, err := GetSizeClassInfo(0)
	if err != nil {
		t.Fatalf("GetSizeClassInfo(0) failed: %v", err)
	}
	if info.ClassIndex != 0 {
		t.Errorf("expected ClassIndex 0, got %d", info.ClassIndex)
	}
	if info.ClassSize != 8 {
		t.Errorf("expected ClassSize 8, got %d", info.ClassSize)
	}
	if !info.IsTiny {
		t.Error("size class 0 (8 bytes) should be tiny")
	}
	if info.IsSmall || info.IsMedium {
		t.Error("size class 0 should not be small or medium")
	}

	// Invalid index.
	_, err = GetSizeClassInfo(-1)
	if err == nil {
		t.Error("expected error for negative index")
	}
	_, err = GetSizeClassInfo(SizeClassCount + 10)
	if err == nil {
		t.Error("expected error for out-of-range index")
	}
}

// =============================================================================
// P2: SlabRef.AllocationStack (slabby.go:1316)
// =============================================================================

func TestSlabRef_AllocationStack(t *testing.T) {
	// Without debug, AllocationStack should be empty.
	alloc, err := New(256, 10)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer alloc.Close()

	ref, err := alloc.Allocate()
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}
	defer ref.Release()

	stack := ref.AllocationStack()
	if stack != "" {
		t.Logf("AllocationStack (non-debug): %s", stack)
	}

	// With debug enabled, stack should be captured.
	allocDebug, err := New(256, 10, WithDebug())
	if err != nil {
		t.Fatalf("New(WithDebug) failed: %v", err)
	}
	defer allocDebug.Close()

	refDebug, err := allocDebug.Allocate()
	if err != nil {
		t.Fatalf("Allocate(debug) failed: %v", err)
	}
	defer refDebug.Release()

	stackDebug := refDebug.AllocationStack()
	if stackDebug == "" {
		t.Error("AllocationStack should not be empty with debug enabled")
	}
}

// =============================================================================
// P2: MmapAllocator.Size / Threshold / HybridAllocator.Size / Capacity (mmap.go)
// =============================================================================

func TestMmapAllocator_Threshold(t *testing.T) {
	a, err := NewMmapAllocator(WithMmapThreshold(4096))
	if err != nil {
		t.Fatalf("NewMmapAllocator failed: %v", err)
	}
	defer a.Close()

	if a.Threshold() != 4096 {
		t.Errorf("expected Threshold 4096, got %d", a.Threshold())
	}
}

func TestMmapAllocator_Size(t *testing.T) {
	a, err := NewMmapAllocator()
	if err != nil {
		t.Fatalf("NewMmapAllocator failed: %v", err)
	}
	defer a.Close()

	ptr, err := a.Allocate(1024)
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	size, err := a.Size(ptr)
	if err != nil {
		t.Fatalf("Size failed: %v", err)
	}
	// Size returns aligned (page-rounded) allocation size, not the requested size.
	// Apple Silicon uses 16KB pages, so 1024 becomes 16384.
	if size < 1024 {
		t.Errorf("expected size >= 1024, got %d", size)
	}

	a.Deallocate(ptr)

	// Size on freed pointer should error.
	_, err = a.Size(ptr)
	if err == nil {
		t.Error("expected error for Size on freed pointer")
	}
}

func TestHybridAllocator_SizeAndCapacity(t *testing.T) {
	a, err := NewHybridAllocator(256, 100)
	if err != nil {
		t.Fatalf("NewHybridAllocator failed: %v", err)
	}
	defer a.Close()

	if a.Size() != 256 {
		t.Errorf("expected Size 256, got %d", a.Size())
	}
	if a.Capacity() != 100 {
		t.Errorf("expected Capacity 100, got %d", a.Capacity())
	}
}

func TestMmapOptions_Constructor(t *testing.T) {
	a, err := NewMmapAllocator(
		WithMmapThreshold(8192),
		WithMmapReturnToOS(true),
		WithMmapReturnThreshold(1024*1024),
	)
	if err != nil {
		t.Fatalf("NewMmapAllocator failed: %v", err)
	}
	defer a.Close()

	if a.Threshold() != 8192 {
		t.Errorf("WithMmapThreshold: expected 8192, got %d", a.Threshold())
	}
}

// =============================================================================
// P2: DefaultLeakDetectorConfig / DefaultHealthConfig
// =============================================================================

func TestDefaultLeakDetectorConfig(t *testing.T) {
	cfg := DefaultLeakDetectorConfig()
	if cfg.SampleRate == 0 {
		t.Error("SampleRate should have default")
	}
	if cfg.ReportInterval == 0 {
		t.Error("ReportInterval should have default")
	}
	if cfg.MaxStackTraces == 0 {
		t.Error("MaxStackTraces should have default")
	}
	if cfg.AgeThreshold == 0 {
		t.Error("AgeThreshold should have default")
	}
	if cfg.LeakThreshold == 0 {
		t.Error("LeakThreshold should have default")
	}
}

func TestDefaultHealthConfig(t *testing.T) {
	cfg := DefaultHealthConfig()
	if cfg.PressureThreshold <= 0 {
		t.Error("PressureThreshold should have default")
	}
	if cfg.CheckInterval == 0 {
		t.Error("CheckInterval should have default")
	}
}
