# Slabby - High-Performance Slab Allocator for Go

A production-ready slab allocator designed for Go applications that require predictable memory allocation patterns with enterprise-grade features including security, monitoring, and fault tolerance.

## Overview

Slabby provides a sophisticated memory management solution that addresses the performance and reliability challenges of high-throughput Go applications. By pre-allocating fixed-size memory blocks (slabs) and managing them through efficient data structures, Slabby eliminates the unpredictability of garbage collection pressure while providing enterprise features essential for production deployments.

## Table of Contents

- [Features](#features)
- [Installation](#installation)
- [Quick Start](#quick-start)
- [API Documentation](#api-documentation)
- [Configuration](#configuration)
- [Architecture & Implementation](#architecture--implementation)
- [Performance Characteristics](#performance-characteristics)
- [Testing](#testing)
- [Monitoring & Observability](#monitoring--observability)
- [Security](#security)
- [Best Practices](#best-practices)
- [Contributing](#contributing)
- [License](#license)

## Features

### Core Capabilities
- **Constant-Time Operations**: O(1) allocation and deallocation performance
- **Cache-Aware Design**: Memory layout optimized for modern CPU architectures
- **Lock-Free Fast Path**: Minimized contention under concurrent workloads
- **Per-CPU Caching**: Reduced inter-CPU communication overhead
- **Batch Operations**: Efficient bulk allocation and deallocation

### Enterprise Features
- **Circuit Breaker Pattern**: Protection against cascade failures during memory pressure
- **Comprehensive Monitoring**: Real-time metrics and health trend analysis
- **Memory Security**: Configurable memory zeroing and corruption detection
- **Buffer Overflow Detection**: Optional guard pages for enhanced security
- **Graceful Degradation**: Heap fallback mechanism when slabs are exhausted
- **Structured Logging**: Native integration with Go's `slog` package

### Production Readiness
- **Thread-Safe Design**: Full concurrency support across multiple goroutines
- **Memory Efficient**: Minimal fragmentation and metadata overhead
- **Highly Configurable**: Extensive customization for diverse workload requirements
- **Observable**: Rich metrics and health indicators for operational insight
- **Fault Tolerant**: Built-in circuit breaker and fallback mechanisms

## Installation

```bash
go get github.com/example/slabby
```

## Quick Start

```go
package main

import (
    "fmt"
    "log"
    "github.com/example/slabby"
)

func main() {
    // Create a slab allocator for 4KB slabs with capacity for 1000 slabs
    allocator, err := slabby.New(4096, 1000,
        slabby.WithSecure(),           // Enable memory zeroing on deallocation
        slabby.WithHealthChecks(true), // Enable health monitoring
    )
    if err != nil {
        log.Fatal(err)
    }
    defer allocator.Close()

    // Allocate a slab
    ref, err := allocator.Allocate()
    if err != nil {
        log.Fatal(err)
    }
    defer ref.Release()

    // Use the allocated memory
    data := ref.GetBytes()
    copy(data, []byte("Hello, Slabby!"))
    
    fmt.Printf("Allocated %d bytes\n", len(data))
    
    // Monitor allocator performance
    stats := allocator.Stats()
    fmt.Printf("Total allocations: %d\n", stats.TotalAllocations)
    fmt.Printf("Memory utilization: %.2f%%\n", stats.MemoryUtilization*100)
}
```

## API Documentation

### Core Interface

#### SlabAllocator

The primary interface for memory allocation operations.

```go
type SlabAllocator interface {
    // Allocation methods
    Allocate() (*SlabRef, error)
    BatchAllocate(count int) ([]*SlabRef, error)
    AllocateWithTimeout(timeout time.Duration) (*SlabRef, error)
    MustAllocate() *SlabRef

    // Deallocation methods
    Deallocate(ref *SlabRef) error
    BatchDeallocate(refs []*SlabRef) error

    // Management
    Reset()
    Stats() *AllocatorStats
    HealthCheck() *HealthMetrics
    Close() error
}
```

#### SlabRef - Memory Reference

Represents a reference to allocated memory with lifecycle management.

```go
type SlabRef struct {
    // Private implementation
}

// Access methods
func (r *SlabRef) GetBytes() []byte
func (r *SlabRef) Size() int
func (r *SlabRef) ID() int32

// Lifecycle methods
func (r *SlabRef) IsValid() bool
func (r *SlabRef) Release() error

// Debug information
func (r *SlabRef) AllocationStack() string
```

### Constructor

```go
func New(slabSize, capacity int, options ...AllocatorOption) (*Slabby, error)
```

Creates a new slab allocator with the specified configuration.

**Parameters:**
- `slabSize`: Size of each individual slab in bytes (must be positive)
- `capacity`: Maximum number of slabs the allocator can manage (must be positive and ≤ MaxInt32)
- `options`: Configuration options for customizing behavior

**Returns:**
- `*Slabby`: Configured allocator instance ready for use
- `error`: Validation error if parameters are invalid

### Allocation Methods

#### Standard Allocation
```go
func (a *Slabby) Allocate() (*SlabRef, error)
```
Allocates a single slab using the fastest available path. Returns `ErrOutOfMemory` when no slabs are available.

#### Batch Allocation
```go
func (a *Slabby) BatchAllocate(count int) ([]*SlabRef, error)
```
Efficiently allocates multiple slabs in a single operation, reducing per-allocation overhead.

#### Timeout-Based Allocation
```go
func (a *Slabby) AllocateWithTimeout(timeout time.Duration) (*SlabRef, error)
```
Attempts allocation with a specified timeout, returning `ErrAllocationTimeout` if the deadline is exceeded.

#### Panic-on-Failure Allocation
```go
func (a *Slabby) MustAllocate() *SlabRef
```
Allocates a slab or panics on failure. Use only when allocation failure should terminate the application.

## Configuration

### Performance Tuning

```go
// Optimize for high-concurrency scenarios
WithShards(count int) AllocatorOption

// Configure CPU cache line alignment
WithCacheLine(size int) AllocatorOption

// Control per-CPU caching behavior
WithPCPUCache(enabled bool) AllocatorOption
```

### Security Configuration

```go
// Enable automatic memory clearing on deallocation
WithSecure() AllocatorOption

// Enable memory corruption detection
WithBitGuard() AllocatorOption

// Enable OS-level buffer overflow protection
WithGuardPages() AllocatorOption

// Enable garbage collection finalizers for leak detection (development only)
WithFinalizers() AllocatorOption
```

### Reliability Features

```go
// Enable health monitoring and circuit breaker functionality
WithHealthChecks(enabled bool) AllocatorOption

// Configure circuit breaker thresholds and recovery timing
WithCircuitBreaker(threshold int64, timeout time.Duration) AllocatorOption

// Enable fallback to heap allocation when slabs are exhausted
WithHeapFallback() AllocatorOption
```

### Observability Options

```go
// Configure structured logging output
WithLogger(logger *slog.Logger) AllocatorOption

// Enable additional debug information (development only)
WithDebug() AllocatorOption

// Set health check monitoring frequency
WithHealthInterval(interval time.Duration) AllocatorOption

// Configure acceptable allocation latency thresholds
WithMaxAllocLatency(latency time.Duration) AllocatorOption
```

## Architecture & Implementation

### Memory Organization

Slabby employs a multi-tier architecture optimized for modern CPU architectures:

```
┌─────────────────┐    ┌──────────────────┐    ┌─────────────────┐
│   Per-CPU       │    │   Lock-Free      │    │    Sharded      │
│   Cache         │ -> │   Stacks         │ -> │    Lists        │
│   (Fastest)     │    │   (Fast)         │    │   (Fallback)    │
└─────────────────┘    └──────────────────┘    └─────────────────┘
```

### Allocation Strategy

The allocator implements a hierarchical approach to minimize latency and contention:

1. **Per-CPU Cache**: Thread-local storage with zero contention
2. **Lock-Free Stacks**: Atomic operations for cross-CPU coordination
3. **Sharded Lists**: Mutex-protected fallback with improved distribution
4. **Heap Fallback**: Standard Go allocation when configured

### Key Algorithms

#### Cache-Line Alignment
Ensures optimal memory layout for CPU cache efficiency:

```go
func alignToCache(size int32, cacheLineSize int) int32 {
    return (size + cacheLineSize - 1) &^ (cacheLineSize - 1)
}
```

#### Optimized Sharding
Uses hash-based distribution to minimize lock contention:

```go
func getShardIndex(slabID int32, shardCount int) int {
    hash := uint32(slabID)
    hash = (hash ^ 61) ^ (hash >> 16)
    hash = hash + (hash << 3)
    hash = hash ^ (hash >> 4)
    hash = hash * 0x27d4eb2d
    hash = hash ^ (hash >> 15)
    return int(hash) % shardCount
}
```

## Performance Characteristics

### Design Goals

- **Low Latency**: Sub-microsecond allocation times under normal conditions
- **High Throughput**: Linear scaling with CPU count up to hardware limits
- **Memory Efficiency**: Minimal overhead and fragmentation
- **Predictable Behavior**: Consistent performance characteristics under load

### Optimization Recommendations

1. **Slab Sizing**: Align slab sizes with your typical allocation patterns and CPU cache lines
2. **Capacity Planning**: Size allocators for peak load plus a safety buffer (typically 20-30%)
3. **Concurrency Tuning**: Use `WithShards(runtime.GOMAXPROCS(0) * 2)` for high-contention scenarios
4. **Cache Configuration**: Match cache line size to your target architecture (64 bytes for x86/ARM64)

## Testing

### Comprehensive Test Suite

The project maintains extensive test coverage across multiple dimensions:

- **Unit Tests**: Core functionality validation with >95% code coverage
- **Integration Tests**: End-to-end scenario verification
- **Concurrency Tests**: Race condition detection and thread safety validation
- **Stress Tests**: High-load stability and resource leak detection
- **Property-Based Tests**: Randomized input validation

### Running Tests

```bash
# Execute complete test suite
go test -v ./...

# Run with race condition detection
go test -race ./...

# Execute stress tests
go test -v -run=TestStress ./...

# Generate and view coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

### Test Categories

#### Functional Validation
- Basic allocation and deallocation operations
- Batch operation correctness
- Error condition handling
- Configuration option verification
- Security feature validation

#### Reliability Testing
- Circuit breaker activation and recovery
- Health monitoring accuracy
- Concurrent access patterns
- Memory leak detection
- Reference lifecycle management

#### Performance Validation
- Allocation latency measurement
- Throughput under sustained load
- Memory efficiency analysis
- Cache effectiveness evaluation
- Fragmentation impact assessment

## Monitoring & Observability

### Performance Statistics

```go
type AllocatorStats struct {
    // Configuration
    Version               string
    TotalSlabs           int
    SlabSize             int
    CacheLineSize        int
    
    // Current State
    UsedSlabs            int
    AvailableSlabs       int
    CurrentAllocations   uint64
    
    // Lifetime Counters
    TotalAllocations     uint64
    TotalDeallocations   uint64
    BatchAllocations     uint64
    BatchDeallocations   uint64
    AllocationErrors     uint64
    DeallocationErrors   uint64
    
    // Performance Metrics
    AvgAllocTimeNs       float64
    MaxAllocTimeNs       int64
    FragmentationRatio   float64
    MemoryUtilization    float64
    
    // Feature Usage
    SecureMode           bool
    BitGuardEnabled      bool
    HeapFallbacks        uint64
    PCPUCacheHits        uint64
    LockFreeHits         uint64
    GuardPageViolations  uint64
}
```

### Health Monitoring

```go
type HealthMetrics struct {
    // Latency Distribution
    AllocLatencyP50      time.Duration
    AllocLatencyP95      time.Duration
    AllocLatencyP99      time.Duration
    
    // System Health
    FragmentationScore   float64    // 0.0 = no fragmentation, 1.0 = high fragmentation
    MemoryPressure       float64    // 0.0 = low pressure, 1.0 = critical
    ErrorRate            float64    // Recent error rate (0.0 - 1.0)
    CacheEfficiency      float64    // Cache hit ratio (0.0 - 1.0)
    
    // Operational Status
    CircuitBreakerOpen   bool
    HealthScore          float64    // Overall health (0.0 - 1.0)
    RecentTrend          string     // "improving", "stable", "degrading"
    LastGCDuration       time.Duration
}
```

### Integration Examples

#### Prometheus Metrics
```go
func exportMetrics(allocator *slabby.Slabby) {
    stats := allocator.Stats()
    health := allocator.HealthCheck()
    
    // Export key metrics
    memoryUtilizationGauge.Set(stats.MemoryUtilization)
    healthScoreGauge.Set(health.HealthScore)
    allocationLatencyHistogram.Observe(float64(health.AllocLatencyP95.Nanoseconds()))
    
    // Track circuit breaker state
    if health.CircuitBreakerOpen {
        circuitBreakerEventsCounter.Inc()
    }
}
```

## Security

### Memory Protection Features

#### Secure Mode
Automatically zeros memory contents upon deallocation to prevent information leakage between allocations. Adds minimal performance overhead while providing important security guarantees.

#### Corruption Detection
The bit guard feature embeds detection patterns in allocated memory to identify corruption, use-after-free, and double-free conditions during runtime.

#### Buffer Overflow Protection
Guard pages provide OS-level memory protection around allocations, immediately detecting buffer overflows and underflows at the cost of increased memory usage.

### Security Configuration Examples

```go
// Maximum security configuration
allocator, err := slabby.New(4096, 1000,
    slabby.WithSecure(),        // Zero memory on deallocation
    slabby.WithBitGuard(),      // Enable corruption detection
    slabby.WithGuardPages(),    // OS-level overflow protection
    slabby.WithFinalizers(),    // Detect memory leaks
)

// Balanced security for production
allocator, err := slabby.New(4096, 1000,
    slabby.WithSecure(),        // Essential for data protection
    slabby.WithBitGuard(),      // Minimal overhead corruption detection
    slabby.WithHealthChecks(true), // Monitor for anomalies
)
```

## Best Practices

### Sizing Guidelines

```go
// Calculate optimal slab size based on allocation patterns
func calculateOptimalSlabSize(typicalSize int) int {
    const cacheLineSize = 64
    // Align to cache line boundary for optimal performance
    return ((typicalSize + cacheLineSize - 1) / cacheLineSize) * cacheLineSize
}

// Determine appropriate capacity
func calculateCapacity(peakConcurrent, safetyMargin int) int {
    return peakConcurrent + (peakConcurrent * safetyMargin / 100)
}
```

### Resource Management

```go
// Proper allocator lifecycle management
func createManagedAllocator(slabSize, capacity int) (*slabby.Slabby, error) {
    allocator, err := slabby.New(slabSize, capacity)
    if err != nil {
        return nil, err
    }
    
    // Ensure cleanup even if explicit Close() is missed
    runtime.SetFinalizer(allocator, (*slabby.Slabby).Close)
    return allocator, nil
}

// Context-aware allocation with timeout handling
func allocateWithContext(ctx context.Context, allocator *slabby.Slabby) (*slabby.SlabRef, error) {
    if deadline, ok := ctx.Deadline(); ok {
        timeout := time.Until(deadline)
        return allocator.AllocateWithTimeout(timeout)
    }
    return allocator.Allocate()
}
```

### Production Configuration

```go
// Production-ready allocator configuration
func NewProductionAllocator(slabSize, capacity int, logger *slog.Logger) (*slabby.Slabby, error) {
    return slabby.New(slabSize, capacity,
        // Security configuration
        slabby.WithSecure(),
        slabby.WithBitGuard(),
        
        // Reliability features
        slabby.WithHealthChecks(true),
        slabby.WithCircuitBreaker(50, 30*time.Second),
        slabby.WithHeapFallback(),
        
        // Performance optimization
        slabby.WithShards(runtime.GOMAXPROCS(0)*2),
        slabby.WithPCPUCache(true),
        
        // Operational visibility
        slabby.WithLogger(logger),
        slabby.WithHealthInterval(15*time.Second),
        slabby.WithMaxAllocLatency(100*time.Microsecond),
    )
}
```

### Error Handling Patterns

```go
func robustAllocation(allocator *slabby.Slabby) (*slabby.SlabRef, error) {
    ref, err := allocator.Allocate()
    switch {
    case err == nil:
        return ref, nil
        
    case errors.Is(err, slabby.ErrOutOfMemory):
        // Implement backoff or alternative allocation strategy
        time.Sleep(10 * time.Millisecond)
        return allocator.AllocateWithTimeout(100 * time.Millisecond)
        
    case errors.Is(err, slabby.ErrCircuitBreakerOpen):
        // Circuit breaker is protecting the system
        return nil, fmt.Errorf("allocation service temporarily unavailable: %w", err)
        
    default:
        // Unexpected error condition
        return nil, fmt.Errorf("allocation failed: %w", err)
    }
}
```

## Contributing

We welcome contributions from the community. Please review our contribution guidelines and code of conduct before submitting pull requests.

### Development Setup

```bash
git clone https://github.com/example/slabby
cd slabby
go mod download
make test
make lint
```

### Contribution Guidelines

- Follow established Go conventions and formatting standards
- Include comprehensive tests for all new functionality
- Update documentation to reflect API changes
- Ensure all CI checks pass before requesting review
- Write clear commit messages describing the change rationale

### Reporting Issues

When reporting issues, please include:
- Go version and target architecture
- Slabby version and configuration
- Minimal reproducible example
- Expected versus actual behavior
- Relevant log output or stack traces

## License

This project is licensed under the GNU General Public License v3.0. See the [LICENSE](LICENSE) file for complete terms.

### GPL v3.0 Overview

The GPL v3.0 provides four essential freedoms:
- **Use**: Run the software for any purpose
- **Study**: Examine and understand how the software works
- **Modify**: Adapt the software to your needs
- **Share**: Distribute copies and modifications

**Important**: GPL v3.0 is a copyleft license. Any derivative works must also be licensed under GPL v3.0 and make source code available.

For alternative licensing arrangements or commercial use inquiries, please contact the project maintainers.

---

**Slabby** provides enterprise-grade memory allocation for Go applications requiring predictable performance and operational excellence.

*Built for high-performance systems that demand reliable, observable, and secure memory management.*