// Package slabby provides high-performance slab allocation with enterprise features.
//
// This file implements the Handle API for generation-counted, safe fast-path allocation.
package slabby

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Handle is a generation-counted opaque handle for safe fast-path allocation.
// Handles prevent use-after-free by embedding a generation counter that changes
// each time a slab ID is recycled.
type Handle struct {
	id        uint64 // 32-bit slab ID | 32-bit generation
	allocator *HandleAllocator
}

// Generation counter bit positions
const (
	handleIDBits   = 32
	handleGenBits  = 32
	handleIDMask   = (1 << handleIDBits) - 1
	handleGenShift = handleIDBits
)

// Common errors for handle operations
var (
	ErrInvalidHandle   = errors.New("slabby: invalid handle")
	ErrStaleHandle     = errors.New("slabby: handle generation mismatch - slab was recycled")
	ErrHandleWrongSize = errors.New("slabby: handle size class mismatch")
)

// HandleAllocator provides generation-counted handles for safe fast allocation.
// This API maintains observability while providing near-fast-allocate performance.
type HandleAllocator struct {
	slab        *Slabby
	generations []uint32   // Per-slab generation counter
	freeList    []int32    // Stack of free slab IDs
	freeMu      sync.Mutex // Protects freeList (not the hot path)
	stats       handleStats
}

type handleStats struct {
	allocations   uint64
	deallocations uint64
	genMismatches uint64
	staleAccesses uint64
}

// NewHandleAllocator creates a handle allocator wrapping a slab allocator.
func NewHandleAllocator(slabSize, capacity int, options ...AllocatorOption) (*HandleAllocator, error) {
	slab, err := New(slabSize, capacity, options...)
	if err != nil {
		return nil, err
	}

	return &HandleAllocator{
		slab:        slab,
		generations: make([]uint32, capacity),
		freeList:    make([]int32, 0, capacity),
	}, nil
}

// AllocateHandle allocates a new handle and returns the data pointer.
// The data pointer is valid until FreeHandle is called.
func (a *HandleAllocator) AllocateHandle(size int) (Handle, []byte, error) {
	// Try fast path first - allocate from slab
	data, slabID, err := a.slab.AllocateFast()
	if err != nil {
		// Fall back to standard allocation
		ref, err := a.slab.Allocate()
		if err != nil {
			return Handle{}, nil, err
		}
		gen := atomic.AddUint32(&a.generations[ref.ID()], 1)
		h := Handle{
			id:        (uint64(gen) << handleGenShift) | uint64(ref.ID()),
			allocator: a,
		}
		return h, ref.GetBytes(), nil
	}

	// Fast path succeeded - increment generation for this slab
	gen := atomic.AddUint32(&a.generations[slabID], 1)
	h := Handle{
		id:        (uint64(gen) << handleGenShift) | uint64(slabID),
		allocator: a,
	}

	atomic.AddUint64(&a.stats.allocations, 1)
	return h, data, nil
}

// FreeHandle deallocates a handle, validating the generation counter.
// Returns ErrStaleHandle if the handle has been recycled.
func (a *HandleAllocator) FreeHandle(h Handle) error {
	slabID := int32(h.id & handleIDMask)
	gen := uint32(h.id >> handleGenShift)

	// Validate handle
	if h.allocator != a {
		return ErrInvalidHandle
	}
	if slabID < 0 || slabID >= a.slab.totalCapacity {
		return ErrInvalidHandle
	}

	// Check generation mismatch
	currentGen := a.generations[slabID]
	if gen != currentGen {
		atomic.AddUint64(&a.stats.genMismatches, 1)
		return ErrStaleHandle
	}

	// Increment generation to invalidate any stale handles
	atomic.AddUint32(&a.generations[slabID], 1)

	// Deallocate the slab
	err := a.slab.DeallocateFast(slabID)
	if err == nil {
		atomic.AddUint64(&a.stats.deallocations, 1)
	}
	return err
}

// GetBytes returns the data for a handle, validating the generation.
// Returns ErrStaleHandle if the handle has been recycled.
func (a *HandleAllocator) GetBytes(h Handle) ([]byte, error) {
	slabID := int32(h.id & handleIDMask)
	gen := uint32(h.id >> handleGenShift)

	// Validate handle
	if h.allocator != a {
		return nil, ErrInvalidHandle
	}
	if slabID < 0 || slabID >= a.slab.totalCapacity {
		return nil, ErrInvalidHandle
	}

	// Check generation mismatch
	currentGen := a.generations[slabID]
	if gen != currentGen {
		atomic.AddUint64(&a.stats.staleAccesses, 1)
		return nil, ErrStaleHandle
	}

	// Return the data
	return a.slab.getSlabBytes(slabID), nil
}

// Stats returns handle-specific statistics
func (a *HandleAllocator) Stats() HandleAllocatorStats {
	return HandleAllocatorStats{
		Allocations:   atomic.LoadUint64(&a.stats.allocations),
		Deallocations: atomic.LoadUint64(&a.stats.deallocations),
		GenMismatches: atomic.LoadUint64(&a.stats.genMismatches),
		StaleAccesses: atomic.LoadUint64(&a.stats.staleAccesses),
		SlabStats:     a.slab.Stats(),
	}
}

// HandleAllocatorStats contains statistics for a handle allocator
type HandleAllocatorStats struct {
	Allocations   uint64          `json:"allocations"`
	Deallocations uint64          `json:"deallocations"`
	GenMismatches uint64          `json:"generation_mismatches"`
	StaleAccesses uint64          `json:"stale_accesses"`
	SlabStats     *AllocatorStats `json:"slab_stats"`
}

// ID returns the slab ID portion of the handle (for debugging)
func (h Handle) ID() int32 {
	return int32(h.id & handleIDMask)
}

// Generation returns the generation portion of the handle (for debugging)
func (h Handle) Generation() uint32 {
	return uint32(h.id >> handleGenShift)
}

// Valid returns true if the handle is non-zero
func (h Handle) Valid() bool {
	return h.id != 0 && h.allocator != nil
}

// String returns a string representation of the handle
func (h Handle) String() string {
	return fmt.Sprintf("Handle(id=%d, gen=%d)", h.ID(), h.Generation())
}

// Size returns the slab size for this allocator
func (a *HandleAllocator) Size() int {
	return int(a.slab.slabSize)
}

// Capacity returns the capacity of this allocator
func (a *HandleAllocator) Capacity() int {
	return int(a.slab.totalCapacity)
}

// Close shuts down the handle allocator and releases resources
func (a *HandleAllocator) Close() error {
	return a.slab.Close()
}

// ConcurrentHandleAllocator provides thread-safe handle allocation with reduced contention.
type ConcurrentHandleAllocator struct {
	allocators [][]*HandleAllocator // Sharded by size class
	shardMask  uint32
	stats      concurrentStats
}

type concurrentStats struct {
	allocations     uint64
	deallocations   uint64
	shardContention uint64
}

// NewConcurrentHandleAllocator creates a handle allocator with multiple shards
// to reduce contention under high concurrency.
func NewConcurrentHandleAllocator(slabSize, capacity, shards int) (*ConcurrentHandleAllocator, error) {
	if shards <= 0 {
		shards = 4 // Default shard count
	}

	shardCount := nextPowerOfTwo(uint32(shards))
	allocators := make([][]*HandleAllocator, shardCount)

	for i := uint32(0); i < shardCount; i++ {
		allocators[i] = make([]*HandleAllocator, 1)
		ha, err := NewHandleAllocator(slabSize, capacity/int(shardCount))
		if err != nil {
			// Clean up on failure
			for j := uint32(0); j < i; j++ {
				allocators[j][0].Close()
			}
			return nil, err
		}
		allocators[i][0] = ha
	}

	return &ConcurrentHandleAllocator{
		allocators: allocators,
		shardMask:  shardCount - 1,
	}, nil
}

// AllocateHandle allocates from a randomly selected shard
func (a *ConcurrentHandleAllocator) AllocateHandle(size int) (Handle, []byte, error) {
	shard := uint32(getFastCPUID()) & a.shardMask
	ha := a.allocators[shard][0]

	h, data, err := ha.AllocateHandle(size)
	if err != nil {
		// Contention fallback - try another shard
		for i := uint32(0); i < uint32(len(a.allocators)); i++ {
			otherShard := (shard + i) & a.shardMask
			otherHa := a.allocators[otherShard][0]
			if otherHa != ha {
				h, data, err = otherHa.AllocateHandle(size)
				if err == nil {
					atomic.AddUint64(&a.stats.shardContention, 1)
					break
				}
			}
		}
	}

	if err == nil {
		atomic.AddUint64(&a.stats.allocations, 1)
	}
	return h, data, err
}

// FreeHandle deallocates a handle
func (a *ConcurrentHandleAllocator) FreeHandle(h Handle) error {
	err := h.allocator.FreeHandle(h)
	if err == nil {
		atomic.AddUint64(&a.stats.deallocations, 1)
	}
	return err
}

// GetBytes returns data for a handle
func (a *ConcurrentHandleAllocator) GetBytes(h Handle) ([]byte, error) {
	return h.allocator.GetBytes(h)
}

// Stats returns aggregated statistics
func (a *ConcurrentHandleAllocator) Stats() []HandleAllocatorStats {
	stats := make([]HandleAllocatorStats, len(a.allocators))
	for i := range a.allocators {
		stats[i] = a.allocators[i][0].Stats()
	}
	return stats
}

// Close shuts down all shards
func (a *ConcurrentHandleAllocator) Close() error {
	for i := range a.allocators {
		if a.allocators[i][0] != nil {
			a.allocators[i][0].Close()
		}
	}
	return nil
}
