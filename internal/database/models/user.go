package models

import "time"

// User is an account holder. Email is always stored lowercased (see
// auth.NormalizeEmail); GoogleID is a pointer so accounts without Google
// linked don't collide on the unique index.
type User struct {
	BaseModel
	Email     string  `json:"email"      gorm:"not null;uniqueIndex"`
	Name      string  `json:"name"`
	AvatarURL string  `json:"avatar_url"`
	GoogleID  *string `json:"-"          gorm:"uniqueIndex"`
}

// LoginCode is a single passwordless sign-in attempt: the email gets a
// 6-digit code and a magic-link token, either of which completes auth.
// Only hashes are stored; a row is dead once consumed, expired, or after
// too many wrong code attempts.
type LoginCode struct {
	BaseModel
	Email      string     `json:"email"      gorm:"not null;index"`
	CodeHash   string     `json:"-"          gorm:"not null"`
	TokenHash  string     `json:"-"          gorm:"not null;uniqueIndex"`
	ExpiresAt  time.Time  `json:"expires_at" gorm:"not null"`
	ConsumedAt *time.Time `json:"-"`
	Attempts   int        `json:"-"          gorm:"not null;default:0"`
}
