// Package kernel ties the subsystems together behind a system call
// interface.
//
// A system call is the boundary between a process and the kernel: the
// process names an operation and its arguments, and the kernel validates
// them before touching any state. Modelling it as an explicit dispatch
// table — rather than as direct method calls — is what makes that boundary
// visible, along with the bookkeeping a real kernel does around it.
package kernel

import (
	"fmt"
	"sort"
	"sync"

	"github.com/zg/os/filesystem"
	"github.com/zg/os/memory"
	"github.com/zg/os/process"
	"github.com/zg/os/scheduler"
)

// SyscallNumber identifies a system call.
type SyscallNumber int

const (
	SysWrite SyscallNumber = iota
	SysRead
	SysOpen
	SysClose
	SysFork
	SysExit
	SysGetPID
	SysMkdir
	SysUnlink
	SysStat
	SysListDir
	SysBrk
)

func (s SyscallNumber) String() string {
	names := map[SyscallNumber]string{
		SysWrite: "write", SysRead: "read", SysOpen: "open", SysClose: "close",
		SysFork: "fork", SysExit: "exit", SysGetPID: "getpid", SysMkdir: "mkdir",
		SysUnlink: "unlink", SysStat: "stat", SysListDir: "listdir", SysBrk: "brk",
	}
	if n, ok := names[s]; ok {
		return n
	}
	return fmt.Sprintf("syscall(%d)", int(s))
}

// Result is a system call's return value, mirroring the Unix convention of
// a value plus an error number.
type Result struct {
	Value int
	Err   error
}

// handler implements one system call.
type handler func(k *Kernel, p *process.Process, args []any) Result

// Kernel holds the state the subsystems share.
type Kernel struct {
	FS        *filesystem.FS
	MMU       *memory.MMU
	Scheduler scheduler.Scheduler

	mu        sync.Mutex
	processes map[process.PID]*process.Process
	current   *process.Process
	handlers  map[SyscallNumber]handler

	// Trace records every call, which is what makes the boundary
	// inspectable.
	Trace []TraceEntry
	// Output collects what processes write to stdout.
	Output []string
}

// TraceEntry is one recorded system call.
type TraceEntry struct {
	PID  process.PID
	Call SyscallNumber
	Args []any
	Res  Result
}

func (t TraceEntry) String() string {
	status := fmt.Sprintf("%d", t.Res.Value)
	if t.Res.Err != nil {
		status = "error: " + t.Res.Err.Error()
	}
	return fmt.Sprintf("[pid %d] %s%v = %s", t.PID, t.Call, t.Args, status)
}

// Config configures a kernel.
type Config struct {
	Inodes    int
	Blocks    int
	Frames    int
	TLBSize   int
	Policy    memory.Policy
	Scheduler scheduler.Scheduler
}

// New boots a kernel.
func New(cfg Config) *Kernel {
	if cfg.Inodes == 0 {
		cfg.Inodes = 64
	}
	if cfg.Blocks == 0 {
		cfg.Blocks = 256
	}
	if cfg.Frames == 0 {
		cfg.Frames = 16
	}
	if cfg.TLBSize == 0 {
		cfg.TLBSize = 4
	}
	if cfg.Policy == nil {
		cfg.Policy = memory.LRU{}
	}
	if cfg.Scheduler == nil {
		cfg.Scheduler = scheduler.RoundRobin{Quantum: 2}
	}

	k := &Kernel{
		FS:        filesystem.New(cfg.Inodes, cfg.Blocks),
		MMU:       memory.NewMMU(cfg.Frames, cfg.TLBSize, cfg.Policy),
		Scheduler: cfg.Scheduler,
		processes: map[process.PID]*process.Process{},
	}
	k.registerHandlers()
	return k
}

func (k *Kernel) registerHandlers() {
	k.handlers = map[SyscallNumber]handler{
		SysGetPID: func(k *Kernel, p *process.Process, _ []any) Result {
			return Result{Value: int(p.PID)}
		},

		SysWrite: func(k *Kernel, p *process.Process, args []any) Result {
			fd, err := argInt(args, 0)
			if err != nil {
				return Result{Err: err}
			}
			data, err := argBytes(args, 1)
			if err != nil {
				return Result{Err: err}
			}
			// Descriptor 1 is stdout by convention, and is not a file.
			if fd == 1 || fd == 2 {
				k.Output = append(k.Output, string(data))
				return Result{Value: len(data)}
			}
			n, err := k.FS.Write(fd, data)
			return Result{Value: n, Err: err}
		},

		SysRead: func(k *Kernel, p *process.Process, args []any) Result {
			fd, err := argInt(args, 0)
			if err != nil {
				return Result{Err: err}
			}
			size, err := argInt(args, 1)
			if err != nil {
				return Result{Err: err}
			}
			if size < 0 {
				return Result{Err: fmt.Errorf("kernel: negative read size %d", size)}
			}
			buf := make([]byte, size)
			n, readErr := k.FS.Read(fd, buf)
			return Result{Value: n, Err: readErr}
		},

		SysOpen: func(k *Kernel, p *process.Process, args []any) Result {
			path, err := argString(args, 0)
			if err != nil {
				return Result{Err: err}
			}
			flags := 0
			if len(args) > 1 {
				if f, err := argInt(args, 1); err == nil {
					flags = f
				}
			}
			fd, err := k.FS.Open(path, flags)
			return Result{Value: fd, Err: err}
		},

		SysClose: func(k *Kernel, p *process.Process, args []any) Result {
			fd, err := argInt(args, 0)
			if err != nil {
				return Result{Err: err}
			}
			return Result{Err: k.FS.Close(fd)}
		},

		SysMkdir: func(k *Kernel, p *process.Process, args []any) Result {
			path, err := argString(args, 0)
			if err != nil {
				return Result{Err: err}
			}
			return Result{Err: k.FS.Mkdir(path, 0o755)}
		},

		SysUnlink: func(k *Kernel, p *process.Process, args []any) Result {
			path, err := argString(args, 0)
			if err != nil {
				return Result{Err: err}
			}
			return Result{Err: k.FS.Remove(path)}
		},

		SysStat: func(k *Kernel, p *process.Process, args []any) Result {
			path, err := argString(args, 0)
			if err != nil {
				return Result{Err: err}
			}
			node, err := k.FS.Stat(path)
			if err != nil {
				return Result{Err: err}
			}
			return Result{Value: node.Size}
		},

		SysListDir: func(k *Kernel, p *process.Process, args []any) Result {
			path, err := argString(args, 0)
			if err != nil {
				return Result{Err: err}
			}
			entries, err := k.FS.ReadDir(path)
			return Result{Value: len(entries), Err: err}
		},

		SysFork: func(k *Kernel, p *process.Process, _ []any) Result {
			child := process.New(p.Name+"-child", p.Remaining, p.Priority)
			child.ArrivalTime = p.ArrivalTime
			// The child inherits a *copy* of the page table, so later
			// mappings in one do not affect the other.
			for page, frame := range p.PageTable {
				child.PageTable[page] = frame
			}
			k.mu.Lock()
			k.processes[child.PID] = child
			k.mu.Unlock()
			// fork returns the child's PID to the parent, as Unix does.
			return Result{Value: int(child.PID)}
		},

		SysExit: func(k *Kernel, p *process.Process, args []any) Result {
			code := 0
			if len(args) > 0 {
				if c, err := argInt(args, 0); err == nil {
					code = c
				}
			}
			p.State = process.StateTerminated
			return Result{Value: code}
		},

		SysBrk: func(k *Kernel, p *process.Process, args []any) Result {
			pages, err := argInt(args, 0)
			if err != nil {
				return Result{Err: err}
			}
			if pages < 0 {
				return Result{Err: fmt.Errorf("kernel: negative allocation %d", pages)}
			}
			// Touching each page is what actually maps it, exactly as a
			// real first-touch allocation would.
			base := len(p.PageTable)
			for i := range pages {
				page := base + i
				phys := k.MMU.Access(memory.Address(page*memory.PageSize), true)
				p.PageTable[page] = int(phys) / memory.PageSize
			}
			return Result{Value: pages}
		},
	}
}

// Spawn creates a process and registers it.
func (k *Kernel) Spawn(name string, burst, priority int) *process.Process {
	p := process.New(name, burst, priority)
	p.State = process.StateReady
	k.mu.Lock()
	k.processes[p.PID] = p
	k.mu.Unlock()
	return p
}

// Processes returns every registered process, ordered by PID.
func (k *Kernel) Processes() []*process.Process {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]*process.Process, 0, len(k.processes))
	for _, p := range k.processes {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// ErrNoSuchCall is returned for an unknown system call number.
var ErrNoSuchCall = fmt.Errorf("kernel: no such system call")

// Syscall performs a system call on behalf of a process.
func (k *Kernel) Syscall(p *process.Process, num SyscallNumber, args ...any) Result {
	if p == nil {
		return Result{Err: fmt.Errorf("kernel: no calling process")}
	}
	// A terminated process must not be able to call into the kernel.
	if p.State == process.StateTerminated && num != SysExit {
		return Result{Err: fmt.Errorf("kernel: process %d has terminated", p.PID)}
	}

	h, ok := k.handlers[num]
	if !ok {
		res := Result{Err: fmt.Errorf("%w: %d", ErrNoSuchCall, int(num))}
		k.Trace = append(k.Trace, TraceEntry{PID: p.PID, Call: num, Args: args, Res: res})
		return res
	}

	res := h(k, p, args)
	k.Trace = append(k.Trace, TraceEntry{PID: p.PID, Call: num, Args: args, Res: res})
	return res
}

// Run schedules every ready process and returns the result.
func (k *Kernel) Run() *scheduler.Result {
	procs := k.Processes()
	ready := make([]*process.Process, 0, len(procs))
	for _, p := range procs {
		if p.State != process.StateTerminated {
			ready = append(ready, p)
		}
	}
	return k.Scheduler.Schedule(ready)
}

// Argument helpers. A system call's arguments arrive untyped, exactly as
// they would from a register or a stack, so each must be checked rather
// than asserted.

func argInt(args []any, i int) (int, error) {
	if i >= len(args) {
		return 0, fmt.Errorf("kernel: missing argument %d", i)
	}
	v, ok := args[i].(int)
	if !ok {
		return 0, fmt.Errorf("kernel: argument %d is %T, want int", i, args[i])
	}
	return v, nil
}

func argString(args []any, i int) (string, error) {
	if i >= len(args) {
		return "", fmt.Errorf("kernel: missing argument %d", i)
	}
	v, ok := args[i].(string)
	if !ok {
		return "", fmt.Errorf("kernel: argument %d is %T, want string", i, args[i])
	}
	return v, nil
}

func argBytes(args []any, i int) ([]byte, error) {
	if i >= len(args) {
		return nil, fmt.Errorf("kernel: missing argument %d", i)
	}
	switch v := args[i].(type) {
	case []byte:
		return v, nil
	case string:
		return []byte(v), nil
	default:
		return nil, fmt.Errorf("kernel: argument %d is %T, want []byte or string", i, args[i])
	}
}
