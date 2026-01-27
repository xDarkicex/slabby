// Package slabby provides high-performance multi-size-class slab allocation
// with enterprise features for production environments.
package slabby

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// Size class count - covers 8B to 8KB with jemalloc-inspired progression
const (
	SizeClassCount        = 19
	DefaultLargeThreshold = 8 * 1024
)

// Size class definitions - jemalloc-inspired progression
var sizeClassTable = [SizeClassCount]int32{
	8, 16, 32, 48, 64, 80, 96, 112, 128, // Tiny (0-8)
	160, 192, 224, 256, 320, 384, 448, 512, // Small (9-16)
	640, 768, // Medium (17-18)
}

// Extended size classes for larger allocations
var extendedSizeClassTable = []int32{
	896, 1024, 1280, 1536, 2048, 3072, 4096, 6144, 8192,
}

// SizeClassInfo contains metadata about a size class
type SizeClassInfo struct {
	ClassIndex   int     `json:"class_index"`
	ClassSize    int32   `json:"class_size"`
	IsTiny       bool    `json:"is_tiny"`
	IsSmall      bool    `json:"is_small"`
	IsMedium     bool    `json:"is_medium"`
	WastePercent float64 `json:"waste_percent"`
}

// GetSizeClassInfo returns metadata for a given size class index
func GetSizeClassInfo(idx int) (SizeClassInfo, error) {
	if idx < 0 || idx >= SizeClassCount {
		return SizeClassInfo{}, fmt.Errorf("slabby: invalid size class index %d", idx)
	}

	size := sizeClassTable[idx]
	return SizeClassInfo{
		ClassIndex:   idx,
		ClassSize:    size,
		IsTiny:       size <= 128,
		IsSmall:      size > 128 && size <= 512,
		IsMedium:     size > 512 && size < 1024,
		WastePercent: 100 * (float64(alignToCache(size, DefaultCacheLine)) - float64(size)) / float64(size),
	}, nil
}

// SizeClassAllocator manages multiple slab allocators
type SizeClassAllocator struct {
	allocators         [SizeClassCount]*Slabby
	sizeToClassLookup  [DefaultLargeThreshold + 1]int8
	extendedAllocators [][]*Slabby
	config             sizeClassConfig
	statsMutex         sync.RWMutex
	largeAllocator     *LargeAllocator
}

// LargeAllocator handles allocations larger than the slab threshold
type LargeAllocator struct {
	threshold      int
	regions        sync.Map
	allocations    uint64
	deallocations  uint64
	bytesAllocated uint64
	activeRegions  int64
}

type largeRegion struct {
	ptr   unsafe.Pointer
	size  int
	class int
}

type sizeClassConfig struct {
	capacity           int
	largeThreshold     int
	enableMetrics      bool
	enableFinalizers   bool
	enableBitGuard     bool
	enableSecure       bool
	enableGuardPages   bool
	enableHealthChecks bool
	enablePCPUCache    bool
	customClasses      []int32
}

type MultiSizeStats struct {
	Version            string       `json:"version"`
	Timestamp          time.Time    `json:"timestamp"`
	TotalAllocators    int          `json:"total_allocators"`
	LargeThreshold     int          `json:"large_threshold"`
	ClassStats         []ClassStats `json:"class_stats"`
	TotalAllocations   uint64       `json:"total_allocations"`
	TotalDeallocations uint64       `json:"total_deallocations"`
	CurrentAllocations uint64       `json:"current_allocations"`
	FastAllocations    uint64       `json:"fast_allocations"`
	TotalAllocTimeNs   uint64       `json:"total_alloc_time_ns"`
	AvgAllocTimeNs     float64      `json:"avg_alloc_time_ns"`
	MaxAllocTimeNs     int64        `json:"max_alloc_time_ns"`
	TotalErrors        uint64       `json:"total_errors"`
	LargeAllocations   uint64       `json:"large_allocations"`
	MemoryEfficiency   float64      `json:"memory_efficiency"`
	FragmentationRatio float64      `json:"fragmentation_ratio"`
}

type ClassStats struct {
	ClassIndex        int     `json:"class_index"`
	ClassSize         int32   `json:"class_size"`
	AlignedSize       int32   `json:"aligned_size"`
	TotalSlabs        int     `json:"total_slabs"`
	UsedSlabs         int     `json:"used_slabs"`
	AvailableSlabs    int     `json:"available_slabs"`
	Allocations       uint64  `json:"allocations"`
	Deallocations     uint64  `json:"deallocations"`
	FastAllocations   uint64  `json:"fast_allocations"`
	AvgAllocTimeNs    float64 `json:"avg_alloc_time_ns"`
	MaxAllocTimeNs    int64   `json:"max_alloc_time_ns"`
	Errors            uint64  `json:"errors"`
	MemoryUtilization float64 `json:"memory_utilization"`
}

type MultiSizeRef struct {
	allocator *SizeClassAllocator
	classIdx  int
	slabRef   *SlabRef
	isLarge   bool
	largePtr  unsafe.Pointer
	size      int
}

type SizeClassOption func(*sizeClassConfig)

func defaultSizeClassConfig() sizeClassConfig {
	return sizeClassConfig{
		capacity:           1024,
		largeThreshold:     DefaultLargeThreshold,
		enableMetrics:      true,
		enableFinalizers:   false,
		enableBitGuard:     false,
		enableSecure:       false,
		enableGuardPages:   false,
		enableHealthChecks: false,
		enablePCPUCache:    true,
	}
}

func WithSizeClassCapacity(capacity int) SizeClassOption {
	return func(c *sizeClassConfig) { c.capacity = capacity }
}

func WithLargeThreshold(threshold int) SizeClassOption {
	return func(c *sizeClassConfig) { c.largeThreshold = threshold }
}

func WithSizeClassBitGuard() SizeClassOption {
	return func(c *sizeClassConfig) { c.enableBitGuard = true }
}

func WithSizeClassSecure() SizeClassOption {
	return func(c *sizeClassConfig) { c.enableSecure = true }
}

func WithSizeClassGuardPages() SizeClassOption {
	return func(c *sizeClassConfig) { c.enableGuardPages = true }
}

func WithSizeClassFinalizers() SizeClassOption {
	return func(c *sizeClassConfig) { c.enableFinalizers = true }
}

func WithSizeClassHealthChecks() SizeClassOption {
	return func(c *sizeClassConfig) { c.enableHealthChecks = true }
}

func WithSizeClassPCPUCache(enabled bool) SizeClassOption {
	return func(c *sizeClassConfig) { c.enablePCPUCache = enabled }
}

// NewSizeClassAllocator creates a new multi-size-class allocator
func NewSizeClassAllocator(options ...SizeClassOption) (*SizeClassAllocator, error) {
	config := defaultSizeClassConfig()
	for _, opt := range options {
		opt(&config)
	}

	if config.capacity <= 0 {
		config.capacity = 1024
	}
	if config.largeThreshold <= 0 {
		config.largeThreshold = DefaultLargeThreshold
	}

	allocator := &SizeClassAllocator{
		config:             config,
		extendedAllocators: make([][]*Slabby, len(extendedSizeClassTable)),
		largeAllocator: &LargeAllocator{
			threshold: config.largeThreshold,
		},
	}

	for size := 0; size <= config.largeThreshold; size++ {
		allocator.sizeToClassLookup[size] = int8(allocator.findSizeClass(int32(size)))
	}

	for i := 0; i < SizeClassCount; i++ {
		size := sizeClassTable[i]
		slab, err := New(int(size), config.capacity,
			WithPCPUCache(config.enablePCPUCache),
			WithHealthChecks(true),
		)
		if err != nil {
			allocator.Close()
			return nil, fmt.Errorf("slabby: failed to create allocator for size class %d: %w", size, err)
		}
		allocator.allocators[i] = slab
	}

	for i, size := range extendedSizeClassTable {
		if int(size) >= config.largeThreshold {
			break
		}
		slab, err := New(int(size), config.capacity,
			WithPCPUCache(config.enablePCPUCache),
		)
		if err != nil {
			allocator.Close()
			return nil, fmt.Errorf("slabby: failed to create allocator for extended size class %d: %w", size, err)
		}
		allocator.extendedAllocators[i] = []*Slabby{slab}
	}

	return allocator, nil
}

func (a *SizeClassAllocator) findSizeClass(size int32) int {
	for i := 0; i < SizeClassCount; i++ {
		if sizeClassTable[i] >= size {
			return i
		}
	}
	for i, sc := range extendedSizeClassTable {
		if sc >= size {
			return SizeClassCount + i
		}
	}
	return -1
}

func (a *SizeClassAllocator) SizeToClass(size int) int {
	if size <= 0 {
		return -1
	}
	if size <= a.config.largeThreshold {
		return int(a.sizeToClassLookup[size])
	}
	return -1
}

func (a *SizeClassAllocator) Allocate(size int) (*MultiSizeRef, error) {
	if size <= 0 {
		return nil, fmt.Errorf("slabby: invalid allocation size %d", size)
	}
	classIdx := a.SizeToClass(size)
	if classIdx >= 0 {
		return a.allocateFromSlab(size, classIdx)
	}
	return a.allocateLarge(size)
}

func (a *SizeClassAllocator) allocateFromSlab(size, classIdx int) (*MultiSizeRef, error) {
	var slabRef *SlabRef
	var err error

	if classIdx < SizeClassCount {
		slabRef, err = a.allocators[classIdx].Allocate()
	} else {
		extIdx := classIdx - SizeClassCount
		if extIdx < len(a.extendedAllocators) && len(a.extendedAllocators[extIdx]) > 0 {
			slabRef, err = a.extendedAllocators[extIdx][0].Allocate()
		} else {
			return nil, fmt.Errorf("slabby: extended size class %d not available", classIdx)
		}
	}

	if err != nil {
		return nil, err
	}

	return &MultiSizeRef{
		allocator: a,
		classIdx:  classIdx,
		slabRef:   slabRef,
		isLarge:   false,
		size:      size,
	}, nil
}

func (a *SizeClassAllocator) allocateLarge(size int) (*MultiSizeRef, error) {
	if size < a.config.largeThreshold {
		return nil, fmt.Errorf("slabby: size %d should use slab allocator", size)
	}

	var classIdx int = SizeClassCount + len(extendedSizeClassTable)
	for i, sc := range extendedSizeClassTable {
		if sc >= int32(size) {
			classIdx = SizeClassCount + i
			break
		}
	}

	ptr, err := a.largeAllocator.Allocate(size)
	if err != nil {
		return nil, err
	}

	return &MultiSizeRef{
		allocator: a,
		classIdx:  classIdx,
		isLarge:   true,
		largePtr:  ptr,
		size:      size,
	}, nil
}

func (a *SizeClassAllocator) AllocateFast(size int) ([]byte, int, error) {
	classIdx := a.SizeToClass(size)
	if classIdx < 0 {
		ptr, err := a.largeAllocator.Allocate(size)
		if err != nil {
			return nil, -1, err
		}
		return unsafe.Slice((*byte)(ptr), size), classIdx, nil
	}

	var slab *Slabby
	if classIdx < SizeClassCount {
		slab = a.allocators[classIdx]
	} else {
		extIdx := classIdx - SizeClassCount
		if extIdx >= len(a.extendedAllocators) || len(a.extendedAllocators[extIdx]) == 0 {
			return nil, -1, fmt.Errorf("slabby: extended size class %d not available", classIdx)
		}
		slab = a.extendedAllocators[extIdx][0]
	}

	data, _, err := slab.AllocateFast()
	if err != nil {
		return nil, -1, err
	}

	return data, classIdx, nil
}

func (a *SizeClassAllocator) DeallocateFast(classIdx int, id int32) error {
	if classIdx < 0 {
		return fmt.Errorf("slabby: invalid class index %d", classIdx)
	}
	if classIdx >= SizeClassCount {
		return fmt.Errorf("slabby: large allocations must use DeallocateLarge")
	}
	return a.allocators[classIdx].DeallocateFast(id)
}

func (a *SizeClassAllocator) DeallocateLarge(ptr unsafe.Pointer) error {
	return a.largeAllocator.Deallocate(ptr)
}

func (r *MultiSizeRef) GetBytes() []byte {
	if r.isLarge {
		return unsafe.Slice((*byte)(r.largePtr), r.size)
	}
	return r.slabRef.GetBytes()
}

func (r *MultiSizeRef) Release() error {
	if r.isLarge {
		return r.allocator.largeAllocator.Deallocate(r.largePtr)
	}
	return r.slabRef.Release()
}

func (r *MultiSizeRef) Size() int {
	return r.size
}

func (r *MultiSizeRef) ClassIndex() int {
	return r.classIdx
}

func (r *MultiSizeRef) IsLarge() bool {
	return r.isLarge
}

func (a *SizeClassAllocator) Stats() *MultiSizeStats {
	a.statsMutex.RLock()
	defer a.statsMutex.RUnlock()

	stats := &MultiSizeStats{
		Version:         Version,
		Timestamp:       time.Now(),
		TotalAllocators: SizeClassCount + len(extendedSizeClassTable),
		LargeThreshold:  a.config.largeThreshold,
		ClassStats:      make([]ClassStats, 0, SizeClassCount+len(extendedSizeClassTable)),
	}

	var totalAllocations, totalDeallocations, totalFastAllocs uint64
	var totalAllocTime uint64
	var maxAllocTime int64
	var totalErrors uint64
	var totalUsedSlabs int

	// Aggregate main size class stats
	for i := 0; i < SizeClassCount; i++ {
		slabStats := a.allocators[i].Stats()
		classStat := ClassStats{
			ClassIndex:        i,
			ClassSize:         sizeClassTable[i],
			TotalSlabs:        slabStats.TotalSlabs,
			UsedSlabs:         slabStats.UsedSlabs,
			AvailableSlabs:    slabStats.AvailableSlabs,
			Allocations:       slabStats.TotalAllocations,
			Deallocations:     slabStats.TotalDeallocations,
			FastAllocations:   slabStats.FastAllocations,
			AvgAllocTimeNs:    slabStats.AvgAllocTimeNs,
			MaxAllocTimeNs:    slabStats.MaxAllocTimeNs,
			Errors:            slabStats.AllocationErrors,
			MemoryUtilization: slabStats.MemoryUtilization,
		}
		stats.ClassStats = append(stats.ClassStats, classStat)

		totalAllocations += slabStats.TotalAllocations
		totalDeallocations += slabStats.TotalDeallocations
		totalFastAllocs += slabStats.FastAllocations
		totalAllocTime += uint64(slabStats.AvgAllocTimeNs * float64(slabStats.TotalAllocations))
		if slabStats.MaxAllocTimeNs > maxAllocTime {
			maxAllocTime = slabStats.MaxAllocTimeNs
		}
		totalErrors += slabStats.AllocationErrors
		totalUsedSlabs += slabStats.UsedSlabs
	}

	// Aggregate extended size class stats
	for i := 0; i < len(a.extendedAllocators); i++ {
		if len(a.extendedAllocators[i]) == 0 {
			continue
		}
		slabStats := a.extendedAllocators[i][0].Stats()
		classStat := ClassStats{
			ClassIndex:        SizeClassCount + i,
			ClassSize:         extendedSizeClassTable[i],
			TotalSlabs:        slabStats.TotalSlabs,
			UsedSlabs:         slabStats.UsedSlabs,
			AvailableSlabs:    slabStats.AvailableSlabs,
			Allocations:       slabStats.TotalAllocations,
			Deallocations:     slabStats.TotalDeallocations,
			FastAllocations:   slabStats.FastAllocations,
			AvgAllocTimeNs:    slabStats.AvgAllocTimeNs,
			MaxAllocTimeNs:    slabStats.MaxAllocTimeNs,
			Errors:            slabStats.AllocationErrors,
			MemoryUtilization: slabStats.MemoryUtilization,
		}
		stats.ClassStats = append(stats.ClassStats, classStat)

		totalAllocations += slabStats.TotalAllocations
		totalDeallocations += slabStats.TotalDeallocations
		totalFastAllocs += slabStats.FastAllocations
		totalAllocTime += uint64(slabStats.AvgAllocTimeNs * float64(slabStats.TotalAllocations))
		if slabStats.MaxAllocTimeNs > maxAllocTime {
			maxAllocTime = slabStats.MaxAllocTimeNs
		}
		totalErrors += slabStats.AllocationErrors
		totalUsedSlabs += slabStats.UsedSlabs
	}

	// Set aggregated stats
	stats.TotalAllocations = totalAllocations
	stats.TotalDeallocations = totalDeallocations
	stats.FastAllocations = totalFastAllocs
	stats.TotalAllocTimeNs = totalAllocTime
	stats.TotalErrors = totalErrors
	stats.CurrentAllocations = uint64(totalUsedSlabs)

	if totalAllocations > 0 {
		stats.AvgAllocTimeNs = float64(totalAllocTime) / float64(totalAllocations)
	}
	stats.MaxAllocTimeNs = maxAllocTime
	stats.LargeAllocations = a.largeAllocator.allocations

	return stats
}

// Close shuts down the allocator and releases resources
func (a *SizeClassAllocator) Close() error {
	// Close all size class allocators
	for i := 0; i < SizeClassCount; i++ {
		if a.allocators[i] != nil {
			a.allocators[i].Close()
		}
	}
	for i := range a.extendedAllocators {
		for j := range a.extendedAllocators[i] {
			if a.extendedAllocators[i][j] != nil {
				a.extendedAllocators[i][j].Close()
			}
		}
	}
	return nil
}

// LargeAllocator methods

// Allocate allocates memory using mmap
func (l *LargeAllocator) Allocate(size int) (unsafe.Pointer, error) {
	// TODO: Implement mmap-based allocation
	// For now, fallback to make slice
	atomic.AddUint64(&l.allocations, 1)
	atomic.AddUint64(&l.bytesAllocated, uint64(size))
	atomic.AddInt64(&l.activeRegions, 1)

	data := make([]byte, size)
	return unsafe.Pointer(&data[0]), nil
}

// Deallocate frees mmap'd memory
func (l *LargeAllocator) Deallocate(ptr unsafe.Pointer) error {
	// TODO: Implement munmap
	atomic.AddUint64(&l.deallocations, 1)
	atomic.AddInt64(&l.activeRegions, -1)
	return nil
}
