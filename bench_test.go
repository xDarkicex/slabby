package slabby

import (
	"fmt"
	"testing"
)

// =============================================================================
// Core Allocation Benchmarks
// =============================================================================

func BenchmarkAllocateDeallocateLockFree(b *testing.B) {
	alloc, err := New(1024, 10000, WithPCPUCache(true))
	if err != nil {
		b.Fatal(err)
	}
	defer alloc.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var ref *SlabRef
		for pb.Next() {
			ref, _ = alloc.Allocate()
			alloc.Deallocate(ref)
		}
	})
}

func BenchmarkAllocateDeallocateFast(b *testing.B) {
	alloc, err := New(1024, 10000, WithPCPUCache(true))
	if err != nil {
		b.Fatal(err)
	}
	defer alloc.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			data, slabID, _ := alloc.AllocateFast()
			_ = data
			alloc.DeallocateFast(slabID)
		}
	})
}

func BenchmarkHighContention(b *testing.B) {
	alloc, err := New(64, 100000, WithPCPUCache(true)) // Much larger capacity
	if err != nil {
		b.Fatal(err)
	}
	defer alloc.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			refs := make([]*SlabRef, 0, 100)
			for i := 0; i < 100; i++ {
				ref, err := alloc.Allocate()
				if err != nil {
					// Skip this iteration if out of memory
					break
				}
				refs = append(refs, ref)
			}
			for _, ref := range refs {
				alloc.Deallocate(ref)
			}
		}
	})
}

// =============================================================================
// HealthAware Benchmarks (Graceful Degradation)
// =============================================================================

// BenchmarkHealthAware_HealthyState benchmarks allocation in HEALTHY state
func BenchmarkHealthAware_HealthyState(b *testing.B) {
	slab, _ := New(1024, 10000)
	ha := NewHealthAware(slab)
	defer ha.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			data, err := ha.Allocate(1024)
			if err != nil {
				b.Fatal(err)
			}
			_ = data
		}
	})
}

// BenchmarkHealthAware_DegradedState benchmarks allocation in DEGRADED state
func BenchmarkHealthAware_DegradedState(b *testing.B) {
	slab, _ := New(1024, 10000)
	ha := NewHealthAware(slab, HealthConfig{
		PressureThreshold: 0.01, // Very low threshold
		CheckInterval:     1,
	})
	defer ha.Close()

	// Fill to trigger DEGRADED state
	for i := 0; i < 1000; i++ {
		slab.Allocate()
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			data, err := ha.Allocate(1024)
			if err != nil {
				b.Fatal(err)
			}
			_ = data
		}
	})
}

// BenchmarkHealthAware_FastPath benchmarks the fast path allocation
func BenchmarkHealthAware_FastPath(b *testing.B) {
	slab, _ := New(1024, 10000)
	ha := NewHealthAware(slab)
	defer ha.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			data, id, err := ha.AllocateFast()
			if err != nil {
				b.Fatal(err)
			}
			_ = data
			ha.DeallocateFast(id)
		}
	})
}

// BenchmarkHealthAware_StateTransitions benchmarks state transition performance
func BenchmarkHealthAware_StateTransitions(b *testing.B) {
	slab, _ := New(1024, 100)
	ha := NewHealthAware(slab, HealthConfig{
		PressureThreshold: 0.5,
		CriticalThreshold: 0.7,
		FallbackThreshold: 0.9,
		RecoveryThreshold: 0.3,
		CheckInterval:     1,
	})
	defer ha.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Fill to trigger transitions
			for i := 0; i < 80; i++ {
				slab.Allocate()
			}
			// Drain to trigger recovery
			stats := slab.Stats()
			for i := 0; i < int(stats.TotalAllocations); i++ {
				slab.DeallocateFast(int32(i))
			}
		}
	})
}

// BenchmarkHealthAware_ObserverOverhead measures observer callback overhead
func BenchmarkHealthAware_ObserverOverhead(b *testing.B) {
	slab, _ := New(1024, 10000)
	ha := NewHealthAware(slab)

	// Add a no-op observer
	ha.RegisterObserver(&noopObserver{})

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			data, _, _ := ha.AllocateFast()
			_ = data
		}
	})
}

// noopObserver is a no-op observer for benchmarking
type noopObserver struct{}

func (n *noopObserver) OnStateChange(prev, curr HealthState, reason string)         {}
func (n *noopObserver) OnMetricsSnapshot(snapshot HealthSnapshot)                   {}
func (n *noopObserver) OnAllocate(state HealthState, latencyNs int64, success bool) {}
func (n *noopObserver) OnDeallocate(state HealthState, success bool)                {}

// =============================================================================
// LeakDetector Benchmarks
// =============================================================================

// BenchmarkLeakDetector_Sampling measures sampling overhead
func BenchmarkLeakDetector_Sampling(b *testing.B) {
	slab, _ := New(1024, 10000)
	ha := NewHealthAware(slab)
	detector := NewLeakDetector(LeakDetectorConfig{
		SampleRate:     1,    // Sample all for benchmark
		ReportInterval: 1000, // Very long interval
	})
	detector.RegisterWith(ha)
	detector.Start()
	defer detector.Stop()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			data, _, _ := ha.AllocateFast()
			_ = data
		}
	})
}

// BenchmarkLeakDetector_LowSampling measures low sampling rate overhead
func BenchmarkLeakDetector_LowSampling(b *testing.B) {
	slab, _ := New(1024, 10000)
	ha := NewHealthAware(slab)
	detector := NewLeakDetector(LeakDetectorConfig{
		SampleRate:     100, // 1% sampling
		ReportInterval: 1000,
	})
	detector.RegisterWith(ha)
	detector.Start()
	defer detector.Stop()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			data, _, _ := ha.AllocateFast()
			_ = data
		}
	})
}

// BenchmarkLeakDetector_ReportGeneration measures report generation time
func BenchmarkLeakDetector_ReportGeneration(b *testing.B) {
	slab, _ := New(1024, 10000)
	ha := NewHealthAware(slab)
	detector := NewLeakDetector(LeakDetectorConfig{
		SampleRate:     10, // 10% sampling
		ReportInterval: 1000,
	})
	detector.RegisterWith(ha)
	detector.Start()

	// Generate some allocations
	for i := 0; i < 10000; i++ {
		data, _, _ := ha.AllocateFast()
		data[0] = byte(i)
		if i%2 == 0 {
			ha.DeallocateFast(int32(i))
		}
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = detector.Report()
		}
	})
}

// =============================================================================
// Comparison Benchmarks (Slabby vs Go Standard Library)
// =============================================================================

// BenchmarkCompareWithGoMake compares slab allocation with Go's make()
func BenchmarkCompareWithGoMake(b *testing.B) {
	b.Run("SlabAllocator", func(b *testing.B) {
		alloc, _ := New(1024, 100000, WithPCPUCache(true)) // Larger capacity
		defer alloc.Close()

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				ref, err := alloc.Allocate()
				if err != nil {
					b.Fatal(err)
				}
				_ = ref.GetBytes()
				alloc.Deallocate(ref)
			}
		})
	})

	b.Run("StandardMake", func(b *testing.B) {
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				data := make([]byte, 1024)
				_ = data
			}
		})
	})
}

// =============================================================================
// Varying Size Benchmarks
// =============================================================================

// BenchmarkAllocate_VaryingSizes benchmarks allocation with different sizes
func BenchmarkAllocate_VaryingSizes(b *testing.B) {
	sizes := []int{64, 256, 1024, 4096, 8192}

	for _, size := range sizes {
		b.Run("Size="+fmt.Sprintf("%d", size), func(b *testing.B) {
			alloc, _ := New(size, 100000) // Larger capacity
			defer alloc.Close()

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					ref, err := alloc.Allocate()
					if err != nil {
						b.Fatal(err)
					}
					_ = ref.GetBytes()
					alloc.Deallocate(ref)
				}
			})
		})
	}
}

// BenchmarkFastAllocate_VaryingSizes benchmarks fast allocation with different sizes
func BenchmarkFastAllocate_VaryingSizes(b *testing.B) {
	sizes := []int{64, 256, 1024, 4096, 8192}

	for _, size := range sizes {
		b.Run("Size="+fmt.Sprintf("%d", size), func(b *testing.B) {
			alloc, _ := New(size, 10000)
			defer alloc.Close()

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					data, id, _ := alloc.AllocateFast()
					_ = data
					alloc.DeallocateFast(id)
				}
			})
		})
	}
}

// =============================================================================
// Concurrency Benchmarks
// =============================================================================

// BenchmarkConcurrentAllocation benchmarks concurrent allocation from multiple goroutines
func BenchmarkConcurrentAllocation(b *testing.B) {
	alloc, _ := New(1024, 100000, WithPCPUCache(true), WithShards(16))
	defer alloc.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ref, err := alloc.Allocate()
			if err != nil {
				b.Fatal(err)
			}
			_ = ref.GetBytes()
			alloc.Deallocate(ref)
		}
	})
}

// BenchmarkMixedWorkload benchmarks a realistic mixed workload
func BenchmarkMixedWorkload(b *testing.B) {
	alloc, _ := New(1024, 100000) // Larger capacity
	defer alloc.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// 70% fast path, 20% standard, 10% batch
			r := uint32(b.N)
			switch r % 10 {
			case 0, 1, 2, 3, 4, 5, 6: // 70%
				data, id, err := alloc.AllocateFast()
				if err != nil {
					continue
				}
				_ = data
				alloc.DeallocateFast(id)
			case 7, 8: // 20%
				ref, err := alloc.Allocate()
				if err != nil {
					continue
				}
				_ = ref.GetBytes()
				alloc.Deallocate(ref)
			default: // 10%
				refs, err := alloc.BatchAllocate(4)
				if err != nil {
					continue
				}
				for _, ref := range refs {
					alloc.Deallocate(ref)
				}
			}
		}
	})
}

// =============================================================================
// Health Metrics Benchmarks
// =============================================================================

// BenchmarkHealthMetrics_Collection measures health metrics collection overhead
func BenchmarkHealthMetrics_Collection(b *testing.B) {
	slab, _ := New(1024, 10000)
	ha := NewHealthAware(slab)
	defer ha.Close()

	// Generate some activity
	for i := 0; i < 5000; i++ {
		data, _, _ := ha.AllocateFast()
		data[0] = byte(i)
		if i%3 == 0 {
			ha.DeallocateFast(int32(i))
		}
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = ha.HealthMetrics()
		}
	})
}

// BenchmarkHealthMetrics_Snapshot measures snapshot generation overhead
func BenchmarkHealthMetrics_Snapshot(b *testing.B) {
	slab, _ := New(1024, 10000)
	ha := NewHealthAware(slab)
	defer ha.Close()

	// Generate some activity
	for i := 0; i < 5000; i++ {
		data, _, _ := ha.AllocateFast()
		data[0] = byte(i)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = ha.GetSnapshot()
		}
	})
}

// =============================================================================
// Mmap/Hybrid Benchmarks (using exported names to avoid duplicates)
// =============================================================================

// BenchmarkMmapOpsAllocation benchmarks mmap-based allocation
func BenchmarkMmapOpsAllocation(b *testing.B) {
	mmap, _ := NewMmapAllocator()
	defer mmap.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ptr, _ := mmap.Allocate(16384)
			_ = ptr
			mmap.Deallocate(ptr)
		}
	})
}

// BenchmarkMmapOpsHybridAllocation benchmarks hybrid slab+mmap allocation
func BenchmarkMmapOpsHybridAllocation(b *testing.B) {
	hybrid, _ := NewHybridAllocator(1024, 10000)
	defer hybrid.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Mix of small (slab) and large (mmap) allocations
			size := 4096 + (b.N%7)*4096
			data, id, _ := hybrid.Allocate(size)
			_ = data
			hybrid.Deallocate(data, id)
		}
	})
}

// =============================================================================
// Slot API Benchmarks
// =============================================================================

func BenchmarkSlotArenaOpsVsFastAllocate(b *testing.B) {
	b.Run("FastAllocate", func(b *testing.B) {
		slab, _ := New(1024, 10000)
		defer slab.Close()

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				data, id, _ := slab.AllocateFast()
				_ = data
				slab.DeallocateFast(id)
			}
		})
	})

	b.Run("SlotArena", func(b *testing.B) {
		arena, _ := NewSlotArena(1024, 10000)
		defer arena.Close()

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				slot, data, _ := arena.AllocateSlot()
				_ = data
				arena.FreeSlot(slot)
			}
		})
	})
}

// =============================================================================
// Handle API Benchmarks
// =============================================================================

// BenchmarkHandleOpsAllocation benchmarks handle-based allocation
func BenchmarkHandleOpsAllocation(b *testing.B) {
	ha, _ := NewHandleAllocator(1024, 10000)
	defer ha.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			handle, data, _ := ha.AllocateHandle(1024)
			_ = data
			ha.FreeHandle(handle)
		}
	})
}

// BenchmarkHandleOpsVsFastAllocate compares handle API with fast allocate
func BenchmarkHandleOpsVsFastAllocate(b *testing.B) {
	b.Run("FastAllocate", func(b *testing.B) {
		slab, _ := New(1024, 10000)
		defer slab.Close()

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				data, id, _ := slab.AllocateFast()
				_ = data
				slab.DeallocateFast(id)
			}
		})
	})

	b.Run("HandleAllocate", func(b *testing.B) {
		ha, _ := NewHandleAllocator(1024, 10000)
		defer ha.Close()

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				handle, data, _ := ha.AllocateHandle(1024)
				_ = data
				ha.FreeHandle(handle)
			}
		})
	})
}
