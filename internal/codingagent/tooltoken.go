package codingagent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"

	"workflow-ai/server/internal/database/models"
)

// The sandbox calls back to use the workflow's own nodes, so it needs to prove
// which job it is. A bearer token does that, with two properties that matter:
// only its digest is stored, so reading the database does not yield the
// authority to act; and it is minted when the run starts and revoked when the
// run ends, so the window is the run itself rather than a clock someone has to
// keep tuning.

// ToolCallbackPath is where the sandbox reaches the workflow's node tools.
const ToolCallbackPath = "/api/mcp/coding-agent"

// PublicBaseURL is the address the sandbox can reach this server on.
//
// Same order as trigger hook registration, and for the same reason its comment
// gives: this is the one "where does the outside world find us" setting, and a
// second variable of our own would only be able to disagree with it. The
// sandbox runs in the provider's cloud, so a loopback address is useless to it
// and a tunnel is needed in development.
func PublicBaseURL() string {
	for _, key := range []string{"PUBLIC_BASE_URL", "OAUTH_REDIRECT_BASE"} {
		if value := strings.TrimRight(strings.TrimSpace(os.Getenv(key)), "/"); value != "" {
			return value
		}
	}
	return ""
}

func HashToolToken(token string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(digest[:])
}

// toolGrantCount reports how many nodes this job was granted. Zero means the
// runtime should configure no tool server at all.
func (s *Service) toolGrantCount(job *models.CodingAgentJob) int {
	if job == nil {
		return 0
	}
	var policy ToolPolicy
	if len(job.ToolPolicy) > 0 && json.Unmarshal(job.ToolPolicy, &policy) == nil && len(policy.Nodes) > 0 {
		return len(policy.Nodes)
	}
	var ids []string
	if err := json.Unmarshal(job.ToolNodeIDs, &ids); err != nil {
		return 0
	}
	return len(ids)
}

func (s *Service) mintToolToken(ctx context.Context, job *models.CodingAgentJob) (string, string, error) {
	base := PublicBaseURL()
	if base == "" {
		return "", "", errors.New("no publicly reachable server address is configured (set PUBLIC_BASE_URL)")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", errors.New("the public server address is invalid")
	}
	hostname := strings.ToLower(parsed.Hostname())
	loopback := hostname == "localhost"
	if address := net.ParseIP(hostname); address != nil {
		loopback = address.IsLoopback()
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		// The token is a bearer credential; sending it in clear over the public
		// internet would hand the job's authority to anyone on the path.
		return "", "", errors.New("the public server address must be https")
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token := hex.EncodeToString(raw)

	if err := s.db.WithContext(ctx).Model(&models.CodingAgentJob{}).
		Where("id = ?", job.ID).
		Update("tool_token_hash", HashToolToken(token)).Error; err != nil {
		return "", "", err
	}
	return base + ToolCallbackPath, token, nil
}

// revokeToolToken drops the digest so a sandbox that outlives its job — a leaked
// process, a retried attempt — cannot keep acting as it. Best effort by design:
// failing a finished job because its token could not be cleared would trade a
// completed run for a cleanup detail, and the job's terminal status already
// refuses the call on its own.
func (s *Service) revokeToolToken(job *models.CodingAgentJob) {
	if err := s.db.Model(&models.CodingAgentJob{}).
		Where("id = ?", job.ID).
		Update("tool_token_hash", "").Error; err != nil {
		slog.Error("could not revoke coding agent tool token", "job_id", job.ID.String(), "error", err)
	}
}
