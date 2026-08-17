package codemode

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// countingCodec is encoding/json with a tally, and deliberately WITHOUT
// UnmarshalString — it exercises the fallback path.
type countingCodec struct {
	mu        sync.Mutex
	marshals  int
	unmarshal int
}

func (c *countingCodec) Marshal(v any) ([]byte, error) {
	c.mu.Lock()
	c.marshals++
	c.mu.Unlock()
	return json.Marshal(v)
}

func (c *countingCodec) Unmarshal(b []byte, v any) error {
	c.mu.Lock()
	c.unmarshal++
	c.mu.Unlock()
	return json.Unmarshal(b, v)
}

func (c *countingCodec) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.marshals, c.unmarshal
}

// stringCodec implements the StringUnmarshaler extension, so the engine must
// never route a sub-call result through the []byte path.
type stringCodec struct {
	countingCodec
	mu      sync.Mutex
	strings int
}

func (c *stringCodec) UnmarshalString(s string, v any) error {
	c.mu.Lock()
	c.strings++
	c.mu.Unlock()
	return json.Unmarshal([]byte(s), v)
}

func (c *stringCodec) stringCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.strings
}

// TestWithCodecDecodesSubCallResults pins that the injected codec — not
// encoding/json — is what a program's view of a tool result comes through.
func TestWithCodecDecodesSubCallResults(t *testing.T) {
	codec := &countingCodec{}
	res, failure := Run(context.Background(),
		`const r = await tools.echo({x: 1}); return r.tool;`,
		[]Binding{echoBinding("echo", "payload")}, fastLimits(), WithCodec(codec))
	if failure != nil {
		t.Fatalf("unexpected failure: %v", failure)
	}
	if res.Result != "echo" {
		t.Fatalf("got %v, want the decoded result", res.Result)
	}
	if _, unmarshals := codec.counts(); unmarshals != 1 {
		t.Fatalf("codec saw %d unmarshals, want exactly the one sub-call result", unmarshals)
	}
}

// TestCodecStringExtensionSkipsTheCopy pins the StringUnmarshaler fast path:
// a codec that offers it must be asked through it, never through Unmarshal.
func TestCodecStringExtensionSkipsTheCopy(t *testing.T) {
	codec := &stringCodec{}
	res, failure := Run(context.Background(),
		`const a = await tools.echo({}); const b = await tools.echo({}); return a.tool + b.tool;`,
		[]Binding{echoBinding("echo", "payload")}, fastLimits(), WithCodec(codec))
	if failure != nil {
		t.Fatalf("unexpected failure: %v", failure)
	}
	if res.Result != "echoecho" {
		t.Fatalf("got %v", res.Result)
	}
	if got := codec.stringCount(); got != 2 {
		t.Fatalf("UnmarshalString called %d times, want 2", got)
	}
	if _, unmarshals := codec.counts(); unmarshals != 0 {
		t.Fatalf("Unmarshal called %d times; the string extension must take every sub-call result", unmarshals)
	}
}

// TestNilOptionsFallBackToDefaults keeps the zero-config path honest: passing
// nothing, or passing an explicitly nil dependency, must still run.
func TestNilOptionsFallBackToDefaults(t *testing.T) {
	res, failure := Run(context.Background(),
		`const r = await tools.echo({}); return r.tool;`,
		[]Binding{echoBinding("echo", "p")}, fastLimits(),
		WithCodec(nil), WithLogger(nil), nil)
	if failure != nil {
		t.Fatalf("unexpected failure: %v", failure)
	}
	if res.Result != "echo" {
		t.Fatalf("got %v", res.Result)
	}
}

// TestWithLoggerReceivesConflictKeysPanic pins the one internal warning: it
// reaches the host's logger with the tool name and the panic value, and the run
// still completes with the call scheduled as conflict-free.
func TestWithLoggerReceivesConflictKeysPanic(t *testing.T) {
	type line struct {
		msg   string
		attrs []any
		ctx   context.Context
	}
	var mu sync.Mutex
	var seen []line

	b := Binding{
		Name:         "boom_keys",
		ConflictKeys: func(string) []string { panic("bad key function") },
		Invoke:       func(context.Context, string) (string, error) { return `{"ok":true}`, nil },
	}
	res, failure := Run(context.Background(),
		`const r = await tools.boom_keys({}); return r.ok;`,
		[]Binding{b}, fastLimits(),
		WithLogger(func(ctx context.Context, msg string, attrs ...any) {
			mu.Lock()
			seen = append(seen, line{msg: msg, attrs: attrs, ctx: ctx})
			mu.Unlock()
		}))
	if failure != nil {
		t.Fatalf("unexpected failure: %v", failure)
	}
	if res.Result != true {
		t.Fatalf("got %v, want the run to survive the panic", res.Result)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("logger saw %d lines, want 1", len(seen))
	}
	if !strings.Contains(seen[0].msg, "ConflictKeys panicked") {
		t.Fatalf("message = %q", seen[0].msg)
	}
	if seen[0].ctx == nil {
		t.Fatal("logger got a nil context; a host correlating by trace needs the run's own")
	}
	// slog-style alternating key/value pairs.
	if len(seen[0].attrs) != 4 || seen[0].attrs[0] != "tool" || seen[0].attrs[1] != "boom_keys" {
		t.Fatalf("attrs = %v, want tool/boom_keys then panic/<value>", seen[0].attrs)
	}
}

// TestToolThreadsDepsThroughOptions pins that NewTool's Codec/Logger reach both
// the wire methods (Call, ParametersJSON) and the engine underneath.
func TestToolThreadsDepsThroughOptions(t *testing.T) {
	codec := &countingCodec{}
	tool, err := NewTool(Options{
		Bindings: []Binding{echoBinding("echo", "payload")},
		Codec:    codec,
	})
	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}
	if len(tool.ParametersJSON()) == 0 {
		t.Fatal("ParametersJSON came back empty")
	}
	out, err := tool.Call(context.Background(),
		`{"code":"const r = await tools.echo({}); return r.tool;","description":"probe"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, `"echo"`) {
		t.Fatalf("out = %s", out)
	}
	marshals, unmarshals := codec.counts()
	// Marshal: the parameter schema + the run result. Unmarshal: the model's
	// arguments + the one sub-call result.
	if marshals != 2 {
		t.Fatalf("codec saw %d marshals, want 2 (schema, result)", marshals)
	}
	if unmarshals != 2 {
		t.Fatalf("codec saw %d unmarshals, want 2 (args, sub-call result)", unmarshals)
	}
}
