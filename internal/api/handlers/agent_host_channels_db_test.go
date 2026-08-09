package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/database/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Who may see which Slack channels.
//
// The host bot is connected once for a whole organization and sits in whatever
// channels that workspace put it in. Listing the bot's own channels therefore
// told every member — including people who are not in that Slack workspace at
// all — the names, ids and membership of its private channels. DB-backed
// because the fix turns on mapping the calling Fernary user to a Slack account
// through the users table, and a fake user would pass while production leaked.
//
//	TEST_DATABASE_URL="host=localhost user=postgres password=postgres dbname=workflow_ai port=5434 sslmode=disable"

func channelsDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run agent host channel tests")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.Organization{},
		&models.OrgMember{}, &models.AgentHostInstallation{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// slackStub records what the handler asked Slack for, which is the thing under
// test: a request that does not name a user is a request for the whole
// workspace.
type slackStub struct {
	mu         sync.Mutex
	calls      []url.Values
	methods    []string
	knownEmail string
}

func (s *slackStub) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/")
		s.mu.Lock()
		s.calls = append(s.calls, r.URL.Query())
		s.methods = append(s.methods, method)
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "users.lookupByEmail":
			if s.knownEmail != "" && r.URL.Query().Get("email") == s.knownEmail {
				_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"U_CALLER"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":false,"error":"users_not_found"}`))

		case "users.conversations":
			// Slack only returns a private channel here when both the named user
			// and the bot are in it, so a correctly scoped call cannot see
			// #board-acquisition.
			_, _ = w.Write([]byte(`{"ok":true,"channels":[
				{"id":"C_ENG","name":"engineering","is_member":true,"is_private":false},
				{"id":"C_MINE","name":"my-private-team","is_member":true,"is_private":true}
			],"response_metadata":{"next_cursor":""}}`))

		case "conversations.list":
			// What the bot can see. Whether #board-acquisition comes back depends
			// entirely on what the handler asked for.
			all := `{"ok":true,"channels":[
				{"id":"C_ENG","name":"engineering","is_member":true,"is_private":false},
				{"id":"C_BOARD","name":"board-acquisition","is_member":true,"is_private":true}
			],"response_metadata":{"next_cursor":""}}`
			if r.URL.Query().Get("types") == "public_channel" {
				all = `{"ok":true,"channels":[
					{"id":"C_ENG","name":"engineering","is_member":true,"is_private":false}
				],"response_metadata":{"next_cursor":""}}`
			}
			_, _ = w.Write([]byte(all))

		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(srv.Close)

	original := slackAgentAPIBase
	slackAgentAPIBase = srv.URL + "/"
	t.Cleanup(func() { slackAgentAPIBase = original })
}

func (s *slackStub) asked(method string) (url.Values, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.methods {
		if m == method {
			return s.calls[i], true
		}
	}
	return nil, false
}

// seedHost creates an org with an active Slack host and one ordinary member.
func seedHost(t *testing.T, db *gorm.DB, memberEmail string) (*WorkflowHandler, string, string) {
	t.Helper()
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "Channel tests", Slug: "channel-tests-" + uuid.NewString()[:8],
		Plan: models.PlanFree, Personal: false, Seats: 5,
	}
	if err := db.Create(&org).Error; err != nil {
		t.Fatalf("seed org: %v", err)
	}
	member := models.User{BaseModel: models.BaseModel{ID: uuid.New()}, Email: memberEmail, Name: "Ordinary Member"}
	if err := db.Create(&member).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.Create(&models.OrgMember{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID.String(), UserID: member.ID.String(), Role: models.RoleMember,
	}).Error; err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	host := models.AgentHostInstallation{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID.String(), InstalledByUserID: uuid.NewString(),
		Provider: "slack", Status: models.AgentHostActive,
		// Unique on (provider, external_workspace_id), so each test needs its own
		// workspace: one Slack workspace can be claimed by only one org.
		ExternalWorkspaceID: "T" + uuid.NewString()[:8],
		BotToken:            "xoxb-test", BotUserID: "U_BOT",
	}
	if err := db.Create(&host).Error; err != nil {
		t.Fatalf("seed host: %v", err)
	}
	return &WorkflowHandler{db: &database.DBClient{DB: db}}, org.ID.String(), host.ID.String()
}

func listChannelsAs(t *testing.T, h *WorkflowHandler, orgID, userID, hostID string) (slackChannelInventory, int) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/agent-hosts/"+hostID+"/channels", nil)
	c.Params = gin.Params{{Key: "id", Value: hostID}}
	// The keys the auth middleware actually writes.
	c.Set("auth.orgID", orgID)
	c.Set("auth.userID", userID)

	h.ListAgentHostChannels(c)

	var inventory slackChannelInventory
	_ = json.Unmarshal(rec.Body.Bytes(), &inventory)
	return inventory, rec.Code
}

// The reported leak: an ordinary member could enumerate every private channel
// the shared bot sits in, whether or not they belonged to any of them.
func TestChannelListIsScopedToTheCallersOwnMembership(t *testing.T) {
	db := channelsDB(t)
	email := "member-" + uuid.NewString()[:8] + "@example.test"
	stub := &slackStub{knownEmail: email}
	stub.serve(t)

	h, orgID, hostID := seedHost(t, db, email)
	var member models.User
	if err := db.First(&member, "email = ?", email).Error; err != nil {
		t.Fatalf("load member: %v", err)
	}

	inventory, status := listChannelsAs(t, h, orgID, member.ID.String(), hostID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	// The request has to name the caller. Without a user id Slack answers for the
	// whole workspace, which is the bug.
	query, called := stub.asked("users.conversations")
	if !called {
		t.Fatal("handler never asked for the caller's own conversations")
	}
	if got := query.Get("user"); got != "U_CALLER" {
		t.Errorf("scoped to user %q, want U_CALLER — an unscoped list is workspace-wide", got)
	}
	if _, wideOpen := stub.asked("conversations.list"); wideOpen {
		t.Error("handler still called conversations.list, which ignores who is asking")
	}

	names := map[string]bool{}
	for _, ch := range inventory.Channels {
		names[ch.Name] = true
	}
	if names["board-acquisition"] {
		t.Error("a private channel the caller does not belong to was disclosed")
	}
	if !names["engineering"] || !names["my-private-team"] {
		t.Errorf("caller lost channels they are entitled to: %v", names)
	}
	if inventory.Scope != "member" {
		t.Errorf("scope = %q, want member", inventory.Scope)
	}
}

// An identity we cannot resolve must narrow the result, never widen it. This is
// the path an install predating the users:read.email scope takes.
func TestAnUnresolvedSlackIdentityFallsBackToPublicChannelsOnly(t *testing.T) {
	db := channelsDB(t)
	// No known email, so users.lookupByEmail fails the way a missing scope does.
	stub := &slackStub{}
	stub.serve(t)

	email := "not-in-slack-" + uuid.NewString()[:8] + "@example.test"
	h, orgID, hostID := seedHost(t, db, email)
	var member models.User
	if err := db.First(&member, "email = ?", email).Error; err != nil {
		t.Fatalf("load member: %v", err)
	}

	inventory, status := listChannelsAs(t, h, orgID, member.ID.String(), hostID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	query, called := stub.asked("conversations.list")
	if !called {
		t.Fatal("expected the public-only fallback to run")
	}
	if got := query.Get("types"); got != "public_channel" {
		t.Errorf("fallback asked for types=%q; it must never request private channels", got)
	}
	for _, ch := range inventory.Channels {
		if ch.IsPrivate {
			t.Errorf("fallback disclosed private channel %q", ch.Name)
		}
	}
	if inventory.Scope != "public" || inventory.Notice == "" {
		t.Errorf("fallback must say why the list is short: scope=%q notice=%q",
			inventory.Scope, inventory.Notice)
	}
}
