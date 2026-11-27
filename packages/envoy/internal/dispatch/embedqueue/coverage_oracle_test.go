package embedqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// mainCoverageQuery is the search coverage statement byte for byte as the Go of commit
// 948aeca5515454dbf0f194cd5de9aa382e9de5f0, the base of dispatch://INFRA-4241, built it as
// coverageQuery: the before text, kept as the oracle that the statement which replaced it is
// compared against, by the equality test below (TestCoverageAnswersWhatMainsStatementAnswers) and
// by the measurement of the two at production's size. It must never be edited to follow a schema
// change: a copy edited to fit answers for nothing that was ever served. If the schema stops
// answering it, the oracle is retired with the tests that read it.
const mainCoverageQuery = `
		with items as (select 'issue'::text as kind, item.id, item.hash, item.written_at from (select s.id, s.text, encode(sha256(convert_to(s.text, 'UTF8')), 'hex') as hash, s.written_at
	from (select i.key as id, issue_embedding_text(i.title, issue_embedding_spec(i.key)) as text, i.created_at as written_at from issues i) s where not (s.text !~ '[^[:space:]]')) item
 union all select 'document_block'::text as kind, item.id, item.hash, item.written_at from (select s.id, s.text, s.hash, s.written_at from (
		select e.id, e.text_snapshot as text, e.content_hash as hash, e.pending_since as written_at
		from embeddings e where e.kind = 'document_block') s where not (s.text !~ '[^[:space:]]')) item
 union all select 'comment_block'::text as kind, item.id, item.hash, item.written_at from (select s.id, s.text, s.hash, s.written_at from (
		select e.id, e.text_snapshot as text, e.content_hash as hash, e.pending_since as written_at
		from embeddings e where e.kind = 'comment_block') s where not (s.text !~ '[^[:space:]]')) item
 union all select 'message_block'::text as kind, item.id, item.hash, item.written_at from (select s.id, s.text, s.hash, s.written_at from (
		select e.id, e.text_snapshot as text, e.content_hash as hash, e.pending_since as written_at
		from embeddings e where e.kind = 'message_block') s where not (s.text !~ '[^[:space:]]')) item
 union all select 'ask_part'::text as kind, item.id, item.hash, item.written_at from (select s.id, s.text, s.hash, s.written_at from (
		select e.id, e.text_snapshot as text, e.content_hash as hash, e.pending_since as written_at
		from embeddings e where e.kind = 'ask_part') s where not (s.text !~ '[^[:space:]]')) item),
		judged as (
			select item.kind, item.written_at, e.pending_since, coalesce(e.dead, false) as dead,
			       coalesce($1::text <> '' and e.embedding is not null and e.model = $1::text and e.embedded_hash = item.hash, false) as current
			from items item
			left join embeddings e on e.kind = item.kind and e.id = item.id
		)
		select kind, true, count(*), count(*) filter (where current), count(*) filter (where dead and not current),
		       floor(extract(epoch from now() - min(greatest(pending_since, written_at)) filter (where not current and not dead)))::bigint
		from judged
		group by kind
		union all select 'component'::text, false, ((select count(*) from components))::bigint, 0::bigint, 0::bigint, null::bigint
 union all select 'session'::text, false, (coalesce((select jsonb_array_length(sessions) from route_roster), 0))::bigint, 0::bigint, 0::bigint, null::bigint
 union all select 'document_version'::text, false, ((select count(*) from artifact_versions))::bigint, 0::bigint, 0::bigint, null::bigint
 union all select 'result'::text, false, ((select count(*) from check_results))::bigint, 0::bigint, 0::bigint, null::bigint
 union all select 'handoff'::text, false, ((select count(*) from handoffs))::bigint, 0::bigint, 0::bigint, null::bigint`

// coverageRow is one row of a coverage statement, all six of its columns: the kind, whether the
// report covers it, its items, how many are current and how many failed, and its oldest lacking
// age in seconds (null when nothing of it lacks).
type coverageRow struct {
	kind                   string
	indexed                bool
	items, current, failed int64
	oldest                 *int64
}

func (r coverageRow) String() string {
	oldest := "null"
	if r.oldest != nil {
		oldest = strconv.FormatInt(*r.oldest, 10)
	}
	return fmt.Sprintf("%s indexed=%t items=%d current=%d failed=%d oldest=%s", r.kind, r.indexed, r.items, r.current, r.failed, oldest)
}

// coverageRowsOf reads every row statement answers for identity ($1), and for the rest of its
// arguments after it, in tx: each whole row rendered, in sorted order, so two answers compare as
// sets of rows, and the rows by kind.
func coverageRowsOf(t *testing.T, tx pgx.Tx, statement, identity string, more ...any) ([]string, map[string]coverageRow) {
	t.Helper()
	rows, err := tx.Query(context.Background(), statement, append([]any{identity}, more...)...)
	if err != nil {
		t.Fatalf("run the coverage statement for %q: %v", identity, err)
	}
	defer rows.Close()
	var rendered []string
	byKind := map[string]coverageRow{}
	for rows.Next() {
		var row coverageRow
		if err := rows.Scan(&row.kind, &row.indexed, &row.items, &row.current, &row.failed, &row.oldest); err != nil {
			t.Fatalf("scan a coverage row: %v", err)
		}
		rendered = append(rendered, row.String())
		byKind[row.kind] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the coverage statement for %q: %v", identity, err)
	}
	slices.Sort(rendered)
	return rendered, byKind
}

// requireMainsAnswer reads mainCoverageQuery and the coverage statement (coverageQuery), the
// latter under planCoverage's settings as Coverage runs it (headRowsOf), in one REPEATABLE READ,
// read-only transaction, so both read one snapshot and one now(), for the configured identity, for
// none ("") and for another identity, and fails the test wherever the two answers differ as sets
// of whole rows, printing both. It answers the rows each gave for the configured identity, by
// kind.
func requireMainsAnswer(t *testing.T, database *store.Store, stage string) (mains, heads map[string]coverageRow) {
	t.Helper()
	return requireMainsAnswerFor(t, database, stage, configuredIdentity)
}

// requireMainsAnswerFor is requireMainsAnswer with identity as the configured embedder's, as it is
// once the embedder has changed: it compares the two answers for identity, for configuredIdentity
// where identity is another, for none and for foreignIdentity, and answers the rows each gave for
// identity, by kind.
func requireMainsAnswerFor(t *testing.T, database *store.Store, stage, identity string) (mains, heads map[string]coverageRow) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("%s: begin the snapshot every statement reads: %v", stage, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	identities := []string{identity, "", foreignIdentity}
	if identity != configuredIdentity {
		identities = append(identities, configuredIdentity)
	}
	for _, compared := range identities {
		mainRows, mainKinds := coverageRowsOf(t, tx, mainCoverageQuery, compared)
		headRows, headKinds := headRowsOf(t, tx, compared)
		if !slices.Equal(mainRows, headRows) {
			t.Errorf("%s, identity %q: the coverage statement answers other rows than main's statement in one snapshot:\nmain's statement:\n  %s\nthe coverage statement:\n  %s",
				stage, compared, strings.Join(mainRows, "\n  "), strings.Join(headRows, "\n  "))
		}
		if compared == identity {
			mains, heads = mainKinds, headKinds
		}
	}
	return mains, heads
}

// headRowsOf reads the coverage statement for identity in tx as Coverage runs it, under the
// settings planCoverage sets, inside a savepoint that it rolls back, which undoes those settings
// and leaves tx's snapshot as it was: a statement after it plans as though they had never been
// set. It reads as a reader who sees every project (everyProject), so it counts every row main's
// statement, which knows no reader, counts.
func headRowsOf(t *testing.T, tx pgx.Tx, identity string) ([]string, map[string]coverageRow) {
	t.Helper()
	ctx := context.Background()
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("open a savepoint for the coverage statement: %v", err)
	}
	defer func() { _ = savepoint.Rollback(ctx) }()
	if err := planCoverage(ctx, savepoint); err != nil {
		t.Fatalf("plan the coverage statement as Coverage does: %v", err)
	}
	return coverageRowsOf(t, savepoint, coverageQuery, identity, everyProjectArg)
}

// updateRow sets set (whose arguments are $3 on) on the embeddings row of kind and id ($1 and
// $2), failing unless it wrote exactly that one row.
func updateRow(t *testing.T, q querier, kind, id, set string, args ...any) {
	t.Helper()
	tag, err := q.Exec(context.Background(), `update embeddings set `+set+` where kind = $1 and id = $2`, append([]any{kind, id}, args...)...)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("update %s %s set %s: %v (rows %d)", kind, id, set, err, tag.RowsAffected())
	}
}

// embedRowAs commits to the row of kind and id a vector of identity at the row's current text,
// as commitEmbeddings commits one.
func embedRowAs(t *testing.T, q querier, kind, id, identity string) {
	t.Helper()
	updateRow(t, q, kind, id, `embedding = $3::vector, model = $4, embedded_hash = content_hash, embedded_at = now()`,
		embed.Literal(oneVector()), identity)
}

// seedWindowStates writes, for each window kind, one row in each state a window row can be in
// that the report tells apart, as the writer of windows writes it (enqueueWindow) and the queue
// then leaves it: embedded at its text by the configured identity, by another, or at an older
// text; waiting for its first vector; dead with no vector or with an older one; and the three
// shapes of text the blank test reads - white space alone, embedded or waiting, and text that
// begins with white space or outside ASCII. Each kind's rows waited a time of their own, so each
// kind's oldest lacking age is its own, and a blank row waited longest of all, so counting one
// would move that age.
func seedWindowStates(t *testing.T, q querier) {
	t.Helper()
	for position, kind := range windowCoverageKinds {
		owner := kind + "/oracle"
		window := func(state, text string) string {
			t.Helper()
			enqueueWindow(t, q, kind, owner, state, text, "b1")
			return owner + "#" + state
		}
		waited := func(hours int) string { return strconv.Itoa(position+hours) + " hours" }
		embedRowAs(t, q, kind, window("current", "A window embedded at its current text"), configuredIdentity)
		updateRow(t, q, kind, window("pending", "A window waiting for its first vector"), `pending_since = now() - $3::interval`, waited(1))
		updateRow(t, q, kind, window("dead", "A window whose text the model refused"),
			`dead = true, confirmed_failures = 10, attempt_count = 12, dead_model = $3`, configuredIdentity)
		older := window("dead-older", "A window that died at a text after the one it was embedded at")
		embedRowAs(t, q, kind, older, configuredIdentity)
		updateRow(t, q, kind, older, `embedded_hash = 'an older text', dead = true, confirmed_failures = 10, dead_model = $3`, configuredIdentity)
		embedRowAs(t, q, kind, window("foreign", "A window another identity embedded"), foreignIdentity)
		stale := window("stale", "A window embedded at an older text")
		embedRowAs(t, q, kind, stale, configuredIdentity)
		updateRow(t, q, kind, stale, `embedded_hash = 'an older text', pending_since = now() - $3::interval`, waited(2))
		updateRow(t, q, kind, window("blank", " \n\t "), `pending_since = now() - $3::interval`, waited(30))
		embedRowAs(t, q, kind, window("blank-embedded", "\t"), configuredIdentity)
		embedRowAs(t, q, kind, window("leading-space", "\n  A window that begins with white space"), configuredIdentity)
		updateRow(t, q, kind, window("non-ascii", "Ünïcödé, a window that begins outside ASCII"), `pending_since = now() - $3::interval`, waited(5))
	}
}

// coverageStates names what seedEveryCoverageState wrote that changeInputsAfterRecording changes.
type coverageStates struct {
	// titleRestored and specRestored are issues whose inputs change with their triggers off: a
	// title, and the markdown of the latest version of specRestoredDocument, in place.
	titleRestored, specRestored, specRestoredDocument string
	// titleEdited and specVersioned are issues whose inputs change with their triggers on: a title
	// edit, and a new version of specVersionedDocument.
	titleEdited, specVersioned, specVersionedDocument string
	// rowGone is an issue whose embeddings row is deleted.
	rowGone string
}

// seedEveryCoverageState writes every state an item can be in, the windows' (seedWindowStates)
// and the issue's: embedded at its title and spec, with no primary document, with a primary
// document that has no version, with a spec that is blank, with a blank or white-space title and
// a spec, with a blank or white-space title and none (no item), with a title that begins with
// white space or outside ASCII, dead, waiting for its first vector, embedded by another identity,
// embedded at an older text, and with no embeddings row at all; and the issues whose inputs
// changeInputsAfterRecording changes, each embedded at its current text.
func seedEveryCoverageState(t *testing.T, database *store.Store) coverageStates {
	t.Helper()
	const project = "ORCL"
	q := database.Pool
	number := 0
	issue := func(title, spec string) (key, document string) {
		t.Helper()
		number++
		key = fmt.Sprintf("%s-%d", project, number)
		seedIssue(t, database, project, key, title)
		if spec != "" {
			document = seedSpec(t, q, project, key, spec)
		}
		return key, document
	}
	current := func(title, spec string) string {
		t.Helper()
		key, _ := issue(title, spec)
		embedRowAs(t, q, "issue", key, configuredIdentity)
		return key
	}

	current("An issue embedded at its title and spec", "## Summary\n\nThe spec it was embedded at.\n")
	current("An issue with no primary document", "")
	noVersion, _ := issue("An issue whose primary document has no version", "")
	mustExec(t, q, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, $2, 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')`, noVersion, project)
	embedRowAs(t, q, "issue", noVersion, configuredIdentity)
	current("An issue whose spec is blank", " \n\t\n")
	current("", "A spec under a blank title")
	current("\t", "A spec under a white-space title")
	issue("", "")
	issue(" \t ", "")
	current("  A title that begins with white space", "")
	current("Ünïcödé, a title that begins outside ASCII", "## Summary\n\nNaïve café.\n")
	dead, _ := issue("An issue whose text the model refused", "")
	updateRow(t, q, "issue", dead, `dead = true, confirmed_failures = 10, attempt_count = 12, dead_model = $3`, configuredIdentity)
	pending, _ := issue("An issue waiting for its first vector", "")
	updateRow(t, q, "issue", pending, `pending_since = now() - interval '3 hours'`)
	foreign, _ := issue("An issue another identity embedded", "")
	embedRowAs(t, q, "issue", foreign, foreignIdentity)
	older := current("An issue embedded at an older text", "")
	updateRow(t, q, "issue", older, `embedded_hash = 'an older text'`)
	// With no row the item ages from its write, which is the oldest of any issue's.
	noRow, _ := issue("An issue the queue holds no row of", "")
	mustExec(t, q, `delete from embeddings where kind = 'issue' and id = $1`, noRow)
	mustExec(t, q, `update issues set created_at = now() - interval '7 hours' where key = $1`, noRow)

	var states coverageStates
	states.titleRestored = current("A title the trigger saw", "## Summary\n\nA spec.\n")
	states.specRestored, states.specRestoredDocument = issue("A spec changed in place", "## Summary\n\nThe first version.\n")
	addVersion(t, q, states.specRestoredDocument, 2, "## Summary\n\nThe latest version, as it was embedded.\n")
	embedRowAs(t, q, "issue", states.specRestored, configuredIdentity)
	states.titleEdited = current("A title its trigger sees edited", "")
	states.specVersioned, states.specVersionedDocument = issue("A spec that takes a new version", "## Summary\n\nThe first version.\n")
	embedRowAs(t, q, "issue", states.specVersioned, configuredIdentity)
	states.rowGone = current("An issue whose row goes once its source is recorded", "")

	seedWindowStates(t, q)
	return states
}

// changeInputsAfterRecording changes, after the record step has recorded every issue's source,
// each input an issue's text is built from: a title and the latest version's markdown, in place,
// each with its table's triggers off (withTableTriggersOff), which leave the issue's row embedded
// at its old text; a title edit and a new spec version with the triggers on, which make the row
// wait for its new text; and an issue's row deleted. It also adds an issue embedded at its text
// that no record run has seen, and rewrites a window at a new text, as its writer would.
func changeInputsAfterRecording(t *testing.T, database *store.Store, states coverageStates) {
	t.Helper()
	withTableTriggersOff(t, database, "issues", func(q querier) {
		mustExec(t, q, `update issues set title = 'A title restored with its trigger off' where key = $1`, states.titleRestored)
	})
	withTableTriggersOff(t, database, "artifact_versions", func(q querier) {
		mustExec(t, q, `update artifact_versions set markdown = $2 where artifact_id = $1 and number = 2`,
			states.specRestoredDocument, "## Summary\n\nThe latest version, changed in place.\n")
	})
	mustExec(t, database.Pool, `update issues set title = 'A title edited with its trigger on' where key = $1`, states.titleEdited)
	addVersion(t, database.Pool, states.specVersionedDocument, 2, "## Summary\n\nA new version.\n")
	mustExec(t, database.Pool, `delete from embeddings where kind = 'issue' and id = $1`, states.rowGone)
	seedIssue(t, database, "ORCL", "ORCL-UNRECORDED", "An issue embedded after the record run")
	embedRowAs(t, database.Pool, "issue", "ORCL-UNRECORDED", configuredIdentity)
	enqueueWindow(t, database.Pool, "document_block", "document_block/oracle", "current", "A window rewritten at a new text", "b1")
}

// trustedIssues counts the issue items (issues whose text has something to embed) whose row
// holds a vector of identity at that row's content_hash: the issues a statement that took
// content_hash for the hash of the issue's current text would count current. stale is how many
// of them have a content_hash that is not that hash, the issues whose inputs changed with their
// triggers off. It is spelled from the pass's own text and hash expressions (issueText,
// contentHash, nothingToEmbed), not from the report's.
func trustedIssues(t *testing.T, database *store.Store, identity string) (trusted, stale int64) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), count(*) filter (where e.content_hash <> `+contentHash(issueText)+`)
		from issues i join embeddings e on e.kind = 'issue' and e.id = i.key
		where not `+nothingToEmbed(issueText)+`
		  and e.embedding is not null and e.model = $1 and e.embedded_hash = e.content_hash`, identity,
	).Scan(&trusted, &stale); err != nil {
		t.Fatalf("count the issues whose row holds a vector of %q at its content_hash: %v", identity, err)
	}
	return trusted, stale
}

// The coverage statement answers what main's statement (mainCoverageQuery) answered, row for row,
// under planCoverage's settings (requireMainsAnswer), over every state an item can be in
// (seedEveryCoverageState), for the configured identity, for none and for another, read in one
// snapshot: before any record run, when every issue builds and hashes its text; once the record
// step has recorded every issue's source, when the report reads most issues' hashes from their
// rows; and once every input an issue's text is built from has changed after its source was
// recorded (changeInputsAfterRecording). This is the report unchanged against the base on a
// seeded corpus.
//
// The last stage is not vacuous: the two issues whose inputs changed with their triggers off still
// hold a vector of the configured identity at their rows' content_hash, so a report that trusted a
// recorded source beyond the inputs it names would count them current. Both statements count
// them lacking.
func TestCoverageAnswersWhatMainsStatementAnswers(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	states := seedEveryCoverageState(t, database)
	requireIssuesCurrent := func(stage string, mains, heads map[string]coverageRow, want int64) {
		t.Helper()
		for name, rows := range map[string]map[string]coverageRow{"main's statement": mains, "the coverage statement": heads} {
			if got := rows["issue"].current; got != want {
				t.Errorf("%s: %s counts %d issues current, want %d: %v", stage, name, got, want, rows["issue"])
			}
		}
	}

	var sources int
	if err := database.Pool.QueryRow(ctx, `select count(*) from embedding_issue_sources`).Scan(&sources); err != nil || sources != 0 {
		t.Fatalf("embedding_issue_sources holds %d rows before any record run (%v), want none", sources, err)
	}
	requireMainsAnswer(t, database, "before any record run")

	if recordOnce(t, database) == 0 {
		t.Fatal("the record step recorded no issue's source, so the report would read no hash from a row")
	}
	mains, heads := requireMainsAnswer(t, database, "after the record step")
	trusted, stale := trustedIssues(t, database, configuredIdentity)
	if stale != 0 {
		t.Fatalf("after the record step %d issues hold a vector at a content_hash that is not their text's, want none yet", stale)
	}
	requireIssuesCurrent("after the record step", mains, heads, trusted)

	changeInputsAfterRecording(t, database, states)
	mains, heads = requireMainsAnswer(t, database, "after the inputs changed")
	trusted, stale = trustedIssues(t, database, configuredIdentity)
	if stale != 2 {
		t.Fatalf("after the inputs changed %d issues hold a vector at a content_hash that is not their text's, want the 2 changed with their triggers off", stale)
	}
	requireIssuesCurrent("after the inputs changed", mains, heads, trusted-stale)
}

// coverageCostProfiles are the costs the plan tests read the coverage statement's plan under, each
// set local after planCoverage's settings: none, as Coverage runs it, and three that push the
// planner's prices to extremes. The first of them made the planner read the window arm by a full
// scan of embeddings_pkey while the arm grouped by kind (coverageQuery). A plan that is the same
// under all four is the same because of which paths the statement leaves the planner, not because
// of what the planner prices them at.
var coverageCostProfiles = []struct{ name, settings string }{
	{"planCoverage's settings alone", ""},
	{"random reads nearly free, tuples and operators dear",
		"set local random_page_cost = 0.001; set local cpu_tuple_cost = 10; set local cpu_operator_cost = 10"},
	{"random reads dear, sequential reads nearly free, index tuples dear",
		"set local random_page_cost = 10000; set local seq_page_cost = 0.001; set local cpu_index_tuple_cost = 100"},
	{"every page nearly free, index tuples free, tuples and operators dear, the whole table cached",
		"set local random_page_cost = 0.001; set local seq_page_cost = 0.001; set local cpu_tuple_cost = 100; " +
			"set local cpu_index_tuple_cost = 0.00001; set local cpu_operator_cost = 100; set local effective_cache_size = '1TB'"},
}

// The coverage statement (coverageQuery), under planCoverage's settings, reads the window kinds by
// one Bitmap Heap Scan of embeddings over a BitmapOr of the two window indexes, whatever the
// planner estimates: its one window arm ORs the two indexes' predicates, each word for word, so the
// planner proves that the two indexes together hold every row the arm selects, and that bitmap is
// the one path to those rows the settings leave enabled (planCoverage). The blank test's regex is
// then in that scan's recheck alone, which an exact bitmap never runs, and in no filter, which
// runs on every row read.
//
// The plan is read on a seeded table of a few dozen rows (seedEveryCoverageState), as seeded and
// again once embeddings is analyzed, under each of coverageCostProfiles. On such a table an
// earlier version of this statement, which left the window path to the planner's estimates, read
// the arm through embeddings_window_owner, with the regex in the filter, since there three bitmap
// index scans cost more than a filter over so few rows: so a plan the planner picks by its
// estimates fails here. This fails the day the kinds, the regexes or either half's predicate stop
// matching the migrations' indexes, or another path to the window rows opens
// (TestNoOtherIndexCanReadTheWindowKinds names it): either leaves the answer right and the
// statement testing every window's text again.
func TestCoverageReadsTheWindowKindsThroughABitmapOfBothIndexes(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedEveryCoverageState(t, database)
	for _, statistics := range []struct{ name, statement string }{
		{"as seeded", ""},
		{"analyzed", "analyze embeddings"},
	} {
		if statistics.statement != "" {
			mustExec(t, database.Pool, statistics.statement)
		}
		for _, profile := range coverageCostProfiles {
			stage := statistics.name + ", " + profile.name
			plan := explainPlannedUnder(t, database, profile.settings)
			if explaintest.SeqScansRelation(t, plan, "embeddings") {
				t.Errorf("%s: the coverage statement's plan reads embeddings by a Seq Scan:\n%s", stage, plan)
			}
			var windows int
			for _, scan := range explaintest.BitmapHeapScans(t, plan, "embeddings") {
				if scan.OrOf("embeddings_window_embedded", "embeddings_window_unembedded") {
					windows++
				}
			}
			var textFilters []string
			for _, node := range explaintest.Nodes(t, plan) {
				if node.Relation == "embeddings" && (strings.Contains(node.Filter, "text_snapshot") || strings.Contains(node.Filter, "~")) {
					textFilters = append(textFilters, node.Type+" (Filter: "+node.Filter+")")
				}
			}
			if windows != 1 {
				t.Errorf("%s: the coverage statement's plan holds %d Bitmap Heap Scans of embeddings over a BitmapOr of embeddings_window_embedded and embeddings_window_unembedded, want one:\n%s",
					stage, windows, plan)
			}
			if len(textFilters) > 0 {
				t.Errorf("%s: the coverage statement tests windows' text in a filter of embeddings: %s\n%s", stage, strings.Join(textFilters, "; "), plan)
			}
		}
	}
}

// With both window indexes gone, the coverage statement under planCoverage's settings reads the
// window rows by a Seq Scan of embeddings, the path enable_seqscan = off disables, and by no index
// scan or bitmap scan of embeddings: the only index reads of embeddings left in the plan are the
// issue arm's lookups of its rows by kind = 'issue'. So no other index of embeddings can read the
// window rows at all, and the bitmap over the two window indexes is the one path the settings
// leave enabled (planCoverage) by construction, not by the planner's estimate. That is what keeps
// the statement's cost the data's in every state of the table. A new index that could read the
// window rows - a partial one whose predicate the arm's conditions prove, or one keyed on a column
// the arm tests by a condition a btree reads, such as embedding is null - would give the planner a
// choice again, to be made by estimates that are wrong for this table in some state it spends
// time in, and this test fails first.
//
// The indexes are dropped in a transaction that is rolled back, on the seeded table of
// TestCoverageReadsTheWindowKindsThroughABitmapOfBothIndexes, analyzed, and the plan is read under
// each of coverageCostProfiles.
func TestNoOtherIndexCanReadTheWindowKinds(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	seedEveryCoverageState(t, database)
	mustExec(t, database.Pool, `analyze embeddings`)
	for _, profile := range coverageCostProfiles {
		plan := func() json.RawMessage {
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("%s: begin the transaction that drops the window indexes: %v", profile.name, err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			for _, window := range []string{"embeddings_window_embedded", "embeddings_window_unembedded"} {
				mustExec(t, tx, `drop index `+window)
			}
			return explainPlannedIn(t, tx, profile.settings)
		}()
		const issueLookup = "kind = 'issue'::text"
		var windowsSeqScanned bool
		var otherReads []string
		for _, node := range explaintest.Nodes(t, plan) {
			if node.Relation != "embeddings" {
				continue
			}
			switch node.Type {
			case "Seq Scan":
				if strings.Contains(node.Filter, "text_snapshot") {
					windowsSeqScanned = true
				}
			case "Index Scan", "Index Only Scan":
				if !strings.Contains(node.IndexCond, issueLookup) {
					otherReads = append(otherReads, node.Type+" using "+node.Index+" (Index Cond: "+node.IndexCond+")")
				}
			case "Bitmap Heap Scan":
				// Read below, with the Bitmap Index Scans its bitmap is built from.
			default:
				otherReads = append(otherReads, node.Type)
			}
		}
		for _, scan := range explaintest.BitmapHeapScans(t, plan, "embeddings") {
			if !strings.Contains(scan.Scan.RecheckCond, issueLookup) {
				otherReads = append(otherReads, scan.Scan.Type+" (Recheck Cond: "+scan.Scan.RecheckCond+")")
			}
			for _, index := range scan.BitmapIndexScans {
				if !strings.Contains(index.IndexCond, issueLookup) {
					otherReads = append(otherReads, index.Type+" on "+index.Index+" (Index Cond: "+index.IndexCond+")")
				}
			}
		}
		if !windowsSeqScanned {
			t.Errorf("%s: with both window indexes dropped, the coverage statement reads the window rows by no Seq Scan of embeddings:\n%s", profile.name, plan)
		}
		if len(otherReads) > 0 {
			t.Errorf("%s: with both window indexes dropped, the coverage statement reads embeddings by %s, which is no lookup of the issue arm's rows by %s; that read is a path to the window rows the planner may choose by its estimates:\n%s",
				profile.name, strings.Join(otherReads, "; "), issueLookup, plan)
		}
	}
}

// explainPlanned explains the coverage statement for the configured identity in a read-only
// transaction under planCoverage's settings, as Coverage runs it, and answers the plan's one
// "Plan" node.
func explainPlanned(t *testing.T, database *store.Store) json.RawMessage {
	t.Helper()
	return explainPlannedUnder(t, database, "")
}

// explainPlannedUnder is explainPlanned with profile, set local statements (empty for none), run
// after planCoverage's settings in the same transaction.
func explainPlannedUnder(t *testing.T, database *store.Store, profile string) json.RawMessage {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin the coverage statement's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return explainPlannedIn(t, tx, profile)
}

// explainPlannedIn explains the coverage statement for the configured identity in tx once
// planCoverage has set its settings there, as Coverage runs it, and then profile has run (empty
// for none), and answers the plan's one "Plan" node. The settings stay set in tx.
func explainPlannedIn(t *testing.T, tx pgx.Tx, profile string) json.RawMessage {
	t.Helper()
	ctx := context.Background()
	if err := planCoverage(ctx, tx); err != nil {
		t.Fatalf("plan the coverage statement as Coverage does: %v", err)
	}
	if profile != "" {
		mustExec(t, tx, profile)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `explain (format json) `+coverageQuery, configuredIdentity, everyProjectArg).Scan(&raw); err != nil {
		t.Fatalf("explain the coverage statement: %v", err)
	}
	return explaintest.OnePlan(t, raw)
}
