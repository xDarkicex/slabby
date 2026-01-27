// Package slabby provides high-performance slab allocation with enterprise features.
//
// This file implements leak detection using the observer pattern.
package slabby

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LeakDetector identifies memory leaks by tracking allocation patterns.
// It implements HealthObserver to integrate with the health monitoring system.
type LeakDetector struct {
	// Configuration
	sampleRate     uint32        // 1 in N allocations to sample
	reportInterval time.Duration // How often to report
	maxStackTraces int           // Max unique stacks to track
	ageThreshold   time.Duration // Consider leak if older than this
	leakThreshold  int           // Min net allocations to suspect leak

	// Tracking - tracks net allocations per stack
	allocations   sync.Map // stackHash -> *stackSet
	uniqueStacks  int32    // Accessed atomically
	totalAllocs   int64    // Total allocations sampled
	totalDeallocs int64    // Total deallocations sampled
	totalLeaks    int64    // Net allocations currently active

	// State
	running atomic.Bool
	stopCh  chan struct{}

	// Callbacks
	onLeakDetected func(info LeakInfo)
	onReport       func(report LeakReport)

	// Sampling
	sampleCounter uint32 // Accessed atomically
}

// stackSet tracks allocations for a specific stack trace
type stackSet struct {
	mu           sync.Mutex
	stackHash    uint64
	stack        string
	frames       []string
	netCount     int // allocations - deallocations (active allocations)
	allocCount   int // Total allocations tracked
	deallocCount int // Total deallocations tracked
	firstSeen    time.Time
	lastSeen     time.Time
}

// LeakInfo contains information about a potential leak
type LeakInfo struct {
	Stack        string        `json:"stack"`
	Count        int           `json:"count"`         // Net active allocations
	AllocCount   int           `json:"alloc_count"`   // Total allocations
	DeallocCount int           `json:"dealloc_count"` // Total deallocations
	Age          time.Duration `json:"age"`
	FirstAlloc   time.Time     `json:"first_allocation"`
	LastAlloc    time.Time     `json:"last_allocation"`
	SuggestedFix string        `json:"suggested_fix"`
}

// LeakReport is a snapshot of leak detection state
type LeakReport struct {
	Timestamp      time.Time  `json:"timestamp"`
	TotalAllocs    int64      `json:"total_allocations"`
	TotalDeallocs  int64      `json:"total_deallocations"`
	TotalLeaks     int64      `json:"net_leaked"`
	UniqueStacks   int        `json:"unique_stack_traces"`
	PotentialLeaks []LeakInfo `json:"potential_leaks"`
}

// LeakDetectorConfig holds configuration for leak detection
type LeakDetectorConfig struct {
	SampleRate     uint32
	ReportInterval time.Duration
	MaxStackTraces int
	AgeThreshold   time.Duration
	LeakThreshold  int
}

// DefaultLeakDetectorConfig returns the default configuration
func DefaultLeakDetectorConfig() LeakDetectorConfig {
	return LeakDetectorConfig{
		SampleRate:     100, // Sample 1% of allocations
		ReportInterval: 30 * time.Second,
		MaxStackTraces: 1000,
		AgeThreshold:   5 * time.Minute,
		LeakThreshold:  3, // Net allocations to suspect leak
	}
}

// NewLeakDetector creates a new leak detector with the given configuration
func NewLeakDetector(config ...LeakDetectorConfig) *LeakDetector {
	cfg := DefaultLeakDetectorConfig()
	if len(config) > 0 {
		cfg = config[0]
	}

	// Apply defaults if not set
	if cfg.SampleRate == 0 {
		cfg.SampleRate = 100
	}
	if cfg.ReportInterval == 0 {
		cfg.ReportInterval = 30 * time.Second
	}
	if cfg.MaxStackTraces == 0 {
		cfg.MaxStackTraces = 1000
	}
	if cfg.AgeThreshold == 0 {
		cfg.AgeThreshold = 5 * time.Minute
	}
	if cfg.LeakThreshold == 0 {
		cfg.LeakThreshold = 3
	}

	return &LeakDetector{
		sampleRate:     cfg.SampleRate,
		reportInterval: cfg.ReportInterval,
		maxStackTraces: cfg.MaxStackTraces,
		ageThreshold:   cfg.AgeThreshold,
		leakThreshold:  cfg.LeakThreshold,
		stopCh:         make(chan struct{}),
	}
}

// Start begins leak detection
func (ld *LeakDetector) Start() {
	if !ld.running.CompareAndSwap(false, true) {
		return // Already running
	}
	go ld.reportLoop()
}

// Stop halts leak detection
func (ld *LeakDetector) Stop() {
	if ld.running.CompareAndSwap(true, false) {
		close(ld.stopCh)
	}
}

// OnAllocate is called for each allocation (sampled)
func (ld *LeakDetector) OnAllocate(state HealthState, latencyNs int64, success bool) {
	if !success || !ld.running.Load() {
		return
	}

	// Sample 1 in N allocations
	counter := atomic.AddUint32(&ld.sampleCounter, 1)
	if ld.sampleRate > 0 && counter%ld.sampleRate != 0 {
		return
	}

	// Capture stack trace with proper frame filtering
	hash, full, frames := ld.captureStack()

	// Get or create stack set
	set := ld.getOrCreateSet(hash, full, frames)

	// Track allocation
	set.mu.Lock()
	set.netCount++
	set.allocCount++
	set.lastSeen = time.Now()
	if set.firstSeen.IsZero() {
		set.firstSeen = set.lastSeen
	}
	set.mu.Unlock()

	atomic.AddInt64(&ld.totalAllocs, 1)
	atomic.AddInt64(&ld.totalLeaks, 1)
}

// OnDeallocate is called for each deallocation
func (ld *LeakDetector) OnDeallocate(state HealthState, success bool) {
	if !success || !ld.running.Load() {
		return
	}

	// Sample 1 in N deallocations
	counter := atomic.AddUint32(&ld.sampleCounter, 1)
	if ld.sampleRate > 0 && counter%ld.sampleRate != 0 {
		return
	}

	// Capture stack trace with proper frame filtering
	hash, full, frames := ld.captureStack()

	// Get or create stack set
	set := ld.getOrCreateSet(hash, full, frames)

	// Track deallocation - decrease net count
	set.mu.Lock()
	if set.netCount > 0 {
		set.netCount--
	}
	set.deallocCount++
	set.lastSeen = time.Now()
	set.mu.Unlock()

	atomic.AddInt64(&ld.totalDeallocs, 1)
	atomic.AddInt64(&ld.totalLeaks, -1)
}

// OnStateChange is called when health state changes
func (ld *LeakDetector) OnStateChange(prev, curr HealthState, reason string) {
	// Not used
}

// OnMetricsSnapshot is called periodically
func (ld *LeakDetector) OnMetricsSnapshot(snapshot HealthSnapshot) {
	// Not used - leak detection has its own reporting
}

// captureStack captures the current goroutine's stack trace with proper filtering
func (ld *LeakDetector) captureStack() (hash uint64, full string, frames []string) {
	var pcs [32]uintptr
	n := runtime.Callers(0, pcs[:])
	if n == 0 {
		return 0, "", nil
	}

	var buf bytes.Buffer
	frames = make([]string, 0, n)

	callersFrames := runtime.CallersFrames(pcs[:n])

	for {
		frame, more := callersFrames.Next()

		// Skip internal slabby frames
		if strings.Contains(frame.Function, "slabby.") {
			if !more {
				break
			}
			continue
		}

		// Format frame
		fileName := frame.File
		if idx := strings.LastIndex(fileName, "/"); idx >= 0 {
			fileName = fileName[idx+1:]
		}

		funcName := frame.Function
		if idx := strings.LastIndex(funcName, "/"); idx >= 0 {
			funcName = funcName[idx+1:]
		}

		fmt.Fprintf(&buf, "%s:%d %s\n", fileName, frame.Line, funcName)
		frames = append(frames, fmt.Sprintf("%s:%d", fileName, frame.Line))

		if !more {
			break
		}
	}

	// Calculate hash using FNV-1a (better collision resistance)
	hash = ld.hashStack(buf.String())

	return hash, buf.String(), frames
}

// hashStack calculates a hash using FNV-1a algorithm
func (ld *LeakDetector) hashStack(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// getOrCreateSet returns the stack set for a stack trace
func (ld *LeakDetector) getOrCreateSet(hash uint64, full string, frames []string) *stackSet {
	set, ok := ld.allocations.Load(hash)
	if ok {
		return set.(*stackSet)
	}

	// Create new set
	newSet := &stackSet{
		stackHash: hash,
		stack:     full,
		frames:    frames,
	}

	actual, loaded := ld.allocations.LoadOrStore(hash, newSet)
	if loaded {
		return actual.(*stackSet)
	}

	// Track unique stacks
	atomic.AddInt32(&ld.uniqueStacks, 1)

	return newSet
}

// reportLoop periodically reports potential leaks
func (ld *LeakDetector) reportLoop() {
	ticker := time.NewTicker(ld.reportInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			report := ld.Report()
			if ld.onReport != nil {
				ld.onReport(report)
			}
		case <-ld.stopCh:
			return
		}
	}
}

// Report generates a leak detection report
func (ld *LeakDetector) Report() LeakReport {
	now := time.Now()
	leaks := make([]LeakInfo, 0)
	nowStr := now.Format(time.RFC3339)

	// Scan all tracked stacks
	ld.allocations.Range(func(key, value any) bool {
		set := value.(*stackSet)
		set.mu.Lock()

		// Only report if net count exceeds threshold
		if set.netCount >= ld.leakThreshold {
			age := now.Sub(set.firstSeen)

			// Check age threshold
			if age > ld.ageThreshold {
				// Generate suggested fix
				suggestion := ld.suggestFix(set)

				leaks = append(leaks, LeakInfo{
					Stack:        set.stack,
					Count:        set.netCount,
					AllocCount:   set.allocCount,
					DeallocCount: set.deallocCount,
					Age:          age,
					FirstAlloc:   set.firstSeen,
					LastAlloc:    set.lastSeen,
					SuggestedFix: suggestion,
				})
			}
		}

		set.mu.Unlock()
		return true
	})

	// Sort leaks by net count (most active allocations first)
	sort.Slice(leaks, func(i, j int) bool {
		return leaks[i].Count > leaks[j].Count
	})

	// Limit to top 10 leaks
	if len(leaks) > 10 {
		leaks = leaks[:10]
	}

	_ = nowStr // For future timestamp field

	return LeakReport{
		Timestamp:      now,
		TotalAllocs:    atomic.LoadInt64(&ld.totalAllocs),
		TotalDeallocs:  atomic.LoadInt64(&ld.totalDeallocs),
		TotalLeaks:     atomic.LoadInt64(&ld.totalLeaks),
		UniqueStacks:   int(atomic.LoadInt32(&ld.uniqueStacks)),
		PotentialLeaks: leaks,
	}
}

// suggestFix generates a suggested fix based on stack trace
func (ld *LeakDetector) suggestFix(set *stackSet) string {
	if len(set.frames) == 0 {
		return "Unable to analyze stack trace"
	}

	// Look at the top frames for common patterns
	topFrame := set.frames[0]

	// Check for mismatch (deallocations exceed allocations)
	if set.deallocCount > set.allocCount {
		return fmt.Sprintf("Allocation/deallocation mismatch at %s - deallocations (%d) exceed allocations (%d). This usually means allocations and deallocations happen in different functions. For accurate tracking, allocate and deallocate from the same function or use the standard Allocate/Deallocate API.",
			topFrame, set.deallocCount, set.allocCount)
	}

	// Check for common leak patterns
	if strings.Contains(topFrame, "defer ") {
		return fmt.Sprintf("Check deferred cleanup at %s - ensure deferred function runs correctly", topFrame)
	}

	if strings.Contains(topFrame, "for ") && strings.Contains(set.stack, "goroutine") {
		return fmt.Sprintf("Check loop at %s - ensure loop exits and deallocates", topFrame)
	}

	if strings.Contains(set.stack, "select") || strings.Contains(set.stack, "case") {
		return fmt.Sprintf("Check channel operation at %s - ensure channel cases handle cleanup", topFrame)
	}

	return fmt.Sprintf("Check allocation at %s - ensure corresponding deallocation", topFrame)
}

// RegisterWith registers this leak detector with a HealthAware allocator
func (ld *LeakDetector) RegisterWith(ha *HealthAware) {
	ha.RegisterObserver(ld)
}

// Stats returns current leak detection statistics
func (ld *LeakDetector) Stats() LeakStats {
	return LeakStats{
		TotalAllocs:   atomic.LoadInt64(&ld.totalAllocs),
		TotalDeallocs: atomic.LoadInt64(&ld.totalDeallocs),
		NetLeaks:      atomic.LoadInt64(&ld.totalLeaks),
		UniqueStacks:  int(atomic.LoadInt32(&ld.uniqueStacks)),
		Running:       ld.running.Load(),
	}
}

// LeakStats contains leak detection statistics
type LeakStats struct {
	TotalAllocs   int64 `json:"total_allocations"`
	TotalDeallocs int64 `json:"total_deallocations"`
	NetLeaks      int64 `json:"net_leaked"`
	UniqueStacks  int   `json:"unique_stack_traces"`
	Running       bool  `json:"running"`
}

// String returns a human-readable summary
func (ld *LeakDetector) String() string {
	stats := ld.Stats()
	return fmt.Sprintf("LeakDetector: allocs=%d deallocs=%d net=%d stacks=%d running=%v",
		stats.TotalAllocs, stats.TotalDeallocs, stats.NetLeaks, stats.UniqueStacks, stats.Running)
}

// Clear removes all tracked allocations
func (ld *LeakDetector) Clear() {
	ld.allocations = sync.Map{}
	atomic.StoreInt64(&ld.totalAllocs, 0)
	atomic.StoreInt64(&ld.totalDeallocs, 0)
	atomic.StoreInt64(&ld.totalLeaks, 0)
	atomic.StoreInt32(&ld.uniqueStacks, 0)
	atomic.StoreUint32(&ld.sampleCounter, 0)
}

// SetOnLeakDetected sets the callback for when leaks are detected
func (ld *LeakDetector) SetOnLeakDetected(fn func(info LeakInfo)) {
	ld.onLeakDetected = fn
}

// SetOnReport sets the callback for periodic reports
func (ld *LeakDetector) SetOnReport(fn func(report LeakReport)) {
	ld.onReport = fn
}

// MarshalJSON implements json.Marshaler for LeakReport
func (r LeakReport) MarshalJSON() ([]byte, error) {
	type Alias LeakReport
	return json.Marshal(&struct {
		*Alias
	}{
		Alias: (*Alias)(&r),
	})
}

// Helper struct for captureStack return value
type stack struct {
	hash   uint64
	full   string
	frames []string
}
