package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"

	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/database/models"

	"github.com/gin-gonic/gin"
)

// GET /api/apikeys
func (h *WorkflowHandler) ListApiKeys(c *gin.Context) {
	var keys []models.ApiKey
	h.db.DB.Select("id, name, key_prefix, last_used_at, created_at").
		Where("user_id = ?", auth.UserID(c)).Find(&keys)
	c.JSON(http.StatusOK, keys)
}

// POST /api/apikeys
func (h *WorkflowHandler) CreateApiKey(c *gin.Context) {
	var body struct {
		Name string `json:"name"`
	}
	c.BindJSON(&body)

	// Generate random key: "wf_" + 32 random hex chars
	randBytes := make([]byte, 16)
	rand.Read(randBytes)
	rawKey := "wf_" + hex.EncodeToString(randBytes)

	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(rawKey)))

	key := models.ApiKey{
		UserID:    auth.UserID(c),
		Name:      body.Name,
		KeyHash:   hash,
		KeyPrefix: rawKey[:11], // "wf_" + first 8 chars
	}
	h.db.DB.Create(&key)

	slog.InfoContext(c.Request.Context(), "api key created",
		"user_id", key.UserID, "key_id", key.ID.String(), "name", key.Name)

	// Return the raw key ONCE (never stored again)
	c.JSON(http.StatusCreated, gin.H{
		"id":     key.ID,
		"name":   key.Name,
		"key":    rawKey,
		"prefix": key.KeyPrefix,
	})
}

// DELETE /api/apikeys/:id
func (h *WorkflowHandler) DeleteApiKey(c *gin.Context) {
	h.db.DB.Where("user_id = ?", auth.UserID(c)).Delete(&models.ApiKey{}, "id = ?", c.Param("id"))
	slog.InfoContext(c.Request.Context(), "api key deleted",
		"user_id", auth.UserID(c), "key_id", c.Param("id"))
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}
