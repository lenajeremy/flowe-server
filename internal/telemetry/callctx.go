package telemetry

import (
	"context"
	"encoding/json"
	"strings"

	"go.opentelemetry.io/otel/attribute"
)

// CallContext is the "who is calling" carried alongside every outbound request
// a node makes. Providers build their HTTP requests from the node's ctx, so
// stamping this once in executeNode lets the shared transport attribute any
// endpoint back to the exact workflow, run and node that reached for it —
// without touching a single provider helper.
type CallContext struct {
	WorkflowID   string
	WorkflowName string
	RunID        string
	Trigger      string
	NodeID       string
	NodeLabel    string
	NodeType     string
	Op           string // integration operation, when the node has one
}

type callCtxKey struct{}

func WithCallContext(ctx context.Context, cc CallContext) context.Context {
	return context.WithValue(ctx, callCtxKey{}, cc)
}

// CallContextFrom returns the active node call, if any.
func CallContextFrom(ctx context.Context) (CallContext, bool) {
	cc, ok := ctx.Value(callCtxKey{}).(CallContext)
	return cc, ok
}

// LogAttrs renders the call identity as slog key/values. Empty fields are
// dropped so log lines stay readable.
func (cc CallContext) LogAttrs() []any {
	out := make([]any, 0, 16)
	add := func(k, v string) {
		if v != "" {
			out = append(out, k, v)
		}
	}
	add("workflow_id", cc.WorkflowID)
	add("workflow", cc.WorkflowName)
	add("run_id", cc.RunID)
	add("trigger", cc.Trigger)
	add("node_id", cc.NodeID)
	add("node", cc.NodeLabel)
	add("node_type", cc.NodeType)
	add("op", cc.Op)
	return out
}

// SpanAttributes mirrors the identity onto the active span, so a trace in Tempo
// can be filtered by workflow or run.
func (cc CallContext) SpanAttributes() []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, 8)
	add := func(k, v string) {
		if v != "" {
			out = append(out, attribute.String(k, v))
		}
	}
	add("fernary.workflow.id", cc.WorkflowID)
	add("fernary.workflow.name", cc.WorkflowName)
	add("fernary.run.id", cc.RunID)
	add("fernary.trigger", cc.Trigger)
	add("fernary.node.id", cc.NodeID)
	add("fernary.node.label", cc.NodeLabel)
	add("fernary.node.type", cc.NodeType)
	add("fernary.integration.op", cc.Op)
	return out
}

// ── Redaction ────────────────────────────────────────────────────

// secretish matches config keys whose values must never reach a log line.
// Matching by name (not by an allow-list of the ~180 op fields) means a new
// provider field can't silently start leaking a credential.
func secretish(key string) bool {
	k := strings.ToLower(key)
	for _, bad := range []string{"token", "secret", "password", "apikey", "api_key", "credential", "authorization", "signature"} {
		if strings.Contains(k, bad) {
			return true
		}
	}
	return false
}

// RedactArgs renders a node's configuration as the arguments a call was made
// with: secrets replaced, empty and zero fields dropped, long values clipped.
// `v` is anything JSON-marshalable — in practice the node's data struct.
func RedactArgs(v any, cap int) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		if secretish(k) {
			if s, ok := val.(string); ok && s != "" {
				out[k] = "[redacted]"
			}
			continue
		}
		switch t := val.(type) {
		case nil:
			continue
		case string:
			if t == "" {
				continue
			}
			out[k] = Clip(t, 200)
		case float64:
			if t == 0 {
				continue
			}
			out[k] = t
		case bool:
			if !t {
				continue
			}
			out[k] = t
		default:
			// Nested objects/arrays: keep a clipped JSON rendering.
			b, _ := json.Marshal(t)
			if s := string(b); s != "null" && s != "{}" && s != "[]" {
				out[k] = Clip(s, 200)
			}
		}
	}
	// Identity fields, not arguments — they're already separate log keys.
	delete(out, "nodeType")
	delete(out, "label")

	b, _ := json.Marshal(out)
	return Clip(string(b), cap)
}

// Clip shortens s to at most n characters, marking that it was cut.
func Clip(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ── Surface ───────────────────────────────────────────────────

// Surface is which part of the product spent the tokens: the AI builder, an LLM
// node in a run, the agent chat loop, or a web-tool summariser. It matters for
// billing as much as for dashboards, because the builder spends before anyone
// pays and has to be separable from work a customer asked for.
type Surface string

const (
	SurfaceBuilder Surface = "builder"
	SurfaceNode    Surface = "node"
	SurfaceAgent   Surface = "agent"
	SurfaceTool    Surface = "tool"
	surfaceUnknown         = "unknown"
)

type surfaceKey struct{}

// WithSurface tags a context for the surfaces that have no CallContext of their
// own — the builder and agent loops run outside a workflow node.
func WithSurface(ctx context.Context, s Surface) context.Context {
	return context.WithValue(ctx, surfaceKey{}, s)
}

// SurfaceFrom resolves the spending surface. An explicit tag wins; otherwise the
// presence of a node call context means this is node work. "unknown" is a bug
// rather than a category — a call site that reports it has not been wired up.
func SurfaceFrom(ctx context.Context) string {
	if s, ok := ctx.Value(surfaceKey{}).(Surface); ok && s != "" {
		return string(s)
	}
	if _, ok := CallContextFrom(ctx); ok {
		return string(SurfaceNode)
	}
	return surfaceUnknown
}
