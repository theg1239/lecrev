package firecracker

import (
	"context"
	"encoding/json"
	"time"
)

type ExecuteRequest struct {
	AttemptID      string
	JobID          string
	FunctionID     string
	Entrypoint     string
	ArtifactBundle []byte
	Payload        json.RawMessage
	Env            map[string]string
	Timeout        time.Duration
	MemoryMB       int
	NetworkPolicy  string
	Region         string
	HostID         string
}

type ExecuteResult struct {
	ExitCode         int
	Logs             string
	Output           json.RawMessage
	SnapshotEligible bool
	StartedAt        time.Time
	FinishedAt       time.Time
}

type Driver interface {
	Name() string
	Execute(ctx context.Context, req ExecuteRequest) (*ExecuteResult, error)
}
