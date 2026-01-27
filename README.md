# Slabby - Zero-Allocation Slab Allocator for Go

<div align="center">

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=for-the-badge&logo=go)](https://golang.org/doc/devel/release.html)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg?style=for-the-badge)](https://opensource.org/licenses/MIT)
[![GoDoc](https://img.shields.io/badge/pkg.go.dev-doc-blue?style=for-the-badge&logo=go)](https://pkg.go.dev/github.com/xDarkicex/slabby)
[![Go Report Card](https://goreportcard.com/badge/github.com/xDarkicex/slabby?style=for-the-badge)](https://goreportcard.com/report/github.com/xDarkicex/slabby)

</div>

**Slabby** is a zero-allocation slab allocator for Go with enterprise-grade features, designed for high-performance applications requiring predictable memory allocation patterns.

> **🚀 Achievement: Zero Allocations** - Slabby achieves true zero-allocation performance through advanced SlabRef pooling and lock-free data structures, delivering enterprise-grade memory management without garbage collection overhead.

## Key Features

**Performance Excellence**
- Zero GC Pressure: Eliminates allocation overhead through sophisticated object pooling
- Sub-50ns Latency: Consistent ~20-23ns allocation times with predictable performance
- Linear Scalability: Performance scales linearly with CPU count up to hardware limits
- Predictable Behavior: Deterministic allocation patterns for real-time systems

**Security Features**
- Memory Isolation: Automatic memory zeroing prevents data leakage between allocations
- Buffer Overflow Detection: OS-level guard pages provide immediate overflow protection
- Corruption Detection: Runtime validation prevents use-after-free and double-free vulnerabilities
- Audit Trail: Comprehensive allocation tracking for security compliance

**Production Reliability**
- Circuit Breaker Protection: Prevents cascade failures during memory pressure scenarios
- Health Monitoring: Real-time performance metrics and trend analysis
- Graceful Degradation: Automatic fallback mechanisms ensure service continuity
- Leak Detection: Built-in memory leak detection with stack trace analysis
- Zero-Downtime Operations: Hot reconfiguration and health-check endpoints

## Table of Contents

- [Installation & Quick Start](#installation--quick-start)
- [Core API](#core-api)
- [Advanced Features](#advanced-features)
  - [Health Monitoring & Graceful Degradation](#health-monitoring--graceful-degradation)
  - [Leak Detection](#leak-detection)
- [Performance Benchmarks](#performance-benchmarks)
- [Configuration Guide](#configuration-guide)
- [Security Features](#security-features)
- [Monitoring & Observability](#monitoring--observability)
- [Production Deployment](#production-deployment)
- [Troubleshooting](#troubleshooting)
- [Contributing](#contributing)
- [License](#license)

## Installation & Quick Start

### Prerequisites
- Go 1.21 or later
- Linux, macOS, or Windows

### Installation
```bash
go get github.com/xDarkicex/slabby
```

### Basic Usage
```go
package main

import (
    "log"
    
    "github.com/xDarkicex/slabby"
)

func main() {
    // Create allocator: 1KB slabs, 1000 capacity
    allocator, err := slabby.New(1024, 1000)
    if err != nil {
        log.Fatal(err)
    }
    defer allocator.Close()

    // Allocate with full safety guarantees
    ref, err := allocator.Allocate()
    if err != nil {
        log.Fatal(err)
    }
    defer ref.Release()

    data := ref.GetBytes()
    // Use data...
}
```

### Fast Path (Zero Overhead)
```go
// Direct slice access - zero SlabRef allocation
data, slabID, err := allocator.AllocateFast()
if err != nil {
    return err
}

// Process data directly
processData(data)

// Return to pool
allocator.DeallocateFast(slabID)
```

## Core API

### SlabAllocator Interface
```go
type SlabAllocator interface {
    // Primary allocation methods
    Allocate() (*SlabRef, error)                    // Zero allocation
    BatchAllocate(count int) ([]*SlabRef, error)    // Bulk operations
    AllocateWithTimeout(time.Duration) (*SlabRef, error)
    MustAllocate() *SlabRef                         // Panic on failure
    
    // Fast path API (zero overhead)
    AllocateFast() ([]byte, int32, error)          // Raw bytes + slab ID
    DeallocateFast(slabID int32) error              // Direct return
    
    // Deallocation
    Deallocate(ref *SlabRef) error
    BatchDeallocate(refs []*SlabRef) error
    
    // Management
    Reset()
    Stats() *AllocatorStats
    HealthCheck() *HealthMetrics
    Close() error
}
```

### SlabRef Methods
```go
type SlabRef struct{}

// Memory access
func (r *SlabRef) GetBytes() []byte              // Direct slice access
func (r *SlabRef) Size() int                     // Slab size
func (r *SlabRef) IsValid() bool                 // Safety check

// Lifecycle
func (r *SlabRef) Release() error                // Return to pool
func (r *SlabRef) ID() int32                     // Debug identifier

// Debug
func (r *SlabRef) AllocationStack() string       // Stack trace (when debug enabled)
```

## Advanced Features

### Health Monitoring & Graceful Degradation

Slabby provides a health-aware wrapper that monitors memory pressure and automatically degrades gracefully under load.

```go
// Create health-aware wrapper
slab, _ := slabby.New(1024, 10000)
ha := slabby.NewHealthAware(slab)

// Health states: HEALTHY → DEGRADED → SURVIVAL → FALLBACK
// - HEALTHY:   Full features enabled
// - DEGRADED:  ~27% faster (skips metadata overhead)
// - SURVIVAL:  Minimal overhead, fast path only
// - FALLBACK:  Uses Go's make() as last resort

// Check current state
state := ha.State()
metrics := ha.HealthMetrics()
healthCheck := ha.QuickHealthCheck()
// Returns: "state=healthy memory=45.2% error=0.00% health=0.55"

// Custom observability via observers
type HealthObserver interface {
    OnStateChange(prev, curr HealthState, reason string)
    OnMetricsSnapshot(snapshot HealthSnapshot)
    OnAllocate(state HealthState, latencyNs int64, success bool)
    OnDeallocate(state HealthState, success bool)
}

// Register custom observer
ha.RegisterObserver(&MyPrometheusObserver{})

// Simple callback for state changes
ha.SetOnStateChange(func(prev, curr slabby.HealthState, reason string) {
    log.Printf("State changed: %s -> %s (%s)", prev, curr, reason)
})

// Usage - automatically adapts to health state
data, err := ha.Allocate(1024)  // Uses appropriate path based on state
```

### Leak Detection

Built-in leak detection using the observer pattern - zero external dependencies.

```go
// Create leak detector
detector := slabby.NewLeakDetector(slabby.LeakDetectorConfig{
    SampleRate:     100,          // Sample 1% of allocations
    ReportInterval: 30 * time.Second,
    AgeThreshold:   5 * time.Minute,
    LeakThreshold:  3,            // Min net allocations to suspect leak
})

// Register with health-aware allocator
detector.RegisterWith(ha)

// Start detection
detector.Start()

// Get leak report
report := detector.Report()
for _, leak := range report.PotentialLeaks {
    log.Printf("LEAK: %d allocations at:\n%s", leak.Count, leak.Stack)
}

// Set up callback for notifications
detector.SetOnReport(func(report slabby.LeakReport) {
    for _, leak := range report.PotentialLeaks {
        log.Printf("LEAK DETECTED: %d active allocations, age=%v",
            leak.Count, leak.Age)
    }
})

// Get statistics
stats := detector.Stats()
log.Printf("Tracked: %d, Leaks: %d, Unique stacks: %d",
    stats.TotalAllocs, stats.TotalLeaks, stats.UniqueStacks)
```

#### LeakReport Structure
```go
type LeakReport struct {
    Timestamp      time.Time  `json:"timestamp"`
    TotalAllocs    int64      `json:"total_allocations"`
    TotalDeallocs  int64      `json:"total_deallocations"`
    TotalLeaks     int64      `json:"net_leaked"`
    UniqueStacks   int        `json:"unique_stack_traces"`
    PotentialLeaks []LeakInfo `json:"potential_leaks"`
}

type LeakInfo struct {
    Stack          string        `json:"stack"`
    Count          int           `json:"count"`           // Net active allocations
    AllocCount     int           `json:"alloc_count"`     // Total allocations
    DeallocCount   int           `json:"dealloc_count"`   // Total deallocations
    Age            time.Duration `json:"age"`
    FirstAlloc     time.Time     `json:"first_allocation"`
    LastAlloc      time.Time     `json:"last_allocation"`
    SuggestedFix   string        `json:"suggested_fix"`
}
```

## Performance Benchmarks

### Latest Benchmark Results (Apple M2, Go 1.25)

> **🚀 50% Performance Improvement**: Latency tracking is now **OFF by default**, delivering ~20-23 ns/op for maximum performance.

**Core Performance Metrics (Latency Tracking OFF - Default):**
```
BenchmarkAllocateDeallocateLockFree-8     11,748,357    20.63 ns/op    0 B/op    0 allocs/op ✅
BenchmarkAllocateDeallocateFast-8          9,877,930    22.79 ns/op    0 B/op    0 allocs/op ✅
```

**Fast Path Performance (Per-CPU Cache):**
```
Size=64B     15,977,674    20.68 ns/op    0 B/op    0 allocs/op
Size=256B    12,858,176    27.96 ns/op    0 B/op    0 allocs/op
Size=1KB     15,803,161    20.76 ns/op    0 B/op    0 allocs/op
Size=4KB     10,663,328    22.82 ns/op    0 B/op    0 allocs/op
Size=8KB      6,893,010    36.33 ns/op    0 B/op    0 allocs/op
```

### Performance Characteristics

| Metric | Value | Notes |
|--------|-------|-------|
| **Throughput** | 32-48M ops/sec | Updated performance (tracking OFF) |
| **Latency (P50)** | 20-23 ns | Sub-25ns for most operations (tracking OFF) |
| **Latency (P99)** | ~35-40 ns | Larger sizes still under 50ns |
| **Cache Hit Rate** | 95%+ | With stable goroutine affinity |
| **Heap Allocations** | 0 B/op | True zero-allocation |
| **Scalability** | Linear | Up to 8+ cores tested |
| **CPU Overhead** | 0% | When latency tracking disabled |

### Comparison with Go's make()

```
SlabAllocator:      21.14 ns/op    0 B/op    0 allocs/op    No GC pressure ✅
StandardMake:        0.55 ns/op    0 B/op    0 allocs/op    Triggers GC ⚠️
```

**Key Insights:**
- Go's `make()` is **38x faster** for single allocations
- **Zero GC pressure** - ideal for latency-sensitive applications
- **Predictable latency** - no GC pauses or allocation spikes
- **Size-independent** performance (20-36ns across all sizes)

**When to Use Slabby:**
- ✅ High-frequency allocations (millions/sec)
- ✅ Latency-sensitive applications (HFT, real-time systems)
- ✅ Need predictable GC behavior
- ✅ Fixed-size allocation patterns
- ❌ Infrequent allocations
- ❌ Highly variable sizes

See [BENCHMARK_ANALYSIS.md](BENCHMARK_ANALYSIS.md) for detailed performance analysis.

## Configuration Guide

### Production Configuration Templates

#### High-Frequency Trading
```go
func NewTradingAllocator() (*slabby.Slabby, error) {
    return slabby.New(4096, 100000,
        slabby.WithMaxAllocLatency(10*time.Microsecond),
        slabby.WithShards(runtime.GOMAXPROCS(0)*8),
        slabby.WithPCPUCache(true),
        slabby.WithSecure(),
        slabby.WithBitGuard(),
        slabby.WithCircuitBreaker(5, 50*time.Millisecond),
        slabby.WithHealthChecks(true),
    )
}
```

#### Microservices Gateway
```go
func NewGatewayAllocator(logger *slog.Logger) (*slabby.Slabby, error) {
    return slabby.New(8192, 50000,
        slabby.WithHeapFallback(),
        slabby.WithCircuitBreaker(100, 5*time.Second),
        slabby.WithHealthChecks(true),
        slabby.WithSecure(),
        slabby.WithBitGuard(),
        slabby.WithLogger(logger),
    )
}
```

### Configuration Options Reference

#### Performance Tuning
```go
WithShards(count int)           // Concurrency optimization (default: GOMAXPROCS)
WithCacheLine(size int)         // CPU cache alignment (64 for x86/ARM64)
WithPCPUCache(enabled bool)     // Per-CPU optimization (default: true)
WithMaxAllocLatency(duration)   // SLA enforcement threshold
```

#### Security Options
```go
WithSecure()                    // Memory zeroing (GDPR/HIPAA compliance)
WithBitGuard()                  // Corruption detection (minimal overhead)
WithGuardPages()                // OS buffer overflow protection
WithFinalizers()                // Memory leak detection (development only)
```

#### Reliability Features
```go
WithHealthChecks(enabled bool)              // Built-in monitoring
WithCircuitBreaker(threshold, timeout)     // Fault tolerance
WithHeapFallback()                         // Service continuity
```

#### Observability
```go
WithLogger(logger *slog.Logger)    // Structured logging
WithDebug()                        // Extended diagnostics (development)
```

## Security Features

### Memory Protection

```go
// Secure mode - automatic memory zeroing
allocator, _ := slabby.New(4096, 1000, slabby.WithSecure())

ref, _ := allocator.Allocate()
data := ref.GetBytes()
copy(data, sensitiveData)
ref.Release()  // Memory automatically zeroed
```

### Corruption Detection

```go
allocator, _ := slabby.New(4096, 1000, slabby.WithBitGuard())
ref, _ := allocator.Allocate()
err := ref.Release()
if errors.Is(err, slabby.ErrMemoryCorruption) {
    alertSecurityTeam(err)
}
```

## Monitoring & Observability

### Statistics
```go
type AllocatorStats struct {
    TotalSlabs          int     `json:"total_slabs"`
    UsedSlabs           int     `json:"used_slabs"`
    TotalAllocations    uint64  `json:"total_allocations"`
    FastAllocations     uint64  `json:"fast_allocations"`     // Fast path metric
    FastDeallocations   uint64  `json:"fast_deallococations"` // Fast path metric
    AvgAllocTimeNs      float64 `json:"avg_alloc_time_ns"`
    MemoryUtilization   float64 `json:"memory_utilization"`
    FragmentationRatio  float64 `json:"fragmentation_ratio"`
    RefPoolHits         uint64  `json:"ref_pool_hits"` // Zero allocation metric
}
```

### Health Metrics
```go
type HealthMetrics struct {
    AllocLatencyP50    time.Duration `json:"alloc_latency_p50"`
    AllocLatencyP95    time.Duration `json:"alloc_latency_p95"`
    AllocLatencyP99    time.Duration `json:"alloc_latency_p99"`
    HealthScore        float64       `json:"health_score"`       // 0.0-1.0
    MemoryPressure     float64       `json:"memory_pressure"`
    ErrorRate          float64       `json:"error_rate"`
    CircuitBreakerOpen bool          `json:"circuit_breaker_open"`
    RecentTrend        string        `json:"recent_trend"`       // "improving", "stable", "degrading"
}
```

## Production Deployment

### Health Check Endpoint
```go
func healthHandler(allocator *slabby.Slabby) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        health := allocator.HealthCheck()
        stats := allocator.Stats()
        
        status := "healthy"
        code := http.StatusOK
        
        if health.HealthScore < 0.7 {
            status = "degraded"
            code = http.StatusServiceUnavailable
        }
        
        if health.CircuitBreakerOpen {
            status = "circuit_breaker_open"
            code = http.StatusServiceUnavailable
        }
        
        response := map[string]interface{}{
            "status": status,
            "health_score": health.HealthScore,
            "memory_utilization": stats.MemoryUtilization,
            "error_rate": health.ErrorRate,
            "zero_allocations": true,
        }
        
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(code)
        json.NewEncoder(w).Encode(response)
    }
}
```

## Troubleshooting

### High Allocation Latency
```go
stats := allocator.Stats()
health := allocator.HealthCheck()

cacheHitRatio := float64(stats.PCCPUCacheHits) / float64(stats.TotalAllocations)
if cacheHitRatio < 0.8 {
    // Solution: Increase shard count
    // slabby.WithShards(runtime.GOMAXPROCS(0) * 4)
}
```

### Memory Pressure
```go
if stats.MemoryUtilization > 0.95 {
    // Enable heap fallback for graceful degradation
    // slabby.WithHeapFallback()
}
```

## Contributing

```bash
# Clone the repository
git clone https://github.com/xDarkicex/slabby.git
cd slabby

# Run tests
go test -v ./...

# Run benchmarks
go test -bench=. -benchmem ./...

# Check code quality
go vet ./...
```

## License

**Slabby** is open source software licensed under the **MIT License**.

- [LICENSE](https://github.com/xDarkicex/slabby/blob/main/LICENSE)

---

**Slabby** - *Zero-allocation slab allocator for Go.*
