package einocodemode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/gtoxlili/codemode-go"
)

type fakeTool struct {
	name string
	run  func(ctx context.Context, args string) (string, error)
}

func (f *fakeTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: f.name, Desc: "a fake " + f.name}, nil
}

func (f *fakeTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	return f.run(ctx, args)
}

// schemaOnly implements BaseTool and nothing else: the model can be shown it,
// but it cannot be executed.
type schemaOnly struct{ name string }

func (s *schemaOnly) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: s.name}, nil
}

func TestBindingsRoundTripThroughAProgram(t *testing.T) {
	ctx := context.Background()
	bindings, err := Bindings(ctx, []tool.BaseTool{
		&fakeTool{name: "lookup", run: func(_ context.Context, args string) (string, error) {
			var a struct {
				ID int `json:"id"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return "", err
			}
			return fmt.Sprintf(`{"id":%d,"score":%d}`, a.ID, a.ID*10), nil
		}},
	})
	if err != nil {
		t.Fatalf("bindings: %v", err)
	}

	ct := codemode.NewTool(codemode.Options{Bindings: bindings})
	out, err := NewTool(ct).InvokableRun(ctx, `{"code":"const rs = await Promise.all([1,2,3].map(id => tools.lookup({id}))); return rs.reduce((n, r) => n + r.score, 0);","description":"Sum three scores"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, `"result":60`) {
		t.Fatalf("got %s", out)
	}
}

func TestBindingsRejectsNonInvokableTools(t *testing.T) {
	_, err := Bindings(context.Background(), []tool.BaseTool{&schemaOnly{name: "display_only"}})
	if err == nil {
		t.Fatal("expected an error naming the tool")
	}
	if !strings.Contains(err.Error(), "display_only") {
		t.Fatalf("the error should name the tool: %v", err)
	}
}

func TestInfoCarriesNameDescriptionAndSchema(t *testing.T) {
	ct := codemode.NewTool(codemode.Options{
		Bindings: []codemode.Binding{{Name: "search", Invoke: func(context.Context, string) (string, error) { return "{}", nil }}},
		Blocked:  []codemode.Blocked{{Name: "ask_user"}},
	})

	info, err := NewTool(ct).Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Name != "run_code" {
		t.Fatalf("name = %q", info.Name)
	}
	if !strings.Contains(info.Desc, "minus: run_code, ask_user.") {
		t.Fatalf("desc = %q", info.Desc)
	}
	if info.ParamsOneOf == nil {
		t.Fatal("ParamsOneOf is nil, so the model would get no argument schema")
	}
	js, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatalf("converting params: %v", err)
	}
	if js == nil || js.Properties == nil {
		t.Fatalf("the schema lost its properties: %+v", js)
	}
	if _, ok := js.Properties.Get("code"); !ok {
		t.Fatal("the schema lost its code property")
	}
	if len(js.Required) != 2 {
		t.Fatalf("required = %v", js.Required)
	}
}
