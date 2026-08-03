// Package email renders outgoing mail as styled HTML. Every email the
// platform sends goes through a template so nothing leaves as raw text.
//
// Two shells are offered:
//
//   - WrapBrandless — a clean, neutral, light card with no mention of Fernary.
//     Used for mail the *user's workflow* sends (the Email node), whose author
//     is the sender, not us.
//   - WrapBranded — Fernary's own dark, branded card. Used for platform mail
//     (sign-in codes, approval requests).
//
// Email-node bodies are authored as Markdown, converted with RenderMarkdown,
// then dropped into WrapBrandless.
package email

import (
	stdhtml "html"
	"os"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

// md converts GitHub-flavoured Markdown to HTML. Raw HTML in the source is
// escaped (the default) — email content may originate from upstream node
// output we don't control, so we never let it inject markup. Single newlines
// become <br> because email bodies are typically written line-by-line.
var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(gmhtml.WithHardWraps()),
)

// RenderMarkdown converts a Markdown string to an HTML fragment (no wrapper).
// On the rare conversion error it falls back to escaped, line-broken text so
// we still never emit raw Markdown.
func RenderMarkdown(src string) string {
	var buf strings.Builder
	if err := md.Convert([]byte(src), &buf); err != nil {
		return "<p>" + strings.ReplaceAll(stdhtml.EscapeString(src), "\n", "<br>") + "</p>"
	}
	return buf.String()
}

// palette holds the handful of colours a shell needs. The markdown element
// styles below are driven entirely by these, so the two shells share one
// stylesheet template.
type palette struct {
	bodyBg, cardBg, cardBorder     string
	text, heading, muted, link     string
	codeBg, code, preBg, pre, rule string
	header, footer                 string // pre-rendered HTML, may be empty
}

var lightPalette = palette{
	bodyBg: "#f4f4f5", cardBg: "#ffffff", cardBorder: "#e4e4e7",
	text: "#3f3f46", heading: "#18181b", muted: "#71717a", link: "#2563eb",
	codeBg: "#f4f4f5", code: "#be185d", preBg: "#1e1e24", pre: "#e4e4e7", rule: "#e4e4e7",
}

// The branded shell is a Fernary brand surface, so it uses Forest — BRAND.md's
// designated near-black green for dark brand surfaces — rather than the app's
// neutral canvas. Link and code colours are the frond hues, brightened for
// legibility on a dark card. (These were violet before the Flowe → Fernary
// rebrand; the sweep for leftover purple missed the server, since it only
// looked at the frontend.)
var darkPalette = palette{
	bodyBg: "#0A1512", cardBg: "#0F1B18", cardBorder: "#1D2C27",
	text: "#C7D2CD", heading: "#FFFFFF", muted: "#8A9A94", link: "#3DD68C",
	codeBg: "#16241F", code: "#9AE06A", preBg: "#0A1512", pre: "#C7D2CD", rule: "#1D2C27",
}

// fontStack is applied to the card container as well as .email-content, because
// the header and footer are injected outside that div and would otherwise render
// in the client's default serif. Google Sans leads it so mail matches the app on
// the few clients that have the face; everything after it is the fallback that
// actually renders in practice.
// Single-quoted family names, not double. This string is interpolated into a
// style="..." attribute, and a double quote there closes the attribute early —
// which silently drops the font and everything after it. Single quotes are valid
// CSS in both a <style> block and an inline attribute.
const fontStack = `'Google Sans',-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif`

// accent is the brand emerald. Buttons take dark ink on it — BRAND.md's rule,
// and the only combination that clears contrast on a colour this bright.
const (
	accent    = "#16C08A"
	accentInk = "#0A1512"
)

// Exported slices of the branded palette, for callers that compose their own
// markup inside a branded shell (the sign-in code slab, for one). They exist so
// those call sites can't quietly drift from the shell wrapping them — which is
// exactly how the old violet hex codes survived the rebrand.
const (
	Heading    = "#FFFFFF" // darkPalette.heading
	Muted      = "#8A9A94" // darkPalette.muted
	Rule       = "#1D2C27" // darkPalette.cardBorder / rule
	CodeSlabBg = "#16241F" // darkPalette.codeBg — a lifted panel on the card
	Accent     = accent
	AccentInk  = accentInk
)

// LogoURL is where the mark is fetched from in branded mail.
//
// It must be an absolute, publicly reachable **PNG**: Gmail strips SVG entirely,
// and a relative path has nothing to resolve against in an inbox. It also has to
// come from the configured FRONTEND_URL rather than the request Origin — a
// sign-in triggered from localhost would otherwise mail a localhost image that
// only renders for the person who sent it.
func LogoURL() string {
	base := strings.TrimRight(strings.TrimSpace(strings.Split(os.Getenv("FRONTEND_URL"), ",")[0]), "/")
	if base == "" || strings.Contains(base, "localhost") || strings.Contains(base, "127.0.0.1") {
		// No usable public base — fall back to the canonical domain so mail sent
		// from a dev box still shows the right mark to whoever receives it.
		base = "https://fernary.com"
	}
	return base + "/email-logo.png"
}

// FromAddress is the sender for platform mail, from AUTH_FROM_EMAIL.
//
// The default is still the old usecelery.io domain on purpose: it is the only
// domain currently verified in Resend, and switching to fernary.com before that
// verification exists would make every platform email fail to send rather than
// merely look inconsistent. Set AUTH_FROM_EMAIL to
// "Fernary <noreply@fernary.com>" the moment the domain is verified.
func FromAddress() string {
	if v := strings.TrimSpace(os.Getenv("AUTH_FROM_EMAIL")); v != "" {
		return v
	}
	return "Fernary <noreply@usecelery.io>"
}

// WrapBrandless wraps an HTML fragment in the neutral, unbranded shell.
// preview is the inbox preheader text (usually the subject).
func WrapBrandless(contentHTML, preview string) string {
	return shell(lightPalette, contentHTML, preview)
}

// WrapBranded wraps an HTML fragment in Fernary's branded shell (the stacked
// lockup as a header, plus a footer). preview is the inbox preheader text.
func WrapBranded(contentHTML, preview string) string {
	p := darkPalette
	p.header = brandedHeader()
	p.footer = `<p style="color:` + p.muted + `;font-size:11px;text-align:center;margin:28px 0 0;line-height:1.55">` +
		`Sent by Fernary — the automation system for AI you can actually leave running.</p>`
	return shell(p, contentHTML, preview)
}

// brandedHeader is the stacked lockup: mark above wordmark.
//
// The wordmark stays as live text on purpose. Most clients block remote images
// by default, so an image-only header would leave a blank, anonymous email at
// exactly the moment the recipient is deciding whether to trust it. With text
// underneath, a blocked image degrades to a plain wordmark and the mail still
// identifies itself. width/height are set explicitly because the file is 2x, and
// several clients ignore CSS sizing on images.
func brandedHeader() string {
	return `<div style="text-align:center;margin:0 0 30px">` +
		`<img src="` + LogoURL() + `" width="48" height="48" alt="Fernary"` +
		` style="display:block;margin:0 auto 10px;width:48px;height:48px;border:0;outline:none;text-decoration:none">` +
		`<div style="font-size:19px;font-weight:700;letter-spacing:-0.02em;color:#FFFFFF;line-height:1">Fernary</div>` +
		`</div>`
}

// Button renders a branded pill call-to-action link for use inside branded
// email content. label and url are escaped.
//
// Wrapped in Outlook's conditional VML because Outlook on Windows ignores
// border-radius and padding on an anchor, which turns the pill into bare
// underlined text. Every other client sees only the anchor.
func Button(url, label string) string {
	href := stdhtml.EscapeString(url)
	text := stdhtml.EscapeString(label)
	return `<div style="text-align:center;margin:28px 0 4px">` +
		`<!--[if mso]><v:roundrect xmlns:v="urn:schemas-microsoft-com:vml" xmlns:w="urn:schemas-microsoft-com:office:word" href="` + href + `" style="height:42px;v-text-anchor:middle;width:220px" arcsize="50%" stroke="f" fillcolor="` + accent + `"><w:anchorlock/><center style="color:` + accentInk + `;font-family:sans-serif;font-size:14px;font-weight:600">` + text + `</center></v:roundrect><![endif]-->` +
		`<!--[if !mso]><!-- --><a href="` + href + `" style="display:inline-block;background:` + accent + `;color:` + accentInk + `;font-size:14px;font-weight:600;text-decoration:none;padding:12px 28px;border-radius:999px">` + text + `</a><!--<![endif]-->` +
		`</div>`
}

// Action renders a branded call-to-action email: a heading, a message, an
// optional quoted content block, and a button. All text is escaped, so it is
// safe to pass arbitrary node output as content. Returns a full HTML document.
func Action(heading, message, content, actionURL, actionLabel, preview string) string {
	var b strings.Builder
	b.WriteString(`<h2 style="margin-top:0">` + stdhtml.EscapeString(heading) + `</h2>`)
	if message != "" {
		b.WriteString(`<p>` + brk(stdhtml.EscapeString(message)) + `</p>`)
	}
	if content != "" {
		b.WriteString(`<blockquote>` + brk(stdhtml.EscapeString(content)) + `</blockquote>`)
	}
	if actionURL != "" {
		b.WriteString(Button(actionURL, actionLabel))
	}
	return WrapBranded(b.String(), preview)
}

func brk(s string) string { return strings.ReplaceAll(s, "\n", "<br>") }

func shell(p palette, contentHTML, preview string) string {
	r := strings.NewReplacer(
		"__BODY_BG__", p.bodyBg,
		"__CARD_BG__", p.cardBg,
		"__CARD_BORDER__", p.cardBorder,
		"__TEXT__", p.text,
		"__HEADING__", p.heading,
		"__MUTED__", p.muted,
		"__LINK__", p.link,
		"__CODE_BG__", p.codeBg,
		"__CODE__", p.code,
		"__PRE_BG__", p.preBg,
		"__PRE__", p.pre,
		"__RULE__", p.rule,
		"__PREVIEW__", stdhtml.EscapeString(preview),
		"__HEADER__", p.header,
		"__FOOTER__", p.footer,
		"__CONTENT__", contentHTML,
		"__FONT__", fontStack,
	)
	return r.Replace(shellTemplate)
}

const shellTemplate = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark">
<style>
.email-content{color:__TEXT__;font-family:__FONT__;font-size:15px;line-height:1.6;word-break:break-word}
.email-content p{margin:0 0 16px}
.email-content h1,.email-content h2,.email-content h3,.email-content h4{color:__HEADING__;font-weight:600;line-height:1.3;margin:24px 0 12px}
.email-content h1{font-size:24px}
.email-content h2{font-size:20px}
.email-content h3{font-size:17px}
.email-content h4{font-size:15px}
.email-content a{color:__LINK__;text-decoration:underline}
.email-content strong{color:__HEADING__;font-weight:600}
.email-content ul,.email-content ol{margin:0 0 16px;padding-left:22px}
.email-content li{margin:4px 0}
.email-content code{font-family:'Google Sans Code',ui-monospace,SFMono-Regular,Menlo,monospace;font-size:13px;background:__CODE_BG__;color:__CODE__;padding:2px 6px;border-radius:5px}
.email-content pre{background:__PRE_BG__;color:__PRE__;padding:16px;border-radius:10px;overflow-x:auto;margin:0 0 16px}
.email-content pre code{background:transparent;color:inherit;padding:0}
.email-content blockquote{margin:0 0 16px;padding:4px 0 4px 16px;border-left:3px solid __RULE__;color:__MUTED__}
.email-content hr{border:0;border-top:1px solid __RULE__;margin:24px 0}
.email-content img{max-width:100%;height:auto;border-radius:8px}
.email-content table{border-collapse:collapse;width:100%;margin:0 0 16px;font-size:14px}
.email-content th,.email-content td{border:1px solid __RULE__;padding:8px 12px;text-align:left}
.email-content th{background:__CODE_BG__;color:__HEADING__;font-weight:600}
.email-content>*:first-child{margin-top:0}
.email-content>*:last-child{margin-bottom:0}
</style>
</head>
<body style="margin:0;padding:0;background:__BODY_BG__">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;color:transparent">__PREVIEW__</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:__BODY_BG__">
<tr><td align="center" style="padding:32px 16px">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:600px;margin:0 auto">
<tr><td style="background:__CARD_BG__;border:1px solid __CARD_BORDER__;border-radius:14px;padding:32px;font-family:__FONT__">
__HEADER__
<div class="email-content">__CONTENT__</div>
__FOOTER__
</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`
