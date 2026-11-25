package shimwire

import "testing"

// PLANTED: fails on purpose, for a throwaway run that shows a failing race leg turns ci-ok red
// (cybermonklabs/legion#5519). Never merged.
func TestPlantedCiOkControlFailsTheRaceLeg(t *testing.T) {
	t.Fatal("PLANTED: this test fails on purpose")
}
