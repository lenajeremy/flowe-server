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
	// botListFails makes the bot's own users.conversations call fail while the
	// caller's succeeds — the partial outage that decides whether a failed
	// lookup is reported as "unknown" or as "the bot is in nothing".
	botListFails bool
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
			// Deliberately no is_member: Slack's reference states it is not returned
			// by this method. The stub used to invent it, which is why a version
			// that left every channel disabled passed.
			//
			// A request naming a user is that user's list; one with no user is the
			// bot's own, which is how is_member gets filled. The bot is in
			// #engineering and #my-private-team but not #user-only.
			if r.URL.Query().Get("user") == "" {
				if s.botListFails {
					_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
					return
				}
				_, _ = w.Write([]byte(`{"ok":true,"channels":[
					{"id":"C_ENG","name":"engineering","is_private":false},
					{"id":"C_MINE","name":"my-private-team","is_private":true}
				],"response_metadata":{"next_cursor":""}}`))
				return
			}
			// Slack returns a private channel here only when the bot shares
			// membership, so #board-acquisition can never appear.
			_, _ = w.Write([]byte(`{"ok":true,"channels":[
				{"id":"C_ENG","name":"engineering","is_private":false},
				{"id":"C_MINE","name":"my-private-team","is_private":true},
				{"id":"C_USERONLY","name":"user-only","is_private":false}
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

// askedFor finds the call to method whose query sets param to value. Order is not
// asserted: the handler makes two users.conversations calls — the caller's list
// and the bot's own — and which comes first is not a guarantee worth pinning.
func (s *slackStub) askedFor(method, param, value string) (url.Values, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.methods {
		if m == method && s.calls[i].Get(param) == value {
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
	if _, called := stub.askedFor("users.conversations", "user", "U_CALLER"); !called {
		t.Error("handler never asked for the caller's own conversations; an unscoped list is workspace-wide")
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

	// The picker disables any channel with is_member false, so an absent flag
	// means nothing is selectable and no deployment can be created. Slack does
	// not send it on this method, so it has to be filled from the bot's own list.
	botIn := map[string]bool{}
	for _, ch := range inventory.Channels {
		botIn[ch.Name] = ch.IsMember != nil && *ch.IsMember
	}
	if !botIn["engineering"] {
		t.Error("engineering came back not-a-member, so the picker would disable it")
	}
	if !botIn["my-private-team"] {
		t.Error("my-private-team came back not-a-member, so the picker would disable it")
	}
	if botIn["user-only"] {
		t.Error("user-only is a channel the bot is not in; marking it joinable would let a deployment post nowhere")
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

// A failed bot-membership lookup must not masquerade as fact.
//
// is_member false means "the bot is not in this channel", and the picker
// disables those. If a failed lookup produced the same false, every channel
// would come back unavailable and an admin could not deploy to channels the bot
// is already sitting in — an outage in a secondary call locking out the whole
// feature.
func TestAFailedBotLookupIsReportedAsUnknownNotAsAbsence(t *testing.T) {
	db := channelsDB(t)
	email := "member-" + uuid.NewString()[:8] + "@example.test"
	stub := &slackStub{knownEmail: email, botListFails: true}
	stub.serve(t)

	h, orgID, hostID := seedHost(t, db, email)
	var member models.User
	if err := db.First(&member, "email = ?", email).Error; err != nil {
		t.Fatalf("load member: %v", err)
	}

	inventory, status := listChannelsAs(t, h, orgID, member.ID.String(), hostID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a secondary lookup failing must not fail the listing", status)
	}
	if !inventory.MembershipUnknown {
		t.Fatal("membership_unknown is false, so the client cannot tell a failed lookup from a bot that joined nothing")
	}
	if inventory.Notice == "" {
		t.Error("nothing told the user why membership could not be checked")
	}
	if len(inventory.Channels) == 0 {
		t.Fatal("the caller's own channels disappeared because a different call failed")
	}
	// Null, not false. A consumer can overlook an envelope flag; it cannot
	// overlook a null where it expected a boolean.
	for _, ch := range inventory.Channels {
		if ch.IsMember != nil {
			t.Errorf("channel %s reported is_member=%v as fact when membership was never determined",
				ch.Name, *ch.IsMember)
		}
	}
}
