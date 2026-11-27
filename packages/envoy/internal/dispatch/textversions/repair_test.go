package textversions

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
	"github.com/sjawhar/envoy/internal/explaintest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// cutCommentsIndex is the partial index that holds the comment versions marked cut (migration
// 20261122100004), which the repair and its census read the table through.
const cutCommentsIndex = "search_text_versions_cut_comments"

// TestTheRepairAndItsCensusReadTheCutCommentVersionsAlone fills the keyword table with 3,000
// comment versions, none marked cut, and analyzes it, so the planner prices plans as it would over
// a filled table. The repair's read of a window (repairWindowQuery), under the generic plan the
// fill's cached statement settles on, and the census's count (ProvenCutFirstBodiesQuery) each read
// the table through search_text_versions_cut_comments and by no other scan, and the window, run,
// reads at most 8 buffers. A whole walk over that table, one version a batch, sends that read
// once: a hold over a table with no cut comment version costs one read of the empty index, where a
// walk of every comment version would read the table 3,001 times.
func TestTheRepairAndItsCensusReadTheCutCommentVersionsAlone(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into search_text_versions (kind, id, issue_key, project_key, event_id, event_seq, started_at, body, body_from, search)
		select 'comment', md5(n::text)::uuid::text, 'ACME-1', 'ACME', n, 1,
		       timestamptz '2026-11-01 00:00:00+00' + n * interval '1 minute',
		       'A note on the rotation, item ' || n, case when n % 2 = 0 then 'event' else 'row' end,
		       to_tsvector('english', 'A note on the rotation, item ' || n)
		  from generate_series(1, 3000) n;
		analyze search_text_versions;
	`); err != nil {
		t.Fatalf("fill the keyword table: %v", err)
	}
	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()

	// The window's parameters are prepared with the types Postgres infers for them with none
	// given, as the fill's pgx connection prepares the statement: text, integer, and bigint for a
	// limit; its literals are the walk's first key and its production bound.
	window := explaintest.AnalyzedGenericPlan(t, ctx, connection, "repair_window", repairWindowQuery,
		[]string{"text", "integer", "bigint"}, []string{"''", "-2147483648", "1000"})
	requireReadThroughTheCutCommentsIndex(t, "the repair's window", window)
	root := explaintest.Nodes(t, window)[0]
	buffers := root.SharedHitBlocks + root.SharedReadBlocks
	t.Logf("the repair's window read %.0f buffers of a table of 3,000 comment versions, none cut", buffers)
	if root.Rows != 0 || buffers > 8 {
		t.Fatalf("the repair's window answered %v rows and read %.0f buffers; want none and at most 8, a read of the empty index", root.Rows, buffers)
	}
	var raw []byte
	if err := database.Pool.QueryRow(ctx, "explain (format json) "+ProvenCutFirstBodiesQuery).Scan(&raw); err != nil {
		t.Fatalf("explain the census's count: %v", err)
	}
	requireReadThroughTheCutCommentsIndex(t, "the census's count", explaintest.OnePlan(t, raw))

	reads := &windowReads{}
	traced := storetest.TracedStore(t, database, reads)
	var logs logtest.Buffer
	fill := testFiller(traced, events.NewHorizon(traced.Pool), &logs)
	fill.repairWindow = 1
	if rebuilt, err := fill.repairCutFirstBodies(ctx); err != nil || rebuilt != 0 {
		t.Fatalf("the walk rebuilt %d versions (%v), want none", rebuilt, err)
	}
	if n := reads.windows.Load(); n != 1 {
		t.Fatalf("the walk read the table's cut comment versions %d times, want once", n)
	}
}

// requireReadThroughTheCutCommentsIndex fails unless plan reads search_text_versions through
// cutCommentsIndex and by no other scan: no sequential scan of the table and no scan of its other
// indexes, the primary key, the item index and the vector's.
func requireReadThroughTheCutCommentsIndex(t *testing.T, what string, plan []byte) {
	t.Helper()
	var indexes []string
	for _, scan := range explaintest.IndexScans(t, plan) {
		indexes = append(indexes, scan.Index)
	}
	t.Logf("%s reads its indexes %q", what, indexes)
	if explaintest.SeqScansRelation(t, plan, "search_text_versions") {
		t.Fatalf("%s reads search_text_versions by a sequential scan; want %s alone", what, cutCommentsIndex)
	}
	others := []string{"search_text_versions_pkey", "search_text_versions_item_start", "search_text_versions_search"}
	if !slices.Contains(indexes, cutCommentsIndex) || slices.ContainsFunc(indexes, func(index string) bool { return slices.Contains(others, index) }) {
		t.Fatalf("%s reads the indexes %q; want %s and no other index of search_text_versions", what, indexes, cutCommentsIndex)
	}
}

// windowReads counts the repair's reads of a window (repairWindowQuery) the store it traces sends
// (storetest.TracedStore).
type windowReads struct {
	windows atomic.Int64
}

func (w *windowReads) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL == repairWindowQuery {
		w.windows.Add(1)
	}
	return ctx
}

func (w *windowReads) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// previousReleaseFirstRevisionQuery and previousReleaseInsertVersionsQuery are, byte for byte, the
// reader of revision 1 and the insert of the keyword fill of the release before this one
// (legion-envoy-v32.37.2, commit fdecb4a8626a, textversions/bodies.go's firstRevisionQuery and
// fill.go's insertVersionsQuery): the statements whose answer and whose write a fill of that
// release makes of a comment this release wrote. The two-build test drives that fill itself
// (cmd/dispatch's TestTheFirstBodyRepairRebuildsACutVersionThePreviousBuildsFillWrote); these stand
// in for it here, where no build but this one runs.
const (
	previousReleaseFirstRevisionQuery = `
	select t.text, b.created_at = $2
	  from public.comment_block_texts t
	  join public.comment_blocks b on b.comment_id = t.comment_id and b.block_id = t.block_id
	 where t.comment_id = $1::uuid and t.revision = 1
	 order by length(t.block_id), t.block_id`
	previousReleaseInsertVersionsQuery = `
	insert into public.search_text_versions
	       (kind, id, issue_key, project_key, event_id, event_seq, started_at, body, body_from,
	        spec_artifact_id, spec_version, search)
	select r.kind, r.id, r.issue_key, r.project_key, r.event_id, r.event_seq, r.started_at, r.body,
	       r.body_from, r.spec_artifact_id::uuid, r.spec_version,
	       case r.kind
	         when 'issue' then issue_search_vector(r.id, r.body,
	           (select a.markdown from public.artifact_versions a
	             where a.artifact_id = r.spec_artifact_id::uuid and a.number = r.spec_version))
	         else search_vector('', search_text(r.body))
	       end
	  from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::bigint[], $6::integer[],
	              $7::timestamptz[], $8::text[], $9::text[], $10::text[], $11::integer[])
	       as r (kind, id, issue_key, project_key, event_id, event_seq, started_at, body, body_from,
	             spec_artifact_id, spec_version)`
)

// TestTheFirstBodyRepairRebuildsACutVersionThePreviousBuildWrote writes a comment longer than an
// event carries as this release's create route writes one, its block index in its own transaction,
// and an edit of it, so its row no longer holds its first body. The previous release's reader of
// revision 1 takes none of revision 1's blocks as the comment's own, since this release dates a
// block's row by the comment's moment and not by its transaction's start, so that release's fill
// keeps the event's cut text, which its insert writes. The census of the cut first bodies counts
// that version, and the first-body repair, walking one comment version a batch, rebuilds it from
// revision 1: its body is revision 1's blocks in their order, from blocks, and its vector is
// the vector of the body the comment was written with (postVectorSQL). The census then counts
// none, and a second walk rebuilds nothing and leaves the row as it is.
func TestTheFirstBodyRepairRebuildsACutVersionThePreviousBuildWrote(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	first := longCommentBody()
	comment := writeComment(t, database, broker, first, true)
	editComment(t, database, broker, comment.id, "A new opening paragraph, written once the rotation was planned.\n\n"+first)
	created := readLoggedEvents(t, database, comment.event)[0]

	rows, err := database.Pool.Query(ctx, previousReleaseFirstRevisionQuery, comment.id, created.CreatedAt)
	if err != nil {
		t.Fatalf("read revision 1 as the previous release reads it: %v", err)
	}
	read, own := 0, 0
	for rows.Next() {
		var text *string
		var mine bool
		if err := rows.Scan(&text, &mine); err != nil {
			t.Fatalf("read revision 1 as the previous release reads it: %v", err)
		}
		read++
		if mine {
			own++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read revision 1 as the previous release reads it: %v", err)
	}
	if read == 0 || own != 0 {
		t.Fatalf("the previous release's reader reads %d blocks of revision 1, %d of them the comment's own; want blocks and none its own, which is why its fill keeps the cut text", read, own)
	}
	cut, truncated := model.EventBody(first)
	if !truncated {
		t.Fatal("the body is one an event carries whole, which tests no cut")
	}
	writePreviousReleaseCutVersion(t, database, created, comment.id, cut)
	requireProvenCutFirstBodies(t, database, 1)

	var logs logtest.Buffer
	fill := testFiller(database, events.NewHorizon(database.Pool), &logs)
	fill.repairWindow = 1
	if rebuilt, err := fill.repairCutFirstBodies(ctx); err != nil || rebuilt != 1 {
		t.Fatalf("the first-body repair rebuilt %d versions (%v), want the one the previous release cut", rebuilt, err)
	}
	blocks := itemblocks.Split(first)
	texts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		texts = append(texts, block.Text)
	}
	want := strings.Join(texts, "\n\n")
	repaired := readRepairedVersion(t, database, comment.id, comment.event, first)
	if repaired.body != want || repaired.bodyFrom != bodyFromBlocks || !repaired.vectorOfFirst {
		t.Fatalf("the repaired version holds %d bytes from %q (vector of the first body %v), want revision 1's %d bytes from %q and the first body's vector",
			len(repaired.body), repaired.bodyFrom, repaired.vectorOfFirst, len(want), bodyFromBlocks)
	}
	if !strings.Contains(logs.String(), rebuiltMessage) {
		t.Errorf("the repair logged %q, want its line of the version it rebuilt", logs.String())
	}
	requireProvenCutFirstBodies(t, database, 0)

	if rebuilt, err := fill.repairCutFirstBodies(ctx); err != nil || rebuilt != 0 {
		t.Fatalf("a second walk rebuilt %d versions (%v), want none", rebuilt, err)
	}
	if again := readRepairedVersion(t, database, comment.id, comment.event, first); again != repaired {
		t.Fatalf("a second walk left the version as %+v, want it as the first left it, %+v", again, repaired)
	}
}

// TestTheFirstBodyRepairLeavesACutVersionNoSourceHolds writes a comment longer than an event
// carries as a build before the block index wrote one, with no block index, which the
// reconciliation then indexes in a transaction of its own, and an edit of it. This release's fill
// keeps the comment.created's cut text: revision 1 was written by another transaction than the
// comment's. The census of the cut first bodies counts none, and the first-body repair, which
// decides by the same reader, rebuilds nothing: the version stays cut, with the event's text.
func TestTheFirstBodyRepairLeavesACutVersionNoSourceHolds(t *testing.T) {
	ctx := t.Context()
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	long := longCommentBody()
	comment := writeComment(t, database, broker, long, false)
	indexComment(t, database, comment.id, long)
	edit := editComment(t, database, broker, comment.id, "The plan moved to the issue's spec.\n\n"+long)
	var logs logtest.Buffer
	got := decideAndWrite(t, database, &logs, readLoggedEvents(t, database, comment.event, edit))
	requireKeptEventText(t, database, got, &logs, comment.id, comment.event, long)
	requireProvenCutFirstBodies(t, database, 0)

	fill := testFiller(database, events.NewHorizon(database.Pool), &logs)
	if rebuilt, err := fill.repairCutFirstBodies(ctx); err != nil || rebuilt != 0 {
		t.Fatalf("the first-body repair rebuilt %d versions (%v), want none: no source holds the body", rebuilt, err)
	}
	cut, _ := model.EventBody(long)
	if kept := readRepairedVersion(t, database, comment.id, comment.event, long); kept.body != cut || kept.bodyFrom != bodyFromCut {
		t.Fatalf("after the repair the version holds %d bytes from %q, want its event's cut %d bytes from %q",
			len(kept.body), kept.bodyFrom, len(cut), bodyFromCut)
	}
	requireProvenCutFirstBodies(t, database, 0)
}

// writePreviousReleaseCutVersion writes the version event starts of comment id as the previous
// release's fill writes one whose whole body no source it reads holds: through that release's
// insert (previousReleaseInsertVersionsQuery), with the event's cut text, from cut.
func writePreviousReleaseCutVersion(t *testing.T, database *store.Store, e event, id, cut string) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(), previousReleaseInsertVersionsQuery,
		[]string{kindComment}, []string{id}, []*string{e.IssueKey}, []string{e.Project}, []int64{e.ID},
		[]int32{e.Seq}, []time.Time{e.CreatedAt}, []string{cut}, []string{bodyFromCut}, []*string{nil}, []*int32{nil}); err != nil {
		t.Fatalf("write the comment.created version as the previous release's fill writes it: %v", err)
	}
}

// requireProvenCutFirstBodies fails unless the census of the cut first bodies the repair rebuilds
// (CountProvenCutFirstBodies, which graphcensus's store census reads) counts want.
func requireProvenCutFirstBodies(t *testing.T, database *store.Store, want int64) {
	t.Helper()
	count, err := CountProvenCutFirstBodies(t.Context(), database.Pool)
	if err != nil || count != want {
		t.Fatalf("the census of the cut first bodies revision 1 proves counts %d (%v), want %d", count, err, want)
	}
}

// repairedVersion is a comment version's row as the repair leaves it: its body, where the body
// came from, and whether its vector is the vector of the body the comment was written with.
type repairedVersion struct {
	body, bodyFrom string
	vectorOfFirst  bool
}

// readRepairedVersion reads the version event started of comment id, its vector held against
// the vector of first (postVectorSQL).
func readRepairedVersion(t *testing.T, database *store.Store, id string, event int64, first string) repairedVersion {
	t.Helper()
	var v repairedVersion
	if err := database.Pool.QueryRow(t.Context(), `
		select body, body_from, search = search_vector('', search_text($3))
		  from search_text_versions where kind = 'comment' and id = $1 and event_id = $2
	`, id, event, first).Scan(&v.body, &v.bodyFrom, &v.vectorOfFirst); err != nil {
		t.Fatalf("read the version of event %d: %v", event, err)
	}
	return v
}
