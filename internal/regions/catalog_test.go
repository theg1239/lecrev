package regions

import "testing"

func TestNormalizeExecutionRegionsDefaultsToAPAC(t *testing.T) {
	t.Parallel()

	got, err := NormalizeExecutionRegions(nil)
	if err != nil {
		t.Fatalf("normalize default regions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 default regions, got %d", len(got))
	}
	if got[0] != "ap-south-1" || got[1] != "ap-south-2" || got[2] != "ap-southeast-1" {
		t.Fatalf("unexpected default regions: %#v", got)
	}
}

func TestNormalizeExecutionRegionsRejectsUSRegion(t *testing.T) {
	t.Parallel()

	if _, err := NormalizeExecutionRegions([]string{"us-east-1"}); err == nil {
		t.Fatal("expected us-east-1 to be rejected")
	}
}
