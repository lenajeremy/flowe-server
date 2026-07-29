package models

// DataStore is a named persistence container a workflow (or the whole account)
// can read/write. Kind fixes the shape (key-value, collection, or text); Scope
// fixes the lifetime and reach (a single run, one workflow across runs, or the
// whole account across workflows). Schema is optional — nil means schemaless.
type DataStore struct {
	BaseModel
	UserID string `json:"user_id"     gorm:"type:uuid;not null;index;uniqueIndex:idx_store_owner_name"`
	// WorkflowID scopes run/workflow stores; empty for account scope. Part of
	// the unique-name index so the same name can exist per workflow.
	WorkflowID string `json:"workflow_id" gorm:"uniqueIndex:idx_store_owner_name"`
	Name       string `json:"name"        gorm:"not null;uniqueIndex:idx_store_owner_name"`
	Kind       string `json:"kind"        gorm:"type:varchar(20);not null"` // kv | collection | text
	Scope      string `json:"scope"       gorm:"type:varchar(20);not null"` // run | workflow | account
	// Schema is a JSON array of {name,type} column defs for typed collections;
	// nil/absent means schemaless. Ignored for kv/text.
	Schema JSONB `json:"schema"      gorm:"type:jsonb"`
}

// DataKV is one key→value entry for a kv store. Text stores reuse this with a
// single reserved key. Value is arbitrary JSON.
type DataKV struct {
	BaseModel
	StoreID string `json:"store_id" gorm:"type:uuid;not null;uniqueIndex:idx_kv_store_key"`
	Key     string `json:"key"      gorm:"not null;uniqueIndex:idx_kv_store_key"`
	Value   JSONB  `json:"value"    gorm:"type:jsonb"`
}

// DataRecord is one row of a collection store. Record is an arbitrary JSON
// object; its own BaseModel ID is the record's stable id.
type DataRecord struct {
	BaseModel
	StoreID string `json:"store_id" gorm:"type:uuid;not null;index"`
	Record  JSONB  `json:"record"   gorm:"type:jsonb;not null;default:'{}'"`
}
