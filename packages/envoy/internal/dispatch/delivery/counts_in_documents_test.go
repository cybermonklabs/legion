package delivery

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/docnumbers"
)

// What the server README states of this package's own bounds, held to the constants that decide
// them: the candidates a pull request's attribution reads, and the lease and renewal of a
// delivery intake's hold. Those last two read as duty's at a glance, since the sentence names
// duty.Hold, but duty takes its TTL, renewal and margin from its caller and fixes no value;
// the numbers are intakeHoldTTL and intakeHoldRenew of this package. The class and the reader
// are internal/docnumbers' own documentation; what is here is the registration, one entry per
// sentence, each duration registered as the number its sentence writes, in seconds.
func TestTheServerReadmeStatesTheBoundsThisPackageHolds(t *testing.T) {
	const readme = "packages/envoy/cmd/dispatch/README.md"
	docnumbers.Check(t, filepath.Join("..", "..", "..", "..", ".."), []docnumbers.Stated{
		{File: readme, Pattern: `best ` + docnumbers.Number + ` candidates are the ones read`,
			Number: attributionCandidateBound,
			Says:   "the candidates a pull request's attribution reads"},
		{File: readme, Pattern: `a row with a ` + docnumbers.Number + `-second lease on the database's clock`,
			Number: int(intakeHoldTTL / time.Second),
			Says:   "the lease a delivery intake's hold takes on the database's clock"},
		{File: readme, Pattern: `on the database's clock, renewed every ` + docnumbers.Number + ` seconds`,
			Number: int(intakeHoldRenew / time.Second),
			Says:   "how often that hold is renewed"},
	})
}
