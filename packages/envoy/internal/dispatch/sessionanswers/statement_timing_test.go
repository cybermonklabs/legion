package sessionanswers

import (
	"os"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestStatementTimings measures the agents' answers read (query) against main's (mainQuery) on
// the seeded measurement database (benchtime), which DISPATCH_BENCH_DATABASE_URL names. CI never
// sets that variable, so CI never runs a measurement.
//
// It is protocol 1: the two texts differ in SQL alone, and one schema runs both, since the schema
// keeps events_person_words_with_declines, which main's text reads, beside
// events_person_words_destinations, which the read reads on the window of the --scale 1 seed;
// on a smaller seed, whose window holds fewer deliveries (cmd/dispatch-seed's sizes,
// WindowDeliveries), the plan for its values can read events_person_words_with_declines instead.
// The test reads whichever database DISPATCH_BENCH_DATABASE_URL names. There is one pair for each
// of the three sessions benchtime binds by the rule the seed's report prints them by
// (answerswindow.Read): the session with old deliveries, and the sessions with the most and the
// fewest deliveries in the window. Each side is bound as List binds the read: the session, the
// start of the seed's answers window, read from the database by its one rule, and the session's
// topic.
//
// For each session, the two texts must answer the same rows, at least one, compared as
// TestTheAnswersReadAnswersMainsRows compares them (requireTheSameAnswerRows); the pair is timed
// (benchtime.Runner.Pair); and each side's shared buffers at its top plan node are read under the
// plan made for its values and under the generic plan, every count and both plans of each side
// are logged, and the read must read fewer than main's under each plan
// (benchtime.Runner.PairBuffers). Each of these is a subtest, so one that fails leaves the rest of
// the run to log its own numbers.
func TestStatementTimings(t *testing.T) {
	runner := benchtime.Open(t, os.Getenv(benchtime.DatabaseURLEnv))
	subjects := runner.Subjects()
	database := benchtime.OpenStore(t, os.Getenv(benchtime.DatabaseURLEnv))
	for _, session := range subjects.AnswerSessions {
		name := "the answers read for " + session.Name
		arguments := []any{session.Session, subjects.AnswersSince, agentTopic(session.Session)}
		mains := benchtime.Statement{SQL: mainQuery, Args: arguments}
		reads := benchtime.Statement{SQL: query, Args: arguments}
		t.Run(name+", its rows", func(t *testing.T) {
			requireTheSameAnswerRows(t, database, session, subjects.AnswersSince)
		})
		runner.Pair(t, name, mains, reads)
		t.Run(name+", its top node's buffers", func(t *testing.T) {
			runner.PairBuffers(t, name, mains, reads)
		})
	}
}

// requireTheSameAnswerRows holds query to mainQuery's rows for session since since, compared as
// requireMainsRows compares them over the fixture (sameAnswerRows): the same columns, and the same
// rows in the same order, each column the same bytes the server sent. It stops the test where the
// columns or the number of rows differ. main's read must answer some row, so the comparison holds
// something and the pair beside it times a read that lists items.
func requireTheSameAnswerRows(t *testing.T, database *store.Store, session benchtime.AnswerSession, since time.Time) {
	t.Helper()
	want := readAnswerRows(t, database, mainQuery, session.Session, since)
	got := readAnswerRows(t, database, query, session.Session, since)
	t.Logf("%s, since %s: main's read answers %d rows, and the read %d",
		session, since.Format(time.RFC3339Nano), len(want.raw), len(got.raw))
	if len(want.raw) == 0 {
		t.Errorf("main's read answers no row for %s, so the comparison holds nothing", session)
	}
	if !sameAnswerRows(t, "the read for "+session.String(), got, want) {
		t.FailNow()
	}
}
