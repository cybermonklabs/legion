//go:build !race

// The guard is static, as lock_level_test.go's is and for the reason given there: it reads the
// module's source, so the race detector adds nothing to it.

package api

//ci:reads packages/envoy/**/*.go !**/*_test.go `packages/envoy/internal/dispatch/api/module_source_test.go#scanModuleSources` walks packages/envoy (infra, testdata and all) and parses every .go file it finds but the test files, for the tests that ask it for the module's sources: `packages/envoy/internal/dispatch/api/lock_level_test.go#TestNoForUpdateOnOwnerTables`, `packages/envoy/internal/dispatch/api/event_insert_guard_test.go#TestOnlyTheBrokerInsertsEvents`, `packages/envoy/internal/dispatch/api/review_order_guard_test.go#TestEveryReviewReadOrdersByReviewOrder`, `packages/envoy/internal/dispatch/api/review_write_guard_test.go#TestOnlyQueueReviewWritesReviews`, `packages/envoy/internal/dispatch/api/one_expression_guard_test.go#TestOneExpressionDecidesWhoWaitsInOneSpelling`, `packages/envoy/internal/dispatch/api/issue_route_writer_guard_test.go#TestOnlyTheIssuePatchWritesTheRoute`; that last also walks packages/envoy and reads every non-test file of it as text

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// brokerEventInsert is the one production source file allowed to insert an event.
const brokerEventInsert = "internal/dispatch/events/broker.go"

// brokerEventInserts is how many event inserts that file holds: the one a write phase queues
// (events.Broker.QueueAppendWithLogins). It keeps the invariants below, and the count is here so
// that an insert moving out of this scan's reach reds rather than passing unseen.
const brokerEventInserts = 1

// legionTestEventInsert is the one non-_test.go fixture seeder allowed to bulk-insert events into
// a test database: legiontest is an ordinary package rather than a _test.go file only because Go
// cannot share test-only code across packages, and its one insert seeds a synthetic Legion history.
const legionTestEventInsert = "internal/dispatch/legion/legiontest/legiontest.go"

// legionTestEventInserts is how many event inserts legionTestEventInsert holds, checked for the
// same reason as brokerEventInserts.
const legionTestEventInserts = 1

var eventInsertStatement = regexp.MustCompile(`\binsert into events\b`)

// TestOnlyTheBrokerInsertsEvents enforces what every reader of the event log by id rests on: every
// row of it is written by events.Broker.Append, which keeps two invariants no other insert would.
//
// The writer invariant: Append's transaction holds its transaction id before the insert draws the
// row's id from events_id_seq (lockEventOwner takes it with pg_current_xact_id). The safe horizon's
// proof needs it (events/horizon.go): a transaction that drew an id at or below one an observer saw
// committed holds a transaction id below the observer's own, so once no event writer holding a
// transaction id below the observer's is open, that id is final. A bare insert, in a transaction
// that has written nothing before it, takes its transaction id only as it writes the row, after its
// default drew the id, so an observation taken between the two could prove a horizon above an id
// still open. The relay (event_stream_relay.go), the delivery hub and `GET /references?since=`
// would then read past that id, and its row, committed later, would reach no open subscription
// until the page subscribed again -- a missing comment or ask on an open page, with nothing to show
// for it in any log.
//
// Owner sequence allocation: Append locks the event's owner row and takes the owner's next seq under
// that lock, so one owner's events commit in id order and their seqs run without a gap or a repeat.
// An insert elsewhere could write a seq out of that order, and the Legion daemon, which fences an
// issue's facts on their seq, would drop as stale an event whose seq it had already passed.
//
// Its reach is the statement as the module scan can assemble it (TestNoForUpdateOnOwnerTables):
// string literals and chains of literals and package-level constants, in every non-test Go file
// of the module. A test inserts events directly on purpose -- a backlog to page through, a row
// shape to serve -- so test sources are outside it, as they are outside that scan.
//
// Only legionTestEventInsert is exempted beside brokerEventInsert, by exact path and insert count,
// so no other non-_test.go file (test helper package or otherwise) can insert into events unseen.
func TestOnlyTheBrokerInsertsEvents(t *testing.T) {
	t.Parallel()
	sources := moduleSources(t)
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	var offences []string
	broker, legionTest := 0, 0
	for _, source := range sources {
		path := filepath.ToSlash(source.path)
		rel := strings.TrimPrefix(path, moduleRoot+"/")
		for _, statement := range sqlStatements(source.file, constants) {
			if !eventInsertStatement.MatchString(statement) {
				continue
			}
			switch rel {
			case brokerEventInsert:
				broker++
			case legionTestEventInsert:
				legionTest++
			default:
				offences = append(offences, path+": "+statement)
			}
		}
	}
	if len(offences) > 0 {
		t.Fatalf("only %s (and fixture seeder %s) may insert into events, and these statements also do:\n\t%s\n"+
			"Append the event through events.Broker.Append instead. It takes its transaction id "+
			"before the insert draws the event's id, which the safe horizon every reader of the log "+
			"by id reads through needs (events/horizon.go): a row inserted without it can commit "+
			"behind a horizon that passed its id and reach no open subscription. It also allocates the "+
			"owner's seq under the owner's row lock.", brokerEventInsert, legionTestEventInsert, strings.Join(offences, "\n\t"))
	}
	if broker != brokerEventInserts {
		t.Fatalf("%s holds %d event inserts the scan can see, want %d: if an insert moved or was "+
			"written around a value this scan cannot resolve, this guard now passes by seeing "+
			"nothing", brokerEventInsert, broker, brokerEventInserts)
	}
	if legionTest != legionTestEventInserts {
		t.Fatalf("%s holds %d event inserts the scan can see, want %d: if the fixture seeder's "+
			"insert moved, grew or was removed, update this guard's exemption to match",
			legionTestEventInsert, legionTest, legionTestEventInserts)
	}
}
