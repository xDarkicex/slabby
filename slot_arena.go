package slabby

import "fmt"

// Slot identifies a stable slab allocation within a SlotArena.
// The slot remains valid until FreeSlot is called.
type Slot int32

// SlotArena exposes a thin, stable-slot API over Slabby's fast path.
// It is intended for fixed-size payloads that need stable addressable slots
// without the generation-counted handle overhead.
type SlotArena struct {
	slab *Slabby
}

// NewSlotArena creates a fixed-size slot arena backed by Slabby.
func NewSlotArena(slabSize, capacity int, options ...AllocatorOption) (*SlotArena, error) {
	slab, err := New(slabSize, capacity, options...)
	if err != nil {
		return nil, err
	}
	return &SlotArena{slab: slab}, nil
}

// AllocateSlot allocates a slot and returns the slot ID plus the backing bytes.
func (a *SlotArena) AllocateSlot() (Slot, []byte, error) {
	if a == nil || a.slab == nil {
		return 0, nil, fmt.Errorf("slabby: slot arena is nil")
	}
	data, slabID, err := a.slab.AllocateFast()
	if err != nil {
		return 0, nil, err
	}
	return Slot(slabID), data, nil
}

// BytesForSlot returns the backing bytes for an allocated slot.
func (a *SlotArena) BytesForSlot(slot Slot) ([]byte, error) {
	if a == nil || a.slab == nil {
		return nil, fmt.Errorf("slabby: slot arena is nil")
	}
	slabID := int32(slot)
	if slabID < 0 || slabID >= a.slab.totalCapacity {
		return nil, ErrInvalidSlabID
	}
	if !a.slab.slabMetadata[slabID].inUse.Load() {
		return nil, ErrInvalidSlabID
	}
	return a.slab.getSlabBytes(slabID), nil
}

// BytesForAllocatedSlot returns the backing bytes for a slot that the caller
// has already validated as allocated and in range.
func (a *SlotArena) BytesForAllocatedSlot(slot Slot) []byte {
	if a == nil || a.slab == nil {
		return nil
	}
	return a.slab.getSlabBytes(int32(slot))
}

// FreeSlot releases a previously allocated slot.
func (a *SlotArena) FreeSlot(slot Slot) error {
	if a == nil || a.slab == nil {
		return fmt.Errorf("slabby: slot arena is nil")
	}
	return a.slab.DeallocateFast(int32(slot))
}

// InUse reports whether the slot is currently allocated.
func (a *SlotArena) InUse(slot Slot) bool {
	if a == nil || a.slab == nil {
		return false
	}
	slabID := int32(slot)
	if slabID < 0 || slabID >= a.slab.totalCapacity {
		return false
	}
	return a.slab.slabMetadata[slabID].inUse.Load()
}

// Size returns the slot payload size in bytes.
func (a *SlotArena) Size() int {
	if a == nil || a.slab == nil {
		return 0
	}
	return int(a.slab.slabSize)
}

// Capacity returns the total number of slots.
func (a *SlotArena) Capacity() int {
	if a == nil || a.slab == nil {
		return 0
	}
	return int(a.slab.totalCapacity)
}

// Stats returns the underlying allocator stats.
func (a *SlotArena) Stats() *AllocatorStats {
	if a == nil || a.slab == nil {
		return nil
	}
	return a.slab.Stats()
}

// MemoryStats reports reserved allocator capacity alongside live slot bytes.
func (a *SlotArena) MemoryStats() MemoryStats {
	if a == nil || a.slab == nil {
		return MemoryStats{}
	}
	return a.slab.MemoryStats()
}

// Close releases arena resources.
func (a *SlotArena) Close() error {
	if a == nil || a.slab == nil {
		return nil
	}
	return a.slab.Close()
}
