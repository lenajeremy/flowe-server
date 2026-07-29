package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

// DataStore is the executor's view of a persistence container's metadata.
type DataStore struct {
	ID         string
	Name       string
	Kind       string // kv | collection | text
	Scope      string // run | workflow | account
	WorkflowID string
	Schema     json.RawMessage // nil = schemaless
}

// storeBackend is the set of primitive read/write ops a persistence backend
// exposes. The DB implementation (DataStoreOps) covers workflow/account scope;
// *runScope covers run scope in-memory. runDataNode dispatches over one of
// these depending on the store's scope.
type storeBackend interface {
	KVGet(storeID, key string) (json.RawMessage, bool, error)
	KVSet(storeID, key string, value json.RawMessage) error
	KVIncrement(storeID, key string, amount float64) (float64, error)
	KVDelete(storeID, key string) error

	RecordAppend(storeID string, record json.RawMessage) (id string, err error)
	RecordQuery(storeID string, filter json.RawMessage, limit int) ([]json.RawMessage, error)
	RecordUpdate(storeID, recordID string, record json.RawMessage) error
	RecordDelete(storeID, recordID string) error
	RecordCount(storeID string) (int64, error)
	RecordClear(storeID string) error

	TextGet(storeID string) (string, error)
	TextSet(storeID, text string) error
	TextAppend(storeID, text string) (string, error)
}

// DataStoreOps is the durable persistence gateway the executor calls for
// workflow/account stores. Set at boot from the db layer (like
// IntegrationCredsLookup); nil means persistence is disabled and Data nodes
// error clearly.
type DataStoreOps interface {
	storeBackend
	// GetStore resolves store metadata for the owner (nil if missing/not owned).
	GetStore(ownerID, storeID string) (*DataStore, error)
}

var DataStores DataStoreOps

// ── run scope (in-memory, ctx-bound) ─────────────────────────────

type runScope struct {
	mu      sync.Mutex
	kv      map[string]map[string]json.RawMessage // storeID → key → value
	records map[string][]runRecord                // storeID → records
}

type runRecord struct {
	id  string
	rec json.RawMessage
}

func newRunScope() *runScope {
	return &runScope{
		kv:      map[string]map[string]json.RawMessage{},
		records: map[string][]runRecord{},
	}
}

type runScopeKey struct{}

// withRunScope attaches a fresh in-memory run scope to ctx (once per run).
func withRunScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, runScopeKey{}, newRunScope())
}

// runScopeFromContext returns the run's in-memory scope, or a detached throwaway
// (e.g. single-node test with no surrounding run).
func runScopeFromContext(ctx context.Context) *runScope {
	if rs, ok := ctx.Value(runScopeKey{}).(*runScope); ok {
		return rs
	}
	return newRunScope()
}

const runTextKey = "value" // reserved kv key backing a run-scoped text store

func (r *runScope) KVGet(storeID, key string) (json.RawMessage, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.kv[storeID][key]
	return v, ok, nil
}

func (r *runScope) kvSetLocked(storeID, key string, value json.RawMessage) {
	if r.kv[storeID] == nil {
		r.kv[storeID] = map[string]json.RawMessage{}
	}
	r.kv[storeID][key] = value
}

func (r *runScope) KVSet(storeID, key string, value json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kvSetLocked(storeID, key, value)
	return nil
}

func (r *runScope) KVIncrement(storeID, key string, amount float64) (float64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := 0.0
	if v, ok := r.kv[storeID][key]; ok {
		_ = json.Unmarshal(v, &cur)
	}
	cur += amount
	b, _ := json.Marshal(cur)
	r.kvSetLocked(storeID, key, b)
	return cur, nil
}

func (r *runScope) KVDelete(storeID, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.kv[storeID], key)
	return nil
}

func (r *runScope) RecordAppend(storeID string, record json.RawMessage) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := newUUID()
	r.records[storeID] = append(r.records[storeID], runRecord{id: id, rec: record})
	return id, nil
}

func (r *runScope) RecordQuery(storeID string, filter json.RawMessage, limit int) ([]json.RawMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []json.RawMessage{}
	for _, rec := range r.records[storeID] {
		if !matchFilter(rec.rec, filter) {
			continue
		}
		out = append(out, injectID(rec.rec, rec.id))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (r *runScope) RecordUpdate(storeID, recordID string, record json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, rec := range r.records[storeID] {
		if rec.id == recordID {
			r.records[storeID][i].rec = record
			return nil
		}
	}
	return fmt.Errorf("record %s not found", recordID)
}

func (r *runScope) RecordDelete(storeID, recordID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	recs := r.records[storeID]
	for i, rec := range recs {
		if rec.id == recordID {
			r.records[storeID] = append(recs[:i], recs[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("record %s not found", recordID)
}

func (r *runScope) RecordCount(storeID string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return int64(len(r.records[storeID])), nil
}

func (r *runScope) RecordClear(storeID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.records, storeID)
	return nil
}

func (r *runScope) TextGet(storeID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var s string
	if v, ok := r.kv[storeID][runTextKey]; ok {
		_ = json.Unmarshal(v, &s)
	}
	return s, nil
}

func (r *runScope) TextSet(storeID, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, _ := json.Marshal(text)
	r.kvSetLocked(storeID, runTextKey, b)
	return nil
}

func (r *runScope) TextAppend(storeID, text string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := ""
	if v, ok := r.kv[storeID][runTextKey]; ok {
		_ = json.Unmarshal(v, &cur)
	}
	cur += text
	b, _ := json.Marshal(cur)
	r.kvSetLocked(storeID, runTextKey, b)
	return cur, nil
}

// ── node dispatch ────────────────────────────────────────────────

func runDataNode(ctx context.Context, d FlowNodeData, outputs map[string]string, ownerID string) (string, error) {
	if DataStores == nil {
		return "", fmt.Errorf("persistence is not configured on this server")
	}
	sub := func(s string) string { return substituteTemplates(s, outputs) }

	storeID := strings.TrimSpace(sub(d.DataStoreId))
	if storeID == "" {
		return "", fmt.Errorf("Data node: no store selected")
	}
	store, err := DataStores.GetStore(ownerID, storeID)
	if err != nil {
		return "", fmt.Errorf("Data node: %w", err)
	}
	if store == nil {
		return "", fmt.Errorf("Data node: store not found or not yours")
	}

	var backend storeBackend = DataStores
	if store.Scope == "run" {
		backend = runScopeFromContext(ctx)
	}

	op := strings.TrimSpace(d.DataOp)
	switch store.Kind {
	case "kv":
		return kvOp(backend, store, op, d, sub)
	case "collection":
		return collectionOp(backend, store, op, d, sub)
	case "text":
		return textOp(backend, store, op, d, sub)
	default:
		return "", fmt.Errorf("Data node: unknown store kind %q", store.Kind)
	}
}

func kvOp(b storeBackend, store *DataStore, op string, d FlowNodeData, sub func(string) string) (string, error) {
	key := sub(d.DataKey)
	switch op {
	case "get":
		v, ok, err := b.KVGet(store.ID, key)
		if err != nil {
			return "", err
		}
		if !ok {
			return "null", nil
		}
		return string(v), nil
	case "set":
		val := toJSONValue(sub(d.DataValue))
		if err := CheckDataSize(val); err != nil {
			return "", fmt.Errorf("Data node: %w", err)
		}
		if err := b.KVSet(store.ID, key, val); err != nil {
			return "", err
		}
		return string(val), nil
	case "increment":
		amt := 1.0
		if s := strings.TrimSpace(sub(d.DataAmount)); s != "" {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				amt = f
			}
		}
		n, err := b.KVIncrement(store.ID, key, amt)
		if err != nil {
			return "", err
		}
		return formatNumber(n), nil
	case "delete":
		if err := b.KVDelete(store.ID, key); err != nil {
			return "", err
		}
		return `{"deleted":true}`, nil
	default:
		return "", fmt.Errorf("Data node: op %q is not valid for a key-value store", op)
	}
}

func collectionOp(b storeBackend, store *DataStore, op string, d FlowNodeData, sub func(string) string) (string, error) {
	switch op {
	case "append":
		rec, err := parseObject(sub(d.DataRecord))
		if err != nil {
			return "", fmt.Errorf("Data node: record must be a JSON object: %w", err)
		}
		if err := CheckDataSize(rec); err != nil {
			return "", fmt.Errorf("Data node: %w", err)
		}
		if err := ValidateRecord(store.Schema, rec); err != nil {
			return "", fmt.Errorf("Data node: %w", err)
		}
		id, err := b.RecordAppend(store.ID, rec)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"id":%q}`, id), nil
	case "query":
		var filter json.RawMessage
		if f := strings.TrimSpace(sub(d.DataFilter)); f != "" && f != "{}" {
			if !json.Valid([]byte(f)) {
				return "", fmt.Errorf("Data node: filter must be valid JSON")
			}
			filter = json.RawMessage(f)
		}
		limit := 100
		if s := strings.TrimSpace(sub(d.DataLimit)); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				limit = n
			}
		}
		recs, err := b.RecordQuery(store.ID, filter, limit)
		if err != nil {
			return "", err
		}
		out, _ := json.Marshal(recs)
		return string(out), nil
	case "update":
		id := sub(d.DataRecordId)
		rec, err := parseObject(sub(d.DataRecord))
		if err != nil {
			return "", fmt.Errorf("Data node: record must be a JSON object: %w", err)
		}
		if err := CheckDataSize(rec); err != nil {
			return "", fmt.Errorf("Data node: %w", err)
		}
		if err := ValidateRecord(store.Schema, rec); err != nil {
			return "", fmt.Errorf("Data node: %w", err)
		}
		if err := b.RecordUpdate(store.ID, id, rec); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"id":%q,"updated":true}`, id), nil
	case "delete":
		id := sub(d.DataRecordId)
		if err := b.RecordDelete(store.ID, id); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"id":%q,"deleted":true}`, id), nil
	case "count":
		n, err := b.RecordCount(store.ID)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(n, 10), nil
	case "clear":
		if err := b.RecordClear(store.ID); err != nil {
			return "", err
		}
		return `{"cleared":true}`, nil
	default:
		return "", fmt.Errorf("Data node: op %q is not valid for a collection store", op)
	}
}

func textOp(b storeBackend, store *DataStore, op string, d FlowNodeData, sub func(string) string) (string, error) {
	switch op {
	case "get":
		return b.TextGet(store.ID)
	case "set":
		txt := sub(d.DataValue)
		if err := CheckDataSize([]byte(txt)); err != nil {
			return "", fmt.Errorf("Data node: %w", err)
		}
		if err := b.TextSet(store.ID, txt); err != nil {
			return "", err
		}
		return txt, nil
	case "append":
		txt := sub(d.DataValue)
		if err := CheckDataSize([]byte(txt)); err != nil {
			return "", fmt.Errorf("Data node: %w", err)
		}
		return b.TextAppend(store.ID, txt)
	default:
		return "", fmt.Errorf("Data node: op %q is not valid for a text store", op)
	}
}

// ── helpers ──────────────────────────────────────────────────────

// toJSONValue keeps valid JSON as-is (numbers, objects, bools) and wraps
// anything else as a JSON string, so `set` on a counter stores 42 not "42".
func toJSONValue(s string) json.RawMessage {
	t := strings.TrimSpace(s)
	if t != "" && json.Valid([]byte(t)) {
		return json.RawMessage(t)
	}
	b, _ := json.Marshal(s)
	return b
}

func formatNumber(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func parseObject(s string) (json.RawMessage, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(m)
	return b, nil
}

// injectID returns the record object with an "_id" field added (read-only
// convenience so downstream update/delete can reference it).
func injectID(rec json.RawMessage, id string) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(rec, &m); err != nil {
		return rec
	}
	m["_id"] = id
	b, _ := json.Marshal(m)
	return b
}

// matchFilter reports whether record contains every key/value in filter
// (top-level equality). A nil/empty filter matches everything.
func matchFilter(record, filter json.RawMessage) bool {
	if len(filter) == 0 {
		return true
	}
	var f, r map[string]any
	if json.Unmarshal(filter, &f) != nil || json.Unmarshal(record, &r) != nil {
		return false
	}
	for k, want := range f {
		if got, ok := r[k]; !ok || !reflect.DeepEqual(got, want) {
			return false
		}
	}
	return true
}

type schemaColumn struct {
	Name string `json:"name"`
	Type string `json:"type"` // text | number | boolean | datetime | json
}

// MaxDataValueBytes caps a single stored value/record/text write. Guards
// against workflow loops ballooning a store row by row.
const MaxDataValueBytes = 64 << 10

// CheckDataSize enforces MaxDataValueBytes; shared by the node and REST paths.
func CheckDataSize(b []byte) error {
	if len(b) > MaxDataValueBytes {
		return fmt.Errorf("value exceeds the %dKB limit", MaxDataValueBytes>>10)
	}
	return nil
}

var validColumnTypes = map[string]bool{"text": true, "number": true, "boolean": true, "datetime": true, "json": true}

// ValidateSchema checks a typed-collection schema definition: a JSON array of
// {name,type} with known types and non-empty unique names. nil = schemaless, ok.
func ValidateSchema(schema json.RawMessage) error {
	if len(schema) == 0 {
		return nil
	}
	var cols []schemaColumn
	if err := json.Unmarshal(schema, &cols); err != nil {
		return fmt.Errorf("schema must be a JSON array of {name,type} columns")
	}
	seen := map[string]bool{}
	for _, c := range cols {
		if c.Name == "" {
			return fmt.Errorf("schema columns need a name")
		}
		if seen[c.Name] {
			return fmt.Errorf("duplicate schema column %q", c.Name)
		}
		seen[c.Name] = true
		if !validColumnTypes[c.Type] {
			return fmt.Errorf("schema column %q has unknown type %q (use text, number, boolean, datetime, or json)", c.Name, c.Type)
		}
	}
	return nil
}

// ValidateRecord checks a record against a typed collection schema. A nil
// schema (schemaless) accepts anything. Missing columns are allowed (nullable);
// present columns must match their declared type. Shared by the node and REST
// paths so panel edits obey the same rules as workflow writes.
func ValidateRecord(schema, record json.RawMessage) error {
	if len(schema) == 0 {
		return nil
	}
	var cols []schemaColumn
	if err := json.Unmarshal(schema, &cols); err != nil {
		return nil // malformed legacy schema shouldn't block writes
	}
	var m map[string]any
	if err := json.Unmarshal(record, &m); err != nil {
		return fmt.Errorf("record must be a JSON object")
	}
	for _, c := range cols {
		v, ok := m[c.Name]
		if !ok || v == nil {
			continue
		}
		if !typeMatches(c.Type, v) {
			return fmt.Errorf("field %q must be %s", c.Name, c.Type)
		}
	}
	return nil
}

func typeMatches(t string, v any) bool {
	switch t {
	case "number":
		_, ok := v.(float64)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "text", "datetime":
		_, ok := v.(string)
		return ok
	case "json":
		return true
	default:
		return true
	}
}
