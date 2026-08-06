package triggers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/githubapp"
)

// GitHub, delivered through the App's own webhook.
//
// Our GitHub connection is a GitHub App, not a classic OAuth App, and an App
// token cannot create per-repository hooks — `POST /repos/{repo}/hooks` answers
// "Resource not accessible by integration" regardless of the OAuth scope we
// ask for, because an App's permissions govern it rather than the scope string.
// Measured against a live connection before this was written.
//
// So GitHub takes the same shape as Slack: one webhook URL configured once in
// the App's settings, one shared signing secret, and every installation's
// events arriving at that single endpoint. Registration is a no-op; the payload
// is the routing information. This is the native model for a GitHub App, and it
// removes per-repo hook creation, renewal and cleanup entirely.
//
// What Register still does is check that the exact repository appears in one
// of the user's Fernary App installations. Merely being able to see a repo as a
// GitHub user is insufficient: the App may be installed on only selected repos.
//
// One wrinkle worth naming: GitHub subscribes at the level of "pull_request",
// not "pull_request opened". The App hears every action on every PR — opened,
// labeled, synchronized, closed — so Parse drops what the trigger did not ask
// for. That filtering happens before a run is admitted; doing it in a branch
// node instead would spend a workflow run to decide it wasn't interested.

func init() { Register(githubAdapter{}) }

type githubAdapter struct{}

func (githubAdapter) Provider() string   { return "github" }
func (githubAdapter) Delivery() Delivery { return Push }

func (githubAdapter) Events() []EventSpec {
	return []EventSpec{
		{
			ID: "pull_request.opened", Label: "Pull request opened", ResourceKind: "repo",
			Filters: []FilterSpec{
				{Key: "base", Label: "Target branch", Placeholder: "main", ResourceKind: "branch"},
				{Key: "author", Label: "Opened by", Placeholder: "octocat", ResourceKind: "user"},
			},
			Sample: map[string]any{
				"number": 42, "title": "Fix the retry loop", "author": "octocat",
				"base": "main", "head": "fix-retries", "url": "https://github.com/o/r/pull/42",
			},
		},
		{
			ID: "pull_request.merged", Label: "Pull request merged", ResourceKind: "repo",
			Filters: []FilterSpec{{Key: "base", Label: "Target branch", Placeholder: "main", ResourceKind: "branch"}},
		},
		{
			ID: "issues.opened", Label: "Issue opened", ResourceKind: "repo",
			Filters: []FilterSpec{{Key: "label", Label: "Has label", Placeholder: "bug"}},
			Sample: map[string]any{
				"number": 17, "title": "Crash on empty input", "author": "octocat",
				"url": "https://github.com/o/r/issues/17",
			},
		},
		{ID: "issue_comment.created", Label: "Comment on an issue or PR", ResourceKind: "repo"},
		{
			ID: "push", Label: "Commits pushed", ResourceKind: "repo",
			Filters: []FilterSpec{{Key: "branch", Label: "Branch", Placeholder: "main", ResourceKind: "branch"}},
		},
		{ID: "release.published", Label: "Release published", ResourceKind: "repo"},
	}
}

// githubHookEvents maps our event ids to the coarser event names GitHub
// actually subscribes to. Several of ours collapse onto one of theirs.
var githubHookEvents = map[string]string{
	"pull_request.opened":   "pull_request",
	"pull_request.merged":   "pull_request",
	"issues.opened":         "issues",
	"issue_comment.created": "issue_comment",
	"push":                  "push",
	"release.published":     "release",
}

// Register creates nothing at GitHub — the App's webhook is already configured
// and already receiving these events. What it does is refuse a trigger that
// would never work, or that the user has no business creating:
//
//   - an event we do not know how to parse,
//   - no repository chosen,
//   - a repository this App installation does not cover. A GitHub user may see
//     a public repository (or a private one through another grant) even when the
//     Fernary installation is limited to a different set of repositories.
func (a githubAdapter) Register(ctx context.Context, conn Conn, t *models.IntegrationTrigger) (Registration, error) {
	if _, ok := githubHookEvents[t.Event]; !ok {
		return Registration{}, fmt.Errorf("github: unknown event %q", t.Event)
	}
	if t.ResourceID == "" {
		return Registration{}, fmt.Errorf("github: no repository selected")
	}
	client := githubapp.NewClient(conn.AccessToken, httpClient)
	app, err := client.GetAppRegistration(ctx, strings.TrimSpace(os.Getenv("GITHUB_APP_SLUG")))
	if err != nil {
		return Registration{}, fmt.Errorf("github: could not verify the App's Permissions & events settings: %w", err)
	}
	if missing := app.MissingWebhookRequirements(); len(missing) > 0 {
		return Registration{}, fmt.Errorf("github: the Fernary App is missing required Permissions & events settings: %s",
			strings.Join(missing, ", "))
	}
	// Resolve which installation these events will arrive under, and refuse the
	// trigger if we cannot. A trigger with no installation id would match nothing
	// at delivery time, and a trigger that never fires with no explanation is
	// worse than one that fails while the user is still looking at the screen.
	installation, err := a.installationFor(ctx, conn, t.ResourceID)
	if err != nil {
		return Registration{}, err
	}
	if missing := installation.MissingWebhookRequirements(); len(missing) > 0 {
		return Registration{}, fmt.Errorf(
			"github: the installation on %s has not approved the App's updated permissions: %s",
			installation.Account.Login, strings.Join(missing, ", "))
	}

	// No RemoteID and no per-trigger Secret: there is no remote object to track,
	// and signatures are checked against the App's one shared secret.
	return Registration{ScopeID: strconv.FormatInt(installation.ID, 10)}, nil
}

// installationFor finds the App installation that covers a repository.
//
// This enumerates the repositories exposed by every accessible installation.
// It intentionally does not infer coverage from the owner: an installation on
// acme can be limited to acme/website and must not admit acme/payments.
func (a githubAdapter) installationFor(ctx context.Context, conn Conn, repo string) (*githubapp.Installation, error) {
	installation, err := githubapp.NewClient(conn.AccessToken, httpClient).InstallationForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	return installation, nil
}

// Unregister is a no-op for the same reason: nothing was created, and the App's
// webhook stays up for every other trigger.
func (githubAdapter) Unregister(context.Context, Conn, *models.IntegrationTrigger) error {
	return nil
}

// Renew is a no-op: GitHub hooks live until deleted.
func (githubAdapter) Renew(context.Context, Conn, *models.IntegrationTrigger) (*time.Time, error) {
	return nil, nil
}

// Handshake: GitHub has none. It sends a "ping" event on creation, which Parse
// turns into zero events, so the delivery is acknowledged and nothing runs.
func (githubAdapter) Handshake(*http.Request, []byte) (int, []byte, bool) {
	return 0, nil, false
}

// Verify checks the App's shared signing secret — the one entered in the
// GitHub App's Webhook settings and mirrored into GITHUB_WEBHOOK_SECRET.
//
// The trigger argument is unused: deliveries arrive at one app-level URL before
// we know which trigger (or how many) they belong to, so authentication cannot
// depend on a per-trigger value.
func (githubAdapter) Verify(r *http.Request, body []byte, _ *models.IntegrationTrigger) error {
	secret := os.Getenv("GITHUB_WEBHOOK_SECRET")
	if secret == "" {
		// Fail closed. An unset secret must never mean "accept anything" — that
		// would turn one missing env var into an open door to every workflow.
		return fmt.Errorf("github: GITHUB_WEBHOOK_SECRET is not configured")
	}
	got := r.Header.Get("X-Hub-Signature-256")
	if got == "" {
		return fmt.Errorf("github: request is not signed")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	// Constant time: a byte-by-byte compare leaks how much of a forged
	// signature was right, which is enough to construct one.
	if !hmac.Equal([]byte(got), []byte(want)) {
		return fmt.Errorf("github: signature does not match")
	}
	return nil
}

func (githubAdapter) Parse(r *http.Request, body []byte) ([]Event, error) {
	hookEvent := r.Header.Get("X-GitHub-Event")
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	if deliveryID == "" {
		return nil, fmt.Errorf("github: delivery has no X-GitHub-Delivery id")
	}

	var p struct {
		Action string `json:"action"`
		Ref    string `json:"ref"`
		Repo   struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		// Present on every delivery from a GitHub App. It is what tells two
		// accounts' identically-named repositories apart.
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		RepositoriesAdded   []githubRepository `json:"repositories_added"`
		RepositoriesRemoved []githubRepository `json:"repositories_removed"`
		Sender              struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"sender"`
		PullRequest *struct {
			Number  int    `json:"number"`
			Title   string `json:"title"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
			Merged  bool   `json:"merged"`
			User    struct {
				Login string `json:"login"`
			} `json:"user"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
			Head struct {
				Ref string `json:"ref"`
			} `json:"head"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		} `json:"pull_request"`
		Issue *struct {
			Number  int    `json:"number"`
			Title   string `json:"title"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
			User    struct {
				Login string `json:"login"`
			} `json:"user"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		} `json:"issue"`
		Comment *struct {
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
			User    struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"comment"`
		Release *struct {
			TagName string `json:"tag_name"`
			Name    string `json:"name"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
		} `json:"release"`
		Commits []struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("github: unreadable payload: %w", err)
	}

	ev := Event{Key: deliveryID, ResourceID: p.Repo.FullName, OccurredAt: time.Now().UTC()}
	if p.Installation.ID != 0 {
		ev.ScopeID = strconv.FormatInt(p.Installation.ID, 10)
	}

	switch {
	case hookEvent == "installation":
		if ev.ScopeID == "" {
			return nil, fmt.Errorf("github: installation event has no installation id")
		}
		switch p.Action {
		case "deleted":
			ev.Lifecycle = &LifecycleEvent{Action: LifecycleScopeRemoved}
		case "suspend":
			ev.Lifecycle = &LifecycleEvent{Action: LifecycleScopeSuspended}
		case "unsuspend":
			ev.Lifecycle = &LifecycleEvent{Action: LifecycleScopeRestored}
		default:
			// created and new_permissions_accepted do not repair a trigger that
			// was tied to a deleted installation. A reinstall gets a new id and
			// the trigger must be recreated against that exact installation.
			return nil, nil
		}

	case hookEvent == "installation_repositories":
		if ev.ScopeID == "" {
			return nil, fmt.Errorf("github: installation repositories event has no installation id")
		}
		switch p.Action {
		case "added":
			ev.Lifecycle = &LifecycleEvent{
				Action:      LifecycleResourcesAdded,
				ResourceIDs: githubRepositoryNames(p.RepositoriesAdded),
			}
		case "removed":
			ev.Lifecycle = &LifecycleEvent{
				Action:      LifecycleResourcesRemoved,
				ResourceIDs: githubRepositoryNames(p.RepositoriesRemoved),
			}
		default:
			return nil, nil
		}

	case hookEvent == "github_app_authorization":
		if p.Action != "revoked" {
			return nil, nil
		}
		if strings.TrimSpace(p.Sender.Login) == "" {
			return nil, fmt.Errorf("github: authorization revocation has no sender")
		}
		accountID := ""
		if p.Sender.ID > 0 {
			accountID = strconv.FormatInt(p.Sender.ID, 10)
		}
		ev.Lifecycle = &LifecycleEvent{
			Action:      LifecycleAuthorizationRevoked,
			AccountName: p.Sender.Login,
			AccountID:   accountID,
		}

	case hookEvent == "pull_request" && p.PullRequest != nil:
		switch {
		case p.Action == "opened":
			ev.Type = "pull_request.opened"
		case p.Action == "closed" && p.PullRequest.Merged:
			ev.Type = "pull_request.merged"
		default:
			return nil, nil // an action this trigger never asked for
		}
		ev.Data = map[string]any{
			"number": p.PullRequest.Number, "title": p.PullRequest.Title,
			"body": p.PullRequest.Body, "url": p.PullRequest.HTMLURL,
			"author": p.PullRequest.User.Login,
			"base":   p.PullRequest.Base.Ref, "head": p.PullRequest.Head.Ref,
			"labels": labelNames(p.PullRequest.Labels), "repo": p.Repo.FullName,
		}

	case hookEvent == "issues" && p.Issue != nil:
		if p.Action != "opened" {
			return nil, nil
		}
		ev.Type = "issues.opened"
		ev.Data = map[string]any{
			"number": p.Issue.Number, "title": p.Issue.Title, "body": p.Issue.Body,
			"url": p.Issue.HTMLURL, "author": p.Issue.User.Login,
			"labels": labelNames(p.Issue.Labels), "repo": p.Repo.FullName,
		}

	case hookEvent == "issue_comment" && p.Comment != nil:
		if p.Action != "created" {
			return nil, nil
		}
		ev.Type = "issue_comment.created"
		ev.Data = map[string]any{
			"body": p.Comment.Body, "url": p.Comment.HTMLURL,
			"author": p.Comment.User.Login, "repo": p.Repo.FullName,
		}
		if p.Issue != nil {
			ev.Data["number"] = p.Issue.Number
			ev.Data["title"] = p.Issue.Title
		}

	case hookEvent == "push":
		ev.Type = "push"
		msgs := make([]string, 0, len(p.Commits))
		for _, c := range p.Commits {
			msgs = append(msgs, c.Message)
		}
		ev.Data = map[string]any{
			"branch": strings.TrimPrefix(p.Ref, "refs/heads/"), "ref": p.Ref,
			"commit_count": len(p.Commits), "messages": msgs, "repo": p.Repo.FullName,
		}

	case hookEvent == "release" && p.Release != nil:
		if p.Action != "published" {
			return nil, nil
		}
		ev.Type = "release.published"
		ev.Data = map[string]any{
			"tag": p.Release.TagName, "name": p.Release.Name,
			"body": p.Release.Body, "url": p.Release.HTMLURL, "repo": p.Repo.FullName,
		}

	default:
		// "ping" on hook creation lands here, as does any event type GitHub adds
		// later. Acknowledged, ignored.
		return nil, nil
	}

	return []Event{ev}, nil
}

type githubRepository struct {
	FullName string `json:"full_name"`
}

func githubRepositoryNames(repositories []githubRepository) []string {
	names := make([]string, 0, len(repositories))
	for _, repository := range repositories {
		if name := strings.TrimSpace(repository.FullName); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func labelNames(in []struct {
	Name string `json:"name"`
}) []string {
	out := make([]string, 0, len(in))
	for _, l := range in {
		out = append(out, l.Name)
	}
	return out
}
