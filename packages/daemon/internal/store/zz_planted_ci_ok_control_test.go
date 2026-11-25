package store

import "testing"

// PLANTED: fails on purpose, for a throwaway run that shows a failing plain leg turns ci-ok red
// (cybermonklabs/legion#5519). Never merged.
func TestPlantedCiOkControlFailsThePlainLeg(t *testing.T) {
	t.Fatal("PLANTED: this test fails on purpose")
}
