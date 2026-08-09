package handlers

import (
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/resend/resend-go/v2"
	"gorm.io/gorm"

	"workflow-ai/server/internal/auth"
	"workflow-ai/server/internal/billing"
	"workflow-ai/server/internal/database/models"
	mail "workflow-ai/server/internal/email"
	"workflow-ai/server/internal/telemetry"
	"workflow-ai/server/internal/tenancy"
)

// Org members and invitations.
//
// Seats are the billing unit for Team, so every endpoint here is really a billing
// endpoint wearing different clothes: the seat count comes from the subscription,
// and an invite commits a seat the moment it is sent rather than when it is
// accepted.

type memberView struct {
	UserID   string `json:"user_id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Avatar   string `json:"avatar_url,omitempty"`
	Role     string `json:"role"`
	JoinedAt string `json:"joined_at"`
	IsYou    bool   `json:"is_you"`
	// Allocation is this person's credit share and what they have used of it.
	// Present only for viewers allowed to see it — an admin, or yourself.
	Allocation *allocationView `json:"allocation,omitempty"`
}

type allocationView struct {
	Limit     int64 `json:"limit"`
	Spent     int64 `json:"spent"`
	Remaining int64 `json:"remaining"`
	Percent   int   `json:"percent"`
	Custom    bool  `json:"custom"`
}

type inviteView struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	ExpiresAt string `json:"expires_at"`
}

// ListMembers — GET /api/org/members
//
// Returns members, pending invitations and the seat position together, because
// every one of them is needed to render the screen and to decide whether the
// invite form should be enabled.
func (h *WorkflowHandler) ListMembers(c *gin.Context) {
	orgID := currentOrgID(c)
	org, err := h.bill.Org(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load your organization"})
		return
	}
	plan := billing.EffectivePlan(org)
	lim := billing.LimitsForOrg(org)

	var rows []struct {
		UserID    string
		Email     string
		Name      string
		AvatarURL string
		Role      string
		CreatedAt time.Time
	}
	if err := h.db.DB.Raw(`
		SELECT m.user_id, u.email, u.name, u.avatar_url, m.role, m.created_at
		FROM org_members m JOIN users u ON u.id = m.user_id
		WHERE m.organization_id = ?
		ORDER BY CASE m.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, m.created_at`,
		orgID).Scan(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load members"})
		return
	}
	me := auth.UserID(c)
	canManage := tenancy.CanManageMembers(h.db.DB, orgID, me)
	members := make([]memberView, 0, len(rows))
	for _, r := range rows {
		m := memberView{
			UserID: r.UserID, Email: r.Email, Name: r.Name, Avatar: r.AvatarURL,
			Role: r.Role, JoinedAt: r.CreatedAt.Format(time.RFC3339), IsYou: r.UserID == me,
		}
		// Everyone sees their own number; only an admin sees everyone else's.
		if canManage || m.IsYou {
			if a, err := h.bill.AllocationFor(org, r.UserID); err == nil && a.Limit > 0 {
				m.Allocation = &allocationView{
					Limit: a.Limit, Spent: a.Spent, Remaining: a.Remaining(),
					Percent: a.UsedPercent(), Custom: a.Custom,
				}
			}
		}
		members = append(members, m)
	}

	var pending []models.OrgInvite
	h.db.DB.Where(`organization_id = ? AND accepted_at IS NULL AND revoked_at IS NULL
		AND expires_at > ?`, orgID, time.Now()).Order("created_at").Find(&pending)
	invites := make([]inviteView, 0, len(pending))
	for _, i := range pending {
		invites = append(invites, inviteView{
			ID: i.ID.String(), Email: i.Email, Role: string(i.Role),
			ExpiresAt: i.ExpiresAt.Format(time.RFC3339),
		})
	}

	// Seats come from the subscription. For a plan that is not per-seat the member
	// cap IS the seat count, so the same numbers render either way.
	paid := org.Seats
	if !billing.LimitsFor(plan).PerSeat {
		paid = lim.MaxMembers
	}
	usage, err := tenancy.Seats(h.db.DB, orgID, paid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not count seats"})
		return
	}

	// Over cap happens after a downgrade: switching Team→Pro leaves the people who
	// were already in the org on a plan that includes one seat. Their access is NOT
	// revoked — silently cutting off a colleague because the owner changed plan is
	// worse than a visible warning, and the owner is the only one who can decide who
	// should stay. So it is surfaced here and left for them to resolve.
	overCap := 0
	if lim.MaxMembers != 0 && usage.Committed() > usage.Paid {
		overCap = usage.Committed() - usage.Paid
	}

	c.JSON(http.StatusOK, gin.H{
		"members": members,
		"invites": invites,
		"seats": gin.H{
			"paid":      usage.Paid,
			"used":      usage.Committed(),
			"available": usage.Available(),
			"over_cap":  overCap,
			"per_seat":  billing.LimitsFor(plan).PerSeat,
		},
		"plan_name":  planDisplayName(plan),
		"plan":       string(plan),
		"can_manage": canManage,
		"can_add":    lim.MaxMembers == 0 || usage.Available() > 0,
		"unlimited":  lim.MaxMembers == 0,
	})
}

// InviteMember — POST /api/org/invites {"email":"…","role":"member"}
func (h *WorkflowHandler) InviteMember(c *gin.Context) {
	var body struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	orgID, me := currentOrgID(c), auth.UserID(c)

	if !tenancy.CanManageMembers(h.db.DB, orgID, me) {
		// 403 rather than 404: the caller legitimately belongs here, they just are
		// not allowed to do this, and pretending the route does not exist would be
		// confusing rather than protective.
		c.JSON(http.StatusForbidden, gin.H{"error": "only owners and admins can invite people"})
		return
	}

	org, err := h.bill.Org(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load your organization"})
		return
	}
	plan := billing.EffectivePlan(org)
	lim := billing.LimitsForOrg(org)

	// A plan with no room for a second person is a pricing wall, not a seat wall,
	// so it gets the upgrade message rather than "add a seat".
	if lim.MaxMembers == 1 {
		c.JSON(http.StatusPaymentRequired, gin.H{
			"error": fmt.Sprintf("the %s plan is for one person. Upgrade to Team to invite others", plan),
			"limit": "members",
		})
		return
	}

	paid := org.Seats
	if !billing.LimitsFor(plan).PerSeat {
		paid = lim.MaxMembers
	}
	if lim.MaxMembers == 0 {
		// Unlimited members: pass a ceiling high enough never to bind.
		paid = 1 << 20
	}

	role := models.OrgRole(body.Role)
	if role == "" {
		role = models.RoleMember
	}
	inv, token, err := tenancy.Invite(h.db.DB, orgID, me, body.Email, role, paid)
	if err != nil {
		switch {
		case errors.Is(err, tenancy.ErrNoSeats):
			c.JSON(http.StatusPaymentRequired, gin.H{"error": err.Error(), "limit": billing.KindOf(err)})
		case errors.Is(err, tenancy.ErrAlreadyMember):
			c.JSON(http.StatusConflict, gin.H{"error": "that person is already in your organization"})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
		return
	}

	// The email is the only place the raw token ever exists. If sending fails the
	// invite is revoked rather than left as a seat held open by a link nobody has.
	if err := sendInviteEmail(inv.Email, org.Name, token, clientBaseURL(c)); err != nil {
		_ = tenancy.Revoke(h.db.DB, orgID, inv.ID.String())
		slog.ErrorContext(c.Request.Context(), "invite email failed", "error", err, "org_id", orgID)
		telemetry.EmailSent(c.Request.Context(), "org_invite", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "could not send the invitation — nothing was charged and the seat is still free"})
		return
	}
	telemetry.EmailSent(c.Request.Context(), "org_invite", nil)
	slog.InfoContext(c.Request.Context(), "org invite sent",
		"org_id", orgID, "invited_by", me, "role", role)

	c.JSON(http.StatusCreated, inviteView{
		ID: inv.ID.String(), Email: inv.Email, Role: string(inv.Role),
		ExpiresAt: inv.ExpiresAt.Format(time.RFC3339),
	})
}

// RevokeInvite — DELETE /api/org/invites/:id
func (h *WorkflowHandler) RevokeInvite(c *gin.Context) {
	orgID := currentOrgID(c)
	if !tenancy.CanManageMembers(h.db.DB, orgID, auth.UserID(c)) {
		c.JSON(http.StatusForbidden, gin.H{"error": "only owners and admins can manage invitations"})
		return
	}
	if err := tenancy.Revoke(h.db.DB, orgID, c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "that invitation is no longer pending"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"revoked": true})
}

// RemoveMember — DELETE /api/org/members/:userId
func (h *WorkflowHandler) RemoveMember(c *gin.Context) {
	orgID, me := currentOrgID(c), auth.UserID(c)
	target := c.Param("userId")

	// Leaving voluntarily is always allowed; removing someone else needs authority.
	if target != me && !tenancy.CanManageMembers(h.db.DB, orgID, me) {
		c.JSON(http.StatusForbidden, gin.H{"error": "only owners and admins can remove people"})
		return
	}
	// A hosted agent delegates its deployer's Fernary identity. The cross-replica
	// authority lock is held by approved external mutations from claim through
	// outcome persistence, so removal either revokes first or waits for the
	// already-authorized operation to finish. Revocation and membership deletion
	// then commit as one database transaction.
	err := h.withHostedAuthorityLock(c.Request.Context(), orgID, target, func(connection *gorm.DB) error {
		return removeMemberAndRevokeAgentAuthority(connection, orgID, target)
	})
	if err != nil {
		if errors.Is(err, tenancy.ErrNotPermitted) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		if errors.Is(err, tenancy.ErrNotMember) {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		slog.ErrorContext(c.Request.Context(), "could not atomically remove organization member",
			"org_id", orgID, "user_id", target, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not remove organization member"})
		return
	}
	slog.InfoContext(c.Request.Context(), "org member removed",
		"org_id", orgID, "removed_by", me, "self", target == me)
	c.JSON(http.StatusOK, gin.H{"removed": true})
}

func removeMemberAndRevokeAgentAuthority(db *gorm.DB, orgID, target string) error {
	return db.Transaction(func(tx *gorm.DB) error {
		deploymentIDs := tx.Model(&models.AgentDeployment{}).Select("id").
			Where("organization_id = ? AND deployed_by_user_id = ?", orgID, target)
		if err := tx.Model(&models.AgentDeployment{}).
			Where("organization_id = ? AND deployed_by_user_id = ? AND status <> ?",
				orgID, target, models.AgentDeploymentRevoked).
			Update("status", models.AgentDeploymentRevoked).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.AgentDeploymentTarget{}).
			Where("deployment_id IN (?)", deploymentIDs).
			Update("enabled", false).Error; err != nil {
			return err
		}
		return tenancy.RemoveMemberWithinTransaction(tx, orgID, target)
	})
}

// AcceptInvite — POST /api/org/invites/accept {"token":"…"}
//
// Requires a session: joining an org has to attach to a real account, so the
// frontend signs the person in first and then posts the token.
func (h *WorkflowHandler) AcceptInvite(c *gin.Context) {
	var body struct {
		Token string `json:"token"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	var user models.User
	if err := h.db.DB.Where("id = ?", auth.UserID(c)).First(&user).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "sign in to accept this invitation"})
		return
	}

	org, err := tenancy.Accept(h.db.DB, body.Token, &user)
	if err != nil {
		switch {
		case errors.Is(err, tenancy.ErrWrongRecipient):
			c.JSON(http.StatusForbidden, gin.H{
				"error": "this invitation was sent to a different email address. " +
					"Sign in with that address to accept it."})
		default:
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "this invitation is no longer valid — ask for a new one"})
		}
		return
	}

	// The session caches the org id, so joining a new one has to re-issue it or the
	// user keeps acting inside their personal org.
	token, err := h.startSession(c, &user)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"organization": gin.H{"id": org.ID.String(), "name": org.Name},
			"note":         "joined — sign in again to switch to this organization",
		})
		return
	}
	slog.InfoContext(c.Request.Context(), "org invite accepted",
		"org_id", org.ID.String(), "user_id", user.ID.String())
	c.JSON(http.StatusOK, gin.H{
		"organization": gin.H{"id": org.ID.String(), "name": org.Name},
		"token":        token,
	})
}

// InviteInfo — GET /api/org/invites/info?token=…
//
// Public: the accept page needs to say who invited you and to which org BEFORE you
// have signed in, otherwise the flow is "log in to find out what this link does".
// Returns only what is safe to show an unauthenticated holder of the link.
func (h *WorkflowHandler) InviteInfo(c *gin.Context) {
	raw := c.Query("token")
	if raw == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token is required"})
		return
	}
	var inv models.OrgInvite
	if err := h.db.DB.Where("token_hash = ?", tenancy.HashInviteToken(raw)).First(&inv).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "this invitation is no longer valid"})
		return
	}
	if !inv.Pending() {
		c.JSON(http.StatusNotFound, gin.H{"error": "this invitation is no longer valid"})
		return
	}
	var org models.Organization
	h.db.DB.Where("id = ?", inv.OrganizationID).First(&org)
	c.JSON(http.StatusOK, gin.H{
		"organization": org.Name,
		"email":        inv.Email,
		"role":         string(inv.Role),
	})
}

// ── invite email ─────────────────────────────────────────────────

func sendInviteEmail(to, orgName, token, linkBase string) error {
	link := linkBase + "/invite?token=" + url.QueryEscape(token)
	apiKey := os.Getenv("RESEND_API_KEY")
	if apiKey == "" {
		// Dev fallback, matching the login flow: no provider, so surface the link.
		slog.Warn("RESEND_API_KEY not set — printing invite link", "email", to, "link", link)
		return nil
	}

	subject := fmt.Sprintf("You've been invited to %s on Fernary", orgName)
	text := fmt.Sprintf(`You've been invited to join %s on Fernary.

Accept the invitation:
%s

This link expires in 7 days and only works for %s.

If you weren't expecting this, you can ignore this email.`, orgName, link, to)

	inner := fmt.Sprintf(`<h2 style="margin-top:0;text-align:center">Join %s</h2>
<p style="text-align:center;color:%s;font-size:13px;margin:0 0 24px">
You've been invited to collaborate on Fernary — workflows that run on a schedule,
pause for approval where it matters, and remember where they left off.</p>
%s
<p style="text-align:center;color:%s;font-size:11px;margin:26px 0 0">
This invitation expires in 7 days and only works for %s.</p>`,
		html.EscapeString(orgName), mail.Muted,
		mail.Button(link, "Accept invitation"),
		mail.Muted, html.EscapeString(to))

	client := resend.NewClient(apiKey)
	_, err := client.Emails.Send(&resend.SendEmailRequest{
		From:    mail.FromAddress(),
		To:      []string{to},
		Subject: subject,
		Text:    text,
		Html:    mail.WrapBranded(inner, subject),
	})
	return err
}

// seatsInUse is a small helper for the billing screen, which wants the same figure
// without the full member listing.
func seatsInUse(db *gorm.DB, orgID string, paid int) tenancy.SeatUsage {
	u, err := tenancy.Seats(db, orgID, paid)
	if err != nil {
		return tenancy.SeatUsage{Paid: paid}
	}
	return u
}

// SetSeats — POST /api/org/seats {"seats":4}
//
// Seat count is set explicitly by an admin rather than tracked from membership.
// Removing someone frees their seat but keeps paying for it until the admin lowers
// the count, which is deliberate: a seat held open for a replacement hire is a
// normal thing to want, and silently giving it up on a removal would mean losing it
// at whatever price applies when they come to re-add it.
//
// The ordering is enforced here: a reduction is refused while the org still holds
// more people than the new count would allow, naming how many have to go. Doing it
// the other way round — cutting the seats and leaving people stranded — is the
// state the over-cap warning exists to describe, and it should not be reachable
// through our own UI.
func (h *WorkflowHandler) SetSeats(c *gin.Context) {
	var body struct {
		Seats int `json:"seats"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	orgID := currentOrgID(c)
	if !tenancy.CanManageMembers(h.db.DB, orgID, auth.UserID(c)) {
		c.JSON(http.StatusForbidden, gin.H{
			"error": "your plan is managed by your organization's owner — ask them to change it"})
		return
	}

	org, err := h.bill.Org(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load your organization"})
		return
	}
	plan := billing.EffectivePlan(org)
	if !billing.LimitsFor(plan).PerSeat {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("the %s plan is not billed per seat", plan)})
		return
	}
	if body.Seats < billing.MinSeats {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("the smallest team is %d seats", billing.MinSeats)})
		return
	}

	// Pending invitations count: each one is holding a seat for someone who has been
	// promised one, and cutting it out from under them would be worse than refusing.
	usage, err := tenancy.Seats(h.db.DB, orgID, body.Seats)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not count seats"})
		return
	}
	if body.Seats < usage.Committed() {
		excess := usage.Committed() - body.Seats
		noun := "people"
		if excess == 1 {
			noun = "person"
		}
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("you have %d %s in this organization, including pending "+
				"invitations. Remove %d %s before dropping to %d seats.",
				usage.Committed(), pluralPeople(usage.Committed()), excess, noun, body.Seats),
			"committed": usage.Committed(),
			"requested": body.Seats,
			"remove":    excess,
		})
		return
	}

	if err := h.bill.SetSeats(c.Request.Context(), org, body.Seats); err != nil {
		if errors.Is(err, billing.ErrStripeNotConfigured) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "billing is not available yet"})
			return
		}
		slog.ErrorContext(c.Request.Context(), "seats: update failed", "error", err, "org_id", orgID)
		c.JSON(http.StatusBadGateway, gin.H{"error": "could not update your seats — nothing was charged"})
		return
	}

	slog.InfoContext(c.Request.Context(), "seats changed",
		"org_id", orgID, "from", org.Seats, "to", body.Seats, "by", auth.UserID(c))

	// Stripe's webhook is what actually updates the stored seat count, and it can
	// land a moment later. Reporting the requested value with a flag saying it is
	// settling is more honest than echoing the stale one.
	c.JSON(http.StatusOK, gin.H{
		"seats":     body.Seats,
		"previous":  org.Seats,
		"increase":  body.Seats > org.Seats,
		"effective": effectiveWhen(body.Seats > org.Seats, org.CurrentPeriodEnd),
	})
}

func pluralPeople(n int) string {
	if n == 1 {
		return "person"
	}
	return "people"
}

// effectiveWhen says when a seat change takes hold: an increase now, a reduction at
// the end of the period the customer already paid for.
func effectiveWhen(increase bool, periodEnd *time.Time) string {
	if increase {
		return "now"
	}
	if periodEnd != nil {
		return periodEnd.Format(time.RFC3339)
	}
	return "period_end"
}

// SetMemberLimit — POST /api/org/members/:userId/limit {"limit":200000}
//
// An admin raising or lowering one person's share. Zero restores the equal split,
// which is why it is not spelled "unlimited": going back to the default should
// track seat changes automatically rather than freezing at whatever the split
// happened to be on the day.
func (h *WorkflowHandler) SetMemberLimit(c *gin.Context) {
	var body struct {
		Limit int64 `json:"limit"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	orgID := currentOrgID(c)
	if !tenancy.CanManageMembers(h.db.DB, orgID, auth.UserID(c)) {
		c.JSON(http.StatusForbidden, gin.H{
			"error": "only owners and admins can change credit limits"})
		return
	}
	if err := h.bill.SetMemberLimit(orgID, c.Param("userId"), body.Limit); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	slog.InfoContext(c.Request.Context(), "member credit limit changed",
		"org_id", orgID, "member", c.Param("userId"), "limit", body.Limit, "by", auth.UserID(c))
	c.JSON(http.StatusOK, gin.H{"limit": body.Limit})
}
