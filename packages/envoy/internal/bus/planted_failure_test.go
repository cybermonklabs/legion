package bus

import "testing"

// A planted failure: this pull request is a control of the Go legs' package selection, and never
// merges. A change to one test file of internal/bus selects internal/bus alone, so its leg runs
// this test and ci-ok must fail.
func TestPlantedFailureForThePackageSelectionControl(t *testing.T) {
	t.Fatal("planted failure: the Go legs' package selection ran internal/bus")
}
