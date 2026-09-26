// Command os demonstrates the operating system concepts this module
// simulates: CPU scheduling, virtual memory, and a filesystem behind a
// system call interface.
package main

import (
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"text/tabwriter"

	"github.com/zg/os/filesystem"
	"github.com/zg/os/kernel"
	"github.com/zg/os/memory"
	"github.com/zg/os/process"
	"github.com/zg/os/scheduler"
)

func main() {
	demo := flag.String("demo", "all", "which demonstration: sched, mem, fs, syscall, all")
	quantum := flag.Int("quantum", 2, "round robin quantum")
	frames := flag.Int("frames", 3, "physical frames for the memory demo")
	seed := flag.Int64("seed", 1, "random seed")
	flag.Parse()

	out := os.Stdout
	switch *demo {
	case "sched":
		SchedulerDemo(out, *quantum)
	case "mem":
		MemoryDemo(out, *frames, *seed)
	case "fs":
		FilesystemDemo(out)
	case "syscall":
		SyscallDemo(out)
	case "all":
		SchedulerDemo(out, *quantum)
		fmt.Fprintln(out)
		MemoryDemo(out, *frames, *seed)
		fmt.Fprintln(out)
		FilesystemDemo(out)
		fmt.Fprintln(out)
		SyscallDemo(out)
	default:
		fmt.Fprintf(os.Stderr, "os: unknown demo %q\n", *demo)
		os.Exit(2)
	}
}

// SchedulerDemo runs one workload through every scheduler and compares the
// metrics, which is where the trade-offs become visible.
func SchedulerDemo(w io.Writer, quantum int) {
	fmt.Fprintln(w, "=== CPU scheduling ===")
	fmt.Fprintln(w)

	process.ResetPIDs()
	specs := []struct {
		name               string
		burst, arrive, pri int
	}{
		{"init", 8, 0, 3},
		{"editor", 2, 1, 1},
		{"compile", 12, 2, 4},
		{"shell", 3, 3, 1},
		{"daemon", 6, 4, 5},
	}
	procs := make([]*process.Process, len(specs))
	for i, s := range specs {
		p := process.New(s.name, s.burst, s.pri)
		p.ArrivalTime = s.arrive
		procs[i] = p
	}

	fmt.Fprintln(w, "workload:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  process\tburst\tarrival\tpriority")
	for i, s := range specs {
		fmt.Fprintf(tw, "  %s\t%d\t%d\t%d\n", procs[i].Name, s.burst, s.arrive, s.pri)
	}
	tw.Flush()
	fmt.Fprintln(w)

	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "algorithm\tavg wait\tavg turnaround\tavg response\tswitches")
	for _, s := range scheduler.All(quantum) {
		r := s.Schedule(procs)
		fmt.Fprintf(tw, "%s\t%.2f\t%.2f\t%.2f\t%d\n",
			s.Name(), r.AverageWaitTime(), r.AverageTurnaroundTime(),
			r.AverageResponseTime(), r.ContextSwitches)
	}
	tw.Flush()

	fmt.Fprintln(w)
	fmt.Fprintf(w, "round robin (quantum %d) timeline:\n  %s\n", quantum,
		scheduler.RoundRobin{Quantum: quantum}.Schedule(procs))
}

// MemoryDemo compares page replacement policies on one reference string.
func MemoryDemo(w io.Writer, frames int, seed int64) {
	fmt.Fprintln(w, "=== virtual memory ===")
	fmt.Fprintln(w)

	// The standard reference string from the literature.
	refs := []int{7, 0, 1, 2, 0, 3, 0, 4, 2, 3, 0, 3, 2, 1, 2, 0, 1, 7, 0, 1}
	fmt.Fprintf(w, "reference string: %v\n", refs)
	fmt.Fprintf(w, "physical frames:  %d\n\n", frames)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "policy\tfaults\tfault rate\tevictions")
	for _, policy := range []memory.Policy{
		memory.FIFO{}, memory.LRU{}, &memory.Clock{}, &memory.Optimal{Future: refs},
	} {
		m := memory.NewMMU(frames, 0, policy)
		m.Run(refs, nil)
		fmt.Fprintf(tw, "%s\t%d\t%.1f%%\t%d\n",
			policy.Name(), m.Stats.PageFaults, m.Stats.FaultRate()*100, m.Stats.Evictions)
	}
	tw.Flush()

	// Bélády's anomaly: more frames, more faults.
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Bélády's anomaly (FIFO, reference string 1 2 3 4 1 2 5 1 2 3 4 5):")
	anomaly := []int{1, 2, 3, 4, 1, 2, 5, 1, 2, 3, 4, 5}
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  frames\tFIFO faults\tLRU faults")
	for f := 3; f <= 5; f++ {
		fifo := memory.NewMMU(f, 0, memory.FIFO{})
		fifo.Run(anomaly, nil)
		lru := memory.NewMMU(f, 0, memory.LRU{})
		lru.Run(anomaly, nil)
		fmt.Fprintf(tw, "  %d\t%d\t%d\n", f, fifo.Stats.PageFaults, lru.Stats.PageFaults)
	}
	tw.Flush()
	fmt.Fprintln(w, "  (FIFO gets worse from 3 to 4 frames; LRU, a stack algorithm, cannot)")

	// TLB effect.
	fmt.Fprintln(w)
	rng := rand.New(rand.NewSource(seed))
	locality := make([]int, 2000)
	for i := range locality {
		// A working set that shifts slowly, so there is locality to exploit.
		locality[i] = (i/100)*2 + rng.Intn(4)
	}
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TLB size\thit rate\tfaults")
	for _, size := range []int{0, 2, 8, 32} {
		m := memory.NewMMU(16, size, memory.LRU{})
		m.Run(locality, nil)
		fmt.Fprintf(tw, "%d\t%.1f%%\t%d\n", size, m.Stats.TLBHitRate()*100, m.Stats.PageFaults)
	}
	tw.Flush()
}

// FilesystemDemo exercises the inode filesystem.
func FilesystemDemo(w io.Writer) {
	fmt.Fprintln(w, "=== filesystem ===")
	fmt.Fprintln(w)

	fs := filesystem.New(64, 256)
	fs.MkdirAll("/home/user/docs", 0o755)
	fs.WriteFile("/home/user/docs/notes.txt", []byte("some notes about operating systems"), 0o644)
	fs.WriteFile("/home/user/.profile", []byte("export PATH=/bin"), 0o644)
	fs.Link("/home/user/docs/notes.txt", "/home/user/notes-link.txt")

	var walk func(path string, depth int)
	walk = func(path string, depth int) {
		entries, err := fs.ReadDir(path)
		if err != nil {
			return
		}
		for _, e := range entries {
			indent := ""
			for range depth {
				indent += "  "
			}
			kind := "file"
			if e.Type == filesystem.TypeDir {
				kind = "dir "
			}
			fmt.Fprintf(w, "  %s%s %-20s inode=%-3d size=%-5d links=%d\n",
				indent, kind, e.Name, e.Inode, e.Size, e.Links)
			if e.Type == filesystem.TypeDir {
				walk(path+"/"+e.Name, depth+1)
			}
		}
	}
	walk("/", 0)

	fmt.Fprintln(w)
	fmt.Fprintln(w, "  the two notes entries share an inode: that is a hard link")
	fmt.Fprintln(w)

	// Removing one name leaves the data reachable.
	fs.Remove("/home/user/docs/notes.txt")
	data, err := fs.ReadFile("/home/user/notes-link.txt")
	if err == nil {
		fmt.Fprintf(w, "  after removing one name, the data is still readable: %q\n", string(data))
	}

	s := fs.Statfs()
	fmt.Fprintf(w, "\n  blocks: %d/%d used, inodes: %d/%d used\n",
		s.UsedBlocks(), s.TotalBlocks, s.TotalInodes-s.FreeInodes, s.TotalInodes)
}

// SyscallDemo shows the kernel boundary: a process making calls, and the
// trace the kernel records.
func SyscallDemo(w io.Writer) {
	fmt.Fprintln(w, "=== system calls ===")
	fmt.Fprintln(w)

	process.ResetPIDs()
	k := kernel.New(kernel.Config{Frames: 4, TLBSize: 2})
	p := k.Spawn("demo", 10, 1)

	k.Syscall(p, kernel.SysGetPID)
	k.Syscall(p, kernel.SysBrk, 6) // more pages than frames, so it must evict
	k.Syscall(p, kernel.SysMkdir, "/tmp")

	fd := k.Syscall(p, kernel.SysOpen, "/tmp/file.txt", filesystem.O_CREATE|filesystem.O_RDWR).Value
	k.Syscall(p, kernel.SysWrite, fd, "written through a system call")
	k.Syscall(p, kernel.SysClose, fd)
	k.Syscall(p, kernel.SysStat, "/tmp/file.txt")
	k.Syscall(p, kernel.SysWrite, 1, "hello from userspace")

	// A deliberately invalid call, to show validation at the boundary.
	k.Syscall(p, kernel.SysOpen, "/nonexistent/path/file.txt")
	k.Syscall(p, kernel.SysExit, 0)

	for _, e := range k.Trace {
		fmt.Fprintf(w, "  %s\n", e)
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "  stdout: %v\n", k.Output)
	fmt.Fprintf(w, "  memory: %s\n", k.MMU.Stats)
}
