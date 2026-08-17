package codemode

import (
	"context"
	"encoding/json"
	"log/slog"
)

// Codec is the JSON implementation the engine uses: decoding a sub-call's
// result before a program sees it, decoding the model's arguments, and encoding
// the run result. It defaults to encoding/json.
//
// The reason to replace it is agreement rather than speed. A host whose tools
// serialize through sonic, goccy or a configured jsoniter has already decided
// things this engine would otherwise decide differently — how big integers
// survive a round trip, whether unknown fields are an error, how invalid UTF-8
// is handled. Passing that codec in is what makes `r.data.x` inside a program
// hold the same value the model would have seen from a direct call.
//
// The two methods are encoding/json's, so most implementations already satisfy
// it; a codec that also implements [StringUnmarshaler] skips a copy on the
// sub-call path.
type Codec interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(data []byte, v any) error
}

// StringUnmarshaler is an optional [Codec] extension. A tool returns its result
// as a string, so a codec that decodes from one directly avoids copying every
// result into a fresh []byte before parsing it — on a fan-out that pulls large
// documents through, that copy is the whole result set again.
//
// Detected by type assertion. Without it the engine converts and calls
// Unmarshal, which is what encoding/json needs anyway.
type StringUnmarshaler interface {
	UnmarshalString(data string, v any) error
}

// Logger receives the engine's internal warnings. There is exactly one today: a
// host tool's ConflictKeys panicked, and the sub-call was scheduled as
// conflict-free rather than failing the run. attrs are slog-style alternating
// key/value pairs. The default logs through slog.Default at warn level.
//
// It takes a context because the warning belongs to one run inside one request.
// A host that correlates logs by trace or tenant reads that from the context,
// and a warning filed under the wrong request is worse than no warning.
type Logger func(ctx context.Context, msg string, attrs ...any)

// RunOption supplies one of [Run]'s host dependencies. Both have defaults that
// need nothing from the caller, so this is variadic: an agent that has no
// opinion about JSON or logging passes nothing.
type RunOption func(*deps)

// WithCodec replaces the JSON implementation. See [Codec].
func WithCodec(c Codec) RunOption {
	return func(d *deps) {
		if c != nil {
			d.codec = c
		}
	}
}

// WithLogger routes the engine's internal warnings to l. See [Logger].
func WithLogger(l Logger) RunOption {
	return func(d *deps) {
		if l != nil {
			d.logger = l
		}
	}
}

// deps is the resolved dependency set for one run. Both fields are always
// non-nil after [resolveDeps], so no call site needs a nil check.
type deps struct {
	codec  Codec
	logger Logger
}

// stdCodec is the default: encoding/json, addressed through the interface.
type stdCodec struct{}

func (stdCodec) Marshal(v any) ([]byte, error)         { return json.Marshal(v) }
func (stdCodec) Unmarshal(b []byte, v any) error       { return json.Unmarshal(b, v) }
func (stdCodec) UnmarshalString(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func slogWarn(ctx context.Context, msg string, attrs ...any) {
	slog.WarnContext(ctx, msg, attrs...)
}

func resolveDeps(opts []RunOption) deps {
	d := deps{codec: stdCodec{}, logger: slogWarn}
	for _, opt := range opts {
		if opt != nil {
			opt(&d)
		}
	}
	return d
}

// unmarshalString decodes from a string, taking the copy-free path when the
// codec offers one.
func (d deps) unmarshalString(s string, v any) error {
	if su, ok := d.codec.(StringUnmarshaler); ok {
		return su.UnmarshalString(s, v)
	}
	return d.codec.Unmarshal([]byte(s), v)
}
