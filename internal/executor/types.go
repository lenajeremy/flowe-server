package executor

import "workflow-ai/server/internal/codingagent"

// NodeType mirrors the TypeScript NodeType union.
type NodeType string

const (
	NodeTypeTextInput           NodeType = "textInput"
	NodeTypeImageInput          NodeType = "imageInput"
	NodeTypeLLM                 NodeType = "llm"
	NodeTypeBranch              NodeType = "branch"
	NodeTypeLoop                NodeType = "loop"
	NodeTypeTextOutput          NodeType = "textOutput"
	NodeTypeHTTPRequest         NodeType = "httpRequest"
	NodeTypeEmailSend           NodeType = "emailSend"
	NodeTypeHumanApproval       NodeType = "humanApproval"
	NodeTypeWebhookTrigger      NodeType = "webhookTrigger"
	NodeTypeScheduledTrigger    NodeType = "scheduledTrigger"
	NodeTypeIntegrationTrigger  NodeType = "integrationTrigger"
	NodeTypeNotion              NodeType = "notion"
	NodeTypeLinear              NodeType = "linear"
	NodeTypeGithub              NodeType = "github"
	NodeTypeGitlab              NodeType = "gitlab"
	NodeTypeGmail               NodeType = "gmail"
	NodeTypeStripe              NodeType = "stripe"
	NodeTypeShopify             NodeType = "shopify"
	NodeTypeGoogleCalendar      NodeType = "googlecalendar"
	NodeTypeOutlook             NodeType = "outlook"
	NodeTypeSlack               NodeType = "slack"
	NodeTypeGoogleDrive         NodeType = "googledrive"
	NodeTypeGoogleDocs          NodeType = "googledocs"
	NodeTypeGoogleSheets        NodeType = "googlesheets"
	NodeTypeJira                NodeType = "jira"
	NodeTypeConfluence          NodeType = "confluence"
	NodeTypeBitbucket           NodeType = "bitbucket"
	NodeTypeGoogleMeet          NodeType = "googlemeet"
	NodeTypeGoogleSlides        NodeType = "googleslides"
	NodeTypeGoogleForms         NodeType = "googleforms"
	NodeTypeGoogleTasks         NodeType = "googletasks"
	NodeTypeGoogleChat          NodeType = "googlechat"
	NodeTypeGoogleKeep          NodeType = "googlekeep"
	NodeTypeGranola             NodeType = "granola"
	NodeTypeResend              NodeType = "resend"
	NodeTypeSendGrid            NodeType = "sendgrid"
	NodeTypeKit                 NodeType = "kit"
	NodeTypeAirtable            NodeType = "airtable"
	NodeTypeClickUp             NodeType = "clickup"
	NodeTypeMonday              NodeType = "monday"
	NodeTypeAsana               NodeType = "asana"
	NodeTypeTypeform            NodeType = "typeform"
	NodeTypeCalendly            NodeType = "calendly"
	NodeTypeDropbox             NodeType = "dropbox"
	NodeTypeNetlify             NodeType = "netlify"
	NodeTypeVercel              NodeType = "vercel"
	NodeTypeSupabase            NodeType = "supabase"
	NodeTypeGumroad             NodeType = "gumroad"
	NodeTypeGoogleSearchConsole NodeType = "googlesearchconsole"
	NodeTypeGoogleContacts      NodeType = "googlecontacts"
	NodeTypeHubspot             NodeType = "hubspot"
	NodeTypeFront               NodeType = "front"
	NodeTypeData                NodeType = "data"
	NodeTypeCodingAgent         NodeType = "codingAgent"
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

	// codingAgent — executes a durable coding task inside an isolated sandbox.
	// Credentials and provider sandbox IDs never live on the canvas.
	CodingAgentRuntime            string   `json:"codingAgentRuntime,omitempty"`
	CodingAgentTask               string   `json:"codingAgentTask,omitempty"`
	CodingAgentRepositoryProvider string   `json:"codingAgentRepositoryProvider,omitempty"`
	CodingAgentRepositoryID       string   `json:"codingAgentRepositoryId,omitempty"`
	CodingAgentRepository         string   `json:"codingAgentRepository,omitempty"`
	CodingAgentBranch             string   `json:"codingAgentBranch,omitempty"`
	CodingAgentWorkspaceMode      string   `json:"codingAgentWorkspaceMode,omitempty"`
	CodingAgentConversationKey    string   `json:"codingAgentConversationKey,omitempty"`
	CodingAgentModel              string   `json:"codingAgentModel,omitempty"`
	CodingAgentMaxDuration        int      `json:"codingAgentMaxDuration,omitempty"`
	CodingAgentAutoStopMinutes    int      `json:"codingAgentAutoStopMinutes,omitempty"`
	CodingAgentAutoDeleteMinutes  int      `json:"codingAgentAutoDeleteMinutes,omitempty"`
	CodingAgentAllowedDomains     []string `json:"codingAgentAllowedDomains,omitempty"`
	// CodingAgentNetworkAccess is "open" (default) or "allowlist". Real work
	// needs the internet — dependencies, package registries, documentation — so
	// an agent that cannot reach it fails at tasks it should manage. Naming any
	// allowed domain implies "allowlist" without having to say so twice.
	CodingAgentNetworkAccess string `json:"codingAgentNetworkAccess,omitempty"`
	CodingAgentAllowWrite    bool   `json:"codingAgentAllowWrite,omitempty"`
	// CodingAgentToolNodes lists the ids of other nodes on this canvas that
	// the agent may call while it works — how it opens a pull request without
	// ever holding a credential itself. Deny-by-default: an empty list is an
	// agent with no tools, which is the right default for something running
	// attacker-influenceable text next to a shell.
	CodingAgentToolNodes []string `json:"codingAgentToolNodes,omitempty"`
	// CodingAgentToolGrants is the integration-level operation/field authority
	// with an exact backing-node resource allowlist. codingAgentToolNodes is
	// retained only to safely migrate old workflows.
	CodingAgentToolGrants []codingagent.ToolGrant `json:"codingAgentToolGrants,omitempty"`

	// integrationTrigger — what this node is subscribed to. The authoritative
	// copy lives in the integration_triggers row (that is what the provider was
	// registered against); these are the canvas's view of it, so the node can
	// render "GitHub · Pull request opened" without a fetch and a manual run can
	// produce a realistic placeholder payload.
	TriggerProvider   string `json:"triggerProvider,omitempty"`
	TriggerEvent      string `json:"triggerEvent,omitempty"`
	TriggerResourceID string `json:"triggerResourceId,omitempty"`

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

	// scheduledTrigger has no node-data config: the schedule (interval or
	// calendar) lives in the ScheduledTrigger table, set via the node sidebar
	// and driven by the background scheduler.

	// data (persistence) — reads/writes a DataStore selected by id
	DataStoreId  string `json:"dataStoreId"`
	DataOp       string `json:"dataOp"`       // get|set|increment|delete|append|query|update|count|clear
	DataKey      string `json:"dataKey"`      // kv key
	DataValue    string `json:"dataValue"`    // kv value / text content (templates ok)
	DataAmount   string `json:"dataAmount"`   // increment amount (default 1)
	DataRecord   string `json:"dataRecord"`   // collection record, JSON object (templates ok)
	DataFilter   string `json:"dataFilter"`   // collection query filter, JSON object
	DataRecordId string `json:"dataRecordId"` // collection record id (update/delete)
	DataLimit    string `json:"dataLimit"`    // collection query limit

	// LLM structured output
	OutputSchema string `json:"outputSchema"` // JSON schema string

	// LLM web tools
	EnableWebSearch bool `json:"enableWebSearch,omitempty"` // gives the LLM web_search + read_url tools

	// notion / linear shared
	IntegrationToken string `json:"integrationToken,omitempty"`
	IntegrationOp    string `json:"integrationOp,omitempty"`

	// notion
	NotionDatabaseId   string `json:"notionDatabaseId,omitempty"`
	NotionPageId       string `json:"notionPageId,omitempty"`
	NotionTitle        string `json:"notionTitle,omitempty"`
	NotionContent      string `json:"notionContent,omitempty"`
	NotionFilter       string `json:"notionFilter,omitempty"`
	NotionQuery        string `json:"notionQuery,omitempty"`
	NotionProperties   string `json:"notionProperties,omitempty"`
	NotionParentPageId string `json:"notionParentPageId,omitempty"` // create_database / create_subpage parent
	NotionSchema       string `json:"notionSchema,omitempty"`       // create_database properties JSON

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
	LinearLabelId     string `json:"linearLabelId,omitempty"` // add_label

	// github
	GithubRepo        string `json:"githubRepo,omitempty"`
	GithubTitle       string `json:"githubTitle,omitempty"`
	GithubBody        string `json:"githubBody,omitempty"`
	GithubIssueNumber string `json:"githubIssueNumber,omitempty"`
	GithubLabels      string `json:"githubLabels,omitempty"`
	GithubState       string `json:"githubState,omitempty"`
	GithubLimit       int    `json:"githubLimit,omitempty"`
	GithubTreeLimit   int    `json:"githubTreeLimit,omitempty"` // list_repo_tree: default 1000, max 5000
	GithubPrNumber    string `json:"githubPrNumber,omitempty"`
	GithubBranch      string `json:"githubBranch,omitempty"`      // PR head / file commit branch / workflow ref
	GithubBase        string `json:"githubBase,omitempty"`        // PR base branch
	GithubMergeMethod string `json:"githubMergeMethod,omitempty"` // merge | squash | rebase
	GithubPath        string `json:"githubPath,omitempty"`        // file path, or optional list_repo_tree prefix
	GithubContent     string `json:"githubContent,omitempty"`     // file content
	// GithubFiles is commit_files' payload: a JSON array of
	// {path, content, deleted, executable}. A whole change set goes in one
	// commit, which is why it is a field rather than one node per file.
	GithubFiles      string `json:"githubFiles,omitempty"`
	GithubCommitMsg  string `json:"githubCommitMessage,omitempty"`
	GithubRef        string `json:"githubRef,omitempty"`        // branch/tag/sha for reads
	GithubTag        string `json:"githubTag,omitempty"`        // create_release tag
	GithubWorkflowId string `json:"githubWorkflowId,omitempty"` // workflow file name or id
	GithubQuery      string `json:"githubQuery,omitempty"`      // search_issues
	GithubSince      string `json:"githubSince,omitempty"`      // ISO 8601 time filter: commits since / issues updated after / runs created from
	GithubUntil      string `json:"githubUntil,omitempty"`      // ISO 8601 time filter: commits until / runs created to

	// gitlab
	GitlabProjectId    string `json:"gitlabProjectId,omitempty"`
	GitlabTitle        string `json:"gitlabTitle,omitempty"`
	GitlabDescription  string `json:"gitlabDescription,omitempty"`
	GitlabIssueIid     string `json:"gitlabIssueIid,omitempty"`
	GitlabLabels       string `json:"gitlabLabels,omitempty"`
	GitlabState        string `json:"gitlabState,omitempty"`
	GitlabLimit        int    `json:"gitlabLimit,omitempty"`
	GitlabMrIid        string `json:"gitlabMrIid,omitempty"`
	GitlabSourceBranch string `json:"gitlabSourceBranch,omitempty"` // MR source
	GitlabTargetBranch string `json:"gitlabTargetBranch,omitempty"` // MR target
	GitlabRef          string `json:"gitlabRef,omitempty"`          // branch/tag for commits/pipeline/file ops
	GitlabPath         string `json:"gitlabPath,omitempty"`         // file path
	GitlabContent      string `json:"gitlabContent,omitempty"`      // file content
	GitlabCommitMsg    string `json:"gitlabCommitMessage,omitempty"`
	GitlabStateEvent   string `json:"gitlabStateEvent,omitempty"` // close | reopen (update_issue)
	GitlabSince        string `json:"gitlabSince,omitempty"`      // ISO 8601: commits since / issues+MRs created after / pipelines updated after
	GitlabUntil        string `json:"gitlabUntil,omitempty"`      // ISO 8601: commits until / issues+MRs created before / pipelines updated before

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
	StripeLimit           int    `json:"stripeLimit,omitempty"`
	StripeCustomerEmail   string `json:"stripeCustomerEmail,omitempty"`
	StripePriceId         string `json:"stripePriceId,omitempty"`
	StripeQuantity        int    `json:"stripeQuantity,omitempty"`
	StripeCustomerId      string `json:"stripeCustomerId,omitempty"`
	StripeCustomerName    string `json:"stripeCustomerName,omitempty"`
	StripeSubscriptionId  string `json:"stripeSubscriptionId,omitempty"`
	StripeProductId       string `json:"stripeProductId,omitempty"`
	StripeProductName     string `json:"stripeProductName,omitempty"`
	StripeAmount          int    `json:"stripeAmount,omitempty"`   // cents
	StripeCurrency        string `json:"stripeCurrency,omitempty"` // usd, eur, …
	StripeInterval        string `json:"stripeInterval,omitempty"` // one-time | month | year
	StripeInvoiceId       string `json:"stripeInvoiceId,omitempty"`
	StripePaymentIntentId string `json:"stripePaymentIntentId,omitempty"`
	StripeRefundReason    string `json:"stripeRefundReason,omitempty"` // duplicate | fraudulent | requested_by_customer

	// shopify
	ShopifyOrderId         string `json:"shopifyOrderId,omitempty"`
	ShopifyLimit           int    `json:"shopifyLimit,omitempty"`
	ShopifyStatus          string `json:"shopifyStatus,omitempty"`
	ShopifyTitle           string `json:"shopifyTitle,omitempty"`
	ShopifyDescription     string `json:"shopifyDescription,omitempty"`
	ShopifyPrice           string `json:"shopifyPrice,omitempty"`
	ShopifyProductId       string `json:"shopifyProductId,omitempty"`
	ShopifyCustomerId      string `json:"shopifyCustomerId,omitempty"`
	ShopifyCustomerEmail   string `json:"shopifyCustomerEmail,omitempty"`
	ShopifyCustomerName    string `json:"shopifyCustomerName,omitempty"`
	ShopifyQuery           string `json:"shopifyQuery,omitempty"`
	ShopifyQuantity        int    `json:"shopifyQuantity,omitempty"`
	ShopifyInventoryItemId string `json:"shopifyInventoryItemId,omitempty"`
	ShopifyLocationId      string `json:"shopifyLocationId,omitempty"`
	ShopifyDelta           int    `json:"shopifyDelta,omitempty"` // inventory adjustment ±
	ShopifyDiscountCode    string `json:"shopifyDiscountCode,omitempty"`
	ShopifyDiscountType    string `json:"shopifyDiscountType,omitempty"`  // percentage | fixed_amount
	ShopifyDiscountValue   string `json:"shopifyDiscountValue,omitempty"` // "10" (% or amount)

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

	// jira
	JiraIssueKey    string `json:"jiraIssueKey,omitempty"`   // e.g. ENG-1234
	JiraProjectKey  string `json:"jiraProjectKey,omitempty"` // e.g. ENG
	JiraSummary     string `json:"jiraSummary,omitempty"`
	JiraDescription string `json:"jiraDescription,omitempty"` // plain text, converted to ADF
	JiraIssueType   string `json:"jiraIssueType,omitempty"`   // Task | Bug | Story | Epic | Sub-task
	JiraJql         string `json:"jiraJql,omitempty"`
	JiraLimit       int    `json:"jiraLimit,omitempty"`
	JiraFields      string `json:"jiraFields,omitempty"`     // comma-separated field list for searches
	JiraAssignee    string `json:"jiraAssignee,omitempty"`   // accountId, email, or "me"
	JiraPriority    string `json:"jiraPriority,omitempty"`   // Highest | High | Medium | Low | Lowest
	JiraLabels      string `json:"jiraLabels,omitempty"`     // comma-separated
	JiraParentKey   string `json:"jiraParentKey,omitempty"`  // sub-task / epic parent
	JiraDueDate     string `json:"jiraDueDate,omitempty"`    // YYYY-MM-DD
	JiraTransition  string `json:"jiraTransition,omitempty"` // target status or transition name
	JiraComment     string `json:"jiraComment,omitempty"`
	JiraTimeSpent   string `json:"jiraTimeSpent,omitempty"`   // add_worklog, e.g. "3h 30m"
	JiraStarted     string `json:"jiraStarted,omitempty"`     // add_worklog start (RFC3339)
	JiraLinkType    string `json:"jiraLinkType,omitempty"`    // Blocks | Relates | Duplicate | Cloners
	JiraLinkedIssue string `json:"jiraLinkedIssue,omitempty"` // link_issues target key
	JiraQuery       string `json:"jiraQuery,omitempty"`       // search_users
	JiraBoardId     string `json:"jiraBoardId,omitempty"`
	JiraSprintId    string `json:"jiraSprintId,omitempty"`
	JiraSprintName  string `json:"jiraSprintName,omitempty"`
	JiraStartDate   string `json:"jiraStartDate,omitempty"`  // create_sprint (RFC3339)
	JiraEndDate     string `json:"jiraEndDate,omitempty"`    // create_sprint (RFC3339)
	JiraAttachName  string `json:"jiraAttachName,omitempty"` // add_attachment file name
	JiraAttachBody  string `json:"jiraAttachBody,omitempty"` // add_attachment text content

	// confluence
	ConfluenceSpaceKey   string `json:"confluenceSpaceKey,omitempty"` // e.g. ENG
	ConfluencePageId     string `json:"confluencePageId,omitempty"`
	ConfluenceTitle      string `json:"confluenceTitle,omitempty"`
	ConfluenceBody       string `json:"confluenceBody,omitempty"` // storage XHTML, or plain text
	ConfluenceParentId   string `json:"confluenceParentId,omitempty"`
	ConfluenceCql        string `json:"confluenceCql,omitempty"` // search_pages
	ConfluenceLimit      int    `json:"confluenceLimit,omitempty"`
	ConfluenceComment    string `json:"confluenceComment,omitempty"`
	ConfluenceLabel      string `json:"confluenceLabel,omitempty"`  // comma-separated
	ConfluenceStatus     string `json:"confluenceStatus,omitempty"` // current | draft (create/update)
	ConfluenceAttachName string `json:"confluenceAttachName,omitempty"`
	ConfluenceAttachBody string `json:"confluenceAttachBody,omitempty"`

	// bitbucket
	BitbucketWorkspace     string `json:"bitbucketWorkspace,omitempty"` // slug; defaults to the connected one
	BitbucketRepo          string `json:"bitbucketRepo,omitempty"`      // repo slug
	BitbucketPrId          string `json:"bitbucketPrId,omitempty"`
	BitbucketTitle         string `json:"bitbucketTitle,omitempty"`
	BitbucketBody          string `json:"bitbucketBody,omitempty"`          // description / comment / issue body
	BitbucketSource        string `json:"bitbucketSource,omitempty"`        // PR source branch
	BitbucketDest          string `json:"bitbucketDest,omitempty"`          // PR destination branch
	BitbucketBranch        string `json:"bitbucketBranch,omitempty"`        // create/delete branch, commit target
	BitbucketRef           string `json:"bitbucketRef,omitempty"`           // branch/tag/commit to read from
	BitbucketPath          string `json:"bitbucketPath,omitempty"`          // file path
	BitbucketContent       string `json:"bitbucketContent,omitempty"`       // commit_file body
	BitbucketMessage       string `json:"bitbucketMessage,omitempty"`       // commit message
	BitbucketMergeStrategy string `json:"bitbucketMergeStrategy,omitempty"` // merge_commit | squash | fast_forward
	BitbucketState         string `json:"bitbucketState,omitempty"`         // PR/issue state filter
	BitbucketLimit         int    `json:"bitbucketLimit,omitempty"`
	BitbucketQuery         string `json:"bitbucketQuery,omitempty"`   // list filter (q=)
	BitbucketPrivate       string `json:"bitbucketPrivate,omitempty"` // create_repository: "true" | "false"
	BitbucketIssueId       string `json:"bitbucketIssueId,omitempty"`
	BitbucketKind          string `json:"bitbucketKind,omitempty"`     // issue kind: bug | enhancement | proposal | task
	BitbucketPriority      string `json:"bitbucketPriority,omitempty"` // trivial | minor | major | critical | blocker

	// googlemeet
	MeetSpace            string `json:"meetSpace,omitempty"`            // spaces/{id} or a meeting code
	MeetAccessType       string `json:"meetAccessType,omitempty"`       // OPEN | TRUSTED | RESTRICTED
	MeetModeration       string `json:"meetModeration,omitempty"`       // ON | OFF
	MeetConferenceRecord string `json:"meetConferenceRecord,omitempty"` // conferenceRecords/{id}
	MeetTranscript       string `json:"meetTranscript,omitempty"`       // .../transcripts/{id}
	MeetFilter           string `json:"meetFilter,omitempty"`           // list filter, e.g. space.name="spaces/abc"
	MeetLimit            int    `json:"meetLimit,omitempty"`

	// googleslides
	SlidesPresentationId string `json:"slidesPresentationId,omitempty"`
	SlidesTitle          string `json:"slidesTitle,omitempty"`
	SlidesSlideId        string `json:"slidesSlideId,omitempty"`  // page objectId
	SlidesLayout         string `json:"slidesLayout,omitempty"`   // TITLE_AND_BODY | TITLE_ONLY | BLANK | SECTION_HEADER
	SlidesHeading        string `json:"slidesHeading,omitempty"`  // title placeholder text
	SlidesBody           string `json:"slidesBody,omitempty"`     // body placeholder / text box text
	SlidesFind           string `json:"slidesFind,omitempty"`     // replace_all_text
	SlidesReplace        string `json:"slidesReplace,omitempty"`  // replace_all_text
	SlidesImageUrl       string `json:"slidesImageUrl,omitempty"` // add_image (must be publicly reachable)
	SlidesObjectId       string `json:"slidesObjectId,omitempty"` // delete_object target
	SlidesNotes          string `json:"slidesNotes,omitempty"`    // speaker notes
	SlidesTemplateId     string `json:"slidesTemplateId,omitempty"`
	SlidesReplacements   string `json:"slidesReplacements,omitempty"` // JSON map {"{{name}}":"Jane"}
	SlidesIndex          int    `json:"slidesIndex,omitempty"`        // insertion position

	// googleforms
	FormsFormId       string `json:"formsFormId,omitempty"`
	FormsTitle        string `json:"formsTitle,omitempty"`
	FormsDescription  string `json:"formsDescription,omitempty"`
	FormsQuestion     string `json:"formsQuestion,omitempty"`
	FormsQuestionType string `json:"formsQuestionType,omitempty"` // TEXT | PARAGRAPH | RADIO | CHECKBOX | DROPDOWN | SCALE | DATE | TIME
	FormsOptions      string `json:"formsOptions,omitempty"`      // comma-separated choices
	FormsRequired     string `json:"formsRequired,omitempty"`     // "true" | "false"
	FormsItemId       string `json:"formsItemId,omitempty"`
	FormsResponseId   string `json:"formsResponseId,omitempty"`
	FormsIndex        int    `json:"formsIndex,omitempty"`
	FormsIsQuiz       string `json:"formsIsQuiz,omitempty"`    // "true" | "false"
	FormsAccepting    string `json:"formsAccepting,omitempty"` // "true" | "false" (accepting responses)
	FormsLimit        int    `json:"formsLimit,omitempty"`

	// googletasks
	TasksListId          string `json:"tasksListId,omitempty"` // defaults to @default
	TasksTaskId          string `json:"tasksTaskId,omitempty"`
	TasksTitle           string `json:"tasksTitle,omitempty"`
	TasksNotes           string `json:"tasksNotes,omitempty"`
	TasksDue             string `json:"tasksDue,omitempty"`    // RFC3339; Tasks keeps the date and drops the time
	TasksStatus          string `json:"tasksStatus,omitempty"` // needsAction | completed
	TasksParent          string `json:"tasksParent,omitempty"` // subtask parent
	TasksPrevious        string `json:"tasksPrevious,omitempty"`
	TasksShowCompleted   string `json:"tasksShowCompleted,omitempty"` // "true" | "false"
	TasksDueMin          string `json:"tasksDueMin,omitempty"`
	TasksDueMax          string `json:"tasksDueMax,omitempty"`
	TasksDestinationList string `json:"tasksDestinationList,omitempty"`
	TasksLimit           int    `json:"tasksLimit,omitempty"`

	// googlechat
	ChatSpace       string `json:"chatSpace,omitempty"`     // spaces/{id}
	ChatMessageId   string `json:"chatMessageId,omitempty"` // spaces/{s}/messages/{m}
	ChatText        string `json:"chatText,omitempty"`
	ChatThread      string `json:"chatThread,omitempty"`      // thread name or thread key
	ChatDisplayName string `json:"chatDisplayName,omitempty"` // create/update space
	ChatSpaceType   string `json:"chatSpaceType,omitempty"`   // SPACE | GROUP_CHAT
	ChatMemberEmail string `json:"chatMemberEmail,omitempty"` // comma-separated for space setup
	ChatMembership  string `json:"chatMembership,omitempty"`  // spaces/{s}/members/{m}
	ChatEmoji       string `json:"chatEmoji,omitempty"`       // reaction, a literal emoji
	ChatFilter      string `json:"chatFilter,omitempty"`
	ChatLimit       int    `json:"chatLimit,omitempty"`

	// googlekeep
	KeepNoteName  string `json:"keepNoteName,omitempty"` // notes/{id}
	KeepTitle     string `json:"keepTitle,omitempty"`
	KeepText      string `json:"keepText,omitempty"`
	KeepListItems string `json:"keepListItems,omitempty"` // one checklist item per line
	KeepEmail     string `json:"keepEmail,omitempty"`     // comma-separated, for sharing
	KeepFilter    string `json:"keepFilter,omitempty"`
	KeepLimit     int    `json:"keepLimit,omitempty"`

	// granola
	GranolaNoteId       string `json:"granolaNoteId,omitempty"`
	GranolaCreatedAfter string `json:"granolaCreatedAfter,omitempty"` // RFC3339; scopes a digest to "since last run"
	GranolaCursor       string `json:"granolaCursor,omitempty"`
	GranolaLimit        int    `json:"granolaLimit,omitempty"`

	// resend
	ResendFrom         string `json:"resendFrom,omitempty"` // must be on a domain verified in Resend
	ResendTo           string `json:"resendTo,omitempty"`   // comma-separated, max 50
	ResendCc           string `json:"resendCc,omitempty"`
	ResendBcc          string `json:"resendBcc,omitempty"`
	ResendReplyTo      string `json:"resendReplyTo,omitempty"`
	ResendSubject      string `json:"resendSubject,omitempty"`
	ResendHtml         string `json:"resendHtml,omitempty"`
	ResendText         string `json:"resendText,omitempty"`
	ResendScheduledAt  string `json:"resendScheduledAt,omitempty"` // ISO 8601 or natural language
	ResendHeaders      string `json:"resendHeaders,omitempty"`     // JSON object
	ResendTags         string `json:"resendTags,omitempty"`        // JSON object, converted to name/value pairs
	ResendBatch        string `json:"resendBatch,omitempty"`       // JSON array of email objects
	ResendEmailId      string `json:"resendEmailId,omitempty"`
	ResendDomain       string `json:"resendDomain,omitempty"`
	ResendDomainId     string `json:"resendDomainId,omitempty"`
	ResendRegion       string `json:"resendRegion,omitempty"`
	ResendEmail        string `json:"resendEmail,omitempty"` // contact / suppression address
	ResendContactId    string `json:"resendContactId,omitempty"`
	ResendFirstName    string `json:"resendFirstName,omitempty"`
	ResendLastName     string `json:"resendLastName,omitempty"`
	ResendUnsubscribed string `json:"resendUnsubscribed,omitempty"` // "true" | "false"
	ResendProperties   string `json:"resendProperties,omitempty"`   // JSON object
	ResendSegmentId    string `json:"resendSegmentId,omitempty"`    // comma-separated when set on a contact
	ResendName         string `json:"resendName,omitempty"`         // segment / broadcast / template name
	ResendBroadcastId  string `json:"resendBroadcastId,omitempty"`
	ResendTemplateId   string `json:"resendTemplateId,omitempty"`
	ResendTemplateVars string `json:"resendTemplateVars,omitempty"` // JSON object
	ResendUrl          string `json:"resendUrl,omitempty"`          // webhook endpoint
	ResendEvents       string `json:"resendEvents,omitempty"`       // comma-separated webhook events
	ResendWebhookId    string `json:"resendWebhookId,omitempty"`
	ResendLimit        int    `json:"resendLimit,omitempty"`

	// sendgrid
	SendGridFrom         string `json:"sendgridFrom,omitempty"` // must be a verified sender
	SendGridTo           string `json:"sendgridTo,omitempty"`
	SendGridCc           string `json:"sendgridCc,omitempty"`
	SendGridBcc          string `json:"sendgridBcc,omitempty"`
	SendGridReplyTo      string `json:"sendgridReplyTo,omitempty"`
	SendGridSubject      string `json:"sendgridSubject,omitempty"`
	SendGridHtml         string `json:"sendgridHtml,omitempty"`
	SendGridText         string `json:"sendgridText,omitempty"`
	SendGridSendAt       string `json:"sendgridSendAt,omitempty"` // unix seconds for mail; "now" or ISO for single sends
	SendGridTemplateId   string `json:"sendgridTemplateId,omitempty"`
	SendGridTemplateData string `json:"sendgridTemplateData,omitempty"` // JSON object
	SendGridEmail        string `json:"sendgridEmail,omitempty"`        // contact / suppression address
	SendGridContactId    string `json:"sendgridContactId,omitempty"`
	SendGridFirstName    string `json:"sendgridFirstName,omitempty"`
	SendGridLastName     string `json:"sendgridLastName,omitempty"`
	SendGridCustomFields string `json:"sendgridCustomFields,omitempty"` // JSON keyed by field ID
	SendGridListId       string `json:"sendgridListId,omitempty"`       // comma-separated where several are allowed
	SendGridSegmentId    string `json:"sendgridSegmentId,omitempty"`
	SendGridSingleSendId string `json:"sendgridSingleSendId,omitempty"`
	SendGridJobId        string `json:"sendgridJobId,omitempty"` // from upsert_contact
	SendGridName         string `json:"sendgridName,omitempty"`
	SendGridQuery        string `json:"sendgridQuery,omitempty"`     // SGQL
	SendGridFieldType    string `json:"sendgridFieldType,omitempty"` // Text | Number | Date
	SendGridStartDate    string `json:"sendgridStartDate,omitempty"` // YYYY-MM-DD
	SendGridEndDate      string `json:"sendgridEndDate,omitempty"`
	SendGridAggregate    string `json:"sendgridAggregate,omitempty"` // day | week | month
	SendGridLimit        int    `json:"sendgridLimit,omitempty"`

	// kit (formerly ConvertKit)
	KitEmail        string `json:"kitEmail,omitempty"`
	KitFirstName    string `json:"kitFirstName,omitempty"`
	KitState        string `json:"kitState,omitempty"`  // active | inactive | bounced | cancelled
	KitFields       string `json:"kitFields,omitempty"` // JSON object of custom field values
	KitSubscriberId string `json:"kitSubscriberId,omitempty"`
	KitCreatedAfter string `json:"kitCreatedAfter,omitempty"` // RFC3339
	KitTagId        string `json:"kitTagId,omitempty"`        // comma-separated when filtering a broadcast
	KitFormId       string `json:"kitFormId,omitempty"`
	KitSequenceId   string `json:"kitSequenceId,omitempty"`
	KitBroadcastId  string `json:"kitBroadcastId,omitempty"`
	KitFieldId      string `json:"kitFieldId,omitempty"`
	KitPurchaseId   string `json:"kitPurchaseId,omitempty"`
	KitPurchase     string `json:"kitPurchase,omitempty"` // JSON purchase object
	KitWebhookId    string `json:"kitWebhookId,omitempty"`
	KitUrl          string `json:"kitUrl,omitempty"`   // webhook target
	KitEvent        string `json:"kitEvent,omitempty"` // webhook event name
	KitName         string `json:"kitName,omitempty"`  // tag / sequence / custom-field label
	KitSubject      string `json:"kitSubject,omitempty"`
	KitContent      string `json:"kitContent,omitempty"`
	KitDescription  string `json:"kitDescription,omitempty"`
	KitSendAt       string `json:"kitSendAt,omitempty"` // RFC3339; omit to leave a broadcast as a draft
	KitLimit        int    `json:"kitLimit,omitempty"`

	// airtable
	AirtableBaseId        string `json:"airtableBaseId,omitempty"`
	AirtableTable         string `json:"airtableTable,omitempty"`    // name or table id
	AirtableTableId       string `json:"airtableTableId,omitempty"`  // schema ops need the id, not the name
	AirtableRecordId      string `json:"airtableRecordId,omitempty"` // comma-separated for delete_records
	AirtableFields        string `json:"airtableFields,omitempty"`   // JSON object for a single record
	AirtableRecords       string `json:"airtableRecords,omitempty"`  // JSON array, max 10 per request
	AirtableTypecast      string `json:"airtableTypecast,omitempty"` // "false" to disable coercion
	AirtableFormula       string `json:"airtableFormula,omitempty"`  // filterByFormula
	AirtableView          string `json:"airtableView,omitempty"`
	AirtableFieldNames    string `json:"airtableFieldNames,omitempty"` // comma-separated columns to return
	AirtableSortField     string `json:"airtableSortField,omitempty"`
	AirtableSortDirection string `json:"airtableSortDirection,omitempty"` // asc | desc
	AirtableOffset        string `json:"airtableOffset,omitempty"`        // pagination
	AirtableMergeOn       string `json:"airtableMergeOn,omitempty"`       // upsert key field(s)
	AirtableComment       string `json:"airtableComment,omitempty"`
	AirtableCommentId     string `json:"airtableCommentId,omitempty"`
	AirtableName          string `json:"airtableName,omitempty"` // base / table / field name
	AirtableDescription   string `json:"airtableDescription,omitempty"`
	AirtableWorkspaceId   string `json:"airtableWorkspaceId,omitempty"`
	AirtableTables        string `json:"airtableTables,omitempty"`      // JSON array for create_base
	AirtableTableFields   string `json:"airtableTableFields,omitempty"` // JSON array for create_table
	AirtableFieldType     string `json:"airtableFieldType,omitempty"`
	AirtableFieldOptions  string `json:"airtableFieldOptions,omitempty"` // JSON object
	AirtableFieldId       string `json:"airtableFieldId,omitempty"`
	AirtableUrl           string `json:"airtableUrl,omitempty"` // webhook notification URL
	AirtableWebhookId     string `json:"airtableWebhookId,omitempty"`
	AirtableCursor        string `json:"airtableCursor,omitempty"`
	AirtableLimit         int    `json:"airtableLimit,omitempty"`

	// monday.com
	MondayBoardId      string `json:"mondayBoardId,omitempty"`
	MondayItemId       string `json:"mondayItemId,omitempty"`
	MondayGroupId      string `json:"mondayGroupId,omitempty"`
	MondayItemName     string `json:"mondayItemName,omitempty"`
	MondayColumnValues string `json:"mondayColumnValues,omitempty"` // JSON object keyed by column id
	MondayUpdateBody   string `json:"mondayUpdateBody,omitempty"`
	MondayCursor       string `json:"mondayCursor,omitempty"`
	MondayLimit        int    `json:"mondayLimit,omitempty"`

	// asana
	AsanaWorkspaceId  string `json:"asanaWorkspaceId,omitempty"`
	AsanaProjectId    string `json:"asanaProjectId,omitempty"`
	AsanaSectionId    string `json:"asanaSectionId,omitempty"`
	AsanaTaskId       string `json:"asanaTaskId,omitempty"`
	AsanaParentTaskId string `json:"asanaParentTaskId,omitempty"`
	AsanaName         string `json:"asanaName,omitempty"`
	AsanaNotes        string `json:"asanaNotes,omitempty"`
	AsanaAssignee     string `json:"asanaAssignee,omitempty"`
	AsanaDueOn        string `json:"asanaDueOn,omitempty"`
	AsanaCompleted    string `json:"asanaCompleted,omitempty"` // "true" | "false"
	AsanaComment      string `json:"asanaComment,omitempty"`
	AsanaLimit        int    `json:"asanaLimit,omitempty"`

	// clickup
	ClickUpWorkspaceId     string `json:"clickupWorkspaceId,omitempty"` // "team" in ClickUp's API
	ClickUpSpaceId         string `json:"clickupSpaceId,omitempty"`
	ClickUpFolderId        string `json:"clickupFolderId,omitempty"`
	ClickUpListId          string `json:"clickupListId,omitempty"` // comma-separated for search_tasks
	ClickUpTaskId          string `json:"clickupTaskId,omitempty"`
	ClickUpCustomTaskIds   string `json:"clickupCustomTaskIds,omitempty"` // "true" when the id is a custom one
	ClickUpName            string `json:"clickupName,omitempty"`
	ClickUpDescription     string `json:"clickupDescription,omitempty"`
	ClickUpStatus          string `json:"clickupStatus,omitempty"`
	ClickUpStatuses        string `json:"clickupStatuses,omitempty"`     // comma-separated filter
	ClickUpPriority        string `json:"clickupPriority,omitempty"`     // 1 urgent … 4 low
	ClickUpDueDate         string `json:"clickupDueDate,omitempty"`      // unix ms
	ClickUpTimeEstimate    string `json:"clickupTimeEstimate,omitempty"` // ms
	ClickUpAssignees       string `json:"clickupAssignees,omitempty"`    // comma-separated numeric user IDs
	ClickUpParent          string `json:"clickupParent,omitempty"`       // parent task, making a subtask
	ClickUpTagName         string `json:"clickupTagName,omitempty"`
	ClickUpSubtasks        string `json:"clickupSubtasks,omitempty"`      // "true" to include subtasks
	ClickUpIncludeClosed   string `json:"clickupIncludeClosed,omitempty"` // "true"
	ClickUpOrderBy         string `json:"clickupOrderBy,omitempty"`
	ClickUpComment         string `json:"clickupComment,omitempty"`
	ClickUpCommentId       string `json:"clickupCommentId,omitempty"`
	ClickUpChecklistId     string `json:"clickupChecklistId,omitempty"`
	ClickUpChecklistItemId string `json:"clickupChecklistItemId,omitempty"`
	ClickUpResolved        string `json:"clickupResolved,omitempty"` // "true" | "false"
	ClickUpFieldId         string `json:"clickupFieldId,omitempty"`
	ClickUpFieldValue      string `json:"clickupFieldValue,omitempty"`
	ClickUpDependsOn       string `json:"clickupDependsOn,omitempty"`
	ClickUpDependencyOf    string `json:"clickupDependencyOf,omitempty"`
	ClickUpLinksTo         string `json:"clickupLinksTo,omitempty"`
	ClickUpDuration        string `json:"clickupDuration,omitempty"`  // ms
	ClickUpStartDate       string `json:"clickupStartDate,omitempty"` // unix ms
	ClickUpEndDate         string `json:"clickupEndDate,omitempty"`
	ClickUpUrl             string `json:"clickupUrl,omitempty"`    // webhook endpoint
	ClickUpEvents          string `json:"clickupEvents,omitempty"` // comma-separated
	ClickUpWebhookId       string `json:"clickupWebhookId,omitempty"`
	ClickUpLimit           int    `json:"clickupLimit,omitempty"`

	// typeform
	TypeformFormId      string `json:"typeformFormId,omitempty"`
	TypeformTitle       string `json:"typeformTitle,omitempty"`
	TypeformDefinition  string `json:"typeformDefinition,omitempty"` // full JSON form definition
	TypeformWorkspaceId string `json:"typeformWorkspaceId,omitempty"`
	TypeformThemeId     string `json:"typeformThemeId,omitempty"`
	TypeformSearch      string `json:"typeformSearch,omitempty"`
	TypeformSince       string `json:"typeformSince,omitempty"` // RFC3339
	TypeformUntil       string `json:"typeformUntil,omitempty"`
	TypeformAfter       string `json:"typeformAfter,omitempty"`       // response token cursor
	TypeformCompleted   string `json:"typeformCompleted,omitempty"`   // "true" | "false"
	TypeformQuery       string `json:"typeformQuery,omitempty"`       // free-text search across answers
	TypeformResponseIds string `json:"typeformResponseIds,omitempty"` // comma-separated tokens
	TypeformUrl         string `json:"typeformUrl,omitempty"`         // webhook URL
	TypeformTag         string `json:"typeformTag,omitempty"`         // webhook tag; PUT is create-or-update
	TypeformSecret      string `json:"typeformSecret,omitempty"`      // webhook signing secret
	TypeformLimit       int    `json:"typeformLimit,omitempty"`

	// calendly
	CalendlyUser         string `json:"calendlyUser,omitempty"` // user URI; defaults to the connected account
	CalendlyOrganization string `json:"calendlyOrganization,omitempty"`
	CalendlyScope        string `json:"calendlyScope,omitempty"`     // user | organization
	CalendlyEventType    string `json:"calendlyEventType,omitempty"` // event type URI
	CalendlyEvent        string `json:"calendlyEvent,omitempty"`     // scheduled event URI
	CalendlyInvitee      string `json:"calendlyInvitee,omitempty"`
	CalendlyNoShow       string `json:"calendlyNoShow,omitempty"`
	CalendlyMembership   string `json:"calendlyMembership,omitempty"`
	CalendlyRoutingForm  string `json:"calendlyRoutingForm,omitempty"`
	CalendlyStatus       string `json:"calendlyStatus,omitempty"`    // active | canceled
	CalendlyStartTime    string `json:"calendlyStartTime,omitempty"` // RFC3339
	CalendlyEndTime      string `json:"calendlyEndTime,omitempty"`
	CalendlyEmail        string `json:"calendlyEmail,omitempty"`
	CalendlyReason       string `json:"calendlyReason,omitempty"` // cancellation reason
	CalendlyInviteeName  string `json:"calendlyInviteeName,omitempty"`
	CalendlyInviteeEmail string `json:"calendlyInviteeEmail,omitempty"`
	CalendlyTimezone     string `json:"calendlyTimezone,omitempty"` // IANA, e.g. Europe/Dublin
	CalendlyGuests       string `json:"calendlyGuests,omitempty"`   // comma-separated emails
	CalendlyAnswers      string `json:"calendlyAnswers,omitempty"`  // JSON array of question/answer objects
	CalendlyUrl          string `json:"calendlyUrl,omitempty"`      // webhook callback
	CalendlyEvents       string `json:"calendlyEvents,omitempty"`   // comma-separated
	CalendlyWebhookId    string `json:"calendlyWebhookId,omitempty"`
	CalendlyLimit        int    `json:"calendlyLimit,omitempty"`

	// dropbox
	DropboxPath        string `json:"dropboxPath,omitempty"`      // absolute from the account root; "" is the root
	DropboxToPath      string `json:"dropboxToPath,omitempty"`    // move/copy destination
	DropboxContent     string `json:"dropboxContent,omitempty"`   // upload body (text)
	DropboxOverwrite   string `json:"dropboxOverwrite,omitempty"` // "true" replaces instead of autorenaming
	DropboxRecursive   string `json:"dropboxRecursive,omitempty"` // "true" to walk subfolders
	DropboxCursor      string `json:"dropboxCursor,omitempty"`
	DropboxQuery       string `json:"dropboxQuery,omitempty"`
	DropboxRev         string `json:"dropboxRev,omitempty"`         // revision from list_revisions
	DropboxUrl         string `json:"dropboxUrl,omitempty"`         // shared link URL
	DropboxVisibility  string `json:"dropboxVisibility,omitempty"`  // public | team_only | password
	DropboxEmail       string `json:"dropboxEmail,omitempty"`       // comma-separated share targets
	DropboxAccessLevel string `json:"dropboxAccessLevel,omitempty"` // viewer | editor
	DropboxMessage     string `json:"dropboxMessage,omitempty"`     // note sent with a share
	DropboxTitle       string `json:"dropboxTitle,omitempty"`       // file request title
	DropboxLimit       int    `json:"dropboxLimit,omitempty"`

	// netlify
	NetlifySiteId              string `json:"netlifySiteId,omitempty"`
	NetlifyAccountId           string `json:"netlifyAccountId,omitempty"`   // team ID; env var writes need it
	NetlifyAccountSlug         string `json:"netlifyAccountSlug,omitempty"` // team URL name; NOT the ID
	NetlifyDeployId            string `json:"netlifyDeployId,omitempty"`
	NetlifyBuildId             string `json:"netlifyBuildId,omitempty"`
	NetlifyFormId              string `json:"netlifyFormId,omitempty"`
	NetlifySubmissionId        string `json:"netlifySubmissionId,omitempty"`
	NetlifyZoneId              string `json:"netlifyZoneId,omitempty"`
	NetlifyRecordId            string `json:"netlifyRecordId,omitempty"`
	NetlifyHookId              string `json:"netlifyHookId,omitempty"`
	NetlifyBuildHookId         string `json:"netlifyBuildHookId,omitempty"`
	NetlifyKeyId               string `json:"netlifyKeyId,omitempty"`
	NetlifyName                string `json:"netlifyName,omitempty"`
	NetlifyTitle               string `json:"netlifyTitle,omitempty"`
	NetlifyCustomDomain        string `json:"netlifyCustomDomain,omitempty"`
	NetlifySiteConfig          string `json:"netlifySiteConfig,omitempty"`   // raw JSON site body
	NetlifyRepo                string `json:"netlifyRepo,omitempty"`         // raw JSON repo settings
	NetlifyConfigureDns        string `json:"netlifyConfigureDns,omitempty"` // "true" | "false"
	NetlifyBranch              string `json:"netlifyBranch,omitempty"`
	NetlifyClearCache          string `json:"netlifyClearCache,omitempty"`  // "true" | "false"
	NetlifyDraft               string `json:"netlifyDraft,omitempty"`       // "true" | "false"
	NetlifyDeployFiles         string `json:"netlifyDeployFiles,omitempty"` // JSON path → SHA1 manifest
	NetlifyReason              string `json:"netlifyReason,omitempty"`      // required by disable_site
	NetlifyEnvKey              string `json:"netlifyEnvKey,omitempty"`
	NetlifyEnvValue            string `json:"netlifyEnvValue,omitempty"`
	NetlifyEnvValueId          string `json:"netlifyEnvValueId,omitempty"`
	NetlifyEnvContext          string `json:"netlifyEnvContext,omitempty"`          // all|dev|dev-server|branch-deploy|deploy-preview|production|branch
	NetlifyEnvContextParameter string `json:"netlifyEnvContextParameter,omitempty"` // branch name when context=branch
	NetlifyEnvScopes           string `json:"netlifyEnvScopes,omitempty"`           // CSV: builds,functions,runtime,post-processing
	NetlifyEnvIsSecret         string `json:"netlifyEnvIsSecret,omitempty"`         // "true" | "false"
	NetlifyEnvVarsJson         string `json:"netlifyEnvVarsJson,omitempty"`         // raw JSON array of variables
	NetlifyRecordType          string `json:"netlifyRecordType,omitempty"`
	NetlifyHostname            string `json:"netlifyHostname,omitempty"`
	NetlifyRecordValue         string `json:"netlifyRecordValue,omitempty"`
	NetlifyTtl                 string `json:"netlifyTtl,omitempty"`
	NetlifyPriority            string `json:"netlifyPriority,omitempty"`  // MX
	NetlifyWeight              string `json:"netlifyWeight,omitempty"`    // SRV
	NetlifyPort                string `json:"netlifyPort,omitempty"`      // SRV
	NetlifyFlag                string `json:"netlifyFlag,omitempty"`      // CAA
	NetlifyTag                 string `json:"netlifyTag,omitempty"`       // CAA
	NetlifyHookType            string `json:"netlifyHookType,omitempty"`  // defaults to "url"
	NetlifyHookEvent           string `json:"netlifyHookEvent,omitempty"` // deploy_created, submission_created, …
	NetlifyHookData            string `json:"netlifyHookData,omitempty"`  // raw JSON data object
	NetlifyUrl                 string `json:"netlifyUrl,omitempty"`       // convenience for a url hook
	NetlifyFilter              string `json:"netlifyFilter,omitempty"`    // all|owner|guest
	NetlifyQuery               string `json:"netlifyQuery,omitempty"`
	NetlifyLogType             string `json:"netlifyLogType,omitempty"`
	NetlifyPage                int    `json:"netlifyPage,omitempty"`    // 1-based
	NetlifyPerPage             int    `json:"netlifyPerPage,omitempty"` // capped at 100

	// vercel
	//
	// VercelTeamId (or VercelTeamSlug) is sent on every call. Without it the token
	// resolves to its owner's personal scope, and a team project 404s.
	VercelTeamId   string `json:"vercelTeamId,omitempty"`
	VercelTeamSlug string `json:"vercelTeamSlug,omitempty"`
	// Project accepts an id or a name; deployment accepts an id or a deployment URL.
	VercelProjectId     string `json:"vercelProjectId,omitempty"`
	VercelDeploymentId  string `json:"vercelDeploymentId,omitempty"`
	VercelName          string `json:"vercelName,omitempty"`   // project name; redeploy reads it back when unset
	VercelTarget        string `json:"vercelTarget,omitempty"` // production|preview|development
	VercelState         string `json:"vercelState,omitempty"`  // READY, ERROR, BUILDING, … (CSV allowed)
	VercelBranch        string `json:"vercelBranch,omitempty"`
	VercelSha           string `json:"vercelSha,omitempty"`
	VercelAlias         string `json:"vercelAlias,omitempty"`
	VercelDomain        string `json:"vercelDomain,omitempty"`
	VercelRedirect      string `json:"vercelRedirect,omitempty"`
	VercelGitBranch     string `json:"vercelGitBranch,omitempty"` // branch-scoped env var, or a branch-locked domain
	VercelEnvKey        string `json:"vercelEnvKey,omitempty"`
	VercelEnvValue      string `json:"vercelEnvValue,omitempty"`
	VercelEnvVarId      string `json:"vercelEnvVarId,omitempty"`      // from list_env_vars
	VercelEnvTarget     string `json:"vercelEnvTarget,omitempty"`     // CSV: production,preview,development
	VercelEnvType       string `json:"vercelEnvType,omitempty"`       // encrypted|plain|sensitive
	VercelProjectConfig string `json:"vercelProjectConfig,omitempty"` // raw JSON body for update_project
	VercelUrl           string `json:"vercelUrl,omitempty"`
	VercelSearch        string `json:"vercelSearch,omitempty"`
	VercelBuildId       string `json:"vercelBuildId,omitempty"` // narrows build logs to one build
	VercelLimit         int    `json:"vercelLimit,omitempty"`

	// supabase
	SupabaseAllowWrite          string `json:"supabaseAllowWrite,omitempty"` // "true" is required before run_sql will execute
	SupabaseAllowedCidrs        string `json:"supabaseAllowedCidrs,omitempty"`
	SupabaseAllowedCidrsV6      string `json:"supabaseAllowedCidrsV6,omitempty"`
	SupabaseApiKeyId            string `json:"supabaseApiKeyId,omitempty"`
	SupabaseApiKeyType          string `json:"supabaseApiKeyType,omitempty"`
	SupabaseAuthConfig          string `json:"supabaseAuthConfig,omitempty"`
	SupabaseBranchName          string `json:"supabaseBranchName,omitempty"`
	SupabaseBranchRef           string `json:"supabaseBranchRef,omitempty"`
	SupabaseConfirmDelete       string `json:"supabaseConfirmDelete,omitempty"`
	SupabaseCursor              string `json:"supabaseCursor,omitempty"`
	SupabaseDbPass              string `json:"supabaseDbPass,omitempty"` // database password for a new project
	SupabaseEntrypointPath      string `json:"supabaseEntrypointPath,omitempty"`
	SupabaseForce               string `json:"supabaseForce,omitempty"`
	SupabaseFunctionBody        string `json:"supabaseFunctionBody,omitempty"`
	SupabaseFunctionSlug        string `json:"supabaseFunctionSlug,omitempty"`
	SupabaseGitBranch           string `json:"supabaseGitBranch,omitempty"`
	SupabaseHostname            string `json:"supabaseHostname,omitempty"`
	SupabaseImportMapPath       string `json:"supabaseImportMapPath,omitempty"`
	SupabaseIncludedSchemas     string `json:"supabaseIncludedSchemas,omitempty"`
	SupabaseInstanceSize        string `json:"supabaseInstanceSize,omitempty"`
	SupabaseIpAddresses         string `json:"supabaseIpAddresses,omitempty"`
	SupabaseLimit               int    `json:"supabaseLimit,omitempty"`
	SupabaseMigrationName       string `json:"supabaseMigrationName,omitempty"`
	SupabaseMigrationVersion    string `json:"supabaseMigrationVersion,omitempty"`
	SupabaseName                string `json:"supabaseName,omitempty"`
	SupabaseOrgSlug             string `json:"supabaseOrgSlug,omitempty"`
	SupabasePersistent          string `json:"supabasePersistent,omitempty"`
	SupabasePostgrestMaxRows    int    `json:"supabasePostgrestMaxRows,omitempty"`
	SupabasePostgrestSchema     string `json:"supabasePostgrestSchema,omitempty"`
	SupabasePostgrestSearchPath string `json:"supabasePostgrestSearchPath,omitempty"`
	SupabaseProjectRef          string `json:"supabaseProjectRef,omitempty"` // the 20-character project ref, NOT the project UUID
	SupabaseRecoveryTimeUnix    string `json:"supabaseRecoveryTimeUnix,omitempty"`
	SupabaseRegion              string `json:"supabaseRegion,omitempty"`
	SupabaseRevealKeys          string `json:"supabaseRevealKeys,omitempty"`
	SupabaseRollbackSql         string `json:"supabaseRollbackSql,omitempty"`
	SupabaseSecretNames         string `json:"supabaseSecretNames,omitempty"`
	SupabaseSecrets             string `json:"supabaseSecrets,omitempty"` // JSON object or array of name/value pairs
	SupabaseSiteUrl             string `json:"supabaseSiteUrl,omitempty"`
	SupabaseSnippetId           string `json:"supabaseSnippetId,omitempty"`
	SupabaseSortBy              string `json:"supabaseSortBy,omitempty"`
	SupabaseSortOrder           string `json:"supabaseSortOrder,omitempty"`
	SupabaseSql                 string `json:"supabaseSql,omitempty"`       // raw SQL; prefer parameters over string interpolation
	SupabaseSqlParams           string `json:"supabaseSqlParams,omitempty"` // JSON array bound to $1, $2 … in the statement
	SupabaseUriAllowList        string `json:"supabaseUriAllowList,omitempty"`
	SupabaseVerifyJwt           string `json:"supabaseVerifyJwt,omitempty"`
	SupabaseWithData            string `json:"supabaseWithData,omitempty"`

	// gumroad
	GumroadAfter           string `json:"gumroadAfter,omitempty"`     // YYYY-MM-DD, exclusive
	GumroadAmount          string `json:"gumroadAmount,omitempty"`    // refund amount in cents; omit to refund in full
	GumroadAmountOff       string `json:"gumroadAmountOff,omitempty"` // cents, or a percentage when the offer type is percent
	GumroadBefore          string `json:"gumroadBefore,omitempty"`    // YYYY-MM-DD, exclusive
	GumroadCategoryId      string `json:"gumroadCategoryId,omitempty"`
	GumroadCode            string `json:"gumroadCode,omitempty"`
	GumroadCustomPermalink string `json:"gumroadCustomPermalink,omitempty"`
	GumroadDescription     string `json:"gumroadDescription,omitempty"`
	GumroadEmail           string `json:"gumroadEmail,omitempty"`
	GumroadIncrementUses   string `json:"gumroadIncrementUses,omitempty"` // "true" counts this check against the licence uses
	GumroadLicenseKey      string `json:"gumroadLicenseKey,omitempty"`
	GumroadMaxPurchases    string `json:"gumroadMaxPurchases,omitempty"`
	GumroadName            string `json:"gumroadName,omitempty"`
	GumroadOfferCodeId     string `json:"gumroadOfferCodeId,omitempty"`
	GumroadOfferType       string `json:"gumroadOfferType,omitempty"`       // cents | percent
	GumroadPageKey         string `json:"gumroadPageKey,omitempty"`         // opaque paging key from a previous list_sales
	GumroadPrice           string `json:"gumroadPrice,omitempty"`           // CENTS — 1000 is $10.00
	GumroadPriceDifference string `json:"gumroadPriceDifference,omitempty"` // variant surcharge, in cents
	GumroadProductId       string `json:"gumroadProductId,omitempty"`
	GumroadRequired        string `json:"gumroadRequired,omitempty"`
	GumroadResourceName    string `json:"gumroadResourceName,omitempty"` // sale | refund | dispute | cancellation | subscription_updated
	GumroadSaleId          string `json:"gumroadSaleId,omitempty"`
	GumroadSubscriberId    string `json:"gumroadSubscriberId,omitempty"`
	GumroadTitle           string `json:"gumroadTitle,omitempty"`
	GumroadTrackingUrl     string `json:"gumroadTrackingUrl,omitempty"`
	GumroadUrl             string `json:"gumroadUrl,omitempty"`
	GumroadWebhookId       string `json:"gumroadWebhookId,omitempty"`

	// googlesearchconsole
	GscSiteUrl          string `json:"gscSiteUrl,omitempty"`   // https://example.com/ or sc-domain:example.com
	GscFeedPath         string `json:"gscFeedPath,omitempty"`  // full sitemap URL
	GscStartDate        string `json:"gscStartDate,omitempty"` // YYYY-MM-DD, Pacific time
	GscEndDate          string `json:"gscEndDate,omitempty"`
	GscDimensions       string `json:"gscDimensions,omitempty"`       // query, page, country, device, date
	GscSearchType       string `json:"gscSearchType,omitempty"`       // web | image | video | news | discover
	GscDataState        string `json:"gscDataState,omitempty"`        // final (default) | all
	GscFilterExpression string `json:"gscFilterExpression,omitempty"` // one "dimension operator value" per line
	GscRowLimit         int    `json:"gscRowLimit,omitempty"`
	GscStartRow         int    `json:"gscStartRow,omitempty"`
	GscInspectionUrl    string `json:"gscInspectionUrl,omitempty"`
	GscLanguageCode     string `json:"gscLanguageCode,omitempty"`

	// googlecontacts
	ContactsResourceName  string `json:"contactsResourceName,omitempty"` // people/c123; comma-separated for batch delete
	ContactsFields        string `json:"contactsFields,omitempty"`       // personFields mask; a sensible default applies
	ContactsQuery         string `json:"contactsQuery,omitempty"`
	ContactsPageToken     string `json:"contactsPageToken,omitempty"`
	ContactsSortOrder     string `json:"contactsSortOrder,omitempty"`
	ContactsGivenName     string `json:"contactsGivenName,omitempty"`
	ContactsFamilyName    string `json:"contactsFamilyName,omitempty"`
	ContactsEmail         string `json:"contactsEmail,omitempty"` // comma-separated
	ContactsPhone         string `json:"contactsPhone,omitempty"` // comma-separated
	ContactsOrganization  string `json:"contactsOrganization,omitempty"`
	ContactsJobTitle      string `json:"contactsJobTitle,omitempty"`
	ContactsAddress       string `json:"contactsAddress,omitempty"`
	ContactsNotes         string `json:"contactsNotes,omitempty"`
	ContactsRawPerson     string `json:"contactsRawPerson,omitempty"` // extra People-API fields as JSON
	ContactsGroupId       string `json:"contactsGroupId,omitempty"`
	ContactsGroupName     string `json:"contactsGroupName,omitempty"`
	ContactsAddMembers    string `json:"contactsAddMembers,omitempty"` // comma-separated resource names
	ContactsRemoveMembers string `json:"contactsRemoveMembers,omitempty"`
	ContactsLimit         int    `json:"contactsLimit,omitempty"`

	// hubspot
	HubspotAfter          string `json:"hubspotAfter,omitempty"`
	HubspotArchived       string `json:"hubspotArchived,omitempty"`
	HubspotAssociations   string `json:"hubspotAssociations,omitempty"` // JSON array of associations to create alongside the record
	HubspotBatchInputs    string `json:"hubspotBatchInputs,omitempty"`  // JSON array, max 100 per request
	HubspotFieldType      string `json:"hubspotFieldType,omitempty"`
	HubspotFilters        string `json:"hubspotFilters,omitempty"` // JSON array of filter groups for search_objects
	HubspotGroupName      string `json:"hubspotGroupName,omitempty"`
	HubspotIdProperty     string `json:"hubspotIdProperty,omitempty"` // look a record up by a unique property such as email instead of its id
	HubspotLabel          string `json:"hubspotLabel,omitempty"`
	HubspotLimit          int    `json:"hubspotLimit,omitempty"`
	HubspotListId         string `json:"hubspotListId,omitempty"`
	HubspotObjectId       string `json:"hubspotObjectId,omitempty"`
	HubspotObjectType     string `json:"hubspotObjectType,omitempty"` // contacts | companies | deals | tickets | notes | tasks | calls | emails | meetings, or a custom type id
	HubspotProperties     string `json:"hubspotProperties,omitempty"` // comma-separated property names to return; v3 omits anything not asked for
	HubspotPropertyName   string `json:"hubspotPropertyName,omitempty"`
	HubspotPropertyType   string `json:"hubspotPropertyType,omitempty"`
	HubspotPropertyValues string `json:"hubspotPropertyValues,omitempty"` // JSON object keyed by HubSpot's internal names, e.g. firstname not First Name
	HubspotQuery          string `json:"hubspotQuery,omitempty"`
	HubspotSortDirection  string `json:"hubspotSortDirection,omitempty"`
	HubspotSortProperty   string `json:"hubspotSortProperty,omitempty"`
	HubspotToObjectId     string `json:"hubspotToObjectId,omitempty"`
	HubspotToObjectType   string `json:"hubspotToObjectType,omitempty"`

	// front
	FrontAssigneeId     string `json:"frontAssigneeId,omitempty"`
	FrontAuthorId       string `json:"frontAuthorId,omitempty"` // teammate id the message or comment is sent as
	FrontBcc            string `json:"frontBcc,omitempty"`
	FrontBody           string `json:"frontBody,omitempty"`
	FrontCc             string `json:"frontCc,omitempty"`
	FrontChannelId      string `json:"frontChannelId,omitempty"` // cha_… — required to start a new conversation
	FrontContactId      string `json:"frontContactId,omitempty"`
	FrontConversationId string `json:"frontConversationId,omitempty"` // cnv_…
	FrontDescription    string `json:"frontDescription,omitempty"`
	FrontHandle         string `json:"frontHandle,omitempty"`       // an address on a channel, e.g. an email address
	FrontHandleSource   string `json:"frontHandleSource,omitempty"` // email | phone | twitter | intercom | custom
	FrontInboxId        string `json:"frontInboxId,omitempty"`
	FrontLimit          int    `json:"frontLimit,omitempty"`
	FrontLinkId         string `json:"frontLinkId,omitempty"`
	FrontName           string `json:"frontName,omitempty"`
	FrontPageToken      string `json:"frontPageToken,omitempty"`
	FrontQuery          string `json:"frontQuery,omitempty"`  // search terms, or event types for list_events
	FrontStatus         string `json:"frontStatus,omitempty"` // archived | open | deleted | spam
	FrontSubject        string `json:"frontSubject,omitempty"`
	FrontTagId          string `json:"frontTagId,omitempty"` // comma-separated tag ids
	FrontTeammateId     string `json:"frontTeammateId,omitempty"`
	FrontTo             string `json:"frontTo,omitempty"`
	FrontUrl            string `json:"frontUrl,omitempty"`
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
	Workflow       WorkflowAST       `json:"workflow"`
	WorkflowID     string            `json:"workflowId,omitempty"`
	OnlyNodeID     string            `json:"onlyNodeId,omitempty"`
	InitialOutputs map[string]string `json:"initialOutputs,omitempty"`
}

// RunOptions narrows a normal manual run without introducing a second
// execution path. OnlyNodeID executes exactly one node; InitialOutputs supplies
// cached values from its upstream ancestors for template/input resolution.
type RunOptions struct {
	OnlyNodeID string
	// EntryNodeID names where the run begins. A trigger sets it so its own
	// graph runs, rather than whichever graph happens to be largest.
	EntryNodeID    string
	InitialOutputs map[string]string
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
	// EventNodeProgress reports non-terminal activity inside a long-running
	// node. Unlike EventNodeWaiting, it never pauses the workflow or creates a
	// human-approval decision.
	EventNodeProgress ExecutionEventType = "node_progress"
	// EventIterationStarted and EventIterationCompleted bracket one pass of a
	// loop body. They are the group headers the log collapses under, so the UI
	// never has to infer where a pass began from the body events themselves.
	EventIterationStarted   ExecutionEventType = "iteration_started"
	EventIterationCompleted ExecutionEventType = "iteration_completed"
	// EventEdgeTaken records that an edge enabled its target. Which path a run
	// took through a branching graph is otherwise unrecoverable: the executor
	// simply skips nodes it never enabled, leaving no trace of the choice.
	EventEdgeTaken ExecutionEventType = "edge_taken"
	// EventNodeSkipped names a node that never ran, and why.
	EventNodeSkipped ExecutionEventType = "node_skipped"
	// EventLogTruncated is emitted once, when a run produces more events than
	// the ceiling allows.
	EventLogTruncated ExecutionEventType = "log_truncated"
)

// SkipReason distinguishes the two ways a node can fail to run. They look
// identical in the event stream — the node is simply absent — but they mean
// opposite things to someone reading the log, so only the executor can tell
// them apart and it has to say which.
type SkipReason string

const (
	// SkipBranchNotTaken: an upstream branch or approval chose another edge.
	// The workflow behaved correctly; this path was not the one.
	SkipBranchNotTaken SkipReason = "branch_not_taken"
	// SkipNotReached: the run ended before this node's turn came up, because
	// an earlier node errored or the run was cancelled.
	SkipNotReached SkipReason = "not_reached"
)

// IterationRef identifies which pass of a loop an event belongs to.
//
// Iterations used to be marked by prefixing the message with "[3/10] ", so the
// only way to group a pass was to parse that text back out. This carries the
// same information structurally.
//
// There is deliberately no nesting. A loop node sitting inside a loop body is
// not iterated — executeNode returns its upstream output verbatim — so a single
// frame always describes an event's position.
type IterationRef struct {
	LoopNodeID string `json:"loopNodeId"`
	// Index is 0-based; Total is the item count at the time the loop started.
	Index int `json:"index"`
	Total int `json:"total"`
	// ItemPreview is a short rendering of the item this pass ran on, so a
	// failed pass can be identified without expanding it.
	ItemPreview string `json:"itemPreview,omitempty"`
}

type ExecutionEvent struct {
	ID        string             `json:"id"`
	Type      ExecutionEventType `json:"type"`
	NodeID    *string            `json:"nodeId,omitempty"`
	NodeLabel *string            `json:"nodeLabel,omitempty"`
	NodeType  *NodeType          `json:"nodeType,omitempty"`
	Message   string             `json:"message"`
	Output    *string            `json:"output,omitempty"`
	Payload   map[string]any     `json:"payload,omitempty"`
	Timestamp int64              `json:"timestamp"`
	RunID     string             `json:"runId,omitempty"`

	// Iteration is set on every event emitted inside a loop body, and on the
	// iteration_started/completed pair that brackets it.
	Iteration *IterationRef `json:"iteration,omitempty"`
	// Status is "ok" or "error" on iteration_completed.
	Status string `json:"status,omitempty"`
	// EdgeID and SourceHandle are set on edge_taken. SourceHandle is the branch
	// output that selected the edge, and is empty for unconditional edges.
	EdgeID       string `json:"edgeId,omitempty"`
	SourceHandle string `json:"sourceHandle,omitempty"`
	// SkipReason is set on node_skipped.
	SkipReason SkipReason `json:"skipReason,omitempty"`
	// OutputTruncated marks an Output cut down to the per-event cap. Without
	// it a clipped payload reads as though the node genuinely returned less.
	OutputTruncated bool `json:"outputTruncated,omitempty"`
}

type EmitFn func(ExecutionEvent)
