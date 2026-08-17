package codemode

import (
	"fmt"
	"time"
)

// Limits is the resource envelope of one run. A zero field falls back to the
// corresponding [DefaultLimits] value, so you can override one knob and leave
// the rest alone.
type Limits struct {
	// WallClock is the hard floor. On expiry the VM is interrupted, which kills
	// hot loops and cannot be swallowed by JavaScript try/catch; if the run has
	// not settled InterruptGrace later, it is torn down.
	WallClock      time.Duration
	InterruptGrace time.Duration

	// ComputeBudget only accumulates while the program is running JavaScript —
	// time spent waiting on tool calls or sleep does not count, so this is not
	// process CPU time. A fan-out over twenty slow APIs does not burn it; a
	// `while (true) {}` burns it in seconds, long before the wall clock.
	ComputeBudget time.Duration

	// MemoryBudgetBytes trips when process heap growth relative to the run's
	// baseline crosses it while the program is running. goja has no per-VM
	// memory ceiling (upstream discussion #629), so this is a trip line sampled
	// every 50ms, not a hard wall: it is a process-wide number, and concurrent
	// allocation elsewhere counts toward it.
	MemoryBudgetBytes int64

	// ResultBudgetBytes caps the sub-call results a run may accumulate. Unlike
	// the memory trip line this is exact — every resolved result is counted as
	// it arrives. It is what stops a program from looping a thousand large
	// documents into the heap.
	ResultBudgetBytes int64

	// MaxCallDepth bounds the JavaScript call stack. goja rejects deeper
	// recursion with an uncatchable StackOverflowError instead of taking the
	// process stack down with it.
	MaxCallDepth int

	// MaxParallel is the sub-call concurrency pool — the parallelism a
	// Promise.all actually gets. MaxSubCalls is the total per run.
	MaxParallel int
	MaxSubCalls int

	// LogBudgetBytes, LogLineRunes and MaxLogLines bound console output.
	// Crossing any of them fails the run with FailureOutputLimit and keeps
	// what was collected — an explicit failure the model can correct, rather
	// than a silent truncation it never learns about.
	LogBudgetBytes int
	LogLineRunes   int
	MaxLogLines    int
}

// DefaultLimits are sized for an agent that calls slow external APIs: ten
// minutes of wall clock is a realistic ceiling for a fan-out over a few dozen
// of them, while two minutes of compute is far more than any honest merge or
// scoring pass needs.
func DefaultLimits() Limits {
	return Limits{
		WallClock:         10 * time.Minute,
		InterruptGrace:    5 * time.Second,
		ComputeBudget:     2 * time.Minute,
		MemoryBudgetBytes: 256 << 20,
		ResultBudgetBytes: 64 << 20,
		MaxCallDepth:      8192,
		MaxParallel:       8,
		MaxSubCalls:       200,
		LogBudgetBytes:    200_000,
		LogLineRunes:      4_000,
		MaxLogLines:       2_000,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.WallClock <= 0 {
		l.WallClock = d.WallClock
	}
	if l.InterruptGrace <= 0 {
		l.InterruptGrace = d.InterruptGrace
	}
	if l.ComputeBudget <= 0 {
		l.ComputeBudget = d.ComputeBudget
	}
	if l.MemoryBudgetBytes <= 0 {
		l.MemoryBudgetBytes = d.MemoryBudgetBytes
	}
	if l.ResultBudgetBytes <= 0 {
		l.ResultBudgetBytes = d.ResultBudgetBytes
	}
	if l.MaxCallDepth <= 0 {
		l.MaxCallDepth = d.MaxCallDepth
	}
	if l.MaxParallel <= 0 {
		l.MaxParallel = d.MaxParallel
	}
	if l.MaxSubCalls <= 0 {
		l.MaxSubCalls = d.MaxSubCalls
	}
	if l.LogBudgetBytes <= 0 {
		l.LogBudgetBytes = d.LogBudgetBytes
	}
	if l.LogLineRunes <= 0 {
		l.LogLineRunes = d.LogLineRunes
	}
	if l.MaxLogLines <= 0 {
		l.MaxLogLines = d.MaxLogLines
	}
	return l
}

// Validate reports the first non-positive field. [Run] treats those as "use the
// default", which is right for a partially filled struct and wrong for values
// that came out of a config file, where a wall_clock of "0s" silently becomes
// ten minutes.
func (l Limits) Validate() error {
	for _, f := range []struct {
		name  string
		value int64
	}{
		{"WallClock", int64(l.WallClock)},
		{"InterruptGrace", int64(l.InterruptGrace)},
		{"ComputeBudget", int64(l.ComputeBudget)},
		{"MemoryBudgetBytes", l.MemoryBudgetBytes},
		{"ResultBudgetBytes", l.ResultBudgetBytes},
		{"MaxCallDepth", int64(l.MaxCallDepth)},
		{"MaxParallel", int64(l.MaxParallel)},
		{"MaxSubCalls", int64(l.MaxSubCalls)},
		{"LogBudgetBytes", int64(l.LogBudgetBytes)},
		{"LogLineRunes", int64(l.LogLineRunes)},
		{"MaxLogLines", int64(l.MaxLogLines)},
	} {
		if f.value <= 0 {
			return fmt.Errorf("codemode: limit %s must be positive, got %d", f.name, f.value)
		}
	}
	return nil
}
