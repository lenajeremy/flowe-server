package executor

// NodeType mirrors the TypeScript NodeType union.
type NodeType string

const (
	NodeTypeTextInput        NodeType = "textInput"
	NodeTypeImageInput       NodeType = "imageInput"
	NodeTypeLLM              NodeType = "llm"
	NodeTypeBranch           NodeType = "branch"
	NodeTypeLoop             NodeType = "loop"
	NodeTypeTextOutput       NodeType = "textOutput"
	NodeTypeHTTPRequest      NodeType = "httpRequest"
	NodeTypeEmailSend        NodeType = "emailSend"
	NodeTypeHumanApproval    NodeType = "humanApproval"
	NodeTypeWebhookTrigger   NodeType = "webhookTrigger"
	NodeTypeScheduledTrigger NodeType = "scheduledTrigger"
	NodeTypeNotion           NodeType = "notion"
	NodeTypeLinear           NodeType = "linear"
	NodeTypeGithub           NodeType = "github"
	NodeTypeGitlab           NodeType = "gitlab"
	NodeTypeGmail            NodeType = "gmail"
	NodeTypeStripe           NodeType = "stripe"
	NodeTypeShopify          NodeType = "shopify"
	NodeTypeGoogleCalendar   NodeType = "googlecalendar"
	NodeTypeOutlook          NodeType = "outlook"
	NodeTypeSlack            NodeType = "slack"
	NodeTypeGoogleDrive      NodeType = "googledrive"
	NodeTypeGoogleDocs       NodeType = "googledocs"
	NodeTypeGoogleSheets     NodeType = "googlesheets"
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
	NotionQuery      string `json:"notionQuery,omitempty"`
	NotionProperties string `json:"notionProperties,omitempty"`

	// linear
	LinearTeamId      string `json:"linearTeamId,omitempty"`
	LinearIssueId     string `json:"linearIssueId,omitempty"`
	LinearTitle       string `json:"linearTitle,omitempty"`
	LinearDescription string `json:"linearDescription,omitempty"`
	LinearPriority    int    `json:"linearPriority,omitempty"`
	LinearCommentBody string `json:"linearCommentBody,omitempty"`
	LinearLimit       int    `json:"linearLimit,omitempty"`
	LinearStateId     string `json:"linearStateId,omitempty"`
	LinearAssigneeId  string `json:"linearAssigneeId,omitempty"`
	LinearQuery       string `json:"linearQuery,omitempty"`
	LinearProjectId   string `json:"linearProjectId,omitempty"`

	// github
	GithubRepo        string `json:"githubRepo,omitempty"`
	GithubTitle       string `json:"githubTitle,omitempty"`
	GithubBody        string `json:"githubBody,omitempty"`
	GithubIssueNumber string `json:"githubIssueNumber,omitempty"`
	GithubLabels      string `json:"githubLabels,omitempty"`
	GithubState       string `json:"githubState,omitempty"`
	GithubLimit       int    `json:"githubLimit,omitempty"`
	GithubPrNumber    string `json:"githubPrNumber,omitempty"`

	// gitlab
	GitlabProjectId   string `json:"gitlabProjectId,omitempty"`
	GitlabTitle       string `json:"gitlabTitle,omitempty"`
	GitlabDescription string `json:"gitlabDescription,omitempty"`
	GitlabIssueIid    string `json:"gitlabIssueIid,omitempty"`
	GitlabLabels      string `json:"gitlabLabels,omitempty"`
	GitlabState       string `json:"gitlabState,omitempty"`
	GitlabLimit       int    `json:"gitlabLimit,omitempty"`
	GitlabMrIid       string `json:"gitlabMrIid,omitempty"`

	// gmail
	GmailTo        string `json:"gmailTo,omitempty"`
	GmailCc        string `json:"gmailCc,omitempty"`
	GmailSubject   string `json:"gmailSubject,omitempty"`
	GmailBody      string `json:"gmailBody,omitempty"`
	GmailQuery     string `json:"gmailQuery,omitempty"`
	GmailMessageId string `json:"gmailMessageId,omitempty"`
	GmailLimit     int    `json:"gmailLimit,omitempty"`
	GmailThreadId  string `json:"gmailThreadId,omitempty"`
	GmailLabelId   string `json:"gmailLabelId,omitempty"`
	GmailLabelName string `json:"gmailLabelName,omitempty"`
	GmailDraftId   string `json:"gmailDraftId,omitempty"`

	// stripe
	StripeLimit         int    `json:"stripeLimit,omitempty"`
	StripeCustomerEmail string `json:"stripeCustomerEmail,omitempty"`
	StripePriceId       string `json:"stripePriceId,omitempty"`
	StripeQuantity      int    `json:"stripeQuantity,omitempty"`

	// shopify
	ShopifyOrderId     string `json:"shopifyOrderId,omitempty"`
	ShopifyLimit       int    `json:"shopifyLimit,omitempty"`
	ShopifyStatus      string `json:"shopifyStatus,omitempty"`
	ShopifyTitle       string `json:"shopifyTitle,omitempty"`
	ShopifyDescription string `json:"shopifyDescription,omitempty"`
	ShopifyPrice       string `json:"shopifyPrice,omitempty"`

	// googlecalendar
	GCalCalendarId  string `json:"gcalCalendarId,omitempty"`
	GCalEventId     string `json:"gcalEventId,omitempty"`
	GCalSummary     string `json:"gcalSummary,omitempty"`
	GCalDescription string `json:"gcalDescription,omitempty"`
	GCalStart       string `json:"gcalStart,omitempty"` // RFC3339, e.g. 2026-07-20T15:00:00Z
	GCalEnd         string `json:"gcalEnd,omitempty"`
	GCalAttendees   string `json:"gcalAttendees,omitempty"` // comma-separated emails
	GCalLimit       int    `json:"gcalLimit,omitempty"`
	GCalText        string `json:"gcalText,omitempty"`     // quick_add natural language
	GCalResponse    string `json:"gcalResponse,omitempty"` // accepted | declined | tentative

	// outlook
	OutlookTo           string `json:"outlookTo,omitempty"`
	OutlookCc           string `json:"outlookCc,omitempty"`
	OutlookSubject      string `json:"outlookSubject,omitempty"`
	OutlookBody         string `json:"outlookBody,omitempty"`
	OutlookQuery        string `json:"outlookQuery,omitempty"`
	OutlookMessageId    string `json:"outlookMessageId,omitempty"`
	OutlookLimit        int    `json:"outlookLimit,omitempty"`
	OutlookStart        string `json:"outlookStart,omitempty"`
	OutlookEnd          string `json:"outlookEnd,omitempty"`
	OutlookFolderId     string `json:"outlookFolderId,omitempty"` // move_message target
	OutlookEventId      string `json:"outlookEventId,omitempty"`  // update/delete/respond_to_event
	OutlookComment      string `json:"outlookComment,omitempty"`  // reply/forward/respond comment
	OutlookResponse     string `json:"outlookResponse,omitempty"` // accept | decline | tentativelyAccept
	OutlookContactName  string `json:"outlookContactName,omitempty"`
	OutlookContactEmail string `json:"outlookContactEmail,omitempty"`

	// slack
	SlackChannel     string `json:"slackChannel,omitempty"`
	SlackText        string `json:"slackText,omitempty"`
	SlackLimit       int    `json:"slackLimit,omitempty"`
	SlackSendAs      string `json:"slackSendAs,omitempty"`      // "bot" (default) | "user"
	SlackUserId      string `json:"slackUserId,omitempty"`      // DM recipient / invite targets (comma-sep ok)
	SlackBotName     string `json:"slackBotName,omitempty"`     // display-name override for bot sends (chat:write.customize)
	SlackThreadTs    string `json:"slackThreadTs,omitempty"`    // parent message ts (reply_in_thread)
	SlackMessageTs   string `json:"slackMessageTs,omitempty"`   // target message ts (update/delete/react/pin)
	SlackEmoji       string `json:"slackEmoji,omitempty"`       // reaction name, no colons
	SlackChannelName string `json:"slackChannelName,omitempty"` // create_channel
	SlackPrivate     string `json:"slackPrivate,omitempty"`     // "true" | "false" (create_channel)
	SlackTopic       string `json:"slackTopic,omitempty"`       // set_channel_topic
	SlackFileName    string `json:"slackFileName,omitempty"`    // upload_file
	SlackFileContent string `json:"slackFileContent,omitempty"` // upload_file (text)
	SlackEmail       string `json:"slackEmail,omitempty"`       // get_user_by_email
	SlackPostAt      string `json:"slackPostAt,omitempty"`      // schedule_message (RFC3339)

	// googledrive
	GDriveFileId   string `json:"gdriveFileId,omitempty"`
	GDriveName     string `json:"gdriveName,omitempty"`
	GDriveQuery    string `json:"gdriveQuery,omitempty"`
	GDriveParentId string `json:"gdriveParentId,omitempty"`
	GDriveLimit    int    `json:"gdriveLimit,omitempty"`
	GDriveContent  string `json:"gdriveContent,omitempty"`  // upload_file text body
	GDriveMimeType string `json:"gdriveMimeType,omitempty"` // upload_file
	GDriveEmail    string `json:"gdriveEmail,omitempty"`    // share_file (empty → anyone-with-link)
	GDriveRole     string `json:"gdriveRole,omitempty"`     // reader | commenter | writer

	// googledocs
	GDocsDocumentId   string `json:"gdocsDocumentId,omitempty"`
	GDocsTitle        string `json:"gdocsTitle,omitempty"`
	GDocsText         string `json:"gdocsText,omitempty"`
	GDocsFindText     string `json:"gdocsFindText,omitempty"`     // replace_text
	GDocsReplaceText  string `json:"gdocsReplaceText,omitempty"`  // replace_text
	GDocsTemplateId   string `json:"gdocsTemplateId,omitempty"`   // create_from_template source doc
	GDocsReplacements string `json:"gdocsReplacements,omitempty"` // JSON map {"{{name}}":"Jane"}

	// googlesheets
	GSheetsSpreadsheetId string `json:"gsheetsSpreadsheetId,omitempty"`
	GSheetsRange         string `json:"gsheetsRange,omitempty"`  // A1 notation, e.g. Sheet1!A1:C10
	GSheetsValues        string `json:"gsheetsValues,omitempty"` // comma-separated cells for one row
	GSheetsTitle         string `json:"gsheetsTitle,omitempty"`
	GSheetsSheetTitle    string `json:"gsheetsSheetTitle,omitempty"` // tab name (add/delete/delete_rows)
	GSheetsFind          string `json:"gsheetsFind,omitempty"`       // find_replace
	GSheetsReplace       string `json:"gsheetsReplace,omitempty"`    // find_replace
	GSheetsRows          string `json:"gsheetsRows,omitempty"`       // JSON array-of-arrays (append_rows)
	GSheetsStartRow      int    `json:"gsheetsStartRow,omitempty"`   // delete_rows (1-based, inclusive)
	GSheetsEndRow        int    `json:"gsheetsEndRow,omitempty"`     // delete_rows (inclusive)
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
