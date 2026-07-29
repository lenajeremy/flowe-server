package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fakeOps is an in-memory DataStoreOps. Its embedded *runScope supplies all the
// storeBackend methods, standing in for durable (workflow/account) storage that
// persists across calls; GetStore returns a fixed store.
type fakeOps struct {
	store *DataStore
	*runScope
}

func (f *fakeOps) GetStore(ownerID, storeID string) (*DataStore, error) { return f.store, nil }

func withDataStores(t *testing.T, store *DataStore) *fakeOps {
	t.Helper()
	ops := &fakeOps{store: store, runScope: newRunScope()}
	DataStores = ops
	t.Cleanup(func() { DataStores = nil })
	return ops
}

func run(t *testing.T, ctx context.Context, d FlowNodeData, outputs map[string]string) string {
	t.Helper()
	out, err := runDataNode(ctx, d, outputs, "owner")
	if err != nil {
		t.Fatalf("runDataNode(%s): %v", d.DataOp, err)
	}
	return out
}

func TestKVIncrementPersistsWorkflowScope(t *testing.T) {
	withDataStores(t, &DataStore{ID: "s1", Kind: "kv", Scope: "workflow"})
	d := FlowNodeData{DataStoreId: "s1", DataOp: "increment", DataKey: "count"}
	if got := run(t, context.Background(), d, nil); got != "1" {
		t.Fatalf("first increment = %q, want 1", got)
	}
	// A second, independent run must see the persisted value (workflow scope).
	if got := run(t, context.Background(), d, nil); got != "2" {
		t.Fatalf("second increment = %q, want 2", got)
	}
}

func TestRunScopeIsFreshPerRun(t *testing.T) {
	ops := withDataStores(t, &DataStore{ID: "s2", Kind: "kv", Scope: "run"})
	d := FlowNodeData{DataStoreId: "s2", DataOp: "increment", DataKey: "c"}

	ctx1 := withRunScope(context.Background())
	if got := run(t, ctx1, d, nil); got != "1" {
		t.Fatalf("run1 first = %q, want 1", got)
	}
	if got := run(t, ctx1, d, nil); got != "2" {
		t.Fatalf("run1 second = %q, want 2", got)
	}
	// A new run starts empty — run scope does not persist.
	ctx2 := withRunScope(context.Background())
	if got := run(t, ctx2, d, nil); got != "1" {
		t.Fatalf("run2 first = %q, want 1 (fresh)", got)
	}
	// The durable backend must never have been touched for a run-scoped store.
	if n, _, _ := ops.runScope.KVGet("s2", "c"); n != nil {
		t.Fatalf("run-scoped write leaked into the durable backend: %s", n)
	}
}

func TestKVSetTemplatingAndTypes(t *testing.T) {
	withDataStores(t, &DataStore{ID: "s3", Kind: "kv", Scope: "workflow"})
	// numeric value stays JSON number
	if got := run(t, context.Background(), FlowNodeData{DataStoreId: "s3", DataOp: "set", DataKey: "k", DataValue: "42"}, nil); got != "42" {
		t.Fatalf("set number = %q, want 42", got)
	}
	// templated string value gets quoted
	got := run(t, context.Background(),
		FlowNodeData{DataStoreId: "s3", DataOp: "set", DataKey: "k", DataValue: "hello {{n1.output}}"},
		map[string]string{"n1": "world"})
	if got != `"hello world"` {
		t.Fatalf("set templated string = %q, want \"hello world\"", got)
	}
	if got := run(t, context.Background(), FlowNodeData{DataStoreId: "s3", DataOp: "get", DataKey: "k"}, nil); got != `"hello world"` {
		t.Fatalf("get = %q", got)
	}
}

// A read of something never written must be empty, not the literal "null" —
// the output gets templated straight into prompts and emails.
func TestGetMissingIsEmpty(t *testing.T) {
	withDataStores(t, &DataStore{ID: "m1", Kind: "kv", Scope: "workflow"})
	ctx := context.Background()
	if got := run(t, ctx, FlowNodeData{DataStoreId: "m1", DataOp: "get", DataKey: "never_set"}, nil); got != "" {
		t.Fatalf("get on a missing key = %q, want empty", got)
	}
	// An explicitly stored null reads as empty too.
	run(t, ctx, FlowNodeData{DataStoreId: "m1", DataOp: "set", DataKey: "k", DataValue: "null"}, nil)
	if got := run(t, ctx, FlowNodeData{DataStoreId: "m1", DataOp: "get", DataKey: "k"}, nil); got != "" {
		t.Fatalf("get on a stored null = %q, want empty", got)
	}
	// A deleted key goes back to empty.
	run(t, ctx, FlowNodeData{DataStoreId: "m1", DataOp: "set", DataKey: "d", DataValue: "5"}, nil)
	run(t, ctx, FlowNodeData{DataStoreId: "m1", DataOp: "delete", DataKey: "d"}, nil)
	if got := run(t, ctx, FlowNodeData{DataStoreId: "m1", DataOp: "get", DataKey: "d"}, nil); got != "" {
		t.Fatalf("get after delete = %q, want empty", got)
	}

	// Text stores behave the same.
	withDataStores(t, &DataStore{ID: "m2", Kind: "text", Scope: "workflow"})
	if got := run(t, ctx, FlowNodeData{DataStoreId: "m2", DataOp: "get"}, nil); got != "" {
		t.Fatalf("text get on an empty store = %q, want empty", got)
	}
}

func TestCollectionAppendQueryCount(t *testing.T) {
	withDataStores(t, &DataStore{ID: "c1", Kind: "collection", Scope: "workflow"})
	ctx := context.Background()
	run(t, ctx, FlowNodeData{DataStoreId: "c1", DataOp: "append", DataRecord: `{"name":"a","n":1}`}, nil)
	run(t, ctx, FlowNodeData{DataStoreId: "c1", DataOp: "append", DataRecord: `{"name":"b","n":2}`}, nil)

	if got := run(t, ctx, FlowNodeData{DataStoreId: "c1", DataOp: "count"}, nil); got != "2" {
		t.Fatalf("count = %q, want 2", got)
	}
	out := run(t, ctx, FlowNodeData{DataStoreId: "c1", DataOp: "query", DataFilter: `{"name":"a"}`}, nil)
	var recs []map[string]any
	if err := json.Unmarshal([]byte(out), &recs); err != nil {
		t.Fatalf("query output not JSON array: %v (%s)", err, out)
	}
	if len(recs) != 1 || recs[0]["name"] != "a" {
		t.Fatalf("filtered query = %s, want the single 'a' record", out)
	}
	if _, ok := recs[0]["_id"]; !ok {
		t.Fatalf("query records should carry _id: %s", out)
	}
}

func TestTextAppend(t *testing.T) {
	withDataStores(t, &DataStore{ID: "t1", Kind: "text", Scope: "workflow"})
	ctx := context.Background()
	run(t, ctx, FlowNodeData{DataStoreId: "t1", DataOp: "set", DataValue: "a"}, nil)
	if got := run(t, ctx, FlowNodeData{DataStoreId: "t1", DataOp: "append", DataValue: "b"}, nil); got != "ab" {
		t.Fatalf("append = %q, want ab", got)
	}
	if got := run(t, ctx, FlowNodeData{DataStoreId: "t1", DataOp: "get"}, nil); got != "ab" {
		t.Fatalf("get = %q, want ab", got)
	}
}

func TestOpNotValidForKind(t *testing.T) {
	withDataStores(t, &DataStore{ID: "k1", Kind: "kv", Scope: "workflow"})
	_, err := runDataNode(context.Background(), FlowNodeData{DataStoreId: "k1", DataOp: "append", DataRecord: "{}"}, nil, "owner")
	if err == nil || !strings.Contains(err.Error(), "not valid for a key-value store") {
		t.Fatalf("expected kind/op mismatch error, got %v", err)
	}
}

func TestSchemaValidation(t *testing.T) {
	schema := json.RawMessage(`[{"name":"n","type":"number"},{"name":"name","type":"text"}]`)
	withDataStores(t, &DataStore{ID: "sc1", Kind: "collection", Scope: "workflow", Schema: schema})
	ctx := context.Background()
	// wrong type for n
	if _, err := runDataNode(ctx, FlowNodeData{DataStoreId: "sc1", DataOp: "append", DataRecord: `{"n":"oops"}`}, nil, "owner"); err == nil {
		t.Fatal("expected type error for n")
	}
	// valid record, plus a missing column (allowed)
	if _, err := runDataNode(ctx, FlowNodeData{DataStoreId: "sc1", DataOp: "append", DataRecord: `{"n":5}`}, nil, "owner"); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
}

func TestHelpers(t *testing.T) {
	if string(toJSONValue("42")) != "42" {
		t.Fatal("toJSONValue(42) should stay a number")
	}
	if string(toJSONValue("hi")) != `"hi"` {
		t.Fatal("toJSONValue(hi) should be quoted")
	}
	if !matchFilter(json.RawMessage(`{"a":1,"b":2}`), json.RawMessage(`{"a":1}`)) {
		t.Fatal("matchFilter should match on subset")
	}
	if matchFilter(json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":2}`)) {
		t.Fatal("matchFilter should not match differing value")
	}
	if !matchFilter(json.RawMessage(`{"a":1}`), nil) {
		t.Fatal("nil filter matches all")
	}
}
