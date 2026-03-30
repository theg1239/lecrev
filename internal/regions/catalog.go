package regions

import (
	"fmt"
	"slices"
	"strings"
)

var DefaultExecutionRegions = []string{
	"ap-south-1",
	"ap-south-2",
	"ap-southeast-1",
}

func DefaultPrimary() string {
	return DefaultExecutionRegions[0]
}

func NormalizeExecutionRegions(requested []string) ([]string, error) {
	if len(requested) == 0 {
		return append([]string(nil), DefaultExecutionRegions...), nil
	}

	normalized := make([]string, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, region := range requested {
		trimmed := strings.TrimSpace(region)
		if trimmed == "" {
			continue
		}
		if !slices.Contains(DefaultExecutionRegions, trimmed) {
			return nil, fmt.Errorf("unsupported region %q: only APAC execution regions are enabled (%s)", trimmed, strings.Join(DefaultExecutionRegions, ", "))
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		normalized = append(normalized, trimmed)
	}
	if len(normalized) == 0 {
		return nil, fmt.Errorf("at least one APAC execution region is required")
	}
	return normalized, nil
}

func ParseCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
