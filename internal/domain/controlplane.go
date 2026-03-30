package domain

import (
	"errors"
	"time"
)

var (
	ErrIdempotencyConflict     = errors.New("idempotency key reused with different request")
	ErrIdempotencyInProgress   = errors.New("idempotent request already in progress")
	ErrFunctionVersionNotReady = errors.New("function version not ready")
	ErrBuildLogsNotReady       = errors.New("build logs not ready")
	ErrExecutionResultNotReady = errors.New("execution result not ready")
)

type IdempotencyStatus string

const (
	IdempotencyStatusPending   IdempotencyStatus = "pending"
	IdempotencyStatusCompleted IdempotencyStatus = "completed"
)

type IdempotencyRecord struct {
	Scope       string            `json:"scope"`
	ProjectID   string            `json:"projectId"`
	Key         string            `json:"key"`
	RequestHash string            `json:"requestHash"`
	ResourceID  string            `json:"resourceId,omitempty"`
	Resource    string            `json:"resource,omitempty"`
	Status      IdempotencyStatus `json:"status"`
	CreatedAt   time.Time         `json:"createdAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
}

type WebhookTrigger struct {
	Token             string    `json:"token"`
	ProjectID         string    `json:"projectId"`
	FunctionVersionID string    `json:"functionVersionId"`
	Description       string    `json:"description,omitempty"`
	Enabled           bool      `json:"enabled"`
	CreatedAt         time.Time `json:"createdAt"`
}
