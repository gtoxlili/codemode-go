// Package codemode lets an LLM agent orchestrate its own tools from a
// JavaScript program instead of one tool call per turn.
//
// The model submits a program as a single tool call. Inside it, `await
// tools.name(args)` calls the tools the host bound, `Promise.all` fans them out
// in parallel, and ordinary JavaScript merges, filters and scores the results.
// Only what the program returns or console.logs goes back into the
// conversation — the intermediate results stay in the process.
//
// Two things come out of that. Independent calls actually run concurrently
// instead of waiting for the model to emit another batch, and a fan-out over
// fifty search hits costs one tool result instead of fifty.
//
// # Layers
//
// [Run] is the engine: a program, a set of [Binding]s, a set of [Limits]. It
// knows nothing about agents, prompts or tool schemas.
//
// [NewTool] wraps the engine into a tool you can hand to a model: a name, a
// description that states which tools a program may call, a JSON Schema for the
// arguments, and a Call method taking the raw argument JSON. Framework adapters
// live in ./adapters and are separate modules.
//
// [Prompt] returns the system-prompt section describing how to write a program
// against the bound tools.
//
// # Sandboxing
//
// The program runs in-process on goja. Capability omission is the real
// boundary: a program sees `tools`, `ToolCallError`, `console`, `sleep` and the
// JavaScript built-ins, and nothing else — no filesystem, no network, no
// require, no process. Every external effect goes through a binding the host
// chose to pass in.
//
// The resource limits are tripwires, not a hard sandbox. goja has no per-VM
// memory ceiling and no instruction counter, so the wall clock, the compute
// budget and the memory trip line are enforced by a 50ms sampler and goja's
// Interrupt. They catch hot loops and allocation bombs in well under a second.
// They do not make the VM safe against an adversary who controls the program
// and is willing to spend real effort on it.
package codemode
