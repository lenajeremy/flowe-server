package models

import "time"

// The tenant boundary.
//
// Every user gets a personal organization of one at signup. Teams, roles and
// invites are not built yet — this is only the boundary, added while the database
// is small enough that it costs one migration instead of a backfill, a dual-read
// period, and surgery on live unique indexes.
//
// The org is also the natural owner of everything that is not per-user: the credit
// balance and, later, sending domains.

// Plan is a billing tier. The zero value is deliberately the free plan so a row
// that predates billing, or one written by a path that forgets to set it, is never
// accidentally entitled.
type Plan string

const (
	PlanFree     Plan = "free"
	PlanPro      Plan = "pro"
	PlanTeam     Plan = "team"
	PlanBusiness Plan = "business"
)

// Organization is the unit that owns work and pays for it.
type Organization struct {
	BaseModel
	Name string `json:"name" gorm:"not null"`
	// Slug is unique across all orgs, so personal orgs derived from an email
	// local-part need a disambiguating suffix (see auth.OrgSlug).
	Slug string `json:"slug" gorm:"not null;uniqueIndex"`
	Plan Plan   `json:"plan" gorm:"type:varchar(20);not null;default:'free'"`
	// Personal marks an org auto-created for a single user. It exists so the
	// billing UI can say "your account" rather than inventing a team, and so a
	// future "convert to team" path knows what it is converting.
	Personal bool `json:"personal" gorm:"not null;default:true"`

	// Stripe linkage. Both are empty until the org first reaches Checkout.
	StripeCustomerID     string `json:"-" gorm:"index"`
	StripeSubscriptionID string `json:"-" gorm:"index"`
	// PlanStatus mirrors the Stripe subscription status verbatim rather than
	// being interpreted at write time, so a status Stripe adds later does not
	// silently read as "active". Entitlement is decided at read time.
	PlanStatus string `json:"plan_status" gorm:"type:varchar(32)"`
	// CurrentPeriodEnd is when the paid period lapses — also when the monthly
	// credit grant renews.
	CurrentPeriodEnd *time.Time `json:"current_period_end"`
	// CancelAtPeriodEnd means the customer has cancelled but is still entitled
	// until CurrentPeriodEnd. Downgrading immediately on cancel would take away
	// something already paid for.
	CancelAtPeriodEnd bool `json:"cancel_at_period_end" gorm:"not null;default:false"`
	// BillingCountry is the two-letter country Stripe resolved at Checkout. Kept
	// for reporting only; the price the customer actually paid comes from Stripe.
	BillingCountry string `json:"billing_country" gorm:"type:varchar(2)"`
	// Seats is the subscription quantity, for plans billed per seat. It drives both
	// the member cap and the credit allowance, which is what keeps revenue coupled
	// to cost: seats alone meter nothing we actually spend, so an allowance that
	// did not scale with them would make a small team running many agents our most
	// expensive customer and our cheapest.
	//
	// Always at least 1. Read from the Stripe subscription's quantity rather than
	// counted from org_members, so a team that has not finished inviting people
	// still gets the allowance it paid for.
	Seats int `json:"seats" gorm:"not null;default:1"`
	// PendingPlan and PendingPlanAt describe a change the customer has already asked
	// for that has not taken effect yet.
	//
	// A downgrade is scheduled for the end of the period they already paid for, so
	// between the request and that date the org is STILL on its current plan with its
	// full allowance. Recording the pending state is what lets the app say so —
	// otherwise the customer clicks "switch to Pro", sees Team everywhere, and
	// reasonably concludes it did not work.
	PendingPlan   Plan       `json:"pending_plan,omitempty" gorm:"type:varchar(20)"`
	PendingSeats  int        `json:"pending_seats,omitempty"`
	PendingPlanAt *time.Time `json:"pending_plan_at,omitempty"`
}

// OrgRole is a member's authority within one org. Only Owner is issued today;
// the rest exist so the column does not need widening when teams ship.
type OrgRole string

const (
	RoleOwner  OrgRole = "owner"
	RoleAdmin  OrgRole = "admin"
	RoleMember OrgRole = "member"
)

// OrgMember joins a user to an org. A user may belong to several, but exactly one
// is their personal org.
type OrgMember struct {
	BaseModel
	OrganizationID string  `json:"organization_id" gorm:"type:uuid;not null;uniqueIndex:idx_org_member,priority:1"`
	UserID         string  `json:"user_id"         gorm:"type:uuid;not null;uniqueIndex:idx_org_member,priority:2;index"`
	Role           OrgRole `json:"role"            gorm:"type:varchar(20);not null;default:'member'"`
	// CreditLimit caps what this person may spend of the org's allowance in one
	// period. Zero means "use the equal share" — the org's allowance divided by its
	// seats — so the common case needs no per-member row maintenance and a seat
	// change re-splits automatically.
	//
	// A cap rather than a sub-balance: the org keeps ONE balance and one ledger, so
	// the invariant that balance equals sum(delta) still holds and there is nothing
	// to reconcile between wallets. Spend is checked against the person's total for
	// the period, which the ledger already records.
	CreditLimit int64 `json:"credit_limit" gorm:"not null;default:0"`
}
