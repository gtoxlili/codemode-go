package codemode

import "strings"

// PromptOptions shapes the section [Prompt] returns.
type PromptOptions struct {
	// ToolName defaults to [DefaultToolName]. Pass whatever you passed to
	// [NewTool].
	ToolName string

	// ReturnShapes states whether the tool descriptions in this deployment end
	// with a line giving the shape of what the tool returns, in the notation
	// [ReturnShape] produces. When true, the section adds how to read those
	// lines; when false it says nothing about them.
	ReturnShapes bool
}

// Prompt returns the system-prompt section for the tool: how to call tools from
// a program, what a call resolves to, what a failed call rejects with, what runs
// in parallel, and that only the return value and console output come back.
//
// It is written in the second person and formatted as a "# Heading" block, the
// usual shape of an agent system prompt section.
func Prompt(opts PromptOptions) string {
	name := opts.ToolName
	if name == "" {
		name = DefaultToolName
	}

	envelope := "- Every call resolves to the same JSON result a direct call returns — navigate it in code (`r.data.hits`) and feed the extracted fields straight into the next call."
	if opts.ReturnShapes {
		envelope = "- Every call resolves to the same JSON envelope a direct call returns; the `Returns` line ending a tool's description gives its shape — navigate it in code (`r.data.hits`) and feed the extracted fields straight into the next call. In these shapes a bare field is a string; other value types are annotated (`line: num`, `truncated: bool`, `…` = unknown), and `?` marks a field that may be absent."
	}

	var b strings.Builder
	b.WriteString("# Batch Orchestration\n\n")
	b.WriteString("`" + name + "` runs one JavaScript program that orchestrates your tools in batch: call tools inside it with `await tools.name(args)` (args: one object per the tool's parameter schema), fan out independent calls with `Promise.all`, and merge, filter, or score the results with plain JavaScript.\n\n")
	b.WriteString(envelope + " A failed call rejects with `ToolCallError` carrying a `toolName` property; try/catch it to handle the failure and continue.\n")
	b.WriteString("- Independent read-only calls run in parallel; calls that write the same resource run one at a time, in submission order. A call fired without `await` still runs — its failure surfaces as an `[unhandled rejection]` log line.\n")
	b.WriteString("- The environment is plain JavaScript. Beyond the language built-ins exactly four names exist: `tools`, `ToolCallError`, `console` (log/info/warn/error/debug — all land in the same log channel), and `await sleep(ms)` — the one way to wait (backoff, pacing; the `setTimeout` family does not exist). No filesystem, network, or import — every external effect goes through `tools`.\n")
	b.WriteString("- Only what the program `return`s or `console.log`s comes back to you — intermediate tool results never enter the conversation, so put what the next step needs into the return value or a few log lines.\n\n")
	b.WriteString("Chaining is the point — results are ordinary JSON values you can transform between calls:\n\n")
	b.WriteString("```js\n")
	b.WriteString("const found = await tools.search_files({query: \"slogan|tagline\"});\n")
	b.WriteString("const top = found.data.hits.slice(0, 20);              // merge / filter / score in code\n")
	b.WriteString("await tools.write_file({path: \"voice-digest.md\",\n")
	b.WriteString("  content: top.map(h => \"- \" + h.path + \":\" + h.line + \" \" + h.snippet).join(\"\\n\")});\n")
	b.WriteString("return {digestEntries: top.length};\n")
	b.WriteString("```")
	return b.String()
}
