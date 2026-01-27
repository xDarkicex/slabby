package slabby

import (
	"testing"
)

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
	// Enable per-CPU cache - now with atomic array access
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
	alloc, err := New(64, 1000, WithPCPUCache(true))
	if err != nil {
		b.Fatal(err)
	}
	defer alloc.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Exhaust all slabs then release
			refs := make([]*SlabRef, 0, 100)
			for i := 0; i < 100; i++ {
				ref, _ := alloc.Allocate()
				refs = append(refs, ref)
			}
			for _, ref := range refs {
				alloc.Deallocate(ref)
			}
		}
	})
}
