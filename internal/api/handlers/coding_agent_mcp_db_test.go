package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"workflow-ai/server/internal/codingagent"
	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func codingAgentMCPDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run coding agent MCP tests")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.AutoMigrate(&models.Workflow{}, &models.CodingAgentJob{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedToolJob creates a workflow with a GitHub node and a running job that has
// been granted (or denied) it, returning the plaintext callback token.
func seedToolJob(t *testing.T, db *gorm.DB, grantedNodeIDs []string) (*WorkflowHandler, string) {
	t.Helper()
	org, user := uuid.NewString(), uuid.NewString()

	nodes, _ := json.Marshal([]executor.WorkflowASTNode{{
		ID: "gh", Data: executor.FlowNodeData{
			NodeType: executor.NodeTypeGithub, Label: "GitHub", GithubRepo: "acme/widget",
		},
	}})
	workflow := models.Workflow{
		BaseModel: models.BaseModel{ID: uuid.New()}, UserID: user, OrganizationID: org,
		Name: "Fix bugs", Nodes: models.JSONB(nodes), Edges: models.JSONB([]byte("[]")),
	}
	if err := db.Create(&workflow).Error; err != nil {
		t.Fatalf("create workflow: %v", err)
	}

	token := "tok-" + uuid.NewString()
	granted, _ := json.Marshal(grantedNodeIDs)
	job := models.CodingAgentJob{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org, UserID: user,
		WorkflowID: workflow.ID.String(), NodeID: "agent", IdempotencyKey: uuid.NewString(),
		Runtime: "codex", Task: "fix it", Status: models.CodingAgentJobRunning,
		ToolTokenHash: codingagent.HashToolToken(token),
		ToolNodeIDs:   models.JSONB(granted),
	}
	if err := db.Create(&job).Error; err != nil {
		t.Fatalf("create job: %v", err)
	}
	t.Cleanup(func() {
		db.Unscoped().Delete(&job)
		db.Unscoped().Delete(&workflow)
	})
	return &WorkflowHandler{db: &database.DBClient{DB: db}}, token
}

func callMCP(t *testing.T, h *WorkflowHandler, token, body string) map[string]any {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/mcp/coding-agent", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if token != "" {
		c.Request.Header.Set("Authorization", "Bearer "+token)
	}
	h.CodingAgentMCP(c)
	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return decoded
}

func TestCodingAgentMCPOffersOnlyGrantedNodes(t *testing.T) {
	db := codingAgentMCPDB(t)

	h, token := seedToolJob(t, db, []string{"gh"})
	granted := callMCP(t, h, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	result, _ := granted["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) == 0 {
		t.Fatalf("a granted node was not offered: %#v", granted)
	}

	// Deny-by-default is the whole safety property: an agent with no grant must
	// see no tools, not every node on the canvas.
	h2, token2 := seedToolJob(t, db, nil)
	ungranted := callMCP(t, h2, token2, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	result2, _ := ungranted["result"].(map[string]any)
	tools2, _ := result2["tools"].([]any)
	if len(tools2) != 0 {
		t.Fatalf("an ungranted job was offered %d tools", len(tools2))
	}
}

func TestCodingAgentMCPRejectsBadCredentials(t *testing.T) {
	db := codingAgentMCPDB(t)
	h, token := seedToolJob(t, db, []string{"gh"})

	for _, tc := range []struct{ name, token string }{
		{"no token", ""},
		{"wrong token", "tok-" + uuid.NewString()},
		{"empty bearer", " "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := callMCP(t, h, tc.token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
			if _, ok := response["error"]; !ok {
				t.Fatalf("call was accepted without a valid token: %#v", response)
			}
		})
	}

	// A sandbox that outlives its job must lose its authority immediately,
	// before anything gets round to clearing the token.
	if err := db.Model(&models.CodingAgentJob{}).
		Where("tool_token_hash = ?", codingagent.HashToolToken(token)).
		Update("status", models.CodingAgentJobSucceeded).Error; err != nil {
		t.Fatalf("finish job: %v", err)
	}
	response := callMCP(t, h, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if _, ok := response["error"]; !ok {
		t.Fatalf("a finished job could still use its token: %#v", response)
	}
}

func TestCodingAgentMCPRefusesUnknownTool(t *testing.T) {
	db := codingAgentMCPDB(t)
	h, token := seedToolJob(t, db, []string{"gh"})

	response := callMCP(t, h, token,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"not_a_tool","arguments":{}}}`)
	result, _ := response["result"].(map[string]any)
	if isError, _ := result["isError"].(bool); !isError {
		t.Fatalf("an unknown tool was not reported as an error: %#v", response)
	}
}
