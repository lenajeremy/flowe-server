package handlers

import (
	"context"
	"errors"
	"fmt"
	stdhtml "html"
	"log/slog"
	"net/mail"
	"os"
	"strings"
	"time"

	"workflow-ai/server/internal/database/models"
	"workflow-ai/server/internal/email"
	"workflow-ai/server/internal/telemetry"

	"github.com/resend/resend-go/v2"
	"gorm.io/gorm"
)

// Signup notifications tell the operators that an account was created. They are
// deliberately not part of the signup transaction: a person who just signed up
// must not see an error, or be held up, because an internal mail failed.

// signupNotifyRecipients reads SIGNUP_NOTIFY_EMAIL, a comma-separated list, in
// the same shape as FEEDBACK_TO_EMAIL. An empty value turns the feature off
// rather than erroring — most deployments will not want these.
func signupNotifyRecipients() ([]string, error) {
	raw := strings.TrimSpace(os.Getenv("SIGNUP_NOTIFY_EMAIL"))
	if raw == "" {
		return nil, nil
	}
	addresses, err := mail.ParseAddressList(raw)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("SIGNUP_NOTIFY_EMAIL is invalid")
	}
	recipients := make([]string, 0, len(addresses))
	for _, address := range addresses {
		recipients = append(recipients, address.Address)
	}
	return recipients, nil
}

// notifyNewSignup mails the operators about one new account.
//
// Fire-and-forget by design, and on its own context: the request context is
// cancelled the moment the signup response is written, so reusing it would
// cancel this mail most of the time. Every failure is logged and swallowed —
// an operator notification is never worth failing somebody's signup over.
func (h *WorkflowHandler) notifyNewSignup(user *models.User, method string) {
	recipients, err := signupNotifyRecipients()
	if err != nil {
		slog.Error("signup notification not sent", "error", err)
		return
	}
	if len(recipients) == 0 {
		return // not configured — the normal case
	}
	apiKey := strings.TrimSpace(os.Getenv("RESEND_API_KEY"))
	if apiKey == "" {
		slog.Warn("signup notification not sent: RESEND_API_KEY is not configured")
		return
	}

	// Snapshot what the mail needs before returning: the caller may mutate or
	// reuse the user after this, and the goroutine outlives the request.
	snapshot := struct {
		Email, Name, ID string
		CreatedAt       time.Time
	}{user.Email, strings.TrimSpace(user.Name), user.ID.String(), user.CreatedAt}

	db := h.db.DB
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// A running total turns each mail into a growth signal rather than an
		// isolated fact. It is a nice-to-have, so a failure here still sends.
		var total int64
		if err := db.Model(&models.User{}).Count(&total).Error; err != nil {
			slog.WarnContext(ctx, "signup notification: user count failed", "error", err)
			total = 0
		}

		name := snapshot.Name
		if name == "" {
			name = "(no name given)"
		}
		when := snapshot.CreatedAt
		if when.IsZero() {
			when = time.Now()
		}

		subject := "New signup: " + snapshot.Email
		lines := []string{
			"Email:  " + snapshot.Email,
			"Name:   " + name,
			"Via:    " + method,
			"When:   " + when.UTC().Format("2006-01-02 15:04 MST"),
			"User:   " + snapshot.ID,
		}
		if total > 0 {
			lines = append(lines, fmt.Sprintf("Total:  %d account%s", total, plural(total)))
		}
		textBody := strings.Join(lines, "\n")

		var b strings.Builder
		b.WriteString(`<table style="border-collapse:collapse;font-size:14px">`)
		rows := [][2]string{
			{"Email", snapshot.Email},
			{"Name", name},
			{"Via", method},
			{"When", when.UTC().Format("2006-01-02 15:04 MST")},
			{"User ID", snapshot.ID},
		}
		if total > 0 {
			rows = append(rows, [2]string{"Total accounts", fmt.Sprintf("%d", total)})
		}
		for _, row := range rows {
			b.WriteString(`<tr><td style="padding:4px 16px 4px 0;opacity:.7">` +
				stdhtml.EscapeString(row[0]) + `</td><td style="padding:4px 0">` +
				stdhtml.EscapeString(row[1]) + `</td></tr>`)
		}
		b.WriteString(`</table>`)

		htmlBody := email.WrapBranded(
			`<h2 style="margin-top:0">New signup</h2>`+b.String(),
			snapshot.Email+" just created an account",
		)

		client := resend.NewClient(apiKey)
		_, mailErr := client.Emails.Send(&resend.SendEmailRequest{
			From:    email.FromAddress(),
			To:      recipients,
			Subject: subject,
			Text:    textBody,
			Html:    htmlBody,
		})
		telemetry.EmailSent(ctx, "signup_notification", mailErr)
		if mailErr != nil {
			slog.ErrorContext(ctx, "signup notification failed to send",
				"error", mailErr, "user_id", snapshot.ID)
			return
		}
		slog.InfoContext(ctx, "signup notification sent",
			"user_id", snapshot.ID, "via", method, "recipients", len(recipients))
	}()
}

func plural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// countUsers exists so tests can assert the query the notifier relies on keeps
// working against the real schema.
func countUsers(db *gorm.DB) (int64, error) {
	var total int64
	err := db.Model(&models.User{}).Count(&total).Error
	return total, err
}
