package refs

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/logtest"
)

// resolverServer is the dashboard the resolver tests read their text with.
const resolverServer = "https://dispatch.example"

// The ids the resolver tests write their items under: one ask a prefix names alone, two an
// ambiguous prefix names, one comment, and one ask of another issue, which a reference naming
// this issue must not resolve to.
const (
	resolverAsk       = "1a2b3c4d-0000-4000-8000-000000000001"
	resolverAmbiguous = "5e6f7a8b-0000-4000-8000-000000000001"
	resolverTwin      = "5e6f7a8b-0000-4000-8000-000000000002"
	resolverComment   = "0c1d2e3f-0000-4000-8000-000000000001"
	resolverElsewhere = "7a7a7a7a-0000-4000-8000-000000000001"
)

// resolverSource is the id of the nth comment a test indexes text from.
func resolverSource(n int) string {
	return fmt.Sprintf("%08d-0000-4000-8000-000000000000", n)
}

// insertResolverIssue writes one issue of the ACME project under number, which
// createReferenceIssue numbers 1 for every issue and so writes only one of (issues is unique on
// (project_key, number)).
func insertResolverIssue(t *testing.T, tx pgx.Tx, key string, number int) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, 'ACME', $2, 'Resolver issue', '{"kind":"user","id":"alice"}', $3)
	`, key, number, "U"+fmt.Sprint(number)); err != nil {
		t.Fatalf("create issue %q: %v", key, err)
	}
}

// storedRefs is every edge stored for one source, in kind then id order.
func storedRefs(t *testing.T, q Queryer, fromKind, fromID string) []Target {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		select to_kind, to_id from refs where from_kind = $1 and from_id = $2 order by to_kind, to_id
	`, fromKind, fromID)
	if err != nil {
		t.Fatalf("read the references of %s %s: %v", fromKind, fromID, err)
	}
	defer rows.Close()
	stored := []Target{}
	for rows.Next() {
		var target Target
		if err := rows.Scan(&target.Kind, &target.ID); err != nil {
			t.Fatalf("scan a reference of %s %s: %v", fromKind, fromID, err)
		}
		stored = append(stored, target)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the references of %s %s: %v", fromKind, fromID, err)
	}
	return stored
}

// A reference whose id is written short is stored under the whole id the one item of the owner it
// names carries, so the target's panel lists it; a prefix two items carry, one no item carries
// and one carried only by an item of another owner are stored exactly as the text writes them, as
// a target that does not exist is. A bare key is an edge to its issue while that issue exists,
// and no row at all otherwise.
func TestMentionsStoreTheWholeIdOfAShortReferenceAndTheKeysThatExist(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ACME")
	insertResolverIssue(t, tx, "ACME-1", 1)
	insertResolverIssue(t, tx, "ACME-2", 2)
	issue := "ACME-1"
	insertPrefixAsk(t, tx, resolverAsk, &issue, nil, "Which?")
	insertPrefixAsk(t, tx, resolverAmbiguous, &issue, nil, "Which?")
	insertPrefixAsk(t, tx, resolverTwin, &issue, nil, "Which?")
	other := "ACME-2"
	insertPrefixAsk(t, tx, resolverElsewhere, &other, nil, "Which?")
	insertPrefixComment(t, tx, resolverComment, "ACME-1", "cited")

	for index, test := range []struct {
		name string
		body string
		want []Target
	}{
		{
			"a prefix one ask of the issue carries",
			"See dispatch://ACME-1/ask/" + resolverAsk[:8] + ".",
			[]Target{{"ask", resolverAsk}},
		},
		{
			"a prefix two asks of the issue carry",
			"See dispatch://ACME-1/ask/" + resolverAmbiguous[:8] + ".",
			[]Target{{"ask", resolverAmbiguous[:8]}},
		},
		{
			"a prefix no ask carries",
			"See dispatch://ACME-1/ask/deadbeef.",
			[]Target{{"ask", "deadbeef"}},
		},
		{
			"a prefix an ask of another issue carries",
			"See dispatch://ACME-1/ask/" + resolverElsewhere[:8] + ".",
			[]Target{{"ask", resolverElsewhere[:8]}},
		},
		{
			"a prefix one comment of the issue carries",
			"See dispatch://ACME-1/comment/" + resolverComment[:8] + ".",
			[]Target{{"comment", resolverComment}},
		},
		{
			"a bare key whose issue exists",
			"ACME-1 is the issue.",
			[]Target{{"issue", "ACME-1"}},
		},
		{
			"a bare key whose issue does not exist",
			"ACME-9 is the issue.",
			[]Target{},
		},
		{
			"the same prefix twice",
			"See dispatch://ACME-1/ask/" + resolverAsk[:8] + " and dispatch://ACME-1/ask/" + resolverAsk[:9] + ".",
			[]Target{{"ask", resolverAsk}},
		},
	} {
		source := resolverSource(index + 1)
		insertPrefixComment(t, tx, source, "ACME-1", test.body)
		located := NewReader(ctx, tx, prefixAuthor, "comment", source, text.NewOrigins(resolverServer)).Mentions(test.body)
		if _, err := replaceRefs(ctx, tx, "comment", source, text.Refs(located), nil); err != nil {
			t.Fatalf("index %s: %v", test.name, err)
		}
		if got := storedRefs(t, tx, "comment", source); !reflect.DeepEqual(got, test.want) {
			t.Errorf("%s stored %v; want %v", test.name, got, test.want)
		}
	}
}

// An ask or a comment written on a project document is owned by that document, and its
// reference names it (`dispatch://<PROJECT>/artifact/<slug>/ask/<id>`), so a short id in it is
// stored under the whole id the one item of that document carries. The same prefix carried only
// by an item of another document of the project, or by one of an issue, names nothing there and
// is stored as the text writes it.
func TestMentionsStoreTheWholeIdOfAProjectDocumentsShortReference(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ACME")
	insertResolverIssue(t, tx, "ACME-1", 1)
	const handbookID = "8b000000-0000-4000-8000-00000000000a"
	const notesID = "8b000000-0000-4000-8000-00000000000b"
	insertPrefixDocument(t, tx, handbookID, nil, "ACME", "handbook", "Handbook", false)
	insertPrefixDocument(t, tx, notesID, nil, "ACME", "notes", "Notes", false)
	const handbookAsk = "2c000000-0000-4000-8000-000000000001"
	const notesAsk = "2c000000-0000-4000-8000-000000000002"
	const handbookComment = "3d000000-0000-4000-8000-000000000001"
	const issueAsk = "3d000000-0000-4000-8000-000000000002"
	handbook, notes, issue := handbookID, notesID, "ACME-1"
	insertPrefixAsk(t, tx, handbookAsk, nil, &handbook, "Which vendor?")
	insertPrefixAsk(t, tx, notesAsk, nil, &notes, "Which region?")
	insertPrefixDocumentComment(t, tx, handbookComment, handbookID, "A remark on the handbook.")
	insertPrefixAsk(t, tx, issueAsk, &issue, nil, "Which way?")

	for index, test := range []struct {
		name string
		body string
		want []Target
	}{
		{
			"a prefix one ask of the document carries",
			"See dispatch://ACME/artifact/handbook/ask/" + handbookAsk[:8] + ".",
			[]Target{{"ask", handbookAsk}},
		},
		{
			"the same prefix on the other document of the project",
			"See dispatch://ACME/artifact/notes/ask/" + notesAsk[:8] + ".",
			[]Target{{"ask", notesAsk}},
		},
		{
			"a prefix one comment of the document carries",
			"See dispatch://ACME/artifact/handbook/comment/" + handbookComment[:8] + ".",
			[]Target{{"comment", handbookComment}},
		},
		{
			"a prefix only an ask of the issue carries",
			"See dispatch://ACME/artifact/handbook/ask/" + issueAsk[:8] + ".",
			[]Target{{"ask", issueAsk[:8]}},
		},
		{
			"a prefix only an ask of the other document carries",
			"See dispatch://ACME/artifact/handbook/comment/" + notesAsk[:8] + ".",
			[]Target{{"comment", notesAsk[:8]}},
		},
		{
			"a document's prefix under the issue",
			"See dispatch://ACME-1/ask/" + handbookAsk[:8] + ".",
			[]Target{{"ask", handbookAsk[:8]}},
		},
	} {
		source := resolverSource(index + 100)
		insertPrefixComment(t, tx, source, "ACME-1", test.body)
		located := NewReader(ctx, tx, prefixAuthor, "comment", source, text.NewOrigins(resolverServer)).Mentions(test.body)
		if _, err := replaceRefs(ctx, tx, "comment", source, text.Refs(located), nil); err != nil {
			t.Fatalf("index %s: %v", test.name, err)
		}
		if got := storedRefs(t, tx, "comment", source); !reflect.DeepEqual(got, test.want) {
			t.Errorf("%s stored %v; want %v", test.name, got, test.want)
		}
	}
}

// A `#id` is stored as the edge the ask's own address makes, resolved against the question blocks
// of the document the text belongs to: a comment anchored to a document reads that document's
// questions, and one on an issue and no document its issue's spec. A `#id` the text's own
// document does not declare names nothing and is stored as no edge.
func TestQuestionReferencesResolveAgainstTheTextsOwnDocument(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ACME")
	insertResolverIssue(t, tx, "ACME-1", 1)
	spec := insertScopeDocument(t, tx, "ACME-1", "spec", true)
	notes := insertScopeDocument(t, tx, "ACME-1", "notes", false)
	specQuestion := insertScopeBlockAsk(t, tx, "ACME-1", spec, "q1")
	notesQuestion := insertScopeBlockAsk(t, tx, "ACME-1", notes, "q2")
	insertScopeVersion(t, tx, spec, 1, ":::ask{#q1}\nWhich?\n:::\n")
	insertScopeVersion(t, tx, notes, 1, ":::ask{#q2}\nWhich?\n:::\n")

	anchored := insertScopeRow(t, tx, "anchored comment", `
		insert into comments (issue_key, author, body, anchor)
		values ('ACME-1', '{"kind":"user","id":"alice"}', 'anchored', jsonb_build_object('artifact_id', $1::text, 'mark_id', 'm1'))
		returning id::text
	`, notes)
	for _, test := range []struct {
		name   string
		source string
		body   string
		want   []Target
	}{
		{
			"a question of the issue's spec, from a comment on the issue",
			resolverSource(1),
			"See #q1.",
			[]Target{{"ask", specQuestion}},
		},
		{
			"the ask's own address, from a comment on the issue",
			resolverSource(2),
			"See dispatch://ACME-1/ask/" + specQuestion + ".",
			[]Target{{"ask", specQuestion}},
		},
		{
			"a question of another document, from a comment on the issue",
			resolverSource(3),
			"See #q2.",
			[]Target{},
		},
		{
			"a question of the document the comment is anchored to",
			anchored,
			"See #q2.",
			[]Target{{"ask", notesQuestion}},
		},
	} {
		if test.source != anchored {
			insertPrefixComment(t, tx, test.source, "ACME-1", test.body)
		}
		located := NewReader(ctx, tx, prefixAuthor, "comment", test.source, text.NewOrigins(resolverServer)).Mentions(test.body)
		if _, err := replaceRefs(ctx, tx, "comment", test.source, text.Refs(located), nil); err != nil {
			t.Fatalf("index %s: %v", test.name, err)
		}
		if got := storedRefs(t, tx, "comment", test.source); !reflect.DeepEqual(got, test.want) {
			t.Errorf("%s stored %v; want %v", test.name, got, test.want)
		}
	}
}

// Every mention keeps the offset it was written at, the bare ones among them, so a block mention
// written after a bare key is still paired to the block holding it.
func TestABareKeyLeavesALaterBlockMentionInItsOwnBlock(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ACME")
	insertResolverIssue(t, tx, "ACME-1", 1)
	source := resolverSource(1)
	body := "ACME-1 is the issue.\n\nSee dispatch://ACME-1/artifact/notes#b-two."
	insertPrefixComment(t, tx, source, "ACME-1", body)
	at := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	indexItemAsAWriteDoes(t, tx, ItemComment, source, body, resolverServer, at)

	rows, err := tx.Query(ctx, `
		select block_id, to_kind, to_id from comment_block_refs where comment_id = $1::uuid
		order by block_id, to_kind
	`, source)
	if err != nil {
		t.Fatalf("read the comment's block mentions: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var block, kind, id string
		if err := rows.Scan(&block, &kind, &id); err != nil {
			t.Fatalf("scan a block mention: %v", err)
		}
		got = append(got, block+" -> "+kind+":"+id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the comment's block mentions: %v", err)
	}
	// A `#b-` fragment names the block id after it (text.EncodeBlockID), so `#b-two` is block
	// two of the document.
	want := []string{"b1 -> issue:ACME-1", "b2 -> block:ACME-1/notes#two"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the comment's block mentions are %v; want %v", got, want)
	}
}

// rebuildFixture writes, in one committed transaction, the project, issue, spec, question block
// and ask a rebuild's text cites, and the comment whose text an older grammar stored no edge of.
func rebuildFixture(t *testing.T, database *store.Store, source, body string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ACME")
	insertResolverIssue(t, tx, "ACME-1", 1)
	spec := insertScopeDocument(t, tx, "ACME-1", "spec", true)
	question := insertScopeBlockAsk(t, tx, "ACME-1", spec, "q1")
	insertScopeVersion(t, tx, spec, 1, ":::ask{#q1}\nWhich?\n:::\n")
	issue := "ACME-1"
	insertPrefixAsk(t, tx, resolverAsk, &issue, nil, "Which?")
	insertPrefixComment(t, tx, source, "ACME-1", body)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the fixture: %v", err)
	}
	return question
}

// rebuild-refs reads every source with this build's whole grammar, so text stored before it
// gains the edges of the bare keys, `#id`s and short ids it writes, with no edit.
func TestRebuildAllReadsTheWholeGrammarOfStoredText(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	source := resolverSource(1)
	body := "ACME-1, see #q1 and dispatch://ACME-1/ask/" + resolverAsk[:8] + "."
	question := rebuildFixture(t, database, source, body)

	if _, err := RebuildAll(ctx, database.Pool, text.NewOrigins(resolverServer)); err != nil {
		t.Fatalf("rebuild the references: %v", err)
	}
	want := []Target{{"ask", question}, {"ask", resolverAsk}, {"issue", "ACME-1"}}
	slices.SortFunc(want, func(a, b Target) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID))
	})
	if got := storedRefs(t, database.Pool, "comment", source); !reflect.DeepEqual(got, want) {
		t.Errorf("the rebuilt comment stored %v; want %v", got, want)
	}
}

// errResolverRead is the failure every read of a failingQueryer meets.
var errResolverRead = errors.New("the database is unreachable")

// failingQueryer is a database no read reaches, the failure the resolver answers as nothing
// resolved.
type failingQueryer struct{}

func (failingQueryer) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errResolverRead
}

func (failingQueryer) QueryRow(context.Context, string, ...any) pgx.Row { return failingRow{} }

type failingRow struct{}

func (failingRow) Scan(...any) error { return errResolverRead }

// countingHandler counts the records logged at or above error, with the source each names.
type countingHandler struct {
	records []string
}

func (h *countingHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelError
}

func (h *countingHandler) Handle(_ context.Context, record slog.Record) error {
	var source string
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "id" {
			source = attr.Value.String()
		}
		return true
	})
	h.records = append(h.records, record.Message+" ["+source+"]")
	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

// A resolver that cannot reach the database answers nothing resolved for every kind, logs each
// failure naming the source, and leaves the write storing what the text writes: the short id as
// written, and no edge for a bare key or a `#id`, which only the database could have resolved.
func TestAFailedResolverLeavesTheWriteStoringTheTextAsWritten(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault), which
	// Reader.failed writes to.
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ACME")
	insertResolverIssue(t, tx, "ACME-1", 1)
	spec := insertScopeDocument(t, tx, "ACME-1", "spec", true)
	insertScopeBlockAsk(t, tx, "ACME-1", spec, "q1")
	insertScopeVersion(t, tx, spec, 1, ":::ask{#q1}\nWhich?\n:::\n")
	issue := "ACME-1"
	insertPrefixAsk(t, tx, resolverAsk, &issue, nil, "Which?")
	source := resolverSource(1)
	body := "ACME-1, see #q1 and dispatch://ACME-1/ask/" + resolverAsk[:8] + "."
	insertPrefixComment(t, tx, source, "ACME-1", body)

	logged := &countingHandler{}
	logtest.SetDefault(t, logged)

	located := NewReader(ctx, failingQueryer{}, prefixAuthor, "comment", source, text.NewOrigins(resolverServer)).Mentions(body)
	if want := text.ExtractAt(body, text.NewOrigins(resolverServer)); !reflect.DeepEqual(located, want) {
		t.Errorf("a failed resolver read %#v; want the text as written, %#v", located, want)
	}
	if _, err := replaceRefs(ctx, tx, "comment", source, text.Refs(located), nil, true); err != nil {
		t.Fatalf("the write failed on what its body cites: %v", err)
	}
	want := []Target{{"ask", resolverAsk[:8]}}
	if got := storedRefs(t, tx, "comment", source); !reflect.DeepEqual(got, want) {
		t.Errorf("a failed resolver stored %v; want %v", got, want)
	}
	if len(logged.records) != 3 {
		t.Errorf("a failed resolver logged %v; want one record for each of the three kinds", logged.records)
	}
	for _, record := range logged.records {
		if !strings.Contains(record, "["+source+"]") {
			t.Errorf("a logged failure does not name its source: %q", record)
		}
	}
}

// failingScanQueryer is a database whose every read answers one row no scan reads: the failure a
// candidate reader meets after its query has run rather than at it.
type failingScanQueryer struct{ failingQueryer }

func (failingScanQueryer) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &failingRows{}, nil
}

// failingRows is one row whose scan fails with errResolverRead. The methods no candidate reader
// calls are those of the nil pgx.Rows it embeds.
type failingRows struct {
	pgx.Rows
	read bool
}

func (r *failingRows) Next() bool {
	next := !r.read
	r.read = true
	return next
}

func (*failingRows) Scan(...any) error { return errResolverRead }
func (*failingRows) Close()            {}
func (*failingRows) Err() error        { return nil }

// A short id whose read fails is logged (Reader.failed), and the error it logs, like the one
// ResolvePrefix answers the API with, is built of constant words: it quotes nothing a reference
// wrote, neither the prefix nor its length nor the owner, so two references of one kind that
// differ in all three fail with the same text. It still wraps the database's failure, met at the
// query or at the scan of each kind's reader. A comment or a message whose delete mark does not
// decode fails with a text that names neither its kind nor its id, whose head is the prefix a
// reference wrote.
func TestAFailedPrefixReadQuotesNothingAReferenceWrote(t *testing.T) {
	// Not parallel: it swaps the process's default logger (logtest.Swap, in loggedErrors), which
	// Reader.failed writes to.
	ctx := context.Background()
	databases := []struct {
		name string
		q    Queryer
	}{
		{"failing its query", failingQueryer{}},
		{"failing its scan", failingScanQueryer{}},
	}
	// Each body writes one short id: a reader whose read fails resolves no later short id of
	// the body and asks for none, so each kind's reader is reached by a body of its own. The two
	// bodies of a kind differ in the prefix, its length and its owner.
	type reference struct {
		body    string
		written []string
	}
	bodies := []struct {
		kind string
		pair [2]reference
	}{
		{"ask", [2]reference{
			{"See dispatch://ACME-1/ask/1a2b3c4d.", []string{"1a2b3c4d", "ACME-1"}},
			{"See dispatch://BETA-22/ask/9f8e7d6c-5b.", []string{"9f8e7d6c-5b", "BETA-22"}},
		}},
		{"comment", [2]reference{
			{"See dispatch://ACME/artifact/handbook/comment/0c1d2e3f-0000.", []string{"0c1d2e3f-0000", "ACME/handbook"}},
			{"See dispatch://BETA/artifact/guide/comment/6a5b4c3d.", []string{"6a5b4c3d", "BETA/guide"}},
		}},
		{"message", [2]reference{
			{"See dispatch://ACME-1/message/5c6d7e8f-9a.", []string{"5c6d7e8f-9a", "ACME-1"}},
			{"See dispatch://BETA-22/message/4e3d2c1b-0a1b.", []string{"4e3d2c1b-0a1b", "BETA-22"}},
		}},
		{"block", [2]reference{
			{"See dispatch://ACME-1/artifact/notes#b-cafef00d.", []string{"cafef00d", "ACME-1/notes"}},
			{"See dispatch://BETA-22/artifact/plan#b-beadfeed42.", []string{"beadfeed42", "BETA-22/plan"}},
		}},
	}
	for _, database := range databases {
		for _, body := range bodies {
			t.Run(body.kind+" with the database "+database.name, func(t *testing.T) {
				var texts [2][]string
				for index, reference := range body.pair {
					for _, err := range loggedErrors(t, database.q, reference.body) {
						requireQuotesNone(t, err, reference.written)
						texts[index] = append(texts[index], err.Error())
					}
					if len(texts[index]) == 0 {
						t.Errorf("the failed read of %q logged no error", reference.body)
					}
				}
				if !slices.Equal(texts[0], texts[1]) {
					t.Errorf("two references of one kind logged %q and %q; want one text", texts[0], texts[1])
				}
			})
		}
	}

	type asked struct {
		scope  Scope
		prefix string
	}
	resolves := []struct {
		kind string
		pair [2]asked
	}{
		{"ask", [2]asked{{Scope{IssueKey: "ACME-1"}, "1a2b3c4d"}, {Scope{IssueKey: "BETA-22"}, "9f8e7d6c-5b"}}},
		{"comment", [2]asked{{Scope{Document: "ACME/handbook"}, "0c1d2e3f-0000"}, {Scope{Document: "BETA/guide"}, "6a5b4c3d"}}},
		{"message", [2]asked{{Scope{IssueKey: "ACME-1"}, "5c6d7e8f-9a"}, {Scope{IssueKey: "BETA-22"}, "4e3d2c1b-0a1b"}}},
		{"artifact", [2]asked{{Scope{IssueKey: "ACME-1"}, "8a9b0c1d"}, {Scope{IssueKey: "BETA-22"}, "2f3e4d5c-6b"}}},
		{"block", [2]asked{{Scope{Document: "ACME-1/notes", Version: 2}, "cafef00d"}, {Scope{Document: "BETA-22/plan", Version: 3}, "beadfeed42"}}},
	}
	for _, database := range databases {
		for _, resolve := range resolves {
			var texts [2]string
			for index, asked := range resolve.pair {
				_, err := ResolvePrefix(ctx, database.q, prefixAuthor, resolve.kind, asked.scope, asked.prefix, Viewer{})
				if err == nil {
					t.Errorf("the %s %q resolved with the database %s", resolve.kind, asked.prefix, database.name)
					continue
				}
				requireQuotesNone(t, err, []string{asked.prefix, cmp.Or(asked.scope.IssueKey, asked.scope.Document)})
				texts[index] = err.Error()
			}
			if texts[0] != texts[1] {
				t.Errorf("two %s prefixes failed with %q and %q with the database %s; want one text", resolve.kind, texts[0], texts[1], database.name)
			}
		}
	}

	// The id of a row a prefix's read matches starts with that prefix, so a candidate's error
	// that quoted its id would quote what the reference wrote.
	const id = "0c1d2e3f-0000-4000-8000-000000000001"
	deletedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	for _, kind := range []string{"comment", "message"} {
		_, err := itemCandidate(kind, id, nil, "", "", "", resolverServer, postMarks{deletedAt: &deletedAt, deletedBy: []byte("no actor")})
		if syntax := new(*json.SyntaxError); !errors.As(err, syntax) {
			t.Errorf("a %s whose delete mark does not decode failed with %v; want the decode's failure wrapped", kind, err)
			continue
		}
		if strings.Contains(err.Error(), id[:8]) {
			t.Errorf("%q quotes the %s's id, whose head a reference wrote", err, kind)
		}
	}
}

// loggedErrors is every error a resolver over q logs as it reads body, failing t on a logged
// error attribute that holds no error.
func loggedErrors(t *testing.T, q Queryer, body string) []error {
	t.Helper()
	recorder := &logtest.Recorder{}
	restore := logtest.Swap(t, recorder)
	defer restore()
	NewReader(context.Background(), q, prefixAuthor, "comment", resolverSource(1), resolverServer).Mentions(body)
	var logged []error
	for _, record := range recorder.Records() {
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key != "error" {
				return true
			}
			if err, ok := attr.Value.Any().(error); ok {
				logged = append(logged, err)
			} else {
				t.Errorf("a logged failure's error is %v, which is no error", attr.Value)
			}
			return true
		})
	}
	return logged
}

// requireQuotesNone fails t where err does not wrap errResolverRead or its text quotes any of
// written, the text a reference wrote.
func requireQuotesNone(t *testing.T, err error, written []string) {
	t.Helper()
	if !errors.Is(err, errResolverRead) {
		t.Errorf("%q does not wrap the database's failure", err)
	}
	for _, quoted := range written {
		if strings.Contains(err.Error(), quoted) {
			t.Errorf("%q quotes %q, which a reference wrote", err, quoted)
		}
	}
}

// A body citing a prefix two asks of the issue carry, a prefix a third ask carries alone and a
// prefix no ask carries reads the lone ask's whole id and the other two as written, and its
// reader logs nothing: a prefix that names two items is no failure, any more than one that names
// none. ResolvePrefix still refuses the shared prefix with ErrPrefixAmbiguous naming both asks,
// the answer the API's 409 lists.
func TestAnAmbiguousShortIDReadsAsWrittenAndLogsNothing(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault), and its claim
	// is that the read logs nothing there.
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ACME")
	insertResolverIssue(t, tx, "ACME-1", 1)
	issue := "ACME-1"
	for _, id := range []string{resolverAsk, resolverAmbiguous, resolverTwin} {
		insertPrefixAsk(t, tx, id, &issue, nil, "Which?")
	}
	recorder := &logtest.Recorder{}
	logtest.SetDefault(t, recorder)

	shared, lone := resolverAmbiguous[:8], resolverAsk[:8]
	body := "See dispatch://ACME-1/ask/" + shared + ", dispatch://ACME-1/ask/" + lone + " and dispatch://ACME-1/ask/deadbeef."
	var read []string
	for _, mention := range NewReader(ctx, tx, prefixAuthor, "comment", resolverSource(1), resolverServer).Mentions(body) {
		read = append(read, mention.Kind+"/"+mention.ID)
	}
	if want := []string{"ask/" + shared, "ask/" + resolverAsk, "ask/deadbeef"}; !slices.Equal(read, want) {
		t.Errorf("the body read %q; want %q", read, want)
	}
	for _, record := range recorder.Records() {
		t.Errorf("the body logged %q; want nothing logged", record.Message)
	}

	_, err = ResolvePrefix(ctx, tx, prefixAuthor, "ask", Scope{IssueKey: issue}, shared, Viewer{})
	var ambiguous *ErrPrefixAmbiguous
	if !errors.As(err, &ambiguous) || ambiguous.Prefix != shared || len(ambiguous.Candidates) != 2 ||
		ambiguous.Candidates[0].ID != resolverAmbiguous || ambiguous.Candidates[1].ID != resolverTwin {
		t.Errorf("the shared prefix %q resolved with %v; want ErrPrefixAmbiguous naming %s and %s", shared, err, resolverAmbiguous, resolverTwin)
	}
}

// countingQueryer counts every read one resolver makes.
type countingQueryer struct {
	Queryer
	reads *int
}

func (q countingQueryer) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	*q.reads++
	return q.Queryer.Query(ctx, sql, args...)
}

func (q countingQueryer) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	*q.reads++
	return q.Queryer.QueryRow(ctx, sql, args...)
}

// One resolver resolves each distinct (kind, owner, prefix) once however many times the texts it
// reads write it, so an ask whose question and options cite the same short id asks once.
func TestAResolverResolvesEachPrefixOnce(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ACME")
	insertResolverIssue(t, tx, "ACME-1", 1)
	issue := "ACME-1"
	insertPrefixAsk(t, tx, resolverAsk, &issue, nil, "Which?")

	cite := "dispatch://ACME-1/ask/" + resolverAsk[:8]
	reads := 0
	resolve := NewReader(ctx, countingQueryer{Queryer: tx, reads: &reads}, prefixAuthor, "comment", resolverSource(1), text.NewOrigins(resolverServer))
	if located := resolve.Mentions("See " + cite + "."); len(located) != 1 || located[0].ID != resolverAsk {
		t.Fatalf("the first text read %#v; want the whole id of %s", located, resolverAsk)
	}
	once := reads
	if located := resolve.Mentions("See " + cite + " and " + cite + " again."); len(located) != 2 {
		t.Fatalf("the second text read %#v; want two references", located)
	}
	if reads != once {
		t.Errorf("a prefix the resolver had resolved cost %d more reads; want none", reads-once)
	}
}
