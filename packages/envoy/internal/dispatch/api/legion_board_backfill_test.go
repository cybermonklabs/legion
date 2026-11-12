package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
)

// TestTheBoardHeaderTellsAProjectWaitingOnItsBackfillFromOneWithNoLegionWork: dispatch://INFRA-2915's
// R8 says a reader can tell a project with no Legion work from a project whose backfill has not
// run, and check k10 fails "when the two empty projects read alike".
//
// The deployment this is about is one that has been running: the facts consumer has committed a
// pass, so `facts_cursor_age_seconds` is an age for every project of it, and a project whose rows
// have not been written carries no row of its own, so `facts_updated_at` is null. A project with
// no Legion work at all carries exactly those two values. Both fields are of the deployment's
// consumer and of the project's own rows, and neither says whether the project has Legion work,
// so the two read alike and the reader R8 names cannot tell them apart.
//
// Measured on the live surface at 819649ba, against a running server: a project seeded with 20
// issues carrying Legion phase work and a project with none both answered
// `{"issues":{}, "facts_updated_at":null, "facts_cursor_age_seconds":4}` at the same moment.
//
// `TestTheBoardHeaderTellsNoLegionWorkFromNoBackfill` covers the other case, where no pass has
// ever committed and the age is null for every project of the deployment. That case is the first
// read of a fresh deployment; this one is every day after it.
func TestTheBoardHeaderTellsAProjectWaitingOnItsBackfillFromOneWithNoLegionWork(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA", "LINUX")
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedLegionBoard(t, database, "INFRA", now)
	writeLegionFacts(t, database, "INFRA", now)

	// A third project Legion has worked, whose stored rows the consumer has not written yet:
	// the history is there, the rows are not.
	waiting := legiontest.Seed(t, database.Pool, legiontest.Spec{Project: "WIDGETS", Trees: 2, Now: now})
	for _, project := range []string{"INFRA", "LINUX", "WIDGETS"} {
		nameLegionDaemon(t, handler, project, "http://legion-"+project+".internal:13370")
	}
	// A pass has committed ten seconds ago, which is the state of any deployment that has been
	// running: every project's cursor age is an age.
	writeLegionFactsCursor(t, database, now.Add(-10*time.Second))

	worked := legionBoardOf(t, handler, "INFRA")
	backfilling := legionBoardOf(t, handler, "WIDGETS")
	idle := legionBoardOf(t, handler, "LINUX")
	if len(worked.Issues) == 0 {
		t.Fatalf("INFRA carries %d stored rows, want the fresh project to carry some", len(worked.Issues))
	}
	if len(backfilling.Issues) != 0 || len(idle.Issues) != 0 {
		t.Fatalf("the two empty projects carry %d and %d stored rows, want none of either",
			len(backfilling.Issues), len(idle.Issues))
	}
	t.Logf("WIDGETS has Legion phase work for %d of its %d issues and no stored row; LINUX has no Legion work at all",
		len(waiting.PhaseWork), len(waiting.Keys))

	header := func(board legionBoardResponse) string {
		t.Helper()
		text, err := json.Marshal(board.Header)
		if err != nil {
			t.Fatalf("read the header of %s: %v", board.Project, err)
		}
		return string(text)
	}
	if header(backfilling) == header(idle) {
		t.Errorf("the header of a project whose backfill has not run reads the same as the header of a project with no Legion work:\n  WIDGETS %s\n  LINUX   %s",
			header(backfilling), header(idle))
	}
}
