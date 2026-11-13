// priority_fixture_test.go writes, in bulk, the rows the priority reads are read and measured
// over: projects, issues, their events, the priority nodes and the `advances` tuples that relate
// the two. Every write here is one COPY into the database storetest.Open cloned from this
// package's migrated template, because these corpora are thousands of rows and a route per row
// would spend the measurement's own minutes writing them.
//
// No row it writes is one a route would not: an issue carries the columns issue_create.go
// writes, an event the columns the broker appends under its owner (events/broker.go), and a node
// and a tuple the columns prioritystore.Write writes
// (store/migrations/20261112024320_priority_nodes.up.sql). Each issue row carries its own
// `last_seq`, the highest seq of the events written beside it, and each project its
// `next_number`, so a route writing to a seeded issue or project afterwards continues the
// numbering rather than colliding with it. What is left out is what the triggers do on every
// other write — the search vector, the title lexemes and the embedding queue row — which no read
// here touches.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seededWriter is the actor every bulk-written row carries: alice, who is one of the two people
// the test server records (seedPeople), so a row she wrote reads as a person's write.
const seededWriter = `{"kind":"user","id":"alice"}`

// The columns each bulk write names, in the order its rows carry them. Every column left out
// either has a default or is one nothing here writes (an issue's assignee, claim and repository,
// an event's owner other than the issue).
var (
	seededProjectColumns = []string{"key", "name", "default_assignee", "next_number", "created_at"}
	seededIssueColumns   = []string{
		"key", "project_key", "number", "title", "status", "parent_key", "created_by",
		"created_at", "updated_at", "closed_at", "rank", "last_seq",
	}
	seededEventColumns = []string{"issue_key", "seq", "type", "actor", "payload", "notify", "published_at", "created_at"}
	seededNodeColumns  = []string{
		"kind", "id", "supertag", "visibility", "state", "attributes", "made_at", "made_by", "source_event",
	}
	seededTupleColumns = []string{"from_kind", "from_id", "kind", "to_kind", "to_id", "at", "who", "source_event", "asserted"}
)

// copySeededRows writes rows into one table by COPY, and fails the test unless every one lands:
// a corpus that landed short is a measurement of a corpus nobody meant to build.
func copySeededRows(t testing.TB, database *store.Store, table string, columns []string, rows [][]any) {
	t.Helper()
	if len(rows) == 0 {
		return
	}
	landed, err := database.Pool.CopyFrom(context.Background(), pgx.Identifier{table}, columns, pgx.CopyFromRows(rows))
	if err != nil {
		t.Fatalf("copy %d rows into %s: %v", len(rows), table, err)
	}
	if landed != int64(len(rows)) {
		t.Fatalf("%d of %d rows landed in %s", landed, len(rows), table)
	}
}

// seededProject is one project row: its key, its default assignee, which every project names
// (20261111210103_projects_default_assignee_not_null) and which is alice, the seeded writer, and
// how many issues the seed writes under it, which is what `next_number` continues from.
func seededProject(key string, issues int, at time.Time) []any {
	return []any{key, key + " project", "alice", issues + 1, at}
}

// seededIssueRow is one issue of a bulk corpus, in the shape the columns above take. A closed
// issue carries its closing moment and every other carries nil, which is what `open=true` and
// the counts of work in flight read.
type seededIssueRow struct {
	key      string
	project  string
	number   int
	title    string
	status   string
	parent   *string
	rank     string
	at       time.Time
	closedAt *time.Time
	lastSeq  int
}

func (r seededIssueRow) values() []any {
	return []any{
		r.key, r.project, r.number, r.title, r.status, r.parent, seededWriter,
		r.at, r.at, r.closedAt, r.rank, r.lastSeq,
	}
}

// seededEvent is one event under an issue, as the broker appends one: published, so a server
// with NATS publishes no backlog of a seeded log, and numbered under its own owner.
func seededEvent(issueKey string, seq int, kind, payload string, at time.Time) []any {
	return []any{issueKey, seq, kind, seededWriter, payload, false, at, at}
}

// seededStatusMovePayload is the payload of the `issue.updated` event a status write appends: the
// status the issue was moved to and a `changed` list naming the field, which is what the parked
// fault's lateral reads to say when an issue was moved into `backlog` or `icebox` and by whom
// (priorityParkedQuery).
func seededStatusMovePayload(t testing.TB, status string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"status": status, "changed": []string{"status"}})
	if err != nil {
		t.Fatalf("encode a status move payload: %v", err)
	}
	return string(payload)
}

// seededPriorityNode is one priority node row: the attributes a person writes, and the three the
// store decides. The rank is the key the one order is drawn by (prioritystore.List), so a seed
// writes keys that sort into the order it meant.
func seededPriorityNode(t testing.TB, id, title, description, owner, rank string, at time.Time) []any {
	t.Helper()
	attributes, err := json.Marshal(map[string]string{
		prioritystore.AttributeTitle:       title,
		prioritystore.AttributeDescription: description,
		prioritystore.AttributeOwner:       owner,
		prioritystore.AttributeRank:        rank,
	})
	if err != nil {
		t.Fatalf("encode the attributes of %s: %v", id, err)
	}
	return []any{
		prioritystore.KindPriority, id, prioritystore.KindPriority, prioritystore.VisibilityProject,
		prioritystore.StateLive, string(attributes), at, seededWriter, prioritystore.NoEvent,
	}
}

// seededAdvancesTuple is one asserted `advances` relation: the issue at the near end, the
// priority at the far end, as prioritystore.Advance writes it.
func seededAdvancesTuple(issueKey, priority string, at time.Time) []any {
	return []any{
		prioritystore.KindIssue, issueKey, prioritystore.RelationAdvances,
		prioritystore.KindPriority, priority, at, seededWriter, prioritystore.NoEvent, true,
	}
}

// seededIssueKey is the key an issue of a bulk corpus carries: the project's key and its number,
// which is the spelling every route writes and every read addresses.
func seededIssueKey(project string, number int) string {
	return fmt.Sprintf("%s-%d", project, number)
}

// seededRank is the board key a bulk-written issue takes. The listing orders by status, then by
// this key, then by creation and the issue's own key, so zero-padded decimals put a corpus in
// the order its indexes run and a page of it is the rows the seed meant to be on it.
func seededRank(index int) string {
	return fmt.Sprintf("%08d", index)
}
