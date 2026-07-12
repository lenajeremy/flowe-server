package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/resend/resend-go/v2"
)

// IntegrationCredsLookup resolves the workflow owner's stored OAuth
// credentials for a provider. workspace is the tenant identifier where the
// API needs one (e.g. the Shopify shop domain); empty otherwise. Set by
// main.go; used when a node has no manual token.
var IntegrationCredsLookup func(userID, provider string) (token, workspace string)

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

func callAnthropic(ctx context.Context, model, system, user string, temp float64, maxTok int, key string, imgs []imageRef) (string, error) {
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
	Model       string          `json:"model"`
	Temperature float64         `json:"temperature"`
	MaxTokens   int             `json:"max_tokens"`
	Messages    []openAIMessage `json:"messages"`
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

func callOpenAI(ctx context.Context, model, system, user string, temp float64, maxTok int, key string, imgs []imageRef) (string, error) {
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
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openai %d: %s", resp.StatusCode, raw)
	}
	var r openAIResponse
	_ = json.Unmarshal(raw, &r)
	if len(r.Choices) > 0 {
		return r.Choices[0].Message.Content, nil
	}
	return "", nil
}

// ── Template substitution ─────────────────────────────────────

var templateRe = regexp.MustCompile(`\{\{([\w-]+)\.output\}\}`)

func substituteTemplates(text string, outputs map[string]string) string {
	return templateRe.ReplaceAllStringFunc(text, func(m string) string {
		parts := templateRe.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		if v, ok := outputs[parts[1]]; ok {
			return v
		}
		return "[no output from " + parts[1] + "]"
	})
}

func isAnthropicModel(model string) bool { return strings.HasPrefix(model, "claude") }

func derefStr(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}
	return *p
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// ── Branch condition evaluator ────────────────────────────────

var (
	reStrCmp     = regexp.MustCompile(`^output\s*(===?|!==?)\s*['"](.*)['"]$`)
	reNumCmp     = regexp.MustCompile(`^output\s*(===?|!==?|>=?|<=?)\s*(-?\d+(?:\.\d+)?)$`)
	reLenCmp     = regexp.MustCompile(`^output\.length\s*(===?|!==?|>=?|<=?)\s*(\d+)$`)
	reIncludes   = regexp.MustCompile(`^output\.includes\(['"](.+)['"]\)$`)
	reStartsWith = regexp.MustCompile(`^output\.startsWith\(['"](.+)['"]\)$`)
	reEndsWith   = regexp.MustCompile(`^output\.endsWith\(['"](.+)['"]\)$`)
)

func cmpFloats(a float64, op string, b float64) bool {
	switch op {
	case "==", "===":
		return a == b
	case "!=", "!==":
		return a != b
	case ">":
		return a > b
	case "<":
		return a < b
	case ">=":
		return a >= b
	case "<=":
		return a <= b
	}
	return false
}

func evaluateBranchCondition(condition, upstream string) string {
	condition = strings.TrimSpace(condition)
	if condition == "true" {
		return "true"
	}
	if condition == "false" {
		return "false"
	}
	truthy := upstream != "" && upstream != "false" && upstream != "0" && upstream != "null" && upstream != "undefined"
	if condition == "output" {
		return boolStr(truthy)
	}
	if condition == "!output" {
		return boolStr(!truthy)
	}
	if m := reIncludes.FindStringSubmatch(condition); len(m) > 0 {
		return boolStr(strings.Contains(upstream, m[1]))
	}
	if m := reStartsWith.FindStringSubmatch(condition); len(m) > 0 {
		return boolStr(strings.HasPrefix(upstream, m[1]))
	}
	if m := reEndsWith.FindStringSubmatch(condition); len(m) > 0 {
		return boolStr(strings.HasSuffix(upstream, m[1]))
	}
	if m := reLenCmp.FindStringSubmatch(condition); len(m) > 0 {
		rhs, _ := strconv.ParseFloat(m[2], 64)
		return boolStr(cmpFloats(float64(len(upstream)), m[1], rhs))
	}
	if m := reStrCmp.FindStringSubmatch(condition); len(m) > 0 {
		switch m[1] {
		case "==", "===":
			return boolStr(upstream == m[2])
		case "!=", "!==":
			return boolStr(upstream != m[2])
		}
	}
	if m := reNumCmp.FindStringSubmatch(condition); len(m) > 0 {
		rhs, err := strconv.ParseFloat(m[2], 64)
		if err == nil {
			if lhs, err2 := strconv.ParseFloat(upstream, 64); err2 == nil {
				return boolStr(cmpFloats(lhs, m[1], rhs))
			}
			switch m[1] {
			case "==", "===":
				return boolStr(upstream == m[2])
			case "!=", "!==":
				return boolStr(upstream != m[2])
			}
		}
	}
	return "false"
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

func executeNode(ctx context.Context, node WorkflowASTNode, outputs map[string]string, edges []WorkflowASTEdge, keys APIKeys, runID, ownerID string, emit func(ExecutionEvent)) (string, error) {
	d := node.Data
	switch d.NodeType {
	case NodeTypeTextInput:
		return derefStr(d.DefaultValue, "(empty text input)"), nil
	case NodeTypeImageInput:
		return derefStr(d.ImageURL, "(no image URL)"), nil
	case NodeTypeLLM:
		model := derefStr(d.Model, "gpt-4o")
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
		if keys.OpenAI == "" {
			return "", fmt.Errorf("OpenAI API key not set")
		}
		if d.EnableWebSearch {
			return callOpenAIWithTools(ctx, model, sys, usr, maxTok, keys.OpenAI, imgs, keys)
		}
		return callOpenAI(ctx, model, sys, usr, temp, maxTok, keys.OpenAI, imgs)
	case NodeTypeBranch:
		cond := derefStr(d.Condition, "false")
		var up string
		for _, e := range edges {
			if e.Target == node.ID {
				if v, ok := outputs[e.Source]; ok {
					up = v
				}
				break
			}
		}
		// Use LLM to evaluate the condition when an API key is available.
		// This lets users write plain-language conditions like "Does the text mention an error?"
		if keys.Anthropic != "" || keys.OpenAI != "" {
			system := `You are a boolean condition evaluator. The user will give you a condition and some text. Reply with exactly one word: true or false. No punctuation, no explanation.`
			prompt := fmt.Sprintf("Condition: %s\n\nText to evaluate:\n%s", cond, up)
			var result string
			var err error
			if keys.Anthropic != "" {
				result, err = callAnthropic(ctx, "claude-haiku-4-5-20251001", system, prompt, 0, 5, keys.Anthropic, nil)
			} else {
				result, err = callOpenAI(ctx, "gpt-4o-mini", system, prompt, 0, 5, keys.OpenAI, nil)
			}
			if err == nil {
				result = strings.TrimSpace(strings.ToLower(result))
				if result == "true" || result == "false" {
					return result, nil
				}
			}
		}
		// Fallback: regex-based evaluation (no API key, or LLM returned unexpected output)
		return evaluateBranchCondition(cond, up), nil
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
			if err := json.Unmarshal([]byte(d.RequestHeaders), &headers); err == nil {
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
		to := substituteTemplates(d.EmailTo, outputs)
		subject := substituteTemplates(d.EmailSubject, outputs)
		body := substituteTemplates(d.EmailBody, outputs)
		resendKey := os.Getenv("RESEND_API_KEY")
		if resendKey == "" {
			return fmt.Sprintf(`{"status":"sent","to":"%s","subject":"%s","note":"dev_mode_no_key"}`, to, subject), nil
		}
		client := resend.NewClient(resendKey)
		params := &resend.SendEmailRequest{
			From:    "workflow-ai <noreply@usecelery.io>",
			To:      []string{to},
			Subject: subject,
			Text:    body,
		}
		sent, err := client.Emails.Send(params)
		if err != nil {
			return "", fmt.Errorf("resend error: %w", err)
		}
		return fmt.Sprintf(`{"status":"sent","to":"%s","subject":"%s","id":"%s"}`, to, subject, sent.Id), nil

	case NodeTypeHumanApproval:
		message := d.ApprovalMessage
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
				emailBody := fmt.Sprintf("%s\n\n---\n\nContent to review:\n\n%s\n\n---\n\nApprove or reject here:\n%s", message, upstreamOutput, runURL)
				client := resend.NewClient(resendKey)
				_, _ = client.Emails.Send(&resend.SendEmailRequest{
					From:    "workflow-ai <noreply@usecelery.io>",
					To:      []string{d.ApprovalEmail},
					Subject: "Action Required: " + node.Data.Label,
					Text:    emailBody,
				})
			}
		}
		timeout := d.ApprovalTimeout
		if timeout <= 0 {
			timeout = 86400 * 7 // 7-day default for "no timeout"
		}
		select {
		case approved := <-ch:
			if approved {
				return "approved", nil
			}
			return "rejected", nil
		case <-time.After(time.Duration(timeout) * time.Second):
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
		var shop string
		if token == "" && IntegrationCredsLookup != nil {
			token, shop = IntegrationCredsLookup(ownerID, "shopify")
		}
		if token == "" {
			return "", fmt.Errorf("Shopify is not connected — use Connect Shopify in the node settings")
		}
		if shop == "" {
			return "", fmt.Errorf("Shopify shop domain is missing — reconnect the store")
		}
		return runShopify(ctx, token, shop, d, outputs)

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
		return runSlack(ctx, token, d, outputs)

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
func RunWorkflow(ctx context.Context, workflow WorkflowAST, keys APIKeys, runID, ownerID string, emit EmitFn) {
	start := time.Now()

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

	startEv := mk(EventWorkflowStarted, nil, nil, "Workflow started")
	startEv.RunID = runID
	emit(startEv)

	nodes, edges := workflow.Nodes, workflow.Edges
	order := topoSort(nodes, edges)

	inDeg := make(map[string]int, len(nodes))
	for _, e := range edges {
		inDeg[e.Target]++
	}
	enabled := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if inDeg[n.ID] == 0 {
			enabled[n.ID] = true
		}
	}

	outputs := make(map[string]string, len(nodes))
	nodeMap := make(map[string]WorkflowASTNode, len(nodes))
	for _, n := range nodes {
		nodeMap[n.ID] = n
	}

	loopHandled := make(map[string]bool) // nodes fully handled inside a loop iteration

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
				iterOutputs := make(map[string]string, len(outputs)+len(bodyNodes)+1)
				for k, v := range outputs {
					iterOutputs[k] = v
				}
				iterOutputs[id] = item // current item is loop node's output for this iteration

				var lastOut string
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
						return
					default:
					}
					iterLabel := fmt.Sprintf("[%d/%d] %s", i+1, len(items), bodyNode.Data.Label)
					emit(mk(EventNodeStarted, &bodyNode, nil, iterLabel))
					out, err := executeNode(ctx, bodyNode, iterOutputs, edges, keys, runID, ownerID, emit)
					if err != nil {
						emit(mk(EventNodeError, &bodyNode, nil, "Error: "+err.Error()))
						lastOut = fmt.Sprintf(`{"error":%q}`, err.Error())
						break
					}
					iterOutputs[bodyID] = out
					emit(mk(EventNodeOutput, &bodyNode, strPtr(out), iterLabel))
					emit(mk(EventNodeCompleted, &bodyNode, nil, iterLabel+" completed"))
					lastOut = out
				}
				iterResults = append(iterResults, lastOut)
			}

			// Mark all body nodes as handled (skip in outer loop)
			for bodyID := range bodySet {
				loopHandled[bodyID] = true
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
			return
		}

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
				}
			} else {
				enabled[e.Target] = true
			}
		}
	}
	emit(mk(EventWorkflowCompleted, nil, nil, "Workflow completed successfully"))
}
