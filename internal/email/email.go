// Package email renders outgoing mail as styled HTML. Every email the
// platform sends goes through a template so nothing leaves as raw text.
//
// Two shells are offered:
//
//   - WrapBrandless — a clean, neutral, light card with no mention of Flowe.
//     Used for mail the *user's workflow* sends (the Email node), whose author
//     is the sender, not us.
//   - WrapBranded — Flowe's own dark, branded card. Used for platform mail
//     (sign-in codes, approval requests).
//
// Email-node bodies are authored as Markdown, converted with RenderMarkdown,
// then dropped into WrapBrandless.
package email

import (
	stdhtml "html"
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

var darkPalette = palette{
	bodyBg: "#0D0D11", cardBg: "#16161C", cardBorder: "#26262E",
	text: "#c7ccd1", heading: "#ffffff", muted: "#667179", link: "#a08cff",
	codeBg: "#22222b", code: "#d7b8ff", preBg: "#0a0a0d", pre: "#c7ccd1", rule: "#26262E",
}

// WrapBrandless wraps an HTML fragment in the neutral, unbranded shell.
// preview is the inbox preheader text (usually the subject).
func WrapBrandless(contentHTML, preview string) string {
	return shell(lightPalette, contentHTML, preview)
}

// WrapBranded wraps an HTML fragment in Flowe's branded shell (wordmark
// header + footer). preview is the inbox preheader text.
func WrapBranded(contentHTML, preview string) string {
	p := darkPalette
	p.header = `<div style="text-align:center;margin:0 0 28px"><span style="font-size:19px;font-weight:700;letter-spacing:-0.02em;color:#ffffff">Flowe</span></div>`
	p.footer = `<p style="color:#667179;font-size:11px;text-align:center;margin:28px 0 0;line-height:1.5">Sent by Flowe · Automation for everyone</p>`
	return shell(p, contentHTML, preview)
}

// Button renders a branded pill call-to-action link for use inside branded
// email content. label and url are escaped.
func Button(url, label string) string {
	return `<div style="text-align:center;margin:28px 0 4px">` +
		`<a href="` + stdhtml.EscapeString(url) + `" style="display:inline-block;background:#a08cff;color:#0a0a0d;font-size:14px;font-weight:600;text-decoration:none;padding:11px 26px;border-radius:999px">` +
		stdhtml.EscapeString(label) + `</a></div>`
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
.email-content{color:__TEXT__;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;font-size:15px;line-height:1.6;word-break:break-word}
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
.email-content code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:13px;background:__CODE_BG__;color:__CODE__;padding:2px 6px;border-radius:5px}
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
<tr><td style="background:__CARD_BG__;border:1px solid __CARD_BORDER__;border-radius:14px;padding:32px">
__HEADER__
<div class="email-content">__CONTENT__</div>
__FOOTER__
</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`
