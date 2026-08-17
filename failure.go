package codemode

import "fmt"

// FailureKind classifies why a run failed. Each kind maps to one thing the
// model can do about it, which is the reason a run reports a kind instead of a
// bare error string: the model is the one reading it.
type FailureKind string

const (
	// FailureException — the program did not compile, threw, or blew the call
	// stack. Fix the syntax or the logic.
	FailureException FailureKind = "exception"
	// FailureTimeout — the wall clock ran out. Narrow the loop, make fewer calls.
	FailureTimeout FailureKind = "timeout"
	// FailureComputeLimit — the compute budget ran out. Only time spent actually
	// running JavaScript counts, so this means the program computed too much,
	// not that a tool was slow.
	FailureComputeLimit FailureKind = "compute-limit"
	// FailureMemoryLimit — heap growth crossed the trip line while the program
	// was running. Stop building unbounded strings and arrays.
	FailureMemoryLimit FailureKind = "memory-limit"
	// FailureResultLimit — accumulated sub-call results crossed their budget.
	// Select or aggregate in code instead of holding everything.
	FailureResultLimit FailureKind = "result-limit"
	// FailureOutputLimit — console output crossed its budget. Print less, return
	// more. The output collected up to that point is kept.
	FailureOutputLimit FailureKind = "output-limit"
	// FailureInvalidReturn — the return value does not survive a JSON round
	// trip (a function, a cycle). Return plain data.
	FailureInvalidReturn FailureKind = "invalid-return"
	// FailureTooManyCalls — the program issued more sub-calls than the limit
	// allows. Nothing for the model to fix; the run is stopped on purpose.
	FailureTooManyCalls FailureKind = "too-many-calls"
	// FailureAborted — the caller canceled. Nothing for the model to fix.
	FailureAborted FailureKind = "aborted"
)

// Failure describes a failed run. Message is written for the model to read and
// act on, so it says what to do differently rather than what went wrong
// internally.
type Failure struct {
	Kind    FailureKind
	Message string
}

func (f *Failure) Error() string {
	return fmt.Sprintf("code run failed (%s): %s", f.Kind, f.Message)
}
