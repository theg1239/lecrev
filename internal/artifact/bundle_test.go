package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBundleRoundTrip(t *testing.T) {
	t.Parallel()

	bundle, err := BundleFromFiles(map[string][]byte{
		"index.mjs":     []byte("export async function handler() { return { ok: true }; }"),
		"function.json": []byte(`{"entrypoint":"index.mjs"}`),
	})
	if err != nil {
		t.Fatalf("bundle files: %v", err)
	}

	dest := t.TempDir()
	if err := ExtractTarGz(bundle, dest); err != nil {
		t.Fatalf("extract bundle: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "index.mjs")); err != nil {
		t.Fatalf("expected extracted entrypoint: %v", err)
	}
}
