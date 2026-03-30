package localnode

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/theg1239/lecrev/internal/artifact"
	"github.com/theg1239/lecrev/internal/firecracker"
)

func TestExecuteNodeHandler(t *testing.T) {
	t.Parallel()

	bundle, err := artifact.BundleFromFiles(map[string][]byte{
		"index.mjs": []byte(`export async function handler(event, context) { return { ok: true, msg: event.msg, region: context.region }; }`),
	})
	if err != nil {
		t.Fatalf("bundle files: %v", err)
	}

	driver := New()
	result, err := driver.Execute(context.Background(), firecracker.ExecuteRequest{
		AttemptID:      "attempt-1",
		JobID:          "job-1",
		FunctionID:     "fn-1",
		Entrypoint:     "index.mjs",
		ArtifactBundle: bundle,
		Payload:        json.RawMessage(`{"msg":"hello"}`),
		Timeout:        5 * time.Second,
		Region:         "ap-south-1",
		HostID:         "host-ap-south-1-a",
	})
	if err != nil {
		t.Fatalf("execute driver: %v", err)
	}

	if string(result.Output) != `{"ok":true,"msg":"hello","region":"ap-south-1"}` {
		t.Fatalf("unexpected output: %s", string(result.Output))
	}
}
