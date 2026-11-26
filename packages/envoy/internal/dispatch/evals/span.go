package evals

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// span.go is how a page writes the days its changes name (dispatch://INFRA-2916, items 5 and 8,
// and R14's 2 s for every change kind): each day whole, from its own input, and, where the changes
// move what days they do not name hold, those days too, in the page's one commit.
//
// The stored rows of a day the stored rows are whole through (eval_days_serving) are a function
// of the inputs and the settings alone: the rows the day's input builds, less each lead time to
// production that counts no pull request before its series' first input day (dropUnshipped), and
// the zero row of every series the day has no input of from that series' first input day on
// (zerosFrom). So a change moves the rows of a day it does not name only through a first input day
// or through the repositories the rows are kept for (the day of a deploy that comes to ship, or
// ships no more, a pull request no item records the deploy of is one the page names,
// refresh.shippedNeighbours):
//
//   - B: a day the page names holds input of a series before the series' first input day, or of a
//     series that had none. The days between the new first day and the old one (through the last
//     day the stored rows are whole through, where it had none) gain the series' zero rows, and
//     each lead time dropUnshipped dropped of them.
//   - E: the page names a series' first input day, and the day holds no input of the series any
//     more. The days from it up to the series' next day with input lose its rows.
//   - C: the repositories that hold a delivery run changed (syncKept). A new repository's rows and
//     members are built of every day from its start on; one whose start moved gains or loses the
//     days between its two starts, and its series' first input days move as in B and E; and one
//     whose runs went loses every row and member.
//
// A page with none of them writes every day it names of a family in one transaction of their own
// (writeProjectDays): one commit, whose eval_days changes are one per eval. A page with one
// builds, in memory and with every read outside a write transaction, the days it names and the
// span of each series that moved (buildSpan), compares the span's rows and members with the stored
// ones, and writes the named days, the span's moved rows and members, and the stored figures of
// the days that moved in the transaction that moves its cursors (spanWrite, commitPage): one commit,
// whose eval_days changes are one per eval and carry the rebuild's mark (MarkRebuild), so an
// evals view is recomputed for them at once and no reader sees a state between the one before the
// change and the one after it. It moves no walk: the span ends at the last day the stored rows are
// whole through (eval_days_serving), and the walk builds each later day from the first input days
// of the days before it, the span's among them.
//
// What else rebuilds many days is no span. A database or a project first seen is walked, its days
// built in the open a batch a pass, each day of every key a commit of its own that moves its
// serving record past it (walkWrite). A release, a lost change cursor
// and a database whose rows' settings are not recorded rebuild the served rows aside and flip what
// moved in one commit (rebuild.go); a page that writes days while one runs keeps their zero rows
// (servingNext), and the triggers of the day rows queue them, a span's among them, for the rebuild
// to build again before its flip.

// dayFamily is one family of computed day rows a page writes whole days of: the walk that builds
// its days, the name its rows carry in a refusal, its evals and its families of eval_day_members.
type dayFamily struct {
	walk     string
	what     string
	slugs    []string
	families []string
}

var (
	deliveryFamily = dayFamily{walk: walkDelivery, what: "delivery", slugs: deliveryDaySlugs, families: deliveryDayFamilies}
	legionFamily   = dayFamily{walk: walkLegion, what: "Legion", slugs: legionDaySlugs, families: legionDayFamilies}
)

// namedDay is one project's day a page writes whole: the rows and the members its input builds
// (raw), before any rule of a first input day, and the zero rows of the day's series. A page writes
// every day it names (a day a change or eval_day_members named) though it holds no row, so its
// write deletes what the day held.
type namedDay struct {
	project string
	day     time.Time
	raw     ProjectDay
	zeros   []DayRow
}

// pageDays is the days of one family a page writes whole, in the order it named them, and name,
// which reads one more day's input and adds it (a day an item of another left, nameLeft), and,
// where the family's days are read several at once, read, which reads the input of the days it is
// given before name adds each (refresh.readDeliveries); nil where name reads its day alone.
type pageDays struct {
	family dayFamily
	order  []projectDay
	days   map[projectDay]*namedDay
	name   func(ctx context.Context, project string, day time.Time) error
	read   func(ctx context.Context, days []projectDay) error
}

func newPageDays(family dayFamily) *pageDays {
	return &pageDays{family: family, days: map[projectDay]*namedDay{}}
}

// add adds day, once.
func (p *pageDays) add(day *namedDay) {
	key := projectDay{project: day.project, day: day.day}
	if _, found := p.days[key]; found {
		return
	}
	p.days[key] = day
	p.order = append(p.order, key)
}

// projects is each project p holds a day of, sorted.
func (p *pageDays) projects() []string {
	held := map[string]bool{}
	for key := range p.days {
		held[key.project] = true
	}
	return slices.Sorted(maps.Keys(held))
}

// nameDelivery adds project's delivery and CI day of read to days, where days holds it not yet, as
// the day's reads build it.
func (r *refresh) nameDelivery(days *pageDays, read deliveryRead, project string) {
	if _, found := days.days[projectDay{project: project, day: read.Day}]; found {
		return
	}
	days.add(&namedDay{
		project: project, day: read.Day, raw: read.Rows(project),
		zeros: ZeroDeliveryDay(project, read.Day, read.rules).Rows,
	})
}

// nameLegion reads project's Legion day and adds it to days, where days holds it not yet.
func (r *refresh) nameLegion(ctx context.Context, days *pageDays, project string, day time.Time) error {
	if _, found := days.days[projectDay{project: project, day: measures.Midnight(day)}]; found {
		return nil
	}
	read, err := r.store.ReadLegionDay(ctx, project, day)
	if err != nil {
		return err
	}
	since, err := r.sinceOf(ctx, project)
	if err != nil {
		return err
	}
	days.add(&namedDay{
		project: project, day: read.Day, raw: read.Rows(since),
		zeros: ZeroLegionDay(project, read.Day, legionDaySlugs...).Rows,
	})
	return nil
}

// nameLeft adds to days each day an item of one of them leaves, as writeProjectDays records it
// owed: the day eval_day_members holds the item under, and the day of the deploy run that shipped
// a pull request the day moves to another run or counts no more. The day it left counts the item
// until it is written again, so it is written with the days that took the item from it. It reads
// them as the stored members stand, before any of the days is written, of every day at once
// (Store.leftDays), and again of the days it adds, up to owedRounds times.
func (r *refresh) nameLeft(ctx context.Context, days *pageDays) error {
	checked := 0
	for range owedRounds {
		named := make([]ProjectDay, 0, len(days.order)-checked)
		for _, key := range days.order[checked:] {
			day := days.days[key]
			named = append(named, ProjectDay{Project: day.project, Day: day.day, Members: day.raw.Members})
		}
		left, err := r.store.leftDays(ctx, named, days.family.families)
		if err != nil {
			return err
		}
		checked = len(days.order)
		left = slices.DeleteFunc(left, func(key projectDay) bool {
			_, found := days.days[key]
			return found
		})
		if len(left) > 0 && days.read != nil {
			if err := days.read(ctx, left); err != nil {
				return err
			}
		}
		for _, key := range left {
			if err := days.name(ctx, key.project, key.day); err != nil {
				return err
			}
		}
		if checked == len(days.order) {
			return nil
		}
	}
	return nil
}

// finalize is day as its project's day holds it by the first input days first: the rows its input
// builds less the lead times dropUnshipped drops, and, where the stored rows are whole through the
// day (computed, eval_days_serving), the zero row of each series it has no input of whose first
// input day is on or before it.
func (n *namedDay) finalize(first map[evalKey]time.Time, computed bool) ProjectDay {
	day := n.raw
	day.Rows = dropUnshipped(day.Rows, first, n.day)
	if computed {
		day = day.WithZeros(zerosFrom(n.zeros, first, n.day))
	}
	return day
}

// trackedKey is one series' rows (an eval's, of one deploy repository) of one project whose days
// beyond the ones a page names its changes can move: start is the first day they can move on; was
// the series' stored first input day, nil where it had none; covered the days built whole for
// the key whatever its first input day, a changed repository's (keptSpans), nil for none; and
// first the series' first input day as the span's days, taken in order from start, find it, nil
// until one holds input of it.
type trackedKey struct {
	start   time.Time
	was     *time.Time
	covered *daySpan
	first   *time.Time
}

// covers reports whether day is one built whole for the key.
func (t *trackedKey) covers(day time.Time) bool {
	return t.covered != nil && !day.Before(t.covered.first) && !day.After(t.covered.last)
}

// active reports whether the key's rows of day can differ from the stored ones: a day built whole
// for it, a day before the span has found its first input day, and a day before its stored first
// input day (every day, where it had none). From the later of the two first days on, a day holds
// what it held: its input's rows, or its zero rows.
func (t *trackedKey) active(day time.Time) bool {
	return !day.Before(t.start) && (t.covers(day) || t.first == nil || t.was == nil || day.Before(*t.was))
}

// trackMoves is each key of each project whose first input day the named days move, with the
// stored first input days firsts (by project): a key a named day holds input of before its first
// input day, or that had none (B), tracked from that day; and a key whose first input day is a
// named day that holds no input of it (E), tracked from it.
func trackMoves(days *pageDays, firsts map[string]map[evalKey]time.Time) map[string]map[evalKey]*trackedKey {
	inputs, named := map[string]map[evalKey][]time.Time{}, map[projectDay]map[evalKey]bool{}
	for key, day := range days.days {
		held := inputKeys(day.raw.Rows)
		named[key] = held
		if inputs[key.project] == nil {
			inputs[key.project] = map[evalKey][]time.Time{}
		}
		for one := range held {
			inputs[key.project][one] = append(inputs[key.project][one], key.day)
		}
	}
	tracked := map[string]map[evalKey]*trackedKey{}
	track := func(project string, key evalKey, start time.Time, was *time.Time) {
		if tracked[project] == nil {
			tracked[project] = map[evalKey]*trackedKey{}
		}
		tracked[project][key] = &trackedKey{start: start, was: was}
	}
	for project, held := range inputs {
		first := firsts[project]
		for key, on := range held {
			earliest := slices.MinFunc(on, time.Time.Compare)
			since, known := first[key]
			switch {
			case !known:
				track(project, key, earliest, nil)
			case earliest.Before(since):
				track(project, key, earliest, &since)
			}
		}
		for key, since := range first {
			if _, moved := tracked[project][key]; moved {
				continue
			}
			if input, written := named[projectDay{project: project, day: since}]; written && !input[key] {
				track(project, key, since, &since)
			}
		}
	}
	return tracked
}

// keptSpans is, of the repositories that hold a delivery run now (is) against those the rows stand
// built for (was), each one whose days are built whole, with those days, through reach, the last
// day the stored delivery rows are whole through: a new repository's from its start on, and one
// whose start moved the days between its two starts; and gone, each repository of was that holds
// no run any more.
func keptSpans(was, is []keptRepo, reach time.Time) (covered map[string]daySpan, gone []string) {
	covered = map[string]daySpan{}
	before := make(map[string]time.Time, len(was))
	for _, repo := range was {
		before[repo.name] = repo.since
	}
	for _, repo := range is {
		since, held := before[repo.name]
		span := daySpan{first: repo.since, last: reach}
		if held {
			if since.Equal(repo.since) {
				delete(before, repo.name)
				continue
			}
			span = daySpan{first: since, last: repo.since.AddDate(0, 0, -1)}
			if repo.since.Before(since) {
				span = daySpan{first: repo.since, last: since.AddDate(0, 0, -1)}
			}
		}
		delete(before, repo.name)
		if span.last.After(reach) {
			span.last = reach
		}
		if !span.first.After(span.last) {
			covered[repo.name] = span
		}
	}
	return covered, slices.Sorted(maps.Keys(before))
}

// spanInput is where a span's days that no change names get their input: raw is each asked
// project's rows and members of day as its input builds them, of the keys active (by project)
// alone where it can read less for them; zeros the zero rows of a project's key on a day.
type spanInput struct {
	raw   func(ctx context.Context, day time.Time, active map[string]map[evalKey]*trackedKey) (map[string]ProjectDay, error)
	zeros func(project string, day time.Time, key evalKey) []DayRow
}

// spanDay is one key's rows of one project's day that a span builds: a day no change names.
type spanDay struct {
	project string
	key     evalKey
	day     time.Time
}

// spanBuilt is what buildSpan builds of the days no change names: the rows of each tracked key on
// each of its days (rows), those days (days), and the members of the days built whole for a
// changed repository, under it (members), with those days by project (memberDays).
type spanBuilt struct {
	rows       []DayRow
	days       map[spanDay]bool
	members    map[memberKey]memberAt
	memberDays map[projectDay]bool
}

// covers reports whether day is one of the days covered builds whole for deployRepo (keptSpans),
// whose every member the span builds.
func covers(covered map[string]daySpan, deployRepo string, day time.Time) bool {
	span, held := covered[deployRepo]
	return held && !day.Before(span.first) && !day.After(span.last)
}

// buildSpan walks the days from the earliest start of tracked through the last named day, in
// order, as the walk takes them: each tracked key finds its first input day at the first day that
// holds input of it, a named day's from the rows the page read and any other's from input. Of the
// days no change names, through reach, it builds each active key's rows (trackedKey.active): a day
// built whole for the key holds what its input builds, by the first input day found so far; a day
// with input of the key holds what it held, and is left; and any other day holds the key's zero
// rows from its first input day on, and the lead times dropUnshipped keeps from then. covered
// names the days whose members it builds too, by repository.
func buildSpan(ctx context.Context, days *pageDays, tracked map[string]map[evalKey]*trackedKey, covered map[string]daySpan, reach time.Time, input spanInput) (spanBuilt, error) {
	built := spanBuilt{days: map[spanDay]bool{}, members: map[memberKey]memberAt{}, memberDays: map[projectDay]bool{}}
	var start, end time.Time
	for _, keys := range tracked {
		for _, key := range keys {
			if start.IsZero() || key.start.Before(start) {
				start = key.start
			}
		}
	}
	if start.IsZero() {
		return built, nil
	}
	end = reach
	for key := range days.days {
		if key.day.After(end) {
			end = key.day
		}
	}
	projects := slices.Sorted(maps.Keys(tracked))
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		active, unnamed := map[string]map[evalKey]*trackedKey{}, map[string]map[evalKey]*trackedKey{}
		for _, project := range projects {
			_, named := days.days[projectDay{project: project, day: day}]
			if !named && day.After(reach) {
				continue
			}
			for key, one := range tracked[project] {
				if !one.active(day) {
					continue
				}
				if active[project] == nil {
					active[project] = map[evalKey]*trackedKey{}
				}
				active[project][key] = one
			}
			if !named && active[project] != nil {
				unnamed[project] = active[project]
			}
		}
		if len(active) == 0 {
			continue
		}
		var raws map[string]ProjectDay
		if len(unnamed) > 0 {
			var err error
			if raws, err = input.raw(ctx, day, unnamed); err != nil {
				return spanBuilt{}, err
			}
		}
		for _, project := range projects {
			keys := active[project]
			if keys == nil {
				continue
			}
			named, written := days.days[projectDay{project: project, day: day}]
			raw := raws[project]
			if written {
				raw = named.raw
			}
			held := inputKeys(raw.Rows)
			for _, key := range slices.SortedFunc(maps.Keys(keys), compareEvalKeys) {
				one := keys[key]
				if !written && (one.covers(day) || !held[key]) {
					first := map[evalKey]time.Time{}
					if one.first != nil {
						first[key] = *one.first
					}
					own := ProjectDay{Rows: dropUnshipped(rowsOf(raw.Rows, key), first, day)}
					built.rows = append(built.rows, own.WithZeros(zerosFrom(input.zeros(project, day, key), first, day)).Rows...)
					built.days[spanDay{project: project, key: key, day: day}] = true
				}
				if held[key] && one.first == nil {
					one.first = &day
				}
			}
			if written {
				continue
			}
			for _, member := range raw.Members {
				if covers(covered, member.DeployRepo, day) {
					built.members[memberKey{project: project, family: member.Family, deployRepo: member.DeployRepo, rowKey: member.RowKey}] =
						memberAt{day: day, repo: member.Repo, shippedBy: member.ShippedBy}
				}
			}
			built.memberDays[projectDay{project: project, day: day}] = true
		}
	}
	return built, nil
}

// rowsOf is the rows of rows that are key's.
func rowsOf(rows []DayRow, key evalKey) []DayRow {
	var own []DayRow
	for _, row := range rows {
		if keyOf(row) == key {
			own = append(own, row)
		}
	}
	return own
}

// spanWrite is what one family's page writes in the page's commit (commitPage): the days its
// changes name, whole (days); the rows of the span's days that moved (rows) and those the days no
// longer hold (gone); the members of the days built whole for a changed repository as they
// are stored (stored), as they are built (members) and the days they are built of (memberDays);
// the repositories that hold no run any more, whose every row and member goes (goneRepos, of
// projects); the days whose stored figures it folds again, by project and eval, each run of them
// apart (spans); and the days it wrote, for the pass's report.
type spanWrite struct {
	family     dayFamily
	build      string
	days       []ProjectDay
	rows       []DayRow
	gone       []DayRow
	stored     map[memberKey]memberAt
	members    map[memberKey]memberAt
	memberDays map[projectDay]bool
	goneRepos  []string
	projects   []string
	spans      writtenSpans
	report     []builtDay
}

// widen adds the days first to last of project's eval evalID to those whose figures the span write
// folds again (writtenSpans.add).
func (b *spanWrite) widen(project, evalID string, first, last time.Time) {
	b.spans.add(project, evalID, first, last)
}

// writeRows writes the span write's day rows in tx: the named days', as writeProjectDays writes
// them, then the span's moved rows (upsertDays) and the keys its days no longer hold, and every row
// of a repository that holds no run any more, of its projects (every project and owner key, which
// grow with the history) in statements of at most statementBytes, each with the evals of its
// projects.
func (b *spanWrite) writeRows(ctx context.Context, tx pgx.Tx) error {
	if err := writeDayRowsIn(ctx, tx, b.days, b.family.slugs, b.build); err != nil {
		return err
	}
	if err := upsertDays(ctx, tx, b.rows, b.build); err != nil {
		return err
	}
	if err := deleteDayRows(ctx, tx, b.gone); err != nil {
		return err
	}
	if len(b.goneRepos) == 0 {
		return nil
	}
	base := paramBytes(3, writerComputation)
	for i := range b.goneRepos {
		base += textBytes(&b.goneRepos[i])
	}
	size := func(project *string) int { return textBytes(project) + evalIDsBytes(*project, b.family.slugs) }
	for projects := range statementChunks(b.projects, base, size) {
		var evalIDs []string
		for _, project := range projects {
			for _, slug := range b.family.slugs {
				evalIDs = append(evalIDs, EvalID(project, slug))
			}
		}
		if _, err := tx.Exec(ctx, `
			delete from eval_days where project_key = any($1) and eval_id = any($2) and writer = $3 and deploy_repo = any($4)
		`, projects, evalIDs, writerComputation, b.goneRepos); err != nil {
			return fmt.Errorf("delete the day rows of %v, which hold no delivery run any more: %w", b.goneRepos, err)
		}
	}
	return nil
}

// writeMembers writes the span write's members in tx, after its rows: the named days', as
// writeProjectDays writes them, the span's that moved (writeMovedMembers), and every member of a
// repository that holds no run any more, of its projects in statements of at most statementBytes.
func (b *spanWrite) writeMembers(ctx context.Context, tx pgx.Tx) error {
	if err := writeDayMembersIn(ctx, tx, b.days, b.family.families); err != nil {
		return err
	}
	if err := writeMovedMembers(ctx, tx, b.stored, b.members, b.memberDays); err != nil {
		return err
	}
	if len(b.goneRepos) == 0 {
		return nil
	}
	base := paramBytes(3)
	for _, list := range [][]string{b.family.families, b.goneRepos} {
		for i := range list {
			base += textBytes(&list[i])
		}
	}
	for projects := range statementChunks(b.projects, base, textBytes) {
		if _, err := tx.Exec(ctx, `
			delete from eval_day_members where project_key = any($1) and family = any($2) and deploy_repo = any($3)
		`, projects, b.family.families, b.goneRepos); err != nil {
			return fmt.Errorf("delete the members of %v, which hold no delivery run any more: %w", b.goneRepos, err)
		}
	}
	return nil
}

// fold folds again, in tx, after the span write's rows and members, the stored figures of the days
// whose rows it moved, each project once, an owner key's into its readers' (foldFiguresInTx).
func (b *spanWrite) fold(ctx context.Context, tx pgx.Tx) error {
	return foldFiguresInTx(ctx, tx, b.build, b.spans.sets()...)
}

// MarkRebuild marks tx as a rebuild's write of day rows, before its first write of a day row: the
// refresher's write of a span (writeSpans) and a verified rebuild's flip (flipRebuild). It sets
// the transaction-local setting dispatch.eval_days_cause to delivery.EvalDaysWrittenByRebuild
// (markCause), as MarkSettingsRewrite sets the settings rewrite's. The Delivery page's live hub
// recomputes an evals view at once for those changes: the rebuild's day rows arrive together, in
// one transaction, with no burst left for the quiet to gather.
func MarkRebuild(ctx context.Context, tx pgx.Tx) error {
	return markCause(ctx, tx, delivery.EvalDaysWrittenByRebuild, "the rebuild of a span")
}

// markCause sets, in tx, the transaction-local setting dispatch.eval_days_cause to cause, which
// eval_days_change_log records as the run_kind of each eval_days change the transaction logs
// (migration 20261117100014); what names the write in an error.
func markCause(ctx context.Context, tx pgx.Tx, cause, what string) error {
	if _, err := tx.Exec(ctx, `select set_config('dispatch.eval_days_cause', $1, true)`, cause); err != nil {
		return fmt.Errorf("mark %s: %w", what, err)
	}
	return nil
}

// writeSpans writes spans in tx, a page's commit, before the page's cursors: marked a rebuild's
// (MarkRebuild), every one's rows, then every one's members, then the stored figures, the order
// every writer of the three takes them in. A span that finds a repository's runs gone begins the
// rebuild that is building again (replaceRebuild): every day of the repository goes, and building
// them all again before the flip would be a whole rebuild in the flip's pass.
func writeSpans(ctx context.Context, tx pgx.Tx, spans []*spanWrite) error {
	if len(spans) == 0 {
		return nil
	}
	// Each statement planned for the arrays it is sent, as the settings rewrite's are.
	if _, err := tx.Exec(ctx, `set local plan_cache_mode = force_custom_plan`); err != nil {
		return fmt.Errorf("plan the rebuild of a span: %w", err)
	}
	if err := MarkRebuild(ctx, tx); err != nil {
		return err
	}
	for _, one := range spans {
		if err := one.writeRows(ctx, tx); err != nil {
			return err
		}
	}
	gone := false
	for _, one := range spans {
		if err := one.writeMembers(ctx, tx); err != nil {
			return err
		}
		gone = gone || len(one.goneRepos) > 0
	}
	for _, one := range spans {
		if err := one.fold(ctx, tx); err != nil {
			return err
		}
	}
	if gone {
		return replaceRebuild(ctx, tx, "", "", nil)
	}
	return nil
}

// committed records what the spans a page's commit wrote in the pass: the days in its report,
// and, since they moved first input days, the pass's own read of them forgotten, so the next write
// reads them as the commit left them (storedFirst).
func (r *refresh) committed(spans []*spanWrite) {
	for _, one := range spans {
		for _, day := range one.report {
			r.report.days[day] = true
		}
		for _, project := range one.projects {
			delete(r.first, walkKey{family: one.family.walk, project: project})
		}
	}
}

// logSpans logs what the spans of one transaction wrote and how long that transaction was open,
// from its begin to its commit: a span is one commit however far it reaches, so its length is what
// bounds how long the rows it writes stay locked.
func logSpans(spans []*spanWrite, open time.Duration) {
	if len(spans) == 0 {
		return
	}
	days, rows, gone, members := 0, 0, 0, 0
	for _, one := range spans {
		days, rows, gone, members = days+len(one.days), rows+len(one.rows), gone+len(one.gone), members+len(one.members)
	}
	slog.Info("dispatch: the eval day-row refresher rebuilt a span in one commit",
		"named_days", days, "span_rows", rows, "span_rows_gone", gone, "span_members", members, "transaction_ms", open.Milliseconds())
}

// commitSpans commits spans in a transaction of their own: the spans of days a write left owed
// after the page's commit (writeOwed).
func (r *refresh) commitSpans(ctx context.Context, spans []*spanWrite) error {
	if len(spans) == 0 {
		return nil
	}
	began := time.Now()
	if err := r.store.inTx(ctx, "the rebuild of a span", func(tx pgx.Tx) error {
		return writeSpans(ctx, tx, spans)
	}); err != nil {
		return err
	}
	r.committed(spans)
	logSpans(spans, time.Since(began))
	r.store.drainChanges(ctx)
	return nil
}

// keptMove is a change of the repositories that hold a delivery run a page read (syncKept): those
// the rows stood built for and those that hold a run now.
type keptMove struct {
	was, is []keptRepo
}

// writeNamed writes the days of one family a page names (days), and with them each day an item of
// theirs left (nameLeft). Where the days move no series' first input day, kept names no change of
// a day the stored rows are whole through (or no walk or rebuild has served the family's rows yet),
// and withPage is not set (the page's commit publishes no default move beside them), it writes
// every day in one transaction of their own (writeProjectDays), by the stored first input days,
// with its zero rows where the stored rows are whole through it (servingNext), and answers nil:
// one commit, which logs one change of each eval the days moved. Where withPage is set with no
// span to build, it answers a span write of the named days alone, so the page's commit writes them
// with its default move. Otherwise it builds the span (span.go's rules) and answers the span write
// the caller commits with its cursors, having written nothing.
func (r *refresh) writeNamed(ctx context.Context, days *pageDays, kept *keptMove, withPage bool) (*spanWrite, error) {
	if err := r.nameLeft(ctx, days); err != nil {
		return nil, err
	}
	family := days.family
	projects := days.projects()
	if family.walk == walkDelivery {
		// A deploy counts for the projects of the pull requests it ships and its repository's
		// owner keys, so the delivery days of one owner can move another's: every owner's are
		// judged together, a repository's own plain owner key and its source key (sourceKey) among
		// them though the store holds no row of either yet (trackKept tracks them with the rest).
		if kept != nil {
			for _, repo := range kept.is {
				r.noteOwners([]string{OwnerKey(repo.name, nil, nil)})
				if !IsEventsRepo(repo.name) {
					r.noteOwners([]string{sourceKey(repo.name)})
				}
			}
		}
		projects = r.owners
	}
	firsts, err := r.storedFirsts(ctx, family.walk, projects)
	if err != nil {
		return nil, err
	}
	nexts := map[string]*time.Time{}
	for _, project := range projects {
		if nexts[project], err = r.servingNext(ctx, family.walk, project); err != nil {
			return nil, err
		}
	}
	computed := func(project string, day time.Time) bool {
		return nexts[project] != nil && day.Before(*nexts[project])
	}
	tracked := trackMoves(days, firsts)
	for project := range tracked {
		if nexts[project] == nil {
			// No rows of the project are served yet: its first walk builds every day, by the first
			// input days it finds, and the page's days hold their input's rows alone until then.
			delete(tracked, project)
		}
	}
	var covered map[string]daySpan
	var gone []string
	var reach time.Time
	if family.walk == walkDelivery {
		// Every project's delivery days are the one delivery walk's.
		if next := nexts[firstOr(projects, "")]; next != nil {
			reach = next.AddDate(0, 0, -1)
			if reach.After(r.today) {
				reach = r.today
			}
			if kept != nil {
				covered, gone = keptSpans(kept.was, kept.is, reach)
			}
		}
	}
	if len(tracked) == 0 && len(covered) == 0 && len(gone) == 0 {
		var written []ProjectDay
		for _, key := range days.order {
			day := days.days[key].finalize(firsts[key.project], computed(key.project, key.day))
			written = append(written, day)
		}
		if withPage {
			b := &spanWrite{family: family, build: r.build, projects: projects, spans: writtenSpans{}}
			for _, day := range written {
				if err := checkProjectDay(family.what, day, family.slugs, family.families); err != nil {
					return nil, err
				}
				b.days = append(b.days, day)
				b.report = append(b.report, builtDay{family: family.walk, project: day.Project, day: measures.Midnight(day.Day)})
				for _, slug := range family.slugs {
					b.widen(day.Project, EvalID(day.Project, slug), day.Day, day.Day)
				}
			}
			return b, nil
		}
		if err := r.store.writeProjectDays(ctx, family, written, r.build, true); err != nil {
			return nil, err
		}
		for _, day := range written {
			r.report.days[builtDay{family: family.walk, project: day.Project, day: measures.Midnight(day.Day)}] = true
		}
		if len(trackMoves(days, firsts)) > 0 {
			// A first input day moved on a day the stored rows are not whole through: the pass reads
			// them again.
			for _, project := range projects {
				delete(r.first, walkKey{family: family.walk, project: project})
			}
		}
		return nil, nil
	}
	var input spanInput
	if family.walk == walkDelivery {
		r.trackKept(tracked, covered, firsts)
		// A page that only finds a repository's runs gone tracks no key, and reads no history.
		if len(tracked) > 0 {
			if input, err = r.deliverySpanInput(ctx, tracked, reach); err != nil {
				return nil, err
			}
		}
	} else {
		reach = r.today
		for _, project := range projects {
			if next := nexts[project]; next != nil && next.AddDate(0, 0, -1).Before(reach) {
				reach = next.AddDate(0, 0, -1)
			}
		}
		input = r.legionSpanInput(tracked, reach)
	}
	built, err := buildSpan(ctx, days, tracked, covered, reach, input)
	if err != nil {
		return nil, err
	}
	b := &spanWrite{family: family, build: r.build, projects: projects, goneRepos: gone, spans: writtenSpans{},
		members: built.members, memberDays: built.memberDays}
	for _, key := range days.order {
		named := days.days[key]
		first := maps.Clone(firsts[key.project])
		for one, moved := range tracked[key.project] {
			delete(first, one)
			if moved.first != nil {
				first[one] = *moved.first
			}
		}
		day := named.finalize(first, computed(key.project, key.day))
		if err := checkProjectDay(family.what, day, family.slugs, family.families); err != nil {
			return nil, err
		}
		b.days = append(b.days, day)
		b.report = append(b.report, builtDay{family: family.walk, project: key.project, day: key.day})
		for _, slug := range family.slugs {
			b.widen(key.project, EvalID(key.project, slug), key.day, key.day)
		}
	}
	if err := r.compareSpan(ctx, b, built, covered); err != nil {
		return nil, err
	}
	if len(gone) > 0 {
		for _, project := range projects {
			for _, slug := range family.slugs {
				if readsDeployRepo(slug) {
					b.widen(project, EvalID(project, slug), everyDay.first, everyDay.last)
				}
			}
		}
	}
	return b, nil
}

// firstOr is the first of values, or none where there is none.
func firstOr(values []string, none string) string {
	if len(values) == 0 {
		return none
	}
	return values[0]
}

// trackKept tracks, in tracked, every key of each repository of covered (keptSpans), of every
// project, from the first of the repository's days that are built whole: its rows of those days
// are what their input builds, and its first input days move with them. A key the named days
// already track keeps the earlier start.
func (r *refresh) trackKept(tracked map[string]map[evalKey]*trackedKey, covered map[string]daySpan, firsts map[string]map[evalKey]time.Time) {
	rules := make(map[string]delivery.DeliverySettings, len(r.deployRepos))
	for _, kept := range r.deployRepos {
		rules[kept.name] = kept.rule
	}
	for _, repo := range slices.Sorted(maps.Keys(covered)) {
		span := covered[repo]
		// The rule the repository's rows are built under (keptRepos): its source's, or the one
		// recorded with its rows where no source reads it any more.
		rule := rules[repo]
		// Each owner's, a repository's own owner keys among them (writeNamed).
		for _, project := range r.owners {
			for _, zero := range ZeroDeliveryDay(project, r.today, []delivery.DeliverySettings{rule}).Rows {
				key := keyOf(zero)
				if key.deployRepo != repo {
					continue
				}
				if tracked[project] == nil {
					tracked[project] = map[evalKey]*trackedKey{}
				}
				one, held := tracked[project][key]
				if !held {
					one = &trackedKey{start: span.first}
					if since, known := firsts[project][key]; known {
						one.was = &since
					}
					tracked[project][key] = one
				}
				if span.first.Before(one.start) {
					one.start = span.first
				}
				one.covered = &span
			}
		}
	}
}

// compareSpan compares what the span built (built) with what the store holds, outside any write
// transaction, and records in b what moved: each built row of a span day whose stored row holds
// other values or is missing, each stored row of a span day of its key that the day no longer
// builds, and the stored members of the days whose members the span built (covered). It
// widens b's figure spans and its report by the days whose rows moved. The refresher lease allows
// one refresher pass at a time, but two writers outside it change rows and members and can commit
// between this read and the span write: the boot's repository rename pass renames an owner key
// (store.RewriteRenamedRepositories), and a project's delete deletes the project's rows
// (eval_days_project_deleted, migration 20261117100006).
func (r *refresh) compareSpan(ctx context.Context, b *spanWrite, built spanBuilt, covered map[string]daySpan) error {
	type scope struct {
		evals, repos map[string]bool
		span         daySpan
	}
	scopes := map[string]*scope{}
	for at := range built.days {
		one := scopes[at.project]
		if one == nil {
			one = &scope{evals: map[string]bool{}, repos: map[string]bool{}}
			scopes[at.project] = one
		}
		one.evals[at.key.eval], one.repos[at.key.deployRepo], one.span = true, true, one.span.widen(at.day)
	}
	var stored []DayRow
	for _, project := range slices.Sorted(maps.Keys(scopes)) {
		one := scopes[project]
		rows, err := r.store.database.Pool.Query(ctx, `
			select `+dayColumns+` from eval_days
			where project_key = $1 and writer = $2 and eval_id = any($3) and deploy_repo = any($4) and day >= $5 and day <= $6
		`, project, writerComputation, slices.Sorted(maps.Keys(one.evals)), slices.Sorted(maps.Keys(one.repos)), one.span.first, one.span.last)
		if err != nil {
			return fmt.Errorf("read the stored rows of %s's span: %w", project, err)
		}
		held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DayRow, error) { return scanDay(row) })
		if err != nil {
			return fmt.Errorf("read the stored rows of %s's span: %w", project, err)
		}
		for _, row := range held {
			if built.days[spanDay{project: project, key: keyOf(row), day: measures.Midnight(row.Day)}] {
				stored = append(stored, row)
			}
		}
	}
	moved := map[projectDay]bool{}
	note := func(row *DayRow) {
		day := measures.Midnight(row.Day)
		moved[projectDay{project: row.ProjectKey, day: day}] = true
		b.widen(row.ProjectKey, row.EvalID, day, day)
	}
	changed, gone := diffDayRows(stored, built.rows)
	for _, row := range changed {
		b.rows = append(b.rows, *row)
		note(row)
	}
	for index := range gone {
		note(&gone[index])
	}
	b.gone = gone
	for _, day := range slices.SortedFunc(maps.Keys(moved), func(a, c projectDay) int {
		return cmp.Or(strings.Compare(a.project, c.project), a.day.Compare(c.day))
	}) {
		b.report = append(b.report, builtDay{family: b.family.walk, project: day.project, day: day.day})
	}
	repos := slices.Sorted(maps.Keys(covered))
	if len(repos) == 0 {
		return nil
	}
	var days []time.Time
	seen := map[time.Time]bool{}
	for at := range built.memberDays {
		if !seen[at.day] {
			seen[at.day] = true
			days = append(days, at.day)
		}
	}
	held, err := r.store.readMembers(ctx, r.owners, b.family.families, repos, days)
	if err != nil {
		return err
	}
	// A stored member stands unless the span built the members under its repository on its day:
	// one of a day the span built for another repository alone is none of the span's.
	for key, at := range held {
		if !built.memberDays[projectDay{project: key.project, day: at.day}] || !covers(covered, key.deployRepo, at.day) {
			delete(held, key)
		}
	}
	b.stored = held
	return nil
}

// spanRowKey is a day row's key among the computation's rows, as one string (dayRowKey).
func spanRowKey(row *DayRow) string {
	return strings.Join(dayRowKey(row), "\x00")
}

// diffDayRows is what built, rows a build made, moves of stored, the stored rows of the keys and
// days it built, each row by its key (spanRowKey): each built row whose stored row holds other
// values or is missing (moved, in built's order), and each stored row of a key no built row holds
// (gone, in the byte order of the keys).
func diffDayRows(stored []DayRow, built ...[]DayRow) (moved []*DayRow, gone []DayRow) {
	held := make(map[string]DayRow, len(stored))
	for _, row := range stored {
		held[spanRowKey(&row)] = row
	}
	for _, rows := range built {
		for index := range rows {
			row := &rows[index]
			key := spanRowKey(row)
			was, found := held[key]
			delete(held, key)
			if !found || !sameDayValues(row, &was) {
				moved = append(moved, row)
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(held)) {
		gone = append(gone, held[key])
	}
	return moved, gone
}

// sameDayValues reports whether built and stored hold the same values, the columns upsertDays
// compares, a time by its microsecond, as it is stored. It answers false where it cannot tell (a
// sample list that holds none against one that is none), and the row is then sent, which rewrites
// it only where a value differs.
func sameDayValues(built, stored *DayRow) bool {
	return built.Listed == stored.Listed && built.N == stored.N && sameFloat(built.Sum, stored.Sum) && sameFloat(built.Num, stored.Num) &&
		sameFloat(built.Den, stored.Den) && sameFloat(built.Max, stored.Max) && (built.Samples == nil) == (stored.Samples == nil) &&
		slices.Equal(built.Samples, stored.Samples) && sameFloat(built.LastValue, stored.LastValue) && sameMoment(built.LastAt, stored.LastAt) &&
		sameMoment(built.LastRecordedAt, stored.LastRecordedAt) && built.LastReading == stored.LastReading &&
		maps.Equal(NonNilCounts(built.Counts), NonNilCounts(stored.Counts)) && built.ViaRepository == stored.ViaRepository
}

func sameFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameMoment(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond))
}

// deleteDayRows deletes, in tx, each of the computation's stored rows rows names by its key, in the
// byte order of the keys and in statements of at most statementBytes of parameters
// (statementChunks).
func deleteDayRows(ctx context.Context, tx pgx.Tx, rows []DayRow) error {
	if len(rows) == 0 {
		return nil
	}
	ordered := make([]*DayRow, len(rows))
	for i := range rows {
		ordered[i] = &rows[i]
	}
	slices.SortFunc(ordered, compareDayRows)
	for chunk := range statementChunks(ordered, paramBytes(6, writerComputation), func(row **DayRow) int { return dayRowKeyBytes(*row) }) {
		projects, evals, deployRepos, series, repos, days := dayRowKeys(chunk)
		if _, err := tx.Exec(ctx, `
			delete from eval_days e
			using unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::date[])
				as gone(project_key, eval_id, deploy_repo, series, repo, day)
			where e.project_key = gone.project_key and e.eval_id = gone.eval_id and e.writer = $7
				and e.deploy_repo = gone.deploy_repo and e.series = gone.series and e.repo = gone.repo and e.day = gone.day
		`, projects, evals, deployRepos, series, repos, days, writerComputation); err != nil {
			return fmt.Errorf("delete the %d day rows a span's days no longer hold: %w", len(rows), err)
		}
	}
	return nil
}

// deliverySpanInput is the input of the delivery days a span builds that no change names, read
// once over the history as the settings rewrite reads it (rewriteSettings): every population pull
// request and, for each repository a tracked key reads as the deploy repository, every deploy run
// on main with its jobs of the names its rule judges by and its deploy cards follow
// (delivery.ReadHistoryRows), the attribution lists of the pull requests (readAttribution), and
// containment over each repository's history under its rule (historyOf); of an events
// repository, its project's posted deploys from the span's start on (postedDeploys). A day's rows
// are each repository's DeliveryDay of it as readDeliveryDays builds it from those rows
// (repoHistory.deliveryDay, postedDayAs, firstShipped), and the pull requests' alone where a key
// no deploy repository moves is active. The PR-checks runs, which the history does not hold, are
// read of a day only where a pr-ci key is active on it and the repository holds one that day
// (checkDays, one read of a repository's days), once, since a span builds each day once
// (buildSpan), by the PR-checks read of a CI read (delivery.ReadCheckRows, ReadCIRows' Checks),
// with the attribution lists of their pull requests (readAttribution).
func (r *refresh) deliverySpanInput(ctx context.Context, tracked map[string]map[evalKey]*trackedKey, reach time.Time) (spanInput, error) {
	pool := r.store.database.Pool
	kept, err := r.keptRepos(ctx)
	if err != nil {
		return spanInput{}, err
	}
	needed, checked := map[string]bool{}, map[string]bool{}
	var start time.Time
	for project, keys := range tracked {
		for key, one := range keys {
			if start.IsZero() || one.start.Before(start) {
				start = one.start
			}
			if key.deployRepo != "" {
				needed[key.deployRepo] = true
				if slugOf(project, key.eval) == SlugPRCI {
					checked[key.deployRepo] = true
				}
			}
		}
	}
	// A pull request counts its lead times at the first deploy to ship it, a GitHub one or a posted
	// one (firstShipped), so where both kinds of repository are kept every one is read to compare.
	compare := slices.ContainsFunc(kept, func(repo keptRepo) bool { return IsEventsRepo(repo.name) }) &&
		slices.ContainsFunc(kept, func(repo keptRepo) bool { return !IsEventsRepo(repo.name) })
	var repos, events []keptRepo
	var judged []string
	for _, repo := range kept {
		if !needed[repo.name] && !compare {
			continue
		}
		if IsEventsRepo(repo.name) {
			events = append(events, repo)
			continue
		}
		repos = append(repos, repo)
		judged = append(judged, repo.rule.Production().JudgedJobNames()...)
		judged = append(judged, measures.DeployCardJobNames(measures.DeployCards(repo.rule))...)
	}
	names := deployRepoNames(repos)
	judged = slices.Compact(slices.Sorted(slices.Values(judged)))
	var history delivery.HistoryRows
	checkDays := map[string]map[time.Time]bool{}
	posted := make([]postedRecord, len(events))
	reads := []func(context.Context) error{
		func(ctx context.Context) (err error) {
			history, err = delivery.ReadHistoryRows(ctx, pool, names, judged, reach.AddDate(0, 0, 1))
			return err
		},
		func(ctx context.Context) error {
			for _, repo := range slices.Sorted(maps.Keys(checked)) {
				days, err := r.store.checkDays(ctx, repo, start, reach.AddDate(0, 0, 1))
				if err != nil {
					return err
				}
				checkDays[repo] = days
			}
			return nil
		},
		func(ctx context.Context) (err error) {
			for i, repo := range events {
				if posted[i], err = postedDeploys(ctx, pool, repo.rule.Project, start); err != nil {
					return err
				}
			}
			return nil
		},
	}
	if err := store.ReadSideBySide(ctx, reads...); err != nil {
		return spanInput{}, fmt.Errorf("read the delivery history for a span: %w", err)
	}
	prs := history.PullRequests
	prsByDay := prsByMergeDay(prs)
	attribute, err := readAttribution(ctx, r.store, prs, nil)
	if err != nil {
		return spanInput{}, err
	}
	histories := make(map[string]repoHistory, len(repos))
	for _, repo := range repos {
		histories[repo.name] = historyOf(repo, history.Deploys[repo.name], prs)
	}
	raw := func(ctx context.Context, day time.Time, active map[string]map[evalKey]*trackedKey) (map[string]ProjectDay, error) {
		alone, as, ci := false, map[string]bool{}, map[string]bool{}
		for project, keys := range active {
			for key := range keys {
				if key.deployRepo == "" {
					alone = true
					continue
				}
				as[key.deployRepo] = true
				if slugOf(project, key.eval) == SlugPRCI && checkDays[key.deployRepo][day] {
					ci[key.deployRepo] = true
				}
			}
		}
		read := deliveryRead{Day: day}
		var merged []delivery.DeliveryPullRequest
		for _, i := range prsByDay[day] {
			merged = append(merged, prs[i])
		}
		if alone {
			one := DeliveryDay{Day: day}
			for _, pr := range merged {
				// A partial row, whose opening is not known yet, as readDeliveryDays leaves it out.
				if pr.CreatedAt != nil {
					one.prs = append(one.prs, attribute.pullRequest(pr, nil, nil))
				}
			}
			read.days = append(read.days, one)
		}
		var built []DeliveryDay
		var rules []delivery.DeliverySettings
		var shown []bool
		for _, repo := range repos {
			name := repo.name
			if !as[name] && !compare {
				continue
			}
			h := histories[name]
			one := h.deliveryDay(day, prs, prsByDay, attribute)
			if ci[name] {
				checks, err := delivery.ReadCheckRows(ctx, pool, name, day, day.AddDate(0, 0, 1))
				if err != nil {
					return nil, fmt.Errorf("read the CI runs of %s on %s: %w", name, day.Format(time.DateOnly), err)
				}
				checkAttribute, err := readAttribution(ctx, r.store, nil, checks.Runs)
				if err != nil {
					return nil, err
				}
				one.checks, one.checkJobs = checkAttribute.checkRuns(checks.Runs), checks.Jobs
			}
			built, rules, shown = append(built, one), append(rules, repo.rule), append(shown, as[name])
		}
		for i, repo := range events {
			if as[repo.name] || compare {
				built, rules = append(built, postedDayAs(day, merged, posted[i], repo)), append(rules, repo.rule)
				shown = append(shown, as[repo.name])
			}
		}
		firstShipped(built)
		for i := range built {
			if shown[i] {
				read.rules, read.days = append(read.rules, rules[i]), append(read.days, built[i])
			}
		}
		rows := make(map[string]ProjectDay, len(active))
		for project := range active {
			rows[project] = read.Rows(project)
		}
		return rows, nil
	}
	// The zero rows of a project's day are built once for each deploy repository, and each key
	// takes its own of them.
	var zeroProject string
	var zeroDay time.Time
	zeroRows := map[string][]DayRow{}
	zeros := func(project string, day time.Time, key evalKey) []DayRow {
		if project != zeroProject || !day.Equal(zeroDay) {
			zeroProject, zeroDay = project, day
			clear(zeroRows)
		}
		rows, built := zeroRows[key.deployRepo]
		if !built {
			var as []delivery.DeliverySettings
			if key.deployRepo != "" {
				rule, _ := r.ruleOf(key.deployRepo)
				as = []delivery.DeliverySettings{rule}
			}
			rows = ZeroDeliveryDay(project, day, as).Rows
			zeroRows[key.deployRepo] = rows
		}
		return rowsOf(rows, key)
	}
	return spanInput{raw: raw, zeros: zeros}, nil
}

// checkDays is each UTC day from from up to to on which repo holds a PR-checks run: the days a
// span reads the repository's PR-checks runs of (deliverySpanInput), one range of
// delivery_runs_kind_started_at.
func (s *Store) checkDays(ctx context.Context, repo string, from, to time.Time) (map[time.Time]bool, error) {
	rows, err := s.database.Pool.Query(ctx, `
		select distinct (started_at at time zone 'UTC')::date from delivery_runs
		where repo = $1 and kind = $2 and started_at >= $3 and started_at < $4
	`, repo, string(delivery.DeliveryRunKindPRChecks), from, to)
	if err != nil {
		return nil, fmt.Errorf("read the days %s holds a PR-checks run on: %w", repo, err)
	}
	days := map[time.Time]bool{}
	var day time.Time
	if _, err := pgx.ForEachRow(rows, []any{&day}, func() error {
		days[measures.Midnight(day)] = true
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the days %s holds a PR-checks run on: %w", repo, err)
	}
	return days, nil
}

// legionSpanInput is the input of the Legion days a span builds that no change names. A Legion
// series' rows of such a day are its zero rows, or none: a day with input of it holds what it held
// and is left (buildSpan), so the span reads which days hold input of each eval and no row. Each
// project's days with a close and with a filing are read once, of the days from its earliest
// tracked start through reach (issue_facts_closed and issue_filings_day), and a day's input is
// each eval's by legionHasInput, the rule LegionDay.Rows builds its rows by.
func (r *refresh) legionSpanInput(tracked map[string]map[evalKey]*trackedKey, reach time.Time) spanInput {
	type inputDays struct{ closed, filed map[time.Time]bool }
	read := map[string]inputDays{}
	raw := func(ctx context.Context, day time.Time, active map[string]map[evalKey]*trackedKey) (map[string]ProjectDay, error) {
		rows := make(map[string]ProjectDay, len(active))
		for project := range active {
			days, held := read[project]
			if !held {
				var start time.Time
				for _, one := range tracked[project] {
					if start.IsZero() || one.start.Before(start) {
						start = one.start
					}
				}
				days = inputDays{closed: map[time.Time]bool{}, filed: map[time.Time]bool{}}
				found, err := r.store.database.Pool.Query(ctx, `
					select true, closed_at from issue_facts where project_key = $1 and closed_at >= $2 and closed_at < $3
					union all
					select false, filed_at from issue_filings where project_key = $1 and filed_at >= $2 and filed_at < $3
				`, project, start, reach.AddDate(0, 0, 1))
				if err != nil {
					return nil, fmt.Errorf("read the days of %s's Legion input: %w", project, err)
				}
				var closed bool
				var at time.Time
				if _, err := pgx.ForEachRow(found, []any{&closed, &at}, func() error {
					if closed {
						days.closed[measures.Midnight(at)] = true
					} else {
						days.filed[measures.Midnight(at)] = true
					}
					return nil
				}); err != nil {
					return nil, fmt.Errorf("read the days of %s's Legion input: %w", project, err)
				}
				read[project] = days
			}
			since, err := r.sinceOf(ctx, project)
			if err != nil {
				return nil, err
			}
			own := ProjectDay{Project: project, Day: day}
			for _, slug := range legionDaySlugs {
				if legionHasInput(slug, days.closed[day], days.filed[day], since, day) {
					// The row stands for the eval's input of the day, and is never written.
					own.Rows = append(own.Rows, DayRow{ProjectKey: project, EvalID: EvalID(project, slug), DayPart: measures.DayPart{Day: day}})
				}
			}
			rows[project] = own
		}
		return rows, nil
	}
	zeros := func(project string, day time.Time, key evalKey) []DayRow {
		return ZeroLegionDay(project, day, slugOf(project, key.eval)).Rows
	}
	return spanInput{raw: raw, zeros: zeros}
}
