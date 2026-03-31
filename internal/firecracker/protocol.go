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

type StreamKind string

const (
	StreamKindNone StreamKind = ""
	StreamKindHTTP StreamKind = "http"
)

type HTTPStreamEventType string

const (
	HTTPStreamEventStart HTTPStreamEventType = "http_start"
	HTTPStreamEventChunk HTTPStreamEventType = "http_chunk"
	HTTPStreamEventEnd   HTTPStreamEventType = "http_end"
)

type HTTPStreamEvent struct {
	Type       HTTPStreamEventType `json:"type"`
	StatusCode int                 `json:"statusCode,omitempty"`
	Headers    map[string]string   `json:"headers,omitempty"`
	Chunk      []byte              `json:"chunk,omitempty"`
}

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

type GuestStreamFrameType string

const (
	GuestStreamFrameHTTPEvent  GuestStreamFrameType = "http_event"
	GuestStreamFrameInvocation GuestStreamFrameType = "invocation"
)

type GuestStreamFrame struct {
	Type       GuestStreamFrameType     `json:"type"`
	HTTPEvent  *HTTPStreamEvent         `json:"httpEvent,omitempty"`
	Invocation *GuestInvocationResponse `json:"invocation,omitempty"`
	Error      string                   `json:"error,omitempty"`
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
	EnableStreaming bool              `json:"enableStreaming,omitempty"`
	StreamKind      StreamKind        `json:"streamKind,omitempty"`
}

type GuestInvocationResponse struct {
	ExitCode   int             `json:"exitCode"`
	Logs       string          `json:"logs"`
	Output     json.RawMessage `json:"output"`
	Error      string          `json:"error,omitempty"`
	StartedAt  time.Time       `json:"startedAt"`
	FinishedAt time.Time       `json:"finishedAt"`
}
