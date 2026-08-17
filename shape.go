package codemode

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"
)

// ReturnShape renders a compact hint at the shape of T, e.g.
// `{hits: [{path, line: num, snippet}], total: num, next?}`.
//
// Tool-calling protocols ship the input schema and nothing about the output, so
// a model writing `r.data.hits` is guessing. The hint is meant for the tail of a
// tool description:
//
//	desc += "\n\nReturns `{data: " + codemode.ReturnShape[SearchResult]() + "}`."
//
// [PromptOptions.ReturnShapes] adds the matching notation guide to the prompt
// section. Because the hint is derived from the type, it tracks the code.
//
// Notation, kept short to cost few tokens: a bare name is a string; anything
// else is annotated (`num`, `bool`, `timestamp`, `…` for unknown); `?` marks an
// omitempty field. Depth is capped at 3 and length at 240 characters, truncated
// with brackets closed so the hint still parses.
//
// Returns the empty string for a type the notation has nothing to say about: an
// opaque payload (json.RawMessage, []byte, any), or a struct whose every field
// is opaque.
func ReturnShape[T any]() string {
	return ReturnShapeOf(reflect.TypeOf((*T)(nil)).Elem())
}

// ReturnShapeOf is [ReturnShape] for a type you have at runtime.
func ReturnShapeOf(t reflect.Type) string {
	s := renderShape(t, 0, map[reflect.Type]bool{})
	if s == "" || s == "…" || s == "{…}" || s == "{}" {
		return ""
	}
	if len(s) > maxShapeLen {
		s = s[:maxShapeLen] + "…"
		var stack []byte
		for _, r := range s {
			switch r {
			case '{':
				stack = append(stack, '}')
			case '[':
				stack = append(stack, ']')
			case '}', ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			}
		}
		for i := len(stack) - 1; i >= 0; i-- {
			s += string(stack[i])
		}
	}
	return s
}

const (
	maxShapeLen   = 240
	shapeMaxDepth = 3
)

var (
	timeType       = reflect.TypeOf(time.Time{})
	rawMessageType = reflect.TypeOf(json.RawMessage{})
)

func renderShape(t reflect.Type, depth int, seen map[reflect.Type]bool) string {
	if t == nil {
		return "…"
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType {
		return "timestamp" // serializes as an RFC3339 string; the name says more
	}
	if t == rawMessageType {
		return ""
	}
	switch t.Kind() {
	case reflect.String:
		return "str"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "num"
	case reflect.Interface:
		return "…"
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return "" // []byte is an opaque payload
		}
		if depth >= shapeMaxDepth {
			return "[…]"
		}
		elem := renderShape(t.Elem(), depth+1, seen)
		if elem == "" {
			elem = "…"
		}
		return "[" + elem + "]"
	case reflect.Map:
		if depth >= shapeMaxDepth {
			return "{…}"
		}
		val := renderShape(t.Elem(), depth+1, seen)
		if val == "" || val == "…" {
			return "{…}"
		}
		return "{key: " + val + "}"
	case reflect.Struct:
		return renderStructShape(t, depth, seen)
	default:
		return ""
	}
}

func renderStructShape(t reflect.Type, depth int, seen map[reflect.Type]bool) string {
	if depth >= shapeMaxDepth {
		return "{…}"
	}
	if seen[t] {
		return "{…}" // self-referential: saying it nests is enough
	}
	seen[t] = true
	defer delete(seen, t)

	var parts []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		// An embedded field's name is its type's name, often lowercase, so
		// IsExported says no — but encoding/json still inlines an embedded
		// unexported struct type's exported fields. Let those through.
		if !f.IsExported() && !f.Anonymous {
			continue
		}
		name := f.Name
		omitempty := false
		if tag, ok := f.Tag.Lookup("json"); ok {
			seg := strings.Split(tag, ",")[0]
			if seg == "-" {
				continue
			}
			if seg != "" {
				name = seg
			}
			omitempty = strings.Contains(tag, "omitempty")
		}
		if omitempty {
			name += "?"
		}
		if f.Anonymous {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && ft != timeType {
				if inlined := renderStructShape(ft, depth, seen); inlined != "" && inlined != "{…}" {
					parts = append(parts, strings.TrimSuffix(strings.TrimPrefix(inlined, "{"), "}"))
				}
				continue
			}
		}
		switch f.Type.Kind() {
		case reflect.String:
			parts = append(parts, name)
		case reflect.Bool:
			parts = append(parts, name+": bool")
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
			parts = append(parts, name+": num")
		default:
			sub := renderShape(f.Type, depth+1, seen)
			if sub == "" {
				// Opaque field: drop it entirely. The shape has nothing useful to
				// say about it, and a bare name would be read as "string".
				continue
			}
			parts = append(parts, name+": "+sub)
		}
	}
	if len(parts) == 0 {
		return "{}"
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
