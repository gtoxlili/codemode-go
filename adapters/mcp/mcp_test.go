package mcpcodemode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gtoxlili/codemode-go"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func testClient(t *testing.T) *client.Client {
	t.Helper()
	s := server.NewMCPServer("test", "0.0.1", server.WithToolCapabilities(false))

	s.AddTool(
		mcp.NewTool("score",
			mcp.WithDescription("Score one candidate"),
			mcp.WithNumber("id", mcp.Required(), mcp.Description("candidate id")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id, err := req.RequireInt("id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf(`{"id":%d,"score":%d}`, id, id*10)), nil
		},
	)
	s.AddTool(
		mcp.NewTool("always_fails", mcp.WithDescription("Always returns an error")),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultError("upstream is down"), nil
		},
	)
	// Hyphens are ordinary in MCP tool names and illegal after a dot in JS.
	s.AddTool(
		mcp.NewTool("get-issue", mcp.WithDescription("Fetch an issue")),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(`{"issue":7}`), nil
		},
	)

	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Initialize(context.Background(), mcp.InitializeRequest{}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return c
}

func TestProgramFansOutOverMCPTools(t *testing.T) {
	ctx := context.Background()
	discovered, err := Tools(ctx, testClient(t))
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if len(discovered) != 3 {
		t.Fatalf("discovered %d tools", len(discovered))
	}

	ct := codemode.NewTool(codemode.Options{Bindings: Bindings(discovered)})
	out, err := ct.Call(ctx, `{"code":"const rs = await Promise.all([1,2,3].map(id => tools.score({id}))); return rs.map(r => r.score);","description":"Score three candidates"}`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got struct {
		Result []int `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("result is not JSON: %v (%s)", err, out)
	}
	if len(got.Result) != 3 || got.Result[2] != 30 {
		t.Fatalf("got %v", got.Result)
	}
}

// An MCP tool that reports isError must reach the program as a catchable
// ToolCallError, not as a successful result whose text happens to say "error".
func TestToolErrorsRejectTheCall(t *testing.T) {
	ctx := context.Background()
	discovered, err := Tools(ctx, testClient(t))
	if err != nil {
		t.Fatalf("tools: %v", err)
	}

	ct := codemode.NewTool(codemode.Options{Bindings: Bindings(discovered)})
	out, err := ct.Call(ctx, `{"code":"try { await tools.always_fails({}); } catch (e) { return {tool: e.toolName, msg: e.message}; } return 'not reached';","description":"Catch a failure"}`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(out, "always_fails") || !strings.Contains(out, "upstream is down") {
		t.Fatalf("got %s", out)
	}
}

func TestCatalogCarriesNamesDescriptionsAndSchemas(t *testing.T) {
	discovered, err := Tools(context.Background(), testClient(t))
	if err != nil {
		t.Fatalf("tools: %v", err)
	}

	cat := Catalog(discovered)
	for _, want := range []string{"## score", "Score one candidate", `"id"`, "## always_fails"} {
		if !strings.Contains(cat, want) {
			t.Errorf("catalog is missing %q:\n%s", want, cat)
		}
	}
	// The catalog lists the name a program writes, not the name on the wire.
	if !strings.Contains(cat, "## get_issue") || strings.Contains(cat, "## get-issue") {
		t.Errorf("catalog should list the callable identifier:\n%s", cat)
	}
}

// A hyphenated MCP tool name has to reach the program as an identifier, and the
// call still has to go out under the name the server registered.
func TestHyphenatedToolNamesAreCallable(t *testing.T) {
	ctx := context.Background()
	discovered, err := Tools(ctx, testClient(t))
	if err != nil {
		t.Fatalf("tools: %v", err)
	}

	ct := codemode.NewTool(codemode.Options{Bindings: Bindings(discovered)})
	out, err := ct.Call(ctx, `{"code":"const r = await tools.get_issue({}); return r.issue;","description":"Fetch one issue"}`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(out, `"result":7`) {
		t.Fatalf("got %s", out)
	}
}

func TestJSIdent(t *testing.T) {
	for in, want := range map[string]string{
		"get-issue":       "get_issue",
		"score":           "score",
		"a.b/c":           "a_b_c",
		"2fast":           "_2fast",
		"":                "_",
		"already_fine$42": "already_fine$42",
	} {
		if got := JSIdent(in); got != want {
			t.Errorf("JSIdent(%q) = %q, want %q", in, got, want)
		}
	}
}
