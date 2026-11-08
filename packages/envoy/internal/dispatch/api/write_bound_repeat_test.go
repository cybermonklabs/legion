package api

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logtest"
)

// TestTheWriteBoundBenchmarksOpenTheirIssues opens the four issues
// BenchmarkTheWritePathsThatStoreADisplayTree opens, in its order, twice over in one server, as
// the acceptance's own command for A6 runs that benchmark: `go test -bench … -count 10` at the
// base and at the head, compared with benchstat.
//
// The benchmark opens one issue per write path through writeBoundIssue, and go test re-invokes
// each sub-benchmark once per -count round against the same server and the same database. Both
// of those open an issue whose title the near-copy gate reads as a possible duplicate of one the
// run already opened - the upload's title against the version write's inside a single round, and
// every title against its own in the round before - so the gate answers 409 POSSIBLE_DUPLICATE
// and writeBoundRequest fails the benchmark. The measurement the acceptance asks for is then a
// single sample of a single path rather than ten of four, which benchstat cannot compare.
//
// The create route takes `force`, which records what the gate refused and goes in, and a
// benchmark fixture is what it is for; a title that carries the round's number would do as well.
// This test is red until the benchmark opens its issues a way the gate does not refuse.
func TestTheWriteBoundBenchmarksOpenTheirIssues(t *testing.T) {
	logtest.SetDefault(t, slog.NewTextHandler(io.Discard, nil))
	handler, _, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	writeBoundProject(t, handler)
	for round := 1; round <= 2; round++ {
		for index, title := range writeBoundIssueTitles {
			t.Logf("round %d, path %d: opening %q", round, index+1, title)
			writeBoundIssue(t, handler, title)
		}
	}
}
