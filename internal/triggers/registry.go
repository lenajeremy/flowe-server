// Package triggers turns "something happened in a connected tool" into a
// workflow run.
//
// Providers disagree about almost everything here. GitHub wants a hook created
// per repository and signs each delivery; Slack registers nothing at all and
// posts every workspace's events to one URL; Gmail has no webhook worth the
// name unless you stand up a Pub/Sub topic, so we ask it instead. The product
// promise is the same in every case — "run this when X happens" — so the
// difference is confined to an adapter, and the rest of the system sees one
// normalized Event whichever way it arrived.
package triggers

import (
	"context"
	"net/http"
	"time"

	"workflow-ai/server/internal/database/models"
)

// Delivery is how an adapter learns about events.
type Delivery string

const (
	Push Delivery = "push"
	Poll Delivery = "poll"
)

// Event is what every adapter produces, whichever way it found out.
type Event struct {
	// Key deduplicates. It must be the provider's own identifier for this
	// delivery (X-GitHub-Delivery, Slack's event_id, a Gmail message id) — never
	// a hash of the body, which differs between two deliveries of the same event.
	Key string
	// Type is the registry event id that matched, e.g. "pull_request.opened".
	Type string
	// ResourceID is the repo/channel/mailbox the event came from, used to route
	// an app-level delivery to the right trigger.
	ResourceID string
	// ScopeID is the installation or workspace the event came from — a GitHub App
	// installation id, a Slack team_id. On an app-level webhook this is the only
	// thing that distinguishes two accounts that happen to name a resource the
	// same way, so routing prefers it over the resource name.
	ScopeID    string
	OccurredAt time.Time
	// Data is the payload handed to the workflow. Adapters flatten the useful
	// parts to the top level so a template reads {{trigger.output.title}} rather
	// than three levels of provider nesting.
	Data map[string]any
	// Lifecycle is set for provider control-plane events rather than user data:
	// an app installation was suspended, repository access changed, or a user
	// revoked authorization. These events update trigger/connection health and
	// must never be injected into a workflow or consume a run.
	Lifecycle *LifecycleEvent
}

// LifecycleAction is provider-neutral so every app-level adapter can report
// the same small set of access changes without teaching the HTTP handler the
// shape or vocabulary of each provider's payload.
type LifecycleAction string

const (
	LifecycleScopeRemoved         LifecycleAction = "scope.removed"
	LifecycleScopeSuspended       LifecycleAction = "scope.suspended"
	LifecycleScopeRestored        LifecycleAction = "scope.restored"
	LifecycleResourcesAdded       LifecycleAction = "resources.added"
	LifecycleResourcesRemoved     LifecycleAction = "resources.removed"
	LifecycleAuthorizationRevoked LifecycleAction = "authorization.revoked"
)

type LifecycleEvent struct {
	Action LifecycleAction
	// ResourceIDs is populated for selected-resource installations whose access
	// changed. GitHub supplies repository full names here.
	ResourceIDs []string
	// AccountName identifies the human authorization that was revoked. It is
	// deliberately a provider account name, not one of our user ids.
	AccountName string
	// AccountID is the provider's immutable account identifier. Names can be
	// changed; adapters should populate both when the provider supplies them so
	// revocations can still find older connections safely.
	AccountID string
}

// EventSpec describes one subscribable event.
//
// It is the single source of truth for three surfaces that used to drift apart:
// the dropdown in the config panel, the enum in the AI builder's catalog, and
// the dispatch inside the adapter. A new event is one entry here.
type EventSpec struct {
	ID    string `json:"id"`    // "pull_request.opened"
	Label string `json:"label"` // "Pull request opened"
	// ResourceKind names what the trigger must be pointed at ("repo",
	// "channel"), and matches the kinds the resource pickers already understand.
	// Empty means the event is account-wide.
	ResourceKind string `json:"resource_kind,omitempty"`
	// Filters are the narrowing fields offered for this event.
	Filters []FilterSpec `json:"filters,omitempty"`
	// Sample is what {{trigger.output}} will look like — shown in the UI and
	// given to the builder so downstream nodes can be wired without a test run.
	Sample map[string]any `json:"sample,omitempty"`
}

type FilterSpec struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Placeholder string `json:"placeholder,omitempty"`
	// ResourceKind, when set, means this filter is chosen from a list the
	// provider can enumerate ("branch", "user") rather than typed. The list is
	// scoped to the trigger's own resource — a repository's branches, not every
	// branch the account can see. Empty means a free-text field.
	ResourceKind string `json:"resource_kind,omitempty"`
}

// Adapter is what every provider implements.
type Adapter interface {
	Provider() string
	Events() []EventSpec
	Delivery() Delivery
}

// Pusher is implemented by providers that deliver events to us.
//
// Register is allowed to be a no-op: Slack's subscription lives in the app
// manifest, so there is nothing per-trigger to create. The interface has to
// tolerate that rather than assume every push provider registers per resource,
// which is exactly why Slack is in the first batch.
type Pusher interface {
	Adapter
	Register(ctx context.Context, conn Conn, t *models.IntegrationTrigger) (Registration, error)
	Unregister(ctx context.Context, conn Conn, t *models.IntegrationTrigger) error
	// Renew extends a subscription that expires. Returning a nil time means this
	// provider's hooks do not expire and the renewal sweep should leave it alone.
	Renew(ctx context.Context, conn Conn, t *models.IntegrationTrigger) (*time.Time, error)
	// Handshake answers a provider's URL-verification challenge before any
	// trigger exists to verify against — Slack's url_verification, Dropbox's
	// challenge echo. Returning handled=false means this is a normal delivery.
	Handshake(r *http.Request, body []byte) (status int, response []byte, handled bool)
	// Verify authenticates the raw body. An error here means the request never
	// touches a workflow. t is nil for app-level providers, which authenticate
	// against an app secret rather than a per-trigger one.
	Verify(r *http.Request, body []byte, t *models.IntegrationTrigger) error
	// Parse turns one delivery into zero or more events. Zero is the common case
	// and not an error: a repo hook subscribed to "pull_request" hears every
	// action on every PR, and all we wanted was "opened".
	Parse(r *http.Request, body []byte) ([]Event, error)
}

// Poller is implemented by providers we have to ask.
//
// The returned cursor is only persisted once the events have been admitted, so
// an adapter may assume that returning events and then crashing means it will
// be asked for the same window again.
type Poller interface {
	Adapter
	Poll(ctx context.Context, conn Conn, t *models.IntegrationTrigger) (events []Event, cursor string, err error)
}

// Conn is the credential an adapter acts with, resolved fresh (and refreshed if
// needed) by the caller. Adapters never read tokens from the database
// themselves — that is how a stale token turns into a silently dead trigger.
type Conn struct {
	AccessToken string
	// WorkspaceID is the provider's own account/workspace identifier: the Slack
	// team_id, the Shopify shop domain. Used to route app-level deliveries.
	WorkspaceID   string
	WorkspaceName string
}

var registry = map[string]Adapter{}

// Register adds an adapter. Called from each provider's init().
func Register(a Adapter) { registry[a.Provider()] = a }

// Get returns the adapter for a provider, or nil.
func Get(provider string) Adapter { return registry[provider] }

// Pusher returns the adapter as a Pusher, or nil if this provider is poll-only.
func GetPusher(provider string) Pusher {
	p, _ := registry[provider].(Pusher)
	return p
}

// GetPoller returns the adapter as a Poller, or nil if this provider is push-only.
func GetPoller(provider string) Poller {
	p, _ := registry[provider].(Poller)
	return p
}

// Catalog lists every provider and its events, newest surface first. It backs
// the config panel's dropdowns and the AI builder's enum, so neither can name
// an event no adapter implements.
func Catalog() map[string][]EventSpec {
	out := make(map[string][]EventSpec, len(registry))
	for name, a := range registry {
		out[name] = a.Events()
	}
	return out
}

// EventIDs lists the valid event ids for a provider.
func EventIDs(provider string) []string {
	a := registry[provider]
	if a == nil {
		return nil
	}
	ids := make([]string, 0, len(a.Events()))
	for _, e := range a.Events() {
		ids = append(ids, e.ID)
	}
	return ids
}

// Supports reports whether a provider/event pair is real. Every write path
// checks this before it stores a trigger, so a typo fails at creation instead
// of becoming a hook that silently never matches.
func Supports(provider, event string) bool {
	for _, id := range EventIDs(provider) {
		if id == event {
			return true
		}
	}
	return false
}

// Registration is what a provider hands back when a trigger is set up.
//
// For providers with an app-level webhook there is no remote object and no
// per-trigger secret; what matters is ScopeID, resolved once here so that every
// later delivery can be matched to this trigger exactly.
type Registration struct {
	RemoteID  string
	Secret    string
	ScopeID   string
	ExpiresAt *time.Time
}
