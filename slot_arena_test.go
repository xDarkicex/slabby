package slabby

import "testing"

func TestSlotArenaRoundTrip(t *testing.T) {
	arena, err := NewSlotArena(16, 4, WithPCPUCache(true))
	if err != nil {
		t.Fatalf("failed to create slot arena: %v", err)
	}
	defer arena.Close()

	slot, data, err := arena.AllocateSlot()
	if err != nil {
		t.Fatalf("allocate slot failed: %v", err)
	}
	if !arena.InUse(slot) {
		t.Fatal("expected allocated slot to report in use")
	}

	for i := range data {
		data[i] = byte(i + 1)
	}

	got, err := arena.BytesForSlot(slot)
	if err != nil {
		t.Fatalf("bytes for slot failed: %v", err)
	}
	for i := range data {
		if got[i] != byte(i+1) {
			t.Fatalf("byte mismatch at %d: got %d want %d", i, got[i], byte(i+1))
		}
	}

	if err := arena.FreeSlot(slot); err != nil {
		t.Fatalf("free slot failed: %v", err)
	}
	if arena.InUse(slot) {
		t.Fatal("expected freed slot to report not in use")
	}
	if _, err := arena.BytesForSlot(slot); err == nil {
		t.Fatal("expected bytes lookup for freed slot to fail")
	}
}

func TestSlotArenaReusesFreedSlot(t *testing.T) {
	arena, err := NewSlotArena(8, 1, WithPCPUCache(true))
	if err != nil {
		t.Fatalf("failed to create slot arena: %v", err)
	}
	defer arena.Close()

	slot1, _, err := arena.AllocateSlot()
	if err != nil {
		t.Fatalf("first allocate failed: %v", err)
	}
	if err := arena.FreeSlot(slot1); err != nil {
		t.Fatalf("free failed: %v", err)
	}

	slot2, _, err := arena.AllocateSlot()
	if err != nil {
		t.Fatalf("second allocate failed: %v", err)
	}
	if slot2 != slot1 {
		t.Fatalf("expected freed slot to be reusable, got %d then %d", slot1, slot2)
	}
}

func TestSlotArenaMemoryStats(t *testing.T) {
	arena, err := NewSlotArena(16, 4, WithPCPUCache(false))
	if err != nil {
		t.Fatalf("failed to create slot arena: %v", err)
	}
	defer arena.Close()

	slot, _, err := arena.AllocateSlot()
	if err != nil {
		t.Fatalf("allocate slot failed: %v", err)
	}
	defer arena.FreeSlot(slot)

	stats := arena.MemoryStats()
	if stats.UsedSlabs != 1 {
		t.Fatalf("used slabs = %d, want 1", stats.UsedSlabs)
	}
	if stats.FreeSlabs != 3 {
		t.Fatalf("free slabs = %d, want 3", stats.FreeSlabs)
	}
	if stats.LiveBytes != 16 {
		t.Fatalf("live bytes = %d, want 16", stats.LiveBytes)
	}
	if stats.ReservedDataBytes != 256 {
		t.Fatalf("reserved data bytes = %d, want 256", stats.ReservedDataBytes)
	}
	if stats.FreeBytes != 240 {
		t.Fatalf("free bytes = %d, want 240", stats.FreeBytes)
	}
	if stats.CapacityUtilization != 0.25 {
		t.Fatalf("capacity utilization = %f, want 0.25", stats.CapacityUtilization)
	}

	allocatorStats := arena.Stats()
	if allocatorStats.LiveBytes != stats.LiveBytes {
		t.Fatalf("allocator live bytes = %d, want %d", allocatorStats.LiveBytes, stats.LiveBytes)
	}
	if allocatorStats.ReservedDataBytes != stats.ReservedDataBytes {
		t.Fatalf("allocator reserved data bytes = %d, want %d", allocatorStats.ReservedDataBytes, stats.ReservedDataBytes)
	}
}
