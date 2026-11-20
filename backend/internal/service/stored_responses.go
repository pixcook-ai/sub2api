package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrStoredResponseNotFound    = errors.New("response not found")
	ErrStoredResponseInvalid     = errors.New("invalid stored response")
	ErrStoredResponseUnsupported = errors.New("unsupported stored response combination")
	ErrStoredResponseQuota       = errors.New("response storage quota exceeded")
	ErrStoredResponseTooLarge    = errors.New("response storage limit exceeded")
	ErrStoredResponseLease       = errors.New("response storage lease lost")
	ErrStoredResponseConflict    = errors.New("response operation conflict")
	ErrStoredResponsesDisabled   = errors.New("response storage is disabled")
)

// StoredResponseOwner identifies a currently authorized caller. APIKeyID is audit
// provenance only: the same user can manage resources with a replacement key.
type StoredResponseOwner struct{ UserID, APIKeyID, GroupID int64 }

// StoredResponseLimits are finite storage budgets. Deleted tombstones still
// consume a row until their original expiry, but retain no response content.
type StoredResponseLimits struct {
	MaxResponseBytes int64
	UserMaxRows      int64
	UserMaxBytes     int64
	GlobalMaxRows    int64
	GlobalMaxBytes   int64
}

type StoredResponseConfig struct {
	Enabled       bool
	RetentionDays int
	LeaseSeconds  int
	Limits        StoredResponseLimits
}

func DefaultStoredResponseConfig() StoredResponseConfig {
	return StoredResponseConfig{RetentionDays: 30, LeaseSeconds: 120, Limits: StoredResponseLimits{
		MaxResponseBytes: 1 << 20, UserMaxRows: 1000, UserMaxBytes: 64 << 20,
		GlobalMaxRows: 100000, GlobalMaxBytes: 1 << 30,
	}}
}

// StoredResponse holds public client content separately from a private backend
// binding. It never contains authentication headers, credentials or native replay.
type StoredResponse struct {
	PublicID                          string
	UserID, APIKeyID, GroupID         int64
	Model, ResolvedModel              string
	AccountID                         int64
	Binding                           string
	CreatedAt, ExpiresAt              time.Time
	State, ExecutionState             string
	Input, Parameters, PublicResponse json.RawMessage
	ContentBytes, ReservedBytes       int64
	LeaseToken                        string
	LeaseUntil                        time.Time
	Version                           int64
	TerminalHash                      string
	DeletedAt                         *time.Time
}

// StoredResponseTerminal is already validated and normalized client content.
// TerminalHash makes an acknowledged duplicate Finish idempotent, never a retry
// of model execution. The repository checks its lease/version before committing.
type StoredResponseTerminal struct {
	State, ExecutionState string
	PublicResponse        json.RawMessage
	TerminalHash          string
}

// StoredResponseRepository is the persistent storage seam. Implementations must
// atomically enforce quotas and owner/group boundaries across processes. All
// lease and expiry comparisons use database time. Reserve assigns database times
// and version; Finalize accepts an identical already-committed token/hash only.
type StoredResponseRepository interface {
	Reserve(context.Context, *StoredResponse, StoredResponseLimits, time.Duration, time.Duration) error
	GetForOwner(context.Context, int64, int64, string) (*StoredResponse, error)
	GetManagementModel(context.Context, int64, int64, string) (string, error)
	Finalize(context.Context, string, string, int64, StoredResponseTerminal) (*StoredResponse, error)
	Renew(context.Context, string, string, int64, time.Duration) error
	Delete(context.Context, int64, int64, string) error
	Sweep(context.Context, int) (int, error)
}
