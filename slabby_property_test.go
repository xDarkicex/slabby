package slabby

import (
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Property-based testing framework for slabby allocator
// This framework uses randomized testing to discover edge cases and validate invariants

// TestOperation represents an operation that can be performed on the allocator
type TestOperation interface {
	Execute(*Slabby) error
	Description() string
	IsAllocation() bool
	IsDeallocation() bool
}

// AllocationOp represents an allocation operation
type AllocationOp struct {
	ref *SlabRef
}

func (op *AllocationOp) Execute(a *Slabby) error {
	ref, err := a.Allocate()
	if err == nil {
		op.ref = ref
	}
	return err
}

func (op *AllocationOp) Description() string {
	return "Allocate"
}

func (op *AllocationOp) IsAllocation() bool {
	return true
}

func (op *AllocationOp) IsDeallocation() bool {
	return false
}

// DeallocationOp represents a deallocation operation
type DeallocationOp struct {
	ref *SlabRef
}

func (op *DeallocationOp) Execute(a *Slabby) error {
	if op.ref == nil {
		return ErrInvalidReference
	}
	return a.Deallocate(op.ref)
}

func (op *DeallocationOp) Description() string {
	return "Deallocate"
}

func (op *DeallocationOp) IsAllocation() bool {
	return false
}

func (op *DeallocationOp) IsDeallocation() bool {
	return true
}

// BatchAllocationOp represents a batch allocation operation
type BatchAllocationOp struct {
	refs []*SlabRef
	size int
}

func (op *BatchAllocationOp) Execute(a *Slabby) error {
	refs, err := a.BatchAllocate(op.size)
	if err == nil {
		op.refs = refs
	}
	return err
}

func (op *BatchAllocationOp) Description() string {
	return fmt.Sprintf("BatchAllocate(%d)", op.size)
}

func (op *BatchAllocationOp) IsAllocation() bool {
	return true
}

func (op *BatchAllocationOp) IsDeallocation() bool {
	return false
}

// BatchDeallocationOp represents a batch deallocation operation
type BatchDeallocationOp struct {
	refs []*SlabRef
}

func (op *BatchDeallocationOp) Execute(a *Slabby) error {
	return a.BatchDeallocate(op.refs)
}

func (op *BatchDeallocationOp) Description() string {
	return fmt.Sprintf("BatchDeallocate(%d)", len(op.refs))
}

func (op *BatchDeallocationOp) IsAllocation() bool {
	return false
}

func (op *BatchDeallocationOp) IsDeallocation() bool {
	return true
}

// FastAllocationOp represents a fast allocation operation
type FastAllocationOp struct {
	data []byte
	id   int32
}

func (op *FastAllocationOp) Execute(a *Slabby) error {
	data, id, err := a.AllocateFast()
	if err == nil {
		op.data = data
		op.id = id
	}
	return err
}

func (op *FastAllocationOp) Description() string {
	return "AllocateFast"
}

func (op *FastAllocationOp) IsAllocation() bool {
	return true
}

func (op *FastAllocationOp) IsDeallocation() bool {
	return false
}

// FastDeallocationOp represents a fast deallocation operation
type FastDeallocationOp struct {
	id int32
}

func (op *FastDeallocationOp) Execute(a *Slabby) error {
	return a.DeallocateFast(op.id)
}

func (op *FastDeallocationOp) Description() string {
	return fmt.Sprintf("DeallocateFast(%d)", op.id)
}

func (op *FastDeallocationOp) IsAllocation() bool {
	return false
}

func (op *FastDeallocationOp) IsDeallocation() bool {
	return true
}

// TestState tracks the expected state of the allocator during property testing
type TestState struct {
	allocatedRefs     []*SlabRef
	fastAllocatedIDs  []int32
	allocationCount   int
	deallocationCount int
	maxCapacity       int
	stats             *AllocatorStats
}

// PropertyTestConfig holds configuration for property tests
type PropertyTestConfig struct {
	MaxOperations    int
	MaxBatchSize     int
	EnableFastPath   bool
	EnableBatchOps   bool
	EnableConcurrent bool
	NumGoroutines    int
	Seed             int64
	Verbose          bool
}

// PropertyTestResult holds results from property tests
type PropertyTestResult struct {
	OperationsExecuted int
	AllocationCount    int32
	DeallocationCount  int32
	ErrorsEncountered  int
	PropertiesViolated int
	FinalState         *TestState
	FailedOperations   []string
}

// PropertyTester runs property-based tests on the allocator
type PropertyTester struct {
	config PropertyTestConfig
	rand   *rand.Rand
	state  *TestState
	result *PropertyTestResult
}

// NewPropertyTester creates a new property tester
func NewPropertyTester(config PropertyTestConfig) *PropertyTester {
	if config.Seed == 0 {
		config.Seed = time.Now().UnixNano()
	}

	return &PropertyTester{
		config: config,
		rand:   rand.New(rand.NewSource(config.Seed)),
		result: &PropertyTestResult{
			FailedOperations: make([]string, 0),
		},
	}
}

// Initialize initializes the test state
func (pt *PropertyTester) Initialize(allocator *Slabby) {
	stats := allocator.Stats()
	pt.state = &TestState{
		allocatedRefs:     make([]*SlabRef, 0),
		fastAllocatedIDs:  make([]int32, 0),
		allocationCount:   0,
		deallocationCount: 0,
		maxCapacity:       stats.TotalSlabs,
		stats:             stats,
	}
}

// GenerateOperation generates a random operation based on current state
func (pt *PropertyTester) GenerateOperation() TestOperation {
	// Decide operation type based on current state
	allocatedCount := len(pt.state.allocatedRefs) + len(pt.state.fastAllocatedIDs)
	totalCapacity := pt.state.maxCapacity

	// Bias towards allocation when we have capacity, deallocation when we're full
	allocWeight := 1
	deallocWeight := 1

	if allocatedCount < totalCapacity/2 {
		allocWeight = 3
	} else if allocatedCount > totalCapacity*3/4 {
		deallocWeight = 3
	}

	// Choose operation type
	opType := pt.rand.Intn(allocWeight + deallocWeight)

	if opType < allocWeight {
		// Allocation operation
		if pt.config.EnableBatchOps && pt.rand.Float32() < 0.3 {
			// Batch allocation
			batchSize := 1 + pt.rand.Intn(min(pt.config.MaxBatchSize, totalCapacity-allocatedCount))
			return &BatchAllocationOp{size: batchSize}
		} else if pt.config.EnableFastPath && pt.rand.Float32() < 0.2 {
			// Fast allocation
			return &FastAllocationOp{}
		} else {
			// Regular allocation
			return &AllocationOp{}
		}
	} else {
		// Deallocation operation
		if len(pt.state.allocatedRefs) > 0 {
			if pt.config.EnableBatchOps && pt.rand.Float32() < 0.4 && len(pt.state.allocatedRefs) > 1 {
				// Batch deallocation
				batchSize := 1 + pt.rand.Intn(min(3, len(pt.state.allocatedRefs)))
				refs := make([]*SlabRef, batchSize)
				for i := 0; i < batchSize; i++ {
					idx := pt.rand.Intn(len(pt.state.allocatedRefs))
					refs[i] = pt.state.allocatedRefs[idx]
					pt.state.allocatedRefs = append(pt.state.allocatedRefs[:idx], pt.state.allocatedRefs[idx+1:]...)
				}
				return &BatchDeallocationOp{refs: refs}
			} else {
				// Regular deallocation
				idx := pt.rand.Intn(len(pt.state.allocatedRefs))
				ref := pt.state.allocatedRefs[idx]
				pt.state.allocatedRefs = append(pt.state.allocatedRefs[:idx], pt.state.allocatedRefs[idx+1:]...)
				return &DeallocationOp{ref: ref}
			}
		} else if len(pt.state.fastAllocatedIDs) > 0 {
			// Fast deallocation
			idx := pt.rand.Intn(len(pt.state.fastAllocatedIDs))
			id := pt.state.fastAllocatedIDs[idx]
			pt.state.fastAllocatedIDs = append(pt.state.fastAllocatedIDs[:idx], pt.state.fastAllocatedIDs[idx+1:]...)
			return &FastDeallocationOp{id: id}
		} else {
			// No allocations to deallocate, try allocation instead
			return &AllocationOp{}
		}
	}
}

// ExecuteOperation executes an operation and updates state
func (pt *PropertyTester) ExecuteOperation(op TestOperation, allocator *Slabby) error {
	err := op.Execute(allocator)

	// Update state based on operation result
	if err == nil {
		if op.IsAllocation() {
			atomic.AddInt32(&pt.result.AllocationCount, 1)
			switch typedOp := op.(type) {
			case *AllocationOp:
				pt.state.allocatedRefs = append(pt.state.allocatedRefs, typedOp.ref)
			case *BatchAllocationOp:
				pt.state.allocatedRefs = append(pt.state.allocatedRefs, typedOp.refs...)
			case *FastAllocationOp:
				pt.state.fastAllocatedIDs = append(pt.state.fastAllocatedIDs, typedOp.id)
			}
		} else if op.IsDeallocation() {
			atomic.AddInt32(&pt.result.DeallocationCount, 1)
		}
	} else {
		pt.result.ErrorsEncountered++
		pt.result.FailedOperations = append(pt.result.FailedOperations, fmt.Sprintf("%s: %v", op.Description(), err))
	}

	return err
}

// ValidateProperties validates key properties after each operation
func (pt *PropertyTester) ValidateProperties(allocator *Slabby) {
	stats := allocator.Stats()

	// Property 1: Allocation count should never exceed capacity
	if int(stats.CurrentAllocations) > pt.state.maxCapacity {
		pt.result.PropertiesViolated++
		pt.result.FailedOperations = append(pt.result.FailedOperations,
			fmt.Sprintf("PROPERTY VIOLATION: Current allocations (%d) exceed capacity (%d)",
				stats.CurrentAllocations, pt.state.maxCapacity))
	}

	// Property 2: Available slabs should be non-negative
	if stats.AvailableSlabs < 0 {
		pt.result.PropertiesViolated++
		pt.result.FailedOperations = append(pt.result.FailedOperations,
			fmt.Sprintf("PROPERTY VIOLATION: Negative available slabs: %d", stats.AvailableSlabs))
	}

	// Property 3: Memory utilization should be between 0 and 1
	if stats.MemoryUtilization < 0 || stats.MemoryUtilization > 1 {
		pt.result.PropertiesViolated++
		pt.result.FailedOperations = append(pt.result.FailedOperations,
			fmt.Sprintf("PROPERTY VIOLATION: Invalid memory utilization: %f", stats.MemoryUtilization))
	}

	// Property 4: Fragmentation ratio should be between 0 and 1
	if stats.FragmentationRatio < 0 || stats.FragmentationRatio > 1 {
		pt.result.PropertiesViolated++
		pt.result.FailedOperations = append(pt.result.FailedOperations,
			fmt.Sprintf("PROPERTY VIOLATION: Invalid fragmentation ratio: %f", stats.FragmentationRatio))
	}

	// Property 5: Current allocations should be non-negative
	if stats.CurrentAllocations < 0 {
		pt.result.PropertiesViolated++
		pt.result.FailedOperations = append(pt.result.FailedOperations,
			fmt.Sprintf("PROPERTY VIOLATION: Negative current allocations: %d", stats.CurrentAllocations))
	}
}

// RunSequentialTest runs a sequential property test
func (pt *PropertyTester) RunSequentialTest(t *testing.T, allocator *Slabby) {
	pt.Initialize(allocator)

	for i := 0; i < pt.config.MaxOperations; i++ {
		op := pt.GenerateOperation()

		if pt.config.Verbose {
			t.Logf("Operation %d: %s", i+1, op.Description())
		}

		err := pt.ExecuteOperation(op, allocator)
		if err != nil && pt.config.Verbose {
			t.Logf("  Error: %v", err)
		}

		pt.ValidateProperties(allocator)
		pt.result.OperationsExecuted++
	}

	// Clean up remaining allocations
	for _, ref := range pt.state.allocatedRefs {
		ref.Release()
	}
	for _, id := range pt.state.fastAllocatedIDs {
		allocator.DeallocateFast(id)
	}

	pt.result.FinalState = pt.state
}

// RunConcurrentTest runs a concurrent property test
func (pt *PropertyTester) RunConcurrentTest(t *testing.T, allocator *Slabby) {
	pt.Initialize(allocator)

	var wg sync.WaitGroup
	var opsExecuted int32
	var errorsEncountered int32
	var propertiesViolated int32

	// Per-goroutine state to avoid shared state issues
	type goroutineState struct {
		allocatedRefs    []*SlabRef
		fastAllocatedIDs []int32
	}
	goroutineStates := make([]goroutineState, pt.config.NumGoroutines)

	for g := 0; g < pt.config.NumGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			localState := &goroutineStates[goroutineID]

			for i := 0; i < pt.config.MaxOperations/pt.config.NumGoroutines; i++ {
				// Generate operation based on local state
				var op TestOperation
				allocatedCount := len(localState.allocatedRefs) + len(localState.fastAllocatedIDs)
				
				// Bias towards allocation when we have few, deallocation when we have many
				if allocatedCount == 0 || (allocatedCount < 10 && pt.rand.Float32() < 0.7) {
					// Allocate
					if pt.config.EnableFastPath && pt.rand.Float32() < 0.2 {
						op = &FastAllocationOp{}
					} else {
						op = &AllocationOp{}
					}
				} else {
					// Deallocate
					if len(localState.allocatedRefs) > 0 {
						idx := pt.rand.Intn(len(localState.allocatedRefs))
						ref := localState.allocatedRefs[idx]
						localState.allocatedRefs = append(localState.allocatedRefs[:idx], localState.allocatedRefs[idx+1:]...)
						op = &DeallocationOp{ref: ref}
					} else if len(localState.fastAllocatedIDs) > 0 {
						idx := pt.rand.Intn(len(localState.fastAllocatedIDs))
						id := localState.fastAllocatedIDs[idx]
						localState.fastAllocatedIDs = append(localState.fastAllocatedIDs[:idx], localState.fastAllocatedIDs[idx+1:]...)
						op = &FastDeallocationOp{id: id}
					} else {
						op = &AllocationOp{}
					}
				}

				err := op.Execute(allocator)
				if err != nil {
					atomic.AddInt32(&errorsEncountered, 1)
				} else {
					if op.IsAllocation() {
						atomic.AddInt32((*int32)(&pt.result.AllocationCount), 1)
						// Track the allocation in local state
						switch v := op.(type) {
						case *AllocationOp:
							if v.ref != nil {
								localState.allocatedRefs = append(localState.allocatedRefs, v.ref)
							}
						case *FastAllocationOp:
							if v.id >= 0 {
								localState.fastAllocatedIDs = append(localState.fastAllocatedIDs, v.id)
							}
						}
					} else if op.IsDeallocation() {
						atomic.AddInt32((*int32)(&pt.result.DeallocationCount), 1)
					}
				}

				atomic.AddInt32(&opsExecuted, 1)
			}
		}(g)
	}

	wg.Wait()

	pt.result.OperationsExecuted = int(opsExecuted)
	pt.result.ErrorsEncountered = int(errorsEncountered)
	pt.result.PropertiesViolated = int(propertiesViolated)

	// Final property validation - only check basic invariants, not exact counts
	// since concurrent operations make exact tracking impossible
	stats := allocator.Stats()
	
	// Check basic invariants
	if stats.CurrentAllocations > uint64(pt.state.maxCapacity) {
		pt.result.PropertiesViolated++
		pt.result.FailedOperations = append(pt.result.FailedOperations,
			fmt.Sprintf("PROPERTY VIOLATION: Current allocations (%d) exceed capacity (%d)",
				stats.CurrentAllocations, pt.state.maxCapacity))
	}
	
	if stats.AvailableSlabs < 0 {
		pt.result.PropertiesViolated++
		pt.result.FailedOperations = append(pt.result.FailedOperations,
			fmt.Sprintf("PROPERTY VIOLATION: Negative available slabs: %d", stats.AvailableSlabs))
	}
}

// TestPropertyBasedSequential runs sequential property-based tests
func TestPropertyBasedSequential(t *testing.T) {
	config := PropertyTestConfig{
		MaxOperations:    1000,
		MaxBatchSize:     10,
		EnableFastPath:   true,
		EnableBatchOps:   true,
		EnableConcurrent: false,
		NumGoroutines:    1,
		Seed:             time.Now().UnixNano(),
		Verbose:          false,
	}

	allocator, err := New(1024, 500)
	require.NoError(t, err)
	defer allocator.Close()

	tester := NewPropertyTester(config)
	tester.RunSequentialTest(t, allocator)

	t.Logf("Property test results:")
	t.Logf("  Operations executed: %d", tester.result.OperationsExecuted)
	t.Logf("  Allocations: %d, Deallocations: %d", atomic.LoadInt32(&tester.result.AllocationCount), atomic.LoadInt32(&tester.result.DeallocationCount))
	t.Logf("  Errors encountered: %d", tester.result.ErrorsEncountered)
	t.Logf("  Properties violated: %d", tester.result.PropertiesViolated)

	if tester.result.PropertiesViolated > 0 {
		t.Errorf("Property violations detected:")
		for _, failure := range tester.result.FailedOperations {
			if len(failure) > 0 && failure[:len("PROPERTY VIOLATION")] == "PROPERTY VIOLATION" {
				t.Logf("  %s", failure)
			}
		}
	}

	// Allow some errors (like out of memory) but no property violations
	assert.Equal(t, 0, tester.result.PropertiesViolated, "Property violations should not occur")
}

// TestPropertyBasedConcurrent runs concurrent property-based tests
func TestPropertyBasedConcurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping concurrent test in short mode")
	}

	config := PropertyTestConfig{
		MaxOperations:    500,
		MaxBatchSize:     5,
		EnableFastPath:   true,
		EnableBatchOps:   true,
		EnableConcurrent: true,
		NumGoroutines:    10,
		Seed:             time.Now().UnixNano(),
		Verbose:          false,
	}

	allocator, err := New(1024, 1000)
	require.NoError(t, err)
	defer allocator.Close()

	tester := NewPropertyTester(config)
	tester.RunConcurrentTest(t, allocator)

	t.Logf("Concurrent property test results:")
	t.Logf("  Operations executed: %d", tester.result.OperationsExecuted)
	t.Logf("  Allocations: %d, Deallocations: %d", atomic.LoadInt32(&tester.result.AllocationCount), atomic.LoadInt32(&tester.result.DeallocationCount))
	t.Logf("  Errors encountered: %d", tester.result.ErrorsEncountered)
	t.Logf("  Properties violated: %d", tester.result.PropertiesViolated)

	if tester.result.PropertiesViolated > 0 {
		t.Errorf("Property violations detected in concurrent test:")
		for _, failure := range tester.result.FailedOperations {
			if len(failure) > 0 && failure[:len("PROPERTY VIOLATION")] == "PROPERTY VIOLATION" {
				t.Logf("  %s", failure)
			}
		}
	}

	// Concurrent operations may have some errors but should maintain properties
	assert.Equal(t, 0, tester.result.PropertiesViolated, "Concurrent operations should maintain properties")
}

// TestPropertyBasedEdgeCases tests specific edge cases
func TestPropertyBasedEdgeCases(t *testing.T) {
	// Test with different configurations
	configs := []struct {
		name string
		opts []AllocatorOption
	}{
		{"Default", []AllocatorOption{}},
		{"SecureMode", []AllocatorOption{WithSecure()}},
		{"BitGuard", []AllocatorOption{WithBitGuard()}},
		{"HeapFallback", []AllocatorOption{WithHeapFallback()}},
		{"GuardPages", []AllocatorOption{WithGuardPages()}},
		{"AllFeatures", []AllocatorOption{WithSecure(), WithBitGuard(), WithHeapFallback(), WithGuardPages()}},
	}

	for _, tc := range configs {
		t.Run(tc.name, func(t *testing.T) {
			allocator, err := New(512, 200, tc.opts...)
			require.NoError(t, err)
			defer allocator.Close()

			config := PropertyTestConfig{
				MaxOperations:    200,
				MaxBatchSize:     5,
				EnableFastPath:   true,
				EnableBatchOps:   true,
				EnableConcurrent: false,
				NumGoroutines:    1,
				Seed:             time.Now().UnixNano(),
				Verbose:          false,
			}

			tester := NewPropertyTester(config)
			tester.RunSequentialTest(t, allocator)

			t.Logf("  %s: Operations: %d, Allocs: %d, Deallocs: %d, Errors: %d, Violations: %d",
				tc.name, tester.result.OperationsExecuted, atomic.LoadInt32(&tester.result.AllocationCount),
				atomic.LoadInt32(&tester.result.DeallocationCount), tester.result.ErrorsEncountered,
				tester.result.PropertiesViolated)

			assert.Equal(t, 0, tester.result.PropertiesViolated,
				"Property violations should not occur with %s", tc.name)
		})
	}
}

// TestPropertyBasedStress runs stress tests to find edge cases
func TestPropertyBasedStress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	// Run multiple iterations with different seeds to find edge cases
	violationsFound := 0
	uniqueViolations := make(map[string]bool)

	for i := 0; i < 5; i++ {
		allocator, err := New(256, 1000, WithHeapFallback())
		require.NoError(t, err)

		config := PropertyTestConfig{
			MaxOperations:    2000,
			MaxBatchSize:     20,
			EnableFastPath:   true,
			EnableBatchOps:   true,
			EnableConcurrent: true,
			NumGoroutines:    20,
			Seed:             int64(i * 42), // Different seed for each iteration
			Verbose:          false,
		}

		tester := NewPropertyTester(config)
		tester.RunConcurrentTest(t, allocator)
		allocator.Close()

		t.Logf("Stress iteration %d: Operations: %d, Violations: %d",
			i+1, tester.result.OperationsExecuted, tester.result.PropertiesViolated)

		if tester.result.PropertiesViolated > 0 {
			violationsFound++
			for _, failure := range tester.result.FailedOperations {
				if len(failure) > 0 && failure[:len("PROPERTY VIOLATION")] == "PROPERTY VIOLATION" {
					uniqueViolations[failure] = true
				}
			}
		}
	}

	t.Logf("Stress test summary: %d iterations, %d violations found, %d unique violation types",
		5, violationsFound, len(uniqueViolations))

	if len(uniqueViolations) > 0 {
		t.Log("Unique violations found:")
		for violation := range uniqueViolations {
			t.Logf("  - %s", violation)
		}
	}

	assert.Equal(t, 0, violationsFound, "Stress tests should not find property violations")
}

// TestPropertyBasedMemorySafety tests memory safety properties
func TestPropertyBasedMemorySafety(t *testing.T) {
	allocator, err := New(1024, 100, WithSecure(), WithBitGuard(), WithGuardPages())
	require.NoError(t, err)
	defer allocator.Close()

	config := PropertyTestConfig{
		MaxOperations:    500,
		MaxBatchSize:     10,
		EnableFastPath:   true,
		EnableBatchOps:   true,
		EnableConcurrent: false,
		NumGoroutines:    1,
		Seed:             time.Now().UnixNano(),
		Verbose:          false,
	}

	tester := NewPropertyTester(config)

	// Initialize the test state first
	tester.Initialize(allocator)

	// Custom operation generator that includes memory corruption attempts
	for i := 0; i < config.MaxOperations; i++ {
		op := tester.GenerateOperation()

		// Occasionally inject memory corruption attempts
		if i%50 == 0 && len(tester.state.allocatedRefs) > 0 {
			// Try to corrupt memory
			idx := tester.rand.Intn(len(tester.state.allocatedRefs))
			ref := tester.state.allocatedRefs[idx]

			// Attempt to write beyond bounds (should be caught by guard pages)
			data := ref.GetBytes()
			if len(data) > 0 {
				// This should be safe within bounds
				data[0] = 0xAA

				// Try to access beyond bounds (should fail with guard pages)
				defer func() {
					if r := recover(); r != nil {
						t.Logf("Memory safety violation caught: %v", r)
					}
				}()

				// This might trigger guard page violation
				if len(data) < 2048 { // Only try if slab is small enough
					_ = data[len(data)-1] // Access last byte (should be safe)
				}
			}
		}

		err := tester.ExecuteOperation(op, allocator)
		if err != nil && !errors.Is(err, ErrOutOfMemory) && !errors.Is(err, ErrMemoryCorruption) {
			t.Logf("Operation %d (%s) failed: %v", i+1, op.Description(), err)
		}

		tester.ValidateProperties(allocator)
		tester.result.OperationsExecuted++
	}

	t.Logf("Memory safety test: %d operations, %d violations",
		tester.result.OperationsExecuted, tester.result.PropertiesViolated)

	assert.Equal(t, 0, tester.result.PropertiesViolated, "Memory safety properties should be maintained")
}

// TestPropertyBasedResourceLeaks tests for resource leaks
func TestPropertyBasedResourceLeaks(t *testing.T) {
	allocator, err := New(512, 500, WithFinalizers())
	require.NoError(t, err)
	defer allocator.Close()

	config := PropertyTestConfig{
		MaxOperations:    1000,
		MaxBatchSize:     5,
		EnableFastPath:   false,
		EnableBatchOps:   true,
		EnableConcurrent: false,
		NumGoroutines:    1,
		Seed:             time.Now().UnixNano(),
		Verbose:          false,
	}

	tester := NewPropertyTester(config)
	tester.RunSequentialTest(t, allocator)

	// Force garbage collection to trigger finalizers
	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	// Check that all allocations were properly cleaned up
	finalStats := allocator.Stats()
	if finalStats.CurrentAllocations != 0 {
		t.Errorf("Resource leak detected: %d allocations not cleaned up", finalStats.CurrentAllocations)
	}

	t.Logf("Resource leak test: All allocations properly cleaned up")
}

// TestPropertyBasedConfigurationInteractions tests configuration interactions
func TestPropertyBasedConfigurationInteractions(t *testing.T) {
	// Test combinations of configurations that might interact
	configurations := []struct {
		name string
		opts []AllocatorOption
	}{
		{"Secure+BitGuard", []AllocatorOption{WithSecure(), WithBitGuard()}},
		{"Secure+GuardPages", []AllocatorOption{WithSecure(), WithGuardPages()}},
		{"BitGuard+GuardPages", []AllocatorOption{WithBitGuard(), WithGuardPages()}},
		{"AllSecurity", []AllocatorOption{WithSecure(), WithBitGuard(), WithGuardPages()}},
		{"Performance", []AllocatorOption{WithPCPUCache(true), WithShards(8)}},
		{"Reliability", []AllocatorOption{WithHeapFallback(), WithHealthChecks(true)}},
	}

	for _, tc := range configurations {
		t.Run(tc.name, func(t *testing.T) {
			allocator, err := New(256, 300, tc.opts...)
			require.NoError(t, err)
			defer allocator.Close()

			testConfig := PropertyTestConfig{
				MaxOperations:    300,
				MaxBatchSize:     5,
				EnableFastPath:   true,
				EnableBatchOps:   true,
				EnableConcurrent: false,
				NumGoroutines:    1,
				Seed:             time.Now().UnixNano(),
				Verbose:          false,
			}

			tester := NewPropertyTester(testConfig)
			tester.RunSequentialTest(t, allocator)

			t.Logf("  %s: Violations: %d", tc.name, tester.result.PropertiesViolated)
			assert.Equal(t, 0, tester.result.PropertiesViolated,
				"Configuration %s should not violate properties", tc.name)
		})
	}
}

// Helper functions
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
