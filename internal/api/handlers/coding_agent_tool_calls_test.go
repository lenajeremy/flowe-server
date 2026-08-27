package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workflow-ai/server/internal/codingagent"
	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func toolCallTestHandler(t *testing.T) (*WorkflowHandler, *models.CodingAgentJob) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.OrgMember{}, &models.CodingAgentJob{}, &models.CodingAgentEvent{}, &models.CodingAgentToolCall{}); err != nil {
		t.Fatal(err)
	}
	orgID, userID := uuid.NewString(), uuid.NewString()
	if err := db.Create(&models.OrgMember{OrganizationID: orgID, UserID: userID, Role: "owner"}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job := &models.CodingAgentJob{
		OrganizationID: orgID, UserID: userID, WorkflowID: uuid.NewString(), WorkflowRunID: uuid.NewString(),
		NodeID: "agent", IdempotencyKey: uuid.NewString(), Runtime: codingagent.RuntimeCodex, Task: "ship",
		Status: models.CodingAgentJobRunning, HeartbeatAt: &now, ToolTokenHash: codingagent.HashToolToken("token"),
		Input: models.JSONB(`{}`), ExecutionPolicy: models.JSONB(`{}`), Result: models.JSONB(`{}`),
		ToolWorkflow: models.JSONB(`{}`), ToolPolicy: models.JSONB(`{"version":1,"nodes":[]}`), ToolNodeIDs: models.JSONB(`[]`),
		NextEventSequence: 1, AvailableAt: now,
	}
	if err := db.Create(job).Error; err != nil {
		t.Fatal(err)
	}
	return &WorkflowHandler{db: &database.DBClient{DB: db}}, job
}

func TestCodingAgentMutationIntentIsDurableAndIdempotentBeforeApproval(t *testing.T) {
	h, job := toolCallTestHandler(t)
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/mcp/coding-agent", nil)
	node := executor.WorkflowASTNode{ID: "github", Data: executor.FlowNodeData{
		NodeType: executor.NodeTypeGithub, Label: "GitHub", GithubRepo: "acme/widget", IntegrationOp: "create_branch",
	}}
	tool := agentTool{Node: node, Schema: map[string]any{"name": "github"}}
	authorized := AgentAuthorizedCall{
		Node: node, Operation: AgentOperationCapability{ID: "create_branch", Effect: AgentEffectWrite},
		Overrides: map[string]any{"githubBranch": "fernary/fix"}, Reason: "prepare the requested pull request",
	}
	first, err := h.prepareCodingAgentToolCall(c, mcpRequest{ID: json.RawMessage(`1`)}, job, tool, authorized, map[string]any{
		"githubBranch": "fernary/fix", "reason": authorized.Reason,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.prepareCodingAgentToolCall(c, mcpRequest{ID: json.RawMessage(`2`)}, job, tool, authorized, map[string]any{
		"githubBranch": "fernary/fix", "reason": authorized.Reason,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != models.CodingAgentToolCallPendingApproval || second.ID != first.ID {
		t.Fatalf("equivalent mutation was not held behind one approval: first=%s second=%s", first.Status, second.ID)
	}
	var count int64
	if err := h.db.DB.Model(&models.CodingAgentToolCall{}).Where("job_id = ?", job.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("created %d durable intents for one equivalent mutation", count)
	}
}

func TestCodingAgentToolAuthenticationStopsImmediatelyOnCancellation(t *testing.T) {
	h, job := toolCallTestHandler(t)
	gin.SetMode(gin.TestMode)
	makeContext := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/api/mcp/coding-agent", nil)
		c.Request.Header.Set("Authorization", "Bearer token")
		return c
	}
	if authenticated, ok := h.authenticateToolCall(makeContext()); !ok || authenticated.ID != job.ID {
		t.Fatal("fresh running job was not authenticated")
	}
	now := time.Now().UTC()
	if err := h.db.DB.Model(job).Updates(map[string]any{"cancel_requested_at": now, "tool_token_hash": ""}).Error; err != nil {
		t.Fatal(err)
	}
	if _, ok := h.authenticateToolCall(makeContext()); ok {
		t.Fatal("cancelled job retained workflow-tool authority")
	}
}
