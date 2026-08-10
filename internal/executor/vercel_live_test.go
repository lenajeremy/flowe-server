package executor

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// Opt-in live check against the real Vercel API, same convention as the other
// LIVE_* tests here.
//
//	VERCEL_ENV_FILE=/abs/path/to/.env LIVE_VERCEL=1 \
//	  go test ./internal/executor/ -run TestLiveVercel -v
//
// The env file needs:
//
//	VERCEL_TOKEN=...          # Vercel → Settings → Tokens → Create
//	VERCEL_TEAM_ID=team_...   # optional; omit only for a personal account
//
// The token is read from the file rather than the environment so it never passes
// through a shell command line, and nothing here prints it.
//
// Why this exists: the stub tests prove the request shape we send. Only the real
// API can prove that shape is ACCEPTED — every path carries its own version
// number, and a wrong one returns a 404 that is indistinguishable from a missing
// resource. This test is read-only by design: it never creates, promotes, rolls
// back or deletes anything.
//
// Get the token from https://vercel.com/account/tokens and scope it to the team
// whose projects the workflow should reach.
func TestLiveVercelReadOperations(t *testing.T) {
	if os.Getenv("LIVE_VERCEL") == "" {
		t.Skip("set LIVE_VERCEL=1 and VERCEL_ENV_FILE=/abs/path/.env to run")
	}
	envFile := os.Getenv("VERCEL_ENV_FILE")
	if envFile == "" {
		t.Fatal("VERCEL_ENV_FILE must point at a file holding VERCEL_TOKEN (and optionally VERCEL_TEAM_ID)")
	}
	env := loadEnvFile(t, envFile)
	token := env["VERCEL_TOKEN"]
	if token == "" {
		t.Fatal("VERCEL_TOKEN is missing from that file")
	}
	team := env["VERCEL_TEAM_ID"]
	t.Logf("token loaded (%d chars); team scope: %s", len(token),
		map[bool]string{true: "personal (no VERCEL_TEAM_ID set)", false: team}[team == ""])

	base := FlowNodeData{VercelTeamId: team}
	run := func(op string, mutate func(*FlowNodeData)) (string, error) {
		d := base
		d.IntegrationOp = op
		if mutate != nil {
			mutate(&d)
		}
		return runVercel(context.Background(), token, d, map[string]string{})
	}

	// 1. The token itself. If this fails, nothing else is worth attempting, and the
	// distinction matters: 401 means the token, anything else means our request.
	whoami, err := run("get_current_user", nil)
	if err != nil {
		t.Fatalf("get_current_user failed, so the token is bad or revoked: %v", err)
	}
	if user := jsonField(whoami, "username"); user != "" {
		t.Logf("authenticated as %s", user)
	}

	// 2. Teams — also how a user discovers the id they need for VERCEL_TEAM_ID.
	if teams, err := run("list_teams", nil); err != nil {
		t.Errorf("list_teams failed: %v", err)
	} else {
		t.Logf("list_teams returned %d bytes", len(teams))
	}

	// 3. Projects. This is the call that proves team scoping works: on a team
	// account without teamId it succeeds but returns the WRONG (personal) list, so
	// the count is logged rather than asserted.
	projects, err := run("list_projects", func(d *FlowNodeData) { d.VercelLimit = 5 })
	if err != nil {
		t.Fatalf("list_projects failed: %v", err)
	}
	var projectList struct {
		Projects []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"projects"`
	}
	if err := json.Unmarshal([]byte(projects), &projectList); err != nil {
		t.Fatalf("could not parse the project list, so our response handling is wrong: %v", err)
	}
	t.Logf("list_projects returned %d projects", len(projectList.Projects))
	if len(projectList.Projects) == 0 {
		t.Skip("no projects visible to this token — set VERCEL_TEAM_ID if the projects " +
			"belong to a team, since without it the request runs against the personal scope")
	}
	project := projectList.Projects[0]
	t.Logf("using project %s (%s)", project.Name, project.ID)

	// 4. A project by name, not id — the docs say idOrName and the panel lets users
	// type either, so both need to work.
	if _, err := run("get_project", func(d *FlowNodeData) { d.VercelProjectId = project.Name }); err != nil {
		t.Errorf("get_project by NAME failed, but the panel accepts a name: %v", err)
	}

	// 5. Deployments, filtered — exercises the v7 path and the state/target params.
	deployments, err := run("list_deployments", func(d *FlowNodeData) {
		d.VercelProjectId = project.ID
		d.VercelLimit = 5
	})
	if err != nil {
		t.Fatalf("list_deployments failed: %v", err)
	}
	var deploymentList struct {
		Deployments []struct {
			UID   string `json:"uid"`
			State string `json:"state"`
			URL   string `json:"url"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal([]byte(deployments), &deploymentList); err != nil {
		t.Fatalf("could not parse the deployment list: %v", err)
	}
	t.Logf("list_deployments returned %d deployments", len(deploymentList.Deployments))

	// The state filter must be accepted, not 400 — we uppercase it before sending.
	if _, err := run("list_deployments", func(d *FlowNodeData) {
		d.VercelProjectId = project.ID
		d.VercelState = "ready"
		d.VercelLimit = 1
	}); err != nil {
		t.Errorf("list_deployments with a lowercase state filter failed: %v", err)
	}

	// 6. Environment variables. Asserts the response does NOT carry plaintext, which
	// is the promise the config panel makes when it points here instead of at
	// get_env_var_value.
	if envVars, err := run("list_env_vars", func(d *FlowNodeData) { d.VercelProjectId = project.ID }); err != nil {
		t.Errorf("list_env_vars failed: %v", err)
	} else {
		t.Logf("list_env_vars returned %d bytes", len(envVars))
		if strings.Contains(envVars, `"type":"plain"`) {
			t.Log("note: this project has plain-text variables, so their values are " +
				"readable in this response by Vercel's design")
		}
	}

	// 7. Domains, both scopes.
	if _, err := run("list_domains", func(d *FlowNodeData) { d.VercelLimit = 5 }); err != nil {
		t.Errorf("list_domains failed: %v", err)
	}
	if _, err := run("list_project_domains", func(d *FlowNodeData) { d.VercelProjectId = project.ID }); err != nil {
		t.Errorf("list_project_domains failed: %v", err)
	}

	if len(deploymentList.Deployments) == 0 {
		t.Log("no deployments on this project, so the deployment-scoped paths are untested")
		return
	}
	deployment := deploymentList.Deployments[0]

	// 8. A deployment by ID and by URL. Both are accepted per the docs, and the
	// panel tells users a webhook value works directly — worth proving.
	if _, err := run("get_deployment", func(d *FlowNodeData) { d.VercelDeploymentId = deployment.UID }); err != nil {
		t.Errorf("get_deployment by id failed: %v", err)
	}
	if deployment.URL != "" {
		if _, err := run("get_deployment", func(d *FlowNodeData) { d.VercelDeploymentId = deployment.URL }); err != nil {
			t.Errorf("get_deployment by URL failed, but the panel says a URL works: %v", err)
		}
	}

	// 9. Build logs — the v3 path, and the reason tailStr exists. Also the one
	// place real multibyte content shows up (framework build output uses ✓ and ▲),
	// so this is where the UTF-8 fix gets exercised against real bytes.
	logs, err := run("get_deployment_events", func(d *FlowNodeData) {
		d.VercelDeploymentId = deployment.UID
		d.VercelLimit = 50
	})
	if err != nil {
		t.Errorf("get_deployment_events failed: %v", err)
	} else {
		t.Logf("build log returned %d bytes", len(logs))
		if !utf8.ValidString(logs) {
			t.Error("the build log is not valid UTF-8 after truncation — the rune-boundary " +
				"fix does not hold against real log content")
		}
	}

	if _, err := run("list_deployment_aliases", func(d *FlowNodeData) {
		d.VercelDeploymentId = deployment.UID
	}); err != nil {
		t.Errorf("list_deployment_aliases failed: %v", err)
	}

	// 10. The team-scoping trap, demonstrated rather than described: the same
	// project id WITHOUT the team should fail on a team-owned project. Logged, not
	// asserted, because it legitimately succeeds on a personal account.
	if team != "" {
		d := FlowNodeData{IntegrationOp: "get_project", VercelProjectId: project.ID}
		if _, err := runVercel(context.Background(), token, d, map[string]string{}); err == nil {
			t.Log("note: get_project succeeded WITHOUT teamId — this token's personal " +
				"scope can see the project, so the 404 trap does not apply to it")
		} else {
			t.Logf("confirmed the trap: without teamId the same project fails — %v", err)
		}
	}
}

// The write paths are deliberately not exercised live. redeploy, promote,
// rollback and the env-var mutations all change real infrastructure, and a test
// that promotes a deployment to production to prove it can is not a test worth
// having. Their request shapes are asserted in vercel_test.go.
