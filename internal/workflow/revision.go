package workflow

import "strings"

// HasReservedRevisionSuffix reports whether name uses the internal marker
// reserved for superseded managed workflow rows.
func HasReservedRevisionSuffix(name string) bool {
	return strings.Contains(strings.TrimSpace(name), RevisionSuffixMarker)
}

func revisionName(name, workflowID string) string {
	return strings.TrimSpace(name) + RevisionSuffixMarker + strings.TrimPrefix(strings.TrimSpace(workflowID), WorkflowIDPrefix+"-")
}
