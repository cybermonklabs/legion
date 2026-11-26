//go:build memory

package docs

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The two rooms TestThePassLockTimeGrowsLinearlyWithTheRoom measures, in blocks: the deep spec's
// 4,682, and 20,000. Each room holds its spec's blocks and passGrowthOtherBlocks more, the paragraph
// createIssueRoom seeds the plan's fragment with (planSeed), which the room's first settlement
// gives its id.
const (
	passGrowthSmall       = 4_682
	passGrowthLarge       = 20_000
	passGrowthOtherBlocks = 1
)

// The deep spec's shape (api's deepXidSpec): chains of a paragraph and a list nested
// passGrowthLevels levels below it, a list and its one item's paragraph at each level, so
// passGrowthChainBlocks blocks a chain, under the spec's heading.
const (
	passGrowthLevels      = 6
	passGrowthChainBlocks = 1 + 2*passGrowthLevels
)

// passGrowthRuns is how many rooms of each size a measurement writes and times, its time the
// median of theirs.
const passGrowthRuns = 3

// passGrowthBound is the most the median time the reconciliation's pass holds the owner lock over
// a room of 20,000 blocks may be, as a multiple of that over a room of 4,682. The rooms' sizes stand
// in the ratio 20,000 / 4,682 = 4.27, which a pass whose cost grows in proportion to the room reads
// at most, its fixed cost pulling the ratio below it; one whose cost grows with the room times its
// logarithm reads 5.0, and one whose cost grows with the square of the room 18.2. The bound is the
// size ratio with half of it again as headroom, 6.4, far below the square's. No measurement on the
// build farm stands beside it yet: this test was written before it was run there, and R15 of this
// issue's spec has the bound set from three paired runs of it on the farm, whose ratios belong
// here, with the bound set from them.
const passGrowthBound = 6.4

// passGrowthTicks is how many reconciliation ticks a measurement runs at most to index every room
// it wrote.
const passGrowthTicks = 10

// passGrowthRoom is one room a measurement wrote, with the blocks it holds once indexed.
type passGrowthRoom struct {
	issueRoom
	blocks int
}

// passGrowthSpec is a spec of blocks blocks in the deep spec's shape: its heading, as many whole
// chains as fit, and one paragraph a block for the rest, each opening as a chain's paragraph does.
// It fails the test where the spec does not hold blocks blocks as the server stamps them.
func passGrowthSpec(t *testing.T, blocks int) string {
	t.Helper()
	chains, rest := (blocks-1)/passGrowthChainBlocks, (blocks-1)%passGrowthChainBlocks
	var spec strings.Builder
	spec.WriteString("# Deep spec of a room\n\n")
	for chain := 1; chain <= chains; chain++ {
		fmt.Fprintf(&spec, "Chain %d holds the outbox.\n\n", chain)
		for level := range passGrowthLevels {
			fmt.Fprintf(&spec, "%s- Level %d of chain %d reads the bus.\n", strings.Repeat("  ", level), level+1, chain)
		}
		spec.WriteString("\n")
	}
	for paragraph := 1; paragraph <= rest; paragraph++ {
		fmt.Fprintf(&spec, "Chain %d holds the outbox alone.\n\n", chains+paragraph)
	}
	tree, err := pmdoc.Parse(spec.String())
	if err != nil {
		t.Fatalf("the deep spec of %d blocks does not parse: %v", blocks, err)
	}
	pmdoc.EnsureBlockIDs(tree)
	if _, offsets, err := pmdoc.RenderWithBlockOffsets(tree); err != nil || len(offsets) != blocks {
		t.Fatalf("the deep spec holds %d blocks (%v), want %d", len(offsets), err, blocks)
	}
	return spec.String()
}

// passGrowthStatistics takes the planner's statistics of every table of database as it stands,
// and keeps them: autovacuum analyzes no table of it afterwards, so every pass the measurement
// times is planned from the statistics taken here, and not from ones a background analyze took
// partway through the passes.
func passGrowthStatistics(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `
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
	for _, table := range tables {
		if _, err := database.Pool.Exec(ctx, `alter table `+table+` set (autovacuum_enabled = false)`); err != nil {
			t.Fatalf("keep autovacuum off %s: %v", table, err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `analyze`); err != nil {
		t.Fatalf("analyze the tables: %v", err)
	}
}

// passGrowthWrite writes the room of the converted issue DOC-<number> (createIssueRoom), its spec
// the deep spec spec, through the real write: the room's first settlement while its spec holds one
// paragraph, which versions each fragment whose text its document's version does not hold - the
// plan's seed among them - and indexes the room in its own transaction, so the room is no longer
// ahead of its versions; then the deep spec written into the spec's fragment and versioned through
// the named-version route's write (passGrowthNamedVersion), which leaves the room's blocks to the
// reconciliation. It answers the room and the blocks it holds once indexed.
func passGrowthWrite(t *testing.T, database *store.Store, service *Service, number, blocks int, spec string) passGrowthRoom {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	room := createIssueRoom(t, database, number)
	if _, err := joinedReplaceText(service, room.spec, "The room's first words.\n", alice); err != nil {
		t.Fatalf("write the first words of %s's spec: %v", room.issue, err)
	}
	if err := service.waitForDurableAppends(ctx, room.tree); err != nil {
		t.Fatalf("wait for the appends of %s's room: %v", room.issue, err)
	}
	if err := service.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle %s's room: %v", room.issue, err)
	}
	if _, err := joinedReplaceText(service, room.spec, spec, alice); err != nil {
		t.Fatalf("write the deep spec of %d blocks into %s's spec: %v", blocks-passGrowthOtherBlocks, room.issue, err)
	}
	passGrowthNamedVersion(t, service, room)
	// The named version holds the edit's text; the room's settlement, which the test's hour-long
	// settle delay holds off, has nothing of it left to version.
	settle(t, database, room.tree)
	if indexed := passGrowthRoomBlocks(t, database, room.tree); indexed >= blocks {
		t.Fatalf("the named version of %s's spec indexed its room's %d blocks in its own request, want them left to the reconciliation", room.issue, indexed)
	}
	return passGrowthRoom{issueRoom: room, blocks: blocks}
}

// passGrowthNamedVersion writes the next version of room's spec as the named-version route writes
// it (api's createNamedVersion): NamedVersion in a transaction joined to the document service,
// marked as the route marks it (WithDeferredBlockIndex), so a version of a room above
// deferBlockIndexBlocks leaves the room's blocks to the block-index reconciliation; the version's
// event; what the write stated appended under that event; and the commit.
func passGrowthNamedVersion(t *testing.T, service *Service, room issueRoom) {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the named version of %s's spec: %v", room.issue, err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	result, err := service.NamedVersion(WithDeferredBlockIndex(joined), room.spec, "The deep spec", model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatalf("write the named version of %s's spec: %v", room.issue, err)
	}
	if !result.Wrote {
		t.Fatalf("the named version of %s's spec wrote nothing, want the deep spec versioned", room.issue)
	}
	event, err := recordIssueEvent(ctx, tx, room.issue, "artifact.version", aliceActor,
		versionEventPayload(t, room.spec, "spec.md", result.Version.Number))
	if err != nil {
		t.Fatalf("record the event of %s's named version: %v", room.issue, err)
	}
	if err := causeInWritePhase(joined, tx, result.Blocks, event); err != nil {
		t.Fatalf("append what %s's named version stated: %v", room.issue, err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the named version of %s's spec: %v", room.issue, err)
	}
}

// passGrowthRoomBlocks is how many document_blocks rows the room's tree artifact keys.
func passGrowthRoomBlocks(t *testing.T, database *store.Store, tree string) int {
	t.Helper()
	var blocks int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from document_blocks where artifact_id = $1`, tree).Scan(&blocks); err != nil {
		t.Fatalf("count the blocks of room %s: %v", tree, err)
	}
	return blocks
}

// passGrowthLockHeld is how long the transaction of the reconciliation's pass has held the owner
// lock, read from the pass's own hook (afterBlockIndexWrite), while that transaction waits between
// its last write and its commit: the time since it began, read off the database's clock, which
// its first statement after its begin takes the owner lock in (lockArtifactOwner). The pass's
// backend is the one of this test's database that sits idle in a transaction holding an advisory
// lock, which the owner lock is.
func passGrowthLockHeld(ctx context.Context, database *store.Store) (time.Duration, error) {
	var holders int
	var seconds float64
	if err := database.Pool.QueryRow(ctx, `
		select count(*), coalesce(max(extract(epoch from clock_timestamp() - a.xact_start)), 0)::float8
		from pg_stat_activity a
		where a.datname = current_database() and a.pid <> pg_backend_pid()
		  and a.state = 'idle in transaction'
		  and exists (select 1 from pg_locks l
		              where l.pid = a.pid and l.locktype = 'advisory' and l.granted
		                and l.database = (select oid from pg_database where datname = current_database()))
	`).Scan(&holders, &seconds); err != nil {
		return 0, err
	}
	if holders != 1 {
		return 0, fmt.Errorf("%d transactions of the test's database sit idle holding an advisory lock, want the pass's one", holders)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// passGrowthReconcile runs the reconciliation's real tick until it has indexed every room of
// rooms, and answers the time the first pass over each held the owner lock (passGrowthLockHeld),
// by the room's tree artifact. It fails where a room is left unindexed or holds other than its
// blocks once indexed.
func passGrowthReconcile(t *testing.T, database *store.Store, service *Service, rooms []passGrowthRoom) map[string]time.Duration {
	t.Helper()
	ctx := context.Background()
	owner := map[string]string{}
	for _, room := range rooms {
		for _, artifact := range []string{room.tree, room.spec, room.verification, room.plan} {
			owner[artifact] = room.tree
		}
	}
	held := map[string]time.Duration{}
	var failures []string
	service.afterBlockIndexWrite = func(artifactID string) error {
		tree, ours := owner[artifactID]
		if !ours {
			return nil
		}
		if _, timed := held[tree]; timed {
			return nil
		}
		lock, err := passGrowthLockHeld(ctx, database)
		if err != nil {
			failures = append(failures, fmt.Sprintf("time the pass over room %s: %v", tree, err))
			return nil
		}
		held[tree] = lock
		return nil
	}
	defer func() { service.afterBlockIndexWrite = nil }()
	for range passGrowthTicks {
		tick, err := service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, nil)
		if err != nil {
			t.Fatalf("reconcile the block index: %v", err)
		}
		if tick.LiveAhead != 0 || tick.Unreadable != 0 {
			t.Fatalf("the tick found %d rooms live ahead and %d unreadable, want each room indexed", tick.LiveAhead, tick.Unreadable)
		}
		if len(held) == len(rooms) {
			break
		}
	}
	for _, failure := range failures {
		t.Error(failure)
	}
	for _, room := range rooms {
		if _, timed := held[room.tree]; !timed {
			t.Fatalf("no pass of the reconciliation indexed the room of %s, of %d blocks, in %d ticks", room.issue, room.blocks, passGrowthTicks)
		}
		if indexed := passGrowthRoomBlocks(t, database, room.tree); indexed != room.blocks {
			t.Fatalf("the room of %s holds %d blocks after its pass, want %d", room.issue, indexed, room.blocks)
		}
	}
	return held
}

// passGrowthMedian is the median of times, of which there is an odd number.
func passGrowthMedian(times []time.Duration) time.Duration {
	sorted := slices.Sorted(slices.Values(times))
	return sorted[len(sorted)/2]
}

// The reconciliation's pass stays one transaction, and the time it holds the owner lock grows in
// proportion to the room (c-pass-growth; R15 of this issue's spec).
//
// Each measurement writes, on a database of its own, passGrowthRuns rooms of 4,682 blocks and as
// many of 20,000, the deep spec's shape, one size after the other, each through the real write
// (passGrowthWrite), and runs the real tick (ReconcileBlockIndex) until every room is indexed,
// timing the transaction of each room's pass that holds the owner lock (passGrowthLockHeld). The
// ratio of the median time over 20,000 blocks to the median over 4,682 stays at or under
// passGrowthBound. It measures twice: once with the tables analyzed after the rooms are written,
// and once with the statistics of a near-empty graph_tuples, analyzed before the rooms are
// written, so the planner's estimate cannot rescue a read whose cost grows with the square of the
// room. Neither measurement's statistics move while its passes run (passGrowthStatistics).
//
// It fails, naming the measurement, the ratio and the bound, where a read of the pass costs more
// than one probe a block: planRoomBlocks reading the room's blocks by a join of issue_room_blocks
// on block_id_key, or refs.blockAuthors reading with MovedRoomBlockSQL.
func TestThePassLockTimeGrowsLinearlyWithTheRoom(t *testing.T) {
	// Serial: it times the owner lock's hold by the wall clock, and each of its measurements holds
	// six rooms of up to 20,000 blocks, far more memory than any other test of the package.
	small := passGrowthSpec(t, passGrowthSmall-passGrowthOtherBlocks)
	large := passGrowthSpec(t, passGrowthLarge-passGrowthOtherBlocks)
	for _, measurement := range []struct {
		name        string
		beforeRooms bool
	}{
		{name: "tables analyzed after the rooms are written"},
		{name: "graph_tuples analyzed near empty before the rooms are written", beforeRooms: true},
	} {
		t.Run(measurement.name, func(t *testing.T) {
			database := storetest.Open(t)
			service, _ := newTestServiceInstance(t, database)
			if measurement.beforeRooms {
				passGrowthStatistics(t, database)
			}
			var rooms []passGrowthRoom
			for run := range passGrowthRuns {
				rooms = append(rooms,
					passGrowthWrite(t, database, service, 2*run+1, passGrowthSmall, small),
					passGrowthWrite(t, database, service, 2*run+2, passGrowthLarge, large))
			}
			if !measurement.beforeRooms {
				passGrowthStatistics(t, database)
			}
			held := passGrowthReconcile(t, database, service, rooms)
			var smallTimes, largeTimes []time.Duration
			for _, room := range rooms {
				if room.blocks == passGrowthSmall {
					smallTimes = append(smallTimes, held[room.tree])
				} else {
					largeTimes = append(largeTimes, held[room.tree])
				}
			}
			smallMedian, largeMedian := passGrowthMedian(smallTimes), passGrowthMedian(largeTimes)
			if smallMedian <= 0 {
				t.Fatalf("%s: the pass over %d blocks held the owner lock %v, which no ratio can be taken over", measurement.name, passGrowthSmall, smallTimes)
			}
			ratio := float64(largeMedian) / float64(smallMedian)
			t.Logf("%s: the pass held the owner lock %v over %d blocks and %v over %d blocks, medians %v and %v, a ratio of %.2f against the bound %.2f",
				measurement.name, smallTimes, passGrowthSmall, largeTimes, passGrowthLarge, smallMedian, largeMedian, ratio, passGrowthBound)
			if ratio > passGrowthBound {
				t.Errorf("%s: the pass held the owner lock a median %v over %d blocks and %v over %d blocks, a ratio of %.2f, above the bound %.2f",
					measurement.name, largeMedian, passGrowthLarge, smallMedian, passGrowthSmall, ratio, passGrowthBound)
			}
		})
	}
}
