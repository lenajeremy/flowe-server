package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// redirectAtlassianTo points the gateway at a stub server for one test.
func redirectAtlassianTo(url string) func() {
	prev := atlassianGateway
	atlassianGateway = url
	return func() { atlassianGateway = prev }
}

// Jira and Confluence reject a plain string where they want a document, so the
// conversion is worth pinning down.
func TestADFShape(t *testing.T) {
	doc := adf("first line\n\nsecond line")
	if doc["type"] != "doc" || doc["version"] != 1 {
		t.Fatalf("expected a versioned doc envelope, got %v", doc)
	}
	content, ok := doc["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("expected the blank line to be dropped, leaving 2 paragraphs, got %v", doc["content"])
	}
	raw, _ := json.Marshal(doc)
	if !strings.Contains(string(raw), `"text":"second line"`) {
		t.Errorf("second paragraph lost its text: %s", raw)
	}
}

func TestADFNeverEmitsAnEmptyDoc(t *testing.T) {
	// An empty doc with no content array is invalid ADF; a single blank
	// paragraph is the minimum Jira accepts.
	content, ok := adf("   ")["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("expected one placeholder paragraph, got %v", content)
	}
}

func TestStorageBodyEscapesTextButPassesMarkupThrough(t *testing.T) {
	plain := storageBody("Tom & Jerry")
	if got := plain["value"]; got != "<p>Tom &amp; Jerry</p>" {
		t.Errorf("ampersand not escaped: %q", got)
	}
	if plain["representation"] != "storage" {
		t.Errorf("wrong representation: %v", plain["representation"])
	}

	markup := storageBody(`<h1>Report</h1><p>body</p>`)
	if got := markup["value"]; got != `<h1>Report</h1><p>body</p>` {
		t.Errorf("existing markup should pass through untouched, got %q", got)
	}
}

func TestJiraTimestamp(t *testing.T) {
	if got := jiraTimestamp("2026-08-04T09:00:00Z"); got != "2026-08-04T09:00:00.000+0000" {
		t.Errorf("worklog start not converted to Jira's offset format: %q", got)
	}
	// Already-offset input is Jira's own format and must not be mangled.
	in := "2026-08-04T09:00:00.000+0100"
	if got := jiraTimestamp(in); got != in {
		t.Errorf("offset timestamp altered: %q", got)
	}
}

// atlassianError has to cope with two different shapes for the "errors" key.
func TestAtlassianErrorUnpacksBothEnvelopes(t *testing.T) {
	jira := atlassianError("Jira", 400,
		[]byte(`{"errorMessages":["Issue does not exist"],"errors":{"summary":"is required"}}`))
	if !strings.Contains(jira.Error(), "Issue does not exist") {
		t.Errorf("lost errorMessages: %v", jira)
	}
	if !strings.Contains(jira.Error(), "summary: is required") {
		t.Errorf("lost per-field errors: %v", jira)
	}

	conf := atlassianError("Confluence", 404,
		[]byte(`{"errors":[{"status":404,"title":"Not Found","detail":"No page with id 123"}]}`))
	if !strings.Contains(conf.Error(), "No page with id 123") {
		t.Errorf("lost Confluence error detail: %v", conf)
	}
}

func TestAtlassianErrorHintsAtPermissionsOn403(t *testing.T) {
	err := atlassianError("Jira", 403, []byte(`{"errorMessages":["Forbidden"]}`))
	if !strings.Contains(err.Error(), "permission") {
		t.Errorf("a 403 should hint at permissions or reauthorization: %v", err)
	}
}

func TestConfluenceSpaceIDPassesNumericKeysThrough(t *testing.T) {
	// A numeric input is already a space id, so it must not cost a lookup.
	id, err := confluenceSpaceID(context.Background(), "tok", "cloud", "98765")
	if err != nil || id != "98765" {
		t.Fatalf("expected the numeric id to pass through, got %q / %v", id, err)
	}
}

// transition_issue resolves a status name against what Jira currently offers.
// The failure path matters most: it should name the alternatives.
func TestJiraTransitionResolvesNameAndReportsAlternatives(t *testing.T) {
	var posted map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"transitions":[
				{"id":"11","name":"Start","to":{"name":"In Progress"}},
				{"id":"31","name":"Finish","to":{"name":"Done"}}]}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&posted)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	restore := redirectAtlassianTo(srv.URL)
	defer restore()

	d := FlowNodeData{IntegrationOp: "transition_issue", JiraIssueKey: "ENG-1", JiraTransition: "done"}
	out, err := runJira(context.Background(), "tok", "cloud", d, map[string]string{})
	if err != nil {
		t.Fatalf("case-insensitive status match failed: %v", err)
	}
	if !strings.Contains(out, "ENG-1") {
		t.Errorf("result should name the issue: %s", out)
	}
	tr, _ := posted["transition"].(map[string]any)
	if tr == nil || tr["id"] != "31" {
		t.Errorf("resolved the wrong transition id: %v", posted)
	}

	d.JiraTransition = "Shipped"
	_, err = runJira(context.Background(), "tok", "cloud", d, map[string]string{})
	if err == nil {
		t.Fatal("expected an error for a status the issue cannot reach")
	}
	if !strings.Contains(err.Error(), "In Progress") || !strings.Contains(err.Error(), "Done") {
		t.Errorf("error should list the reachable statuses: %v", err)
	}
}

func TestJiraRequiresACloudID(t *testing.T) {
	_, err := runJira(context.Background(), "tok", "", FlowNodeData{IntegrationOp: "get_issue"}, nil)
	if err == nil || !strings.Contains(err.Error(), "reconnect") {
		t.Errorf("a missing site should tell the user to reconnect, got %v", err)
	}
}

func TestJiraLabelsDropSpaces(t *testing.T) {
	sub := func(s string) string { return s }
	d := FlowNodeData{JiraLabels: "needs triage, backend ,"}
	fields, err := jiraIssueFields(context.Background(), "tok", "cloud", d, sub, false)
	if err != nil {
		t.Fatal(err)
	}
	labels, _ := fields["labels"].([]string)
	if len(labels) != 2 || labels[0] != "needs-triage" || labels[1] != "backend" {
		t.Errorf("Jira rejects labels with spaces; got %v", labels)
	}
}

// An update must not carry project/issuetype, or Jira treats it as a create.
func TestJiraUpdateOmitsCreateOnlyFields(t *testing.T) {
	sub := func(s string) string { return s }
	fields, err := jiraIssueFields(context.Background(), "tok", "cloud",
		FlowNodeData{JiraSummary: "New title", JiraProjectKey: "ENG"}, sub, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["project"]; ok {
		t.Error("update sent a project field")
	}
	if _, ok := fields["issuetype"]; ok {
		t.Error("update sent an issuetype field")
	}
	if fields["summary"] != "New title" {
		t.Errorf("summary missing: %v", fields)
	}
}

func TestBitbucketPathEscapingKeepsSlashes(t *testing.T) {
	if got := pathEscapeSegments("docs/my notes.md"); got != "docs/my%20notes.md" {
		t.Errorf("path segments escaped wrongly: %q", got)
	}
}

func TestBitbucketRepoScopedOpsNeedARepo(t *testing.T) {
	_, err := runBitbucket(context.Background(), "tok", "acme",
		FlowNodeData{IntegrationOp: "list_branches"}, nil)
	if err == nil || !strings.Contains(err.Error(), "repository") {
		t.Errorf("expected a repository-required error, got %v", err)
	}
}

func TestUnsupportedOpsAreNamed(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func() (string, error)
	}{
		{"jira", func() (string, error) {
			return runJira(context.Background(), "t", "c", FlowNodeData{IntegrationOp: "explode"}, nil)
		}},
		{"confluence", func() (string, error) {
			return runConfluence(context.Background(), "t", "c", FlowNodeData{IntegrationOp: "explode"}, nil)
		}},
		{"bitbucket", func() (string, error) {
			return runBitbucket(context.Background(), "t", "w", FlowNodeData{IntegrationOp: "explode"}, nil)
		}},
	} {
		_, err := tc.run()
		if err == nil || !strings.Contains(err.Error(), "explode") {
			t.Errorf("%s: unsupported op should be named in the error, got %v", tc.name, err)
		}
	}
}
