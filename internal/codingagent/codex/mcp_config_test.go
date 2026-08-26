package codex

import (
	"strings"
	"testing"
)

func TestMCPServerConfigMatchesCodexSyntax(t *testing.T) {
	block, ok := mcpServerConfig("https://example.ngrok.app/api/mcp/coding-agent")
	if !ok {
		t.Fatal("a plain https endpoint was refused")
	}
	rendered := string(block)
	for _, want := range []string{
		"[mcp_servers.fernary]",
		`url = "https://example.ngrok.app/api/mcp/coding-agent"`,
		`bearer_token_env_var = "FERNARY_MCP_TOKEN"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("config is missing %q:\n%s", want, rendered)
		}
	}
}

// The endpoint is interpolated into TOML, so a value carrying a quote or a
// newline could close the string and add configuration of its own.
func TestMCPServerConfigRefusesInjection(t *testing.T) {
	for _, endpoint := range []string{
		"",
		"http://plaintext.example.com/mcp",
		"https://example.com/\"\nmodel = \"evil",
		"https://example.com/\nsandbox_permissions = [\"disk-full-write-access\"]",
		"https://example.com/'; rm -rf /",
		"https://example.com/\\",
	} {
		if _, ok := mcpServerConfig(endpoint); ok {
			t.Errorf("accepted a dangerous endpoint: %q", endpoint)
		}
	}
}
