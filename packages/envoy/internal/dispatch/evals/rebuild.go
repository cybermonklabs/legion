package evals

import (
	"bytes"
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
)

// rebuild.go is the verified rebuild (dispatch://INFRA-2916): how stored rows that serve readers
// are rebuilt where no input changed and every day may have moved. A release (a process whose
// first pass finds the rows served by another build), a lost change cursor, and delivery settings
// the rows stand built under that are not recorded each rebuild the served families
// (eval_days_serving) aside and make what moved visible in one short commit. A family no rows are
// served of yet, a database's or a project's first seen, has nothing to keep serving: its walk
// builds it in the open, one day of every key a commit that moves its serving record past it
// (walkWrite, recordWalkIn), each day and every window through it readable once written and "no
// data" before (Built.Withhold).
//
//   - The stored rows keep serving through the rebuild. A rebuild walks each family's days as a
//     walk does (walkDays, with the walk's own builders and first input days), a batch a pass, and
//     stages only what differs from the stored rows, members and figures (eval_rebuild_days,
//     eval_rebuild_members, eval_rebuild_range_figures, eval_rebuild_day_figures, migration
//     20261117100021), op `u` or `d`; each batch's staged rows and its progress commit together,
//     in a short transaction, so a pass that fails resumes where the last batch left it. Once its
//     day walks are done it folds every stored figure of its projects over the stored rows overlaid
//     with the staged ones, a batch of end days a pass, and stages the figures that moved.
//   - A page writes in the open during a rebuild, as it does at any time (R14), and judges each
//     day's zero rows by what the stored rows are whole through (servingNext), so it keeps them.
//     Every statement that writes day rows, members or figures, whatever process runs it, an older
//     build's during a roll among them, queues the days it touched that the rebuild had built
//     (eval_rebuild_reverify, by the tables' triggers); the flip's pass builds the queued days
//     again, immediately before the flip, which replaces what was staged of them (verifyQueued), so
//     the flip neither undoes a write in the open nor writes it twice. No writer deletes what was
//     staged: the flip's pass replaces it. A queued day that moves a series' first input day the
//     rebuild found walks the rebuild back to it.
//   - The flip is one transaction of set-based statements that read the staging by the rebuild's
//     id and send no list (flipRebuild): it writes the staged rows, members and figures, logs an
//     eval_days change of each eval whose figures alone moved, under the rebuild's mark
//     (MarkRebuild), so an evals view is recomputed for it at once and gets one delta, moves the
//     serving record to the rebuild's build, and records the walks the rebuild built as built. A
//     rebuild whose values do not move stages nothing, so its flip writes no row and logs nothing.
//     The staged rows are then deleted in short chunks (cleanRebuilds).
//
// At most one rebuild is building. A rewrite of a settings change and a page that finds a
// repository's runs gone abandon it and begin it again
// (replaceRebuild), since its staged rows may have missed what they moved; a lost cursor does too,
// as a rebuild of every served family. Only its own build's process walks it: while two builds
// roll, the build before the release, which served the rows when its process began, begins no
// rebuild (releaseWatch), so the roll flips once. One whose build has not moved it for staleRebuild, a
// build no process runs, is abandoned, and begun again by the build that finds it where it was no
// release.

// The reasons a rebuild begins for (eval_rebuilds.reason).
const (
	rebuildRelease  = "release"
	rebuildCursor   = "cursor"
	rebuildSettings = "settings"
	rebuildFirst    = "first"
)

// rebuildChunk is the most staged rows one statement of the staging's cleanup deletes, each
// statement a transaction of its own (cleanRebuilds).
const rebuildChunk = 5000

// servingState is one family's serving record (eval_days_serving): the build whose rules built the
// stored rows, and the first day they are not whole through.
type servingState struct {
	build string
	next  time.Time
}

// readServing is every serving record, by walk.
func (s *Store) readServing(ctx context.Context) (map[walkKey]servingState, error) {
	rows, err := s.database.Pool.Query(ctx, `select family, project_key, built_by, next_day from eval_days_serving`)
	if err != nil {
		return nil, fmt.Errorf("read what the day rows are served by: %w", err)
	}
	serving := map[walkKey]servingState{}
	var key walkKey
	var state servingState
	if _, err := pgx.ForEachRow(rows, []any{&key.family, &key.project, &state.build, &state.next}, func() error {
		state.next = measures.Midnight(state.next)
		serving[key] = state
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read what the day rows are served by: %w", err)
	}
	return serving, nil
}

// servingNext is the first day the stored rows of family (project's, for the Legion rows) are not
// whole through (eval_days_serving), nil where no walk has built a day of them and no rebuild has
// flipped them: every page write judges a day's zero rows by it, whatever walk or rebuild runs. It
// reads the records once a page.
func (r *refresh) servingNext(ctx context.Context, family, project string) (*time.Time, error) {
	if r.serving == nil {
		serving, err := r.store.readServing(ctx)
		if err != nil {
			return nil, err
		}
		r.serving = serving
	}
	state, served := r.serving[walkOf(family, project)]
	if !served {
		return nil, nil
	}
	next := state.next
	return &next, nil
}

// rebuildState is the rebuild that is building: its id, reason and build, whether its build has
// not moved it for staleRebuild, and its walks.
type rebuildState struct {
	id     int64
	reason string
	build  string
	stale  bool
	walks  map[walkKey]*time.Time
}

// readRebuild is the rebuild that is building, nil where none is.
func (s *Store) readRebuild(ctx context.Context) (*rebuildState, error) {
	rows, err := s.database.Pool.Query(ctx, `
		select b.id, b.reason, b.built_by, b.updated_at < now() - $1::interval, w.family, w.project_key, w.next_day
		from eval_rebuilds b left join eval_rebuild_walks w on w.rebuild_id = b.id
		where b.state = 'building'
	`, staleRebuild)
	if err != nil {
		return nil, fmt.Errorf("read the rebuild that is building: %w", err)
	}
	var rb *rebuildState
	var id int64
	var reason, build string
	var stale bool
	var family, project *string
	var next *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&id, &reason, &build, &stale, &family, &project, &next}, func() error {
		if rb == nil {
			rb = &rebuildState{id: id, reason: reason, build: build, stale: stale, walks: map[walkKey]*time.Time{}}
		}
		if family != nil {
			var day *time.Time
			if next != nil {
				day = new(measures.Midnight(*next))
			}
			rb.walks[walkKey{family: *family, project: *project}] = day
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the rebuild that is building: %w", err)
	}
	return rb, nil
}

// walksOf is rb's walks of family, in the order of their projects.
func (rb *rebuildState) walksOf(family string) []walkKey {
	var keys []walkKey
	for key := range rb.walks {
		if key.family == family {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b walkKey) int { return strings.Compare(a.project, b.project) })
	return keys
}

// releaseWatch is what a process's refresher keeps of the release rule from one pass to the next:
// whether its first pass has read whom the stored rows are served by, the builds it found there
// (seen), and whether this process rebuilds them (want). A process whose first pass finds them
// served by another build than its own wants to, a release; one whose first pass finds its own, or
// none, never does, so the build before a release, whose processes still run while the release
// rolls out, does not rebuild them back once the release has flipped. A process that wants to stops
// wanting once its own build serves every family, or once a build it did not find at its first pass
// serves one: another release flipped after this one began, and the newest build's flip stands.
type releaseWatch struct {
	decided bool
	want    bool
	seen    map[string]bool
}

// wants reports whether the process of build rebuilds the rows serving serves for a release.
func (w *releaseWatch) wants(build string, serving map[walkKey]servingState) bool {
	builds := map[string]bool{}
	for _, state := range serving {
		builds[state.build] = true
	}
	if !w.decided {
		w.decided, w.seen = true, builds
		for served := range builds {
			w.want = w.want || served != build
		}
	}
	if !w.want {
		return false
	}
	own := true
	for served := range builds {
		if served != build && !w.seen[served] {
			w.want = false
			return false
		}
		own = own && served == build
	}
	if own {
		w.want = false
	}
	return w.want
}

// servedWalks is the walks a rebuild of every served family walks: the delivery and CI rows where
// delivery settings exist, and each project's Legion rows, where their rows are served
// (eval_days_serving), and each project's stored figures where they are built
// (eval_figures_built). A family no rows are served of is a walk's to build in the open, as a
// database or a project first seen is: there is nothing to keep serving.
func (r *refresh) servedWalks(ctx context.Context) ([]walkKey, error) {
	serving, err := r.store.readServing(ctx)
	if err != nil {
		return nil, err
	}
	built, err := figuresThrough(ctx, r.store.database.Pool, r.projects, "")
	if err != nil {
		return nil, err
	}
	var keys []walkKey
	if r.delivers() && served(serving, walkKey{family: walkDelivery}) {
		keys = append(keys, walkKey{family: walkDelivery})
	}
	for _, project := range r.projects {
		if served(serving, walkKey{family: walkLegion, project: project}) {
			keys = append(keys, walkKey{family: walkLegion, project: project})
		}
		if _, held := built[project]; held {
			keys = append(keys, walkKey{family: walkFigures, project: project})
		}
	}
	return keys, nil
}

// served reports whether serving holds a serving record of the walk key.
func served(serving map[walkKey]servingState, key walkKey) bool {
	_, held := serving[key]
	return held
}

// caughtUp reports whether every walk of a served family has built today: no walk begun again is
// rebuilding them in the open, which a rebuild waits for.
func (r *refresh) caughtUp(walks map[walkKey]walkState, serving map[walkKey]servingState, families ...string) bool {
	for key := range serving {
		if len(families) > 0 && !slices.Contains(families, key.family) {
			continue
		}
		if walk, found := walks[key]; !found || walk.next == nil || !walk.next.After(r.today) {
			return false
		}
	}
	return true
}

// replaceRebuild abandons, in tx, the rebuild that is building, where one is, and begins one in its
// place: of reason (the abandoned one's where reason is empty) by build (the abandoned one's where
// build is empty), over walks and every walk the abandoned one held, each to begin. With no rebuild
// building and no walks it begins none. The abandoned one's staged rows are deleted in short chunks
// (cleanRebuilds).
func replaceRebuild(ctx context.Context, tx pgx.Tx, reason, build string, walks []walkKey) error {
	var was int64
	var wasReason, wasBuild string
	err := tx.QueryRow(ctx, `
		update eval_rebuilds set state = 'abandoned', updated_at = now() where state = 'building' returning id, reason, built_by
	`).Scan(&was, &wasReason, &wasBuild)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if len(walks) == 0 {
			return nil
		}
	case err != nil:
		return fmt.Errorf("abandon the rebuild that is building: %w", err)
	}
	if reason == "" {
		reason = wasReason
	}
	if build == "" {
		build = wasBuild
	}
	var id int64
	if err := tx.QueryRow(ctx, `insert into eval_rebuilds (reason, built_by) values ($1, $2) returning id`, reason, build).Scan(&id); err != nil {
		return fmt.Errorf("begin a rebuild for %s: %w", reason, err)
	}
	families, projects := walkColumns(walks)
	if _, err := tx.Exec(ctx, `
		insert into eval_rebuild_walks (rebuild_id, family, project_key)
		select $1, family, project_key from (
			select family, project_key from unnest($2::text[], $3::text[]) as given(family, project_key)
			union
			select family, project_key from eval_rebuild_walks where rebuild_id = $4
		) walks
		order by family, project_key
	`, id, families, projects, was); err != nil {
		return fmt.Errorf("record the walks of the rebuild for %s: %w", reason, err)
	}
	slog.Info("dispatch: the eval day-row refresher began a rebuild", "reason", reason, "build", build, "abandoned", was)
	return nil
}

// abandonRebuild abandons, in tx, the rebuild that is building, where one is, and begins none.
func abandonRebuild(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `update eval_rebuilds set state = 'abandoned', updated_at = now() where state = 'building'`); err != nil {
		return fmt.Errorf("abandon the rebuild that is building: %w", err)
	}
	return nil
}

// inTx runs write in a transaction of its own, and commits it.
func (s *Store) inTx(ctx context.Context, what string, write func(tx pgx.Tx) error) error {
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin %s: %w", what, err)
	}
	defer tx.Rollback(ctx)
	if err := write(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %s: %w", what, err)
	}
	return nil
}

// rebuildDays is the rebuild's part of the pass's walks (refresh.walk): it begins the rebuild a
// release asks for (releaseWatch) once no walk begun again rebuilds a served family in the open,
// abandons one whose build has not moved it for staleRebuild, and, of a rebuild its own build runs,
// builds what the pass's batch (gapBatch) has left of each of its walks of the day rows, staging
// what differs from the stored rows. A walk of a family a walk begun again rebuilds in the open
// waits for it. walks and serving are the walk records and serving records the pass's walks read.
// It answers whether a walk of the day rows has days left.
func (r *refresh) rebuildDays(ctx context.Context, walks map[walkKey]walkState, serving map[walkKey]servingState) (bool, error) {
	r.serving = serving
	rb, err := r.store.readRebuild(ctx)
	if err != nil {
		return false, err
	}
	if rb != nil && rb.build != r.build && rb.stale {
		// No process of its build moved it for staleRebuild: none runs. A release's is no other
		// build's to make; any other is, since what it rebuilds for still stands.
		if err := r.store.inTx(ctx, "the rebuild of a build no process runs", func(tx pgx.Tx) error {
			if rb.reason == rebuildRelease {
				return abandonRebuild(ctx, tx)
			}
			return replaceRebuild(ctx, tx, "", r.build, nil)
		}); err != nil {
			return false, err
		}
		if rb, err = r.store.readRebuild(ctx); err != nil {
			return false, err
		}
	}
	if release := r.release.wants(r.build, serving); rb == nil && release && r.caughtUp(walks, serving) {
		keys, err := r.servedWalks(ctx)
		if err != nil {
			return false, err
		}
		if err := r.store.inTx(ctx, "the rebuild of a release", func(tx pgx.Tx) error {
			return replaceRebuild(ctx, tx, rebuildRelease, r.build, keys)
		}); err != nil {
			return false, err
		}
		if rb, err = r.store.readRebuild(ctx); err != nil {
			return false, err
		}
	}
	r.rebuilding = rb
	return r.walkRebuild(ctx, walks)
}

// walkRebuild builds what the pass's batch has left of each walk of the day rows of the rebuild
// its own build runs (r.rebuilding), the delivery walk and then each project's Legion walk.
func (r *refresh) walkRebuild(ctx context.Context, walks map[walkKey]walkState) (bool, error) {
	rb := r.rebuilding
	if rb == nil || rb.build != r.build {
		return false, nil
	}
	pending := false
	for _, family := range []dayFamily{deliveryFamily, legionFamily} {
		for _, key := range rb.walksOf(family.walk) {
			if family.walk == walkDelivery && !r.delivers() {
				continue
			}
			if !r.caughtUp(walks, r.serving, family.walk) {
				// A walk begun again rebuilds the family in the open: the rebuild waits for it.
				pending = true
				continue
			}
			more, err := r.rebuildWalk(ctx, rb, family, key)
			if err != nil {
				return false, err
			}
			pending = pending || more
		}
	}
	return pending, nil
}

// stageBatch is what one batch of a rebuild's walk built: each project's day of its family, whole,
// which the batch stages once it is built (stageDays).
type stageBatch struct {
	rebuild *rebuildState
	days    []ProjectDay
	// loaded is each project whose first input days the batch read of the rebuild (firstOf), whose
	// first input days, as the batch's days moved them (noteBuilt), it records.
	loaded map[string]bool
}

// rebuildWalk builds what the pass's batch has left of rb's walk key of family, as the walk builds
// it (walkDays, by the walk's builders and the first input days the rebuild found), staging each
// batch and its progress in one transaction.
func (r *refresh) rebuildWalk(ctx context.Context, rb *rebuildState, family dayFamily, key walkKey) (bool, error) {
	r.staging = &stageBatch{rebuild: rb, loaded: map[string]bool{}}
	r.done = map[builtDay]bool{}
	defer func() { r.staging = nil }()
	record := func(ctx context.Context, key walkKey, next time.Time) error {
		return r.stageWalk(ctx, rb, family, key, next)
	}
	if family.walk == walkDelivery {
		return r.walkDays(ctx, key, rb.walks[key], r.deliveryStart, r.deliveryWalkDay, record)
	}
	return r.walkDays(ctx, key, rb.walks[key],
		func(ctx context.Context) (*time.Time, error) { return r.legionStart(ctx, key.project) },
		func(ctx context.Context, day time.Time) error { return r.writeLegion(ctx, key.project, day) },
		record)
}

// stageWalk stages the days the batch of rb's walk key built (r.staging) with the walk's progress,
// next, and the first input days it has found (r.walkFirst), in one transaction.
func (r *refresh) stageWalk(ctx context.Context, rb *rebuildState, family dayFamily, key walkKey, next time.Time) error {
	firsts := map[string]map[evalKey]time.Time{}
	for project := range r.staging.loaded {
		if first, ok := r.walkFirst[walkKey{family: key.family, project: project}]; ok {
			firsts[project] = first
		}
	}
	if err := r.stageDays(ctx, rb, family, r.staging.days, &walkProgress{key: key, next: next, firsts: firsts}, nil); err != nil {
		return err
	}
	rb.walks[key] = &next
	return nil
}

// walkProgress is where a staged batch leaves its walk: the first day it has not built, and the
// first input days it has found, by project.
type walkProgress struct {
	key    walkKey
	next   time.Time
	firsts map[string]map[evalKey]time.Time
}

// rebuildFirsts is the first input days rb's walk of family has found, by project (of project
// alone where it is not empty): the days its batches recorded with their progress.
func (s *Store) rebuildFirsts(ctx context.Context, rb int64, family, project string) (map[string]map[evalKey]time.Time, error) {
	filter, args := `rebuild_id = $1 and family = $2`, []any{rb, family}
	if project != "" {
		filter, args = filter+` and project_key = $3`, append(args, project)
	}
	rows, err := s.database.Pool.Query(ctx, `select project_key, eval_id, deploy_repo, day from eval_rebuild_firsts where `+filter, args...)
	if err != nil {
		return nil, fmt.Errorf("read the first input days the rebuild found: %w", err)
	}
	firsts := map[string]map[evalKey]time.Time{}
	var one string
	var key evalKey
	var day time.Time
	if _, err := pgx.ForEachRow(rows, []any{&one, &key.eval, &key.deployRepo, &day}, func() error {
		if firsts[one] == nil {
			firsts[one] = map[evalKey]time.Time{}
		}
		firsts[one][key] = measures.Midnight(day)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the first input days the rebuild found: %w", err)
	}
	return firsts, nil
}

// stagedRow is a staged day row and its op.
type stagedRow struct {
	op  string
	row DayRow
}

// stagedMember is a staged member: its key, where the rebuild counts it (nil for op `d`), and the
// day the stored member stood on when it was staged (nil for none).
type stagedMember struct {
	op   string
	key  memberKey
	at   *memberAt
	from *time.Time
}

// stageDays stages what days, one family's days of rb as the rebuild built them, differ from the
// stored rows and members, in one transaction: it deletes what the rebuild had staged of those days
// (a batch walked again, or a day a page wrote and the flip's pass builds again), stages each built
// row whose stored row holds other values or is missing (`u`), each stored row of a day the days no
// longer hold (`d`), each built member whose stored member stands elsewhere or is missing (`u`,
// from the day it stood on), and each stored member of a day that no day of the batch counts any
// more (`d`, which a `u` of another day staged before outranks). It records progress (where it is
// not nil) and drops verified, the days it built again, from the queue of days to verify. Only
// the refresher writes the computation's rows and members, one pass at a time
// (RefresherLease), so what it reads before the transaction is what stands at its commit.
func (r *refresh) stageDays(ctx context.Context, rb *rebuildState, family dayFamily, days []ProjectDay, progress *walkProgress, verified []projectDay) error {
	began := time.Now()
	pool := r.store.database.Pool
	byProject := map[string][]time.Time{}
	for _, day := range days {
		byProject[day.Project] = append(byProject[day.Project], measures.Midnight(day.Day))
	}
	var rows []stagedRow
	var members []stagedMember
	for _, project := range slices.Sorted(maps.Keys(byProject)) {
		dates := byProject[project]
		evalIDs := make([]string, len(family.slugs))
		for i, slug := range family.slugs {
			evalIDs[i] = EvalID(project, slug)
		}
		read, err := pool.Query(ctx, `
			select `+dayColumns+` from eval_days
			where project_key = $1 and writer = $2 and eval_id = any($3) and day = any($4::date[])
		`, project, writerComputation, evalIDs, dates)
		if err != nil {
			return fmt.Errorf("read the stored rows of %s a rebuild built: %w", project, err)
		}
		held, err := pgx.CollectRows(read, func(row pgx.CollectableRow) (DayRow, error) { return scanDay(row) })
		if err != nil {
			return fmt.Errorf("read the stored rows of %s a rebuild built: %w", project, err)
		}
		onDays, err := r.store.membersOn(ctx, project, family.families, dates)
		if err != nil {
			return err
		}
		var builtRows [][]DayRow
		var built []memberKey
		builtAt := map[memberKey]memberAt{}
		for _, day := range days {
			if day.Project != project {
				continue
			}
			builtRows = append(builtRows, day.Rows)
			for _, member := range day.Members {
				key := memberKey{project: project, family: member.Family, deployRepo: member.DeployRepo, rowKey: member.RowKey}
				if _, twice := builtAt[key]; !twice {
					built = append(built, key)
				}
				builtAt[key] = memberAt{day: measures.Midnight(day.Day), repo: member.Repo, shippedBy: member.ShippedBy}
			}
		}
		moved, gone := diffDayRows(held, builtRows...)
		for _, row := range moved {
			rows = append(rows, stagedRow{op: "u", row: *row})
		}
		for _, row := range gone {
			rows = append(rows, stagedRow{op: "d", row: row})
		}
		// A member built that stands on one of the days is among onDays already, by its primary key;
		// only the others are read where they stand.
		var elsewhere []memberKey
		for _, key := range built {
			if _, held := onDays[key]; !held {
				elsewhere = append(elsewhere, key)
			}
		}
		standing, err := r.store.membersByKey(ctx, elsewhere)
		if err != nil {
			return err
		}
		for _, key := range built {
			at := builtAt[key]
			was, found := onDays[key]
			if !found {
				was, found = standing[key]
			}
			if found && was.day.Equal(at.day) && was.repo == at.repo && was.shippedBy == at.shippedBy {
				continue
			}
			one := stagedMember{op: "u", key: key, at: &at}
			if found {
				one.from = &was.day
			}
			members = append(members, one)
		}
		for _, key := range slices.SortedFunc(maps.Keys(onDays), compareMemberKeys) {
			if _, counted := builtAt[key]; !counted {
				members = append(members, stagedMember{op: "d", key: key, from: new(onDays[key].day)})
			}
		}
	}
	err := r.store.inTx(ctx, "a rebuild's staged rows", func(tx pgx.Tx) error {
		for _, project := range slices.Sorted(maps.Keys(byProject)) {
			evalIDs := make([]string, len(family.slugs))
			for i, slug := range family.slugs {
				evalIDs[i] = EvalID(project, slug)
			}
			if _, err := tx.Exec(ctx, `
				delete from eval_rebuild_days where rebuild_id = $1 and project_key = $2 and eval_id = any($3) and day = any($4::date[])
			`, rb.id, project, evalIDs, byProject[project]); err != nil {
				return fmt.Errorf("delete the staged rows of %s a rebuild builds again: %w", project, err)
			}
			if _, err := tx.Exec(ctx, `
				delete from eval_rebuild_members where rebuild_id = $1 and project_key = $2 and family = any($3)
					and (op = 'u' and day = any($4::date[]) or op = 'd' and from_day = any($4::date[]))
			`, rb.id, project, family.families, byProject[project]); err != nil {
				return fmt.Errorf("delete the staged members of %s a rebuild builds again: %w", project, err)
			}
		}
		if err := stageRows(ctx, tx, rb, rows); err != nil {
			return err
		}
		if err := stageMembers(ctx, tx, rb.id, members); err != nil {
			return err
		}
		if progress != nil {
			if err := recordRebuildProgress(ctx, tx, rb.id, *progress); err != nil {
				return err
			}
		}
		if err := dropVerified(ctx, tx, rb.id, family.walk, verified); err != nil {
			return err
		}
		return touchRebuild(ctx, tx, rb.id)
	})
	if err != nil {
		return err
	}
	slog.Debug("dispatch: the eval day-row refresher staged a rebuild's days", "family", family.what, "days", len(days),
		"rows", len(rows), "members", len(members), "transaction_ms", time.Since(began).Milliseconds())
	return nil
}

// membersOn is every stored member of project of families counted on one of days, by its key.
func (s *Store) membersOn(ctx context.Context, project string, families []string, days []time.Time) (map[memberKey]memberAt, error) {
	rows, err := s.database.Pool.Query(ctx, `
		select family, deploy_repo, row_key, day, repo, shipped_by from eval_day_members
		where project_key = $1 and family = any($2) and day = any($3::date[])
	`, project, families, days)
	if err != nil {
		return nil, fmt.Errorf("read the members of %s a rebuild built: %w", project, err)
	}
	held := map[memberKey]memberAt{}
	key := memberKey{project: project}
	var at memberAt
	if _, err := pgx.ForEachRow(rows, []any{&key.family, &key.deployRepo, &key.rowKey, &at.day, &at.repo, &at.shippedBy}, func() error {
		at.day = measures.Midnight(at.day)
		held[key] = at
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the members of %s a rebuild built: %w", project, err)
	}
	return held, nil
}

// membersByKey is where each of keys stands stored, by its primary key, in statements of at most
// statementBytes of parameters.
func (s *Store) membersByKey(ctx context.Context, keys []memberKey) (map[memberKey]memberAt, error) {
	held := map[memberKey]memberAt{}
	size := func(key *memberKey) int {
		return 4*4 + len(key.project) + len(key.family) + len(key.deployRepo) + len(key.rowKey)
	}
	for chunk := range statementChunks(keys, paramBytes(4), size) {
		projects, families, deployRepos, rowKeys := make([]string, len(chunk)), make([]string, len(chunk)), make([]string, len(chunk)), make([]string, len(chunk))
		for i, key := range chunk {
			projects[i], families[i], deployRepos[i], rowKeys[i] = key.project, key.family, key.deployRepo, key.rowKey
		}
		rows, err := s.database.Pool.Query(ctx, `
			select stored.project_key, stored.family, stored.deploy_repo, stored.row_key, stored.day, stored.repo, stored.shipped_by
			from unnest($1::text[], $2::text[], $3::text[], $4::text[]) as given(project_key, family, deploy_repo, row_key)
			join eval_day_members stored on stored.project_key = given.project_key and stored.family = given.family
				and stored.deploy_repo = given.deploy_repo and stored.row_key = given.row_key
		`, projects, families, deployRepos, rowKeys)
		if err != nil {
			return nil, fmt.Errorf("read where %d members a rebuild built stand: %w", len(chunk), err)
		}
		var key memberKey
		var at memberAt
		if _, err := pgx.ForEachRow(rows, []any{&key.project, &key.family, &key.deployRepo, &key.rowKey, &at.day, &at.repo, &at.shippedBy}, func() error {
			at.day = measures.Midnight(at.day)
			held[key] = at
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read where %d members a rebuild built stand: %w", len(chunk), err)
		}
	}
	return held, nil
}

// stageRowsStatement stages day rows as writeDayRows' statement writes them (dayRowRuns' arguments,
// the writer's place, $18, holding the rebuild's id), each with op $22. A row staged again takes
// the values and the moment of the later staging.
const stageRowsStatement = `
	insert into eval_rebuild_days (rebuild_id, op, project_key, eval_id, deploy_repo, series, repo, day, listed, n, sum, num, den, max,
		samples, last_value, last_at, last_recorded_at, last_reading, counts, via_repository, built_by)
	select $18::bigint, $22, project_key, eval_id, deploy_repo, series, repo, day, listed, n, sum, num, den, max,
		samples::double precision[], last_value, last_at, last_recorded_at, nullif(last_reading, '')::uuid, counts::jsonb, via_repository, $17
	from unnest($1::text[], $2::text[], $21::text[], $3::text[], $4::text[], $5::date[], $6::bool[], $7::int[],
		$8::double precision[], $9::double precision[], $10::double precision[], $11::double precision[], $12::text[],
		$13::double precision[], $14::timestamptz[], $19::timestamptz[], $20::text[], $15::text[], $16::int[])
		as given(project_key, eval_id, deploy_repo, series, repo, day, listed, n, sum, num, den, max, samples,
			last_value, last_at, last_recorded_at, last_reading, counts, via_repository)
	on conflict (rebuild_id, project_key, eval_id, deploy_repo, series, repo, day) do update
	set op = excluded.op, listed = excluded.listed, n = excluded.n, sum = excluded.sum, num = excluded.num, den = excluded.den,
		max = excluded.max, samples = excluded.samples, last_value = excluded.last_value, last_at = excluded.last_at,
		last_recorded_at = excluded.last_recorded_at, last_reading = excluded.last_reading, counts = excluded.counts,
		via_repository = excluded.via_repository, built_by = excluded.built_by, updated_at = excluded.updated_at`

// stageRows stages rows in tx, those of each op in statements of at most statementBytes of
// parameters (dayRowRuns), each a `u` row with rb's build, as the flip writes it. The owner key of
// each `u` row is recorded first (recordOwners), so the rebuild's fold reads its staged rows.
func stageRows(ctx context.Context, tx pgx.Tx, rb *rebuildState, rows []stagedRow) error {
	var owners []string
	for _, one := range rows {
		if one.op == "u" && isOwner(one.row.ProjectKey) {
			owners = append(owners, one.row.ProjectKey)
		}
	}
	if err := recordOwners(ctx, tx, owners); err != nil {
		return err
	}
	for _, op := range []string{"u", "d"} {
		var of []DayRow
		for _, one := range rows {
			if one.op == op {
				of = append(of, one.row)
			}
		}
		if len(of) == 0 {
			continue
		}
		runs, err := dayRowRuns(of, rb.build, true, paramBytes(dayRowArrays, rb.build, writerComputation, op))
		if err != nil {
			return err
		}
		for args := range runs {
			args[17] = rb.id
			if _, err := tx.Exec(ctx, stageRowsStatement, append(args, op)...); err != nil {
				return fmt.Errorf("stage %d day rows of a rebuild: %w", len(of), err)
			}
		}
	}
	return nil
}

// stageMembers stages members in tx, in statements of at most statementBytes of parameters: a `u`
// member outranks a `d` one of its key, which a later `d` leaves as it stands.
func stageMembers(ctx context.Context, tx pgx.Tx, rb int64, members []stagedMember) error {
	size := func(one *stagedMember) int {
		return 9*4 + len(one.op) + len(one.key.project) + len(one.key.family) + len(one.key.deployRepo) + len(one.key.rowKey) + 2*8 + memberAtBytes(one.at)
	}
	for chunk := range statementChunks(members, paramBytes(9)+12, size) {
		count := len(chunk)
		ops, projects, families, deployRepos, rowKeys := make([]string, count), make([]string, count), make([]string, count), make([]string, count), make([]string, count)
		days, from := make([]*time.Time, count), make([]*time.Time, count)
		repos, shippedBy := make([]string, count), make([]string, count)
		for i, one := range chunk {
			ops[i], projects[i], families[i], deployRepos[i], rowKeys[i] = one.op, one.key.project, one.key.family, one.key.deployRepo, one.key.rowKey
			from[i] = one.from
			if one.at != nil {
				days[i], repos[i], shippedBy[i] = new(one.at.day), one.at.repo, one.at.shippedBy
			}
		}
		if _, err := tx.Exec(ctx, `
			insert into eval_rebuild_members (rebuild_id, op, project_key, family, deploy_repo, row_key, day, repo, shipped_by, from_day)
			select $1, given.op, given.project_key, given.family, given.deploy_repo, given.row_key, given.day, given.repo, given.shipped_by, given.from_day
			from unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::date[], $8::text[], $9::text[], $10::date[])
				as given(op, project_key, family, deploy_repo, row_key, day, repo, shipped_by, from_day)
			on conflict (rebuild_id, project_key, family, deploy_repo, row_key) do update
			set op = excluded.op, day = excluded.day, repo = excluded.repo, shipped_by = excluded.shipped_by, from_day = excluded.from_day
			where excluded.op = 'u'
		`, rb, ops, projects, families, deployRepos, rowKeys, days, repos, shippedBy, from); err != nil {
			return fmt.Errorf("stage %d members of a rebuild: %w", count, err)
		}
	}
	return nil
}

// memberAtBytes is the bytes a member's place adds to stageMembers' statement: its repository and
// shipping run.
func memberAtBytes(at *memberAt) int {
	if at == nil {
		return 0
	}
	return len(at.repo) + len(at.shippedBy)
}

// recordRebuildProgress records, in tx, where a staged batch leaves its walk, and the first input
// days it found, in statements of at most statementBytes of parameters.
func recordRebuildProgress(ctx context.Context, tx pgx.Tx, rb int64, progress walkProgress) error {
	if _, err := tx.Exec(ctx, `update eval_rebuild_walks set next_day = $4 where rebuild_id = $1 and family = $2 and project_key = $3`,
		rb, progress.key.family, progress.key.project, progress.next); err != nil {
		return fmt.Errorf("record the progress of the rebuild's %s walk of %q: %w", progress.key.family, progress.key.project, err)
	}
	type first struct {
		project string
		key     evalKey
		day     time.Time
	}
	var firsts []first
	for _, project := range slices.Sorted(maps.Keys(progress.firsts)) {
		for _, key := range slices.SortedFunc(maps.Keys(progress.firsts[project]), compareEvalKeys) {
			firsts = append(firsts, first{project: project, key: key, day: progress.firsts[project][key]})
		}
	}
	size := func(one *first) int { return 4*4 + len(one.project) + len(one.key.eval) + len(one.key.deployRepo) + 8 }
	for chunk := range statementChunks(firsts, paramBytes(4, progress.key.family)+12, size) {
		projects, evalIDs, deployRepos, days := make([]string, len(chunk)), make([]string, len(chunk)), make([]string, len(chunk)), make([]time.Time, len(chunk))
		for i, one := range chunk {
			projects[i], evalIDs[i], deployRepos[i], days[i] = one.project, one.key.eval, one.key.deployRepo, one.day
		}
		if _, err := tx.Exec(ctx, `
			insert into eval_rebuild_firsts (rebuild_id, family, project_key, eval_id, deploy_repo, day)
			select $1, $2, given.project_key, given.eval_id, given.deploy_repo, given.day
			from unnest($3::text[], $4::text[], $5::text[], $6::date[]) as given(project_key, eval_id, deploy_repo, day)
			on conflict (rebuild_id, family, project_key, eval_id, deploy_repo) do update set day = excluded.day
			where eval_rebuild_firsts.day <> excluded.day
		`, rb, progress.key.family, projects, evalIDs, deployRepos, days); err != nil {
			return fmt.Errorf("record the first input days the rebuild found: %w", err)
		}
	}
	return nil
}

// dropVerified deletes, in tx, the days of family verified holds from the rebuild's queue of days
// to verify: the flip's pass built them again.
func dropVerified(ctx context.Context, tx pgx.Tx, rb int64, family string, verified []projectDay) error {
	size := func(one *projectDay) int { return 4 + len(one.project) + 4 + 8 }
	for chunk := range statementChunks(verified, paramBytes(2, family)+12, size) {
		projects, days := make([]string, len(chunk)), make([]time.Time, len(chunk))
		for i, one := range chunk {
			projects[i], days[i] = one.project, one.day
		}
		if _, err := tx.Exec(ctx, `
			delete from eval_rebuild_reverify queued using unnest($3::text[], $4::date[]) as given(project_key, day)
			where queued.rebuild_id = $1 and queued.family = $2 and queued.project_key = given.project_key and queued.day = given.day
		`, rb, family, projects, days); err != nil {
			return fmt.Errorf("drop %d verified days from the rebuild's queue: %w", len(chunk), err)
		}
	}
	return nil
}

// touchRebuild records, in tx, that the rebuild rb moved.
func touchRebuild(ctx context.Context, tx pgx.Tx, rb int64) error {
	if _, err := tx.Exec(ctx, `update eval_rebuilds set updated_at = now() where id = $1`, rb); err != nil {
		return fmt.Errorf("record that the rebuild moved: %w", err)
	}
	return nil
}

// finishRebuild is the rest of the rebuild's part of a pass, after the walks of the day rows and
// the stored figures' part (figuresPass): once the rebuild its own build runs has no day left to
// walk (rowsLeft), it folds what the pass's batch has left of its figures over the stored rows
// overlaid with its staged ones; once those are done, it builds again the days pages wrote while
// it ran (verifyQueued) and flips it (flipRebuild). Then it deletes, in short chunks, the staged
// rows of every rebuild that is not building. It answers whether the rebuild or its cleanup has
// work left.
func (r *refresh) finishRebuild(ctx context.Context, rowsLeft bool) (bool, error) {
	pending := rowsLeft
	if rb := r.rebuilding; rb != nil && rb.build == r.build && !rowsLeft {
		folding, err := r.foldRebuild(ctx, rb)
		if err != nil {
			return false, err
		}
		switch {
		case folding:
			pending = true
		default:
			moved, err := r.verifyQueued(ctx, rb)
			if err != nil {
				return false, err
			}
			if moved {
				pending = true
				break
			}
			flipped, err := r.flipRebuild(ctx, rb)
			if err != nil {
				return false, err
			}
			// A rebuild replaced since the pass read it (replaceRebuild, by a lost cursor of the
			// figures this pass) flips nothing; the one in its place is the next pass's.
			pending = pending || !flipped
			r.rebuilding = nil
		}
	}
	cleaning, err := r.cleanRebuilds(ctx)
	if err != nil {
		return false, err
	}
	return pending || cleaning, nil
}

// foldRebuild builds what the pass's batch has left of each of rb's figures walks: from the first
// end day it has not folded, or, where it is to begin, from the day after the project's first
// listed row the stored rows overlaid with the staged ones hold, it folds up to the batch's end days
// left through FigureLead days after today and stages the figures that moved (stageFigures), each
// project's part one transaction with its progress. It stops between two projects for an owed wake
// (yields). It answers whether end days are left.
func (r *refresh) foldRebuild(ctx context.Context, rb *rebuildState) (bool, error) {
	last := r.today.AddDate(0, 0, FigureLead)
	pending := false
	for _, key := range rb.walksOf(walkFigures) {
		if r.yields(ctx) {
			return true, nil
		}
		next := rb.walks[key]
		if next == nil {
			start, err := r.store.overlayFiguresStart(ctx, rb.id, key.project)
			if err != nil {
				return false, err
			}
			if start == nil {
				start = new(last.AddDate(0, 0, 1))
			}
			if start.After(last) {
				if err := r.store.inTx(ctx, "a rebuild's figures walk", func(tx pgx.Tx) error {
					return recordRebuildProgress(ctx, tx, rb.id, walkProgress{key: key, next: *start})
				}); err != nil {
					return false, err
				}
				rb.walks[key] = start
				continue
			}
			next = start
		}
		if next.After(last) {
			continue
		}
		spent := rebuildBatchKey(key)
		end, ok := r.batched.through(spent, r.batch, *next, last)
		if !ok {
			pending = true
			continue
		}
		if err := r.stageFigures(ctx, rb, key.project, *next, end, &walkProgress{key: key, next: end.AddDate(0, 0, 1)}); err != nil {
			return false, err
		}
		rb.walks[key] = new(end.AddDate(0, 0, 1))
		r.batched.spend(spent, int(end.Sub(*next)/(24*time.Hour))+1)
		r.walkBuilt++
		pending = pending || end.Before(last)
	}
	return pending, nil
}

// overlayFiguresStart is the first end day of project's figures walk of the rebuild rb: the day
// after the first listed row the stored rows or the rebuild's staged ones hold, nil where neither
// holds one.
func (s *Store) overlayFiguresStart(ctx context.Context, rb int64, project string) (*time.Time, error) {
	var first *time.Time
	if err := s.database.Pool.QueryRow(ctx, `
		select least(
			(select min(d.day) from eval_project_owners o join eval_days d on d.project_key = o.owner where o.project_key = $1 and d.listed),
			(select min(s.day) from eval_project_owners o join eval_rebuild_days s on s.project_key = o.owner
				where o.project_key = $1 and s.rebuild_id = $2 and s.op = 'u' and s.listed))
	`, project, rb).Scan(&first); err != nil {
		return nil, fmt.Errorf("read the first day row of %s a rebuild holds: %w", project, err)
	}
	if first == nil {
		return nil, nil
	}
	return new(measures.Midnight(*first).AddDate(0, 0, 1)), nil
}

// stagedRange is a staged range figure row: its op, the row the rebuild folded (nil for op `d`),
// and the stored row it was staged against (nil where none stood).
type stagedRange struct {
	op       string
	key      rangeFigureKey
	row, was *rangeFigureRow
}

// stagedDay is a staged day figure row, as stagedRange is a range's.
type stagedDay struct {
	op       string
	key      dayFigureKey
	row, was *dayFigureRow
}

// stageFigures folds every stored figure of project whose range ends on a day from first through
// last, and the day row of every day before one of those end days, as buildFigureEnds folds them,
// of every cut the stored rows or rb's staged ones hold, over the stored rows overlaid with the
// staged ones (readOverlayGroups), compares each with the stored figure, and stages, in one
// transaction, each that moved or is new (`u`) and each stored one no row holds any more (`d`),
// each beside the stored row it was staged against, after deleting what rb had staged of those
// end days and days; with progress, where it is not nil.
func (r *refresh) stageFigures(ctx context.Context, rb *rebuildState, project string, first, last time.Time, progress *walkProgress) error {
	began := time.Now()
	pool := r.store.database.Pool
	entries, err := readEntries(ctx, pool, project, nil)
	if err != nil {
		return err
	}
	byID := entriesByID(entries)
	scope := figureScope{build: rb.build, project: project, through: last}
	read := Range{From: first.AddDate(0, 0, -figureSpan), To: last}
	cuts, err := readFigureCuts(ctx, pool, project, read, nil)
	if err != nil {
		return err
	}
	staged, err := pool.Query(ctx, `
		select distinct cut.repo, s.deploy_repo from eval_project_owners o join eval_rebuild_days s on s.project_key = o.owner
		cross join lateral (select `+cutOf("s", "$2")+` as repo) cut
		where o.project_key = $2 and s.rebuild_id = $1 and s.op = 'u' and s.listed and s.day >= $3 and s.day < $4 and cut.repo is not null
	`, rb.id, project, read.From, read.To)
	if err != nil {
		return fmt.Errorf("read the cuts of %s's staged rows: %w", project, err)
	}
	more, err := pgx.CollectRows(staged, scanFigureCut)
	if err != nil {
		return fmt.Errorf("read the cuts of %s's staged rows: %w", project, err)
	}
	cuts = slices.Concat(cuts, more)
	slices.SortFunc(cuts, compareFigureCuts)
	cuts = slices.Compact(cuts)
	evalIDs := slices.Sorted(maps.Keys(byID))
	built := map[rangeFigureKey]rangeFigureRow{}
	builtDays := map[dayFigureKey]dayFigureRow{}
	for _, cut := range cuts {
		groups, err := readOverlayGroups(ctx, pool, rb.id, scope, byID, evalIDs, cut, read)
		if err != nil {
			return err
		}
		for _, evalID := range slices.Sorted(maps.Keys(groups)) {
			groups[evalID].foldEnds(evalID, cut, first, last, func(row rangeFigureRow) { built[row.rangeFigureKey] = row },
				func(row dayFigureRow) { builtDays[row.dayFigureKey] = row })
		}
	}
	storedRanges, storedDays, err := readStoredFigures(ctx, pool, project, evalIDs, first, last)
	if err != nil {
		return err
	}
	var ranges []stagedRange
	for _, key := range slices.SortedFunc(maps.Keys(built), compareRangeKeys) {
		row := built[key]
		was, held := storedRanges[key]
		if held && was.key == row.key && bytes.Equal(was.figures, row.figures) && was.oldest.Equal(row.oldest) {
			continue
		}
		one := stagedRange{op: "u", key: key, row: &row}
		if held {
			one.was = &was
		}
		ranges = append(ranges, one)
	}
	for _, key := range slices.SortedFunc(maps.Keys(storedRanges), compareRangeKeys) {
		if _, kept := built[key]; !kept {
			was := storedRanges[key]
			ranges = append(ranges, stagedRange{op: "d", key: key, was: &was})
		}
	}
	var days []stagedDay
	for _, key := range slices.SortedFunc(maps.Keys(builtDays), compareDayFigureKeys) {
		row := builtDays[key]
		was, held := storedDays[key]
		if held && was.key == row.key && bytes.Equal(was.figures, row.figures) && was.stamp == row.stamp {
			continue
		}
		one := stagedDay{op: "u", key: key, row: &row}
		if held {
			one.was = &was
		}
		days = append(days, one)
	}
	for _, key := range slices.SortedFunc(maps.Keys(storedDays), compareDayFigureKeys) {
		if _, kept := builtDays[key]; !kept {
			was := storedDays[key]
			days = append(days, stagedDay{op: "d", key: key, was: &was})
		}
	}
	if err := r.store.inTx(ctx, "a rebuild's staged figures", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `delete from eval_rebuild_range_figures where rebuild_id = $1 and project_key = $2 and end_day >= $3 and end_day <= $4`,
			rb.id, project, first, last); err != nil {
			return fmt.Errorf("delete the staged range figures of %s a rebuild folds again: %w", project, err)
		}
		if _, err := tx.Exec(ctx, `delete from eval_rebuild_day_figures where rebuild_id = $1 and project_key = $2 and day >= $3 and day < $4`,
			rb.id, project, first.AddDate(0, 0, -1), last); err != nil {
			return fmt.Errorf("delete the staged day figures of %s a rebuild folds again: %w", project, err)
		}
		if err := stageRanges(ctx, tx, rb.id, project, ranges); err != nil {
			return err
		}
		if err := stageDayFigures(ctx, tx, rb.id, project, days); err != nil {
			return err
		}
		if progress != nil {
			if err := recordRebuildProgress(ctx, tx, rb.id, *progress); err != nil {
				return err
			}
		}
		return touchRebuild(ctx, tx, rb.id)
	}); err != nil {
		return err
	}
	slog.Debug("dispatch: the eval day-row refresher staged a rebuild's figures", "project", project, "first_end", first.Format(time.DateOnly),
		"last_end", last.Format(time.DateOnly), "ranges", len(ranges), "days", len(days), "transaction_ms", time.Since(began).Milliseconds())
	return nil
}

// readStoredFigures reads project's stored range figures ending first through last and its stored
// day figures of the evals evalIDs of the days before them, by their keys: evalIDs, every eval of
// the project, which no route bounds, in statements of at most statementBytes.
func readStoredFigures(ctx context.Context, q delivery.Querier, project string, evalIDs []string, first, last time.Time) (map[rangeFigureKey]rangeFigureRow, map[dayFigureKey]dayFigureRow, error) {
	rows, err := q.Query(ctx, `
		select repo, days, end_day, eval_id, deploy_repo, fold_key, figures, oldest_updated_at from eval_range_figures
		where project_key = $1 and end_day >= $2 and end_day <= $3
	`, project, first, last)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s's stored range figures: %w", project, err)
	}
	ranges := map[rangeFigureKey]rangeFigureRow{}
	var one rangeFigureRow
	var n int32
	if _, err := pgx.ForEachRow(rows, []any{&one.repo, &n, &one.end, &one.eval, &one.deployRepo, &one.key, &one.figures, &one.oldest}, func() error {
		one.days, one.end, one.oldest = int(n), one.end.UTC(), one.oldest.UTC()
		ranges[one.rangeFigureKey] = one
		one.figures = nil
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("read %s's stored range figures: %w", project, err)
	}
	days := map[dayFigureKey]dayFigureRow{}
	for chunk := range statementChunks(evalIDs, paramBytes(1, project)+2*(4+8), textBytes) {
		rows, err = q.Query(ctx, `
			select repo, day, eval_id, deploy_repo, fold_key, figures, rows_stamp from eval_day_figures
			where project_key = $1 and eval_id = any($4) and day >= $2 and day < $3
		`, project, first.AddDate(0, 0, -1), last, chunk)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s's stored day figures: %w", project, err)
		}
		var day dayFigureRow
		if _, err := pgx.ForEachRow(rows, []any{&day.repo, &day.day, &day.eval, &day.deployRepo, &day.key, &day.figures, &day.stamp}, func() error {
			day.day = day.day.UTC()
			days[day.dayFigureKey] = day
			day.figures = nil
			return nil
		}); err != nil {
			return nil, nil, fmt.Errorf("read %s's stored day figures: %w", project, err)
		}
	}
	return ranges, days, nil
}

// stageRanges stages ranges, project's range figures of the rebuild rb, in tx, in statements of
// at most statementBytes of parameters.
func stageRanges(ctx context.Context, tx pgx.Tx, rb int64, project string, ranges []stagedRange) error {
	size := func(one *stagedRange) int {
		n := 13*4 + len(one.op) + len(one.key.repo) + len(one.key.eval) + len(one.key.deployRepo) + 2*8 + 1
		if one.row != nil {
			n += len(one.row.figures) + 2*8
		}
		if one.was != nil {
			n += len(one.was.figures) + 2*8
		}
		return n
	}
	for chunk := range statementChunks(ranges, paramBytes(13, project)+12, size) {
		count := len(chunk)
		ops, repos, evalIDs, deployRepos := make([]string, count), make([]string, count), make([]string, count), make([]string, count)
		ns, ends, held := make([]int32, count), make([]time.Time, count), make([]bool, count)
		keys, wasKeys := make([]*int64, count), make([]*int64, count)
		figures, wasFigures := make([][]byte, count), make([][]byte, count)
		oldest, wasOldest := make([]*time.Time, count), make([]*time.Time, count)
		for i, one := range chunk {
			ops[i], repos[i], evalIDs[i], deployRepos[i] = one.op, one.key.repo, one.key.eval, one.key.deployRepo
			ns[i], ends[i], held[i] = int32(one.key.days), one.key.end, one.was != nil
			if one.row != nil {
				keys[i], figures[i], oldest[i] = new(one.row.key), one.row.figures, new(one.row.oldest)
			}
			if one.was != nil {
				wasKeys[i], wasFigures[i], wasOldest[i] = new(one.was.key), one.was.figures, new(one.was.oldest)
			}
		}
		if _, err := tx.Exec(ctx, `
			insert into eval_rebuild_range_figures (rebuild_id, op, project_key, repo, days, end_day, eval_id, deploy_repo, fold_key, figures,
				oldest_updated_at, held, was_fold_key, was_figures, was_oldest_updated_at)
			select $1, given.op, $2, given.repo, given.days, given.end_day, given.eval_id, given.deploy_repo, given.fold_key, given.figures,
				given.oldest, given.held, given.was_key, given.was_figures, given.was_oldest
			from unnest($3::text[], $4::text[], $5::int[], $6::date[], $7::text[], $8::text[], $9::bigint[], $10::bytea[], $11::timestamptz[],
				$12::bool[], $13::bigint[], $14::bytea[], $15::timestamptz[])
				as given(op, repo, days, end_day, eval_id, deploy_repo, fold_key, figures, oldest, held, was_key, was_figures, was_oldest)
		`, rb, project, ops, repos, ns, ends, evalIDs, deployRepos, keys, figures, oldest, held, wasKeys, wasFigures, wasOldest); err != nil {
			return fmt.Errorf("stage %d range figures of %s: %w", count, project, err)
		}
	}
	return nil
}

// stageDayFigures stages days, project's day figures of the rebuild rb, in tx, in statements of at
// most statementBytes of parameters.
func stageDayFigures(ctx context.Context, tx pgx.Tx, rb int64, project string, days []stagedDay) error {
	size := func(one *stagedDay) int {
		n := 12*4 + len(one.op) + len(one.key.repo) + len(one.key.eval) + len(one.key.deployRepo) + 8 + 1
		if one.row != nil {
			n += len(one.row.figures) + 2*8
		}
		if one.was != nil {
			n += len(one.was.figures) + 2*8
		}
		return n
	}
	for chunk := range statementChunks(days, paramBytes(12, project)+12, size) {
		count := len(chunk)
		ops, repos, evalIDs, deployRepos := make([]string, count), make([]string, count), make([]string, count), make([]string, count)
		dates, held := make([]time.Time, count), make([]bool, count)
		keys, wasKeys, stamps, wasStamps := make([]*int64, count), make([]*int64, count), make([]*int64, count), make([]*int64, count)
		figures, wasFigures := make([][]byte, count), make([][]byte, count)
		for i, one := range chunk {
			ops[i], repos[i], evalIDs[i], deployRepos[i] = one.op, one.key.repo, one.key.eval, one.key.deployRepo
			dates[i], held[i] = one.key.day, one.was != nil
			if one.row != nil {
				keys[i], figures[i], stamps[i] = new(one.row.key), one.row.figures, new(one.row.stamp)
			}
			if one.was != nil {
				wasKeys[i], wasFigures[i], wasStamps[i] = new(one.was.key), one.was.figures, new(one.was.stamp)
			}
		}
		if _, err := tx.Exec(ctx, `
			insert into eval_rebuild_day_figures (rebuild_id, op, project_key, repo, day, eval_id, deploy_repo, fold_key, figures, rows_stamp,
				held, was_fold_key, was_figures, was_rows_stamp)
			select $1, given.op, $2, given.repo, given.day, given.eval_id, given.deploy_repo, given.fold_key, given.figures, given.stamp,
				given.held, given.was_key, given.was_figures, given.was_stamp
			from unnest($3::text[], $4::text[], $5::date[], $6::text[], $7::text[], $8::bigint[], $9::bytea[], $10::bigint[],
				$11::bool[], $12::bigint[], $13::bytea[], $14::bigint[])
				as given(op, repo, day, eval_id, deploy_repo, fold_key, figures, stamp, held, was_key, was_figures, was_stamp)
		`, rb, project, ops, repos, dates, evalIDs, deployRepos, keys, figures, stamps, held, wasKeys, wasFigures, wasStamps); err != nil {
			return fmt.Errorf("stage %d day figures of %s: %w", count, project, err)
		}
	}
	return nil
}

// readOverlayGroups is readFigureGroups over the stored rows overlaid with the rebuild rb's staged
// ones: each stored row of a computed eval a staged row of its key stands for is left out, and
// each staged `u` row that is listed takes its place or is added, its via_repository as the project
// reads it (viaAsRead), as DayWindowIn reads a stored row's. A logged eval's rows are the
// readings write's, which no rebuild stages. A cut's rows hold one row of each owner (the
// project's own key, or an owner key it reads, owners.go), eval, series, repository and day: a
// staged row of an owner key stands for that owner's row alone, beside the project's own row of
// the same series and day, and a staged row of the project's default repository for that one
// alone, beside its own row of the same series and day stored under legionfacts.DefaultRepo, which
// the default's cut folds too (cutOf); so the stored rows are read with the repository each stands
// under (WindowRow.repo: DayWindowIn answers it in the read of a repository's cut, and every row of
// the project's own cut stands under ""), and a staged row stands for the stored row of its whole
// key. The staged rows come after the stored ones, and newFigureGroup sorts the group into the
// folds' one order (sortWindowRows), so each is folded where a read of the rows the flip leaves
// folds it.
func readOverlayGroups(ctx context.Context, q delivery.Querier, rb int64, scope figureScope, entries map[string]Entry, evalIDs []string, cut figureCut, r Range) (map[string]*figureGroup, error) {
	groups := map[string]*figureGroup{}
	var read []Entry
	var computed []string
	for _, evalID := range evalIDs {
		if entry, held := entries[evalID]; held && KeyedByDeployRepo(entry.Definition) == (cut.deployRepo != "") {
			read = append(read, entry)
			if entry.Definition.Computed() {
				computed = append(computed, evalID)
			}
		}
	}
	if len(read) == 0 || !r.From.Before(r.To) {
		return groups, nil
	}
	live, err := DayWindowIn(ctx, q, []string{scope.project}, read, DeployReposOf(scope.project, cut.deployRepo), cut.repos(), r, nil)
	if err != nil {
		return nil, err
	}
	type rowKey struct {
		owner, repo, eval, series string
		day                       time.Time
	}
	shadowed := map[rowKey]bool{}
	var added []WindowRow
	if len(computed) > 0 {
		rows, err := q.Query(ctx, `
			select s.project_key, s.repo, s.op, s.listed, `+evalIDAs("o.project_key", "s.eval_id", "o.owner")+`, s.series, s.day, s.n, s.sum, s.num,
				s.den, s.max, s.samples, s.last_value, s.last_at, nullif(s.counts, '{}'), `+viaAsRead("s.via_repository")+`, s.updated_at
			from eval_project_owners o join eval_rebuild_days s on s.project_key = o.owner
			where o.project_key = $2 and s.rebuild_id = $1 and `+evalIDAs("o.project_key", "s.eval_id", "o.owner")+` = any($3)
				and `+repoCut("s", "$7::text[]", "$2")+` and s.deploy_repo = $4 and s.day >= $5::date and s.day < $6::date
		`, rb, scope.project, computed, cut.deployRepo, r.From.UTC(), r.To.UTC(), []string{cut.repo})
		if err != nil {
			return nil, fmt.Errorf("read the staged rows of %s: %w", scope.project, err)
		}
		var op, owner, repo string
		var listed bool
		var row WindowRow
		part := &row.Part
		if _, err := pgx.ForEachRow(rows, []any{&owner, &repo, &op, &listed, &row.EvalID, &part.Series, &part.Day, &part.N, &part.Sum, &part.Num, &part.Den,
			&part.Max, &part.Samples, &part.LastValue, &part.LastAt, &part.Counts, &part.ViaRepository, &row.UpdatedAt}, func() error {
			row.Range, row.UpdatedAt, row.owner, row.repo = RangeWindow, row.UpdatedAt.UTC(), owner, repo
			part.LastRecordedAt, part.LastReading = nil, ""
			utcPart(part)
			shadowed[rowKey{owner: owner, repo: repo, eval: row.EvalID, series: part.Series, day: part.Day}] = true
			if op == "u" && listed {
				added = append(added, row)
			}
			row = WindowRow{}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the staged rows of %s: %w", scope.project, err)
		}
	}
	byEval := map[string][]WindowRow{}
	for _, row := range live {
		if !shadowed[rowKey{owner: row.owner, repo: row.repo, eval: row.EvalID, series: row.Part.Series, day: row.Part.Day}] {
			byEval[row.EvalID] = append(byEval[row.EvalID], row)
		}
	}
	for _, row := range added {
		byEval[row.EvalID] = append(byEval[row.EvalID], row)
	}
	for evalID, held := range byEval {
		entry := entries[evalID]
		groups[evalID] = newFigureGroup(entry, foldKeyOf(entry), held)
	}
	return groups, nil
}

// verifyQueued builds again, by rb's rules, every day a statement wrote in the open while rb ran
// and after rb had built it (eval_rebuild_reverify, which the triggers of the day rows and the
// members fill whatever process's statement it was), and stages what differs from the rows that
// stand (stageDays), replacing what it had staged of the day; then folds again the figures of the
// ranges that hold those days, and of every end day whose stored figures a statement wrote
// (stageFigures): so the flip neither undoes a write in the open nor writes it twice. A day whose
// input moves a first input day rb found (one before it, or a first input day that holds no input
// of it any more) walks rb back to it instead (walkBack), and the days after it are built by the
// walk again: it answers whether one did.
func (r *refresh) verifyQueued(ctx context.Context, rb *rebuildState) (bool, error) {
	rows, err := r.store.database.Pool.Query(ctx, `
		select family, project_key, day from eval_rebuild_reverify where rebuild_id = $1 order by family, project_key, day
	`, rb.id)
	if err != nil {
		return false, fmt.Errorf("read the days the rebuild builds again: %w", err)
	}
	queued := map[string][]projectDay{}
	var family string
	var one projectDay
	if _, err := pgx.ForEachRow(rows, []any{&family, &one.project, &one.day}, func() error {
		one.day = measures.Midnight(one.day)
		queued[family] = append(queued[family], one)
		return nil
	}); err != nil {
		return false, fmt.Errorf("read the days the rebuild builds again: %w", err)
	}
	if len(queued) == 0 {
		return false, nil
	}
	moved := false
	spans := map[string]daySpan{}
	for _, family := range []dayFamily{deliveryFamily, legionFamily} {
		days := queued[family.walk]
		if len(days) == 0 {
			continue
		}
		named := newPageDays(family)
		for _, one := range days {
			switch {
			case family.walk == walkDelivery && !r.delivers():
				// No project has delivery on, no delivery row: the day holds none.
				named.add(&namedDay{project: one.project, day: one.day, raw: ProjectDay{Project: one.project, Day: one.day}})
			case family.walk == walkDelivery:
				read, err := r.readDelivery(ctx, one.day, nil, nil)
				if err != nil {
					return false, err
				}
				r.nameDelivery(named, read, one.project)
			default:
				if err := r.nameLegion(ctx, named, one.project, one.day); err != nil {
					return false, err
				}
			}
		}
		firsts, err := r.store.rebuildFirsts(ctx, rb.id, family.walk, "")
		if err != nil {
			return false, err
		}
		back := map[string]time.Time{}
		for _, key := range named.order {
			day := named.days[key]
			input, first := inputKeys(day.raw.Rows), firsts[day.project]
			shifts := false
			for key := range input {
				if since, found := first[key]; !found || day.day.Before(since) {
					shifts = true
				}
			}
			for key, since := range first {
				if since.Equal(day.day) && !input[key] {
					shifts = true
				}
			}
			walk := walkOf(family.walk, day.project).project
			if at, held := back[walk]; shifts && (!held || day.day.Before(at)) {
				back[walk] = day.day
			}
		}
		if len(back) > 0 {
			if err := r.walkBack(ctx, rb, family, back); err != nil {
				return false, err
			}
			moved = true
			continue
		}
		built := make([]ProjectDay, len(named.order))
		for i, key := range named.order {
			day := named.days[key]
			built[i] = day.finalize(firsts[day.project], true)
			if _, folds := rb.walks[walkKey{family: walkFigures, project: day.project}]; folds {
				spans[day.project] = spans[day.project].widen(day.day)
			}
		}
		if err := r.stageDays(ctx, rb, family, built, nil, days); err != nil {
			return false, err
		}
	}
	if moved {
		return true, nil
	}
	last := r.today.AddDate(0, 0, FigureLead)
	ends := map[string]daySpan{}
	for project, span := range spans {
		ends[project] = daySpan{first: span.first.AddDate(0, 0, 1), last: span.last.AddDate(0, 0, figureSpan)}
	}
	for _, one := range queued[walkFigures] {
		if span, held := ends[one.project]; held {
			ends[one.project] = span.widen(one.day)
		} else {
			ends[one.project] = daySpan{first: one.day, last: one.day}
		}
	}
	for _, project := range slices.Sorted(maps.Keys(ends)) {
		span := ends[project]
		if span.last.After(last) {
			span.last = last
		}
		if !span.first.After(span.last) {
			if err := r.stageFigures(ctx, rb, project, span.first, span.last, nil); err != nil {
				return false, err
			}
		}
	}
	if len(queued[walkFigures]) > 0 {
		if err := r.store.inTx(ctx, "the rebuild's queue of end days", func(tx pgx.Tx) error {
			return dropVerified(ctx, tx, rb.id, walkFigures, queued[walkFigures])
		}); err != nil {
			return false, err
		}
	}
	return false, nil
}

// walkBack moves rb's walks of family back to the days back holds, by the walk's project (the
// empty string for the delivery walk), in one transaction: each walk stands at its day, forgets
// the first input days it found from it on and the queued days from it on, which it builds again,
// and the figures walk of each project whose rows it walks again begins again.
func (r *refresh) walkBack(ctx context.Context, rb *rebuildState, family dayFamily, back map[string]time.Time) error {
	projects := []string{}
	for walk := range back {
		if walk == "" {
			projects = slices.Clone(r.projects)
			break
		}
		projects = append(projects, walk)
	}
	if err := r.store.inTx(ctx, "the walk back of a rebuild", func(tx pgx.Tx) error {
		for _, walk := range slices.Sorted(maps.Keys(back)) {
			day := back[walk]
			if _, err := tx.Exec(ctx, `
				update eval_rebuild_walks set next_day = $4
				where rebuild_id = $1 and family = $2 and project_key = $3 and (next_day is null or next_day > $4)
			`, rb.id, family.walk, walk, day); err != nil {
				return fmt.Errorf("walk the rebuild's %s walk of %q back: %w", family.walk, walk, err)
			}
			for _, statement := range []string{
				`delete from eval_rebuild_firsts where rebuild_id = $1 and family = $2 and ($3 = '' or project_key = $3) and day >= $4`,
				`delete from eval_rebuild_reverify where rebuild_id = $1 and family = $2 and ($3 = '' or project_key = $3) and day >= $4`,
			} {
				if _, err := tx.Exec(ctx, statement, rb.id, family.walk, walk, day); err != nil {
					return fmt.Errorf("walk the rebuild's %s walk of %q back: %w", family.walk, walk, err)
				}
			}
		}
		if _, err := tx.Exec(ctx, `update eval_rebuild_walks set next_day = null where rebuild_id = $1 and family = $2 and project_key = any($3)`,
			rb.id, walkFigures, projects); err != nil {
			return fmt.Errorf("begin the rebuild's figures walks again: %w", err)
		}
		return touchRebuild(ctx, tx, rb.id)
	}); err != nil {
		return err
	}
	slog.Info("dispatch: a day a page wrote moved a first input day the rebuild found; the rebuild walks back", "family", family.what, "to", back)
	return nil
}

// flipStatements is the flip's statements (flipRebuild), each set-based over the staging of the
// rebuild $1 and sending nothing else: first the rebuild flipped, where it is still building, so
// the triggers that queue the days a write touches for the building rebuild (migration
// 20261117100021) queue none of the flip's own; then the staged rows, the members and the figures,
// the order every writer of the three takes them in, each written in the byte order of its keys;
// the figures each written only where the stored figure stands as it was staged (a figure a
// readings write moved since is left as it stands), their rows locked in that order first; an
// eval_days change of each eval whose figures the rebuild staged, which widens the change its rows
// logged in the transaction where they moved; and the serving records, the walks and the figures'
// marker. The refresher's lease keeps every other writer of the rebuild's record out of the
// transaction. A staged row of a project deleted since is left; an owner key's (owners.go) belongs
// to its repository, no project, and is written.
var flipStatements = []struct{ what, sql string }{
	{"rebuild flipped", `update eval_rebuilds set state = 'flipped', updated_at = now() where id = $1 and state = 'building'`},
	{"day rows written", `
		insert into eval_days (project_key, eval_id, writer, deploy_repo, series, repo, day, listed, n, sum, num, den, max, samples,
			last_value, last_at, last_recorded_at, last_reading, counts, via_repository, built_by, updated_at)
		select s.project_key, s.eval_id, '` + writerComputation + `', s.deploy_repo, s.series, s.repo, s.day, s.listed, s.n, s.sum, s.num, s.den,
			s.max, s.samples, s.last_value, s.last_at, s.last_recorded_at, s.last_reading, s.counts, s.via_repository, s.built_by, s.updated_at
		from eval_rebuild_days s
		where s.rebuild_id = $1 and s.op = 'u' and (starts_with(s.project_key, '` + ownerPrefix + `') or s.project_key in (select key from projects))
		order by s.project_key collate "C", s.eval_id collate "C", s.deploy_repo collate "C", s.series collate "C", s.repo collate "C", s.day
		on conflict (project_key, eval_id, writer, deploy_repo, series, repo, day) do update
		set listed = excluded.listed, n = excluded.n, sum = excluded.sum, num = excluded.num, den = excluded.den, max = excluded.max,
			samples = excluded.samples, last_value = excluded.last_value, last_at = excluded.last_at,
			last_recorded_at = excluded.last_recorded_at, last_reading = excluded.last_reading, counts = excluded.counts,
			via_repository = excluded.via_repository, built_by = excluded.built_by, updated_at = excluded.updated_at
		where (eval_days.listed, eval_days.n, eval_days.sum, eval_days.num, eval_days.den, eval_days.max, eval_days.samples,
				eval_days.last_value, eval_days.last_at, eval_days.last_recorded_at, eval_days.last_reading, eval_days.counts,
				eval_days.via_repository)
			is distinct from (excluded.listed, excluded.n, excluded.sum, excluded.num, excluded.den, excluded.max, excluded.samples,
				excluded.last_value, excluded.last_at, excluded.last_recorded_at, excluded.last_reading, excluded.counts,
				excluded.via_repository)`},
	{"day rows deleted", `
		delete from eval_days d using eval_rebuild_days s
		where s.rebuild_id = $1 and s.op = 'd' and d.project_key = s.project_key and d.eval_id = s.eval_id and d.writer = '` + writerComputation + `'
			and d.deploy_repo = s.deploy_repo and d.series = s.series and d.repo = s.repo and d.day = s.day`},
	{"members written", `
		insert into eval_day_members (project_key, family, deploy_repo, row_key, day, repo, shipped_by)
		select s.project_key, s.family, s.deploy_repo, s.row_key, s.day, s.repo, s.shipped_by
		from eval_rebuild_members s
		where s.rebuild_id = $1 and s.op = 'u' and (starts_with(s.project_key, '` + ownerPrefix + `') or s.project_key in (select key from projects))
		order by s.project_key collate "C", s.family collate "C", s.deploy_repo collate "C", s.row_key collate "C"
		on conflict (project_key, family, deploy_repo, row_key) do update
		set day = excluded.day, repo = excluded.repo, shipped_by = excluded.shipped_by
		where (eval_day_members.day, eval_day_members.repo, eval_day_members.shipped_by)
			is distinct from (excluded.day, excluded.repo, excluded.shipped_by)`},
	{"members deleted", `
		delete from eval_day_members m using eval_rebuild_members s
		where s.rebuild_id = $1 and s.op = 'd' and m.project_key = s.project_key and m.family = s.family
			and m.deploy_repo = s.deploy_repo and m.row_key = s.row_key and m.day = s.from_day`},
	{"range figures locked", `
		select 1 from eval_range_figures f join eval_rebuild_range_figures s on s.project_key = f.project_key and s.repo = f.repo
			and s.days = f.days and s.end_day = f.end_day and s.eval_id = f.eval_id and s.deploy_repo = f.deploy_repo
		where s.rebuild_id = $1
		order by f.project_key collate "C", f.repo collate "C", f.days, f.end_day, f.eval_id collate "C", f.deploy_repo collate "C"
		for update of f`},
	{"range figures written", `
		update eval_range_figures f
		set fold_key = s.fold_key, figures = s.figures, oldest_updated_at = s.oldest_updated_at, built_by = b.built_by
		from eval_rebuild_range_figures s join eval_rebuilds b on b.id = s.rebuild_id
		where s.rebuild_id = $1 and s.op = 'u' and s.held and f.project_key = s.project_key and f.repo = s.repo and f.days = s.days
			and f.end_day = s.end_day and f.eval_id = s.eval_id and f.deploy_repo = s.deploy_repo
			and (f.fold_key, f.figures, f.oldest_updated_at) = (s.was_fold_key, s.was_figures, s.was_oldest_updated_at)`},
	{"range figures made", `
		insert into eval_range_figures (project_key, repo, days, end_day, eval_id, deploy_repo, fold_key, figures, oldest_updated_at, built_by)
		select s.project_key, s.repo, s.days, s.end_day, s.eval_id, s.deploy_repo, s.fold_key, s.figures, s.oldest_updated_at, b.built_by
		from eval_rebuild_range_figures s join eval_rebuilds b on b.id = s.rebuild_id join projects p on p.key = s.project_key
		where s.rebuild_id = $1 and s.op = 'u' and not s.held
		order by s.project_key collate "C", s.repo collate "C", s.days, s.end_day, s.eval_id collate "C", s.deploy_repo collate "C"
		on conflict (project_key, repo, days, end_day, eval_id, deploy_repo) do nothing`},
	{"range figures deleted", `
		delete from eval_range_figures f using eval_rebuild_range_figures s
		where s.rebuild_id = $1 and s.op = 'd' and f.project_key = s.project_key and f.repo = s.repo and f.days = s.days
			and f.end_day = s.end_day and f.eval_id = s.eval_id and f.deploy_repo = s.deploy_repo
			and (f.fold_key, f.figures, f.oldest_updated_at) = (s.was_fold_key, s.was_figures, s.was_oldest_updated_at)`},
	{"day figures locked", `
		select 1 from eval_day_figures f join eval_rebuild_day_figures s on s.project_key = f.project_key and s.repo = f.repo
			and s.day = f.day and s.eval_id = f.eval_id and s.deploy_repo = f.deploy_repo
		where s.rebuild_id = $1
		order by f.project_key collate "C", f.repo collate "C", f.day, f.eval_id collate "C", f.deploy_repo collate "C"
		for update of f`},
	{"day figures written", `
		update eval_day_figures f
		set fold_key = s.fold_key, figures = s.figures, rows_stamp = s.rows_stamp, built_by = b.built_by
		from eval_rebuild_day_figures s join eval_rebuilds b on b.id = s.rebuild_id
		where s.rebuild_id = $1 and s.op = 'u' and s.held and f.project_key = s.project_key and f.repo = s.repo and f.day = s.day
			and f.eval_id = s.eval_id and f.deploy_repo = s.deploy_repo
			and (f.fold_key, f.figures, f.rows_stamp) = (s.was_fold_key, s.was_figures, s.was_rows_stamp)`},
	{"day figures made", `
		insert into eval_day_figures (project_key, repo, day, eval_id, deploy_repo, fold_key, figures, rows_stamp, built_by)
		select s.project_key, s.repo, s.day, s.eval_id, s.deploy_repo, s.fold_key, s.figures, s.rows_stamp, b.built_by
		from eval_rebuild_day_figures s join eval_rebuilds b on b.id = s.rebuild_id join projects p on p.key = s.project_key
		where s.rebuild_id = $1 and s.op = 'u' and not s.held
		order by s.project_key collate "C", s.repo collate "C", s.day, s.eval_id collate "C", s.deploy_repo collate "C"
		on conflict (project_key, repo, day, eval_id, deploy_repo) do nothing`},
	{"day figures deleted", `
		delete from eval_day_figures f using eval_rebuild_day_figures s
		where s.rebuild_id = $1 and s.op = 'd' and f.project_key = s.project_key and f.repo = s.repo and f.day = s.day
			and f.eval_id = s.eval_id and f.deploy_repo = s.deploy_repo
			and (f.fold_key, f.figures, f.rows_stamp) = (s.was_fold_key, s.was_figures, s.was_rows_stamp)`},
	{"evals whose figures moved", `
		select eval_days_change_log(moved.project_key, moved.eval_id, moved.first_day, moved.last_day)
		from (
			select project_key, eval_id, min(first_day) as first_day, max(last_day) as last_day
			from (
				select project_key, eval_id, end_day - days as first_day, end_day - 1 as last_day from eval_rebuild_range_figures where rebuild_id = $1
				union all
				select project_key, eval_id, day, day from eval_rebuild_day_figures where rebuild_id = $1
			) spans
			where project_key in (select key from projects)
			group by project_key, eval_id
			order by project_key, eval_id
		) moved`},
	{"serving records", `
		insert into eval_days_serving (family, project_key, built_by, next_day, updated_at)
		select w.family, w.project_key, b.built_by, w.next_day, now()
		from eval_rebuild_walks w join eval_rebuilds b on b.id = w.rebuild_id
		where w.rebuild_id = $1 and w.family in ('` + walkDelivery + `', '` + walkLegion + `') and w.next_day is not null
			and (w.family = '` + walkDelivery + `' or w.project_key in (select key from projects))
		order by w.family, w.project_key
		on conflict (family, project_key) do update
		set built_by = excluded.built_by, next_day = greatest(eval_days_serving.next_day, excluded.next_day), updated_at = excluded.updated_at`},
	{"walks", `
		insert into eval_days_walks (family, project_key, built_by, next_day, updated_at)
		select w.family, w.project_key, b.built_by, w.next_day, now()
		from eval_rebuild_walks w join eval_rebuilds b on b.id = w.rebuild_id
		where w.rebuild_id = $1 and w.next_day is not null and (w.family = '` + walkDelivery + `' or w.project_key in (select key from projects))
		order by w.family, w.project_key
		on conflict (family, project_key) do update
		set built_by = excluded.built_by, next_day = excluded.next_day, updated_at = excluded.updated_at
		where eval_days_walks.next_day < excluded.next_day`},
	{"figures built", `
		insert into eval_figures_built (project_key, through, built_by, updated_at)
		select w.project_key, w.next_day - 1, b.built_by, now()
		from eval_rebuild_walks w join eval_rebuilds b on b.id = w.rebuild_id
		where w.rebuild_id = $1 and w.family = '` + walkFigures + `' and w.next_day is not null and w.project_key in (select key from projects)
		order by w.project_key
		on conflict (project_key) do update set through = excluded.through, updated_at = excluded.updated_at
		where eval_figures_built.through < excluded.through`},
}

// flipRebuild makes what rb staged visible in one transaction of flipStatements, marked a
// rebuild's (MarkRebuild), so the live hub recomputes an evals view of its eval_days changes at
// once, one change per eval: the flip of a rebuild that staged nothing writes no row, member or
// figure and logs no change, and moves the rebuild's state, the serving records, the walks and the
// figures' marker alone. The transaction first locks rb's row and reads it again: a rebuild
// replaced since the pass read it (replaceRebuild abandoned it)
// flips nothing, and flipRebuild answers false. It then moves the change log's outbox into the
// log, and logs how many rows each statement wrote and how long the transaction was open.
func (r *refresh) flipRebuild(ctx context.Context, rb *rebuildState) (bool, error) {
	began := time.Now()
	written := make([]any, 0, 2*len(flipStatements))
	building := false
	if err := r.store.inTx(ctx, "the flip of a rebuild", func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `select state = 'building' from eval_rebuilds where id = $1 for update`, rb.id).Scan(&building); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read the rebuild %d before its flip: %w", rb.id, err)
		}
		if !building {
			return nil
		}
		if err := MarkRebuild(ctx, tx); err != nil {
			return err
		}
		for _, statement := range flipStatements {
			tag, err := tx.Exec(ctx, statement.sql, rb.id)
			if err != nil {
				return fmt.Errorf("flip the rebuild (%s): %w", statement.what, err)
			}
			written = append(written, strings.ReplaceAll(statement.what, " ", "_"), tag.RowsAffected())
		}
		return nil
	}); err != nil {
		return false, err
	}
	if !building {
		slog.Info("dispatch: the eval day-row refresher flipped nothing: the rebuild was replaced before its flip",
			"rebuild", rb.id, "reason", rb.reason, "build", rb.build)
		return false, nil
	}
	open := time.Since(began)
	r.store.drainChanges(ctx)
	r.serving = nil
	slog.Info("dispatch: the eval day-row refresher flipped a rebuild",
		append([]any{"reason", rb.reason, "build", rb.build, "transaction_ms", open.Milliseconds()}, written...)...)
	return true, nil
}

// cleanRebuilds deletes the rows every rebuild that is not building holds, in each table that holds
// a rebuild's rows besides eval_rebuilds itself, a statement of at most rebuildChunk rows each, each
// a transaction of its own, so no delete holds a lock or the event horizon for long; and marks each
// rebuild whose rows are gone. Each statement names its table, so the item-row census (api's
// TestEveryWriterOfAnItemRowIsOnTheOwnerLockCensus) reads it as a write of no item row. It stops
// between two statements for an owed wake (yields), and answers whether rows are left.
func (r *refresh) cleanRebuilds(ctx context.Context) (bool, error) {
	pool := r.store.database.Pool
	var ids []int64
	if err := pool.QueryRow(ctx, `select coalesce(array_agg(id), '{}') from eval_rebuilds where state <> 'building' and staged`).Scan(&ids); err != nil {
		return false, fmt.Errorf("read the rebuilds whose staged rows are left: %w", err)
	}
	if len(ids) == 0 {
		return false, nil
	}
	for _, statement := range []string{
		`delete from eval_rebuild_days where ctid = any(array(select ctid from eval_rebuild_days where rebuild_id = any($1) limit $2))`,
		`delete from eval_rebuild_members where ctid = any(array(select ctid from eval_rebuild_members where rebuild_id = any($1) limit $2))`,
		`delete from eval_rebuild_range_figures where ctid = any(array(select ctid from eval_rebuild_range_figures where rebuild_id = any($1) limit $2))`,
		`delete from eval_rebuild_day_figures where ctid = any(array(select ctid from eval_rebuild_day_figures where rebuild_id = any($1) limit $2))`,
		`delete from eval_rebuild_firsts where ctid = any(array(select ctid from eval_rebuild_firsts where rebuild_id = any($1) limit $2))`,
		`delete from eval_rebuild_reverify where ctid = any(array(select ctid from eval_rebuild_reverify where rebuild_id = any($1) limit $2))`,
		`delete from eval_rebuild_walks where ctid = any(array(select ctid from eval_rebuild_walks where rebuild_id = any($1) limit $2))`,
	} {
		for {
			if r.yields(ctx) {
				return true, nil
			}
			tag, err := pool.Exec(ctx, statement, ids, rebuildChunk)
			if err != nil {
				return false, fmt.Errorf("delete the staged rows of %v (%s): %w", ids, statement, err)
			}
			r.walkBuilt++
			if tag.RowsAffected() < rebuildChunk {
				break
			}
		}
	}
	if _, err := pool.Exec(ctx, `update eval_rebuilds set staged = false where id = any($1)`, ids); err != nil {
		return false, fmt.Errorf("record the staged rows of %v deleted: %w", ids, err)
	}
	return false, nil
}
