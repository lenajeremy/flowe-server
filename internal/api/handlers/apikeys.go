package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"

	"workflow-ai/server/internal/database/models"

	"github.com/gin-gonic/gin"
)

// GET /api/apikeys
func (h *WorkflowHandler) ListApiKeys(c *gin.Context) {
	var keys []models.ApiKey
	h.db.DB.Select("id, name, key_prefix, last_used_at, created_at").Find(&keys)
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
		Name:      body.Name,
		KeyHash:   hash,
		KeyPrefix: rawKey[:11], // "wf_" + first 8 chars
	}
	h.db.DB.Create(&key)

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
	h.db.DB.Delete(&models.ApiKey{}, "id = ?", c.Param("id"))
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}
