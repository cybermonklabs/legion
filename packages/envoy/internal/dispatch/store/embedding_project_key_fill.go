package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// ErrEmbeddingProjectKeyProgressGone is the row 20261109120100 creates for the fill to record
// its place in not being there to read: the row absent from the table, or the table absent from
// the database. Every read of that row below answers it wrapped around Postgres's own error, so
// the caller that has to tell this apart from a failure it should try again - the loop that
// keeps taking the lease until the labelling is over (api.RunEmbeddingProjectKeyFill) - reads
// one sentinel rather than a SQLSTATE of its own.
//
// The two are one condition and not two because the fill wants the same thing of both: nothing
// in this release deletes either, so a database missing one is a database somebody repaired by
// hand, one restored from a dump taken before 20261109120100, or one somebody is moving, and in
// every one of them the pass has no cursor to start from and no done to record. Waiting brings
// neither back.
var ErrEmbeddingProjectKeyProgressGone = errors.New("the embeddings project_key fill has no progress row to read")

// embeddingProjectKeyProgressGone wraps err in that sentinel where it is one of the two
// absences, and hands back every other error as it stands. 42P01 is undefined_table, which is
// what Postgres answers for the whole statement when the table is not there; pgx.ErrNoRows is
// what a `select` of the one row answers when the table is there and the row is not.
func embeddingProjectKeyProgressGone(err error) error {
	var pgErr *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pgErr) && pgErr.Code == "42P01") {
		return fmt.Errorf("%w: %w", ErrEmbeddingProjectKeyProgressGone, err)
	}
	return err
}

// EmbeddingProjectKeyBatch is how many embeddings rows one transaction of
// FillEmbeddingProjectKeys labels, and this is the one passage that says what a label costs and
// how fast the pass goes; everything else about the fill points here.
//
// What it costs is not the row. embeddings.embedding is stored external (pg_attribute.attstorage
// = 'e'), so the vector stays in TOAST and no TOAST row is rewritten: a label writes one new heap
// tuple of about 253 bytes. What it costs is the vector index the row is in. Relabelling 20,000
// rows of the load-test deployment grew embeddings_cosine from 133,328 KiB to 231,136 KiB -
// 4.9 KiB a labelled row with embeddings_kind_project also present, 6.6 KiB without it - and an
// HNSW index only grows, so vacuum gives none of it back. Extrapolated to that deployment's
// 1,109,536 embedded rows (an extrapolation, not a measurement: the 20,000-row figure is what was
// measured) a whole pass asks for about 5.2 GiB of free disk and keeps it until someone rebuilds
// the index, which is what a deployment has to have free before it takes this release.
//
// How fast it goes, in the two shapes the round measured. Labelled in place, as this pass does,
// each row pays its own HNSW maintenance: 120,000 rows in 12 m 50 s and 19,650 in 86 s, which is
// 156 to 228 rows a second, so a deployment of 1,109,536 rows is one to two hours. Relabelled
// whole, with the index work taken once at the end rather than row by row, the same 1,109,536
// rows took 31 m 41 s - 584 rows a second, of which about 31 m is the HNSW work alone. The two
// figures are the same work paid in two shapes, and the pass plans by the first: nothing here
// goes faster than 228 rows a second (INFRA-988,
// 20261109120100_embeddings_project_key_backfill.up.sql).
//
// 200 rows is about a second at those rates, which is the longest a live write of a row this
// batch has already taken - the enqueue of a window whose text just changed - waits for the
// batch's commit. Smaller batches would buy that writer little and cost one more commit, one
// more `for update` of the cursor and one more descent of embeddings_pkey each.
const EmbeddingProjectKeyBatch = 200

// embeddingProjectKeyProgressEvery is how often a running fill says where it has got to. The pass
// is one to two hours on a deployment of a million vectors, at 156 to 228 rows a second
// (EmbeddingProjectKeyBatch's measurements), so a line every half minute is 120 to 240 lines for
// the whole of it: enough for a reader of a rolling deploy to see the row count climbing within
// one scroll of the log, and far short of the one line per batch - 5,500 of them - that logging
// each second of work would be.
//
// It is a variable only so a test can see a progress line without labelling a deployment's worth
// of rows, as duty's jitterN is a variable only so a test can fix the draw
// (internal/dispatch/duty/ticker.go:22).
var embeddingProjectKeyProgressEvery = 30 * time.Second

// fillEmbeddingProjectKeysCursorQuery reads the cursor the next batch starts from. It is a
// statement of its own so that the batch below can be planned against the values it answers,
// bound as parameters, which is what lets the planner read them as the constants they are.
//
// The cursor is read `for update`, which is what makes two processes filling at once take
// different batches rather than the same one: the lock is held to the batch's commit, so the
// second waits for the first and then reads the cursor it moved. The pass also runs under the
// lease embeddings-project-key-fill (api.RunEmbeddingProjectKeyFill), which keeps a second
// process out of the fill altogether; this lock is what still holds where a lease has expired
// under a process that stalled rather than stopped, and it costs one read of a one-row table
// per batch.
//
// Why the cursor is not a CTE of the batch any longer. It was `with progress as (select kind,
// id from embeddings_project_key_backfill for update)`, joined to embeddings in the batch's
// own CTE, and the planner made that join a nested loop whose inner side is the cursor:
// measured on the load-test deployment's own database, 1,104,667 embedded rows,
// `EXPLAIN (ANALYZE, BUFFERS)` of the batch as the fill issues it read
//
//	->  CTE Scan on progress p (actual time=316.918..316.918 rows=1.00 loops=400)
//	->  Index Scan using embeddings_pkey on embeddings e (actual time=0.028..1.794 rows=400.00 loops=1)
//	Rows Removed by Join Filter: 200
//	Execution Time: 126769.773 ms
//
// - 127 s for one batch of 200 rows, which is about eight days for the table, and the whole of
// it is the cursor re-read once per candidate row. A cursor the planner cannot see is also a
// cursor it cannot push into the index: the scan answered 400 rows for a batch of 200 and a
// join filter threw half of them away, where a bound pair is an index condition and the scan
// answers exactly the batch. The shape passed its tests and a warm reading of it because on a
// small warm database the same statement is fast - the fill labelled 2,200 rows in 30 s there -
// so the test that holds this shape in place is a plan test, not a timing one
// (TestTheEmbeddingProjectKeyBatchReadsOneBoundedIndexScan).
const fillEmbeddingProjectKeysCursorQuery = `select kind, id from embeddings_project_key_backfill for update`

// fillEmbeddingProjectKeysBatchQuery is one batch: it takes the next EmbeddingProjectKeyBatch
// rows by keyset from the cursor, labels each one whose project_key is still null from the
// owner the row belongs to, moves the cursor to the last row it took, and answers the rows it
// labelled and whether the pass is finished. $1 and $2 are the cursor
// fillEmbeddingProjectKeysCursorQuery answered, $3 the batch size.
//
// `batch` is the keyset: the next rows above the cursor in (kind, id) order, which is
// embeddings' own primary key, so the scan starts where the last batch stopped instead of
// counting past it as an `offset` would. It takes the rows of every kind, the issue's whole-text
// rows among the four window kinds, because every arm of meaning search now reads the column
// (api/search.go).
//
// No index is added for this read, and in particular no partial
// `embeddings (kind, id) where project_key is null`. The keyset is the table's own primary
// key, so a batch is one descent of embeddings_pkey and exactly $3 entries of it wherever the
// cursor stands - the plan test reads 200 entries at the start of the table and 200 from the
// middle of it. What a partial index buys a backfill is the labelled rows a late batch walks
// past, and a keyset leaves none to walk past: every row the pass reads it leaves behind it.
// The rows it reads and does not write are the ones stored since 20261109120000, which the
// write functions labelled themselves, and those are spread over the whole key range rather
// than banked above the cursor. Against nothing it would cost a second index built over
// 1,104,667 rows under the ACCESS EXCLUSIVE lock of a migration (20261109120000's header), an
// entry to maintain on every row written for as long as it existed, and an index that shrinks
// to empty as the pass it serves finishes.
//
// `labelled` writes only where project_key is still null, so a row written since 20261109120000
// - which the write functions label themselves - is read and left alone rather than rewritten,
// and a second run of the pass over the same keys writes nothing. The value is
// embeddings_owner_project_key's, the one definition of the project a row belongs to
// (20261109120000), and `batch` computes it: once per row of the batch, in the target list of
// the scan that reads the row anyway, rather than once in the update's own filter and again in
// its `set`, which is what naming a plpgsql function twice would cost.
//
// It also writes only where that value is a project. A row whose owner is gone resolves to
// null and would be written null over null: a new heap tuple of about 253 bytes and an entry
// in every index the row is in, the vector index among them, which only grows
// (EmbeddingProjectKeyBatch holds what that costs). It gains nothing, since the row comes out
// as unlabelled as it went in: measured over the sixteen unlabellable rows this round read,
// eleven of them moved tuple for a null written over null (INFRA-988). Such a row stays null
// for ever: nothing can label it, no join would have returned it to a scoped search either,
// and the block index deletes such a window when it next sees its owner. The cursor still
// walks past it like any other row, because the cursor comes from `batch`, which takes every
// row whatever its owner.
//
// So the count is of the rows that gained a project, `count(project_key)` over what the write
// returned: the nine owner shapes that resolve to no project - an item whose row is gone, an id
// in no shape a uuid cast takes, a retired whole-text row, each kind of orphan - are walked
// past and not written, and counting them told a reader of the finish line that 16 rows had
// been labelled where 5 had (INFRA-988).
//
// The cursor comes from `batch` and never from `labelled`'s `returning`: a row already labelled
// returns nothing, so a cursor taken from the write would stall on the first such row. `done`
// is set when the batch took no further row, which is the pass finished; the cursor then stays
// where it is.
//
// `batch` is read once and kept. Two references to a CTE already mean `as materialized`, which
// is written out here because the defect this statement replaced was a side the planner read
// again for every candidate row, and an edit that left this CTE with one reference would
// quietly inline it into the two statements that read it - and inlined, the owner lookup in its
// target list would be made once per reference rather than once.
const fillEmbeddingProjectKeysBatchQuery = `
	with batch as materialized (
		select e.kind, e.id,
		       case when e.project_key is null then embeddings_owner_project_key(e.kind, e.id) end as owner_project_key
		  from embeddings e
		 where (e.kind, e.id) > ($1::text, $2::text)
		 order by e.kind, e.id
		 limit $3
	), last as (
		select b.kind, b.id from batch b order by b.kind desc, b.id desc limit 1
	), labelled as (
		update embeddings e
		   set project_key = b.owner_project_key
		  from batch b
		 where e.kind = b.kind and e.id = b.id and e.project_key is null
		   and b.owner_project_key is not null
		returning e.project_key
	), moved as (
		update embeddings_project_key_backfill
		   set kind = coalesce((select kind from last), embeddings_project_key_backfill.kind),
		       id = coalesce((select id from last), embeddings_project_key_backfill.id),
		       done = not exists (select 1 from last)
		returning done
	)
	select (select count(project_key) from labelled), (select done from moved)`

// EmbeddingProjectKeyFillDone answers whether the pass has recorded itself finished, which is
// the one column of the cursor row a reader outside the pass needs: the process that keeps
// trying for the lease until the labelling is over reads it between tries
// (api.RunEmbeddingProjectKeyFill), and reads it rather than calling the fill again, which would
// write the fill's own "nothing to do" line every interval for the life of the process. The pass
// is finished once the rows are labelled and embeddings is the partitioned table
// (partitionEmbeddings), so a database still holding the unpartitioned one reads false.
//
// It costs one row of a one-row table by its only key and takes no lock: a pass running in
// another process holds that row `for update` only for the length of a batch, and a reader that
// sees the value a batch is about to change tries again at its own interval.
func (s *Store) EmbeddingProjectKeyFillDone(ctx context.Context) (bool, error) {
	var done bool
	if err := s.Pool.QueryRow(ctx, "select done from embeddings_project_key_backfill").Scan(&done); err != nil {
		return false, fmt.Errorf("read whether the embeddings project_key fill is finished: %w",
			embeddingProjectKeyProgressGone(err))
	}
	if !done {
		return false, nil
	}
	return s.embeddingsPartitioned(ctx)
}

// FillEmbeddingProjectKeys labels the embeddings rows stored before 20261109120000 added
// project_key, and answers how many of them gained a project; then it moves embeddings onto the
// table partitioned by that project (PartitionEmbeddings). The boot starts it in the background
// under a lease (api.RunEmbeddingProjectKeyFill): each half is one to two hours on a deployment
// of a million vectors (EmbeddingProjectKeyBatch, EmbeddingPartitionCopyBatch), and until the
// labelling has recorded itself done an unlabelled row is invisible to the column's own
// predicate - which is worse than the release before this one, since that release found the row
// through the owner each meaning arm joins. So a project-scoped meaning search is answered by
// that older, joined predicate until the labelling records itself done (api/search.go,
// meaningProjectScope), the server serves while the pass runs, and every batch moves rows onto
// the predicate the column exists for. It answers an error, and the runner tries again, until
// both halves are done.
//
// What embeddings is, in order. Before the swap it is the table 0072 made, keyed (kind, id): a
// `before insert` trigger labels a row that names no project from its owner (20261110064913),
// and 20261120105818's mirror trigger writes every write of it into embeddings_partitioned as
// well. Once the labelling's cursor reads done, the copy has carried every stored row over and
// every live process serves buildCommit, this process's own commit, embeddings_swap_in_partitioned
// swaps embeddings_partitioned in under embeddings' own name: list-partitioned by project_key,
// keyed (kind, id, project_key), and labelled by an `after insert` trigger that moves a row of no
// project into its owner's partition. The runner tries again every five minutes until the
// labelling's cursor reads done and embeddings is partitioned (EmbeddingProjectKeyFillDone).
func (s *Store) FillEmbeddingProjectKeys(ctx context.Context, buildCommit string) (int64, error) {
	labelled, err := s.labelEmbeddingProjectKeys(ctx)
	if err != nil {
		return labelled, err
	}
	return labelled, s.PartitionEmbeddings(ctx, buildCommit)
}

// labelEmbeddingProjectKeys is FillEmbeddingProjectKeys' first half, the labelling.
//
// A pass already finished costs one row: the progress row 20261109120100 creates says so, and
// this reads nothing else. Otherwise it loops, each iteration one short transaction that takes
// the next batch by keyset, labels it and commits, so no row it writes stays uncommitted for
// longer than its own batch and an enqueue that meets one of its keys waits for that batch
// alone. 20261109120100's header holds the measurement that says why.
//
// What it logs, which is the whole of what a reader of a rolling deploy sees of an hour-long
// pass: one line that it began and the cursor it begins from, one every
// embeddingProjectKeyProgressEvery while it runs, with the rows and batches so far, and one that
// it finished, with the rows, the batches and how long it took. A pass with nothing to do says so
// in the one line that costs it, and the lines are distinct sentences rather than one sentence in
// two tenses, so a reader of a stopped deployment's log can tell a fill still running from a fill
// that finished.
func (s *Store) labelEmbeddingProjectKeys(ctx context.Context) (int64, error) {
	var fromKind, fromID string
	var done bool
	if err := s.Pool.QueryRow(ctx, "select kind, id, done from embeddings_project_key_backfill").
		Scan(&fromKind, &fromID, &done); err != nil {
		return 0, fmt.Errorf("read the embeddings project_key fill's progress: %w",
			embeddingProjectKeyProgressGone(err))
	}
	if done {
		slog.Info("dispatch: the embeddings project_key fill has nothing to do: every row stored before this release carries its project")
		return 0, nil
	}
	// The cursor the pass starts from, which is the empty pair on a database no pass has reached
	// yet and the last row of the last committed batch on one a killed process left part-labelled:
	// the reader of the second boot's log sees it resume rather than start again.
	started := time.Now()
	slog.Info("dispatch: the embeddings project_key fill began",
		"from_kind", fromKind, "from_id", fromID, "batch", EmbeddingProjectKeyBatch)
	var labelled, batches int64
	said := started
	for {
		rows, finished, err := s.fillEmbeddingProjectKeysBatch(ctx)
		if err != nil {
			return labelled, err
		}
		labelled += rows
		batches++
		if finished {
			slog.Info("dispatch: the embeddings project_key fill finished",
				"rows", labelled, "batches", batches, "duration", time.Since(started))
			return labelled, nil
		}
		// Timed from the last line rather than counted in batches, so the rate of the lines holds
		// whatever the database's own rate of rows is: a batch is about a second on the deployment
		// the measurements come from, and a tenth of that on an empty test database.
		if now := time.Now(); now.Sub(said) >= embeddingProjectKeyProgressEvery {
			said = now
			slog.Info("dispatch: the embeddings project_key fill is labelling",
				"rows", labelled, "batches", batches, "elapsed", now.Sub(started))
		}
	}
}

// fillEmbeddingProjectKeysBatch runs one batch in a transaction of its own and commits it,
// answering the rows it labelled and whether the batch took no further row, which is the pass
// finished.
//
// Two statements, one transaction: the cursor is read and locked first and the batch is then
// planned against its values. Both properties the one statement had are the transaction's and
// not the statement's - the row lock is held to the commit, and the batch and the cursor it
// moves commit together or not at all - so splitting it costs a round trip to a one-row table
// and buys the plan fillEmbeddingProjectKeysCursorQuery's comment measures.
func (s *Store) fillEmbeddingProjectKeysBatch(ctx context.Context) (int64, bool, error) {
	// A batch is a transaction like any other, so it is marked like any other: nothing it runs
	// may take a second pooled connection while it is open.
	ctx = WithTransactionTracking(ctx)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("begin an embeddings project_key fill batch: %w", err)
	}
	defer tx.Rollback(ctx)
	var fromKind, fromID string
	if err := tx.QueryRow(ctx, fillEmbeddingProjectKeysCursorQuery).Scan(&fromKind, &fromID); err != nil {
		return 0, false, fmt.Errorf("take the embeddings project_key fill's cursor: %w",
			embeddingProjectKeyProgressGone(err))
	}
	var labelled int64
	var finished bool
	if err := tx.QueryRow(ctx, fillEmbeddingProjectKeysBatchQuery, fromKind, fromID, EmbeddingProjectKeyBatch).
		Scan(&labelled, &finished); err != nil {
		return 0, false, fmt.Errorf("label a batch of embeddings rows with their project: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("commit an embeddings project_key fill batch: %w", err)
	}
	return labelled, finished, nil
}

// EmbeddingPartitionCopyBatch is how many embeddings rows one transaction of partitionEmbeddings'
// copy carries to embeddings_partitioned (20261120105330). What a row costs there is its vector:
// a new TOAST value of about 6 KB and an insertion into its partition's HNSW graph, which is
// most of it. The rows are taken FOR SHARE, so a write of one of them - the queue storing its
// vector, an edit enqueueing its new text - waits for the batch's commit, and 200 rows keeps
// that wait near the second EmbeddingProjectKeyBatch keeps it near for the labelling.
const EmbeddingPartitionCopyBatch = 200

// embeddingsPartitioned answers whether embeddings is the partitioned table yet, which it is
// once embeddings_swap_in_partitioned has run.
func (s *Store) embeddingsPartitioned(ctx context.Context) (bool, error) {
	var partitioned bool
	if err := s.Pool.QueryRow(ctx, embeddingsPartitionedQuery).Scan(&partitioned); err != nil {
		return false, fmt.Errorf("read whether embeddings is partitioned by project: %w", err)
	}
	return partitioned, nil
}

const embeddingsPartitionedQuery = `select relkind = 'p' from pg_class where oid = 'embeddings'::regclass`

// ErrEmbeddingsSwapWaits is the partition swap waiting for the fleet to serve one build: the copy
// is done, and PartitionEmbeddings answers it, wrapped around the commits of the other builds it
// waits for, instead of swapping. The runner logs it and tries again at its interval
// (api.RunEmbeddingProjectKeyFill).
//
// Why it waits. The swap replaces embeddings' key (kind, id) with (kind, id, project_key), since a
// partitioned table's unique key must hold its partition key, and every release before this one
// inserts its coverage rows with `on conflict (kind, id) do nothing` (embedqueue/coverage.go),
// which Postgres refuses when it plans the statement once no unique key on (kind, id) alone is
// left (SQLSTATE 42P10). A partitioned table cannot carry that key, so no form of the swap keeps
// that statement working. The registry records each process's commit, not which release came
// first, so the swap cannot tell an earlier build from a later one, and it waits for every build
// of another commit the fleet shows (embeddingsSwapOtherCommitsQuery): one registered within the
// registry's prune age, and one that published its dashboard within the hour, recorded under the
// publishing build's own commit (dashboard_publishes, migration 20261121120653). An image build
// always publishes its dashboard before its coverage pass runs and before it registers; a server
// run with no web files (no index.html in its web dist) publishes nothing and is seen only once
// it registers. It reads the fleet before the copy's count and again under the swap's own locks,
// so a build whose publish or registration commits before the swap's locks are held holds it.
// What is left is a build of another commit whose publish commits after the swap has taken them:
// a rollback begun as the swap commits, which costs what a rollback past the swap costs, the same
// statement failing (cmd/dispatch/README.md, "Database migrations", says what that build then
// does). An unstamped process (the empty commit) serves a build no commit names: beside a stamped
// one it is another commit and holds the swap, and an unstamped process swaps beside unstamped
// ones alone, which is a test's or a local server's fleet, since an image build always stamps its
// commit (packages/envoy/docker/Dockerfile refuses anything but a full sha).
var ErrEmbeddingsSwapWaits = errors.New("the embeddings partition swap waits while the fleet holds a build of another commit")

// embeddingsSwapOtherCommitsQuery is the commits of the builds the swap waits for, each once, an
// unstamped build's empty one among them: every server_processes row of another commit than $1,
// the swapping process's own, whose heartbeat is within $2 seconds, OwnerLockLegacyWindow (the
// registry's prune age, so a process frozen or cut from the database counts until its row is
// pruned), and every dashboard_publishes row of another commit that published within $3 seconds,
// OwnerLockMarkWindow (an earlier build's boot from its publish to its registration). These are
// the owner lock's two windows of an earlier build (OwnerLockLegacyWindow says why each), read
// by commit rather than by protocol. The publishes are read from dashboard_publishes, which a
// publish writes under its own commit, and not from dashboard_builds, whose commit is the first
// publisher's of that web build: two releases that ship one web dist share a row, and its commit
// says nothing of whose boot last renewed it (migration 20261121120653).
const embeddingsSwapOtherCommitsQuery = `
	select coalesce(array_agg(distinct commit order by commit), '{}') from (
		select commit from server_processes
		 where commit <> $1 and heartbeat_at > now() - make_interval(secs => $2)
		union all
		select commit from dashboard_publishes
		 where commit <> $1 and published_at > now() - make_interval(secs => $3)
	) others`

// embeddingsSwapOtherCommits reads embeddingsSwapOtherCommitsQuery on q, a pool or the swap's
// own transaction, for buildCommit.
func embeddingsSwapOtherCommits(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, buildCommit string) ([]string, error) {
	var others []string
	if err := q.QueryRow(ctx, embeddingsSwapOtherCommitsQuery, buildCommit,
		OwnerLockLegacyWindow.Seconds(), OwnerLockMarkWindow.Seconds()).Scan(&others); err != nil {
		return nil, fmt.Errorf("read the builds of other commits in the fleet: %w", err)
	}
	return others, nil
}

// errEmbeddingsSwapWaitsFor is ErrEmbeddingsSwapWaits naming the swapping build and the others.
func errEmbeddingsSwapWaitsFor(buildCommit string, others []string) error {
	return fmt.Errorf("%w: this process serves %q, and the fleet holds builds of %q (server_processes within %s, dashboard_publishes within %s)",
		ErrEmbeddingsSwapWaits, buildCommit, others, OwnerLockLegacyWindow, OwnerLockMarkWindow)
}

// embeddingsCopyCountsQuery counts both tables in one statement, so in one snapshot: the mirror
// writes both in the writer's own transaction, so a snapshot sees equal counts whenever every
// write reached both, and it takes no lock beyond the ACCESS SHARE every read takes.
const embeddingsCopyCountsQuery = `select (select count(*) from embeddings), (select count(*) from embeddings_partitioned)`

// PartitionEmbeddings is FillEmbeddingProjectKeys' second half: it copies every row of
// embeddings into embeddings_partitioned, the table 20261120105330 built beside it, and then
// swaps the two (embeddings_swap_in_partitioned). Writes made while it copies are carried over by
// 20261120105818's trigger, so once the copy has read the last row the two tables hold the same
// rows, and the swap holds both tables' ACCESS EXCLUSIVE locks for as long as it takes to compare
// their counts and rename, bounded by pgmigrate.LockTimeout while it waits for them. A swap that
// waits longer is given up and answered as an error, which the runner tries again.
//
// Before it takes those locks it makes two reads that take none. One is the fleet: while it holds
// a build of another commit than buildCommit, this process's own (cmd/dispatch's
// main.buildCommit, empty in an unstamped build; embeddingsSwapOtherCommitsQuery says which
// builds it counts), it answers ErrEmbeddingsSwapWaits naming those commits and swaps nothing
// (that error says why); the swap reads the fleet again under its locks
// (swapInEmbeddingPartitions). The other is both tables' counts in one snapshot
// (embeddingsCopyCountsQuery). Counts that differ mean a write reached one table and not the
// other - one made as a replica, or with the mirror's trigger disabled, or by hand, none of which
// fires the mirror - and no later pass would make them agree, so it says so and empties the copy
// and copies again (resetEmbeddingsCopy), once in the same call; a second disagreement empties it
// and answers an error, and the next try copies again. So a disagreement costs a copy and holds
// no reader, where taking the locks to find it held every read of embeddings for up to the lock
// wait on each try. The counts catch a write that changed how many rows one table holds and
// nothing else: a write that bypassed the mirror and left the counts equal - an update made as a
// replica, or a delete and an insert - is swapped in as the copy holds it.
//
// A database whose embeddings is partitioned already costs one catalog read. Before it copies, it
// gives every project that has none its partition (embeddings_ensure_project_partition): a
// project written with its table's triggers off has none, and the copy of its rows would be
// refused. The copy resumes from its cursor (embeddings_partitioned_copy), so a process killed
// mid-copy costs the next try the batch it was in. It logs as the labelling does: a line that it
// began and where from, one every embeddingProjectKeyProgressEvery while it copies, one when the
// copy finished, one when the counts disagree, and one when embeddings is swapped, with how long
// the swap held the two tables.
func (s *Store) PartitionEmbeddings(ctx context.Context, buildCommit string) error {
	partitioned, err := s.embeddingsPartitioned(ctx)
	if err != nil {
		return err
	}
	if partitioned {
		slog.Info("dispatch: embeddings is partitioned by project already: there is nothing to copy")
		return nil
	}
	var fromKind, fromID string
	var done bool
	if err := s.Pool.QueryRow(ctx, "select kind, id, done from embeddings_partitioned_copy").
		Scan(&fromKind, &fromID, &done); err != nil {
		return fmt.Errorf("read the embeddings partition copy's progress: %w", err)
	}
	if _, err := s.Pool.Exec(ctx, `select embeddings_ensure_project_partition(key) from projects order by key`); err != nil {
		return fmt.Errorf("give every project its partition of embeddings before the copy: %w", err)
	}
	for attempt := 1; ; attempt++ {
		if !done {
			if err := s.copyEmbeddings(ctx, fromKind, fromID); err != nil {
				return err
			}
		}
		others, err := embeddingsSwapOtherCommits(ctx, s.Pool, buildCommit)
		if err != nil {
			return err
		}
		if len(others) > 0 {
			return errEmbeddingsSwapWaitsFor(buildCommit, others)
		}
		var stored, copied int64
		if err := s.Pool.QueryRow(ctx, embeddingsCopyCountsQuery).Scan(&stored, &copied); err != nil {
			return fmt.Errorf("count embeddings and its partitioned copy: %w", err)
		}
		if stored == copied {
			break
		}
		slog.Warn("dispatch: the embeddings partition copy does not hold the rows embeddings holds, so it is emptied and copied again: a write reached one table and not the other, as a write made as a replica, with the mirror's trigger disabled or by hand does",
			"embeddings_rows", stored, "copy_rows", copied, "attempt", attempt)
		if err := s.resetEmbeddingsCopy(ctx); err != nil {
			return err
		}
		if attempt == 2 {
			return fmt.Errorf("the embeddings partition copy disagreed with embeddings (%d rows against %d) twice in a row: it is emptied, and the next try copies it again", copied, stored)
		}
		fromKind, fromID, done = "", "", false
	}
	return s.swapInEmbeddingPartitions(ctx, buildCommit)
}

// copyEmbeddings copies every row of embeddings above the cursor (fromKind, fromID) into
// embeddings_partitioned, batch by batch (copyEmbeddingsBatch), and logs that it began, its
// progress and that it finished.
func (s *Store) copyEmbeddings(ctx context.Context, fromKind, fromID string) error {
	started := time.Now()
	slog.Info("dispatch: the embeddings partition copy began",
		"from_kind", fromKind, "from_id", fromID, "batch", EmbeddingPartitionCopyBatch)
	// rows is every row the copy read, and inserted the rows of them the mirror had not carried
	// over already: on a database that took writes while the copy ran, inserted is smaller, and
	// the two together say how much of the table the copy itself moved.
	var rows, inserted, batches int64
	said := started
	for done := false; !done; {
		read, wrote, finished, err := s.copyEmbeddingsBatch(ctx)
		if err != nil {
			return err
		}
		done = finished
		rows += read
		inserted += wrote
		batches++
		if now := time.Now(); !done && now.Sub(said) >= embeddingProjectKeyProgressEvery {
			said = now
			slog.Info("dispatch: the embeddings partition copy is copying",
				"rows", rows, "inserted", inserted, "batches", batches, "elapsed", now.Sub(started))
		}
	}
	slog.Info("dispatch: the embeddings partition copy finished",
		"rows", rows, "inserted", inserted, "batches", batches, "duration", time.Since(started))
	return nil
}

// resetEmbeddingsCopy empties embeddings_partitioned, every partition of it, and moves the copy's
// cursor back to the start, in one transaction waiting at most pgmigrate.LockTimeout for its
// locks. TRUNCATE takes ACCESS EXCLUSIVE on embeddings_partitioned alone, which no read of
// embeddings touches before the swap, so a reset holds no reader; a write of embeddings waits
// for it, since the mirror writes the copy in the writer's transaction, for as long as the
// truncate takes. The mirror goes on writing every later write into the emptied table, and the
// copy that follows meets those rows and leaves them as the mirror wrote them.
func (s *Store) resetEmbeddingsCopy(ctx context.Context) error {
	ctx = WithTransactionTracking(ctx)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the embeddings partition copy's reset: %w", err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range []string{
		fmt.Sprintf("set local lock_timeout = %d", pgmigrate.LockTimeout.Milliseconds()),
		"truncate embeddings_partitioned",
		"update embeddings_partitioned_copy set kind = '', id = '', done = false",
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("reset the embeddings partition copy (%s): %w", statement, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the embeddings partition copy's reset: %w", err)
	}
	return nil
}

// copyEmbeddingsBatch copies the next EmbeddingPartitionCopyBatch rows of embeddings above the
// copy's cursor, in embeddings' primary key order, and moves the cursor past them, in one
// transaction, answering how many rows it read, how many of them it inserted (a row the mirror
// carried over already is left as the mirror wrote it), and whether it took no further row, which
// is the copy finished. The cursor is read and locked by a statement of its own, so the batch is
// planned against its values as the labelling's is (fillEmbeddingProjectKeysCursorQuery).
func (s *Store) copyEmbeddingsBatch(ctx context.Context) (read, inserted int64, finished bool, err error) {
	ctx = WithTransactionTracking(ctx)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, false, fmt.Errorf("begin an embeddings partition copy batch: %w", err)
	}
	defer tx.Rollback(ctx)
	var fromKind, fromID string
	if err := tx.QueryRow(ctx, `select kind, id from embeddings_partitioned_copy for update`).
		Scan(&fromKind, &fromID); err != nil {
		return 0, 0, false, fmt.Errorf("take the embeddings partition copy's cursor: %w", err)
	}
	if err := tx.QueryRow(ctx, copyEmbeddingsBatchQuery, fromKind, fromID, EmbeddingPartitionCopyBatch).
		Scan(&read, &inserted, &finished); err != nil {
		return 0, 0, false, fmt.Errorf("copy a batch of embeddings rows into their partitions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, false, fmt.Errorf("commit an embeddings partition copy batch: %w", err)
	}
	return read, inserted, finished, nil
}

// copyEmbeddingsBatchQuery is one batch of the copy. $1 and $2 are the cursor, $3 the batch size.
//
// The batch's rows are read FOR SHARE, and that lock is what makes the copy and
// 20261120105818's mirror one ordering of each row's writes: a write of a row the batch holds
// waits for the batch's commit, and then its trigger meets the copied row and writes over it,
// where without the lock the write could land between the batch's read and its insert and be
// overwritten by the older copy, or a delete could run there and the copy bring the row back.
// A row written before the batch read it is in the mirror's table already, and the insert leaves
// it as the mirror wrote it (do nothing).
const copyEmbeddingsBatchQuery = `
	with batch as materialized (
		select e.kind, e.id, e.text_snapshot, e.content_hash, e.embedded_hash, e.model, e.embedding,
		       e.attempt_count, e.confirmed_failures, e.next_attempt_at, e.dead, e.embedded_at,
		       e.created_at, e.pending_since, e.dead_model, e.covers, e.project_key
		  from embeddings e
		 where (e.kind, e.id) > ($1::text, $2::text)
		 order by e.kind, e.id
		 limit $3
		   for share
	), last as (
		select b.kind, b.id from batch b order by b.kind desc, b.id desc limit 1
	), copied as (
		insert into embeddings_partitioned (kind, id, text_snapshot, content_hash, embedded_hash, model,
		                                    embedding, attempt_count, confirmed_failures, next_attempt_at,
		                                    dead, embedded_at, created_at, pending_since, dead_model,
		                                    covers, project_key)
		select kind, id, text_snapshot, content_hash, embedded_hash, model, embedding, attempt_count,
		       confirmed_failures, next_attempt_at, dead, embedded_at, created_at, pending_since,
		       dead_model, covers, project_key
		  from batch
		    on conflict (kind, id, project_key) do nothing
		returning 1
	), moved as (
		update embeddings_partitioned_copy
		   set kind = coalesce((select kind from last), embeddings_partitioned_copy.kind),
		       id = coalesce((select id from last), embeddings_partitioned_copy.id),
		       done = not exists (select 1 from last)
		returning done
	)
	select (select count(*) from batch), (select count(*) from copied), (select done from moved)`

// swapInEmbeddingPartitions runs embeddings_swap_in_partitioned in a transaction of its own,
// waiting for the two tables' locks at most pgmigrate.LockTimeout, as a migration waits for its
// own: while the swap waits for embeddings' ACCESS EXCLUSIVE lock, every read and write of the
// table queues behind it, and a bound that long is the one a migration already makes them pay. It
// takes the two locks first, in the swap function's own order, and reads the fleet again under
// them (embeddingsSwapOtherCommitsQuery): a build of another commit whose dashboard publish or
// registration committed since the read before the copy's count holds the swap, which then
// changes nothing and answers ErrEmbeddingsSwapWaits. A build whose publish commits after this
// read is the rollback past the swap ErrEmbeddingsSwapWaits describes.
//
// Once it has committed, it analyzes embeddings itself (analyzeSwappedEmbeddings). Autovacuum
// analyzes each partition as its rows change, but never a partitioned table itself, so without
// this the planner would cost every statement over the parent from no statistics of it for good,
// where the old table had its own. One that fails is logged and the swap stands, since a plan
// without the parent's statistics is slower, not wrong.
func (s *Store) swapInEmbeddingPartitions(ctx context.Context, buildCommit string) error {
	ctx = WithTransactionTracking(ctx)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the embeddings partition swap: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, fmt.Sprintf("set local lock_timeout = %d", pgmigrate.LockTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("bound the embeddings partition swap's lock wait: %w", err)
	}
	started := time.Now()
	if _, err := tx.Exec(ctx, "lock table embeddings, embeddings_partitioned in access exclusive mode"); err != nil {
		return fmt.Errorf("take the embeddings partition swap's locks: %w", err)
	}
	others, err := embeddingsSwapOtherCommits(ctx, tx, buildCommit)
	if err != nil {
		return err
	}
	if len(others) > 0 {
		return errEmbeddingsSwapWaitsFor(buildCommit, others)
	}
	if _, err := tx.Exec(ctx, "select embeddings_swap_in_partitioned()"); err != nil {
		return fmt.Errorf("swap the partitioned embeddings table in: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the embeddings partition swap: %w", err)
	}
	slog.Info("dispatch: embeddings is partitioned by project: the copy is swapped in", "held", time.Since(started))
	analyzed := time.Now()
	if err := analyzeSwappedEmbeddings(ctx, s.Pool); err != nil {
		slog.Warn("dispatch: analyzing the swapped-in embeddings failed; the planner reads its partitions' statistics alone until an ANALYZE of embeddings runs", "error", err)
		return nil
	}
	slog.Info("dispatch: embeddings is analyzed after its swap", "duration", time.Since(analyzed))
	return nil
}

// analyzeSwappedEmbeddings analyzes embeddings once it is partitioned: ANALYZE ONLY embeddings,
// which reads one sample of the parent's rows and leaves each partition's own statistics to
// autovacuum, and where the server takes no ONLY, ANALYZE embeddings, which reads the same sample
// and each partition's too. ONLY is PostgreSQL 18 syntax: 16 and 17 refuse it (42601,
// syntax_error), and a Cloud SQL instance may still run 16 (infra/README.md, databaseVersion). Both
// take SHARE UPDATE EXCLUSIVE on embeddings, which no read or write of it waits for but which a
// project's partition attach takes too (embeddings_add_project_partition), so a project created
// while it runs waits for it.
func analyzeSwappedEmbeddings(ctx context.Context, exec interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}) error {
	_, err := exec.Exec(ctx, "analyze only embeddings")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42601" {
		return err
	}
	if _, err := exec.Exec(ctx, "analyze embeddings"); err != nil {
		return fmt.Errorf("analyze embeddings, after ANALYZE ONLY was refused as a syntax error (a server before PostgreSQL 18): %w", err)
	}
	return nil
}
