# os

System software that manages resources and provides common services for
programs — simulated in userspace, so the mechanisms are observable and
testable.

This is **not** a bootable kernel. Go's runtime assumes an operating system
underneath it — a scheduler, a garbage collector, threads — so a real kernel
written in it would have to discard most of the language. What this models
instead are the mechanisms themselves: CPU scheduling, virtual memory
translation and page replacement, an inode filesystem, and a system call
boundary. Each is implemented properly and measured, which is the part
worth understanding.

## Install

```sh
go install github.com/zg/os/cmd/os@latest
```

## Use

```sh
os                        # run every demonstration
os -demo sched            # scheduler comparison
os -demo mem -frames 3    # page replacement and Bélády's anomaly
os -demo fs               # inodes, directories, hard links
os -demo syscall          # the kernel boundary and its trace
```

```
algorithm                      avg wait  avg turnaround  avg response  switches
first-come, first-served       11.00     17.20           11.00         5
shortest job first              8.00     14.20            8.00         5
shortest remaining time first   6.00     12.20            3.80         6
round robin                    11.80     18.00            2.80        14
priority (preemptive)           8.40     14.60            6.00         8
multi-level feedback queue     12.20     18.40            0.00        17
```

That table is the point: SJF wins on wait time, round robin wins on
responsiveness and pays for it in context switches, and MLFQ gets the best
response of all by putting every arrival at the top queue — at the highest
switching cost.

## Subsystems

**`scheduler`** — FCFS, SJF, SRTF, round robin, priority (with ageing), and
MLFQ. Each consumes the same workload and reports wait, turnaround,
response and switch count.

**`memory`** — page tables, a TLB, and FIFO / LRU / clock / optimal
replacement, with fault, eviction and write-back statistics.

**`filesystem`** — inodes, a free block bitmap, directories as files, hard
links, file descriptors with independent offsets.

**`kernel`** — a system call dispatch table over the three, with argument
validation and a trace of every call.

## What the tests establish

These are simulations, so the tests assert the *properties the real
algorithms have* rather than golden numbers:

- **SJF minimises average wait time.** Provably optimal, so no other
  non-preemptive scheduler may beat it when all processes arrive together.
- **Round robin bounds response time** at (n−1)q — the reason to use it at
  all — and a smaller quantum trades more context switches for a better
  response, which is checked in both directions.
- **A large quantum degenerates round robin into FCFS**, and the timelines
  are compared slice by slice to prove it.
- **Ageing prevents starvation.** A low-priority job buried under a stream
  of urgent ones must still finish.
- **Bélády's anomaly is exhibited by FIFO**: on the standard reference
  string, going from 3 frames to 4 makes faults go *up*, 9 → 10. A policy
  that does not show this is not really FIFO.
- **LRU cannot exhibit it**, being a stack algorithm — verified across 1 to
  12 frames on a random reference string.
- **The optimal policy is a lower bound**: no implementable policy beats it,
  and it hits exactly the textbook 9 faults.
- **Clock gives a second chance**, and degenerates to FIFO when every
  reference bit is set — both pinned, since the second is easy to mistake
  for a bug.
- **Every scheduler conserves CPU time**: the timeline must account for
  exactly the total burst, with no overlaps and nothing running before it
  arrives.

## Design notes

**A stale TLB entry is a correctness bug, not a performance one.** When a
frame is reclaimed, the evicted page's translation must leave the TLB —
otherwise a later lookup resolves to a frame now holding different data.
There is a test that evicts a page and checks the next access faults rather
than being served from the stale entry.

**Wait time is derived, not accumulated.** Computing it from the timeline
at the end means it cannot drift out of step with what actually ran.

**Response time records the first dispatch only.** A preempted process that
overwrote it on every dispatch would report nonsense for round robin, which
is precisely the scheduler the metric exists to evaluate.

**Schedulers clone their input.** Running one twice on the same workload
must give the same answer and must not mutate the caller's processes — both
are tested for every algorithm.

**File offsets live in the open file description, not the inode**, mirroring
Unix: two descriptors on one file have independent positions.

**An inode outlives its names.** The link count decides when blocks are
freed, which is what makes hard links work — and what makes forgetting it a
silent leak.

**Failed allocation rolls back.** A write that runs out of space frees the
blocks it already took, rather than leaving a half-written file holding
them.

**System call arguments arrive untyped**, as they would from registers, and
every one is checked rather than asserted. A terminated process cannot call
back into the kernel.

## Tests

```sh
go test -race ./...
go test -bench=. ./...
```
