package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

// What we actually send to Vercel.
//
// Every path here is versioned individually (v13 deployments, v7 the list, v12
// the cancel, v9/v10 projects) and there is no way to spot a wrong version by
// reading the Go: it comes back as a 404 that looks like a missing resource. The
// same is true of teamId, whose absence turns a real project into a 404. These
// assert on the request line, because that is the thing that can be silently
// wrong.

type vercelStub struct {
	server *httptest.Server
	// requests records method + path + query, in order.
	requests []string
	bodies   []map[string]any
	// respond, when set, supplies the body for the Nth request.
	respond func(n int, r *http.Request) (int, string)
}

func newVercelStub(t *testing.T) *vercelStub {
	t.Helper()
	stub := &vercelStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := len(stub.requests)
		stub.requests = append(stub.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		stub.bodies = append(stub.bodies, body)

		status, payload := http.StatusOK, `{"ok":true}`
		if stub.respond != nil {
			status, payload = stub.respond(n, r)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))

	previousAPI, previousClient := vercelAPI, integrationHTTP
	vercelAPI = stub.server.URL
	integrationHTTP = stub.server.Client()
	t.Cleanup(func() {
		vercelAPI, integrationHTTP = previousAPI, previousClient
		stub.server.Close()
	})
	return stub
}

// asked reports whether any request carried this query parameter with this value,
// so the assertion does not depend on url.Values' encoding order.
func (s *vercelStub) asked(t *testing.T, index int, param, want string) bool {
	t.Helper()
	if index >= len(s.requests) {
		return false
	}
	_, rawQuery, _ := strings.Cut(s.requests[index], "?")
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatalf("unparseable query %q: %v", rawQuery, err)
	}
	return values.Get(param) == want
}

func runOp(t *testing.T, d FlowNodeData) (string, error) {
	t.Helper()
	return runVercel(context.Background(), "tok", d, map[string]string{})
}

// The endpoint versions, per operation. A wrong one is a 404 in production and
// nothing at all in review.
func TestVercelOperationsHitTheRightVersionedPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		data FlowNodeData
		want string
	}{
		{"list", FlowNodeData{IntegrationOp: "list_deployments"}, "GET /v7/deployments"},
		{"get", FlowNodeData{IntegrationOp: "get_deployment", VercelDeploymentId: "dpl_1"}, "GET /v13/deployments/dpl_1"},
		{"events", FlowNodeData{IntegrationOp: "get_deployment_events", VercelDeploymentId: "dpl_1"}, "GET /v3/deployments/dpl_1/events"},
		{"cancel", FlowNodeData{IntegrationOp: "cancel_deployment", VercelDeploymentId: "dpl_1"}, "PATCH /v12/deployments/dpl_1/cancel"},
		{"delete", FlowNodeData{IntegrationOp: "delete_deployment", VercelDeploymentId: "dpl_1"}, "DELETE /v13/deployments/dpl_1"},
		{"aliases", FlowNodeData{IntegrationOp: "list_deployment_aliases", VercelDeploymentId: "dpl_1"}, "GET /v2/deployments/dpl_1/aliases"},
		{"projects", FlowNodeData{IntegrationOp: "list_projects"}, "GET /v10/projects"},
		{"project", FlowNodeData{IntegrationOp: "get_project", VercelProjectId: "site"}, "GET /v9/projects/site"},
		{"promote", FlowNodeData{IntegrationOp: "promote_deployment", VercelProjectId: "p", VercelDeploymentId: "d"}, "POST /v10/projects/p/promote/d"},
		{"rollback", FlowNodeData{IntegrationOp: "rollback_deployment", VercelProjectId: "p", VercelDeploymentId: "d"}, "POST /v1/projects/p/rollback/d"},
		{"env list", FlowNodeData{IntegrationOp: "list_env_vars", VercelProjectId: "p"}, "GET /v10/projects/p/env"},
		{"env value", FlowNodeData{IntegrationOp: "get_env_var_value", VercelProjectId: "p", VercelEnvVarId: "e"}, "GET /v1/projects/p/env/e"},
		{"env delete", FlowNodeData{IntegrationOp: "delete_env_var", VercelProjectId: "p", VercelEnvVarId: "e"}, "DELETE /v9/projects/p/env/e"},
		{"runtime logs", FlowNodeData{IntegrationOp: "get_runtime_logs", VercelProjectId: "p", VercelDeploymentId: "d"}, "GET /v1/projects/p/deployments/d/runtime-logs"},
		{"domains", FlowNodeData{IntegrationOp: "list_domains"}, "GET /v5/domains"},
		{"project domains", FlowNodeData{IntegrationOp: "list_project_domains", VercelProjectId: "p"}, "GET /v9/projects/p/domains"},
		{"add domain", FlowNodeData{IntegrationOp: "add_project_domain", VercelProjectId: "p", VercelDomain: "a.com"}, "POST /v10/projects/p/domains"},
		{"teams", FlowNodeData{IntegrationOp: "list_teams"}, "GET /v2/teams"},
		{"user", FlowNodeData{IntegrationOp: "get_current_user"}, "GET /v2/user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newVercelStub(t)
			if _, err := runOp(t, tc.data); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(stub.requests) == 0 {
				t.Fatal("no request was made")
			}
			gotLine, _, _ := strings.Cut(stub.requests[0], "?")
			if gotLine != tc.want {
				t.Errorf("sent %q, want %q", gotLine, tc.want)
			}
		})
	}
}

// Team scoping is the failure this integration is most likely to hit, so it has
// to be on EVERY request, not just the ones that obviously belong to a team.
func TestEveryVercelCallCarriesTheTeamWhenSet(t *testing.T) {
	for _, op := range []struct {
		name string
		data FlowNodeData
	}{
		{"list_deployments", FlowNodeData{IntegrationOp: "list_deployments"}},
		{"get_deployment", FlowNodeData{IntegrationOp: "get_deployment", VercelDeploymentId: "d"}},
		{"list_projects", FlowNodeData{IntegrationOp: "list_projects"}},
		{"list_env_vars", FlowNodeData{IntegrationOp: "list_env_vars", VercelProjectId: "p"}},
		{"list_domains", FlowNodeData{IntegrationOp: "list_domains"}},
		{"promote_deployment", FlowNodeData{IntegrationOp: "promote_deployment", VercelProjectId: "p", VercelDeploymentId: "d"}},
		{"delete_env_var", FlowNodeData{IntegrationOp: "delete_env_var", VercelProjectId: "p", VercelEnvVarId: "e"}},
	} {
		t.Run(op.name, func(t *testing.T) {
			stub := newVercelStub(t)
			data := op.data
			data.VercelTeamId = "team_abc"
			if _, err := runOp(t, data); err != nil {
				t.Fatalf("%v", err)
			}
			if !stub.asked(t, 0, "teamId", "team_abc") {
				t.Errorf("%s dropped teamId — the request runs against the personal "+
					"scope and a team resource 404s. Sent: %s", op.name, stub.requests[0])
			}
		})
	}
}

// The slug is the documented alternative, and sending both is not valid.
func TestVercelSlugIsUsedOnlyWhenNoTeamIDIsSet(t *testing.T) {
	stub := newVercelStub(t)
	if _, err := runOp(t, FlowNodeData{IntegrationOp: "list_projects", VercelTeamSlug: "acme"}); err != nil {
		t.Fatal(err)
	}
	if !stub.asked(t, 0, "slug", "acme") {
		t.Errorf("slug not sent: %s", stub.requests[0])
	}

	stub2 := newVercelStub(t)
	if _, err := runOp(t, FlowNodeData{
		IntegrationOp: "list_projects", VercelTeamId: "team_abc", VercelTeamSlug: "acme",
	}); err != nil {
		t.Fatal(err)
	}
	if !stub2.asked(t, 0, "teamId", "team_abc") {
		t.Error("teamId should win when both are set")
	}
	if stub2.asked(t, 0, "slug", "acme") {
		t.Error("sent both teamId and slug; Vercel expects one or the other")
	}
}

// Redeploy has two traps: `name` is required even though the deployment already
// knows it, and without forceNew Vercel returns the EXISTING deployment, which
// reads to a user as "my redeploy did nothing".
func TestRedeployReadsTheProjectNameBackAndForcesAFreshBuild(t *testing.T) {
	stub := newVercelStub(t)
	stub.respond = func(n int, _ *http.Request) (int, string) {
		if n == 0 {
			return http.StatusOK, `{"name":"my-site","target":"production"}`
		}
		return http.StatusOK, `{"id":"dpl_new"}`
	}

	if _, err := runOp(t, FlowNodeData{IntegrationOp: "redeploy", VercelDeploymentId: "dpl_old"}); err != nil {
		t.Fatal(err)
	}
	if len(stub.requests) != 2 {
		t.Fatalf("made %d requests, want 2 (read the deployment, then create) — %v",
			len(stub.requests), stub.requests)
	}
	if !strings.HasPrefix(stub.requests[1], "POST /v13/deployments") {
		t.Errorf("second request was %q", stub.requests[1])
	}
	if !stub.asked(t, 1, "forceNew", "1") {
		t.Error("forceNew=1 missing — Vercel would deduplicate and hand back the " +
			"deployment that already exists, so nothing would rebuild")
	}
	if got := stub.bodies[1]["name"]; got != "my-site" {
		t.Errorf("body name = %v, want the project name read off the source deployment", got)
	}
	if got := stub.bodies[1]["deploymentId"]; got != "dpl_old" {
		t.Errorf("body deploymentId = %v", got)
	}
}

// When the name is supplied there is no reason to spend a request discovering it.
func TestRedeploySkipsTheLookupWhenGivenAName(t *testing.T) {
	stub := newVercelStub(t)
	if _, err := runOp(t, FlowNodeData{
		IntegrationOp: "redeploy", VercelDeploymentId: "dpl_old", VercelName: "my-site",
	}); err != nil {
		t.Fatal(err)
	}
	if len(stub.requests) != 1 {
		t.Errorf("made %d requests, want 1 — the name was already known", len(stub.requests))
	}
}

// A live-streaming log endpoint would hang the run until it timed out.
func TestBuildLogsNeverRequestAFollowStream(t *testing.T) {
	stub := newVercelStub(t)
	if _, err := runOp(t, FlowNodeData{
		IntegrationOp: "get_deployment_events", VercelDeploymentId: "dpl_1",
	}); err != nil {
		t.Fatal(err)
	}
	_, rawQuery, _ := strings.Cut(stub.requests[0], "?")
	if strings.Contains(rawQuery, "follow") {
		t.Errorf("sent follow= on the build log; that returns an infinite stream of "+
			"live events and the run would hang. Query: %s", rawQuery)
	}
	if stub.asked(t, 0, "limit", "-1") {
		t.Error("limit=-1 asks for every log line ever, unbounded")
	}
}

// create_env_var must send target as a LIST, and must refuse rather than silently
// creating a variable that applies to nothing.
func TestCreateEnvVarSendsTargetsAsAListAndRequiresOne(t *testing.T) {
	stub := newVercelStub(t)
	if _, err := runOp(t, FlowNodeData{
		IntegrationOp: "create_env_var", VercelProjectId: "p",
		VercelEnvKey: "API_URL", VercelEnvValue: "https://x", VercelEnvTarget: "production, preview",
	}); err != nil {
		t.Fatal(err)
	}
	targets, ok := stub.bodies[0]["target"].([]any)
	if !ok {
		t.Fatalf("target was %T, want a JSON array", stub.bodies[0]["target"])
	}
	if len(targets) != 2 || targets[0] != "production" || targets[1] != "preview" {
		t.Errorf("target = %v, want [production preview] with the space trimmed", targets)
	}
	if got := stub.bodies[0]["type"]; got != "encrypted" {
		t.Errorf("type = %v, want encrypted by default", got)
	}

	// No target at all is a user error, not a request to make.
	blank := newVercelStub(t)
	_, err := runOp(t, FlowNodeData{
		IntegrationOp: "create_env_var", VercelProjectId: "p", VercelEnvKey: "K",
	})
	if err == nil {
		t.Fatal("expected an error when no target was chosen")
	}
	if !strings.Contains(err.Error(), "production") {
		t.Errorf("error %q should name the valid targets", err)
	}
	if len(blank.requests) != 0 {
		t.Error("a request was sent despite the missing target")
	}
}

// A PATCH sends only what changed. Sending an empty value would blank the
// variable, which is data loss dressed as an update.
func TestUpdateEnvVarOmitsFieldsThatWereNotSet(t *testing.T) {
	stub := newVercelStub(t)
	if _, err := runOp(t, FlowNodeData{
		IntegrationOp: "update_env_var", VercelProjectId: "p", VercelEnvVarId: "e",
		VercelEnvValue: "new-value",
	}); err != nil {
		t.Fatal(err)
	}
	body := stub.bodies[0]
	if got := body["value"]; got != "new-value" {
		t.Errorf("value = %v", got)
	}
	for _, absent := range []string{"key", "target", "type", "gitBranch"} {
		if _, present := body[absent]; present {
			t.Errorf("%q was sent despite not being set — that would overwrite the "+
				"variable's current %s", absent, absent)
		}
	}

	// And an update with nothing in it is refused rather than sent as a no-op PATCH.
	empty := newVercelStub(t)
	if _, err := runOp(t, FlowNodeData{
		IntegrationOp: "update_env_var", VercelProjectId: "p", VercelEnvVarId: "e",
	}); err == nil {
		t.Error("expected an error when there is nothing to update")
	} else if len(empty.requests) != 0 {
		t.Error("sent an empty PATCH")
	}
}

// Every op that needs an id must say which one, before spending a request.
func TestVercelMissingIdentifiersFailWithoutCallingTheAPI(t *testing.T) {
	for _, tc := range []struct {
		op      string
		mustSay string
		data    FlowNodeData
	}{
		{"get_deployment", "deployment", FlowNodeData{IntegrationOp: "get_deployment"}},
		{"get_project", "project", FlowNodeData{IntegrationOp: "get_project"}},
		{"list_env_vars", "project", FlowNodeData{IntegrationOp: "list_env_vars"}},
		{"delete_env_var", "environment variable", FlowNodeData{IntegrationOp: "delete_env_var", VercelProjectId: "p"}},
		{"assign_alias", "alias", FlowNodeData{IntegrationOp: "assign_alias", VercelDeploymentId: "d"}},
		{"get_domain", "domain", FlowNodeData{IntegrationOp: "get_domain"}},
		{"get_runtime_logs", "project", FlowNodeData{IntegrationOp: "get_runtime_logs"}},
		{"promote_deployment", "project", FlowNodeData{IntegrationOp: "promote_deployment"}},
	} {
		t.Run(tc.op, func(t *testing.T) {
			stub := newVercelStub(t)
			_, err := runOp(t, tc.data)
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("error %q does not name what is missing (%q)", err, tc.mustSay)
			}
			if len(stub.requests) != 0 {
				t.Errorf("spent a request anyway: %v", stub.requests)
			}
		})
	}
}

// An unknown op must be rejected, not quietly treated as a read.
func TestUnknownVercelOperationIsRejected(t *testing.T) {
	stub := newVercelStub(t)
	_, err := runOp(t, FlowNodeData{IntegrationOp: "drop_everything"})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("err = %v, want an unsupported-operation error", err)
	}
	if len(stub.requests) != 0 {
		t.Error("an unknown operation reached the network")
	}
}

// Error messages are the whole support story for this integration: a 404 that
// really means "you forgot the team" has to say so.
func TestVercelErrorsExplainTheLikelyCause(t *testing.T) {
	for _, tc := range []struct {
		status int
		expect string
	}{
		{http.StatusUnauthorized, "reconnect"},
		{http.StatusForbidden, "scope"},
		{http.StatusNotFound, "Team ID"},
		{http.StatusTooManyRequests, "rate-limit"},
	} {
		stub := newVercelStub(t)
		stub.respond = func(int, *http.Request) (int, string) {
			return tc.status, `{"error":{"code":"forbidden","message":"upstream said no"}}`
		}
		_, err := runOp(t, FlowNodeData{IntegrationOp: "list_projects"})
		if err == nil {
			t.Fatalf("%d produced no error", tc.status)
		}
		if !strings.Contains(err.Error(), tc.expect) {
			t.Errorf("%d error %q does not mention %q", tc.status, err, tc.expect)
		}
		// The provider's own message must survive; ours is an addition, not a
		// replacement.
		if !strings.Contains(err.Error(), "upstream said no") {
			t.Errorf("%d dropped Vercel's own message: %q", tc.status, err)
		}
	}
}

// A 204 with no body must not look like a failure to the next node.
func TestVercelEmptyResponseBecomesValidJSON(t *testing.T) {
	stub := newVercelStub(t)
	stub.respond = func(int, *http.Request) (int, string) { return http.StatusNoContent, "" }
	out, err := runOp(t, FlowNodeData{IntegrationOp: "delete_env_var", VercelProjectId: "p", VercelEnvVarId: "e"})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output %q is not JSON: %v", out, err)
	}
	if parsed["ok"] != true {
		t.Errorf("output = %v, want ok:true", parsed)
	}
}

// Templates have to resolve, or a deployment id read from an earlier node arrives
// as the literal text "{{trigger.output}}".
func TestVercelResolvesTemplatesFromEarlierNodes(t *testing.T) {
	stub := newVercelStub(t)
	_, err := runVercel(context.Background(), "tok", FlowNodeData{
		IntegrationOp:      "get_deployment",
		VercelDeploymentId: "{{trigger.output}}",
	}, map[string]string{"trigger": "dpl_from_webhook"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stub.requests[0], "dpl_from_webhook") {
		t.Errorf("template was not substituted: %s", stub.requests[0])
	}
}

// tailStr exists so a failed build's error survives truncation. Truncating from
// the front would keep the install phase and throw the error away.
func TestBuildLogTruncationKeepsTheEnd(t *testing.T) {
	long := strings.Repeat("installing…\n", 4000) + "FATAL: build failed on line 12"
	if got := tailStr(long, 200); !strings.Contains(got, "FATAL: build failed") {
		t.Error("tailStr dropped the end of the log, which is the only useful part")
	}
	if got := tailStr("short", 200); got != "short" {
		t.Errorf("tailStr mangled a short string: %q", got)
	}
}

// Both truncation helpers cut at a byte offset, which can land inside a
// multibyte character and emit its continuation bytes alone. Go does not error
// on invalid UTF-8 — json.Marshal substitutes U+FFFD — so the only symptom is
// mojibake in a run output. Real payloads hit this constantly: a Next.js build
// log is full of ✓ and ▲, and names and subjects are routinely non-ASCII.
//
// Reported by review against tailStr and reproduced before fixing: tailStr("a€b", 3)
// returned bytes 2e 2e 2e 82 ac 62. truncateStr had the same defect.
func TestTruncationNeverSplitsAMultibyteCharacter(t *testing.T) {
	// Every cut position across a string of mixed rune widths: 1-byte, 3-byte and
	// the 4-byte emoji, so each n lands inside a rune at some point.
	subject := "a€b✓c🚀d"
	for n := 0; n <= len(subject)+2; n++ {
		if got := truncateStr(subject, n); !utf8.ValidString(got) {
			t.Errorf("truncateStr(%q, %d) = %q — invalid UTF-8 (% x)", subject, n, got, []byte(got))
		}
		if got := tailStr(subject, n); !utf8.ValidString(got) {
			t.Errorf("tailStr(%q, %d) = %q — invalid UTF-8 (% x)", subject, n, got, []byte(got))
		}
	}

	// The exact case from the review.
	if got := tailStr("a€b", 3); !utf8.ValidString(got) {
		t.Errorf("the reported case still yields invalid UTF-8: %q (% x)", got, []byte(got))
	}

	// Adjusting to a boundary must not swallow the whole string, or a log tail
	// becomes an ellipsis. At most three bytes are given up.
	long := strings.Repeat("✓", 500) + "FATAL"
	tail := tailStr(long, 100)
	if !strings.HasSuffix(tail, "FATAL") {
		t.Errorf("boundary adjustment lost the end of the string: %q", tail)
	}
	if len(tail) < 100-3 {
		t.Errorf("tailStr gave up %d bytes adjusting to a boundary, want at most 3",
			100-len(tail))
	}

	// A string that is entirely one multibyte rune, cut below its width, must
	// still be valid — the degenerate case that would tempt an off-by-one.
	for n := 0; n < 4; n++ {
		if got := tailStr("🚀", n); !utf8.ValidString(got) {
			t.Errorf("tailStr(%q, %d) = %q — invalid", "🚀", n, got)
		}
		if got := truncateStr("🚀", n); !utf8.ValidString(got) {
			t.Errorf("truncateStr(%q, %d) = %q — invalid", "🚀", n, got)
		}
	}

	// Pure ASCII must be unaffected, since that is the overwhelming majority of
	// payloads and any change there would be a silent regression in output size.
	ascii := strings.Repeat("x", 50)
	if got := truncateStr(ascii, 10); got != strings.Repeat("x", 10)+"..." {
		t.Errorf("ASCII truncation changed: %q", got)
	}
	if got := tailStr(ascii, 10); got != "..."+strings.Repeat("x", 10) {
		t.Errorf("ASCII tail changed: %q", got)
	}
}
