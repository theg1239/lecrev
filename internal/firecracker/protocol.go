package firecracker

import (
	"encoding/json"
	"time"
)

type GuestAction string

const (
	GuestActionPing    GuestAction = "ping"
	GuestActionPrepare GuestAction = "prepare"
	GuestActionExecute GuestAction = "execute"
)

type GuestRequest struct {
	Action     GuestAction             `json:"action"`
	Ping       *GuestPingRequest       `json:"ping,omitempty"`
	Prepare    *GuestPrepareRequest    `json:"prepare,omitempty"`
	Invocation *GuestInvocationRequest `json:"invocation,omitempty"`
}

type GuestResponse struct {
	Error      string                   `json:"error,omitempty"`
	Ping       *GuestPingResponse       `json:"ping,omitempty"`
	Prepare    *GuestPrepareResponse    `json:"prepare,omitempty"`
	Invocation *GuestInvocationResponse `json:"invocation,omitempty"`
}

type GuestPingRequest struct{}

type GuestPingResponse struct {
	Ready bool `json:"ready"`
}

type GuestPrepareRequest struct {
	FunctionID     string            `json:"functionId"`
	Entrypoint     string            `json:"entrypoint"`
	ArtifactBundle []byte            `json:"artifactBundle"`
	Env            map[string]string `json:"env,omitempty"`
}

type GuestPrepareResponse struct {
	Prepared       bool   `json:"prepared"`
	WorkerPrepared bool   `json:"workerPrepared,omitempty"`
	Logs           string `json:"logs,omitempty"`
}

type GuestInvocationRequest struct {
	AttemptID       string            `json:"attemptId"`
	JobID           string            `json:"jobId"`
	FunctionID      string            `json:"functionId"`
	Entrypoint      string            `json:"entrypoint"`
	ArtifactBundle  []byte            `json:"artifactBundle,omitempty"`
	UsePreparedRoot bool              `json:"usePreparedRoot,omitempty"`
	Payload         json.RawMessage   `json:"payload"`
	Env             map[string]string `json:"env"`
	TimeoutMillis   int64             `json:"timeoutMillis"`
	Region          string            `json:"region"`
	HostID          string            `json:"hostId"`
}

type GuestInvocationResponse struct {
	ExitCode   int             `json:"exitCode"`
	Logs       string          `json:"logs"`
	Output     json.RawMessage `json:"output"`
	Error      string          `json:"error,omitempty"`
	StartedAt  time.Time       `json:"startedAt"`
	FinishedAt time.Time       `json:"finishedAt"`
}
