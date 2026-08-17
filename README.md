# codemode-go

[![Go Reference](https://pkg.go.dev/badge/github.com/gtoxlili/codemode-go.svg)](https://pkg.go.dev/github.com/gtoxlili/codemode-go)
[![CI](https://github.com/gtoxlili/codemode-go/actions/workflows/ci.yml/badge.svg)](https://github.com/gtoxlili/codemode-go/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/gtoxlili/codemode-go)](https://goreportcard.com/report/github.com/gtoxlili/codemode-go)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**Programmatic tool calling for Go agents.** Not a replacement for tool calling — a second execution strategy for the tools you already have. Direct calls keep working exactly as they do; the model also gets to write one JavaScript program that calls many of them and returns a digest. No new agent loop, no rewriting tools into another API, nothing hidden behind a runtime.

[中文文档](README.zh-CN.md)

![A run_code tool call in production: the model's JavaScript program inspects the workspace, fans three searches out with Promise.all, branches into a conditional file read, calls a memory tool, sleeps, computes locally, and returns one compact report. Below the program, a timeline of the five sub-calls it made with their durations, then the tool result: logs plus the object the program returned.](assets/code-call.png)

*Eight steps, three searches overlapping at about 130ms each, five sub-calls, one result back. Everything the program read in between stayed in the process.*

## The shape of it

If you are writing an agent in Go, your runtime already has a tool registry — `read`, `list`, `grep`, `edit`, `bash`, whatever your product needed. Each entry is already a function from arguments to a result. Binding them costs a few lines each:

```go
bindings := []codemode.Binding{
    {Name: "list_files", Invoke: listFiles},
    {Name: "read_file",  Invoke: readFile,  ConflictKeys: fileKey},
    {Name: "grep",       Invoke: grep,      ConflictKeys: fileKey},
    {Name: "write_file", Invoke: writeFile, ConflictKeys: fileKey, Mutating: true},
}
tool, err := codemode.NewTool(codemode.Options{Bindings: bindings})
```

and the same capabilities the model reaches one call at a time become something it can program against:

```js
const listed = await tools.list_files({path: "src"});
const hits = await Promise.all(
  listed.data.entries.map(p => tools.grep({path: p, query: "TODO|FIXME"}))
);
const worst = hits
  .flatMap(h => h.data.matches)
  .filter(m => m.line < 50)
  .slice(0, 20);
await tools.write_file({path: "debt.md", content: worst.map(m => "- " + m.path).join("\n")});
return {found: worst.length};
```

Two things follow. Independent calls actually run at the same time, because `Promise.all` is a real thing and "emit two tool calls and hope the runtime parallelizes them" is not. And the intermediate results stay in the process — the model sees `{found: 20}`, not the four hundred lines it read to get there.

The registry is the point. There is no protocol boundary in the middle: a `Binding` is a name and a `func(ctx, argsJSON) (string, error)`, so anything your Go code can call is a tool a program can call.

Mounting it is additive. Your tools stay in the model's tool list and keep working as direct calls; the model gains one more tool, and with it a second way to reach the same capabilities. The generated description says that as a set relation — *the tools callable in a program are exactly those in your current tool list, minus this one and minus what you blocked* — so nothing re-lists schemas the model already has, and adding a tool later costs nothing here. What that leaves on you: `Bindings` has to match what you actually mounted, because nothing in this library can see your tool list.

`ConflictKeys` is what keeps a mixed batch safe: bindings declare which resource each call touches, so reads of different files still overlap while a write and a read of the same file serialize. Your runtime is the only thing that can know that — the engine has no schema for your arguments. See [scheduling](#scheduling) for the rules.

One thing to know before wiring it in: the program runs in-process on goja and sees no filesystem, network or imports, but that is capability omission rather than isolation. This is not a security sandbox and is not meant for untrusted or multi-tenant programs; [what a program can see](#what-a-program-can-see) says exactly where the line falls.

Pulled out of an agent that has been running it in production.

## What is code mode?

Code mode gives the model one tool that runs a program, and exposes the rest of its tools as an API that program can call. It goes by several names: [Anthropic](https://www.anthropic.com/engineering/code-execution-with-mcp) calls it code execution with MCP, [Cloudflare](https://blog.cloudflare.com/code-mode/) calls it Code Mode, the [CodeAct paper](https://arxiv.org/abs/2402.01030) calls it code-as-action.

Where implementations differ is what code execution is *for*. Many make it the primary path to tools — the model gets one `execute_code` and the rest are reached through it. This one is deliberately additive: it is a library you embed, your tool registry stays the source of truth, and the program tool only projects it. The model keeps every tool it had and gains a second way to reach the same capabilities. See [how it compares](#how-it-compares).

## Why

**Context.** Intermediate results stay in the process. A sweep over fifty search hits costs one tool result instead of fifty, and the next turn does not carry the forty-five that turned out to be irrelevant. Anthropic's writeup puts one such case at 150,000 tokens down to 2,000.

**Parallelism you actually get.** `Promise.all` puts calls into a real worker pool. Waiting for the model to emit another batch of tool calls does not.

**Logic the model would otherwise narrate.** Filtering, scoring, joining two sources on a key — a program does it in four lines instead of a paragraph of reasoning over data that had to be pasted into the context first.

It is built to sit alongside direct tool calls rather than replace them. Most turns are one or two calls, where a program is pure overhead; the tool description tells the model when a program is worth it.

## Install

```bash
go get github.com/gtoxlili/codemode-go
```

Go 1.25+. The core is pure Go on the [goja](https://github.com/dop251/goja) stack — goja itself plus `goja_nodejs` for its event loop, and nothing beyond those two. No cgo, no Node, no Deno, no subprocess, no container.

## Use

Three steps. Bind your tools, mount the tool, teach the model.

```go
import "github.com/gtoxlili/codemode-go"

fileKey := func(args string) []string { return []string{"file:" + pathOf(args)} }

bindings := []codemode.Binding{{
    Name:         "read_file",
    Invoke:       readFile, // func(ctx context.Context, argsJSON string) (string, error)
    ConflictKeys: fileKey,
}, {
    Name:         "write_file",
    Invoke:       writeFile,
    ConflictKeys: fileKey,
    Mutating:     true,
}}

tool, err := codemode.NewTool(codemode.Options{
    Bindings: bindings,
    Blocked: []codemode.Blocked{{
        Name:   "ask_user",
        Reason: "ask_user ends the turn — ask before you start the program",
    }},
})
```

`NewTool` fails on a name appearing twice across `Bindings` and `Blocked`. The engine keeps the first binding under a name and drops the rest, so a duplicate would silently decide which tool a program reaches, and a name in both lists would have the description call it uncallable while the binding underneath still ran.

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

### JSON and logging

Both default to the standard library and need nothing from you. Replace them when your runtime has already made the decision elsewhere:

```go
codemode.Options{
    Bindings: bindings,
    Codec:    sonicCodec{},   // Marshal + Unmarshal, encoding/json's signatures
    Logger:   func(ctx context.Context, msg string, attrs ...any) {
        myLogger.FromContext(ctx).Warn(msg, attrs...)
    },
}
```

`Run` takes the same two as trailing options — `codemode.WithCodec(c)`, `codemode.WithLogger(l)` — so an agent calling the engine directly is not forced through `NewTool`.

The reason to pass a codec is agreement, not speed. Your tools serialize their results with something; the engine decodes those results before a program sees them. If the two disagree — on how a large integer survives a round trip, on unknown fields, on invalid UTF-8 — then `r.data.x` inside a program is not quite the value the model would have read from a direct call. Handing the engine the codec your tools already use removes that gap. A codec that also has `UnmarshalString(string, any) error` gets used through it: tool results arrive as strings, and the fallback copies each one into a fresh `[]byte` before parsing.

`Logger` receives internal warnings — today there is exactly one, a tool's `ConflictKeys` panicking, which is reported rather than raised because it is a bug in the tool and not in the program. It takes a context because the warning belongs to one run inside one request, and a host that correlates logs by trace or tenant reads that from there.

### Return-shape hints

Tool protocols ship the input schema and say nothing about the output, so a program navigating `r.data.hits` is working from a guess. `ReturnShape` derives a compact hint from a Go result type, meant for the tail of a tool description:

```go
desc += "\n\nReturns `{data: " + codemode.ReturnShape[SearchResult]() + "}`."
// Returns `{data: {query, hits: [{path, line: num, snippet}], truncated: bool, cursor?}}`.
```

A bare name is a string, other types are annotated, `?` marks an omitempty field. `ReturnShapes: true` in `PromptOptions` adds the matching notation guide to the prompt section; with it false the section says nothing about return shapes.

## Adapters

Separate modules, so framework dependencies never enter the core's graph.

### eino

```bash
go get github.com/gtoxlili/codemode-go/adapters/eino
```

```go
bindings, err := einocodemode.Bindings(ctx, myTools)
ct, err := codemode.NewTool(codemode.Options{Bindings: bindings})
myTools = append(myTools, einocodemode.NewTool(ct))
```

[eino](https://github.com/cloudwego/eino) tools carry no notion of writing or of which resource they touch, so the bindings come back with `Mutating` false and `ConflictKeys` nil and every call schedules as conflict-free. The returned slice is plain and can be edited before it is passed on.

### MCP

```bash
go get github.com/gtoxlili/codemode-go/adapters/mcp
```

An agent's tools do not all have to be its own. If some of them come from MCP servers your runtime dials, this binds those too:

```go
discovered, err := mcpcodemode.Tools(ctx, mcpClient)
bindings = append(bindings, mcpcodemode.Bindings(discovered)...)
```

That is the ordinary wiring, and it stays additive: the MCP tools are in the model's tool list, still directly callable, and now also reachable from a program.

There is a second wiring for the case where sixty MCP tools in the tool list cost sixty schemas per request and most of them are cold. Take them out of the tool list, and hand the model `Catalog(discovered)` in the program tool's description instead — a text listing costs far less than sixty tool definitions, at the price of those tools no longer being directly callable:

```go
ct, err := codemode.NewTool(codemode.Options{
    Bindings:    bindings,
    Description: intro + "\n\n" + mcpcodemode.Catalog(discovered),
})
```

An overriding description is needed there because the generated one states that a program can call the tools in the model's tool list, which stops being true once they are out of it.

Works with [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go) clients over any transport: stdio, SSE, streamable HTTP, in-process.

## How it compares

Code mode implementations get compared as one group when they sit at different levels. The level decides whether a project is an alternative to this one or a different thing entirely.

**A library you embed, projecting the host's own tools.** Same abstraction boundary as this: you hand it a name, a schema and a handler, and it hands the model a programmable API. Your agent loop stays yours.

| Project | Host language | Model writes | Runs in | Tools arrive as |
|---|---|---|---|---|
| **codemode-go** | Go | JavaScript | goja, in-process | `Binding{Name, Invoke}` → `tools.name(args)` |
| [tool-sandbox](https://github.com/domdomegg/tool-sandbox) | TypeScript | JavaScript | WASM | `Tool{name, inputSchema, handler}` → `tool(name, args)` |

**An agent framework or runtime with code mode built in.** Same idea, but it owns the loop: adopting one means adopting its agent model.

| Project | Host language | Model writes | Runs in |
|---|---|---|---|
| [deepseek-harness Code Mode](https://github.com/deepseek-ai/deepseek-harness) | TypeScript | TypeScript | one fresh Node worker per run |
| [Microsoft Agent Framework CodeAct](https://github.com/microsoft/agent-framework/blob/main/docs/decisions/0024-codeact-integration.md) | Python, .NET | Python | Hyperlight, backend left pluggable |
| [smolagents CodeAgent](https://github.com/huggingface/smolagents) | Python | Python | restricted interpreter, or E2B / Docker |
| [strands-code-agent](https://github.com/aws-samples/sample-strands-code-agent) | Python | Python | persistent REPL, three backends |

The tables are not a survey. They are the projects at the same abstraction boundary, which is the only place a comparison means anything. Standalone servers that sit in front of a pile of MCP servers answer a different question and are not alternatives to this one; see the [FAQ](#is-this-an-mcp-server-i-can-add-to-claude-code) for why the shapes do not swap.

**What it is good at.** Pure Go, one process, no protocol boundary between the program and your tools, and nothing imposed on your agent loop. It models resource conflicts explicitly rather than treating every concurrent call as independent: `Mutating` and `ConflictKeys` are how a host says *this call writes file X*, and that is knowledge only the host has. What else comes in the box: the system-prompt section, a failure taxonomy phrased for the model to correct from, and hooks around every sub-call.

Starting a run costs one goja VM, which is microseconds — so when a program is the wrong choice for a two-call task, that is about the tokens spent writing it, not the runtime.

**What it is not good at.** It is not a security boundary — see below, and if you need one, the WASM and container-backed options above are genuinely stronger. The model writes JavaScript, not Go, so your tools are reachable only through the bindings you pass; there is no way to hand a program a Go value directly. There is no typed SDK generation, so the model works from your tool descriptions rather than from TypeScript types with autocomplete — `ReturnShape` narrows that gap but does not close it. And a program shares the host process's memory, which is why the memory limit is a trip line with a documented margin of error rather than a hard ceiling.

## What a program can see

| Name | What it is |
|---|---|
| `tools` | `tools.name(args)` returns a promise resolving to that tool's result, parsed if it is JSON |
| `ToolCallError` | what a failed call rejects with, carrying `.toolName`; catch it and continue |
| `console` | log/info/warn/error/debug, all into one log channel |
| `sleep(ms)` | the only way to wait; the `setTimeout` family does not exist |

Plus the JavaScript built-ins. No filesystem, no network, no `require`, no `process`, no `fetch`. Every external effect goes through a binding you passed in.

Capability omission is the real boundary here. The rest, stated plainly: **this is not a security sandbox.** The program runs in-process on goja, which has no per-VM memory ceiling and no instruction counter, so the wall clock, the compute budget and the memory trip line are sampled every 50ms and enforced with goja's `Interrupt`.

Sampling sets how precisely those fire, not how soon. An allocation bomb crosses the memory line almost at once — a doubling string dies in about 200ms under the default 256MB — while a loop that only computes runs until its compute budget is gone, two minutes by default. Both of those are what a model writes by accident. Neither stops someone who controls the program and is trying.

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

All overridable through `Options.Limits`. Zero means "use the default", which is right for a partially filled struct and wrong for values that came out of a config file, where a `wall_clock` of zero silently becomes ten minutes; `Validate()` is there to report those.

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

**Serialization needs a key from both calls.** A call with no keys conflicts with nothing, so declaring them on the writers alone buys nothing — a write and a read of the same file still overlap unless the reader names that file too. Every tool that touches a resource has to say so, not just the ones that change it.

Conflicts are pruned as soon as a call finishes, since two calls can only conflict while they overlap. Without that, writing a digest and then reading a batch of files back — including the one just written — would serialize the whole read fan-out behind a write that is already done.

## FAQ

### Is this an MCP server I can add to Claude Code?

No. It is a library for the agent you are building, not a server you register with someone else's client.

That form does exist elsewhere, and it is structurally limited in a way worth knowing about: MCP is asymmetric. A server exposes tools; a client exposes roots, sampling and elicitation. Nothing in the protocol lets a server call back into the client, so a gateway can never touch Claude Code's Read, Edit or Bash — it can project only the capabilities it dials itself. That is the part no deployment choice works around, and it is what makes a gateway the wrong shape for making a runtime's own primitives programmable. If a pile of MCP tools is your whole problem, a gateway is a reasonable answer.

### Do my tools leave the model's tool list?

No. Mounting this adds one tool and changes nothing else; every tool the model had stays mounted and directly callable, and the program tool simply says that those same tools are also reachable from a program. Taking tools out of the tool list is a separate thing you can choose to do with the MCP adapter's `Catalog`, and it is a trade — cheaper per turn, no longer directly callable.

### Is this a security sandbox?

No. Capability omission is real — a program has no filesystem, network, or imports, and every effect goes through a binding that was passed in. The resource limits are a different story: they are sampled trip lines that catch what a model writes by accident, not a boundary against an adversary.

### Why JavaScript instead of Go?

Because the model writes it and goja runs it in-process with no cgo, no subprocess, and no container. Compiling and running model-written Go means a toolchain, a build step, and real isolation work before anything runs at all. Models also write far more JavaScript than they write Go, which shows up directly in how often the program is correct on the first attempt.

### Does this replace normal tool calling?

No. It is designed to coexist with direct tool calls: most turns are one or two calls, where a program is pure overhead, and the tool description describes when a program is worth it so the model can choose.

### How much context does it actually save?

It depends entirely on how much data your tools return and how much of it the answer needs. The saving is the part the program throws away — fan out over fifty results and return five, and the other forty-five never enter the conversation. Anthropic's writeup measures one such case at 150,000 tokens down to 2,000. A program that fetches one thing and returns it saves nothing.

### Do I need MCP?

No. MCP is one source of tools, and there is an adapter for it. A `Binding` is just a name and a `func(ctx, argsJSON) (string, error)`, so anything you can call from Go can be a tool.

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
