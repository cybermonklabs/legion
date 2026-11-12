package pgmigrate

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/docnumbers"
)

// What the documents state of this package's own bound, held to the constant that decides it: how
// long a migration waits for its lock. The class and the reader are internal/docnumbers' own
// documentation; what is here is the registration, one entry per sentence, the duration as the
// number its sentence writes, in seconds and spelled as that page spells it.
func TestTheDocumentsStateTheLockBoundThisPackageHolds(t *testing.T) {
	const site = "docs/site/src/content/docs/dispatch/running-dispatch.md"
	docnumbers.Check(t, filepath.Join("..", "..", "..", ".."), []docnumbers.Stated{
		{File: site, Pattern: `Each migration waits at most ` + docnumbers.Number + ` seconds\s+for a lock`,
			Number: int(LockTimeout / time.Second),
			Says:   "how long a migration waits for its lock"},
	})
}
