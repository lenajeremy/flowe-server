package executor

// NodeType mirrors the TypeScript NodeType union.
type NodeType string

const (
	NodeTypeTextInput    NodeType = "textInput"
	NodeTypeImageInput   NodeType = "imageInput"
	NodeTypeLLM          NodeType = "llm"
	NodeTypeBranch       NodeType = "branch"
	NodeTypeLoop         NodeType = "loop"
	NodeTypeTextOutput   NodeType = "textOutput"
	NodeTypeHTTPRequest      NodeType = "httpRequest"
	NodeTypeEmailSend        NodeType = "emailSend"
	NodeTypeHumanApproval    NodeType = "humanApproval"
	NodeTypeWebhookTrigger   NodeType = "webhookTrigger"
	NodeTypeScheduledTrigger NodeType = "scheduledTrigger"
	NodeTypeNotion           NodeType = "notion"
	NodeTypeLinear           NodeType = "linear"
)

type FlowNodeData struct {
	NodeType      NodeType `json:"nodeType"`
	Label         string   `json:"label"`
	DefaultValue  *string  `json:"defaultValue,omitempty"`
	ImageURL      *string  `json:"imageUrl,omitempty"`
	Model         *string  `json:"model,omitempty"`
	SystemPrompt  *string  `json:"systemPrompt,omitempty"`
	UserPrompt    *string  `json:"userPrompt,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	MaxTokens     *int     `json:"maxTokens,omitempty"`
	Condition     *string  `json:"condition,omitempty"`
	LoopOverField *string  `json:"loopOverField,omitempty"`
	Mode          *string  `json:"mode,omitempty"`

	// httpRequest
	URL            string `json:"url"`
	Method         string `json:"method"`         // GET, POST, PUT, DELETE, PATCH
	RequestHeaders string `json:"requestHeaders"` // JSON string
	RequestBody    string `json:"requestBody"`

	// emailSend
	EmailTo      string `json:"emailTo"`
	EmailSubject string `json:"emailSubject"`
	EmailBody    string `json:"emailBody"`

	// humanApproval
	ApprovalMessage string `json:"approvalMessage"`
	ApprovalTimeout int    `json:"approvalTimeout"` // seconds, 0 = no timeout
	ApprovalEmail   string `json:"approvalEmail"`   // optional email to notify

	// scheduledTrigger
	Interval string `json:"interval"` // "5m","15m","30m","1h","6h","12h","24h"

	// LLM structured output
	OutputSchema string `json:"outputSchema"` // JSON schema string

	// LLM web tools
	EnableWebSearch bool `json:"enableWebSearch,omitempty"` // gives the LLM web_search + read_url tools

	// notion / linear shared
	IntegrationToken string `json:"integrationToken,omitempty"`
	IntegrationOp    string `json:"integrationOp,omitempty"`

	// notion
	NotionDatabaseId string `json:"notionDatabaseId,omitempty"`
	NotionPageId     string `json:"notionPageId,omitempty"`
	NotionTitle      string `json:"notionTitle,omitempty"`
	NotionContent    string `json:"notionContent,omitempty"`
	NotionFilter     string `json:"notionFilter,omitempty"`

	// linear
	LinearTeamId      string `json:"linearTeamId,omitempty"`
	LinearIssueId     string `json:"linearIssueId,omitempty"`
	LinearTitle       string `json:"linearTitle,omitempty"`
	LinearDescription string `json:"linearDescription,omitempty"`
	LinearPriority    int    `json:"linearPriority,omitempty"`
	LinearCommentBody string `json:"linearCommentBody,omitempty"`
	LinearLimit       int    `json:"linearLimit,omitempty"`
}

type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type WorkflowASTNode struct {
	ID       string       `json:"id"`
	Type     NodeType     `json:"type"`
	Position Position     `json:"position"`
	Data     FlowNodeData `json:"data"`
}

type WorkflowASTEdge struct {
	ID           string  `json:"id"`
	Source       string  `json:"source"`
	Target       string  `json:"target"`
	SourceHandle *string `json:"sourceHandle,omitempty"`
	TargetHandle *string `json:"targetHandle,omitempty"`
}

type WorkflowAST struct {
	Version   string            `json:"version"`
	Name      string            `json:"name"`
	Nodes     []WorkflowASTNode `json:"nodes"`
	Edges     []WorkflowASTEdge `json:"edges"`
	CreatedAt string            `json:"createdAt"`
}

type APIKeys struct {
	Anthropic string
	OpenAI    string
	Brave     string
	Jina      string
}

type RunRequest struct {
	Workflow   WorkflowAST `json:"workflow"`
	WorkflowID string      `json:"workflowId,omitempty"`
}

type ExecutionEventType string

const (
	EventWorkflowStarted   ExecutionEventType = "workflow_started"
	EventNodeStarted       ExecutionEventType = "node_started"
	EventNodeOutput        ExecutionEventType = "node_output"
	EventNodeCompleted     ExecutionEventType = "node_completed"
	EventNodeError         ExecutionEventType = "node_error"
	EventWorkflowCompleted ExecutionEventType = "workflow_completed"
	EventWorkflowError     ExecutionEventType = "workflow_error"
	EventNodeWaiting       ExecutionEventType = "node_waiting"
)

type ExecutionEvent struct {
	ID        string             `json:"id"`
	Type      ExecutionEventType `json:"type"`
	NodeID    *string            `json:"nodeId,omitempty"`
	NodeLabel *string            `json:"nodeLabel,omitempty"`
	NodeType  *NodeType          `json:"nodeType,omitempty"`
	Message   string             `json:"message"`
	Output    *string            `json:"output,omitempty"`
	Timestamp int64              `json:"timestamp"`
	RunID     string             `json:"runId,omitempty"`
}

type EmitFn func(ExecutionEvent)
