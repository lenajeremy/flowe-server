package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/billing"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/executor"
	"workflow-ai/server/internal/telemetry"

	"github.com/gin-gonic/gin"
)

type analyzeAgentDeploymentRequest struct {
	ModelID string `json:"modelId"`
}

type agentAIAnalysis struct {
	Goal         string                  `json:"goal"`
	Summary      string                  `json:"summary"`
	Integrations []AgentIntegrationGrant `json:"integrations"`
	Nodes        []AgentNodeGrant        `json:"nodes,omitempty"`
	Warnings     []string                `json:"warnings"`
}

// AnalyzeAgentDeployment asks a second model pass to infer the deployment goal
// from the workflow and its Builder conversation. There is deliberately no
// user-supplied goal field on this endpoint.
func (h *WorkflowHandler) AnalyzeAgentDeployment(c *gin.Context) {
	workflow, ok := h.loadOwnedWorkflow(c, c.Param("id"))
	if !ok {
		return
	}
	var request analyzeAgentDeploymentRequest
	if err := c.ShouldBindJSON(&request); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid analysis request"})
		return
	}
	if !auth.Allow(c.Request.Context(), h.redis, "rl:agent-analysis:"+auth.UserID(c), 10, time.Minute) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many permission analyses — try again in a minute"})
		return
	}
	plan, err := h.bill.CheckBalance(currentOrgID(c), auth.UserID(c))
	if err != nil {
		if errors.Is(err, billing.ErrOverCap) {
			c.JSON(http.StatusPaymentRequired, gin.H{"error": err.Error(), "limit": billing.KindOf(err)})
			return
		}
		slog.ErrorContext(c.Request.Context(), "agent permission analysis balance check failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not start permission analysis"})
		return
	}
	ctx := telemetry.WithSurface(c.Request.Context(), telemetry.SurfaceAgent)
	ctx = telemetry.WithBilling(ctx, billing.BillingContextFor(currentOrgID(c), auth.UserID(c), plan))

	ast, err := workflowASTFromModel(workflow)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	// Deliberately not the agent default. This decides which operations an agent
	// may perform and which fields it may set — it is a security judgement made
	// once per deployment, not a per-message turn, so it stays on the model that
	// reasons best. Cheapening it saves nothing measurable and risks a policy that
	// grants more than it should.
	runtimeModel, err := prepareAgentAnalysisModel(request.ModelID)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	prompt := h.agentPermissionAnalysisPrompt(workflow, ast)
	raw, err := requestAgentPermissionAnalysis(ctx, runtimeModel, prompt)
	if err != nil {
		slog.WarnContext(ctx, "agent permission analysis failed", "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "permission analyzer failed", "detail": err.Error()})
		return
	}
	var ai agentAIAnalysis
	if err := decodeAgentAnalysisJSON(raw, &ai); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "permission analyzer returned invalid JSON", "detail": err.Error()})
		return
	}
	proposed := AgentCapabilityPolicy{Version: agentCapabilityPolicyVersion, Integrations: ai.Integrations, Nodes: ai.Nodes}
	policy, normalizationWarnings := normalizeAgentCapabilityPolicy(ast, proposed)
	if len(policy.Integrations) == 0 {
		policy = defaultSafeAgentPolicy(ast)
		normalizationWarnings = append(normalizationWarnings,
			"The AI recommendation contained no valid grants, so Fernary used the closed read-only fallback.")
	}
	warnings := append([]string{}, ai.Warnings...)
	warnings = append(warnings, normalizationWarnings...)
	warnings = append(warnings, agentPolicyRiskWarnings(ast, policy)...)
	analysis := AgentPermissionAnalysis{
		Goal: strings.TrimSpace(ai.Goal), Summary: strings.TrimSpace(ai.Summary),
		Policy: policy, Warnings: warnings, Source: "ai",
	}
	analysis.Review = summarizeAgentPolicy(ast, policy, analysis.Goal, warnings)
	policyJSON, _ := json.Marshal(policy)
	warningJSON, _ := json.Marshal(warnings)
	record := models.AgentPermissionAnalysisRecord{
		OrganizationID: currentOrgID(c), WorkflowID: workflow.ID.String(), RequestedByUserID: auth.UserID(c),
		SnapshotHash: agentSnapshotHash(workflow.Name, workflow.Nodes, workflow.Edges),
		ModelID:      runtimeModel.Spec.ID, Goal: analysis.Goal, Summary: analysis.Summary,
		RecommendedPolicy: models.JSONB(policyJSON), Warnings: models.JSONB(warningJSON),
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := h.db.DB.Create(&record).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store permission analysis"})
		return
	}
	analysis.AnalysisID = record.ID.String()
	c.JSON(http.StatusOK, analysis)
}

func (h *WorkflowHandler) agentPermissionAnalysisPrompt(workflow *models.Workflow, ast executor.WorkflowAST) string {
	type safeNode struct {
		ID            string            `json:"id"`
		Label         string            `json:"label"`
		Type          executor.NodeType `json:"type"`
		SavedDefaults string            `json:"savedDefaults"`
	}
	nodes := make([]safeNode, 0, len(ast.Nodes))
	remainingDefaults := 60 << 10
	for _, node := range ast.Nodes {
		defaults := agentSafeSavedConfig(node.Data)
		limit := min(4000, remainingDefaults)
		if limit <= 0 {
			defaults = "{defaults omitted: analysis input cap reached}"
		} else {
			defaults = truncate(defaults, limit)
			remainingDefaults -= len(defaults)
		}
		nodes = append(nodes, safeNode{
			ID: node.ID, Label: node.Data.Label, Type: node.Data.NodeType, SavedDefaults: defaults,
		})
	}
	capabilityJSON, _ := json.Marshal(agentIntegrationCapabilities(ast))
	nodeJSON, _ := json.Marshal(nodes)
	conversationJSON, _ := json.Marshal(h.agentBuilderConversation(workflow.ID.String()))
	return fmt.Sprintf(`Analyze this saved workflow as a team-chat agent deployment.

Infer the deployment goal yourself from the workflow and Builder conversation. Do not ask for or assume a separately entered deployment goal.

Return exactly one JSON object with this shape:
{"goal":"one sentence","summary":"one plain-language sentence","integrations":[{"nodeType":"exact type","nodeIds":["exact backing node id"],"allowedOperations":["exact operation"],"allowedOverrideFields":["exact field"]}],"warnings":["plain-language warning"]}

Security rules:
- Choose only integration types, backing node ids, operations, and fields present in the capability catalog below.
- Emit at most one permission grant per integration type. Use nodeIds only to restrict that grant to the configured resources needed for the inferred goal.
- Start narrow. Prefer read operations. Do not grant an operation merely because the node type supports it.
- Saved settings are pinned unless the inferred goal clearly requires changing a field per request.
- Do not select credential, secret, token, authorization-header, password, or environment-value fields.
- Omit search/global-list operations when they could escape a saved folder, repository, project, mailbox, channel, or account boundary.
- A write or destructive operation may be selected only when the inferred goal clearly needs it; Fernary will still require requester approval for every such call.
- If uncertain, omit the capability and explain the limitation in warnings.

Workflow name: %s
Workflow description: %s
Secret-safe saved nodes: %s
Builder conversation (text only, bounded): %s
Server capability catalog: %s`,
		workflow.Name, truncate(workflow.Description, 2000), nodeJSON, conversationJSON, capabilityJSON)
}

func (h *WorkflowHandler) agentBuilderConversation(workflowID string) []map[string]string {
	var chat models.WorkflowChat
	if err := h.db.DB.Where("workflow_id = ?", workflowID).First(&chat).Error; err != nil {
		return nil
	}
	var raw []map[string]any
	if json.Unmarshal(chat.Messages, &raw) != nil {
		return nil
	}
	if len(raw) > 24 {
		raw = raw[len(raw)-24:]
	}
	out := make([]map[string]string, 0, len(raw))
	for _, message := range raw {
		role, _ := message["role"].(string)
		content, _ := message["content"].(string)
		if role == "" || content == "" {
			continue
		}
		out = append(out, map[string]string{"role": truncate(role, 24), "content": truncate(content, 2000)})
	}
	return out
}

func requestAgentPermissionAnalysis(ctx context.Context, runtimeModel agentRuntimeModel, prompt string) (result string, resultErr error) {
	ctx, finish := telemetry.StartLLM(ctx, runtimeModel.Spec.Provider, runtimeModel.Spec.ID)
	defer func() { finish(len(result), resultErr) }()

	if runtimeModel.Spec.Provider == "anthropic" {
		body, _ := json.Marshal(map[string]any{
			"model": runtimeModel.Spec.ID, "max_tokens": 4000,
			"system":   "You are a security-focused least-privilege deployment analyzer. Return JSON only.",
			"messages": []map[string]string{{"role": "user", "content": prompt}},
		})
		resp, err := doAnthropicRequestContext(ctx, runtimeModel.APIKey, body)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("anthropic %d: %s", resp.StatusCode, truncate(string(raw), 500))
		}
		var parsed struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Usage struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return "", err
		}
		telemetry.LLMTokens(ctx, "anthropic", runtimeModel.Spec.ID, telemetry.Usage{
			InputTokens: parsed.Usage.InputTokens, OutputTokens: parsed.Usage.OutputTokens,
			CacheReadTokens: parsed.Usage.CacheReadInputTokens, CacheWriteTokens: parsed.Usage.CacheCreationInputTokens,
		})
		var text strings.Builder
		for _, block := range parsed.Content {
			if block.Type == "text" {
				text.WriteString(block.Text)
			}
		}
		return text.String(), nil
	}

	payload := map[string]any{
		"model": runtimeModel.Spec.ID,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a security-focused least-privilege deployment analyzer. Return JSON only."},
			{"role": "user", "content": prompt},
		},
		"response_format": map[string]string{"type": "json_object"},
		// The Anthropic branch above sets 4000; this one set nothing, which on a
		// model that thinks from the same budget can return an empty body.
		"max_completion_tokens": 4000,
	}
	// This path does not go through the executor's router, so provider-specific
	// fields have to be applied here too — a thinking model with no budget left
	// returns an empty policy, and an empty policy is not a safe default.
	for key, value := range executor.OpenAICompatibleBody(runtimeModel.Spec.ID) {
		payload[key] = value
	}
	body, _ := json.Marshal(payload)
	resp, err := doOpenAIRequestContext(ctx, runtimeModel.ProviderURL, runtimeModel.APIKey, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("provider %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			PromptDetails    struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", err
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("provider returned no analysis")
	}
	uncached := parsed.Usage.PromptTokens - parsed.Usage.PromptDetails.CachedTokens
	if uncached < 0 {
		uncached = 0
	}
	telemetry.LLMTokens(ctx, runtimeModel.Spec.Provider, runtimeModel.Spec.ID, telemetry.Usage{
		InputTokens: uncached, OutputTokens: parsed.Usage.CompletionTokens,
		CacheReadTokens: parsed.Usage.PromptDetails.CachedTokens,
	})
	return parsed.Choices[0].Message.Content, nil
}

func decodeAgentAnalysisJSON(raw string, out *agentAIAnalysis) error {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	start, end := strings.Index(trimmed, "{"), strings.LastIndex(trimmed, "}")
	if start < 0 || end < start {
		return errors.New("no JSON object found")
	}
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), out); err != nil {
		return err
	}
	if strings.TrimSpace(out.Goal) == "" {
		return errors.New("analysis omitted its inferred goal")
	}
	return nil
}

func agentPolicyRiskWarnings(ast executor.WorkflowAST, policy AgentCapabilityPolicy) []string {
	normalized, _ := normalizeAgentCapabilityPolicy(ast, policy)
	capabilities := map[executor.NodeType]AgentIntegrationCapability{}
	for _, capability := range agentIntegrationCapabilities(ast) {
		capabilities[capability.NodeType] = capability
	}
	warnings := []string{}
	for _, grant := range normalized.Integrations {
		capability, exists := capabilities[grant.NodeType]
		if !exists {
			continue
		}
		label := capability.Label
		operations := map[string]AgentOperationCapability{}
		for _, operation := range capability.Operations {
			operations[operation.ID] = operation
		}
		for _, operationID := range grant.AllowedOperations {
			operation := operations[operationID]
			if operation.Sensitive {
				warnings = append(warnings, fmt.Sprintf("%s can read sensitive configuration through %s; verify that teammates should see those results.", label, strings.ToLower(operation.Label)))
			}
			if strings.HasPrefix(strings.ToLower(operationID), "search") {
				warnings = append(warnings, fmt.Sprintf("%s can search beyond some saved resource boundaries; verify that this is intentional.", label))
			}
			if operation.Effect == AgentEffectDestructive {
				warnings = append(warnings, fmt.Sprintf("%s includes %s. Every call will require requester approval.", label, strings.ToLower(operation.Label)))
			}
		}
		for _, field := range grant.AllowedOverrideFields {
			lower := strings.ToLower(field)
			if field == "url" || strings.HasSuffix(lower, "repo") || strings.HasSuffix(lower, "id") ||
				strings.Contains(lower, "channel") || strings.Contains(lower, "folder") || strings.Contains(lower, "project") ||
				strings.Contains(lower, "account") || strings.Contains(lower, "workspace") {
				warnings = append(warnings, fmt.Sprintf("%s may choose a different target through %s.", label, field))
			}
		}
	}
	return warnings
}
