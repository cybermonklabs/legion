package delivery

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/docnumbers"
)

// What the server README states of this package's own bounds, held to the constants that decide
// them: the candidates a pull request's attribution reads, the lease and renewal of a delivery
// intake's hold, how often a standby tries for that hold, and the same lease where a second
// sentence states it of a dead process's replacement. The first of them names duty.Hold, which
// reads as duty's own number at a glance, but duty takes its TTL, renewal and margin from its
// caller and fixes no value; the numbers are intakeHoldTTL, intakeHoldRenew and
// durableBindInterval of this package. The class and the reader
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
		{File: readme, Pattern: `try for the hold every ` + docnumbers.Number + ` seconds`,
			Number: int(durableBindInterval / time.Second),
			Says:   "how often a standby process tries for a durable's hold"},
		{File: readme, Pattern: `replaced within the hold's ` + docnumbers.Number + ` seconds and one try`,
			Number: int(intakeHoldTTL / time.Second),
			Says:   "the hold a dead process's replacement waits out, the lease again"},
	})
}
