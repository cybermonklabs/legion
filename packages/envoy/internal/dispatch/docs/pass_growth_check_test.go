//go:build memory

package docs

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The two rooms TestThePassLockTimeGrowsLinearlyWithTheRoom measures, in blocks: 4,682, which was
// the size of api's deep spec before a list item became a block of its own beside its paragraph
// (R7 of the issue-tree spec; that spec now holds 6,842, api's deepXidBlocks), and 20,000. The
// rooms keep those sizes, which the bound below and the measurement graphstore's
// CustomPlansStatement records are taken over. Each room holds its blocks in two documents of the
// issue room, half in its spec's fragment and half in its plan's, each in the deep spec's shape
// (passGrowthRoomText): 20,000 blocks of that shape in one document make more elements than a
// document may hold (pmdoc.MaxDocumentElements), and the room of 4,682 is split as the room of
// 20,000 is, so the ratio of their times measures the room's size and not one document against
// two. The plan's document replaces the paragraph createIssueRoom seeds the plan's fragment with
// (planSeed), and the room's verification holds no block.
const (
	passGrowthSmall = 4_682
	passGrowthLarge = 20_000
)

// The deep spec's shape (api's deepXidSpec): chains of a paragraph and a list nested
// passGrowthLevels levels below it, a list, its one item and the item's paragraph at each level, so
// passGrowthChainBlocks blocks a chain, under the document's heading.
const (
	passGrowthLevels      = 6
	passGrowthChainBlocks = 1 + 3*passGrowthLevels
)

// passGrowthRuns is how many runs a measurement makes (passGrowthRun), each over a room of each
// size, each room in a database of its own; each size's time is the median of theirs.
const passGrowthRuns = 3

// passGrowthBound is the most the median time the reconciliation's pass over a version that
// changes one block holds the owner lock in a room of 20,000 blocks may be, as a multiple of that
// in a room of 4,682. The rooms' sizes stand in the ratio 20,000 / 4,682 = 4.27, which a pass
// whose cost grows in proportion to the room reads, less what its fixed cost takes off; one whose
// cost grows with the room times its logarithm reads 5.0, and one whose cost grows with the
// square of the room 18.2. The pass reads between the first two: each probe of a block descends
// indexes that are deeper over the larger room, 66 buffers a block over 4,682 blocks and 71 over
// 20,000.
//
// The bound is set from three runs of this test on the build farm on 2026-11-26, one Job each, on
// c4d-highmem-32-lssd machines (runs 1 and 2) and a c4d-highmem-64-lssd (run 3): the test in a
// container of 4 CPUs and 12 GiB, and its Postgres 18.6 in a sidecar of 2 CPUs and 4 GiB on its
// default settings (shared_buffers 128 MB, fsync on). Each run read two ratios, the first with
// the tables analyzed after the rooms are indexed and the second with graph_tuples analyzed near
// empty, each the median over 20,000 blocks over the median over 4,682:
//
//	run 1: 4.22 (1.775 s over 420 ms) and 4.93 (1.956 s over 397 ms)
//	run 2: 4.24 (1.767 s over 417 ms) and 4.36 (1.734 s over 397 ms)
//	run 3: 4.50 (1.624 s over 361 ms) and 4.83 (1.838 s over 380 ms)
//
// The bound is the largest of the six, 4.93, with half of it again as headroom: 7.4, under half
// the square's 18.2. Each of the two reads R15 names, put back alone on the same farm, reads more
// than twice the bound: planRoomBlocks' join 16 with the tables analyzed after the rooms are
// indexed, and blockAuthors' read with MovedRoomBlockSQL over 20 with graph_tuples analyzed near
// empty.
//
// Those runs timed a run's pass over 4,682 blocks and its pass over 20,000 some 20 s apart, the
// writing and first pass of the larger room between them, and a slow spell of the machine that
// spanned the larger room's run slowed its pass alone: a Job run beside four others on 2026-11-28
// read 1.718 s, 2.651 s and 2.480 s over 20,000 blocks against 364 ms, 364 ms and 354 ms over
// 4,682, a ratio of 6.82 on the same code that reads 4.6 to 4.7 in CI. A run now writes and
// indexes both rooms first and times the two passes one right after the other (passGrowthRun), so
// a spell that slows one slows the other, and the ratio of the medians reads the room's growth
// and not the spell.
const passGrowthBound = 7.4

// passGrowthStop is how many times the bound a run lets the pass over 20,000 blocks hold the owner
// lock over the pass over 4,682 blocks it has just timed, at the least, before it stops that pass
// (passGrowthReconcile, passGrowthStopAt): a pass stopped there has read a ratio of 8.14, past the
// bound, whatever it would have gone on to hold, where the six runs of a Job of this test on the
// build farm on 2026-11-28 read 3.7 to 5.5, each over its own two passes. Each of the two reads
// R15 names holds the lock far longer, one of them past go test's 10 minutes. Measured there the
// same day with this test as it stood before the stop, planRoomBlocks' join held it a median 55.6
// s over 20,000 blocks against 3.39 s over 4,682 with the tables analyzed after the rooms are
// indexed (16.39), and blockAuthors' read with MovedRoomBlockSQL 6 min 59 s against 21.5 s with
// graph_tuples analyzed near empty (19.52), the test 24 minutes in all: under the check's run,
// which sets go test no timeout, the test panicked at 10 minutes and named no ratio. A run lets
// the pass hold the lock longer where its share of the time left before go test's deadline allows
// (passGrowthStopAt), so a pass that fits in that share runs to its end and the test names the
// ratio it read whole.
const passGrowthStop = 1.1

// passGrowthMargin is the time a run leaves before go test's deadline, past the passes it lets run
// (passGrowthStopAt), for the services' shutdown, the databases' drop and go test's report.
const passGrowthMargin = time.Minute

// passGrowthTicks is how many reconciliation ticks a run makes at most for the pass it waits for.
const passGrowthTicks = 10

// The paragraph of a room's spec that the version a run times the pass over changes
// (passGrowthReword), as passGrowthDocument writes it, and the words that version gives it.
const (
	passGrowthParagraph = "Chain 1 of the spec holds the outbox.\n"
	passGrowthReworded  = "Chain 1 of the spec holds the outbox it was given.\n"
)

// passGrowthRoom is the room a run wrote, with the blocks it holds once indexed.
type passGrowthRoom struct {
	issueRoom
	blocks int
}

// passGrowthText is the text of the two documents a room holds its blocks in (passGrowthRoomText),
// and the blocks each holds.
type passGrowthText struct {
	spec, plan             string
	specBlocks, planBlocks int
}

// passGrowthRoomText is the text of a room of blocks blocks: its spec and its plan, half the room's
// blocks each (passGrowthDocument).
func passGrowthRoomText(t *testing.T, blocks int) passGrowthText {
	t.Helper()
	text := passGrowthText{specBlocks: blocks / 2, planBlocks: blocks - blocks/2}
	text.spec = passGrowthDocument(t, "spec", text.specBlocks)
	text.plan = passGrowthDocument(t, "plan", text.planBlocks)
	return text
}

// passGrowthDocument is a document of blocks blocks in the deep spec's shape, each block of which
// names the document name, so the two documents of a room hold no block of the same words: its
// heading, as many whole chains as fit, and one paragraph a block for the rest, each opening as a
// chain's paragraph does. It fails the test where the document makes more elements than one may
// hold (pmdoc.MaxDocumentElements), which its parse refuses, or does not hold blocks blocks as the
// server stamps them.
func passGrowthDocument(t *testing.T, name string, blocks int) string {
	t.Helper()
	chains, rest := (blocks-1)/passGrowthChainBlocks, (blocks-1)%passGrowthChainBlocks
	var document strings.Builder
	fmt.Fprintf(&document, "# Deep %s of a room\n\n", name)
	for chain := 1; chain <= chains; chain++ {
		fmt.Fprintf(&document, "Chain %d of the %s holds the outbox.\n\n", chain, name)
		for level := range passGrowthLevels {
			fmt.Fprintf(&document, "%s- Level %d of chain %d of the %s reads the bus.\n", strings.Repeat("  ", level), level+1, chain, name)
		}
		document.WriteString("\n")
	}
	for paragraph := 1; paragraph <= rest; paragraph++ {
		fmt.Fprintf(&document, "Chain %d of the %s holds the outbox alone.\n\n", chains+paragraph, name)
	}
	tree, err := pmdoc.Parse(document.String())
	if err != nil {
		t.Fatalf("the deep %s of %d blocks does not parse: %v", name, blocks, err)
	}
	pmdoc.EnsureBlockIDs(tree)
	if _, offsets, err := pmdoc.RenderWithBlockOffsets(tree); err != nil || len(offsets) != blocks {
		t.Fatalf("the deep %s holds %d blocks (%v), want %d", name, len(offsets), err, blocks)
	}
	return document.String()
}

// passGrowthTables is every table of database, each by its qualified name.
func passGrowthTables(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select format('%I.%I', n.nspname, c.relname)
		from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where c.relkind = 'r' and n.nspname = 'public'
		order by 1
	`)
	if err != nil {
		t.Fatalf("list the tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list the tables: %v", err)
	}
	return tables
}

// passGrowthStatistics takes the planner's statistics of every table of database as it stands,
// and keeps them: autovacuum analyzes no table of it afterwards, so every pass the run makes after
// it is planned from the statistics taken here, and not from ones a background analyze took
// partway through.
func passGrowthStatistics(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, table := range passGrowthTables(t, database) {
		if _, err := database.Pool.Exec(ctx, `alter table `+table+` set (autovacuum_enabled = false)`); err != nil {
			t.Fatalf("keep autovacuum off %s: %v", table, err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `analyze`); err != nil {
		t.Fatalf("analyze the tables: %v", err)
	}
}

// passGrowthSettlePages leaves the pages of database as a server holds them once a room's first
// pass is long past: each row marked on its page as committed, which Postgres does the first time
// a later statement reads the row (a hint bit), and no page of the buffer cache waiting to be
// written out. It reads every table whole, which marks every row, and then checkpoints, which
// writes every such page out.
//
// A room's first pass leaves both undone, and a room of 20,000 blocks is more pages than the
// buffer cache of a server on Postgres' default settings holds (128 MB: the build farm's sidecar
// and CI's service alike), where a room of 4,682 fits in it. The pass over the next version of the
// larger room then marks the rows it reads and writes other pages out to make room for them, in
// the time it holds the owner lock, and the pass over the smaller room does neither. Measured on
// 2026-11-26 on the build farm without this step: a pass over 20,000 blocks wrote out 5,451 pages
// in 0.49 s of the 2.86 s it held the lock, and one whose statistics were taken before its room
// was written marked the rows of 4,297 pages, where the passes over 4,682 blocks wrote out 3 to 5
// pages. That is the first pass's cost, paid by whichever statement comes next at the speed of the
// machine's disk, and no cost of the pass a run times.
func passGrowthSettlePages(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, table := range passGrowthTables(t, database) {
		if _, err := database.Pool.Exec(ctx, `select count(*) from `+table); err != nil {
			t.Fatalf("read %s whole: %v", table, err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `checkpoint`); err != nil {
		t.Fatalf("write out the pages the buffer cache holds: %v", err)
	}
}

// passGrowthWords is how many texts the block index has recorded of the blocks of room, one for
// each version a block's words changed in (document_block_texts), the record that a block went,
// which holds no text, left out.
func passGrowthWords(t *testing.T, database *store.Store, room passGrowthRoom) int {
	t.Helper()
	var texts int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from document_block_texts where artifact_id = $1 and text is not null`, room.tree).Scan(&texts); err != nil {
		t.Fatalf("count the texts recorded of the blocks of %s: %v", room.issue, err)
	}
	return texts
}

// passGrowthMarkHead marks the head rows of the issue key whole, as the write that makes an
// issue's first node does (graphstore.QueueHeadMark): createIssueRoom writes the issue by SQL, with
// no node, so the issue is marked here, before any write of its tree. An issue no mark names is
// one the block index's tick fills from the store (graphcensus.FillIssueHeads) after its passes,
// reading every node of the issue in a transaction of its own; no issue the server creates is left
// unmarked, and this test times the pass, not that fill.
func passGrowthMarkHead(t *testing.T, database *store.Store, key string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the head mark of %s: %v", key, err)
	}
	defer tx.Rollback(ctx)
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueHeadMark(b, key)
		return nil
	}); err != nil {
		t.Fatalf("mark the head rows of %s whole: %v", key, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the head mark of %s: %v", key, err)
	}
}

// passGrowthWrite writes the room of the converted issue DOC-1 (createIssueRoom), marked whole in
// the head projection (passGrowthMarkHead), its spec and its plan the two documents of text,
// through the real write: the room's first settlement while its spec holds one paragraph, which
// versions each fragment whose text its document's version does not hold - the plan's seed among
// them - and indexes the room in its own transaction, so the room is no longer ahead of its
// versions; then each document of text written into its own fragment and versioned through the
// named-version route's write (passGrowthNamedVersion), the spec's before the plan's is written,
// so each version is written over a room whose every other fragment reads as its document's
// latest version, and leaves the room's blocks to the reconciliation, whose one pass over the room
// indexes both documents. It answers the room and the blocks it holds once indexed.
func passGrowthWrite(t *testing.T, database *store.Store, service *Service, text passGrowthText) passGrowthRoom {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	room := createIssueRoom(t, database, 1)
	passGrowthMarkHead(t, database, room.issue)
	if _, err := joinedReplaceText(service, room.spec, "The room's first words.\n", alice); err != nil {
		t.Fatalf("write the first words of %s's spec: %v", room.issue, err)
	}
	if err := service.waitForDurableAppends(ctx, room.tree); err != nil {
		t.Fatalf("wait for the appends of %s's room: %v", room.issue, err)
	}
	if err := service.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle %s's room: %v", room.issue, err)
	}
	written := 0
	for _, document := range []struct {
		id, name, text string
		blocks         int
	}{
		{room.spec, "spec.md", text.spec, text.specBlocks},
		{room.plan, "plan.md", text.plan, text.planBlocks},
	} {
		if _, err := joinedReplaceText(service, document.id, document.text, alice); err != nil {
			t.Fatalf("write the deep document of %d blocks into %s's %s: %v", document.blocks, room.issue, document.name, err)
		}
		passGrowthNamedVersion(t, service, room, document.id, document.name)
		written += document.blocks
		if indexed := countRows(t, database, "document_blocks", room.tree); indexed >= written {
			t.Fatalf("the named version of %s's %s indexed its room's %d blocks in its own request, want them left to the reconciliation", room.issue, document.name, indexed)
		}
	}
	// The named versions hold the edits' text; the room's settlement, which the test's hour-long
	// settle delay holds off, has nothing of them left to version.
	settle(t, database, room.tree)
	return passGrowthRoom{issueRoom: room, blocks: written}
}

// passGrowthReword writes a version of room's spec that changes one block, through the real write:
// the spec of text with the paragraph of its first chain reworded (passGrowthReworded) written
// into its fragment, which keeps every other block as it stands, and versioned through the
// named-version route's write (passGrowthNamedVersion), which leaves the room's blocks to the
// reconciliation. It answers how many texts the room's blocks have recorded before that version
// (passGrowthWords): the pass over it records one more, the reworded paragraph's (passGrowthRun).
func passGrowthReword(t *testing.T, database *store.Store, service *Service, room passGrowthRoom, text passGrowthText) int {
	t.Helper()
	reworded := strings.Replace(text.spec, passGrowthParagraph, passGrowthReworded, 1)
	if reworded == text.spec {
		t.Fatalf("the deep spec of %s holds no paragraph %q to reword", room.issue, passGrowthParagraph)
	}
	texts := passGrowthWords(t, database, room)
	if _, err := joinedReplaceText(service, room.spec, reworded, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write the reworded paragraph into %s's spec.md: %v", room.issue, err)
	}
	passGrowthNamedVersion(t, service, room.issueRoom, room.spec, "spec.md")
	if recorded := passGrowthWords(t, database, room); recorded != texts {
		t.Fatalf("the named version of %s's reworded spec.md recorded %d texts of its room's blocks in its own request, want them left to the reconciliation", room.issue, recorded-texts)
	}
	// As passGrowthWrite leaves the room: the named version holds the edit's text.
	settle(t, database, room.tree)
	return texts
}

// passGrowthNamedVersion writes the next version of document, the document of room named name, as
// the named-version route writes it (api's createNamedVersion): NamedVersion in a transaction
// joined to the document service, marked as the route marks it (WithDeferredBlockIndex), so a
// version of a room above deferBlockIndexBlocks leaves the room's blocks to the block-index
// reconciliation; the version's event; what the write stated appended under that event; and the
// commit.
func passGrowthNamedVersion(t *testing.T, service *Service, room issueRoom, document, name string) {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the named version of %s's %s: %v", room.issue, name, err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	result, err := service.NamedVersion(WithDeferredBlockIndex(joined), document, "The deep spec's shape", model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatalf("write the named version of %s's %s: %v", room.issue, name, err)
	}
	if !result.Wrote {
		t.Fatalf("the named version of %s's %s wrote nothing, want its deep document versioned", room.issue, name)
	}
	event, err := recordIssueEvent(ctx, tx, room.issue, "artifact.version", aliceActor,
		versionEventPayload(t, document, name, result.Version.Number))
	if err != nil {
		t.Fatalf("record the event of the named version of %s's %s: %v", room.issue, name, err)
	}
	if err := causeInWritePhase(joined, tx, result.Blocks, event); err != nil {
		t.Fatalf("append what the named version of %s's %s stated: %v", room.issue, name, err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the named version of %s's %s: %v", room.issue, name, err)
	}
}

// passGrowthHolders is how many transactions of database, other than the reader's, hold an
// advisory lock, which the owner lock is, and how long the oldest of them has run, read off the
// database's clock from xact_start, which Postgres takes at the transaction's begin. Where idle,
// it counts only those that sit idle in a transaction.
func passGrowthHolders(ctx context.Context, database *store.Store, idle bool) (int, time.Duration, error) {
	var holders int
	var seconds float64
	if err := database.Pool.QueryRow(ctx, `
		select count(*), coalesce(max(extract(epoch from clock_timestamp() - a.xact_start)), 0)::float8
		from pg_stat_activity a
		where a.datname = current_database() and a.pid <> pg_backend_pid()
		  and (not $1::boolean or a.state = 'idle in transaction')
		  and exists (select 1 from pg_locks l
		              where l.pid = a.pid and l.locktype = 'advisory' and l.granted
		                and l.database = (select oid from pg_database where datname = current_database()))
	`, idle).Scan(&holders, &seconds); err != nil {
		return 0, 0, err
	}
	return holders, time.Duration(seconds * float64(time.Second)), nil
}

// passGrowthLockHeld is how long the transaction of the reconciliation's pass has held the owner
// lock, read from the pass's own hook (afterBlockIndexWrite), while that transaction waits between
// its last write and its commit: the time since it began (passGrowthHolders). Its first statement
// after its begin plans its statements at the tables' sizes (graphstore.PlanAtCurrentSizes) and
// its second takes the owner lock (lockArtifactOwner), so the time since the begin reads as the
// time the lock is held. The pass's backend is the one of this test's database that sits idle in
// a transaction holding an advisory lock.
func passGrowthLockHeld(ctx context.Context, database *store.Store) (time.Duration, error) {
	holders, held, err := passGrowthHolders(ctx, database, true)
	if err != nil {
		return 0, err
	}
	if holders != 1 {
		return 0, fmt.Errorf("%d transactions of the test's database sit idle holding an advisory lock, want the pass's one", holders)
	}
	return held, nil
}

// passGrowthWatch reads, every 100 ms until end is called, how long the oldest transaction of
// database that holds an advisory lock has run (passGrowthHolders): while a pass runs, how long it
// has held the owner lock. It calls cancel once that time is longer than stop, and end answers the
// time it read then and whether it did. Where stop is zero it reads nothing.
func passGrowthWatch(database *store.Store, stop time.Duration, cancel context.CancelFunc) (end func() (time.Duration, bool)) {
	if stop <= 0 {
		return func() (time.Duration, bool) { return 0, false }
	}
	done, watched := make(chan struct{}), make(chan struct{})
	var stopped time.Duration
	go func() {
		defer close(watched)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			if _, held, err := passGrowthHolders(context.Background(), database, false); err == nil && held > stop {
				stopped = held
				cancel()
				return
			}
		}
	}()
	return sync.OnceValues(func() (time.Duration, bool) {
		close(done)
		<-watched
		return stopped, stopped > 0
	})
}

// passGrowthReconcile runs the reconciliation's real tick until a pass has indexed room, and
// answers the time that pass, the first over the room, held the owner lock (passGrowthLockHeld).
// Where stop is above zero, a pass that holds the lock longer than stop is stopped
// (passGrowthWatch): its tick is cancelled, so its transaction rolls back, and the answer is how
// long the pass had held the lock then, less than it would have held, and true. It fails where no
// pass indexes the room, or a pass it did not stop leaves the room holding other than its blocks.
func passGrowthReconcile(t *testing.T, database *store.Store, service *Service, room passGrowthRoom, stop time.Duration) (time.Duration, bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	artifacts := []string{room.tree, room.spec, room.verification, room.plan}
	var held time.Duration
	var failure error
	timed := false
	service.afterBlockIndexWrite = func(artifactID string) error {
		if timed || !slices.Contains(artifacts, artifactID) {
			return nil
		}
		timed = true
		held, failure = passGrowthLockHeld(context.Background(), database)
		return nil
	}
	defer func() { service.afterBlockIndexWrite = nil }()
	end := passGrowthWatch(database, stop, cancel)
	defer end()
	var reconciled error
	for range passGrowthTicks {
		var tick BlockIndexTick
		if tick, reconciled = service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, nil); reconciled != nil {
			break
		}
		if tick.LiveAhead != 0 || tick.Unreadable != 0 {
			t.Fatalf("the tick found %d rooms live ahead and %d unreadable, want the room indexed", tick.LiveAhead, tick.Unreadable)
		}
		if timed {
			break
		}
	}
	if stopped, ok := end(); ok {
		return stopped, true
	}
	if reconciled != nil {
		t.Fatalf("reconcile the block index: %v", reconciled)
	}
	if !timed {
		t.Fatalf("no pass of the reconciliation indexed the room of %s, of %d blocks, in %d ticks", room.issue, room.blocks, passGrowthTicks)
	}
	if failure != nil {
		t.Fatalf("time the pass over the room of %s: %v", room.issue, failure)
	}
	if indexed := countRows(t, database, "document_blocks", room.tree); indexed != room.blocks {
		t.Fatalf("the room of %s holds %d blocks after its pass, want %d", room.issue, indexed, room.blocks)
	}
	return held, false
}

// passGrowthTimes is what one run of a measurement read (passGrowthRun): how long the pass over
// the version that changes one block held the owner lock over the room of passGrowthSmall blocks
// and over the room of passGrowthLarge, and whether the run stopped the second (passGrowthStop),
// whose time is then how long it had held the lock when it was stopped.
type passGrowthTimes struct {
	small, large time.Duration
	stopped      bool
}

// largeHeld is the time over the larger room as the test's messages state it.
func (times passGrowthTimes) largeHeld() string {
	if times.stopped {
		return fmt.Sprintf("more than %v (stopped)", times.large)
	}
	return times.large.String()
}

// passGrowthSide is one room of a run, on a database and a service of its own, with the time its
// first pass held the owner lock and the texts its blocks recorded before the version the run
// times the pass over (passGrowthReword).
type passGrowthSide struct {
	database *store.Store
	service  *Service
	room     passGrowthRoom
	text     passGrowthText
	first    time.Duration
	texts    int
}

// passGrowthRun is one run of a measurement over a room of each of texts' sizes, the smaller
// first, each on a database of its own: each room written through the real write
// (passGrowthWrite) and indexed by the real tick (passGrowthReconcile), whose first pass over it
// writes every block; then a version of each that changes one block (passGrowthReword), and the
// real tick again over each, the smaller room's first, whose passes over those versions the run
// times one right after the other. It answers how long each held the owner lock; the pass over the
// larger room it stops once that pass has held the lock as long as passGrowthStopAt allows, at
// least passGrowthStop times the bound over the smaller room's (passGrowthTimes), where deadline
// is go test's (zero for none) and runsLeft how many runs the test may still make, this one among
// them. The tables' statistics of each database are taken once
// (passGrowthStatistics): before its room is written where nearEmpty, so both passes are planned on
// the statistics of a store that holds no block, and else once its room is indexed, so the timed
// pass is planned on the statistics of the room it reads. Before the versions are written, the
// pages the first passes wrote are left as a server holds them long after them
// (passGrowthSettlePages), the larger room's first, so the smaller room's pages, whose pass is
// timed first, are the ones the buffer cache read last; so the timed passes pay nothing of the
// first ones.
//
// Each database holds one room, so its tables grow with the room: a read of the pass that walks a
// table once for each block, whatever it holds of other rooms, costs the square of the room here,
// as one that walks the room's own rows once for each block does. Both rooms are written and
// indexed before either timed pass, so the two passes run seconds apart and a slow spell of the
// machine slows both. The run ends before the next begins, its services shut down and its
// databases dropped.
func passGrowthRun(t *testing.T, name string, texts [2]passGrowthText, nearEmpty bool, deadline time.Time, runsLeft int) passGrowthTimes {
	t.Helper()
	var times passGrowthTimes
	if !t.Run(name, func(t *testing.T) {
		started := time.Now()
		var sides [2]passGrowthSide
		for i, text := range texts {
			side := passGrowthSide{database: storetest.Open(t), text: text}
			side.service, _ = newTestServiceInstance(t, side.database)
			if nearEmpty {
				passGrowthStatistics(t, side.database)
			}
			side.room = passGrowthWrite(t, side.database, side.service, text)
			side.first, _ = passGrowthReconcile(t, side.database, side.service, side.room, 0)
			if !nearEmpty {
				passGrowthStatistics(t, side.database)
			}
			sides[i] = side
		}
		small, large := &sides[0], &sides[1]
		passGrowthSettlePages(t, large.database)
		passGrowthSettlePages(t, small.database)
		for _, side := range []*passGrowthSide{small, large} {
			side.texts = passGrowthReword(t, side.database, side.service, side.room, side.text)
		}
		times.small, _ = passGrowthReconcile(t, small.database, small.service, small.room, 0)
		times.large, times.stopped = passGrowthReconcile(t, large.database, large.service, large.room,
			passGrowthStopAt(times.small, deadline, runsLeft, time.Since(started)))
		timed := []*passGrowthSide{small, large}
		if times.stopped {
			timed = timed[:1]
		}
		for _, side := range timed {
			if recorded := passGrowthWords(t, side.database, side.room) - side.texts; recorded != 1 {
				t.Fatalf("the pass over the version that rewords one paragraph of %s's spec.md recorded the texts of %d blocks, want the one paragraph's", side.room.issue, recorded)
			}
		}
		t.Logf("the rooms of %d and %d blocks: their first passes, which write every block, held the owner lock %v and %v, and the passes over a version that changes one block %v and %s",
			small.room.blocks, large.room.blocks, small.first, large.first, times.small, times.largeHeld())
	}) {
		t.FailNow()
	}
	return times
}

// passGrowthStopAt is how long a run lets its pass over the larger room hold the owner lock before
// it stops that pass (passGrowthReconcile), where small is how long its pass over the smaller room
// held it, runsLeft how many runs the test may still make, this one among them, and overhead how
// long this run took before that pass, which each run left to make is taken to take again: the
// longer of passGrowthStop times the bound over small, past which the pass has read a ratio above
// the bound, and the run's share of the time left before deadline less passGrowthMargin, so a
// pass that fits in that share holds the lock to its end and its ratio is measured whole. Where
// go test sets no deadline (deadline is zero), it answers zero, and no pass is stopped.
func passGrowthStopAt(small time.Duration, deadline time.Time, runsLeft int, overhead time.Duration) time.Duration {
	if deadline.IsZero() {
		return 0
	}
	floor := time.Duration(passGrowthStop * passGrowthBound * float64(small))
	share := (time.Until(deadline) - passGrowthMargin - time.Duration(runsLeft-1)*overhead) / time.Duration(runsLeft)
	return max(floor, share)
}

// passGrowthMedians is the median of runs, of which there is an odd number, over each room. A
// pass the run stopped sorts after every pass that held the lock to its end, as one that would
// have held it longer, which can raise the median over the larger room and never lower it. Fewer
// than half the runs have stopped one (passGrowthDecided), so the median over the larger room is
// a pass that ran to its end.
func passGrowthMedians(runs []passGrowthTimes) (small, large time.Duration) {
	smalls := make([]time.Duration, 0, len(runs))
	for _, run := range runs {
		smalls = append(smalls, run.small)
	}
	slices.Sort(smalls)
	larges := slices.SortedFunc(slices.Values(runs), func(a, b passGrowthTimes) int {
		if a.stopped != b.stopped {
			if a.stopped {
				return 1
			}
			return -1
		}
		return cmp.Compare(a.large, b.large)
	})
	return smalls[len(smalls)/2], larges[len(larges)/2].large
}

// passGrowthHeld is the times of runs over each room as the test's messages state them.
func passGrowthHeld(runs []passGrowthTimes) (small, large string) {
	smalls, larges := make([]string, 0, len(runs)), make([]string, 0, len(runs))
	for _, run := range runs {
		smalls, larges = append(smalls, run.small.String()), append(larges, run.largeHeld())
	}
	return "[" + strings.Join(smalls, " ") + "]", "[" + strings.Join(larges, " ") + "]"
}

// passGrowthDecided fails the measurement once a majority of its passGrowthRuns runs has stopped
// the pass over the larger room: a stopped pass sorts after every pass that held the lock to its
// end (passGrowthMedians), so whatever the runs not yet made read, the median over the larger room
// is a stopped pass, one that read a ratio of at least passGrowthStop times the bound over its own
// run's pass over the smaller room (passGrowthStopAt). It names the ratio each run read over its
// own two passes: a run that held the larger room's pass to its end, the ratio of the two times; a
// stopped run, the ratio of the time the pass had held the lock when it was stopped to the smaller
// room's, which the pass would only have gone past. A majority of the runs read more than the
// least of the stopped runs' ratios, whatever the others read, and that least ratio is above the
// bound.
func passGrowthDecided(t *testing.T, measurement string, runs []passGrowthTimes) {
	t.Helper()
	stopped, least := 0, 0.0
	read := make([]string, 0, len(runs))
	for index, run := range runs {
		ratio := float64(run.large) / float64(run.small)
		if !run.stopped {
			read = append(read, fmt.Sprintf("run %d read a ratio of %.2f", index+1, ratio))
			continue
		}
		if stopped == 0 || ratio < least {
			least = ratio
		}
		stopped++
		read = append(read, fmt.Sprintf("run %d read a ratio of more than %.2f when it was stopped", index+1, ratio))
	}
	if stopped <= passGrowthRuns/2 {
		return
	}
	small, large := passGrowthHeld(runs)
	t.Fatalf("%s: %d of the %d runs stopped the pass over %d blocks once it had held the owner lock at least %.2f times the bound over its run's pass over %d blocks and the run's share of the time left, %s over %d blocks against %s over %d: %s; so whatever the other runs read, a majority of the %d runs read a ratio of more than %.2f, above the bound %.2f",
		measurement, stopped, passGrowthRuns, passGrowthLarge, passGrowthStop, passGrowthSmall, large, passGrowthLarge, small, passGrowthSmall,
		strings.Join(read, ", "), passGrowthRuns, least, passGrowthBound)
}

// The reconciliation's pass stays one transaction, and the time the pass over a version that
// changes one block holds the owner lock grows in proportion to the room (c-pass-growth; R15 of
// the issue-tree spec).
//
// Each measurement makes passGrowthRuns runs (passGrowthRun), each over a room of 4,682 blocks and
// a room of 20,000, each room on a database of its own: the room's blocks in the deep spec's shape
// and in two documents of the room, half in each (passGrowthRoomText), written through the real
// write and indexed by the real tick (ReconcileBlockIndex), then a version of each that changes
// one block, and the real tick again, timing the transaction of each room's pass that holds the
// owner lock (passGrowthLockHeld), the two passes one right after the other. The ratio of the
// median time over 20,000 blocks to the median over 4,682 stays at or under passGrowthBound. It
// measures twice: once with the statistics of a near-empty graph_tuples, analyzed before the rooms
// are written, so the planner's estimate cannot rescue a read whose cost grows with the square of
// the room, and once with the tables analyzed after the rooms are indexed. No run's statistics
// move after they are taken (passGrowthStatistics).
//
// It fails, naming the measurement, the ratio and the bound, where a read of the pass costs more
// than one probe a block: planRoomBlocks reading the room's blocks by a join of issue_room_blocks
// on block_id_key, whose plan under the analyzed measurement's statistics compares each block's
// row with every block the version holds, or refs.blockAuthors reading with MovedRoomBlockSQL,
// whose plan under the near-empty measurement's reads the in_document tuple of every block in the
// store for each block. Such a read holds the lock for minutes over 20,000 blocks, and the test
// reaches its verdict within the time go test gives it: a run lets the pass over 20,000 blocks
// run to its end where it fits in the run's share of the time left before go test's deadline, so
// the ratio of the medians is measured whole, and else stops it once it has held the lock the
// longer of that share and passGrowthStop times the bound over the run's pass over 4,682 blocks
// (passGrowthStopAt); a measurement fails once a majority of its runs has stopped it
// (passGrowthDecided), naming the ratio each run read, a stopped run's as the ratio its pass had
// reached when it was stopped, and a measurement that fails ends the test, the near-empty one,
// which catches blockAuthors' read, first. Under either read the test so fails naming the
// measurement, the ratio and the bound within the 10 minutes go test gives the check's run.
func TestThePassLockTimeGrowsLinearlyWithTheRoom(t *testing.T) {
	// Serial: it times the owner lock's hold by the wall clock, and a run holds a room of 4,682
	// blocks and one of 20,000 at once, far more memory than any other test of the package.
	texts := [2]passGrowthText{passGrowthRoomText(t, passGrowthSmall), passGrowthRoomText(t, passGrowthLarge)}
	deadline, _ := t.Deadline()
	measurements := []struct {
		name      string
		nearEmpty bool
	}{
		{name: "graph_tuples analyzed near empty before the rooms are written", nearEmpty: true},
		{name: "tables analyzed after the rooms are indexed"},
	}
	for index, measurement := range measurements {
		if !t.Run(measurement.name, func(t *testing.T) {
			var runs []passGrowthTimes
			for run := range passGrowthRuns {
				runsLeft := passGrowthRuns - run + passGrowthRuns*(len(measurements)-index-1)
				runs = append(runs, passGrowthRun(t, fmt.Sprintf("run %d", run+1), texts, measurement.nearEmpty, deadline, runsLeft))
				passGrowthDecided(t, measurement.name, runs)
			}
			smallMedian, largeMedian := passGrowthMedians(runs)
			if smallMedian <= 0 {
				t.Fatalf("%s: the pass over %d blocks held the owner lock a median %v, which no ratio can be taken over", measurement.name, passGrowthSmall, smallMedian)
			}
			small, large := passGrowthHeld(runs)
			ratio := float64(largeMedian) / float64(smallMedian)
			t.Logf("%s: the pass held the owner lock %s over %d blocks and %s over %d blocks, medians %v and %v, a ratio of %.2f against the bound %.2f",
				measurement.name, small, passGrowthSmall, large, passGrowthLarge, smallMedian, largeMedian, ratio, passGrowthBound)
			if ratio > passGrowthBound {
				t.Errorf("%s: the pass held the owner lock a median %v over %d blocks and %v over %d blocks, a ratio of %.2f, above the bound %.2f",
					measurement.name, largeMedian, passGrowthLarge, smallMedian, passGrowthSmall, ratio, passGrowthBound)
			}
		}) {
			return
		}
	}
}
