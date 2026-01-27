// Package slabby provides high-performance slab allocation with enterprise features.
//
// This file implements mmap-based large allocation support for allocations
// exceeding the slab threshold (default 8KB).
package slabby

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

// MmapAllocator handles large allocations using mmap for memory mapping.
// This provides efficient handling of allocations larger than the slab threshold.
type MmapAllocator struct {
	// Configuration
	pageSize        int
	threshold       int
	returnToOS      bool
	returnThreshold int64 // bytes allocated before returning to OS

	// Tracking
	regions sync.Map // key: ptr, value: *mmapRegion
	stats   mmapStats

	// Platform-specific
	osFuncs mmapOS
}

// mmapRegion tracks an mmap'd memory region
type mmapRegion struct {
	ptr   unsafe.Pointer
	size  int
	class int // Size class for reporting
}

// mmapStats contains allocation statistics
type mmapStats struct {
	allocations    uint64
	deallocations  uint64
	bytesAllocated uint64
	activeRegions  int64
	returnsToOS    uint64
	failedAlloc    uint64
}

// mmapOS contains platform-specific mmap functions
type mmapOS struct {
	mmap    func(length int) (unsafe.Pointer, error)
	munmap  func(addr unsafe.Pointer, length int) error
	madvise func(addr unsafe.Pointer, length int, advice int) error
}

// mmap advice constants (from sys/mman.h)
const (
	madvNormal   = 0
	madvDontNeed = 4
	madvFree     = 8
)

// Common errors
var (
	ErrMmapFailed    = errors.New("slabby: mmap allocation failed")
	ErrMunmapFailed  = errors.New("slabby: munmap failed")
	ErrMadviseFailed = errors.New("slabby: madvise failed")
)

// MmapOption is a functional option for mmap allocator configuration
type MmapOption func(*mmapConfig)

type mmapConfig struct {
	threshold       int
	returnToOS      bool
	returnThreshold int64
}

// DefaultMmapConfig returns the default mmap configuration
func defaultMmapConfig() mmapConfig {
	return mmapConfig{
		threshold:       8 * 1024, // 8KB default threshold
		returnToOS:      true,
		returnThreshold: 64 * 1024 * 1024, // 64MB
	}
}

// WithMmapThreshold sets the size threshold for mmap allocations
func WithMmapThreshold(threshold int) MmapOption {
	return func(c *mmapConfig) { c.threshold = threshold }
}

// WithMmapReturnToOS enables returning memory to the OS
func WithMmapReturnToOS(enabled bool) MmapOption {
	return func(c *mmapConfig) { c.returnToOS = enabled }
}

// WithMmapReturnThreshold sets the threshold for returning memory to OS
func WithMmapReturnThreshold(bytes int64) MmapOption {
	return func(c *mmapConfig) { c.returnThreshold = bytes }
}

// NewMmapAllocator creates a new mmap-based allocator for large allocations
func NewMmapAllocator(options ...MmapOption) (*MmapAllocator, error) {
	config := defaultMmapConfig()
	for _, opt := range options {
		opt(&config)
	}

	// Get platform page size
	pageSize := os.Getpagesize()
	if pageSize < 4096 {
		pageSize = 4096
	}

	allocator := &MmapAllocator{
		pageSize:        pageSize,
		threshold:       config.threshold,
		returnToOS:      config.returnToOS,
		returnThreshold: config.returnThreshold,
		osFuncs:         getMmapOS(),
	}

	return allocator, nil
}

// Allocate allocates memory using mmap
func (a *MmapAllocator) Allocate(size int) (unsafe.Pointer, error) {
	if size <= 0 {
		return nil, fmt.Errorf("slabby: invalid allocation size %d", size)
	}

	// Align size to page size for efficient mmap
	alignedSize := a.alignSize(size)

	// Allocate using mmap
	ptr, err := a.osFuncs.mmap(alignedSize)
	if err != nil {
		atomic.AddUint64(&a.stats.failedAlloc, 1)
		return nil, fmt.Errorf("%w: %v", ErrMmapFailed, err)
	}

	// Store region info
	region := &mmapRegion{
		ptr:   ptr,
		size:  alignedSize,
		class: a.sizeClass(size),
	}
	a.regions.Store(ptr, region)

	// Update stats
	atomic.AddUint64(&a.stats.allocations, 1)
	atomic.AddUint64(&a.stats.bytesAllocated, uint64(alignedSize))
	atomic.AddInt64(&a.stats.activeRegions, 1)

	// Check if we should return memory to OS
	if a.returnToOS {
		a.checkReturnToOS()
	}

	return ptr, nil
}

// Deallocate frees mmap'd memory
func (a *MmapAllocator) Deallocate(ptr unsafe.Pointer) error {
	if ptr == nil {
		return nil
	}

	// LoadAndDelete is atomic - only one goroutine succeeds
	// This prevents double-free race conditions
	value, ok := a.regions.LoadAndDelete(ptr)
	if !ok {
		return fmt.Errorf("slabby: unknown mmap region or already freed")
	}

	r := value.(*mmapRegion)

	// Update stats
	atomic.AddUint64(&a.stats.deallocations, 1)
	atomic.AddUint64(&a.stats.bytesAllocated, ^uint64(0)-uint64(r.size)+1)
	atomic.AddInt64(&a.stats.activeRegions, -1)

	// Return memory to OS via madvise if enabled (best effort, ignore errors)
	if a.returnToOS && r.size >= a.pageSize {
		_ = a.osFuncs.madvise(r.ptr, r.size, madvFree)
		atomic.AddUint64(&a.stats.returnsToOS, 1)
	}

	// Unmap the memory
	return a.osFuncs.munmap(r.ptr, r.size)
}

// Size returns the size of an allocation
func (a *MmapAllocator) Size(ptr unsafe.Pointer) (int, error) {
	region, ok := a.regions.Load(ptr)
	if !ok {
		return 0, fmt.Errorf("slabby: unknown mmap region")
	}
	return region.(*mmapRegion).size, nil
}

// Stats returns allocation statistics
func (a *MmapAllocator) Stats() MmapAllocatorStats {
	return MmapAllocatorStats{
		Allocations:    atomic.LoadUint64(&a.stats.allocations),
		Deallocations:  atomic.LoadUint64(&a.stats.deallocations),
		BytesAllocated: atomic.LoadUint64(&a.stats.bytesAllocated),
		ActiveRegions:  atomic.LoadInt64(&a.stats.activeRegions),
		ReturnsToOS:    atomic.LoadUint64(&a.stats.returnsToOS),
		FailedAlloc:    atomic.LoadUint64(&a.stats.failedAlloc),
	}
}

// MmapAllocatorStats contains statistics for an mmap allocator
type MmapAllocatorStats struct {
	Allocations    uint64 `json:"allocations"`
	Deallocations  uint64 `json:"deallocations"`
	BytesAllocated uint64 `json:"bytes_allocated"`
	ActiveRegions  int64  `json:"active_regions"`
	ReturnsToOS    uint64 `json:"returns_to_os"`
	FailedAlloc    uint64 `json:"failed_allocations"`
}

// Threshold returns the size threshold for mmap allocations
func (a *MmapAllocator) Threshold() int {
	return a.threshold
}

// Private methods

// alignSize rounds up to page size for efficient mmap
func (a *MmapAllocator) alignSize(size int) int {
	if size <= 0 {
		return a.pageSize
	}
	return ((size + a.pageSize - 1) / a.pageSize) * a.pageSize
}

// sizeClass returns a size class for reporting
func (a *MmapAllocator) sizeClass(size int) int {
	if size <= 8*1024 {
		return 0 // 8KB
	} else if size <= 16*1024 {
		return 1 // 16KB
	} else if size <= 32*1024 {
		return 2 // 32KB
	} else if size <= 64*1024 {
		return 3 // 64KB
	} else if size <= 128*1024 {
		return 4 // 128KB
	} else if size <= 256*1024 {
		return 5 // 256KB
	} else if size <= 512*1024 {
		return 6 // 512KB
	} else if size <= 1024*1024 {
		return 7 // 1MB
	}
	return 8 // > 1MB
}

// checkReturnToOS checks if we should advise the OS to reclaim memory
func (a *MmapAllocator) checkReturnToOS() {
	bytes := atomic.LoadUint64(&a.stats.bytesAllocated)
	if int64(bytes) > a.returnThreshold {
		// Suggest free pages to OS
		runtime.GC()
	}
}

// getMmapOS returns platform-specific mmap functions
func getMmapOS() mmapOS {
	return mmapOS{
		mmap:    unixMmap,
		munmap:  unixMunmap,
		madvise: unixMadvise,
	}
}

// Close shuts down the mmap allocator
func (a *MmapAllocator) Close() error {
	return nil
}

// HybridAllocator combines slab and mmap allocation for optimal performance
type HybridAllocator struct {
	slab      *Slabby
	mmap      *MmapAllocator
	threshold int
}

// NewHybridAllocator creates a hybrid allocator using both slabs and mmap
func NewHybridAllocator(slabSize, slabCapacity int, options ...AllocatorOption) (*HybridAllocator, error) {
	slab, err := New(slabSize, slabCapacity, options...)
	if err != nil {
		return nil, err
	}

	mmap, err := NewMmapAllocator()
	if err != nil {
		slab.Close()
		return nil, err
	}

	return &HybridAllocator{
		slab:      slab,
		mmap:      mmap,
		threshold: 8 * 1024, // 8KB default threshold
	}, nil
}

// Allocate allocates memory, routing to slab or mmap based on size
func (a *HybridAllocator) Allocate(size int) ([]byte, int32, error) {
	if size <= 0 {
		return nil, -1, fmt.Errorf("slabby: invalid allocation size %d", size)
	}

	if size <= a.threshold {
		// Use slab allocator
		data, slabID, err := a.slab.AllocateFast()
		return data, slabID, err
	}

	// Use mmap for large allocations
	ptr, err := a.mmap.Allocate(size)
	if err != nil {
		return nil, -1, err
	}

	return unsafe.Slice((*byte)(ptr), size), -1, nil
}

// Deallocate deallocates memory from slab or mmap
func (a *HybridAllocator) Deallocate(data []byte, id int32) error {
	if id >= 0 {
		// Slab allocation
		return a.slab.DeallocateFast(id)
	}

	// Large allocation
	ptr := unsafe.Pointer(&data[0])
	return a.mmap.Deallocate(ptr)
}

// Stats returns combined statistics
func (a *HybridAllocator) Stats() HybridStats {
	return HybridStats{
		SlabStats: a.slab.Stats(),
		MmapStats: a.mmap.Stats(),
	}
}

// HybridStats contains combined statistics for a hybrid allocator
type HybridStats struct {
	SlabStats *AllocatorStats    `json:"slab_stats"`
	MmapStats MmapAllocatorStats `json:"mmap_stats"`
}

// Close shuts down the hybrid allocator
func (a *HybridAllocator) Close() error {
	a.mmap.Close()
	return a.slab.Close()
}

// Size returns the slab size
func (a *HybridAllocator) Size() int {
	return int(a.slab.slabSize)
}

// Capacity returns the slab capacity
func (a *HybridAllocator) Capacity() int {
	return int(a.slab.totalCapacity)
}
