package database

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/hub"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// livePreviewCap bounds the value pushed to watching canvases — the node card
// shows a glance, not the whole record. The Data page serves the full value.
const livePreviewCap = 512

func preview(s string) string {
	if len(s) > livePreviewCap {
		return s[:livePreviewCap] + "…"
	}
	return s
}

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// publishKV notifies open canvases that a key changed.
func publishKV(storeID, key, value string) {
	hub.Data.Publish(hub.DataChange{StoreID: storeID, Key: key, Value: preview(value)})
}

// publishCount notifies open canvases of a collection's new row count.
func (o DataStoreOps) publishCount(storeID string) {
	if n, err := o.RecordCount(storeID); err == nil {
		hub.Data.Publish(hub.DataChange{StoreID: storeID, Count: &n})
	}
}

// DataStoreOps implements executor.DataStoreOps against Postgres. Wired at boot
// so the executor's Data node can read/write durable (workflow/account) stores.
// Run-scoped stores never reach here — the executor keeps those in memory.
type DataStoreOps struct{ DB *gorm.DB }

const textKey = "value" // reserved kv key backing a text store

// GetStore resolves a store within one organization. Scoped by org rather than by
// user because an account-scoped store is deliberately shared across the org.
func (o DataStoreOps) GetStore(orgID, storeID string) (*executor.DataStore, error) {
	var st models.DataStore
	err := o.DB.Where("id = ? AND organization_id = ?", storeID, orgID).First(&st).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &executor.DataStore{
		ID:         st.ID.String(),
		Name:       st.Name,
		Kind:       st.Kind,
		Scope:      st.Scope,
		WorkflowID: st.WorkflowID,
		Schema:     json.RawMessage(st.Schema),
	}, nil
}

// ── key-value ────────────────────────────────────────────────────

func (o DataStoreOps) KVGet(storeID, key string) (json.RawMessage, bool, error) {
	var kv models.DataKV
	err := o.DB.Where("store_id = ? AND key = ?", storeID, key).First(&kv).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return json.RawMessage(kv.Value), true, nil
}

func (o DataStoreOps) KVSet(storeID, key string, value json.RawMessage) error {
	kv := models.DataKV{StoreID: storeID, Key: key, Value: models.JSONB(value)}
	err := o.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "store_id"}, {Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
	}).Create(&kv).Error
	if err == nil {
		publishKV(storeID, key, string(value))
	}
	return err
}

func (o DataStoreOps) KVIncrement(storeID, key string, amount float64) (float64, error) {
	var result float64
	err := o.DB.Transaction(func(tx *gorm.DB) error {
		var kv models.DataKV
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("store_id = ? AND key = ?", storeID, key).First(&kv).Error
		cur := 0.0
		if err == nil {
			_ = json.Unmarshal(kv.Value, &cur)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		cur += amount
		result = cur
		b, _ := json.Marshal(cur)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return tx.Create(&models.DataKV{StoreID: storeID, Key: key, Value: b}).Error
		}
		return tx.Model(&kv).Update("value", models.JSONB(b)).Error
	})
	if err == nil {
		publishKV(storeID, key, formatFloat(result))
	}
	return result, err
}

func (o DataStoreOps) KVDelete(storeID, key string) error {
	err := o.DB.Unscoped().Where("store_id = ? AND key = ?", storeID, key).
		Delete(&models.DataKV{}).Error
	if err == nil {
		hub.Data.Publish(hub.DataChange{StoreID: storeID, Key: key, Deleted: true})
	}
	return err
}

// ── collection ───────────────────────────────────────────────────

func (o DataStoreOps) RecordAppend(storeID string, record json.RawMessage) (string, error) {
	rec := models.DataRecord{StoreID: storeID, Record: models.JSONB(record)}
	if err := o.DB.Create(&rec).Error; err != nil {
		return "", err
	}
	o.publishCount(storeID)
	return rec.ID.String(), nil
}

func (o DataStoreOps) RecordQuery(storeID string, filter json.RawMessage, limit int) ([]json.RawMessage, error) {
	q := o.DB.Where("store_id = ?", storeID)
	if len(filter) > 0 {
		q = q.Where("record @> ?", string(filter)) // jsonb containment
	}
	var recs []models.DataRecord
	if err := q.Order("created_at asc").Limit(limit).Find(&recs).Error; err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(recs))
	for _, r := range recs {
		out = append(out, injectRecordID(json.RawMessage(r.Record), r.ID.String()))
	}
	return out, nil
}

func (o DataStoreOps) RecordUpdate(storeID, recordID string, record json.RawMessage) error {
	res := o.DB.Model(&models.DataRecord{}).
		Where("id = ? AND store_id = ?", recordID, storeID).
		Update("record", models.JSONB(record))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("record %s not found", recordID)
	}
	o.publishCount(storeID)
	return nil
}

func (o DataStoreOps) RecordDelete(storeID, recordID string) error {
	res := o.DB.Unscoped().Where("id = ? AND store_id = ?", recordID, storeID).
		Delete(&models.DataRecord{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("record %s not found", recordID)
	}
	o.publishCount(storeID)
	return nil
}

func (o DataStoreOps) RecordCount(storeID string) (int64, error) {
	var n int64
	err := o.DB.Model(&models.DataRecord{}).Where("store_id = ?", storeID).Count(&n).Error
	return n, err
}

func (o DataStoreOps) RecordClear(storeID string) error {
	err := o.DB.Unscoped().Where("store_id = ?", storeID).Delete(&models.DataRecord{}).Error
	if err == nil {
		zero := int64(0)
		hub.Data.Publish(hub.DataChange{StoreID: storeID, Count: &zero, Deleted: true})
	}
	return err
}

// ── text ─────────────────────────────────────────────────────────

func (o DataStoreOps) TextGet(storeID string) (string, error) {
	v, ok, err := o.KVGet(storeID, textKey)
	if err != nil || !ok {
		return "", err
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s, nil
	}
	return string(v), nil
}

func (o DataStoreOps) TextSet(storeID, text string) error {
	b, _ := json.Marshal(text)
	return o.KVSet(storeID, textKey, b)
}

func (o DataStoreOps) TextAppend(storeID, text string) (string, error) {
	var result string
	err := o.DB.Transaction(func(tx *gorm.DB) error {
		var kv models.DataKV
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("store_id = ? AND key = ?", storeID, textKey).First(&kv).Error
		cur := ""
		if err == nil {
			_ = json.Unmarshal(kv.Value, &cur)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		cur += text
		result = cur
		b, _ := json.Marshal(cur)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return tx.Create(&models.DataKV{StoreID: storeID, Key: textKey, Value: b}).Error
		}
		return tx.Model(&kv).Update("value", models.JSONB(b)).Error
	})
	if err == nil {
		publishKV(storeID, textKey, result)
	}
	return result, err
}

// injectRecordID adds a read-only "_id" field to a record object so downstream
// nodes can update/delete it.
func injectRecordID(rec json.RawMessage, id string) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(rec, &m); err != nil {
		return rec
	}
	m["_id"] = id
	b, _ := json.Marshal(m)
	return b
}
