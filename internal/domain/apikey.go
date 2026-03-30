package domain

import "time"

type APIKey struct {
	KeyHash     string    `json:"-"`
	TenantID    string    `json:"tenantId"`
	Description string    `json:"description,omitempty"`
	IsAdmin     bool      `json:"isAdmin"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"createdAt"`
	LastUsedAt  time.Time `json:"lastUsedAt,omitempty"`
}
