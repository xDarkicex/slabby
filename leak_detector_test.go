package slabby

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =============================================================================
// P1: LeakDetector.OnAllocate (leak_detector.go:153)
// P1: LeakDetector.OnDeallocate (leak_detector.go:185)
// =============================================================================

// TestLeakDetector_OnAllocateOnDeallocate_Basic verifies that sampled
// allocations and deallocations produce correct net counts.
func TestLeakDetector_OnAllocateOnDeallocate_Basic(t *testing.T) {
	detector := NewLeakDetector(LeakDetectorConfig{
		SampleRate:     1, // Sample everything
		ReportInterval: time.Hour,
	})
	detector.Start()
	defer detector.Stop()

	const n = 1000
	for i := 0; i < n; i++ {
		detector.OnAllocate(StateHealthy, 0, true)
	}
	for i := 0; i < n/2; i++ {
		detector.OnDeallocate(StateHealthy, true)
	}

	stats := detector.Stats()
	if stats.TotalAllocs != n {
		t.Errorf("expected %d total allocs, got %d", n, stats.TotalAllocs)
	}
	if stats.TotalDeallocs != n/2 {
		t.Errorf("expected %d total deallocs, got %d", n/2, stats.TotalDeallocs)
	}
	if stats.NetLeaks != n/2 {
		t.Errorf("expected %d net leaks, got %d", n/2, stats.NetLeaks)
	}
}

// TestLeakDetector_OnAllocate_Concurrent fires OnAllocate from multiple
// goroutines simultaneously, verifying atomic counters stay consistent.
func TestLeakDetector_OnAllocate_Concurrent(t *testing.T) {
	detector := NewLeakDetector(LeakDetectorConfig{
		SampleRate:     1, // Sample everything
		ReportInterval: time.Hour,
	})
	detector.Start()
	defer detector.Stop()

	const goroutines = 8
	const iters = 5000

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				detector.OnAllocate(StateHealthy, 100, true)
			}
		}()
	}
	wg.Wait()

	stats := detector.Stats()
	expected := int64(goroutines * iters)
	if stats.TotalAllocs != expected {
		t.Errorf("expected %d total allocs, got %d", expected, stats.TotalAllocs)
	}
	if stats.NetLeaks != expected {
		t.Errorf("expected %d net leaks, got %d", expected, stats.NetLeaks)
	}
}

// TestLeakDetector_OnAllocateOnDeallocate_Concurrent interleaves allocates
// and deallocates from multiple goroutines, verifying net counts.
func TestLeakDetector_OnAllocateOnDeallocate_Concurrent(t *testing.T) {
	detector := NewLeakDetector(LeakDetectorConfig{
		SampleRate:     1, // Sample everything
		ReportInterval: time.Hour,
	})
	detector.Start()
	defer detector.Stop()

	const goroutines = 8
	const pairs = 2000 // each pair = 1 alloc + 1 dealloc

	var wg sync.WaitGroup
	var totalAllocs int64
	var totalDeallocs int64

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < pairs; i++ {
				detector.OnAllocate(StateHealthy, 100, true)
				atomic.AddInt64(&totalAllocs, 1)
				detector.OnDeallocate(StateHealthy, true)
				atomic.AddInt64(&totalDeallocs, 1)
			}
		}()
	}
	wg.Wait()

	stats := detector.Stats()
	if stats.TotalAllocs != totalAllocs {
		t.Errorf("total allocs mismatch: expected %d, got %d", totalAllocs, stats.TotalAllocs)
	}
	if stats.TotalDeallocs != totalDeallocs {
		t.Errorf("total deallocs mismatch: expected %d, got %d", totalDeallocs, stats.TotalDeallocs)
	}
	if stats.NetLeaks != 0 {
		t.Errorf("expected 0 net leaks after balanced alloc/dealloc, got %d", stats.NetLeaks)
	}
}

// TestLeakDetector_OnAllocate_FailedAllocs verifies that failed allocations
// (success=false) are NOT counted.
func TestLeakDetector_OnAllocate_FailedAllocs(t *testing.T) {
	detector := NewLeakDetector(LeakDetectorConfig{
		SampleRate:     1,
		ReportInterval: time.Hour,
	})
	detector.Start()
	defer detector.Stop()

	// Successful allocs.
	for i := 0; i < 100; i++ {
		detector.OnAllocate(StateHealthy, 100, true)
	}
	// Failed allocs — should NOT be counted.
	for i := 0; i < 50; i++ {
		detector.OnAllocate(StateDegraded, 0, false)
	}

	stats := detector.Stats()
	if stats.TotalAllocs != 100 {
		t.Errorf("expected 100 total allocs (ignoring failures), got %d", stats.TotalAllocs)
	}
}

// TestLeakDetector_OnAllocate_WhileStopped verifies that allocations are
// ignored when the detector is not running.
func TestLeakDetector_OnAllocate_WhileStopped(t *testing.T) {
	detector := NewLeakDetector(LeakDetectorConfig{
		SampleRate:     1,
		ReportInterval: time.Hour,
	})
	// Not started — calls should be no-ops.

	for i := 0; i < 100; i++ {
		detector.OnAllocate(StateHealthy, 100, true)
	}
	for i := 0; i < 50; i++ {
		detector.OnDeallocate(StateHealthy, true)
	}

	stats := detector.Stats()
	if stats.TotalAllocs != 0 {
		t.Errorf("expected 0 allocs while stopped, got %d", stats.TotalAllocs)
	}
	if stats.TotalDeallocs != 0 {
		t.Errorf("expected 0 deallocs while stopped, got %d", stats.TotalDeallocs)
	}
}

// TestLeakDetector_Report_GeneratesCorrectly verifies the report structure.
func TestLeakDetector_Report_GeneratesCorrectly(t *testing.T) {
	detector := NewLeakDetector(LeakDetectorConfig{
		SampleRate:     1,
		ReportInterval: time.Hour,
		LeakThreshold:  2,
		AgeThreshold:   1, // 1ns — any real age passes; 0 gets coerced to 5m by constructor
	})
	detector.Start()
	defer detector.Stop()

	// Create a pattern: 3 allocs, 1 dealloc → net=2 at each unique stack
	for i := 0; i < 3; i++ {
		detector.OnAllocate(StateHealthy, 100, true)
	}
	detector.OnDeallocate(StateHealthy, true)

	report := detector.Report()

	if report.TotalAllocs != 3 {
		t.Errorf("expected 3 total allocs in report, got %d", report.TotalAllocs)
	}
	if report.TotalDeallocs != 1 {
		t.Errorf("expected 1 total dealloc in report, got %d", report.TotalDeallocs)
	}
	if report.TotalLeaks != 2 {
		t.Errorf("expected 2 net leaks, got %d", report.TotalLeaks)
	}

	// With LeakThreshold=2 and AgeThreshold=0, we should see leaks.
	if len(report.PotentialLeaks) == 0 {
		t.Errorf("expected potential leaks in report (stacks=%d, threshold=%d)",
			report.UniqueStacks, 2)
	}

	// Verify JSON marshaling.
	data, err := report.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty JSON")
	}
}

// TestLeakDetector_Report_EmptyReturnsNoLeaks verifies an empty detector
// produces a valid empty report.
func TestLeakDetector_Report_EmptyReturnsNoLeaks(t *testing.T) {
	detector := NewLeakDetector()
	report := detector.Report()

	if report.TotalAllocs != 0 || report.TotalDeallocs != 0 {
		t.Error("empty detector should have zero counters")
	}
	if len(report.PotentialLeaks) != 0 {
		t.Error("empty detector should have no leaks")
	}
}

// =============================================================================
// P1: LeakDetector.Clear (leak_detector.go:447)
// =============================================================================

// TestLeakDetector_Clear verifies Clear resets all counters and the
// allocation map.
func TestLeakDetector_Clear(t *testing.T) {
	detector := NewLeakDetector(LeakDetectorConfig{
		SampleRate:     1,
		ReportInterval: time.Hour,
	})
	detector.Start()
	defer detector.Stop()

	for i := 0; i < 500; i++ {
		detector.OnAllocate(StateHealthy, 100, true)
	}
	for i := 0; i < 200; i++ {
		detector.OnDeallocate(StateHealthy, true)
	}

	detector.Clear()

	stats := detector.Stats()
	if stats.TotalAllocs != 0 {
		t.Errorf("expected 0 allocs after clear, got %d", stats.TotalAllocs)
	}
	if stats.TotalDeallocs != 0 {
		t.Errorf("expected 0 deallocs after clear, got %d", stats.TotalDeallocs)
	}
	if stats.NetLeaks != 0 {
		t.Errorf("expected 0 net leaks after clear, got %d", stats.NetLeaks)
	}
	if stats.UniqueStacks != 0 {
		t.Errorf("expected 0 unique stacks after clear, got %d", stats.UniqueStacks)
	}

	// Report should be empty after clear.
	report := detector.Report()
	if len(report.PotentialLeaks) != 0 {
		t.Error("report should be empty after clear")
	}

	// Allocations after clear should work and start from zero.
	for i := 0; i < 10; i++ {
		detector.OnAllocate(StateHealthy, 100, true)
	}
	statsAfter := detector.Stats()
	if statsAfter.TotalAllocs != 10 {
		t.Errorf("expected 10 allocs after clear+reuse, got %d", statsAfter.TotalAllocs)
	}
}

// TestLeakDetector_Clear_ConcurrentWithReport verifies that Clear and Report
// can run concurrently without racing.
func TestLeakDetector_Clear_ConcurrentWithReport(t *testing.T) {
	detector := NewLeakDetector(LeakDetectorConfig{
		SampleRate:     1,
		ReportInterval: time.Hour,
	})
	defer detector.Stop()

	detector.Start()
	// Pre-populate.
	for i := 0; i < 300; i++ {
		detector.OnAllocate(StateHealthy, 100, true)
	}

	var wg sync.WaitGroup
	var reportPanics int64
	var clearPanics int64

	wg.Add(2)
	// Goroutine A: repeatedly call Report.
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				atomic.AddInt64(&reportPanics, 1)
			}
		}()
		for i := 0; i < 200; i++ {
			_ = detector.Report()
		}
	}()

	// Goroutine B: repeatedly call Clear.
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				atomic.AddInt64(&clearPanics, 1)
			}
		}()
		for i := 0; i < 200; i++ {
			detector.Clear()
		}
	}()
	wg.Wait()

	if atomic.LoadInt64(&reportPanics) > 0 {
		t.Error("Report panicked under concurrent Clear")
	}
	if atomic.LoadInt64(&clearPanics) > 0 {
		t.Error("Clear panicked under concurrent Report")
	}
}
