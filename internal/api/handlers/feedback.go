package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"image"
	_ "image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/mail"
	"os"
	"regexp"
	"strings"
	"time"

	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/database/models"
	platformmail "workflow-ai/server/internal/email"
	"workflow-ai/server/internal/telemetry"
	"workflow-ai/server/internal/tenancy"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/resend/resend-go/v2"
)

const (
	feedbackScreenshotField = "screenshot"
	maxFeedbackImageBytes   = 8 << 20
	maxFeedbackMessageRunes = 4000
	maxFeedbackPageRunes    = 2048
	maxFeedbackImageSide    = 8192
)

var feedbackViewportPattern = regexp.MustCompile(`^[1-9][0-9]{1,4}x[1-9][0-9]{1,4}$`)

type feedbackSubmission struct {
	Message    string
	Page       string
	Viewport   string
	Screenshot []byte
	Width      int
	Height     int
	User       models.User
	Org        models.Organization
}

// feedbackEmailSender is a narrow seam for testing the authenticated handler
// without teaching the rest of WorkflowHandler about an email provider. The
// production implementation remains server-owned so neither the destination nor
// the Resend credential is ever exposed to the browser.
var feedbackEmailSender = sendFeedbackEmail

// SubmitFeedback accepts an annotated PNG of the current app and delivers it to
// the configured product-feedback inbox. It is authenticated and rate-limited:
// screenshots can contain private workflow data, and an anonymous attachment
// relay would quickly become an abuse vector.
func (h *WorkflowHandler) SubmitFeedback(c *gin.Context) {
	userID := auth.UserID(c)
	if h.redis != nil && !auth.Allow(c.Request.Context(), h.redis, "rl:feedback:"+userID, 5, time.Hour) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many feedback reports — try again later"})
		return
	}

	if _, err := feedbackRecipients(); err != nil || strings.TrimSpace(os.Getenv("RESEND_API_KEY")) == "" {
		if err != nil {
			slog.ErrorContext(c.Request.Context(), "feedback recipient is not configured", "error", err)
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "feedback email is not configured"})
		return
	}

	// BodyLimit already caps the entire request at 10 MiB. Limit multipart's
	// in-memory allowance too, then independently cap the actual file below.
	if err := c.Request.ParseMultipartForm(maxFeedbackImageBytes); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "too large") {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "screenshot is too large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid feedback submission"})
		return
	}

	file, header, err := c.Request.FormFile(feedbackScreenshotField)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "an annotated screenshot is required"})
		return
	}
	defer file.Close()

	screenshot, width, height, err := readFeedbackScreenshot(file, header)
	if err != nil {
		var tooLarge *feedbackImageTooLargeError
		if errors.As(err, &tooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	message, err := boundedFeedbackText(c.PostForm("message"), maxFeedbackMessageRunes, "message")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	page, err := boundedFeedbackText(c.PostForm("page"), maxFeedbackPageRunes, "page path")
	if err != nil || page == "" || !strings.HasPrefix(page, "/") || strings.HasPrefix(page, "//") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page path must be a valid in-app path"})
		return
	}
	viewport := strings.TrimSpace(c.PostForm("viewport"))
	if !feedbackViewportPattern.MatchString(viewport) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "viewport must look like 1440x900"})
		return
	}

	var user models.User
	if err := h.db.DB.Where("id = ?", userID).First(&user).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not identify the feedback sender"})
		return
	}
	// The org id is cached on the session at sign-in, and removing someone from
	// an org does not invalidate their existing sessions. Without this check a
	// former member's still-valid token would keep sending feedback stamped with
	// a workspace they no longer belong to.
	if !tenancy.IsMember(h.db.DB, currentOrgID(c), userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "you are no longer a member of this workspace"})
		return
	}
	var org models.Organization
	if err := h.db.DB.Where("id = ?", currentOrgID(c)).First(&org).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not identify the feedback workspace"})
		return
	}

	submission := feedbackSubmission{
		Message: message, Page: page, Viewport: viewport,
		Screenshot: screenshot, Width: width, Height: height,
		User: user, Org: org,
	}
	if err := feedbackEmailSender(c.Request.Context(), submission); err != nil {
		slog.ErrorContext(c.Request.Context(), "feedback email failed", "error", err, "user_id", userID, "org_id", currentOrgID(c))
		telemetry.EmailSent(c.Request.Context(), "product_feedback", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "could not send feedback — please try again"})
		return
	}

	telemetry.EmailSent(c.Request.Context(), "product_feedback", nil)
	c.JSON(http.StatusCreated, gin.H{"ok": true})
}

type feedbackImageTooLargeError struct{ reason string }

func (e *feedbackImageTooLargeError) Error() string { return e.reason }

func readFeedbackScreenshot(file multipart.File, header *multipart.FileHeader) ([]byte, int, int, error) {
	if header == nil {
		return nil, 0, 0, errors.New("an annotated screenshot is required")
	}
	limited := io.LimitReader(file, maxFeedbackImageBytes+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, 0, 0, errors.New("could not read screenshot")
	}
	if len(payload) == 0 {
		return nil, 0, 0, errors.New("screenshot is empty")
	}
	if len(payload) > maxFeedbackImageBytes {
		return nil, 0, 0, &feedbackImageTooLargeError{reason: "screenshot must be smaller than 8 MiB"}
	}
	if contentType := http.DetectContentType(payload); contentType != "image/png" {
		return nil, 0, 0, errors.New("screenshot must be a PNG image")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(payload))
	if err != nil || format != "png" || config.Width < 1 || config.Height < 1 {
		return nil, 0, 0, errors.New("screenshot is not a valid PNG image")
	}
	if config.Width > maxFeedbackImageSide || config.Height > maxFeedbackImageSide {
		return nil, 0, 0, &feedbackImageTooLargeError{reason: "screenshot dimensions are too large"}
	}
	return payload, config.Width, config.Height, nil
}

func boundedFeedbackText(value string, maxRunes int, label string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\x00", ""))
	if len([]rune(value)) > maxRunes {
		return "", fmt.Errorf("%s is too long", label)
	}
	return value, nil
}

func feedbackRecipients() ([]string, error) {
	raw := strings.TrimSpace(os.Getenv("FEEDBACK_TO_EMAIL"))
	addresses, err := mail.ParseAddressList(raw)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("FEEDBACK_TO_EMAIL is invalid")
	}
	recipients := make([]string, 0, len(addresses))
	for _, address := range addresses {
		recipients = append(recipients, address.Address)
	}
	return recipients, nil
}

func sendFeedbackEmail(ctx context.Context, submission feedbackSubmission) error {
	recipients, err := feedbackRecipients()
	if err != nil {
		return err
	}
	apiKey := strings.TrimSpace(os.Getenv("RESEND_API_KEY"))
	if apiKey == "" {
		return errors.New("RESEND_API_KEY is not configured")
	}

	senderName := strings.TrimSpace(submission.User.Name)
	if senderName == "" {
		senderName = submission.User.Email
	}
	subjectName := strings.NewReplacer("\r", " ", "\n", " ").Replace(senderName)
	subject := "Fernary feedback from " + subjectName
	message := submission.Message
	if message == "" {
		message = "No additional note was provided."
	}

	text := fmt.Sprintf(`New Fernary product feedback

From: %s <%s>
Workspace: %s
Page: %s
Viewport: %s
Screenshot: %dx%d

%s`, senderName, submission.User.Email, submission.Org.Name, submission.Page,
		submission.Viewport, submission.Width, submission.Height, message)

	inner := fmt.Sprintf(`<h2 style="margin-top:0">New product feedback</h2>
<p style="color:%s;margin:0 0 18px">Reply to this email to follow up with the person who sent it.</p>
<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="width:100%%;margin:0 0 20px;font-size:13px">
<tr><td style="color:%s;padding:4px 14px 4px 0;width:86px">From</td><td>%s &lt;%s&gt;</td></tr>
<tr><td style="color:%s;padding:4px 14px 4px 0">Workspace</td><td>%s</td></tr>
<tr><td style="color:%s;padding:4px 14px 4px 0">Page</td><td><code>%s</code></td></tr>
<tr><td style="color:%s;padding:4px 14px 4px 0">Viewport</td><td>%s · %d×%d capture</td></tr>
</table>
<div style="border-left:3px solid %s;padding:2px 0 2px 14px;margin:0 0 22px;white-space:pre-wrap">%s</div>
<img src="cid:feedback-screenshot" alt="Annotated Fernary screenshot" style="display:block;width:100%%;height:auto;border:1px solid %s;border-radius:10px" />`,
		platformmail.Muted,
		platformmail.Muted, html.EscapeString(senderName), html.EscapeString(submission.User.Email),
		platformmail.Muted, html.EscapeString(submission.Org.Name),
		platformmail.Muted, html.EscapeString(submission.Page),
		platformmail.Muted, html.EscapeString(submission.Viewport), submission.Width, submission.Height,
		platformmail.Accent, html.EscapeString(message), platformmail.Rule)

	client := resend.NewClient(apiKey)
	_, err = client.Emails.SendWithOptions(ctx, &resend.SendEmailRequest{
		From:    platformmail.FromAddress(),
		To:      recipients,
		ReplyTo: submission.User.Email,
		Subject: subject,
		Text:    text,
		Html:    platformmail.WrapBranded(inner, subject),
		Attachments: []*resend.Attachment{{
			Content: submission.Screenshot, Filename: "fernary-feedback.png",
			ContentType: "image/png", ContentId: "feedback-screenshot",
		}},
	}, &resend.SendEmailOptions{IdempotencyKey: "feedback-" + uuid.NewString()})
	return err
}
