// Package einocodemode wires codemode into CloudWeGo eino.
//
// Two functions, used together:
//
//	bindings, err := einocodemode.Bindings(ctx, myTools)
//	ct, err := codemode.NewTool(codemode.Options{Bindings: bindings})
//	myTools = append(myTools, einocodemode.NewTool(ct))
//
// eino tools carry no notion of writing or of which resource they touch, so the
// bindings come back with Mutating false and ConflictKeys nil, and the scheduler
// treats every call as conflict-free. The slice is plain and can be edited
// before it is passed on:
//
//	for i := range bindings {
//	    if bindings[i].Name == "write_file" {
//	        bindings[i].Mutating = true
//	        bindings[i].ConflictKeys = pathKey
//	    }
//	}
package einocodemode

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
	"github.com/gtoxlili/codemode-go"
)

// Bindings converts eino tools into codemode bindings. Both sides speak "JSON
// string in, string out", so nothing is translated.
//
// A tool that is not an [tool.InvokableTool] is reported as an error rather
// than dropped, since a binding that is silently absent surfaces to the model
// as "unknown tool". eino's enhanced (multimodal) tool interfaces are not
// invokable in this sense either: their results carry image, audio and file
// content, which does not survive the string result a binding returns.
func Bindings(ctx context.Context, ts []tool.BaseTool) ([]codemode.Binding, error) {
	out := make([]codemode.Binding, 0, len(ts))
	for _, t := range ts {
		info, err := t.Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading tool info: %w", err)
		}
		inv, ok := t.(tool.InvokableTool)
		if !ok {
			return nil, fmt.Errorf("tool %q is not an InvokableTool, so a program cannot call it: drop it or list it in Options.Blocked", info.Name)
		}
		name := info.Name
		out = append(out, codemode.Binding{
			Name: name,
			Invoke: func(ctx context.Context, args string) (string, error) {
				return inv.InvokableRun(ctx, args)
			},
		})
	}
	return out, nil
}

// NewTool presents a [codemode.Tool] as an eino tool, ready to append to the
// slice you pass to ToolsNode or a ReAct agent.
func NewTool(t *codemode.Tool) tool.InvokableTool {
	return &einoTool{inner: t}
}

type einoTool struct {
	inner *codemode.Tool
}

func (t *einoTool) Info(context.Context) (*schema.ToolInfo, error) {
	var s jsonschema.Schema
	if err := json.Unmarshal(t.inner.ParametersJSON(), &s); err != nil {
		return nil, fmt.Errorf("converting the parameter schema: %w", err)
	}
	return &schema.ToolInfo{
		Name:        t.inner.Name(),
		Desc:        t.inner.Description(),
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&s),
	}, nil
}

// InvokableRun returns a failed run as the tool's result rather than as an
// error.
//
// eino's ToolsNode propagates a tool error up and aborts the graph; it does not
// hand it to the model. Everything Call reports is written for the model to
// correct from — the failure kind, what to do differently, the tail of what the
// program printed — so surfacing it as an error would abort the agent over a
// program the model could have fixed on the next turn, and the whole failure
// taxonomy would never reach it.
func (t *einoTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	out, err := t.inner.Call(ctx, argumentsInJSON)
	if err != nil {
		return err.Error(), nil
	}
	return out, nil
}
