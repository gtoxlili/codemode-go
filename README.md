# codemode-go

[![Go Reference](https://pkg.go.dev/badge/github.com/gtoxlili/codemode-go.svg)](https://pkg.go.dev/github.com/gtoxlili/codemode-go)
[![CI](https://github.com/gtoxlili/codemode-go/actions/workflows/ci.yml/badge.svg)](https://github.com/gtoxlili/codemode-go/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/gtoxlili/codemode-go)](https://goreportcard.com/report/github.com/gtoxlili/codemode-go)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**Code mode for Go agents.** The model writes one JavaScript program that calls your tools, instead of one tool call per turn.

[中文文档](README.zh-CN.md)

![A run_code tool call in production. The model's JavaScript program inspects the workspace, fans three searches out with Promise.all, branches into a conditional file read, calls a memory tool, sleeps, computes locally, and returns one compact report. Below the program, a timeline of the five sub-calls it made with their durations, then the tool result: logs plus the object the program returned.](assets/code-call.png)

*Eight steps, three searches overlapping at about 130ms each, five sub-calls, one result back. Everything the program read in between stayed in the process.*

## What is code mode?

Code mode gives the model one tool that runs a program, and exposes the rest of your tools as an API that program can call. The model writes `await tools.search({query})` instead of emitting a tool call and waiting a turn for the answer.

Two things follow. Independent calls run at the same time, because `Promise.all` is a real thing and "emit two tool calls and hope the runtime parallelizes them" is not. And intermediate results stay in the process — the model sees what the program returns, not the fifty search hits it looked at to produce it.

The idea goes by several names. [Anthropic](https://www.anthropic.com/engineering/code-execution-with-mcp) calls it code execution with MCP, [Cloudflare](https://blog.cloudflare.com/code-mode/) calls it Code Mode, the [CodeAct paper](https://arxiv.org/abs/2402.01030) calls it code-as-action. Same shape: replace JSON tool calls with a program.

This library is the Go implementation. Pulled out of an agent that has been running it in production.

```js
// one tool call, twenty candidates, one digest back
const handles = await tools.list_candidates({campaign: "spring"});
const profiles = await Promise.all(handles.data.map(h => tools.fetch_profile({handle: h})));
const fit = profiles
  .filter(p => p.data.followers > 10000)
  .map(p => ({handle: p.data.handle, ratio: p.data.engagement / p.data.followers}))
  .sort((a, b) => b.ratio - a.ratio)
  .slice(0, 5);
return fit;
```

The same work through direct tool calls is twenty-one calls across at least three turns, and every intermediate profile lands in the conversation and stays there for the rest of the session.

## Why

**Context.** Intermediate results stay in the process. A sweep over fifty search hits costs one tool result instead of fifty, and the next turn does not carry the forty-five that turned out to be irrelevant. Anthropic's writeup puts one such case at 150,000 tokens down to 2,000.

**Parallelism you actually get.** `Promise.all` puts calls into a real worker pool. Waiting for the model to emit another batch of tool calls does not.

**Logic the model would otherwise narrate.** Filtering, scoring, joining two sources on a key — a program does it in four lines instead of a paragraph of reasoning over data that had to be pasted into the context first.

It is built to sit alongside direct tool calls rather than replace them. Most turns are one or two calls, where a program is pure overhead; the tool description tells the model when a program is worth it.

## Install

```bash
go get github.com/gtoxlili/codemode-go
```

Go 1.25+. The core module depends on [goja](https://github.com/dop251/goja) and nothing else. No cgo, no Node, no Deno, no container.

## Use

Three steps. Bind your tools, mount the tool, teach the model.

```go
import "github.com/gtoxlili/codemode-go"

bindings := []codemode.Binding{{
    Name:   "search_files",
    Invoke: searchFiles, // func(ctx context.Context, argsJSON string) (string, error)
}, {
    Name:         "write_file",
    Invoke:       writeFile,
    Mutating:     true,
    ConflictKeys: func(args string) []string { return []string{"file:" + pathOf(args)} },
}}

tool := codemode.NewTool(codemode.Options{
    Bindings: bindings,
    Blocked: []codemode.Blocked{{
        Name:   "ask_user",
        Reason: "ask_user ends the turn — ask before you start the program",
    }},
})
```

`tool` gives you `Name()`, `Description()`, `Parameters()` and `Call(ctx, argsJSON)`, which is what a tool-calling loop needs. `Prompt` returns the matching system-prompt section:

```go
systemPrompt += "\n\n" + codemode.Prompt(codemode.PromptOptions{})
```

The description says what the tool is and which tools a program may call. The prompt section covers how to write one: that a call resolves to the same envelope a direct call returns, that a failed call rejects with a catchable error, what runs in parallel, and that only the return value and console output come back.

### Blocked tools

`Blocked` names a tool the model has in its tool list but cannot call from inside a program. Blocked names appear in the generated description's "minus" list, and a program that calls one gets the `Reason` back as that call's error rather than an "unknown tool".

### Watching a run

`OnCall` fires around every sub-call and `OnProgram` once per run, before the program starts:

```go
codemode.Options{
    Bindings: bindings,
    OnCall: func(ctx context.Context, ev codemode.CallEvent) {
        // ev.Seq, ev.Tool, ev.Args, ev.Phase, ev.Duration, ev.Err
        ui.Push(ctx, ev)
    },
    OnProgram: func(ctx context.Context, code, description string) {
        ui.Label(ctx, description) // "Vet 20 candidates and score fit"
    },
}
```

Both run inline on the goroutine making the call, so a slow observer slows the program down. Events carry the raw arguments, not a digest.

### Return-shape hints

Tool protocols ship the input schema and say nothing about the output, so a program navigating `r.data.hits` is working from a guess. `ReturnShape` derives a compact hint from a Go result type, meant for the tail of a tool description:

```go
desc += "\n\nReturns `{data: " + codemode.ReturnShape[SearchResult]() + "}`."
// Returns `{data: {query, hits: [{path, line: num, snippet}], truncated: bool, cursor?}}`.
```

A bare name is a string, other types are annotated, `?` marks an omitempty field. `ReturnShapes: true` in `PromptOptions` adds the matching notation guide to the prompt section; with it false the section says nothing about return shapes.

## Adapters

Separate modules, so the core stays at one dependency.

### eino

```bash
go get github.com/gtoxlili/codemode-go/adapters/eino
```

```go
bindings, err := einocodemode.Bindings(ctx, myTools)
ct := codemode.NewTool(codemode.Options{Bindings: bindings})
myTools = append(myTools, einocodemode.NewTool(ct))
```

[eino](https://github.com/cloudwego/eino) tools carry no notion of writing or of which resource they touch, so the bindings come back with `Mutating` false and `ConflictKeys` nil and every call schedules as conflict-free. The returned slice is plain and can be edited before it is passed on.

### MCP

```bash
go get github.com/gtoxlili/codemode-go/adapters/mcp
```

```go
discovered, err := mcpcodemode.Tools(ctx, mcpClient)
ct := codemode.NewTool(codemode.Options{Bindings: mcpcodemode.Bindings(discovered)})
```

That form assumes the server's tools are already in the model's tool list, where sixty of them cost sixty schemas per request. The second form keeps them out of it and puts the catalog in the program tool's description instead, for a per-turn cost of one description:

```go
ct := codemode.NewTool(codemode.Options{
    Bindings:    mcpcodemode.Bindings(discovered),
    Description: intro + "\n\n" + mcpcodemode.Catalog(discovered),
})
```

It supplies its own description because the generated one states that a program can call the tools in the model's tool list, which no longer holds once they are out of it.

Works with [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go) clients over any transport: stdio, SSE, streamable HTTP, in-process.

## How it compares

Code mode is an idea with implementations in several ecosystems. Where they differ is what language the model writes, what runs it, and how much of the surrounding machinery you have to build yourself.

| Project | Model writes | Runs in | Your host is | Isolation |
|---|---|---|---|---|
| **codemode-go** | JavaScript | goja, in-process | Go | capability omission + sampled limits |
| [Cloudflare Code Mode](https://blog.cloudflare.com/code-mode/) | TypeScript | V8 isolate on Workers | TypeScript / Workers | V8 isolate, no network |
| [Anthropic code execution with MCP](https://www.anthropic.com/engineering/code-execution-with-mcp) | TypeScript | your choice — it is a pattern, not a library | anything | yours to build |
| [UTCP code-mode](https://github.com/universal-tool-calling-protocol/code-mode) | TypeScript / Python | Node `vm` | TypeScript, Python | Node `vm` context |
| [Edison-Watch/mcp-code-mode](https://github.com/Edison-Watch/mcp-code-mode) | TypeScript | Deno | Python | Deno permissions + AST allowlist |
| [smolagents CodeAgent](https://github.com/huggingface/smolagents) | Python | restricted interpreter, or E2B / Docker | Python | depends on the executor you pick |
| [langchain-sandbox](https://github.com/langchain-ai/langchain-sandbox) | Python | Pyodide in Deno | Python | Deno permissions |
| [pydantic/mcp-run-python](https://github.com/pydantic/mcp-run-python) | Python | Pyodide in Deno, as an MCP server | anything | Deno permissions |
| [Protocol-Lattice/go-agent](https://github.com/Protocol-Lattice/go-agent) | Go | its UTCP runtime | Go (that framework) | requires opting into unsafe tools |

**What this one is good at.** It is a library, not a framework and not a runtime. There is no Node, Deno, Docker, or cloud platform underneath it — one Go dependency, one process, and the same wiring whether the agent loop is hand-rolled, eino, or an MCP client. What comes in the box beyond the engine: the system-prompt section, a scheduler that serializes calls touching the same resource, a failure taxonomy phrased for the model to correct from, and hooks around every sub-call. A run costs one goja VM to start, which is microseconds, so a program that makes two calls is not a losing trade.

**What it is not good at.** It is not a security boundary — see below, and if you need one, the Deno and V8-isolate options above are genuinely stronger. The model writes JavaScript, not Go, so your Go tools are reachable only through the bindings you pass; there is no way to hand a program a Go value directly. There is no typed SDK generation, so the model works from your tool descriptions rather than from TypeScript types with autocomplete — `ReturnShape` narrows that gap but does not close it. And a program shares the host process's memory, which is why the memory limit is a trip line with a documented margin of error rather than a hard ceiling.

## What a program can see

| Name | What it is |
|---|---|
| `tools` | `tools.name(args)` returns a promise resolving to that tool's result, parsed if it is JSON |
| `ToolCallError` | what a failed call rejects with, carrying `.toolName`; catch it and continue |
| `console` | log/info/warn/error/debug, all into one log channel |
| `sleep(ms)` | the only way to wait; the `setTimeout` family does not exist |

Plus the JavaScript built-ins. No filesystem, no network, no `require`, no `process`, no `fetch`. Every external effect goes through a binding you passed in.

Capability omission is the real boundary here. The rest, stated plainly: **this is not a security sandbox.** The program runs in-process on goja, which has no per-VM memory ceiling and no instruction counter. The wall clock, the compute budget and the memory trip line are enforced by a 50ms sampler plus goja's `Interrupt`. They catch hot loops and allocation bombs in well under a second, which is what a model produces by accident. They do not stop someone who controls the program and is trying.

## Limits

| Limit | Default | Notes |
|---|---|---|
| Wall clock | 10 min | Interrupt, then a 5s grace period, then torn down |
| Compute budget | 2 min | only counts time actually running JS — waiting on tools does not burn it |
| Memory | 256 MB | heap growth over the run's baseline, re-checked after a forced GC before killing anything |
| Accumulated results | 64 MB | exact, counted as each result arrives |
| Call depth | 8192 | |
| Parallel sub-calls | 8 | the pool a `Promise.all` gets |
| Sub-calls per run | 200 | |
| Console output | 200KB / 2000 lines | crossing it fails the run and keeps what was collected |

All overridable through `Options.Limits`. Zero means "use the default", so `Validate()` exists for values that came out of a config file — a `wall_clock` accidentally set to zero would otherwise silently become ten minutes.

The compute budget and the wall clock differ in what they count. A fan-out over twenty slow APIs can run for minutes without burning any compute budget, because none of that time is spent running JavaScript. A `while (true) {}` burns it in two minutes flat, long before the wall clock. Slow tools and runaway programs are different problems and get different limits.

## Failures

A failed run reports a kind, and each kind maps to one thing the model can do about it. The message is written to be read by the model, with the tail of whatever the program printed attached.

| Kind | What to do |
|---|---|
| `exception` | fix the syntax or the logic |
| `timeout` | narrow the loop, make fewer calls |
| `compute-limit` | compute less — this is not about slow tools |
| `memory-limit` | stop building unbounded strings and arrays |
| `result-limit` | select or aggregate in code instead of holding everything |
| `output-limit` | print less, return more |
| `invalid-return` | return plain JSON-serializable data |
| `too-many-calls` | nothing; the run was stopped on purpose |
| `aborted` | nothing; the caller canceled |

A sub-call that fails does not fail the run: it rejects with a `ToolCallError` the program can catch, so one dead source does not throw away the work already done. A call fired without `await` still runs, and if it fails its rejection shows up as an `[unhandled rejection]` log line rather than evaporating.

## Scheduling

Calls start in the order the program made them. Ordinary calls go into a pool of `MaxParallel`. A call that shares a `ConflictKey` with an outstanding one, where at least one of the two mutates, waits for the pool to drain and runs alone.

Keys come from the tool, not the runtime — the scheduler does not know any tool's parameter schema, so the tool is the only thing that can say what a given call touches. A constant works for a shared backend, `"deck:" + id` for a resource id, an absolute path for a file. Keys are compared as strings, so `out/a.jpg` and `./out/a.jpg` are two keys and the collision between them goes unseen.

Conflicts are pruned as soon as a call finishes, since two calls can only conflict while they overlap. Without that, writing a digest and then reading a batch of files back — including the one just written — would serialize the whole read fan-out behind a write that is already done.

## FAQ

### Is this a security sandbox?

No. Capability omission is real — a program has no filesystem, network, or imports, and every effect goes through a binding that was passed in. The resource limits are a different story: they are sampled trip lines that catch what a model writes by accident, not a boundary against an adversary.

### Why JavaScript instead of Go?

Because the model writes it and goja runs it in-process with no cgo, no subprocess, and no container. Compiling and running model-written Go means a toolchain, a build step, and real isolation work before anything runs at all. Models also write far more JavaScript than they write Go, which shows up directly in how often the program is correct on the first attempt.

### Does this replace normal tool calling?

No. It is designed to coexist with direct tool calls: most turns are one or two calls, where a program is pure overhead, and the tool description describes when a program is worth it so the model can choose.

### How much context does it actually save?

It depends entirely on how much data your tools return and how much of it the answer needs. The saving is the part the program throws away — fan out over fifty results and return five, and the other forty-five never enter the conversation. Anthropic's writeup measures one such case at 150,000 tokens down to 2,000. A program that fetches one thing and returns it saves nothing.

### Do I need MCP?

No. MCP is one source of tools, and there is an adapter for it because that is where the savings are largest. A `Binding` is just a name and a `func(ctx, argsJSON) (string, error)`, so anything you can call from Go can be a tool.

### Which Go agent frameworks does this work with?

Any of them, and none of them. The core produces a name, a description, a JSON Schema and a `Call` — wire those into whatever loop you have. There is a ready-made adapter for [eino](https://github.com/cloudwego/eino) and one for MCP clients. A hand-written loop over the OpenAI or Anthropic SDK takes about five lines.

### Do I need Node, Deno, or Docker?

No. goja is a JavaScript engine written in pure Go. The program runs inside your process.

### What happens if the model writes an infinite loop?

It dies in about two minutes, on the compute budget, well before the wall clock — and the model gets back a `compute-limit` failure telling it to compute less, along with whatever the program printed before it hung. `try/catch` cannot swallow the interrupt; there is a test for that.

### Can a program call another program?

No. The tool excludes itself from its own bindings, and its name is the first entry in the description's "minus" list, so the model is told as much.

## Further reading

- [Code execution with MCP](https://www.anthropic.com/engineering/code-execution-with-mcp) — Anthropic, the clearest writeup of the problem
- [Code Mode: the better way to use MCP](https://blog.cloudflare.com/code-mode/) — Cloudflare
- [Executable Code Actions Elicit Better LLM Agents](https://arxiv.org/abs/2402.01030) — the CodeAct paper, where the measurements start

## Status

v0.x. The engine is stable and in production; the surface around it may still move. Issues and PRs welcome.

MIT.
