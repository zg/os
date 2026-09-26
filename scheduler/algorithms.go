package scheduler

import (
	"sort"

	"github.com/zg/os/process"
)

// FCFS runs processes to completion in arrival order.
//
// Simple and free of starvation, but subject to the convoy effect: one long
// job at the front delays every short job behind it, inflating average
// wait time badly.
type FCFS struct{}

func (FCFS) Name() string { return "first-come, first-served" }

func (f FCFS) Schedule(procs []*process.Process) *Result {
	r := &Result{Processes: prepare(procs)}
	now := 0
	for _, p := range r.Processes {
		// The CPU idles if the next arrival is in the future.
		if now < p.ArrivalTime {
			now = p.ArrivalTime
		}
		p.MarkStarted(now)
		start := now
		now += p.Remaining
		p.Remaining = 0
		p.CompletionTime = now
		record(r, p, start, now)
	}
	r.TotalTime = now
	finish(r)
	return r
}

// SJF runs the shortest available job next, without preemption.
//
// Provably optimal for average wait time, but it needs burst times in
// advance — which a real kernel does not have — and a steady stream of
// short jobs starves long ones.
type SJF struct{}

func (SJF) Name() string { return "shortest job first" }

func (s SJF) Schedule(procs []*process.Process) *Result {
	r := &Result{Processes: prepare(procs)}
	now, done := 0, 0

	for done < len(r.Processes) {
		// Pick the shortest job that has arrived.
		var next *process.Process
		for _, p := range r.Processes {
			if p.Remaining == 0 || p.ArrivalTime > now {
				continue
			}
			if next == nil || p.Remaining < next.Remaining ||
				(p.Remaining == next.Remaining && p.PID < next.PID) {
				next = p
			}
		}

		if next == nil {
			// Nothing has arrived yet; idle until the next arrival rather
			// than spinning.
			earliest := -1
			for _, p := range r.Processes {
				if p.Remaining > 0 && (earliest < 0 || p.ArrivalTime < earliest) {
					earliest = p.ArrivalTime
				}
			}
			if earliest < 0 {
				break
			}
			now = earliest
			continue
		}

		next.MarkStarted(now)
		start := now
		now += next.Remaining
		next.Remaining = 0
		next.CompletionTime = now
		record(r, next, start, now)
		done++
	}
	r.TotalTime = now
	finish(r)
	return r
}

// SRTF is preemptive shortest-job-first: a newly arrived shorter job
// preempts the running one.
type SRTF struct{}

func (SRTF) Name() string { return "shortest remaining time first" }

func (s SRTF) Schedule(procs []*process.Process) *Result {
	r := &Result{Processes: prepare(procs)}
	now, done := 0, 0

	for done < len(r.Processes) {
		var next *process.Process
		for _, p := range r.Processes {
			if p.Remaining == 0 || p.ArrivalTime > now {
				continue
			}
			if next == nil || p.Remaining < next.Remaining ||
				(p.Remaining == next.Remaining && p.PID < next.PID) {
				next = p
			}
		}
		if next == nil {
			earliest := -1
			for _, p := range r.Processes {
				if p.Remaining > 0 && (earliest < 0 || p.ArrivalTime < earliest) {
					earliest = p.ArrivalTime
				}
			}
			if earliest < 0 {
				break
			}
			now = earliest
			continue
		}

		// Run one tick at a time: preemption can only be decided by
		// re-checking after each unit of time.
		next.MarkStarted(now)
		record(r, next, now, now+1)
		next.Remaining--
		now++
		if next.Remaining == 0 {
			next.CompletionTime = now
			done++
		}
	}
	r.TotalTime = now
	finish(r)
	return r
}

// RoundRobin gives each process a fixed quantum in turn.
//
// This is what bounds response time: with n processes and quantum q, no
// process waits longer than (n-1)q to run. A smaller quantum improves
// responsiveness but costs more context switches, which is the trade-off
// the metrics make visible.
type RoundRobin struct{ Quantum int }

func (rr RoundRobin) Name() string { return "round robin" }

func (rr RoundRobin) Schedule(procs []*process.Process) *Result {
	quantum := rr.Quantum
	if quantum <= 0 {
		quantum = 1
	}

	r := &Result{Processes: prepare(procs)}
	now, done := 0, 0

	var queue []*process.Process
	arrived := 0

	// Admit everything that has arrived by `now`.
	admit := func() {
		for arrived < len(r.Processes) && r.Processes[arrived].ArrivalTime <= now {
			queue = append(queue, r.Processes[arrived])
			arrived++
		}
	}
	admit()

	for done < len(r.Processes) {
		if len(queue) == 0 {
			if arrived >= len(r.Processes) {
				break
			}
			now = r.Processes[arrived].ArrivalTime
			admit()
			continue
		}

		p := queue[0]
		queue = queue[1:]

		p.MarkStarted(now)
		slice := min(quantum, p.Remaining)
		start := now
		now += slice
		p.Remaining -= slice
		record(r, p, start, now)

		// Admit arrivals *before* requeueing the preempted process, so a
		// process that arrived during this quantum runs before the one
		// that just used its turn. Requeueing first would give the running
		// process an unfair extra turn.
		admit()

		if p.Remaining > 0 {
			queue = append(queue, p)
		} else {
			p.CompletionTime = now
			done++
		}
	}
	r.TotalTime = now
	finish(r)
	return r
}

// Priority runs the highest-priority available process, optionally with
// ageing to prevent starvation.
type Priority struct {
	Preemptive bool
	// AgeInterval, when positive, promotes a waiting process's effective
	// priority every this many ticks. Without it, a stream of high-priority
	// work starves low-priority processes indefinitely.
	AgeInterval int
}

func (p Priority) Name() string {
	if p.Preemptive {
		return "priority (preemptive)"
	}
	return "priority"
}

func (pr Priority) Schedule(procs []*process.Process) *Result {
	r := &Result{Processes: prepare(procs)}
	now, done := 0, 0

	// effective holds the aged priority, so the original is preserved for
	// reporting.
	effective := map[process.PID]int{}
	waiting := map[process.PID]int{}
	for _, p := range r.Processes {
		effective[p.PID] = p.Priority
	}

	for done < len(r.Processes) {
		var next *process.Process
		for _, p := range r.Processes {
			if p.Remaining == 0 || p.ArrivalTime > now {
				continue
			}
			if next == nil || effective[p.PID] < effective[next.PID] ||
				(effective[p.PID] == effective[next.PID] && p.PID < next.PID) {
				next = p
			}
		}
		if next == nil {
			earliest := -1
			for _, p := range r.Processes {
				if p.Remaining > 0 && (earliest < 0 || p.ArrivalTime < earliest) {
					earliest = p.ArrivalTime
				}
			}
			if earliest < 0 {
				break
			}
			now = earliest
			continue
		}

		next.MarkStarted(now)
		run := next.Remaining
		if pr.Preemptive {
			run = 1 // re-decide every tick
		}
		record(r, next, now, now+run)
		next.Remaining -= run
		now += run
		waiting[next.PID] = 0

		if pr.AgeInterval > 0 {
			for _, p := range r.Processes {
				if p == next || p.Remaining == 0 || p.ArrivalTime > now {
					continue
				}
				waiting[p.PID] += run
				for waiting[p.PID] >= pr.AgeInterval && effective[p.PID] > 0 {
					effective[p.PID]--
					waiting[p.PID] -= pr.AgeInterval
				}
			}
		}

		if next.Remaining == 0 {
			next.CompletionTime = now
			done++
		}
	}
	r.TotalTime = now
	finish(r)
	return r
}

// MLFQ is a multi-level feedback queue.
//
// It approximates SJF without knowing burst times in advance: everything
// starts in the highest-priority queue, and a process that uses its whole
// quantum is demoted. Short and interactive jobs therefore stay near the
// top, while CPU-bound jobs sink. Periodic boosting returns everything to
// the top queue, which is what stops the sunken jobs starving.
type MLFQ struct {
	// Quanta gives each level's time slice, longest at the bottom.
	Quanta []int
	// BoostInterval, when positive, promotes everything to the top queue
	// this often.
	BoostInterval int
}

func (MLFQ) Name() string { return "multi-level feedback queue" }

func (m MLFQ) Schedule(procs []*process.Process) *Result {
	quanta := m.Quanta
	if len(quanta) == 0 {
		quanta = []int{1, 2, 4, 8}
	}

	r := &Result{Processes: prepare(procs)}
	now, done := 0, 0
	lastBoost := 0

	queues := make([][]*process.Process, len(quanta))
	arrived := 0

	admit := func() {
		for arrived < len(r.Processes) && r.Processes[arrived].ArrivalTime <= now {
			// New arrivals enter at the top, which is what gives an
			// interactive process immediate responsiveness.
			queues[0] = append(queues[0], r.Processes[arrived])
			arrived++
		}
	}
	admit()

	boost := func() {
		if m.BoostInterval <= 0 || now-lastBoost < m.BoostInterval {
			return
		}
		lastBoost = now
		for level := 1; level < len(queues); level++ {
			queues[0] = append(queues[0], queues[level]...)
			queues[level] = nil
		}
	}

	for done < len(r.Processes) {
		boost()

		level := -1
		for i, q := range queues {
			if len(q) > 0 {
				level = i
				break
			}
		}
		if level < 0 {
			if arrived >= len(r.Processes) {
				break
			}
			now = r.Processes[arrived].ArrivalTime
			admit()
			continue
		}

		p := queues[level][0]
		queues[level] = queues[level][1:]

		p.MarkStarted(now)
		slice := min(quanta[level], p.Remaining)
		start := now
		now += slice
		p.Remaining -= slice
		record(r, p, start, now)

		admit()

		switch {
		case p.Remaining == 0:
			p.CompletionTime = now
			done++
		case slice == quanta[level] && level < len(queues)-1:
			// Used the whole quantum: CPU-bound, so demote it.
			queues[level+1] = append(queues[level+1], p)
		default:
			queues[level] = append(queues[level], p)
		}
	}
	r.TotalTime = now
	finish(r)
	return r
}

// All returns one of each scheduler, for comparison.
func All(quantum int) []Scheduler {
	return []Scheduler{
		FCFS{},
		SJF{},
		SRTF{},
		RoundRobin{Quantum: quantum},
		Priority{},
		Priority{Preemptive: true, AgeInterval: 5},
		MLFQ{Quanta: []int{1, 2, 4, 8}, BoostInterval: 20},
	}
}

// SortByPID orders processes by identifier, for stable reporting.
func SortByPID(procs []*process.Process) {
	sort.SliceStable(procs, func(i, j int) bool { return procs[i].PID < procs[j].PID })
}
