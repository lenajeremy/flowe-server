package daytona

import "testing"

func TestEnvironmentPrefixQuotesValuesAndRejectsNames(t *testing.T) {
	got := environmentPrefix(map[string]string{
		"SAFE_NAME": "don't expand $HOME; touch /tmp/nope",
		"bad-name":  "ignored",
	})
	if got != `env SAFE_NAME='don'"'"'t expand $HOME; touch /tmp/nope' ` {
		t.Fatalf("environment prefix = %q", got)
	}
}

func TestCommandCompositionChangesDirectoryBeforeApplyingEnvironment(t *testing.T) {
	command := composeCommand("/tmp/work", map[string]string{"CODEX_HOME": "/tmp/codex"}, "codex login --device-auth")
	want := "cd -- '/tmp/work' && env CODEX_HOME='/tmp/codex' codex login --device-auth"
	if command != want {
		t.Fatalf("composed command = %q, want %q", command, want)
	}
}

func TestSafeRemotePathRejectsTraversal(t *testing.T) {
	for _, value := range []string{"relative/file", "/tmp/../secret", "/tmp/\x00secret"} {
		if safeRemotePath(value) {
			t.Fatalf("safeRemotePath(%q) = true", value)
		}
	}
	if !safeRemotePath("/home/daytona/project/file.txt") {
		t.Fatal("normalized absolute path was rejected")
	}
}

func TestCappedBufferKeepsMostRecentOutput(t *testing.T) {
	buffer := cappedBuffer{limit: 5}
	buffer.WriteString("abc")
	buffer.WriteString("def")
	if got := buffer.String(); got != "bcdef" {
		t.Fatalf("buffer = %q, want bcdef", got)
	}
	buffer.WriteString("123456")
	if got := buffer.String(); got != "23456" {
		t.Fatalf("buffer = %q, want 23456", got)
	}
}

func TestNormalizeDomainsDropsMalformedAndDuplicates(t *testing.T) {
	got := normalizeDomains([]string{" API.OPENAI.COM ", "api.openai.com", "https://github.com", "github.com"})
	if len(got) != 2 || got[0] != "api.openai.com" || got[1] != "github.com" {
		t.Fatalf("normalizeDomains = %#v", got)
	}
}

func TestSandboxNetworkSettingsUsesOneDaytonaFirewallMode(t *testing.T) {
	blockAll, allowList := sandboxNetworkSettings(true, []string{"api.openai.com", "github.com"})
	if blockAll {
		t.Fatal("domain allow list was combined with networkBlockAll")
	}
	if allowList == nil || *allowList != "api.openai.com,github.com" {
		t.Fatalf("allow list = %v", allowList)
	}
	blockAll, allowList = sandboxNetworkSettings(true, nil)
	if !blockAll || allowList != nil {
		t.Fatalf("block-only settings = %v, %v", blockAll, allowList)
	}
}

func TestCommandEnvironmentRejectsCredentials(t *testing.T) {
	if err := validateCommandEnvironment(map[string]string{"CODEX_HOME": "/tmp/codex"}); err != nil {
		t.Fatalf("safe environment rejected: %v", err)
	}
	if err := validateCommandEnvironment(map[string]string{"GITHUB_TOKEN": "secret"}); err == nil {
		t.Fatal("credential environment variable was accepted")
	}
}
