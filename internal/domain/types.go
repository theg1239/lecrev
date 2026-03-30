package domain

import (
	"encoding/json"
	"time"
)

type SourceType string

const (
	SourceTypeBundle SourceType = "bundle"
	SourceTypeGit    SourceType = "git"
)

type NetworkPolicy string

const (
	NetworkPolicyNone      NetworkPolicy = "none"
	NetworkPolicyAllowlist NetworkPolicy = "allowlist"
	NetworkPolicyFull      NetworkPolicy = "full"
)

type FunctionState string

const (
	FunctionStateBuilding FunctionState = "building"
	FunctionStateReady    FunctionState = "ready"
	FunctionStateFailed   FunctionState = "failed"
)

type JobState string

const (
	JobStateQueued     JobState = "queued"
	JobStateScheduling JobState = "scheduling"
	JobStateAssigned   JobState = "assigned"
	JobStateRunning    JobState = "running"
	JobStateRetrying   JobState = "retrying"
	JobStateSucceeded  JobState = "succeeded"
	JobStateFailed     JobState = "failed"
)

type AttemptState string

const (
	AttemptStateAssigned  AttemptState = "assigned"
	AttemptStateStarting  AttemptState = "starting"
	AttemptStateRunning   AttemptState = "running"
	AttemptStateSucceeded AttemptState = "succeeded"
	AttemptStateFailed    AttemptState = "failed"
)

type StartMode string

const (
	StartModeCold         StartMode = "cold"
	StartModeBlankWarm    StartMode = "blank-warm"
	StartModeFunctionWarm StartMode = "function-warm"
)

type HostState string

const (
	HostStateActive   HostState = "active"
	HostStateDraining HostState = "draining"
	HostStateDown     HostState = "down"
)

type Project struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenantId"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

type DeploySource struct {
	Type         SourceType        `json:"type"`
	BundleBase64 string            `json:"bundleBase64,omitempty"`
	InlineFiles  map[string]string `json:"inlineFiles,omitempty"`
	GitURL       string            `json:"gitUrl,omitempty"`
	GitRef       string            `json:"gitRef,omitempty"`
	SubPath      string            `json:"subPath,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type DeployRequest struct {
	ProjectID      string        `json:"projectId"`
	Name           string        `json:"name"`
	Runtime        string        `json:"runtime"`
	Entrypoint     string        `json:"entrypoint"`
	MemoryMB       int           `json:"memoryMb"`
	TimeoutSec     int           `json:"timeoutSec"`
	NetworkPolicy  NetworkPolicy `json:"networkPolicy"`
	Regions        []string      `json:"regions"`
	EnvRefs        []string      `json:"envRefs"`
	MaxRetries     int           `json:"maxRetries"`
	IdempotencyKey string        `json:"idempotencyKey"`
	Source         DeploySource  `json:"source"`
}

type Artifact struct {
	Digest     string               `json:"digest"`
	SizeBytes  int64                `json:"sizeBytes"`
	BundleKey  string               `json:"bundleKey"`
	StartupKey string               `json:"startupKey"`
	Regions    map[string]time.Time `json:"regions"`
	CreatedAt  time.Time            `json:"createdAt"`
}

type FunctionVersion struct {
	ID             string        `json:"id"`
	ProjectID      string        `json:"projectId"`
	Name           string        `json:"name"`
	Runtime        string        `json:"runtime"`
	Entrypoint     string        `json:"entrypoint"`
	MemoryMB       int           `json:"memoryMb"`
	TimeoutSec     int           `json:"timeoutSec"`
	NetworkPolicy  NetworkPolicy `json:"networkPolicy"`
	Regions        []string      `json:"regions"`
	EnvRefs        []string      `json:"envRefs"`
	MaxRetries     int           `json:"maxRetries"`
	BuildJobID     string        `json:"buildJobId,omitempty"`
	SourceType     SourceType    `json:"sourceType"`
	ArtifactDigest string        `json:"artifactDigest"`
	State          FunctionState `json:"state"`
	CreatedAt      time.Time     `json:"createdAt"`
}

type BuildJob struct {
	ID                string    `json:"id"`
	FunctionVersionID string    `json:"functionVersionId"`
	TargetRegion      string    `json:"targetRegion,omitempty"`
	State             string    `json:"state"`
	Error             string    `json:"error,omitempty"`
	Request           []byte    `json:"-"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

type JobResult struct {
	ExitCode   int             `json:"exitCode"`
	Logs       string          `json:"logs"`
	Output     json.RawMessage `json:"output"`
	HostID     string          `json:"hostId"`
	Region     string          `json:"region"`
	StartedAt  time.Time       `json:"startedAt"`
	FinishedAt time.Time       `json:"finishedAt"`
}

type ExecutionJob struct {
	ID                string          `json:"id"`
	FunctionVersionID string          `json:"functionVersionId"`
	ProjectID         string          `json:"projectId"`
	TargetRegion      string          `json:"targetRegion,omitempty"`
	State             JobState        `json:"state"`
	Payload           json.RawMessage `json:"payload"`
	MaxRetries        int             `json:"maxRetries"`
	AttemptCount      int             `json:"attemptCount"`
	LastAttemptID     string          `json:"lastAttemptId,omitempty"`
	Error             string          `json:"error,omitempty"`
	Result            *JobResult      `json:"result,omitempty"`
	CreatedAt         time.Time       `json:"createdAt"`
	UpdatedAt         time.Time       `json:"updatedAt"`
}

type Attempt struct {
	ID                string       `json:"id"`
	JobID             string       `json:"jobId"`
	FunctionVersionID string       `json:"functionVersionId"`
	HostID            string       `json:"hostId,omitempty"`
	Region            string       `json:"region"`
	State             AttemptState `json:"state"`
	StartMode         StartMode    `json:"startMode,omitempty"`
	StartedAt         time.Time    `json:"startedAt,omitempty"`
	LeaseExpiresAt    time.Time    `json:"leaseExpiresAt"`
	Error             string       `json:"error,omitempty"`
	CreatedAt         time.Time    `json:"createdAt"`
	UpdatedAt         time.Time    `json:"updatedAt"`
}

type Host struct {
	ID             string         `json:"id"`
	Region         string         `json:"region"`
	Driver         string         `json:"driver"`
	State          HostState      `json:"state"`
	AvailableSlots int            `json:"availableSlots"`
	BlankWarm      int            `json:"blankWarm"`
	FunctionWarm   map[string]int `json:"functionWarm"`
	LastHeartbeat  time.Time      `json:"lastHeartbeat"`
}

type RegionStats struct {
	AvailableHosts int `json:"availableHosts"`
	BlankWarm      int `json:"blankWarm"`
	FunctionWarm   int `json:"functionWarm"`
}

type Region struct {
	Name            string    `json:"name"`
	State           string    `json:"state"`
	AvailableHosts  int       `json:"availableHosts"`
	BlankWarm       int       `json:"blankWarm"`
	FunctionWarm    int       `json:"functionWarm"`
	LastHeartbeatAt time.Time `json:"lastHeartbeatAt"`
	LastError       string    `json:"lastError,omitempty"`
}

type WarmPool struct {
	Region            string    `json:"region"`
	HostID            string    `json:"hostId"`
	FunctionVersionID string    `json:"functionVersionId,omitempty"`
	BlankWarm         int       `json:"blankWarm"`
	FunctionWarm      int       `json:"functionWarm"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

type CostRecord struct {
	ID              string    `json:"id"`
	TenantID        string    `json:"tenantId"`
	ProjectID       string    `json:"projectId"`
	JobID           string    `json:"jobId"`
	AttemptID       string    `json:"attemptId,omitempty"`
	HostID          string    `json:"hostId,omitempty"`
	Region          string    `json:"region,omitempty"`
	StartMode       StartMode `json:"startMode,omitempty"`
	CPUMs           int64     `json:"cpuMs"`
	MemoryMBMs      int64     `json:"memoryMbMs"`
	WarmInstanceMs  int64     `json:"warmInstanceMs"`
	DataEgressBytes int64     `json:"dataEgressBytes"`
	CreatedAt       time.Time `json:"createdAt"`
}

type Assignment struct {
	AttemptID         string
	JobID             string
	FunctionVersionID string
	ArtifactDigest    string
	Entrypoint        string
	EnvRefs           []string
	Payload           json.RawMessage
	NetworkPolicy     NetworkPolicy
	TimeoutSec        int
}

func (a AttemptState) Terminal() bool {
	return a == AttemptStateSucceeded || a == AttemptStateFailed
}
