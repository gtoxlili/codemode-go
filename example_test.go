package codemode_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gtoxlili/codemode-go"
)

// A program calls tools, merges what comes back, and returns a digest. The
// nineteen candidates it looked at and threw away never reach the model.
func Example() {
	fetch := codemode.Binding{
		Name: "fetch_profile",
		Invoke: func(_ context.Context, args string) (string, error) {
			var a struct {
				Handle string `json:"handle"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return "", err
			}
			return fmt.Sprintf(`{"handle":%q,"followers":%d}`, a.Handle, len(a.Handle)*1000), nil
		},
	}

	res, failure := codemode.Run(context.Background(), `
const handles = ["ada", "grace", "barbara"];
const profiles = await Promise.all(handles.map(h => tools.fetch_profile({handle: h})));
const top = profiles.sort((a, b) => b.followers - a.followers)[0];
console.log("checked " + profiles.length + " profiles");
return {winner: top.handle, followers: top.followers};
`, []codemode.Binding{fetch}, codemode.DefaultLimits())

	if failure != nil {
		fmt.Println("failed:", failure)
		return
	}
	out, _ := json.Marshal(res.Result)
	fmt.Println(res.Logs[0])
	fmt.Println(string(out))
	// Output:
	// checked 3 profiles
	// {"followers":7000,"winner":"barbara"}
}

// A failed tool call rejects with a ToolCallError the program can catch, so one
// dead source does not throw away the work already done.
func Example_partialFailure() {
	ok := codemode.Binding{
		Name:   "search_docs",
		Invoke: func(context.Context, string) (string, error) { return `{"hits":["a","b"]}`, nil },
	}
	broken := codemode.Binding{
		Name:   "search_web",
		Invoke: func(context.Context, string) (string, error) { return "", fmt.Errorf("rate limited (429)") },
	}

	res, failure := codemode.Run(context.Background(), `
const out = {hits: [], failed: []};
for (const name of ["search_docs", "search_web"]) {
  try {
    const r = await tools[name]({query: "changelog"});
    out.hits.push(...r.hits);
  } catch (e) {
    if (!(e instanceof ToolCallError)) throw e;
    out.failed.push(e.toolName);
  }
}
return out;
`, []codemode.Binding{ok, broken}, codemode.DefaultLimits())

	if failure != nil {
		fmt.Println("failed:", failure)
		return
	}
	out, _ := json.Marshal(res.Result)
	fmt.Println(string(out))
	// Output:
	// {"failed":["search_web"],"hits":["a","b"]}
}

// The tool form: a name, a description, an argument schema, and a Call that
// takes the model's raw argument JSON. Hand those to whatever tool-calling loop
// you already have.
func ExampleNewTool() {
	tool, err := codemode.NewTool(codemode.Options{
		Bindings: []codemode.Binding{{
			Name:   "count_words",
			Invoke: func(context.Context, string) (string, error) { return `{"count":42}`, nil },
		}},
		Blocked: []codemode.Blocked{{
			Name:   "ask_user",
			Reason: "ask_user ends the turn, so a program cannot call it — ask before you start the program",
		}},
	})
	if err != nil {
		panic(err) // a name in both Bindings and Blocked, or listed twice
	}

	fmt.Println(tool.Name())
	fmt.Println(tool.Description())

	out, err := tool.Call(context.Background(),
		`{"code":"const r = await tools.count_words({path:'a.md'}); return r.count * 2;","description":"Double a word count"}`)
	fmt.Println(out, err)
	// Output:
	// run_code
	// Runs one JavaScript program that orchestrates your tools in batch; only what the program prints or returns comes back to the conversation. The tools callable in a program are exactly those in your current tool list, minus: run_code, ask_user. Reach for it when a batch of calls collapses into one digest — parallel fan-out, chained transforms, filtering bulk results down to what matters; a lone call is cheaper made directly.
	// {"logs":[],"result":84} <nil>
}

type searchHit struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Snippet string `json:"snippet"`
}

type searchResults struct {
	Query     string      `json:"query"`
	Hits      []searchHit `json:"hits"`
	Truncated bool        `json:"truncated"`
	Cursor    string      `json:"cursor,omitempty"`
}

// Append the shape of what a tool returns to its description, so the model
// navigates results instead of guessing field names five calls deep.
func ExampleReturnShape() {
	fmt.Println("Returns `{data: " + codemode.ReturnShape[searchResults]() + "}`.")
	// Output:
	// Returns `{data: {query, hits: [{path, line: num, snippet}], truncated: bool, cursor?}}`.
}
