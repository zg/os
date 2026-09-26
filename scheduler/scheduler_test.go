package scheduler

import (
	"testing"

	"github.com/zg/os/process"
)

// workload builds a fixed set of processes. Burst, arrival and priority are
// given per process.
func workload(specs ...[3]int) []*process.Process {
	process.ResetPIDs()
	out := make([]*process.Process, len(specs))
	for i, s := range specs {
		p := process.New(string(rune('A'+i)), s[0], s[2])
		p.ArrivalTime = s[1]
		out[i] = p
	}
	return out
}

// Every scheduler must run every process to completion for exactly its
// burst time — no more, no less. A scheduler that loses or duplicates CPU
// time is broken regardless of its metrics.
func TestAllSchedulersConserveCPUTime(t *testing.T) {
	procs := workload(
		[3]int{5, 0, 2}, [3]int{3, 1, 1}, [3]int{8, 2, 3}, [3]int{2, 3, 1},
	)
	totalBurst := 0
	for _, p := range procs {
		totalBurst += p.Burst
	}

	for _, s := range All(2) {
		t.Run(s.Name(), func(t *testing.T) {
			r := s.Schedule(procs)

			if len(r.Processes) != len(procs) {
				t.Fatalf("got %d processes, want %d", len(r.Processes), len(procs))
			}

			scheduled := 0
			for _, slice := range r.Timeline {
				scheduled += slice.Duration()
			}
			if scheduled != totalBurst {
				t.Errorf("timeline accounts for %d units, want %d", scheduled, totalBurst)
			}

			for _, p := range r.Processes {
				if p.Remaining != 0 {
					t.Errorf("%s did not finish: %d remaining", p.Name, p.Remaining)
				}
				if p.State != process.StateTerminated {
					t.Errorf("%s is %s, want terminated", p.Name, p.State)
				}
				if p.CompletionTime < p.ArrivalTime+p.Burst {
					t.Errorf("%s completed at %d, impossible given arrival %d and burst %d",
						p.Name, p.CompletionTime, p.ArrivalTime, p.Burst)
				}
			}
		})
	}
}

// A process cannot run before it arrives, and the timeline must not overlap
// itself.
func TestTimelineIsPhysicallyPossible(t *testing.T) {
	procs := workload([3]int{4, 0, 1}, [3]int{3, 5, 2}, [3]int{2, 8, 1})

	for _, s := range All(2) {
		t.Run(s.Name(), func(t *testing.T) {
			r := s.Schedule(procs)

			arrival := map[process.PID]int{}
			for _, p := range r.Processes {
				arrival[p.PID] = p.ArrivalTime
			}

			prevEnd := 0
			for i, slice := range r.Timeline {
				if slice.Start < arrival[slice.PID] {
					t.Errorf("slice %d: %s runs at %d but arrives at %d",
						i, slice.Name, slice.Start, arrival[slice.PID])
				}
				if slice.Start < prevEnd {
					t.Errorf("slice %d overlaps the previous one: starts %d, previous ended %d",
						i, slice.Start, prevEnd)
				}
				if slice.End <= slice.Start {
					t.Errorf("slice %d has non-positive duration", i)
				}
				prevEnd = slice.End
			}
		})
	}
}

// FCFS is exactly arrival order, and the convoy effect follows from it.
func TestFCFSRunsInArrivalOrder(t *testing.T) {
	procs := workload([3]int{10, 0, 1}, [3]int{1, 1, 1}, [3]int{1, 2, 1})
	r := FCFS{}.Schedule(procs)

	if len(r.Timeline) != 3 {
		t.Fatalf("got %d slices, want 3", len(r.Timeline))
	}
	want := []string{"A", "B", "C"}
	for i, w := range want {
		if r.Timeline[i].Name != w {
			t.Errorf("slice %d is %s, want %s", i, r.Timeline[i].Name, w)
		}
	}
	// The convoy: the two 1-unit jobs wait behind the 10-unit one.
	if r.AverageWaitTime() < 6 {
		t.Errorf("average wait %.1f is lower than the convoy effect allows", r.AverageWaitTime())
	}
}

// SJF is provably optimal for average wait time, so no other
// non-preemptive scheduler may beat it on a workload where all processes
// arrive together.
func TestSJFMinimisesAverageWait(t *testing.T) {
	// All arrive at 0, so arrival order cannot help another algorithm.
	procs := workload([3]int{7, 0, 1}, [3]int{3, 0, 1}, [3]int{1, 0, 1}, [3]int{5, 0, 1})

	sjf := SJF{}.Schedule(procs)
	fcfs := FCFS{}.Schedule(procs)

	if sjf.AverageWaitTime() > fcfs.AverageWaitTime() {
		t.Errorf("SJF wait %.2f should not exceed FCFS wait %.2f",
			sjf.AverageWaitTime(), fcfs.AverageWaitTime())
	}
	// Shortest first: 1, 3, 5, 7. Waits are 0, 1, 4, 9 → average 3.5.
	if got := sjf.AverageWaitTime(); got != 3.5 {
		t.Errorf("average wait = %.2f, want 3.5", got)
	}
}

// Round robin bounds response time: that is the whole reason to use it.
func TestRoundRobinBoundsResponseTime(t *testing.T) {
	// Four processes arriving together, quantum 2.
	procs := workload([3]int{10, 0, 1}, [3]int{10, 0, 1}, [3]int{10, 0, 1}, [3]int{10, 0, 1})
	const quantum = 2

	r := RoundRobin{Quantum: quantum}.Schedule(procs)

	// With n processes and quantum q, nothing waits more than (n-1)q to
	// run for the first time.
	bound := (len(procs) - 1) * quantum
	for _, p := range r.Processes {
		if p.ResponseTime > bound {
			t.Errorf("%s responded after %d, exceeding the bound of %d",
				p.Name, p.ResponseTime, bound)
		}
	}

	// FCFS has no such bound, and should be worse here.
	fcfs := FCFS{}.Schedule(procs)
	if r.AverageResponseTime() >= fcfs.AverageResponseTime() {
		t.Errorf("round robin response %.1f should beat FCFS %.1f",
			r.AverageResponseTime(), fcfs.AverageResponseTime())
	}
}

// A smaller quantum improves response time but costs more context
// switches. That trade-off is the reason quantum is tunable at all.
func TestQuantumTradesSwitchesForResponsiveness(t *testing.T) {
	procs := workload([3]int{12, 0, 1}, [3]int{12, 0, 1}, [3]int{12, 0, 1})

	small := RoundRobin{Quantum: 1}.Schedule(procs)
	large := RoundRobin{Quantum: 6}.Schedule(procs)

	if small.AverageResponseTime() > large.AverageResponseTime() {
		t.Errorf("a smaller quantum should respond sooner: %.1f vs %.1f",
			small.AverageResponseTime(), large.AverageResponseTime())
	}
	if small.ContextSwitches <= large.ContextSwitches {
		t.Errorf("a smaller quantum should cost more switches: %d vs %d",
			small.ContextSwitches, large.ContextSwitches)
	}
}

// A quantum at least as long as the longest burst makes round robin
// degenerate into FCFS.
func TestLargeQuantumBecomesFCFS(t *testing.T) {
	procs := workload([3]int{3, 0, 1}, [3]int{4, 1, 1}, [3]int{2, 2, 1})

	rr := RoundRobin{Quantum: 100}.Schedule(procs)
	fcfs := FCFS{}.Schedule(procs)

	if len(rr.Timeline) != len(fcfs.Timeline) {
		t.Fatalf("timelines differ in length: %d vs %d", len(rr.Timeline), len(fcfs.Timeline))
	}
	for i := range rr.Timeline {
		if rr.Timeline[i].Name != fcfs.Timeline[i].Name {
			t.Errorf("slice %d: round robin ran %s, FCFS ran %s",
				i, rr.Timeline[i].Name, fcfs.Timeline[i].Name)
		}
	}
}

func TestPriorityRunsMostUrgentFirst(t *testing.T) {
	// All arrive together; priority 0 is most urgent.
	procs := workload([3]int{3, 0, 3}, [3]int{3, 0, 1}, [3]int{3, 0, 2})
	r := Priority{}.Schedule(procs)

	if r.Timeline[0].Name != "B" {
		t.Errorf("first to run is %s, want B (priority 1)", r.Timeline[0].Name)
	}
	if r.Timeline[len(r.Timeline)-1].Name != "A" {
		t.Errorf("last to run is %s, want A (priority 3)", r.Timeline[len(r.Timeline)-1].Name)
	}
}

// Without ageing, a stream of urgent work starves a low-priority process.
// With it, the low-priority process must eventually run.
func TestPriorityAgeingPreventsStarvation(t *testing.T) {
	process.ResetPIDs()
	// One low-priority long job, then a stream of urgent short ones.
	var procs []*process.Process
	low := process.New("low", 5, 10)
	low.ArrivalTime = 0
	procs = append(procs, low)
	for i := range 8 {
		p := process.New("urgent", 2, 0)
		p.ArrivalTime = i * 2
		procs = append(procs, p)
	}

	aged := Priority{Preemptive: true, AgeInterval: 3}.Schedule(procs)

	var lowResult *process.Process
	for _, p := range aged.Processes {
		if p.Name == "low" {
			lowResult = p
		}
	}
	if lowResult == nil {
		t.Fatal("the low-priority process is missing from the result")
	}
	if !lowResult.HasStarted() {
		t.Error("the low-priority process never ran despite ageing")
	}
	if lowResult.Remaining != 0 {
		t.Errorf("the low-priority process did not finish: %d remaining", lowResult.Remaining)
	}
}

// MLFQ approximates SJF without knowing burst times: a short interactive
// job arriving among long CPU-bound ones must still respond quickly.
func TestMLFQFavoursShortJobs(t *testing.T) {
	process.ResetPIDs()
	long1 := process.New("long1", 20, 0)
	long2 := process.New("long2", 20, 0)
	short := process.New("short", 2, 0)
	short.ArrivalTime = 4

	procs := []*process.Process{long1, long2, short}
	r := MLFQ{Quanta: []int{1, 2, 4, 8}, BoostInterval: 50}.Schedule(procs)

	var shortResult *process.Process
	for _, p := range r.Processes {
		if p.Name == "short" {
			shortResult = p
		}
	}
	if shortResult == nil {
		t.Fatal("the short job is missing")
	}
	// It should be dispatched promptly, since it enters at the top queue
	// while the long jobs have sunk.
	if shortResult.ResponseTime > 4 {
		t.Errorf("short job responded after %d; MLFQ should favour it", shortResult.ResponseTime)
	}
	// And finish long before the long jobs do.
	for _, p := range r.Processes {
		if p.Name != "short" && p.CompletionTime < shortResult.CompletionTime {
			t.Errorf("%s finished before the short job", p.Name)
		}
	}
}

// Boosting is what stops a sunken job starving forever.
func TestMLFQBoostPreventsStarvation(t *testing.T) {
	process.ResetPIDs()
	long := process.New("long", 30, 0)
	procs := []*process.Process{long}
	for i := range 10 {
		p := process.New("short", 1, 0)
		p.ArrivalTime = i * 2
		procs = append(procs, p)
	}

	r := MLFQ{Quanta: []int{1, 2, 4}, BoostInterval: 10}.Schedule(procs)
	for _, p := range r.Processes {
		if p.Remaining != 0 {
			t.Errorf("%s starved: %d remaining", p.Name, p.Remaining)
		}
	}
}

// Response time measures the *first* dispatch, so a preempted process must
// not have it overwritten.
func TestResponseTimeIsFirstDispatchOnly(t *testing.T) {
	procs := workload([3]int{6, 0, 1}, [3]int{6, 0, 1})
	r := RoundRobin{Quantum: 2}.Schedule(procs)

	for _, p := range r.Processes {
		if p.ResponseTime > p.WaitTime {
			t.Errorf("%s: response time %d exceeds total wait %d, so it was overwritten on a later dispatch",
				p.Name, p.ResponseTime, p.WaitTime)
		}
	}
}

// The CPU must idle rather than run something that has not arrived.
func TestIdleGapsWhenNothingHasArrived(t *testing.T) {
	procs := workload([3]int{2, 0, 1}, [3]int{2, 10, 1})

	for _, s := range All(2) {
		t.Run(s.Name(), func(t *testing.T) {
			r := s.Schedule(procs)
			if r.TotalTime < 12 {
				t.Errorf("total time %d; the second process cannot finish before 12", r.TotalTime)
			}
		})
	}
}

func TestEmptyWorkload(t *testing.T) {
	for _, s := range All(2) {
		t.Run(s.Name(), func(t *testing.T) {
			r := s.Schedule(nil)
			if len(r.Timeline) != 0 || r.TotalTime != 0 {
				t.Errorf("empty workload produced %d slices over %d time", len(r.Timeline), r.TotalTime)
			}
			// The metrics must not divide by zero.
			if r.AverageWaitTime() != 0 || r.Throughput() != 0 {
				t.Error("metrics on an empty workload should be zero")
			}
		})
	}
}

// Scheduling the same workload twice must give the same answer — and must
// not mutate the caller's processes.
func TestSchedulingIsRepeatable(t *testing.T) {
	procs := workload([3]int{5, 0, 2}, [3]int{3, 1, 1}, [3]int{7, 2, 3})

	for _, s := range All(2) {
		t.Run(s.Name(), func(t *testing.T) {
			first := s.Schedule(procs)
			second := s.Schedule(procs)

			if first.TotalTime != second.TotalTime {
				t.Errorf("total time differs between runs: %d vs %d", first.TotalTime, second.TotalTime)
			}
			if len(first.Timeline) != len(second.Timeline) {
				t.Fatalf("timeline length differs: %d vs %d", len(first.Timeline), len(second.Timeline))
			}
			for i := range first.Timeline {
				if first.Timeline[i] != second.Timeline[i] {
					t.Errorf("slice %d differs: %+v vs %+v", i, first.Timeline[i], second.Timeline[i])
				}
			}
			// The caller's processes must be untouched.
			for _, p := range procs {
				if p.Remaining != p.Burst {
					t.Errorf("%s was mutated: remaining %d, burst %d", p.Name, p.Remaining, p.Burst)
				}
			}
		})
	}
}

func TestQuantumDefaultsToOne(t *testing.T) {
	procs := workload([3]int{3, 0, 1})
	r := RoundRobin{Quantum: 0}.Schedule(procs)
	if r.TotalTime != 3 {
		t.Errorf("total time = %d, want 3", r.TotalTime)
	}
}
