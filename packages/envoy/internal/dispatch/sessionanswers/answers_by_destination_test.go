package sessionanswers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// destinationsIndex is the index the read finds a session's events in the window through: a GIN
// index of published_destinations and created_at, partial on a person's events of the five
// types.
const destinationsIndex = "events_person_words_destinations"

// askPayloadIndex is the index answer_events reads the listed asks' ask.answered events through,
// by their payload id. It is the one other read of events the statement makes.
const askPayloadIndex = "events_ask_payload_id"

// TestTheAnswersReadAnswersMainsRows holds the answers read (query) to the rows main's read
// (mainQuery) answers, over the answers fixture.
//
// It first takes the inventory of the fixture: the list the reader reads over it must hold an
// item of each kind and of each relation the list holds, so the rows compared below hold every
// kind of item. Then, over the fixture as written and after each edit of an item, the two
// statements must answer the same rows, column for column and in the same order, for the reader
// and for two other sessions, with the window's start 72 hours and 7 days back. A window of 7
// days holds the reader's deliveries 4 to 6 days old too.
func TestTheAnswersReadAnswersMainsRows(t *testing.T) {
	f := newAnswersFixture(t)
	ctx := context.Background()
	since := f.now.Add(-72 * time.Hour)

	inventory, err := List(ctx, f.database.Pool, answersReader, since)
	if err != nil {
		t.Fatalf("list the reader's answers: %v", err)
	}
	kinds, relations := map[string]bool{}, map[string]bool{}
	for _, item := range inventory {
		kinds[item.Kind] = true
		relations[item.Relation] = true
	}
	for _, kind := range []string{
		model.SessionAnswerKindAnswer, model.SessionAnswerKindComment, model.SessionAnswerKindMessage, model.SessionAnswerKindDeclined,
	} {
		if !kinds[kind] {
			t.Errorf("no item of the reader's list over the fixture is of kind %s", kind)
		}
	}
	for _, relation := range []string{
		model.SessionAnswerRelationAnsweredYourAsk, model.SessionAnswerRelationAnsweredAnAsk,
		model.SessionAnswerRelationDeclinedYourAsk, model.SessionAnswerRelationDeclinedAnAsk,
		model.SessionAnswerRelationRepliedOnYourAsk, model.SessionAnswerRelationRepliedInCommentThread,
		model.SessionAnswerRelationCommentedOnDocument, model.SessionAnswerRelationCommentedOnIssue,
		model.SessionAnswerRelationMentionedYou, model.SessionAnswerRelationSentYouMessage,
		model.SessionAnswerRelationRepliedToYourMessage,
	} {
		if !relations[relation] {
			t.Errorf("no item of the reader's list over the fixture has the relation %s", relation)
		}
	}

	requireMainsRows(t, f, "the fixture as written")

	// The edits of the items, each a step held to main's rows once it is written: an edit of each
	// kind that has one, and a delete of a comment and of a message, whose rows main's read
	// answers with the mark of the delete (deleted_at, deleted_by). A decline is never resolved
	// again (api's transitionAskTx refuses an ask that is resolved), so it has no step of its own.
	minutes := func(n int) time.Time { return f.now.Add(-time.Duration(n) * time.Minute) }
	toReader := agentTopic(answersReader)
	items := f.items
	for _, step := range []struct {
		name  string
		edit  func()
		shows func(listed func(kind, id string) model.SessionAnswer)
	}{
		{"an answer replaced by a second answer", func() {
			f.answer(items.answered, alice, "Postgres, with a pool of 8.", minutes(10), toReader)
		}, func(listed func(kind, id string) model.SessionAnswer) {
			if item := listed(model.SessionAnswerKindAnswer, items.answered.ID); !item.Replaced || item.Text != "Postgres, with a pool of 8." {
				t.Errorf("the reader's list holds the answer as %+v, want the second answer, replaced", item)
			}
		}},
		{"a comment on an issue edited", func() {
			f.editComment(items.onIssue, "The queue needs two indexes.", minutes(9), toReader)
		}, func(listed func(kind, id string) model.SessionAnswer) {
			if item := listed(model.SessionAnswerKindComment, items.onIssue.ID); item.EditedAt == nil || item.Text != "The queue needs two indexes." {
				t.Errorf("the reader's list holds the comment as %+v, want its edit", item)
			}
		}},
		{"a mention edited", func() {
			f.editComment(items.mention, "@session-reader, look at the pool and the queue.", minutes(8), roleTopic)
		}, func(listed func(kind, id string) model.SessionAnswer) {
			item := listed(model.SessionAnswerKindComment, items.mention.ID)
			if item.Relation != model.SessionAnswerRelationMentionedYou || item.EditedAt == nil || item.Text != "@session-reader, look at the pool and the queue." {
				t.Errorf("the reader's list holds the mention as %+v, want its edit", item)
			}
		}},
		// The edit route rewords only an open ask (ASK_NOT_OPEN), and a settlement writes a
		// decided ask's wording back into its block (docs/ask_blocks.go). The answer and the
		// decline carry their ask's question too, so their asks are reworded here as a write of
		// the row, which holds the two arms to the question the row holds, whoever wrote it.
		{"asks reworded", func() {
			f.rewordAsk(items.open, "Which jobs of the retry queue may wait?", minutes(7))
			f.rewordAsk(items.answered, "Which database, and which version, backs the queue?", minutes(6))
			f.rewordAsk(items.declined, "May the queue drop jobs older than a week?", minutes(5), toReader)
		}, func(listed func(kind, id string) model.SessionAnswer) {
			for _, carried := range []struct {
				kind, id string
				ask      *fixtureAsk
			}{
				{model.SessionAnswerKindComment, items.inOpenThread.ID, items.open},
				{model.SessionAnswerKindComment, items.inAnswersThread.ID, items.answered},
				{model.SessionAnswerKindAnswer, items.answered.ID, items.answered},
				{model.SessionAnswerKindDeclined, items.declined.ID, items.declined},
			} {
				if item := listed(carried.kind, carried.id); item.Ask == nil || item.Ask.Question != carried.ask.Question {
					t.Errorf("the reader's list holds the %s %s with the ask %+v, want the question %q", carried.kind, carried.id, item.Ask, carried.ask.Question)
				}
			}
		}},
		{"a message on an issue edited", func() {
			f.editMessage(items.onIssueMessage, "Ship it tomorrow.", minutes(4), toReader)
		}, func(listed func(kind, id string) model.SessionAnswer) {
			if item := listed(model.SessionAnswerKindMessage, items.onIssueMessage.ID); item.EditedAt == nil || item.Text != "Ship it tomorrow." {
				t.Errorf("the reader's list holds the message as %+v, want its edit", item)
			}
		}},
		{"a comment and a message deleted", func() {
			f.deleteComment(items.inOpenThread, minutes(3), toReader)
			f.deleteMessage(items.aimed, minutes(2))
		}, func(listed func(kind, id string) model.SessionAnswer) {
			for _, deleted := range []struct{ kind, id string }{
				{model.SessionAnswerKindComment, items.inOpenThread.ID},
				{model.SessionAnswerKindMessage, items.aimed.ID},
			} {
				if item := listed(deleted.kind, deleted.id); item.DeletedAt == nil || item.DeletedBy == nil || *item.DeletedBy != alice || item.Text != "" {
					t.Errorf("the reader's list holds the %s %s as %+v, want it marked deleted by %+v with no text", deleted.kind, deleted.id, item, alice)
				}
			}
		}},
	} {
		step.edit()
		f.send()
		listed, err := List(ctx, f.database.Pool, answersReader, since)
		if err != nil {
			t.Fatalf("list the reader's answers after %s: %v", step.name, err)
		}
		step.shows(func(kind, id string) model.SessionAnswer {
			t.Helper()
			index := slices.IndexFunc(listed, func(item model.SessionAnswer) bool { return item.Kind == kind && item.ID == id })
			if index < 0 {
				t.Fatalf("after %s, the reader's list holds no %s %s: %+v", step.name, kind, id, listed)
			}
			return listed[index]
		})
		requireMainsRows(t, f, step.name)
	}
}

// requireMainsRows holds query to mainQuery's rows over the fixture as step left it, for the
// reader and two other sessions (the asker, and one of the 20 sessions of the other topics), each
// with the window's start 72 hours and 7 days back. Each statement is bound as List binds it: the
// session, the window's start and the session's topic. The rows must be the same in number and
// order, and each column the same bytes the server sent, so a value that differs only in its type
// or its time zone differs too. main's read must answer some row for each, so the comparison
// holds something.
func requireMainsRows(t *testing.T, f *answersFixture, step string) {
	t.Helper()
	for _, session := range []string{answersReader, answersAsker, noiseSession(3)} {
		for _, window := range []time.Duration{72 * time.Hour, 7 * 24 * time.Hour} {
			subject := fmt.Sprintf("after %s, %s's read since %v back", step, session, window)
			since := f.now.Add(-window)
			want := readAnswerRows(t, f.database, mainQuery, session, since)
			got := readAnswerRows(t, f.database, query, session, since)
			if len(want.raw) == 0 {
				t.Errorf("%s: main's read answers no row, so the comparison holds nothing", subject)
			}
			sameAnswerRows(t, subject, got, want)
		}
	}
}

// sameAnswerRows holds got, what query answered as subject, to want, what mainQuery answered: the
// same columns, and the same rows in the same order, each column the same bytes the server sent
// and null only where main's is. It fails t through Errorf for each difference, and answers false
// where the columns or the number of rows differ, since no row is compared then.
func sameAnswerRows(t testing.TB, subject string, got, want answerRows) bool {
	t.Helper()
	if !slices.Equal(got.columns, want.columns) {
		t.Errorf("%s answers the columns %v, want main's %v", subject, got.columns, want.columns)
		return false
	}
	if len(got.raw) != len(want.raw) {
		t.Errorf("%s answers %d rows, want main's %d:\n%s\nmain's:\n%s", subject, len(got.raw), len(want.raw), got, want)
		return false
	}
	for row := range want.raw {
		for column := range want.raw[row] {
			if !bytes.Equal(got.raw[row][column], want.raw[row][column]) || (got.raw[row][column] == nil) != (want.raw[row][column] == nil) {
				t.Errorf("%s answers row %d's %s as %v, want main's %v", subject, row, want.columns[column],
					got.values[row][column], want.values[row][column])
			}
		}
	}
	return true
}

// answerRows is what an answers statement answered: its columns, by name and type, and each row
// both as the bytes the server sent and as pgx decodes them, which a failure prints.
type answerRows struct {
	columns []string
	raw     [][][]byte
	values  [][]any
}

func (r answerRows) String() string {
	var text strings.Builder
	for _, row := range r.values {
		fmt.Fprintf(&text, "  %v\n", row)
	}
	return text.String()
}

// readAnswerRows runs statement for session since since, bound as List binds the read.
func readAnswerRows(t *testing.T, database *store.Store, statement, session string, since time.Time) answerRows {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), statement, session, since, contracts.LegacyAgentSubject(session))
	if err != nil {
		t.Fatalf("read %s's answers: %v", session, err)
	}
	defer rows.Close()
	var answered answerRows
	for _, field := range rows.FieldDescriptions() {
		answered.columns = append(answered.columns, fmt.Sprintf("%s (type %d)", field.Name, field.DataTypeOID))
	}
	for rows.Next() {
		raw := make([][]byte, 0, len(answered.columns))
		for _, value := range rows.RawValues() {
			raw = append(raw, bytes.Clone(value))
		}
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("decode %s's answers: %v", session, err)
		}
		answered.raw = append(answered.raw, raw)
		answered.values = append(answered.values, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s's answers: %v", session, err)
	}
	return answered
}

// TestTheAnswersReadReadsOnlyTheSessionsWindowThroughItsIndex holds the answers read (query) to
// reading, of events, only the reader's deliveries in the window, through destinationsIndex. The
// plans are the generic plan that a prepared statement settles on
// (explaintest.AnalyzedGenericPlan) and a plan made for the values of the read, both run and
// measured, with sequential scans off and no other setting (explaintest.GenericPlan).
//
// Under each plan, the read must read events through destinationsIndex alone, but for
// answer_events' read through askPayloadIndex (readsTheSessionsWindow). That Bitmap Index Scan
// must answer exactly the events the fixture delivered to the reader in the window that are a
// person's and of the five types, and the Bitmap Heap Scan must answer each of them: so the read
// reads none of the agents' events and none of the issue.updated events delivered to the reader
// in the window, none of the 4,000 events delivered to other sessions in the window, and none of
// the reader's 300 deliveries before it. No scan of the plan may read a relation without an index
// condition (explaintest.ScansWithoutIndexCond).
//
// The control is main's text, which reads events_person_words_with_declines and filters the
// array on its rows: the check of the read of events, run on it, must fail under each plan, and
// the test fails when it passes, since the check would then be one that cannot fail.
func TestTheAnswersReadReadsOnlyTheSessionsWindowThroughItsIndex(t *testing.T) {
	f := newAnswersFixture(t)
	ctx := context.Background()
	var indexed bool
	if err := f.database.Pool.QueryRow(ctx, `select to_regclass($1) is not null`, destinationsIndex).Scan(&indexed); err != nil {
		t.Fatalf("look for %s: %v", destinationsIndex, err)
	}
	if !indexed {
		t.Errorf("the schema holds no index %s, which the read finds the session's events in the window through", destinationsIndex)
	}
	since := f.now.Add(-72 * time.Hour)
	topic := contracts.LegacyAgentSubject(answersReader)
	delivered := f.delivered(answersReader, since)
	t.Logf("the fixture delivered %d events of a person of the five types to %s in the window", delivered, answersReader)
	connection, err := f.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the generic plans: %v", err)
	}
	defer connection.Release()
	arguments := []any{answersReader, since, topic}
	parameters, literals := benchtime.PreparedArguments(t, "the answers read", arguments...)

	texts := []struct {
		name, statement string
		control         bool
	}{
		{"the answers read", query, false},
		{"main's answers read", mainQuery, true},
	}
	for number, text := range texts {
		plans := []struct {
			name string
			plan json.RawMessage
		}{
			{"the generic plan", explaintest.AnalyzedGenericPlan(t, ctx, connection, fmt.Sprintf("answers_read_%d", number),
				text.statement, parameters, literals, "set local enable_seqscan = off")},
			{"the plan for its values", explaintest.AnalyzedPlan(t, ctx, f.database.Pool, text.statement,
				[]string{"set local enable_seqscan = off"}, arguments...)},
		}
		for _, plan := range plans {
			subject := fmt.Sprintf("%s under %s", text.name, plan.name)
			if text.control {
				explaintest.RequireControlFails(t, subject, "the control", "a read of the window's every event from a read of the session's",
					plan.plan, func(t testing.TB) { readsTheSessionsWindow(t, plan.plan, subject) })
				continue
			}
			if read := readsTheSessionsWindow(t, plan.plan, subject); read != nil {
				if read.Bitmap.Loops != 1 || read.Bitmap.Rows != float64(delivered) {
					t.Errorf("%s reads %v rows through %s in each of %v loops, want once the %d events the fixture delivered to %s in the window:\n%s",
						subject, read.Bitmap.Rows, destinationsIndex, read.Bitmap.Loops, delivered, answersReader, plan.plan)
				}
				if read.Scan.Rows != read.Bitmap.Rows || read.Scan.RowsRemovedByFilter != 0 {
					t.Errorf("%s answers %v of the %v rows its bitmap holds and removes %v by its filter, want every row and none removed by its filter or its recheck:\n%s",
						subject, read.Scan.Rows, read.Bitmap.Rows, read.Scan.RowsRemovedByFilter, plan.plan)
				}
			}
			if unkeyed := explaintest.ScansWithoutIndexCond(t, plan.plan); len(unkeyed) > 0 {
				t.Errorf("%s reads %v under no index condition:\n%s", subject, unkeyed, plan.plan)
			}
		}
	}
}

// readsTheSessionsWindow holds plan, an analyzed plan of an answers read, to its read of events.
// Every scan of events is one Bitmap Heap Scan whose bitmap is one Bitmap Index Scan of
// destinationsIndex, whose Index Cond names both published_destinations and created_at, except
// answer_events' read through askPayloadIndex. The read of the window runs once, since four arms
// read its CTE and Postgres materializes a CTE that more than one reads. It answers that one
// Bitmap Heap Scan, and nil where the plan holds none or more than one. Each failure names
// subject and prints the plan through t.Errorf, which is what a control reads
// (explaintest.RequireControlFails).
func readsTheSessionsWindow(t testing.TB, plan json.RawMessage, subject string) *explaintest.BitmapHeapScan {
	t.Helper()
	for _, node := range explaintest.Nodes(t, plan) {
		if node.Relation != "events" || node.Type == "Bitmap Heap Scan" {
			continue
		}
		if (node.Type == "Index Scan" || node.Type == "Index Only Scan") && node.Index == askPayloadIndex {
			continue
		}
		t.Errorf("%s reads events by %s, want a Bitmap Heap Scan through %s, or answer_events' read through %s:\n%s",
			subject, node, destinationsIndex, askPayloadIndex, plan)
	}
	var reads []explaintest.BitmapHeapScan
	for _, scan := range explaintest.BitmapHeapScans(t, plan, "events") {
		answerEvents := len(scan.BitmapIndexScans) > 0 && !slices.ContainsFunc(scan.BitmapIndexScans, func(index explaintest.Node) bool {
			return index.Index != askPayloadIndex
		})
		if answerEvents {
			continue
		}
		if scan.Bitmap.Type != "Bitmap Index Scan" || scan.Bitmap.Index != destinationsIndex {
			t.Errorf("%s reads events by a Bitmap Heap Scan through %s, want one Bitmap Index Scan of %s:\n%s",
				subject, scan.Bitmap, destinationsIndex, plan)
			continue
		}
		if cond := scan.Bitmap.IndexCond; !explaintest.CondNamesFirstKey(cond, "published_destinations") ||
			!explaintest.CondNamesFirstKey(cond, "created_at") {
			t.Errorf("%s reads %s on `%s`, want a condition on both published_destinations and created_at, which keeps the read to the session's events in the window:\n%s",
				subject, scan.Bitmap, scan.Bitmap.IndexCond, plan)
			continue
		}
		reads = append(reads, scan)
	}
	if len(reads) != 1 {
		t.Errorf("%s reads events through %s %d times, want once:\n%s", subject, destinationsIndex, len(reads), plan)
		return nil
	}
	return &reads[0]
}
