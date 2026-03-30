package httpapi

import (
	"encoding/json"
	"time"

	"github.com/theg1239/lecrev/internal/domain"
)

type createFunctionRequest struct {
	Name           string          `json:"name"`
	Environment    string          `json:"environment,omitempty"`
	Runtime        string          `json:"runtime"`
	Entrypoint     string          `json:"entrypoint"`
	MemoryMB       int             `json:"memoryMb"`
	TimeoutSec     int             `json:"timeoutSec"`
	NetworkPolicy  string          `json:"networkPolicy"`
	Regions        []string        `json:"regions"`
	EnvRefs        []string        `json:"envRefs"`
	MaxRetries     int             `json:"maxRetries"`
	IdempotencyKey string          `json:"idempotencyKey"`
	Source         json.RawMessage `json:"source"`
}

type invokeRequest struct {
	Payload        json.RawMessage `json:"payload"`
	IdempotencyKey string          `json:"idempotencyKey"`
}

type createWebhookTriggerRequest struct {
	Description string `json:"description"`
	Token       string `json:"token"`
}

type drainHostRequest struct {
	Reason string `json:"reason"`
}

type functionVersionSummary struct {
	ID             string               `json:"id"`
	Name           string               `json:"name"`
	Runtime        string               `json:"runtime"`
	State          domain.FunctionState `json:"state"`
	Entrypoint     string               `json:"entrypoint"`
	MemoryMB       int                  `json:"memoryMb"`
	TimeoutSec     int                  `json:"timeoutSec"`
	NetworkPolicy  domain.NetworkPolicy `json:"networkPolicy"`
	Regions        []string             `json:"regions"`
	BuildJobID     string               `json:"buildJobId,omitempty"`
	ArtifactDigest string               `json:"artifactDigest,omitempty"`
	CreatedAt      time.Time            `json:"createdAt"`
}

type buildJobSummary struct {
	ID                string    `json:"id"`
	FunctionVersionID string    `json:"functionVersionId"`
	TargetRegion      string    `json:"targetRegion,omitempty"`
	State             string    `json:"state"`
	Error             string    `json:"error,omitempty"`
	LogsReady         bool      `json:"logsReady"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

type executionResultSummary struct {
	ExitCode    int       `json:"exitCode"`
	HostID      string    `json:"hostId"`
	Region      string    `json:"region"`
	StartedAt   time.Time `json:"startedAt"`
	FinishedAt  time.Time `json:"finishedAt"`
	LogsReady   bool      `json:"logsReady"`
	OutputReady bool      `json:"outputReady"`
}

type executionJobSummary struct {
	ID                string                  `json:"id"`
	FunctionVersionID string                  `json:"functionVersionId"`
	TargetRegion      string                  `json:"targetRegion,omitempty"`
	State             domain.JobState         `json:"state"`
	MaxRetries        int                     `json:"maxRetries"`
	AttemptCount      int                     `json:"attemptCount"`
	LastAttemptID     string                  `json:"lastAttemptId,omitempty"`
	Error             string                  `json:"error,omitempty"`
	Result            *executionResultSummary `json:"result,omitempty"`
	CreatedAt         time.Time               `json:"createdAt"`
	UpdatedAt         time.Time               `json:"updatedAt"`
}

type projectOverviewTotals struct {
	Functions     int            `json:"functions"`
	BuildsByState map[string]int `json:"buildsByState"`
	JobsByState   map[string]int `json:"jobsByState"`
}

type projectOverviewResponse struct {
	Project         domain.Project           `json:"project"`
	Totals          projectOverviewTotals    `json:"totals"`
	RecentFunctions []functionVersionSummary `json:"recentFunctions"`
	RecentBuildJobs []buildJobSummary        `json:"recentBuildJobs"`
	RecentJobs      []executionJobSummary    `json:"recentJobs"`
}

type deploymentSummary struct {
	ID                string               `json:"id"`
	ProjectID         string               `json:"projectId"`
	ProjectName       string               `json:"projectName"`
	FunctionVersionID string               `json:"functionVersionId"`
	Name              string               `json:"name"`
	Runtime           string               `json:"runtime"`
	SourceType        domain.SourceType    `json:"sourceType"`
	Environment       string               `json:"environment,omitempty"`
	Branch            string               `json:"branch,omitempty"`
	CommitSHA         string               `json:"commitSha,omitempty"`
	GitURL            string               `json:"gitUrl,omitempty"`
	Status            string               `json:"status"`
	FunctionState     domain.FunctionState `json:"functionState"`
	Regions           []string             `json:"regions"`
	Build             *buildJobSummary     `json:"build,omitempty"`
	LastJob           *executionJobSummary `json:"lastJob,omitempty"`
	CreatedAt         time.Time            `json:"createdAt"`
	UpdatedAt         time.Time            `json:"updatedAt"`
}
