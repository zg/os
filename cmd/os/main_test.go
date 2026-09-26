package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestSchedulerDemo(t *testing.T) {
	var out bytes.Buffer
	SchedulerDemo(&out, 2)
	got := out.String()

	for _, want := range []string{
		"CPU scheduling", "first-come, first-served", "round robin",
		"multi-level feedback queue", "avg wait", "timeline",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q", want)
		}
	}
}

func TestMemoryDemo(t *testing.T) {
	var out bytes.Buffer
	MemoryDemo(&out, 3, 1)
	got := out.String()

	for _, want := range []string{
		"virtual memory", "FIFO", "LRU", "clock", "optimal",
		"Bélády's anomaly", "TLB size",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q", want)
		}
	}
}

func TestFilesystemDemo(t *testing.T) {
	var out bytes.Buffer
	FilesystemDemo(&out)
	got := out.String()

	for _, want := range []string{"filesystem", "home", "notes.txt", "hard link", "inode"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q", want)
		}
	}
	// The hard-linked data must survive removal of one name.
	if !strings.Contains(got, "still readable") {
		t.Errorf("the hard link demonstration did not complete:\n%s", got)
	}
}

func TestSyscallDemo(t *testing.T) {
	var out bytes.Buffer
	SyscallDemo(&out)
	got := out.String()

	for _, want := range []string{"system calls", "getpid", "brk", "mkdir", "open", "write", "exit"} {
		if !strings.Contains(got, want) {
			t.Errorf("trace missing %q", want)
		}
	}
	// The deliberately invalid call must show as an error.
	if !strings.Contains(got, "error:") {
		t.Errorf("the invalid system call was not reported:\n%s", got)
	}
	if !strings.Contains(got, "hello from userspace") {
		t.Errorf("process output missing:\n%s", got)
	}
}
