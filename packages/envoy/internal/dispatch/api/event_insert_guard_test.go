//go:build !race

// The guard is static, as lock_level_test.go's is and for the reason given there: it reads the
// module's source, so the race detector adds nothing to it.

package api

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// brokerEventInsert is the source file the serving processes insert every event through.
const brokerEventInsert = "internal/dispatch/events/broker.go"

// brokerEventInserts is how many event inserts that file holds: the one a write phase queues
// (events.Broker.QueueAppendWithLogins). It keeps the invariants below, and the count is here so
// that an insert moving out of this scan's reach reds rather than passing unseen.
const brokerEventInserts = 1

// eventInserter is a file besides the broker's that inserts events, how many inserts it holds, and
// why the invariants below have no reader to break there.
type eventInserter struct {
	inserts int
	why     string
}

// otherEventInserters are the files besides the broker's allowed to insert an event, each named
// with how many inserts the scan sees in it, so one moving out of the scan's reach, or a second
// one added, reds here.
var otherEventInserters = map[string]eventInserter{
	"going/eval/write.go": {inserts: 1, why: "the eval's seed (WriteSeed) writes, in one transaction, a database " +
		"no server has opened (holdsTheSeed refuses one that holds a project), and numbers each owner's seqs " +
		"in time order itself (orderEvents) before it commits: no relay, hub or reader by id is open on it " +
		"while it writes, so the horizon has no reader to pass an id, and every seq is final before anyone reads one"},
}

var eventInsertStatement = regexp.MustCompile(`\binsert into events\b`)

// TestOnlyTheBrokerInsertsEvents enforces what every reader of the event log by id rests on: every
// row a serving process writes is written by events.Broker.Append, which keeps two invariants no
// other insert would. The one other writer, the eval's seed (otherEventInserters), writes a
// database before any server opens it, so neither invariant has a reader there; it is named with
// its reason and its count, and any other file that inserts an event reds.
//
// The writer invariant: Append's transaction holds its transaction id before the insert draws the
// row's id from events_id_seq (lockEventOwner takes it with pg_current_xact_id). The safe horizon's
// proof needs it (events/horizon.go): a transaction that drew an id at or below one an observer saw
// committed holds a transaction id below the observer's own, so once no transaction id below the
// observer's is running, that id is final. A bare insert, in a transaction that has written
// nothing before it, takes its transaction id only as it writes the row, after its default drew the
// id, so an observation taken between the two could prove a horizon above an id still open. The
// relay (event_stream_relay.go), the delivery hub and `GET /references?since=` would then read past
// that id, and its row, committed later, would reach no open subscription until the page subscribed
// again -- a missing comment or ask on an open page, with nothing to show for it in any log.
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
func TestOnlyTheBrokerInsertsEvents(t *testing.T) {
	sources := moduleSources(t)
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	var offences []string
	broker := 0
	others := map[string]int{}
	for _, source := range sources {
		path := filepath.ToSlash(source.path)
		for _, statement := range sqlStatements(source.file, constants) {
			if !eventInsertStatement.MatchString(statement) {
				continue
			}
			if strings.HasSuffix(path, brokerEventInsert) {
				broker++
				continue
			}
			if name, ok := otherEventInserter(path); ok {
				others[name]++
				continue
			}
			offences = append(offences, path+": "+statement)
		}
	}
	if len(offences) > 0 {
		t.Fatalf("only %s may insert into events, and these statements also do:\n\t%s\n"+
			"Append the event through events.Broker.Append instead. It takes its transaction id "+
			"before the insert draws the event's id, which the safe horizon every reader of the log "+
			"by id reads through needs (events/horizon.go): a row inserted without it can commit "+
			"behind a horizon that passed its id and reach no open subscription. It also allocates the "+
			"owner's seq under the owner's row lock.", brokerEventInsert, strings.Join(offences, "\n\t"))
	}
	if broker != brokerEventInserts {
		t.Fatalf("%s holds %d event inserts the scan can see, want %d: if an insert moved or was "+
			"written around a value this scan cannot resolve, this guard now passes by seeing "+
			"nothing", brokerEventInsert, broker, brokerEventInserts)
	}
	for name, inserter := range otherEventInserters {
		if others[name] != inserter.inserts {
			t.Errorf("%s holds %d event inserts the scan can see, want %d (%s): if an insert moved or was "+
				"written around a value this scan cannot resolve, this guard now passes by seeing nothing",
				name, others[name], inserter.inserts, inserter.why)
		}
	}
}

// otherEventInserter is the entry of otherEventInserters path names, if any: the file itself, by
// its path from the module's root, and no other file of the same name.
func otherEventInserter(path string) (string, bool) {
	for name := range otherEventInserters {
		if path == name || strings.HasSuffix(path, "/"+name) {
			return name, true
		}
	}
	return "", false
}

// TestTheEventInsertGuardSeesAPlantedInserter checks that the guard lets the eval seed's insert
// through by its own path alone: an insert in another file, one of the same name in another
// directory included, is an offence.
func TestTheEventInsertGuardSeesAPlantedInserter(t *testing.T) {
	if _, ok := otherEventInserter("../../../going/eval/write.go"); !ok {
		t.Error("the guard does not know the eval seed's insert by its path")
	}
	for _, path := range []string{"../../../internal/dispatch/api/planted.go", "../../../cmd/dispatch-seed/write.go", "../../../notgoing/eval/write.go"} {
		if name, ok := otherEventInserter(path); ok {
			t.Errorf("the guard lets %s through as the named inserter %s", path, name)
		}
	}
	if !eventInsertStatement.MatchString("insert into events (issue_key, seq) values ($1, $2)") {
		t.Error("the guard does not see a planted insert into events")
	}
}
