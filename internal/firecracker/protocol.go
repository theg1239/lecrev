package firecracker

import (
	"encoding/json"
	"time"
)

type GuestInvocationRequest struct {
	AttemptID      string            `json:"attemptId"`
	JobID          string            `json:"jobId"`
	FunctionID     string            `json:"functionId"`
	Entrypoint     string            `json:"entrypoint"`
	ArtifactBundle []byte            `json:"artifactBundle"`
	Payload        json.RawMessage   `json:"payload"`
	Env            map[string]string `json:"env"`
	TimeoutMillis  int64             `json:"timeoutMillis"`
	Region         string            `json:"region"`
	HostID         string            `json:"hostId"`
}

type GuestInvocationResponse struct {
	ExitCode   int             `json:"exitCode"`
	Logs       string          `json:"logs"`
	Output     json.RawMessage `json:"output"`
	Error      string          `json:"error,omitempty"`
	StartedAt  time.Time       `json:"startedAt"`
	FinishedAt time.Time       `json:"finishedAt"`
}
