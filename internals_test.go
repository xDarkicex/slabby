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
// P4: indexedFreeStack.push / pop / reset (slabby.go:1340-1367)
// =============================================================================

func TestIndexedFreeStack_PushPop(t *testing.T) {
	s := newIndexedFreeStack(10)

	// Empty pop should return false.
	if _, ok := s.pop(); ok {
		t.Error("pop on empty stack should return false")
	}

	// Push and pop.
	for i := 0; i < 10; i++ {
		if !s.push(int32(i)) {
			t.Fatalf("push %d failed", i)
		}
	}

	// Overflow: push should return false.
	if s.push(99) {
		t.Error("push on full stack should return false")
	}

	// LIFO order.
	for i := 9; i >= 0; i-- {
		v, ok := s.pop()
		if !ok {
			t.Fatalf("pop %d failed", i)
		}
		if v != int32(i) {
			t.Errorf("expected %d, got %d", i, v)
		}
	}

	// Empty again.
	if _, ok := s.pop(); ok {
		t.Error("pop on exhausted stack should return false")
	}
}

func TestIndexedFreeStack_Reset(t *testing.T) {
	s := newIndexedFreeStack(5)
	for i := 0; i < 5; i++ {
		s.push(int32(i))
	}

	s.reset()

	if _, ok := s.pop(); ok {
		t.Error("pop after reset should return false")
	}

	// Reuse after reset.
	for i := 0; i < 5; i++ {
		if !s.push(int32(i + 100)) {
			t.Fatalf("push after reset failed at %d", i)
		}
	}
	for i := 4; i >= 0; i-- {
		v, _ := s.pop()
		if v != int32(i+100) {
			t.Errorf("expected %d, got %d", i+100, v)
		}
	}
}

func TestIndexedFreeStack_Concurrent(t *testing.T) {
	s := newIndexedFreeStack(5000)

	// Fill from multiple goroutines.
	var wg sync.WaitGroup
	const goroutines = 4
	const perG = 1000

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(base int32) {
			defer wg.Done()
			for i := int32(0); i < perG; i++ {
				for !s.push(base*perG + i) {
					// Busy-wait if full (shouldn't happen with correct sizing).
				}
			}
		}(int32(g))
	}
	wg.Wait()

	// Drain.
	seen := make(map[int32]bool)
	for i := 0; i < goroutines*perG; i++ {
		v, ok := s.pop()
		if !ok {
			t.Fatalf("pop failed at %d", i)
		}
		if seen[v] {
			t.Errorf("duplicate pop: %d", v)
		}
		seen[v] = true
	}
}

// =============================================================================
// P4: perCPUCacheArray.get / put / putInternal (slabby.go:1394-1459)
// =============================================================================

func TestPerCPUCache_PutGet(t *testing.T) {
	slab, err := New(64, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	cache := slab.perCPUCache
	if cache == nil {
		t.Fatal("perCPUCache should be enabled by default")
	}

	// Put a slab ID and get it back.
	if !cache.put(42) {
		t.Error("put should succeed")
	}

	v, ok := cache.get(slab)
	if !ok {
		t.Error("get should succeed after put")
	}
	if v != 42 {
		t.Errorf("expected 42, got %d", v)
	}
}

func TestPerCPUCache_PutFull(t *testing.T) {
	slab, err := New(64, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	cache := slab.perCPUCache

	// Fill one CPU cache completely.
	for i := 0; i < PCCPUCacheSize; i++ {
		if !cache.put(int32(i)) {
			t.Fatalf("put %d failed", i)
		}
	}

	// Next put should fail.
	if cache.put(999) {
		t.Error("put on full cache should return false")
	}
}

func TestPerCPUCache_Concurrent(t *testing.T) {
	slab, err := New(64, 2000)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	cache := slab.perCPUCache

	const goroutines = 8
	const iters = 500

	var wg sync.WaitGroup
	var putFails int64
	var getSuccesses int64

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(base int32) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				id := base*int32(iters) + int32(i)

				// Try to put.
				if cache.put(id) || cache.put(id+1) {
					// success
				}

				// Try to get — may or may not succeed depending on which CPU.
				if _, ok := cache.get(slab); ok {
					atomic.AddInt64(&getSuccesses, 1)
				}
			}
		}(int32(g))
	}
	wg.Wait()

	_ = putFails
	t.Logf("Concurrent cache: %d gets succeeded", getSuccesses)
}

// =============================================================================
// P4: coloredShard.push / pop / tryPop (slabby.go:2444-2480)
// =============================================================================

func TestColoredShard_PushPop(t *testing.T) {
	shard := &coloredShard{}

	// Pop on empty should return false.
	if _, ok := shard.pop(); ok {
		t.Error("pop on empty shard should return false")
	}
	if _, ok := shard.tryPop(); ok {
		t.Error("tryPop on empty shard should return false")
	}

	// Push and pop.
	for i := 0; i < 100; i++ {
		shard.push(int32(i))
	}

	// LIFO order (lock-free stack).
	for i := 99; i >= 0; i-- {
		v, ok := shard.pop()
		if !ok {
			t.Fatalf("pop %d failed", i)
		}
		if v != int32(i) {
			t.Errorf("expected %d, got %d", i, v)
		}
	}

	if _, ok := shard.pop(); ok {
		t.Error("pop after drain should return false")
	}
}

func TestColoredShard_TryPop(t *testing.T) {
	shard := &coloredShard{}
	shard.push(7)

	v, ok := shard.tryPop()
	if !ok {
		t.Fatal("tryPop should succeed when count > 0")
	}
	if v != 7 {
		t.Errorf("expected 7, got %d", v)
	}

	// Second tryPop should see count=0 and return false.
	if _, ok := shard.tryPop(); ok {
		t.Error("tryPop on empty should return false")
	}
}

func TestColoredShard_Concurrent(t *testing.T) {
	shard := &coloredShard{}
	const goroutines = 8
	const iters = 1000

	var wg sync.WaitGroup
	var pushes int64
	var pops int64

	// Pushers.
	for g := 0; g < goroutines/2; g++ {
		wg.Add(1)
		go func(base int32) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				shard.push(base*int32(iters) + int32(i))
				atomic.AddInt64(&pushes, 1)
			}
		}(int32(g))
	}

	// Poppers.
	for g := 0; g < goroutines/2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if _, ok := shard.tryPop(); ok || true {
					// tryPop may fail under contention — just keep going.
				}
				if v, ok := shard.pop(); ok {
					_ = v
					atomic.AddInt64(&pops, 1)
				}
			}
		}()
	}
	wg.Wait()

	t.Logf("Concurrent shard: %d pushes, %d pops", pushes, pops)
}

// =============================================================================
// P4: coloredShardedFreeList.get / put (slabby.go:2484-2528)
// =============================================================================

func TestColoredShardedFreeList_GetPut(t *testing.T) {
	slab, err := New(64, 200)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	cfl := slab.coloredLists

	// Colored list is pre-populated during New(). Drain it first.
	seen := make(map[int32]bool)
	for i := int32(0); i < slab.totalCapacity; i++ {
		v, ok := cfl.get(slab, 0)
		if !ok {
			t.Fatalf("get failed at %d (drained=%d)", i, len(seen))
		}
		if seen[v] {
			t.Errorf("duplicate get: %d", v)
		}
		seen[v] = true
	}

	// Should be exhausted.
	if _, ok := cfl.get(slab, 0); ok {
		t.Error("get on empty colored list should return false")
	}

	// Now put back and get again — verify round-trip.
	for id := range seen {
		cfl.put(id, slab)
	}
	seen2 := make(map[int32]bool)
	for i := 0; i < len(seen); i++ {
		v, ok := cfl.get(slab, 0)
		if !ok {
			t.Fatalf("re-get failed at %d", i)
		}
		if seen2[v] {
			t.Errorf("duplicate on re-get: %d", v)
		}
		seen2[v] = true
	}
}

func TestColoredShardedFreeList_Concurrent(t *testing.T) {
	slab, err := New(64, 2000)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	cfl := slab.coloredLists

	const goroutines = 8
	const iters = 300

	var wg sync.WaitGroup
	var putCount int64
	var getCount int64

	// Pre-populate.
	for i := int32(0); i < slab.totalCapacity; i++ {
		cfl.put(i, slab)
	}

	// Concurrent get/put cycles.
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if v, ok := cfl.get(slab, 0); ok {
					atomic.AddInt64(&getCount, 1)
					cfl.put(v, slab)
					atomic.AddInt64(&putCount, 1)
				}
			}
		}()
	}
	wg.Wait()

	t.Logf("Concurrent colored list: %d gets, %d puts", getCount, putCount)
}

// =============================================================================
// P4: prefetchSlab / prefetchSlabRange / prefetchSliceSafe (slabby.go:1805-1874)
// =============================================================================

func TestPrefetchSlab(t *testing.T) {
	slab, err := New(256, 100)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	// Prefetch a valid slab — should not panic.
	slab.prefetchSlab(0)
	slab.prefetchSlab(99)

	// Prefetch invalid IDs — should not panic (guarded internally).
	slab.prefetchSlab(-1)
	slab.prefetchSlab(100)
	slab.prefetchSlab(99999)
}

func TestPrefetchSlabRange(t *testing.T) {
	slab, err := New(128, 50)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	// Prefetch a valid range.
	slab.prefetchSlabRange(0, 10)

	// Prefetch past the end — should not panic (guarded internally).
	slab.prefetchSlabRange(40, 20)
}

func TestPrefetchSliceSafe(t *testing.T) {
	data := make([]byte, 4096)

	// Normal prefetch.
	prefetchSliceSafe(data, 0, 1024)

	// Edge: zero size.
	prefetchSliceSafe(data, 100, 0)

	// Edge: negative offset.
	prefetchSliceSafe(data, -1, 100)

	// Edge: past end.
	prefetchSliceSafe(data, 4000, 200)
}

// =============================================================================
// P4: checkGuardPages (slabby.go:1877)
// =============================================================================

func TestCheckGuardPages_Clean(t *testing.T) {
	slab, err := New(256, 10, WithGuardPages())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	// Allocate a slab — guard pages should be clean.
	ref, err := slab.Allocate()
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	// checkGuardPages is called during Deallocate.
	err = ref.Release()
	if err != nil {
		t.Errorf("Release with clean guard pages failed: %v", err)
	}
}

// =============================================================================
// P4: zeroSlabMemory (slabby.go:1773)
// =============================================================================

func TestZeroSlabMemory(t *testing.T) {
	slab, err := New(64, 10, WithSecure())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	ref, err := slab.Allocate()
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}
	id := ref.ID()

	// Write a pattern.
	data := ref.GetBytes()
	for i := range data {
		data[i] = 0xFF
	}

	// Release — should zero memory.
	ref.Release()

	// Allocate again and check that memory was zeroed.
	ref2, err := slab.Allocate()
	if err != nil {
		t.Fatalf("re-allocate failed: %v", err)
	}
	defer ref2.Release()

	if ref2.ID() == id {
		data2 := ref2.GetBytes()
		for i, b := range data2 {
			if b != 0 {
				t.Errorf("memory at %d not zeroed: got %x", i, b)
				break
			}
		}
	}
}

// =============================================================================
// P4: createCacheAlignedSlice (slabby.go:2354)
// =============================================================================

func TestCreateCacheAlignedSlice(t *testing.T) {
	for _, lineSize := range []int{64, 128} {
		t.Run(fmt.Sprintf("line=%d", lineSize), func(t *testing.T) {
			data, base := createCacheAlignedSlice(1024, 1, lineSize)

			if len(data) != 1024 {
				t.Errorf("expected length 1024, got %d", len(data))
			}

			// Base should be aligned to cache line.
			if base%uintptr(lineSize) != 0 {
				t.Errorf("base %x not aligned to %d", base, lineSize)
			}

			// First byte address should match the returned base.
			firstAddr := uintptr(unsafe.Pointer(&data[0]))
			if firstAddr != base {
				t.Errorf("first byte addr %x != base %x", firstAddr, base)
			}
		})
	}
}

// =============================================================================
// P4: getImprovedShardIndex / getSlabColor / getColoredShardIndex
//      (slabby.go:2368-2402)
// =============================================================================

func TestGetImprovedShardIndex(t *testing.T) {
	const shardCount = 8

	// Distribution: all indices should be in [0, shardCount).
	for i := int32(0); i < 1000; i++ {
		idx := getImprovedShardIndex(i, shardCount)
		if idx < 0 || idx >= shardCount {
			t.Errorf("shard index %d out of range for slab %d", idx, i)
		}
	}
}

func TestGetSlabColor(t *testing.T) {
	const alignedSize = 64
	colors := make(map[int]int)
	for i := int32(0); i < 1000; i++ {
		c := getSlabColor(i, alignedSize)
		if c < 0 {
			t.Errorf("negative color %d for slab %d", c, i)
		}
		colors[c]++
	}

	t.Logf("Color distribution: %v unique colors across 1000 slabs", len(colors))
}

func TestGetColoredShardIndex(t *testing.T) {
	const shardCount = 8
	const color = 0

	for i := int32(0); i < 1000; i++ {
		idx := getColoredShardIndex(i, shardCount, color)
		if idx < 0 || idx >= shardCount {
			t.Errorf("colored shard index %d out of range for slab %d", idx, i)
		}
	}
}

// =============================================================================
// P4: LeakDetector.captureStack / hashStack / getOrCreateSet / suggestFix
//      (leak_detector.go:226-412)
// =============================================================================

func TestLeakDetector_CaptureStack(t *testing.T) {
	detector := NewLeakDetector()

	hash, full, frames := detector.captureStack()
	if hash == 0 {
		t.Error("hash should be non-zero")
	}
	if full == "" {
		t.Error("full stack should not be empty")
	}
	if len(frames) == 0 {
		t.Error("frames should not be empty")
	}

	t.Logf("captureStack: hash=%x frames=%d", hash, len(frames))
}

func TestLeakDetector_HashStack(t *testing.T) {
	detector := NewLeakDetector()

	h1 := detector.hashStack("test stack trace 1")
	h2 := detector.hashStack("test stack trace 2")
	h3 := detector.hashStack("test stack trace 1") // Same as h1.

	if h1 == 0 {
		t.Error("hash should be non-zero")
	}
	if h1 == h2 {
		t.Error("different stacks should produce different hashes")
	}
	if h1 != h3 {
		t.Error("same stack should produce same hash")
	}
}

func TestLeakDetector_GetOrCreateSet(t *testing.T) {
	detector := NewLeakDetector()

	hash := uint64(0xABCD)
	frames := []string{"foo.go:10", "bar.go:20"}

	set1 := detector.getOrCreateSet(hash, "trace 1", frames)
	set2 := detector.getOrCreateSet(hash, "trace 1", frames) // Same hash — should return existing.

	if set1 != set2 {
		t.Error("same hash should return same pointer")
	}

	set3 := detector.getOrCreateSet(0x1234, "trace 2", frames)
	if set1 == set3 {
		t.Error("different hash should return different set")
	}
}

func TestLeakDetector_SuggestFix(t *testing.T) {
	detector := NewLeakDetector()

	// Empty frames.
	s := detector.suggestFix(&stackSet{frames: nil})
	if s == "" {
		t.Error("suggestFix should return something for empty frames")
	}

	// Normal frames.
	s = detector.suggestFix(&stackSet{
		frames:       []string{"main.go:42", "helper.go:10"},
		netCount:     5,
		allocCount:   10,
		deallocCount: 5,
	})
	if s == "" {
		t.Error("suggestFix should return something")
	}

	// Imbalanced: more deallocs than allocs.
	s = detector.suggestFix(&stackSet{
		frames:       []string{"main.go:42"},
		netCount:     -2,
		allocCount:   3,
		deallocCount: 5,
	})
	if s == "" {
		t.Error("suggestFix should return something for imbalanced case")
	}
}

// =============================================================================
// P4: Slabby.allocateHeapFallback (slabby.go:1628)
// =============================================================================

func TestAllocateHeapFallback(t *testing.T) {
	slab, err := New(256, 2, WithHeapFallback())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	// Exhaust slabs.
	refs := make([]*SlabRef, 2)
	for i := 0; i < 2; i++ {
		ref, err := slab.Allocate()
		if err != nil {
			t.Fatalf("Allocate %d failed: %v", i, err)
		}
		refs[i] = ref
	}

	// Next allocation triggers heap fallback.
	ref, err := slab.Allocate()
	if err != nil {
		t.Fatalf("heap fallback allocate failed: %v", err)
	}
	if !ref.isHeapAlloc {
		t.Error("fallback allocation should be marked isHeapAlloc")
	}
	if ref.slabID != -1 {
		t.Errorf("fallback slabID should be -1, got %d", ref.slabID)
	}

	// Heap fallback data should be usable.
	data := ref.GetBytes()
	data[0] = 0xAB
	data[len(data)-1] = 0xCD

	ref.Release()

	for _, r := range refs {
		r.Release()
	}

	stats := slab.Stats()
	if stats.HeapFallbacks == 0 {
		t.Error("heap fallback stat should be > 0")
	}
}

// =============================================================================
// P4: Slabby.isCircuitBreakerOpen (slabby.go:1946)
// =============================================================================

func TestIsCircuitBreakerOpen_States(t *testing.T) {
	// Without health checks, circuit breaker is nil → always closed.
	slab, err := New(256, 10)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	if slab.isCircuitBreakerOpen() {
		t.Error("nil circuit breaker should report closed")
	}

	// With health checks, starts closed.
	slab2, err := New(256, 10, WithHealthChecks(true),
		WithCircuitBreaker(5, time.Second))
	if err != nil {
		t.Fatalf("New with circuit breaker failed: %v", err)
	}
	defer slab2.Close()

	if slab2.isCircuitBreakerOpen() {
		t.Error("circuit breaker should start closed")
	}
}

// =============================================================================
// P4: Slabby.calculateHealthScore (slabby.go:2061)
// =============================================================================

func TestCalculateHealthScore(t *testing.T) {
	slab, err := New(256, 100,
		WithHealthChecks(true),
		WithHealthInterval(time.Hour), // Don't need ticks.
	)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	// Fresh allocator should have a high health score.
	score := slab.calculateHealthScore()
	if score < 0 || score > 1 {
		t.Errorf("health score out of range: %f", score)
	}
	if score < 0.7 {
		t.Errorf("fresh allocator health score too low: %f", score)
	}
}

// =============================================================================
// P4: Slabby.analyzeTrend (slabby.go:2173)
// =============================================================================

func TestAnalyzeTrend(t *testing.T) {
	slab, err := New(256, 100,
		WithHealthChecks(true),
		WithHealthInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	// All 1.0 scores → stable.
	for i := range slab.healthMetrics.trendHistory {
		slab.healthMetrics.trendHistory[i] = 1.0
	}
	trend := slab.analyzeTrend()
	if trend != "stable" {
		t.Errorf("constant scores should be stable, got %s", trend)
	}

	// Declining scores.
	for i := range slab.healthMetrics.trendHistory {
		slab.healthMetrics.trendHistory[i] = 1.0 - float64(i)*0.1
	}
	trend = slab.analyzeTrend()
	t.Logf("declining trend: %s", trend)

	// Improving scores.
	for i := range slab.healthMetrics.trendHistory {
		slab.healthMetrics.trendHistory[i] = float64(i) * 0.1
	}
	trend = slab.analyzeTrend()
	t.Logf("improving trend: %s", trend)
}

// =============================================================================
// P4: alignToCache / nextPowerOfTwo / nanotime (slabby.go:2562-2576)
// =============================================================================

func TestAlignToCache(t *testing.T) {
	tests := []struct {
		size     int32
		lineSize int
		expected int32
	}{
		{0, 64, 0},
		{1, 64, 64},
		{63, 64, 64},
		{64, 64, 64},
		{65, 64, 128},
		{127, 64, 128},
		{128, 128, 128},
		{129, 128, 256},
	}
	for _, tt := range tests {
		got := alignToCache(tt.size, tt.lineSize)
		if got != tt.expected {
			t.Errorf("alignToCache(%d, %d) = %d, want %d",
				tt.size, tt.lineSize, got, tt.expected)
		}
	}
}

func TestNextPowerOfTwo(t *testing.T) {
	tests := []struct {
		input    uint32
		expected uint32
	}{
		{0, 1},
		{1, 1},
		{2, 2},
		{3, 4},
		{5, 8},
		{8, 8},
		{9, 16},
		{1023, 1024},
		{1024, 1024},
	}
	for _, tt := range tests {
		got := nextPowerOfTwo(tt.input)
		if got != tt.expected {
			t.Errorf("nextPowerOfTwo(%d) = %d, want %d",
				tt.input, got, tt.expected)
		}
	}
}

func TestNanotime(t *testing.T) {
	t1 := nanotime()
	t2 := nanotime()
	if t2 < t1 {
		t.Error("time must be monotonic")
	}
}

// =============================================================================
// P4: fastrand (slabby.go:498)
// =============================================================================

func TestFastrand(t *testing.T) {
	slab, err := New(64, 10)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	// Generate values and check they're not all the same.
	seen := make(map[uint32]bool)
	for i := 0; i < 100; i++ {
		v := slab.fastrand()
		if v == 0 {
			t.Errorf("fastrand returned 0 at iteration %d", i)
		}
		seen[v] = true
	}
	if len(seen) < 90 {
		t.Errorf("fastrand lacks entropy: only %d unique values out of 100", len(seen))
	}
}

func TestFastrand_Concurrent(t *testing.T) {
	slab, err := New(64, 10)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer slab.Close()

	const goroutines = 8
	const iters = 200

	var wg sync.WaitGroup
	seen := sync.Map{}

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				v := slab.fastrand()
				if v == 0 {
					t.Errorf("fastrand returned 0 under concurrency")
				}
				seen.Store(v, true)
			}
		}()
	}
	wg.Wait()
}

// =============================================================================
// P4: getFastCPUID (slabby.go:2538)
// =============================================================================

func TestGetFastCPUID(t *testing.T) {
	// Same goroutine should return consistent values.
	id1 := getFastCPUID()
	id2 := getFastCPUID()

	if id1 == 0 {
		t.Error("getFastCPUID should not return 0")
	}
	if id1 != id2 {
		t.Error("getFastCPUID should be stable within same goroutine")
	}
}

func TestGetFastCPUID_DifferentGoroutines(t *testing.T) {
	var id1, id2 uint64
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		id1 = getFastCPUID()
	}()
	go func() {
		defer wg.Done()
		id2 = getFastCPUID()
	}()
	wg.Wait()

	if id1 == 0 || id2 == 0 {
		t.Error("getFastCPUID should not return 0")
	}
	t.Logf("goroutine 1: %x, goroutine 2: %x", id1, id2)
}
