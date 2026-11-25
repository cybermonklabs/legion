package plan

import "testing"

// A planted caller of Places's signature before this round, which took the blocks alone: the
// merged-tree vet must refuse it. It is the control of that vet and is never merged.
func TestPlantedCallerOfTheOldSignature(t *testing.T) {
	_, _ = Places(nil)
}
