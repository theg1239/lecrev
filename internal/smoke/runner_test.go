package smoke

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/theg1239/lecrev/internal/artifact"
	"github.com/theg1239/lecrev/internal/devstack"
	"github.com/theg1239/lecrev/internal/regions"
)

func TestRunEmbeddedStackEndToEnd(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	stack, err := devstack.StartEmbedded(ctx, devstack.Config{
		LoadEnv:          false,
		ExecutionRegions: append([]string(nil), regions.DefaultExecutionRegions...),
		SecretsBackend:   "memory",
	})
	if err != nil {
		t.Fatalf("start embedded stack: %v", err)
	}
	defer stack.Close()

	result, err := Run(ctx, Config{
		BaseURL:   "http://embedded.lecrev",
		APIKey:    stack.APIKey,
		ProjectID: "smoke-test",
		Regions:   append([]string(nil), stack.Regions...),
		Client:    NewHandlerClient(stack.Handler),
		Timeout:   20 * time.Second,
	})
	if err != nil {
		t.Fatalf("run smoke: %v", err)
	}
	if result.Job.Result == nil {
		t.Fatal("expected terminal job result")
	}
	if result.Job.Result.Region == "" {
		t.Fatal("expected result region to be recorded")
	}
	if strings.TrimSpace(result.Job.Result.LogsKey) == "" {
		t.Fatal("expected archived logs key to be recorded")
	}
	if strings.TrimSpace(result.Job.Result.OutputKey) == "" {
		t.Fatal("expected archived output key to be recorded")
	}
	if len(result.Attempts) == 0 {
		t.Fatal("expected at least one attempt")
	}
	logs, err := stack.Objects.Get(ctx, result.Job.Result.LogsKey)
	if err != nil {
		t.Fatalf("get archived logs: %v", err)
	}
	output, err := stack.Objects.Get(ctx, result.Job.Result.OutputKey)
	if err != nil {
		t.Fatalf("get archived output: %v", err)
	}
	if len(output) == 0 {
		t.Fatal("expected archived output content")
	}
	if string(logs) != result.Job.Result.Logs {
		t.Fatalf("expected archived logs %q, got %q", result.Job.Result.Logs, string(logs))
	}
	if string(output) != string(result.Job.Result.Output) {
		t.Fatalf("expected archived output %s, got %s", string(result.Job.Result.Output), string(output))
	}
	lastAttempt := result.Attempts[len(result.Attempts)-1]
	if got := artifact.ExecutionLogsKey(result.Job.ID, lastAttempt.ID); result.Job.Result.LogsKey != got {
		t.Fatalf("expected logs key %s, got %s", got, result.Job.Result.LogsKey)
	}
	if len(result.Costs) == 0 {
		t.Fatal("expected at least one cost record")
	}

	select {
	case err := <-stack.Errors():
		t.Fatalf("embedded stack returned async error: %v", err)
	default:
	}
}
