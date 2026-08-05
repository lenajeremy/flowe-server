package models

import "time"

// OrgInvite is a pending invitation to join an organization.
//
// Modelled on LoginCode: only a HASH of the token is stored, so a database dump
// cannot be replayed into org membership. The raw token exists once, in the email.
//
// Kept as its own table rather than a nullable-user OrgMember row. A membership
// row for someone who has not accepted would be counted by every seat check and
// every "who is in this org" query, and each of those would then need to remember
// to exclude it — the kind of implicit condition that gets forgotten exactly once
// and lets an unaccepted invite consume a paid seat forever.
type OrgInvite struct {
	BaseModel
	OrganizationID string `json:"organization_id" gorm:"type:uuid;not null;index"`
	// Email is the address invited, always lowercased (see auth.NormalizeEmail).
	// Part of a unique index with the org so re-inviting the same person replaces
	// the pending invite instead of accumulating them.
	Email string `json:"email" gorm:"not null;index:idx_invite_org_email,unique,composite:organization_id"`
	// Role the invitee will hold. Validated on send, not on accept, so a role
	// removed from the vocabulary later cannot be granted by an old invite.
	Role OrgRole `json:"role" gorm:"type:varchar(20);not null;default:'member'"`
	// InvitedBy records who sent it, for the audit trail teams will need.
	InvitedBy string `json:"invited_by" gorm:"type:uuid;not null"`
	// TokenHash is sha256 of the raw token. Never the token itself.
	TokenHash  string     `json:"-" gorm:"not null;uniqueIndex"`
	ExpiresAt  time.Time  `json:"expires_at" gorm:"not null;index"`
	AcceptedAt *time.Time `json:"accepted_at"`
	// RevokedAt marks an invite withdrawn before acceptance. Kept rather than
	// deleted so "we sent this and then took it back" stays visible.
	RevokedAt *time.Time `json:"revoked_at"`
}

// Pending reports whether an invite can still be accepted.
func (i OrgInvite) Pending() bool {
	return i.AcceptedAt == nil && i.RevokedAt == nil && time.Now().Before(i.ExpiresAt)
}
