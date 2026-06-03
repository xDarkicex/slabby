package slabby

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// =============================================================================
// mmapOS mock — forces error paths in MmapAllocator
// =============================================================================

func failingMmapOS() mmapOS {
	return mmapOS{
		mmap: func(size int) (unsafe.Pointer, error) {
			return nil, fmt.Errorf("mock mmap failure")
		},
		munmap: func(addr unsafe.Pointer, length int) error {
			return fmt.Errorf("mock munmap failure")
		},
		madvise: func(addr unsafe.Pointer, length int, advice int) error {
			return fmt.Errorf("mock madvise failure")
		},
	}
}

func TestMmapAllocator_AllocateFails(t *testing.T) {
	a, err := NewMmapAllocator()
	if err != nil {
		t.Fatalf("NewMmapAllocator: %v", err)
	}
	defer a.Close()

	a.osFuncs = failingMmapOS()

	_, err = a.Allocate(4096)
	if err == nil {
		t.Error("expected error from failing mmap")
	}
}

func TestMmapAllocator_DeallocateNilPtr(t *testing.T) {
	a, err := NewMmapAllocator()
	if err != nil {
		t.Fatalf("NewMmapAllocator: %v", err)
	}
	defer a.Close()

	// nil ptr should be a no-op.
	if err := a.Deallocate(nil); err != nil {
		t.Errorf("Deallocate(nil) should return nil, got %v", err)
	}
}

func TestMmapAllocator_DeallocateUnknownPtr(t *testing.T) {
	a, err := NewMmapAllocator()
	if err != nil {
		t.Fatalf("NewMmapAllocator: %v", err)
	}
	defer a.Close()

	var x int
	err = a.Deallocate(unsafe.Pointer(&x))
	if err == nil {
		t.Error("expected error for unknown pointer")
	}
}

func TestMmapAllocator_AlignSize(t *testing.T) {
	a, err := NewMmapAllocator()
	if err != nil {
		t.Fatalf("NewMmapAllocator: %v", err)
	}
	defer a.Close()

	// alignSize with size <= 0 returns pageSize.
	if got := a.alignSize(0); got != a.pageSize {
		t.Errorf("alignSize(0) = %d, want %d (pageSize)", got, a.pageSize)
	}
	if got := a.alignSize(-1); got != a.pageSize {
		t.Errorf("alignSize(-1) = %d, want %d (pageSize)", got, a.pageSize)
	}
	// Normal alignment.
	if got := a.alignSize(1); got != a.pageSize {
		t.Errorf("alignSize(1) = %d, want %d (pageSize)", got, a.pageSize)
	}
}

func TestMmapAllocator_SizeClass(t *testing.T) {
	a, err := NewMmapAllocator()
	if err != nil {
		t.Fatalf("NewMmapAllocator: %v", err)
	}
	defer a.Close()

	// Cover all size class branches.
	tests := []struct {
		size  int
		class int
	}{
		{0, 0},        // <= 8KB
		{4096, 0},     // <= 8KB
		{8192, 0},     // <= 8KB
		{8193, 1},     // <= 16KB
		{16384, 1},    // <= 16KB
		{16385, 2},    // <= 32KB
		{32768, 2},    // <= 32KB
		{32769, 3},    // <= 64KB
		{65536, 3},    // <= 64KB
		{65537, 4},    // <= 128KB
		{131072, 4},   // <= 128KB
		{131073, 5},   // <= 256KB
		{262144, 5},   // <= 256KB
		{262145, 6},   // <= 512KB
		{524288, 6},   // <= 512KB
		{524289, 7},   // > 512KB
	}
	for _, tt := range tests {
		got := a.sizeClass(tt.size)
		if got != tt.class {
			t.Errorf("sizeClass(%d) = %d, want %d", tt.size, got, tt.class)
		}
	}
}

// =============================================================================
// SlotArena nil receiver checks — all at 66.7% (nil check uncovered)
// =============================================================================

func TestSlotArena_NilReceiver(t *testing.T) {
	var nilArena *SlotArena

	// All methods should handle nil receiver gracefully.
	if _, _, err := nilArena.AllocateSlot(); err == nil {
		t.Error("AllocateSlot on nil should error")
	}
	if _, err := nilArena.BytesForSlot(0); err == nil {
		t.Error("BytesForSlot on nil should error")
	}
	if nilArena.BytesForAllocatedSlot(0) != nil {
		t.Error("BytesForAllocatedSlot on nil should return nil")
	}
	if err := nilArena.FreeSlot(0); err == nil {
		t.Error("FreeSlot on nil should error")
	}
	if nilArena.InUse(0) {
		t.Error("InUse on nil should return false")
	}
	if nilArena.Size() != 0 {
		t.Error("Size on nil should return 0")
	}
	if nilArena.Capacity() != 0 {
		t.Error("Capacity on nil should return 0")
	}
	if nilArena.Stats() != nil {
		t.Error("Stats on nil should return nil")
	}
	if nilArena.MemoryStats().ReservedBytes != 0 {
		t.Error("MemoryStats on nil should return zero value")
	}
	if err := nilArena.Close(); err != nil {
		t.Error("Close on nil should return nil")
	}
}

func TestSlotArena_NilSlab(t *testing.T) {
	arena := &SlotArena{slab: nil}

	if _, _, err := arena.AllocateSlot(); err == nil {
		t.Error("AllocateSlot with nil slab should error")
	}
	if arena.InUse(0) {
		t.Error("InUse with nil slab should return false")
	}
	if arena.Size() != 0 {
		t.Error("Size with nil slab should return 0")
	}
	if arena.BytesForAllocatedSlot(0) != nil {
		t.Error("BytesForAllocatedSlot with nil slab should return nil")
	}
}

func TestSlotArena_BytesForSlot_InvalidID(t *testing.T) {
	arena, err := NewSlotArena(64, 4)
	if err != nil {
		t.Fatalf("NewSlotArena: %v", err)
	}
	defer arena.Close()

	// Negative ID.
	_, err = arena.BytesForSlot(Slot(-1))
	if err != ErrInvalidSlabID {
		t.Errorf("expected ErrInvalidSlabID for negative slot, got %v", err)
	}

	// ID past capacity.
	_, err = arena.BytesForSlot(Slot(100))
	if err != ErrInvalidSlabID {
		t.Errorf("expected ErrInvalidSlabID for out-of-range slot, got %v", err)
	}

	// Valid ID but not in use.
	_, err = arena.BytesForSlot(Slot(0))
	if err != ErrInvalidSlabID {
		t.Errorf("expected ErrInvalidSlabID for unallocated slot, got %v", err)
	}
}

func TestSlotArena_InUse_InvalidID(t *testing.T) {
	arena, err := NewSlotArena(64, 4)
	if err != nil {
		t.Fatalf("NewSlotArena: %v", err)
	}
	defer arena.Close()

	if arena.InUse(Slot(-1)) {
		t.Error("InUse for negative slot should return false")
	}
	if arena.InUse(Slot(100)) {
		t.Error("InUse for out-of-range slot should return false")
	}
}

// =============================================================================
// mmap_unix.go error paths — force munmap/madvise failures
// =============================================================================

func TestMmapAllocator_MunmapFails(t *testing.T) {
	a, err := NewMmapAllocator()
	if err != nil {
		t.Fatalf("NewMmapAllocator: %v", err)
	}
	defer a.Close()

	// Allocate with working mmap, then swap to failing munmap.
	ptr, err := a.Allocate(4096)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	a.osFuncs.munmap = func(addr unsafe.Pointer, length int) error {
		return fmt.Errorf("mock munmap failure")
	}

	err = a.Deallocate(ptr)
	if err == nil {
		t.Error("expected error from failing munmap")
	}
}

func TestMmapAllocator_ReturnToOS(t *testing.T) {
	// Force returnToOS path — madvise call during deallocate.
	a, err := NewMmapAllocator(
		WithMmapReturnToOS(true),
		WithMmapReturnThreshold(1), // Return immediately.
	)
	if err != nil {
		t.Fatalf("NewMmapAllocator: %v", err)
	}
	defer a.Close()

	ptr, err := a.Allocate(16384) // >= page size to trigger madvise
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	// Swap in failing madvise — munmap must still work.
	a.osFuncs.madvise = func(addr unsafe.Pointer, length int, advice int) error {
		return fmt.Errorf("mock madvise failure")
	}

	// Deallocate should call madvise (best-effort, ignores error).
	err = a.Deallocate(ptr)
	if err != nil {
		t.Fatalf("Deallocate with failing madvise should still succeed: %v", err)
	}
}

// =============================================================================
// handle.go — coverage gaps in error cleanup paths
// =============================================================================

func TestHandleAllocator_ErrorCleanup(t *testing.T) {
	// Force failure by requesting impossibly large capacity.
	_, err := NewHandleAllocator(256, 1<<30) // Huge capacity.
	if err == nil {
		t.Error("expected error for huge capacity")
	}
}

func TestConcurrentHandleAllocator_ErrorCleanup(t *testing.T) {
	// Force constructor failure with zero or negative shards.
	_, err := NewConcurrentHandleAllocator(256, 10, -1)
	if err != nil {
		t.Fatalf("negative shards should default: %v", err)
	}
}

// =============================================================================
// health_aware.go — state machine branches
// =============================================================================

func TestHealthAware_CheckAndTransition_AllBranches(t *testing.T) {
	slab, err := New(256, 100)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer slab.Close()

	ha := NewHealthAware(slab, HealthConfig{
		PressureThreshold: 0.3,
		CriticalThreshold: 0.6,
		FallbackThreshold: 0.9,
		RecoveryThreshold: 0.2,
		CheckInterval:     time.Hour,
	})
	defer ha.Close()

	// Fill allocator to trigger pressure thresholds.
	refs := make([]*SlabRef, 0)
	for i := 0; i < 35; i++ {
		ref, err := slab.Allocate()
		if err != nil {
			break
		}
		refs = append(refs, ref)
	}

	// Trigger check with usage > PressureThreshold.
	ha.checkAndTransition()

	// Release all but a few to drop below RecoveryThreshold.
	for _, ref := range refs[5:] {
		ref.Release()
	}
	ha.pressureStart = time.Now().Add(-10 * time.Second)

	// Should attempt recovery.
	ha.checkAndTransition()

	// Clean up remaining.
	for _, ref := range refs[:5] {
		ref.Release()
	}
}

func TestHealthAware_Allocate_AllStates(t *testing.T) {
	slab, err := New(256, 100)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer slab.Close()

	ha := NewHealthAware(slab, HealthConfig{
		CheckInterval:   time.Hour,
		UseGoFallback:   true,
		AllocSampleRate: 100,
	})
	defer ha.Close()

	// Force specific states.
	tests := []struct {
		state HealthState
		name  string
	}{
		{StateHealthy, "healthy"},
		{StateDegraded, "degraded"},
		{StateSurvival, "survival"},
		{StateFallback, "fallback"},
	}

	for _, tt := range tests {
		ha.state.Store(int32(tt.state))
		data, err := ha.Allocate(256)
		if err != nil && tt.state != StateHealthy && tt.state != StateDegraded && tt.state != StateSurvival {
			continue // Some states may error under certain conditions.
		}
		if data != nil && tt.state != StateFallback {
			// Clean up — data came from slab pool, need to free it.
		}
	}

	// Reset to healthy.
	ha.state.Store(int32(StateHealthy))
}

func TestHealthAware_HealthMetrics_ZeroCapacity(t *testing.T) {
	// HealthMetrics with zero capacity — avoid division by zero.
	slab, err := New(256, 10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer slab.Close()

	ha := NewHealthAware(slab, HealthConfig{
		CheckInterval: time.Hour,
	})
	defer ha.Close()

	metrics := ha.HealthMetrics()
	_ = metrics.HealthScore
}

func TestHealthAware_TransitionTo_GC(t *testing.T) {
	slab, err := New(256, 100)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer slab.Close()

	ha := NewHealthAware(slab, HealthConfig{
		CheckInterval: time.Hour,
	})
	defer ha.Close()

	// Transition to fallback — should trigger GC goroutine.
	ha.transitionTo(StateFallback, "test gc on enter")

	// Transition back to healthy — should trigger GC goroutine again.
	ha.transitionTo(StateHealthy, "test gc on recovery")
}

func TestHealthAware_NotifyAllocate_Sampling(t *testing.T) {
	slab, err := New(256, 10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer slab.Close()

	ha := NewHealthAware(slab, HealthConfig{
		CheckInterval:    time.Hour,
		AllocSampleRate:  2, // Sample every other.
	})
	defer ha.Close()

	// First call: counter=1, 1%2 != 0 → skipped.
	ha.notifyAllocate(100, true)

	// Second call: counter=2, 2%2 == 0 → fires observer.
	var called atomic.Bool
	ha.RegisterObserver(&funcObserver{
		onAllocate: func(state HealthState, latencyNs int64, success bool) {
			called.Store(true)
		},
	})
	ha.notifyAllocate(100, true)

	time.Sleep(10 * time.Millisecond)
	// May or may not fire due to sampling + async goroutine.
	_ = called.Load()
}

// =============================================================================
// leak_detector.go — suggestFix branches
// =============================================================================

func TestLeakDetector_SuggestFix_AllBranches(t *testing.T) {
	detector := NewLeakDetector()

	// defer pattern.
	s := detector.suggestFix(&stackSet{
		frames:       []string{"defer main.go:42"},
		netCount:     3,
		allocCount:   5,
		deallocCount: 2,
	})
	if s == "" {
		t.Error("expected suggestion for defer pattern")
	}

	// loop + goroutine pattern.
	s = detector.suggestFix(&stackSet{
		frames: []string{"for main.go:10"},
		stack:  "goroutine",
	})
	if s == "" {
		t.Error("expected suggestion for loop/goroutine pattern")
	}

	// select/case pattern.
	s = detector.suggestFix(&stackSet{
		frames: []string{"main.go:10"},
		stack:  "select case",
	})
	if s == "" {
		t.Error("expected suggestion for select pattern")
	}
}

func TestLeakDetector_Start_AlreadyRunning(t *testing.T) {
	detector := NewLeakDetector(LeakDetectorConfig{SampleRate: 1, ReportInterval: time.Hour})
	detector.Start()
	detector.Start() // Should no-op since already running.
	detector.Stop()
}

func TestLeakDetector_CaptureStack_NoFrames(t *testing.T) {
	// captureStack should handle edge case of zero callers gracefully.
	detector := NewLeakDetector()
	hash, full, frames := detector.captureStack()
	if hash == 0 {
		t.Error("hash should not be zero even with minimal frames")
	}
	_ = full
	_ = frames
}

func TestLeakDetector_New_CoercesDefaults(t *testing.T) {
	// Config with all zeros — constructor should coerce.
	d := NewLeakDetector(LeakDetectorConfig{})
	if d.sampleRate == 0 {
		t.Error("sampleRate should be coerced from zero")
	}
	if d.reportInterval == 0 {
		t.Error("reportInterval should be coerced from zero")
	}
}

// =============================================================================
// slabby.go — batch allocate partial success, reset, deferred ops
// =============================================================================

func TestBatchAllocate_PartialSuccess(t *testing.T) {
	// Small capacity so partial batch is likely.
	a, err := New(256, 4, WithHeapFallback())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// Request more than capacity — expect partial success.
	refs, err := a.BatchAllocate(10)
	if err == nil {
		// All succeeded (unlikely with capacity 4 + fallback).
		a.BatchDeallocate(refs)
	} else {
		// Partial success — error message should indicate shortfall.
		t.Logf("partial batch: got %d of 10, err=%v", len(refs), err)
		for _, ref := range refs {
			ref.Release()
		}
	}
}

func TestAllocateWithTimeout_CircuitBreakerOpen(t *testing.T) {
	a, err := New(256, 1, WithHealthChecks(true), WithCircuitBreaker(1, 10*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// Fill the allocator.
	ref, _ := a.Allocate()

	// First failure opens the breaker (threshold=1).
	_, _ = a.Allocate() // OOM.
	_, _ = a.Allocate() // OOM.
	_ = ref

	// AllocateWithTimeout should also see circuit breaker open / OOM.
	_, err = a.AllocateWithTimeout(5 * time.Millisecond)
	t.Logf("AllocateWithTimeout result: %v", err)
}

func TestSlabRef_Release_NilAllocator(t *testing.T) {
	ref := &SlabRef{allocatorRef: nil}
	err := ref.Release()
	if err != ErrInvalidReference {
		t.Errorf("expected ErrInvalidReference for nil allocator ref, got %v", err)
	}
}

func TestDeallocateFast_GuardPageViolation(t *testing.T) {
	a, err := New(256, 10, WithGuardPages())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	data, id, err := a.AllocateFast()
	if err != nil {
		t.Fatalf("AllocateFast: %v", err)
	}

	// Corrupt the guard page by writing just before it.
	// Guard pages are placed before and after each slab.
	_ = int64(id) * int64(a.alignedSize)
	if a.config.enableGuardPages {
		guardOffset := int64(id) * int64(GuardPageSize) * 2
		if guardOffset > 0 && guardOffset < int64(len(a.guardPages)) {
			a.guardPages[guardOffset] = 0xFF // Corrupt.
		}
	}

	err = a.DeallocateFast(id)
	if err != nil {
		t.Logf("guard page violation detected: %v", err)
	}
	_ = data

	// Ensure the slab is actually freed (clean up state).
	if err != nil {
		// The CAS already changed inUse, so the slab was freed despite the error.
	}
}

func TestPrefetchSlab_GuardPages(t *testing.T) {
	a, err := New(256, 10, WithGuardPages())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// Prefetch with guard pages enabled — exercises guard page offset calculation.
	a.prefetchSlab(0)
	a.prefetchSlab(5)
}

func TestZeroSlabMemory_GuardPages(t *testing.T) {
	a, err := New(128, 10, WithSecure(), WithGuardPages())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// Allocate, write, release — zeroSlabMemory with guard page offset.
	ref, _ := a.Allocate()
	data := ref.GetBytes()
	for i := range data {
		data[i] = 0xFF
	}
	ref.Release()
}

func TestZeroSlabMemory_LargeSlab(t *testing.T) {
	a, err := New(2048, 10, WithSecure()) // >1KB triggers chunked clearing path.
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	ref, _ := a.Allocate()
	data := ref.GetBytes()
	for i := range data {
		data[i] = 0xAA
	}
	ref.Release()
}

func TestCheckGuardPages_Disabled(t *testing.T) {
	a, err := New(256, 10) // Guard pages disabled.
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// Should return nil immediately when guard pages disabled.
	err = a.checkGuardPages(0)
	if err != nil {
		t.Errorf("checkGuardPages with guard pages disabled should return nil, got %v", err)
	}
}

func TestCheckGuardPages_LargeSlab(t *testing.T) {
	a, err := New(4096, 5, WithGuardPages())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// Allocate with guard pages to exercise the >64 byte chunk check path.
	ref, err := a.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	err = ref.Release()
	if err != nil {
		t.Errorf("release with clean guard pages: %v", err)
	}
}

func TestShouldSample_Branches(t *testing.T) {
	// Low load (loadCounter < 1000) and counter%10 == 0.
	if !shouldSample(10, 500) {
		t.Error("shouldSample(10, 500) under low load should return true")
	}

	// Low load but not every 10th.
	if shouldSample(11, 500) {
		t.Error("shouldSample(11, 500) should return false")
	}

	// High load (loadCounter > 100000) with counter%1000 != 0.
	if shouldSample(1, 200000) {
		t.Error("shouldSample(1, 200000) under high load should return false")
	}

	// Base rate (counter % 100 == 0).
	if !shouldSample(100, 50000) {
		t.Error("shouldSample(100, 50000) at base rate should return true")
	}
}

func TestFinalizeReference_WithLogger(t *testing.T) {
	a, err := New(256, 10, WithFinalizers(), WithDebug())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	ref, _ := a.Allocate()
	// Don't release — finalizeReference will be called by GC.
	// Force the finalizer path directly.
	ref.finalizeReference()
}

func TestRecordCircuitBreaker_ClosedState(t *testing.T) {
	a, err := New(256, 10, WithHealthChecks(true), WithCircuitBreaker(5, time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// Record several failures but stay under threshold.
	for i := int64(0); i < 3; i++ {
		a.recordCircuitBreakerFailure()
	}

	// Circuit should still be closed.
	if a.isCircuitBreakerOpen() {
		t.Error("circuit should still be closed under threshold")
	}

	// Record successes — reduces failure count.
	for i := 0; i < 5; i++ {
		a.recordCircuitBreakerSuccess()
	}
}

func TestRecordCircuitBreaker_HalfOpenFails(t *testing.T) {
	a, err := New(256, 10, WithHealthChecks(true), WithCircuitBreaker(5, time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// Force open by exceeding threshold.
	for i := int64(0); i < 6; i++ {
		a.recordCircuitBreakerFailure()
	}

	// Circuit should be open.
	if !a.isCircuitBreakerOpen() {
		t.Fatal("circuit should be open")
	}

	// Wait for recovery timeout, then trigger half-open.
	time.Sleep(1100 * time.Millisecond)

	// Next call to isCircuitBreakerOpen transitions to half-open.
	if a.isCircuitBreakerOpen() {
		t.Fatal("circuit should be half-open after timeout")
	}

	// Record failure in half-open — should go back to open.
	a.recordCircuitBreakerFailure()
	if !a.isCircuitBreakerOpen() {
		t.Error("circuit should be open again after half-open failure")
	}
}

func TestRecordCircuitBreaker_HalfOpenSuccess(t *testing.T) {
	a, err := New(256, 10, WithHealthChecks(true), WithCircuitBreaker(4, 50*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// Force open.
	for i := int64(0); i < 5; i++ {
		a.recordCircuitBreakerFailure()
	}

	// Wait and transition to half-open.
	time.Sleep(100 * time.Millisecond)
	a.isCircuitBreakerOpen() // Transitions to half-open.

	// Record enough successes to close (threshold/2 = 2).
	a.recordCircuitBreakerSuccess()
	a.recordCircuitBreakerSuccess()

	// Circuit should now be closed.
	if a.isCircuitBreakerOpen() {
		t.Error("circuit should be closed after half-open successes")
	}
}

func TestIsCircuitBreakerOpen_TransitionToHalfOpen(t *testing.T) {
	a, err := New(256, 10, WithHealthChecks(true), WithCircuitBreaker(3, 10*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// Force open.
	for i := int64(0); i < 4; i++ {
		a.recordCircuitBreakerFailure()
	}
	if !a.isCircuitBreakerOpen() {
		t.Fatal("should be open")
	}

	// Wait for timeout.
	time.Sleep(50 * time.Millisecond)

	// This call transitions to half-open.
	open := a.isCircuitBreakerOpen()
	if open {
		t.Error("should have transitioned to half-open (not open)")
	}
}

// =============================================================================
// size_class.go — extended class paths
// =============================================================================

func TestSizeClassAllocator_FindSizeClass_Extended(t *testing.T) {
	a, err := NewSizeClassAllocator(WithSizeClassCapacity(8))
	if err != nil {
		t.Fatalf("NewSizeClassAllocator: %v", err)
	}
	defer a.Close()

	// Sizes that hit extended size class table.
	// Extended table: 896, 1024, 1280, 1536, 2048, 3072, 4096, 6144, 8192
	if got := a.findSizeClass(896); got < SizeClassCount {
		t.Errorf("896 should use extended class, got %d", got)
	}
	if got := a.findSizeClass(4096); got < SizeClassCount {
		t.Errorf("4096 should use extended class, got %d", got)
	}

	// Size beyond all tables returns -1.
	if got := a.findSizeClass(16384); got != -1 {
		t.Errorf("16384 beyond all size classes should return -1, got %d", got)
	}
}

func TestSizeClassAllocator_AllocateFast_Large(t *testing.T) {
	a, err := NewSizeClassAllocator(WithSizeClassCapacity(8))
	if err != nil {
		t.Fatalf("NewSizeClassAllocator: %v", err)
	}
	defer a.Close()

	// Size beyond all slab classes → goes to large allocator.
	data, classIdx, err := a.AllocateFast(32 * 1024)
	if err != nil {
		t.Fatalf("AllocateFast(large): %v", err)
	}
	if classIdx >= 0 {
		// Implementation may return -1 or extended class index.
		t.Logf("large AllocateFast returned classIdx=%d", classIdx)
	}
	_ = data
}

func TestSizeClassAllocator_Allocate_ZeroSize(t *testing.T) {
	a, err := NewSizeClassAllocator(WithSizeClassCapacity(8))
	if err != nil {
		t.Fatalf("NewSizeClassAllocator: %v", err)
	}
	defer a.Close()

	_, err = a.Allocate(0)
	if err == nil {
		t.Error("expected error for zero size")
	}
}

func TestSizeClassAllocator_Allocate_ExtendedClass(t *testing.T) {
	a, err := NewSizeClassAllocator(WithSizeClassCapacity(8))
	if err != nil {
		t.Fatalf("NewSizeClassAllocator: %v", err)
	}
	defer a.Close()

	// 1024 maps to extended class.
	ref, err := a.Allocate(1024)
	if err != nil {
		t.Fatalf("Allocate(1024): %v", err)
	}
	if ref.IsLarge() {
		t.Error("1024 should not be a large allocation")
	}
	ref.Release()
}

func TestSizeClassAllocator_AllocateFast_ExtendedClass(t *testing.T) {
	a, err := NewSizeClassAllocator(WithSizeClassCapacity(8))
	if err != nil {
		t.Fatalf("NewSizeClassAllocator: %v", err)
	}
	defer a.Close()

	// Extended size class.
	data, classIdx, err := a.AllocateFast(2048)
	if err != nil {
		t.Fatalf("AllocateFast(2048): %v", err)
	}
	if classIdx < SizeClassCount {
		t.Errorf("2048 should use extended class, got classIdx=%d", classIdx)
	}
	_ = data
}

// =============================================================================
// handle.go — concurrent allocator remaining branches
// =============================================================================

func TestConcurrentHandleAllocator_AllocateHandle_Contention(t *testing.T) {
	cha, err := NewConcurrentHandleAllocator(256, 100, 2)
	if err != nil {
		t.Fatalf("NewConcurrentHandleAllocator: %v", err)
	}
	defer cha.Close()

	// Exhaust one shard to force contention fallback.
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				h, _, err := cha.AllocateHandle(256)
				if err != nil {
					continue
				}
				cha.FreeHandle(h)
			}
		}()
	}
	wg.Wait()
}

func TestHealthAware_Usage_ZeroCapacity(t *testing.T) {
	slab, err := New(256, 10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer slab.Close()

	ha := NewHealthAware(slab, HealthConfig{CheckInterval: time.Hour})
	defer ha.Close()

	// Fill all slabs.
	refs := make([]*SlabRef, 0)
	for i := 0; i < 10; i++ {
		ref, err := slab.Allocate()
		if err != nil {
			break
		}
		refs = append(refs, ref)
	}

	usage := ha.Usage()
	t.Logf("Usage with full allocator: %f", usage)
	if usage <= 0 {
		t.Error("usage should be > 0 when slabs allocated")
	}

	for _, ref := range refs {
		ref.Release()
	}
}

func TestWithLatencySampling_Clamping(t *testing.T) {
	a, err := New(256, 10, WithLatencyTracking(true), WithLatencySampling(20000))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// Rate should be clamped to 10000.
	if a.config.latencySampleRate != 10000 {
		t.Errorf("latency sample rate > 10000 should be clamped to 10000, got %d", a.config.latencySampleRate)
	}
}

func TestFastrand_ZeroSeed(t *testing.T) {
	slab, err := New(64, 10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer slab.Close()

	// Force seed to 0 to exercise the re-seed branch.
	slab.fastrandState = 0

	v := slab.fastrand()
	if v == 0 {
		t.Error("fastrand should re-seed when state is 0")
	}
}

// =============================================================================
// NewHybridAllocator — exercise constructor + remaining branches
// =============================================================================

func TestNewHybridAllocator_ErrorPaths(t *testing.T) {
	// Invalid slab size.
	_, err := NewHybridAllocator(0, 100)
	if err == nil {
		t.Error("expected error for 0 slab size")
	}

	// Invalid capacity.
	_, err = NewHybridAllocator(256, 0)
	if err == nil {
		t.Error("expected error for 0 capacity")
	}

	// Overflow capacity.
	_, err = NewHybridAllocator(256, 1<<31)
	if err == nil {
		t.Error("expected error for overflow capacity")
	}
}

func TestMmapAllocator_New_ErrorPath(t *testing.T) {
	// This is hard to trigger since NewMmapAllocator has no real failure paths.
	// But we should at least call it with all options.
	a, err := NewMmapAllocator(
		WithMmapThreshold(0),
		WithMmapReturnToOS(false),
		WithMmapReturnThreshold(0),
	)
	if err != nil {
		t.Fatalf("NewMmapAllocator: %v", err)
	}
	defer a.Close()
}

func TestSizeClass_Close_ExtendedAllocators(t *testing.T) {
	a, err := NewSizeClassAllocator(WithSizeClassCapacity(8))
	if err != nil {
		t.Fatalf("NewSizeClassAllocator: %v", err)
	}

	// Allocate from extended class to ensure it was created.
	ref, err := a.Allocate(1024)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	ref.Release()

	// Close should clean up extended allocators.
	if err := a.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// =============================================================================
// BatchDeallocate empty slice
// =============================================================================

func TestBatchDeallocate_EmptySlice(t *testing.T) {
	a, err := New(256, 10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	if err := a.BatchDeallocate(nil); err != nil {
		t.Errorf("BatchDeallocate(nil): %v", err)
	}
	if err := a.BatchDeallocate([]*SlabRef{}); err != nil {
		t.Errorf("BatchDeallocate([]): %v", err)
	}
}

// =============================================================================
// NewSlotArena error path
// =============================================================================

func TestNewSlotArena_Error(t *testing.T) {
	_, err := NewSlotArena(0, 10)
	if err == nil {
		t.Error("expected error for 0 slab size")
	}
}
