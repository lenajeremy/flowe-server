package handlers

import (
	"encoding/json"
	"net/http"

	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"

	"github.com/gin-gonic/gin"
)

var validKinds = map[string]bool{"kv": true, "collection": true, "text": true}
var validScopes = map[string]bool{"run": true, "workflow": true, "account": true}

func (h *WorkflowHandler) ops() database.DataStoreOps {
	return database.DataStoreOps{DB: h.db.DB}
}

// loadOwnedStore fetches a store owned by the caller, writing a 404 and
// returning ok=false otherwise.
func (h *WorkflowHandler) loadOwnedStore(c *gin.Context, id string) (*models.DataStore, bool) {
	var st models.DataStore
	if err := h.db.DB.First(&st, "id = ? AND user_id = ?", id, auth.UserID(c)).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "store not found"})
		return nil, false
	}
	return &st, true
}

// GET /api/data-stores?workflow_id=  — the caller's stores. With workflow_id,
// returns that workflow's stores plus all account-scoped stores (the set a node
// in that workflow can target).
func (h *WorkflowHandler) ListDataStores(c *gin.Context) {
	q := h.db.DB.Where("user_id = ?", auth.UserID(c))
	if wf := c.Query("workflow_id"); wf != "" {
		// Nested Where forces parentheses: user_id = ? AND (workflow_id = ? OR
		// scope = 'account') — never let the OR escape the owner filter.
		q = q.Where(h.db.DB.Where("workflow_id = ?", wf).Or("scope = ?", "account"))
	}
	var stores []models.DataStore
	q.Order("created_at desc").Find(&stores)
	c.JSON(http.StatusOK, stores)
}

// POST /api/data-stores
func (h *WorkflowHandler) CreateDataStore(c *gin.Context) {
	var body struct {
		Name       string          `json:"name"`
		Kind       string          `json:"kind"`
		Scope      string          `json:"scope"`
		WorkflowID string          `json:"workflow_id"`
		Schema     json.RawMessage `json:"schema"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if !validKinds[body.Kind] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be kv, collection, or text"})
		return
	}
	if !validScopes[body.Scope] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scope must be run, workflow, or account"})
		return
	}
	if err := executor.ValidateSchema(body.Schema); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// run/workflow stores belong to a workflow the caller owns; account stores
	// are global and carry no workflow id.
	if body.Scope == "account" {
		body.WorkflowID = ""
	} else {
		if body.WorkflowID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "workflow_id is required for run/workflow scope"})
			return
		}
		var wf models.Workflow
		if err := h.db.DB.First(&wf, "id = ? AND user_id = ?", body.WorkflowID, auth.UserID(c)).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
			return
		}
	}

	store := models.DataStore{
		UserID:     auth.UserID(c),
		WorkflowID: body.WorkflowID,
		Name:       body.Name,
		Kind:       body.Kind,
		Scope:      body.Scope,
		Schema:     models.JSONB(body.Schema),
	}
	if err := h.db.DB.Create(&store).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "a store with that name already exists in this scope"})
		return
	}
	c.JSON(http.StatusCreated, store)
}

// dataStoresForAI returns the stores a workflow's data nodes can target
// (that workflow's own + all account-scoped), as JSON for the AI builder.
func (h *WorkflowHandler) dataStoresForAI(userID, workflowID string) string {
	q := h.db.DB.Where("user_id = ?", userID)
	if workflowID != "" {
		q = q.Where(h.db.DB.Where("workflow_id = ?", workflowID).Or("scope = ?", "account"))
	} else {
		q = q.Where("scope = ?", "account")
	}
	var stores []models.DataStore
	q.Order("created_at desc").Find(&stores)

	type aiStore struct {
		ID     string          `json:"id"`
		Name   string          `json:"name"`
		Kind   string          `json:"kind"`
		Scope  string          `json:"scope"`
		Schema json.RawMessage `json:"schema,omitempty"`
	}
	out := make([]aiStore, 0, len(stores))
	for _, s := range stores {
		out = append(out, aiStore{ID: s.ID.String(), Name: s.Name, Kind: s.Kind, Scope: s.Scope, Schema: json.RawMessage(s.Schema)})
	}
	b, _ := json.Marshal(map[string]any{
		"stores": out,
		"note":   "Use these ids as dataStoreId. If none fits, propose one with create_data_store.",
	})
	return string(b)
}

// GET /api/data-stores/:id
func (h *WorkflowHandler) GetDataStore(c *gin.Context) {
	st, ok := h.loadOwnedStore(c, c.Param("id"))
	if !ok {
		return
	}
	c.JSON(http.StatusOK, st)
}

// PATCH /api/data-stores/:id  — rename or adjust the schema.
func (h *WorkflowHandler) UpdateDataStore(c *gin.Context) {
	st, ok := h.loadOwnedStore(c, c.Param("id"))
	if !ok {
		return
	}
	var body struct {
		Name   *string         `json:"name"`
		Schema json.RawMessage `json:"schema"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]any{}
	if body.Name != nil && *body.Name != "" {
		updates["name"] = *body.Name
	}
	if body.Schema != nil {
		if err := executor.ValidateSchema(body.Schema); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		updates["schema"] = models.JSONB(body.Schema)
	}
	if len(updates) > 0 {
		h.db.DB.Model(st).Updates(updates)
	}
	h.db.DB.First(st, "id = ?", st.ID)
	c.JSON(http.StatusOK, st)
}

// DELETE /api/data-stores/:id  — removes the store and all its data.
func (h *WorkflowHandler) DeleteDataStore(c *gin.Context) {
	st, ok := h.loadOwnedStore(c, c.Param("id"))
	if !ok {
		return
	}
	id := st.ID.String()
	h.db.DB.Unscoped().Where("store_id = ?", id).Delete(&models.DataKV{})
	h.db.DB.Unscoped().Where("store_id = ?", id).Delete(&models.DataRecord{})
	h.db.DB.Unscoped().Delete(&models.DataStore{}, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// GET /api/data-stores/:id/entries  — the store's data, shaped by kind.
func (h *WorkflowHandler) ListDataEntries(c *gin.Context) {
	st, ok := h.loadOwnedStore(c, c.Param("id"))
	if !ok {
		return
	}
	id := st.ID.String()
	switch st.Kind {
	case "kv":
		var kvs []models.DataKV
		h.db.DB.Where("store_id = ?", id).Order("key asc").Find(&kvs)
		c.JSON(http.StatusOK, gin.H{"kind": "kv", "entries": kvs})
	case "collection":
		var recs []models.DataRecord
		h.db.DB.Where("store_id = ?", id).Order("created_at asc").Limit(500).Find(&recs)
		c.JSON(http.StatusOK, gin.H{"kind": "collection", "entries": recs})
	case "text":
		txt, _ := h.ops().TextGet(id)
		c.JSON(http.StatusOK, gin.H{"kind": "text", "value": txt})
	}
}

// PUT /api/data-stores/:id/entries  — manual edit from the Data panel.
// kv:         {"key": "...", "value": <json>}
// text:       {"value": "..."}
// collection: {"record": {...}} to append, or {"id": "...", "record": {...}} to update
func (h *WorkflowHandler) PutDataEntry(c *gin.Context) {
	st, ok := h.loadOwnedStore(c, c.Param("id"))
	if !ok {
		return
	}
	id := st.ID.String()
	var body struct {
		Key    string          `json:"key"`
		Value  json.RawMessage `json:"value"`
		ID     string          `json:"id"`
		Record json.RawMessage `json:"record"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var err error
	switch st.Kind {
	case "kv":
		if body.Key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key is required"})
			return
		}
		if len(body.Value) == 0 || !json.Valid(body.Value) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "value must be valid JSON"})
			return
		}
		if err := executor.CheckDataSize(body.Value); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		err = h.ops().KVSet(id, body.Key, body.Value)
	case "text":
		// Accept a JSON string ({"value":"…"} — including the empty string,
		// which clears the text) or fall back to the raw bytes.
		var s string
		if unmarshalErr := json.Unmarshal(body.Value, &s); unmarshalErr != nil {
			s = string(body.Value)
		}
		if err := executor.CheckDataSize([]byte(s)); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		err = h.ops().TextSet(id, s)
	case "collection":
		// Panel edits obey the same rules as workflow writes: JSON object,
		// size cap, and the store's schema (when typed).
		var m map[string]any
		if json.Unmarshal(body.Record, &m) != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "record must be a JSON object"})
			return
		}
		if err := executor.CheckDataSize(body.Record); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := executor.ValidateRecord(json.RawMessage(st.Schema), body.Record); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if body.ID != "" {
			err = h.ops().RecordUpdate(id, body.ID, body.Record)
		} else {
			var newID string
			newID, err = h.ops().RecordAppend(id, body.Record)
			if err == nil {
				c.JSON(http.StatusOK, gin.H{"id": newID})
				return
			}
		}
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// DELETE /api/data-stores/:id/entries/:entry  — kv key or collection record id.
func (h *WorkflowHandler) DeleteDataEntry(c *gin.Context) {
	st, ok := h.loadOwnedStore(c, c.Param("id"))
	if !ok {
		return
	}
	id, entry := st.ID.String(), c.Param("entry")
	var err error
	switch st.Kind {
	case "kv":
		err = h.ops().KVDelete(id, entry)
	case "collection":
		err = h.ops().RecordDelete(id, entry)
	case "text":
		err = h.ops().TextSet(id, "")
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// POST /api/data-stores/:id/clear  — wipe all data, keep the store.
func (h *WorkflowHandler) ClearDataStore(c *gin.Context) {
	st, ok := h.loadOwnedStore(c, c.Param("id"))
	if !ok {
		return
	}
	id := st.ID.String()
	switch st.Kind {
	case "collection":
		_ = h.ops().RecordClear(id)
	default:
		h.db.DB.Unscoped().Where("store_id = ?", id).Delete(&models.DataKV{})
	}
	c.JSON(http.StatusOK, gin.H{"status": "cleared"})
}
