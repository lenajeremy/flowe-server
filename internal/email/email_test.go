package email

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Email is the one surface we can't inspect after the fact — it lands in
// someone's inbox and there is no console to check. So the brand rules that
// matter are asserted here rather than eyeballed once and hoped about.

func TestBrandedShellHasNoPreRebrandPurple(t *testing.T) {
	html := WrapBranded("<p>hello</p>", "preview")
	// The exact violets that survived the Flowe → Fernary rename in this file.
	for _, dead := range []string{"#a08cff", "#d7b8ff", "#22222b", "#0D0D11", "#16161C"} {
		if strings.Contains(strings.ToLower(html), strings.ToLower(dead)) {
			t.Errorf("branded shell still contains pre-rebrand colour %s", dead)
		}
	}
	if !strings.Contains(html, "#0A1512") {
		t.Error("branded shell should sit on Forest (#0A1512), BRAND.md's dark brand surface")
	}
}

// A double quote inside a style="..." attribute closes the attribute early, so
// the declaration and everything after it is silently dropped. That is how the
// header and footer ended up rendering in the client's default serif: the font
// stack was interpolated into the card's inline style with "Google Sans" in
// double quotes. Nothing errors; the type just goes wrong in the inbox.
func TestInlineStylesNeverContainDoubleQuotes(t *testing.T) {
	attr := regexp.MustCompile(`style="([^"]*)"`)
	for name, html := range map[string]string{
		"branded":   WrapBranded("<p>x</p>", "p"),
		"brandless": WrapBrandless("<p>x</p>", "p"),
		"button":    Button("https://x.test", "Go"),
	} {
		for _, m := range attr.FindAllStringSubmatch(html, -1) {
			// A truncated attribute leaves a dangling fragment: the declaration
			// list ends mid-property. Catch the specific shape that bit us.
			if strings.HasSuffix(strings.TrimSpace(m[1]), "font-family:") {
				t.Errorf("%s: style attribute ends at an empty font-family — a quote closed it early: %q", name, m[1])
			}
		}
		if strings.Contains(html, `font-family:"`) {
			t.Errorf("%s: font-family uses double quotes; use single quotes so inline styles survive", name)
		}
	}
	if strings.Contains(fontStack, `"`) {
		t.Error("fontStack must use single-quoted family names — it is interpolated into a style attribute")
	}
}

func TestBrandedHeaderCarriesLogoAndWordmark(t *testing.T) {
	html := WrapBranded("<p>hello</p>", "preview")

	img := regexp.MustCompile(`<img src="(https://[^"]+\.png)"[^>]*alt="Fernary"`)
	m := img.FindStringSubmatch(html)
	if m == nil {
		t.Fatal("no absolute https PNG logo with alt text in the branded header")
	}
	if strings.Contains(m[1], "localhost") || strings.Contains(m[1], "127.0.0.1") {
		t.Errorf("logo URL %q is not reachable from an inbox", m[1])
	}
	if !strings.Contains(html, `width="48"`) || !strings.Contains(html, `height="48"`) {
		t.Error("logo needs explicit width/height attributes — several clients ignore CSS sizing")
	}
	// Clients block remote images by default; the mail must still name itself.
	if !strings.Contains(html, ">Fernary</div>") {
		t.Error("wordmark must be live text so a blocked image still leaves the mail identifiable")
	}
}

func TestLogoURLPrefersConfiguredFrontendAndRejectsLocalhost(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"", "https://fernary.com/email-logo.png"},
		{"http://localhost:5173", "https://fernary.com/email-logo.png"},
		{"http://127.0.0.1:4719", "https://fernary.com/email-logo.png"},
		{"https://app.fernary.com/", "https://app.fernary.com/email-logo.png"},
		{"https://app.fernary.com,https://other.example", "https://app.fernary.com/email-logo.png"},
	} {
		t.Setenv("FRONTEND_URL", tc.env)
		if got := LogoURL(); got != tc.want {
			t.Errorf("FRONTEND_URL=%q → LogoURL() = %q, want %q", tc.env, got, tc.want)
		}
	}
}

func TestButtonSurvivesOutlook(t *testing.T) {
	b := Button("https://fernary.com/run/x?a=1&b=2", "Review & respond")
	if !strings.Contains(b, "v:roundrect") || !strings.Contains(b, "[if mso]") {
		t.Error("no VML fallback — Outlook drops border-radius and padding, leaving bare text")
	}
	if !strings.Contains(b, "[if !mso]") {
		t.Error("the anchor must be hidden from Outlook or the button renders twice")
	}
	// Both branches must escape, or a crafted label/URL breaks out of the markup.
	if strings.Count(b, "Review &amp; respond") != 2 {
		t.Errorf("label not escaped in both branches: %s", b)
	}
	if strings.Contains(b, "a=1&b=2") {
		t.Error("URL ampersand not escaped")
	}
}

func TestBrandlessShellStaysUnbranded(t *testing.T) {
	html := WrapBrandless("<p>a workflow's own message</p>", "preview")
	// Mail the user's workflow sends is theirs; our name and mark stay out of it.
	for _, ours := range []string{"Fernary", "email-logo.png", accent} {
		if strings.Contains(html, ours) {
			t.Errorf("brandless shell leaked %q — the sender is the user, not us", ours)
		}
	}
}

func TestActionEscapesUntrustedContent(t *testing.T) {
	// content is arbitrary upstream node output — it must never become markup.
	html := Action("Action required", "msg", `<script>alert(1)</script>`, "https://x.test", "Go", "p")
	if strings.Contains(html, "<script>") {
		t.Error("node output was injected as live markup")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("expected the script tag to appear escaped")
	}
}

// TestWritePreviews renders the three templates to disk for visual review.
// Opt-in, because a test that writes files on every run is a nuisance:
//
//	MAIL_PREVIEW_DIR=/tmp/mailprev go test ./internal/email/ -run Previews
func TestWritePreviews(t *testing.T) {
	dir := os.Getenv("MAIL_PREVIEW_DIR")
	if dir == "" {
		t.Skip("set MAIL_PREVIEW_DIR to write email previews")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	code, link := "418 902", "https://fernary.com/auth/verify?token=demo"
	signin := fmt.Sprintf(`<h2 style="margin-top:0;text-align:center">Sign in to Fernary</h2>
<p style="text-align:center;color:%s;font-size:13px;margin:0 0 22px">Enter this code — it expires in 10 minutes.</p>
<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="width:auto;margin:0 auto 4px"><tr>
<td align="center" style="background:%s;border:1px solid %s;border-radius:10px;padding:14px 26px;text-align:center">
<span style="color:%s;font-size:30px;font-weight:600;font-family:'Google Sans Code',ui-monospace,SFMono-Regular,Menlo,monospace;letter-spacing:9px">%s</span>
</td></tr></table>
%s
<p style="text-align:center;color:%s;font-size:11px;margin:26px 0 0">If you didn't request this, you can safely ignore this email — nobody can sign in without the code above.</p>`,
		Muted, CodeSlabBg, Rule, Heading, code, Button(link, "Sign in instantly"), Muted)

	files := map[string]string{
		"signin.html": WrapBranded(signin, "Your Fernary sign-in code"),
		"approval.html": Action("Action required",
			"Post this release note to the team channel?",
			"@shopify/shopify-api@9.7.1\nPublished 2024-04-08T16:21:53Z\nhttps://github.com/Shopify/shopify-api-js/releases",
			"https://fernary.com/run/demo", "Review & respond", "Review before posting"),
		"brandless.html": WrapBrandless(RenderMarkdown(
			"## Weekly digest\n\nThree orders are still unshipped after 3 days:\n\n"+
				"- #1041 — Dublin\n- #1043 — Cork\n- #1048 — Galway\n\nRun `store ops` to chase them."),
			"Weekly digest"),
	}
	for name, html := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(html), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", filepath.Join(dir, name))
	}
}
