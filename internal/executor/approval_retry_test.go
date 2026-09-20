package executor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// stubRetrier stands in for the real graph retrier so the loop can be driven
// without a live model call. It records what it was asked to revise.
type stubRetrier struct {
	retryable bool
	calls     []string // feedback, in the order received
	outputs   []string // what to return for each successive call
}

func (s *stubRetrier) CanRetry(string) bool { return s.retryable }

func (s *stubRetrier) Retry(_ context.Context, _, feedback, _ string) (string, error) {
	s.calls = append(s.calls, feedback)
	if len(s.calls) <= len(s.outputs) {
		return s.outputs[len(s.calls)-1], nil
	}
	return "revised", nil
}

func approvalGraph(gateTimeout int) WorkflowAST {
	seed := "first draft"
	source := WorkflowASTNode{ID: "src", Data: FlowNodeData{
		NodeType: NodeTypeTextInput, Label: "Draft", DefaultValue: &seed,
	}}
	gate := WorkflowASTNode{ID: "gate", Data: FlowNodeData{
		NodeType: NodeTypeHumanApproval, Label: "Review", ApprovalTimeout: gateTimeout,
	}}
	out := WorkflowASTNode{ID: "yes", Data: FlowNodeData{NodeType: NodeTypeTextOutput, Label: "Ship it"}}
	no := WorkflowASTNode{ID: "no", Data: FlowNodeData{NodeType: NodeTypeTextOutput, Label: "Dropped"}}
	approved, rejected := "approved", "rejected"
	return WorkflowAST{Name: "Gate", Nodes: []WorkflowASTNode{source, gate, out, no}, Edges: []WorkflowASTEdge{
		{ID: "e1", Source: "src", Target: "gate"},
		{ID: "e-yes", Source: "gate", Target: "yes", SourceHandle: &approved},
		{ID: "e-no", Source: "gate", Target: "no", SourceHandle: &rejected},
	}}
}

// feed answers each successive pause with the next decision in the list. The
// gate registers its channel only once it starts, so each decision is offered
// until it is taken rather than racing a single attempt.
func feed(runID string, decisions ...ApprovalDecision) {
	go func() {
		for _, d := range decisions {
			deadline := time.Now().Add(4 * time.Second)
			for time.Now().Before(deadline) {
				if ResolveApproval(runID+":gate", d) {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()
}

func runWithRetrier(t *testing.T, wf WorkflowAST, runID string, r ApprovalRetrier) []ExecutionEvent {
	t.Helper()
	var events []ExecutionEvent
	ctx := WithApprovalRetrier(context.Background(), r)
	RunWorkflow(ctx, wf, APIKeys{}, runID, "owner", "org",
		func(ev ExecutionEvent) { events = append(events, ev) })
	return events
}

// The whole point of the feature: a reviewer can reject, steer, and have the
// same run try again — rather than losing the run and starting over.
func TestRejectWithFeedbackRetriesAndThenApproves(t *testing.T) {
	stub := &stubRetrier{retryable: true, outputs: []string{"second draft", "third draft"}}
	feed("run-retry",
		ApprovalDecision{Action: ApprovalRetry, Feedback: "shorter please"},
		ApprovalDecision{Action: ApprovalRetry, Feedback: "less formal"},
		ApprovalDecision{Action: ApprovalApproved},
	)
	events := runWithRetrier(t, approvalGraph(5), "run-retry", stub)

	feedbacks := []string{}
	for _, ev := range eventsOfType(events, EventApprovalFeedback) {
		feedbacks = append(feedbacks, ev.Message)
	}
	if len(feedbacks) != 2 {
		t.Fatalf("recorded %d feedback events, want 2: %v", len(feedbacks), feedbacks)
	}
	if feedbacks[0] != "shorter please" || feedbacks[1] != "less formal" {
		t.Fatalf("feedback not recorded in order: %v", feedbacks)
	}
	if len(stub.calls) != 2 {
		t.Fatalf("retrier called %d times, want 2", len(stub.calls))
	}

	// After two revisions the reviewer approved, so the run must leave by the
	// approved edge — a retry must not be mistaken for a rejection.
	var tookApproved bool
	for _, ev := range eventsOfType(events, EventEdgeTaken) {
		if ev.EdgeID == "e-yes" {
			tookApproved = true
		}
		if ev.EdgeID == "e-no" {
			t.Fatal("took the rejected edge after the reviewer approved")
		}
	}
	if !tookApproved {
		t.Fatal("never took the approved edge")
	}
}

// A gate whose upstream node has no prompt cannot act on feedback. Rather than
// spin, it resolves as a rejection so the run always terminates.
func TestRetryOnANonModelNodeResolvesAsRejection(t *testing.T) {
	stub := &stubRetrier{retryable: false}
	feed("run-noretry", ApprovalDecision{Action: ApprovalRetry, Feedback: "try again"})
	events := runWithRetrier(t, approvalGraph(5), "run-noretry", stub)

	if len(stub.calls) != 0 {
		t.Fatalf("retrier was called %d times for a node that cannot retry", len(stub.calls))
	}
	var tookRejected bool
	for _, ev := range eventsOfType(events, EventEdgeTaken) {
		if ev.EdgeID == "e-no" {
			tookRejected = true
		}
	}
	if !tookRejected {
		t.Fatal("a retry that cannot be honoured must still resolve the gate")
	}
}

// The waiting event is what the UI reads to decide whether to offer the retry
// option at all, so the flag has to reflect the upstream node.
func TestWaitingEventAdvertisesWhetherRetryIsPossible(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retryable bool
		runID     string
	}{
		{"model upstream", true, "run-flag-yes"},
		{"plain upstream", false, "run-flag-no"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			feed(tc.runID, ApprovalDecision{Action: ApprovalRejected})
			events := runWithRetrier(t, approvalGraph(5), tc.runID, &stubRetrier{retryable: tc.retryable})

			waiting := eventsOfType(events, EventNodeWaiting)
			if len(waiting) == 0 {
				t.Fatal("gate never announced that it was waiting")
			}
			got, _ := waiting[0].Payload["canRetry"].(bool)
			if got != tc.retryable {
				t.Fatalf("canRetry = %v, want %v", got, tc.retryable)
			}
			if src, _ := waiting[0].Payload["sourceNodeId"].(string); src != "src" {
				t.Fatalf("sourceNodeId = %q, want the node feeding the gate", src)
			}
		})
	}
}

func TestSteerPromptCarriesTheRejectedOutputAndTheFeedback(t *testing.T) {
	got := steerPrompt("Write a release note.", "It was fine.", "Too vague — name the fix.")
	for _, want := range []string{
		"Write a release note.", // the original instruction survives
		"It was fine.",          // what the reviewer turned down
		"Too vague — name the fix.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("steer prompt is missing %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "Write a release note.") != 0 {
		t.Fatal("the original prompt must lead, so the steer reads as the latest turn")
	}
}

// The retrier must not offer to re-run something with no prompt to change.
func TestGraphRetrierOnlyRetriesModelNodes(t *testing.T) {
	r := &graphApprovalRetrier{nodeMap: map[string]WorkflowASTNode{
		"llm":   {ID: "llm", Data: FlowNodeData{NodeType: NodeTypeLLM}},
		"slack": {ID: "slack", Data: FlowNodeData{NodeType: NodeTypeSlack}},
	}}
	if !r.CanRetry("llm") {
		t.Fatal("a model node should be retryable")
	}
	if r.CanRetry("slack") {
		t.Fatal("a Slack node has no prompt to steer")
	}
	if r.CanRetry("missing") {
		t.Fatal("a node absent from the graph is not retryable")
	}
}

// The approval mail used to read an APP_URL that existed nowhere else in the
// codebase and in no .env file, so it was always empty and every link fell back
// to localhost — including in production.
func TestApprovalLinksUseTheConfiguredFrontendOrigin(t *testing.T) {
	t.Setenv("FRONTEND_URL", "https://fernary.com")
	if got := appBaseURL(context.Background()); got != "https://fernary.com" {
		t.Fatalf("appBaseURL = %q, want the configured frontend origin", got)
	}

	// FRONTEND_URL is a comma-separated allowlist elsewhere; links take the first.
	t.Setenv("FRONTEND_URL", "https://fernary.com,https://staging.fernary.com")
	if got := appBaseURL(context.Background()); got != "https://fernary.com" {
		t.Fatalf("appBaseURL = %q, want the first entry of the allowlist", got)
	}

	// A stale APP_URL must not resurrect the old behaviour.
	t.Setenv("APP_URL", "http://localhost:4905")
	t.Setenv("FRONTEND_URL", "https://fernary.com")
	if got := appBaseURL(context.Background()); got != "https://fernary.com" {
		t.Fatalf("appBaseURL = %q — APP_URL must no longer be consulted", got)
	}
}
