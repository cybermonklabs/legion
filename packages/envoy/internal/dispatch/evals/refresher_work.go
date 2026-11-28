package evals

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
//     ships and counts for its projects (D1), in the run's projects now and before; the start day
//     of each of its PR-checks runs, each of which counts for its project (delivery_days.go's
//     attribution), in the run's projects now and before; and, where its merge moved from before
//     its repository's history began, where it was an item of no day of the repository, the start
//     day of the deploy that shipped it there (shippedNeighbours).
//   - run and run_job: the run's start day, its delivery rows (a deploy) and its CI rows (every
//     run on main of the deploy workflow and every PR-checks run), in each project that day counts
//     it for and the projects eval_day_members holds it under; then the merge day of each pull
//     request the run ships now (delivery/containment.go's rule, DeliveryDay.linked) and of each
//     it shipped when that pull request was last counted (shipped_by), whose rows hold the
//     completion of the run that ships it as their deploy time; and, where the run is or can
//     have been the first deploy of its repository to complete, the start day of the deploy
//     that is the first to complete without it, which ships the pull requests merged before the
//     repository's history began when the run does not (shippedNeighbours: no item records the
//     deploy of such a pull request).
//   - settings: a change of a project's delivery settings rewrites the days of the delivery and CI
//     evals it moves of each repository whose rule it moved, and of no other repository, from
//     their first input day on, staged aside in short transactions and published in one with the
//     rule, and begins no walk (settings_rewrite.go: settingsMoves names what each field of a rule
//     moves and movedDeliveryDays the days it can move, and the pass finds the change by comparing
//     each repository's rule it reads with the one the rows stand built under, so one that moves no
//     eval rewrites nothing). A source move publishes a new selection (syncSettings); the
//     repository gains rows where it had no rule before (syncKept), staged aside and published by
//     the page's commit that records its first rule (stageKept). A settings save rewrites that
//     project's failure rows. Changes to published defaults or source-key readers rewrite the
//     affected measured projects' failure rows after publication (applyFailures, failure_days.go).
//   - production_events: a posted deploy of a project whose events repository is read, its
//     finished day before the change and after it (t_old and t_new), the merge days of the pull
//     requests it names now and the days eval_day_members holds it and the pull requests it
//     shipped under, in its project alone (applyPosted); and in every project whose failure
//     measures are on, the days the row counts on before and after the change (a deploy's
//     finish, its failures' deploy's finish, a restore's time) and the finish day of the deploy
//     that restored a failure it names (applyFailures).
//   - issue_facts: the issue's Legion close day before the change and after it, which the change
//     names itself (t_old and t_new, migration 20261117100002) and no read is made to find: a
//     Legion day counts an issue on its close day alone, so an issue closed again on another day
//     rewrites its old day and its new one, and an open issue whose facts moved touches no day.
//     eval_day_members is not read for it, since the day it holds the issue under is the close
//     day before the change, which the change already names: the days rewritten are the days
//     the change marks, and no other. A facts row whose repository moved logs, beside its own
//     change, an issue_filings change for the issue's filing, which the Legion days cut by that
//     repository on the filing's day (migration 20261117100002).
//   - issue_filings: the filing's day before and after, in the filing's project.
//   - eval_readings: no day, since the readings write keeps a logged eval's day rows itself, the
//     zero rows its readings leave owed among them (writeLoggedZeros).
//   - session_group, eval_versions and eval_days: no day.
//
// The event log adds two more: an issue event that can move the count of open P0 issues with no
// owner (a change of an issue's priority, status or route, its creation or close, a claim or a
// release, on an issue that is a P0 or whose priority moved) rewrites the project's unowned-p0
// row of today; and one that moves an issue's repository or its parent rewrites the days of the
// filings of that issue and of every issue below it, whose repository resolves through it.
//
// A day a change names is written from its own input, and every day it does not name is the
// walk's (refresh.walkDays) or, where the change moves what such a day holds, the page's own
// (span.go), so the stored rows are a function of the inputs and the settings alone, whichever
// order the inputs arrived in (dispatch://INFRA-2916, items 5 and 8):
//
//   - the write holds the rows the day builds and deletes the rows of the day it no longer builds
//     (deleteStaleDays), and writes the zero row of each series the day has no input of from the
//     series' first input day on only where the stored rows are whole through the day
//     (eval_days_serving, servingNext), whatever walk or rebuild runs: a day after them, or of a
//     family no rebuild has flipped yet, holds its rows with input alone, and reads "no data"
//     where it has none, until the walk or the rebuild builds it;
//   - a page whose days move a series' first input day earlier, or create one, or find that a
//     series' first input day holds no input of it any more, and one that reads a change in the
//     repositories that hold a delivery run (syncKept), builds every other day those moves reach,
//     through the last day the stored rows are whole through, and writes them with the days it
//     names and its cursors in one transaction (refresh.writeNamed, commitPage): it moves no
//     walk, and a view of the rows is sent one change (dispatch://INFRA-2916, R14);
//   - the walk computes every day it reaches, a day a page of the same pass wrote included, and
//     every day holding rows of its family, whether or not it builds a row of it; each series'
//     first input day it builds by is the first it holds among the days before the walk's next
//     day, which the walk or a page's span built, or the first it builds itself
//     (refresh.firstOf).
//
// A release, a lost change cursor and a database whose rows' settings are not recorded begin no
// walk: they rebuild aside and flip what moved in one commit (rebuild.go). A family no rows are
// served of, a database's or a project's first seen, has nothing to keep serving and is walked in
// the open (walkDays).

// refresh is one pass's state: the build and the day it writes as, the projects and the delivery
// settings it read, the first input day of each project's series it has read (of each deploy
// repository's rows of it, evalKey), each project's LegionSince, what it has written, and the
// page's own reads and writes: the delivery days it has read, the days it has written and the
// projects whose unowned-p0 row of today it has written. now is the pass's moment, which every row
// is built as of; clock is the refresher's clock, which each page reads afresh for its change
// cursor (page).
type refresh struct {
	store      *Store
	build      string
	now, today time.Time
	clock      func() time.Time
	// batch is how many days of each walk the pass builds, how many end days a batch of a project's
	// figures walk folds at most, and how many a batch of a stale cut's refold does; refoldBudget
	// is about how long each such batch takes after the first of the project's figures walk or of
	// the cut's refold in the pass, none where no time bounds it (refresherSchedule,
	// refoldBatchDays).
	batch        int
	refoldBudget time.Duration
	// projects is every project's key, in key order, and known holds them; absent holds the keys
	// the page has looked for among the projects and not found (knows). owners is the delivery
	// family's owners, every project and every owner key (owners.go) the store holds rows of or
	// the pass's reads build rows of (noteOwners), whose delivery and CI days the walks, pages,
	// spans, settings rewrites and rebuilds write alike. An owner key is no project: the Legion
	// walks, the logged and unowned-p0 rows, the seed and the stored figures are the projects'.
	projects []string
	owners   []string
	known    map[string]bool
	absent   map[string]bool
	// rules is the rule of each deploy source repository of a delivery-on project
	// (delivery.DeployRules), and of each such project's events repository where it holds an
	// events source (eventsRule), by delivery.RuleKey: empty where no project has delivery on,
	// which leaves the delivery and CI rows unwritten, since no production rule judges a run and no
	// deploy card follows a job.
	rules map[string]delivery.DeliverySettings
	// failures is each project whose failure measures are on, by key (failureProjectOf), read
	// with rules; failuresWritten the loop's UTC day on which a whole write of this process last
	// wrote each project's failure rows (writeFailures), which the walk writes once a day
	// (walkFailures).
	failures        map[string]failureProject
	failuresWritten map[string]time.Time
	// builtUnder is the rule each repository's stored delivery and CI rows stand built under
	// (eval_days_rules, by delivery.RuleKey), read by the pass's first page (builtRead) and moved
	// by each of its pages that brings the rows to the rules it reads (syncSettings, syncKept);
	// empty where none is recorded. keptUnder is the repositories the rows stand built for (those
	// of builtUnder with a start), read with it and moved by each page that reads another set
	// (syncKept).
	builtUnder map[string]builtRule
	keptUnder  []keptRepo
	builtRead  bool
	// first is the first input day of each project's series the store holds (storedFirst), read
	// once a pass and moved by each day a page writes (noteBuilt); walkFirst is the walk's own,
	// read of the days before walkFrom, where each walk's batch began (a rebuild's: the first input
	// days its walk found, rebuildFirsts), and moved by each day the walk writes (firstOf).
	first     map[walkKey]map[evalKey]time.Time
	walkFirst map[walkKey]map[evalKey]time.Time
	walkFrom  map[walkKey]time.Time
	since     map[string]LegionSince
	// reads, done and unowned hold for one page (page sets them anew) and then the walks: a page
	// writes a day once, from one read of it, and a change to that day committed after the read
	// is on a later page, which must read and write the day again. Kept for the whole pass, they
	// would let that page skip the day while its cursor moved past the change, and the day would
	// stand stale until another change named it. The walk sets done anew too, since it computes
	// every day it reaches, a day a page wrote included. deployRepos is the page's one read of the
	// repositories whose rows of the evals that read the deploy repository are kept
	// (readDeployRepos), made after the page has read its changes, so a repository whose first run
	// is among them has its rows written.
	reads       map[time.Time]deliveryRead
	done        map[builtDay]bool
	unowned     map[string]bool
	deployRepos []keptRepo
	// serving is the page's one read of the serving records (servingNext): what tells a day the
	// stored rows are whole through from one they are not.
	serving map[walkKey]servingState
	// release is the loop's watch of the release rule (releaseWatch), which load takes from the
	// loop's seeding; rebuilding is the rebuild that is building, as the pass's walks read it
	// (rebuildDays); and staging, while a rebuild's walk builds a batch, the days it built, which
	// the batch stages where a walk writes them (stageWalk).
	release    *releaseWatch
	rebuilding *rebuildState
	staging    *stageBatch
	// walkApplies is the deploy runs of each kept repository the delivery walk's batch read once
	// from walkAppliesFrom on (deliveryWalkDay), for the pass's walk alone, and walkPosted the
	// posted deploys of each kept events repository the batch read with them, that finished from
	// walkAppliesFrom on.
	walkApplies     map[string]*delivery.ApplyRows
	walkPosted      map[string]postedRecord
	walkAppliesFrom time.Time
	// walkHeld is each project's day of each walk's batch that holds rows of the walk's family,
	// which the walk writes whether or not it builds a row of it (heldDays).
	walkHeld map[builtDay]bool
	// walking is set while the pass walks, whose days run in order from the first input day or
	// from days the walk built, so they write every zero row from each series' first input day
	// on, and whose days' stored figures are folded once the walks' batches are written
	// (foldWalked) rather than in each day's own transaction.
	walking bool
	// walked is the span of days the pass's walks wrote, by family and project (noteWalked).
	walked map[walkKey]daySpan
	// batched is the walks' batch of the gap whose days the pass's walks build (gapBatch), and
	// walkBuilt how many writes they have made: days of the day rows' walks, daily writes and
	// projects' batches of the figures walk. A pass that has made one stops its walks for an owed
	// wake (yields).
	batched   *gapBatch
	walkBuilt int
	report    refreshed
}

// load reads the projects and the delivery settings and seeds the projects the pass owes a seed
// (the seed's rules, seed.go): every project on the first pass of this process, so the catalogue
// this build states reaches every project; on a later pass every project no seed has reached
// (UnseededProjects: one a command or a fixture wrote by SQL while the server ran); and on every
// pass each project whose seed failed before, and each whose seed was refused for its `evals-md`
// once that artifact is a document (seedRefusals). A project whose seed fails is logged and seeded
// again by the next pass; its day rows are written all the same, since a day row needs no
// definition. seeds is the loop's, which each pass reads and moves, and which holds the loop's
// watch of the release rule too (releaseWatch).
func (r *refresh) load(ctx context.Context, seeds *seeding) error {
	r.release = &seeds.release
	var err error
	if r.projects, err = r.store.Projects(ctx); err != nil {
		return err
	}
	r.known = map[string]bool{}
	for _, project := range r.projects {
		r.known[project] = true
	}
	repoOwners, err := readOwners(ctx, r.store.database.Pool)
	if err != nil {
		return err
	}
	r.owners = slices.Concat(r.projects, repoOwners)
	slices.Sort(r.owners)
	owed := r.projects
	if seeds.everyProject {
		if owed, err = r.store.UnseededProjects(ctx); err != nil {
			return err
		}
	}
	if owed, err = seeds.refused.pending(ctx, r.store.database.Pool, slices.Concat(owed, seeds.owed())); err != nil {
		return err
	}
	for _, project := range owed {
		_, err := r.store.Ensure(ctx, project)
		seeds.note(project, err)
		r.report.seeded = append(r.report.seeded, project)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("dispatch: the eval day-row refresher could not seed a project's evals; the next pass tries again",
				"project", project, "error", err)
		}
	}
	seeds.everyProject = true
	if seeds.failuresWritten == nil {
		seeds.failuresWritten = map[string]time.Time{}
	}
	r.failuresWritten = seeds.failuresWritten
	return r.loadSettings(ctx)
}

// loadSettings reads every project's delivery settings (delivery.ListProjectSettings) as the rule
// of each deploy source repository of a delivery-on project (delivery.DeployRules) and of each such
// project's events repository where it holds an events source (eventsRule), by delivery.RuleKey,
// and as each project whose failure measures are on (failureProjectOf).
func (r *refresh) loadSettings(ctx context.Context) error {
	projects, err := delivery.ListProjectSettings(ctx, r.store.database.Pool)
	if err != nil {
		return fmt.Errorf("read the delivery settings: %w", err)
	}
	r.rules = map[string]delivery.DeliverySettings{}
	for _, rule := range delivery.DeployRules(projects) {
		r.rules[delivery.RuleKey(rule.DeployRepo)] = rule
	}
	r.failures = map[string]failureProject{}
	for _, project := range projects {
		if project.Delivery && holdsEventsSource(project) {
			rule := eventsRule(project.Project)
			r.rules[delivery.RuleKey(rule.DeployRepo)] = rule
		}
		if failures, on := failureProjectOf(project); on {
			r.failures[project.Project] = failures
		}
	}
	return nil
}

// delivers reports whether any project has delivery on with a deploy source: the delivery and CI
// rows are written only then.
func (r *refresh) delivers() bool {
	return len(r.rules) > 0
}

// ruleOf is the rule the pass reads for repo (refresh.rules), its DeployRepo repo as its rows are
// keyed, false where no source reads it.
func (r *refresh) ruleOf(repo string) (delivery.DeliverySettings, bool) {
	rule, held := r.rules[delivery.RuleKey(repo)]
	rule.DeployRepo = repo
	return rule, held
}

// pageWork is what one page of the feeds names: the pull requests and runs whose rows changed, by
// row key; the merge times its changes moved a pull request from and the earliest start they name
// of a deploy run, by repository; the rows of each project's record of deploys and failures whose
// changes it names, by project; the Legion days the facts and filing changes name, by project; the
// issues whose repository or parent moved; the projects whose count of unowned P0 issues may have
// moved; the walks to begin again; the rebuild to begin; whether a change of the delivery settings
// is among its changes; whether the page records the rules the delivery and CI rows stand built
// under with its cursors, and whether that record moves the repositories every evals view chooses
// among; and the repositories they stand built for, with those they stood built for before.
type pageWork struct {
	prs  map[string]pullRequestRef
	runs map[string]runRef
	// left is, for each repository, the merge time of each pull request of it before a change of
	// the page that moved that merge or removed it (the change's t_old), and ran the earliest
	// time a change of the page names of a deploy run of it (t_old and t_new, the run's start
	// before the change and after it): the deploy that ships a pull request merged before its
	// repository's history began is found by them, since no item records that deploy
	// (shippedNeighbours).
	left map[string][]time.Time
	ran  map[string]time.Time
	// prodEvents is, for each project, the ids of the rows of its record (production_events) the
	// page's changes name, posted deploys, GitHub deploys and failures alike, each with the UTC
	// midnights of the time it counts on before and after the change (a deploy's finished_at, a
	// failure's deploy's, a restore's time): a posted deploy's days (applyPosted), and the failure
	// measures' days (applyFailures).
	prodEvents map[string]map[string][]time.Time
	legion     map[string]map[time.Time]bool
	reparented map[string]bool
	unowned    map[string]bool
	// restart names the families whose walks begin again: walkDelivery and walkLegion (every
	// project's) and walkLogged (every project's). rebuild is the reason of the rebuild of every
	// served family the page's commit begins (replaceRebuild): a database whose cursors are gone, a
	// lost change cursor, or rules the rows stand built under that are not recorded; the empty
	// string for none. Where it is set, a walk of a served family is the rebuild's and does not
	// begin again (commitPage).
	restart map[string]bool
	rebuild string
	// settings is set where one of the page's changes is of the delivery settings: the page reads
	// the settings again and brings the rows to them (syncSettings).
	settings bool
	// settingsOf is the projects whose delivery settings the page's changes name (a settings
	// change's repo), whose failure rows the page writes whole (applyFailures).
	settingsOf map[string]bool
	// failureFirstDayOf is the projects for which a settings change moved the effective first
	// measured day, which the Evals page reads even if the project has no failure day rows.
	failureFirstDayOf map[string]bool
	// failureSelection names projects whose source-key readers or published defaults the page
	// moves. Their failure rows are written against the new selection after it commits.
	failureSelection []string
	// record is set where commitPage records refresh.builtUnder in eval_days_rules with the cursors
	// and the rebuild the page begins (syncSettings, syncKept). settingsApplied is set where that
	// record moves the repositories a read of the evals chooses among (DeployReposIn: a source
	// repository it gains or loses, or the project whose source reads one, which reads its source
	// key), or none was recorded and the page read a change of the
	// settings row: the commit then says that the day rows stand at the settings
	// (markSettingsApplied), which every evals view is recomputed for, and otherwise says nothing,
	// since no evals view reads what it moves.
	record, settingsApplied bool
	// kept is the repositories kept now (keptRepos), nil where they are the ones recorded, and
	// keptWas those the rows stood built for until then (syncKept): the page builds the days of each
	// repository that differs between the two in its commit (refresh.writeNamed).
	kept, keptWas []keptRepo
	// defaults is the projects' default repositories commitPage publishes, nil where every project's
	// stands published (syncDefaults, defaults.go).
	defaults *defaultsMove
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
		prs: map[string]pullRequestRef{}, runs: map[string]runRef{}, left: map[string][]time.Time{}, ran: map[string]time.Time{},
		prodEvents: map[string]map[string][]time.Time{}, legion: map[string]map[time.Time]bool{}, reparented: map[string]bool{},
		unowned: map[string]bool{}, restart: map[string]bool{}, settingsOf: map[string]bool{},
		failureFirstDayOf: map[string]bool{},
	}
}

// restartAll begins every walk again and a rebuild of every served family for reason: a database
// no pass has committed on, or a change cursor whose change left the log before it passed its
// retention, which lost changes no one can name. A served family's walk is the rebuild's, and
// begins no walk (commitPage).
func (w *pageWork) restartAll(reason string) {
	w.restart[walkDelivery], w.restart[walkLegion], w.restart[walkLogged], w.rebuild = true, true, true, reason
}

// addChange records what change touches (refresher_work.go's list above). A ref that names no row
// (a pull request number or a run id that is no number, or a pull request number past the 32 bits
// delivery_pull_requests.number holds) names nothing.
func (w *pageWork) addChange(change delivery.Change) {
	switch change.Kind {
	case delivery.ChangeKindPullRequest:
		if parsed, err := strconv.ParseInt(change.Ref, 10, 32); err == nil {
			number := int(parsed)
			w.prs[PullRequestMember(change.Repo, number)] = pullRequestRef{repo: change.Repo, number: number}
			if change.TOld != nil && (change.TNew == nil || !change.TNew.Equal(*change.TOld)) {
				w.left[change.Repo] = append(w.left[change.Repo], *change.TOld)
			}
		}
	case delivery.ChangeKindRun, delivery.ChangeKindRunJob:
		if id, err := strconv.ParseInt(change.Ref, 10, 64); err == nil {
			w.runs[RunMember(change.Repo, id)] = runRef{repo: change.Repo, id: id}
			if change.RunKind == string(delivery.DeliveryRunKindDeploy) {
				for _, at := range []*time.Time{change.TOld, change.TNew} {
					if earliest, known := w.ran[change.Repo]; at != nil && (!known || at.Before(earliest)) {
						w.ran[change.Repo] = *at
					}
				}
			}
		}
	case delivery.ChangeKindSettings:
		// The page reads the settings again and rewrites what they moved (syncSettings), and the
		// project's failure rows whole (applyFailures).
		w.settings = true
		w.settingsOf[change.Repo] = true
		if change.Ref == failureFirstDaySettingsRef {
			w.failureFirstDayOf[change.Repo] = true
		}
	case delivery.ChangeKindProductionEvents:
		// A row of a project's record (repo the project, ref the row's id), with the time it
		// counts on before and after: a posted deploy's days are rewritten in its project
		// (applyPosted), and those days of every project whose failure measures are on
		// (applyFailures).
		if w.prodEvents[change.Repo] == nil {
			w.prodEvents[change.Repo] = map[string][]time.Time{}
		}
		days := w.prodEvents[change.Repo][change.Ref]
		for _, at := range []*time.Time{change.TOld, change.TNew} {
			if at != nil {
				days = append(days, measures.Midnight(*at))
			}
		}
		w.prodEvents[change.Repo][change.Ref] = days
	case delivery.ChangeKindIssueFacts, delivery.ChangeKindIssueFilings:
		// Each names its Legion days itself, in its project: a facts change the issue's close day
		// before and after it, a filing change the filing's day before and after it, nil where
		// there is none.
		for _, at := range []*time.Time{change.TOld, change.TNew} {
			if at != nil {
				if w.legion[change.Repo] == nil {
					w.legion[change.Repo] = map[time.Time]bool{}
				}
				w.legion[change.Repo][measures.Midnight(*at)] = true
			}
		}
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
// it, brings the delivery and CI rows to the delivery settings it reads (syncSettings), writes the
// days the page names, and moves the cursors in a transaction of its own, which also records the
// walks the page begins again and the rebuild it begins, and, where the page's days move what days
// it does not name hold, writes them and those days (apply, commitPage). When the page publishes
// defaults or source rules, the failure rows follow that commit, under the published selection.
// It then writes each day a write left owed (writeOwed). It answers whether a feed holds more rows.
//
// A database holding no cursor row has committed no pass: the cursors start at the feeds' heads,
// and a rebuild of every family begins (rebuild.go), which builds every day the feeds' history
// could name. A change cursor that may have passed over changes it never read (delivery.CursorLost,
// judged on the database's clock for the change and on the page's own reading of the refresher's
// clock against when a page last read the change log to its end) moves to the log's head, and a
// rebuild of every family begins, which covers every change the log ever held, while the stored
// rows keep serving.
//
// The page reads the clock once, before its reads, and that one reading is what the lost-cursor
// check is judged at and what a read of the change log to its end stamps (delivery.ChangeCursor
// and its Through): a page shorter than limit, a move to the head, and the first read. A full page
// moves the change cursor and carries its ReadAt forward, so a pass that catches up a backlog of
// changes, judged page by page, finds the cursor lost once the retention has passed since the last
// read of the whole log, a prune having taken changes after it meanwhile. The rows are built as of
// the pass's moment (now), whatever the page's reading.
func (r *refresh) page(ctx context.Context, safe int64, limit int) (bool, error) {
	r.beginPage()
	parts := &r.report.parts
	parts.pages++
	checked := r.clock()
	cursors, committed, err := r.store.readRefresherCursors(ctx)
	if err != nil {
		return false, err
	}
	work := newPageWork()
	var moved feedCursors
	full := false
	if !committed {
		head, err := delivery.ChangesHead(ctx, r.store.database.Pool)
		if err != nil {
			return false, fmt.Errorf("read the head of the delivery changes: %w", err)
		}
		head.ReadAt = checked
		moved = feedCursors{changes: head, events: safe}
		work.restartAll(rebuildFirst)
		parts.mark(partChanges)
	} else {
		after := cursors.changes
		lost, err := delivery.CursorLost(ctx, r.store.database.Pool, after, checked)
		if err != nil {
			return false, fmt.Errorf("read the delivery change at %d: %w", after.Seq, err)
		}
		if lost {
			if moved.changes, err = delivery.ChangesHead(ctx, r.store.database.Pool); err != nil {
				return false, fmt.Errorf("read the head of the delivery changes: %w", err)
			}
			moved.changes.ReadAt = checked
			work.restartAll(rebuildCursor)
		} else {
			changes, err := delivery.ChangesSince(ctx, r.store.database.Pool, after.Seq, limit)
			if err != nil {
				return false, fmt.Errorf("read the delivery changes after %d: %w", after.Seq, err)
			}
			moved.changes = after.Through(changes, limit, checked)
			for _, change := range changes {
				work.addChange(change)
			}
			full = len(changes) == limit
			parts.changes += len(changes)
		}
		parts.mark(partChanges)
		events, err := r.store.readRefresherEvents(ctx, cursors.events, safe, limit)
		if err != nil {
			return false, err
		}
		moved.events = events.position
		full = full || events.full
		work.addEvents(events)
		parts.mark(partEvents)
	}
	if work.settings {
		// The settings moved: the first days this pass read before were read under the old ones,
		// so each is read again.
		if err := r.loadSettings(ctx); err != nil {
			return false, err
		}
		for _, owner := range r.owners {
			delete(r.first, walkKey{family: walkDelivery, project: owner})
		}
	}
	if err := r.syncSettings(ctx, work); err != nil {
		return false, err
	}
	parts.mark(partSettings)
	if err := r.syncKept(ctx, work); err != nil {
		return false, err
	}
	parts.mark(partKept)
	if err := r.syncDefaults(ctx, work); err != nil {
		return false, err
	}
	parts.mark(partDefaults)
	rebuilds, err := r.apply(ctx, work)
	if err != nil {
		return false, err
	}
	// Ordinarily the failure writes precede the cursors, so a failed write leaves its input
	// changes to be read again. A page publishing a selection must use it after the commit.
	afterPublish := work.defaults != nil || work.record
	if !afterPublish {
		if err := r.applyFailures(ctx, work); err != nil {
			return false, err
		}
	}
	parts.mark(partWork)
	if err := r.commitPage(ctx, moved, work.restarts(r.projects), work, rebuilds); err != nil {
		return false, err
	}
	parts.mark(partCommit)
	if afterPublish {
		if err := r.applyFailures(ctx, work); err != nil {
			// The cursors have moved. Leave the failure walk owing a whole write on retry.
			clear(r.failuresWritten)
			return false, err
		}
		r.store.drainChanges(ctx)
	}
	if err := r.writeOwed(ctx); err != nil {
		return false, err
	}
	parts.mark(partOwed)
	return full, nil
}

// beginPage starts a page: its delivery reads, the unowned-p0 rows it wrote, and its reads of the
// repositories whose rows are kept per deploy repository and of the serving records begin empty
// (refresh's rule on why they hold for one page).
func (r *refresh) beginPage() {
	r.reads, r.done, r.unowned = map[time.Time]deliveryRead{}, map[builtDay]bool{}, map[string]bool{}
	r.deployRepos = nil
	r.serving = nil
}

// walkOf is the walk that builds project's days of family: the one delivery walk of every
// project's delivery and CI rows, or project's Legion walk.
func walkOf(family, project string) walkKey {
	if family == walkDelivery {
		return walkKey{family: walkDelivery}
	}
	return walkKey{family: family, project: project}
}

// apply writes every day w names, each family's whole days by writeNamed's rule: where the days of
// a family move no day they do not name, in one transaction of their own, and otherwise in the
// page's commit, as the span writes it answers, which the caller commits with the page's cursors
// (commitPage). The days a write left owed before this page (owedDays) are named with the page's
// own: a pass that failed after a moving write and before the day's own left them so. The
// unowned-p0 rows are written before it answers. The caller writes the failure rows separately
// (applyFailures), since those of a publishing page must follow its commit.
func (r *refresh) apply(ctx context.Context, w *pageWork) ([]*spanWrite, error) {
	owed, err := r.store.owedDays(ctx, r.owners)
	if err != nil {
		return nil, err
	}
	spans, err := r.applyFamilies(ctx, w, owed)
	if err != nil {
		return nil, err
	}
	for _, project := range slices.Sorted(maps.Keys(w.unowned)) {
		known, err := r.knows(ctx, project)
		if err != nil {
			return nil, err
		}
		if known {
			if err := r.writeUnowned(ctx, project); err != nil {
				return nil, err
			}
		}
	}
	return spans, nil
}

// applyFamilies writes the delivery and CI days w and owed name (applyDelivery), then their Legion
// days (applyLegion), and answers the span writes of the families whose days move what days they
// do not name hold, the delivery family's first, for the caller to commit.
func (r *refresh) applyFamilies(ctx context.Context, w *pageWork, owed []owedDay) ([]*spanWrite, error) {
	var spans []*spanWrite
	whole, err := r.applyDelivery(ctx, w, owed)
	if err != nil {
		return nil, err
	}
	if whole != nil {
		spans = append(spans, whole)
	}
	legion, err := r.applyLegion(ctx, w, owed)
	if err != nil {
		return nil, err
	}
	return append(spans, legion...), nil
}

// owedRounds bounds how many times one page writes the owed days again: a write of an owed day may
// leave another owed, where the store moved since the page read its changes, which a later page's
// read of the same move names too.
const owedRounds = 3

// writeOwed writes every day of the pass's projects and owner keys (owners.go) a write left owing a
// rewrite (owedDays) once the page has committed: a day an item moved off, or the day of the
// deploy run a pull request
// moved off, which this page's own writes left so. A page names the days its items leave before it
// writes (nameLeft), so there is none unless the store moved since. Each is written whole, as a
// change naming it is, though the page wrote it already: the mark may be younger than that write.
// Where such a day moves what other days hold, they are written with it in a commit of their own
// (commitSpans).
func (r *refresh) writeOwed(ctx context.Context) error {
	for range owedRounds {
		owed, err := r.store.owedDays(ctx, r.owners)
		if err != nil || len(owed) == 0 {
			return err
		}
		spans, err := r.applyFamilies(ctx, newPageWork(), owed)
		if err != nil {
			return err
		}
		if err := r.commitSpans(ctx, spans); err != nil {
			return err
		}
	}
	return nil
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
// written, so the runs that shipped the linked pull requests are written last, and with them the
// deploys that ship now, or shipped, the pull requests no item records the deploy of
// (shippedNeighbours). No link beyond those is followed: the rows of the items behind them did
// not move. With them it writes the days of each posted deploy w names (applyPosted), each
// delivery day of owed, and, where the page read another set of kept repositories (w.kept), the
// days of each that differs. It names the days first, each read once and every day of each round
// of naming in one read (readDeliveries), and then writes them as writeNamed says, which answers
// the span write the page's commit holds, nil where the days were written in a transaction of
// their own.
func (r *refresh) applyDelivery(ctx context.Context, w *pageWork, owed []owedDay) (*spanWrite, error) {
	if !r.delivers() && w.kept == nil {
		return nil, nil
	}
	served, err := r.servingNext(ctx, walkDelivery, "")
	if err != nil {
		return nil, err
	}
	days := newPageDays(deliveryFamily)
	// On a day the delivery walk has not made whole yet, no other owner key holds rows of it in
	// eval_days: naming every owner key the day's read builds rows of ensures the page writes all
	// of the day's keys in one commit, never half of a day while a later page holds its other runs.
	// A day a window of whose read the bound refused (deliveryRead.refused) is named for every owner,
	// so its write deletes the rows of the evals the refusal drops wherever the day held them, and
	// the day reads "no data" for them in every project and owner key.
	whole := map[time.Time]bool{}
	days.name = func(ctx context.Context, project string, day time.Time) error {
		read, err := r.readDelivery(ctx, day, nil, nil)
		if err != nil {
			return err
		}
		r.nameDelivery(days, read, project)
		refused := len(read.refused) > 0
		if (refused || served == nil || !read.Day.Before(*served)) && !whole[read.Day] {
			whole[read.Day] = true
			for _, owner := range r.owners {
				if refused || len(read.Rows(owner).Rows) > 0 {
					r.nameDelivery(days, read, owner)
				}
			}
		}
		return nil
	}
	days.read = func(ctx context.Context, keys []projectDay) error {
		named := make([]time.Time, len(keys))
		for i, key := range keys {
			named[i] = key.day
		}
		return r.readDeliveries(ctx, named)
	}
	if len(w.prs)+len(w.runs) > 0 {
		items, err := r.store.deliveryItemDays(ctx, w.prs, w.runs)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, item := range items {
			seen[item.member] = true
		}
		linked, held, err := r.nameDeliveryItems(ctx, days, items, true)
		if err != nil {
			return nil, err
		}
		neighbours, err := r.shippedNeighbours(ctx, w, held, linked)
		if err != nil {
			return nil, err
		}
		next, err := r.followUp(ctx, linked, held, seen)
		if err != nil {
			return nil, err
		}
		if _, held, err = r.nameDeliveryItems(ctx, days, next, false); err != nil {
			return nil, err
		}
		last, err := r.followUp(ctx, nil, held, seen)
		if err != nil {
			return nil, err
		}
		if _, _, err = r.nameDeliveryItems(ctx, days, slices.Concat(last, neighbours), false); err != nil {
			return nil, err
		}
	}
	if err := r.applyPosted(ctx, days, w.prodEvents); err != nil {
		return nil, err
	}
	var owing []projectDay
	for _, day := range owed {
		if !slices.Contains(deliveryDayFamilies, day.family) {
			continue
		}
		writes, err := r.writesDelivery(ctx, day.project)
		if err != nil {
			return nil, err
		}
		if writes {
			owing = append(owing, projectDay{project: day.project, day: day.day})
		}
	}
	if err := days.read(ctx, owing); err != nil {
		return nil, err
	}
	for _, day := range owing {
		if err := days.name(ctx, day.project, day.day); err != nil {
			return nil, err
		}
	}
	var kept *keptMove
	if w.kept != nil {
		kept = &keptMove{was: w.keptWas, is: w.kept}
	}
	if len(days.order) == 0 && kept == nil {
		return nil, nil
	}
	return r.writeNamed(ctx, days, kept, w.defaults != nil)
}

// applyPosted names, in days, the delivery days of each posted deploy changed names (by project,
// each row id with the days its change counts on before and after) in its project, where the
// project's events repository is read (refresh.rules): the day it finished before and after the
// change, the days the pull requests it names merged on (the merge-to-production rows it times),
// and the days eval_day_members holds it, and the pull requests it shipped, under when they were
// last counted. A row the record still holds that is no posted deploy (a GitHub deploy, whose run
// a source repository's rows count, or a failure) names no day: it moves no row of these evals.
// Each project's rows are read in one statement (prodevents.ChangedRows), and the days it names in
// one read of several days (readDeliveries).
func (r *refresh) applyPosted(ctx context.Context, days *pageDays, changed map[string]map[string][]time.Time) error {
	for _, project := range slices.Sorted(maps.Keys(changed)) {
		repo := EventsRepo(project)
		if _, read := r.rules[delivery.RuleKey(repo)]; !read {
			continue
		}
		known, err := r.knows(ctx, project)
		if err != nil {
			return err
		}
		if !known {
			continue
		}
		ids := slices.Sorted(maps.Keys(changed[project]))
		stored, err := prodevents.ChangedRows(ctx, r.store.database.Pool, project, ids)
		if err != nil {
			return err
		}
		named := map[time.Time]bool{}
		var members []string
		prs := map[string]pullRequestRef{}
		for _, id := range ids {
			row, found := stored[id]
			if found && !row.Posted {
				continue
			}
			for _, day := range changed[project][id] {
				named[day] = true
			}
			members = append(members, PostedMember(id))
			for _, ref := range row.PullRequests {
				if name, number, ok := parsePullRequestRef(ref); ok {
					prs[PullRequestMember(name, number)] = pullRequestRef{repo: name, number: number}
				}
			}
		}
		if len(members) == 0 {
			continue
		}
		held, err := r.store.heldMembers(ctx, []string{project}, []string{KindDelivery}, []string{repo}, members)
		if err != nil {
			return err
		}
		shipped, err := r.store.shippedMembers(ctx, []string{project}, members)
		if err != nil {
			return err
		}
		for _, member := range slices.Concat(held, shipped) {
			named[member.day] = true
		}
		if len(prs) > 0 {
			merged, err := r.store.deliveryItemDays(ctx, prs, nil)
			if err != nil {
				return err
			}
			for _, item := range merged {
				if item.day != nil {
					named[*item.day] = true
				}
			}
		}
		ordered := slices.SortedFunc(maps.Keys(named), time.Time.Compare)
		if err := r.readDeliveries(ctx, ordered); err != nil {
			return err
		}
		for _, day := range ordered {
			if err := days.name(ctx, project, day); err != nil {
				return err
			}
		}
	}
	return nil
}

// shippedNeighbours is the deploy runs, each with its start day, whose shipped pull requests the
// page's changes can have moved without any item recording it. A deploy ships every pull request
// whose first shipping apply it is, whenever the pull request merged (delivery.FirstShippingApply),
// and a pull request merged before its repository's history began (keptRepo.since) is an item of
// no day of that repository (DeliveryDay.Rows), so eval_day_members holds no deploy for it: the
// first deploy to complete ships each of them, and another does once that deploy's run goes,
// once another completes before it, or once the pull request's merge moves.
//
// Such a pull request moves between two deploys, through a change of a run, only where one of
// the two is the changed run: the first shipping apply among the runs no change names is the same
// before the page's changes and after them. A changed run's own days are named already (its start
// day, and the day eval_day_members holds it under), so what is left to name is that unchanged
// deploy, where a changed run ships such a pull request now (linked holds it with a merge day
// before the repository's start) or can have shipped one before. It can have only where no deploy
// of the repository's history succeeded before the changed run started, by the earliest start
// the page's changes name of a deploy run of the repository (w.ran) and the earliest day
// eval_day_members holds one of them under (held): a run completes no earlier than it starts, so
// a deploy that succeeded before then ships those pull requests before the changed run could,
// then and now (delivery.ReadLatestShipBefore, one read of an index for each repository a deploy
// run of which the page's changes name). So the page of a run that is not the first of its
// repository to complete, which is nearly every page, reads nothing more. Where either holds, the
// answer is each run other than the changed ones that ships first a pull request merged before
// the repository's start: the one that ships what merged before every run was created, and,
// where GitHub created a run before the UTC day its history starts on, the one that ships what
// merged from that creation on.
//
// A pull request whose own merge moved from before the repository's start (w.left) left the
// deploy that shipped it there: the first shipping apply, among the runs no change names, of that
// merge. The deploy that ships it now is linked to its merge day, as any pull request's is.
//
// The start is the one the stored rows stand built for: where the page found the set of
// repositories changed (w.kept), the one recorded until then (w.keptWas), and a repository that
// set did not hold has no row to move. Either answer reads the repository's deploy runs whole.
func (r *refresh) shippedNeighbours(ctx context.Context, w *pageWork, held []heldMember, linked []deliveryItem) ([]deliveryItem, error) {
	if len(w.runs) == 0 && len(w.left) == 0 {
		return nil, nil
	}
	changed := map[string]map[int64]bool{}
	for _, run := range w.runs {
		if changed[run.repo] == nil {
			changed[run.repo] = map[int64]bool{}
		}
		changed[run.repo][run.id] = true
	}
	kept, err := r.keptRepos(ctx)
	if err != nil {
		return nil, err
	}
	pool := r.store.database.Pool
	var neighbours []deliveryItem
	for _, repo := range kept {
		if !repo.deploys {
			continue
		}
		production := repo.rule.Production()
		since := repo.since
		if w.kept != nil {
			at := slices.IndexFunc(w.keptWas, func(was keptRepo) bool { return was.name == repo.name })
			if at < 0 {
				continue
			}
			since = w.keptWas[at].since
		}
		var moments []time.Time
		for _, merged := range w.left[repo.name] {
			if merged.Before(since) {
				moments = append(moments, merged)
			}
		}
		var ran *time.Time
		if at, known := w.ran[repo.name]; known {
			ran = &at
		}
		first, err := r.firstToComplete(ctx, repo.name, production, since, changed[repo.name], ran, held, linked)
		if err != nil {
			return nil, err
		}
		if len(moments) == 0 && !first {
			continue
		}
		rows, err := delivery.ReadApplyRows(ctx, pool, repo.rule, time.Time{})
		if err != nil {
			return nil, fmt.Errorf("read the deploy runs of %s: %w", repo.name, err)
		}
		applies := slices.DeleteFunc(delivery.ProductionApplies(rows.Runs, rows.Jobs, production).Applies, func(apply delivery.Apply) bool {
			return changed[repo.name][apply.RunID]
		})
		if first {
			// What merged before every run was created, then what merged from the creation of each
			// run created before the repository's start on.
			moments = append(moments, time.Time{})
			for _, apply := range applies {
				if apply.CreatedAt.Before(since) {
					moments = append(moments, apply.CreatedAt)
				}
			}
		}
		ships := map[int64]bool{}
		for _, merged := range moments {
			if apply := delivery.FirstShippingApply(merged, applies); apply != nil {
				ships[apply.RunID] = true
			}
		}
		for _, run := range rows.Runs {
			if ships[run.RunID] {
				day := measures.Midnight(run.StartedAt)
				neighbours = append(neighbours, deliveryItem{member: RunMember(repo.name, run.RunID), day: &day})
			}
		}
	}
	return neighbours, nil
}

// firstToComplete reports whether one of the changed runs of repo ships now, or can have shipped
// before the page's changes, a pull request merged before since, the start of repo's history
// (shippedNeighbours' rule): linked holds such a pull request, or no deploy of repo succeeded under
// production, its rule, before the earlier of ran, the earliest start the page's changes name of a
// deploy run of repo (nil where they name none), and the earliest day held holds a run of repo
// under.
func (r *refresh) firstToComplete(ctx context.Context, repo string, production delivery.Production, since time.Time, changed map[int64]bool,
	ran *time.Time, held []heldMember, linked []deliveryItem) (bool, error) {
	if len(changed) == 0 {
		return false, nil
	}
	if slices.ContainsFunc(linked, func(item deliveryItem) bool {
		name, ok := pullRequestMemberRepo(item.member)
		return ok && name == repo && item.day != nil && item.day.Before(since)
	}) {
		return true, nil
	}
	earliest := ran
	for _, member := range held {
		if run, ok := parseRunMember(member.member); ok && run.repo == repo && (earliest == nil || member.day.Before(*earliest)) {
			earliest = &member.day
		}
	}
	if earliest == nil {
		return false, nil
	}
	latest, err := delivery.ReadLatestShipBefore(ctx, r.store.database.Pool, repo, production, *earliest)
	if err != nil {
		return false, fmt.Errorf("read the last deploy of %s to succeed before %s: %w", repo, earliest.Format(time.RFC3339), err)
	}
	return latest.Before(since), nil
}

// followUp is the items of linked and the deploy runs that shipped the items of held when they
// were last counted (by row key) that seen does not hold yet, each run with the day its row stands
// at now, and adds them to seen.
func (r *refresh) followUp(ctx context.Context, linked []deliveryItem, held []heldMember, seen map[string]bool) ([]deliveryItem, error) {
	var next []deliveryItem
	for _, item := range linked {
		if !seen[item.member] {
			seen[item.member] = true
			next = append(next, item)
		}
	}
	runs := map[string]runRef{}
	for _, member := range held {
		if run, ok := parseRunMember(member.shippedBy); ok && !seen[member.shippedBy] {
			seen[member.shippedBy] = true
			runs[member.shippedBy] = run
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

// pullRequestMemberRepo is the repository of the pull request a pull request's row key names
// (PullRequestMember), false for a key that names none.
func pullRequestMemberRepo(member string) (string, bool) {
	rest, ok := strings.CutPrefix(member, "pull_request:")
	hash := strings.LastIndex(rest, "#")
	if !ok || hash < 0 {
		return "", false
	}
	return rest[:hash], true
}

// nameDeliveryItems names, in days, for each item, its day in every project that day counts it
// for, and the day eval_day_members holds it under in that project, and answers where
// eval_day_members holds the items, each with the deploy run that shipped it when it was last
// counted. Where follow is set it also names the merge day of every pull request a run among
// items shipped when that pull request was last counted, which the run's day no longer names once
// it stops shipping it, and answers the items of other days each item's day, and each of those
// pull requests' days, links it to now.
func (r *refresh) nameDeliveryItems(ctx context.Context, days *pageDays, items []deliveryItem, follow bool) ([]deliveryItem, []heldMember, error) {
	if len(items) == 0 {
		return nil, nil, nil
	}
	members := make([]string, len(items))
	for i, item := range items {
		members[i] = item.member
	}
	kept, err := r.keptRepos(ctx)
	if err != nil {
		return nil, nil, err
	}
	held, err := r.store.heldMembers(ctx, r.owners, deliveryDayFamilies, append([]string{""}, deployRepoNames(kept)...), members)
	if err != nil {
		return nil, nil, err
	}
	var shipped []heldMember
	if follow {
		if shipped, err = r.store.shippedMembers(ctx, r.owners, members); err != nil {
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
	ordered := slices.SortedFunc(maps.Keys(jobs), time.Time.Compare)
	if err := r.readDeliveries(ctx, ordered); err != nil {
		return nil, nil, err
	}
	for _, day := range ordered {
		read, err := r.readDelivery(ctx, day, nil, nil)
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
			writes, err := r.writesDelivery(ctx, project)
			if err != nil {
				return nil, nil, err
			}
			if writes {
				if err := days.name(ctx, project, day); err != nil {
					return nil, nil, err
				}
			}
		}
	}
	return linked, held, nil
}

// keptRepos is the repositories whose rows of the evals that read the deploy repository the pass
// keeps, read once a page (refresh.deployRepos), in the byte order of their names, each with its
// rule: of the repositories that hold a delivery run (readDeployRepos), each that a deploy source
// of a delivery-on project reads (refresh.rules), under that source's rule, and each whose rule
// stands recorded with the rows (refresh.builtUnder) where no source reads it any more, under that
// rule and no project, as the base kept every repository that holds runs; and the events
// repository of each project whose posted deploys are read that holds one (readPostedRepos). The
// intake keeps the runs of a source repository alone, and a repository's runs stay when no source
// reads it any more, so its rows and its rule stay with them: a source moved away from it and back,
// or a later source on it under the same rule, rebuilds none of its days (syncKept), and which
// repositories a project reads is the selection recorded with the rules (DeployReposIn).
func (r *refresh) keptRepos(ctx context.Context) ([]keptRepo, error) {
	if r.deployRepos != nil {
		return r.deployRepos, nil
	}
	if err := r.readBuiltOnce(ctx); err != nil {
		return nil, err
	}
	held, err := readDeployRepos(ctx, r.store.database.Pool)
	if err != nil {
		return nil, err
	}
	repos := []keptRepo{}
	var events []delivery.DeliverySettings
	for _, repo := range held {
		if IsEventsRepo(repo.name) {
			continue
		}
		if rule, read := r.ruleOf(repo.name); read {
			repo.rule = rule
			repos = append(repos, repo)
			continue
		}
		if recorded, found := r.builtUnder[delivery.RuleKey(repo.name)]; found {
			repo.rule = recorded.rule
			repo.rule.DeployRepo, repo.rule.Project = repo.name, ""
			repos = append(repos, repo)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(r.rules)) {
		if IsEventsRepo(r.rules[key].DeployRepo) {
			events = append(events, r.rules[key])
		}
	}
	posted, err := readPostedRepos(ctx, r.store.database.Pool, events)
	if err != nil {
		return nil, err
	}
	repos = append(repos, posted...)
	slices.SortFunc(repos, func(a, b keptRepo) int { return cmp.Compare(a.name, b.name) })
	r.deployRepos = repos
	return r.deployRepos, nil
}

// readDelivery is the delivery rows of day (readDeliveryDays) with each kept repository as the
// deploy repository (keptRepos), read once a page (refresh.reads), each repository's deploy runs
// taken from applies, and each events repository's posted deploys from posted, where they hold
// them.
func (r *refresh) readDelivery(ctx context.Context, day time.Time, applies map[string]*delivery.ApplyRows,
	posted map[string]postedRecord) (deliveryRead, error) {
	day = measures.Midnight(day)
	if read, ok := r.reads[day]; ok {
		return read, nil
	}
	if err := r.readDays(ctx, []time.Time{day}, applies, posted); err != nil {
		return deliveryRead{}, err
	}
	return r.reads[day], nil
}

// readDeliveries reads the delivery rows of each of days the page has not read yet in one read of
// several days (readDeliveryDays), each repository's deploy runs read of the days' windows alone:
// the days one round of a page's naming names, which readDelivery then answers. A page makes a
// handful of rounds whatever the number of days it names, so it reads the deploy runs a handful of
// times, where a read of each day read every deploy run created from the day on.
func (r *refresh) readDeliveries(ctx context.Context, days []time.Time) error {
	return r.readDays(ctx, days, nil, nil)
}

// readDays reads, in one read (readDeliveryDays), each of days the page has not read yet, each
// kept repository's deploy runs taken from applies and each events repository's posted deploys
// from posted, where they hold them, and keeps each read for the page (refresh.reads) and its
// owner keys (noteOwners). A day a window of whose read the bound of a window's read refused costs
// that day alone: it is kept like any other, with no row of the evals the window decides
// (deliveryRead.refused, DeliveryDay.refuse), whose write deletes those rows (applyDelivery names
// it for every owner), and each refused window is logged at WARN, naming the day, the window, the
// evals it drops and the reason. The cursors of the page that wrote the day pass the change that
// named it, so it stands so until a change names it again, or a walk or a rebuild reaches it,
// whose read refuses it again while the window holds more than the bound, and builds it whole once
// it holds no more; a settings rewrite or a span, which read the whole history with no such bound
// (delivery.HistoryRows), build it from every row.
func (r *refresh) readDays(ctx context.Context, days []time.Time, applies map[string]*delivery.ApplyRows,
	posted map[string]postedRecord) error {
	unread := make([]time.Time, 0, len(days))
	for _, day := range days {
		day = measures.Midnight(day)
		if _, ok := r.reads[day]; !ok {
			unread = append(unread, day)
		}
	}
	if len(unread) == 0 {
		return nil
	}
	slices.SortFunc(unread, time.Time.Compare)
	unread = slices.Compact(unread)
	kept, err := r.keptRepos(ctx)
	if err != nil {
		return err
	}
	reads, err := r.store.readDeliveryDays(ctx, kept, unread, applies, posted)
	if err != nil {
		return err
	}
	for i, read := range reads {
		for _, window := range read.refused {
			slog.Warn("dispatch: the eval day-row refresher drops a delivery day's rows of the evals a window past the bound decides",
				"day", read.Day.Format(time.DateOnly), "window", window.Window(), "repo", window.Repo, "bound", window.Bound,
				"evals", strings.Join(refusedEvals(window), ","), "reason", window.String())
		}
		r.noteOwners(read.owners())
		r.reads[unread[i]] = read
	}
	return nil
}

// noteOwners adds to the delivery family's owners (refresh.owners) each owner key of keys it does
// not hold yet, in key order: a key whose first rows a read builds.
func (r *refresh) noteOwners(keys []string) {
	for _, key := range keys {
		if at, held := slices.BinarySearch(r.owners, key); !held {
			r.owners = slices.Insert(r.owners, at, key)
		}
	}
}

// writesDelivery reports whether the pass writes key's delivery and CI days: a project (knows), or
// an owner key, whose rows belong to its repository.
func (r *refresh) writesDelivery(ctx context.Context, key string) (bool, error) {
	if isOwner(key) {
		return true, nil
	}
	return r.knows(ctx, key)
}

// knows reports whether key is a project the pass writes rows of: one it read (load) or one made
// since. A page reads the input that counts for a project once that project is committed, an
// issue of it that a pull request now names or a close of one of its issues, and the page's
// cursors pass that input when the page commits, so a project the pass did not know would lose
// the days the input moves, which no later change names. A key not among the projects the pass
// read is looked for in the table, once a page (refresh.absent), and one found joins the pass's
// projects and owners.
func (r *refresh) knows(ctx context.Context, key string) (bool, error) {
	if r.known[key] {
		return true, nil
	}
	if r.absent[key] {
		return false, nil
	}
	var found bool
	if err := r.store.database.Pool.QueryRow(ctx, `select exists (select 1 from projects where key = $1)`, key).Scan(&found); err != nil {
		return false, fmt.Errorf("read the project %s: %w", key, err)
	}
	if !found {
		if r.absent == nil {
			r.absent = map[string]bool{}
		}
		r.absent[key] = true
		return false, nil
	}
	r.known[key] = true
	r.projects = append(r.projects, key)
	slices.Sort(r.projects)
	r.noteOwners([]string{key})
	return true, nil
}

// ownerDay is one owner's part of a delivery walk's day (buildDelivery): what the day writes of it,
// and the rows the day's input built of it, by which its first input days move once the day is
// written (noteBuilt).
type ownerDay struct {
	day   ProjectDay
	built []DayRow
}

// buildDelivery is the delivery walk's build of project's delivery and CI rows of read's day,
// those of the evals that read the deploy repository once for each repository read held: the rows
// the day builds, less each lead time that counts no pull request before its series' first input
// day (dropUnshipped), and the zero row of every series the day has no input of whose first input
// day is on or before it (zerosFrom), by the walk's own first input days (firstOf), but those of
// the evals a refused window of the day's read drops (deliveryRead.zeros); nil where the batch
// wrote the day already (refresh.done). A project the day counts nothing for and holds no series
// of before that day writes nothing unless the walk finds the day holding rows of it (walkHeld):
// its write deletes what the day held, a refused day's rows of the evals the refusal drops among
// them. The walk writes its days in order, from the first input day or from the days it built, so
// it finds each series' first input day as it goes (noteBuilt, once the day is written). A page
// writes its days by writeNamed.
func (r *refresh) buildDelivery(ctx context.Context, read deliveryRead, project string) (*ownerDay, error) {
	key := builtDay{family: walkDelivery, project: project, day: read.Day}
	if r.done[key] {
		return nil, nil
	}
	first, err := r.firstOf(ctx, walkDelivery, project)
	if err != nil {
		return nil, err
	}
	built := read.Rows(project)
	built.Rows = dropUnshipped(built.Rows, first, read.Day)
	var zeros []DayRow
	if begunBy(first, read.Day) {
		zeros = zerosFrom(read.zeros(project), first, read.Day)
	}
	day := built.WithZeros(zeros)
	if len(day.Rows) == 0 && !r.walkHeld[key] {
		return nil, nil
	}
	return &ownerDay{day: day, built: built.Rows}, nil
}

// walkWrite is a walk's write of one day of family, the days of it the walk built (one for each
// project, or owner key, it writes): in the open, all of them in one transaction (writeDays) that
// moves the walk's record, and the family's serving record, past the day (recordWalkIn), so no
// commit holds a day's rows of some of the keys a project reads and not of the others, and the
// day reads as built from the commit that writes it, their stored figures folded at the end of the
// walks' batches (noteWalked, foldWalked); or, while a rebuild's walk builds a batch (r.staging),
// held for the batch to stage (stageWalk). A day that writes nothing commits nothing: the next
// day's commit, or the record at the end of the batch (walkDays), moves the records past it.
func (r *refresh) walkWrite(ctx context.Context, family dayFamily, key walkKey, day time.Time, days ...ProjectDay) error {
	if r.staging != nil {
		r.staging.days = append(r.staging.days, days...)
		return nil
	}
	if len(days) == 0 {
		return nil
	}
	if err := r.store.writeDays(ctx, family, days, r.build, false, func(tx pgx.Tx) error {
		return r.recordWalkIn(ctx, tx, key, day.AddDate(0, 0, 1))
	}); err != nil {
		return err
	}
	for _, one := range days {
		r.noteWalked(family.walk, one.Project, one.Day)
	}
	return nil
}

// applyLegion writes the Legion days w names: the days the facts and filing changes named, and the
// filing days and the close and held days of every issue below an issue whose repository or
// parent moved, whose repository rows resolve through it, and with them each Legion day of owed.
// Each project's days are written as writeNamed says, and it answers the span writes the page's
// commit holds, one for each project whose days move what days they do not name hold.
func (r *refresh) applyLegion(ctx context.Context, w *pageWork, owed []owedDay) ([]*spanWrite, error) {
	days := map[string]map[time.Time]bool{}
	add := func(project string, day time.Time) {
		if days[project] == nil {
			days[project] = map[time.Time]bool{}
		}
		days[project][day] = true
	}
	if len(w.reparented) > 0 {
		moved := slices.Sorted(maps.Keys(w.reparented))
		filed, err := r.store.subtreeFilingDays(ctx, moved)
		if err != nil {
			return nil, err
		}
		closed, err := r.store.subtreeCloseDays(ctx, moved)
		if err != nil {
			return nil, err
		}
		for _, day := range slices.Concat(filed, closed) {
			add(day.project, day.day)
		}
	}
	for project, named := range w.legion {
		for day := range named {
			add(project, day)
		}
	}
	for _, day := range owed {
		if slices.Contains(legionDayFamilies, day.family) {
			add(day.project, day.day)
		}
	}
	var spans []*spanWrite
	for _, project := range slices.Sorted(maps.Keys(days)) {
		known, err := r.knows(ctx, project)
		if err != nil {
			return nil, err
		}
		if !known {
			continue
		}
		named := newPageDays(legionFamily)
		named.name = func(ctx context.Context, project string, day time.Time) error {
			return r.nameLegion(ctx, named, project, day)
		}
		for _, day := range slices.SortedFunc(maps.Keys(days[project]), time.Time.Compare) {
			if err := named.name(ctx, project, day); err != nil {
				return nil, err
			}
		}
		one, err := r.writeNamed(ctx, named, nil, false)
		if err != nil {
			return nil, err
		}
		if one != nil {
			spans = append(spans, one)
		}
	}
	return spans, nil
}

// writeLegion is project's Legion walk's write of its Legion rows of day, as deliveryWalkDay is the
// delivery walk's: the rows the day builds, and the zero row of every series the day has no input
// of whose first input day is on or before it, by the walk's own first input days; once a batch
// (refresh.done). A day with no input and no series begun before it writes nothing unless the
// walk finds the day holding rows of it (walkHeld): its write deletes what the day held. The
// project's day is the one key a Legion eval reads, written in a transaction of its own that moves
// the project's Legion records past it (walkWrite); a rebuild's walk stages the day instead.
func (r *refresh) writeLegion(ctx context.Context, project string, day time.Time) error {
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
	if len(written.Rows) == 0 && !r.walkHeld[key] {
		return nil
	}
	if err := r.walkWrite(ctx, legionFamily, walkKey{family: walkLegion, project: project}, read.Day, written); err != nil {
		return err
	}
	r.noteBuilt(walkLegion, project, read.Day, built.Rows)
	r.done[key], r.report.days[key] = true, true
	return nil
}

// writeUnowned writes project's unowned-p0 row of today, once a page (refresh.unowned).
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

// fillLogged writes the zero rows of project's logged evals from each series' first day to today
// (fillLoggedZeros), recorded where it wrote any.
func (r *refresh) fillLogged(ctx context.Context, project string) error {
	filled, err := r.fillLoggedZeros(ctx, project)
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

// firstOf is the first input day of each eval of family that project holds a row of, by eval and
// deploy repository (evalKey), as a write of a day builds its rows by: the store's (storedFirst)
// outside a walk, and in it the walk's own, the first day of each eval's all-repositories rows of
// the days before the day the walk's batch began at (walkFrom), every one of which the walk built
// or a write outside it built from its input, read once a pass and moved by every day the walk
// writes (noteBuilt). So the walk begins a series at the first day it builds holding input of it,
// and a day before the walk's own first days holds no row of it, whatever rows a day the walk now
// builds held before: a whole walk over the same inputs holds the same. A rebuild's walk reads the
// first input days its batches found and recorded instead (rebuildFirsts): the days before its
// batch are the ones it staged, not the stored ones. A batch reads those of every project it
// writes at its begin (beginBatch, loadWalkFirsts); a project it finds after, here.
func (r *refresh) firstOf(ctx context.Context, family, project string) (map[evalKey]time.Time, error) {
	if !r.walking {
		return r.storedFirst(ctx, family, project)
	}
	key := walkKey{family: family, project: project}
	if first, ok := r.walkFirst[key]; ok {
		return first, nil
	}
	if err := r.loadWalkFirsts(ctx, family, []string{project}); err != nil {
		return nil, err
	}
	return r.walkFirst[key], nil
}

// loadWalkFirsts reads the walk's first input days of family (firstOf) of each of projects in one
// read, and keeps them for the walk's batch (walkFirst): a rebuild's walk's that its batches
// recorded (rebuildFirsts, of every project where projects are more than one), each project's
// marked as one whose first days the batch records (stageBatch.loaded); or the store's of the days
// before the batch's first (readFirsts). Each project's is what a read of it alone answers.
func (r *refresh) loadWalkFirsts(ctx context.Context, family string, projects []string) error {
	if len(projects) == 0 {
		return nil
	}
	var firsts map[string]map[evalKey]time.Time
	if r.staging != nil {
		one := ""
		if len(projects) == 1 {
			one = projects[0]
		}
		found, err := r.store.rebuildFirsts(ctx, r.staging.rebuild.id, family, one)
		if err != nil {
			return err
		}
		firsts = make(map[string]map[evalKey]time.Time, len(projects))
		for _, project := range projects {
			r.staging.loaded[project] = true
			if firsts[project] = found[project]; firsts[project] == nil {
				firsts[project] = map[evalKey]time.Time{}
			}
		}
	} else {
		var before *time.Time
		if from, ok := r.walkFrom[walkOf(family, projects[0])]; ok {
			before = &from
		}
		var err error
		if firsts, err = r.readFirsts(ctx, family, projects, before); err != nil {
			return err
		}
	}
	if r.walkFirst == nil {
		r.walkFirst = map[walkKey]map[evalKey]time.Time{}
	}
	for project, first := range firsts {
		r.walkFirst[walkKey{family: family, project: project}] = first
	}
	return nil
}

// storedFirst is the first input day of each eval of family that project holds a row of, by eval
// and deploy repository (evalKey), read once a pass and moved by every day a page writes
// (noteBuilt): the first day of each eval's all-repositories rows of the series its zero day holds
// (firstDays), of each kept repository's rows of an eval that reads the deploy repository apart.
// It is where a walk begins (deliveryStart, legionStart), so the walk rewrites every day holding a
// row of its family.
func (r *refresh) storedFirst(ctx context.Context, family, project string) (map[evalKey]time.Time, error) {
	firsts, err := r.storedFirsts(ctx, family, []string{project})
	if err != nil {
		return nil, err
	}
	return firsts[project], nil
}

// storedFirsts is storedFirst of each of projects, by project, those the pass has not read yet
// read in one read (readFirsts).
func (r *refresh) storedFirsts(ctx context.Context, family string, projects []string) (map[string]map[evalKey]time.Time, error) {
	firsts := make(map[string]map[evalKey]time.Time, len(projects))
	var unread []string
	for _, project := range projects {
		if first, ok := r.first[walkKey{family: family, project: project}]; ok {
			firsts[project] = first
		} else {
			unread = append(unread, project)
		}
	}
	read, err := r.readFirsts(ctx, family, unread, nil)
	if err != nil {
		return nil, err
	}
	for project, first := range read {
		r.first[walkKey{family: family, project: project}] = first
		firsts[project] = first
	}
	return firsts, nil
}

// readFirsts reads the first day of each of projects' evals of family before before (every day,
// where it is nil), as storedFirst says, in one read (firstDays), by project.
func (r *refresh) readFirsts(ctx context.Context, family string, projects []string, before *time.Time) (map[string]map[evalKey]time.Time, error) {
	if len(projects) == 0 {
		return map[string]map[evalKey]time.Time{}, nil
	}
	zeros := make(map[string][]DayRow, len(projects))
	switch family {
	case walkDelivery:
		kept, err := r.keptRepos(ctx)
		if err != nil {
			return nil, err
		}
		rules := keptRules(kept)
		for _, project := range projects {
			zeros[project] = ZeroDeliveryDay(project, r.today, rules).Rows
		}
	case walkLegion:
		for _, project := range projects {
			zeros[project] = ZeroLegionDay(project, r.today, legionDaySlugs...).Rows
		}
	}
	return r.store.firstDays(ctx, zeros, before)
}

// zerosFrom is each of zeros whose eval's first input day, of its deploy repository's rows, is on
// or before day: the zero rows a day with no input of a series writes, which no day before a
// series' first input day holds.
func zerosFrom(zeros []DayRow, first map[evalKey]time.Time, day time.Time) []DayRow {
	var kept []DayRow
	for _, zero := range zeros {
		if since, ok := first[keyOf(zero)]; ok && !day.Before(since) {
			kept = append(kept, zero)
		}
	}
	return kept
}

// begunBy reports whether first holds a first input day on or before day: where it holds none,
// zerosFrom keeps none of a day's zero rows, which its writer then need not build.
func begunBy(first map[evalKey]time.Time, day time.Time) bool {
	for _, since := range first {
		if !day.Before(since) {
			return true
		}
	}
	return false
}

// noteBuilt moves the first input day of each eval built holds an all-repositories row of (of each
// deploy repository apart) to day, where day is before it or the eval held none: its first input
// day is now. A walk's day moves the walk's first days (firstOf), any other the pass's own.
func (r *refresh) noteBuilt(family, project string, day time.Time, built []DayRow) {
	firsts := r.first
	if r.walking {
		firsts = r.walkFirst
	}
	first := firsts[walkKey{family: family, project: project}]
	for _, row := range built {
		if row.Repo != "" {
			continue
		}
		if since, ok := first[keyOf(row)]; ok && !day.Before(since) {
			continue
		}
		first[keyOf(row)] = day
	}
}

// walk builds what the pass's batch (gapBatch) has left of each walk: the delivery walk (where
// delivery settings exist) and each project's Legion walk, whose days it writes in the open (a
// family's first walk among them, whose serving record follows it, recordWalkIn); the walks of the
// rebuild that is building (rebuildDays), which stage theirs; each project's logged zero fill and
// unowned-p0 row of today; and once a UTC day every project's failure rows whole (walkFailures);
// and then folds the stored figures of the days it wrote (foldWalked).
// It answers whether a walk in the open has days left, after the batch or because the walks stopped
// for an owed wake (yields), and whether a rebuild's walk has, each of which asks for the pass that
// builds them. It computes every day it reaches, so the days the pass's pages wrote are written
// again where a batch reaches them (refresh.done begins empty).
func (r *refresh) walk(ctx context.Context) (bool, bool, error) {
	walks, err := r.store.readWalks(ctx)
	if err != nil {
		return false, false, err
	}
	serving, err := r.store.readServing(ctx)
	if err != nil {
		return false, false, err
	}
	parts := &r.report.parts
	parts.mark(partWalkReads)
	r.walking, r.done = true, map[builtDay]bool{}
	defer func() { r.walking = false }()
	live := false
	if key := (walkKey{family: walkDelivery}); r.delivers() {
		more, err := r.walkDays(ctx, key, walks[key].next, r.deliveryStart, r.deliveryWalkDay, r.recordWalk)
		if err != nil {
			return false, false, err
		}
		live = live || more
	}
	parts.mark(partDeliveryWalk)
	for _, project := range r.projects {
		key := walkKey{family: walkLegion, project: project}
		more, err := r.walkDays(ctx, key, walks[key].next,
			func(ctx context.Context) (*time.Time, error) { return r.legionStart(ctx, project) },
			func(ctx context.Context, day time.Time) error { return r.writeLegion(ctx, project, day) },
			r.recordWalk)
		if err != nil {
			return false, false, err
		}
		live = live || more
	}
	parts.mark(partLegionWalks)
	rebuilding, err := r.rebuildDays(ctx, walks, serving)
	if err != nil {
		return false, false, err
	}
	parts.mark(partRebuildWalks)
	for _, project := range r.projects {
		if err := r.walkDaily(ctx, walks, walkKey{family: walkLogged, project: project},
			func(ctx context.Context) error { return r.fillLogged(ctx, project) }); err != nil {
			return false, false, err
		}
		if err := r.walkDaily(ctx, walks, walkKey{family: walkUnowned, project: project},
			func(ctx context.Context) error { return r.writeUnowned(ctx, project) }); err != nil {
			return false, false, err
		}
	}
	if err := r.walkFailures(ctx); err != nil {
		return false, false, err
	}
	parts.mark(partDaily)
	err = r.foldWalked(ctx)
	parts.mark(partFoldWalked)
	return live || r.report.yielded, rebuilding, err
}

// yields reports whether the pass's walks stop before their next write, a day of a walk, a daily
// write or a project's batch of the figures walk, and records it in the pass's report: a wake is
// owed (duty.WakeOwed), so a change waits behind the write being made rather than the whole batch,
// and the walks have made one write in this pass already, so no run of wakes keeps them from
// building. A write of the walks commits an eval_days change, which wakes the loop as a change
// does, and the pass cannot tell that echo from a change: so in the server a pass that walks
// builds about one write, and what it reads once for the days it builds (beginBatch, the deploy
// runs of deliveryWalkDay) the pass the echo owes reads again. That pass builds what the batch has
// left after its own changes' work (gapBatch). A test's pass, which no hub wakes, builds the
// whole batch from one read.
func (r *refresh) yields(ctx context.Context) bool {
	if r.walkBuilt == 0 || !duty.WakeOwed(ctx) {
		return false
	}
	r.report.yielded = true
	return true
}

// walkDays builds what the pass's batch has left of the walk key, standing at next (nil for a walk
// with no record, or one begun again): from next, or, where it is to begin, from the first input
// day begin answers, it builds up to the batch's days left of the walk through today with build,
// stopping between two days for an owed wake (yields), and records where it stands with record. A
// walk with nothing to walk (begin answers nil) stands at tomorrow. It answers whether days before
// today are left. The days a pass builds of the walk build by the first input days of the days
// before their first (beginBatch), and write each project's day of them that holds rows of the
// walk's family whether or not they build a row of it (heldDays), so a day whose input left holds
// no row a whole walk would not hold. A rebuild's walk is one too (rebuildWalk): its days are
// staged where a walk's are written (walkWrite), and its record is the staging of its batch
// (stageWalk).
func (r *refresh) walkDays(ctx context.Context, key walkKey, next *time.Time,
	begin func(context.Context) (*time.Time, error), build func(context.Context, time.Time) error,
	record func(context.Context, walkKey, time.Time) error) (bool, error) {
	if r.yields(ctx) {
		return true, nil
	}
	begun := next == nil
	if begun {
		start, err := begin(ctx)
		if err != nil {
			return false, err
		}
		if start == nil {
			return false, record(ctx, key, r.today.AddDate(0, 0, 1))
		}
		next = start
	}
	if next.After(r.today) {
		if begun {
			return false, record(ctx, key, *next)
		}
		return false, nil
	}
	spent := r.batchKey(key)
	last, ok := r.batched.through(spent, r.batch, *next, r.today)
	if !ok {
		return true, nil
	}
	if err := r.beginBatch(ctx, key, *next, last); err != nil {
		return false, err
	}
	built := 0
	day := *next
	for ; !day.After(last); day = day.AddDate(0, 0, 1) {
		if r.yields(ctx) {
			break
		}
		if err := build(ctx, day); err != nil {
			return false, err
		}
		built++
		r.walkBuilt++
	}
	if built == 0 {
		return true, nil
	}
	r.batched.spend(spent, built)
	if err := record(ctx, key, day); err != nil {
		return false, err
	}
	return !day.After(r.today), nil
}

// batchKey is the key the walks' batch of the gap (gapBatch) counts key's days under: its own, or,
// for a rebuild's walk of it (r.staging), rebuildBatchKey's.
func (r *refresh) batchKey(key walkKey) walkKey {
	if r.staging != nil {
		return rebuildBatchKey(key)
	}
	return key
}

// rebuildBatchKey is the key the walks' batch of the gap (gapBatch) counts the days of a rebuild's
// walk key under, one apart from the walk's own, so a walk in the open and a rebuild's walk of one
// family, and a project's figures walk and the rebuild's, each build up to a whole batch per gap.
func rebuildBatchKey(key walkKey) walkKey {
	key.family = "rebuild " + key.family
	return key
}

// beginBatch begins a batch of the walk key from from through through: the walk's first input days
// of every project the batch writes are read anew of the days before from, in one read
// (loadWalkFirsts), and the days of the batch that hold rows of its family (walkHeld) are read,
// once for the days the pass builds of the batch (yields says how many that is under the loop).
func (r *refresh) beginBatch(ctx context.Context, key walkKey, from, through time.Time) error {
	if r.walkFrom == nil {
		r.walkFrom, r.walkFirst, r.walkHeld = map[walkKey]time.Time{}, map[walkKey]map[evalKey]time.Time{}, map[builtDay]bool{}
	}
	r.walkFrom[key] = from
	projects, slugs := r.owners, deliveryDaySlugs
	if key.family == walkLegion {
		projects, slugs = []string{key.project}, legionDaySlugs
	}
	if err := r.loadWalkFirsts(ctx, key.family, projects); err != nil {
		return err
	}
	held, err := r.store.heldDays(ctx, key.family, projects, slugs, from, through)
	if err != nil {
		return err
	}
	maps.Copy(r.walkHeld, held)
	return nil
}

// deliveryStart is the first input day of the delivery walk, the UTC midnight of the earliest of
// the first merge of a pull request, the first start of a run of a kept repository (keptRepos) and
// the first day any project holds a delivery or CI row of, nil where none exists. Every project's
// first days are taken together, in one read (storedFirsts), so the walk begins at a midnight
// however many projects there are, none included.
func (r *refresh) deliveryStart(ctx context.Context) (*time.Time, error) {
	kept, err := r.keptRepos(ctx)
	if err != nil {
		return nil, err
	}
	earliest, err := r.store.deliveryInputStart(ctx, kept)
	if err != nil {
		return nil, err
	}
	stored, err := r.storedFirsts(ctx, walkDelivery, r.owners)
	if err != nil {
		return nil, err
	}
	firsts := map[evalKey]time.Time{}
	for _, first := range stored {
		// An eval id names its project, so no two projects' entries share a key.
		maps.Copy(firsts, first)
	}
	return earliestOf(earliest, firsts), nil
}

// legionStart is the first input day of project's Legion walk: the earliest of its first close of
// an issue with Legion phase work, its first filing and the first day it holds a Legion row of,
// nil where none exists.
func (r *refresh) legionStart(ctx context.Context, project string) (*time.Time, error) {
	earliest, err := r.store.legionInputStart(ctx, project)
	if err != nil {
		return nil, err
	}
	first, err := r.storedFirst(ctx, walkLegion, project)
	if err != nil {
		return nil, err
	}
	return earliestOf(earliest, first), nil
}

// earliestOf is the UTC midnight of the earliest of earliest and the days of first.
func earliestOf(earliest *time.Time, first map[evalKey]time.Time) *time.Time {
	for _, day := range first {
		if earliest == nil || day.Before(*earliest) {
			earliest = &day
		}
	}
	if earliest == nil {
		return nil
	}
	day := measures.Midnight(*earliest)
	return &day
}

// deliveryWalkDay writes one day of the delivery walk: every project's and every owner key's
// delivery and CI rows of it, of every kept repository, built for each owner (buildDelivery) and
// written together in one transaction (walkWrite), so a read that folds a project's own rows with
// those of the owner keys it reads (eval_project_owners) finds all of the day or none of it; each
// owner's first input days then move by what the day built of it (noteBuilt). The deploy runs of
// each kept repository every day the pass builds of a batch reads from its own start on, and the
// posted deploys of each kept events repository that finished from the pass's first day on, are
// read once, at the pass's first day (delivery.ReadApplyRows, postedDeploys), each repository's
// read side by side, and each day takes its part of them: a batch's days run in order, so the
// first day's read holds every later day's runs and posts, and the pass the loop owes after one
// that yielded (yields) reads them again.
func (r *refresh) deliveryWalkDay(ctx context.Context, day time.Time) error {
	if r.walkApplies == nil || day.Before(r.walkAppliesFrom) {
		kept, err := r.keptRepos(ctx)
		if err != nil {
			return err
		}
		applies := make([]delivery.ApplyRows, len(kept))
		posted := make([]postedRecord, len(kept))
		var reads []func(context.Context) error
		r.walkApplies = make(map[string]*delivery.ApplyRows, len(kept))
		for i, repo := range kept {
			if IsEventsRepo(repo.name) {
				project := repo.rule.Project
				reads = append(reads, func(ctx context.Context) (err error) {
					posted[i], err = postedDeploys(ctx, r.store.database.Pool, project, day)
					return err
				})
				continue
			}
			settings := repo.rule
			reads = append(reads, func(ctx context.Context) (err error) {
				if applies[i], err = delivery.ReadApplyRows(ctx, r.store.database.Pool, settings, day); err != nil {
					return fmt.Errorf("read the deploy runs of %s from %s on: %w", repo.name, day.Format(time.DateOnly), err)
				}
				return nil
			})
			r.walkApplies[repo.name] = &applies[i]
		}
		if err := store.ReadSideBySide(ctx, reads...); err != nil {
			r.walkApplies, r.walkPosted = nil, nil
			return err
		}
		r.walkPosted = make(map[string]postedRecord)
		for i, repo := range kept {
			if IsEventsRepo(repo.name) {
				r.walkPosted[repo.name] = posted[i]
			}
		}
		r.walkAppliesFrom = day
	}
	read, err := r.readDelivery(ctx, day, r.walkApplies, r.walkPosted)
	if err != nil {
		return err
	}
	var owned []ownerDay
	for _, project := range r.owners {
		one, err := r.buildDelivery(ctx, read, project)
		if err != nil {
			return err
		}
		if one != nil {
			owned = append(owned, *one)
		}
	}
	days := make([]ProjectDay, len(owned))
	for i, one := range owned {
		days[i] = one.day
	}
	if err := r.walkWrite(ctx, deliveryFamily, walkKey{family: walkDelivery}, read.Day, days...); err != nil {
		return err
	}
	for _, one := range owned {
		r.noteBuilt(walkDelivery, one.day.Project, read.Day, one.built)
		key := builtDay{family: walkDelivery, project: one.day.Project, day: read.Day}
		r.done[key], r.report.days[key] = true, true
	}
	return nil
}

// walkDaily makes the once-a-day write of the walk key: write runs on the first walking pass of
// each UTC day, and on the first walking pass of a walk begun again (no record of it), and the
// walk then stands at tomorrow; a pass whose walks stopped for an owed wake
// (yields) leaves it to the next. The logged evals' zero fill is one (fillLogged through today),
// and the unowned-p0 row of today another (writeUnowned): its count reads issues alone, so it is
// written whether or not delivery settings exist, a project made since the last pass included.
func (r *refresh) walkDaily(ctx context.Context, walks map[walkKey]walkState, key walkKey, write func(context.Context) error) error {
	state, found := walks[key]
	if found && state.next != nil && state.next.After(r.today) {
		return nil
	}
	if r.yields(ctx) {
		return nil
	}
	if err := write(ctx); err != nil {
		return err
	}
	r.walkBuilt++
	return r.recordWalk(ctx, key, r.today.AddDate(0, 0, 1))
}
