// Package memory models virtual memory: address translation through a page
// table, a TLB, and the page replacement policies that decide what to evict
// when physical memory is full.
package memory

import (
	"container/list"
	"fmt"
	"sort"
)

// PageSize is the bytes per page. A power of two means the split between
// page number and offset is a shift and a mask rather than a division.
const PageSize = 4096

// Address is a virtual or physical address.
type Address int

// PageNumber returns the page containing the address.
func (a Address) PageNumber() int { return int(a) / PageSize }

// Offset returns the address's offset within its page.
func (a Address) Offset() int { return int(a) % PageSize }

// Frame is a physical page frame.
type Frame struct {
	Number int
	// Page and PID identify what currently occupies the frame.
	Page int
	PID  int
	// Referenced and Dirty are the bits hardware would set; the clock
	// policy uses Referenced, and Dirty decides whether eviction costs a
	// write-back.
	Referenced bool
	Dirty      bool
	LoadTime   int
	LastUsed   int
}

// Stats counts what happened during a run.
type Stats struct {
	Accesses   int
	PageFaults int
	TLBHits    int
	TLBMisses  int
	Evictions  int
	// DiskWrites counts evictions of dirty frames, which are the expensive
	// ones: a clean frame can simply be dropped.
	DiskWrites int
}

// FaultRate returns page faults per access.
func (s Stats) FaultRate() float64 {
	if s.Accesses == 0 {
		return 0
	}
	return float64(s.PageFaults) / float64(s.Accesses)
}

// TLBHitRate returns TLB hits per lookup.
func (s Stats) TLBHitRate() float64 {
	total := s.TLBHits + s.TLBMisses
	if total == 0 {
		return 0
	}
	return float64(s.TLBHits) / float64(total)
}

func (s Stats) String() string {
	return fmt.Sprintf("accesses=%d faults=%d (%.1f%%) tlb=%d/%d (%.1f%%) evictions=%d writes=%d",
		s.Accesses, s.PageFaults, s.FaultRate()*100,
		s.TLBHits, s.TLBHits+s.TLBMisses, s.TLBHitRate()*100,
		s.Evictions, s.DiskWrites)
}

// Policy chooses which frame to evict when memory is full.
type Policy interface {
	Name() string
	// Evict returns the frame index to reclaim.
	Evict(frames []*Frame, clock int) int
	// Touch notes an access to a resident frame, for policies that need
	// access history.
	Touch(frame *Frame, clock int)
}

// FIFO evicts the frame loaded longest ago.
//
// Simple, but ignores usage entirely — and suffers Bélády's anomaly, where
// adding frames can *increase* the fault count.
type FIFO struct{}

func (FIFO) Name() string { return "FIFO" }

func (FIFO) Evict(frames []*Frame, _ int) int {
	oldest := 0
	for i, f := range frames {
		if f.LoadTime < frames[oldest].LoadTime {
			oldest = i
		}
	}
	return oldest
}

func (FIFO) Touch(*Frame, int) {}

// LRU evicts the least recently used frame.
//
// A good approximation of the optimal policy for real workloads, because
// recently used pages tend to be used again. Exact LRU needs a timestamp
// update on every access, which is why hardware usually implements an
// approximation like clock instead.
type LRU struct{}

func (LRU) Name() string { return "LRU" }

func (LRU) Evict(frames []*Frame, _ int) int {
	oldest := 0
	for i, f := range frames {
		if f.LastUsed < frames[oldest].LastUsed {
			oldest = i
		}
	}
	return oldest
}

func (LRU) Touch(f *Frame, clock int) { f.LastUsed = clock }

// Clock (second chance) approximates LRU using one reference bit per frame.
//
// The hand sweeps the frames; a frame with its reference bit set gets a
// second chance and the bit is cleared. This is what real hardware supports,
// since it needs only a single bit per page rather than a timestamp.
type Clock struct{ hand int }

func (c *Clock) Name() string { return "clock (second chance)" }

func (c *Clock) Evict(frames []*Frame, _ int) int {
	for {
		f := frames[c.hand]
		if !f.Referenced {
			victim := c.hand
			c.hand = (c.hand + 1) % len(frames)
			return victim
		}
		// Second chance: clear the bit and move on. The loop terminates
		// because each pass clears at least one bit.
		f.Referenced = false
		c.hand = (c.hand + 1) % len(frames)
	}
}

func (c *Clock) Touch(f *Frame, _ int) { f.Referenced = true }

// Optimal evicts the page used furthest in the future.
//
// Unimplementable in reality — it needs the future — but it is the lower
// bound every other policy is measured against.
type Optimal struct {
	// Future is the remaining reference string.
	Future []int
	pos    int
}

func (o *Optimal) Name() string { return "optimal (Bélády)" }

func (o *Optimal) Evict(frames []*Frame, _ int) int {
	victim, furthest := 0, -1
	for i, f := range frames {
		next := o.nextUse(f.Page)
		if next == -1 {
			return i // never used again: evict immediately
		}
		if next > furthest {
			furthest, victim = next, i
		}
	}
	return victim
}

// nextUse returns how far ahead page is referenced, or -1 if never.
func (o *Optimal) nextUse(page int) int {
	for i := o.pos; i < len(o.Future); i++ {
		if o.Future[i] == page {
			return i
		}
	}
	return -1
}

func (o *Optimal) Touch(*Frame, int) {}

// Advance moves the policy's position through the reference string.
func (o *Optimal) Advance() { o.pos++ }

// TLB is a translation lookaside buffer: a small cache of recent
// translations, so a hit avoids walking the page table.
type TLB struct {
	capacity int
	entries  map[int]*list.Element
	order    *list.List // front is most recently used
}

type tlbEntry struct {
	page  int
	frame int
}

// NewTLB returns a TLB of the given capacity.
func NewTLB(capacity int) *TLB {
	if capacity < 0 {
		capacity = 0
	}
	return &TLB{
		capacity: capacity,
		entries:  make(map[int]*list.Element, capacity),
		order:    list.New(),
	}
}

// Lookup returns the frame for a page, if cached.
func (t *TLB) Lookup(page int) (int, bool) {
	el, ok := t.entries[page]
	if !ok {
		return 0, false
	}
	t.order.MoveToFront(el)
	return el.Value.(*tlbEntry).frame, true
}

// Add caches a translation, evicting the least recently used entry when
// full.
func (t *TLB) Add(page, frame int) {
	if t.capacity == 0 {
		return
	}
	if el, ok := t.entries[page]; ok {
		el.Value.(*tlbEntry).frame = frame
		t.order.MoveToFront(el)
		return
	}
	if t.order.Len() >= t.capacity {
		back := t.order.Back()
		if back != nil {
			delete(t.entries, back.Value.(*tlbEntry).page)
			t.order.Remove(back)
		}
	}
	t.entries[page] = t.order.PushFront(&tlbEntry{page: page, frame: frame})
}

// Invalidate removes a page's translation. This must happen whenever a
// frame is reclaimed, or the TLB would hand out a translation to memory
// that now belongs to a different page — a stale mapping is a correctness
// bug, not a performance one.
func (t *TLB) Invalidate(page int) {
	if el, ok := t.entries[page]; ok {
		t.order.Remove(el)
		delete(t.entries, page)
	}
}

// Flush empties the TLB, as a context switch would.
func (t *TLB) Flush() {
	t.entries = make(map[int]*list.Element, t.capacity)
	t.order.Init()
}

// Len returns the number of cached translations.
func (t *TLB) Len() int { return t.order.Len() }

// MMU is a memory management unit: it translates virtual addresses, handles
// page faults, and evicts under pressure.
type MMU struct {
	frames []*Frame
	// pageTable maps a virtual page to a frame index for resident pages.
	pageTable map[int]int
	tlb       *TLB
	policy    Policy
	clock     int
	Stats     Stats
}

// NewMMU returns an MMU with numFrames of physical memory.
func NewMMU(numFrames, tlbSize int, policy Policy) *MMU {
	if numFrames < 1 {
		numFrames = 1
	}
	if policy == nil {
		policy = LRU{}
	}
	return &MMU{
		frames:    make([]*Frame, 0, numFrames),
		pageTable: map[int]int{},
		tlb:       NewTLB(tlbSize),
		policy:    policy,
	}
}

// Capacity returns the number of physical frames.
func (m *MMU) Capacity() int { return cap(m.frames) }

// Resident reports whether a page is in memory.
func (m *MMU) Resident(page int) bool {
	_, ok := m.pageTable[page]
	return ok
}

// ResidentPages returns the pages currently in memory, sorted.
func (m *MMU) ResidentPages() []int {
	out := make([]int, 0, len(m.pageTable))
	for p := range m.pageTable {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// Access translates a virtual address, loading the page on a fault.
// It returns the physical address.
func (m *MMU) Access(addr Address, write bool) Address {
	m.clock++
	m.Stats.Accesses++

	page := addr.PageNumber()
	offset := addr.Offset()

	// The TLB is consulted first; a hit avoids the page table walk.
	if frameIdx, ok := m.tlb.Lookup(page); ok {
		m.Stats.TLBHits++
		f := m.frames[frameIdx]
		m.policy.Touch(f, m.clock)
		if write {
			f.Dirty = true
		}
		return Address(f.Number*PageSize + offset)
	}
	m.Stats.TLBMisses++

	frameIdx, resident := m.pageTable[page]
	if !resident {
		frameIdx = m.handleFault(page)
	}

	f := m.frames[frameIdx]
	m.policy.Touch(f, m.clock)
	if write {
		f.Dirty = true
	}
	m.tlb.Add(page, frameIdx)
	return Address(f.Number*PageSize + offset)
}

// handleFault brings a page into memory, evicting if necessary.
func (m *MMU) handleFault(page int) int {
	m.Stats.PageFaults++

	// Free frame available.
	if len(m.frames) < cap(m.frames) {
		f := &Frame{
			Number:   len(m.frames),
			Page:     page,
			LoadTime: m.clock,
			LastUsed: m.clock,
		}
		m.frames = append(m.frames, f)
		idx := len(m.frames) - 1
		m.pageTable[page] = idx
		m.policy.Touch(f, m.clock)
		return idx
	}

	// Memory is full: evict.
	victim := m.policy.Evict(m.frames, m.clock)
	f := m.frames[victim]
	m.Stats.Evictions++
	if f.Dirty {
		// A dirty frame must be written back before reuse; a clean one can
		// simply be dropped.
		m.Stats.DiskWrites++
	}

	// The evicted page's translation must leave the TLB, or a later lookup
	// would resolve to a frame that no longer holds it.
	m.tlb.Invalidate(f.Page)
	delete(m.pageTable, f.Page)

	f.Page = page
	f.Dirty = false
	f.Referenced = false
	f.LoadTime = m.clock
	f.LastUsed = m.clock
	m.pageTable[page] = victim
	m.policy.Touch(f, m.clock)
	return victim
}

// Run replays a reference string, returning the resulting statistics.
func (m *MMU) Run(pages []int, writes map[int]bool) Stats {
	for _, p := range pages {
		m.Access(Address(p*PageSize), writes[p])
		if opt, ok := m.policy.(*Optimal); ok {
			opt.Advance()
		}
	}
	return m.Stats
}
