package codemode

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func fileKeys(args string) []string {
	var a struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal([]byte(args), &a)
	if a.Path == "" {
		return nil
	}
	return []string{"file:" + a.Path}
}

type schedEvent struct {
	name  string
	phase string
	at    time.Time
}

func TestSchedulerExclusiveCallsSerialize(t *testing.T) {
	var mu sync.Mutex
	var events []schedEvent
	record := func(name string, d time.Duration) func(context.Context, string) (string, error) {
		return func(context.Context, string) (string, error) {
			mu.Lock()
			events = append(events, schedEvent{name, "start", time.Now()})
			mu.Unlock()
			time.Sleep(d)
			mu.Lock()
			events = append(events, schedEvent{name, "end", time.Now()})
			mu.Unlock()
			return `{"ok":true}`, nil
		}
	}
	writer := Binding{Name: "write_file", Mutating: true, ConflictKeys: fileKeys, Invoke: record("write_file", 80*time.Millisecond)}
	reader := Binding{Name: "read_file", ConflictKeys: fileKeys, Invoke: record("read_file", 80*time.Millisecond)}

	// Two reads of different files overlap. The write that follows shares
	// file:a with one of them, so it waits for the pool to drain and runs alone.
	mustRun(t, `
await Promise.all([
  tools.read_file({path: "a"}),
  tools.read_file({path: "b"}),
]);
await tools.write_file({path: "a", content: "x"});
return "ok";
`, []Binding{writer, reader}, fastLimits())

	mu.Lock()
	defer mu.Unlock()
	var readEnds []time.Time
	var writeStart time.Time
	for _, e := range events {
		switch {
		case e.name == "read_file" && e.phase == "end":
			readEnds = append(readEnds, e.at)
		case e.name == "write_file" && e.phase == "start":
			writeStart = e.at
		}
	}
	if len(readEnds) != 2 || writeStart.IsZero() {
		t.Fatalf("events = %+v", events)
	}
	for _, re := range readEnds {
		if writeStart.Before(re) {
			t.Fatal("the write started before a same-resource read finished")
		}
	}
}

// Conflicts only hold between calls that overlap. Without pruning finished
// calls, writing a digest and then reading a batch back — including the file
// just written — would serialize the whole fan-out behind a write that is done.
func TestSchedulerCompletedCallsPrunedFromConflictSet(t *testing.T) {
	var mu sync.Mutex
	var cur, peak int
	writer := Binding{Name: "write_file", Mutating: true, ConflictKeys: fileKeys,
		Invoke: func(context.Context, string) (string, error) {
			time.Sleep(30 * time.Millisecond)
			return `{"ok":true}`, nil
		}}
	reader := Binding{Name: "read_file", ConflictKeys: fileKeys,
		Invoke: func(context.Context, string) (string, error) {
			mu.Lock()
			cur++
			if cur > peak {
				peak = cur
			}
			mu.Unlock()
			time.Sleep(80 * time.Millisecond)
			mu.Lock()
			cur--
			mu.Unlock()
			return `{"ok":true}`, nil
		}}

	mustRun(t, `
await tools.write_file({path: "digest.md", content: "x"});
const paths = ["a", "b", "c", "d", "digest.md", "f"];
await Promise.all(paths.map(p => tools.read_file({path: p})));
return "ok";
`, []Binding{writer, reader}, fastLimits())

	mu.Lock()
	defer mu.Unlock()
	if peak != 6 {
		t.Fatalf("all six reads should overlap in one batch, peak concurrency was %d", peak)
	}
}

func TestSchedulerRespectsMaxParallel(t *testing.T) {
	var mu sync.Mutex
	var cur, peak int
	limits := fastLimits()
	limits.MaxParallel = 3
	b := Binding{Name: "work", Invoke: func(context.Context, string) (string, error) {
		mu.Lock()
		cur++
		if cur > peak {
			peak = cur
		}
		mu.Unlock()
		time.Sleep(40 * time.Millisecond)
		mu.Lock()
		cur--
		mu.Unlock()
		return `{"ok":true}`, nil
	}}

	mustRun(t, `
await Promise.all([...Array(12).keys()].map(i => tools.work({i})));
return "ok";
`, []Binding{b}, limits)

	mu.Lock()
	defer mu.Unlock()
	if peak > 3 {
		t.Fatalf("pool of 3 exceeded, peak concurrency was %d", peak)
	}
	if peak < 2 {
		t.Fatalf("the pool never overlapped anything, peak concurrency was %d", peak)
	}
}

// ConflictKeys is host code. If it panics the run keeps going with the call
// scheduled as conflict-free — a broken key function is a host bug, not a
// reason to fail the model's program.
func TestSchedulerAbsorbsConflictKeysPanic(t *testing.T) {
	b := Binding{
		Name:         "boom_keys",
		ConflictKeys: func(string) []string { panic("bad key function") },
		Invoke:       func(context.Context, string) (string, error) { return `{"ok":true}`, nil },
	}
	res := mustRun(t, `const r = await tools.boom_keys({}); return r.ok;`, []Binding{b}, fastLimits())
	if res.Result != true {
		t.Fatalf("got %v", res.Result)
	}
}

func TestWithCallEventsObservesEveryCall(t *testing.T) {
	var mu sync.Mutex
	var seen []CallEvent
	bindings := WithCallEvents([]Binding{
		echoBinding("a", "1"),
		{Name: "b", Invoke: func(context.Context, string) (string, error) {
			return "", context.DeadlineExceeded
		}},
	}, func(_ context.Context, ev CallEvent) {
		mu.Lock()
		seen = append(seen, ev)
		mu.Unlock()
	})

	mustRun(t, `
await tools.a({x: 1});
try { await tools.b({}); } catch (e) {}
return "ok";
`, bindings, fastLimits())

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 4 {
		t.Fatalf("expected start+end for two calls, got %d events: %+v", len(seen), seen)
	}
	if seen[0].Phase != PhaseStart || seen[0].Tool != "a" || seen[0].Seq != 1 {
		t.Fatalf("first event = %+v", seen[0])
	}
	if seen[1].Phase != PhaseDone || seen[1].Duration <= 0 {
		t.Fatalf("second event = %+v", seen[1])
	}
	if seen[3].Phase != PhaseError || seen[3].Err == nil {
		t.Fatalf("fourth event = %+v", seen[3])
	}
}
