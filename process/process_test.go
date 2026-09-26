package process

import "testing"

func TestPIDsAreUnique(t *testing.T) {
	ResetPIDs()
	seen := map[PID]bool{}
	for range 1000 {
		p := New("test", 1, 1)
		if seen[p.PID] {
			t.Fatalf("duplicate PID %d", p.PID)
		}
		seen[p.PID] = true
	}
}

func TestResetPIDs(t *testing.T) {
	ResetPIDs()
	first := New("a", 1, 1).PID
	ResetPIDs()
	if again := New("b", 1, 1).PID; again != first {
		t.Errorf("after reset, PID = %d, want %d", again, first)
	}
}

func TestNewProcessDefaults(t *testing.T) {
	ResetPIDs()
	p := New("worker", 10, 3)

	if p.State != StateNew {
		t.Errorf("state = %s, want new", p.State)
	}
	if p.Remaining != p.Burst {
		t.Errorf("remaining = %d, want burst %d", p.Remaining, p.Burst)
	}
	if p.PageTable == nil || p.Registers == nil {
		t.Error("page table and registers should be initialised")
	}
	if p.HasStarted() {
		t.Error("a new process has not started")
	}
}

// Response time measures the first dispatch only; later ones must not
// overwrite it.
func TestMarkStartedOnlyOnce(t *testing.T) {
	ResetPIDs()
	p := New("test", 5, 1)
	p.ArrivalTime = 2

	p.MarkStarted(5)
	if p.ResponseTime != 3 {
		t.Errorf("response time = %d, want 3", p.ResponseTime)
	}
	if !p.HasStarted() {
		t.Error("HasStarted should be true after the first dispatch")
	}

	// A later dispatch must not change it.
	p.MarkStarted(20)
	if p.ResponseTime != 3 {
		t.Errorf("response time = %d after a second dispatch, want 3", p.ResponseTime)
	}
	if p.StartTime != 5 {
		t.Errorf("start time = %d, want 5", p.StartTime)
	}
}

func TestTurnaroundTime(t *testing.T) {
	ResetPIDs()
	p := New("test", 5, 1)
	p.ArrivalTime = 2
	p.CompletionTime = 12

	// Only meaningful once terminated.
	if got := p.TurnaroundTime(); got != 0 {
		t.Errorf("turnaround on a running process = %d, want 0", got)
	}
	p.State = StateTerminated
	if got := p.TurnaroundTime(); got != 10 {
		t.Errorf("turnaround = %d, want 10", got)
	}
}

// A clone must be independent, or re-running a scheduler on the same
// workload would carry the previous run's state.
func TestCloneIsIndependent(t *testing.T) {
	ResetPIDs()
	p := New("original", 5, 1)
	p.PageTable[0] = 10
	p.Registers["rax"] = 42

	c := p.Clone()
	c.PageTable[1] = 20
	c.Registers["rbx"] = 99
	c.Remaining = 0

	if _, ok := p.PageTable[1]; ok {
		t.Error("the page table is shared with the clone")
	}
	if _, ok := p.Registers["rbx"]; ok {
		t.Error("the registers are shared with the clone")
	}
	if p.Remaining != 5 {
		t.Errorf("the original's remaining changed to %d", p.Remaining)
	}
	// The copied values must carry over.
	if c.PageTable[0] != 10 || c.Registers["rax"] != 42 {
		t.Error("the clone did not inherit the original's state")
	}
}

func TestStateStrings(t *testing.T) {
	want := map[State]string{
		StateNew: "new", StateReady: "ready", StateRunning: "running",
		StateBlocked: "blocked", StateTerminated: "terminated",
	}
	for s, w := range want {
		if got := s.String(); got != w {
			t.Errorf("%d.String() = %q, want %q", s, got, w)
		}
	}
	if got := State(99).String(); got != "unknown" {
		t.Errorf("= %q, want unknown", got)
	}
}

func TestProcessString(t *testing.T) {
	ResetPIDs()
	p := New("worker", 10, 3)
	got := p.String()
	for _, want := range []string{"worker", "pid=", "state=new", "10"} {
		if !contains(got, want) {
			t.Errorf("%q missing %q", got, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
