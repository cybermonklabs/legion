package supervise

import (
	"slices"
	"testing"
)

// SuspendSources answers a list kept by hand beside its constants, and a project release names
// how each claim it stopped ended in its words (api.ReleasedClaimEnds), which every plugin reads
// through a strict enum: a source left out of the list would be a word the release writes that
// neither the golden file nor the contracts suite holds. So the list is held here to the
// constants of its type as the compiler reads them from this package's source (packageSets), and
// a constant added and not listed fails, as TestStatesIsEveryDeclaredStateOnce holds States(),
// the other list the same words are built from.
func TestSuspendSourcesNameEveryConstant(t *testing.T) {
	var declared []SuspendSource
	for _, value := range packageSets(t).sources {
		declared = append(declared, SuspendSource(value))
	}
	if len(declared) == 0 {
		t.Fatalf("read no constant of SuspendSource's type from the source")
	}
	listed := SuspendSources()
	if got, want := slices.Sorted(slices.Values(listed)), slices.Sorted(slices.Values(declared)); !slices.Equal(got, want) {
		t.Errorf("SuspendSources() = %v, want each declared SuspendSource once: %v", listed, want)
	}
}
