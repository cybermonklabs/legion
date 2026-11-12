package delivery

import (
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/internal/docnumbers"
)

// What the server README states of this package's own bounds, held to the constants that decide
// them: the candidates a pull request's attribution reads. The class and the reader are
// internal/docnumbers' own documentation; what is here is the registration, one entry per
// sentence.
func TestTheServerReadmeStatesTheBoundsThisPackageHolds(t *testing.T) {
	const readme = "packages/envoy/cmd/dispatch/README.md"
	docnumbers.Check(t, filepath.Join("..", "..", "..", "..", ".."), []docnumbers.Stated{
		{File: readme, Pattern: `best ` + docnumbers.Number + ` candidates are the ones read`,
			Number: attributionCandidateBound,
			Says:   "the candidates a pull request's attribution reads"},
	})
}
