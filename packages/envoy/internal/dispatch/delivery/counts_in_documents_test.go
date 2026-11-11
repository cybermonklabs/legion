package delivery

import (
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/internal/docnumbers"
)

// What the server README states of this package's own bounds, held to the constants that decide
// them. The class this guards is the one dispatch://INFRA-558 met four times: a number the code
// owns, written again in prose, with nothing holding the two together, so a raise leaves the
// sentence a person reads behind while every test stays green.
//
// The reader is internal/docnumbers, which internal/dispatch/store registers its own connection
// counts with; this is the second registration of it, and it is data: one entry per sentence,
// naming the file, the pattern, the constant and what the number is of.
func TestTheServerReadmeStatesTheBoundsThisPackageHolds(t *testing.T) {
	const readme = "packages/envoy/cmd/dispatch/README.md"
	docnumbers.Check(t, filepath.Join("..", "..", "..", "..", ".."), []docnumbers.Stated{
		{File: readme, Pattern: `best ` + docnumbers.Number + ` candidates are the ones read`,
			Number: attributionCandidateBound,
			Says:   "the candidates a pull request's attribution reads"},
	})
}
