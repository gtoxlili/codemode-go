package codemode

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fastLimits keeps the budgets small enough that the timeout paths are actually
// exercised, and large enough that the happy paths finish.
func fastLimits() Limits {
	return Limits{
		WallClock:      10 * time.Second,
		InterruptGrace: 500 * time.Millisecond,
		MaxCallDepth:   1024,
		MaxParallel:    8,
		MaxSubCalls:    50,
		LogBudgetBytes: 100_000,
		LogLineRunes:   4_000,
		MaxLogLines:    1_000,
	}
}

func echoBinding(name, payload string) Binding {
	return Binding{
		Name: name,
		Invoke: func(ctx context.Context, args string) (string, error) {
			return fmt.Sprintf(`{"tool":%q,"echo":%s,"args":%s}`, name, strconv.Quote(payload), args), nil
		},
	}
}

func sleepBinding(name string, d time.Duration, marker string) Binding {
	return Binding{
		Name: name,
		Invoke: func(ctx context.Context, args string) (string, error) {
			select {
			case <-time.After(d):
				return fmt.Sprintf(`{"tool":%q,"marker":%q}`, name, marker), nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}
}

func mustRun(t *testing.T, code string, bindings []Binding, limits Limits) Result {
	t.Helper()
	res, failure := Run(context.Background(), code, bindings, limits)
	if failure != nil {
		t.Fatalf("unexpected failure: %v", failure)
	}
	return res
}

func mustFail(t *testing.T, code string, bindings []Binding, limits Limits) (Result, *Failure) {
	t.Helper()
	res, failure := Run(context.Background(), code, bindings, limits)
	if failure == nil {
		t.Fatalf("expected a failure, got result %+v", res)
	}
	return res, failure
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestRunBasicAwaitAndReturn(t *testing.T) {
	res := mustRun(t, `
const r = await tools.echo({x: 1});
return {got: r.echo, name: r.tool};
`, []Binding{echoBinding("echo", "42")}, fastLimits())
	if !res.HasResult {
		t.Fatal("expected a result")
	}
	m := res.Result.(map[string]any)
	if m["got"] != "42" || m["name"] != "echo" {
		t.Fatalf("got %+v", m)
	}
}

func TestRunPromiseAllIsParallel(t *testing.T) {
	start := time.Now()
	res := mustRun(t, `
const [a, b] = await Promise.all([tools.slow_a({}), tools.slow_b({})]);
return {a: a.marker, b: b.marker};
`, []Binding{
		sleepBinding("slow_a", 200*time.Millisecond, "A"),
		sleepBinding("slow_b", 200*time.Millisecond, "B"),
	}, fastLimits())

	// Two 200ms calls overlapping land well under 350ms; serialized they exceed 400ms.
	if elapsed := time.Since(start); elapsed >= 350*time.Millisecond || elapsed < 190*time.Millisecond {
		t.Fatalf("Promise.all should overlap the calls, took %s", elapsed)
	}
	m := res.Result.(map[string]any)
	if m["a"] != "A" || m["b"] != "B" {
		t.Fatalf("got %+v", m)
	}
}

func TestRunToolCallErrorCarriesToolName(t *testing.T) {
	bindings := []Binding{
		{Name: "boom", Invoke: func(context.Context, string) (string, error) {
			return "", fmt.Errorf("rate limited (429)")
		}},
		echoBinding("ok", "fine"),
	}
	res := mustRun(t, `
let failed = null;
try {
  await tools.boom({});
} catch (e) {
  if (!(e instanceof ToolCallError)) throw new Error("not a ToolCallError: " + (e && e.constructor && e.constructor.name));
  failed = {tool: e.toolName, msg: e.message};
}
const fine = await tools.ok({});
return {failed, fine: fine.echo};
`, bindings, fastLimits())

	m := res.Result.(map[string]any)
	failed := m["failed"].(map[string]any)
	if failed["tool"] != "boom" {
		t.Fatalf("toolName = %v", failed["tool"])
	}
	if !strings.Contains(failed["msg"].(string), "429") {
		t.Fatalf("message lost the tool's error: %v", failed["msg"])
	}
	if m["fine"] != "fine" {
		t.Fatalf("the run should continue after a caught failure, got %+v", m)
	}
}

func TestRunConsoleLogsAndUndefinedReturn(t *testing.T) {
	res := mustRun(t, `
console.log("step 1", {n: 1});
console.error("warn line");
console.log([1,2,3]);
`, nil, fastLimits())

	if res.HasResult {
		t.Fatal("a program that returns nothing must report HasResult false")
	}
	want := []string{`step 1 {"n":1}`, "warn line", "[1,2,3]"}
	if asJSON(t, res.Logs) != asJSON(t, want) {
		t.Fatalf("logs = %v, want %v", res.Logs, want)
	}
}

func TestRunSyntaxErrorIsException(t *testing.T) {
	_, failure := mustFail(t, `const x = {`, nil, fastLimits())
	if failure.Kind != FailureException || !strings.Contains(failure.Message, "SyntaxError") {
		t.Fatalf("got %+v", failure)
	}
}

func TestRunThrownErrorIsException(t *testing.T) {
	_, failure := mustFail(t, `throw new Error("kaboom");`, nil, fastLimits())
	if failure.Kind != FailureException || !strings.Contains(failure.Message, "kaboom") {
		t.Fatalf("got %+v", failure)
	}
}

func TestRunInvalidReturn(t *testing.T) {
	// A function: JSON.stringify drops it to undefined, JSON.parse then throws.
	if _, f := mustFail(t, `return () => 1;`, nil, fastLimits()); f.Kind != FailureInvalidReturn {
		t.Fatalf("got %+v", f)
	}
	// A cycle: JSON.stringify throws outright.
	if _, f := mustFail(t, "const a = {}; a.self = a; return a;", nil, fastLimits()); f.Kind != FailureInvalidReturn {
		t.Fatalf("got %+v", f)
	}
}

func TestRunInfiniteLoopTimesOut(t *testing.T) {
	limits := fastLimits()
	limits.WallClock = 300 * time.Millisecond
	limits.InterruptGrace = 300 * time.Millisecond
	res, failure := mustFail(t, `
console.log("before loop");
let i = 0;
while (true) { i++; }
`, nil, limits)

	if failure.Kind != FailureTimeout {
		t.Fatalf("got %+v", failure)
	}
	// Output collected before the failure survives it.
	if asJSON(t, res.Logs) != asJSON(t, []string{"before loop"}) {
		t.Fatalf("logs = %v", res.Logs)
	}
}

func TestRunTryCatchCannotSwallowTheInterrupt(t *testing.T) {
	limits := fastLimits()
	limits.WallClock = 250 * time.Millisecond
	limits.InterruptGrace = 250 * time.Millisecond
	limits.CPUBudget = time.Hour // keep the compute budget out of this test
	_, failure := mustFail(t, `
try {
  while (true) {}
} catch (e) {
  while (true) {}
}
`, nil, limits)
	if failure.Kind != FailureTimeout {
		t.Fatalf("got %+v", failure)
	}
}

func TestRunComputeBudgetKillsHotLoopEarly(t *testing.T) {
	limits := fastLimits()
	limits.WallClock = 30 * time.Second
	limits.CPUBudget = 300 * time.Millisecond
	start := time.Now()
	res, failure := mustFail(t, `
console.log("before loop");
let i = 0;
while (true) { i++; }
`, nil, limits)

	if failure.Kind != FailureComputeLimit {
		t.Fatalf("got %+v", failure)
	}
	if time.Since(start) >= 5*time.Second {
		t.Fatal("the compute budget should trip long before the wall clock")
	}
	if asJSON(t, res.Logs) != asJSON(t, []string{"before loop"}) {
		t.Fatalf("logs = %v", res.Logs)
	}
}

func TestRunComputeBudgetIgnoresToolWaitTime(t *testing.T) {
	limits := fastLimits()
	limits.CPUBudget = 500 * time.Millisecond
	res := mustRun(t, `
const r = await tools.slow({});
return r.marker;
`, []Binding{sleepBinding("slow", 3*time.Second, "done")}, limits)
	if res.Result != "done" {
		t.Fatalf("a 3s tool call must not burn a 500ms compute budget, got %v", res.Result)
	}
}

func TestRunComputeBudgetIgnoresSleepWaitTime(t *testing.T) {
	limits := fastLimits()
	limits.CPUBudget = 400 * time.Millisecond
	res := mustRun(t, `
console.log("napping");
await sleep(1200);
return "woke";
`, nil, limits)
	if res.Result != "woke" {
		t.Fatalf("backing off must not burn the compute budget, got %v", res.Result)
	}
}

// A program parked on sleep must not be blamed for allocations made elsewhere
// in the process — memory is only attributed while the program is running.
func TestRunMemoryBudgetSparesWaitingPrograms(t *testing.T) {
	go func() {
		time.Sleep(150 * time.Millisecond) // let the run start and take its baseline
		noise := make([]byte, 8<<20)       // 8MB, over the 4MB budget below
		_ = noise
		runtime.GC() // make the allocation observable
	}()
	limits := fastLimits()
	limits.MemoryBudgetBytes = 4 << 20
	res := mustRun(t, `await sleep(900); return "unharmed";`, nil, limits)
	if res.Result != "unharmed" {
		t.Fatalf("got %v", res.Result)
	}
}

func TestRunMemoryBudgetTripsOnStringBomb(t *testing.T) {
	limits := fastLimits()
	limits.MemoryBudgetBytes = 8 << 20
	limits.WallClock = 30 * time.Second
	start := time.Now()
	res, failure := mustFail(t, `
console.log("before bomb");
let s = "x";
while (true) { s = s + s; }
`, nil, limits)

	if failure.Kind != FailureMemoryLimit {
		t.Fatalf("got %+v", failure)
	}
	if time.Since(start) >= 10*time.Second {
		t.Fatal("the memory trip line should catch a doubling bomb quickly")
	}
	if asJSON(t, res.Logs) != asJSON(t, []string{"before bomb"}) {
		t.Fatalf("logs = %v", res.Logs)
	}
}

// The heap metric counts allocated-but-unswept objects, so a program producing
// only short-lived garbage can cross the line between collections. The forced
// re-check must let it through.
func TestRunMemoryBudgetSparesTransientGarbage(t *testing.T) {
	limits := fastLimits()
	limits.MemoryBudgetBytes = 8 << 20 // far below the total garbage produced
	limits.WallClock = 30 * time.Second
	res := mustRun(t, `
let n = 0;
for (let i = 0; i < 200; i++) { const s = "x".repeat(1 << 20); n += s.length; }
return n;
`, nil, limits)
	if asJSON(t, res.Result) != strconv.Itoa(200<<20) {
		t.Fatalf("got %v", res.Result)
	}
}

func TestRunUnhandledRejectionSurfacesInLogs(t *testing.T) {
	boom := Binding{Name: "boom", Invoke: func(context.Context, string) (string, error) {
		return "", fmt.Errorf("rate limited (429)")
	}}
	res := mustRun(t, `
tools.boom({});   // deliberately not awaited
await sleep(300); // give the failure time to land
return "done";
`, []Binding{boom}, fastLimits())

	joined := strings.Join(res.Logs, "\n")
	if !strings.Contains(joined, "[unhandled rejection]") || !strings.Contains(joined, "429") {
		t.Fatalf("a fire-and-forget failure must not evaporate, logs = %v", res.Logs)
	}
}

func TestRunNonObjectArgsRejected(t *testing.T) {
	res := mustRun(t, `
const out = {};
try { await tools.echo("nope"); } catch (e) { out.str = e.constructor.name + ": " + e.message; }
try { await tools.echo([1, 2]); } catch (e) { out.arr = e.constructor.name + ": " + e.message; }
const ok = await tools.echo({fine: true});
out.ok = ok.args;
return out;
`, []Binding{echoBinding("echo", "x")}, fastLimits())

	m := res.Result.(map[string]any)
	if s := m["str"].(string); !strings.Contains(s, "TypeError") || !strings.Contains(s, "got string") {
		t.Fatalf("string args should be rejected in the prelude: %s", s)
	}
	if s := m["arr"].(string); !strings.Contains(s, "got an array") {
		t.Fatalf("array args should be rejected in the prelude: %s", s)
	}
	if asJSON(t, m["ok"]) != `{"fine":true}` {
		t.Fatalf("object args should reach the tool unchanged, got %v", m["ok"])
	}
}

func TestRunResultBudgetLimitsAccumulatedSubResults(t *testing.T) {
	bigPayload := strings.Repeat("y", 1<<20)
	limits := fastLimits()
	limits.ResultBudgetBytes = 3 << 20
	big := Binding{Name: "big", Invoke: func(context.Context, string) (string, error) {
		return `{"blob":"` + bigPayload + `"}`, nil
	}}
	_, failure := mustFail(t, `
for (let i = 0; i < 10; i++) { await tools.big({i}); }
return "never";
`, []Binding{big}, limits)
	if failure.Kind != FailureResultLimit {
		t.Fatalf("got %+v", failure)
	}
}

func TestRunDeepRecursionIsBounded(t *testing.T) {
	_, failure := mustFail(t, `
const f = (n) => n <= 0 ? 0 : 1 + f(n - 1);
f(100000);
`, nil, fastLimits())
	if failure.Kind != FailureException || !strings.Contains(failure.Message, "maximum call depth exceeded") {
		t.Fatalf("got %+v", failure)
	}

	res := mustRun(t, `
const fib = (n) => n < 2 ? n : fib(n - 1) + fib(n - 2);
return fib(15);
`, nil, fastLimits())
	if asJSON(t, res.Result) != "610" {
		t.Fatalf("recursion within the limit must still work, got %v", res.Result)
	}
}

func TestRunOutputLimitKeepsPrefix(t *testing.T) {
	limits := fastLimits()
	limits.MaxLogLines = 5
	res, failure := mustFail(t, `
for (let i = 0; i < 100; i++) { console.log("line " + i); }
return "done";
`, nil, limits)

	if failure.Kind != FailureOutputLimit {
		t.Fatalf("got %+v", failure)
	}
	if len(res.Logs) > 5 || res.Logs[0] != "line 0" {
		t.Fatalf("the prefix must be kept, logs = %v", res.Logs)
	}
}

func TestRunTooManySubCalls(t *testing.T) {
	limits := fastLimits()
	limits.MaxSubCalls = 3
	_, failure := mustFail(t, `
for (let i = 0; i < 10; i++) { await tools.echo({i}); }
`, []Binding{echoBinding("echo", "x")}, limits)
	if failure.Kind != FailureTooManyCalls {
		t.Fatalf("got %+v", failure)
	}
}

func TestRunCanceledContextAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, failure := Run(ctx, `
console.log("waiting");
const r = await tools.slow({});
return r;
`, []Binding{sleepBinding("slow", 10*time.Second, "late")}, fastLimits())

	if failure == nil || failure.Kind != FailureAborted {
		t.Fatalf("got %+v", failure)
	}
	if time.Since(start) >= 3*time.Second {
		t.Fatal("cancellation must not wait for the tool")
	}
	if asJSON(t, res.Logs) != asJSON(t, []string{"waiting"}) {
		t.Fatalf("logs = %v", res.Logs)
	}
}

func TestRunSubCallContextCancelsWithTheRun(t *testing.T) {
	// The program fires a slow call without awaiting and returns immediately.
	// The call must be canceled rather than left running against someone's API.
	var canceled sync.WaitGroup
	canceled.Add(1)
	bindings := []Binding{{Name: "slow", Invoke: func(ctx context.Context, args string) (string, error) {
		select {
		case <-time.After(5 * time.Second):
			return `{"late":true}`, nil
		case <-ctx.Done():
			canceled.Done()
			return "", ctx.Err()
		}
	}}}
	mustRun(t, `tools.slow({}); return "early";`, bindings, fastLimits())

	done := make(chan struct{})
	go func() { canceled.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an in-flight sub-call was not canceled when the run settled")
	}
}

func TestRunModernSyntax(t *testing.T) {
	res := mustRun(t, "\n"+
		"const xs = [...Array(5).keys()];\n"+
		"const doubled = xs.map(x => x * 2).filter(x => x > 2);\n"+
		"const {a, ...rest} = {a: 1, b: 2, c: 3};\n"+
		"const s = `sum:${xs.reduce((p, c) => p + c, 0)}`;\n"+
		"return {doubled, a, rest, s, opt: rest?.b ?? \"none\"};\n", nil, fastLimits())

	want := `{"doubled":[4,6,8],"a":1,"rest":{"b":2,"c":3},"s":"sum:10","opt":2}`
	var got, expected any
	_ = json.Unmarshal([]byte(asJSON(t, res.Result)), &got)
	_ = json.Unmarshal([]byte(want), &expected)
	if asJSON(t, got) != asJSON(t, expected) {
		t.Fatalf("got %s, want %s", asJSON(t, got), want)
	}
}

func TestRunNoAmbientCapabilities(t *testing.T) {
	res := mustRun(t, `
const names = Object.getOwnPropertyNames(globalThis).sort();
const has = {
  fetch: typeof fetch,
  require: typeof require,
  process: typeof process,
  print: typeof print,
  setTimeout: typeof setTimeout,
  setInterval: typeof setInterval,
  setImmediate: typeof setImmediate,
  clearTimeout: typeof clearTimeout,
  bridgeCall: typeof __call,
  bridgeLog: typeof __log,
  bridgeFulfill: typeof __fulfill,
  bridgeProgramError: typeof __programError,
  bridgeToolNames: typeof __toolNames,
};
return {ambient: has, globals: names};
`, []Binding{echoBinding("zzz_tool", "x")}, fastLimits())

	m := res.Result.(map[string]any)
	for name, typ := range m["ambient"].(map[string]any) {
		if typ != "undefined" {
			t.Errorf("global %s must not be visible, got %v", name, typ)
		}
	}

	// Everything the program can see, beyond the contract, has to be an
	// ECMAScript built-in. Those are the language; anything else is a capability.
	allowed := map[string]bool{
		"globalThis": true, "NaN": true, "Infinity": true, "undefined": true,
		"tools": true, "sleep": true,
		"Object": true, "Function": true, "Array": true, "String": true, "Number": true,
		"Boolean": true, "Math": true, "Date": true, "RegExp": true, "JSON": true,
		"Error": true, "EvalError": true, "RangeError": true, "ReferenceError": true,
		"SyntaxError": true, "TypeError": true, "URIError": true, "AggregateError": true,
		"Promise": true, "Symbol": true, "Map": true, "Set": true, "WeakMap": true,
		"WeakSet": true, "Proxy": true, "Reflect": true, "Intl": true, "BigInt": true,
		"ArrayBuffer": true, "DataView": true, "Float32Array": true, "Float64Array": true,
		"Int8Array": true, "Int16Array": true, "Int32Array": true, "Uint8Array": true,
		"Uint8ClampedArray": true, "Uint16Array": true, "Uint32Array": true,
		"BigInt64Array": true, "BigUint64Array": true, "WeakRef": true, "FinalizationRegistry": true,
		"isFinite": true, "isNaN": true, "parseFloat": true, "parseInt": true,
		"decodeURI": true, "decodeURIComponent": true, "encodeURI": true, "encodeURIComponent": true,
		"escape": true, "unescape": true, "eval": true, "GoError": true,
	}
	for _, g := range m["globals"].([]any) {
		if name := g.(string); !allowed[name] {
			t.Errorf("unexpected global %q visible to the program", name)
		}
	}
}

func TestRunSleepHelper(t *testing.T) {
	start := time.Now()
	res := mustRun(t, `await sleep(120); return "woke";`, nil, fastLimits())
	if res.Result != "woke" || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("got %v after %s", res.Result, time.Since(start))
	}
}

func TestRunNonJSONToolResultResolvesAsString(t *testing.T) {
	bindings := []Binding{{Name: "plain", Invoke: func(context.Context, string) (string, error) {
		return "just text, not json", nil
	}}}
	res := mustRun(t, `
const r = await tools.plain({});
return {got: r, type: typeof r};
`, bindings, fastLimits())

	m := res.Result.(map[string]any)
	if m["got"] != "just text, not json" || m["type"] != "string" {
		t.Fatalf("got %+v", m)
	}
}

func TestRunPanickingToolBecomesACatchableError(t *testing.T) {
	bindings := []Binding{{Name: "bad", Invoke: func(context.Context, string) (string, error) {
		panic("host bug")
	}}}
	res := mustRun(t, `
try { await tools.bad({}); } catch (e) { return {caught: e.message, tool: e.toolName}; }
return "not reached";
`, bindings, fastLimits())

	m := res.Result.(map[string]any)
	if !strings.Contains(m["caught"].(string), "host bug") || m["tool"] != "bad" {
		t.Fatalf("got %+v", m)
	}
}

func TestTailLogs(t *testing.T) {
	lines := []string{"a", "b", "c", "d"}
	if got := TailLogs(lines, 4); got != "…(older output omitted)\nc\nd" {
		t.Fatalf("got %q", got)
	}
	if got := TailLogs(lines, 1000); got != "a\nb\nc\nd" {
		t.Fatalf("got %q", got)
	}
	if got := TailLogs(nil, 100); got != "" {
		t.Fatalf("got %q", got)
	}
}
