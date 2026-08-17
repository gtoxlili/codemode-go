package codemode

import (
	"context"
	"sync/atomic"
	"time"
)

// Binding is one tool a program can call as tools.Name(args).
//
// Invoke receives the run-scoped context — canceled when the run settles, times
// out or is aborted — and the arguments the program passed, serialized as a
// JSON object. It returns the tool's result as text. If that text parses as
// JSON the program sees the parsed value; otherwise it sees the raw string.
//
// A returned error rejects the call's promise with a ToolCallError carrying
// .toolName, which the program can catch and keep going. A panic is absorbed
// and turned into the same thing.
type Binding struct {
	Name   string
	Invoke func(ctx context.Context, args string) (string, error)

	// Mutating declares that the tool writes something that outlives the call.
	// It only matters together with ConflictKeys: two calls are serialized when
	// they share a key and at least one of them mutates.
	Mutating bool

	// ConflictKeys reports which resources this call touches, derived from its
	// own arguments — the scheduler does not know any tool's parameter schema,
	// so the tool has to answer. Typical keys: a constant for a shared backend,
	// "deck:"+id for a resource id, an absolute path for a file.
	//
	// nil means the call never conflicts with anything and always runs in the
	// parallel pool. Panics are absorbed and treated as nil.
	ConflictKeys func(args string) []string
}

// Phase is the stage of a sub-call an observer is being told about.
type Phase string

const (
	PhaseStart Phase = "start"
	PhaseDone  Phase = "done"
	PhaseError Phase = "error"
)

// CallEvent is one observation of a sub-call. Seq is the start order within the
// run, counting from 1. Duration and Err are only set on PhaseDone/PhaseError.
type CallEvent struct {
	Seq      int64
	Tool     string
	Args     string
	Phase    Phase
	Duration time.Duration
	Err      error
}

// WithCallEvents wraps every binding so that on receives a start event before
// each sub-call and a done or error event after it. Events carry the raw
// arguments, not a digest.
//
// on is called from the goroutine running the sub-call, so it must be safe for
// concurrent use, and it runs inline: a slow observer slows the program down.
func WithCallEvents(bindings []Binding, on func(ctx context.Context, ev CallEvent)) []Binding {
	if on == nil {
		return bindings
	}
	var seq atomic.Int64
	out := make([]Binding, len(bindings))
	for i, b := range bindings {
		inner := b.Invoke
		name := b.Name
		b.Invoke = func(ctx context.Context, args string) (string, error) {
			n := seq.Add(1)
			on(ctx, CallEvent{Seq: n, Tool: name, Args: args, Phase: PhaseStart})
			started := time.Now()
			res, err := inner(ctx, args)
			ev := CallEvent{Seq: n, Tool: name, Args: args, Phase: PhaseDone, Duration: time.Since(started)}
			if err != nil {
				ev.Phase, ev.Err = PhaseError, err
			}
			on(ctx, ev)
			return res, err
		}
		out[i] = b
	}
	return out
}
