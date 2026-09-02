package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"workflow-ai/server/internal/codingagent"
	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/email"
	"workflow-ai/server/internal/telemetry"

	"github.com/resend/resend-go/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ── Trigger source ────────────────────────────────────────────
//
// Handlers tag the run context with how the run was started so traces and
// metrics can split manual editor runs from webhook/schedule/API traffic.

type triggerCtxKey struct{}

func WithTrigger(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, triggerCtxKey{}, source)
}

func triggerFromContext(ctx context.Context) string {
	if s, ok := ctx.Value(triggerCtxKey{}).(string); ok && s != "" {
		return s
	}
	return "manual"
}

// Approval waits are bounded, always. An unbounded gate strands the run
// (holding a goroutine) and, on a schedule, strands a fresh one every cycle
// with nothing surfacing that they're waiting — so a missing or oversized
// timeout is corrected here rather than trusted from node data.
const (
	// DefaultApprovalTimeout covers "someone will look at this tomorrow" and is
	// applied to nodes saved before timeouts were mandatory (stored as 0).
	DefaultApprovalTimeout = 24 * 60 * 60
	// MaxApprovalTimeout is the ceiling any approval may wait.
	MaxApprovalTimeout = 3 * 24 * 60 * 60
)

// NormalizeApprovalTimeout resolves a node's configured wait (in seconds) to an
// enforceable one: absent/invalid becomes the default, anything beyond the
// ceiling is capped.
func NormalizeApprovalTimeout(secs int) int {
	if secs <= 0 {
		return DefaultApprovalTimeout
	}
	if secs > MaxApprovalTimeout {
		return MaxApprovalTimeout
	}
	return secs
}

type workflowIDCtxKey struct{}

// WithWorkflowID tags a run with the saved workflow it belongs to, so every
// call it makes is attributable to that workflow (the AST alone carries only a
// name). Empty for ad-hoc runs of an unsaved canvas.
func WithWorkflowID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, workflowIDCtxKey{}, id)
}

func workflowIDFromContext(ctx context.Context) string {
	s, _ := ctx.Value(workflowIDCtxKey{}).(string)
	return s
}

// The workflow's display name, set by RunWorkflow from the AST so node-level
// telemetry can name the flow without threading it through every signature.
type workflowNameCtxKey struct{}

func workflowNameFromContext(ctx context.Context) string {
	s, _ := ctx.Value(workflowNameCtxKey{}).(string)
	return s
}

// Workflow progress event timestamps use the same run-relative clock as the
// executor's ordinary node events. Long-running nodes emit from callbacks far
// below RunWorkflow, so the start time travels in context instead of exposing
// wall-clock milliseconds in an otherwise elapsed-time field.
type workflowStartedAtCtxKey struct{}

type workflowSnapshotCtxKey struct{}

// workflowSnapshotFromContext returns the exact graph this execution received,
// including unsaved canvas changes. Coding-agent jobs freeze this value before
// queueing so their callback authority cannot drift with later edits.
func workflowSnapshotFromContext(ctx context.Context) (WorkflowAST, bool) {
	wf, ok := ctx.Value(workflowSnapshotCtxKey{}).(WorkflowAST)
	return wf, ok
}

func workflowElapsedMillis(ctx context.Context) int64 {
	if startedAt, ok := ctx.Value(workflowStartedAtCtxKey{}).(time.Time); ok {
		return time.Since(startedAt).Milliseconds()
	}
	return 0
}

// IntegrationCredsLookup resolves the workflow owner's stored OAuth
// credentials for a provider. workspace is the tenant identifier where the
// API needs one (e.g. the Shopify shop domain); empty otherwise. Set by
// main.go; used when a node has no manual token.
var IntegrationCredsLookup func(userID, provider string) (token, workspace string)

// IntegrationCredsLookupForOrg is the tenant-safe production lookup. The
// legacy two-argument hook remains for isolated executor tests and callers
// without organization context, but servers should configure this variant.
var IntegrationCredsLookupForOrg func(orgID, userID, provider string) (token, workspace string)

// IntegrationUserTokenLookup resolves the workflow owner's user-identity
// grant (e.g. Slack xoxp- token) for providers whose actions can run either
// as the bot or on the connecting human's behalf. Set by main.go; returns ""
// when the connection predates user grants.
var IntegrationUserTokenLookup func(userID, provider string) string

// IntegrationUserTokenLookupForOrg is the tenant-safe equivalent for a
// provider's optional human-identity grant.
var IntegrationUserTokenLookupForOrg func(orgID, userID, provider string) string

// CodingAgentRun is installed by the server when Daytona and at least one
// coding runtime are configured. Keeping execution behind this hook lets the
// pure executor tests run without external infrastructure.
var CodingAgentRun func(context.Context, codingagent.SubmitRequest, func(codingagent.StreamEvent)) (jobID string, status string, result []byte, summary, lastError string, err error)

type integrationWorkspaceCtxKey struct{}

func integrationWorkspaceFromContext(ctx context.Context) string {
	workspace, _ := ctx.Value(integrationWorkspaceCtxKey{}).(string)
	return workspace
}

// ── Approval channels ──────────────────────────────────────────

var (
	approvalChannels   = make(map[string]chan bool)
	approvalChannelsMu sync.Mutex
)

func RegisterApprovalChannel(runID string) chan bool {
	ch := make(chan bool, 1)
	approvalChannelsMu.Lock()
	approvalChannels[runID] = ch
	approvalChannelsMu.Unlock()
	return ch
}

func ResolveApproval(runID string, approved bool) bool {
	approvalChannelsMu.Lock()
	ch, ok := approvalChannels[runID]
	approvalChannelsMu.Unlock()
	if !ok {
		return false
	}
	ch <- approved
	approvalChannelsMu.Lock()
	delete(approvalChannels, runID)
	approvalChannelsMu.Unlock()
	return true
}

// ── UUID ──────────────────────────────────────────────────────

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}

func strPtr(s string) *string    { return &s }
func ntPtr(t NodeType) *NodeType { return &t }

// ── Anthropic ─────────────────────────────────────────────────

// imageRef holds a parsed base64 data URL for vision API calls.
type imageRef struct {
	MediaType string // e.g. "image/jpeg"
	Data      string // raw base64, no prefix
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float64            `json:"temperature"`
	System      string             `json:"system"`
	Messages    []anthropicMessage `json:"messages"`
}
type anthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []anthropicBlock
}
type anthropicBlock struct {
	Type   string                `json:"type"`
	Text   string                `json:"text,omitempty"`
	Source *anthropicImageSource `json:"source,omitempty"`
}
type anthropicImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/jpeg"
	Data      string `json:"data"`
}
type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func callAnthropic(ctx context.Context, model, system, user string, temp float64, maxTok int, key string, imgs []imageRef) (out string, err error) {
	ctx, llmDone := telemetry.StartLLM(ctx, "anthropic", model)
	defer func() { llmDone(len(out), err) }()

	system = WithClock(system)

	var msgContent interface{}
	if len(imgs) > 0 {
		blocks := make([]anthropicBlock, 0, len(imgs)+1)
		for _, img := range imgs {
			blocks = append(blocks, anthropicBlock{
				Type: "image",
				Source: &anthropicImageSource{
					Type:      "base64",
					MediaType: img.MediaType,
					Data:      img.Data,
				},
			})
		}
		blocks = append(blocks, anthropicBlock{Type: "text", Text: user})
		msgContent = blocks
	} else {
		msgContent = user
	}

	body, _ := json.Marshal(anthropicRequest{
		Model: model, MaxTokens: maxTok, Temperature: temp,
		System:   system,
		Messages: []anthropicMessage{{Role: "user", Content: msgContent}},
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anthropic %d: %s", resp.StatusCode, raw)
	}
	// Record before parsing the content: a response we cannot read still cost
	// tokens, and dropping usage on a parse failure is a billing leak.
	telemetry.LLMTokens(ctx, "anthropic", model, usageFromAnthropic(raw))
	var r anthropicResponse
	_ = json.Unmarshal(raw, &r)
	for _, b := range r.Content {
		if b.Type == "text" {
			return b.Text, nil
		}
	}
	return "", nil
}

// ── OpenAI ────────────────────────────────────────────────────

type openAIRequest struct {
	Model string `json:"model"`
	// omitempty: gpt-5.x models reject temperature 0 — only send a real value.
	Temperature float64 `json:"temperature,omitempty"`
	// max_completion_tokens replaced max_tokens; gpt-5.x rejects the old name.
	MaxTokens int             `json:"max_completion_tokens"`
	Messages  []openAIMessage `json:"messages"`
}
type openAIMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []openAIBlock
}
type openAIBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *openAIImageURL `json:"image_url,omitempty"`
}
type openAIImageURL struct {
	URL string `json:"url"`
}
type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func callOpenAI(ctx context.Context, route llmRoute, model, system, user string, temp float64, maxTok int, imgs []imageRef) (out string, err error) {
	ctx, llmDone := telemetry.StartLLM(ctx, route.Provider, model)
	defer func() { llmDone(len(out), err) }()

	system = WithClock(system)

	var userContent interface{}
	if len(imgs) > 0 {
		blocks := make([]openAIBlock, 0, len(imgs)+1)
		for _, img := range imgs {
			blocks = append(blocks, openAIBlock{
				Type:     "image_url",
				ImageURL: &openAIImageURL{URL: "data:" + img.MediaType + ";base64," + img.Data},
			})
		}
		blocks = append(blocks, openAIBlock{Type: "text", Text: user})
		userContent = blocks
	} else {
		userContent = user
	}

	body, _ := json.Marshal(openAIRequest{
		Model: model, Temperature: temp, MaxTokens: maxTok,
		Messages: []openAIMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: userContent},
		},
	})
	body = withRouteBody(body, route.Body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, route.URL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+route.Key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openai %d: %s", resp.StatusCode, raw)
	}
	telemetry.LLMTokens(ctx, "openai", model, usageFromOpenAI(raw))
	var r openAIResponse
	_ = json.Unmarshal(raw, &r)
	if len(r.Choices) > 0 {
		return r.Choices[0].Message.Content, nil
	}
	return "", nil
}

// ── Template substitution ─────────────────────────────────────

// templateRe matches {{nodeId.output}} and, optionally, a dotted field path into
// that node's JSON output: {{nodeId.output.field}} or {{nodeId.output.a.b}}.
var templateRe = regexp.MustCompile(`\{\{([\w-]+)\.output((?:\.[\w-]+)*)\}\}`)

func substituteTemplates(text string, outputs map[string]string) string {
	return templateRe.ReplaceAllStringFunc(text, func(m string) string {
		parts := templateRe.FindStringSubmatch(m)
		if len(parts) < 3 {
			return m
		}
		base, ok := outputs[parts[1]]
		if !ok {
			telemetry.TemplateMiss(parts[1])
			return "[no output from " + parts[1] + "]"
		}
		if parts[2] == "" {
			return base // whole output — the long-standing behaviour
		}
		return resolveJSONPath(base, parts[2])
	})
}

// resolveJSONPath walks a leading-dot path (".a.b") into a JSON-encoded string,
// returning the leaf as text. Non-JSON or a missing field yields a readable
// placeholder rather than a hard failure.
func resolveJSONPath(raw, path string) string {
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return "[" + strings.TrimPrefix(path, ".") + " unavailable — output is not JSON]"
	}
	for _, key := range strings.Split(strings.TrimPrefix(path, "."), ".") {
		obj, ok := v.(map[string]any)
		if !ok {
			return "[no field " + key + "]"
		}
		if v, ok = obj[key]; !ok {
			return "[no field " + key + "]"
		}
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func isAnthropicModel(model string) bool { return strings.HasPrefix(model, "claude") }

func derefStr(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}
	return *p
}

// ── Topo sort ─────────────────────────────────────────────────

func topoSort(nodes []WorkflowASTNode, edges []WorkflowASTEdge) []string {
	inDeg := make(map[string]int, len(nodes))
	adj := make(map[string][]string, len(nodes))
	for _, n := range nodes {
		inDeg[n.ID] = 0
	}
	for _, e := range edges {
		adj[e.Source] = append(adj[e.Source], e.Target)
		inDeg[e.Target]++
	}
	var q []string
	for _, n := range nodes {
		if inDeg[n.ID] == 0 {
			q = append(q, n.ID)
		}
	}
	out := make([]string, 0, len(nodes))
	seen := make(map[string]bool, len(nodes))
	for len(q) > 0 {
		id := q[0]
		q = q[1:]
		out = append(out, id)
		seen[id] = true
		for _, nb := range adj[id] {
			inDeg[nb]--
			if inDeg[nb] == 0 {
				q = append(q, nb)
			}
		}
	}
	for _, n := range nodes {
		if !seen[n.ID] {
			out = append(out, n.ID)
		}
	}
	return out
}

// ── Execute single node ────────────────────────────────────────

// executeNode wraps the real node dispatch in a span plus execution metrics.
// Every path that runs a node — main graph, loop bodies, agent-chat single
// nodes — funnels through here.
func executeNode(ctx context.Context, node WorkflowASTNode, outputs map[string]string, edges []WorkflowASTEdge, keys APIKeys, runID, ownerID string, emit func(ExecutionEvent)) (string, error) {
	// Identity for everything this node touches. Stamped into ctx so the shared
	// outbound transport can attribute each endpoint it reaches back to this
	// workflow/run/node without any per-provider plumbing.
	cc := telemetry.CallContext{
		WorkflowID:   workflowIDFromContext(ctx),
		WorkflowName: workflowNameFromContext(ctx),
		RunID:        runID,
		Trigger:      triggerFromContext(ctx),
		NodeID:       node.ID,
		NodeLabel:    node.Data.Label,
		NodeType:     string(node.Data.NodeType),
		Op:           node.Data.IntegrationOp,
	}

	ctx, span := telemetry.Tracer.Start(ctx, "node "+string(node.Data.NodeType),
		trace.WithAttributes(cc.SpanAttributes()...))
	ctx = telemetry.WithCallContext(ctx, cc)

	start := time.Now()
	slog.DebugContext(ctx, "node started",
		"run_id", runID, "node_id", node.ID, "node_type", node.Data.NodeType, "label", node.Data.Label)
	out, err := runNodeInner(ctx, node, outputsForNode(node.ID, outputs, edges), edges, keys, runID, ownerID, emit)

	// The audit line: caller, arguments, and what came back.
	telemetry.RecordNodeCall(ctx, cc, node.Data, out, err, time.Since(start))

	// Integration nodes share one op field; provider identity is the node
	// type. Centralising here covers all 13 providers plus future ones.
	if op := node.Data.IntegrationOp; op != "" {
		telemetry.SpanAttrs(ctx,
			attribute.String("fernary.integration.provider", string(node.Data.NodeType)),
			attribute.String("fernary.integration.op", op))
		telemetry.RecordIntegrationCall(ctx, string(node.Data.NodeType), op, err, time.Since(start))
	}

	// Charge the nominal per-operation fee, on success only. LLM nodes are billed
	// on the tokens they actually reported (see telemetry.LLMTokens), so charging
	// them here as well would bill them twice.
	if err == nil && !tokenBilledNode(node.Data.NodeType) {
		telemetry.NodeSpend(ctx, string(node.Data.NodeType), node.Data.IntegrationOp)
	}

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "node execution failed",
			"run_id", runID, "node_id", node.ID, "node_type", node.Data.NodeType,
			"label", node.Data.Label, "duration_ms", time.Since(start).Milliseconds(),
			"error", err.Error())
	} else {
		slog.InfoContext(ctx, "node completed",
			"run_id", runID, "node_id", node.ID, "node_type", node.Data.NodeType,
			"label", node.Data.Label, "duration_ms", time.Since(start).Milliseconds(),
			"output_chars", len(out))
	}
	telemetry.RecordNodeExecution(ctx, string(node.Data.NodeType), err, time.Since(start))
	span.End()
	return out, err
}

// outputsForNode preserves support for the original canvas default
// {{previousNode.output}}. Modern templates use a concrete node ID, but older
// saved workflows rely on this alias. Resolve it from the first incoming edge,
// matching the executor's existing implicit-input behavior.
func outputsForNode(nodeID string, outputs map[string]string, edges []WorkflowASTEdge) map[string]string {
	for _, edge := range edges {
		if edge.Target != nodeID {
			continue
		}
		output, ok := outputs[edge.Source]
		if !ok {
			continue
		}
		resolved := make(map[string]string, len(outputs)+1)
		for id, value := range outputs {
			resolved[id] = value
		}
		resolved["previousNode"] = output
		return resolved
	}
	return outputs
}

func runNodeInner(ctx context.Context, node WorkflowASTNode, outputs map[string]string, edges []WorkflowASTEdge, keys APIKeys, runID, ownerID string, emit func(ExecutionEvent)) (string, error) {
	d := node.Data
	if d.IntegrationOp != "" && d.IntegrationToken == "" && IntegrationCredsLookupForOrg != nil {
		token, workspace := IntegrationCredsLookupForOrg(OrgFromContext(ctx), ownerID, string(d.NodeType))
		d.IntegrationToken = token
		if workspace != "" {
			ctx = context.WithValue(ctx, integrationWorkspaceCtxKey{}, workspace)
		}
	}
	switch d.NodeType {
	case NodeTypeTextInput:
		return derefStr(d.DefaultValue, "(empty text input)"), nil
	case NodeTypeImageInput:
		return derefStr(d.ImageURL, "(no image URL)"), nil
	case NodeTypeCodingAgent:
		if CodingAgentRun == nil {
			return "", errors.New("coding agent execution is not configured")
		}
		runtime := strings.TrimSpace(d.CodingAgentRuntime)
		if runtime == "" {
			runtime = codingagent.RuntimeCodex
		}
		workspaceMode := codingagent.WorkspaceMode(strings.TrimSpace(d.CodingAgentWorkspaceMode))
		if workspaceMode == "" {
			workspaceMode = codingagent.WorkspacePersistent
		}
		maxDuration := d.CodingAgentMaxDuration
		if maxDuration == 0 {
			maxDuration = 30 * 60
		}
		autoStop := d.CodingAgentAutoStopMinutes
		if autoStop == 0 {
			autoStop = 15
		}
		autoDelete := d.CodingAgentAutoDeleteMinutes
		if autoDelete == 0 {
			autoDelete = 7 * 24 * 60
		}
		repositoryProvider := strings.ToLower(strings.TrimSpace(d.CodingAgentRepositoryProvider))
		if repositoryProvider == "" {
			repositoryProvider = codingagent.RepositoryGitHub
		}
		// Runtime and repository domains are mandatory. Canvas configuration is
		// additive so a user cannot accidentally make authentication or cloning
		// fail while granting an extra package/documentation host.
		allowedDomains := []string{
			// The runtime's own control plane. Without these the agent cannot
			// start, so they are never the caller's to omit.
			"openai.com", "*.openai.com", "chatgpt.com", "*.chatgpt.com",
			// npm is the runtime/bootstrap registry. Other ecosystems are not
			// silently widened in restricted mode; owners add only the registries
			// their task needs and stay within Daytona's 20-domain ceiling.
			"registry.npmjs.org",
		}
		if repositoryProvider == codingagent.RepositoryGitLab {
			allowedDomains = append(allowedDomains, "gitlab.com", "*.gitlab.com")
		} else {
			allowedDomains = append(allowedDomains, "github.com", "*.github.com", "*.githubusercontent.com")
		}
		allowedDomains = append(allowedDomains, d.CodingAgentAllowedDomains...)
		if len(d.CodingAgentToolGrants) > 0 || len(d.CodingAgentToolNodes) > 0 {
			if callback, parseErr := url.Parse(codingagent.PublicBaseURL()); parseErr == nil && callback.Hostname() != "" {
				callbackHost := strings.ToLower(callback.Hostname())
				found := false
				for _, domain := range allowedDomains {
					if strings.EqualFold(domain, callbackHost) {
						found = true
						break
					}
				}
				if !found {
					allowedDomains = append(allowedDomains, callbackHost)
				}
			}
		}

		// Open egress is the default because a coding agent that cannot reach a
		// package registry or a documentation site fails at the work it exists
		// to do. Restriction is opt-in, and naming domains opts in by itself.
		//
		// Note the provider treats a domain list as deny-by-default and refuses
		// it alongside networkBlockAll, so "restricted" is expressed purely as a
		// non-empty list. Sending both would be rejected outright.
		networkOpen := !strings.EqualFold(strings.TrimSpace(d.CodingAgentNetworkAccess), "allowlist") &&
			len(d.CodingAgentAllowedDomains) == 0
		if networkOpen {
			allowedDomains = nil
		}
		task := substituteTemplates(d.CodingAgentTask, outputs)
		conversationKey := substituteTemplates(d.CodingAgentConversationKey, outputs)
		var toolWorkflow json.RawMessage
		if snapshot, ok := workflowSnapshotFromContext(ctx); ok {
			if raw, marshalErr := json.Marshal(snapshot); marshalErr == nil {
				toolWorkflow = raw
			}
		}
		jobID, status, result, summary, lastError, err := CodingAgentRun(ctx, codingagent.SubmitRequest{
			OrganizationID: OrgFromContext(ctx), UserID: ownerID,
			WorkflowID: workflowIDFromContext(ctx), WorkflowRunID: runID, NodeID: node.ID,
			ConversationKey: conversationKey, Runtime: runtime, Task: task,
			ToolWorkflow: toolWorkflow, ToolGrants: d.CodingAgentToolGrants,
			ToolNodeIDs: d.CodingAgentToolNodes,
			// The rendered task contains the upstream values it references. Do not
			// duplicate every upstream output into the durable job record, where it
			// would unnecessarily retain unrelated secrets.
			Input: nil,
			Policy: codingagent.ExecutionPolicy{
				WorkspaceMode: workspaceMode, RepositoryProvider: repositoryProvider,
				RepositoryID: strings.TrimSpace(d.CodingAgentRepositoryID), Repository: strings.TrimSpace(d.CodingAgentRepository),
				Branch: strings.TrimSpace(d.CodingAgentBranch), Model: strings.TrimSpace(d.CodingAgentModel),
				MaxDurationSeconds: maxDuration, AutoStopMinutes: autoStop, AutoDeleteMinutes: autoDelete,
				NetworkBlockAll: !networkOpen && len(allowedDomains) == 0, AllowedDomains: allowedDomains,
				AllowWorkspaceWrite: d.CodingAgentAllowWrite,
			},
		}, func(progress codingagent.StreamEvent) {
			event, ok := codingAgentProgressExecutionEvent(ctx, node, runID, progress)
			if ok {
				emit(event)
			}
		})
		if err != nil {
			return "", err
		}
		if status != string(models.CodingAgentJobSucceeded) {
			if lastError == "" {
				lastError = "coding agent task ended with status " + status
			}
			return "", fmt.Errorf("coding agent job %s: %s", jobID, lastError)
		}
		var output any
		if len(result) > 0 {
			_ = json.Unmarshal(result, &output)
		}
		response, _ := json.Marshal(map[string]any{
			"jobId": jobID, "status": status, "summary": summary, "result": output,
		})
		return string(response), nil
	case NodeTypeLLM:
		model := derefStr(d.Model, DefaultLLMModel)
		sys := substituteTemplates(derefStr(d.SystemPrompt, ""), outputs)
		userPromptTpl := derefStr(d.UserPrompt, "")

		// Extract image data URLs from any {{nodeId.output}} references so they
		// can be sent as vision content blocks instead of raw base64 text.
		promptOutputs := make(map[string]string, len(outputs))
		for k, v := range outputs {
			promptOutputs[k] = v
		}
		var imgs []imageRef
		for _, m := range templateRe.FindAllStringSubmatch(userPromptTpl, -1) {
			if len(m) < 2 {
				continue
			}
			nodeID := m[1]
			v, ok := promptOutputs[nodeID]
			if !ok || !strings.HasPrefix(v, "data:image/") {
				continue
			}
			// parse "data:image/jpeg;base64,<data>"
			rest := strings.TrimPrefix(v, "data:")
			parts := strings.SplitN(rest, ";base64,", 2)
			if len(parts) != 2 {
				continue
			}
			imgs = append(imgs, imageRef{MediaType: parts[0], Data: parts[1]})
			promptOutputs[nodeID] = "[attached image]"
		}
		usr := substituteTemplates(userPromptTpl, promptOutputs)

		temp := 0.7
		if d.Temperature != nil {
			temp = *d.Temperature
		}
		maxTok := 1024
		if d.MaxTokens != nil {
			maxTok = *d.MaxTokens
		}
		// A credit hold is only meaningful if a single call has a bounded worst
		// case, and an unset or arbitrarily large MaxTokens has none. Clamping to
		// the plan's ceiling is what makes the reservation a real check rather than
		// a gesture.
		maxTok = clampMaxTokens(ctx, maxTok)
		if d.OutputSchema != "" {
			sys += "\n\nRespond ONLY with valid JSON that matches this schema. No markdown, no explanation, just JSON:\n" + d.OutputSchema
		}
		if isAnthropicModel(model) {
			if keys.Anthropic == "" {
				return "", fmt.Errorf("Anthropic API key not set")
			}
			if d.EnableWebSearch {
				return callAnthropicWithTools(ctx, model, sys, usr, maxTok, keys.Anthropic, imgs, keys)
			}
			return callAnthropic(ctx, model, sys, usr, temp, maxTok, keys.Anthropic, imgs)
		}
		route, err := routeForModel(model, keys)
		if err != nil {
			return "", err
		}
		if d.EnableWebSearch {
			return callOpenAIWithTools(ctx, route, model, sys, usr, maxTok, imgs, keys)
		}
		return callOpenAI(ctx, route, model, sys, usr, temp, maxTok, imgs)
	case NodeTypeBranch:
		cond := derefStr(d.Condition, "")
		if cond == "" {
			return "", fmt.Errorf("branch %q has no condition set", node.Data.Label)
		}
		var up string
		for _, e := range edges {
			if e.Target == node.ID {
				if v, ok := outputs[e.Source]; ok {
					up = v
				}
				break
			}
		}
		// Conditions are evaluated by an LLM (plain-language or code-style both
		// work). No silent fallback: if every available provider fails, the node
		// errors with the real reason instead of quietly picking a path.
		system := `You are a boolean condition evaluator. The user will give you a condition and some text. Reply with exactly one word: true or false. No punctuation, no explanation.`
		prompt := fmt.Sprintf("Condition: %s\n\nText to evaluate:\n%s", cond, up)
		type attempt struct {
			name string
			call func() (string, error)
		}
		// One word of output, so the cheapest small model that answers correctly is
		// the right one. Ordered cheapest-first; each is only attempted if its key
		// is set, and a failure falls through to the next.
		var attempts []attempt
		for _, small := range []string{DefaultSmallModel, "gpt-5.4-mini"} {
			route, err := routeForModel(small, keys)
			if err != nil {
				continue
			}
			model, providerRoute := small, route
			attempts = append(attempts, attempt{providerRoute.Provider, func() (string, error) {
				return callOpenAI(ctx, providerRoute, model, system, prompt, 0, 8, nil)
			}})
		}
		if keys.Anthropic != "" {
			attempts = append(attempts, attempt{"anthropic", func() (string, error) {
				return callAnthropic(ctx, "claude-haiku-4-5-20251001", system, prompt, 0, 8, keys.Anthropic, nil)
			}})
		}
		if len(attempts) == 0 {
			return "", fmt.Errorf("branch conditions need an LLM — set GEMINI_API_KEY, OPENAI_API_KEY or ANTHROPIC_API_KEY on the server")
		}
		var failures []string
		for _, a := range attempts {
			result, err := a.call()
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", a.name, err))
				continue
			}
			verdict := strings.Trim(strings.TrimSpace(strings.ToLower(result)), `.!"' `+"`")
			if verdict == "true" || verdict == "false" {
				return verdict, nil
			}
			failures = append(failures, fmt.Sprintf("%s: expected true/false, got %q", a.name, result))
		}
		return "", fmt.Errorf("branch condition evaluation failed — %s", strings.Join(failures, "; "))
	case NodeTypeLoop:
		// Collect upstream output and return it — RunWorkflow handles actual iteration
		for _, e := range edges {
			if e.Target == node.ID {
				if v, ok := outputs[e.Source]; ok {
					return v, nil
				}
			}
		}
		return "[]", nil
	case NodeTypeTextOutput:
		for _, e := range edges {
			if e.Target == node.ID {
				if v, ok := outputs[e.Source]; ok {
					return v, nil
				}
			}
		}
		return "(no input)", nil

	case NodeTypeHTTPRequest:
		url := substituteTemplates(d.URL, outputs)
		// Only real web schemes — blocks file://, gopher://, etc.
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return "", fmt.Errorf("URL must start with http:// or https://")
		}
		method := d.Method
		if method == "" {
			method = "GET"
		}
		var reqBody io.Reader
		if d.RequestBody != "" {
			body := substituteTemplates(d.RequestBody, outputs)
			reqBody = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		if d.RequestHeaders != "" {
			var headers map[string]string
			if err := json.Unmarshal([]byte(substituteTemplates(d.RequestHeaders, outputs)), &headers); err == nil {
				for k, v := range headers {
					req.Header.Set(k, v)
				}
			}
		}
		client := ssrfSafeClient(30 * time.Second)
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		// Cap the response body to avoid memory exhaustion from a hostile endpoint.
		respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		if err != nil {
			return "", err
		}
		return string(respBytes), nil

	case NodeTypeEmailSend:
		// Multiple recipients supported: comma-separated. Delivery is a
		// broadcast — each recipient gets their OWN copy (batch send), so
		// addresses are never exposed to the other recipients.
		var recipients []string
		for _, r := range strings.Split(substituteTemplates(d.EmailTo, outputs), ",") {
			if r = strings.TrimSpace(r); r != "" {
				recipients = append(recipients, r)
			}
		}
		if len(recipients) == 0 {
			return "", fmt.Errorf("email node %q has no recipient", d.Label)
		}
		to := strings.Join(recipients, ", ")
		subject := substituteTemplates(d.EmailSubject, outputs)
		body := substituteTemplates(d.EmailBody, outputs)
		resendKey := os.Getenv("RESEND_API_KEY")
		if resendKey == "" {
			return fmt.Sprintf(`{"status":"sent","to":"%s","recipients":%d,"subject":"%s","note":"dev_mode_no_key"}`, to, len(recipients), subject), nil
		}
		// The body is authored as Markdown → HTML, wrapped in a styled but
		// unbranded shell (this mail is from the user's workflow, not Fernary).
		// Text keeps the raw Markdown as a readable plain-text fallback.
		htmlBody := email.WrapBrandless(email.RenderMarkdown(body), subject)
		client := resend.NewClient(resendKey)
		// Same verified sending domain as platform mail — the shell is unbranded
		// but the envelope still has to come from a domain we own.
		from := email.FromAddress()
		if len(recipients) == 1 {
			sent, err := client.Emails.Send(&resend.SendEmailRequest{
				From: from, To: recipients, Subject: subject, Text: body, Html: htmlBody,
			})
			telemetry.EmailSent(ctx, "workflow_node", err)
			if err != nil {
				return "", fmt.Errorf("resend error: %w", err)
			}
			return fmt.Sprintf(`{"status":"sent","to":"%s","subject":"%s","id":"%s"}`, to, subject, sent.Id), nil
		}
		reqs := make([]*resend.SendEmailRequest, 0, len(recipients))
		for _, r := range recipients {
			reqs = append(reqs, &resend.SendEmailRequest{
				From: from, To: []string{r}, Subject: subject, Text: body, Html: htmlBody,
			})
		}
		var ids []string
		for start := 0; start < len(reqs); start += 100 { // Resend batch cap
			sent, err := client.Batch.Send(reqs[start:min(start+100, len(reqs))])
			telemetry.EmailSent(ctx, "workflow_node", err)
			if err != nil {
				return "", fmt.Errorf("resend batch error: %w", err)
			}
			for _, e := range sent.Data {
				ids = append(ids, e.Id)
			}
		}
		idsJSON, _ := json.Marshal(ids)
		return fmt.Sprintf(`{"status":"sent","recipients":%d,"subject":"%s","ids":%s}`, len(recipients), subject, idsJSON), nil

	case NodeTypeHumanApproval:
		message := substituteTemplates(d.ApprovalMessage, outputs)
		if message == "" {
			message = "Please review and approve or reject this step."
		}
		ch := RegisterApprovalChannel(runID + ":" + node.ID)
		emit(ExecutionEvent{
			ID:      newUUID(),
			Type:    EventNodeWaiting,
			NodeID:  strPtr(node.ID),
			Message: message,
			RunID:   runID,
		})

		// Send notification email if configured
		if d.ApprovalEmail != "" {
			appURL := os.Getenv("APP_URL")
			if appURL == "" {
				appURL = "http://localhost:4905"
			}
			runURL := fmt.Sprintf("%s/run/%s", appURL, runID)

			// Find the upstream node output (the content to review)
			var upstreamOutput string
			for _, e := range edges {
				if e.Target == node.ID {
					if v, ok := outputs[e.Source]; ok {
						upstreamOutput = v
					}
					break
				}
			}

			resendKey := os.Getenv("RESEND_API_KEY")
			if resendKey != "" {
				emailText := fmt.Sprintf("%s\n\n---\n\nContent to review:\n\n%s\n\n---\n\nApprove or reject here:\n%s", message, upstreamOutput, runURL)
				// A platform notification → Fernary-branded shell + CTA button.
				htmlBody := email.Action("Action required", message, upstreamOutput, runURL, "Review & respond", node.Data.Label)
				client := resend.NewClient(resendKey)
				_, mailErr := client.Emails.Send(&resend.SendEmailRequest{
					From:    email.FromAddress(),
					To:      []string{d.ApprovalEmail},
					Subject: "Action Required: " + node.Data.Label,
					Text:    emailText,
					Html:    htmlBody,
				})
				telemetry.EmailSent(ctx, "approval", mailErr)
			}
		}
		timeout := NormalizeApprovalTimeout(d.ApprovalTimeout)
		waitStart := time.Now()
		result := "cancelled"
		telemetry.ApprovalPending(ctx, 1)
		defer func() {
			telemetry.ApprovalPending(ctx, -1)
			telemetry.ApprovalResolved(ctx, result, time.Since(waitStart))
			slog.InfoContext(ctx, "approval resolved",
				"run_id", runID, "node_id", node.ID, "result", result,
				"waited_ms", time.Since(waitStart).Milliseconds())
		}()
		select {
		case approved := <-ch:
			if approved {
				result = "approved"
				return "approved", nil
			}
			result = "rejected"
			return "rejected", nil
		case <-time.After(time.Duration(timeout) * time.Second):
			result = "timeout"
			approvalChannelsMu.Lock()
			delete(approvalChannels, runID+":"+node.ID)
			approvalChannelsMu.Unlock()
			return "rejected", fmt.Errorf("approval timed out after %d seconds", timeout)
		case <-ctx.Done():
			approvalChannelsMu.Lock()
			delete(approvalChannels, runID+":"+node.ID)
			approvalChannelsMu.Unlock()
			return "", fmt.Errorf("workflow cancelled")
		}

	case NodeTypeWebhookTrigger:
		// DefaultValue is injected with the received payload by ReceiveWebhook handler
		if d.DefaultValue != nil && *d.DefaultValue != "" && *d.DefaultValue != "null" {
			return *d.DefaultValue, nil
		}
		return `{"trigger":"webhook"}`, nil

	case NodeTypeScheduledTrigger:
		return `{"trigger":"scheduled","time":"` + time.Now().Format(time.RFC3339) + `"}`, nil

	case NodeTypeIntegrationTrigger:
		// The hooks handler injects the normalized event here. The placeholder
		// describes the shape a real event will have rather than being an empty
		// object, so a manual run on the canvas can still exercise the nodes
		// downstream instead of failing on a missing field.
		if d.DefaultValue != nil && *d.DefaultValue != "" && *d.DefaultValue != "null" {
			return *d.DefaultValue, nil
		}
		return `{"provider":"` + d.TriggerProvider + `","event":"` + d.TriggerEvent +
			`","resource":"` + d.TriggerResourceID + `","data":{},"test":true}`, nil

	case NodeTypeData:
		return runDataNode(ctx, d, outputs, ownerID)

	case NodeTypeNotion:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "notion")
		}
		if token == "" {
			return "", fmt.Errorf("Notion is not connected — use Connect Notion in the node settings")
		}
		switch d.IntegrationOp {
		case "create_page":
			return notionCreatePage(ctx, token,
				substituteTemplates(d.NotionDatabaseId, outputs),
				substituteTemplates(d.NotionTitle, outputs),
				substituteTemplates(d.NotionContent, outputs))
		case "query_database":
			return notionQueryDatabase(ctx, token,
				substituteTemplates(d.NotionDatabaseId, outputs),
				substituteTemplates(d.NotionFilter, outputs))
		case "append_blocks":
			return notionAppendBlocks(ctx, token,
				substituteTemplates(d.NotionPageId, outputs),
				substituteTemplates(d.NotionContent, outputs))
		case "update_page":
			return notionUpdatePage(ctx, token,
				substituteTemplates(d.NotionPageId, outputs),
				substituteTemplates(d.NotionProperties, outputs))
		case "get_page_content":
			return notionGetPageContent(ctx, token,
				substituteTemplates(d.NotionPageId, outputs))
		case "search":
			return notionSearch(ctx, token,
				substituteTemplates(d.NotionQuery, outputs))
		case "add_comment":
			return notionAddComment(ctx, token,
				substituteTemplates(d.NotionPageId, outputs),
				substituteTemplates(d.NotionContent, outputs))
		case "create_database":
			return notionCreateDatabase(ctx, token,
				substituteTemplates(d.NotionParentPageId, outputs),
				substituteTemplates(d.NotionTitle, outputs),
				substituteTemplates(d.NotionSchema, outputs))
		case "get_database":
			return notionGetDatabase(ctx, token,
				substituteTemplates(d.NotionDatabaseId, outputs))
		case "create_subpage":
			return notionCreateSubpage(ctx, token,
				substituteTemplates(d.NotionParentPageId, outputs),
				substituteTemplates(d.NotionTitle, outputs),
				substituteTemplates(d.NotionContent, outputs))
		case "archive_page":
			return notionArchivePage(ctx, token,
				substituteTemplates(d.NotionPageId, outputs))
		case "list_users":
			return notionListUsers(ctx, token)
		case "list_comments":
			return notionListComments(ctx, token,
				substituteTemplates(d.NotionPageId, outputs))
		default:
			return "", fmt.Errorf("unknown Notion operation: %s", d.IntegrationOp)
		}

	case NodeTypeLinear:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "linear")
		}
		if token == "" {
			return "", fmt.Errorf("Linear is not connected — use Connect Linear in the node settings")
		}
		switch d.IntegrationOp {
		case "create_issue":
			return linearCreateIssue(ctx, token,
				substituteTemplates(d.LinearTeamId, outputs),
				substituteTemplates(d.LinearTitle, outputs),
				substituteTemplates(d.LinearDescription, outputs),
				d.LinearPriority)
		case "get_issues":
			return linearGetIssues(ctx, token,
				substituteTemplates(d.LinearTeamId, outputs),
				d.LinearLimit)
		case "create_comment":
			return linearCreateComment(ctx, token,
				substituteTemplates(d.LinearIssueId, outputs),
				substituteTemplates(d.LinearCommentBody, outputs))
		case "update_issue":
			return linearUpdateIssue(ctx, token, substituteTemplates(d.LinearIssueId, outputs), linearUpdateInput{
				Title:       substituteTemplates(d.LinearTitle, outputs),
				Description: substituteTemplates(d.LinearDescription, outputs),
				Priority:    d.LinearPriority,
				StateID:     substituteTemplates(d.LinearStateId, outputs),
				AssigneeID:  substituteTemplates(d.LinearAssigneeId, outputs),
				ProjectID:   substituteTemplates(d.LinearProjectId, outputs),
			})
		case "search_issues":
			return linearSearchIssues(ctx, token,
				substituteTemplates(d.LinearQuery, outputs),
				d.LinearLimit)
		case "list_projects":
			return linearListProjects(ctx, token)
		case "get_issue":
			return linearGetIssue(ctx, token,
				substituteTemplates(d.LinearIssueId, outputs))
		case "list_teams":
			return linearListTeams(ctx, token)
		case "list_users":
			return linearListUsers(ctx, token)
		case "list_states":
			return linearListStates(ctx, token,
				substituteTemplates(d.LinearTeamId, outputs))
		case "list_labels":
			return linearListLabels(ctx, token,
				substituteTemplates(d.LinearTeamId, outputs))
		case "add_label":
			return linearAddLabel(ctx, token,
				substituteTemplates(d.LinearIssueId, outputs),
				substituteTemplates(d.LinearLabelId, outputs))
		case "archive_issue":
			return linearArchiveIssue(ctx, token,
				substituteTemplates(d.LinearIssueId, outputs))
		case "create_project":
			return linearCreateProject(ctx, token,
				substituteTemplates(d.LinearTeamId, outputs),
				substituteTemplates(d.LinearTitle, outputs),
				substituteTemplates(d.LinearDescription, outputs))
		case "list_cycles":
			return linearListCycles(ctx, token,
				substituteTemplates(d.LinearTeamId, outputs))
		case "list_comments":
			return linearListComments(ctx, token,
				substituteTemplates(d.LinearIssueId, outputs))
		default:
			return "", fmt.Errorf("unknown Linear operation: %s", d.IntegrationOp)
		}

	case NodeTypeGithub:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "github")
		}
		if token == "" {
			return "", fmt.Errorf("GitHub is not connected — use Connect GitHub in the node settings")
		}
		return runGithub(ctx, token, d, outputs)

	case NodeTypeGitlab:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "gitlab")
		}
		if token == "" {
			return "", fmt.Errorf("GitLab is not connected — use Connect GitLab in the node settings")
		}
		return runGitlab(ctx, token, d, outputs)

	case NodeTypeGmail:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "gmail")
		}
		if token == "" {
			return "", fmt.Errorf("Gmail is not connected — use Connect Gmail in the node settings")
		}
		return runGmail(ctx, token, d, outputs)

	case NodeTypeStripe:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "stripe")
		}
		if token == "" {
			return "", fmt.Errorf("Stripe is not connected — use Connect Stripe in the node settings")
		}
		return runStripe(ctx, token, d, outputs)

	case NodeTypeShopify:
		token := substituteTemplates(d.IntegrationToken, outputs)
		shop := integrationWorkspaceFromContext(ctx)
		if (token == "" || shop == "") && IntegrationCredsLookup != nil {
			legacyToken, legacyShop := IntegrationCredsLookup(ownerID, "shopify")
			if token == "" {
				token = legacyToken
			}
			if shop == "" {
				shop = legacyShop
			}
		}
		if token == "" {
			return "", fmt.Errorf("Shopify is not connected — use Connect Shopify in the node settings")
		}
		if shop == "" {
			return "", fmt.Errorf("Shopify shop domain is missing — reconnect the store")
		}
		return runShopify(ctx, token, shop, d, outputs)

	case NodeTypeGoogleMeet:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "googlemeet")
		}
		if token == "" {
			return "", fmt.Errorf("Google Meet is not connected — use Connect Google Meet in the node settings")
		}
		return runGoogleMeet(ctx, token, d, outputs)

	case NodeTypeGoogleSlides:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "googleslides")
		}
		if token == "" {
			return "", fmt.Errorf("Google Slides is not connected — use Connect Google Slides in the node settings")
		}
		return runGoogleSlides(ctx, token, d, outputs)

	case NodeTypeGoogleForms:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "googleforms")
		}
		if token == "" {
			return "", fmt.Errorf("Google Forms is not connected — use Connect Google Forms in the node settings")
		}
		return runGoogleForms(ctx, token, d, outputs)

	case NodeTypeGoogleTasks:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "googletasks")
		}
		if token == "" {
			return "", fmt.Errorf("Google Tasks is not connected — use Connect Google Tasks in the node settings")
		}
		return runGoogleTasks(ctx, token, d, outputs)

	case NodeTypeGoogleChat:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "googlechat")
		}
		if token == "" {
			return "", fmt.Errorf("Google Chat is not connected — use Connect Google Chat in the node settings")
		}
		return runGoogleChat(ctx, token, d, outputs)

	case NodeTypeGoogleKeep:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "googlekeep")
		}
		if token == "" {
			return "", fmt.Errorf("Google Keep is not connected — use Connect Google Keep in the node settings")
		}
		return runGoogleKeep(ctx, token, d, outputs)

	case NodeTypeResend:
		key := substituteTemplates(d.IntegrationToken, outputs)
		if key == "" && IntegrationCredsLookup != nil {
			key, _ = IntegrationCredsLookup(ownerID, "resend")
		}
		if key == "" {
			return "", fmt.Errorf("Resend is not connected — add your Resend API key in the node settings")
		}
		return runResend(ctx, key, d, outputs)

	case NodeTypeSendGrid:
		key := substituteTemplates(d.IntegrationToken, outputs)
		if key == "" && IntegrationCredsLookup != nil {
			key, _ = IntegrationCredsLookup(ownerID, "sendgrid")
		}
		if key == "" {
			return "", fmt.Errorf("SendGrid is not connected — add your SendGrid API key in the node settings")
		}
		return runSendGrid(ctx, key, d, outputs)

	case NodeTypeKit:
		key := substituteTemplates(d.IntegrationToken, outputs)
		if key == "" && IntegrationCredsLookup != nil {
			key, _ = IntegrationCredsLookup(ownerID, "kit")
		}
		if key == "" {
			return "", fmt.Errorf("Kit is not connected — add your Kit V4 API key in the node settings")
		}
		return runKit(ctx, key, d, outputs)

	case NodeTypeAirtable:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "airtable")
		}
		if token == "" {
			return "", fmt.Errorf("Airtable is not connected — use Connect Airtable in the node settings")
		}
		return runAirtable(ctx, token, d, outputs)

	case NodeTypeClickUp:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "clickup")
		}
		if token == "" {
			return "", fmt.Errorf("ClickUp is not connected — use Connect ClickUp in the node settings")
		}
		return runClickUp(ctx, token, d, outputs)

	case NodeTypeMonday:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "monday")
		}
		if token == "" {
			return "", fmt.Errorf("monday.com is not connected — use Connect monday.com in the node settings")
		}
		return runMonday(ctx, token, d, outputs)

	case NodeTypeAsana:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "asana")
		}
		if token == "" {
			return "", fmt.Errorf("Asana is not connected — use Connect Asana in the node settings")
		}
		return runAsana(ctx, token, d, outputs)

	case NodeTypeTypeform:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "typeform")
		}
		if token == "" {
			return "", fmt.Errorf("Typeform is not connected — use Connect Typeform in the node settings")
		}
		return runTypeform(ctx, token, d, outputs)

	case NodeTypeCalendly:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "calendly")
		}
		if token == "" {
			return "", fmt.Errorf("Calendly is not connected — use Connect Calendly in the node settings")
		}
		return runCalendly(ctx, token, d, outputs)

	case NodeTypeDropbox:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "dropbox")
		}
		if token == "" {
			return "", fmt.Errorf("Dropbox is not connected — use Connect Dropbox in the node settings")
		}
		return runDropbox(ctx, token, d, outputs)

	case NodeTypeNetlify:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "netlify")
		}
		if token == "" {
			return "", fmt.Errorf("Netlify is not connected — use Connect Netlify in the node settings")
		}
		return runNetlify(ctx, token, d, outputs)

	case NodeTypeVercel:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "vercel")
		}
		if token == "" {
			return "", fmt.Errorf("Vercel is not connected — use Connect Vercel in the node settings")
		}
		return runVercel(ctx, token, d, outputs)

	case NodeTypeSupabase:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "supabase")
		}
		if token == "" {
			return "", fmt.Errorf("Supabase is not connected — use Connect Supabase in the node settings")
		}
		return runSupabase(ctx, token, d, outputs)

	case NodeTypeGumroad:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "gumroad")
		}
		if token == "" {
			return "", fmt.Errorf("Gumroad is not connected — use Connect Gumroad in the node settings")
		}
		return runGumroad(ctx, token, d, outputs)

	case NodeTypeGoogleSearchConsole:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "googlesearchconsole")
		}
		if token == "" {
			return "", fmt.Errorf("Google Search Console is not connected — use Connect Google Search Console in the node settings")
		}
		return runGoogleSearchConsole(ctx, token, d, outputs)

	case NodeTypeGoogleContacts:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "googlecontacts")
		}
		if token == "" {
			return "", fmt.Errorf("Google Contacts is not connected — use Connect Google Contacts in the node settings")
		}
		return runGoogleContacts(ctx, token, d, outputs)

	case NodeTypeHubspot:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "hubspot")
		}
		if token == "" {
			return "", fmt.Errorf("HubSpot is not connected — use Connect HubSpot in the node settings")
		}
		return runHubSpot(ctx, token, d, outputs)

	case NodeTypeFront:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "front")
		}
		if token == "" {
			return "", fmt.Errorf("Front is not connected — use Connect Front in the node settings")
		}
		return runFront(ctx, token, d, outputs)

	case NodeTypeSentry:
		token := substituteTemplates(d.IntegrationToken, outputs)
		org := integrationWorkspaceFromContext(ctx)
		if (token == "" || org == "") && IntegrationCredsLookup != nil {
			legacyToken, legacyOrg := IntegrationCredsLookup(ownerID, "sentry")
			if token == "" {
				token = legacyToken
			}
			if org == "" {
				org = legacyOrg
			}
		}
		if token == "" {
			return "", fmt.Errorf("Sentry is not connected — use Connect Sentry in the node settings")
		}
		return runSentry(ctx, token, org, d, outputs)

	case NodeTypeGranola:
		key := substituteTemplates(d.IntegrationToken, outputs)
		if key == "" && IntegrationCredsLookup != nil {
			key, _ = IntegrationCredsLookup(ownerID, "granola")
		}
		if key == "" {
			return "", fmt.Errorf("Granola is not connected — add your Granola API key in the node settings")
		}
		return runGranola(ctx, key, d, outputs)

	case NodeTypeJira:
		token := substituteTemplates(d.IntegrationToken, outputs)
		cloudID := integrationWorkspaceFromContext(ctx)
		if (token == "" || cloudID == "") && IntegrationCredsLookup != nil {
			var t string
			var legacyCloudID string
			t, legacyCloudID = IntegrationCredsLookup(ownerID, "jira")
			if token == "" {
				token = t
			}
			if cloudID == "" {
				cloudID = legacyCloudID
			}
		}
		if token == "" {
			return "", fmt.Errorf("Jira is not connected — use Connect Jira in the node settings")
		}
		return runJira(ctx, token, cloudID, d, outputs)

	case NodeTypeConfluence:
		token := substituteTemplates(d.IntegrationToken, outputs)
		cloudID := integrationWorkspaceFromContext(ctx)
		if (token == "" || cloudID == "") && IntegrationCredsLookup != nil {
			var t string
			var legacyCloudID string
			t, legacyCloudID = IntegrationCredsLookup(ownerID, "confluence")
			if token == "" {
				token = t
			}
			if cloudID == "" {
				cloudID = legacyCloudID
			}
		}
		if token == "" {
			return "", fmt.Errorf("Confluence is not connected — use Connect Confluence in the node settings")
		}
		return runConfluence(ctx, token, cloudID, d, outputs)

	case NodeTypeBitbucket:
		token := substituteTemplates(d.IntegrationToken, outputs)
		workspace := integrationWorkspaceFromContext(ctx)
		if (token == "" || workspace == "") && IntegrationCredsLookup != nil {
			var t string
			var legacyWorkspace string
			t, legacyWorkspace = IntegrationCredsLookup(ownerID, "bitbucket")
			if token == "" {
				token = t
			}
			if workspace == "" {
				workspace = legacyWorkspace
			}
		}
		if token == "" {
			return "", fmt.Errorf("Bitbucket is not connected — use Connect Bitbucket in the node settings")
		}
		return runBitbucket(ctx, token, workspace, d, outputs)

	case NodeTypeGoogleCalendar:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "googlecalendar")
		}
		if token == "" {
			return "", fmt.Errorf("Google Calendar is not connected — use Connect Google Calendar in the node settings")
		}
		return runGoogleCalendar(ctx, token, d, outputs)

	case NodeTypeOutlook:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "outlook")
		}
		if token == "" {
			return "", fmt.Errorf("Outlook is not connected — use Connect Outlook in the node settings")
		}
		return runOutlook(ctx, token, d, outputs)

	case NodeTypeSlack:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "slack")
		}
		if token == "" {
			return "", fmt.Errorf("Slack is not connected — use Connect Slack in the node settings")
		}
		userToken := ""
		if IntegrationUserTokenLookupForOrg != nil {
			userToken = IntegrationUserTokenLookupForOrg(OrgFromContext(ctx), ownerID, "slack")
		} else if IntegrationUserTokenLookup != nil {
			userToken = IntegrationUserTokenLookup(ownerID, "slack")
		}
		return runSlack(ctx, token, userToken, d, outputs)

	case NodeTypeGoogleDrive:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "googledrive")
		}
		if token == "" {
			return "", fmt.Errorf("Google Drive is not connected — use Connect Google Drive in the node settings")
		}
		return runGoogleDrive(ctx, token, d, outputs)

	case NodeTypeGoogleDocs:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "googledocs")
		}
		if token == "" {
			return "", fmt.Errorf("Google Docs is not connected — use Connect Google Docs in the node settings")
		}
		return runGoogleDocs(ctx, token, d, outputs)

	case NodeTypeGoogleSheets:
		token := substituteTemplates(d.IntegrationToken, outputs)
		if token == "" && IntegrationCredsLookup != nil {
			token, _ = IntegrationCredsLookup(ownerID, "googlesheets")
		}
		if token == "" {
			return "", fmt.Errorf("Google Sheets is not connected — use Connect Google Sheets in the node settings")
		}
		return runGoogleSheets(ctx, token, d, outputs)
	}
	return "", fmt.Errorf("unknown node type: %s", d.NodeType)
}

func codingAgentProgressExecutionEvent(ctx context.Context, node WorkflowASTNode, runID string, progress codingagent.StreamEvent) (ExecutionEvent, bool) {
	if strings.TrimSpace(progress.Message) == "" && len(progress.Payload) == 0 {
		return ExecutionEvent{}, false
	}
	payload := make(map[string]any, len(progress.Payload)+1)
	for key, value := range progress.Payload {
		payload[key] = value
	}
	payload["activityType"] = progress.Type
	return ExecutionEvent{
		ID: newUUID(), Type: EventNodeProgress, NodeID: strPtr(node.ID), NodeLabel: strPtr(node.Data.Label),
		NodeType: ntPtr(node.Data.NodeType), Message: progress.Message, Payload: payload,
		Timestamp: workflowElapsedMillis(ctx), RunID: runID,
	}, true
}

// ── Loop helpers ───────────────────────────────────────────────

// reachableFrom returns all node IDs reachable via edges from startID (not including startID).
func reachableFrom(startID string, edges []WorkflowASTEdge) map[string]bool {
	visited := make(map[string]bool)
	queue := []string{startID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range edges {
			if e.Source == cur && !visited[e.Target] {
				visited[e.Target] = true
				queue = append(queue, e.Target)
			}
		}
	}
	return visited
}

// stripCodeFences removes markdown code fences (```json … ``` or ``` … ```) from s.
func stripCodeFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence line (e.g. "```json")
	if nl := strings.Index(s, "\n"); nl != -1 {
		s = s[nl+1:]
	} else {
		return s // malformed — nothing after the fence
	}
	// Drop the closing fence
	if strings.HasSuffix(strings.TrimSpace(s), "```") {
		s = s[:strings.LastIndex(s, "```")]
	}
	return strings.TrimSpace(s)
}

// itemPreview renders a loop item compactly enough to identify a pass in a log
// line. Whitespace is collapsed because items are often pretty-printed JSON,
// and a preview that wraps over six lines defeats the point of a preview.
func itemPreview(item string) string {
	return truncateStr(strings.Join(strings.Fields(item), " "), 80)
}

// extractLoopItems parses input JSON and extracts the array at the given dot-path field.
// If field is empty, input itself must be an array. Falls back to line-splitting for plain text.
func extractLoopItems(input, field string) []string {
	if input == "" {
		return nil
	}
	// LLMs sometimes wrap JSON in markdown code fences even when instructed not to.
	// Strip them before attempting to parse.
	clean := stripCodeFences(input)
	var data interface{}
	if err := json.Unmarshal([]byte(clean), &data); err != nil {
		// If stripping didn't help, try the original
		if err2 := json.Unmarshal([]byte(input), &data); err2 != nil {
			var lines []string
			for _, l := range strings.Split(strings.TrimSpace(input), "\n") {
				if l = strings.TrimSpace(l); l != "" {
					lines = append(lines, l)
				}
			}
			return lines
		}
	}
	if field != "" {
		current := data
		for _, part := range strings.Split(field, ".") {
			m, ok := current.(map[string]interface{})
			if !ok {
				return nil
			}
			current = m[part]
		}
		data = current
	}
	arr, ok := data.([]interface{})
	if !ok {
		b, _ := json.Marshal(data)
		return []string{string(b)}
	}
	result := make([]string, len(arr))
	for i, item := range arr {
		if s, ok := item.(string); ok {
			result[i] = s
		} else {
			b, _ := json.Marshal(item)
			result[i] = string(b)
		}
	}
	return result
}

// ── Run workflow ──────────────────────────────────────────────

// RunWorkflow executes a workflow AST. ownerID is the workflow owner's user
// ID, used to resolve their integration connections (OAuth tokens).
func RunWorkflow(ctx context.Context, workflow WorkflowAST, keys APIKeys, runID, ownerID, orgID string, emit EmitFn, runOptions ...RunOptions) {
	start := time.Now()
	ctx = context.WithValue(ctx, workflowStartedAtCtxKey{}, start)
	var options RunOptions
	if len(runOptions) > 0 {
		options = runOptions[0]
	}
	toolSnapshot := workflow
	if options.ToolWorkflow != nil {
		toolSnapshot = *options.ToolWorkflow
	}
	ctx = context.WithValue(ctx, workflowSnapshotCtxKey{}, toolSnapshot)
	workflow = workflowWithoutCodingAgentTools(workflow, options.OnlyNodeID)

	trigger := triggerFromContext(ctx)
	ctx, span := telemetry.Tracer.Start(ctx, "workflow.run",
		trace.WithAttributes(
			attribute.String("fernary.run.id", runID),
			attribute.String("fernary.workflow.name", workflow.Name),
			attribute.String("fernary.trigger", trigger),
			attribute.Int("fernary.workflow.nodes", len(workflow.Nodes)),
		))
	defer span.End()

	// Run-scoped stores live in-memory for this run only.
	ctx = withRunScope(ctx)
	ctx = WithOrg(ctx, orgID)
	ctx = context.WithValue(ctx, workflowNameCtxKey{}, workflow.Name)

	slog.InfoContext(ctx, "workflow run started",
		"run_id", runID, "workflow", workflow.Name, "trigger", trigger,
		"nodes", len(workflow.Nodes), "edges", len(workflow.Edges))

	telemetry.AddActiveRuns(ctx, 1)
	runStatus := "completed"
	defer func() {
		telemetry.AddActiveRuns(ctx, -1)
		telemetry.RecordWorkflowRun(ctx, runStatus, trigger, time.Since(start))
		slog.InfoContext(ctx, "workflow run finished",
			"run_id", runID, "workflow", workflow.Name, "status", runStatus,
			"duration_ms", time.Since(start).Milliseconds())
	}()
	// Watch the event stream for the terminal error — RunWorkflow reports
	// failure through events, not a return value. The ctx-aware slog carries
	// the trace id into Loki, so error logs link back to their trace. Every
	// event also lands on the run span as a span event: the trace alone
	// replays the run's timeline.
	userEmit := emit
	// Event counters for the log ceilings. Execution is sequential, so these
	// need no lock — every emit in a run happens on this goroutine.
	eventCount := 0
	truncationAnnounced := false
	emit = func(ev ExecutionEvent) {
		telemetry.RecordRunEvent(ctx, string(ev.Type))
		evAttrs := []attribute.KeyValue{}
		if ev.NodeLabel != nil {
			evAttrs = append(evAttrs, attribute.String("node.label", *ev.NodeLabel))
		}
		if ev.Message != "" {
			evAttrs = append(evAttrs, attribute.String("message", truncateStr(ev.Message, 200)))
		}
		span.AddEvent(string(ev.Type), trace.WithAttributes(evAttrs...))

		switch ev.Type {
		case EventWorkflowError:
			runStatus = "error"
			span.SetStatus(codes.Error, ev.Message)
			slog.ErrorContext(ctx, "workflow run failed",
				"run_id", runID, "workflow", workflow.Name, "trigger", trigger, "reason", ev.Message)
		case EventNodeWaiting:
			slog.InfoContext(ctx, "node waiting for approval",
				"run_id", runID, "message", truncateStr(ev.Message, 200))
		}

		// The ceilings live here because this is the one point every event
		// passes through, however deep in the graph it was raised.
		if ev.Output != nil && len(*ev.Output) > maxEventOutput {
			clipped := truncateStr(*ev.Output, maxEventOutput)
			ev.Output = &clipped
			ev.OutputTruncated = true
		}
		eventCount++
		if eventCount > maxRunEvents {
			// A run still has to report how it ended, so the terminal pair is
			// never dropped — otherwise a truncated log would look like a run
			// that hung.
			if ev.Type != EventWorkflowCompleted && ev.Type != EventWorkflowError {
				if !truncationAnnounced {
					truncationAnnounced = true
					userEmit(ExecutionEvent{
						ID:        newUUID(),
						Type:      EventLogTruncated,
						Timestamp: time.Since(start).Milliseconds(),
						Message: fmt.Sprintf("Log truncated after %d events — later steps are omitted. The run itself is unaffected.",
							maxRunEvents),
					})
				}
				return
			}
		}
		userEmit(ev)
	}

	mk := func(t ExecutionEventType, node *WorkflowASTNode, output *string, msg string) ExecutionEvent {
		ev := ExecutionEvent{ID: newUUID(), Type: t, Timestamp: time.Since(start).Milliseconds(), Message: msg, Output: output}
		if node != nil {
			ev.NodeID = strPtr(node.ID)
			ev.NodeLabel = strPtr(node.Data.Label)
			nt := node.Data.NodeType
			ev.NodeType = ntPtr(nt)
		}
		return ev
	}

	// mkIter is mk for events raised inside a loop body: the same event,
	// stamped with which pass produced it.
	mkIter := func(t ExecutionEventType, node *WorkflowASTNode, output *string, msg string, ref *IterationRef) ExecutionEvent {
		ev := mk(t, node, output, msg)
		ev.Iteration = ref
		return ev
	}

	startEv := mk(EventWorkflowStarted, nil, nil, "Workflow started")
	startEv.RunID = runID
	emit(startEv)

	nodes, edges := workflow.Nodes, workflow.Edges
	order := topoSort(nodes, edges)

	inDeg := make(map[string]int, len(nodes))
	for _, e := range edges {
		inDeg[e.Target]++
	}
	// Only the chosen graph supplies roots. Without this every node with no
	// inbound edge starts a run, including ones parked on the canvas to be
	// called as an agent's tools rather than to fire on their own.
	runnable := runnableNodes(nodes, edges, options.EntryNodeID)
	enabled := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if inDeg[n.ID] == 0 && runnable[n.ID] {
			enabled[n.ID] = true
		}
	}

	outputs := make(map[string]string, len(nodes))
	nodeMap := make(map[string]WorkflowASTNode, len(nodes))
	for _, n := range nodes {
		nodeMap[n.ID] = n
	}

	// A node test uses the same credential lookup, billing, telemetry, approval,
	// and dispatch code as a graph run, but seeds template resolution from the
	// most recent outputs of upstream nodes instead of re-executing them.
	if options.OnlyNodeID != "" {
		node, ok := nodeMap[options.OnlyNodeID]
		if !ok {
			emit(mk(EventWorkflowError, nil, nil, "Test node is not present in this workflow"))
			return
		}
		upstream := upstreamNodeSet(options.OnlyNodeID, edges)
		for id, output := range options.InitialOutputs {
			if upstream[id] {
				outputs[id] = output
			}
		}
		for _, requiredID := range requiredOutputNodeIDs(node, edges) {
			if _, ok := outputs[requiredID]; ok {
				continue
			}
			label := requiredID
			if requiredNode, exists := nodeMap[requiredID]; exists && requiredNode.Data.Label != "" {
				label = requiredNode.Data.Label
			}
			emit(mk(EventWorkflowError, nil, nil, fmt.Sprintf("Run this graph first — %q has no previous output", label)))
			return
		}

		emit(mk(EventNodeStarted, &node, nil, node.Data.Label))
		out, err := executeNode(ctx, node, outputs, edges, keys, runID, ownerID, emit)
		if err != nil {
			emit(mk(EventNodeError, &node, nil, "Error: "+err.Error()))
			emit(mk(EventWorkflowError, nil, nil, fmt.Sprintf("Node test failed at %q", node.Data.Label)))
			return
		}
		emit(mk(EventNodeOutput, &node, strPtr(out), node.Data.Label))
		emit(mk(EventNodeCompleted, &node, nil, node.Data.Label+" completed"))
		emit(mk(EventWorkflowCompleted, nil, nil, "Node test completed successfully"))
		return
	}

	loopHandled := make(map[string]bool) // nodes fully handled inside a loop iteration

	// Path bookkeeping. Which way a run went through a branching graph is
	// otherwise unrecoverable after the fact: the executor just skips nodes it
	// never enabled, so an untaken path and a path that errored out before its
	// turn leave exactly the same trace — none.
	executed := make(map[string]bool) // nodes that actually ran
	rejected := make(map[string]bool) // targets of an edge a decision declined

	emitEdgeTaken := func(from WorkflowASTNode, e WorkflowASTEdge, handle string) {
		label := e.Target
		if target, ok := nodeMap[e.Target]; ok && target.Data.Label != "" {
			label = target.Data.Label
		}
		ev := mk(EventEdgeTaken, &from, nil, from.Data.Label+" → "+label)
		ev.EdgeID = e.ID
		ev.SourceHandle = handle
		emit(ev)
	}

	// emitSkips names every node that never ran, and why. A node the log simply
	// omits is ambiguous — the reader cannot tell a path that was not chosen
	// from one the run never got to — and only the executor knows which it was.
	// A node whose incoming edge a branch explicitly declined is named as such;
	// everything further downstream is reported as not reached, which is what
	// it literally was.
	emitSkips := func() {
		for _, n := range nodes {
			if executed[n.ID] {
				continue
			}
			reason, msg := SkipNotReached, n.Data.Label+" was not reached"
			if rejected[n.ID] {
				reason, msg = SkipBranchNotTaken, n.Data.Label+" was not on the path taken"
			}
			ev := mk(EventNodeSkipped, &n, nil, msg)
			ev.SkipReason = reason
			emit(ev)
		}
	}

	for _, id := range order {
		if loopHandled[id] {
			continue
		}
		node, ok := nodeMap[id]
		if !ok || !enabled[id] {
			continue
		}
		select {
		case <-ctx.Done():
			emit(mk(EventWorkflowError, nil, nil, "Workflow cancelled"))
			emitSkips()
			return
		default:
		}

		// ── Loop node: iterate over items ─────────────────────
		if node.Data.NodeType == NodeTypeLoop {
			emit(mk(EventNodeStarted, &node, nil, node.Data.Label))

			// Get upstream output
			var upstreamOutput string
			for _, e := range edges {
				if e.Target == id {
					if v, ok2 := outputs[e.Source]; ok2 {
						upstreamOutput = v
						break
					}
				}
			}
			field := ""
			if node.Data.LoopOverField != nil {
				field = *node.Data.LoopOverField
			}
			items := extractLoopItems(upstreamOutput, field)

			// Find all body nodes (reachable from loop node)
			bodySet := reachableFrom(id, edges)

			// Get body nodes in topo order
			var bodyNodes []WorkflowASTNode
			for _, n := range nodes {
				if bodySet[n.ID] {
					bodyNodes = append(bodyNodes, n)
				}
			}
			// Only include edges where both endpoints are body nodes so that
			// the loop node → first-body-node edge doesn't inflate inDegree
			// and cause body nodes to never reach inDeg==0 in topoSort.
			var bodyEdges []WorkflowASTEdge
			for _, e := range edges {
				if bodySet[e.Source] && bodySet[e.Target] {
					bodyEdges = append(bodyEdges, e)
				}
			}
			bodyOrder := topoSort(bodyNodes, bodyEdges)

			// Execute loop body for each item
			var iterResults []string
			for i, item := range items {
				// The pass is identified structurally rather than by prefixing
				// "[3/10] " onto each message, which is what the log used to do
				// — grouping a pass then meant parsing that text back out.
				ref := &IterationRef{
					LoopNodeID:  id,
					Index:       i,
					Total:       len(items),
					ItemPreview: itemPreview(item),
				}
				emit(mkIter(EventIterationStarted, &node, nil,
					fmt.Sprintf("Item %d of %d", i+1, len(items)), ref))

				// Nodes emit events of their own from inside executeNode — an
				// approval waiting, for one — and those belong to this pass too.
				iterEmit := func(ev ExecutionEvent) {
					if ev.Iteration == nil {
						ev.Iteration = ref
					}
					emit(ev)
				}

				iterOutputs := make(map[string]string, len(outputs)+len(bodyNodes)+1)
				for k, v := range outputs {
					iterOutputs[k] = v
				}
				iterOutputs[id] = item // current item is loop node's output for this iteration

				var lastOut string
				iterStatus := "ok"
				for _, bodyID := range bodyOrder {
					if !bodySet[bodyID] {
						continue
					}
					bodyNode, ok2 := nodeMap[bodyID]
					if !ok2 {
						continue
					}
					select {
					case <-ctx.Done():
						emit(mk(EventWorkflowError, nil, nil, "Workflow cancelled"))
						emitSkips()
						return
					default:
					}
					emit(mkIter(EventNodeStarted, &bodyNode, nil, bodyNode.Data.Label, ref))
					out, err := executeNode(ctx, bodyNode, iterOutputs, edges, keys, runID, ownerID, iterEmit)
					if err != nil {
						emit(mkIter(EventNodeError, &bodyNode, nil, "Error: "+err.Error(), ref))
						lastOut = fmt.Sprintf(`{"error":%q}`, err.Error())
						iterStatus = "error"
						break
					}
					iterOutputs[bodyID] = out
					emit(mkIter(EventNodeOutput, &bodyNode, strPtr(out), bodyNode.Data.Label, ref))
					emit(mkIter(EventNodeCompleted, &bodyNode, nil, bodyNode.Data.Label+" completed", ref))
					lastOut = out
				}

				doneMsg := fmt.Sprintf("Item %d of %d completed", i+1, len(items))
				if iterStatus == "error" {
					doneMsg = fmt.Sprintf("Item %d of %d failed", i+1, len(items))
				}
				doneEv := mkIter(EventIterationCompleted, &node, strPtr(lastOut), doneMsg, ref)
				doneEv.Status = iterStatus
				emit(doneEv)

				iterResults = append(iterResults, lastOut)
			}

			// Mark all body nodes as handled (skip in outer loop)
			for bodyID := range bodySet {
				loopHandled[bodyID] = true
				executed[bodyID] = true
				outputs[bodyID] = "[loop iteration]"
			}

			// Enable nodes downstream of the loop body (outside the body)
			for bodyID := range bodySet {
				for _, e := range edges {
					if e.Source == bodyID && !bodySet[e.Target] {
						enabled[e.Target] = true
					}
				}
			}

			resultJSON, _ := json.Marshal(iterResults)
			outputs[id] = string(resultJSON)
			executed[id] = true
			emit(mk(EventNodeOutput, &node, strPtr(string(resultJSON)), node.Data.Label))
			emit(mk(EventNodeCompleted, &node, nil, node.Data.Label+" completed"))
			continue
		}

		// ── Normal node execution ─────────────────────────────
		emit(mk(EventNodeStarted, &node, nil, node.Data.Label))

		out, err := executeNode(ctx, node, outputs, edges, keys, runID, ownerID, emit)
		if err != nil {
			emit(mk(EventNodeError, &node, nil, "Error: "+err.Error()))
			emit(mk(EventWorkflowError, nil, nil, fmt.Sprintf("Workflow failed at %q", node.Data.Label)))
			emitSkips()
			return
		}

		executed[id] = true
		outputs[id] = out
		emit(mk(EventNodeOutput, &node, strPtr(out), node.Data.Label))
		emit(mk(EventNodeCompleted, &node, nil, node.Data.Label+" completed"))

		for _, e := range edges {
			if e.Source != id {
				continue
			}
			if node.Data.NodeType == NodeTypeBranch || node.Data.NodeType == NodeTypeHumanApproval {
				if e.SourceHandle != nil && *e.SourceHandle == out {
					enabled[e.Target] = true
					emitEdgeTaken(node, e, out)
				} else {
					// Recorded so the skip sweep can say "not on the path
					// taken" here, and plain "not reached" further downstream.
					rejected[e.Target] = true
				}
			} else {
				enabled[e.Target] = true
				emitEdgeTaken(node, e, "")
			}
		}
	}
	emitSkips()
	emit(mk(EventWorkflowCompleted, nil, nil, "Workflow completed successfully"))
}

// workflowWithoutCodingAgentTools keeps tool-only nodes available in the
// frozen authorization snapshot while preventing a normal/scheduled run from
// executing those same side effects as disconnected graph roots. An explicit
// single-node test remains available for owners who want to test a backing
// integration node itself.
func workflowWithoutCodingAgentTools(workflow WorkflowAST, onlyNodeID string) WorkflowAST {
	if onlyNodeID != "" {
		return workflow
	}
	toolOnly := map[string]bool{}
	for _, node := range workflow.Nodes {
		if node.Data.NodeType != NodeTypeCodingAgent {
			continue
		}
		for _, grant := range node.Data.CodingAgentToolGrants {
			for _, nodeID := range grant.NodeIDs {
				toolOnly[nodeID] = true
			}
			if grant.NodeID != "" {
				toolOnly[grant.NodeID] = true
			}
		}
		for _, nodeID := range node.Data.CodingAgentToolNodes {
			toolOnly[nodeID] = true
		}
	}
	if len(toolOnly) == 0 {
		return workflow
	}
	filtered := workflow
	filtered.Nodes = make([]WorkflowASTNode, 0, len(workflow.Nodes))
	for _, node := range workflow.Nodes {
		if !toolOnly[node.ID] {
			filtered.Nodes = append(filtered.Nodes, node)
		}
	}
	filtered.Edges = make([]WorkflowASTEdge, 0, len(workflow.Edges))
	for _, edge := range workflow.Edges {
		if !toolOnly[edge.Source] && !toolOnly[edge.Target] {
			filtered.Edges = append(filtered.Edges, edge)
		}
	}
	return filtered
}

func upstreamNodeSet(targetID string, edges []WorkflowASTEdge) map[string]bool {
	upstream := make(map[string]bool)
	seen := map[string]bool{targetID: true}
	queue := []string{targetID}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, edge := range edges {
			if edge.Target != current || seen[edge.Source] {
				continue
			}
			seen[edge.Source] = true
			upstream[edge.Source] = true
			queue = append(queue, edge.Source)
		}
	}
	return upstream
}

// requiredOutputNodeIDs finds every cached upstream value a standalone node
// test needs. Explicit {{node.output}} templates always count. Nodes whose
// runtime consumes their first incoming edge implicitly count that source too.
func requiredOutputNodeIDs(node WorkflowASTNode, edges []WorkflowASTEdge) []string {
	seen := make(map[string]bool)
	var required []string
	add := func(id string) {
		if id == "" || id == node.ID || seen[id] {
			return
		}
		seen[id] = true
		required = append(required, id)
	}

	if raw, err := json.Marshal(node.Data); err == nil {
		for _, match := range templateRe.FindAllSubmatch(raw, -1) {
			if len(match) > 1 {
				id := string(match[1])
				if id == "previousNode" {
					for _, edge := range edges {
						if edge.Target == node.ID {
							id = edge.Source
							break
						}
					}
				}
				add(id)
			}
		}
	}

	switch node.Data.NodeType {
	case NodeTypeBranch, NodeTypeLoop, NodeTypeTextOutput:
		for _, edge := range edges {
			if edge.Target == node.ID {
				add(edge.Source)
				break
			}
		}
	}
	return required
}
