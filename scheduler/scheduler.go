// Package scheduler implements CPU scheduling algorithms.
//
// Each scheduler consumes the same workload and produces a trace of which
// process ran when, plus the standard metrics. Running them against one
// workload is what makes the trade-offs visible: FCFS has the convoy
// problem, SJF is optimal for average wait but can starve long jobs, round
// robin bounds response time at the cost of context switches, and MLFQ
// approximates SJF without needing to know burst times in advance.
package scheduler

import (
	"fmt"
	"sort"

	"github.com/zg/os/process"
)

// Slice is one span of CPU time given to a process.
type Slice struct {
	PID   process.PID
	Name  string
	Start int
	End   int
}

func (s Slice) Duration() int { return s.End - s.Start }

// Result is the outcome of scheduling a workload.
type Result struct {
	Timeline  []Slice
	Processes []*process.Process
	// ContextSwitches counts dispatches, which is the cost side of the
	// trade-off a small quantum buys.
	ContextSwitches int
	TotalTime       int
}

// AverageWaitTime returns the mean time processes spent ready but not
// running.
func (r *Result) AverageWaitTime() float64 {
	if len(r.Processes) == 0 {
		return 0
	}
	total := 0
	for _, p := range r.Processes {
		total += p.WaitTime
	}
	return float64(total) / float64(len(r.Processes))
}

// AverageTurnaroundTime returns the mean time from arrival to completion.
func (r *Result) AverageTurnaroundTime() float64 {
	if len(r.Processes) == 0 {
		return 0
	}
	total := 0
	for _, p := range r.Processes {
		total += p.TurnaroundTime()
	}
	return float64(total) / float64(len(r.Processes))
}

// AverageResponseTime returns the mean time from arrival to first dispatch.
func (r *Result) AverageResponseTime() float64 {
	if len(r.Processes) == 0 {
		return 0
	}
	total := 0
	for _, p := range r.Processes {
		total += p.ResponseTime
	}
	return float64(total) / float64(len(r.Processes))
}

// Throughput is processes completed per unit time.
func (r *Result) Throughput() float64 {
	if r.TotalTime == 0 {
		return 0
	}
	return float64(len(r.Processes)) / float64(r.TotalTime)
}

// Scheduler assigns CPU time to processes.
type Scheduler interface {
	Name() string
	Schedule(procs []*process.Process) *Result
}

// prepare clones the workload and sorts it by arrival, so a scheduler can
// be run repeatedly on the same input without earlier timings leaking in.
func prepare(procs []*process.Process) []*process.Process {
	out := make([]*process.Process, len(procs))
	for i, p := range procs {
		c := p.Clone()
		c.Remaining = c.Burst
		c.State = process.StateReady
		c.WaitTime = 0
		c.CompletionTime = 0
		c.ResponseTime = 0
		out[i] = c
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ArrivalTime != out[j].ArrivalTime {
			return out[i].ArrivalTime < out[j].ArrivalTime
		}
		return out[i].PID < out[j].PID
	})
	return out
}

// finish computes the metrics that follow from the timeline.
func finish(r *Result) {
	for _, p := range r.Processes {
		p.State = process.StateTerminated
		// Wait time is the time spent in the system not running. Deriving
		// it rather than accumulating it during the run means it cannot
		// drift out of step with the timeline.
		p.WaitTime = p.TurnaroundTime() - p.Burst
		if p.WaitTime < 0 {
			p.WaitTime = 0
		}
	}
}

// record appends a slice, merging it with the previous one when the same
// process simply continues — so the timeline shows real dispatches rather
// than one entry per tick.
func record(r *Result, p *process.Process, start, end int) {
	if n := len(r.Timeline); n > 0 && r.Timeline[n-1].PID == p.PID && r.Timeline[n-1].End == start {
		r.Timeline[n-1].End = end
		return
	}
	r.Timeline = append(r.Timeline, Slice{PID: p.PID, Name: p.Name, Start: start, End: end})
	r.ContextSwitches++
}

// String renders a timeline as a Gantt chart.
func (r *Result) String() string {
	out := ""
	for _, s := range r.Timeline {
		out += fmt.Sprintf("| %s (%d-%d) ", s.Name, s.Start, s.End)
	}
	return out + "|"
}
