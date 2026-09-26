package memory

import (
	"math/rand"
	"testing"
)

func TestAddressSplit(t *testing.T) {
	a := Address(PageSize*3 + 100)
	if got := a.PageNumber(); got != 3 {
		t.Errorf("page = %d, want 3", got)
	}
	if got := a.Offset(); got != 100 {
		t.Errorf("offset = %d, want 100", got)
	}
	if got := Address(0).PageNumber(); got != 0 {
		t.Errorf("page = %d, want 0", got)
	}
}

// A page fault must occur the first time a page is touched, and not on
// subsequent touches while it stays resident.
func TestFirstTouchFaultsThenResident(t *testing.T) {
	m := NewMMU(4, 0, LRU{})

	m.Access(Address(0), false)
	if m.Stats.PageFaults != 1 {
		t.Errorf("faults = %d, want 1 on first touch", m.Stats.PageFaults)
	}
	for range 10 {
		m.Access(Address(0), false)
	}
	if m.Stats.PageFaults != 1 {
		t.Errorf("faults = %d; a resident page must not fault again", m.Stats.PageFaults)
	}
}

// Translation must land in the right frame at the right offset.
func TestTranslationPreservesOffset(t *testing.T) {
	m := NewMMU(4, 0, LRU{})
	phys := m.Access(Address(PageSize*2+123), false)
	if got := phys.Offset(); got != 123 {
		t.Errorf("offset = %d, want 123", got)
	}
	// Two accesses to the same page must resolve to the same frame.
	again := m.Access(Address(PageSize*2+456), false)
	if phys.PageNumber() != again.PageNumber() {
		t.Errorf("same page mapped to frames %d and %d", phys.PageNumber(), again.PageNumber())
	}
}

func TestFIFOEvictsOldest(t *testing.T) {
	m := NewMMU(3, 0, FIFO{})
	// Fill with pages 0, 1, 2; then 3 must evict 0.
	m.Run([]int{0, 1, 2, 3}, nil)

	if m.Resident(0) {
		t.Error("FIFO should have evicted the oldest page (0)")
	}
	for _, p := range []int{1, 2, 3} {
		if !m.Resident(p) {
			t.Errorf("page %d should be resident", p)
		}
	}
}

// LRU must keep a recently used page even if it was loaded first — that is
// the whole difference from FIFO.
func TestLRUKeepsRecentlyUsed(t *testing.T) {
	m := NewMMU(3, 0, LRU{})
	// Load 0, 1, 2, then touch 0 again so it is the most recent.
	m.Run([]int{0, 1, 2, 0, 3}, nil)

	if !m.Resident(0) {
		t.Error("LRU evicted page 0 despite it being used most recently")
	}
	if m.Resident(1) {
		t.Error("LRU should have evicted page 1, the least recently used")
	}
}

// Clock gives a referenced page a second chance before evicting it.
//
// The scenario has to leave some reference bits clear: when *every* bit is
// set, the sweep clears them all and clock correctly degenerates to FIFO.
func TestClockSecondChance(t *testing.T) {
	m := NewMMU(3, 0, &Clock{})

	// Fill the frames. Every reference bit is now set.
	m.Run([]int{0, 1, 2}, nil)

	// This eviction sweeps and clears every bit, evicting page 0.
	m.Access(Address(3*PageSize), false)
	if m.Resident(0) {
		t.Fatal("test setup: page 0 should have been evicted by the first sweep")
	}

	// Pages 1 and 2 now have clear reference bits. Touch page 1 so only it
	// is referenced.
	m.Access(Address(1*PageSize), false)

	// The next eviction must spare page 1 and take page 2.
	m.Access(Address(4*PageSize), false)

	if !m.Resident(1) {
		t.Error("clock evicted page 1 despite its reference bit being set")
	}
	if m.Resident(2) {
		t.Error("clock should have evicted page 2, whose reference bit was clear")
	}
}

// With every reference bit set, clock has no information to act on and
// behaves exactly like FIFO. Pinning this so the degenerate case is a
// documented property rather than a surprise.
func TestClockDegeneratesToFIFOWhenAllReferenced(t *testing.T) {
	refs := []int{0, 1, 2, 3, 4, 5}

	clock := NewMMU(3, 0, &Clock{})
	clock.Run(refs, nil)

	fifo := NewMMU(3, 0, FIFO{})
	fifo.Run(refs, nil)

	if clock.Stats.PageFaults != fifo.Stats.PageFaults {
		t.Errorf("clock took %d faults, FIFO took %d; with no reuse they should match",
			clock.Stats.PageFaults, fifo.Stats.PageFaults)
	}
}

// The optimal policy is a lower bound: no implementable policy may beat it.
func TestOptimalIsALowerBound(t *testing.T) {
	refs := []int{7, 0, 1, 2, 0, 3, 0, 4, 2, 3, 0, 3, 2, 1, 2, 0, 1, 7, 0, 1}

	opt := NewMMU(3, 0, &Optimal{Future: refs})
	opt.Run(refs, nil)

	for _, policy := range []Policy{FIFO{}, LRU{}, &Clock{}} {
		m := NewMMU(3, 0, policy)
		m.Run(refs, nil)
		if m.Stats.PageFaults < opt.Stats.PageFaults {
			t.Errorf("%s had %d faults, fewer than optimal's %d — optimal is not optimal",
				policy.Name(), m.Stats.PageFaults, opt.Stats.PageFaults)
		}
	}

	// This reference string is the textbook one: optimal takes 9 faults
	// with 3 frames.
	if opt.Stats.PageFaults != 9 {
		t.Errorf("optimal took %d faults, want 9 for the standard reference string",
			opt.Stats.PageFaults)
	}
}

// Bélády's anomaly: with FIFO, *adding* a frame can increase the number of
// faults. It is the classic argument against FIFO, and a policy that does
// not exhibit it on this reference string is not really FIFO.
func TestFIFOExhibitsBeladysAnomaly(t *testing.T) {
	refs := []int{1, 2, 3, 4, 1, 2, 5, 1, 2, 3, 4, 5}

	three := NewMMU(3, 0, FIFO{})
	three.Run(refs, nil)

	four := NewMMU(4, 0, FIFO{})
	four.Run(refs, nil)

	if four.Stats.PageFaults <= three.Stats.PageFaults {
		t.Errorf("expected Bélády's anomaly: 3 frames gave %d faults, 4 frames gave %d",
			three.Stats.PageFaults, four.Stats.PageFaults)
	}
	t.Logf("Bélády's anomaly confirmed: 3 frames = %d faults, 4 frames = %d faults",
		three.Stats.PageFaults, four.Stats.PageFaults)
}

// LRU is a stack algorithm and so cannot exhibit the anomaly: more frames
// never means more faults.
func TestLRUHasNoAnomaly(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	refs := make([]int, 500)
	for i := range refs {
		refs[i] = rng.Intn(12)
	}

	prev := -1
	for frames := 1; frames <= 12; frames++ {
		m := NewMMU(frames, 0, LRU{})
		m.Run(refs, nil)
		if prev >= 0 && m.Stats.PageFaults > prev {
			t.Errorf("LRU with %d frames had %d faults, more than %d frames' %d — LRU is a stack algorithm and must not regress",
				frames, m.Stats.PageFaults, frames-1, prev)
		}
		prev = m.Stats.PageFaults
	}
}

// More frames must eventually mean fewer faults for LRU.
func TestMoreFramesReduceFaults(t *testing.T) {
	refs := []int{0, 1, 2, 3, 0, 1, 2, 3, 0, 1, 2, 3}

	few := NewMMU(2, 0, LRU{})
	few.Run(refs, nil)
	many := NewMMU(8, 0, LRU{})
	many.Run(refs, nil)

	if many.Stats.PageFaults >= few.Stats.PageFaults {
		t.Errorf("8 frames gave %d faults, not fewer than 2 frames' %d",
			many.Stats.PageFaults, few.Stats.PageFaults)
	}
	// With enough frames, every page faults exactly once.
	if many.Stats.PageFaults != 4 {
		t.Errorf("with 8 frames and 4 distinct pages, want 4 faults, got %d", many.Stats.PageFaults)
	}
}

func TestTLBHitsAvoidTableWalks(t *testing.T) {
	m := NewMMU(8, 4, LRU{})
	// The same page repeatedly: the first is a miss, the rest are hits.
	for range 10 {
		m.Access(Address(0), false)
	}
	if m.Stats.TLBMisses != 1 {
		t.Errorf("TLB misses = %d, want 1", m.Stats.TLBMisses)
	}
	if m.Stats.TLBHits != 9 {
		t.Errorf("TLB hits = %d, want 9", m.Stats.TLBHits)
	}
	if got := m.Stats.TLBHitRate(); got < 0.89 || got > 0.91 {
		t.Errorf("hit rate = %.2f, want ~0.9", got)
	}
}

func TestTLBEvictsLRU(t *testing.T) {
	tlb := NewTLB(2)
	tlb.Add(1, 10)
	tlb.Add(2, 20)
	tlb.Lookup(1)  // make 1 the most recent
	tlb.Add(3, 30) // must evict 2

	if _, ok := tlb.Lookup(2); ok {
		t.Error("TLB should have evicted the least recently used entry")
	}
	if _, ok := tlb.Lookup(1); !ok {
		t.Error("TLB evicted the most recently used entry")
	}
}

// A stale TLB entry is a correctness bug, not a performance one: it would
// translate a page to a frame that now holds different data.
func TestTLBInvalidatedOnEviction(t *testing.T) {
	// Two frames and a TLB big enough to hold every translation, so a
	// stale entry would definitely be hit.
	m := NewMMU(2, 8, FIFO{})

	m.Access(Address(0), false)          // page 0 -> frame 0
	m.Access(Address(PageSize), false)   // page 1 -> frame 1
	m.Access(Address(PageSize*2), false) // page 2 evicts page 0

	if m.Resident(0) {
		t.Fatal("test setup: page 0 should have been evicted")
	}

	// Accessing page 0 again must fault, not be served from a stale TLB
	// entry pointing at the frame page 2 now occupies.
	before := m.Stats.PageFaults
	m.Access(Address(0), false)
	if m.Stats.PageFaults != before+1 {
		t.Error("an evicted page was served from a stale TLB entry")
	}
}

func TestTLBFlush(t *testing.T) {
	tlb := NewTLB(4)
	tlb.Add(1, 10)
	tlb.Add(2, 20)
	if tlb.Len() != 2 {
		t.Fatalf("len = %d, want 2", tlb.Len())
	}
	tlb.Flush()
	if tlb.Len() != 0 {
		t.Errorf("len after flush = %d, want 0", tlb.Len())
	}
	if _, ok := tlb.Lookup(1); ok {
		t.Error("flush did not clear the entries")
	}
}

func TestZeroSizeTLB(t *testing.T) {
	tlb := NewTLB(0)
	tlb.Add(1, 10)
	if _, ok := tlb.Lookup(1); ok {
		t.Error("a zero-capacity TLB must never hit")
	}
	// An MMU with no TLB must still work.
	m := NewMMU(4, 0, LRU{})
	m.Run([]int{0, 1, 0, 1}, nil)
	if m.Stats.PageFaults != 2 {
		t.Errorf("faults = %d, want 2", m.Stats.PageFaults)
	}
}

// Only dirty evictions cost a write-back; a clean frame can be dropped.
func TestDirtyPagesCauseWriteBacks(t *testing.T) {
	clean := NewMMU(2, 0, FIFO{})
	clean.Run([]int{0, 1, 2, 3}, nil)
	if clean.Stats.DiskWrites != 0 {
		t.Errorf("read-only access caused %d write-backs, want 0", clean.Stats.DiskWrites)
	}

	dirty := NewMMU(2, 0, FIFO{})
	dirty.Run([]int{0, 1, 2, 3}, map[int]bool{0: true, 1: true})
	if dirty.Stats.DiskWrites == 0 {
		t.Error("evicting dirty pages should have caused write-backs")
	}
	if dirty.Stats.DiskWrites > dirty.Stats.Evictions {
		t.Errorf("write-backs (%d) exceed evictions (%d)", dirty.Stats.DiskWrites, dirty.Stats.Evictions)
	}
}

// Resident pages must never exceed the physical frames available.
func TestResidentSetBoundedByFrames(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for _, policy := range []Policy{FIFO{}, LRU{}, &Clock{}} {
		t.Run(policy.Name(), func(t *testing.T) {
			const frames = 5
			m := NewMMU(frames, 2, policy)
			for range 1000 {
				m.Access(Address(rng.Intn(50)*PageSize), rng.Intn(2) == 0)
				if got := len(m.ResidentPages()); got > frames {
					t.Fatalf("%d pages resident with only %d frames", got, frames)
				}
			}
		})
	}
}

func TestStatsRates(t *testing.T) {
	s := Stats{Accesses: 100, PageFaults: 25, TLBHits: 75, TLBMisses: 25}
	if got := s.FaultRate(); got != 0.25 {
		t.Errorf("fault rate = %v, want 0.25", got)
	}
	if got := s.TLBHitRate(); got != 0.75 {
		t.Errorf("TLB hit rate = %v, want 0.75", got)
	}
	// Must not divide by zero.
	empty := Stats{}
	if empty.FaultRate() != 0 || empty.TLBHitRate() != 0 {
		t.Error("empty stats should report zero rates")
	}
}

func TestMMUDefaults(t *testing.T) {
	// A zero or negative frame count must still produce a working MMU.
	m := NewMMU(0, 0, nil)
	if m.Capacity() < 1 {
		t.Errorf("capacity = %d, want at least 1", m.Capacity())
	}
	m.Access(Address(0), false)
	if m.Stats.Accesses != 1 {
		t.Error("a minimal MMU should still translate")
	}
}

func BenchmarkAccess(b *testing.B) {
	m := NewMMU(64, 16, LRU{})
	rng := rand.New(rand.NewSource(1))
	b.ResetTimer()
	for b.Loop() {
		m.Access(Address(rng.Intn(256)*PageSize), false)
	}
}
