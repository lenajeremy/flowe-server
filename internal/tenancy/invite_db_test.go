package tenancy

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"workflow-ai/server/internal/database/models"
)

// Invites against a real Postgres. Opt-in via TEST_DATABASE_URL:
//
//	TEST_DATABASE_URL="host=localhost user=postgres password=postgres dbname=workflow_ai port=5434 sslmode=disable"
//
// The seat accounting is why these are DB-backed rather than unit tests. Selling a
// seat twice is the failure that matters, and it only appears under concurrency.

func dbForTest(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run invite tests")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.Organization{},
		&models.OrgMember{}, &models.OrgInvite{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// newUser creates a user with cleanup registered.
func newUser(t *testing.T, db *gorm.DB, email string) *models.User {
	t.Helper()
	u := models.User{Email: email}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("user_id = ?", u.ID.String()).Delete(&models.OrgMember{})
		db.Unscoped().Delete(&models.User{}, "id = ?", u.ID)
	})
	return &u
}

// newTeamOrg creates an org owned by a fresh user.
func newTeamOrg(t *testing.T, db *gorm.DB, seats int) (*models.Organization, *models.User) {
	t.Helper()
	owner := newUser(t, db, "owner-"+uuid.NewString()+"@example.test")
	org := models.Organization{
		Name: "Test Team", Slug: "team-" + uuid.NewString()[:8],
		Plan: models.PlanTeam, Personal: false, Seats: seats,
	}
	org.ID = uuid.New()
	if err := db.Create(&org).Error; err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := db.Create(&models.OrgMember{
		OrganizationID: org.ID.String(), UserID: owner.ID.String(), Role: models.RoleOwner,
	}).Error; err != nil {
		t.Fatalf("create owner membership: %v", err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("organization_id = ?", org.ID.String()).Delete(&models.OrgInvite{})
		db.Unscoped().Where("organization_id = ?", org.ID.String()).Delete(&models.OrgMember{})
		db.Unscoped().Delete(&models.Organization{}, "id = ?", org.ID)
	})
	return &org, owner
}

func TestPendingInvitesHoldSeatsOpen(t *testing.T) {
	db := dbForTest(t)
	// 3 seats: the owner takes one, so two invitations fit and a third must not.
	org, owner := newTeamOrg(t, db, 3)

	for i := 0; i < 2; i++ {
		if _, _, err := Invite(db, org.ID.String(), owner.ID.String(),
			uuid.NewString()+"@example.test", models.RoleMember, org.Seats); err != nil {
			t.Fatalf("invite %d: %v", i+1, err)
		}
	}
	// The seat is committed by the INVITE, not by acceptance. Counting only members
	// here would let an org invite twenty people onto three seats and be
	// over-subscribed the moment they all accepted.
	_, _, err := Invite(db, org.ID.String(), owner.ID.String(),
		uuid.NewString()+"@example.test", models.RoleMember, org.Seats)
	if !errors.Is(err, ErrNoSeats) {
		t.Fatalf("third invite: got %v, want ErrNoSeats", err)
	}

	usage, err := Seats(db, org.ID.String(), org.Seats)
	if err != nil {
		t.Fatalf("seats: %v", err)
	}
	if usage.Members != 1 || usage.PendingInvites != 2 || usage.Available() != 0 {
		t.Fatalf("unexpected usage: %+v (available %d)", usage, usage.Available())
	}
}

func TestConcurrentInvitesCannotOversubscribeSeats(t *testing.T) {
	db := dbForTest(t)
	// 4 seats, owner takes one, so exactly 3 invitations may succeed.
	org, owner := newTeamOrg(t, db, 4)

	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := Invite(db, org.ID.String(), owner.ID.String(),
				uuid.NewString()+"@example.test", models.RoleMember, org.Seats)
			if err == nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if granted != 3 {
		t.Fatalf("%d invitations issued against 3 free seats — seats were oversubscribed", granted)
	}
	usage, _ := Seats(db, org.ID.String(), org.Seats)
	if usage.Committed() > usage.Paid {
		t.Fatalf("committed %d seats against %d paid", usage.Committed(), usage.Paid)
	}
}

func TestReinvitingReplacesRatherThanStacking(t *testing.T) {
	db := dbForTest(t)
	org, owner := newTeamOrg(t, db, 3)
	email := uuid.NewString() + "@example.test"

	_, first, err := Invite(db, org.ID.String(), owner.ID.String(), email, models.RoleMember, org.Seats)
	if err != nil {
		t.Fatalf("first invite: %v", err)
	}
	_, second, err := Invite(db, org.ID.String(), owner.ID.String(), email, models.RoleAdmin, org.Seats)
	if err != nil {
		t.Fatalf("second invite: %v", err)
	}

	// One pending invite, so one seat held — not two for the same person.
	usage, _ := Seats(db, org.ID.String(), org.Seats)
	if usage.PendingInvites != 1 {
		t.Fatalf("%d pending invites for one address, want 1", usage.PendingInvites)
	}

	// The superseded token must no longer work, or the older email remains a way in
	// with the older role.
	invitee := newUser(t, db, email)
	if _, err := Accept(db, first, invitee); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("superseded token accepted: %v", err)
	}
	if _, err := Accept(db, second, invitee); err != nil {
		t.Fatalf("current token rejected: %v", err)
	}
	var m models.OrgMember
	db.Where("organization_id = ? AND user_id = ?", org.ID.String(), invitee.ID.String()).First(&m)
	if m.Role != models.RoleAdmin {
		t.Fatalf("role = %q, want admin from the newer invite", m.Role)
	}
}

func TestAcceptRequiresTheInvitedEmail(t *testing.T) {
	db := dbForTest(t)
	org, owner := newTeamOrg(t, db, 3)
	invited := uuid.NewString() + "@example.test"
	_, token, err := Invite(db, org.ID.String(), owner.ID.String(), invited, models.RoleMember, org.Seats)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	// A forwarded or leaked link must not admit whoever opens it.
	stranger := newUser(t, db, "stranger-"+uuid.NewString()+"@example.test")
	if _, err := Accept(db, token, stranger); !errors.Is(err, ErrWrongRecipient) {
		t.Fatalf("stranger accepted an invite for someone else: %v", err)
	}
	var members int64
	db.Model(&models.OrgMember{}).Where("organization_id = ?", org.ID.String()).Count(&members)
	if members != 1 {
		t.Fatalf("org has %d members after a rejected accept, want 1 (the owner)", members)
	}

	// The rightful recipient still works afterwards.
	right := newUser(t, db, invited)
	if _, err := Accept(db, token, right); err != nil {
		t.Fatalf("invited user rejected: %v", err)
	}
}

func TestAcceptingTwiceDoesNotDuplicateMembership(t *testing.T) {
	db := dbForTest(t)
	org, owner := newTeamOrg(t, db, 3)
	email := uuid.NewString() + "@example.test"
	_, token, _ := Invite(db, org.ID.String(), owner.ID.String(), email, models.RoleMember, org.Seats)
	invitee := newUser(t, db, email)

	if _, err := Accept(db, token, invitee); err != nil {
		t.Fatalf("first accept: %v", err)
	}
	// A double-clicked link, or a retried request. The second attempt fails because
	// the invite is consumed — what must NOT happen is a second membership row,
	// which would consume a second seat.
	_, err := Accept(db, token, invitee)
	if err != nil && !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("second accept: unexpected error %v", err)
	}
	var members int64
	db.Model(&models.OrgMember{}).
		Where("organization_id = ? AND user_id = ?", org.ID.String(), invitee.ID.String()).
		Count(&members)
	if members != 1 {
		t.Fatalf("%d membership rows for one user, want 1", members)
	}
}

func TestExpiredInviteIsRefused(t *testing.T) {
	db := dbForTest(t)
	org, owner := newTeamOrg(t, db, 3)
	email := uuid.NewString() + "@example.test"
	inv, token, _ := Invite(db, org.ID.String(), owner.ID.String(), email, models.RoleMember, org.Seats)

	// An email forwarded months later must not be a way in.
	db.Model(&models.OrgInvite{}).Where("id = ?", inv.ID).
		Update("expires_at", time.Now().Add(-time.Hour))

	invitee := newUser(t, db, email)
	if _, err := Accept(db, token, invitee); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("expired invite accepted: %v", err)
	}
	// And an expired invite stops holding its seat.
	usage, _ := Seats(db, org.ID.String(), org.Seats)
	if usage.PendingInvites != 0 {
		t.Fatalf("expired invite still holds a seat: %+v", usage)
	}
}

func TestRevokingFreesTheSeat(t *testing.T) {
	db := dbForTest(t)
	org, owner := newTeamOrg(t, db, 2) // owner + one
	email := uuid.NewString() + "@example.test"
	inv, token, err := Invite(db, org.ID.String(), owner.ID.String(), email, models.RoleMember, org.Seats)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if _, _, err := Invite(db, org.ID.String(), owner.ID.String(),
		uuid.NewString()+"@example.test", models.RoleMember, org.Seats); !errors.Is(err, ErrNoSeats) {
		t.Fatalf("expected the second invite to be refused, got %v", err)
	}

	if err := Revoke(db, org.ID.String(), inv.ID.String()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Seat freed, so another invitation now fits.
	if _, _, err := Invite(db, org.ID.String(), owner.ID.String(),
		uuid.NewString()+"@example.test", models.RoleMember, org.Seats); err != nil {
		t.Fatalf("invite after revoke: %v", err)
	}
	// And the revoked token is dead.
	invitee := newUser(t, db, email)
	if _, err := Accept(db, token, invitee); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("revoked token accepted: %v", err)
	}
}

func TestRemovingAMemberFreesTheirSeat(t *testing.T) {
	db := dbForTest(t)
	org, owner := newTeamOrg(t, db, 2)
	email := uuid.NewString() + "@example.test"
	_, token, _ := Invite(db, org.ID.String(), owner.ID.String(), email, models.RoleMember, org.Seats)
	invitee := newUser(t, db, email)
	if _, err := Accept(db, token, invitee); err != nil {
		t.Fatalf("accept: %v", err)
	}

	if usage, _ := Seats(db, org.ID.String(), org.Seats); usage.Available() != 0 {
		t.Fatalf("expected no free seats, got %+v", usage)
	}
	if err := RemoveMember(db, org.ID.String(), invitee.ID.String()); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if usage, _ := Seats(db, org.ID.String(), org.Seats); usage.Available() != 1 {
		t.Fatalf("seat not freed after removal: %+v", usage)
	}
}

func TestTheLastOwnerCannotBeRemoved(t *testing.T) {
	db := dbForTest(t)
	org, owner := newTeamOrg(t, db, 3)
	// An org with no owner has nobody who can manage billing or membership, and no
	// self-service way back.
	if err := RemoveMember(db, org.ID.String(), owner.ID.String()); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("removed the only owner: %v", err)
	}
}

func TestOnlyOwnersAndAdminsCanManageMembers(t *testing.T) {
	db := dbForTest(t)
	org, owner := newTeamOrg(t, db, 3)
	if !CanManageMembers(db, org.ID.String(), owner.ID.String()) {
		t.Fatal("the owner cannot manage members")
	}
	// A plain member must not be able to invite people onto seats the org pays for,
	// nor remove colleagues.
	email := uuid.NewString() + "@example.test"
	_, token, _ := Invite(db, org.ID.String(), owner.ID.String(), email, models.RoleMember, org.Seats)
	plain := newUser(t, db, email)
	if _, err := Accept(db, token, plain); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if CanManageMembers(db, org.ID.String(), plain.ID.String()) {
		t.Fatal("a plain member can manage members")
	}
	// And a complete stranger certainly cannot.
	stranger := newUser(t, db, "outsider-"+uuid.NewString()+"@example.test")
	if CanManageMembers(db, org.ID.String(), stranger.ID.String()) {
		t.Fatal("a non-member can manage members")
	}
}

func TestOwnerRoleCannotBeInvited(t *testing.T) {
	db := dbForTest(t)
	org, owner := newTeamOrg(t, db, 3)
	// Transferring ownership is a different operation with different consequences;
	// it must not be reachable by sending an email.
	if _, _, err := Invite(db, org.ID.String(), owner.ID.String(),
		uuid.NewString()+"@example.test", models.RoleOwner, org.Seats); err == nil {
		t.Fatal("an owner invitation was accepted")
	}
}

func TestInvitingAnExistingMemberIsRefused(t *testing.T) {
	db := dbForTest(t)
	org, owner := newTeamOrg(t, db, 5)
	if _, _, err := Invite(db, org.ID.String(), owner.ID.String(),
		owner.Email, models.RoleMember, org.Seats); !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("re-inviting an existing member: got %v, want ErrAlreadyMember", err)
	}
}

func TestOnlyTheHashIsStored(t *testing.T) {
	db := dbForTest(t)
	org, owner := newTeamOrg(t, db, 3)
	inv, token, err := Invite(db, org.ID.String(), owner.ID.String(),
		uuid.NewString()+"@example.test", models.RoleMember, org.Seats)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	// A database dump must not be replayable into org membership.
	var stored string
	db.Raw(`SELECT token_hash FROM org_invites WHERE id = ?`, inv.ID).Scan(&stored)
	if stored == token {
		t.Fatal("the raw token is stored in the database")
	}
	if stored != hashToken(token) {
		t.Fatalf("stored value is not the token's hash")
	}
}

func TestJoiningATeamChangesWhichOrgYouActIn(t *testing.T) {
	db := dbForTest(t)
	org, owner := newTeamOrg(t, db, 3)
	email := uuid.NewString() + "@example.test"
	_, token, err := Invite(db, org.ID.String(), owner.ID.String(), email, models.RoleMember, org.Seats)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	// The invitee has a personal org of their own, as everyone does.
	invitee := newUser(t, db, email)
	personal, err := Provision(db, invitee)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM credit_ledger WHERE organization_id = ?`, personal.ID)
		db.Exec(`DELETE FROM credit_balances WHERE organization_id = ?`, personal.ID)
		db.Unscoped().Where("id = ?", personal.ID).Delete(&models.Organization{})
	})
	if got, _ := OrgForUser(db, invitee.ID.String()); got.ID != personal.ID {
		t.Fatal("before joining, the personal org should be the acting org")
	}

	if _, err := Accept(db, token, invitee); err != nil {
		t.Fatalf("accept: %v", err)
	}

	// Caught in live testing: with the personal org winning, accepting an invitation
	// did nothing observable — the new member stayed on the free plan and could not
	// reach the team's work, with no switcher to get out of it.
	got, err := OrgForUser(db, invitee.ID.String())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.ID != org.ID {
		t.Fatalf("acting org is %s (personal=%v), want the team org %s",
			got.ID, got.Personal, org.ID)
	}
}

func TestAnOrgWithASecondMemberIsNoLongerPersonal(t *testing.T) {
	db := dbForTest(t)
	org, owner := newTeamOrg(t, db, 3)
	// Force the starting state Provision actually produces, rather than the one the
	// test helper sets — the whole point is that Provision leaves this true.
	db.Model(&models.Organization{}).Where("id = ?", org.ID).Update("personal", true)

	email := uuid.NewString() + "@example.test"
	_, token, _ := Invite(db, org.ID.String(), owner.ID.String(), email, models.RoleMember, org.Seats)
	invitee := newUser(t, db, email)
	if _, err := Accept(db, token, invitee); err != nil {
		t.Fatalf("accept: %v", err)
	}

	var after models.Organization
	if err := db.Where("id = ?", org.ID).First(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after.Personal {
		t.Fatal("an org with two members is still flagged personal, which makes every " +
			"query ordering by that flag a no-op")
	}
}
