package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

// ExecuteSingleNode runs one workflow node in isolation with a caller-supplied
// outputs map (the chat session's state bag) and optional per-call overrides.
//
// Overrides are merged into a COPY of the node's data via JSON round-trip:
// the stored workflow is read-only to this path by construction, so a chat
// turn can adjust a prompt/query/channel for one call without ever mutating
// the canvas. Keys must match FlowNodeData JSON tags (e.g. "userPrompt").
//
// The returned output is NOT written back into outputs — the caller owns
// state persistence (and truncation policy).
func ExecuteSingleNode(
	ctx context.Context,
	node WorkflowASTNode,
	overrides map[string]any,
	outputs map[string]string,
	edges []WorkflowASTEdge,
	keys APIKeys,
	runID, ownerID, orgID string,
	emit func(ExecutionEvent),
) (string, error) {
	ctx = WithOrg(ctx, orgID)
	if len(overrides) > 0 {
		merged, err := mergeNodeData(node.Data, overrides)
		if err != nil {
			return "", fmt.Errorf("invalid overrides for node %q: %w", node.Data.Label, err)
		}
		node.Data = merged
	}
	if emit == nil {
		emit = func(ExecutionEvent) {}
	}
	if outputs == nil {
		outputs = map[string]string{}
	}
	return executeNode(ctx, node, outputs, edges, keys, runID, ownerID, emit)
}

// ResolveSingleNodeData returns the exact node configuration ExecuteSingleNode
// will use after applying per-call overrides. Approval systems use it to pin,
// hash, and display the effective call before any side effect occurs.
func ResolveSingleNodeData(node WorkflowASTNode, overrides map[string]any) (FlowNodeData, error) {
	if len(overrides) == 0 {
		return node.Data, nil
	}
	return mergeNodeData(node.Data, overrides)
}

// ResolveSingleNodeDataForExecution returns the exact configuration produced by
// applying overrides and one immutable output snapshot. Hosted approvals hash,
// display, and execute this value so templates cannot change after review.
func ResolveSingleNodeDataForExecution(node WorkflowASTNode, overrides map[string]any, outputs map[string]string) (FlowNodeData, error) {
	data, err := ResolveSingleNodeData(node, overrides)
	if err != nil {
		return FlowNodeData{}, err
	}
	// Deep-copy pointer fields before replacing strings.
	raw, err := json.Marshal(data)
	if err != nil {
		return FlowNodeData{}, err
	}
	var resolved FlowNodeData
	if err := json.Unmarshal(raw, &resolved); err != nil {
		return FlowNodeData{}, err
	}
	value := reflect.ValueOf(&resolved).Elem()
	for index := 0; index < value.NumField(); index++ {
		field := value.Field(index)
		switch field.Kind() {
		case reflect.String:
			field.SetString(substituteTemplates(field.String(), outputs))
		case reflect.Pointer:
			if !field.IsNil() && field.Elem().Kind() == reflect.String {
				text := substituteTemplates(field.Elem().String(), outputs)
				copy := reflect.New(field.Type().Elem())
				copy.Elem().SetString(text)
				field.Set(copy)
			}
		}
	}
	return resolved, nil
}

// mergeNodeData overlays override values onto a copy of the node data using
// the JSON representation, so callers speak the same field names the frontend
// and AI catalog use. Unknown keys are rejected to surface orchestrator
// hallucinations instead of silently ignoring them.
func mergeNodeData(data FlowNodeData, overrides map[string]any) (FlowNodeData, error) {
	base, err := json.Marshal(data)
	if err != nil {
		return data, err
	}
	var m map[string]any
	if err := json.Unmarshal(base, &m); err != nil {
		return data, err
	}

	// Validate override keys against the struct's JSON tags via a probe
	// marshal of an empty struct union with the populated one.
	valid := knownFlowDataKeys()
	for k, v := range overrides {
		if !valid[k] {
			return data, fmt.Errorf("unknown field %q", k)
		}
		if k == "nodeType" || k == "label" || k == "integrationToken" {
			return data, fmt.Errorf("field %q cannot be overridden", k)
		}
		m[k] = v
	}

	remerged, err := json.Marshal(m)
	if err != nil {
		return data, err
	}
	var out FlowNodeData
	if err := json.Unmarshal(remerged, &out); err != nil {
		return data, fmt.Errorf("override value has wrong type: %w", err)
	}
	return out, nil
}

// knownFlowDataKeys returns the set of JSON field names FlowNodeData accepts.
// Built by marshalling a fully zero struct is insufficient (omitempty hides
// keys), so we reflect over the JSON of a round-tripped map built from tags.
var (
	flowDataKeys     map[string]bool
	flowDataKeysOnce sync.Once
)

func knownFlowDataKeys() map[string]bool {
	flowDataKeysOnce.Do(func() {
		keys := map[string]bool{}
		for _, tag := range flowNodeDataJSONTags() {
			keys[tag] = true
		}
		flowDataKeys = keys
	})
	return flowDataKeys
}

// flowNodeDataJSONTags reflects FlowNodeData's json tags (minus omitempty).
func flowNodeDataJSONTags() []string {
	t := reflect.TypeOf(FlowNodeData{})
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}
