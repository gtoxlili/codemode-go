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
	if len(discovered) != 2 {
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
}
