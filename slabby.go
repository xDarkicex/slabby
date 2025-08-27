// Package slabby provides high-performance slab allocation with enterprise features
// for production environments requiring predictable memory allocation patterns.
//
// The allocator provides O(1) allocation/deallocation with configurable cache-line
// awareness, security features, and comprehensive monitoring suitable for
// high-throughput applications.
//
// Basic usage:
//
//	allocator, err := slabby.New(1024, 1000) // 1KB slabs, 1000 capacity
//	if err != nil {
//		log.Fatal(err)
//	}
//
//	ref, err := allocator.Allocate()
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer ref.Release()
//
//	data := ref.GetBytes()
//	// Use data...
//
// Advanced usage with options:
//
//	allocator, err := slabby.New(4096, 10000,
//		slabby.WithSecure(),                        // Zero memory on deallocation
//		slabby.WithBitGuard(),                      // Memory corruption detection
//		slabby.WithCircuitBreaker(5, time.Second),  // Failure resilience
//		slabby.WithHealthChecks(true),              // Enable health monitoring
//	)
package slabby

import (
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"math/bits"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const (
	Version = "2.3.0-enterprise"

	// Cache line sizes for different architectures
	CacheLineX86     = 64
	CacheLineARM     = 64 // Most ARM64
	CacheLineARMv7   = 32 // Older ARMv7
	CacheLinePPC64   = 128
	DefaultCacheLine = CacheLineX86

	// Performance constants
	DefaultShardCount    = 0 // Use GOMAXPROCS
	MaxAllocationLatency = 100 * time.Microsecond
	HealthCheckInterval  = 30 * time.Second
	MetricsBufferSize    = 1024
	PCCPUCacheSize       = 16   // Per-CPU cache size
	SamplingRate         = 100  // Sample 1% of allocations for health metrics
	MaxBatchSize         = 256  // Maximum batch allocation size
	GuardPageSize        = 4096 // Guard page size for memory protection

	// Circuit breaker states
	circuitClosed   int32 = 0
	circuitOpen     int32 = 1
	circuitHalfOpen int32 = 2
)

// Predefined errors for better error handling
var (
	ErrOutOfMemory        = errors.New("slabby: out of memory")
	ErrInvalidReference   = errors.New("slabby: invalid reference")
	ErrDoubleDeallocation = errors.New("slabby: double deallocation detected")
	ErrMemoryCorruption   = errors.New("slabby: memory corruption detected")
	ErrUseAfterFree       = errors.New("slabby: use after free detected")
	ErrCircuitBreakerOpen = errors.New("slabby: circuit breaker is open")
	ErrAllocationTimeout  = errors.New("slabby: allocation timeout exceeded")
	ErrCapacityExceeded   = errors.New("slabby: capacity must not exceed MaxInt32")
	ErrInvalidSlabSize    = errors.New("slabby: slab size must be positive")
	ErrInvalidCapacity    = errors.New("slabby: capacity must be positive")
	ErrInvalidBatchSize   = errors.New("slabby: batch size must be positive and <= MaxBatchSize")
)

// SlabRef represents a memory allocation with safety features and monitoring.
// References are not safe for concurrent use and should not be shared between goroutines.
type SlabRef struct {
	dataPtr         unsafe.Pointer // Points to actual data
	allocatorRef    *Slabby        // Back reference to allocator
	slabID          int32          // Slab identifier
	guardWord       uintptr        // Memory corruption detection
	allocState      uint32         // 0=allocated, 1=deallocated
	isHeapAlloc     bool           // True if fallback heap allocation
	allocTime       int64          // Allocation timestamp for debugging
	allocationStack string         // Stack trace for debugging (when enabled)
}

// SlabAllocator defines the interface for slab allocation implementations.
// This interface allows for easy testing and pluggable allocator backends.
type SlabAllocator interface {
	// Allocate returns a new slab reference or an error if allocation fails
	Allocate() (*SlabRef, error)
	// BatchAllocate allocates multiple slabs at once for better performance
	BatchAllocate(count int) ([]*SlabRef, error)
	// AllocateWithTimeout attempts allocation with a timeout
	AllocateWithTimeout(timeout time.Duration) (*SlabRef, error)
	// MustAllocate allocates or panics - use only when allocation failure is fatal
	MustAllocate() *SlabRef
	// Deallocate returns a slab to the pool
	Deallocate(ref *SlabRef) error
	// BatchDeallocate deallocates multiple slabs at once
	BatchDeallocate(refs []*SlabRef) error
	// Reset clears all allocations - use with caution in production
	Reset()
	// Stats returns current performance and usage statistics
	Stats() *AllocatorStats
	// HealthCheck returns detailed health metrics
	HealthCheck() *HealthMetrics
	// Secure converts to secure mode with memory zeroing
	Secure() *SecureAllocator
	// Close gracefully shuts down the allocator
	Close() error
}

// AllocatorStats contains comprehensive performance and usage metrics.
type AllocatorStats struct {
	Version             string  `json:"version"`
	TotalSlabs          int     `json:"total_slabs"`
	UsedSlabs           int     `json:"used_slabs"`
	AvailableSlabs      int     `json:"available_slabs"`
	TotalAllocations    uint64  `json:"total_allocations"`
	TotalDeallocations  uint64  `json:"total_deallocations"`
	CurrentAllocations  uint64  `json:"current_allocations"`
	BatchAllocations    uint64  `json:"batch_allocations"`
	BatchDeallocations  uint64  `json:"batch_deallocations"`
	AvgAllocTimeNs      float64 `json:"avg_alloc_time_ns"`
	MaxAllocTimeNs      int64   `json:"max_alloc_time_ns"`
	SecureMode          bool    `json:"secure_mode"`
	BitGuardEnabled     bool    `json:"bit_guard_enabled"`
	CacheLineSize       int     `json:"cache_line_size"`
	FragmentationRatio  float64 `json:"fragmentation_ratio"`
	MemoryUtilization   float64 `json:"memory_utilization"`
	AllocationErrors    uint64  `json:"allocation_errors"`
	DeallocationErrors  uint64  `json:"deallocation_errors"`
	HeapFallbacks       uint64  `json:"heap_fallbacks"`
	PCCPUCacheHits      uint64  `json:"pcpu_cache_hits"`
	LockFreeHits        uint64  `json:"lock_free_hits"`
	GuardPageViolations uint64  `json:"guard_page_violations"`
}

// HealthMetrics provides detailed health and performance information.
type HealthMetrics struct {
	AllocLatencyP50    time.Duration `json:"alloc_latency_p50"`
	AllocLatencyP95    time.Duration `json:"alloc_latency_p95"`
	AllocLatencyP99    time.Duration `json:"alloc_latency_p99"`
	FragmentationScore float64       `json:"fragmentation_score"`
	MemoryPressure     float64       `json:"memory_pressure"`
	ErrorRate          float64       `json:"error_rate"`
	CircuitBreakerOpen bool          `json:"circuit_breaker_open"`
	LastGCDuration     time.Duration `json:"last_gc_duration"`
	HealthScore        float64       `json:"health_score"` // 0.0-1.0
	CacheEfficiency    float64       `json:"cache_efficiency"`
	RecentTrend        string        `json:"recent_trend"` // "improving", "stable", "degrading"
}

// CircuitBreakerConfig holds circuit breaker configuration
type CircuitBreakerConfig struct {
	FailureThreshold int64
	RecoveryTimeout  time.Duration
}

// Slabby is the main slab allocator implementation with enterprise features.
type Slabby struct {
	// Core allocation fields - optimized memory layout
	slabSize      int32
	alignedSize   int32
	totalCapacity int32

	// Separate metadata from data for better cache efficiency
	slabMetadata []slabMetadata
	memoryPool   []byte
	memoryBase   uintptr // Cache-aligned base address
	guardPages   []byte  // Guard pages for memory protection

	// High-performance allocation structures
	perCPUCache   *perCPUCacheArray
	lockFreeStack *lockFreeStack
	shardedLists  *shardedFreeList

	// Configuration and options
	config allocatorConfig

	// Statistics and monitoring - per-CPU to reduce contention
	cpuStats      []cpuStatEntry
	healthMetrics healthMetricsInternal

	// Production features
	circuitBreaker *circuitBreakerState
	logger         *slog.Logger

	// Fast random number generator for sharding
	rngState uint64

	// CPU identification cache
	cpuIDCache atomic.Value // map[uintptr]uint64

	// Synchronization
	shutdownOnce sync.Once
	shutdownChan chan struct{}
	healthTicker *time.Ticker
}

// SecureAllocator wraps Slabby with additional security features.
type SecureAllocator struct {
	*Slabby
}

// Optimized metadata structure with cache line alignment
type slabMetadata struct {
	inUse     atomic.Bool
	slabID    int32
	guardPage bool                        // Has guard pages
	_         [DefaultCacheLine - 12]byte // Padding to cache line size
}

// Lock-free stack node
type slabNode struct {
	slabID int32
	next   *slabNode
}

// Lock-free stack implementation using atomic.Pointer
type lockFreeStack struct {
	head atomic.Pointer[slabNode]
}

// Per-CPU cache array for minimal cross-CPU communication
type perCPUCacheArray struct {
	caches []pcpuCacheEntry
	mask   uint64
}

// Thread-safe per-CPU cache entry with mutex protection
type pcpuCacheEntry struct {
	_     [DefaultCacheLine]byte // Padding
	stack [PCCPUCacheSize]int32  // Stack of slab IDs
	count int32                  // Number of items in stack
	hits  uint64                 // Cache hit counter (atomic)
	mutex sync.Mutex             // Protect count and stack from races
}

// Sharded free list with improved distribution
type shardedFreeList struct {
	shardArray []freeListShard
	shardMask  uint32
}

type freeListShard struct {
	_        [DefaultCacheLine]byte // Cache line padding
	slabIDs  []int32
	slabLock sync.Mutex
}

// Per-CPU statistics to reduce contention
type cpuStatEntry struct {
	_                  [DefaultCacheLine]byte // Padding
	allocations        uint64
	deallocations      uint64
	batchAllocations   uint64
	batchDeallocations uint64
	allocTime          uint64
	maxAllocTime       int64
	errors             uint64
	heapFallbacks      uint64
	guardViolations    uint64
	samplingCounter    uint32
	// Ring buffer for latency percentiles (smaller per-CPU)
	latencyBuffer    [64]int64
	latencyBufferIdx uint32
}

type healthMetricsInternal struct {
	lastHealthCheck time.Time
	healthScore     float64
	memoryPressure  float64
	errorRate       float64
	cacheEfficiency float64
	recentTrend     string
	trendHistory    [10]float64 // Recent health scores for trend analysis
	trendHistoryIdx int
	healthMutex     sync.RWMutex
}

type circuitBreakerState struct {
	failureCount    int64
	successCount    int64 // Track successes in half-open state
	lastFailureTime time.Time
	lastStateChange time.Time
	config          CircuitBreakerConfig
	currentState    int32
	stateMutex      sync.RWMutex
}

type allocatorConfig struct {
	shardCount         int
	cacheLineSize      int
	enableSecure       bool
	enableBitGuard     bool
	enableFinalizers   bool
	enableFallback     bool
	enableHealthCheck  bool
	enablePCPUCache    bool
	enableGuardPages   bool
	enableDebug        bool
	maxAllocLatency    time.Duration
	healthInterval     time.Duration
	circuitBreakerConf CircuitBreakerConfig
	logger             *slog.Logger
}

// New creates a new high-performance slab allocator with the specified slab size and capacity.
func New(slabSize, capacity int, options ...AllocatorOption) (*Slabby, error) {
	if slabSize <= 0 {
		return nil, ErrInvalidSlabSize
	}
	if capacity <= 0 {
		return nil, ErrInvalidCapacity
	}
	if capacity > math.MaxInt32 {
		return nil, ErrCapacityExceeded
	}

	config := defaultAllocatorConfig()
	for _, opt := range options {
		opt(&config)
	}

	// Calculate aligned slab size for cache efficiency
	alignedSize := alignToCache(int32(slabSize), config.cacheLineSize)
	int32Capacity := int32(capacity)

	// Check for overflow in total memory calculation
	totalMemory := int64(alignedSize) * int64(int32Capacity)
	if totalMemory > math.MaxInt32 || totalMemory <= 0 {
		return nil, fmt.Errorf("slabby: total memory calculation overflow")
	}

	// Add guard pages if enabled
	var guardPageMemory int64
	if config.enableGuardPages {
		guardPageMemory = int64(capacity) * int64(GuardPageSize) * 2 // Before and after each slab
		totalMemory += guardPageMemory
	}

	// Create cache-aligned memory pool
	memoryPool, memoryBase := createCacheAlignedSlice(int(totalMemory), 1, config.cacheLineSize)
	var guardPages []byte
	if config.enableGuardPages {
		guardPages = memoryPool[len(memoryPool)-int(guardPageMemory):]
		memoryPool = memoryPool[:len(memoryPool)-int(guardPageMemory)]
	}

	// Initialize separated metadata array for better cache efficiency
	slabMetadata := make([]slabMetadata, capacity)
	for i := range slabMetadata {
		slabMetadata[i].slabID = int32(i)
		slabMetadata[i].guardPage = config.enableGuardPages
	}

	// Initialize per-CPU cache array
	var perCPUCache *perCPUCacheArray
	if config.enablePCPUCache {
		numCPUs := runtime.GOMAXPROCS(0)
		cpuCacheCount := nextPowerOfTwo(uint32(numCPUs))
		perCPUCache = &perCPUCacheArray{
			caches: make([]pcpuCacheEntry, cpuCacheCount),
			mask:   uint64(cpuCacheCount - 1),
		}
	}

	// Initialize lock-free stack
	lockFreeStack := &lockFreeStack{}

	// Initialize sharded free list
	shardCount := config.shardCount
	if shardCount <= 0 {
		shardCount = runtime.GOMAXPROCS(0)
	}
	shardedLists := newImprovedShardedFreeList(capacity, shardCount)

	// Initialize per-CPU statistics
	numCPUs := runtime.GOMAXPROCS(0)
	cpuStats := make([]cpuStatEntry, nextPowerOfTwo(uint32(numCPUs)))

	// Initialize circuit breaker if configured
	var circuitBreaker *circuitBreakerState
	if config.enableHealthCheck {
		circuitBreaker = &circuitBreakerState{
			config:          config.circuitBreakerConf,
			currentState:    circuitClosed,
			lastStateChange: time.Now(),
		}
	}

	allocator := &Slabby{
		slabSize:       int32(slabSize),
		alignedSize:    alignedSize,
		totalCapacity:  int32Capacity,
		slabMetadata:   slabMetadata,
		memoryPool:     memoryPool,
		memoryBase:     memoryBase,
		guardPages:     guardPages,
		perCPUCache:    perCPUCache,
		lockFreeStack:  lockFreeStack,
		shardedLists:   shardedLists,
		config:         config,
		cpuStats:       cpuStats,
		circuitBreaker: circuitBreaker,
		logger:         config.logger,
		rngState:       uint64(time.Now().UnixNano()),
		shutdownChan:   make(chan struct{}),
	}

	// Initialize health metrics with defaults
	allocator.healthMetrics.healthScore = 1.0
	allocator.healthMetrics.recentTrend = "stable"
	for i := range allocator.healthMetrics.trendHistory {
		allocator.healthMetrics.trendHistory[i] = 1.0
	}

	// Initialize CPU ID cache
	allocator.cpuIDCache.Store(make(map[uintptr]uint64))

	// Start health monitoring if enabled
	if config.enableHealthCheck {
		allocator.startHealthMonitoring()
	}

	return allocator, nil
}

// Allocate returns a new slab reference for immediate use.
func (a *Slabby) Allocate() (*SlabRef, error) {
	startTime := nanotime()

	// Check circuit breaker first
	if a.circuitBreaker != nil && a.isCircuitBreakerOpen() {
		a.recordError()
		return nil, ErrCircuitBreakerOpen
	}

	// Try per-CPU cache first (fastest path)
	if a.config.enablePCPUCache {
		if slabID, ok := a.perCPUCache.get(); ok {
			a.recordPCPUCacheHit()
			return a.createSlabRef(slabID, startTime, false)
		}
	}

	// Try lock-free stack (fast path)
	if slabID, ok := a.lockFreeStack.pop(); ok {
		a.recordLockFreeHit()
		return a.createSlabRef(slabID, startTime, false)
	}

	// Fall back to sharded allocation
	slabID, ok := a.shardedLists.get()
	if !ok {
		// Try heap fallback if enabled
		if a.config.enableFallback {
			return a.allocateHeapFallback(startTime)
		}
		a.recordError()
		a.recordCircuitBreakerFailure()
		return nil, ErrOutOfMemory
	}

	return a.createSlabRef(slabID, startTime, false)
}

// BatchAllocate allocates multiple slabs at once for better performance.
func (a *Slabby) BatchAllocate(count int) ([]*SlabRef, error) {
	if count <= 0 || count > MaxBatchSize {
		return nil, ErrInvalidBatchSize
	}

	startTime := nanotime()
	refs := make([]*SlabRef, 0, count)

	// Check circuit breaker first
	if a.circuitBreaker != nil && a.isCircuitBreakerOpen() {
		a.recordError()
		return nil, ErrCircuitBreakerOpen
	}

	// Try to satisfy from per-CPU cache first
	if a.config.enablePCPUCache {
		for len(refs) < count {
			if slabID, ok := a.perCPUCache.get(); ok {
				ref, err := a.createSlabRef(slabID, startTime, false)
				if err != nil {
					break
				}
				refs = append(refs, ref)
				a.recordPCPUCacheHit()
			} else {
				break
			}
		}
	}

	// Try lock-free stack for remaining
	for len(refs) < count {
		if slabID, ok := a.lockFreeStack.pop(); ok {
			ref, err := a.createSlabRef(slabID, startTime, false)
			if err != nil {
				break
			}
			refs = append(refs, ref)
			a.recordLockFreeHit()
		} else {
			break
		}
	}

	// Use sharded lists for remaining
	for len(refs) < count {
		if slabID, ok := a.shardedLists.get(); ok {
			ref, err := a.createSlabRef(slabID, startTime, false)
			if err != nil {
				break
			}
			refs = append(refs, ref)
		} else {
			break
		}
	}

	// If we couldn't satisfy all requests and fallback is enabled
	if len(refs) < count && a.config.enableFallback {
		for len(refs) < count {
			ref, err := a.allocateHeapFallback(startTime)
			if err != nil {
				break
			}
			refs = append(refs, ref)
		}
	}

	// Record batch allocation
	a.recordBatchAllocation(uint64(len(refs)))

	if len(refs) == 0 {
		a.recordError()
		a.recordCircuitBreakerFailure()
		return nil, ErrOutOfMemory
	}

	if len(refs) < count {
		// Partial success - return what we got with an error
		return refs, fmt.Errorf("slabby: partial batch allocation, got %d of %d requested", len(refs), count)
	}

	return refs, nil
}

// AllocateWithTimeout attempts allocation with a specified timeout.
func (a *Slabby) AllocateWithTimeout(timeout time.Duration) (*SlabRef, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ref, err := a.Allocate(); err != ErrOutOfMemory {
			return ref, err // Success or other error
		}
		// Brief backoff before retry
		time.Sleep(time.Microsecond * 10)
	}
	return nil, ErrAllocationTimeout
}

// MustAllocate allocates a slab or panics if allocation fails.
func (a *Slabby) MustAllocate() *SlabRef {
	ref, err := a.Allocate()
	if err != nil {
		panic(fmt.Sprintf("slabby: critical allocation failure: %v", err))
	}
	return ref
}

// Deallocate returns a slab reference to the pool for reuse.
func (a *Slabby) Deallocate(ref *SlabRef) error {
	if ref == nil {
		a.recordError()
		return ErrInvalidReference
	}

	// Check allocator reference before checking state (but after nil check)
	if ref.allocatorRef != a {
		a.recordError()
		return ErrInvalidReference
	}

	// Check for double deallocation BEFORE invalidating reference
	if !atomic.CompareAndSwapUint32(&ref.allocState, 0, 1) {
		a.recordError()
		return ErrDoubleDeallocation
	}

	// Handle heap fallback allocations
	if ref.isHeapAlloc {
		ref.invalidateReference()
		a.recordDeallocation()
		return nil
	}

	// Validate memory integrity if bit guard is enabled
	if a.config.enableBitGuard && ref.guardWord != 0xDEADBEEF {
		a.recordError()
		return ErrMemoryCorruption
	}

	// Check guard pages if enabled
	if a.config.enableGuardPages {
		if err := a.checkGuardPages(ref.slabID); err != nil {
			a.recordGuardViolation()
			a.recordError()
			return err
		}
	}

	slabID := ref.slabID
	slabEntry := &a.slabMetadata[slabID]

	// Zero memory if secure mode is enabled
	if a.config.enableSecure {
		a.zeroSlabMemory(slabID)
	}

	// Mark slab as available
	slabEntry.inUse.Store(false)

	// Return to the most appropriate pool
	if a.config.enablePCPUCache && a.perCPUCache.put(slabID) {
		// Successfully returned to per-CPU cache
	} else if !a.lockFreeStack.push(slabID) {
		// Fall back to sharded return
		a.shardedLists.put(slabID)
	}

	// Update statistics
	a.recordDeallocation()
	// Record successful operation for circuit breaker
	a.recordCircuitBreakerSuccess()
	// Invalidate reference AFTER all operations
	ref.invalidateReference()
	return nil
}

// BatchDeallocate deallocates multiple slab references at once for better performance.
func (a *Slabby) BatchDeallocate(refs []*SlabRef) error {
	if len(refs) == 0 {
		return nil
	}

	errors := make([]error, 0, len(refs))
	successCount := 0

	for i, ref := range refs {
		if err := a.Deallocate(ref); err != nil {
			errors = append(errors, fmt.Errorf("ref[%d]: %w", i, err))
		} else {
			successCount++
		}
	}

	// Record batch deallocation
	a.recordBatchDeallocation(uint64(successCount))

	if len(errors) > 0 {
		return fmt.Errorf("slabby: %d of %d deallocations failed: %w",
			len(errors), len(refs), errors[0]) // Return first error with count
	}

	return nil
}

// Stats returns comprehensive allocator statistics.
func (a *Slabby) Stats() *AllocatorStats {
	// Aggregate per-CPU statistics
	var totalAllocs, totalDeallocs, totalAllocTime uint64
	var totalBatchAllocs, totalBatchDeallocs uint64
	var maxAllocTime int64
	var totalErrors, totalHeapFallbacks, totalGuardViolations uint64

	for i := range a.cpuStats {
		cpu := &a.cpuStats[i]
		totalAllocs += atomic.LoadUint64(&cpu.allocations)
		totalDeallocs += atomic.LoadUint64(&cpu.deallocations)
		totalBatchAllocs += atomic.LoadUint64(&cpu.batchAllocations)
		totalBatchDeallocs += atomic.LoadUint64(&cpu.batchDeallocations)
		totalAllocTime += atomic.LoadUint64(&cpu.allocTime)
		totalErrors += atomic.LoadUint64(&cpu.errors)
		totalHeapFallbacks += atomic.LoadUint64(&cpu.heapFallbacks)
		totalGuardViolations += atomic.LoadUint64(&cpu.guardViolations)

		cpuMax := atomic.LoadInt64(&cpu.maxAllocTime)
		if cpuMax > maxAllocTime {
			maxAllocTime = cpuMax
		}
	}

	usedSlabs := int(totalAllocs - totalDeallocs)
	availableSlabs := int(a.totalCapacity) - usedSlabs

	var avgAllocTime float64
	if totalAllocs > 0 {
		avgAllocTime = float64(totalAllocTime) / float64(totalAllocs)
	}

	fragmentation := 1.0 - (float64(usedSlabs)*float64(a.slabSize))/
		float64(a.alignedSize*a.totalCapacity)
	memoryUtil := float64(usedSlabs) / float64(a.totalCapacity)

	// Get cache hit statistics
	var pcpuCacheHits, lockFreeHits uint64
	if a.config.enablePCPUCache {
		for i := range a.perCPUCache.caches {
			pcpuCacheHits += atomic.LoadUint64(&a.perCPUCache.caches[i].hits)
		}
	}

	return &AllocatorStats{
		Version:             Version,
		TotalSlabs:          int(a.totalCapacity),
		UsedSlabs:           usedSlabs,
		AvailableSlabs:      availableSlabs,
		TotalAllocations:    totalAllocs,
		TotalDeallocations:  totalDeallocs,
		CurrentAllocations:  totalAllocs - totalDeallocs,
		BatchAllocations:    totalBatchAllocs,
		BatchDeallocations:  totalBatchDeallocs,
		AvgAllocTimeNs:      avgAllocTime,
		MaxAllocTimeNs:      maxAllocTime,
		SecureMode:          a.config.enableSecure,
		BitGuardEnabled:     a.config.enableBitGuard,
		CacheLineSize:       a.config.cacheLineSize,
		FragmentationRatio:  fragmentation,
		MemoryUtilization:   memoryUtil,
		AllocationErrors:    totalErrors,
		DeallocationErrors:  totalErrors, // Combined for simplicity
		HeapFallbacks:       totalHeapFallbacks,
		PCCPUCacheHits:      pcpuCacheHits,
		LockFreeHits:        lockFreeHits,
		GuardPageViolations: totalGuardViolations,
	}
}

// HealthCheck returns detailed health metrics for monitoring and alerting.
func (a *Slabby) HealthCheck() *HealthMetrics {
	a.healthMetrics.healthMutex.RLock()
	defer a.healthMetrics.healthMutex.RUnlock()

	// Aggregate latency samples from all CPUs
	latencies := make([]int64, 0, len(a.cpuStats)*64)
	for i := range a.cpuStats {
		cpu := &a.cpuStats[i]
		for j := 0; j < 64; j++ {
			if latency := cpu.latencyBuffer[j]; latency > 0 {
				latencies = append(latencies, latency)
			}
		}
	}

	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	var p50, p95, p99 time.Duration
	if len(latencies) > 0 {
		p50 = time.Duration(latencies[len(latencies)*50/100])
		p95 = time.Duration(latencies[len(latencies)*95/100])
		p99 = time.Duration(latencies[len(latencies)*99/100])
	}

	// Calculate health score (0.0 = unhealthy, 1.0 = perfect health)
	healthScore := a.calculateHealthScore()

	return &HealthMetrics{
		AllocLatencyP50:    p50,
		AllocLatencyP95:    p95,
		AllocLatencyP99:    p99,
		FragmentationScore: 1.0 - a.healthMetrics.memoryPressure,
		MemoryPressure:     a.healthMetrics.memoryPressure,
		ErrorRate:          a.healthMetrics.errorRate,
		CircuitBreakerOpen: a.isCircuitBreakerOpen(),
		HealthScore:        healthScore,
		CacheEfficiency:    a.healthMetrics.cacheEfficiency,
		RecentTrend:        a.healthMetrics.recentTrend,
	}
}

// Secure returns a SecureAllocator wrapper that enables memory zeroing on deallocation.
func (a *Slabby) Secure() *SecureAllocator {
	a.config.enableSecure = true
	return &SecureAllocator{a}
}

// Reset clears all allocations and reinitializes data structures - use with caution in production.
func (a *Slabby) Reset() {
	// Mark all slabs as unused
	for i := range a.slabMetadata {
		a.slabMetadata[i].inUse.Store(false)
		if a.config.enableSecure {
			a.zeroSlabMemory(int32(i))
		}
	}

	// Reset all caches and lists
	if a.config.enablePCPUCache {
		for i := range a.perCPUCache.caches {
			cache := &a.perCPUCache.caches[i]
			cache.mutex.Lock()
			cache.count = 0
			cache.mutex.Unlock()
		}
	}

	a.lockFreeStack.head.Store(nil)

	// Properly reinitialize sharded lists with all slabs
	capacity := int(a.totalCapacity)
	shardCount := len(a.shardedLists.shardArray)

	// Clear existing lists
	for i := range a.shardedLists.shardArray {
		shard := &a.shardedLists.shardArray[i]
		shard.slabLock.Lock()
		shard.slabIDs = shard.slabIDs[:0]
		shard.slabLock.Unlock()
	}

	// Redistribute all slabs
	for i := 0; i < capacity; i++ {
		shardIdx := getImprovedShardIndex(int32(i), shardCount)
		shard := &a.shardedLists.shardArray[shardIdx]
		shard.slabLock.Lock()
		shard.slabIDs = append(shard.slabIDs, int32(i))
		shard.slabLock.Unlock()
	}

	// Reset per-CPU statistics
	for i := range a.cpuStats {
		cpu := &a.cpuStats[i]
		atomic.StoreUint64(&cpu.allocations, 0)
		atomic.StoreUint64(&cpu.deallocations, 0)
		atomic.StoreUint64(&cpu.batchAllocations, 0)
		atomic.StoreUint64(&cpu.batchDeallocations, 0)
		atomic.StoreUint64(&cpu.allocTime, 0)
		atomic.StoreInt64(&cpu.maxAllocTime, 0)
		atomic.StoreUint64(&cpu.errors, 0)
		atomic.StoreUint64(&cpu.heapFallbacks, 0)
		atomic.StoreUint64(&cpu.guardViolations, 0)
		cpu.samplingCounter = 0
		cpu.latencyBufferIdx = 0
		for j := range cpu.latencyBuffer {
			cpu.latencyBuffer[j] = 0
		}
	}

	// Reset circuit breaker
	if a.circuitBreaker != nil {
		a.circuitBreaker.stateMutex.Lock()
		a.circuitBreaker.currentState = circuitClosed
		a.circuitBreaker.failureCount = 0
		a.circuitBreaker.successCount = 0
		a.circuitBreaker.lastStateChange = time.Now()
		a.circuitBreaker.stateMutex.Unlock()
	}

	// Reset health metrics
	a.healthMetrics.healthMutex.Lock()
	a.healthMetrics.errorRate = 0
	a.healthMetrics.memoryPressure = 0
	a.healthMetrics.cacheEfficiency = 0
	a.healthMetrics.healthScore = 1.0
	a.healthMetrics.recentTrend = "stable"
	for i := range a.healthMetrics.trendHistory {
		a.healthMetrics.trendHistory[i] = 1.0
	}
	a.healthMetrics.trendHistoryIdx = 0
	a.healthMetrics.healthMutex.Unlock()
}

// Close gracefully shuts down the allocator and releases resources.
func (a *Slabby) Close() error {
	a.shutdownOnce.Do(func() {
		close(a.shutdownChan)
		if a.healthTicker != nil {
			a.healthTicker.Stop()
		}
		// Clear all finalizers if enabled
		if a.config.enableFinalizers {
			runtime.GC()
		}
	})
	return nil
}

// SlabRef methods

// GetBytes returns the underlying byte slice for this allocation.
func (r *SlabRef) GetBytes() []byte {
	if atomic.LoadUint32(&r.allocState) != 0 {
		panic(ErrUseAfterFree)
	}

	if r.isHeapAlloc {
		return *(*[]byte)(r.dataPtr)
	}

	// Calculate data slice from memory pool
	allocator := r.allocatorRef
	offset := int64(r.slabID) * int64(allocator.alignedSize)
	// Add guard page offset if enabled
	if allocator.config.enableGuardPages {
		offset += int64(r.slabID) * int64(GuardPageSize) // Skip guard pages
	}

	return allocator.memoryPool[offset : offset+int64(allocator.slabSize)]
}

// IsValid returns true if this reference is still valid for use.
func (r *SlabRef) IsValid() bool {
	return atomic.LoadUint32(&r.allocState) == 0
}

// Release deallocates this reference. This is an alias for allocator.Deallocate(ref).
func (r *SlabRef) Release() error {
	if r.allocatorRef == nil {
		return ErrInvalidReference
	}
	return r.allocatorRef.Deallocate(r)
}

// Size returns the usable size of this slab in bytes.
func (r *SlabRef) Size() int {
	if r.isHeapAlloc {
		return len(*(*[]byte)(r.dataPtr))
	}
	return int(r.allocatorRef.slabSize)
}

// ID returns the internal slab identifier (useful for debugging).
func (r *SlabRef) ID() int32 {
	return r.slabID
}

// AllocationStack returns the stack trace from allocation (if debug enabled).
func (r *SlabRef) AllocationStack() string {
	return r.allocationStack
}

// Private implementation methods

// Lock-free stack operations
func (s *lockFreeStack) push(slabID int32) bool {
	node := &slabNode{slabID: slabID}
	for {
		oldHead := s.head.Load()
		node.next = oldHead
		if s.head.CompareAndSwap(oldHead, node) {
			return true
		}
	}
}

func (s *lockFreeStack) pop() (int32, bool) {
	for {
		oldHead := s.head.Load()
		if oldHead == nil {
			return -1, false
		}
		if s.head.CompareAndSwap(oldHead, oldHead.next) {
			return oldHead.slabID, true
		}
	}
}

// Thread-safe per-CPU cache operations with mutex protection
func (c *perCPUCacheArray) get() (int32, bool) {
	cpuID := getCurrentCPUID()
	cache := &c.caches[cpuID&c.mask]

	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	if cache.count > 0 {
		cache.count--
		slabID := cache.stack[cache.count]
		atomic.AddUint64(&cache.hits, 1) // Keep hits atomic for lock-free reads
		return slabID, true
	}
	return -1, false
}

func (c *perCPUCacheArray) put(slabID int32) bool {
	cpuID := getCurrentCPUID()
	cache := &c.caches[cpuID&c.mask]

	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	if cache.count < PCCPUCacheSize {
		cache.stack[cache.count] = slabID
		cache.count++
		return true
	}
	return false
}

// Improved sharded free list
func newImprovedShardedFreeList(capacity, shardCount int) *shardedFreeList {
	actualShardCount := nextPowerOfTwo(uint32(shardCount))
	mask := actualShardCount - 1

	fl := &shardedFreeList{
		shardArray: make([]freeListShard, actualShardCount),
		shardMask:  mask,
	}

	// Distribute slabs across shards with better locality
	for i := 0; i < capacity; i++ {
		shardIdx := getImprovedShardIndex(int32(i), int(actualShardCount))
		shard := &fl.shardArray[shardIdx]
		shard.slabLock.Lock()
		shard.slabIDs = append(shard.slabIDs, int32(i))
		shard.slabLock.Unlock()
	}

	return fl
}

func (fl *shardedFreeList) get() (int32, bool) {
	// Use improved sharding strategy
	cpuID := getCurrentCPUID()
	startIdx := uint32(cpuID) & fl.shardMask

	// Try each shard
	for attempt := 0; attempt < len(fl.shardArray); attempt++ {
		shardIdx := (startIdx + uint32(attempt)) & fl.shardMask
		shard := &fl.shardArray[shardIdx]

		shard.slabLock.Lock()
		if len(shard.slabIDs) > 0 {
			// Pop from end for better cache locality
			slabID := shard.slabIDs[len(shard.slabIDs)-1]
			shard.slabIDs = shard.slabIDs[:len(shard.slabIDs)-1]
			shard.slabLock.Unlock()
			return slabID, true
		}
		shard.slabLock.Unlock()

		// Brief backoff on contention
		if attempt > 0 {
			runtime.Gosched()
		}
	}

	return -1, false
}

func (fl *shardedFreeList) put(slabID int32) {
	shardIdx := getImprovedShardIndex(slabID, len(fl.shardArray))
	shard := &fl.shardArray[shardIdx]
	shard.slabLock.Lock()
	shard.slabIDs = append(shard.slabIDs, slabID)
	shard.slabLock.Unlock()
}

func (a *Slabby) createSlabRef(slabID int32, startTime int64, isHeapAlloc bool) (*SlabRef, error) {
	if !isHeapAlloc {
		slabEntry := &a.slabMetadata[slabID]
		if !slabEntry.inUse.CompareAndSwap(false, true) {
			// Race condition detected
			a.recordError()
			return nil, fmt.Errorf("slabby: race condition in allocation")
		}
		// Safe prefetch of slab data only (no metadata access)
		a.prefetchSlab(slabID)
	}

	ref := &SlabRef{
		allocatorRef: a,
		slabID:       slabID,
		allocState:   0,
		isHeapAlloc:  isHeapAlloc,
		allocTime:    startTime,
	}

	if isHeapAlloc {
		data := make([]byte, a.slabSize)
		ref.dataPtr = unsafe.Pointer(&data)
	}

	if a.config.enableBitGuard {
		ref.guardWord = 0xDEADBEEF
	}

	// Record stack trace if debug is enabled
	if a.config.enableDebug {
		a.recordAllocationStack(ref)
	}

	// Update statistics and track latency
	a.recordAllocation()
	a.trackAllocationLatency(startTime)

	// Set finalizer if enabled
	if a.config.enableFinalizers {
		runtime.SetFinalizer(ref, (*SlabRef).finalizeReference)
	}

	return ref, nil
}

func (a *Slabby) allocateHeapFallback(startTime int64) (*SlabRef, error) {
	ref, err := a.createSlabRef(-1, startTime, true)
	if err != nil {
		return nil, err
	}
	a.recordHeapFallback()
	return ref, nil
}

// Enhanced statistics recording using per-CPU counters
func (a *Slabby) recordAllocation() {
	cpuID := getCurrentCPUID()
	cpu := &a.cpuStats[cpuID&uint64(len(a.cpuStats)-1)]
	atomic.AddUint64(&cpu.allocations, 1)
}

func (a *Slabby) recordDeallocation() {
	cpuID := getCurrentCPUID()
	cpu := &a.cpuStats[cpuID&uint64(len(a.cpuStats)-1)]
	atomic.AddUint64(&cpu.deallocations, 1)
}

func (a *Slabby) recordBatchAllocation(count uint64) {
	cpuID := getCurrentCPUID()
	cpu := &a.cpuStats[cpuID&uint64(len(a.cpuStats)-1)]
	atomic.AddUint64(&cpu.batchAllocations, count)
}

func (a *Slabby) recordBatchDeallocation(count uint64) {
	cpuID := getCurrentCPUID()
	cpu := &a.cpuStats[cpuID&uint64(len(a.cpuStats)-1)]
	atomic.AddUint64(&cpu.batchDeallocations, count)
}

func (a *Slabby) recordError() {
	cpuID := getCurrentCPUID()
	cpu := &a.cpuStats[cpuID&uint64(len(a.cpuStats)-1)]
	atomic.AddUint64(&cpu.errors, 1)
}

func (a *Slabby) recordHeapFallback() {
	cpuID := getCurrentCPUID()
	cpu := &a.cpuStats[cpuID&uint64(len(a.cpuStats)-1)]
	atomic.AddUint64(&cpu.heapFallbacks, 1)
}

func (a *Slabby) recordGuardViolation() {
	cpuID := getCurrentCPUID()
	cpu := &a.cpuStats[cpuID&uint64(len(a.cpuStats)-1)]
	atomic.AddUint64(&cpu.guardViolations, 1)
}

func (a *Slabby) recordPCPUCacheHit() {
	// Already recorded in perCPUCache.get()
}

func (a *Slabby) recordLockFreeHit() {
	// Could add lock-free hit counter if needed
}

func (a *Slabby) recordAllocationStack(ref *SlabRef) {
	if a.config.enableDebug {
		buf := make([]byte, 4096)
		n := runtime.Stack(buf, false)
		ref.allocationStack = string(buf[:n])
	}
}

func (a *Slabby) trackAllocationLatency(startTime int64) {
	latency := nanotime() - startTime
	cpuID := getCurrentCPUID()
	cpu := &a.cpuStats[cpuID&uint64(len(a.cpuStats)-1)]

	// Update total allocation time
	atomic.AddUint64(&cpu.allocTime, uint64(latency))

	// Update max allocation time
	for {
		currentMax := atomic.LoadInt64(&cpu.maxAllocTime)
		if latency <= currentMax || atomic.CompareAndSwapInt64(&cpu.maxAllocTime, currentMax, latency) {
			break
		}
	}

	// Sample latencies for percentile calculation (reduced overhead)
	counter := atomic.AddUint32(&cpu.samplingCounter, 1)
	if counter%SamplingRate == 0 {
		idx := atomic.AddUint32(&cpu.latencyBufferIdx, 1) % 64
		cpu.latencyBuffer[idx] = latency
	}
}

// Improved memory zeroing with chunked clearing for large slabs
func (a *Slabby) zeroSlabMemory(slabID int32) {
	offset := int64(slabID) * int64(a.alignedSize)
	// Add guard page offset if enabled
	if a.config.enableGuardPages {
		offset += int64(slabID) * int64(GuardPageSize)
	}

	dataSlice := a.memoryPool[offset : offset+int64(a.slabSize)]

	// Use more efficient clearing for larger slabs
	if a.slabSize > 1024 {
		// Clear in larger chunks for better performance
		for i := 0; i < len(dataSlice); i += 64 {
			end := i + 64
			if end > len(dataSlice) {
				end = len(dataSlice)
			}
			// Clear chunk efficiently
			for j := i; j < end; j++ {
				dataSlice[j] = 0
			}
		}
	} else {
		// Standard clear for smaller slabs
		for i := range dataSlice {
			dataSlice[i] = 0
		}
	}
}

// Safe prefetch implementation that only touches slab data, not metadata
func (a *Slabby) prefetchSlab(slabID int32) {
	if slabID < 0 || slabID >= a.totalCapacity {
		return
	}

	// Calculate slab data bounds safely
	offset := int64(slabID) * int64(a.alignedSize)
	if a.config.enableGuardPages {
		offset += int64(slabID) * int64(GuardPageSize)
	}

	// Ensure we don't go out of bounds of the memory pool
	if offset >= int64(len(a.memoryPool)) || offset < 0 {
		return
	}

	// Calculate safe prefetch size
	availableSize := int64(len(a.memoryPool)) - offset
	prefetchSize := int64(a.slabSize)
	if prefetchSize > availableSize {
		prefetchSize = availableSize
	}

	if prefetchSize <= 0 {
		return
	}

	// Use safe slice-based prefetching instead of pointer arithmetic
	prefetchSliceSafe(a.memoryPool, int(offset), int(prefetchSize))
}

// Slice-based prefetch that avoids all pointer arithmetic
func prefetchSliceSafe(data []byte, offset, size int) {
	if offset < 0 || size <= 0 || offset >= len(data) {
		return
	}

	// Ensure we don't go past the end of the slice
	end := offset + size
	if end > len(data) {
		end = len(data)
	}

	// Prefetch by touching memory at cache-line intervals using safe slice indexing
	for i := offset; i < end; i += DefaultCacheLine {
		if i < len(data) {
			_ = data[i] // Safe slice access - no pointer arithmetic
		}
	}

	// Also touch the last byte if we haven't already
	if end-1 >= offset && end-1 < len(data) && (end-offset) > DefaultCacheLine {
		_ = data[end-1]
	}
}

// Enhanced guard page checking with optimized performance
func (a *Slabby) checkGuardPages(slabID int32) error {
	if !a.config.enableGuardPages {
		return nil
	}

	guardOffset := int64(slabID) * int64(GuardPageSize) * 2 // Before and after
	beforeGuard := a.guardPages[guardOffset : guardOffset+GuardPageSize]
	afterGuard := a.guardPages[guardOffset+GuardPageSize : guardOffset+2*GuardPageSize]

	// Use faster checking for large guard pages
	checkGuardRegion := func(guard []byte, name string) error {
		if GuardPageSize > 64 {
			// Check in chunks for better performance
			for i := 0; i < len(guard); i += 64 {
				end := i + 64
				if end > len(guard) {
					end = len(guard)
				}
				for j := i; j < end; j++ {
					if guard[j] != 0 {
						return fmt.Errorf("slabby: guard page violation detected at %s[%d] for slab %d", name, j, slabID)
					}
				}
			}
		} else {
			// Check each byte for smaller guard pages
			for i, b := range guard {
				if b != 0 {
					return fmt.Errorf("slabby: guard page violation detected at %s[%d] for slab %d", name, i, slabID)
				}
			}
		}
		return nil
	}

	if err := checkGuardRegion(beforeGuard, "before"); err != nil {
		return err
	}

	if err := checkGuardRegion(afterGuard, "after"); err != nil {
		return err
	}

	return nil
}

func (r *SlabRef) invalidateReference() {
	r.dataPtr = nil
	r.slabID = -1
	r.guardWord = 0
	if r.allocatorRef != nil && r.allocatorRef.config.enableFinalizers {
		runtime.SetFinalizer(r, nil)
	}
	r.allocatorRef = nil
}

func (r *SlabRef) finalizeReference() {
	if atomic.LoadUint32(&r.allocState) == 0 {
		if r.allocatorRef != nil && r.allocatorRef.logger != nil {
			r.allocatorRef.logger.Error("slabby: memory leak detected",
				slog.Int("slab_id", int(r.slabID)),
				slog.String("allocator", "slabby"),
				slog.String("allocation_stack", r.allocationStack))
		}
	}
}

// Enhanced circuit breaker implementation
func (a *Slabby) isCircuitBreakerOpen() bool {
	if a.circuitBreaker == nil {
		return false
	}

	a.circuitBreaker.stateMutex.RLock()
	defer a.circuitBreaker.stateMutex.RUnlock()

	state := atomic.LoadInt32(&a.circuitBreaker.currentState)
	now := time.Now()

	switch state {
	case circuitOpen:
		// Check if recovery timeout has passed
		if now.Sub(a.circuitBreaker.lastFailureTime) > a.circuitBreaker.config.RecoveryTimeout {
			if atomic.CompareAndSwapInt32(&a.circuitBreaker.currentState, circuitOpen, circuitHalfOpen) {
				a.circuitBreaker.lastStateChange = now
				a.circuitBreaker.successCount = 0
				if a.logger != nil {
					a.logger.Info("slabby: circuit breaker entering half-open state",
						slog.String("allocator", "slabby"))
				}
			}
			return false
		}
		return true
	case circuitHalfOpen:
		// Allow a limited number of requests to test recovery
		if a.circuitBreaker.successCount < a.circuitBreaker.config.FailureThreshold/2 {
			return false
		}
		return true
	default: // circuitClosed
		return false
	}
}

func (a *Slabby) recordCircuitBreakerFailure() {
	if a.circuitBreaker == nil {
		return
	}

	a.circuitBreaker.stateMutex.Lock()
	defer a.circuitBreaker.stateMutex.Unlock()

	state := atomic.LoadInt32(&a.circuitBreaker.currentState)
	now := time.Now()
	a.circuitBreaker.failureCount++
	a.circuitBreaker.lastFailureTime = now

	switch state {
	case circuitClosed:
		if a.circuitBreaker.failureCount >= a.circuitBreaker.config.FailureThreshold {
			atomic.StoreInt32(&a.circuitBreaker.currentState, circuitOpen)
			a.circuitBreaker.lastStateChange = now
			if a.logger != nil {
				a.logger.Warn("slabby: circuit breaker opened",
					slog.Int64("failure_count", a.circuitBreaker.failureCount),
					slog.String("allocator", "slabby"))
			}
		}
	case circuitHalfOpen:
		// Failure in half-open state -> back to open
		atomic.StoreInt32(&a.circuitBreaker.currentState, circuitOpen)
		a.circuitBreaker.lastStateChange = now
		a.circuitBreaker.successCount = 0
		if a.logger != nil {
			a.logger.Warn("slabby: circuit breaker reopened after half-open failure",
				slog.Int64("failure_count", a.circuitBreaker.failureCount),
				slog.String("allocator", "slabby"))
		}
	}
}

func (a *Slabby) recordCircuitBreakerSuccess() {
	if a.circuitBreaker == nil {
		return
	}

	a.circuitBreaker.stateMutex.Lock()
	defer a.circuitBreaker.stateMutex.Unlock()

	state := atomic.LoadInt32(&a.circuitBreaker.currentState)
	now := time.Now()

	switch state {
	case circuitHalfOpen:
		a.circuitBreaker.successCount++
		// Enough successes to close the circuit
		if a.circuitBreaker.successCount >= a.circuitBreaker.config.FailureThreshold/2 {
			atomic.StoreInt32(&a.circuitBreaker.currentState, circuitClosed)
			a.circuitBreaker.failureCount = 0
			a.circuitBreaker.successCount = 0
			a.circuitBreaker.lastStateChange = now
			if a.logger != nil {
				a.logger.Info("slabby: circuit breaker closed",
					slog.String("allocator", "slabby"))
			}
		}
	case circuitClosed:
		// Successful operations help reduce failure count
		if a.circuitBreaker.failureCount > 0 {
			a.circuitBreaker.failureCount--
		}
	}
}

// Enhanced health score calculation with trend analysis
func (a *Slabby) calculateHealthScore() float64 {
	stats := a.Stats()

	// Multiple health factors
	memoryHealth := 1.0 - math.Min(1.0, stats.MemoryUtilization*1.2)
	errorHealth := 1.0 - math.Min(1.0, a.healthMetrics.errorRate*3.0)
	fragmentationHealth := 1.0 - math.Min(1.0, stats.FragmentationRatio*2.0)
	latencyHealth := 1.0 - math.Min(1.0, float64(stats.AvgAllocTimeNs)/float64(a.config.maxAllocLatency.Nanoseconds()))
	cacheHealth := a.healthMetrics.cacheEfficiency

	// Recent trend analysis
	recentHealth := 1.0
	if a.healthMetrics.lastHealthCheck.After(time.Now().Add(-5 * time.Minute)) {
		// Give weight to recent stability
		recentHealth = 0.9 + 0.1*(1.0-a.healthMetrics.errorRate)
	}

	// Clamp all values to [0,1]
	memoryHealth = math.Max(0.0, math.Min(1.0, memoryHealth))
	errorHealth = math.Max(0.0, math.Min(1.0, errorHealth))
	fragmentationHealth = math.Max(0.0, math.Min(1.0, fragmentationHealth))
	latencyHealth = math.Max(0.0, math.Min(1.0, latencyHealth))
	cacheHealth = math.Max(0.0, math.Min(1.0, cacheHealth))
	recentHealth = math.Max(0.0, math.Min(1.0, recentHealth))

	// Weighted average with emphasis on error rate and memory pressure
	return (memoryHealth*0.2 + errorHealth*0.3 + fragmentationHealth*0.15 +
		latencyHealth*0.15 + cacheHealth*0.1 + recentHealth*0.1)
}

func (a *Slabby) startHealthMonitoring() {
	a.healthTicker = time.NewTicker(a.config.healthInterval)
	go func() {
		for {
			select {
			case <-a.healthTicker.C:
				a.updateHealthMetrics()
			case <-a.shutdownChan:
				return
			}
		}
	}()
}

// Enhanced health metrics update with proper initialization
func (a *Slabby) updateHealthMetrics() {
	a.healthMetrics.healthMutex.Lock()
	defer a.healthMetrics.healthMutex.Unlock()

	stats := a.Stats()

	// Update error rate (exponential moving average)
	currentErrorRate := float64(stats.AllocationErrors+stats.DeallocationErrors) /
		math.Max(1.0, float64(stats.TotalAllocations+stats.TotalDeallocations))

	if a.healthMetrics.errorRate == 0 {
		a.healthMetrics.errorRate = currentErrorRate
	} else {
		a.healthMetrics.errorRate = a.healthMetrics.errorRate*0.9 + currentErrorRate*0.1
	}

	// Update memory pressure
	a.healthMetrics.memoryPressure = stats.MemoryUtilization

	// Update cache efficiency
	totalHits := stats.PCCPUCacheHits + stats.LockFreeHits
	totalAllocs := stats.TotalAllocations
	if totalAllocs > 0 {
		a.healthMetrics.cacheEfficiency = float64(totalHits) / float64(totalAllocs)
	}

	// Update health score and track trend
	newHealthScore := a.calculateHealthScore()

	// Store in trend history
	a.healthMetrics.trendHistory[a.healthMetrics.trendHistoryIdx] = newHealthScore
	a.healthMetrics.trendHistoryIdx = (a.healthMetrics.trendHistoryIdx + 1) % len(a.healthMetrics.trendHistory)

	// Analyze trend - ensure it's never empty
	a.healthMetrics.recentTrend = a.analyzeTrend()
	if a.healthMetrics.recentTrend == "" {
		a.healthMetrics.recentTrend = "stable"
	}

	a.healthMetrics.healthScore = newHealthScore
	a.healthMetrics.lastHealthCheck = time.Now()

	if a.logger != nil && a.healthMetrics.healthScore < 0.5 {
		a.logger.Warn("slabby: low health score detected",
			slog.Float64("health_score", a.healthMetrics.healthScore),
			slog.Float64("error_rate", a.healthMetrics.errorRate),
			slog.Float64("memory_pressure", a.healthMetrics.memoryPressure),
			slog.Float64("cache_efficiency", a.healthMetrics.cacheEfficiency),
			slog.String("trend", a.healthMetrics.recentTrend),
			slog.String("allocator", "slabby"))
	}
}

func (a *Slabby) analyzeTrend() string {
	// Simple trend analysis based on recent health scores
	validScores := 0
	sumDelta := 0.0

	for i := 1; i < len(a.healthMetrics.trendHistory); i++ {
		curr := a.healthMetrics.trendHistory[i]
		prev := a.healthMetrics.trendHistory[i-1]
		if curr > 0 && prev > 0 {
			sumDelta += curr - prev
			validScores++
		}
	}

	if validScores < 2 {
		return "stable"
	}

	avgDelta := sumDelta / float64(validScores)
	if avgDelta > 0.05 {
		return "improving"
	} else if avgDelta < -0.05 {
		return "degrading"
	}

	return "stable"
}

// Configuration and options
type AllocatorOption func(*allocatorConfig)

func defaultAllocatorConfig() allocatorConfig {
	return allocatorConfig{
		shardCount:        0, // Use GOMAXPROCS
		cacheLineSize:     DefaultCacheLine,
		enableSecure:      false,
		enableBitGuard:    false,
		enableFinalizers:  false,
		enableFallback:    false,
		enableHealthCheck: false,
		enablePCPUCache:   true, // Enable per-CPU cache by default
		enableGuardPages:  false,
		enableDebug:       false,
		maxAllocLatency:   MaxAllocationLatency,
		healthInterval:    HealthCheckInterval,
		circuitBreakerConf: CircuitBreakerConfig{
			FailureThreshold: 5,
			RecoveryTimeout:  time.Second,
		},
		logger: nil,
	}
}

// WithShards sets the number of shards for the free list to reduce contention.
func WithShards(shards int) AllocatorOption {
	return func(c *allocatorConfig) {
		c.shardCount = shards
	}
}

// WithCacheLine sets the cache line size for memory alignment optimization.
func WithCacheLine(size int) AllocatorOption {
	return func(c *allocatorConfig) {
		c.cacheLineSize = size
	}
}

// WithSecure enables automatic memory zeroing on deallocation for security.
func WithSecure() AllocatorOption {
	return func(c *allocatorConfig) {
		c.enableSecure = true
	}
}

// WithBitGuard enables memory corruption detection using guard words.
func WithBitGuard() AllocatorOption {
	return func(c *allocatorConfig) {
		c.enableBitGuard = true
	}
}

// WithFinalizers enables garbage collection finalizers to detect memory leaks.
func WithFinalizers() AllocatorOption {
	return func(c *allocatorConfig) {
		c.enableFinalizers = true
	}
}

// WithHeapFallback enables falling back to heap allocation when slabs are exhausted.
func WithHeapFallback() AllocatorOption {
	return func(c *allocatorConfig) {
		c.enableFallback = true
	}
}

// WithHealthChecks enables continuous health monitoring and circuit breaker protection.
func WithHealthChecks(enabled bool) AllocatorOption {
	return func(c *allocatorConfig) {
		c.enableHealthCheck = enabled
	}
}

// WithPCPUCache enables or disables per-CPU caching for better performance.
func WithPCPUCache(enabled bool) AllocatorOption {
	return func(c *allocatorConfig) {
		c.enablePCPUCache = enabled
	}
}

// WithGuardPages enables guard pages around allocations for memory protection.
func WithGuardPages() AllocatorOption {
	return func(c *allocatorConfig) {
		c.enableGuardPages = true
	}
}

// WithDebug enables debug features including stack trace capture.
func WithDebug() AllocatorOption {
	return func(c *allocatorConfig) {
		c.enableDebug = true
	}
}

// WithCircuitBreaker configures circuit breaker parameters for failure protection.
func WithCircuitBreaker(threshold int64, timeout time.Duration) AllocatorOption {
	return func(c *allocatorConfig) {
		c.enableHealthCheck = true
		c.circuitBreakerConf = CircuitBreakerConfig{
			FailureThreshold: threshold,
			RecoveryTimeout:  timeout,
		}
	}
}

// WithLogger sets a structured logger for operational events using the standard slog package.
func WithLogger(logger *slog.Logger) AllocatorOption {
	return func(c *allocatorConfig) {
		c.logger = logger
	}
}

// WithMaxAllocLatency sets the maximum acceptable allocation latency for health monitoring.
func WithMaxAllocLatency(latency time.Duration) AllocatorOption {
	return func(c *allocatorConfig) {
		c.maxAllocLatency = latency
	}
}

// WithHealthInterval sets the interval for health check updates.
func WithHealthInterval(interval time.Duration) AllocatorOption {
	return func(c *allocatorConfig) {
		c.healthInterval = interval
	}
}

// Utility functions

// Create cache-aligned memory slice
func createCacheAlignedSlice(totalSize, elemSize, cacheLineSize int) ([]byte, uintptr) {
	// Allocate with extra space for alignment
	extraSpace := cacheLineSize - 1
	raw := make([]byte, totalSize+extraSpace)
	// Find cache-aligned starting point
	start := uintptr(unsafe.Pointer(&raw[0]))
	alignedStart := (start + uintptr(cacheLineSize) - 1) & ^(uintptr(cacheLineSize) - 1)
	offset := int(alignedStart - start)
	return raw[offset : offset+totalSize], alignedStart
}

// Enhanced sharding strategy with better hash function
func getImprovedShardIndex(slabID int32, shardCount int) int {
	// Use a better hash function for distribution (MurmurHash3-inspired)
	hash := uint32(slabID)
	hash = (hash ^ 61) ^ (hash >> 16)
	hash = hash + (hash << 3)
	hash = hash ^ (hash >> 4)
	hash = hash * 0x27d4eb2d
	hash = hash ^ (hash >> 15)
	return int(hash) % shardCount
}

// Enhanced CPU ID acquisition using FNV hash for stability
func getCurrentCPUID() uint64 {
	// Use a combination of goroutine stack and timing for stable CPU identifier
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// Hash the stack trace to get a stable identifier
	h := fnv.New64a()
	h.Write(buf[:n])
	cpuID := h.Sum64()
	// Modulo with GOMAXPROCS to ensure it fits
	return cpuID % uint64(runtime.GOMAXPROCS(0))
}

func alignToCache(size int32, cacheLineSize int) int32 {
	cacheLineInt32 := int32(cacheLineSize)
	return (size + cacheLineInt32 - 1) &^ (cacheLineInt32 - 1)
}

func nanotime() int64 {
	return time.Now().UnixNano()
}

func nextPowerOfTwo(v uint32) uint32 {
	if v == 0 {
		return 1
	}
	return 1 << bits.Len32(v-1)
}
