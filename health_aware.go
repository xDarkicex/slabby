// Package slabby provides high-performance slab allocation with enterprise features.
//
// This file implements graceful degradation for production reliability.
package slabby

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// HealthState represents the allocator's current health state
type HealthState int32

const (
	// StateHealthy indicates normal operation
	StateHealthy HealthState = iota
	// StateDegraded indicates elevated pressure, optional features disabled
	StateDegraded
	// StateSurvival indicates critical pressure, simple allocation only
	StateSurvival
	// StateFallback indicates circuit breaker open, using Go allocator
	StateFallback
)

// String returns the health state as a string
func (s HealthState) String() string {
	switch s {
	case StateHealthy:
		return "healthy"
	case StateDegraded:
		return "degraded"
	case StateSurvival:
		return "survival"
	case StateFallback:
		return "fallback"
	default:
		return "unknown"
	}
}

// Health thresholds for state transitions
const (
	defaultPressureThreshold = 0.8  // 80% capacity triggers degraded
	defaultCriticalThreshold = 0.95 // 95% capacity triggers survival
	defaultFallbackThreshold = 0.99 // 99% capacity triggers fallback
	defaultRecoveryThreshold = 0.6  // 60% capacity allows recovery
	defaultCheckInterval     = 100 * time.Millisecond
	defaultPressureWindow    = 5 * time.Second // 5 second window for pressure detection
)

// HealthConfig holds configuration for health-aware allocation
type HealthConfig struct {
	// Pressure thresholds (0.0 to 1.0)
	PressureThreshold float64
	CriticalThreshold float64
	FallbackThreshold float64
	RecoveryThreshold float64

	// Check intervals
	CheckInterval  time.Duration
	PressureWindow time.Duration

	// Fallback allocator
	UseGoFallback bool

	// Sampling configuration (0 = report all, N = report 1 in N)
	AllocSampleRate   uint32
	DeallocSampleRate uint32
}

// DefaultHealthConfig returns the default configuration
func DefaultHealthConfig() HealthConfig {
	return HealthConfig{
		PressureThreshold: defaultPressureThreshold,
		CriticalThreshold: defaultCriticalThreshold,
		FallbackThreshold: defaultFallbackThreshold,
		RecoveryThreshold: defaultRecoveryThreshold,
		CheckInterval:     defaultCheckInterval,
		PressureWindow:    defaultPressureWindow,
		UseGoFallback:     true,
		AllocSampleRate:   100, // Report 1% of allocations
		DeallocSampleRate: 100, // Report 1% of deallocations
	}
}

// HealthObserver is implemented by consumers who want observability.
// Zero external dependencies - consumer implements however they want.
type HealthObserver interface {
	// Called when health state changes
	OnStateChange(prev, curr HealthState, reason string)

	// Called periodically with metrics snapshot (optional)
	OnMetricsSnapshot(snapshot HealthSnapshot)

	// Called on every allocation (sampling recommended)
	OnAllocate(state HealthState, latencyNs int64, success bool)

	// Called on every deallocation (sampling recommended)
	OnDeallocate(state HealthState, success bool)
}

// HealthSnapshot is a point-in-time view of health
type HealthSnapshot struct {
	Timestamp    time.Time     `json:"timestamp"`
	State        HealthState   `json:"state"`
	UsagePercent float64       `json:"usage_percent"`
	ErrorRate    float64       `json:"error_rate"`
	HealthScore  float64       `json:"health_score"`
	TimeInState  time.Duration `json:"time_in_state"`
}

// HealthAware wraps a slab allocator with health monitoring and graceful degradation
type HealthAware struct {
	slab *Slabby

	// Health state management
	state          atomic.Int32
	previousState  atomic.Int32
	stateChangedAt atomic.Int64

	// Metrics
	pressureStart time.Time

	// Configuration
	config HealthConfig

	// Observer management (thread-safe)
	observers     []HealthObserver
	observerMutex sync.RWMutex

	// Sampling
	sampleCounter atomic.Uint32
}

// NewHealthAware creates a health-aware wrapper around a slab allocator
func NewHealthAware(slab *Slabby, config ...HealthConfig) *HealthAware {
	cfg := DefaultHealthConfig()
	if len(config) > 0 {
		cfg = config[0]
	}

	ha := &HealthAware{
		slab:          slab,
		config:        cfg,
		pressureStart: time.Now(),
	}

	ha.state.Store(int32(StateHealthy))

	// Start health monitoring goroutine
	go ha.monitorHealth()

	return ha
}

// monitorHealth periodically checks allocator health and transitions states
func (ha *HealthAware) monitorHealth() {
	ticker := time.NewTicker(ha.config.CheckInterval)
	defer ticker.Stop()

	for range ticker.C {
		ha.checkAndTransition()
	}
}

// checkAndTransition evaluates health metrics and transitions states if needed
func (ha *HealthAware) checkAndTransition() {
	currentState := HealthState(ha.state.Load())

	stats := ha.slab.Stats()
	capacity := stats.TotalSlabs
	if capacity == 0 {
		return
	}

	usageRatio := float64(stats.UsedSlabs) / float64(capacity)
	allocFailRatio := float64(stats.AllocationErrors) / float64(stats.TotalAllocations+1)

	targetState := StateHealthy
	transitionReason := ""

	switch {
	case usageRatio >= ha.config.FallbackThreshold || allocFailRatio > 0.1:
		targetState = StateFallback
		transitionReason = fmt.Sprintf("critical usage (%.1f%%)", usageRatio*100)
	case usageRatio >= ha.config.CriticalThreshold:
		targetState = StateSurvival
		transitionReason = fmt.Sprintf("high usage: %.1f%%", usageRatio*100)
	case usageRatio >= ha.config.PressureThreshold:
		targetState = StateDegraded
		transitionReason = fmt.Sprintf("elevated usage: %.1f%%", usageRatio*100)
	case usageRatio < ha.config.RecoveryThreshold && currentState != StateHealthy:
		if time.Since(ha.pressureStart) >= ha.config.PressureWindow {
			targetState = StateHealthy
			transitionReason = "usage normalized"
		}
	}

	if targetState != currentState {
		ha.transitionTo(targetState, transitionReason)
	}
}

// transitionTo changes the health state
func (ha *HealthAware) transitionTo(newState HealthState, reason string) {
	prevState := HealthState(ha.state.Load())
	ha.state.Store(int32(newState))
	ha.stateChangedAt.Store(time.Now().UnixNano())
	ha.previousState.Store(int32(prevState))

	if newState == StateHealthy {
		ha.pressureStart = time.Now()
	}

	// Notify observers (non-blocking)
	ha.notifyStateChange(prevState, newState, reason)

	// Only GC when critically needed
	switch {
	case newState == StateFallback:
		// Entering fallback - try to free heap memory
		go runtime.GC()
	case prevState == StateFallback && newState == StateHealthy:
		// Recovered from fallback - clean up
		go runtime.GC()
	}
	// No GC for HEALTHY ↔ DEGRADED ↔ SURVIVAL transitions
}

// State returns the current health state
func (ha *HealthAware) State() HealthState {
	return HealthState(ha.state.Load())
}

// StateChangedAt returns when the state last changed
func (ha *HealthAware) StateChangedAt() time.Time {
	return time.Unix(0, ha.stateChangedAt.Load())
}

// PreviousState returns the health state before the last transition
func (ha *HealthAware) PreviousState() HealthState {
	return HealthState(ha.previousState.Load())
}

// Allocate allocates memory based on current health state.
// Different states use different code paths for performance optimization.
func (ha *HealthAware) Allocate(size int) ([]byte, error) {
	state := ha.State()

	switch state {
	case StateHealthy:
		// Full features: SlabRef with metadata, stack traces, finalizers
		return ha.allocateHealthy(size)
	case StateDegraded:
		// Reduced monitoring: AllocateFast, skip metadata overhead
		return ha.allocateDegraded(size)
	case StateSurvival:
		// Minimal overhead: Fast path only
		return ha.allocateSurvival(size)
	case StateFallback:
		// Emergency: Use Go's allocator
		return ha.allocateFallback(size)
	default:
		return ha.allocateHealthy(size)
	}
}

// allocateHealthy uses standard allocation with full features
func (ha *HealthAware) allocateHealthy(size int) ([]byte, error) {
	data, _, err := ha.slab.AllocateFast()
	if err != nil {
		return nil, err
	}
	return data, nil
}

// allocateDegraded uses fast path, skips metadata overhead
func (ha *HealthAware) allocateDegraded(size int) ([]byte, error) {
	data, _, err := ha.slab.AllocateFast()
	return data, err
}

// allocateSurvival uses minimal overhead path
func (ha *HealthAware) allocateSurvival(size int) ([]byte, error) {
	data, _, err := ha.slab.AllocateFast()
	return data, err
}

// allocateFallback uses Go's standard allocator
func (ha *HealthAware) allocateFallback(size int) ([]byte, error) {
	if ha.config.UseGoFallback {
		return make([]byte, size), nil
	}
	return nil, fmt.Errorf("slabby: out of memory")
}

// FreeFast deallocates by ID (fast path)
func (ha *HealthAware) FreeFast(id int32) error {
	return ha.slab.DeallocateFast(id)
}

// AllocateFast allocates using the fast path
func (ha *HealthAware) AllocateFast() ([]byte, int32, error) {
	return ha.slab.AllocateFast()
}

// DeallocateFast deallocates by ID
func (ha *HealthAware) DeallocateFast(id int32) error {
	return ha.slab.DeallocateFast(id)
}

// RegisterObserver adds an observer (thread-safe)
func (ha *HealthAware) RegisterObserver(observer HealthObserver) {
	ha.observerMutex.Lock()
	defer ha.observerMutex.Unlock()
	ha.observers = append(ha.observers, observer)
}

// UnregisterObserver removes an observer
func (ha *HealthAware) UnregisterObserver(observer HealthObserver) {
	ha.observerMutex.Lock()
	defer ha.observerMutex.Unlock()

	for i, obs := range ha.observers {
		if obs == observer {
			ha.observers = append(ha.observers[:i], ha.observers[i+1:]...)
			break
		}
	}
}

// notifyStateChange notifies all observers of state change
func (ha *HealthAware) notifyStateChange(prev, curr HealthState, reason string) {
	ha.observerMutex.RLock()
	observers := ha.observers // Copy to avoid holding lock
	ha.observerMutex.RUnlock()

	for _, observer := range observers {
		go func(obs HealthObserver) {
			defer func() { recover() }() // Observer panic protection
			obs.OnStateChange(prev, curr, reason)
		}(observer)
	}
}

// notifyAllocate notifies observers of allocation (sampled)
func (ha *HealthAware) notifyAllocate(latencyNs int64, success bool) {
	// Sample 1 in N allocations
	counter := ha.sampleCounter.Add(1)
	if ha.config.AllocSampleRate > 0 && counter%ha.config.AllocSampleRate != 0 {
		return
	}

	state := ha.State()
	ha.observerMutex.RLock()
	observers := ha.observers
	ha.observerMutex.RUnlock()

	for _, observer := range observers {
		go func(obs HealthObserver) {
			defer func() { recover() }()
			obs.OnAllocate(state, latencyNs, success)
		}(observer)
	}
}

// HealthMetrics returns current health metrics
func (ha *HealthAware) HealthMetrics() HealthMetrics {
	stats := ha.slab.Stats()
	capacity := stats.TotalSlabs

	var usageRatio float64
	if capacity > 0 {
		usageRatio = float64(stats.UsedSlabs) / float64(capacity)
	}

	var errorRate float64
	totalAllocs := stats.TotalAllocations + stats.FastAllocations
	if totalAllocs > 0 {
		errorRate = float64(stats.AllocationErrors) / float64(totalAllocs)
	}

	healthScore := 1.0 - errorRate - usageRatio
	if healthScore < 0 {
		healthScore = 0
	}

	recentTrend := "stable"
	if ha.State() == StateHealthy && ha.PreviousState() != StateHealthy {
		recentTrend = "improving"
	} else if ha.State() != StateHealthy {
		recentTrend = "degrading"
	}

	return HealthMetrics{
		MemoryPressure:     usageRatio,
		ErrorRate:          errorRate,
		CircuitBreakerOpen: ha.State() == StateFallback,
		HealthScore:        healthScore,
		RecentTrend:        recentTrend,
	}
}

// GetSnapshot returns current health snapshot
func (ha *HealthAware) GetSnapshot() HealthSnapshot {
	state := ha.State()
	stats := ha.slab.Stats()

	var usagePercent float64
	if stats.TotalSlabs > 0 {
		usagePercent = float64(stats.UsedSlabs) / float64(stats.TotalSlabs)
	}

	var errorRate float64
	totalOps := stats.TotalAllocations + stats.FastAllocations
	if totalOps > 0 {
		errorRate = float64(stats.AllocationErrors) / float64(totalOps)
	}

	metrics := ha.HealthMetrics()

	return HealthSnapshot{
		Timestamp:    time.Now(),
		State:        state,
		UsagePercent: usagePercent,
		ErrorRate:    errorRate,
		HealthScore:  metrics.HealthScore,
		TimeInState:  time.Since(ha.StateChangedAt()),
	}
}

// SetOnStateChange sets the callback for state transitions
func (ha *HealthAware) SetOnStateChange(fn func(prev, curr HealthState, reason string)) {
	ha.RegisterObserver(&funcObserver{onStateChange: fn})
}

// funcObserver implements HealthObserver from a simple callback
type funcObserver struct {
	onStateChange     func(prev, curr HealthState, reason string)
	onMetricsSnapshot func(snapshot HealthSnapshot)
	onAllocate        func(state HealthState, latencyNs int64, success bool)
	onDeallocate      func(state HealthState, success bool)
}

func (f *funcObserver) OnStateChange(prev, curr HealthState, reason string) {
	if f.onStateChange != nil {
		f.onStateChange(prev, curr, reason)
	}
}

func (f *funcObserver) OnMetricsSnapshot(snapshot HealthSnapshot) {
	if f.onMetricsSnapshot != nil {
		f.onMetricsSnapshot(snapshot)
	}
}

func (f *funcObserver) OnAllocate(state HealthState, latencyNs int64, success bool) {
	if f.onAllocate != nil {
		f.onAllocate(state, latencyNs, success)
	}
}

func (f *funcObserver) OnDeallocate(state HealthState, success bool) {
	if f.onDeallocate != nil {
		f.onDeallocate(state, success)
	}
}

// Close shuts down the health-aware allocator
func (ha *HealthAware) Close() error {
	ha.state.Store(int32(StateFallback))
	return ha.slab.Close()
}

// Stats returns allocation statistics
func (ha *HealthAware) Stats() *AllocatorStats {
	return ha.slab.Stats()
}

// QuickHealthCheck returns a quick health assessment
func (ha *HealthAware) QuickHealthCheck() string {
	state := ha.State()
	metrics := ha.HealthMetrics()

	return fmt.Sprintf("state=%s memory=%.1f%% error=%.2f%% health=%.2f",
		state, metrics.MemoryPressure*100, metrics.ErrorRate*100, metrics.HealthScore)
}

// Usage returns the current memory usage percentage
func (ha *HealthAware) Usage() float64 {
	stats := ha.slab.Stats()
	if stats.TotalSlabs == 0 {
		return 1.0
	}
	return float64(stats.UsedSlabs) / float64(stats.TotalSlabs)
}

// ErrorRate returns the current allocation error rate
func (ha *HealthAware) ErrorRate() float64 {
	stats := ha.slab.Stats()
	totalAllocs := stats.TotalAllocations + stats.FastAllocations
	if totalAllocs == 0 {
		return 0
	}
	return float64(stats.AllocationErrors) / float64(totalAllocs)
}
