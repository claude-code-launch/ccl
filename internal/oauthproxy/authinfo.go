package oauthproxy

import "time"

// Status represents the lifecycle state of an AuthInfo entry. It mirrors the
// status vocabulary the doctor surface consumes: a credential is active,
// intentionally disabled, temporarily in error, or in some pending/unknown
// state.
type Status string

const (
	// StatusUnknown means the auth state could not be determined.
	StatusUnknown Status = "unknown"
	// StatusActive indicates the credential is valid and ready for use.
	StatusActive Status = "active"
	// StatusPending indicates the credential is waiting for an external action.
	StatusPending Status = "pending"
	// StatusError indicates the credential is temporarily unavailable.
	StatusError Status = "error"
	// StatusDisabled marks the credential as intentionally disabled.
	StatusDisabled Status = "disabled"
)

// QuotaState captures recent quota information for a credential, surfaced to
// the doctor diagnostics when a backend reports it has been rate-limited.
type QuotaState struct {
	// Exceeded indicates the credential recently hit a quota error.
	Exceeded bool
	// Reason provides an optional provider-specific description.
	Reason string
	// NextRecoverAt is when the credential may become available again.
	NextRecoverAt time.Time
	// BackoffLevel stores the progressive cooldown exponent for rate limits.
	BackoffLevel int
}

// AuthInfo is ccl's self-owned view of a loaded OAuth credential, replacing the
// CLIProxyAPI coreauth.Auth type in Runtime.ListAuths. Only the fields the
// doctor diagnostics and the per-backend listAuths implementations need are
// carried; fields are named to match the previous coreauth.Auth surface so the
// doctor consumers and tests need no semantic changes.
type AuthInfo struct {
	ID             string
	Provider       string
	FileName       string
	Label          string
	Status         Status
	StatusMessage  string
	Disabled       bool
	Unavailable    bool
	Metadata       map[string]any
	Quota          QuotaState
	NextRetryAfter time.Time
}
