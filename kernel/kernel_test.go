package kernel

import (
	"errors"
	"strings"
	"testing"

	"github.com/zg/os/filesystem"
	"github.com/zg/os/memory"
	"github.com/zg/os/process"
	"github.com/zg/os/scheduler"
)

func newKernel() *Kernel {
	process.ResetPIDs()
	return New(Config{Inodes: 32, Blocks: 128, Frames: 8, TLBSize: 4})
}

func TestGetPID(t *testing.T) {
	k := newKernel()
	p := k.Spawn("test", 5, 1)

	res := k.Syscall(p, SysGetPID)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.Value != int(p.PID) {
		t.Errorf("= %d, want %d", res.Value, p.PID)
	}
}

func TestWriteToStdout(t *testing.T) {
	k := newKernel()
	p := k.Spawn("test", 5, 1)

	res := k.Syscall(p, SysWrite, 1, "hello kernel")
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.Value != len("hello kernel") {
		t.Errorf("wrote %d bytes, want %d", res.Value, len("hello kernel"))
	}
	if len(k.Output) != 1 || k.Output[0] != "hello kernel" {
		t.Errorf("output = %v", k.Output)
	}
}

func TestFileSyscalls(t *testing.T) {
	k := newKernel()
	p := k.Spawn("test", 5, 1)

	open := k.Syscall(p, SysOpen, "/test.txt", filesystem.O_CREATE|filesystem.O_RDWR)
	if open.Err != nil {
		t.Fatal(open.Err)
	}
	fd := open.Value

	if res := k.Syscall(p, SysWrite, fd, "file contents"); res.Err != nil {
		t.Fatal(res.Err)
	}
	if res := k.Syscall(p, SysClose, fd); res.Err != nil {
		t.Fatal(res.Err)
	}

	stat := k.Syscall(p, SysStat, "/test.txt")
	if stat.Err != nil {
		t.Fatal(stat.Err)
	}
	if stat.Value != len("file contents") {
		t.Errorf("size = %d, want %d", stat.Value, len("file contents"))
	}
}

func TestDirectorySyscalls(t *testing.T) {
	k := newKernel()
	p := k.Spawn("test", 5, 1)

	if res := k.Syscall(p, SysMkdir, "/dir"); res.Err != nil {
		t.Fatal(res.Err)
	}
	if res := k.Syscall(p, SysOpen, "/dir/a", filesystem.O_CREATE); res.Err != nil {
		t.Fatal(res.Err)
	}
	list := k.Syscall(p, SysListDir, "/dir")
	if list.Err != nil {
		t.Fatal(list.Err)
	}
	if list.Value != 1 {
		t.Errorf("directory has %d entries, want 1", list.Value)
	}

	if res := k.Syscall(p, SysUnlink, "/dir/a"); res.Err != nil {
		t.Fatal(res.Err)
	}
	if list := k.Syscall(p, SysListDir, "/dir"); list.Value != 0 {
		t.Errorf("directory has %d entries after unlink, want 0", list.Value)
	}
}

// fork must produce a distinct process that inherits a *copy* of the page
// table, not a shared reference.
func TestForkCopiesPageTable(t *testing.T) {
	k := newKernel()
	parent := k.Spawn("parent", 5, 1)
	parent.PageTable[0] = 10
	parent.PageTable[1] = 11

	res := k.Syscall(parent, SysFork)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	childPID := process.PID(res.Value)
	if childPID == parent.PID {
		t.Fatal("fork returned the parent's PID")
	}

	var child *process.Process
	for _, p := range k.Processes() {
		if p.PID == childPID {
			child = p
		}
	}
	if child == nil {
		t.Fatal("the forked child was not registered")
	}
	if len(child.PageTable) != 2 {
		t.Errorf("child page table has %d entries, want 2", len(child.PageTable))
	}

	// Mutating the child must not affect the parent.
	child.PageTable[2] = 99
	if _, ok := parent.PageTable[2]; ok {
		t.Error("the page table is shared between parent and child")
	}
}

func TestExitTerminatesProcess(t *testing.T) {
	k := newKernel()
	p := k.Spawn("test", 5, 1)

	res := k.Syscall(p, SysExit, 0)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if p.State != process.StateTerminated {
		t.Errorf("state = %s, want terminated", p.State)
	}
}

// A terminated process must not be able to call into the kernel again.
func TestTerminatedProcessCannotSyscall(t *testing.T) {
	k := newKernel()
	p := k.Spawn("test", 5, 1)
	k.Syscall(p, SysExit, 0)

	res := k.Syscall(p, SysGetPID)
	if res.Err == nil {
		t.Error("a terminated process should not be able to make system calls")
	}
}

// Arguments arrive untyped, exactly as they would from a register. Every
// one must be validated rather than asserted.
func TestSyscallValidatesArguments(t *testing.T) {
	k := newKernel()
	p := k.Spawn("test", 5, 1)

	tests := []struct {
		name string
		call SyscallNumber
		args []any
	}{
		{"missing argument", SysWrite, nil},
		{"wrong type for fd", SysWrite, []any{"not an int", "data"}},
		{"wrong type for path", SysOpen, []any{42}},
		{"missing path", SysMkdir, nil},
		{"negative read size", SysRead, []any{3, -1}},
		{"negative allocation", SysBrk, []any{-5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if res := k.Syscall(p, tt.call, tt.args...); res.Err == nil {
				t.Error("invalid arguments were accepted")
			}
		})
	}
}

func TestUnknownSyscall(t *testing.T) {
	k := newKernel()
	p := k.Spawn("test", 5, 1)

	res := k.Syscall(p, SyscallNumber(9999))
	if !errors.Is(res.Err, ErrNoSuchCall) {
		t.Errorf("err = %v, want ErrNoSuchCall", res.Err)
	}
	// Even a rejected call is traced.
	if len(k.Trace) == 0 {
		t.Error("an unknown system call was not traced")
	}
}

func TestNilProcessRejected(t *testing.T) {
	k := newKernel()
	if res := k.Syscall(nil, SysGetPID); res.Err == nil {
		t.Error("a call with no process should fail")
	}
}

// Every call is recorded, which is what makes the kernel boundary
// inspectable.
func TestTraceRecordsEveryCall(t *testing.T) {
	k := newKernel()
	p := k.Spawn("test", 5, 1)

	k.Syscall(p, SysGetPID)
	k.Syscall(p, SysWrite, 1, "x")
	k.Syscall(p, SysMkdir, "/d")

	if len(k.Trace) != 3 {
		t.Fatalf("trace has %d entries, want 3", len(k.Trace))
	}
	for _, e := range k.Trace {
		if e.PID != p.PID {
			t.Errorf("trace entry attributed to pid %d, want %d", e.PID, p.PID)
		}
	}
	if got := k.Trace[0].String(); !strings.Contains(got, "getpid") {
		t.Errorf("trace rendering = %q", got)
	}
}

// brk must actually map pages through the MMU, so allocation is visible in
// the memory statistics.
func TestBrkMapsPages(t *testing.T) {
	k := newKernel()
	p := k.Spawn("test", 5, 1)

	res := k.Syscall(p, SysBrk, 4)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.Value != 4 {
		t.Errorf("allocated %d pages, want 4", res.Value)
	}
	if len(p.PageTable) != 4 {
		t.Errorf("page table has %d entries, want 4", len(p.PageTable))
	}
	if k.MMU.Stats.PageFaults == 0 {
		t.Error("allocating pages caused no page faults; they were not really mapped")
	}
}

// Allocating more pages than there are frames must still work — that is the
// point of virtual memory — by evicting.
func TestBrkBeyondPhysicalMemory(t *testing.T) {
	process.ResetPIDs()
	k := New(Config{Frames: 4, TLBSize: 2, Policy: memory.LRU{}})
	p := k.Spawn("greedy", 5, 1)

	res := k.Syscall(p, SysBrk, 20)
	if res.Err != nil {
		t.Fatalf("allocating beyond physical memory should succeed: %v", res.Err)
	}
	if k.MMU.Stats.Evictions == 0 {
		t.Error("expected evictions when allocating beyond physical memory")
	}
	if got := len(k.MMU.ResidentPages()); got > 4 {
		t.Errorf("%d pages resident with only 4 frames", got)
	}
}

func TestKernelRunSchedules(t *testing.T) {
	process.ResetPIDs()
	k := New(Config{Scheduler: scheduler.RoundRobin{Quantum: 2}})
	k.Spawn("a", 4, 1)
	k.Spawn("b", 6, 1)

	r := k.Run()
	if len(r.Processes) != 2 {
		t.Fatalf("scheduled %d processes, want 2", len(r.Processes))
	}
	if r.TotalTime != 10 {
		t.Errorf("total time = %d, want 10", r.TotalTime)
	}
	for _, p := range r.Processes {
		if p.Remaining != 0 {
			t.Errorf("%s did not finish", p.Name)
		}
	}
}

// Terminated processes must not be rescheduled.
func TestRunSkipsTerminatedProcesses(t *testing.T) {
	process.ResetPIDs()
	k := New(Config{Scheduler: scheduler.FCFS{}})
	a := k.Spawn("a", 4, 1)
	k.Spawn("b", 4, 1)
	k.Syscall(a, SysExit, 0)

	r := k.Run()
	if len(r.Processes) != 1 {
		t.Errorf("scheduled %d processes, want 1", len(r.Processes))
	}
}

func TestConfigDefaults(t *testing.T) {
	k := New(Config{})
	if k.FS == nil || k.MMU == nil || k.Scheduler == nil {
		t.Fatal("a zero config left a subsystem unset")
	}
	if k.MMU.Capacity() < 1 {
		t.Error("MMU has no frames")
	}
}

func TestSyscallNumberString(t *testing.T) {
	if got := SysWrite.String(); got != "write" {
		t.Errorf("= %q", got)
	}
	if got := SyscallNumber(999).String(); !strings.Contains(got, "999") {
		t.Errorf("= %q", got)
	}
}

// A realistic sequence exercising several subsystems together.
func TestIntegrationWorkflow(t *testing.T) {
	k := newKernel()
	p := k.Spawn("worker", 10, 1)

	// Allocate memory, create a directory tree, write and read a file.
	if res := k.Syscall(p, SysBrk, 3); res.Err != nil {
		t.Fatal(res.Err)
	}
	if res := k.Syscall(p, SysMkdir, "/data"); res.Err != nil {
		t.Fatal(res.Err)
	}

	open := k.Syscall(p, SysOpen, "/data/out.txt", filesystem.O_CREATE|filesystem.O_RDWR)
	if open.Err != nil {
		t.Fatal(open.Err)
	}
	fd := open.Value

	const payload = "integration test payload"
	if res := k.Syscall(p, SysWrite, fd, payload); res.Err != nil {
		t.Fatal(res.Err)
	}
	k.Syscall(p, SysClose, fd)

	// Reopen and read it back.
	reopen := k.Syscall(p, SysOpen, "/data/out.txt", filesystem.O_RDONLY)
	if reopen.Err != nil {
		t.Fatal(reopen.Err)
	}
	read := k.Syscall(p, SysRead, reopen.Value, len(payload))
	if read.Err != nil {
		t.Fatal(read.Err)
	}
	if read.Value != len(payload) {
		t.Errorf("read %d bytes, want %d", read.Value, len(payload))
	}
	k.Syscall(p, SysClose, reopen.Value)

	k.Syscall(p, SysExit, 0)

	if p.State != process.StateTerminated {
		t.Error("the process did not terminate")
	}
	if k.FS.OpenCount() != 0 {
		t.Errorf("%d descriptors leaked", k.FS.OpenCount())
	}
	if len(k.Trace) < 8 {
		t.Errorf("trace has %d entries, expected the whole workflow", len(k.Trace))
	}
}
