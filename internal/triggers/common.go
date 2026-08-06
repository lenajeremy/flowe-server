package triggers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"
)

// httpClient is shared by every adapter. The timeout is deliberate: a
// registration call that hangs would otherwise hold a user's save request open
// until the reverse proxy gives up on it.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// HookURL is the address a provider posts to. It has to be the public origin —
// a provider cannot reach localhost, which is why trigger registration only
// really works against a deployed instance.
//
// Reuses OAUTH_REDIRECT_BASE rather than inventing a second base-URL variable:
// it is already the "where does the outside world find us" setting, and having
// two would let them disagree.
func HookURL(provider, triggerID string) string {
	base := os.Getenv("PUBLIC_BASE_URL")
	if base == "" {
		base = os.Getenv("OAUTH_REDIRECT_BASE")
	}
	if base == "" {
		base = "http://localhost:8080"
	}
	u := strings.TrimRight(base, "/") + "/api/hooks/" + provider
	if triggerID != "" {
		u += "/" + triggerID
	}
	return u
}

// randomSecret mints a signing secret for a hook we are about to register.
func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("could not generate a signing secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Matches reports whether an event survives a trigger's filters.
//
// Filtering here — before a run is admitted — is the difference between a busy
// repo costing nothing and costing a workflow run per commit. A branch node
// would reach the same verdict after paying for the run.
//
// Rules are deliberately forgiving: an unset or blank filter matches
// everything, comparison is case-insensitive, and a filter naming a field the
// event does not carry does not match (a filter that silently passed would be
// worse than one that visibly never fires).
func Matches(t *models.IntegrationTrigger, ev Event) bool {
	if len(t.Filters) == 0 {
		return true
	}
	var filters map[string]any
	if err := json.Unmarshal(t.Filters, &filters); err != nil {
		return true // an unreadable filter must not silently swallow every event
	}
	for key, raw := range filters {
		want := strings.TrimSpace(fmt.Sprint(raw))
		if want == "" || raw == nil {
			continue
		}
		got, ok := ev.Data[key]
		if !ok {
			return false
		}
		if !valueMatches(got, want) {
			return false
		}
	}
	return true
}

// valueMatches compares one filter value against one event field. A list field
// (labels) matches when any element matches, which is what "has label bug"
// means to a person.
func valueMatches(got any, want string) bool {
	switch v := got.(type) {
	case []string:
		for _, s := range v {
			if strings.EqualFold(strings.TrimSpace(s), want) {
				return true
			}
		}
		return false
	case []any:
		for _, s := range v {
			if strings.EqualFold(strings.TrimSpace(fmt.Sprint(s)), want) {
				return true
			}
		}
		return false
	default:
		return strings.EqualFold(strings.TrimSpace(fmt.Sprint(got)), want)
	}
}
