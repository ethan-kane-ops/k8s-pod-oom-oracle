package render

import (
	"encoding/json"
	"fmt"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/oom"
)

// JSON renders one report as indented JSON with a trailing newline.
func JSON(report *oom.Report) ([]byte, error) {
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling report: %w", err)
	}
	return append(b, '\n'), nil
}

// JSONList renders a set of reports. A nil slice renders as [] rather than
// null, so consumers can iterate without a nil check.
func JSONList(reports []oom.Report) ([]byte, error) {
	if reports == nil {
		reports = []oom.Report{}
	}
	b, err := json.MarshalIndent(reports, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling reports: %w", err)
	}
	return append(b, '\n'), nil
}
