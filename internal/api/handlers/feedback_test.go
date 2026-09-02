package handlers

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"workflow-ai/server/internal/database"
	"workflow-ai/server/internal/database/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type feedbackMultipartFile struct{ *bytes.Reader }

func (feedbackMultipartFile) Close() error { return nil }

func feedbackPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 20, G: 184, B: 134, A: 255})
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func feedbackHeader(filename, contentType string, size int) *multipart.FileHeader {
	return &multipart.FileHeader{
		Filename: filename,
		Size:     int64(size),
		Header: textproto.MIMEHeader{
			"Content-Disposition": {`form-data; name="screenshot"; filename="` + filename + `"`},
			"Content-Type":        {contentType},
		},
	}
}

func TestReadFeedbackScreenshotAcceptsARealPNG(t *testing.T) {
	payload := feedbackPNG(t, 1440, 900)
	got, width, height, err := readFeedbackScreenshot(
		feedbackMultipartFile{bytes.NewReader(payload)}, feedbackHeader("capture.png", "image/png", len(payload)),
	)
	if err != nil {
		t.Fatalf("read screenshot: %v", err)
	}
	if !bytes.Equal(got, payload) || width != 1440 || height != 900 {
		t.Fatalf("screenshot = %d bytes, %dx%d", len(got), width, height)
	}
}

func TestReadFeedbackScreenshotChecksBytesNotClaimedContentType(t *testing.T) {
	payload := []byte("<svg onload=alert(1)></svg>")
	_, _, _, err := readFeedbackScreenshot(
		feedbackMultipartFile{bytes.NewReader(payload)}, feedbackHeader("capture.png", "image/png", len(payload)),
	)
	if err == nil || !strings.Contains(err.Error(), "PNG") {
		t.Fatalf("spoofed image returned %v", err)
	}
}

func TestReadFeedbackScreenshotRejectsOversizedDimensions(t *testing.T) {
	payload := feedbackPNG(t, maxFeedbackImageSide+1, 1)
	_, _, _, err := readFeedbackScreenshot(
		feedbackMultipartFile{bytes.NewReader(payload)}, feedbackHeader("wide.png", "image/png", len(payload)),
	)
	if err == nil || !strings.Contains(err.Error(), "dimensions") {
		t.Fatalf("oversized dimensions returned %v", err)
	}
}

func TestFeedbackRecipientsAreValidated(t *testing.T) {
	t.Setenv("FEEDBACK_TO_EMAIL", "Jeremiah <owner@example.com>, team@example.com")
	got, err := feedbackRecipients()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "owner@example.com,team@example.com" {
		t.Fatalf("recipients = %#v", got)
	}

	t.Setenv("FEEDBACK_TO_EMAIL", "not an address")
	if _, err := feedbackRecipients(); err == nil {
		t.Fatal("invalid feedback recipient was accepted")
	}
}

func TestBoundedFeedbackTextUsesRuneLength(t *testing.T) {
	if got, err := boundedFeedbackText("  hello\x00  ", 5, "message"); err != nil || got != "hello" {
		t.Fatalf("bounded text = %q, %v", got, err)
	}
	if _, err := boundedFeedbackText("ééé", 2, "message"); err == nil {
		t.Fatal("overlong Unicode message was accepted")
	}
}

func TestSubmitFeedbackUsesAuthenticatedSenderAndWorkspace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.Organization{}, &models.OrgMember{}); err != nil {
		t.Fatal(err)
	}
	user := models.User{Email: "tester@example.com", Name: "Product Tester"}
	org := models.Organization{Name: "Test workspace", Slug: "test-workspace", Personal: true, Seats: 1}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&org).Error; err != nil {
		t.Fatal(err)
	}
	membership := models.OrgMember{
		OrganizationID: org.ID.String(), UserID: user.ID.String(), Role: models.RoleOwner,
	}
	if err := db.Create(&membership).Error; err != nil {
		t.Fatal(err)
	}

	t.Setenv("FEEDBACK_TO_EMAIL", "feedback@example.com")
	t.Setenv("RESEND_API_KEY", "re_test")
	previousSender := feedbackEmailSender
	t.Cleanup(func() { feedbackEmailSender = previousSender })
	var sent feedbackSubmission
	feedbackEmailSender = func(_ context.Context, submission feedbackSubmission) error {
		sent = submission
		return nil
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("screenshot", "capture.png")
	if err != nil {
		t.Fatal(err)
	}
	payload := feedbackPNG(t, 1280, 720)
	if _, err := part.Write(payload); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"message": "The button did not respond", "page": "/workflows/workflow-1", "viewport": "1280x720",
	} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/feedback", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = request
	// These are the request-scoped values set by auth.RequireAuth in production.
	c.Set("auth.userID", user.ID.String())
	c.Set("auth.orgID", org.ID.String())
	handler := &WorkflowHandler{db: &database.DBClient{DB: db}}
	handler.SubmitFeedback(c)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if sent.User.ID != user.ID || sent.Org.ID != org.ID {
		t.Fatalf("sender scope = user %s, org %s", sent.User.ID, sent.Org.ID)
	}
	if sent.Message != "The button did not respond" || sent.Page != "/workflows/workflow-1" || sent.Viewport != "1280x720" {
		t.Fatalf("submission metadata = %#v", sent)
	}
	if sent.Width != 1280 || sent.Height != 720 || !bytes.Equal(sent.Screenshot, payload) {
		t.Fatalf("submission screenshot = %d bytes, %dx%d", len(sent.Screenshot), sent.Width, sent.Height)
	}
}

// A session caches the org it acts in, and removing someone from an org does
// not invalidate their sessions. Without a membership check at submit time, a
// former member's still-valid token keeps sending feedback stamped with a
// workspace they have been removed from.
func TestSubmitFeedbackRefusesAFormerMember(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.Organization{}, &models.OrgMember{}); err != nil {
		t.Fatal(err)
	}
	user := models.User{Email: "former@example.com", Name: "Former Member"}
	org := models.Organization{Name: "Test workspace", Slug: "test-workspace", Seats: 2}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&org).Error; err != nil {
		t.Fatal(err)
	}
	// No membership row: the person was removed after their session was issued.

	t.Setenv("FEEDBACK_TO_EMAIL", "feedback@example.com")
	t.Setenv("RESEND_API_KEY", "re_test")
	previousSender := feedbackEmailSender
	t.Cleanup(func() { feedbackEmailSender = previousSender })
	delivered := false
	feedbackEmailSender = func(context.Context, feedbackSubmission) error {
		delivered = true
		return nil
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("screenshot", "capture.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(feedbackPNG(t, 1280, 720)); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"message": "still here", "page": "/workflows/workflow-1", "viewport": "1280x720",
	} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/feedback", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = request
	c.Set("auth.userID", user.ID.String())
	c.Set("auth.orgID", org.ID.String())
	handler := &WorkflowHandler{db: &database.DBClient{DB: db}}
	handler.SubmitFeedback(c)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", recorder.Code, recorder.Body.String())
	}
	if delivered {
		t.Fatal("feedback from a former member was delivered")
	}
}
