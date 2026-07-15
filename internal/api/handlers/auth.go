package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/database/models"
	mail "workflow-ai/server/internal/email"

	"github.com/gin-gonic/gin"
	"github.com/resend/resend-go/v2"
)

// Passwordless email + Google sign-in. Every email/start issues a 6-digit
// code AND a magic-link token; either one completes auth and signs the user
// in (creating the account on first use).

const loginCodeTTL = 10 * time.Minute

func normalizeEmail(e string) string {
	return strings.ToLower(strings.TrimSpace(e))
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func randomOTP() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	return fmt.Sprintf("%06d", n.Int64())
}

// frontendURL returns the primary frontend origin (first entry of the
// comma-separated FRONTEND_URL env var).
func frontendURL() string {
	raw := strings.Split(os.Getenv("FRONTEND_URL"), ",")[0]
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		raw = "http://localhost:5173"
	}
	return raw
}

func authFromEmail() string {
	if v := os.Getenv("AUTH_FROM_EMAIL"); v != "" {
		return v
	}
	return "workflow-ai <noreply@usecelery.io>"
}

func publicUser(u *models.User) gin.H {
	return gin.H{
		"id":         u.ID.String(),
		"email":      u.Email,
		"name":       u.Name,
		"avatar_url": u.AvatarURL,
	}
}

// ── Email: start ────────────────────────────────────────────────

// POST /api/auth/email/start
func (h *WorkflowHandler) AuthEmailStart(c *gin.Context) {
	var body struct {
		Email string `json:"email"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	email := normalizeEmail(body.Email)
	if email == "" || !strings.Contains(email, "@") || len(email) > 254 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "enter a valid email address"})
		return
	}

	ctx := c.Request.Context()
	if !auth.Allow(ctx, h.redis, "rl:otp:email:"+email, 3, 10*time.Minute) ||
		!auth.Allow(ctx, h.redis, "rl:otp:ip:"+c.ClientIP(), 10, time.Hour) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many requests — try again in a few minutes"})
		return
	}

	// Only the newest code for an email is valid.
	now := time.Now()
	h.db.DB.Model(&models.LoginCode{}).
		Where("email = ? AND consumed_at IS NULL", email).
		Update("consumed_at", now)

	code := randomOTP()
	token := randomToken()
	lc := models.LoginCode{
		Email:     email,
		CodeHash:  sha256hex(code),
		TokenHash: sha256hex(token),
		ExpiresAt: now.Add(loginCodeTTL),
	}
	if err := h.db.DB.Create(&lc).Error; err != nil {
		slog.Error("auth: failed to store login code", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not start sign-in"})
		return
	}

	// The magic link goes back to wherever the user is actually running the
	// app — the request Origin when allowed, else the configured frontend.
	linkBase := frontendURL()
	if o := strings.TrimRight(c.GetHeader("Origin"), "/"); o != "" && auth.OriginAllowed(o) {
		linkBase = o
	}

	if err := sendLoginEmail(email, code, token, linkBase); err != nil {
		slog.Error("auth: failed to send login email", "error", err, "email", email)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not send the sign-in email"})
		return
	}

	// Identical response whether or not an account exists — no enumeration.
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func sendLoginEmail(email, code, token, linkBase string) error {
	apiKey := os.Getenv("RESEND_API_KEY")
	magicLink := linkBase + "/auth/verify?token=" + url.QueryEscape(token)
	if apiKey == "" {
		// Dev fallback: no email provider — surface the code in server logs.
		slog.Warn("auth: RESEND_API_KEY not set — printing login code", "email", email, "code", code, "link", magicLink)
		return nil
	}

	text := fmt.Sprintf(`Your sign-in code is:

    %s

This code expires in 10 minutes.

Or click this link to sign in instantly:
%s

If you didn't request this, you can safely ignore this email.`, code, magicLink)

	inner := fmt.Sprintf(`<h2 style="margin-top:0;text-align:center">Sign in to Flowe</h2>
<p style="text-align:center;color:#667179;font-size:13px;margin:0 0 24px">Enter this code — it expires in 10 minutes.</p>
<p style="text-align:center;color:#ffffff;font-size:32px;font-family:ui-monospace,monospace;letter-spacing:8px;margin:0 0 8px">%s</p>
%s
<p style="text-align:center;color:#667179;font-size:11px;margin:24px 0 0">If you didn't request this, you can safely ignore this email.</p>`,
		code, mail.Button(magicLink, "Sign in instantly"))
	html := mail.WrapBranded(inner, "Your Flowe sign-in code")

	client := resend.NewClient(apiKey)
	_, err := client.Emails.Send(&resend.SendEmailRequest{
		From:    authFromEmail(),
		To:      []string{email},
		Subject: code + " is your Flowe sign-in code",
		Text:    text,
		Html:    html,
	})
	return err
}

// ── Email: verify ───────────────────────────────────────────────

// POST /api/auth/email/verify — body is {email, code} or {token}
func (h *WorkflowHandler) AuthEmailVerify(c *gin.Context) {
	var body struct {
		Email string `json:"email"`
		Code  string `json:"code"`
		Token string `json:"token"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	var lc models.LoginCode
	now := time.Now()

	switch {
	case body.Token != "":
		if err := h.db.DB.Where("token_hash = ? AND consumed_at IS NULL AND expires_at > ?",
			sha256hex(body.Token), now).First(&lc).Error; err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired sign-in link"})
			return
		}

	case body.Email != "" && body.Code != "":
		email := normalizeEmail(body.Email)
		if err := h.db.DB.Where("email = ? AND consumed_at IS NULL AND expires_at > ?", email, now).
			Order("created_at desc").First(&lc).Error; err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired code"})
			return
		}
		if lc.Attempts >= 5 {
			c.JSON(http.StatusGone, gin.H{"error": "too many attempts — request a new code"})
			return
		}
		h.db.DB.Model(&lc).Update("attempts", lc.Attempts+1)
		if subtle.ConstantTimeCompare([]byte(sha256hex(body.Code)), []byte(lc.CodeHash)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired code"})
			return
		}

	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "provide email+code or token"})
		return
	}

	h.db.DB.Model(&lc).Update("consumed_at", now)

	user, err := h.findOrCreateUserByEmail(lc.Email)
	if err != nil {
		slog.Error("auth: find/create user failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not sign you in"})
		return
	}

	token, err := h.startSession(c, user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not sign you in"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": publicUser(user), "token": token})
}

func (h *WorkflowHandler) findOrCreateUserByEmail(email string) (*models.User, error) {
	var user models.User
	err := h.db.DB.Where("email = ?", email).First(&user).Error
	if err == nil {
		return &user, nil
	}
	user = models.User{Email: email}
	if err := h.db.DB.Create(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

// startSession creates a Redis session and returns the raw bearer token the
// client stores and sends back as `Authorization: Bearer <token>`.
func (h *WorkflowHandler) startSession(c *gin.Context, user *models.User) (string, error) {
	token, err := auth.CreateSession(c.Request.Context(), h.redis, user.ID.String())
	if err != nil {
		slog.Error("auth: create session failed", "error", err)
		return "", err
	}
	return token, nil
}

// ── Google OAuth ────────────────────────────────────────────────

// GET /api/auth/google/connect
func (h *WorkflowHandler) AuthGoogleConnect(c *gin.Context) {
	clientID := os.Getenv("GOOGLE_CLIENT_ID")
	if clientID == "" {
		c.String(http.StatusBadRequest, "GOOGLE_CLIENT_ID not configured on server")
		return
	}
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", oauthRedirectBase()+"/api/auth/google/callback")
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("prompt", "select_account")
	q.Set("state", newOAuthState("", openerOrigin(c))) // login flow: no user yet
	c.Redirect(http.StatusFound, "https://accounts.google.com/o/oauth2/v2/auth?"+q.Encode())
}

func oauthRedirectBase() string {
	base := os.Getenv("OAUTH_REDIRECT_BASE")
	if base == "" {
		base = "http://localhost:8080"
	}
	return strings.TrimRight(base, "/")
}

// GET /api/auth/google/callback
func (h *WorkflowHandler) AuthGoogleCallback(c *gin.Context) {
	_, openerOrig, _, stateOK := consumeOAuthState(c.Query("state"))
	if !stateOK {
		authResultPage(c, openerOrig, "", false, "Sign-in session expired — please try again.")
		return
	}
	if errParam := c.Query("error"); errParam != "" {
		authResultPage(c, openerOrig, "", false, "Google sign-in was cancelled.")
		return
	}
	code := c.Query("code")
	if code == "" {
		authResultPage(c, openerOrig, "", false, "Google did not return an authorization code.")
		return
	}

	claims, err := exchangeGoogleCode(code)
	if err != nil {
		slog.Error("auth: google exchange failed", "error", err)
		authResultPage(c, openerOrig, "", false, "Could not verify your Google account.")
		return
	}

	user, err := h.upsertGoogleUser(claims)
	if err != nil {
		slog.Error("auth: google upsert failed", "error", err)
		authResultPage(c, openerOrig, "", false, "Could not sign you in.")
		return
	}

	token, err := h.startSession(c, user)
	if err != nil {
		authResultPage(c, openerOrig, "", false, "Could not sign you in.")
		return
	}
	authResultPage(c, openerOrig, token, true, "")
}

type googleClaims struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified string `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
	Aud           string `json:"aud"`
}

// exchangeGoogleCode swaps the auth code for tokens and validates the
// id_token via Google's tokeninfo endpoint (signature checked Google-side;
// we verify audience and that the email is verified).
func exchangeGoogleCode(code string) (*googleClaims, error) {
	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", os.Getenv("GOOGLE_CLIENT_ID"))
	form.Set("client_secret", os.Getenv("GOOGLE_CLIENT_SECRET"))
	form.Set("redirect_uri", oauthRedirectBase()+"/api/auth/google/callback")
	form.Set("grant_type", "authorization_code")

	resp, err := http.PostForm("https://oauth2.googleapis.com/token", form)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange returned %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if json.Unmarshal(raw, &tok) != nil || tok.IDToken == "" {
		return nil, fmt.Errorf("no id_token in response")
	}

	infoResp, err := http.Get("https://oauth2.googleapis.com/tokeninfo?id_token=" + url.QueryEscape(tok.IDToken))
	if err != nil {
		return nil, fmt.Errorf("tokeninfo: %w", err)
	}
	defer infoResp.Body.Close()
	infoRaw, _ := io.ReadAll(infoResp.Body)
	if infoResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tokeninfo returned %d", infoResp.StatusCode)
	}
	var claims googleClaims
	if json.Unmarshal(infoRaw, &claims) != nil {
		return nil, fmt.Errorf("tokeninfo parse failed")
	}
	if claims.Aud != os.Getenv("GOOGLE_CLIENT_ID") {
		return nil, fmt.Errorf("id_token audience mismatch")
	}
	if claims.EmailVerified != "true" || claims.Email == "" {
		return nil, fmt.Errorf("google email not verified")
	}
	return &claims, nil
}

// upsertGoogleUser links or creates the account: match by google_id first,
// then by verified email (linking Google to an existing OTP account), else
// create a new user.
func (h *WorkflowHandler) upsertGoogleUser(claims *googleClaims) (*models.User, error) {
	email := normalizeEmail(claims.Email)

	var user models.User
	if err := h.db.DB.Where("google_id = ?", claims.Sub).First(&user).Error; err == nil {
		h.fillProfile(&user, claims)
		return &user, nil
	}

	if err := h.db.DB.Where("email = ?", email).First(&user).Error; err == nil {
		sub := claims.Sub
		user.GoogleID = &sub
		h.fillProfile(&user, claims)
		if err := h.db.DB.Model(&user).Updates(map[string]any{
			"google_id": sub, "name": user.Name, "avatar_url": user.AvatarURL,
		}).Error; err != nil {
			return nil, err
		}
		return &user, nil
	}

	sub := claims.Sub
	user = models.User{Email: email, Name: claims.Name, AvatarURL: claims.Picture, GoogleID: &sub}
	if err := h.db.DB.Create(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

// fillProfile backfills empty profile fields from Google claims.
func (h *WorkflowHandler) fillProfile(user *models.User, claims *googleClaims) {
	updates := map[string]any{}
	if user.Name == "" && claims.Name != "" {
		user.Name = claims.Name
		updates["name"] = claims.Name
	}
	if user.AvatarURL == "" && claims.Picture != "" {
		user.AvatarURL = claims.Picture
		updates["avatar_url"] = claims.Picture
	}
	if len(updates) > 0 {
		h.db.DB.Model(user).Updates(updates)
	}
}

// authResultPage notifies the login page (popup opener) and closes. On success
// it hands the session bearer token to the opener via postMessage (targeting
// the opener origin captured at connect time, never '*'), which the SPA stores
// for Authorization headers.
func authResultPage(c *gin.Context, targetOrigin, token string, ok bool, errMsg string) {
	status := "ok"
	detail := "You're signed in — this window will close."
	if !ok {
		status = "error"
		detail = errMsg
	}
	payload, _ := json.Marshal(map[string]string{
		"type":   "auth-oauth",
		"status": status,
		"error":  errMsg,
		"token":  token,
	})
	if targetOrigin == "" {
		targetOrigin = frontendURL()
	}
	target, _ := json.Marshal(targetOrigin)
	safeHTML := `<!doctype html><html><body style="font-family:system-ui;background:#0D0D11;color:#fff;display:flex;align-items:center;justify-content:center;height:100vh;margin:0">
<div style="text-align:center"><p style="font-size:15px">Google sign-in</p>
<p style="font-size:12px;color:#667179;max-width:420px">` + html.EscapeString(detail) + `</p></div>
<script>
if (window.opener) { window.opener.postMessage(` + string(payload) + `, ` + string(target) + `); setTimeout(() => window.close(), 800); }
</script></body></html>`
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(safeHTML))
}

// ── Me / logout ─────────────────────────────────────────────────

// GET /api/auth/me — public route; resolves the bearer token itself so it can
// return a clean 401 JSON body instead of the middleware's.
func (h *WorkflowHandler) AuthMe(c *gin.Context) {
	token := auth.BearerToken(c)
	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"user": nil})
		return
	}
	userID, ok := auth.GetSession(c.Request.Context(), h.redis, token)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"user": nil})
		return
	}
	var user models.User
	if err := h.db.DB.First(&user, "id = ?", userID).Error; err != nil {
		auth.DestroySession(c.Request.Context(), h.redis, token)
		c.JSON(http.StatusUnauthorized, gin.H{"user": nil})
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": publicUser(&user)})
}

// POST /api/auth/logout
func (h *WorkflowHandler) AuthLogout(c *gin.Context) {
	if token := auth.BearerToken(c); token != "" {
		auth.DestroySession(c.Request.Context(), h.redis, token)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
