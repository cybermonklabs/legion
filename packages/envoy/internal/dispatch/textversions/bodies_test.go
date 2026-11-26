package textversions

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
	"github.com/sjawhar/envoy/internal/explaintest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestACutCommentBodyIsRebuiltFromItsFirstRevision writes a comment longer than an event carries,
// three paragraphs and a list, as the create route writes one: its row and its block index in
// its own transaction, with its comment.created event, whose body is cut. An edit then inserts a
// block before the first, so the comment's row no longer holds the body the event cut. The
// comment.created version takes its body whole from revision 1 of the comment's blocks, in their
// order, and the row written with insertVersions holds the vector of the body the comment was
// written with.
func TestACutCommentBodyIsRebuiltFromItsFirstRevision(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()

	first := longCommentBody()
	if units := itemblocks.Units16(first); units <= contracts.DispatchEventBodyMax {
		t.Fatalf("the comment's body is %d UTF-16 units, which an event carries whole; it must be over %d",
			units, contracts.DispatchEventBodyMax)
	}
	comment := writeComment(t, database, broker, first, true)
	edited := "A new opening paragraph, written before the others once the rotation was planned.\n\n" + first
	edit := editComment(t, database, broker, comment.id, edited)

	var logs logtest.Buffer
	got := decideAndWrite(t, database, &logs, readLoggedEvents(t, database, comment.event, edit))

	created := startedBy(t, got, comment.event)
	_, truncated := model.EventBody(first)
	if !truncated || !created.BodyTruncated {
		t.Fatalf("the comment.created event does not carry the cut body it should test (truncated %v, the version's body_truncated %v)",
			truncated, created.BodyTruncated)
	}
	blocks := itemblocks.Split(first)
	texts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		texts = append(texts, block.Text)
	}
	if len(texts) != 4 {
		t.Fatalf("the comment's body splits into %d blocks, want its three paragraphs and its list", len(texts))
	}
	want := strings.Join(texts, "\n\n")
	if created.BodyFrom != bodyFromBlocks {
		t.Fatalf("the comment.created version's body_from is %q, want %q", created.BodyFrom, bodyFromBlocks)
	}
	if created.Body != want {
		t.Fatalf("the comment.created version's body is %d bytes starting %q, want revision 1's %d blocks in their order, %d bytes starting %q",
			len(created.Body), abridged(created.Body, 80), len(texts), len(want), abridged(want, 80))
	}
	if after := startedBy(t, got, edit); after.BodyFrom != bodyFromRow || after.Body != edited {
		t.Fatalf("the comment.edited version holds %d bytes from %q, want the edited body's %d from %q",
			len(after.Body), after.BodyFrom, len(edited), bodyFromRow)
	}
	if got.Cut != 0 || logs.Len() != 0 {
		t.Fatalf("the batch kept %d cut bodies and logged %q, want none", got.Cut, logs.String())
	}

	var body, bodyFrom string
	var equal bool
	if err := database.Pool.QueryRow(ctx, `
		select body, body_from, search = search_vector('', search_text($3))
		  from search_text_versions where kind = 'comment' and id = $1 and event_id = $2
	`, comment.id, comment.event, first).Scan(&body, &bodyFrom, &equal); err != nil {
		t.Fatalf("read the comment.created row: %v", err)
	}
	if body != want || bodyFrom != bodyFromBlocks {
		t.Fatalf("the comment.created row holds %d bytes from %q, want revision 1's %d bytes from %q",
			len(body), bodyFrom, len(want), bodyFromBlocks)
	}
	if !equal {
		t.Fatal("the comment.created row's vector is not search_vector('', search_text(<the body the comment was written with>))")
	}
}

// TestACutBodyNoSourceHoldsKeepsItsEventsText holds the last source of a cut body: where neither
// the comment's present row nor revision 1 of its blocks holds the body an event cut, the
// version keeps the event's text, says so in body_from, counts in the batch's cut count and is
// named, with its event, in a warning, once: a batch whose bound leaves the event to the next
// batch does not warn of it.
func TestACutBodyNoSourceHoldsKeepsItsEventsText(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	long := longCommentBody()

	t.Run("revision 1's blocks were made in another transaction than the comment", func(t *testing.T) {
		// A comment an older build wrote, whose blocks the reconciliation indexed later in a
		// transaction of its own; an edit then leaves its row holding another body.
		comment := writeComment(t, database, broker, long, false)
		indexComment(t, database, comment.id, long)
		edit := editComment(t, database, broker, comment.id, "The plan moved to the issue's spec.\n\n"+long)

		var logs logtest.Buffer
		got := decideAndWrite(t, database, &logs, readLoggedEvents(t, database, comment.event, edit))
		requireKeptEventText(t, database, got, &logs, comment.id, comment.event, long)
	})

	t.Run("a cut comment.edited version followed by a second edit", func(t *testing.T) {
		comment := writeComment(t, database, broker, "Rotate the staging key first.", true)
		firstEdit := editComment(t, database, broker, comment.id, long)
		secondEdit := editComment(t, database, broker, comment.id, "Rotated; the long plan is in the spec now.")

		// A batch bound to 1 byte decides the comment.created alone and leaves the cut edit to
		// the next batch, which is the one that warns of it.
		batch := readLoggedEvents(t, database, comment.event, firstEdit, secondEdit)
		var logs logtest.Buffer
		if bounded, err := decideBoundTo1Byte(t, database, &logs, batch); err != nil || bounded != 1 {
			t.Fatalf("the batch bound to 1 byte decided %d of the comment's events (%v), want its comment.created alone", bounded, err)
		}
		got := decideAndWrite(t, database, &logs, batch)
		if len(got.Versions) != 3 {
			t.Fatalf("the comment's three events start %d versions, want 3", len(got.Versions))
		}
		requireKeptEventText(t, database, got, &logs, comment.id, firstEdit, long)
	})
}

// TestACutEditThatKeepsTheBodyStartsNoVersion writes a comment longer than an event carries, and
// a batch writes its comment.created version, whole from the comment's row. In a later batch, so
// the comment's version is in the table and not in the batch, a comment.edited whose event cuts
// the body writes the same body again. Its whole body, read from the row, equals the comment's
// latest version's, so it starts no version, and the table keeps the one row.
func TestACutEditThatKeepsTheBodyStartsNoVersion(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	long := longCommentBody()
	comment := writeComment(t, database, broker, long, true)
	var logs logtest.Buffer
	first := decideAndWrite(t, database, &logs, readLoggedEvents(t, database, comment.event))
	if created := startedBy(t, first, comment.event); created.BodyFrom != bodyFromRow || created.Body != long {
		t.Fatalf("the comment.created version holds %d bytes from %q, want the comment's %d from %q",
			len(created.Body), created.BodyFrom, len(long), bodyFromRow)
	}

	edit := editComment(t, database, broker, comment.id, long)
	edited := readLoggedEvents(t, database, edit)
	if _, truncated := model.EventBody(long); !truncated {
		t.Fatal("the body is one an event carries whole, which tests no cut")
	}
	got := decideAndWrite(t, database, &logs, edited)
	if len(got.Versions) != 0 {
		t.Fatalf("the cut comment.edited that keeps the body starts %d versions (the first from %q), want none",
			len(got.Versions), got.Versions[0].BodyFrom)
	}
	var rows int
	if err := database.Pool.QueryRow(t.Context(),
		`select count(*) from search_text_versions where kind = 'comment' and id = $1`, comment.id).Scan(&rows); err != nil {
		t.Fatalf("count the comment's rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("the comment has %d rows, want the one of its comment.created", rows)
	}
}

// TestAReversedPairOfCutEditsKeepsEachEditsText writes a comment and then two edits of it whose
// bodies are longer than an event carries, in transactions that interleave as a reversed pair:
// A begins, B begins, B writes the comment's row and appends its comment.edited and commits, and
// A then writes the row and appends its comment.edited and commits. B's event has the lower id
// and the later created_at, and the row holds A's body, with A's start as its edited_at, which is
// before B's created_at. The row holds an event's text only where no later event of the comment
// changes it, and A's edit does, so B's version keeps its event's cut text, since no source holds
// B's whole body any more; A's version, which no later event changes, takes A's whole body from
// the row.
//
// Its cases are a comment on an issue and a comment on a project's document of no issue, whose
// events the document owns: they name the document and no issue, and take the document's seq
// (api's ownerOf). So presentBody finds A's edit among the later events of each kind of owner.
// Each case reads its batch as the fill's batch reads it (readBatch), which reads the document an
// event names.
func TestAReversedPairOfCutEditsKeepsEachEditsText(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	document := insertProjectDocument(t, database)
	for _, test := range []struct {
		name  string
		owner commentOwner
	}{
		{"a comment on an issue", commentOwner{issue: new(textversionstest.Issue)}},
		{"a comment on a project's document", commentOwner{document: &document}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			long := longCommentBody()
			aBody := "Edit A moves the rotation to the spring.\n\n" + long
			bBody := "Edit B moves the rotation to the autumn.\n\n" + long
			comment := writeOwnedComment(t, database, broker, test.owner, "Rotate the staging key first.", true)

			a, _ := textversionstest.BeginTransaction(t, database)
			defer func() { _ = a.Rollback(context.Background()) }()
			b, _ := textversionstest.BeginTransaction(t, database)
			defer func() { _ = b.Rollback(context.Background()) }()
			bEvent := writeEdit(t, b, broker, comment.id, bBody)
			if err := b.Commit(ctx); err != nil {
				t.Fatalf("commit transaction B: %v", err)
			}
			aEvent := writeEdit(t, a, broker, comment.id, aBody)
			if err := a.Commit(ctx); err != nil {
				t.Fatalf("commit transaction A: %v", err)
			}
			var aAt, bAt, editedAt time.Time
			if err := database.Pool.QueryRow(ctx, `
				select (select created_at from events where id = $1), (select created_at from events where id = $2),
				       (select edited_at from comments where id = $3::uuid)
			`, aEvent, bEvent, comment.id).Scan(&aAt, &bAt, &editedAt); err != nil {
				t.Fatalf("read the times of the pair: %v", err)
			}
			if bEvent > aEvent || !bAt.After(aAt) || !editedAt.Equal(aAt) {
				t.Fatalf("the pair is not reversed: B's event %d at %v, A's event %d at %v, the row edited at %v; want B's id lower, B's time later and the row edited at A's",
					bEvent, bAt, aEvent, aAt, editedAt)
			}

			batch := readBatch(t, database, comment.event-1, aEvent)
			for _, e := range batch {
				if read := (commentOwner{issue: e.IssueKey, document: e.ArtifactID}); read.String() != test.owner.String() {
					t.Fatalf("event %d (%s) is read as owned by %s, want the comment's owner, %s", e.ID, e.Type, read, test.owner)
				}
			}
			var logs logtest.Buffer
			got := decideAndWrite(t, database, &logs, batch)
			aCut, _ := model.EventBody(aBody)
			bCut, _ := model.EventBody(bBody)
			holds := func(v version) string {
				switch v.Body {
				case aBody:
					return "A's whole body"
				case bBody:
					return "B's whole body"
				case aCut:
					return "A's cut text"
				case bCut:
					return "B's cut text"
				}
				return fmt.Sprintf("%d other bytes", len(v.Body))
			}
			versions := make([]string, 0, len(got.Versions))
			for _, v := range got.Versions {
				versions = append(versions, fmt.Sprintf("event %d: %s from %q", v.EventID, holds(v), v.BodyFrom))
			}
			if len(got.Versions) != 3 {
				t.Fatalf("the comment's comment.created (event %d), B's edit (event %d) and A's edit (event %d) start %d versions, want 3: %v",
					comment.event, bEvent, aEvent, len(got.Versions), versions)
			}
			requireKeptEventText(t, database, got, &logs, comment.id, bEvent, bBody)
			if v := startedBy(t, got, aEvent); v.BodyFrom != bodyFromRow || v.Body != aBody {
				t.Fatalf("A's version holds %s from %q, want A's whole body from %q", holds(v), v.BodyFrom, bodyFromRow)
			}
			var stored, bodyFrom string
			if err := database.Pool.QueryRow(ctx, `
				select body, body_from from search_text_versions where kind = 'comment' and id = $1 and event_id = $2
			`, comment.id, aEvent).Scan(&stored, &bodyFrom); err != nil {
				t.Fatalf("read the row of A's edit: %v", err)
			}
			if stored != aBody || bodyFrom != bodyFromRow {
				t.Fatalf("the row of A's edit holds %d bytes from %q, want A's whole %d bytes from %q", len(stored), bodyFrom, len(aBody), bodyFromRow)
			}
		})
	}
}

// TestEachKindsWholeBodyStatementsReadNoPayloadTheyNeedNotRead holds what Postgres does with the
// statements that read each kind of post's whole body (postBodyReads), its present row and its
// body history, under the plan the fill's connection may keep once it caches a statement, a
// generic one, which orders a statement's conditions by their cost alone. Neither statement
// unpacks the payload of an event of another type than those that change the post's text: with
// the owner of a post, written and then edited as the routes write them, holding 3,000 later
// events of another type, each with a payload of about 3 KB, which Postgres stores out of line,
// each statement, read for the edit, reads fewer buffers than one for each 2 of those events. The
// present row's statement answers the post's row, which no later event changes; the history's,
// which counts the owner's events of the types that change it from the post's create, answers
// none, since none does.
//
// That bound is stable, with a wide margin both ways. A buffer count does not depend on time or
// on what the cache holds (the test sums buffers hit and read), only on how the rows lie on their
// pages. Reading only the later events' types reads their rows, dozens to a page, and their
// index: 77 buffers when this was written, 19 times under the bound. Unpacking each payload, as
// the statement does with the payload's test outside the CASE, reads the out-of-line value's
// index and pages for each event besides: 3 buffers an event, 9,077 in all, 6 times over it.
func TestEachKindsWholeBodyStatementsReadNoPayloadTheyNeedNotRead(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	connection, err := database.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()
	posts := map[string]struct {
		write     func(t *testing.T) writtenComment
		otherType string
	}{
		kindComment: {func(t *testing.T) writtenComment {
			return writeComment(t, database, broker, "Rotate the staging key first.", false)
		}, "comment.resolved"},
		kindMessage: {func(t *testing.T) writtenComment {
			return writeIssueMessage(t, database, broker, "Rotate the staging key first.")
		}, "message.accepted"},
	}
	for _, kind := range slices.Sorted(maps.Keys(postBodyReads)) {
		read := postBodyReads[kind]
		post, ok := posts[kind]
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			if !ok {
				t.Fatalf("the test writes no %s, whose whole body postBodyReads reads: write one", kind)
			}
			const later = 3000
			if slices.Contains(read.laterTypes, post.otherType) {
				t.Fatalf("%s changes a %s's text, so its payload is read: pick another type", post.otherType, kind)
			}
			written := post.write(t)
			edit := correctPost(t, database, broker, kind, written.id, "Rotate the staging key, then production.", false)
			e := readLoggedEvents(t, database, edit)[0]
			var before int64
			if err := database.Pool.QueryRow(ctx, `select pg_relation_size(reltoastrelid) from pg_class where oid = 'public.events'::regclass`).Scan(&before); err != nil {
				t.Fatalf("measure the events' out-of-line values: %v", err)
			}
			// The issue's last_seq moves past them too, so a later append of the issue, the next
			// kind's post, takes the seq after them.
			if _, err := database.Pool.Exec(ctx, `
				with later as (
				       insert into events (issue_key, seq, type, actor, payload, notify)
				       select $1, $2 + g, $3, '{"kind":"user","id":"alice"}',
				              jsonb_build_object('id', $4::text, 'note',
				                (select string_agg(md5(g::text || ':' || part::text), '') from generate_series(1, 96) part)),
				              false
				         from generate_series(1, $5::integer) g
				       returning seq)
				update issues set last_seq = (select max(seq) from later) where key = $1
			`, e.IssueKey, e.Seq, post.otherType, written.id, later); err != nil {
				t.Fatalf("write the %s's later events: %v", kind, err)
			}
			var outOfLine int64
			if err := database.Pool.QueryRow(ctx,
				`select pg_relation_size(reltoastrelid) from pg_class where oid = 'public.events'::regclass`).Scan(&outOfLine); err != nil {
				t.Fatalf("measure the events' out-of-line values: %v", err)
			}
			if outOfLine-before < later*2048 {
				t.Fatalf("the events' out-of-line values grew by %d bytes, under 2 KB for each of the %d later events, so their payloads are not stored out of line as the test needs",
					outOfLine-before, later)
			}

			// EXECUTE takes the statements' arguments as presentBody and historyBody pass them
			// (arguments, historyArguments), as literals Postgres quotes, and each statement is
			// prepared with the parameter types Postgres infers for it when it is prepared with
			// none, as the fill's pgx connection prepares it. No fill ran, so the history counts
			// the post's changes from its create's seq, which the batch would give it.
			created := readLoggedEvents(t, database, written.event)[0]
			var literals []string
			if err := database.Pool.QueryRow(ctx,
				`select array[format('%L', $1::text), format('%L', $2::text), format('%L', $3::text), format('%s', $4::integer), format('%L', $5::text[]),
				              format('%L', $6::text), format('%s', $7::integer)]`,
				read.historyArguments(e, kind, written.id, created.Seq)...).Scan(&literals); err != nil {
				t.Fatalf("write the statements' arguments: %v", err)
			}
			for _, statement := range []struct {
				name, sql  string
				parameters []string
				rows       float64
			}{
				{"present_body", read.present, []string{"text", "text", "uuid", "integer", "text[]"}, 1},
				{"history_body", read.history, []string{"text", "text", "uuid", "integer", "text[]", "text", "integer"}, 0},
			} {
				plan := explaintest.AnalyzedGenericPlan(t, ctx, connection, statement.name, statement.sql,
					statement.parameters, literals[:len(statement.parameters)])
				root := explaintest.Nodes(t, plan)[0]
				if root.Rows != statement.rows {
					t.Fatalf("the %s statement of a %s answered %v rows; want %v, since no later event changes it", statement.name, kind, root.Rows, statement.rows)
				}
				buffers := root.SharedHitBlocks + root.SharedReadBlocks
				t.Logf("the %s statement of a %s read %.0f buffers past %d later events of another type", statement.name, kind, buffers, later)
				if buffers >= later/2 {
					t.Fatalf("the %s statement of a %s read %.0f buffers past %d later events of another type; want fewer than %d, one for each 2 of them, as reading their types alone reads",
						statement.name, kind, buffers, later, later/2)
				}
			}
		})
	}
}

// TestAnIssueCreatedAfterItsFirstDocumentVersionIsWarnedOfOnce writes issue ACME-1 with version
// 1 of its primary document in a transaction of its own, before the one that appends its
// issue.created, so version 1 was not made at the event's created_at; and between the two, issue
// ACME-2 as the create route writes one (createIssue), with version 1 in the transaction of its
// issue.created, so ACME-1's issue.created is second in the batch. The batch warns of ACME-1's
// event once (requireWarnedOnceByItsBatch): bound to 1 byte, it decides ACME-2's event alone and
// logs no warning, and whole, it logs exactly one, which names ACME-1 and its event, and none of
// ACME-2. The warning names when ACME-1's version 1 was made, and the batch starts both versions.
func TestAnIssueCreatedAfterItsFirstDocumentVersionIsWarnedOfOnce(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	textversionstest.SeedProject(t, database)
	broker := events.NewBroker()

	var early textversionstest.WrittenIssue
	textversionstest.Commit(t, database, func(tx pgx.Tx) {
		early = textversionstest.InsertIssue(t, tx, 1, "Rotate the signing keys", "# Rotation\n\nMove each signing key to its successor.")
	})
	routedEvent := createIssue(t, database, broker, 2, "Audit the verifiers", "# Audit\n\nRead each verifier's key.")
	lateEvent := textversionstest.CommitEvents(t, database, broker, early.Event("issue.created", early.Title))[0]
	batch := readLoggedEvents(t, database, routedEvent, lateEvent)
	var made time.Time
	if err := database.Pool.QueryRow(ctx, `select created_at from artifact_versions where artifact_id = $1::uuid and number = 1`,
		early.Artifact).Scan(&made); err != nil {
		t.Fatalf("read ACME-1's first document version: %v", err)
	}
	if made.Equal(batch[1].CreatedAt) {
		t.Fatal("ACME-1's first document version was made at its issue.created's created_at, which tests no warning")
	}

	got, warned := requireWarnedOnceByItsBatch(t, database, batch, "did not write version 1 of its primary document", early.Key, lateEvent)
	if len(got.Versions) != 2 {
		t.Fatalf("the two issue.created events start %d versions, want 2", len(got.Versions))
	}
	if at, err := time.Parse(time.RFC3339Nano, warned["version_created_at"]); err != nil || !at.Equal(made) {
		t.Fatalf("the warning names version 1 of %s's primary document made at %q, want %s",
			early.Key, warned["version_created_at"], made.Format(time.RFC3339Nano))
	}
}

// TestAnIssueEventBeforeAnyVersionOfItsIssueIsWarnedOfOnce holds the two warnings of an issue
// event that meets its issue with no version, in the table or earlier in the batch, which only a
// log that lacks the issue's issue.created holds (seedIssue): of an issue whose rows are there,
// which its version reads, and of an issue whose rows are not, which starts no version. Each case
// is a batch whose first event, the issue.created of an issue written as the create route writes
// one, warns of nothing, and whose second is the issue.updated warned of: a batch bound to 1 byte
// decides the first event alone and logs no warning, and the whole batch then logs exactly one,
// which names the issue and the event. The cases are two batches rather than one of three events,
// since a bound that ends a batch at its second event never decides its third.
func TestAnIssueEventBeforeAnyVersionOfItsIssueIsWarnedOfOnce(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	textversionstest.SeedProject(t, database)
	broker := events.NewBroker()

	t.Run("an issue whose rows a reset of the log kept", func(t *testing.T) {
		// ACME-1's rows, with version 1 of its primary document, are written with no
		// issue.created, as a reset of the log that kept them leaves them. A retitle is then
		// written as the update route writes one, and version 2 of the document after it, so the
		// document's version at the retitle's time is not its latest.
		var issue textversionstest.WrittenIssue
		textversionstest.Commit(t, database, func(tx pgx.Tx) {
			issue = textversionstest.InsertIssue(t, tx, 1, "Rotate the signing keys", "# Rotation\n\nMove each signing key to its successor.")
		})
		created := createIssue(t, database, broker, 2, "Audit the verifiers", "# Audit\n\nRead each verifier's key.")
		title := issue.Title + ", then audit the verifiers"
		var retitled int64
		textversionstest.Commit(t, database, func(tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `update issues set title = $2 where key = $1`, issue.Key, title); err != nil {
				t.Fatalf("retitle %s: %v", issue.Key, err)
			}
			retitled = textversionstest.AppendEvents(t, tx, broker, issue.Event("issue.updated", title))[0]
		})
		textversionstest.Commit(t, database, func(tx pgx.Tx) {
			textversionstest.AppendEvents(t, tx, broker, issue.InsertVersion(t, tx, 2, "# Rotation\n\nMove each signing key, then audit each verifier."))
		})

		got, warned := requireWarnedOnceByItsBatch(t, database, readLoggedEvents(t, database, created, retitled),
			"found no earlier version of the issue in the log", issue.Key, retitled)
		if len(got.Versions) != 2 {
			t.Fatalf("the batch's two events start %d versions, want 2", len(got.Versions))
		}
		// The retitle's version holds the title its event carries, which the issue's row holds
		// too, and the version of the primary document of the issue's rows made by the event's
		// time; the warning names where its body came from.
		want := specVersion{artifact: issue.Artifact, number: 1}
		v := startedBy(t, got, retitled)
		if spec, _ := v.spec(); v.Body != title || v.BodyFrom != bodyFromEvent || spec != want {
			t.Fatalf("the retitle's version holds %q from %q and names version %d of document %q; want %q from %q and version %d of %q, the primary document's at the event's time",
				v.Body, v.BodyFrom, spec.number, spec.artifact, title, bodyFromEvent, want.number, want.artifact)
		}
		if warned["body_from"] != bodyFromEvent {
			t.Fatalf("the warning names body_from %q, want the version's %q", warned["body_from"], bodyFromEvent)
		}
	})

	t.Run("an issue the database does not hold", func(t *testing.T) {
		// An event's issue_key references issues, so an event whose own columns name an issue
		// names one the database holds: an issue event of an issue with no row names it in its
		// payload alone, here in an event of the project. ACME-4 is never written.
		created := createIssue(t, database, broker, 3, "Retire the old keys", "# Retirement\n\nRetire each key the rotation replaced.")
		project, missing := textversionstest.Project, textversionstest.Project+"-4"
		retitled := textversionstest.CommitEvents(t, database, broker, model.Event{ProjectKey: &project, Type: "issue.updated",
			Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: map[string]any{"key": missing, "title": "Audit the verifiers again"}})[0]

		got, _ := requireWarnedOnceByItsBatch(t, database, readLoggedEvents(t, database, created, retitled),
			"names an issue the log never created and the database does not hold", missing, retitled)
		if len(got.Versions) != 1 {
			t.Fatalf("the batch's two events start %d versions, want the issue.created's alone", len(got.Versions))
		}
		startedBy(t, got, created)
	})
}

// requireKeptEventText holds the version event starts of comment id, whose body was body when
// the event was written, to the event's cut text: body_from cut, as decided and as written, one
// cut body in the batch, and one warning, naming the comment and the event.
func requireKeptEventText(t *testing.T, database *store.Store, got decided, logs *logtest.Buffer, id string, event int64, body string) {
	t.Helper()
	cut, truncated := model.EventBody(body)
	if !truncated {
		t.Fatal("the body is one an event carries whole, which tests no cut")
	}
	v := startedBy(t, got, event)
	if !v.BodyTruncated || v.BodyFrom != bodyFromCut || v.Body != cut {
		t.Fatalf("the version of event %d holds %d bytes from %q (body_truncated %v), want its event's cut %d bytes from %q",
			event, len(v.Body), v.BodyFrom, v.BodyTruncated, len(cut), bodyFromCut)
	}
	if got.Cut != 1 {
		t.Fatalf("the batch counts %d cut bodies, want 1", got.Cut)
	}
	var stored, bodyFrom string
	if err := database.Pool.QueryRow(t.Context(), `
		select body, body_from from search_text_versions where kind = 'comment' and id = $1 and event_id = $2
	`, id, event).Scan(&stored, &bodyFrom); err != nil {
		t.Fatalf("read the row of event %d: %v", event, err)
	}
	if stored != cut || bodyFrom != bodyFromCut {
		t.Fatalf("the row of event %d holds %d bytes from %q, want its event's cut %d bytes from %q",
			event, len(stored), bodyFrom, len(cut), bodyFromCut)
	}
	warnings := loggedWarnings(t, logs)
	if len(warnings) != 1 {
		t.Fatalf("the batch logged %d warnings, want the one of the cut body: %q", len(warnings), logs.String())
	}
	if warnings[0]["kind"] != "comment" || warnings[0]["id"] != id || warnings[0]["event_id"] != fmt.Sprint(event) {
		t.Fatalf("the warning names %v %v at event %v, want comment %s at event %d: %q",
			warnings[0]["kind"], warnings[0]["id"], warnings[0]["event_id"], id, event, logs.String())
	}
}

// requireWarnedOnceByItsBatch holds the warning of event, the second of batch, to the batch that
// decides event: batch bound to 1 byte decides its first event alone and logs no warning, and the
// whole batch, decided and written (decideAndWrite), then logs exactly one, whose message holds
// message and which names issue and event. It answers what the whole batch decided and the
// warning, with every attribute as its text.
func requireWarnedOnceByItsBatch(t *testing.T, database *store.Store, batch []event, message, issue string, event int64) (decided, map[string]string) {
	t.Helper()
	var logs logtest.Buffer
	decidedEvents, err := decideBoundTo1Byte(t, database, &logs, batch)
	if err != nil || decidedEvents != 1 {
		t.Fatalf("the batch bound to 1 byte decided %d of its %d events (%v), want the first alone", decidedEvents, len(batch), err)
	}
	if warnings := loggedWarnings(t, &logs); len(warnings) != 0 {
		t.Fatalf("the batch bound to 1 byte, which leaves event %d to the next batch, logged %d warnings, want none: %q",
			event, len(warnings), logs.String())
	}
	got := decideAndWrite(t, database, &logs, batch)
	warnings := loggedWarnings(t, &logs)
	if len(warnings) != 1 {
		t.Fatalf("the batches logged %d warnings, want the one of event %d: %q", len(warnings), event, logs.String())
	}
	warned := warnings[0]
	if !strings.Contains(warned["msg"], message) || warned["issue"] != issue || warned["event_id"] != fmt.Sprint(event) {
		t.Fatalf("the warning says %q of issue %s at event %s; want one that says %q of issue %s at event %d",
			warned["msg"], warned["issue"], warned["event_id"], message, issue, event)
	}
	return got, warned
}

// decideBoundTo1Byte decides the versions of batch as a batch bound to 1 byte decides them, with
// the warnings logged to logs, in a transaction it rolls back, and answers how many of batch's
// events it decided.
func decideBoundTo1Byte(t *testing.T, database *store.Store, logs *logtest.Buffer, batch []event) (int, error) {
	t.Helper()
	tx, _ := textversionstest.BeginTransaction(t, database)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, decidedEvents, err := decideVersions(t.Context(), tx, slog.New(slog.NewJSONHandler(logs, nil)), &currentRules, batch, 1)
	return decidedEvents, err
}

// createIssue writes issue ACME-n of textversionstest.Project with title and its primary document
// of markdown, and its issue.created event, in one transaction, as the create route writes one,
// so version 1 of the document is made at the event's created_at, and answers the event's id.
func createIssue(t *testing.T, database *store.Store, broker *events.Broker, n int, title, markdown string) int64 {
	t.Helper()
	var event int64
	textversionstest.Commit(t, database, func(tx pgx.Tx) {
		issue := textversionstest.InsertIssue(t, tx, n, title, markdown)
		event = textversionstest.AppendEvents(t, tx, broker, issue.Event("issue.created", title))[0]
	})
	return event
}

// longCommentBody is a body over contracts.DispatchEventBodyMax UTF-16 units: three paragraphs of
// about 10,500 units each, which together fit in an event, and a list after them, which does not,
// so its event carries the three paragraphs alone. Each block is under
// contracts.DispatchBlockUnits, so the create route would take it.
func longCommentBody() string {
	paragraph := func(topic string) string {
		var b strings.Builder
		for number := 1; b.Len() < 10_500; number++ {
			fmt.Fprintf(&b, "The %s rotation step %d moves one signing key to its successor. ", topic, number)
		}
		return strings.TrimSpace(b.String())
	}
	var list strings.Builder
	for number := 1; number <= 60; number++ {
		if number > 1 {
			list.WriteString("\n")
		}
		fmt.Fprintf(&list, "- delta check %d confirms the verifier reads the new key", number)
	}
	return strings.Join([]string{paragraph("alpha"), paragraph("bravo"), paragraph("charlie"), list.String()}, "\n\n")
}

// writtenComment is a comment writeOwnedComment wrote and the id of its comment.created event.
type writtenComment struct {
	id    string
	event int64
}

// commentOwner is what owns a comment and its events, as the routes name it (api's ownerOf): an
// issue, or a project's document of no issue, whose comments' events name the document and no
// issue and take the document's seq. Exactly one of the two is set.
type commentOwner struct {
	issue, document *string
}

// String names the owner: its issue's key or its document's id.
func (o commentOwner) String() string {
	switch {
	case o.issue != nil:
		return "issue " + *o.issue
	case o.document != nil:
		return "document " + *o.document
	}
	return "no owner"
}

// writeComment writes a comment of body on textversionstest.Issue (writeOwnedComment).
func writeComment(t *testing.T, database *store.Store, broker *events.Broker, body string, indexed bool) writtenComment {
	t.Helper()
	return writeOwnedComment(t, database, broker, commentOwner{issue: new(textversionstest.Issue)}, body, indexed)
}

// noServerURL is the server URL these rows index a comment under. No comment they write holds a
// dashboard URL, which is the one form the server's own URL reads (refs.IndexItem), so none names
// anything.
const noServerURL = ""

// writeOwnedComment writes a comment of body that owner owns, with its comment.created event,
// whose body the event carries as model.EventBody cuts it, in one transaction, as the create route
// does; with indexed, its block index too, in that same transaction (refs.IndexItem), as every
// route that writes a comment does, and without, none, as a build before the block index wrote
// one.
func writeOwnedComment(t *testing.T, database *store.Store, broker *events.Broker, owner commentOwner, body string, indexed bool) writtenComment {
	t.Helper()
	ctx := t.Context()
	var comment writtenComment
	textversionstest.Commit(t, database, func(tx pgx.Tx) {
		var createdAt time.Time
		if err := tx.QueryRow(ctx, `
			insert into comments (issue_key, artifact_id, author, body) values ($1, $2::uuid, '{"kind":"user","id":"alice"}', $3)
			returning id::text, created_at
		`, owner.issue, owner.document, body).Scan(&comment.id, &createdAt); err != nil {
			t.Fatalf("insert the comment: %v", err)
		}
		if indexed {
			if _, err := refs.IndexItem(ctx, tx, refs.ItemComment, comment.id, body, noServerURL, createdAt, nil, nil); err != nil {
				t.Fatalf("index the comment's blocks: %v", err)
			}
		}
		comment.event = appendOwnedCommentEvent(t, tx, broker, owner, "comment.created", comment.id, body, createdAt)
	})
	return comment
}

// insertProjectDocument writes the row of a document of textversionstest.Project that no issue
// owns, which is all a comment on it needs, and answers its id.
func insertProjectDocument(t *testing.T, database *store.Store) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(t.Context(), `
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ($1, 'runbook', 'runbook.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text
	`, textversionstest.Project).Scan(&id); err != nil {
		t.Fatalf("insert the project's document: %v", err)
	}
	return id
}

// indexComment indexes the blocks of comment id, whose body is body, in a transaction of its own,
// as the block index's reconciliation indexes a comment a build without the index wrote.
func indexComment(t *testing.T, database *store.Store, id, body string) {
	t.Helper()
	ctx := t.Context()
	textversionstest.Commit(t, database, func(tx pgx.Tx) {
		var createdAt time.Time
		if err := tx.QueryRow(ctx, `select created_at from comments where id = $1::uuid`, id).Scan(&createdAt); err != nil {
			t.Fatalf("read the comment: %v", err)
		}
		if _, err := refs.IndexItem(ctx, tx, refs.ItemComment, id, body, noServerURL, createdAt, nil, nil); err != nil {
			t.Fatalf("index the comment's blocks: %v", err)
		}
	})
}

// editComment writes body as comment id's new body in a transaction of its own, as the edit route
// does (writeEdit), and answers the id of its comment.edited event.
func editComment(t *testing.T, database *store.Store, broker *events.Broker, id, body string) int64 {
	t.Helper()
	var event int64
	textversionstest.Commit(t, database, func(tx pgx.Tx) { event = writeEdit(t, tx, broker, id, body) })
	return event
}

// writeEdit writes body as comment id's new body in tx, as the edit route does: its edited_at
// tx's now(), its block index and its comment.edited event, which the comment's owner owns, and
// whose id it answers.
func writeEdit(t *testing.T, tx pgx.Tx, broker *events.Broker, id, body string) int64 {
	t.Helper()
	ctx := t.Context()
	var editedAt time.Time
	var owner commentOwner
	if err := tx.QueryRow(ctx, `
		update comments set body = $2, edited_at = now() where id = $1::uuid returning edited_at, issue_key, artifact_id::text
	`, id, body).Scan(&editedAt, &owner.issue, &owner.document); err != nil {
		t.Fatalf("edit the comment: %v", err)
	}
	if _, err := refs.IndexItem(ctx, tx, refs.ItemComment, id, body, noServerURL, editedAt, nil, nil); err != nil {
		t.Fatalf("index the edited comment's blocks: %v", err)
	}
	return appendOwnedCommentEvent(t, tx, broker, owner, "comment.edited", id, body, editedAt)
}

// appendCommentEvent appends the event of type of comment id on textversionstest.Issue, whose body
// is body, through the broker, and answers its id (appendOwnedCommentEvent).
func appendCommentEvent(t *testing.T, tx pgx.Tx, broker *events.Broker, eventType, id, body string, createdAt time.Time) int64 {
	t.Helper()
	return appendOwnedCommentEvent(t, tx, broker, commentOwner{issue: new(textversionstest.Issue)}, eventType, id, body, createdAt)
}

// appendOwnedCommentEvent appends the event of type of comment id, which owner owns, whose body
// is body, through the broker, and answers its id. The event and the comment in its payload name
// the owner, as the routes write them.
func appendOwnedCommentEvent(t *testing.T, tx pgx.Tx, broker *events.Broker, owner commentOwner, eventType, id, body string, createdAt time.Time) int64 {
	t.Helper()
	actor := model.Actor{Kind: "user", ID: "alice"}
	payload := model.CommentEventPayload{Comment: model.Comment{
		ID: id, IssueKey: owner.issue, ArtifactID: owner.document, Author: actor, CreatedAt: createdAt,
		Mentions: []model.Mention{}, Deliveries: []model.CommentDelivery{},
	}}
	payload.Body, payload.BodyTruncated = model.EventBody(body)
	appended, err := broker.Append(t.Context(), tx, model.Event{
		IssueKey: owner.issue, ArtifactID: owner.document, Type: eventType, Actor: actor, Payload: payload,
	})
	if err != nil {
		t.Fatalf("append %s: %v", eventType, err)
	}
	return appended.ID
}

// readLoggedEvents reads the events ids from the log as the fill reads it: the columns of each
// row the rules read, its project as the log names it, and an ask's body (askBodySQL), in id
// order.
func readLoggedEvents(t *testing.T, database *store.Store, ids ...int64) []event {
	t.Helper()
	rows, err := database.Pool.Query(t.Context(), `
		select e.id, e.type, e.issue_key, e.artifact_id::text, coalesce(e.project_key, i.project_key, ar.project_key, ''), e.seq,
		       e.created_at, e.payload,
		       case when e.type = any($2::text[]) then `+askBodySQL+` end
		  from events e
		  left join issues i on i.key = e.issue_key
		  left join artifacts ar on ar.id = e.artifact_id
		 where e.id = any($1) order by e.id
	`, ids, askTypes)
	if err != nil {
		t.Fatalf("read the events: %v", err)
	}
	defer rows.Close()
	var read []event
	for rows.Next() {
		var e event
		var payload []byte
		if err := rows.Scan(&e.ID, &e.Type, &e.IssueKey, &e.ArtifactID, &e.Project, &e.Seq, &e.CreatedAt, &payload, &e.AskBody); err != nil {
			t.Fatalf("read an event: %v", err)
		}
		e.Payload = payload
		read = append(read, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the events: %v", err)
	}
	if len(read) != len(ids) {
		t.Fatalf("read %d of the %d events", len(read), len(ids))
	}
	return read
}

// readBatch reads the events above after up to through as the fill's batch reads them, with the
// production bounds (filler.readEvents, readEventsQuery), and fails the test where the bounds
// leave any of them to a later batch.
func readBatch(t *testing.T, database *store.Store, after, through int64) []event {
	t.Helper()
	ctx := t.Context()
	tx, _ := textversionstest.BeginTransaction(t, database)
	defer func() { _ = tx.Rollback(context.Background()) }()
	read, _, err := newFiller(database.Pool, nil).readEvents(ctx, tx, after, through)
	if err != nil {
		t.Fatalf("read the events after event %d: %v", after, err)
	}
	if len(read) == 0 || read[len(read)-1].ID != through {
		t.Fatalf("the fill's read took %d events after event %d, which end before event %d", len(read), after, through)
	}
	return read
}

// decideAndWrite decides the versions of a batch of events and writes them, in one transaction,
// as the fill's batch does, with the warnings logged to logs.
func decideAndWrite(t *testing.T, database *store.Store, logs *logtest.Buffer, batch []event) decided {
	t.Helper()
	ctx := t.Context()
	var got decided
	textversionstest.Commit(t, database, func(tx pgx.Tx) {
		decidedVersions, _, err := decideVersions(ctx, tx, slog.New(slog.NewJSONHandler(logs, nil)), &currentRules, batch, batchBytes)
		if err != nil {
			t.Fatalf("decide the versions: %v", err)
		}
		if err := insertVersions(ctx, tx, decidedVersions.Versions, false); err != nil {
			t.Fatalf("write the versions: %v", err)
		}
		got = decidedVersions
	})
	return got
}

// startedBy is the version event started in got.
func startedBy(t *testing.T, got decided, event int64) version {
	t.Helper()
	for _, v := range got.Versions {
		if v.EventID == event {
			return v
		}
	}
	t.Fatalf("event %d starts no version; the batch decided %d", event, len(got.Versions))
	return version{}
}

// loggedWarnings is each WARN record of logs, a JSON handler's output, with every attribute as
// its text. JSONLines decodes a number to a float64, which is written back without an exponent,
// so an event id reads as the log wrote it.
func loggedWarnings(t *testing.T, logs *logtest.Buffer) []map[string]string {
	t.Helper()
	var warnings []map[string]string
	for _, record := range logtest.JSONLines(t, logs.String()) {
		if record["level"] != "WARN" {
			continue
		}
		attributes := make(map[string]string, len(record))
		for key, value := range record {
			if number, ok := value.(float64); ok {
				attributes[key] = strconv.FormatFloat(number, 'f', -1, 64)
			} else {
				attributes[key] = fmt.Sprint(value)
			}
		}
		warnings = append(warnings, attributes)
	}
	return warnings
}
