package tenancy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"workflow-ai/server/internal/database/models"
)

// Org invites.
//
// The seat accounting here is the part worth reading carefully. An org pays for N
// seats and every seat must be accounted for by either a member or a pending
// invite — counting only members would let someone invite twenty people onto a
// two-seat plan and have them all accept.

// InviteTTL bounds how long an invitation is good for. Long enough to survive a
// weekend, short enough that a forwarded email from months ago is not a way in.
const InviteTTL = 7 * 24 * time.Hour

var (
	ErrNoSeats        = errors.New("no seats available")
	ErrInviteInvalid  = errors.New("this invitation is no longer valid")
	ErrAlreadyMember  = errors.New("already a member of this organization")
	ErrNotPermitted   = errors.New("not permitted")
	ErrNotMember      = errors.New("not a member of this organization")
	ErrWrongRecipient = errors.New("this invitation was sent to a different email address")
)

// SeatUsage is what an org has committed against what it pays for.
type SeatUsage struct {
	// Paid is the seat count the subscription bills for.
	Paid int
	// Members are people already in the org.
	Members int
	// PendingInvites are sent, unexpired, unaccepted invitations. They hold a seat
	// open: if they did not, an org could issue unlimited invites and be
	// over-subscribed the moment they were all accepted.
	PendingInvites int
}

// Committed is seats already spoken for.
func (s SeatUsage) Committed() int { return s.Members + s.PendingInvites }

// Available is how many more people can be invited.
func (s SeatUsage) Available() int {
	if n := s.Paid - s.Committed(); n > 0 {
		return n
	}
	return 0
}

// Seats reports an org's seat usage. paidSeats comes from the caller because
// resolving it needs the plan, and this package deliberately knows nothing about
// billing.
func Seats(db *gorm.DB, orgID string, paidSeats int) (SeatUsage, error) {
	u := SeatUsage{Paid: paidSeats}
	var members, invites int64
	if err := db.Model(&models.OrgMember{}).
		Where("organization_id = ?", orgID).Count(&members).Error; err != nil {
		return u, err
	}
	if err := db.Model(&models.OrgInvite{}).
		Where(`organization_id = ? AND accepted_at IS NULL AND revoked_at IS NULL
			AND expires_at > ?`, orgID, time.Now()).Count(&invites).Error; err != nil {
		return u, err
	}
	u.Members, u.PendingInvites = int(members), int(invites)
	return u, nil
}

// Invite creates an invitation and returns it with the raw token, which is the
// only time that token exists — only its hash is stored.
//
// Re-inviting an address replaces its pending invite rather than adding a second.
// Two live invitations for one person would hold two seats and, worse, the second
// email would silently invalidate the first if only one could be accepted.
func Invite(db *gorm.DB, orgID, invitedBy, email string, role models.OrgRole, paidSeats int) (*models.OrgInvite, string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") || len(email) > 254 {
		return nil, "", fmt.Errorf("enter a valid email address")
	}
	switch role {
	case models.RoleAdmin, models.RoleMember:
		// Fine. Owner is deliberately not invitable — ownership transfers are a
		// different operation with different consequences.
	default:
		return nil, "", fmt.Errorf("role must be admin or member")
	}

	// Already in the org? Say so plainly rather than sending an email that would
	// fail on accept.
	var existing int64
	if err := db.Model(&models.OrgMember{}).
		Joins("JOIN users u ON u.id = org_members.user_id").
		Where("org_members.organization_id = ? AND lower(u.email) = ?", orgID, email).
		Count(&existing).Error; err != nil {
		return nil, "", err
	}
	if existing > 0 {
		return nil, "", ErrAlreadyMember
	}

	raw := newInviteToken()
	inv := models.OrgInvite{
		OrganizationID: orgID,
		Email:          email,
		Role:           role,
		InvitedBy:      invitedBy,
		TokenHash:      hashToken(raw),
		ExpiresAt:      time.Now().Add(InviteTTL),
	}

	err := db.Transaction(func(tx *gorm.DB) error {
		// The seat check and the insert must be one transaction, or two concurrent
		// invites both see the last free seat.
		//
		// Locking the org row rather than the invites serialises invite creation for
		// this org: there is no single row to lock over "the set of invites", and
		// SELECT … FOR UPDATE on a count proves nothing about rows not yet inserted.
		var org models.Organization
		if err := tx.Raw(
			`SELECT * FROM organizations WHERE id = ? FOR UPDATE`, orgID).Scan(&org).Error; err != nil {
			return err
		}

		// Supersede any pending invite for this address instead of stacking one.
		if err := tx.Model(&models.OrgInvite{}).
			Where("organization_id = ? AND email = ? AND accepted_at IS NULL AND revoked_at IS NULL",
				orgID, email).
			Update("revoked_at", time.Now()).Error; err != nil {
			return err
		}

		usage, err := Seats(tx, orgID, paidSeats)
		if err != nil {
			return err
		}
		if usage.Available() < 1 {
			return fmt.Errorf("%w: %d of %d seats are in use. Add a seat to invite "+
				"someone else", ErrNoSeats, usage.Committed(), usage.Paid)
		}

		// A superseded invite is revoked above, so this upserts onto the unique
		// (organization_id, email) index rather than colliding with it.
		return tx.Where("organization_id = ? AND email = ?", orgID, email).
			Assign(map[string]any{
				"role":        inv.Role,
				"invited_by":  inv.InvitedBy,
				"token_hash":  inv.TokenHash,
				"expires_at":  inv.ExpiresAt,
				"accepted_at": nil,
				"revoked_at":  nil,
			}).FirstOrCreate(&inv).Error
	})
	if err != nil {
		return nil, "", err
	}
	return &inv, raw, nil
}

// Accept turns an invitation into a membership for the given user.
//
// The user's email must match the invited address. Without that check a leaked
// invite link is a way into someone else's organization for whoever opens it.
func Accept(db *gorm.DB, rawToken string, user *models.User) (*models.Organization, error) {
	if strings.TrimSpace(rawToken) == "" {
		return nil, ErrInviteInvalid
	}
	var org models.Organization
	err := db.Transaction(func(tx *gorm.DB) error {
		var inv models.OrgInvite
		// Locked for update so a link opened twice in quick succession cannot create
		// two memberships.
		if err := tx.Raw(`SELECT * FROM org_invites WHERE token_hash = ? FOR UPDATE`,
			hashToken(rawToken)).Scan(&inv).Error; err != nil {
			return err
		}
		if inv.ID.String() == "" || inv.OrganizationID == "" {
			return ErrInviteInvalid
		}
		if !inv.Pending() {
			return ErrInviteInvalid
		}
		if !strings.EqualFold(strings.TrimSpace(user.Email), inv.Email) {
			return ErrWrongRecipient
		}

		// Already a member — accept idempotently rather than erroring, since a
		// double-clicked link should land the user in the org either way.
		var already int64
		if err := tx.Model(&models.OrgMember{}).
			Where("organization_id = ? AND user_id = ?", inv.OrganizationID, user.ID).
			Count(&already).Error; err != nil {
			return err
		}
		if already == 0 {
			member := models.OrgMember{
				OrganizationID: inv.OrganizationID,
				UserID:         user.ID.String(),
				Role:           inv.Role,
			}
			if err := tx.Create(&member).Error; err != nil {
				return err
			}
			// An org with a second member is no longer a personal one. Provision sets
			// Personal true for everybody and nothing else ever cleared it, which left
			// the flag permanently true and made every query that ordered by it a
			// no-op — including the one that decides which org a session acts in.
			if err := tx.Model(&models.Organization{}).
				Where("id = ?", inv.OrganizationID).
				Update("personal", false).Error; err != nil {
				return err
			}
		}
		now := time.Now()
		if err := tx.Model(&models.OrgInvite{}).Where("id = ?", inv.ID).
			Update("accepted_at", now).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", inv.OrganizationID).First(&org).Error
	})
	if err != nil {
		return nil, err
	}
	return &org, nil
}

// Revoke withdraws a pending invitation, freeing its seat.
func Revoke(db *gorm.DB, orgID, inviteID string) error {
	res := db.Model(&models.OrgInvite{}).
		Where("id = ? AND organization_id = ? AND accepted_at IS NULL AND revoked_at IS NULL",
			inviteID, orgID).
		Update("revoked_at", time.Now())
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrInviteInvalid
	}
	return nil
}

// RemoveMember takes someone out of an org, freeing their seat.
//
// The last owner cannot be removed: an org with no owner has nobody who can
// manage billing or membership, and there is no self-service way back from that.
func RemoveMember(db *gorm.DB, orgID, memberUserID string) error {
	return db.Transaction(func(tx *gorm.DB) error {
		return RemoveMemberWithinTransaction(tx, orgID, memberUserID)
	})
}

// RemoveMemberWithinTransaction applies the membership checks and deletion on
// a transaction owned by the caller. It exists for invariants that must commit
// atomically with membership removal, such as revoking authority delegated to
// hosted agents. Callers must not pass a non-transactional database handle.
func RemoveMemberWithinTransaction(tx *gorm.DB, orgID, memberUserID string) error {
	var m models.OrgMember
	if err := tx.Where("organization_id = ? AND user_id = ?", orgID, memberUserID).
		First(&m).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotMember
		}
		return err
	}
	if m.Role == models.RoleOwner {
		var owners int64
		if err := tx.Model(&models.OrgMember{}).
			Where("organization_id = ? AND role = ?", orgID, models.RoleOwner).
			Count(&owners).Error; err != nil {
			return err
		}
		if owners <= 1 {
			return fmt.Errorf("%w: an organization needs at least one owner", ErrNotPermitted)
		}
	}
	return tx.Where("organization_id = ? AND user_id = ?", orgID, memberUserID).
		Delete(&models.OrgMember{}).Error
}

// CanManageMembers reports whether a user may invite and remove people.
func CanManageMembers(db *gorm.DB, orgID, userID string) bool {
	var m models.OrgMember
	if err := db.Where("organization_id = ? AND user_id = ?", orgID, userID).
		First(&m).Error; err != nil {
		return false
	}
	return m.Role == models.RoleOwner || m.Role == models.RoleAdmin
}

// ── tokens ───────────────────────────────────────────────────────

func newInviteToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the process cannot generate secrets at all;
		// continuing would mint a predictable invite token.
		panic("tenancy: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// HashInviteToken exposes the hash for callers that look an invite up by its raw
// token — the public "what is this link?" endpoint. Exported so no caller is
// tempted to re-derive the hashing and get it subtly different.
func HashInviteToken(raw string) string { return hashToken(raw) }
