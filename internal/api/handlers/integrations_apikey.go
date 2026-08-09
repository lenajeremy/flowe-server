package handlers

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"workflow-ai/server/internal/database/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Providers that authenticate with a long-lived API key rather than OAuth.
//
// These have no authorize URL and no token exchange: the user pastes a key and we
// store it in the same IntegrationConnection row an OAuth provider would use, so
// everything downstream — encryption at rest, the executor's credential lookup,
// the connections page, disconnect — works unchanged.
//
// They are deliberately kept out of oauthProviders. Anything that guards on
// "is this a provider we know" uses knownProvider instead.

type apiKeyProvider struct {
	name string
	// label is what the connect card calls it.
	label string
	// keyPrefix, when set, is checked before saving. Most of these services mint
	// keys with a recognisable prefix, and catching a pasted wrong-service key
	// here is far kinder than a 401 on the first workflow run.
	keyPrefix string
	// hint tells the user where to find the key.
	hint string
	// verifyURL is a cheap authenticated GET used to prove the key works before
	// we store it. Empty means store without checking.
	verifyURL string
	// header is the auth header name; empty means Authorization: Bearer.
	header string
}

var apiKeyProviders = map[string]apiKeyProvider{
	"resend": {
		name: "resend", label: "Resend", keyPrefix: "re_",
		hint:      "resend.com/api-keys",
		verifyURL: "https://api.resend.com/domains",
	},
	"sendgrid": {
		name: "sendgrid", label: "SendGrid", keyPrefix: "SG.",
		hint:      "app.sendgrid.com → Settings → API Keys",
		verifyURL: "https://api.sendgrid.com/v3/user/profile",
	},
	"kit": {
		name: "kit", label: "Kit",
		hint:      "Kit → Settings → Developer → create a V4 API key (a v3 key will not work)",
		verifyURL: "https://api.kit.com/v4/account",
		header:    "X-Kit-Api-Key",
	},
	"granola": {
		name: "granola", label: "Granola", keyPrefix: "grn_",
		hint:      "Granola → Settings → Workspace → API access (Business or Enterprise plan)",
		verifyURL: "https://public-api.granola.ai/v1/notes?limit=1",
	},
}

// knownProvider reports whether a provider name is one we support at all,
// through either auth style.
func knownProvider(p string) bool {
	if _, ok := oauthProviders[p]; ok {
		return true
	}
	_, ok := apiKeyProviders[p]
	return ok
}

// SetIntegrationKey stores an API key for a key-authenticated provider.
func (h *WorkflowHandler) SetIntegrationKey(c *gin.Context) {
	prov, ok := apiKeyProviders[c.Param("provider")]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "this provider does not use an API key"})
		return
	}
	var body struct {
		APIKey string `json:"api_key"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected an api_key field"})
		return
	}
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "the API key is empty"})
		return
	}
	if prov.keyPrefix != "" && !strings.HasPrefix(key, prov.keyPrefix) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("that does not look like a %s key — they start with %q",
				prov.label, prov.keyPrefix),
		})
		return
	}
	if err := verifyAPIKey(prov, key); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	userID, orgID := currentUserID(c), currentOrgID(c)
	conn := models.IntegrationConnection{
		UserID:         userID,
		OrganizationID: orgID,
		Provider:       prov.name,
		AccessToken:    key,
		Scope:          "api_key",
	}
	err := h.withHostedAuthorityLock(c.Request.Context(), orgID, userID, func(connection *gorm.DB) error {
		return connection.Transaction(func(tx *gorm.DB) error {
			if err := tx.Unscoped().Where("organization_id = ? AND user_id = ? AND provider = ?",
				orgID, userID, prov.name).Delete(&models.IntegrationConnection{}).Error; err != nil {
				return err
			}
			return tx.Create(&conn).Error
		})
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not save the API key"})
		return
	}
	// Never log the key itself, only that one arrived.
	slog.InfoContext(c.Request.Context(), "integration api key saved",
		"provider", prov.name, "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"connected": prov.name})
}

// verifyAPIKey spends one cheap request to prove the key works, so a typo is
// caught at paste time instead of inside a workflow run at 3am.
func verifyAPIKey(prov apiKeyProvider, key string) error {
	if prov.verifyURL == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, prov.verifyURL, nil)
	if err != nil {
		return fmt.Errorf("could not check the key")
	}
	if prov.header != "" {
		req.Header.Set(prov.header, key)
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Accept", "application/json")

	if _, err := doOAuthRequest(req); err != nil {
		// The upstream body can contain the key back; keep it out of the response.
		return fmt.Errorf("%s rejected that key — check it was copied in full and has not been revoked",
			prov.label)
	}
	return nil
}
