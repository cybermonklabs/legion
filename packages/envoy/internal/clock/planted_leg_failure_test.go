package clock

import "testing"

// A planted failure for INFRA-1075: it shows a failing unit leg turns envoy-go and ci-ok red.
// Not for merge.
func TestPlantedLegFailure(t *testing.T) { t.Fatal("planted failure in a unit leg") }
