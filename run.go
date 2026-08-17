package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/eventloop"
)

// Result is a successful run. HasResult is false when the program returned
// undefined — it only brought logs back.
//
// Result holds the program's return value as the exact JSON the program
// serialized (a json.RawMessage inside the any): object key order is the
// program's own insertion order and survives the host's re-encoding verbatim.
// The alternative — exporting the JS object into a Go map — throws that order
// away and re-renders differently on every marshal, which is poison for
// anything that compares or caches the rendered result (prompt caches, tests,
// dedup). The field stays `any` so reflection-based schema hints still see an
// open value rather than an opaque payload.
type Result struct {
	Logs      []string `json:"logs"`
	Result    any      `json:"result,omitempty"`
	HasResult bool     `json:"-"`
}

var errTooManySubCalls = errors.New("sub-call limit exceeded; the program issued too many tool calls in one run")

// Interrupt sentinels. goja's Interrupt surfaces on the JavaScript side as an
// exception that try/catch cannot swallow but that does reach the rejection
// path, so the wrapper recognizes these by prefix and reports the right kind
// instead of a generic exception.
const (
	interruptTimeoutMark = "__codemode_wall_clock__"
	interruptComputeMark = "__codemode_compute_budget__"
	interruptMemoryMark  = "__codemode_memory_budget__"
	interruptHaltMark    = "__codemode_halt__"
)

func markKind(mark string) (FailureKind, string, bool) {
	switch {
	case strings.HasPrefix(mark, interruptTimeoutMark):
		return FailureTimeout, "wall clock budget exceeded", true
	case strings.HasPrefix(mark, interruptComputeMark):
		return FailureComputeLimit, "compute budget exceeded (time spent actually running JS, waiting for tool calls does not count)", true
	case strings.HasPrefix(mark, interruptMemoryMark):
		return FailureMemoryLimit, "memory budget exceeded — avoid building unbounded strings or arrays; stream through results instead", true
	}
	return "", "", false
}

type engineResult struct {
	res    Result
	failed *Failure
}

// Run executes code as the body of an async function — top-level await and
// return both work — with bindings reachable as tools.Name(args).
//
// Every run gets a fresh VM and no state survives it. Canceling ctx settles the
// run as FailureAborted and aborts sub-calls that are still in flight.
//
// opts supply the host's JSON codec and warning logger; both default, so an
// agent with no opinion about either passes nothing. See [WithCodec] and
// [WithLogger].
//
// Two bindings sharing a name is a caller error this level cannot report: the
// first wins and the rest are dropped. [NewTool] rejects that case instead.
//
// Three invariants keep this from deadlocking, and all three are load-bearing:
//
//   - vm.Interrupt is called directly from the watchdog, never queued onto the
//     event loop. A queued interrupt never runs, because the loop is exactly
//     what is stuck in the hot loop you are trying to kill.
//   - loop.Terminate runs in the background. It waits for the loop to go idle,
//     so a binding that ignores context cancellation and spins would otherwise
//     hang the caller's request instead of one cleanup goroutine.
//   - Every path that ends the run goes through one sync.Once. First one wins,
//     the rest are no-ops.
func Run(ctx context.Context, code string, bindings []Binding, limits Limits, opts ...RunOption) (Result, *Failure) {
	limits = limits.withDefaults()
	dep := resolveDeps(opts)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	loop := eventloop.NewEventLoop(eventloop.EnableConsole(false))

	byName := make(map[string]Binding, len(bindings))
	names := make([]string, 0, len(bindings))
	for _, b := range bindings {
		if _, dup := byName[b.Name]; dup {
			continue
		}
		byName[b.Name] = b
		names = append(names, b.Name)
	}

	logs := newLogCollector(limits)
	sched := newScheduler(runCtx, limits, dep)

	settle := make(chan engineResult, 1)
	done := make(chan struct{})
	var finishOnce sync.Once
	finish := func(r engineResult) {
		finishOnce.Do(func() {
			settle <- r
			close(done)
		})
	}
	fail := func(kind FailureKind, format string, a ...any) {
		finish(engineResult{
			res:    Result{Logs: logs.snapshot()},
			failed: &Failure{Kind: kind, Message: fmt.Sprintf(format, a...)},
		})
	}

	// vmRef publishes the loop's Runtime to the watchdogs. Interrupt is the one
	// Runtime method that is safe to call from another goroutine.
	var vmRef atomic.Pointer[goja.Runtime]

	// resultBytes enforces ResultBudgetBytes; pendingWaits counts outstanding
	// sleeps so the sampler can tell running from waiting.
	var resultBytes atomic.Int64
	var pendingWaits atomic.Int64

	var stopOnce sync.Once
	hardStop := func() {
		stopOnce.Do(func() {
			cancel()
			if vm := vmRef.Load(); vm != nil {
				vm.Interrupt(interruptHaltMark)
			}
			go loop.Terminate()
		})
	}

	// Wall clock and external cancellation.
	go func() {
		timer := time.NewTimer(limits.WallClock)
		defer timer.Stop()
		select {
		case <-done:
			return
		case <-runCtx.Done():
			fail(FailureAborted, "run canceled before completion: %v", context.Cause(runCtx))
			hardStop()
		case <-timer.C:
			// Interrupt first. It only bites JavaScript that is executing — a
			// hot loop dies here — and is a no-op if the program is waiting on a
			// tool, which is what the grace period is for.
			if vm := vmRef.Load(); vm != nil {
				vm.Interrupt(interruptTimeoutMark)
			}
			grace := time.NewTimer(limits.InterruptGrace)
			defer grace.Stop()
			select {
			case <-done:
				return
			case <-grace.C:
				fail(FailureTimeout, "wall clock budget of %s exceeded", limits.WallClock)
				hardStop()
			}
		}
	}()

	// Compute budget and memory trip line, sampled. "Active" means no sub-call
	// and no sleep is outstanding, i.e. the VM is running or runnable. Only
	// active ticks charge compute, and only active ticks check memory — a
	// program parked on a slow API neither burns its budget nor gets blamed for
	// another session's allocations.
	//
	// The precision trade is deliberate: goja exposes no instruction counter and
	// no per-VM heap accounting, so this samples instead. Compute over-counts
	// brief idle inside an active window, which is harmless. Memory is a
	// process-wide number, so allocation elsewhere during an active window still
	// counts — leave headroom.
	go func() {
		baseHeap := sampleHeapObjects()
		var busy time.Duration
		var lastConfirmGC time.Time
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				if sched.inflight() > 0 || pendingWaits.Load() > 0 {
					continue
				}
				busy += 50 * time.Millisecond
				if busy >= limits.ComputeBudget {
					if vm := vmRef.Load(); vm != nil {
						vm.Interrupt(interruptComputeMark)
					}
					fail(FailureComputeLimit, "compute budget of %s exceeded", limits.ComputeBudget)
					hardStop()
					return
				}
				if heap := sampleHeapObjects(); int64(heap) > int64(baseHeap)+limits.MemoryBudgetBytes {
					// The heap metric counts allocated-but-not-yet-swept objects,
					// so a program that only makes short-lived garbage can cross
					// the line between two collections. Force one GC and look
					// again before killing it. Rate-limited to 1/s: a real bomb's
					// objects are live and survive the first re-check anyway.
					if time.Since(lastConfirmGC) < time.Second {
						continue
					}
					lastConfirmGC = time.Now()
					runtime.GC()
					if heap = sampleHeapObjects(); int64(heap) <= int64(baseHeap)+limits.MemoryBudgetBytes {
						continue
					}
					if vm := vmRef.Load(); vm != nil {
						vm.Interrupt(interruptMemoryMark)
					}
					fail(FailureMemoryLimit, "memory budget of %d bytes exceeded while the program was running", limits.MemoryBudgetBytes)
					hardStop()
					return
				}
			}
		}
	}()

	// mkToolCallErr is a JS-side factory: ToolCallError is a class, and a class
	// constructor has to be called with new.
	var mkToolCallErr goja.Value

	// runOnLoop wraps everything posted to the loop thread with one recovery
	// point. goja reports host misuse and internal defects as panics, and
	// eventloop's goroutine has no recover of its own — without this, an
	// adversarial program that trips an engine bug takes the whole process down.
	// All JavaScript execution, microtasks included, unwinds from these frames.
	runOnLoop := func(fn func(vm *goja.Runtime)) bool {
		return loop.RunOnLoop(func(vm *goja.Runtime) {
			defer func() {
				if r := recover(); r != nil {
					fail(FailureException, "engine internal error: %v", r)
					hardStop()
				}
			}()
			fn(vm)
		})
	}

	loop.Start()
	ok := runOnLoop(func(vm *goja.Runtime) {
		vmRef.Store(vm)
		if runCtx.Err() != nil {
			// Canceled before we even started; the watchdog already settled it.
			vm.Interrupt(interruptHaltMark)
			return
		}
		vm.SetMaxCallStackSize(limits.MaxCallDepth)

		// A sub-call the program never awaited and never caught — a
		// fire-and-forget write that failed is the usual case — must not
		// evaporate. Rejections still unhandled when the run settles are
		// appended to the logs, so the model can see which call failed behind
		// its back. Both structures are only touched on the loop thread.
		// The slice keeps first-rejection order: flushing by map iteration
		// would shuffle the log lines on every run, and the logs are
		// model-visible output that must render deterministically.
		unhandled := make(map[*goja.Promise]struct{})
		var unhandledOrder []*goja.Promise
		vm.SetPromiseRejectionTracker(func(p *goja.Promise, op goja.PromiseRejectionOperation) {
			if op == goja.PromiseRejectionReject {
				if _, seen := unhandled[p]; !seen {
					unhandled[p] = struct{}{}
					unhandledOrder = append(unhandledOrder, p)
				}
			} else {
				delete(unhandled, p)
			}
		})
		flushUnhandled := func() {
			for _, p := range unhandledOrder {
				if _, still := unhandled[p]; still {
					logs.append("[unhandled rejection] " + p.Result().String())
				}
			}
			clear(unhandled)
			unhandledOrder = unhandledOrder[:0]
		}

		// sleep is the only waiting primitive, implemented in Go so the sampler
		// can see it. Backing off between calls and pacing a poll are legitimate
		// things for a program to do and must not burn the compute budget. The
		// setTimeout family is deleted below: it would wait without being
		// counted, and it is not part of the contract.
		vm.Set("sleep", func(call goja.FunctionCall) goja.Value {
			ms := call.Argument(0).ToInteger()
			if ms < 0 {
				ms = 0
			}
			promise, resolve, _ := vm.NewPromise()
			pendingWaits.Add(1)
			time.AfterFunc(time.Duration(ms)*time.Millisecond, func() {
				pendingWaits.Add(-1)
				runOnLoop(func(*goja.Runtime) { _ = resolve(goja.Undefined()) })
			})
			return vm.ToValue(promise)
		})

		vm.Set("__toolNames", names)
		vm.Set("__log", func(call goja.FunctionCall) goja.Value {
			if logs.append(call.Argument(0).String()) {
				fail(FailureOutputLimit, "console output exceeded its budget; the output collected before this point is attached after this message")
				vm.Interrupt("console output budget exceeded")
			}
			return goja.Undefined()
		})
		vm.Set("__call", func(call goja.FunctionCall) goja.Value {
			name := call.Argument(0).String()
			argsJSON := call.Argument(1).String()
			b, known := byName[name]
			promise, resolve, reject := vm.NewPromise()
			if !known {
				_ = reject(toolCallError(vm, mkToolCallErr, name, fmt.Errorf("unknown tool %q", name)))
				return vm.ToValue(promise)
			}
			future, limitErr := sched.Submit(b, argsJSON)
			if limitErr != nil {
				// Over the limit: kill the run — an unintended loop must not keep
				// burning someone's API quota — and reject this call too, so the
				// program's own semantics stay consistent.
				fail(FailureTooManyCalls, "the program exceeded the limit of %d tool calls in one run", limits.MaxSubCalls)
				_ = reject(toolCallError(vm, mkToolCallErr, name, limitErr))
				vm.Interrupt("sub-call limit exceeded")
				return vm.ToValue(promise)
			}
			go func() {
				r := <-future
				if r.err != nil {
					// Failures are not charged against the result budget.
					runOnLoop(func(vm *goja.Runtime) {
						_ = reject(toolCallError(vm, mkToolCallErr, name, r.err))
					})
					return
				}
				if resultBytes.Add(int64(len(r.out))) > limits.ResultBudgetBytes {
					fail(FailureResultLimit, "accumulated tool results exceeded the budget of %d bytes — select or aggregate results in code instead of holding all of them", limits.ResultBudgetBytes)
					if vm := vmRef.Load(); vm != nil {
						vm.Interrupt(interruptHaltMark)
					}
					return
				}
				runOnLoop(func(vm *goja.Runtime) {
					// Resolve with the raw result string; the JS-side tools
					// wrapper JSON.parses it (see prelude). Parsing on the JS
					// side keeps object key order equal to the document order —
					// a Go-map round trip would randomize Object.keys() /
					// JSON.stringify() inside the program on every run. A
					// result that is not JSON stays a plain string, same as
					// before: a tool's contract is text, not necessarily JSON.
					_ = resolve(vm.ToValue(r.out))
				})
			}()
			return vm.ToValue(promise)
		})
		vm.Set("__fulfill", func(call goja.FunctionCall) goja.Value {
			flushUnhandled()
			// The wrapper hands over the JSON string the program's value
			// serialized to. Keeping it as raw JSON — instead of Export()ing
			// the object into a Go map — preserves the program's own key
			// order through every later re-encoding (see Result doc).
			//
			// Validate before trusting: __bridge is lexically reachable from
			// the program (script-level const shares the global lexical
			// environment; it is hidden from enumeration, not from naming),
			// so a program can call __bridge.fulfill({a: 1}) directly and
			// goja's .String() would smuggle "[object Object]" into the
			// RawMessage — the host's encoder then fails far from the cause.
			raw, isString := call.Argument(0).Export().(string)
			if !isString || !json.Valid([]byte(raw)) {
				finish(engineResult{res: Result{Logs: logs.snapshot()}, failed: &Failure{
					Kind:    FailureInvalidReturn,
					Message: "the completion value is not a JSON string; return plain JSON-serializable data from the program",
				}})
				return goja.Undefined()
			}
			finish(engineResult{res: Result{Logs: logs.snapshot(), Result: json.RawMessage(raw), HasResult: true}})
			return goja.Undefined()
		})
		vm.Set("__fulfillEmpty", func(goja.FunctionCall) goja.Value {
			flushUnhandled()
			finish(engineResult{res: Result{Logs: logs.snapshot()}})
			return goja.Undefined()
		})
		vm.Set("__invalidReturn", func(call goja.FunctionCall) goja.Value {
			fail(FailureInvalidReturn, "the return value is not lossless JSON (%s); return plain JSON-serializable data — strings, numbers, booleans, arrays, objects", call.Argument(0).String())
			return goja.Undefined()
		})
		vm.Set("__programError", func(call goja.FunctionCall) goja.Value {
			flushUnhandled()
			msg := call.Argument(0).String()
			kind, friendly, marked := markKind(msg)
			if marked {
				msg = friendly
			} else {
				kind = FailureException
			}
			finish(engineResult{res: Result{Logs: logs.snapshot()}, failed: &Failure{Kind: kind, Message: msg}})
			return goja.Undefined()
		})

		if _, err := vm.RunString(prelude); err != nil {
			finish(engineResult{failed: &Failure{Kind: FailureException, Message: fmt.Sprintf("engine prelude failed: %v", err)}})
			return
		}
		mkToolCallErr = vm.Get("__mkToolCallError")

		// The wrapper adds exactly one line, so compile errors report line
		// numbers one higher than the source. The rejection handler recognizes
		// the budget sentinels first: an Interrupt cannot be caught by the
		// program but does arrive here as a rejection, and it is a timeout, not
		// an exception. The fulfilled value crosses the bridge as the JSON
		// string it serializes to — the string both validates losslessness and
		// IS the result (see __fulfill); stringify returning undefined means a
		// bare function/symbol, which is not lossless JSON either.
		wrapped := "(async () => {\n" + code + "\n})().then(\n" +
			"  (v) => { if (v === undefined) { __bridge.fulfillEmpty(); return; } " +
			"let s; try { s = JSON.stringify(v); } catch (e) { __bridge.invalidReturn(String(e)); return; } " +
			"if (s === undefined) { __bridge.invalidReturn('the value serializes to undefined'); return; } " +
			"__bridge.fulfill(s); },\n" +
			"  (e) => __bridge.programError(e == null ? 'null or undefined thrown' : " +
			"String((e && e.stack) || e)),\n" +
			");"
		if _, err := vm.RunString(wrapped); err != nil {
			msg := err.Error()
			kind, friendly, marked := markKind(msg)
			switch {
			case marked:
				msg = friendly
			case isStackOverflow(err):
				// goja's StackOverflowError is uncatchable and arrives as a Go
				// error. Give the model a sentence it can act on, not a stack.
				kind, msg = FailureException, "maximum call depth exceeded — infinite recursion in the program?"
			default:
				kind = FailureException
			}
			finish(engineResult{res: Result{Logs: logs.snapshot()}, failed: &Failure{Kind: kind, Message: msg}})
		}
	})
	if !ok {
		finish(engineResult{failed: &Failure{Kind: FailureAborted, Message: "run canceled before the program started"}})
	}

	// Return as soon as the program settles. Teardown happens in hardStop's
	// background goroutine, so nothing on the way out can hang the caller.
	r := <-settle
	hardStop()
	return r.res, r.failed
}

// prelude builds the program's entire visible surface and then deletes the
// bridge and the timer globals. __bridge is a lexical const: it does not live
// on globalThis and does not show up in Reflect.ownKeys, but scripts share the
// global lexical environment, so a program that knows the name CAN still
// reference it — hiding is against enumeration, not naming. Anything reachable
// through it therefore validates its inputs (see __fulfill).
const prelude = `const __bridge = {
  call: __call, log: __log, fulfill: __fulfill, fulfillEmpty: __fulfillEmpty,
  invalidReturn: __invalidReturn, programError: __programError, names: __toolNames,
};
for (const k of ['__call','__log','__fulfill','__fulfillEmpty','__invalidReturn',
                 '__programError','__toolNames','require',
                 'setTimeout','setInterval','setImmediate',
                 'clearTimeout','clearInterval','clearImmediate']) {
  delete globalThis[k];
}
class ToolCallError extends Error {
  constructor(toolName, message) { super(message); this.name = 'ToolCallError'; this.toolName = toolName; }
}
const __fmt = (v) => {
  if (typeof v === 'string') return v;
  try { const s = JSON.stringify(v); return s === undefined ? String(v) : s; } catch (e) { return String(v); }
};
const console = {
  log:   (...a) => __bridge.log(a.map(__fmt).join(' ')),
  info:  (...a) => __bridge.log(a.map(__fmt).join(' ')),
  warn:  (...a) => __bridge.log(a.map(__fmt).join(' ')),
  error: (...a) => __bridge.log(a.map(__fmt).join(' ')),
  debug: (...a) => __bridge.log(a.map(__fmt).join(' ')),
};
const tools = {};
for (const n of __bridge.names) {
  tools[n] = (args) => {
    const a = args ?? {};
    if (typeof a !== 'object' || Array.isArray(a)) {
      return Promise.reject(new TypeError(
        'tools.' + n + '(args): args must be one object matching the tool\'s parameter schema, got ' +
        (Array.isArray(a) ? 'an array' : typeof a)));
    }
    return __bridge.call(n, JSON.stringify(a)).then((s) => {
      try { return JSON.parse(s); } catch (e) { return s; }
    });
  };
}
const __mkToolCallError = (t, m) => new ToolCallError(t, m);`

func isStackOverflow(err error) bool {
	var soe *goja.StackOverflowError
	return errors.As(err, &soe)
}

// heapObjectsDesc is looked up once. runtime/metrics is used instead of
// ReadMemStats because it does not stop the world — with one sampler per
// concurrent run, a stop-the-world read would charge every request in the
// process for the pause.
var heapObjectsDesc = func() *metrics.Description {
	for _, d := range metrics.All() {
		if d.Name == "/memory/classes/heap/objects:bytes" {
			return &d
		}
	}
	return nil
}()

func sampleHeapObjects() uint64 {
	if heapObjectsDesc == nil {
		return 0 // unreachable in practice; 0 means the trip line never fires
	}
	sample := []metrics.Sample{{Name: heapObjectsDesc.Name}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

// toolCallError builds a ToolCallError through the JS-side factory so the
// program can instanceof it and read .toolName. If the factory is somehow
// unavailable the program still gets the message, as a plain string.
func toolCallError(vm *goja.Runtime, factory goja.Value, name string, err error) goja.Value {
	msg := err.Error()
	fn, ok := goja.AssertFunction(factory)
	if !ok {
		return vm.ToValue(msg)
	}
	v, ctorErr := fn(goja.Undefined(), vm.ToValue(name), vm.ToValue(msg))
	if ctorErr != nil {
		return vm.ToValue(msg)
	}
	return v
}
