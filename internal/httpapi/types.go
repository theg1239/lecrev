package httpapi

import "encoding/json"

type createFunctionRequest struct {
	Name           string          `json:"name"`
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
