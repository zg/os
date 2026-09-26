// Package process models processes and the control block a kernel keeps for
// each one.
package process

import (
	"fmt"
	"sync/atomic"
)

// State is a process's position in its lifecycle.
//
//	New -> Ready -> Running -> Terminated
//	          ^        |
//	          |        v
//	          +---- Blocked
type State uint8

const (
	StateNew State = iota
	StateReady
	StateRunning
	StateBlocked
	StateTerminated
)

func (s State) String() string {
	switch s {
	case StateNew:
		return "new"
	case StateReady:
		return "ready"
	case StateRunning:
		return "running"
	case StateBlocked:
		return "blocked"
	case StateTerminated:
		return "terminated"
	}
	return "unknown"
}

// PID is a process identifier.
type PID int

var nextPID atomic.Int64

// NewPID allocates a unique identifier.
func NewPID() PID { return PID(nextPID.Add(1)) }

// ResetPIDs restarts numbering, for deterministic tests.
func ResetPIDs() { nextPID.Store(0) }

// Process is a process control block: everything the kernel must remember
// about a process while it is not running.
type Process struct {
	PID      PID
	Name     string
	State    State
	Priority int // lower is more urgent

	// Burst is the total CPU time the process needs; Remaining counts down
	// as it runs.
	Burst     int
	Remaining int

	// ArrivalTime is when the process entered the system; the rest are
	// filled in by the scheduler and are what the metrics are computed
	// from.
	ArrivalTime    int
	StartTime      int
	CompletionTime int
	WaitTime       int
	// ResponseTime is from arrival to first running, which is the metric
	// interactive systems actually care about.
	ResponseTime int
	firstRun     bool

	// PageTable maps this process's virtual pages to physical frames.
	PageTable map[int]int
	// Registers is a token saved context, enough to show that a context
	// switch preserves and restores state.
	Registers map[string]int64
}

// New returns a process in the New state.
func New(name string, burst, priority int) *Process {
	return &Process{
		PID:       NewPID(),
		Name:      name,
		State:     StateNew,
		Priority:  priority,
		Burst:     burst,
		Remaining: burst,
		PageTable: map[int]int{},
		Registers: map[string]int64{},
	}
}

// TurnaroundTime is the total time from arrival to completion.
func (p *Process) TurnaroundTime() int {
	if p.State != StateTerminated {
		return 0
	}
	return p.CompletionTime - p.ArrivalTime
}

// MarkStarted records the first time a process runs, for response time.
// Only the first call has any effect — response time is about the *first*
// dispatch, not every one.
func (p *Process) MarkStarted(now int) {
	if p.firstRun {
		return
	}
	p.firstRun = true
	p.StartTime = now
	p.ResponseTime = now - p.ArrivalTime
}

// HasStarted reports whether the process has ever run.
func (p *Process) HasStarted() bool { return p.firstRun }

// Clone returns a copy, so a scheduler can be re-run on the same workload
// without the previous run's timings leaking into it.
func (p *Process) Clone() *Process {
	c := *p
	c.PageTable = make(map[int]int, len(p.PageTable))
	for k, v := range p.PageTable {
		c.PageTable[k] = v
	}
	c.Registers = make(map[string]int64, len(p.Registers))
	for k, v := range p.Registers {
		c.Registers[k] = v
	}
	return &c
}

func (p *Process) String() string {
	return fmt.Sprintf("pid=%d name=%s state=%s remaining=%d/%d priority=%d",
		p.PID, p.Name, p.State, p.Remaining, p.Burst, p.Priority)
}
