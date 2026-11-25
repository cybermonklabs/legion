package startup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A release that moves a relation into the node and tuple store keeps, for one release, the stored
// objects a build of the release before still reads or writes: two builds serve one database
// during a rolling release and after a rollback (cmd/dispatch/README.md, "Two builds at once").
// The release after deletes them, and it deletes them in a step of every boot rather than in a
// migration applied once (Retire): a migration runs whatever build the fleet would roll back to,
// while the step reads, at each boot, whether the build a rollback reaches still needs the
// objects, and deletes them at the first boot where it does not. Retirements is that step's one
// list, and every such deletion is an entry of it.

// Retirement is one set of stored objects a release before this one kept for one release, which
// the boot deletes once two things hold (Retire). The first is the release's own: the newest
// server_build_announcements row naming a commit other than the booting process's own lists every
// relation kind of Kinds, since that row names the build a rollback reaches, or this boot created
// the database from empty, so no earlier release holds rows a rollback could reach. The second is
// the entry's own: once its Backfill has run, Holds answers nothing.
type Retirement struct {
	// Name names the objects in every line the step logs about them.
	Name string
	// Kinds are the relation kinds of the first release no build of which needs the objects: none
	// reads them, and none writes a row that only they answer. They are the kinds that release
	// moved into the store, which every build of it and after it announces (refs.TupleKinds). That
	// is the release that kept the objects where its own build needs them no more, and a later one
	// where a build of it still does, as the decisions document's (decisionsDocumentRetirement) and
	// the issue tree's arms of graph_edges (IssueTreeArmsRetirement). It is one of the lists below,
	// one per release.
	Kinds []string
	// Stands reports whether the objects still stand. An entry whose objects are gone does nothing
	// more, so a boot after the one that deleted them reads one probe and changes nothing.
	Stands func(ctx context.Context, q queryer) (bool, error)
	// Backfill, where set, brings in what a build of an earlier release wrote that the objects
	// still answer, before Holds is read: the backfill the release that kept them built, called as
	// it was built, batches and commits included. It runs only where the announcement the first
	// condition reads holds, never over a database this boot created from empty, which holds no
	// row an earlier release wrote, and one that fails keeps the objects for the next boot.
	Backfill func(ctx context.Context, database *store.Store) error
	// Holds answers each thing that still needs the objects, named for the log, and nothing where
	// they may go. It is read on the shared pool first, and again in the deleting transaction.
	Holds func(ctx context.Context, q queryer) ([]Holder, error)
	// Delete deletes the objects, in the order its statements need, in the step's transaction:
	// one that fails rolls back whole and leaves every object standing.
	Delete func(ctx context.Context, tx pgx.Tx) error
}

// AsksKinds are the relation kinds dispatch://INFRA-1150's release moved into the store, the
// release that moved decisions, readings, asks and their answers: an ask's anchored_to, owned_by
// and followed_by, and a decision block's from_ask and decided_by. Every build of that release and
// after it announces them, and a build of a release before it announces none of followed_by,
// from_ask and decided_by. Every entry that deletes what that release kept, and that no build of
// it needs, reads this one list.
var AsksKinds = []string{
	graphstore.TupleAnchoredTo, graphstore.TupleOwnedBy, graphstore.TupleFollowedBy,
	refs.TupleFromAsk, graphstore.TupleDecidedBy,
}

// IssueTreeKinds are the relation kinds dispatch://INFRA-1253's release moved into the store, the
// issue-room release that moved issues and their documents - an issue's child_of, needs,
// duplicate_of and filed_from, a document's attached_to, and a component's depends_on and an
// issue's affects - and the capability issue_room, which no relation kind spells and which only a
// build of that release or a later one announces. A document's mentions, which that release
// states out of the document's node too, is no member: dispatch://INFRA-1149's build announces it
// already, for a block's mentions, so it tells no build of that release from one before it. Every
// entry that deletes what that release kept, and that no build of it needs, reads this one list.
var IssueTreeKinds = []string{
	graphstore.TupleChildOf, graphstore.TupleNeeds, graphstore.TupleDuplicateOf, graphstore.TupleFiledFrom,
	refs.TupleAttachedTo, graphstore.TupleDependsOn, graphstore.TupleAffects,
	processes.CapabilityIssueRoom,
}

// Holder is one thing that still needs a retirement's objects, as the attributes of the line the
// step logs for it.
type Holder []slog.Attr

// queryer is what a retirement reads through: the shared pool before the transaction opens, and
// the transaction itself.
type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// Retirements are the deletions the boot runs (Retire). Each entry's own file declares it and
// registers it (registerRetirement), so adding an entry edits no shared list.
//
// They run in the order of their names, which registerRetirement keeps whatever order the
// package's files initialise in: the order is fixed by the names alone, so every boot of a build
// runs its entries in one order. No entry depends on another having run: each reads the
// announcements and its own objects alone, deletes objects no other entry names, and runs in a
// transaction of its own, so one that keeps its objects or fails leaves every other entry's run as
// it would have been.
var Retirements []Retirement

// registerRetirement adds retirement to Retirements at the place its name sorts to, and answers
// it, so an entry's own file registers it with one declaration:
//
//	var _ = registerRetirement(entry)
//
// Two entries of one name are a mistake of the build, which it refuses before main runs, since
// the log names an entry by its name alone.
func registerRetirement(retirement Retirement) Retirement {
	at, taken := slices.BinarySearchFunc(Retirements, retirement.Name, func(registered Retirement, name string) int {
		return strings.Compare(registered.Name, name)
	})
	if taken {
		panic("startup: two retirements are named " + retirement.Name)
	}
	Retirements = slices.Insert(Retirements, at, retirement)
	return retirement
}

// retireLock is the transaction advisory lock each retirement's transaction takes ("DRETIRES"), so
// two processes booting at once delete one entry's objects once: the second waits for the first's
// commit and then finds them gone. It is a single 64-bit key, apart from every other single-key
// advisory lock of the server for the reason processes' registryLock gives.
const retireLock int64 = 0x4452455449524553

// retireLockTimeout bounds each lock the transaction waits for, the tables its statements alter
// among them: a table another transaction holds fails the retirement, which logs it and leaves the
// objects for the next boot, rather than keep the process from serving.
const retireLockTimeout = "5s"

// rollbackAnnouncement is the newest server_build_announcements row naming a commit other than
// the booting process's own, the build a rollback reaches: its seq, its commit, and the relation
// kinds it lists, none for a build of a release before the column.
type rollbackAnnouncement struct {
	seq    int64
	commit string
	kinds  []string
}

// Retire runs every retirement of retirements over pool, for a process serving commit, whose
// boot created the database from empty where fromEmpty (store.Store.CreatedFromEmpty). It runs at
// every boot, after the migrations and before the process serves, and it never stops the boot: a
// retirement that does not delete, and one whose backfill or transaction fails, leaves every one
// of its objects in place and logs why, naming the announcement whose kinds fell short or each
// thing Holds answered, and the next boot tries again. A retirement whose objects are gone does
// nothing.
func Retire(ctx context.Context, pool *store.Pool, commit string, fromEmpty bool, logger *slog.Logger, retirements []Retirement) {
	if logger == nil {
		logger = slog.Default()
	}
	var rollback *rollbackAnnouncement
	if !fromEmpty {
		var err error
		if rollback, err = newestOtherAnnouncement(ctx, pool, commit); err != nil {
			logger.Error("dispatch: the boot's retirements read no announcement and kept every object", "error", err)
			return
		}
	}
	for _, retirement := range retirements {
		retire(ctx, pool, commit, fromEmpty, rollback, logger.With("retirement", retirement.Name), retirement)
	}
}

// retire runs one retirement (Retire): its guard, then its backfill, then what holds its objects,
// and only where nothing does, its transaction. A retirement whose guard does not hold reads
// nothing more, since the build a rollback reaches may still write what Holds would name.
func retire(ctx context.Context, pool *store.Pool, commit string, fromEmpty bool, rollback *rollbackAnnouncement, logger *slog.Logger, retirement Retirement) {
	stands, err := retirement.Stands(ctx, pool)
	if err != nil {
		logger.Error("dispatch: the boot's retirement could not read whether its objects stand, and kept them", "error", err)
		return
	}
	if !stands {
		return
	}
	from := fromAttrs(commit, fromEmpty, rollback)
	if !fromEmpty {
		missing := missingKinds(rollback, retirement.Kinds)
		switch {
		case rollback == nil:
			logger.Warn("dispatch: the boot's retirement kept its objects: no announcement names a build other than this one, so the build a rollback reaches is not known",
				from...)
			return
		case len(missing) > 0:
			logger.Warn("dispatch: the boot's retirement kept its objects: the build a rollback reaches does not list every relation kind of the release that kept them",
				append(from, "missing", missing)...)
			return
		}
	}
	// A database this boot created from empty holds no row an earlier release wrote, so there is
	// nothing for the backfill to bring in.
	if retirement.Backfill != nil && !fromEmpty {
		if err := retirement.Backfill(ctx, &store.Store{Pool: pool}); err != nil {
			logger.Error("dispatch: the boot's retirement's backfill failed; its objects stay for the next boot", append(from, "error", err)...)
			return
		}
	}
	holders, err := retirement.Holds(ctx, pool)
	if err != nil {
		logger.Error("dispatch: the boot's retirement could not read what holds its objects, and kept them", append(from, "error", err)...)
		return
	}
	logHolders(ctx, logger, holders)
	if len(holders) > 0 {
		return
	}
	deleted, holders, err := retireInTx(ctx, pool, retirement)
	switch {
	case err != nil:
		logger.Error("dispatch: the boot's retirement failed and changed nothing; its objects stay for the next boot", append(from, "error", err)...)
	case len(holders) > 0:
		logHolders(ctx, logger, holders)
	case deleted:
		logger.Info("dispatch: the boot's retirement deleted its objects", from...)
	}
}

// retireInTx deletes retirement's objects in one transaction, under retireLock and with every lock
// wait bounded (retireLockTimeout). It reads again under the lock whether the objects stand, and
// what holds them, which it answers in place of deleting: a process that booted beside this one may
// have deleted them, or something may have come to hold them, since the reads on the shared pool.
func retireInTx(ctx context.Context, pool *store.Pool, retirement Retirement) (bool, []Holder, error) {
	var deleted bool
	var holders []Holder
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select set_config('lock_timeout', $1, true)`, retireLockTimeout); err != nil {
			return fmt.Errorf("bound the lock waits: %w", err)
		}
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, retireLock); err != nil {
			return fmt.Errorf("take the retirement lock: %w", err)
		}
		stands, err := retirement.Stands(ctx, tx)
		if err != nil || !stands {
			return err
		}
		if holders, err = retirement.Holds(ctx, tx); err != nil || len(holders) > 0 {
			return err
		}
		if err := retirement.Delete(ctx, tx); err != nil {
			return err
		}
		deleted = true
		return nil
	})
	if err != nil {
		return false, nil, err
	}
	return deleted, holders, nil
}

// newestOtherAnnouncement reads the newest server_build_announcements row naming a commit other
// than commit, nil where there is none: a database only this build has announced, and one whose
// fleet never served a stamped commit, which is never announced.
func newestOtherAnnouncement(ctx context.Context, pool *store.Pool, commit string) (*rollbackAnnouncement, error) {
	var found rollbackAnnouncement
	err := pool.QueryRow(ctx, `
		select seq, commit, coalesce(tuple_kinds, '{}') from server_build_announcements
		where commit <> $1 order by seq desc limit 1
	`, commit).Scan(&found.seq, &found.commit, &found.kinds)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the newest announcement of another build: %w", err)
	}
	return &found, nil
}

// missingKinds is each kind of kinds that rollback does not list, all of them where there is no
// announcement.
func missingKinds(rollback *rollbackAnnouncement, kinds []string) []string {
	var missing []string
	for _, kind := range kinds {
		if rollback == nil || !slices.Contains(rollback.kinds, kind) {
			missing = append(missing, kind)
		}
	}
	return missing
}

// fromAttrs names what a retirement's guard read for the log: a database this boot created from
// empty, or the announcement of the build a rollback reaches, with the kinds it lists.
func fromAttrs(commit string, fromEmpty bool, rollback *rollbackAnnouncement) []any {
	switch {
	case fromEmpty:
		return []any{"created_from_empty", true, "commit", commit}
	case rollback == nil:
		return []any{"commit", commit}
	default:
		return []any{"commit", commit, "announcement_seq", rollback.seq, "announcement_commit", rollback.commit,
			"announcement_kinds", rollback.kinds}
	}
}

// logHolders logs one line for each thing that still holds a retirement's objects.
func logHolders(ctx context.Context, logger *slog.Logger, holders []Holder) {
	for _, holder := range holders {
		logger.LogAttrs(ctx, slog.LevelWarn, "dispatch: the boot's retirement kept its objects: something still holds them", holder...)
	}
}
