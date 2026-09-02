package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/database/models"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Whether the resource picker can actually reach Vercel.
//
// DB-backed on purpose, and not for ceremony. Vercel is the FIRST API-key
// provider to expose a resource list: every other provider in
// listProviderResources authenticates with OAuth. The credential comes back
// through FreshAccessTokenForOrg, which was written for OAuth tokens and
// contains a refresh path keyed on ExpiresAt. A pasted API key is stored in the
// same AccessToken column with a nil expiry, so the whole question is whether
// that function hands the key back untouched. A fake credential lookup would
// pass while the real picker returned an empty list and no error — the exact
// shape of failure nobody notices until a user opens the panel.
//
//	TEST_DATABASE_URL="host=localhost user=postgres password=postgres dbname=workflow_ai port=5434 sslmode=disable"

func vercelResourceDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the Vercel resource-picker tests")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.IntegrationConnection{}, &models.Organization{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// vercelStubAPI records the paths the picker asks for and answers with the real
// response shapes.
type vercelStubAPI struct {
	paths []string
}

func (s *vercelStubAPI) start(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.paths = append(s.paths, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v2/teams":
			_, _ = w.Write([]byte(`{"teams":[{"id":"team_abc","name":"Acme","slug":"acme"}]}`))
		case r.URL.Path == "/v10/projects":
			// Only the team's projects; a personal-scope request returns none, which
			// is what a team-scoped token really does.
			if r.URL.Query().Get("teamId") == "" {
				_, _ = w.Write([]byte(`{"projects":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"projects":[{"id":"prj_1","name":"fernary-web","framework":"nextjs"}]}`))
		case r.URL.Path == "/v5/domains":
			_, _ = w.Write([]byte(`{"domains":[{"name":"fernary.com"}]}`))
		case r.URL.Path == "/v7/deployments":
			_, _ = w.Write([]byte(`{"deployments":[
				{"uid":"dpl_1","name":"fernary-web","url":"fernary-web-abc.vercel.app","state":"READY","target":"production"},
				{"uid":"dpl_2","name":"fernary-web","url":"fernary-web-def.vercel.app","state":"ERROR","target":null}
			]}`))
		case strings.HasSuffix(r.URL.Path, "/env"):
			_, _ = w.Write([]byte(`{"envs":[
				{"id":"env_1","key":"API_URL","target":["production"],"type":"encrypted"},
				{"id":"env_2","key":"STRIPE_SECRET","target":["production","preview"],"type":"sensitive"}
			]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"stub has no route"}}`))
		}
	}))
	previous := vercelResourceAPI
	vercelResourceAPI = server.URL
	t.Cleanup(func() {
		vercelResourceAPI = previous
		server.Close()
	})
}

// seedVercelKey stores a pasted API key exactly the way SetIntegrationKey does.
func seedVercelKey(t *testing.T, db *gorm.DB) (*WorkflowHandler, string, string) {
	t.Helper()
	orgID, userID := uuid.NewString(), uuid.NewString()
	conn := models.IntegrationConnection{
		UserID: userID, OrganizationID: orgID, Provider: "vercel",
		AccessToken: "vercel-pat-token",
		// The detail under test: an API key has NO expiry, where every OAuth
		// connection has one.
		Scope: "api_key",
	}
	if err := db.Create(&conn).Error; err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(&conn) })
	return &WorkflowHandler{db: &database.DBClient{DB: db}}, orgID, userID
}

func TestVercelResourcesReachThroughAnAPIKeyConnection(t *testing.T) {
	db := vercelResourceDB(t)
	stub := &vercelStubAPI{}
	stub.start(t)
	h, orgID, userID := seedVercelKey(t, db)

	// The whole point: an api_key connection must produce a usable credential
	// through the OAuth-shaped lookup.
	got, err := h.listProviderResources(orgID, userID, "vercel")
	if err != nil {
		t.Fatalf("listProviderResources failed for an API-key provider: %v", err)
	}
	byKind := map[string][]integrationResource{}
	for _, r := range got {
		byKind[r.Type] = append(byKind[r.Type], r)
	}
	if len(byKind["team"]) != 1 || byKind["team"][0].ID != "team_abc" {
		t.Errorf("teams = %v, want one team_abc", byKind["team"])
	}
	if byKind["team"][0].Name != "Acme" {
		t.Errorf("team label = %q, want the human name rather than the id", byKind["team"][0].Name)
	}
	// Domains are personal-scope here, teams are not; both must survive.
	if len(byKind["domain"]) != 1 {
		t.Errorf("domains = %v", byKind["domain"])
	}
}

func TestVercelPickerNeverSendsAnUnscopedTeamRequest(t *testing.T) {
	db := vercelResourceDB(t)
	stub := &vercelStubAPI{}
	stub.start(t)
	h, orgID, userID := seedVercelKey(t, db)

	// A team parent must scope every request it makes, or the picker lists the
	// personal account's projects while claiming to show the team's.
	if _, err := h.listChildResources(orgID, userID, "vercel", "team_abc"); err != nil {
		t.Fatalf("listChildResources(team) failed: %v", err)
	}
	var checked int
	for _, path := range stub.paths {
		if !strings.HasPrefix(path, "/v10/projects") && !strings.HasPrefix(path, "/v5/domains") {
			continue
		}
		checked++
		_, rawQuery, _ := strings.Cut(path, "?")
		values, _ := url.ParseQuery(rawQuery)
		if values.Get("teamId") != "team_abc" {
			t.Errorf("%s did not carry teamId=team_abc — the picker would show the "+
				"personal account's list under a team's name", path)
		}
	}
	if checked == 0 {
		t.Fatal("no project or domain request was made at all")
	}
}

func TestVercelCompositeParentListsDeploymentsAndEnvVars(t *testing.T) {
	db := vercelResourceDB(t)
	stub := &vercelStubAPI{}
	stub.start(t)
	h, orgID, userID := seedVercelKey(t, db)

	got, err := h.listChildResources(orgID, userID, "vercel", "team_abc/fernary-web")
	if err != nil {
		t.Fatalf("composite parent failed: %v", err)
	}
	byKind := map[string][]integrationResource{}
	for _, r := range got {
		byKind[r.Type] = append(byKind[r.Type], r)
	}
	if len(byKind["deployment"]) != 2 {
		t.Fatalf("deployments = %v, want 2", byKind["deployment"])
	}
	// A dpl_ id is unreadable, so the label has to carry environment and outcome.
	first := byKind["deployment"][0]
	if first.ID != "dpl_1" {
		t.Errorf("deployment id = %q, want the uid the executor needs", first.ID)
	}
	for _, want := range []string{"production", "ready"} {
		if !strings.Contains(first.Name, want) {
			t.Errorf("deployment label %q does not mention %q", first.Name, want)
		}
	}
	// target:null is a preview deployment, not an empty label.
	if second := byKind["deployment"][1]; !strings.Contains(second.Name, "preview") {
		t.Errorf("a null target should read as preview, got %q", second.Name)
	}

	if len(byKind["envvar"]) != 2 {
		t.Fatalf("env vars = %v, want 2", byKind["envvar"])
	}
	// The id addresses the variable; the key is what a person recognises.
	if byKind["envvar"][0].ID != "env_1" {
		t.Errorf("env var id = %q, want the id the executor needs", byKind["envvar"][0].ID)
	}
	if !strings.Contains(byKind["envvar"][0].Name, "API_URL") {
		t.Errorf("env var label %q does not name the key", byKind["envvar"][0].Name)
	}

	// No VALUE may appear in any label — these reach the browser, and one of the
	// seeded variables is a Stripe secret.
	blob, _ := json.Marshal(got)
	for _, leak := range []string{"secret-value", "sk_live", "value\":"} {
		if strings.Contains(string(blob), leak) {
			t.Errorf("a resource label carries %q; env var values must never be listed", leak)
		}
	}

	// Both requests must be project- AND team-scoped.
	var sawDeployments, sawEnv bool
	for _, path := range stub.paths {
		if strings.HasPrefix(path, "/v7/deployments") {
			sawDeployments = true
			if !strings.Contains(path, "teamId=team_abc") || !strings.Contains(path, "projectId=fernary-web") {
				t.Errorf("deployment request under-scoped: %s", path)
			}
		}
		if strings.Contains(path, "/env") {
			sawEnv = true
			if !strings.Contains(path, "teamId=team_abc") {
				t.Errorf("env request lost the team: %s", path)
			}
		}
	}
	if !sawDeployments || !sawEnv {
		t.Errorf("expected both requests; deployments=%v env=%v", sawDeployments, sawEnv)
	}
}

// "team/" means the project has not been chosen yet. That must be a quiet empty
// list, not an error — the picker shows an error toast for a real failure, and
// the user has done nothing wrong by not having picked yet.
func TestVercelPartialParentWaitsQuietly(t *testing.T) {
	db := vercelResourceDB(t)
	stub := &vercelStubAPI{}
	stub.start(t)
	h, orgID, userID := seedVercelKey(t, db)

	got, err := h.listChildResources(orgID, userID, "vercel", "team_abc/")
	if err != nil {
		t.Fatalf("a half-chosen parent must not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected an empty list, got %v", got)
	}
	if len(stub.paths) != 0 {
		t.Errorf("spent requests on an unchosen project: %v", stub.paths)
	}
}

// A personal account has no team, so the parent is "/project".
func TestVercelPersonalScopeCompositeParent(t *testing.T) {
	db := vercelResourceDB(t)
	stub := &vercelStubAPI{}
	stub.start(t)
	h, orgID, userID := seedVercelKey(t, db)

	if _, err := h.listChildResources(orgID, userID, "vercel", "/fernary-web"); err != nil {
		t.Fatalf("personal-scope composite parent failed: %v", err)
	}
	for _, path := range stub.paths {
		if strings.Contains(path, "teamId=") {
			t.Errorf("sent a teamId for a personal-scope request: %s", path)
		}
	}
	if len(stub.paths) == 0 {
		t.Fatal("no request was made")
	}
}

// Not connected must be distinguishable from a failure, or the panel raises an
// error toast at someone who simply has not pasted a token yet.
func TestVercelResourcesReportNotConnected(t *testing.T) {
	db := vercelResourceDB(t)
	stub := &vercelStubAPI{}
	stub.start(t)
	h := &WorkflowHandler{db: &database.DBClient{DB: db}}

	_, err := h.listProviderResources(uuid.NewString(), uuid.NewString(), "vercel")
	if err == nil {
		t.Fatal("expected an error with no connection stored")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("error %q must be the not-connected sentinel so the picker stays quiet", err)
	}
	if len(stub.paths) != 0 {
		t.Errorf("called Vercel with no credential: %v", stub.paths)
	}
}
