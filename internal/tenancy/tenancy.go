package tenancy

import (
	"crypto/md5"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"workflow-ai/server/internal/billing/credits"
	"workflow-ai/server/internal/database/models"
)

// Resolving and provisioning the tenant boundary.
//
// Every user has exactly one personal organization, created at signup. Teams and
// invites are not built; this package exists so that "which org owns this?" has
// exactly one answer computed in exactly one place.

var ErrNoOrg = errors.New("user has no organization")

// PersonalOrgID derives a user's personal org id from their user id.
//
// Deterministic on purpose, and it must stay byte-identical to the SQL used by
// the backfill (md5('org:' || user_id::text)::uuid in migrate_org.go). Postgres
// casts md5's 32 hex characters straight to a uuid, so hashing the same string
// here and taking the raw 16 bytes yields the same value. Idempotency across both
// paths depends on that agreement — changing either one alone orphans every row.
func PersonalOrgID(userID string) uuid.UUID {
	sum := md5.Sum([]byte("org:" + strings.ToLower(strings.TrimSpace(userID))))
	id, err := uuid.FromBytes(sum[:])
	if err != nil {
		// Impossible: md5 is always 16 bytes, which is exactly a uuid.
		panic(fmt.Sprintf("tenancy: md5 is not uuid-sized: %v", err))
	}
	return id
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug builds the org's URL-safe name: the email local part plus a short hash
// suffix. The suffix is not decoration — alex@a.com and alex@b.com would
// otherwise collide on a unique index and the second signup would fail.
func Slug(email, userID string) string {
	local := email
	if i := strings.Index(email, "@"); i > 0 {
		local = email[:i]
	}
	base := strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(local), "-"), "-")
	if base == "" {
		base = "org"
	}
	sum := md5.Sum([]byte(strings.ToLower(userID)))
	return fmt.Sprintf("%s-%x", base, sum[:3])
}

// Provision creates a user's personal org, membership, opening credit balance and
// signup grant, or returns the existing org if it is already there.
//
// Idempotent, and safe to call on every login: the org id is derived rather than
// generated, so a concurrent duplicate collides on the primary key instead of
// creating a second org.
func Provision(db *gorm.DB, user *models.User) (*models.Organization, error) {
	orgID := PersonalOrgID(user.ID.String())

	var existing models.Organization
	if err := db.Where("id = ?", orgID).First(&existing).Error; err == nil {
		return &existing, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	name := strings.TrimSpace(user.Name)
	if name == "" {
		name = strings.SplitN(user.Email, "@", 2)[0]
	}
	org := models.Organization{
		Name:     name,
		Slug:     Slug(user.Email, user.ID.String()),
		Plan:     models.PlanFree,
		Personal: true,
	}
	org.ID = orgID

	err := db.Transaction(func(tx *gorm.DB) error {
		// Two signups racing on the same user cannot both win: the id is derived,
		// so the loser hits the primary key and is treated as a no-op.
		if err := tx.Where("id = ?", orgID).
			FirstOrCreate(&org, models.Organization{}).Error; err != nil {
			return err
		}
		member := models.OrgMember{
			OrganizationID: orgID.String(),
			UserID:         user.ID.String(),
			Role:           models.RoleOwner,
		}
		if err := tx.Where("organization_id = ? AND user_id = ?", orgID, user.ID).
			FirstOrCreate(&member).Error; err != nil {
			return err
		}
		return grantOpening(tx, orgID.String(), models.PlanFree)
	})
	if err != nil {
		// A racing caller may have created it between the check and the insert.
		var raced models.Organization
		if db.Where("id = ?", orgID).First(&raced).Error == nil {
			return &raced, nil
		}
		return nil, err
	}
	return &org, nil
}

// grantOpening seeds the balance row and the ledger entry that explains it. The
// balance must always be derivable from ledger history, so credits never appear
// without a row saying where they came from.
func grantOpening(tx *gorm.DB, orgID string, plan models.Plan) error {
	grant := credits.PlanGrant(plan)
	now := time.Now()
	bal := models.CreditBalance{
		OrganizationID: orgID,
		Balance:        grant,
		LastGrantAt:    &now,
	}
	if err := tx.Where("organization_id = ?", orgID).FirstOrCreate(&bal).Error; err != nil {
		return err
	}
	ref := "signup:" + orgID
	entry := models.CreditLedger{
		OrganizationID: orgID,
		Delta:          grant,
		Reason:         models.ReasonSignupGrant,
		ExternalRef:    &ref,
	}
	// ExternalRef is unique, so a repeated call cannot grant twice.
	if err := tx.Where("external_ref = ?", ref).FirstOrCreate(&entry).Error; err != nil {
		return err
	}
	return nil
}

// OrgForUser resolves the org a request acts within.
//
// Falls back to provisioning rather than failing: a user who predates the tenancy
// migration, or whose signup was interrupted between the two writes, should get an
// org on their next request instead of a 500 they cannot clear.
func OrgForUser(db *gorm.DB, userID string) (*models.Organization, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, ErrNoOrg
	}
	var org models.Organization
	err := db.Raw(`
		SELECT o.* FROM organizations o
		JOIN org_members m ON m.organization_id = o.id
		WHERE m.user_id = ? AND o.deleted_at IS NULL
		ORDER BY o.personal DESC, o.created_at ASC
		LIMIT 1`, userID).Scan(&org).Error
	if err != nil {
		return nil, err
	}
	if org.ID != uuid.Nil {
		return &org, nil
	}

	var user models.User
	if err := db.Where("id = ?", userID).First(&user).Error; err != nil {
		return nil, ErrNoOrg
	}
	return Provision(db, &user)
}
