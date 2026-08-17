package codemode

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type hit struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Snippet string `json:"snippet"`
}

type searchResult struct {
	Query     string `json:"query"`
	Hits      []hit  `json:"hits"`
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated"`
	Next      string `json:"next,omitempty"`
	Internal  string `json:"-"`
	unexposed string
}

type withTime struct {
	CreatedAt time.Time `json:"created_at"`
	Name      string    `json:"name"`
}

type opaque struct {
	Payload json.RawMessage `json:"payload"`
	Blob    []byte          `json:"blob"`
}

type node struct {
	Name     string  `json:"name"`
	Children []*node `json:"children"`
}

type embedded struct {
	hit
	Score float64 `json:"score"`
}

func TestReturnShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"struct", ReturnShape[searchResult](), "{query, hits: [{path, line: num, snippet}], total: num, truncated: bool, next?}"},
		{"scalar", ReturnShape[string](), "str"},
		{"slice", ReturnShape[[]hit](), "[{path, line: num, snippet}]"},
		{"time", ReturnShape[withTime](), "{created_at: timestamp, name}"},
		{"embedded inlines", ReturnShape[embedded](), "{path, line: num, snippet, score: num}"},
		{"opaque fields dropped", ReturnShape[opaque](), ""},
		{"any", ReturnShape[any](), ""},
		{"raw message", ReturnShape[json.RawMessage](), ""},
		{"map of structs", ReturnShape[map[string]hit](), "{key: {path, line: num, snippet}}"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestReturnShapeStopsAtSelfReference(t *testing.T) {
	// The type is marked seen before its own fields render, so the nested list
	// collapses to "there is more of this here" instead of unrolling.
	if got := ReturnShape[node](); got != "{name, children: [{…}]}" {
		t.Fatalf("got %q", got)
	}
}

func TestReturnShapeIsBounded(t *testing.T) {
	type wide struct {
		A, B, C, D, E, F, G, H, I, J, K, L, M, N, O, P, Q, R, S, T string
		AA, BB, CC, DD, EE, FF, GG, HH, II, JJ, KK, LL, MM         string
	}
	got := ReturnShape[wide]()
	if len(got) > maxShapeLen+8 {
		t.Fatalf("shape hint should be capped, got %d chars", len(got))
	}
	if !strings.HasSuffix(got, "}") {
		t.Fatalf("truncation should close its brackets, got %q", got)
	}
}
