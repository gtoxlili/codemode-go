package codemode

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestToolDescriptionListsWhatIsNotCallable(t *testing.T) {
	tool := NewTool(Options{
		Bindings: []Binding{echoBinding("search", "x")},
		Blocked:  []Blocked{{Name: "ask_user"}, {Name: "execute_shell", Reason: "call it directly"}},
	})

	desc := tool.Description()
	// The claim is exact — "your tool list minus this list" — and the tool is in
	// the model's list while being uncallable from a program, so it has to lead.
	if !strings.Contains(desc, "minus: run_code, ask_user, execute_shell.") {
		t.Fatalf("description = %q", desc)
	}
	if tool.Name() != DefaultToolName {
		t.Fatalf("name = %q", tool.Name())
	}
}

func TestToolBlockedCallsReturnGuidance(t *testing.T) {
	tool := NewTool(Options{
		Bindings: []Binding{echoBinding("search", "x")},
		Blocked:  []Blocked{{Name: "execute_shell", Reason: "call execute_shell directly; batch shell work is one call with a shell script"}},
		Limits:   fastLimits(),
	})

	res, failure := tool.Run(context.Background(), `
try { await tools.execute_shell({cmd: "ls"}); } catch (e) { return e.message; }
return "not reached";
`)
	if failure != nil {
		t.Fatalf("unexpected failure: %v", failure)
	}
	if !strings.Contains(res.Result.(string), "batch shell work is one call") {
		t.Fatalf("a blocked tool should explain itself, got %v", res.Result)
	}
}

func TestToolCallReturnsLogsAndResult(t *testing.T) {
	tool := NewTool(Options{Bindings: []Binding{echoBinding("echo", "42")}, Limits: fastLimits()})
	args := `{"code":"console.log('hi'); const r = await tools.echo({}); return r.echo;","description":"Echo once"}`

	out, err := tool.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got struct {
		Logs   []string `json:"logs"`
		Result string   `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("result is not JSON: %v (%s)", err, out)
	}
	if len(got.Logs) != 1 || got.Logs[0] != "hi" || got.Result != "42" {
		t.Fatalf("got %+v", got)
	}
}

func TestToolCallAttachesOutputToFailures(t *testing.T) {
	tool := NewTool(Options{Limits: fastLimits()})
	args := `{"code":"console.log('made it this far'); throw new Error('kaboom');","description":"Fail"}`

	_, err := tool.Call(context.Background(), args)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exception") || !strings.Contains(msg, "kaboom") {
		t.Fatalf("the failure should name its kind and cause: %s", msg)
	}
	if !strings.Contains(msg, "made it this far") {
		t.Fatalf("the failure should carry the captured output: %s", msg)
	}
}

func TestToolCallRejectsEmptyCode(t *testing.T) {
	tool := NewTool(Options{Limits: fastLimits()})
	if _, err := tool.Call(context.Background(), `{"code":"  ","description":"nothing"}`); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := tool.Call(context.Background(), `not json`); err == nil {
		t.Fatal("expected an error")
	}
}

func TestToolOnProgramSeesTheDescription(t *testing.T) {
	var gotCode, gotDesc string
	tool := NewTool(Options{
		Limits: fastLimits(),
		OnProgram: func(_ context.Context, code, description string) {
			gotCode, gotDesc = code, description
		},
	})
	if _, err := tool.Call(context.Background(), `{"code":"return 1;","description":"Add one number"}`); err != nil {
		t.Fatalf("call: %v", err)
	}
	if gotCode != "return 1;" || gotDesc != "Add one number" {
		t.Fatalf("got %q / %q", gotCode, gotDesc)
	}
}

func TestToolBoundsConcurrentRuns(t *testing.T) {
	var mu sync.Mutex
	var cur, peak int
	tool := NewTool(Options{
		MaxConcurrentRuns: 2,
		Limits:            fastLimits(),
		Bindings: []Binding{{Name: "work", Invoke: func(context.Context, string) (string, error) {
			mu.Lock()
			cur++
			if cur > peak {
				peak = cur
			}
			mu.Unlock()
			time.Sleep(60 * time.Millisecond)
			mu.Lock()
			cur--
			mu.Unlock()
			return `{"ok":true}`, nil
		}}},
	})

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tool.Run(context.Background(), `await tools.work({}); return "ok";`)
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if peak > 2 {
		t.Fatalf("at most 2 programs should run at once, saw %d", peak)
	}
}

func TestToolParametersAreValidJSONSchema(t *testing.T) {
	tool := NewTool(Options{})
	var schema map[string]any
	if err := json.Unmarshal(tool.ParametersJSON(), &schema); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	props := schema["properties"].(map[string]any)
	if _, ok := props["code"]; !ok {
		t.Fatal("schema is missing code")
	}
	if _, ok := props["description"]; !ok {
		t.Fatal("schema is missing description")
	}
	// Parameters hands out a fresh map every time; mutating it must not leak.
	p := tool.Parameters()
	delete(p, "properties")
	if _, ok := tool.Parameters()["properties"]; !ok {
		t.Fatal("Parameters returned shared state")
	}
}

func TestPromptTracksToolNameAndShapes(t *testing.T) {
	p := Prompt(PromptOptions{})
	if !strings.Contains(p, "`run_code` runs one JavaScript program") {
		t.Fatalf("prompt = %q", p)
	}
	if strings.Contains(p, "`Returns` line") {
		t.Fatal("the Returns convention must not be promised unless it is enabled")
	}

	p = Prompt(PromptOptions{ToolName: "batch", ReturnShapes: true})
	if !strings.Contains(p, "`batch` runs one JavaScript program") {
		t.Fatalf("prompt = %q", p)
	}
	if !strings.Contains(p, "`Returns` line") {
		t.Fatal("ReturnShapes should teach the Returns convention")
	}
}
