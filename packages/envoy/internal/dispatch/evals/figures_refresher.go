package evals

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
)

// The refresher's part of the stored figures (figures.go): the change log's read that folds again
// the figures of the days whose rows a write left unfolded, the figures walk, which builds a
// project's figures whole once and then each new end day, and the refold of each cut a move of a
// default repository marked stale, which every pass runs batch after batch, each batch a short
// transaction of its own, until its mark is cleared.
//
// Every build that holds this code writes the figures, in the transaction of each write of day
// rows, and keeps them as the day rows are kept (eval_days.built_by, migration 20261117100006): a
// read serves a stored figure whichever build folded it (eval_range_figures.built_by). A project's
// figures are first folded whole by its figures walk (eval_days_walks, family `figures`), one walk
// whichever build's process runs it, and are built once that first walk is done
// (eval_figures_built), from which a read of an offered range folds the stored figures
// (figuresThrough); the walk then folds each end day a new UTC day adds. A release, a lost cursor
// of the figures or of the refresher refold every built figure aside, in a rebuild (rebuild.go),
// and stage only the figures that move: so a release whose folds agree with the build before it
// writes nothing, and one whose folds differ lands what they move in its flip, which logs an
// eval_days change of each eval whose figures it moved, so the views read them. The stored figures
// stay in use through a deploy, a roll of two builds and a rollback alike.
//
// While two builds whose folds differ roll, a figure a page's write folds is the fold of the
// build whose process made the write, as each day row is, until the release's rebuild flips: it
// folds again, before its flip, every end day a statement wrote figures of, and the ranges of every
// day a statement wrote rows of, while it ran.
//
// A write that leaves its fold to the next pass is logged like any other: a register write, which
// moves an eval's fold key or listing and folds nothing, logs its eval_versions change, and a write
// of day rows by a build that holds the day rows and not the figures (an earlier build of the
// change that brought them; the release before both holds neither) logs its eval_days change, by
// eval_days' trigger (migration 20261117100014). Every build that writes figures reads every such
// change from the figures' own cursor in the change log (figureChanges, eval_days_cursor's
// `figures` row), which no build without the figures reads, so a change such a build's refresher
// read off the refresher's own cursor is still folded. A read sees figures older than the rows
// from that write's commit until the next pass of a build with the figures, which the change's
// own notification wakes and which starts as soon as the pass running then ends, and the pass's
// own length.

// figuresCursor names the figures' own cursor in eval_days_cursor (figureChanges).
const figuresCursor = "figures"

// figuresPass is the stored figures' part of a pass's background work, after the feeds' pages and
// the walks of the day rows: the read of the change log (figureChanges), then, where no walk of the
// day rows in the open has days left, a batch of each figures walk (walkFigures), which waits for
// those walks (a walk begun again) so each range is folded once, from the rows as they leave them,
// rather than once more for each batch of days they move under it. It answers whether a figures
// walk has end days left. The refold of the stale cuts waits for no walk (refoldStaleCuts): a walk's
// writes fold the figures of the days they write in every cut (foldWalked).
func (r *refresh) figuresPass(ctx context.Context, walksPending bool, page int) (bool, error) {
	if err := r.figureChanges(ctx, page); err != nil {
		return false, err
	}
	if walksPending {
		return false, nil
	}
	return r.walkFigures(ctx)
}

// everyDay is the span an eval_versions change names: every day of the eval, since a new version
// of its definition or of one of its series can move its fold key.
var everyDay = daySpan{first: time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), last: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)}

// addSpan widens spans' span of evalID to hold first to last.
func addSpan(spans map[string]daySpan, evalID string, first, last time.Time) {
	span, held := spans[evalID]
	if !held || first.Before(span.first) {
		span.first = first
	}
	if !held || last.After(span.last) {
		span.last = last
	}
	spans[evalID] = span
}

// figureChanges is the figures' read of the change log, page after page until one comes back
// short: every eval_days change after the figures' cursor, whichever build's write logged it, and
// every eval_versions change, each by project and eval. For each project it folds again, in a
// transaction of its own, the figures of every day the changes span whose rows moved since they
// were folded (refoldFigures): none where a build's write folded them in its own transaction, which
// costs the two short reads of the moved-day test; the days of a build that writes no figures; and
// every day of an eval a register write or the seed gave another fold key. It then moves the
// cursor, so a pass that fails reads the same changes again. The cursor is the change log's as the
// refresher's own is (delivery.ChangeCursor): it starts at the log's head, the walks folding every
// figure before it; and one that may have passed over changes it never read (delivery.CursorLost)
// moves to the log's head and begins a rebuild of the built projects' figures (rebuild.go), which
// folds every one again aside and flips those that moved: the day rows' own cursor lost nothing.
func (r *refresh) figureChanges(ctx context.Context, limit int) error {
	pool := r.store.database.Pool
	for {
		checked := r.clock()
		var cursor delivery.ChangeCursor
		err := pool.QueryRow(ctx, `select position, at, updated_at from eval_days_cursor where name = $1`, figuresCursor).Scan(
			&cursor.Seq, &cursor.At, &cursor.ReadAt)
		if errors.Is(err, pgx.ErrNoRows) {
			head, err := delivery.ChangesHead(ctx, pool)
			if err != nil {
				return fmt.Errorf("read the head of the delivery changes: %w", err)
			}
			head.ReadAt = checked
			return r.moveFigureCursor(ctx, head, false)
		}
		if err != nil {
			return fmt.Errorf("read the stored figures' change cursor: %w", err)
		}
		lost, err := delivery.CursorLost(ctx, pool, cursor, checked)
		if err != nil {
			return fmt.Errorf("read the delivery change at %d: %w", cursor.Seq, err)
		}
		if lost {
			head, err := delivery.ChangesHead(ctx, pool)
			if err != nil {
				return fmt.Errorf("read the head of the delivery changes: %w", err)
			}
			head.ReadAt = checked
			slog.Warn("dispatch: the stored figures' change cursor may have passed changes it never read; a rebuild of the built figures begins",
				"cursor", cursor.Seq, "head", head.Seq)
			return r.moveFigureCursor(ctx, head, true)
		}
		changes, err := delivery.ChangesSince(ctx, pool, cursor.Seq, limit)
		if err != nil {
			return fmt.Errorf("read the delivery changes after %d: %w", cursor.Seq, err)
		}
		spans := map[string]map[string]daySpan{}
		for _, change := range changes {
			first, last := everyDay.first, everyDay.last
			switch {
			case !r.known[change.Repo]:
				continue
			case change.Kind == delivery.ChangeKindEvalDays && change.TOld != nil && change.TNew != nil:
				first, last = measures.Midnight(*change.TOld), measures.Midnight(*change.TNew)
			case change.Kind != delivery.ChangeKindEvalVersions:
				continue
			}
			if spans[change.Repo] == nil {
				spans[change.Repo] = map[string]daySpan{}
			}
			addSpan(spans[change.Repo], change.Ref, first, last)
		}
		for _, project := range slices.Sorted(maps.Keys(spans)) {
			if err := r.refoldChanged(ctx, project, spans[project]); err != nil {
				return err
			}
		}
		if err := r.moveFigureCursor(ctx, cursor.Through(changes, limit, checked), false); err != nil {
			return err
		}
		if len(changes) < limit {
			return nil
		}
	}
}

// moveFigureCursor moves the figures' change cursor to cursor (its ReadAt stamping the row, as the
// refresher's change cursor is stamped, commitPage), and, where restart is set, begins a rebuild
// of the figures of every project whose figures are built (replaceRebuild), in one transaction: a
// change the lost cursor passed is read by no other pass, whichever build's process runs it. The
// walks that rebuild walks are read on the pool before the transaction begins (servedWalks), as
// commitPage reads a rebuild's, so the pass holds one connection at a time.
func (r *refresh) moveFigureCursor(ctx context.Context, cursor delivery.ChangeCursor, restart bool) error {
	var walks []walkKey
	if restart {
		served, err := r.servedWalks(ctx)
		if err != nil {
			return err
		}
		walks = slices.DeleteFunc(served, func(key walkKey) bool { return key.family != walkFigures })
	}
	tx, err := r.store.database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the stored figures' change cursor: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		insert into eval_days_cursor (name, position, at, updated_at) values ($1, $2, $3, $4)
		on conflict (name) do update set position = excluded.position, at = excluded.at, updated_at = excluded.updated_at
	`, figuresCursor, cursor.Seq, cursor.At, cursor.ReadAt); err != nil {
		return fmt.Errorf("move the stored figures' change cursor: %w", err)
	}
	if restart {
		if err := replaceRebuild(ctx, tx, rebuildCursor, r.build, walks); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the stored figures' change cursor: %w", err)
	}
	return nil
}

// refoldChanged folds again project's figures of the days of spans (by eval id) whose rows moved,
// in one transaction, under the locks of its logged evals among them (lockLogged), which every
// readings write, zero fill and register write of one takes too, so none of them writes a row of
// theirs between the read of the rows and the write of their figures.
func (r *refresh) refoldChanged(ctx context.Context, project string, spans map[string]daySpan) error {
	tx, err := r.store.database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the stored figures of %s: %w", project, err)
	}
	defer tx.Rollback(ctx)
	entries, err := readEntries(ctx, tx, project, slices.Sorted(maps.Keys(spans)))
	if err != nil {
		return err
	}
	if _, err := lockLogged(ctx, tx, entries); err != nil {
		return err
	}
	scope, ok, err := readFigureScope(ctx, tx, r.build, project)
	if err != nil || !ok {
		return err
	}
	if err := refoldFigures(ctx, tx, scope, entriesByID(entries), spans); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the stored figures of %s: %w", project, err)
	}
	return nil
}

// lockLogged takes the lock of each logged eval of entries (lockEvals), and answers whether it took
// any: a computed eval's rows are the refresher's alone, whose passes the refresher's lease runs one
// at a time, and a logged eval's rows are its readings writes' and its zero fill's too, which each
// take its lock before they read or write a row. The fold that takes them has read its entries
// before, and holds no transaction id while it waits; a register write that commits between the
// two moves the eval's fold key, which a read compares, and logs the eval_versions change that
// folds it again.
func lockLogged(ctx context.Context, tx pgx.Tx, entries []Entry) (bool, error) {
	var logged []string
	for _, entry := range entries {
		if !entry.Definition.Computed() {
			logged = append(logged, entry.Definition.EvalID)
		}
	}
	if len(logged) == 0 {
		return false, nil
	}
	return true, lockEvals(ctx, tx, logged)
}

// noteWalked records that the pass's walk of family wrote project's rows of day, whose figures it
// folds at the end of the walk (foldWalked), by widening the walk's span of days to hold it: a
// walk's day is never the zero time, which widen reads as no day.
func (r *refresh) noteWalked(family, project string, day time.Time) {
	key := walkKey{family: family, project: project}
	if r.walked == nil {
		r.walked = map[walkKey]daySpan{}
	}
	r.walked[key] = r.walked[key].widen(day)
}

// foldWalked folds the figures of the days the pass's walks wrote (noteWalked) whose rows moved,
// one transaction per project once the walks' batches are written: a walk writes each day in a
// transaction of its own, and a range that holds several of a batch's days would be folded once
// for each of them in their own transactions, up to walkBatch times a pass. Until it commits, a
// read sees the figures of the rows before the batch, for the length of one pass, and a view
// recomputed then answers them; so the fold's commit logs a change of each eval whose figures it
// moved (foldFiguresOfDays), and the view is recomputed once more, from the figures of the rows
// it read. A pass that fails first leaves the days' stamps unmoved, and the next pass's read of
// the change log folds them (figureChanges).
//
// An owner key's walked days (owners.go) are folded into the figures of each project whose read
// folds its rows (readersOf, one read of every owner key walked), with that project's own.
func (r *refresh) foldWalked(ctx context.Context) error {
	walked := r.walked
	r.walked = nil
	var owners []string
	for key := range walked {
		if isOwner(key.project) {
			owners = append(owners, key.project)
		}
	}
	slices.Sort(owners)
	readers, err := readersOf(ctx, r.store.database.Pool, slices.Compact(owners))
	if err != nil {
		return err
	}
	byProject := map[string]map[string]daySpan{}
	for key, span := range walked {
		slugs := legionDaySlugs
		if key.family == walkDelivery {
			slugs = deliveryDaySlugs
		}
		projects := []string{key.project}
		if isOwner(key.project) {
			projects = readers[key.project]
		}
		for _, project := range projects {
			if byProject[project] == nil {
				byProject[project] = map[string]daySpan{}
			}
			for _, slug := range slugs {
				addSpan(byProject[project], EvalID(project, slug), span.first, span.last)
			}
		}
	}
	for _, project := range slices.Sorted(maps.Keys(byProject)) {
		if err := r.store.foldFiguresOfDays(ctx, r.build, project, byProject[project]); err != nil {
			return err
		}
	}
	return nil
}

// foldFiguresOfDays folds again, in a transaction of its own, project's figures of the days of
// spans whose rows moved (refoldProject, foldFiguresInTx's fold). Its evals are computed ones,
// whose rows the refresher alone writes, so it takes no eval's lock.
//
// The transaction writes no day row, so eval_days' triggers log nothing of it, and no trigger
// sits on the figure tables: it logs an eval_days change itself of each eval whose figures it
// folded again, spanning their days (eval_days_change_log, migration 20261117100014), in the
// change log's outbox as a day write's are. A view recomputed between the day rows' commits and
// this one read those rows beside the figures of before them, and nothing else would mark it once
// the figures stand. The fold could not run in the batch's last day commit instead: that commit
// logs a change only where the day's own rows moved, and a view recomputed between two earlier
// commits of the batch would still answer figures the last commit then moved. A fold that moves
// no figure logs nothing.
func (s *Store) foldFiguresOfDays(ctx context.Context, build, project string, spans map[string]daySpan) error {
	if len(spans) == 0 {
		return nil
	}
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the stored figures of %s: %w", project, err)
	}
	defer tx.Rollback(ctx)
	folded, ok, err := refoldProject(ctx, tx, build, project, false, spans)
	if err != nil || !ok {
		return err
	}
	for _, evalID := range slices.Sorted(maps.Keys(folded)) {
		if _, err := tx.Exec(ctx, `select eval_days_change_log($1, $2, $3::date, $4::date)`, project, evalID, folded[evalID].first, folded[evalID].last); err != nil {
			return fmt.Errorf("log the figures of %s folded again: %w", evalID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the stored figures of %s: %w", project, err)
	}
	if len(folded) > 0 {
		s.drainChanges(ctx)
	}
	return nil
}

// walkFigures builds what the pass's batch (gapBatch) has left of each project's figures walk,
// whichever build's process runs it: from the first end day it has not built, or, where it is to
// begin (no record, a project's first), from the day after the project's first listed day row, it
// folds up to the batch's end days left of the walk through FigureLead days after today
// (buildFigureEnds), and records where it stands in the same transaction, so the figures and the
// walk that says how far they are folded commit together. It stops between two projects for an
// owed wake (yields), each project's part being one transaction. The part that reaches the last end
// day records the project's figures built through it (eval_figures_built), which the first whole
// walk sets and every later day moves on; so once a walk has built the last end day, each later UTC
// day's first walking pass builds the end day that day adds, two days after the new today. A
// project with no listed day row stands past the last end day, and one whose first row is later
// than that stands at the day after it, built: no range before holds a row, and the write of its
// first rows folds them. A release folds built figures again aside, in its rebuild (rebuild.go),
// not here. It answers whether end days before the last are left, which asks for the pass that
// builds them.
func (r *refresh) walkFigures(ctx context.Context) (bool, error) {
	walks, err := r.store.readWalks(ctx)
	if err != nil {
		return false, err
	}
	last := r.today.AddDate(0, 0, FigureLead)
	pending := false
	for _, project := range r.projects {
		if r.yields(ctx) {
			return true, nil
		}
		key := walkKey{family: walkFigures, project: project}
		next := walks[key].next
		begun := next == nil
		if begun {
			start, err := r.store.figuresStart(ctx, project)
			if err != nil {
				return false, err
			}
			if start == nil {
				start = new(last.AddDate(0, 0, 1))
			}
			next = start
		}
		if next.After(last) {
			if begun {
				if err := r.recordFiguresBuilt(ctx, key, *next); err != nil {
					return false, err
				}
			}
			continue
		}
		end, ok := r.batched.through(key, r.batch, *next, last)
		if !ok {
			pending = true
			continue
		}
		if err := r.buildFigures(ctx, key, *next, end, end.Equal(last)); err != nil {
			return false, err
		}
		r.batched.spend(key, int(end.Sub(*next)/(24*time.Hour))+1)
		r.walkBuilt++
		pending = pending || end.Before(last)
	}
	return pending, nil
}

// recordFiguresBuilt records, in one transaction, that key's walk stands at next, past the last end
// day, and that its project's figures are built through the day before it: a project whose ranges
// before next hold no row.
func (r *refresh) recordFiguresBuilt(ctx context.Context, key walkKey, next time.Time) error {
	tx, err := r.store.database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the figures walk of %s: %w", key.project, err)
	}
	defer tx.Rollback(ctx)
	if err := r.recordWalkIn(ctx, tx, key, next); err != nil {
		return err
	}
	if err := markFiguresBuilt(ctx, tx, key.project, next.AddDate(0, 0, -1), r.build); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the figures walk of %s: %w", key.project, err)
	}
	return nil
}

// buildFigures is one batch of key's figures walk: the figures of every range of the project that
// ends on a day from first through last, where the walk stands after them, and, where built (the
// batch reaches the last end day), the project's figures built through last, in one transaction.
func (r *refresh) buildFigures(ctx context.Context, key walkKey, first, last time.Time, built bool) error {
	tx, err := r.store.database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the figures walk of %s: %w", key.project, err)
	}
	defer tx.Rollback(ctx)
	entries, err := readEntries(ctx, tx, key.project, nil)
	if err != nil {
		return err
	}
	byID := entriesByID(entries)
	if _, err := lockLogged(ctx, tx, entries); err != nil {
		return err
	}
	scope := figureScope{build: r.build, project: key.project, through: last}
	if err := buildFigureEnds(ctx, tx, scope, byID, nil, true, first, last); err != nil {
		return err
	}
	if err := r.recordWalkIn(ctx, tx, key, last.AddDate(0, 0, 1)); err != nil {
		return err
	}
	if built {
		if err := markFiguresBuilt(ctx, tx, key.project, last, r.build); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the figures walk of %s through %s: %w", key.project, last.Format(time.DateOnly), err)
	}
	return nil
}

// markFiguresBuilt records in q that project's figures are built through through, by build where
// no build had built them before: eval_figures_built's row is made by the first whole walk and
// moves on with each later day, never back.
func markFiguresBuilt(ctx context.Context, q executor, project string, through time.Time, build string) error {
	if _, err := q.Exec(ctx, `
		insert into eval_figures_built (project_key, through, built_by, updated_at) values ($1, $2, $3, now())
		on conflict (project_key) do update set through = excluded.through, updated_at = excluded.updated_at
		where eval_figures_built.through < excluded.through
	`, project, through, build); err != nil {
		return fmt.Errorf("record the stored figures of %s built through %s: %w", project, through.Format(time.DateOnly), err)
	}
	return nil
}

// figuresStart is the first end day of project's figures walk: the day after its first listed day
// row, its own or one of an owner key it reads (eval_project_owners, owners.go), whose figures it
// folds, nil where it reads none.
func (s *Store) figuresStart(ctx context.Context, project string) (*time.Time, error) {
	var first *time.Time
	if err := s.database.Pool.QueryRow(ctx, `
		select min(d.day) from eval_project_owners o join eval_days d on d.project_key = o.owner
		where o.project_key = $1 and d.listed
	`, project).Scan(&first); err != nil {
		return nil, fmt.Errorf("read the first day row of %s: %w", project, err)
	}
	if first == nil {
		return nil, nil
	}
	start := measures.Midnight(*first).AddDate(0, 0, 1)
	return &start, nil
}

// builtCut is how far one project's stored figures are built (eval_figures_built), and which of
// its evals a stale mark of the cut a read reads names (eval_figure_cuts_stale): none where no mark
// holds the cut, every one where the mark names every eval (every, its evals null), and else each
// eval stale holds.
type builtCut struct {
	through time.Time
	every   bool
	stale   map[string]bool
}

// serves reports whether the stored figures of the cut answer evalID, an eval of the project: no
// stale mark of the cut names it.
func (b builtCut) serves(evalID string) bool {
	return !b.every && !b.stale[evalID]
}

// figuresThrough reads in q, for each of projects whose stored figures are built, the last end day
// they are built through (eval_figures_built), and which of its evals a mark of its cut of repo
// ("" for its own rows, which no mark holds) names stale (eval_figure_cuts_stale): a read of the
// cut over a range of FigureLengths ending on or before that day may read the stored figures of
// every eval no mark names (figures.go), whichever build serves the read and whichever folded
// them, and reads the day rows of every other. A project is absent until the first whole walk of
// its figures by any build is done (the first release that holds the stored figures, or a project
// made later), and stays once it is, through every later release, roll and rollback. An eval a
// move of a default repository moved rows of in a cut is named stale there from the commit of the
// move that marks it until the commit of its refold's last batch (refoldStaleCuts), and every other
// eval of the cut is read from its stored figures throughout, since no move moved the rows they
// fold. One statement: a primary-key probe of each project's mark.
func figuresThrough(ctx context.Context, q delivery.Querier, projects []string, repo string) (map[string]builtCut, error) {
	rows, err := q.Query(ctx, `
		select b.project_key, b.through, s.project_key is not null, s.evals from eval_figures_built b
		left join eval_figure_cuts_stale s on s.project_key = b.project_key and s.repo = $2
		where b.project_key = any($1)
	`, projects, repo)
	if err != nil {
		return nil, fmt.Errorf("read how far the stored figures of %v are built: %w", projects, err)
	}
	built := map[string]builtCut{}
	var project string
	var day time.Time
	var marked bool
	var evals []string
	if _, err := pgx.ForEachRow(rows, []any{&project, &day, &marked, &evals}, func() error {
		cut := builtCut{through: measures.Midnight(day), every: marked && evals == nil}
		if len(evals) > 0 {
			cut.stale = make(map[string]bool, len(evals))
			for _, evalID := range evals {
				cut.stale[evalID] = true
			}
		}
		built[project] = cut
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read how far the stored figures of %v are built: %w", projects, err)
	}
	return built, nil
}

// staleCut is one cut of a project's stored figures, by its repository (every deploy repository
// of it), that a move of a default repository marked stale (eval_figure_cuts_stale, figures.go).
type staleCut struct {
	project, repo string
}

// compareStaleCuts orders stale cuts by project and then by repository, each by its bytes, as the
// marks' primary key does under `collate "C"`.
func compareStaleCuts(a, b staleCut) int {
	return cmp.Or(strings.Compare(a.project, b.project), strings.Compare(a.repo, b.repo))
}

// markStaleCuts marks each cut of marks stale, in tx, the commit of the move of the default
// repositories that moves what they fold (publishDefaults), naming the evals marks holds for it:
// the ids of the project's evals whose rows the move moved into or out of the cut (staleMarksOf),
// none of which holds ','. A cut no mark holds is marked at generation 1, naming those evals. One a
// mark holds already, whose refold may have begun, is marked at its next generation, its refold to
// begin again from the cut's first end day (next_day null), naming the evals of both marks, or
// every eval where the standing mark names every one (null): so a refold begun before the move
// folds again, under the defaults it moved to, every eval either move moved, and clears no mark
// the move set (refoldStaleCuts). A cut of a project deleted since is left. In statements of at
// most statementBytes, each in the byte order of the marks' keys, the order every writer of them
// takes them in.
func markStaleCuts(ctx context.Context, tx pgx.Tx, marks map[staleCut][]string) error {
	cuts := slices.SortedFunc(maps.Keys(marks), compareStaleCuts)
	joined := make(map[staleCut]string, len(marks))
	for cut, evals := range marks {
		joined[cut] = strings.Join(evals, ",")
	}
	size := func(cut *staleCut) int { return 3*4 + len(cut.project) + len(cut.repo) + len(joined[*cut]) }
	for chunk := range statementChunks(cuts, paramBytes(3), size) {
		projects, repos, evals := make([]string, len(chunk)), make([]string, len(chunk)), make([]string, len(chunk))
		for index, cut := range chunk {
			projects[index], repos[index], evals[index] = cut.project, cut.repo, joined[cut]
		}
		if _, err := tx.Exec(ctx, `
			insert into eval_figure_cuts_stale (project_key, repo, generation, next_day, evals)
			select given.project_key, given.repo, 1, null, string_to_array(given.evals, ',')
			from unnest($1::text[], $2::text[], $3::text[]) with ordinality as given(project_key, repo, evals, sent)
			join projects p on p.key = given.project_key
			order by given.sent
			on conflict (project_key, repo) do update set generation = eval_figure_cuts_stale.generation + 1, next_day = null,
				evals = case when eval_figure_cuts_stale.evals is not null and excluded.evals is not null then array(
					select id from unnest(eval_figure_cuts_stale.evals || excluded.evals) as id group by id order by id collate "C") end
		`, projects, repos, evals); err != nil {
			return fmt.Errorf("mark %d cuts of the stored figures stale: %w", len(chunk), err)
		}
	}
	return nil
}

// refoldBatchBudget is about how long the server's schedule holds one batch of a stale cut's refold
// to (serverRefresherSchedule, refoldStaleCuts, refoldBatchDays). refoldCut holds the locks of the
// logged evals it folds (lockLogged) for the whole batch, and a readings write of such an eval
// waits on that lock, so the server's batch is kept well inside R14's 2 s: a batch of 28 end days
// of INFRA's late history, 5.7 s on the build farm, would hold the write past it. A schedule with
// no budget, a test's or RefreshThrough's, has no readings write beside it to keep waiting short.
const refoldBatchBudget = 250 * time.Millisecond

// refoldBatchDays is how many end days the next batch of a stale cut's refold folds, after a batch
// of days end days whose transaction took took: as many as budget (the schedule's,
// refresherSchedule.refoldBudget) holds at that batch's time per end day, at least one and at most
// most (the pass's batch size, refresh.batch). A batch that took no time the clock can tell asks
// for most, and so does every batch where budget is none, which bounds no batch by time.
func refoldBatchDays(days int, took, budget time.Duration, most int) int {
	most = max(most, 1)
	if budget <= 0 || took <= 0 {
		return most
	}
	return min(max(int(budget*time.Duration(days)/took), 1), most)
}

// refoldStaleCuts folds again each stale cut to its end, whichever build's process runs it, the
// cuts in the byte order of their keys, batch after batch until the cut's mark is cleared, each
// batch a transaction of its own (refoldStaleBatch). The pass's first batch folds one end day,
// short wherever the refold stands (a pass that goes on after a stop begins where the mark left it,
// however late in the cut's history), and each later one as many as the schedule's budget
// (refresh.refoldBudget) holds at the time per end day of the batch before it (refoldBatchDays):
// the server's schedule holds a batch to about 250 ms (refoldBatchBudget), so none holds the locks
// of the project's logged evals for long, and one with no budget folds the pass's batch size in
// each later batch. It stops between two batches for an owed wake (yields), so a pass folds at
// least one batch and a change waits behind one batch rather than the whole refold; the owed
// pass, which the loop starts at once, goes on from where the marks stand. It answers whether a
// mark is left, which only a stop for a wake leaves: so the pass that publishes a move begins the
// refold of the cuts it marks once its pages are done, and no gap paces it (RefresherGap).
func (r *refresh) refoldStaleCuts(ctx context.Context) (bool, error) {
	rows, err := r.store.database.Pool.Query(ctx, `select project_key, repo from eval_figure_cuts_stale order by project_key collate "C", repo collate "C"`)
	if err != nil {
		return false, fmt.Errorf("read the stale cuts of the stored figures: %w", err)
	}
	cuts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (staleCut, error) {
		var cut staleCut
		err := row.Scan(&cut.project, &cut.repo)
		return cut, err
	})
	if err != nil {
		return false, fmt.Errorf("read the stale cuts of the stored figures: %w", err)
	}
	days, folded := 1, false
	for _, cut := range cuts {
		for done := false; !done; {
			if folded && r.yields(ctx) {
				return true, nil
			}
			var took time.Duration
			if done, took, err = r.refoldStaleBatch(ctx, cut, days); err != nil {
				return false, err
			}
			folded = true
			r.walkBuilt++
			days = refoldBatchDays(days, took, r.refoldBudget, r.batch)
		}
	}
	return false, nil
}

// refoldStaleBatch is one batch of days end days of the stale cut's refold, in one transaction: it
// reads the cut's mark, folds again the evals it names (every eval where it names none) from the
// first end day the mark's refold has not folded, or, where it is to begin, from the cut's first
// end day (refoldCut), and records with the mark where the refold stands, or, once the batch folded
// through the project's last end day, deletes the mark, so a read of the cut reads every eval's
// stored figures from this commit on (figuresThrough). Each of the two writes the mark only where
// its generation is the one the batch read: a move that marked the cut again since leaves it
// stale, its refold to begin again (markStaleCuts). A cut no mark holds any more is done. It
// answers whether the cut is done and how long its transaction took, which sizes the next batch
// (refoldBatchDays), and logs both beside how long it held the locks of the logged evals it folds,
// from their take (lockLogged) to its commit, 0 where it took none: the wait a readings write of
// one of them may have had.
func (r *refresh) refoldStaleBatch(ctx context.Context, cut staleCut, days int) (bool, time.Duration, error) {
	began := time.Now()
	done := true
	var from *time.Time
	var next, locked time.Time
	if err := r.store.inTx(ctx, "a stale cut's refold", func(tx pgx.Tx) error {
		var generation int64
		var evals []string
		err := tx.QueryRow(ctx, `select generation, next_day, evals from eval_figure_cuts_stale where project_key = $1 and repo = $2`,
			cut.project, cut.repo).Scan(&generation, &from, &evals)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the mark of %s's stale cut of %s: %w", cut.project, cut.repo, err)
		}
		if next, done, locked, err = refoldCut(ctx, tx, r.build, cut.project, cut.repo, evals, from, days); err != nil {
			return err
		}
		if done {
			_, err = tx.Exec(ctx, `delete from eval_figure_cuts_stale where project_key = $1 and repo = $2 and generation = $3`,
				cut.project, cut.repo, generation)
		} else {
			_, err = tx.Exec(ctx, `update eval_figure_cuts_stale set next_day = $3 where project_key = $1 and repo = $2 and generation = $4`,
				cut.project, cut.repo, next, generation)
		}
		if err != nil {
			return fmt.Errorf("record the refold of %s's stale cut of %s: %w", cut.project, cut.repo, err)
		}
		return nil
	}); err != nil {
		return false, 0, err
	}
	took := time.Since(began)
	var held time.Duration
	if !locked.IsZero() {
		held = time.Since(locked)
	}
	slog.Info("dispatch: the eval day-row refresher folded a batch of a stale cut of the stored figures", "project", cut.project, "repo", cut.repo,
		"from", from, "next", next.Format(time.DateOnly), "done", done, "transaction_ms", took.Milliseconds(), "locks_held_ms", held.Milliseconds())
	return done, took, nil
}

// refoldCut folds again, in tx, under build, project's stored figures of its cut of repo, every
// deploy repository of it, of each eval evals names, or of every eval of the project where evals
// is nil: each range that ends on a day from from, or from the cut's first end day where from is
// nil (cutStart), through at most days end days in all and at most the last end day the project's
// figures are folded through (readFigureScope), and the day row of every day before one of them
// (buildFigureEnds of the one cut), under the locks of the logged evals among them (lockLogged),
// which every readings write of one takes too. It writes and deletes the stored rows of those
// evals alone, so the stored figures of every other eval of the cut, whose rows no move that
// marked the cut moved, stand as they are. It answers the first end day it left, whether it folded
// through the last one, and when it took those locks (the zero time where it took none): a cut's
// refold is whole once a run of calls from nil reaches it, so the one call of a transaction that
// passes days past the scope's span folds a cut whole, as a move's own commit could where that fits
// R14's 2 s. A project none of whose figures are folded, and a cut that holds no stored figure and
// no row, have nothing to fold.
func refoldCut(ctx context.Context, tx pgx.Tx, build, project, repo string, evals []string, from *time.Time, days int) (next time.Time, done bool, locked time.Time, err error) {
	scope, ok, err := readFigureScope(ctx, tx, build, project)
	if err != nil || !ok {
		return time.Time{}, true, time.Time{}, err
	}
	entries, err := readEntries(ctx, tx, project, evals)
	if err != nil {
		return time.Time{}, false, time.Time{}, err
	}
	took, err := lockLogged(ctx, tx, entries)
	if err != nil {
		return time.Time{}, false, time.Time{}, err
	}
	if took {
		locked = time.Now()
	}
	first := from
	if first == nil {
		if first, err = cutStart(ctx, tx, project, repo); err != nil || first == nil {
			return time.Time{}, err == nil, locked, err
		}
	}
	if first.After(scope.through) {
		return *first, true, locked, nil
	}
	last := first.AddDate(0, 0, max(days, 1)-1)
	if last.After(scope.through) {
		last = scope.through
	}
	if err := buildFigureEnds(ctx, tx, scope, entriesByID(entries), &repo, evals == nil, *first, last); err != nil {
		return time.Time{}, false, locked, err
	}
	return last.AddDate(0, 0, 1), last.Equal(scope.through), locked, nil
}

// cutStart is the first end day of the refold of project's cut of repo: the day after the first
// day the cut's stored figures or its rows hold, whichever is earlier, so the refold deletes every
// stored figure a move left with no row and folds every row the move brought (cutOf: the rows of
// repo the project reads, and its own rows stored under legionfacts.DefaultRepo where repo is its
// published default); nil where neither holds one. One probe of the stored figures' primary key,
// one of eval_days_window for each key the project reads, and one for its rows under
// legionfacts.DefaultRepo.
func cutStart(ctx context.Context, q delivery.Querier, project, repo string) (*time.Time, error) {
	var first *time.Time
	if err := q.QueryRow(ctx, `
		select least(
			(select min(day) from eval_day_figures where project_key = $1 and repo = $2),
			(select min(owned.day) from eval_project_owners o cross join lateral (
				select d.day from eval_days d where d.project_key = o.owner and d.listed and d.repo = $2 order by d.day limit 1
			) owned where o.project_key = $1),
			(select min(d.day) from eval_days d where d.project_key = $1 and d.listed and d.repo = '`+legionfacts.DefaultRepo+`'
				and (select p.repo from eval_project_repos p where p.project_key = $1) = $2))
	`, project, repo).Scan(&first); err != nil {
		return nil, fmt.Errorf("read the first day of %s's cut of %s: %w", project, repo, err)
	}
	if first == nil {
		return nil, nil
	}
	return new(measures.Midnight(*first).AddDate(0, 0, 1)), nil
}
