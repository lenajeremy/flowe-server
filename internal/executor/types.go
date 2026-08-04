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
	NodeTypeJira             NodeType = "jira"
	NodeTypeConfluence       NodeType = "confluence"
	NodeTypeBitbucket        NodeType = "bitbucket"
	NodeTypeGoogleMeet       NodeType = "googlemeet"
	NodeTypeGoogleSlides     NodeType = "googleslides"
	NodeTypeGoogleForms      NodeType = "googleforms"
	NodeTypeGoogleTasks      NodeType = "googletasks"
	NodeTypeGoogleChat       NodeType = "googlechat"
	NodeTypeGoogleKeep       NodeType = "googlekeep"
	NodeTypeGranola          NodeType = "granola"
	NodeTypeResend           NodeType = "resend"
	NodeTypeSendGrid         NodeType = "sendgrid"
	NodeTypeData             NodeType = "data"
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
	GithubPrNumber    string `json:"githubPrNumber,omitempty"`
	GithubBranch      string `json:"githubBranch,omitempty"`      // PR head / file commit branch / workflow ref
	GithubBase        string `json:"githubBase,omitempty"`        // PR base branch
	GithubMergeMethod string `json:"githubMergeMethod,omitempty"` // merge | squash | rebase
	GithubPath        string `json:"githubPath,omitempty"`        // file path (get_file / create_or_update_file)
	GithubContent     string `json:"githubContent,omitempty"`     // file content
	GithubCommitMsg   string `json:"githubCommitMessage,omitempty"`
	GithubRef         string `json:"githubRef,omitempty"`        // branch/tag/sha for reads
	GithubTag         string `json:"githubTag,omitempty"`        // create_release tag
	GithubWorkflowId  string `json:"githubWorkflowId,omitempty"` // workflow file name or id
	GithubQuery       string `json:"githubQuery,omitempty"`      // search_issues
	GithubSince       string `json:"githubSince,omitempty"`      // ISO 8601 time filter: commits since / issues updated after / runs created from
	GithubUntil       string `json:"githubUntil,omitempty"`      // ISO 8601 time filter: commits until / runs created to

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
