package postgres

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/theg1239/lecrev/internal/domain"
)

func TestNormalizeRawJSONSanitizesInvalidJSONStringPayload(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage([]byte("{\"body\":\"hello\\u0000world\"}"))
	normalized := normalizeRawJSON(raw)

	var decoded map[string]string
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		t.Fatalf("unmarshal normalized json: %v", err)
	}
	if got := decoded["body"]; got != "hello\ufffdworld" {
		t.Fatalf("expected nul rune to be replaced, got %q", got)
	}
}

func TestSanitizeJobResultForStorageNormalizesResultOutput(t *testing.T) {
	t.Parallel()

	result := &domain.JobResult{
		Logs:   "ok\x00logs",
		HostID: "host-\x00id",
		Region: "ap-south-1",
		Output: json.RawMessage([]byte("{\"body\":\"hello\\u0000world\"}")),
	}

	sanitized := sanitizeJobResultForStorage(result)
	if sanitized == nil {
		t.Fatal("expected sanitized result")
	}
	if sanitized.Logs != "ok\ufffdlogs" {
		t.Fatalf("expected sanitized logs, got %q", sanitized.Logs)
	}
	if sanitized.HostID != "host-\ufffdid" {
		t.Fatalf("expected sanitized host id, got %q", sanitized.HostID)
	}
	if bytes.Equal(sanitized.Output, result.Output) {
		t.Fatal("expected output to be rewritten for storage safety")
	}
	var decoded map[string]string
	if err := json.Unmarshal(sanitized.Output, &decoded); err != nil {
		t.Fatalf("unmarshal sanitized output: %v", err)
	}
	if got := decoded["body"]; got != "hello\ufffdworld" {
		t.Fatalf("expected sanitized output body, got %q", got)
	}
}
