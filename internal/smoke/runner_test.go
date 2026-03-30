package smoke

import (
	"context"
	"testing"
	"time"

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
	if len(result.Attempts) == 0 {
		t.Fatal("expected at least one attempt")
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
