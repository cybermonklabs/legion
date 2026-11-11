package store

import (
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/internal/docnumbers"
)

// The three documents the connection-count check reads, and the two constants they state. Three
// rounds of dispatch://INFRA-558 went to one class: a merge raises LeasePoolSize or
// ConnectionsPerProcess, every test of both sides stays green because no test reads a sentence,
// and the number a person reads is left behind. This row reads those sentences, so the next raise
// fails here instead of in a person's reading.
//
// The reader is internal/docnumbers, which another package registers its own constants and
// documents with in the same shape; what is here is the registration, and nothing else. Each entry
// names one sentence: the file, a pattern over docnumbers.Number, the constant that number must
// equal, and what the number is of. A word reads as readily as digits, since this package's count
// is spelled in .dispatch/architecture/dispatch-store.md and written in digits everywhere else,
// and a pattern that matches nothing fails too.
//
// Keep it to these three documents and these two constants. A document that states no count needs
// no entry, and a sentence that states a number of something else (a migration version, a review
// round) belongs to no entry here.
func TestTheDocumentsStateTheCountsThisPackageHolds(t *testing.T) {
	const (
		site      = "docs/site/src/content/docs/dispatch/running-dispatch.md"
		readme    = "packages/envoy/cmd/dispatch/README.md"
		storeArch = ".dispatch/architecture/dispatch-store.md"
	)
	number := docnumbers.Number
	docnumbers.Check(t, filepath.Join("..", "..", "..", "..", ".."), []docnumbers.Stated{
		{File: site, Pattern: `up to ` + number + ` for the work it runs once across the servers`,
			Number: LeasePoolSize,
			Says:   "the connections one server opens for the work it runs once across the servers"},
		{File: site, Pattern: `That is\s+` + number + `\s+connections for one server at its ceiling`,
			Number: ConnectionsPerProcess, Says: "the connections one server opens at its ceiling"},
		{File: readme, Pattern: `callers of the two intakes' holds, ` + number,
			Number: LeasePoolSize,
			Says:   "the lease pool's connections, one per leased name and per hold caller"},
		{File: readme, Pattern: `= ` + number + ` connections to Postgres`,
			Number: ConnectionsPerProcess,
			Says:   "the sum of the counted pools and the permanent listeners"},
		{File: readme, Pattern: `opens at most 64 \+ ` + number + ` \+`, Number: LeasePoolSize,
			Says: "the lease term of that sum, which states the pool's own size inside it"},
		{File: readme, Pattern: `\| Leases \(` + "`store.LeasePoolSize`" + `\) \| ` + number + ` \|`,
			Number: LeasePoolSize, Says: "the lease pool's row of the pool table"},
		{File: readme, Pattern: `Two processes at their ceiling hold ` + number,
			Number: 2 * ConnectionsPerProcess, Says: "two processes at their ceiling"},
		{File: storeArch, Pattern: `per caller of a delivery intake's hold, ` + number,
			Number: LeasePoolSize,
			Says:   "the lease pool's connections, one per leased loop and per hold caller"},
	})
}
