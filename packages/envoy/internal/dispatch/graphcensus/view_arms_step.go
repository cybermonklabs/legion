package graphcensus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ViewArmsStepLine opens every line the boot's step over graph_edges' four old arms logs
// (RunViewArmsStep), whatever it found: it deleted them, found none standing, left them in place
// and says why, or failed. A reader that waits for the step to have run waits for one such line.
const ViewArmsStepLine = "dispatch: the graph_edges arms step"

const (
	// ViewArmsDeletedLine is the line the step logs when it deleted the four arms.
	ViewArmsDeletedLine = ViewArmsStepLine + " deleted the four old arms"
	// ViewArmsNothingToDoLine is the line the step logs when graph_edges holds none of them.
	ViewArmsNothingToDoLine = ViewArmsStepLine + " has nothing to do: graph_edges holds none of the four old arms"
	// ViewArmsKeptLine opens each line the step logs when it leaves them in place, with its reason
	// after a colon.
	ViewArmsKeptLine = ViewArmsStepLine + " leaves the four old arms"
)

// viewArmsKinds are the kinds the four old arms answer, which an announcement lists once its build
// writes them as tuples (refs.TupleKinds): a block's in_document, which the view renders as its
// part_of its document, a link's mentions, and a comment's anchored_to and owned_by. A block's
// pointer is a link of a kind the installed block schema names, which only a build that wrote
// pointers as tuples lists, and no build lists a pointer kind without listing mentions.
var viewArmsKinds = []string{refs.TupleInDocument, graphstore.TupleMentions, graphstore.TupleAnchoredTo, graphstore.TupleOwnedBy}

// viewArmsCensusKinds are the census's kinds whose row side is a row of one of the three tables
// the four arms read and whose node side is the tuples of the kinds the arms answer (blockProjections,
// commentProjections, decisionProjections): a block's membership and place, its links but a
// decision's ties, each of the three ties, and a comment's relations. A tuple of any of these that
// no row names is a node-side difference, which is what "every current tuple of their kinds is
// named by a row that carries its node" means.
var viewArmsCensusKinds = []string{"block", "block_ref", refs.TupleShaped, refs.TupleFromAsk, graphstore.TupleDecidedBy, graphstore.KindComment}

// viewArmsUnmarked counts, for each of the three tables the four arms read, the rows that name no
// node: the rows the arms answer. A row past the store's address bound is one of them for as long
// as it stands, since no writer gives it a node, so while one stands the step keeps the arms, which
// answer it. The step reads them when it runs and never again: once it has deleted the arms, a row
// written after it that names no node - which only an address past the bound still makes, a block
// of a document whose ref key leaves no room for the block id, a link to a far end past the bound,
// a comment on such a document - has no edge in graph_edges and no count in refs.BacklinkCounts.
// The store census counts each such row as its kind's addresses_past_bound (StoreReport.Write).
const viewArmsUnmarked = `
	select (select count(*) from document_blocks where node_kind is null),
	       (select count(*) from block_refs where node_kind is null),
	       (select count(*) from comments where node_kind is null)`

// viewArmsJoin matches, in the definition of graph_edges with its whitespace folded to single
// spaces, a join the tuple arm makes to the row of a table whose arm is deleted: the table the
// probe reads, under whatever alias the catalogue gives it, with the test that the row names the
// node the tuple comes from. The arms over those tables test `node_kind IS NULL` instead.
var viewArmsJoin = regexp.MustCompile(`FROM (document_blocks|block_refs|comments) \w+ WHERE \w+\.node_kind = edge\.from_kind`)

// RunViewArmsStep is the step of every boot that deletes the four arms of graph_edges that the
// graph-store spec moved (`r-view-end` of that spec, R9 of the issue-tree spec): the arm over
// document_blocks, which answers a block's part_of its document, the arm over block_refs, which
// answers a block's links, and the two arms over comments, which answer its anchored_to and its
// owned_by, each over the rows that name no node, with the tuple arm's join to those three tables.
// The boot runs it once its run of the backfill has logged that it is done, which writes the rows
// that name no node, on the backfill's own goroutine and before RunTypedBlockSupertagsStep, and never
// at a boot whose run logged no done line.
//
// It takes viewEndStepLock, which RunTypedBlockSupertagsStep takes as well, on a connection of its
// own, and tests its condition again under it: it does nothing, and logs that it has nothing to do,
// once graph_edges' definition holds none of the arms or of the joins, so two processes that boot
// together never restate the view at once and the second finds the first's work done. It deletes
// nothing unless the newest server_build_announcements row that names a commit other than commit,
// this process's own, lists every kind the arms answer (viewArmsKinds) - every process of the fleet
// before this one writes those kinds as tuples, so a rollback reaches a release that retracts a
// tuple when it deletes a row - or fresh says this boot created the database from empty, which has
// no earlier release to roll back to. It then proves, in the transaction that holds the lock, that
// every row of the three tables names its node and that the census of the kinds the arms answer
// reads no difference in either direction (viewArmsCensusKinds), and only then states the view again
// from store.GraphEdgesView of the newest arm set, store.GraphEdgesFourArmsDeleted, and commits.
// A failed proof leaves the arms in place, logs each difference the census read, and the server
// serves on; a later boot tries again. Its error is a statement's that failed, which the caller
// logs. The proof holds of the rows at the moment the step commits: a row written after it that
// names no node, which only an address past the store's bound makes, has no edge from then on
// (viewArmsUnmarked).
//
// It answers true where graph_edges holds none of the four arms afterwards, whether this call
// deleted them or found them gone, and false where it left them: the caller then tells the counts
// that read the view's arms (refs.SetFourArmsDeleted) to read none of them either, beside the
// reads of the view's definition every process makes before it serves (ViewArmsGone) and after
// (FollowViewArms).
func RunViewArmsStep(ctx context.Context, database *store.Store, commit string, fresh bool, logger *slog.Logger) (gone bool, err error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin the step over graph_edges' four old arms: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, viewEndStepLock); err != nil {
		return false, fmt.Errorf("take the lock of the steps over graph_edges' reads: %w", err)
	}
	definition, err := viewDefinition(ctx, tx)
	if err != nil {
		return false, err
	}
	standing := ViewArmsStanding(definition)
	if len(standing) == 0 {
		logger.Info(ViewArmsNothingToDoLine)
		return true, nil
	}
	if !fresh {
		var announced string
		var listed []string
		err := tx.QueryRow(ctx, `
			select a.commit, a.tuple_kinds from server_build_announcements a
			where a.commit <> $1 order by a.seq desc limit 1
		`, commit).Scan(&announced, &listed)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			logger.Info(ViewArmsKeptLine+": no build other than this one has announced itself, so none is known to write the kinds of the arms as tuples",
				"standing", standing)
			return false, nil
		case err != nil:
			return false, fmt.Errorf("read the newest announcement of another build: %w", err)
		}
		if missing := slices.DeleteFunc(slices.Clone(viewArmsKinds), func(kind string) bool { return slices.Contains(listed, kind) }); len(missing) > 0 {
			logger.Info(ViewArmsKeptLine+": the newest announcement of another build does not list every kind they answer",
				"standing", standing, "announced_commit", announced, "missing", missing)
			return false, nil
		}
	}
	var blocks, links, comments int64
	if err := tx.QueryRow(ctx, viewArmsUnmarked).Scan(&blocks, &links, &comments); err != nil {
		return false, fmt.Errorf("read the rows the four old arms answer: %w", err)
	}
	if blocks > 0 || links > 0 || comments > 0 {
		logger.Warn(ViewArmsKeptLine+": rows of document_blocks, block_refs or comments name no node, which the arms answer",
			"standing", standing, "document_blocks", blocks, "block_refs", links, "comments", comments)
		return false, nil
	}
	if _, err := tx.Exec(ctx, `set local jit = off`); err != nil {
		return false, fmt.Errorf("turn JIT off for the census of the kinds the four old arms answer: %w", err)
	}
	differing := false
	for _, name := range viewArmsCensusKinds {
		held, found := projectionNamed(name)
		if !found {
			return false, fmt.Errorf("the census holds no kind %s", name)
		}
		kind, err := censusOfKind(ctx, tx, held, nil)
		if err != nil {
			return false, err
		}
		if kind.RowDifferences+kind.NodeDifferences == 0 {
			continue
		}
		differing = true
		logger.Warn(ViewArmsKeptLine+": the census of a kind they answer reads differences",
			"kind", kind.Kind, "row_differences", kind.RowDifferences, "node_differences", kind.NodeDifferences)
		for _, difference := range append(kind.ReportedRows, kind.ReportedNodes...) {
			logger.Warn(ViewArmsStepLine+" found a difference between a row and the store",
				"kind", kind.Kind, "at", difference.At, "cause", difference.Cause)
		}
	}
	if differing {
		return false, nil
	}
	if _, err := tx.Exec(ctx, store.GraphEdgesView(store.GraphEdgesFourArmsDeleted)); err != nil {
		return false, fmt.Errorf("state graph_edges without the four old arms: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit the step over graph_edges' four old arms: %w", err)
	}
	logger.Info(ViewArmsDeletedLine, "was_standing", standing, "kinds_checked", viewArmsCensusKinds)
	return true, nil
}

// projectionNamed is the census's kind of that name.
func projectionNamed(name string) (projection, bool) {
	for _, held := range projections {
		if held.name == name {
			return held, true
		}
	}
	return projection{}, false
}

// rowQueryer is what reads one row: a pool or a transaction.
type rowQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// viewDefinition is graph_edges as pg_get_viewdef prints it in its pretty form, which
// ViewArmsStanding reads.
func viewDefinition(ctx context.Context, q rowQueryer) (string, error) {
	var definition string
	if err := q.QueryRow(ctx, `select pg_get_viewdef('graph_edges'::regclass, true)`).Scan(&definition); err != nil {
		return "", fmt.Errorf("read the definition of graph_edges: %w", err)
	}
	return definition, nil
}

// ViewArmsGone reads whether graph_edges, as the database holds it now, holds none of the four old
// arms and none of the tuple arm's joins to their tables (ViewArmsStanding answers none). A process
// reads it before it serves and sets refs.SetFourArmsDeleted from it, so a process that boots after
// another process's step deleted the arms counts as the view answers from its first request.
func ViewArmsGone(ctx context.Context, q rowQueryer) (bool, error) {
	definition, err := viewDefinition(ctx, q)
	if err != nil {
		return false, err
	}
	return len(ViewArmsStanding(definition)) == 0, nil
}

// ViewArmsFollowInterval is how often a process reads again whether graph_edges holds the four old
// arms (FollowViewArms).
const ViewArmsFollowInterval = time.Minute

// FollowViewArms keeps refs.SetFourArmsDeleted to the view the database holds, for the life of the
// process: about every ViewArmsFollowInterval (duty.RunJittered) it reads the view's definition
// again (ViewArmsGone) and sets the flag from it, whichever way the view moved - another process's
// step deleted the arms, or a migration of another build stated the view again with them. A read
// that fails is logged and changes nothing; the next one tries again.
//
// Between a change and this process's next read its counts read the arm set the view had: a process
// that last read the four arms standing, after another process's step deleted them, counts each of
// their kinds from its table's rows that name no node and from the tuples joined to the row that
// carries their node, as the view answered before the deletion. Those are the view's answers now
// but for one class: no row inside the store's address bound names no node (the step proved it,
// and every writer of this build names the node of a row inside the bound), and every current
// tuple of those kinds is named by a row that carries its node (the step's census, which the
// writers keep), so the table arms add nothing and the join drops nothing; a row past the bound
// written in that interval, which the view no longer answers (viewArmsUnmarked), is counted by
// that process until its next read, at most one interval later.
func FollowViewArms(ctx context.Context, pool *store.Pool, logger *slog.Logger) {
	duty.RunJittered(ctx, ViewArmsFollowInterval, func() bool { return false }, func(ctx context.Context, _ time.Time) {
		gone, err := ViewArmsGone(ctx, pool)
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("dispatch: read again whether graph_edges holds its four old arms", "error", err)
			}
			return
		}
		refs.SetFourArmsDeleted(gone)
	})
}

// ViewArmsStanding names each of the four old arms that definition, graph_edges as pg_get_viewdef
// prints it in its pretty form, still holds - `document_blocks`, `block_refs`, `comments_anchored_to`
// and `comments_owned_by` - and each join the tuple arm makes to the rows of those tables for the
// kinds the arms answer, `join_document_blocks`, `join_block_refs` and `join_comments`. The form
// puts each top-level UNION ALL on a line of its own, opens each arm's own FROM at three spaces and
// prints a constant column as `'<value>'::text AS <column>`, and an arm that opens a sub-select
// names none of the three tables first. The arms come in the order the view states them and the
// joins after them, and the answer is empty where the view holds none of them.
func ViewArmsStanding(definition string) []string {
	var standing []string
	add := func(name string) {
		if !slices.Contains(standing, name) {
			standing = append(standing, name)
		}
	}
	for _, arm := range strings.Split(strings.TrimSuffix(strings.TrimRight(definition, "\n"), ";"), "\nUNION ALL\n") {
		relation := ""
		for _, line := range strings.Split(arm, "\n") {
			if rest, found := strings.CutPrefix(line, "   FROM "); found {
				if fields := strings.Fields(rest); len(fields) > 0 {
					relation = fields[0]
				}
				break
			}
		}
		switch relation {
		case "document_blocks", "block_refs":
			add(relation)
		case "comments":
			for _, kind := range []string{graphstore.TupleAnchoredTo, graphstore.TupleOwnedBy} {
				if strings.Contains(arm, "'"+kind+"'::text AS kind") {
					add("comments_" + kind)
				}
			}
		}
	}
	for _, join := range viewArmsJoin.FindAllStringSubmatch(strings.Join(strings.Fields(definition), " "), -1) {
		add("join_" + join[1])
	}
	return standing
}
