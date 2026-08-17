// Package mcpcodemode turns the tools of an MCP server into tools a codemode
// program can call.
//
// The wiring when the server's tools are already in the model's tool list:
//
//	discovered, err := mcpcodemode.Tools(ctx, mcpClient)
//	ct := codemode.NewTool(codemode.Options{Bindings: mcpcodemode.Bindings(discovered)})
//
// The wiring when they are not, where the per-turn cost is one tool description
// rather than one schema per MCP tool:
//
//	catalog := mcpcodemode.Catalog(discovered)
//	ct := codemode.NewTool(codemode.Options{
//	    Bindings:    mcpcodemode.Bindings(discovered),
//	    Description: "Runs one JavaScript program that orchestrates the tools below with `await tools.name(args)`; " +
//	        "only what the program prints or returns comes back to the conversation.\n\n" + catalog,
//	})
//
// The second form supplies its own Description because the generated one states
// that a program can call the tools in the model's tool list, which does not
// hold once they are out of it.
package mcpcodemode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gtoxlili/codemode-go"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// Tool is one tool of an MCP server: its declaration, and a binding that calls
// it.
type Tool struct {
	mcp.Tool
	Binding codemode.Binding
}

// Tools lists the server's tools and pairs each with a binding. The client must
// already be initialized.
func Tools(ctx context.Context, c *client.Client) ([]Tool, error) {
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("listing MCP tools: %w", err)
	}
	out := make([]Tool, 0, len(res.Tools))
	for _, t := range res.Tools {
		name := t.Name
		out = append(out, Tool{
			Tool: t,
			Binding: codemode.Binding{
				Name: name,
				Invoke: func(ctx context.Context, args string) (string, error) {
					return call(ctx, c, name, args)
				},
			},
		})
	}
	return out, nil
}

// Bindings pulls the bindings out, for [codemode.Options].
func Bindings(ts []Tool) []codemode.Binding {
	out := make([]codemode.Binding, len(ts))
	for i, t := range ts {
		out[i] = t.Binding
	}
	return out
}

func call(ctx context.Context, c *client.Client, name, args string) (string, error) {
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	if strings.TrimSpace(args) != "" {
		req.Params.Arguments = json.RawMessage(args)
	}

	res, err := c.CallTool(ctx, req)
	if err != nil {
		return "", err
	}

	// Prefer structured content: it is already JSON, so the program navigates
	// fields instead of parsing prose.
	if len(res.RawStructuredContent) > 0 {
		if res.IsError {
			return "", fmt.Errorf("%s", res.RawStructuredContent)
		}
		return string(res.RawStructuredContent), nil
	}
	if res.StructuredContent != nil {
		b, mErr := json.Marshal(res.StructuredContent)
		if mErr == nil {
			if res.IsError {
				return "", fmt.Errorf("%s", b)
			}
			return string(b), nil
		}
	}

	// Otherwise flatten the content blocks. Non-text blocks (images, audio,
	// embedded resources) are reported rather than dropped: a program cannot do
	// anything with the bytes, and pretending the result was empty would send
	// the model looking for a bug that is not there.
	var sb strings.Builder
	for _, item := range res.Content {
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		switch v := item.(type) {
		case mcp.TextContent:
			sb.WriteString(v.Text)
		case *mcp.TextContent:
			sb.WriteString(v.Text)
		default:
			fmt.Fprintf(&sb, "[%T is not readable from a program; call %s directly]", item, name)
		}
	}
	if res.IsError {
		return "", fmt.Errorf("%s", sb.String())
	}
	return sb.String(), nil
}

// Catalog renders the tools as a text block for the run_code description or the
// system prompt: one entry per tool, with its description and its input schema.
// It is what a model writes calls against when the tools are not in its tool
// list.
func Catalog(ts []Tool) string {
	var sb strings.Builder
	sb.WriteString("Tools callable from a program, as `await tools.name(args)`:\n")
	for _, t := range ts {
		fmt.Fprintf(&sb, "\n## %s\n", t.Name)
		if d := strings.TrimSpace(t.Description); d != "" {
			sb.WriteString(d + "\n")
		}
		if schema := inputSchema(t.Tool); schema != "" {
			sb.WriteString("args: " + schema + "\n")
		}
	}
	return sb.String()
}

func inputSchema(t mcp.Tool) string {
	if len(t.RawInputSchema) > 0 {
		return string(t.RawInputSchema)
	}
	b, err := json.Marshal(t.InputSchema)
	if err != nil {
		return ""
	}
	return string(b)
}
