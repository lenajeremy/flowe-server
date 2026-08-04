package database

import (
	"encoding/json"
	"os"
	"testing"

	"workflow-ai/server/internal/database/models"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Exercises the gorm DataStoreOps against a real Postgres (jsonb containment,
// OnConflict upsert, FOR UPDATE atomic increment) — the parts the executor's
// fake-backed unit tests can't reach. Opt-in: set TEST_DATABASE_URL, e.g.
//
//	TEST_DATABASE_URL="host=localhost user=postgres password=postgres dbname=workflow_ai port=5434 sslmode=disable"
func dbForTest(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed persistence tests")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.DataStore{}, &models.DataKV{}, &models.DataRecord{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestDataStoreOpsDB(t *testing.T) {
	db := dbForTest(t)
	ops := DataStoreOps{DB: db}
	owner := uuid.NewString()

	newStore := func(name, kind string) string {
		s := models.DataStore{UserID: owner, Name: name, Kind: kind, Scope: "account"}
		if err := db.Create(&s).Error; err != nil {
			t.Fatalf("create %s store: %v", kind, err)
		}
		id := s.ID.String()
		t.Cleanup(func() {
			db.Unscoped().Where("store_id = ?", id).Delete(&models.DataKV{})
			db.Unscoped().Where("store_id = ?", id).Delete(&models.DataRecord{})
			db.Unscoped().Delete(&models.DataStore{}, "id = ?", id)
		})
		return id
	}

	// ── kv: atomic increment, upsert, ownership ──
	kv := newStore("counter", "kv")
	if got, _ := ops.GetStore(owner, kv); got == nil || got.Kind != "kv" {
		t.Fatalf("GetStore returned %+v", got)
	}
	if s, _ := ops.GetStore(uuid.NewString(), kv); s != nil {
		t.Fatal("store leaked across owners")
	}
	if n, _ := ops.KVIncrement(kv, "c", 1); n != 1 {
		t.Fatalf("increment #1 = %v, want 1", n)
	}
	if n, _ := ops.KVIncrement(kv, "c", 4); n != 5 {
		t.Fatalf("increment #2 = %v, want 5", n)
	}
	if err := ops.KVSet(kv, "c", json.RawMessage("9")); err != nil { // upsert over existing
		t.Fatalf("KVSet: %v", err)
	}
	if v, ok, _ := ops.KVGet(kv, "c"); !ok || string(v) != "9" {
		t.Fatalf("KVGet = %s ok=%v, want 9", v, ok)
	}
	_ = ops.KVDelete(kv, "c")
	if _, ok, _ := ops.KVGet(kv, "c"); ok {
		t.Fatal("KVDelete left the key")
	}

	// ── collection: append, jsonb-containment query, _id, update, delete ──
	col := newStore("seen", "collection")
	id1, _ := ops.RecordAppend(col, json.RawMessage(`{"sku":"A","qty":2}`))
	_, _ = ops.RecordAppend(col, json.RawMessage(`{"sku":"B","qty":3}`))
	if n, _ := ops.RecordCount(col); n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
	recs, _ := ops.RecordQuery(col, json.RawMessage(`{"sku":"A"}`), 10)
	if len(recs) != 1 {
		t.Fatalf("containment query returned %d, want 1", len(recs))
	}
	var m map[string]any
	_ = json.Unmarshal(recs[0], &m)
	if m["_id"] != id1 {
		t.Fatalf("_id = %v, want %v", m["_id"], id1)
	}
	if err := ops.RecordUpdate(col, id1, json.RawMessage(`{"sku":"A","qty":99}`)); err != nil {
		t.Fatalf("RecordUpdate: %v", err)
	}
	if err := ops.RecordDelete(col, id1); err != nil {
		t.Fatalf("RecordDelete: %v", err)
	}
	if n, _ := ops.RecordCount(col); n != 1 {
		t.Fatalf("count after delete = %d, want 1", n)
	}

	// ── text: atomic append ──
	txt := newStore("log", "text")
	_ = ops.TextSet(txt, "hello ")
	if r, _ := ops.TextAppend(txt, "world"); r != "hello world" {
		t.Fatalf("TextAppend = %q, want 'hello world'", r)
	}
	if g, _ := ops.TextGet(txt); g != "hello world" {
		t.Fatalf("TextGet = %q, want 'hello world'", g)
	}
}
