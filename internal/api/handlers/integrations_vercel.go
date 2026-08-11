package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Pickable Vercel resources.
//
// Two things make this different from every other provider's resource list.
//
// **Nothing except teams can be listed without knowing the team.** A token that
// belongs to someone in several teams resolves to their personal scope, so
// asking for projects with no team returns their personal projects — not an
// error, just the wrong list. The picker therefore mirrors the executor exactly:
// no team chosen means personal scope in both places. That is why teams are the
// only top-level list worth its own kind, and why projects appear at the top
// level too (a personal account has no team to pick).
//
// **Two of the useful lists are two levels deep.** Deployments and environment
// variables belong to a project, which belongs to a team, and the resource route
// carries a single opaque `parent`. So Vercel's parent is composite:
//
//	"team_abc"              → projects and domains in that team
//	"team_abc/prj_xyz"      → deployments and env vars in that project
//	"/prj_xyz"              → the same, on a personal account (no team)
//
// GitHub already does this — its branch parent is "owner/repo" — so the
// convention is borrowed rather than invented. Split on the FIRST slash only:
// team and project ids never contain one, and neither do project names.

// vercelResourceAPI is a var so tests can point it at a stub.
var vercelResourceAPI = "https://api.vercel.com"

func vercelResourceCall(token, path string, out any) error {
	req, _ := http.NewRequest(http.MethodGet, vercelResourceAPI+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	raw, err := doOAuthRequest(req)
	if err != nil {
		return err
	}
	if json.Unmarshal(raw, out) != nil {
		return fmt.Errorf("parse vercel response for %s", path)
	}
	return nil
}

// vercelScopeQuery renders the team scope the same way the executor does, so a
// picker and a run can never disagree about which account they are looking at.
func vercelScopeQuery(team string) string {
	if team == "" {
		return ""
	}
	// A slug is accepted here too; teamId is what the picker stores.
	return "&teamId=" + url.QueryEscape(team)
}

func vercelTeamResources(token string) ([]integrationResource, error) {
	var payload struct {
		Teams []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Slug string `json:"slug"`
		} `json:"teams"`
	}
	if err := vercelResourceCall(token, "/v2/teams?limit=100", &payload); err != nil {
		return nil, err
	}
	out := make([]integrationResource, 0, len(payload.Teams))
	for _, t := range payload.Teams {
		// The slug reads better than team_xxx, but the id is what the API wants,
		// so show one and store the other.
		out = append(out, integrationResource{
			ID: t.ID, Name: firstNonEmptyStr(t.Name, t.Slug, t.ID), Type: "team",
		})
	}
	return out, nil
}

func vercelProjectResources(token, team string) ([]integrationResource, error) {
	var payload struct {
		Projects []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Framework string `json:"framework"`
		} `json:"projects"`
	}
	if err := vercelResourceCall(token, "/v10/projects?limit=100"+vercelScopeQuery(team), &payload); err != nil {
		return nil, err
	}
	out := make([]integrationResource, 0, len(payload.Projects))
	for _, p := range payload.Projects {
		// The name, not the id: every project endpoint takes idOrName, the name is
		// what someone recognises in a dropdown, and it is what they would have
		// typed. Storing it keeps the saved workflow readable.
		out = append(out, integrationResource{ID: p.Name, Name: p.Name, Type: "project"})
	}
	return out, nil
}

func vercelDomainResources(token, team string) ([]integrationResource, error) {
	var payload struct {
		Domains []struct {
			Name string `json:"name"`
		} `json:"domains"`
	}
	if err := vercelResourceCall(token, "/v5/domains?limit=100"+vercelScopeQuery(team), &payload); err != nil {
		return nil, err
	}
	out := make([]integrationResource, 0, len(payload.Domains))
	for _, d := range payload.Domains {
		out = append(out, integrationResource{ID: d.Name, Name: d.Name, Type: "domain"})
	}
	return out, nil
}

func vercelDeploymentResources(token, team, project string) ([]integrationResource, error) {
	var payload struct {
		Deployments []struct {
			UID   string `json:"uid"`
			Name  string `json:"name"`
			URL   string `json:"url"`
			State string `json:"state"`
			// Target is null for a preview deployment.
			Target  *string `json:"target"`
			Created int64   `json:"created"`
		} `json:"deployments"`
	}
	path := "/v7/deployments?limit=20&projectId=" + url.QueryEscape(project) + vercelScopeQuery(team)
	if err := vercelResourceCall(token, path, &payload); err != nil {
		return nil, err
	}
	out := make([]integrationResource, 0, len(payload.Deployments))
	for _, d := range payload.Deployments {
		// A bare dpl_… id is unreadable, so the label carries what someone would
		// actually recognise: which environment, whether it worked, and its host.
		target := "preview"
		if d.Target != nil && *d.Target != "" {
			target = *d.Target
		}
		label := fmt.Sprintf("%s · %s", target, strings.ToLower(firstNonEmptyStr(d.State, "unknown")))
		if d.URL != "" {
			label += " · " + d.URL
		}
		out = append(out, integrationResource{ID: d.UID, Name: label, Type: "deployment"})
	}
	return out, nil
}

func vercelEnvVarResources(token, team, project string) ([]integrationResource, error) {
	var payload struct {
		Envs []struct {
			ID     string   `json:"id"`
			Key    string   `json:"key"`
			Target []string `json:"target"`
			Type   string   `json:"type"`
		} `json:"envs"`
	}
	path := "/v10/projects/" + url.PathEscape(project) + "/env?" + strings.TrimPrefix(vercelScopeQuery(team), "&")
	if err := vercelResourceCall(token, path, &payload); err != nil {
		return nil, err
	}
	out := make([]integrationResource, 0, len(payload.Envs))
	for _, e := range payload.Envs {
		// The panel needs the variable's ID — its key alone will not address it —
		// but a list of ids is unusable, so the label is the key plus where it
		// applies. No value is included: these labels reach the browser.
		label := e.Key
		if len(e.Target) > 0 {
			label += " · " + strings.Join(e.Target, ", ")
		}
		out = append(out, integrationResource{ID: e.ID, Name: label, Type: "envvar"})
	}
	return out, nil
}

// vercelResources is the top-level list: the teams a token can reach, plus the
// projects and domains visible in its personal scope.
//
// Personal-scope projects are included deliberately. A personal account has no
// team to pick, and leaving the team blank is exactly what the executor treats as
// personal scope — so the picker shows what a run with no team would actually
// see.
func vercelResources(token string) ([]integrationResource, error) {
	teams, err := vercelTeamResources(token)
	if err != nil {
		// Teams are the one list that needs no scope, so a failure here means the
		// token itself is bad and nothing else will work either.
		return nil, err
	}
	out := teams
	// A token scoped to a single team returns nothing for these, which is fine —
	// the picker waits for a team instead. Neither failure should hide the teams.
	if projects, err := vercelProjectResources(token, ""); err == nil {
		out = append(out, projects...)
	}
	if domains, err := vercelDomainResources(token, ""); err == nil {
		out = append(out, domains...)
	}
	return out, nil
}

// vercelChildResources resolves the composite parent described at the top of this
// file: a team alone, or "team/project".
func vercelChildResources(token, parent string) ([]integrationResource, error) {
	team, project, nested := strings.Cut(parent, "/")
	if !nested {
		// A team: its projects and its domains.
		projects, err := vercelProjectResources(token, team)
		if err != nil {
			return nil, err
		}
		out := projects
		if domains, err := vercelDomainResources(token, team); err == nil {
			out = append(out, domains...)
		}
		return out, nil
	}
	if strings.TrimSpace(project) == "" {
		// "team/" — the project has not been chosen yet. An empty list keeps the
		// picker in its waiting state rather than raising a toast.
		return []integrationResource{}, nil
	}
	deployments, err := vercelDeploymentResources(token, team, project)
	if err != nil {
		return nil, err
	}
	out := deployments
	// Environment variables need a project the token can administer; a
	// read-only member still gets usable deployments, so don't lose those.
	if envVars, err := vercelEnvVarResources(token, team, project); err == nil {
		out = append(out, envVars...)
	}
	return out, nil
}
