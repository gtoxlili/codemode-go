package codemode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultToolName is the tool's name unless [Options.Name] overrides it.
const DefaultToolName = "run_code"

// Blocked names a tool the model can see in its tool list but cannot call from
// inside a program. It shows up in the tool description's "minus" list, and a
// program that calls it anyway gets Reason back as the call's error rather than
// an "unknown tool".
type Blocked struct {
	Name   string
	Reason string
}

// Options configures [NewTool].
type Options struct {
	// Bindings are the tools a program may call. Required.
	Bindings []Binding

	// Blocked are tools the model has but a program may not call.
	Blocked []Blocked

	// Name defaults to [DefaultToolName].
	Name string

	// Description overrides the generated one. The generated text states which
	// tools are callable, derived from Bindings and Blocked; an override does
	// not, and is not checked against them.
	Description string

	// Limits defaults to [DefaultLimits].
	Limits Limits

	// MaxConcurrentRuns bounds how many programs this tool runs at once,
	// defaulting to 4. Each run is one VM plus its own pool of MaxParallel
	// sub-calls. Waiting for a slot respects context cancellation.
	MaxConcurrentRuns int

	// OnCall observes each sub-call. See [WithCallEvents].
	OnCall func(ctx context.Context, ev CallEvent)

	// OnProgram fires once per run, before the program starts, with the code and
	// the model's one-line description of what it does.
	OnProgram func(ctx context.Context, code, description string)

	// LogTailBytes is how much captured output is attached to a failure.
	// Defaults to 8000.
	LogTailBytes int
}

// Tool is the run_code tool: a name, a description, an argument schema, and a
// Call that takes the model's raw argument JSON. The adapters under ./adapters
// map those onto specific frameworks.
type Tool struct {
	name         string
	desc         string
	bindings     []Binding
	limits       Limits
	slots        chan struct{}
	onProgram    func(ctx context.Context, code, description string)
	logTailBytes int
}

// NewTool assembles the tool. The generated description enumerates Bindings and
// Blocked as they are at this moment, so it is a snapshot: a tool added to the
// model's tool list afterwards is not in it.
func NewTool(opts Options) *Tool {
	name := opts.Name
	if name == "" {
		name = DefaultToolName
	}
	limits := opts.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	runs := opts.MaxConcurrentRuns
	if runs <= 0 {
		runs = 4
	}
	tail := opts.LogTailBytes
	if tail <= 0 {
		tail = 8000
	}

	bindings := make([]Binding, 0, len(opts.Bindings)+len(opts.Blocked))
	bindings = append(bindings, opts.Bindings...)
	for _, b := range opts.Blocked {
		reason := b.Reason
		if reason == "" {
			reason = fmt.Sprintf("%s cannot be called from inside a program — call it directly", b.Name)
		}
		bindings = append(bindings, Binding{
			Name:   b.Name,
			Invoke: func(context.Context, string) (string, error) { return "", fmt.Errorf("%s", reason) },
		})
	}
	bindings = WithCallEvents(bindings, opts.OnCall)

	desc := opts.Description
	if desc == "" {
		desc = describe(name, opts.Blocked)
	}

	return &Tool{
		name:         name,
		desc:         desc,
		bindings:     bindings,
		limits:       limits,
		slots:        make(chan struct{}, runs),
		onProgram:    opts.OnProgram,
		logTailBytes: tail,
	}
}

// describe states what the tool is, which tools a program may call, and when to
// reach for it.
//
// The "minus" list leads with the tool's own name. The sentence is an exact
// claim — callable set equals current tool list minus this list — and the tool
// is in the model's tool list while being uncallable from a program, so leaving
// it out would make the claim false.
//
// Selection is described by mechanism (fan-out, chaining, filtering) rather
// than by a call-count threshold, which models follow literally.
func describe(name string, blocked []Blocked) string {
	minus := make([]string, 0, len(blocked)+1)
	minus = append(minus, name)
	for _, b := range blocked {
		if b.Name != name {
			minus = append(minus, b.Name)
		}
	}
	return "Runs one JavaScript program that orchestrates your tools in batch; only what the program prints or returns comes back to the conversation. " +
		"The tools callable in a program are exactly those in your current tool list, minus: " + strings.Join(minus, ", ") + ". " +
		"Reach for it when a batch of calls collapses into one digest — parallel fan-out, chained transforms, filtering bulk results down to what matters; a lone call is cheaper made directly."
}

func (t *Tool) Name() string        { return t.name }
func (t *Tool) Description() string { return t.desc }

// Parameters returns the JSON Schema for the tool's arguments, as a fresh map
// you may mutate.
func (t *Tool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"code": map[string]any{
				"type":        "string",
				"description": "The program: the body of an async JavaScript function. Top-level await and return work.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "Clear, concise description of what this program does in active voice, 5-10 words (shown in the UI). Examples: \"Vet 20 candidates and score fit\"; \"Sweep four sources for release notes\".",
			},
		},
		"required": []string{"code", "description"},
	}
}

// ParametersJSON is [Tool.Parameters] marshaled, for the many APIs that want
// the schema as bytes.
func (t *Tool) ParametersJSON() json.RawMessage {
	b, _ := json.Marshal(t.Parameters())
	return b
}

type toolArgs struct {
	Code        string `json:"code"`
	Description string `json:"description"`
}

// Call runs a program from the model's raw argument JSON and returns the result
// as JSON: {"logs": [...], "result": ...}, with result omitted when the program
// returned nothing.
//
// A failed run comes back as an error whose message carries the failure kind
// and the tail of whatever the program printed, phrased for the model to read.
func (t *Tool) Call(ctx context.Context, argsJSON string) (string, error) {
	var args toolArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Code) == "" {
		return "", fmt.Errorf("invalid arguments: code is required")
	}
	if t.onProgram != nil {
		t.onProgram(ctx, args.Code, args.Description)
	}

	res, failure := t.Run(ctx, args.Code)
	if failure != nil {
		msg := failure.Error()
		if tail := TailLogs(res.Logs, t.logTailBytes); tail != "" {
			msg += "\n\nCaptured output:\n" + tail
		}
		return "", fmt.Errorf("%s", msg)
	}
	out, err := json.Marshal(res)
	if err != nil {
		return "", fmt.Errorf("serializing the run result: %w", err)
	}
	return string(out), nil
}

// Run executes a program against this tool's bindings and limits, waiting for a
// concurrency slot first, and returns the structured Result and Failure rather
// than the wire form [Tool.Call] produces.
func (t *Tool) Run(ctx context.Context, code string) (Result, *Failure) {
	select {
	case t.slots <- struct{}{}:
		defer func() { <-t.slots }()
	case <-ctx.Done():
		return Result{}, &Failure{Kind: FailureAborted, Message: "canceled while waiting for a run slot"}
	}
	return Run(ctx, code, t.bindings, t.limits)
}
