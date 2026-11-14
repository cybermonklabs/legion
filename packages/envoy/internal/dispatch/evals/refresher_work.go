package evals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// refresher_work.go is what one refresher pass does with what its feeds read (refresher.go has the
// loop): which days each change and event touches, the writes of those days, and the walks.
//
// What each change touches, the oracle dispatch://INFRA-2916's k12 reads:
//
//   - pull_request: the pull request's merge day, in each project that day counts it for now, and
//     the day and project eval_day_members holds it under (where it was counted before), so a
//     pull request merged again or moved to another project rewrites both days; then the start
//     day of the deploy run that ships it now and of the one that shipped it when it was last
//     counted (eval_day_members.shipped_by), each of which counts it among the pull requests it
//     ships and counts for its projects (D1), in the run's projects now and before.
//   - run and run_job: the run's start day, its delivery rows (a deploy) and its CI rows (every
//     run on main of the deploy workflow and every PR-checks run), in each project that day counts
//     it for and the projects eval_day_members holds it under; then the merge day of each pull
//     request the run ships now (delivery/containment.go's rule, DeliveryDay.linked) and of each
//     it shipped when that pull request was last counted (shipped_by), whose rows hold the
//     completion of the run that ships it as their deploy time.
//   - settings: every day of every delivery and CI series from its first input day, by beginning
//     the delivery walk again; a change of repo_projects (whose change names the repository) also
//     begins every project's Legion walk again, since the mapping decides the repository an issue
//     or a filing with none of its own resolves to (ReadLegionDay).
//   - issue_facts: the Legion close day of the issue, read from its row, and the day
//     eval_day_members holds it under, so an issue closed again on another day rewrites its old
//     day and its new one, and an open issue whose facts moved touches no day.
//   - issue_filings: the filing's day before and after, in the filing's project.
//   - eval_readings: no day, since the readings write keeps a logged eval's day rows itself; the
//     eval's zero rows are filled up to today, which a reading measured before its eval's first
//     day leaves a gap in.
//   - session_group, eval_versions and eval_days: no day.
//
// The event log adds two more: an issue event that can move the count of open P0 issues with no
// owner (a change of an issue's priority, status or route, its creation or close, a claim or a
// release, on an issue that is a P0 or whose priority moved) rewrites the project's unowned-p0
// row of today; and one that moves an issue's repository or its parent rewrites the days of the
// filings of that issue and of every issue below it, whose repository resolves through it.

// The delivery_changes kinds the refresher reads a day from (delivery.Change.Kind).
const (
	changePullRequest  = "pull_request"
	changeRun          = "run"
	changeRunJob       = "run_job"
	changeSettings     = "settings"
	changeIssueFacts   = "issue_facts"
	changeIssueFilings = "issue_filings"
	changeEvalReadings = "eval_readings"
)

// refresh is one pass's state: the build and the day it writes as, the projects and the delivery
// settings it read, the first input day of each project's series it has read, each project's
// LegionSince, the delivery days it has read, the days it has written and what it has written.
type refresh struct {
	store      *Store
	build      string
	now, today time.Time
	batch      int
	projects   []string
	known      map[string]bool
	// settings is nil where no delivery settings exist, which leaves the delivery and CI rows
	// unwritten: without a deploy repository no day of them can be read.
	settings *delivery.DeliverySettings
	first    map[walkKey]map[string]time.Time
	since    map[string]LegionSince
	reads    map[time.Time]DeliveryDay
	done     map[builtDay]bool
	unowned  map[string]bool
	// walking is set while the pass walks, whose days run in order and so need no gap filled
	// behind a first day they move (lowered).
	walking bool
	lowered []lowered
	report  refreshed
}

// lowered is a series' first day that a written day moved earlier, outside a walk: the days from
// the day after it to the day before the first day the eval held before (today, where it held
// none) have no row of the eval, and the pass fills them with zero rows (fillGaps).
type lowered struct {
	family  string
	project string
	evalID  string
	from    time.Time
	before  *time.Time
}

// load seeds every project no seed has reached (UnseededProjects, Ensure: a project a command or
// a fixture wrote by SQL while the server ran) and reads the projects and the delivery settings.
// A project whose seed fails is logged and seeded again by the next pass; its day rows are written
// all the same, since a day row needs no definition.
func (r *refresh) load(ctx context.Context) error {
	unseeded, err := r.store.UnseededProjects(ctx)
	if err != nil {
		return err
	}
	for _, project := range unseeded {
		if _, err := r.store.Ensure(ctx, project); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("dispatch: the eval day-row refresher could not seed a project's evals; the next pass tries again",
				"project", project, "error", err)
		}
	}
	if r.projects, err = r.store.readProjects(ctx); err != nil {
		return err
	}
	r.known = map[string]bool{}
	for _, project := range r.projects {
		r.known[project] = true
	}
	if err := r.loadSettings(ctx); err != nil {
		return err
	}
	r.done = map[builtDay]bool{}
	// A project the seed just reached was made since the last walk of today, which wrote every
	// project's count of today but its own.
	for _, project := range unseeded {
		if r.known[project] {
			if err := r.writeUnowned(ctx, project); err != nil {
				return err
			}
		}
	}
	return nil
}

// loadSettings reads the delivery settings, nil where none exist yet.
func (r *refresh) loadSettings(ctx context.Context) error {
	settings, err := delivery.GetSettings(ctx, r.store.database.Pool)
	switch {
	case errors.Is(err, delivery.ErrNoSettings):
		r.settings = nil
	case err != nil:
		return fmt.Errorf("read the delivery settings: %w", err)
	default:
		r.settings = &settings
	}
	return nil
}

// pageWork is what one page of the feeds names: the pull requests and runs whose rows changed, by
// row key; the issues whose stored facts changed, by project and key; the filing days that moved,
// by project; the issues whose repository or parent moved; the projects whose count of unowned P0
// issues may have moved; the logged evals whose readings changed, by project; and the walks to
// begin again.
type pageWork struct {
	prs        map[string]pullRequestRef
	runs       map[string]runRef
	issues     map[[2]string]bool
	filings    map[string]map[time.Time]bool
	reparented map[string]bool
	unowned    map[string]bool
	logged     map[string]map[string]bool
	// restart names the families whose walks begin again: walkDelivery, walkLegion (every
	// project's) and walkLogged (every project's).
	restart  map[string]bool
	settings bool
}

// pullRequestRef is a pull request's row by its key; runRef a run's.
type pullRequestRef struct {
	repo   string
	number int
}

type runRef struct {
	repo string
	id   int64
}

func newPageWork() *pageWork {
	return &pageWork{
		prs: map[string]pullRequestRef{}, runs: map[string]runRef{}, issues: map[[2]string]bool{},
		filings: map[string]map[time.Time]bool{}, reparented: map[string]bool{}, unowned: map[string]bool{},
		logged: map[string]map[string]bool{}, restart: map[string]bool{},
	}
}

// restartAll begins every walk again: a database no pass has committed on, or a change cursor the
// log was pruned below, whose lost changes no one can name.
func (w *pageWork) restartAll() {
	w.restart[walkDelivery], w.restart[walkLegion], w.restart[walkLogged] = true, true, true
}

// addChange records what change touches (refresher_work.go's list above). A ref that names no row
// (a pull request number or a run id that is no number) names nothing.
func (w *pageWork) addChange(change delivery.Change) {
	switch change.Kind {
	case changePullRequest:
		if number, err := strconv.Atoi(change.Ref); err == nil {
			w.prs[PullRequestMember(change.Repo, number)] = pullRequestRef{repo: change.Repo, number: number}
		}
	case changeRun, changeRunJob:
		if id, err := strconv.ParseInt(change.Ref, 10, 64); err == nil {
			w.runs[RunMember(change.Repo, id)] = runRef{repo: change.Repo, id: id}
		}
	case changeSettings:
		w.settings = true
		w.restart[walkDelivery] = true
		if change.Repo != "" {
			w.restart[walkLegion] = true
		}
	case changeIssueFacts:
		w.issues[[2]string{change.Repo, change.Ref}] = true
	case changeIssueFilings:
		for _, at := range []*time.Time{change.TOld, change.TNew} {
			if at != nil {
				if w.filings[change.Repo] == nil {
					w.filings[change.Repo] = map[time.Time]bool{}
				}
				w.filings[change.Repo][utcMidnight(*at)] = true
			}
		}
	case changeEvalReadings:
		if w.logged[change.Repo] == nil {
			w.logged[change.Repo] = map[string]bool{}
		}
		w.logged[change.Repo][change.Ref] = true
	}
}

// addEvents records what a page of events touches.
func (w *pageWork) addEvents(page eventPage) {
	for _, project := range page.unowned {
		w.unowned[project] = true
	}
	for _, issue := range page.reparented {
		w.reparented[issue] = true
	}
}

// restarts is the walks w begins again, over projects.
func (w *pageWork) restarts(projects []string) []walkKey {
	var keys []walkKey
	if w.restart[walkDelivery] {
		keys = append(keys, walkKey{family: walkDelivery})
	}
	for _, family := range []string{walkLegion, walkLogged} {
		if w.restart[family] {
			for _, project := range projects {
				keys = append(keys, walkKey{family: family, project: project})
			}
		}
	}
	return keys
}

// page is one page of each feed: it reads where each cursor stands and a page of each feed after
// it, writes the days the page names, and moves the cursors in a transaction of its own, which
// also records the walks the page begins again. It answers whether a feed holds more rows.
//
// A database holding no cursor row has committed no pass: the cursors start at the feeds' heads,
// and every walk begins, which builds every day the feeds' history could name. A change cursor
// whose own row the log no longer holds may have passed over changes it never read
// (delivery.PruneChanges): the cursor moves to the log's head and every walk begins again, which
// covers every change the log ever held.
func (r *refresh) page(ctx context.Context, safe int64, limit int) (bool, error) {
	pool := r.store.database.Pool
	cursors, err := r.store.readRefresherCursors(ctx)
	if err != nil {
		return false, err
	}
	work := newPageWork()
	positions := map[string]int64{}
	full := false
	if len(cursors) == 0 {
		head, err := delivery.LatestChangeSeq(ctx, pool)
		if err != nil {
			return false, fmt.Errorf("read the head of the delivery changes: %w", err)
		}
		positions[changesCursor], positions[eventsCursor] = head, safe
		work.restartAll()
	} else {
		after := cursors[changesCursor]
		lost, err := changesLost(ctx, r.store, after)
		if err != nil {
			return false, err
		}
		if lost {
			head, err := delivery.LatestChangeSeq(ctx, pool)
			if err != nil {
				return false, fmt.Errorf("read the head of the delivery changes: %w", err)
			}
			positions[changesCursor] = head
			work.restartAll()
		} else {
			changes, err := delivery.ChangesSince(ctx, pool, after, limit)
			if err != nil {
				return false, fmt.Errorf("read the delivery changes after %d: %w", after, err)
			}
			positions[changesCursor] = after
			for _, change := range changes {
				positions[changesCursor] = change.Seq
				work.addChange(change)
			}
			full = len(changes) == limit
		}
		events, err := r.store.readRefresherEvents(ctx, cursors[eventsCursor], safe, limit)
		if err != nil {
			return false, err
		}
		positions[eventsCursor] = events.position
		full = full || events.full
		work.addEvents(events)
	}
	if work.settings {
		// The settings or a mapping moved: the days this pass read or wrote before were read
		// under the old ones, so each is read again and written again where a walk reaches it.
		if err := r.loadSettings(ctx); err != nil {
			return false, err
		}
		r.reads, r.done = map[time.Time]DeliveryDay{}, map[builtDay]bool{}
		for _, project := range r.projects {
			delete(r.first, walkKey{family: walkDelivery, project: project})
		}
	}
	if err := r.apply(ctx, work); err != nil {
		return false, err
	}
	if err := r.store.commitPage(ctx, positions, work.restarts(r.projects), r.build, r.now); err != nil {
		return false, err
	}
	return full, nil
}

// changesLost reports whether the change at the cursor is gone from the log, which is a cursor
// that may have passed over changes it never read. A cursor of 0 has read nothing and lost nothing.
func changesLost(ctx context.Context, s *Store, cursor int64) (bool, error) {
	if cursor == 0 {
		return false, nil
	}
	at, err := delivery.ChangeAt(ctx, s.database.Pool, cursor)
	if err != nil {
		return false, fmt.Errorf("read the delivery change at %d: %w", cursor, err)
	}
	return at == nil, nil
}

// apply writes every day w names, then fills the zero rows behind a first day those writes moved
// earlier.
func (r *refresh) apply(ctx context.Context, w *pageWork) error {
	if err := r.applyDelivery(ctx, w); err != nil {
		return err
	}
	if err := r.applyLegion(ctx, w); err != nil {
		return err
	}
	for _, project := range slices.Sorted(maps.Keys(w.unowned)) {
		if r.known[project] {
			if err := r.writeUnowned(ctx, project); err != nil {
				return err
			}
		}
	}
	for _, project := range slices.Sorted(maps.Keys(w.logged)) {
		if r.known[project] {
			if err := r.fillLogged(ctx, project, slices.Sorted(maps.Keys(w.logged[project]))); err != nil {
				return err
			}
		}
	}
	return r.fillGaps(ctx)
}

// deliveryItem is a pull request or a run by its row key, and the UTC midnight of its day as its
// row stands now: the pull request's merge, the run's start; nil where it has none (a pull request
// not merged, or a row gone).
type deliveryItem struct {
	member string
	day    *time.Time
}

// applyDelivery writes the delivery and CI days of the pull requests and runs w names, then the
// days those items are linked to: the deploy run that ships a pull request now (DeliveryDay.linked)
// and the one that shipped it when it was last counted (eval_day_members.shipped_by), and the pull
// requests a run ships now and shipped before. A pull request a changed run ships now may have
// shipped with another run before, which counts it among its pull requests until its day is
// written, so the runs that shipped the linked pull requests are written last. No link beyond
// those is followed: the rows of the items behind them did not move.
func (r *refresh) applyDelivery(ctx context.Context, w *pageWork) error {
	if r.settings == nil || len(w.prs)+len(w.runs) == 0 {
		return nil
	}
	items, err := r.store.deliveryItemDays(ctx, w.prs, w.runs)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, item := range items {
		seen[item.member] = true
	}
	linked, former, err := r.rewriteDelivery(ctx, items, true)
	if err != nil {
		return err
	}
	next, err := r.followUp(ctx, linked, former, seen)
	if err != nil {
		return err
	}
	_, former, err = r.rewriteDelivery(ctx, next, false)
	if err != nil {
		return err
	}
	last, err := r.followUp(ctx, nil, former, seen)
	if err != nil {
		return err
	}
	_, _, err = r.rewriteDelivery(ctx, last, false)
	return err
}

// followUp is the items of linked and the deploy runs former names (by row key) that seen does
// not hold yet, each run with the day its row stands at now, and adds them to seen.
func (r *refresh) followUp(ctx context.Context, linked []deliveryItem, former []string, seen map[string]bool) ([]deliveryItem, error) {
	var next []deliveryItem
	for _, item := range linked {
		if !seen[item.member] {
			seen[item.member] = true
			next = append(next, item)
		}
	}
	runs := map[string]runRef{}
	for _, member := range former {
		if run, ok := parseRunMember(member); ok && !seen[member] {
			seen[member] = true
			runs[member] = run
		}
	}
	if len(runs) == 0 {
		return next, nil
	}
	days, err := r.store.deliveryItemDays(ctx, nil, runs)
	if err != nil {
		return nil, err
	}
	return append(next, days...), nil
}

// parseRunMember is the run a run's row key names (RunMember), false for a key that names none.
func parseRunMember(member string) (runRef, bool) {
	rest, ok := strings.CutPrefix(member, "run:")
	slash := strings.LastIndex(rest, "/")
	if !ok || slash < 0 {
		return runRef{}, false
	}
	id, err := strconv.ParseInt(rest[slash+1:], 10, 64)
	if err != nil {
		return runRef{}, false
	}
	return runRef{repo: rest[:slash], id: id}, true
}

// rewriteDelivery writes, for each item, its day in every project that day counts it for, and the
// day eval_day_members holds it under in that project, and answers the deploy runs that shipped a
// pull request among items when it was last counted. Where follow is set it also writes the merge
// day of every pull request a run among items shipped when that pull request was last counted,
// which the run's day no longer names once it stops shipping it, and answers the items of other
// days each item's day, and each of those pull requests' days, links it to now.
func (r *refresh) rewriteDelivery(ctx context.Context, items []deliveryItem, follow bool) ([]deliveryItem, []string, error) {
	if len(items) == 0 {
		return nil, nil, nil
	}
	members := make([]string, len(items))
	for i, item := range items {
		members[i] = item.member
	}
	held, err := r.store.heldMembers(ctx, r.projects, deliveryDayFamilies, members)
	if err != nil {
		return nil, nil, err
	}
	var former []string
	for _, member := range held {
		if member.shippedBy != "" {
			former = append(former, member.shippedBy)
		}
	}
	var shipped []heldMember
	if follow {
		if shipped, err = r.store.shippedMembers(ctx, r.projects, members); err != nil {
			return nil, nil, err
		}
	}
	type job struct{ projects, members map[string]bool }
	jobs := map[time.Time]*job{}
	jobOf := func(day time.Time) *job {
		if jobs[day] == nil {
			jobs[day] = &job{projects: map[string]bool{}, members: map[string]bool{}}
		}
		return jobs[day]
	}
	for _, item := range items {
		if item.day != nil {
			jobOf(*item.day).members[item.member] = true
		}
	}
	for _, member := range held {
		jobOf(member.day).projects[member.project] = true
	}
	// A pull request a run shipped before is an item of its merge day too: the run that ships it
	// now, which the day's read links it to, counts it among its pull requests from this change on.
	for _, member := range shipped {
		job := jobOf(member.day)
		job.projects[member.project], job.members[member.member] = true, true
	}
	var linked []deliveryItem
	for _, day := range slices.SortedFunc(maps.Keys(jobs), time.Time.Compare) {
		read, err := r.readDelivery(ctx, day)
		if err != nil {
			return nil, nil, err
		}
		projects := maps.Clone(jobs[day].projects)
		for member := range jobs[day].members {
			for _, project := range read.countedFor(member) {
				projects[project] = true
			}
			if follow {
				for _, item := range read.linked(member) {
					linked = append(linked, deliveryItem{member: item.member, day: &item.day})
				}
			}
		}
		for _, project := range slices.Sorted(maps.Keys(projects)) {
			if r.known[project] {
				if err := r.writeDelivery(ctx, read, project, true); err != nil {
					return nil, nil, err
				}
			}
		}
	}
	return linked, former, nil
}

// readDelivery is the delivery rows of day (ReadDeliveryDay), read once a pass.
func (r *refresh) readDelivery(ctx context.Context, day time.Time) (DeliveryDay, error) {
	if read, ok := r.reads[day]; ok {
		return read, nil
	}
	read, err := r.store.ReadDeliveryDay(ctx, *r.settings, day)
	if err != nil {
		return DeliveryDay{}, err
	}
	r.reads[day] = read
	return read, nil
}

// writeDelivery writes project's delivery and CI rows of read's day, with a zero row of every
// series the day has no input of whose first input day is on or before it (zerosFrom), once a
// pass. A project the day counts nothing for and holds no series of before that day writes nothing
// unless explicit, which a project eval_day_members named is: its write zeroes what the day held.
func (r *refresh) writeDelivery(ctx context.Context, read DeliveryDay, project string, explicit bool) error {
	key := builtDay{family: walkDelivery, project: project, day: read.Day}
	if r.done[key] {
		return nil
	}
	first, err := r.firstOf(ctx, walkDelivery, project)
	if err != nil {
		return err
	}
	built := read.Rows(project)
	day := built.WithZeros(zerosFrom(ZeroDeliveryDay(*r.settings, project, read.Day).Rows, first, read.Day))
	if len(day.Rows) == 0 && !explicit {
		return nil
	}
	if err := r.store.WriteProjectDay(ctx, day, r.build); err != nil {
		return err
	}
	r.noteBuilt(walkDelivery, project, read.Day, built.Rows)
	r.done[key], r.report.days[key] = true, true
	return nil
}

// applyLegion writes the Legion days w names: the close day and the held day of each issue whose
// stored facts changed, the filing days that moved, and the filing days of every issue below an
// issue whose repository or parent moved.
func (r *refresh) applyLegion(ctx context.Context, w *pageWork) error {
	days := map[string]map[time.Time]bool{}
	add := func(project string, day time.Time) {
		if days[project] == nil {
			days[project] = map[time.Time]bool{}
		}
		days[project][day] = true
	}
	if len(w.issues) > 0 {
		touched, err := r.store.legionItemDays(ctx, slices.Collect(maps.Keys(w.issues)))
		if err != nil {
			return err
		}
		for _, day := range touched {
			add(day.project, day.day)
		}
	}
	if len(w.reparented) > 0 {
		touched, err := r.store.subtreeFilingDays(ctx, slices.Sorted(maps.Keys(w.reparented)))
		if err != nil {
			return err
		}
		for _, day := range touched {
			add(day.project, day.day)
		}
	}
	for project, filed := range w.filings {
		for day := range filed {
			add(project, day)
		}
	}
	for _, project := range slices.Sorted(maps.Keys(days)) {
		if !r.known[project] {
			continue
		}
		for _, day := range slices.SortedFunc(maps.Keys(days[project]), time.Time.Compare) {
			if err := r.writeLegion(ctx, project, day, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeLegion writes project's Legion rows of day, with a zero row of every series the day has no
// input of whose first input day is on or before it, once a pass. A day with no input and no
// series begun before it writes nothing unless explicit, which a change naming the day is: its
// write zeroes what the day held.
func (r *refresh) writeLegion(ctx context.Context, project string, day time.Time, explicit bool) error {
	key := builtDay{family: walkLegion, project: project, day: day}
	if r.done[key] {
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
	first, err := r.firstOf(ctx, walkLegion, project)
	if err != nil {
		return err
	}
	built := read.Rows(since)
	written := built.WithZeros(zerosFrom(ZeroLegionDay(project, read.Day, legionDaySlugs...).Rows, first, read.Day))
	if len(written.Rows) == 0 && !explicit {
		return nil
	}
	if err := r.store.WriteLegionProjectDay(ctx, written, r.build); err != nil {
		return err
	}
	r.noteBuilt(walkLegion, project, read.Day, built.Rows)
	r.done[key], r.report.days[key] = true, true
	return nil
}

// writeUnowned writes project's unowned-p0 row of today, once a pass.
func (r *refresh) writeUnowned(ctx context.Context, project string) error {
	if r.unowned[project] {
		return nil
	}
	if err := r.store.WriteUnownedP0Day(ctx, project, r.today, r.build); err != nil {
		return err
	}
	r.unowned[project] = true
	r.report.days[builtDay{family: SlugUnownedP0, project: project, day: r.today}] = true
	return nil
}

// fillLogged writes the zero rows of project's logged evals ids names (every one where ids is nil)
// from each series' first day to today (fillLoggedZeros), recorded where it wrote any.
func (r *refresh) fillLogged(ctx context.Context, project string, ids []string) error {
	filled, err := r.store.fillLoggedZeros(ctx, project, ids, r.today, r.build)
	if err != nil {
		return err
	}
	if filled > 0 {
		r.report.days[builtDay{family: walkLogged, project: project, day: r.today}] = true
	}
	return nil
}

// sinceOf is project's LegionSince, read once a pass.
func (r *refresh) sinceOf(ctx context.Context, project string) (LegionSince, error) {
	if since, ok := r.since[project]; ok {
		return since, nil
	}
	since, err := r.store.ReadLegionSince(ctx, project)
	if err != nil {
		return LegionSince{}, err
	}
	r.since[project] = since
	return since, nil
}

// firstOf is the first input day of each eval of family that project holds a row of, by eval id,
// read once a pass and moved by every day the pass writes (noteBuilt): the first day of each
// eval's all-repositories rows of the series its zero day holds (firstDays).
func (r *refresh) firstOf(ctx context.Context, family, project string) (map[string]time.Time, error) {
	key := walkKey{family: family, project: project}
	if first, ok := r.first[key]; ok {
		return first, nil
	}
	var zero []DayRow
	switch family {
	case walkDelivery:
		zero = ZeroDeliveryDay(*r.settings, project, r.today).Rows
	case walkLegion:
		zero = ZeroLegionDay(project, r.today, legionDaySlugs...).Rows
	}
	first, err := r.store.firstDays(ctx, project, zero)
	if err != nil {
		return nil, err
	}
	r.first[key] = first
	return first, nil
}

// zerosFrom is each of zeros whose eval's first input day is on or before day: the zero rows a day
// with no input of a series writes, which no day before a series' first input day holds.
func zerosFrom(zeros []DayRow, first map[string]time.Time, day time.Time) []DayRow {
	var kept []DayRow
	for _, zero := range zeros {
		if since, ok := first[zero.EvalID]; ok && !day.Before(since) {
			kept = append(kept, zero)
		}
	}
	return kept
}

// noteBuilt moves the first input day of each eval built holds an all-repositories row of to day,
// where day is before it or the eval held none: its first input day is now. Outside a walk the
// days from day to the old first day (to today, where there was none) have no row of the eval, and
// the pass fills them (fillGaps).
func (r *refresh) noteBuilt(family, project string, day time.Time, built []DayRow) {
	first := r.first[walkKey{family: family, project: project}]
	for _, row := range built {
		if row.Repo != "" {
			continue
		}
		since, ok := first[row.EvalID]
		if ok && !day.Before(since) {
			continue
		}
		if !r.walking {
			moved := lowered{family: family, project: project, evalID: row.EvalID, from: day}
			if ok {
				moved.before = &since
			}
			r.lowered = append(r.lowered, moved)
		}
		first[row.EvalID] = day
	}
}

// fillGaps writes the zero rows of every day behind a first day the pass moved earlier, from the
// day after the new first day to the day before the old one (to today, where the eval held no
// row), each project's of each family in one transaction. A day that holds a row of a series
// already keeps it (insertMissingDays).
func (r *refresh) fillGaps(ctx context.Context) error {
	if len(r.lowered) == 0 {
		return nil
	}
	gaps := map[walkKey][]DayRow{}
	for _, moved := range r.lowered {
		end := r.today
		if moved.before != nil {
			end = moved.before.AddDate(0, 0, -1)
		}
		slug := strings.TrimPrefix(moved.evalID, moved.project+"/")
		for day := moved.from.AddDate(0, 0, 1); !day.After(end); day = day.AddDate(0, 0, 1) {
			var zero []DayRow
			switch moved.family {
			case walkDelivery:
				zero = ZeroDeliveryDay(*r.settings, moved.project, day).Rows
			case walkLegion:
				zero = ZeroLegionDay(moved.project, day, slug).Rows
			}
			key := walkKey{family: moved.family, project: moved.project}
			for _, row := range zero {
				if row.EvalID == moved.evalID {
					gaps[key] = append(gaps[key], row)
				}
			}
		}
	}
	r.lowered = nil
	for _, key := range slices.SortedFunc(maps.Keys(gaps), func(a, b walkKey) int {
		return strings.Compare(a.family+" "+a.project, b.family+" "+b.project)
	}) {
		if err := r.store.insertZeroRows(ctx, gaps[key], r.build); err != nil {
			return err
		}
	}
	return nil
}

// walk makes a batch of each walk: the delivery walk (where delivery settings exist), and each
// project's Legion walk and logged zero fill. It answers whether a walk has days left, which asks
// for the next pass at once.
func (r *refresh) walk(ctx context.Context) (bool, error) {
	walks, err := r.store.readWalks(ctx)
	if err != nil {
		return false, err
	}
	r.walking = true
	defer func() { r.walking = false }()
	pending := false
	if r.settings != nil {
		more, err := r.walkDays(ctx, walks, walkKey{family: walkDelivery}, r.deliveryStart, r.deliveryWalkDay)
		if err != nil {
			return false, err
		}
		pending = pending || more
	}
	for _, project := range r.projects {
		more, err := r.walkDays(ctx, walks, walkKey{family: walkLegion, project: project},
			func(ctx context.Context) (*time.Time, error) { return r.legionStart(ctx, project) },
			func(ctx context.Context, day time.Time) error { return r.writeLegion(ctx, project, day, false) })
		if err != nil {
			return false, err
		}
		pending = pending || more
		if err := r.walkLogged(ctx, walks, project); err != nil {
			return false, err
		}
	}
	return pending, nil
}

// walkDays makes one batch of the walk key: from the first day it has not built, or, where it is
// to begin (no record, a record of another build, or one begun again), from the first input day
// begin answers, it builds up to r.batch days through today with build, and records where it
// stands. A walk with nothing to walk (begin answers nil) stands at tomorrow. It answers whether
// days before today are left.
func (r *refresh) walkDays(ctx context.Context, walks map[walkKey]walkState, key walkKey,
	begin func(context.Context) (*time.Time, error), build func(context.Context, time.Time) error) (bool, error) {
	state, found := walks[key]
	begun := !found || state.build != r.build || state.next == nil
	next := state.next
	if begun {
		start, err := begin(ctx)
		if err != nil {
			return false, err
		}
		if start == nil {
			return false, r.store.recordWalk(ctx, key, r.build, r.today.AddDate(0, 0, 1), r.now)
		}
		next = start
	}
	if next.After(r.today) {
		if begun {
			return false, r.store.recordWalk(ctx, key, r.build, *next, r.now)
		}
		return false, nil
	}
	last := next.AddDate(0, 0, max(r.batch, 1)-1)
	if last.After(r.today) {
		last = r.today
	}
	for day := *next; !day.After(last); day = day.AddDate(0, 0, 1) {
		if err := build(ctx, day); err != nil {
			return false, err
		}
	}
	if err := r.store.recordWalk(ctx, key, r.build, last.AddDate(0, 0, 1), r.now); err != nil {
		return false, err
	}
	return last.Before(r.today), nil
}

// deliveryStart is the first input day of the delivery walk: the earliest of the first merge of a
// pull request, the first start of a run of the deploy repository and the first day any project
// holds a delivery or CI row of, nil where none exists.
func (r *refresh) deliveryStart(ctx context.Context) (*time.Time, error) {
	earliest, err := r.store.deliveryInputStart(ctx, r.settings.DeployRepo)
	if err != nil {
		return nil, err
	}
	for _, project := range r.projects {
		first, err := r.firstOf(ctx, walkDelivery, project)
		if err != nil {
			return nil, err
		}
		earliest = earliestOf(earliest, first)
	}
	return earliest, nil
}

// legionStart is the first input day of project's Legion walk: the earliest of its first close of
// an issue with Legion phase work, its first filing and the first day it holds a Legion row of,
// nil where none exists.
func (r *refresh) legionStart(ctx context.Context, project string) (*time.Time, error) {
	earliest, err := r.store.legionInputStart(ctx, project)
	if err != nil {
		return nil, err
	}
	first, err := r.firstOf(ctx, walkLegion, project)
	if err != nil {
		return nil, err
	}
	return earliestOf(earliest, first), nil
}

// earliestOf is the UTC midnight of the earliest of earliest and the days of first.
func earliestOf(earliest *time.Time, first map[string]time.Time) *time.Time {
	for _, day := range first {
		if earliest == nil || day.Before(*earliest) {
			earliest = &day
		}
	}
	if earliest == nil {
		return nil
	}
	day := utcMidnight(*earliest)
	return &day
}

// deliveryWalkDay writes one day of the delivery walk: every project's delivery and CI rows of it,
// and on today every project's unowned-p0 row of today.
func (r *refresh) deliveryWalkDay(ctx context.Context, day time.Time) error {
	read, err := r.readDelivery(ctx, day)
	if err != nil {
		return err
	}
	for _, project := range r.projects {
		if err := r.writeDelivery(ctx, read, project, false); err != nil {
			return err
		}
	}
	if day.Equal(r.today) {
		for _, project := range r.projects {
			if err := r.writeUnowned(ctx, project); err != nil {
				return err
			}
		}
	}
	return nil
}

// walkLogged fills project's logged evals' zero rows through today once a day, and again when its
// walk is begun again.
func (r *refresh) walkLogged(ctx context.Context, walks map[walkKey]walkState, project string) error {
	key := walkKey{family: walkLogged, project: project}
	state, found := walks[key]
	if found && state.build == r.build && state.next != nil && state.next.After(r.today) {
		return nil
	}
	if err := r.fillLogged(ctx, project, nil); err != nil {
		return err
	}
	return r.store.recordWalk(ctx, key, r.build, r.today.AddDate(0, 0, 1), r.now)
}
