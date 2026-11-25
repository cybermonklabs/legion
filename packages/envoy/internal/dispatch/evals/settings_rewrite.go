package evals

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// settings_rewrite.go is the refresher's answer to a change of the delivery settings
// (dispatch://INFRA-2916, R14): one pass rewrites, in one transaction, the days of the delivery
// and CI evals whose rows the change moves, and begins no walk. The settings the stored delivery
// and CI rows stand built under are recorded beside them (eval_days_settings, migration
// 20261117100021), and a page compares them with the settings it reads before it writes a day
// (syncSettings): field by field, since a settings change in the log names no field, and since rows
// can stand built under other settings than the change before the page's cursor implies (a page
// whose pass read the settings after a later change committed).
//
// A change that moves no eval writes nothing and begins no walk. One of a setting no day row reads
// (a workflow path, the population rule's authors and excluded repositories, the roll-up job), a
// move of the deploy repository (the evals that read it are kept for every repository that holds
// a delivery run, and a read chooses among them by the repository recorded with the rows:
// deploy_repos.go, DeployRepoIn), or one that leaves the production rule and every deploy card
// following the same names with the same step (the order of the production names, a staging job
// renamed between two names the production rule follows), moves no eval by settingsMoves, and the
// pass reads nothing. A production job name no run holds moves the rule's names but its verdict
// on no run and no card's part, so the rewrite reads the history and, once the delivery walk has
// written today, builds no day: it builds only the days whose rows the change
// can move (movedDeliveryDays). Until then it builds every day (rewriteSettings says why). Each
// of its writes sends a row, a member or a figure only where its value moved.

// settingsMoves is the delivery and CI evals (by slug) whose day rows a change of the delivery
// settings from prev to next can move, and the families of eval_day_members it can move, read from
// the code that builds the rows (readDeliveryDays, DeliveryDay.Rows and the measures they call).
// Each is a rule every repository's rows are built by alike, so it moves the rows of each
// repository that holds a run it reads, and of no other (rewriteSettings):
//
//   - the production rule (production_job_names, production_apply_step_name): it decides which run
//     is a deploy (ProductionApplies), so containment (the three lead times to production, and the
//     delivery family, whose shipped_by is containment's and whose deploy runs count for the
//     projects of the pull requests they ship), deploy-frequency and deploy-run-success. And each
//     deploy card either settings make, with the CI family: a push run counts for the projects of
//     the deploy it is (attribution.pushRuns), which containment names, so the rule moves which
//     projects count each card's rows and members. The rule reads its names as a set
//     (Production.Job), so their order alone moves nothing.
//   - each deploy card (production-deploy, staging-e2e and staging-deploy, cardParts): the job
//     names it counts a run by and its apply step, and whether the settings make it at all. So the
//     production rule moves production-deploy, a staging job name its card, and a production job
//     name a staging card follows that card too (a name two cards follow counts for the first).
//     The push runs the cards read are every push run on main whatever its jobs, so a card's own
//     parts move no member.
//   - deploy_repo: none. Every read of a day names it (the deploys, the apply runs, the CI runs),
//     and containment ships its pull requests alone, but the evals those reads build are kept for
//     every repository that holds a delivery run, each as if it were the deploy repository, and
//     the eval list reads those of the repository recorded with the rows (DeployRepoIn).
//   - deploy_workflow_path, pr_checks_workflow_path, population_authors, excluded_repos and
//     pr_checks_rollup_job_name: none. The first four decide what the intake and the reconcile
//     store, each store a change of its own, and no read of a day names them; the roll-up job is
//     read at read time (the Pipeline's failing jobs, pr-ci's items) and by no day row.
func settingsMoves(prev, next delivery.DeliverySettings) (slugs, families []string) {
	moved, family := map[string]bool{}, map[string]bool{}
	before, after := cardParts(prev), cardParts(next)
	if partOf(prev.ProductionJobNames, prev.ProductionApplyStepName) != partOf(next.ProductionJobNames, next.ProductionApplyStepName) {
		for _, slug := range slices.Concat(leadTimeSlugs, []string{SlugDeployFrequency, SlugDeployRunSuccess}) {
			moved[slug] = true
		}
		for card, slug := range cardSlugs {
			_, was := before[card]
			_, is := after[card]
			if was || is {
				moved[slug] = true
				family[KindCI] = true
			}
		}
		family[KindDelivery] = true
	}
	for card, slug := range cardSlugs {
		was, held := before[card]
		now, holds := after[card]
		if held != holds || was != now {
			moved[slug] = true
		}
	}
	return slices.Sorted(maps.Keys(moved)), slices.Sorted(maps.Keys(family))
}

// rulePart is what a production rule, or a deploy card, counts a run by: the job names it
// follows, sorted and each once, joined by a NUL (no job name holds one), and its apply step.
type rulePart struct {
	names   string
	step    string
	stepped bool
}

// partOf is the rulePart of a rule that follows names and reads the apply step step (nil for
// none).
func partOf(names []string, step *string) rulePart {
	part := rulePart{names: strings.Join(slices.Compact(slices.Sorted(slices.Values(names))), "\x00")}
	if step != nil {
		part.step, part.stepped = *step, true
	}
	return part
}

// cardParts is each deploy card settings make (measures.DeployCards) by its id, with what it counts
// a run by: the names it follows that no card before it follows, which measures.DeployJobsDay
// gives it, and its apply step.
func cardParts(settings delivery.DeliverySettings) map[measures.DeployJobCardID]rulePart {
	parts := map[measures.DeployJobCardID]rulePart{}
	taken := map[string]bool{}
	for _, card := range measures.DeployCards(settings) {
		var names []string
		for _, name := range card.JobNames {
			if !taken[name] {
				names = append(names, name)
			}
		}
		for _, name := range card.JobNames {
			taken[name] = true
		}
		parts[card.CardID] = partOf(names, card.Production.ApplyStep)
	}
	return parts
}

// sameBuiltSettings reports whether a and b agree on every delivery setting eval_days_settings
// records, each exactly as stored: the four a day row reads, and the deploy repository, which
// moves no row but is the one whose rows a read answers (DeployRepoIn).
func sameBuiltSettings(a, b delivery.DeliverySettings) bool {
	return slices.Equal(a.ProductionJobNames, b.ProductionJobNames) &&
		sameName(a.ProductionApplyStepName, b.ProductionApplyStepName) && sameName(a.StagingE2EJobName, b.StagingE2EJobName) &&
		sameName(a.StagingApplyJobName, b.StagingApplyJobName) && a.DeployRepo == b.DeployRepo
}

// sameName reports whether two optional names are one.
func sameName(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// readBuiltUnder reads the delivery settings the stored delivery and CI rows stand built under
// (builtUnderIn), nil where none are recorded, and the repositories that hold a delivery run they
// stand built for, each with its start (kept_repos, kept_since).
func (s *Store) readBuiltUnder(ctx context.Context) (*delivery.DeliverySettings, []keptRepo, error) {
	return builtUnderIn(ctx, s.database.Pool)
}

// builtUnderIn reads, in q, the delivery settings the stored delivery and CI rows stand built
// under (eval_days_settings), the fields recorded there alone (sameBuiltSettings), nil where none
// are recorded, and the repositories that hold a delivery run they stand built for.
func builtUnderIn(ctx context.Context, q delivery.Querier) (*delivery.DeliverySettings, []keptRepo, error) {
	var built delivery.DeliverySettings
	var names []string
	var since []time.Time
	err := q.QueryRow(ctx, `
		select production_job_names, production_apply_step_name, staging_e2e_job_name, staging_apply_job_name, deploy_repo,
			kept_repos, kept_since
		from eval_days_settings where singleton
	`).Scan(&built.ProductionJobNames, &built.ProductionApplyStepName, &built.StagingE2EJobName, &built.StagingApplyJobName,
		&built.DeployRepo, &names, &since)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil, nil
	case err != nil:
		return nil, nil, fmt.Errorf("read the delivery settings the day rows stand built under: %w", err)
	case len(names) != len(since):
		return nil, nil, fmt.Errorf("the day rows stand built for %d repositories with %d starts", len(names), len(since))
	}
	kept := make([]keptRepo, len(names))
	for i, name := range names {
		kept[i] = keptRepo{name: name, since: measures.Midnight(since[i])}
	}
	return &built, kept, nil
}

// BuiltSettingsIn is, read in q, the delivery settings an eval's stored delivery and CI rows
// answer under: the saved settings (delivery.GetSettings), with each field eval_days_settings
// records (sameBuiltSettings: the four a day row reads and the deploy repository) as the refresher
// recorded it with the rows (recordBuiltUnder), nil where the deployment has set none. Between a
// save of the settings and the refresher's commit that brings the rows to them, a reader of the
// evals then reads the rows and the settings they were built under together, the old ones whole,
// and from that commit on the new ones whole. Where no pass has recorded the rows' settings (a
// walk that begins again builds every day under the saved ones), it is the saved settings.
func BuiltSettingsIn(ctx context.Context, q delivery.Querier) (*delivery.DeliverySettings, error) {
	settings, err := delivery.GetSettings(ctx, q)
	if errors.Is(err, delivery.ErrNoSettings) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	built, _, err := builtUnderIn(ctx, q)
	if err != nil {
		return nil, err
	}
	if built != nil {
		settings.ProductionJobNames, settings.ProductionApplyStepName = built.ProductionJobNames, built.ProductionApplyStepName
		settings.StagingE2EJobName, settings.StagingApplyJobName = built.StagingE2EJobName, built.StagingApplyJobName
		settings.DeployRepo = built.DeployRepo
	}
	return &settings, nil
}

// recordBuiltUnder records, in q, settings as the delivery settings the stored delivery and CI rows
// stand built under (eval_days_settings), the fields sameBuiltSettings compares.
func recordBuiltUnder(ctx context.Context, q pgx.Tx, settings delivery.DeliverySettings) error {
	if _, err := q.Exec(ctx, `
		insert into eval_days_settings (production_job_names, production_apply_step_name, staging_e2e_job_name, staging_apply_job_name, deploy_repo)
		values ($1, $2, $3, $4, $5)
		on conflict (singleton) do update
		set production_job_names = excluded.production_job_names,
			production_apply_step_name = excluded.production_apply_step_name,
			staging_e2e_job_name = excluded.staging_e2e_job_name, staging_apply_job_name = excluded.staging_apply_job_name,
			deploy_repo = excluded.deploy_repo, updated_at = now()
	`, nonNil(settings.ProductionJobNames), settings.ProductionApplyStepName,
		settings.StagingE2EJobName, settings.StagingApplyJobName, settings.DeployRepo); err != nil {
		return fmt.Errorf("record the delivery settings the day rows stand built under: %w", err)
	}
	return nil
}

// syncSettings brings the stored delivery and CI rows to the delivery settings the pass reads
// (r.settings) before the page writes a day under them, on every page, so no row is written under
// settings the rows do not stand built under:
//
//   - where the recorded settings (eval_days_settings, read once a pass) agree with them on every
//     field recorded there (sameBuiltSettings), nothing;
//   - where they differ, it rewrites the days of the evals the difference moves whose rows it can
//     move (settingsMoves, rewriteSettings), which records the settings in its own transaction,
//     or, where the difference moves no eval, has the page record them with its cursors (w.built);
//   - where none are recorded (a database no pass of this release has brought to its settings, or
//     one a bench reset), the rows' settings are unknown, so it has the page record them with its
//     cursors and, in the same transaction, begin a rebuild of every served family (rebuild.go),
//     which verifies every day under the settings it reads aside while the stored rows keep
//     serving, or, where no delivery rows are served yet, begin the delivery walk again, which
//     builds every day under them in the open.
//
// The commit that records the settings says that the rows stand at them (markSettingsApplied),
// which recomputes every evals view, where it moves the deploy repository, the one recorded field
// a read of the evals takes (DeployRepoIn), or where none was recorded and the page read a change
// of the settings row, and says nothing otherwise: the rewrite's eval_days changes, one of each
// eval whose rows it moved, recompute the evals views of their projects, and a difference that
// moves no row and not the deploy repository moves nothing an evals view reads. Until the first
// record a read takes the deploy repository the settings name, so that record moves it only where
// a save came after the pass read them, whose change a page then reads. A move of the deploy
// repository alone moves no eval, so it writes no row: the page records the new repository with
// its cursors, in the commit that says the rows stand at the settings, from which on a read
// chooses its rows (DeployRepoIn).
func (r *refresh) syncSettings(ctx context.Context, w *pageWork) error {
	if r.settings == nil {
		return nil
	}
	if !r.builtRead {
		built, kept, err := r.store.readBuiltUnder(ctx)
		if err != nil {
			return err
		}
		r.builtUnder, r.keptUnder, r.builtRead = built, kept, true
	}
	next := *r.settings
	if r.builtUnder == nil {
		served, err := r.servingNext(ctx, walkDelivery, "")
		if err != nil {
			return err
		}
		switch {
		case served == nil:
			w.restart[walkDelivery] = true
		case w.rebuild == "":
			w.rebuild = rebuildSettings
		}
		w.built, w.settingsApplied, r.builtUnder = &next, w.settings, &next
		return nil
	}
	if sameBuiltSettings(*r.builtUnder, next) {
		return nil
	}
	repoMoved := r.builtUnder.DeployRepo != next.DeployRepo
	if slugs, families := settingsMoves(*r.builtUnder, next); len(slugs) > 0 {
		if err := r.rewriteSettings(ctx, *r.builtUnder, slugs, families, repoMoved); err != nil {
			return err
		}
	} else {
		w.built, w.settingsApplied = &next, repoMoved
	}
	r.builtUnder = &next
	return nil
}

// syncKept compares the repositories that hold a delivery run now (keptRepos, read once the page
// has read its changes) with those the delivery and CI rows stand built for (keptUnder, recorded
// beside the settings they stand built under), after syncSettings, so the record exists or the
// page's commit makes it. Where they differ the page records the new set with its cursors
// (w.kept), and, where the stored delivery rows are whole through a day (eval_days_serving),
// builds in the same transaction the days of each repository that differs (w.keptWas,
// refresh.writeNamed): a new repository's from its start on, the days between a moved one's two
// starts, and none of a gone one's, whose rows and members go. Those are the rows and members a
// whole walk over the same runs holds, where the change that brought a repository's first runs
// names the days of its runs alone. A crash before the commit leaves the record as it was, so the
// next page finds the same difference. A move of the deploy repository moves no run, and so
// nothing here.
func (r *refresh) syncKept(ctx context.Context, w *pageWork) error {
	if r.settings == nil {
		return nil
	}
	kept, err := r.keptRepos(ctx)
	if err != nil {
		return err
	}
	if sameKept(kept, r.keptUnder) {
		return nil
	}
	w.kept, w.keptWas = kept, r.keptUnder
	r.keptUnder = kept
	return nil
}

// sameKept reports whether a and b, each in the byte order of the names, are the same
// repositories with the same starts.
func sameKept(a, b []keptRepo) bool {
	return slices.EqualFunc(a, b, func(x, y keptRepo) bool { return x.name == y.name && x.since.Equal(y.since) })
}

// recordKept records, in q, kept as the repositories the stored delivery and CI rows stand built
// for (eval_days_settings' kept_repos and kept_since), on the row the settings are recorded on.
func recordKept(ctx context.Context, q pgx.Tx, kept []keptRepo) error {
	names, since := make([]string, len(kept)), make([]time.Time, len(kept))
	for i, repo := range kept {
		names[i], since[i] = repo.name, repo.since
	}
	if _, err := q.Exec(ctx, `update eval_days_settings set kept_repos = $1, kept_since = $2::date[], updated_at = now() where singleton`, names, since); err != nil {
		return fmt.Errorf("record the repositories the day rows stand built for: %w", err)
	}
	return nil
}

// rewriteWorkers is the most goroutines rewriteSettings runs at once, each computing one
// repository's containment under one rule or building one day: the CPUs the process may use, up
// to this many.
const rewriteWorkers = 8

// rewriteDays is how many days rewriteSettings builds at a time: what it holds at once is a
// chunk's days, their rows and their samples, as many as a pass's batch of the delivery walk
// writes, rather than every day of the history, beside the rows of the evals it rewrites of every
// day built, which it writes once the last chunk is built.
const rewriteDays = walkBatch

// memberKey is a member's identity in eval_day_members, and memberAt where it is counted.
type memberKey struct {
	project, family, deployRepo, rowKey string
}

type memberAt struct {
	day             time.Time
	repo, shippedBy string
}

// readMembers reads every member of projects under families counted under one of deployRepos on
// one of days, by its key, and none where days is empty. eval_day_members_day (project_key,
// family, day) keys its filter of the days. projects (every project and owner key) and days (every
// day of the history a rewrite builds) grow with the history, so each goes in cuts
// (statementChunks), each statement one cut of each: the projects in cuts of at most half of
// statementBytes, all in one where they fit, and the days in cuts of what the rest of a statement
// holds. A member stands on one day, so no two statements read it.
func (s *Store) readMembers(ctx context.Context, projects, families, deployRepos []string, days []time.Time) (map[memberKey]memberAt, error) {
	held := map[memberKey]memberAt{}
	if len(families) == 0 || len(projects) == 0 || len(deployRepos) == 0 || len(days) == 0 {
		return held, nil
	}
	projectBytes := 0
	for i := range projects {
		projectBytes += textBytes(&projects[i])
	}
	// A cut of the projects takes at most reserve bytes, the rest of a statement its other lists:
	// statementChunks closes a cut at statementBytes, counted from statementBytes-reserve.
	reserve := min(projectBytes, statementBytes/2)
	base := paramBytes(4) + reserve
	for _, list := range [][]string{families, deployRepos} {
		for i := range list {
			base += textBytes(&list[i])
		}
	}
	dayBytes := func(*time.Time) int { return 4 + 8 }
	for named := range statementChunks(projects, statementBytes-reserve, textBytes) {
		for dates := range statementChunks(days, base, dayBytes) {
			rows, err := s.database.Pool.Query(ctx, `
				select project_key, family, deploy_repo, row_key, day, repo, shipped_by from eval_day_members
				where project_key = any($1) and family = any($2) and deploy_repo = any($3) and day = any($4::date[])
			`, named, families, deployRepos, dates)
			if err != nil {
				return nil, fmt.Errorf("read the members of %v: %w", families, err)
			}
			var key memberKey
			var at memberAt
			if _, err := pgx.ForEachRow(rows, []any{&key.project, &key.family, &key.deployRepo, &key.rowKey, &at.day, &at.repo, &at.shippedBy}, func() error {
				at.day = measures.Midnight(at.day)
				held[key] = at
				return nil
			}); err != nil {
				return nil, fmt.Errorf("read the members of %v: %w", families, err)
			}
		}
	}
	return held, nil
}

// rewriteSettings rewrites, under the settings the pass reads (r.settings), the days of the evals
// slugs names and the members of families whose rows the change from prev, the settings the rows
// stand built under, can move, as the whole delivery walk under the new settings writes them, in
// one pass that begins no walk. It rewrites them for each kept repository (keptRepos) that holds a
// deploy run, which the production rule and the deploy cards judge. A repository that holds none
// moves with no setting, and its rows and members are left as they are:
//
//   - one read of the history, its parts side by side: every population pull request, and every
//     deploy run on main of each repository rewritten with its jobs of the names either rule
//     judges by and each card the change moves follows under either settings
//     (delivery.ReadHistoryRows), the walk's first input day (deliveryInputStart, over every kept
//     repository), each project's first days (firstDays) and the record of the walks
//     (readWalks);
//   - the attribution lists of the pull requests (readAttribution), and beside that read
//     containment over each repository's history as the deploy repository (historyOf): the new
//     production rule's verdict on each run (ProductionApplies, which judges a run by its own jobs
//     alone, and by its jobs of the judged names as by every job), each pull request's first
//     shipping apply (delivery.ShippingIndex), and each deploy's shipped set, the pull requests
//     whose first shipping apply it is (Deploys.ShippedBy's rule over every day at once); and,
//     where it builds the days the change can move alone (below), the old rule's verdict and first
//     shipping applies (containmentOf), the new rule's where the rule did not change. No
//     containment reads the attribution, so each repository's under each rule runs on a
//     goroutine of its own, as many at once as rewriteWorkers allows;
//   - the days to build, from the delivery walk's first day through today: those whose rows the
//     change can move (movedDeliveryDays), none for a change that moves no verdict and no card's
//     part, and every day after one of them that was a moved eval's first input day and holds no
//     input of it under the new settings (lostFirstDays); or every day, where the rows may not
//     stand built under prev: no delivery rows are served yet (eval_days_serving), or the delivery
//     walk is to begin or has days left (a family's first walk begins it), and a day it has not
//     reached holds rows of the settings before those its walk began under, which no comparison of
//     prev with the new settings finds;
//   - those days in day order a chunk at a time (rewriteDays), each chunk's days built side by
//     side, each repository's DeliveryDay as readDeliveryDays builds it from those rows and each
//     project's rows of the day (deliveryRead.Rows); then, in day order, each project's day as the
//     walk writes it (writeDelivery, dropUnshipped, zerosFrom and noteBuilt), by the first input
//     days the walk builds by: those the build itself finds, day by day from the first, where it
//     builds every day, and otherwise the store's, which the walk built, but those lostFirstDays
//     drops; with the zero rows of each series from its first input day on where the stored
//     delivery rows are whole through the day (servingNext), and with none where they are not
//     (writeNamed's rule); and of the days written, those holding a row or that can hold a stored
//     row of a moved eval (on or after its first stored first input day), the rows of the evals
//     slugs names and the members of families, held until the last chunk is built; and beside that
//     build, the stored members of families of the repositories rewritten on those days
//     (readMembers), none where it builds none, of the CI family the push runs' alone: the days
//     build no PR-checks run, whose project its pull request's issue decides (attribution.check),
//     which no setting moves, so its members stand as stored;
//   - then, in one transaction: those rows upserted where a value moved (upsertDays, which cuts
//     them into statements of at most statementBytes of parameters); once, over every day
//     written, those evals' stored rows of the repositories rewritten that the days no longer hold
//     deleted (deleteStaleDays' rule, deleteStaleSettingsDays); the members of families of those
//     repositories whose day, repository or shipping run moved, or that are new, upserted, and
//     those of the days written that the days no longer hold deleted, and no other member sent;
//     the stored figures of every day whose rows moved folded again, over each project's span of
//     the days written (foldFiguresInTx, whose quantiles read each rank across the days' sorted
//     samples, measures.SortedSamples); and the settings recorded as the ones the rows stand built
//     under, the repositories they stand built for left as they are (syncKept's). Its eval_days
//     changes are one per eval, as every transaction's are (migration 20261117100014), each under
//     the project that reads the eval (an owner key's under each project that reads the key), so
//     the evals views of each project whose rows moved hear of the whole change once, and each
//     carries the rewrite's mark (MarkSettingsRewrite), so the view is recomputed for them at
//     once. Where the deploy repository moved too (repoMoved), the transaction says that the rows
//     stand at the settings (markSettingsApplied), which every evals view is recomputed for, since
//     a read of any project's evals takes that repository; otherwise no view of a project whose
//     rows did not move hears of it. With no day to build it writes no row, member or figure: the
//     settings recorded, its word that the rows stand at them where the deploy repository moved,
//     and a building rebuild begun again (replaceRebuild) alone.
//
// A day it does not build holds the rows it would build: its inputs, its rows' first days and the
// settings its rows read are what they were built from. It leaves eval_days_owed as it is: it
// writes the moved evals of a day alone, and a day owed is owed the rewrite of every eval of its
// families (writeOwed). It records each day it wrote in the pass's report, as the walk does, and
// the first days it moved (noteBuilt), but no day as done (refresh.done): a change of the same
// page that names a day writes that day whole after it. Once it has committed it logs what each of
// its parts took and what it wrote (rewriteTimes).
func (r *refresh) rewriteSettings(ctx context.Context, prev delivery.DeliverySettings, slugs, families []string, repoMoved bool) error {
	var times rewriteTimes
	times.start()
	next := *r.settings
	pool := r.store.database.Pool
	end := r.today.AddDate(0, 0, 1)
	prevCards, cards := measures.DeployCards(prev), measures.DeployCards(next)
	prevProduction, production := prev.Production(), next.Production()
	kept, err := r.keptRepos(ctx)
	if err != nil {
		return err
	}
	var rewritten []keptRepo
	for _, repo := range kept {
		if repo.deploys {
			rewritten = append(rewritten, repo)
		}
	}
	names, every := deployRepoNames(rewritten), deployRepoNames(kept)
	// One read of the runs serves both rules and the cards the change moves, under both settings.
	// ProductionApplies picks a run's production job among its jobs of the rule's own names alone
	// (Production.Job) and judges that job alone (Production.Conclusion): the jobs of other names it
	// is given reach only its unmatched runs, which no day row reads. DeployJobsDay counts each card
	// by its jobs of the names it follows alone. So each rule's verdict on a run read with the jobs
	// of all these names, and each moved card's part of a day (dayOf, movedDeliveryDays), is the one
	// a read of its own names gives.
	//
	// A name that only cards the change leaves follow is not read, and the rewrite keeps what a read
	// of it would build. Of the cards' rows it keeps the moved cards' alone (slugs). Their members
	// are the day's push runs, every push run on main whatever its jobs, each counting for the
	// projects of the deploy it is: a card's jobs move none of them, and the production rule moves
	// which projects count each, so the rewrite keeps them where families names the CI family
	// (settingsMoves). A card's jobs move only the values of its own rows, never which rows a day
	// holds, since DeployJobsDay gives every card a part of each day with a push run; so each
	// project's day holds the rows the walk's does, which the first days noteBuilt moves, the days
	// written and their zero rows read. The production rule's names, unlike those, are read
	// whatever slugs names (the two rules name one set where it did not move): its verdicts decide
	// which projects a deploy counts for (attribution.deploy) and which lead times a day keeps
	// (dropUnshipped), and so which rows each project's day holds.
	followed := slices.DeleteFunc(slices.Concat(prevCards, cards), func(card measures.DeployJobCard) bool {
		return !slices.Contains(slugs, cardSlugs[card.CardID])
	})
	judged := slices.Compact(slices.Sorted(slices.Values(slices.Concat(prevProduction.JudgedJobNames(), production.JudgedJobNames(),
		measures.DeployCardJobNames(followed)))))

	var history delivery.HistoryRows
	var input *time.Time
	var walks map[walkKey]walkState
	// owners is the delivery family's owners (refresh.owners), and the owner keys the rewritten
	// history counts rows for that the store holds none of yet, added below once it is attributed.
	owners := slices.Clone(r.owners)
	var firsts map[string]map[evalKey]time.Time
	if err := store.ReadSideBySide(ctx,
		func(ctx context.Context) (err error) {
			history, err = delivery.ReadHistoryRows(ctx, pool, names, judged, end)
			return err
		},
		func(ctx context.Context) (err error) {
			input, err = r.store.deliveryInputStart(ctx, every)
			return err
		},
		func(ctx context.Context) (err error) {
			zeros := make(map[string][]DayRow, len(owners))
			for _, project := range owners {
				zeros[project] = ZeroDeliveryDay(next, project, r.today, every).Rows
			}
			firsts, err = r.store.firstDays(ctx, zeros, nil)
			return err
		},
		func(ctx context.Context) (err error) {
			walks, err = r.store.readWalks(ctx)
			return err
		},
	); err != nil {
		return fmt.Errorf("read the delivery history for a settings change: %w", err)
	}
	times.history = times.lap()
	earliest := map[evalKey]time.Time{}
	for _, first := range firsts {
		// An eval id names its project, so no two projects' entries share a key.
		maps.Copy(earliest, first)
	}
	ids := map[string]bool{}
	for _, project := range owners {
		for _, slug := range slugs {
			ids[EvalID(project, slug)] = true
		}
	}
	// heldFrom is the first day each project can hold a stored row of a moved eval on: the earliest
	// first input day the store holds of one (every row of a series lies on or after it). A day
	// built from it on is written though it builds no row, so its stale rows go.
	heldFrom := map[string]time.Time{}
	for project, first := range firsts {
		for key, since := range first {
			if from, held := heldFrom[project]; ids[key.eval] && (!held || since.Before(from)) {
				heldFrom[project] = since
			}
		}
	}
	prs := history.PullRequests
	prsByDay := prsByMergeDay(prs)
	start := earliestOf(input, earliest)
	served, err := r.servingNext(ctx, walkDelivery, "")
	if err != nil {
		return err
	}
	walk, found := walks[walkKey{family: walkDelivery}]
	// The days the change can move alone are built where the stored delivery rows are whole and the
	// delivery walk has written today (no walk begun again rebuilds them), and they need the old
	// rule's verdict beside the new one's.
	movedOnly := start != nil && served != nil && found && walk.next != nil && walk.next.After(r.today)
	// The first input days the rows are built by are the whole walk's. Where every day is built,
	// they are those the build itself finds, day by day from the first (noteBuilt): a series begins
	// on the first day it holds input under the new settings, whatever rows the store held of it.
	// Where the days the change can move are built alone, the walk has written today, and the
	// store's first days are the ones it built (refresh.firstOf), but a first input day the change
	// leaves with no input (lost, below).
	for _, project := range owners {
		first := firsts[project]
		if !movedOnly {
			first = map[evalKey]time.Time{}
		}
		r.first[walkKey{family: walkDelivery, project: project}] = first
	}
	// computed reports whether the stored delivery rows are whole through day (servingNext): a day
	// they are not holds the rows with input alone, as any write outside the walk leaves it
	// (writeNamed).
	computed := func(day time.Time) bool {
		return served != nil && day.Before(*served)
	}
	// The old rule is the new one where its names (in their order) and its step are the same, as
	// for a staging job renamed, and containmentOf reads the settings' production rule alone, so
	// its verdict is the one histories holds.
	sameRule := slices.Equal(prev.ProductionJobNames, next.ProductionJobNames) && sameName(prev.ProductionApplyStepName, next.ProductionApplyStepName)
	histories := make([]repoHistory, len(rewritten))
	var was []containment
	if movedOnly {
		was = make([]containment, len(rewritten))
	}
	// Each containment below reads the history and writes its own slot.
	var attribute attribution
	if err := store.ReadSideBySide(ctx,
		func(ctx context.Context) (err error) {
			attribute, err = readAttribution(ctx, r.store, prs, nil)
			return err
		},
		func(ctx context.Context) error {
			group, groupCtx := errgroup.WithContext(ctx)
			group.SetLimit(min(runtime.GOMAXPROCS(0), rewriteWorkers))
			for k, repo := range rewritten {
				group.Go(func() error {
					if err := groupCtx.Err(); err != nil {
						return err
					}
					histories[k] = historyOf(repo, history.Deploys[repo.name], prs, production)
					return nil
				})
				if movedOnly && !sameRule {
					group.Go(func() error {
						if err := groupCtx.Err(); err != nil {
							return err
						}
						was[k] = containmentOf(repo.name, history.Deploys[repo.name], prs, prevProduction)
						return nil
					})
				}
			}
			return group.Wait()
		},
	); err != nil {
		return err
	}
	times.attribution = times.lap()
	for _, owner := range historyOwners(histories, prs, attribute) {
		if slices.Contains(owners, owner) {
			continue
		}
		owners = append(owners, owner)
		r.first[walkKey{family: walkDelivery, project: owner}] = map[evalKey]time.Time{}
		for _, slug := range slugs {
			ids[EvalID(owner, slug)] = true
		}
	}
	r.noteOwners(owners)
	var days []time.Time
	if movedOnly {
		if sameRule {
			for k := range histories {
				was[k] = histories[k].containment
			}
		}
		days = movedDeliveryDays(histories, was, prs, attribute, prevCards, cards, slugs, owners, firsts, *start, r.today)
	} else if start != nil {
		for day := *start; !day.After(r.today); day = day.AddDate(0, 0, 1) {
			days = append(days, day)
		}
	}
	// dayOf is each project's rows of day: each repository's DeliveryDay of it, as readDeliveryDays
	// builds it, from the history's rows (repoHistory.deliveryDay).
	dayOf := func(day time.Time) []ProjectDay {
		read := deliveryRead{Day: day, repos: names, days: make([]DeliveryDay, len(histories))}
		for k := range histories {
			read.days[k] = histories[k].deliveryDay(day, cards, prs, prsByDay, attribute)
		}
		rows := make([]ProjectDay, len(owners))
		for p, project := range owners {
			rows[p] = read.Rows(project)
		}
		return rows
	}
	if movedOnly {
		days = r.lostFirstDays(days, owners, firsts, ids, names, dayOf)
	}
	times.days = times.lap()

	var rows []DayRow
	var pairProjects []string
	var pairDays []time.Time
	var stored map[memberKey]memberAt
	members := map[memberKey]memberAt{}
	written := map[projectDay]bool{}
	spans := map[string]daySpan{}
	// The stored members are read of the days built alone, beside their build, and of none where
	// none is built. writeMovedMembers looks up each member built, and sends it where the one stored
	// stands elsewhere or is not found: a member stored on a day not built is sent either way, since
	// its day is not the one built; and one stored on a day built is read. It deletes only members
	// stored on a day written, and every day written is built.
	if err := store.ReadSideBySide(ctx,
		func(ctx context.Context) (err error) {
			stored, err = r.store.readMembers(ctx, owners, families, names, days)
			return err
		},
		func(ctx context.Context) error {
			for chunk := range slices.Chunk(days, rewriteDays) {
				// The chunk's days built side by side.
				built := make([][]ProjectDay, len(chunk))
				group, groupCtx := errgroup.WithContext(ctx)
				group.SetLimit(min(runtime.GOMAXPROCS(0), rewriteWorkers))
				for index, day := range chunk {
					group.Go(func() error {
						if err := groupCtx.Err(); err != nil {
							return err
						}
						built[index] = dayOf(day)
						return nil
					})
				}
				if err := group.Wait(); err != nil {
					return err
				}

				// The chunk's days the walk writes, in day order, as it moves each eval's first input
				// day. A day of a project that builds no row and can hold a stored row of a moved eval
				// (heldFrom) is written all the same, so its stale rows and members go.
				for index, day := range chunk {
					for p, project := range owners {
						own := built[index][p]
						first := r.first[walkKey{family: walkDelivery, project: project}]
						own.Rows = dropUnshipped(own.Rows, first, day)
						full := own
						if computed(day) && begunBy(first, day) {
							full = own.WithZeros(zerosFrom(ZeroDeliveryDay(next, project, day, names).Rows, first, day))
						}
						if from, held := heldFrom[project]; len(full.Rows) == 0 && (!held || day.Before(from)) {
							continue
						}
						pairProjects, pairDays = append(pairProjects, project), append(pairDays, day)
						written[projectDay{project: project, day: day}], spans[project] = true, spans[project].widen(day)
						for _, row := range full.Rows {
							if ids[row.EvalID] {
								rows = append(rows, row)
							}
						}
						for _, member := range full.Members {
							if slices.Contains(families, member.Family) {
								key := memberKey{project: project, family: member.Family, deployRepo: member.DeployRepo, rowKey: member.RowKey}
								members[key] = memberAt{day: day, repo: member.Repo, shippedBy: member.ShippedBy}
							}
						}
						r.noteBuilt(walkDelivery, project, day, own.Rows)
						r.report.days[builtDay{family: walkDelivery, project: project, day: day}] = true
					}
				}
			}
			return nil
		},
	); err != nil {
		return err
	}
	// The days build the CI family's push runs alone (dayOf): a PR-checks run's member stands as
	// stored, and none goes for not being built.
	if slices.Contains(families, KindCI) {
		pushes := map[string]bool{}
		for _, h := range histories {
			for _, runs := range h.pushesByDay {
				for _, run := range runs {
					pushes[RunMember(run.Repo, run.RunID)] = true
				}
			}
		}
		maps.DeleteFunc(stored, func(key memberKey, _ memberAt) bool {
			return key.family == KindCI && !pushes[key.rowKey]
		})
	}
	times.members = times.lap()

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the rewrite of a settings change: %w", err)
	}
	defer tx.Rollback(ctx)
	// Each statement planned for the arrays it is sent: a connection's cached statement can turn
	// generic after its fifth run.
	if _, err := tx.Exec(ctx, `set local plan_cache_mode = force_custom_plan`); err != nil {
		return fmt.Errorf("plan the rewrite of a settings change: %w", err)
	}
	if err := MarkSettingsRewrite(ctx, tx); err != nil {
		return err
	}
	if repoMoved {
		if err := markSettingsApplied(ctx, tx); err != nil {
			return err
		}
	}
	before, err := readRewriteCounts(ctx, tx)
	if err != nil {
		return err
	}
	times.bookkeeping = times.lap()
	if err := upsertDays(ctx, tx, rows, r.build); err != nil {
		return err
	}
	if err := deleteStaleSettingsDays(ctx, tx, pairProjects, pairDays, slugs, names, rows); err != nil {
		return err
	}
	times.dayRows = times.lap()
	if err := writeMovedMembers(ctx, tx, stored, members, written); err != nil {
		return err
	}
	times.memberWrites = times.lap()
	moved := make(map[string]map[string]daySpan, len(spans))
	for project, span := range spans {
		moved[project] = slugSpans(project, slugs, span)
	}
	if err := foldFiguresInTx(withFigureTimes(ctx, &times.figures), tx, r.build, moved); err != nil {
		return err
	}
	times.figureScope = times.lap() - times.figures.moved - times.figures.plan - times.figures.fold
	if err := recordBuiltUnder(ctx, tx, next); err != nil {
		return err
	}
	// A rebuild that is building may have staged rows the new settings move: it begins again,
	// under them (rebuild.go).
	if err := replaceRebuild(ctx, tx, "", "", nil); err != nil {
		return err
	}
	after, err := readRewriteCounts(ctx, tx)
	if err != nil {
		return err
	}
	times.bookkeeping += times.lap()
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the rewrite of a settings change: %w", err)
	}
	times.commit = times.lap()
	r.store.drainChanges(ctx)
	times.drain = times.lap()
	times.log(after.minus(before), len(days), repoMoved)
	return nil
}

// markSettingsApplied logs, in tx, that the day rows stand at the delivery settings once tx
// commits: one eval_days change of no eval (delivery.SettingsApplied), carrying the settings
// rewrite's mark, recorded in the change log's outbox as every eval change is
// (delivery_change_log_pending, migration 20261117100001). The refresher logs it in the
// transaction that records a deploy repository other than the one recorded with the rows, or the
// first record of the settings (syncSettings): the rewrite's where the change moved rows too, or
// otherwise the page's commit (commitPage). The Delivery page's live hub recomputes every evals
// view for it, at once: a read of any project's evals takes the deploy repository recorded with
// the rows (DeployRepoIn), so a view is recomputed when the repository and the rows both stand,
// and not at the change of the settings row, whose recompute would answer the new settings over
// rows the old ones built. A change that moves rows and not the deploy repository logs none: its
// rewrite's eval_days changes wake the views of the projects whose rows moved, and no other
// view's answer moves.
func markSettingsApplied(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `select delivery_change_log_pending($1, $2, null, null, null, null)`,
		delivery.ChangeKindEvalDays, delivery.EvalDaysWrittenBySettingsRewrite); err != nil {
		return fmt.Errorf("log that the day rows stand at the delivery settings: %w", err)
	}
	return nil
}

// rewriteTimes is how long each part of one rewrite of a delivery settings change took
// (rewriteSettings), read off a stopwatch it starts as it begins, so every moment of the rewrite
// falls in exactly one part, in the order it runs them:
//
//   - history, the read of the history and the repositories rewritten, the walk's first input
//     day, each project's first days and the walks' records side by side, the wall time of the
//     slowest of those reads;
//   - attribution, the serving record's read and the read of the pull requests' attribution
//     beside each repository's containment under each rule, the wall time of the slower;
//   - days, the days to build (movedDeliveryDays, lostFirstDays);
//   - members, the read of the stored members beside the build of those days, the wall time of
//     the slower;
//   - bookkeeping, the transaction begun and marked, the settings applied where the deploy
//     repository moved, the settings recorded, a building rebuild begun again and the two reads
//     of what the transaction wrote (rewriteCountsStatement);
//   - dayRows, the day rows upserted and the stale ones deleted; memberWrites, the members
//     written;
//   - the figures' refold (foldFiguresInTx): figureScope, its reads of the projects that read
//     the owner keys written, of each project's figure scope and of its evals, and whatever it
//     does between its timed parts, and figures, its reads of the moved days, of the rows it folds
//     and its fold and write (figureTimes);
//   - commit, and drain, the change log's outbox moved into the log once it has committed.
type rewriteTimes struct {
	stopwatch
	history, attribution, days, members time.Duration
	bookkeeping, dayRows, memberWrites  time.Duration
	figureScope                         time.Duration
	figures                             figureTimes
	commit, drain                       time.Duration
}

// log logs the rewrite's parts in the order it ran them, then what it wrote, how many days it
// built and whether the deploy repository moved with the rows: the line a measurement of a
// settings write's way to the open pages reads.
func (t *rewriteTimes) log(wrote rewriteCounts, days int, repoMoved bool) {
	slog.Info("dispatch: the eval day-row refresher rewrote what a delivery settings change moved",
		"history_ms", t.history.Milliseconds(), "attribution_ms", t.attribution.Milliseconds(), "days_ms", t.days.Milliseconds(),
		"members_and_build_ms", t.members.Milliseconds(), "day_rows_ms", t.dayRows.Milliseconds(),
		"member_writes_ms", t.memberWrites.Milliseconds(), "figure_scope_ms", t.figureScope.Milliseconds(),
		"moved_figure_days_ms", t.figures.moved.Milliseconds(), "figure_plan_ms", t.figures.plan.Milliseconds(),
		"figure_plan_reads", t.figures.reads, "figure_plan_rows", t.figures.rows, "fold_and_write_ms", t.figures.fold.Milliseconds(),
		"bookkeeping_ms", t.bookkeeping.Milliseconds(), "commit_ms", t.commit.Milliseconds(), "drain_ms", t.drain.Milliseconds(),
		"total_ms", t.total().Milliseconds(), "days_built", days, "day_rows_written", wrote.dayRows,
		"day_rows_deleted", wrote.dayRowsGone, "members_written", wrote.members, "evals_moved", wrote.evals,
		"figures_written", wrote.figures, "deploy_repo_moved", repoMoved)
}

// rewriteCounts is what a rewrite's transaction has written so far (rewriteCountsStatement): the
// day rows it inserted or updated and those it deleted, the members and the stored figures it
// wrote or deleted, and the evals its eval_days changes name.
type rewriteCounts struct {
	dayRows, dayRowsGone, members, figures, evals int64
}

// minus is what c holds beyond before.
func (c rewriteCounts) minus(before rewriteCounts) rewriteCounts {
	return rewriteCounts{
		dayRows: c.dayRows - before.dayRows, dayRowsGone: c.dayRowsGone - before.dayRowsGone, members: c.members - before.members,
		figures: c.figures - before.figures, evals: c.evals - before.evals,
	}
}

// readRewriteCounts reads, in tx, what tx has written so far (rewriteCountsStatement).
func readRewriteCounts(ctx context.Context, tx pgx.Tx) (rewriteCounts, error) {
	var c rewriteCounts
	if err := tx.QueryRow(ctx, rewriteCountsStatement).Scan(&c.dayRows, &c.dayRowsGone, &c.members, &c.figures, &c.evals); err != nil {
		return rewriteCounts{}, fmt.Errorf("read what the rewrite of a settings change wrote: %w", err)
	}
	return c, nil
}

// rewriteCountsStatement reads what the transaction it runs in has written so far: the day rows it
// inserted or updated and those it deleted, the members and the stored figures it inserted,
// updated or deleted, from the transaction's own statistics (pg_stat_get_xact_tuples_inserted,
// _updated and _deleted, which count each row a statement wrote, and no row an upsert's `where`
// left as it stood), and how many evals its eval_days changes name (dispatch.eval_days_pending,
// migration 20261117100014). The statistics also hold what earlier transactions of the connection
// wrote that its backend has not reported yet, which it does between transactions, so the rewrite
// reads them at its transaction's start and at its end and logs the difference.
const rewriteCountsStatement = `
	select pg_stat_get_xact_tuples_inserted('eval_days'::regclass) + pg_stat_get_xact_tuples_updated('eval_days'::regclass),
		pg_stat_get_xact_tuples_deleted('eval_days'::regclass),
		pg_stat_get_xact_tuples_inserted('eval_day_members'::regclass) + pg_stat_get_xact_tuples_updated('eval_day_members'::regclass)
			+ pg_stat_get_xact_tuples_deleted('eval_day_members'::regclass),
		pg_stat_get_xact_tuples_inserted('eval_range_figures'::regclass) + pg_stat_get_xact_tuples_updated('eval_range_figures'::regclass)
			+ pg_stat_get_xact_tuples_deleted('eval_range_figures'::regclass)
			+ pg_stat_get_xact_tuples_inserted('eval_day_figures'::regclass) + pg_stat_get_xact_tuples_updated('eval_day_figures'::regclass)
			+ pg_stat_get_xact_tuples_deleted('eval_day_figures'::regclass),
		(select count(*) from jsonb_object_keys(coalesce(nullif(current_setting('dispatch.eval_days_pending', true), ''), '{}')::jsonb))
`

// MarkSettingsRewrite marks tx as the refresher's rewrite of a delivery settings change, before its
// first write of a day row: it sets the transaction-local setting dispatch.eval_days_cause to
// delivery.EvalDaysWrittenBySettingsRewrite, which eval_days_change_log records as the run_kind of
// each eval_days change the transaction logs (migration 20261117100014). The Delivery page's live
// hub recomputes an evals view at once for those changes, as for a rebuild's and a publishing of
// the defaults' (delivery.EvalDaysImmediate): the rewrite's day rows arrive together, in one
// transaction, with no burst left for the quiet to gather, while the page has waited for the pass
// since the settings change. MarkRebuild sets the same setting to a rebuild's cause; no other
// writer of day rows sets it.
func MarkSettingsRewrite(ctx context.Context, tx pgx.Tx) error {
	return markCause(ctx, tx, delivery.EvalDaysWrittenBySettingsRewrite, "the rewrite of a settings change")
}

// movedDeliveryDays is, in day order, each day from start through today whose rows of the evals
// slugs names a change of the delivery settings can move, where the production rule's verdict on
// each repository's history goes from was to that of histories (containmentOf, historyOf) and the
// deploy cards from before to after. A repository's rows of a day are built from the pull requests
// merged that day, each with its first shipping apply, the deploy runs started that day, each with
// whether it reached production and the pull requests it ships first, and the push runs started
// that day with the jobs the cards follow (rewriteSettings' dayOf), and from each eval's first
// input day (dropUnshipped, zerosFrom). So, for each repository of histories:
//
//   - the start day of each deploy run whose apply moved: one under one rule and none under the
//     other, or another (its creation or its completion);
//   - for each pull request whose first shipping apply moved, its merge day, whose lead times and
//     member time it to its deploy, and the start day of each of the two applies' runs, whose
//     shipped sets, and so whose with_prs and projects, it moves;
//   - where slugs names a deploy card, each day whose push runs the two settings' cards build that
//     card's part of otherwise (measures.DeployJobsDay): every day with a push run, where the
//     card is made or unmade;
//   - from the earliest of those days on, the days a first day moved earlier reaches. One of them
//     can hold the first row of an eval of a project that held none on or before it, which moves
//     the eval's first input day to it (noteBuilt), and the whole rebuild then writes the eval's
//     zero rows, and a lead time's rows that count no deployed pull request, on each day after it
//     up to the old first day (zerosFrom, dropUnshipped), days whose inputs did not move. So for
//     each eval slugs names and each project that can count a row of it, every day from the
//     earliest moved day up to the day before the eval's stored first day (firsts), where that is
//     later, or through today, where it has none. Each of those evals' rows, a deploy card's
//     included, counts for the projects of the repository's pull requests, which alone its
//     deploys ship and its push runs, those deploys, count for.
//
// Every other day builds the rows it holds: its inputs, its evals' first days and what it reads
// of the settings are what its rows were built from.
func movedDeliveryDays(histories []repoHistory, was []containment, prs []delivery.DeliveryPullRequest, attribute attribution,
	before, after []measures.DeployJobCard, slugs, projects []string, firsts map[string]map[evalKey]time.Time, start, today time.Time) []time.Time {
	var cards []measures.DeployJobCardID
	for _, slug := range slugs {
		if card, ok := CardOf(slug); ok {
			cards = append(cards, card)
		}
	}
	moved := map[time.Time]bool{}
	for k, h := range histories {
		var span daySpan
		note := func(day time.Time) {
			if day.Before(start) || day.After(today) {
				return
			}
			moved[day], span = true, span.widen(day)
		}
		started := func(runID int64) time.Time { return measures.Midnight(h.runByID[runID].StartedAt) }
		for id, apply := range h.applied {
			if old, held := was[k].applied[id]; !held || old != apply {
				note(started(id))
			}
		}
		for id := range was[k].applied {
			if _, held := h.applied[id]; !held {
				note(started(id))
			}
		}
		for i := range prs {
			now, then := h.containing[i], was[k].containing[i]
			if now == nil && then == nil || now != nil && then != nil && *now == *then {
				continue
			}
			// Either apply ships the pull request, which has merged.
			note(measures.Midnight(*prs[i].MergedAt))
			for _, apply := range []*delivery.Apply{now, then} {
				if apply != nil {
					note(started(apply.RunID))
				}
			}
		}
		if len(cards) > 0 {
			for day, pushes := range h.pushesByDay {
				old, built := measures.DeployJobsDay(day, pushes, h.jobs, before), measures.DeployJobsDay(day, pushes, h.jobs, after)
				if slices.ContainsFunc(cards, func(card measures.DeployJobCardID) bool { return !reflect.DeepEqual(old[card], built[card]) }) {
					note(day)
				}
			}
		}
		if span.first.IsZero() {
			continue
		}
		counted := map[string]bool{}
		for i := range prs {
			if prs[i].Repo == h.repo {
				counted[attribute.of(prs[i]).project] = true
			}
		}
		// Each eval's and project's days run from the earliest moved day, so together they run
		// through the latest last day of any, reach (the zero time, before every day, where none).
		var reach time.Time
		for _, slug := range slugs {
			for _, project := range projects {
				if !counted[project] {
					continue
				}
				last := today
				if first, held := firsts[project][evalKey{eval: EvalID(project, slug), deployRepo: h.repo}]; held {
					if !span.first.Before(first) {
						continue
					}
					last = first.AddDate(0, 0, -1)
				}
				if last.After(reach) {
					reach = last
				}
			}
		}
		for day := span.first; !day.After(reach); day = day.AddDate(0, 0, 1) {
			moved[day] = true
		}
	}
	return slices.SortedFunc(maps.Keys(moved), time.Time.Compare)
}

// lostFirstDays is days, the days a change of the delivery settings can move (movedDeliveryDays),
// with every day after the first of them that was a series' first input day and holds no input of
// it under the new settings, through today. firsts is each project's stored first input days,
// which the delivery walk built (the walk has written today), r.first holding the same; ids
// the moved evals and names the repositories rewritten; dayOf builds each project's rows of a day
// under the new settings. Such a day is one the change can move, and building it alone would keep
// the series' zero rows from it on, or a lead time that ships no pull request (dropUnshipped), where
// a whole walk under the new settings begins the series at its next day with input and holds no
// row of it before. So the series' first input day is dropped from r.first, where each day built
// moves it again (noteBuilt), and every day from the lost one on is built, its stale rows deleted
// with the rest of the days written.
func (r *refresh) lostFirstDays(days []time.Time, owners []string, firsts map[string]map[evalKey]time.Time, ids map[string]bool, names []string,
	dayOf func(time.Time) []ProjectDay) []time.Time {
	building := make(map[time.Time]bool, len(days))
	for _, day := range days {
		building[day] = true
	}
	type candidate struct {
		project int
		key     evalKey
	}
	candidates := map[time.Time][]candidate{}
	for p, project := range owners {
		for key, since := range firsts[project] {
			if ids[key.eval] && slices.Contains(names, key.deployRepo) && building[since] {
				candidates[since] = append(candidates[since], candidate{project: p, key: key})
			}
		}
	}
	var lost *time.Time
	for day, keys := range candidates {
		built := dayOf(day)
		for _, one := range keys {
			if !inputKeys(built[one.project].Rows)[one.key] {
				delete(r.first[walkKey{family: walkDelivery, project: owners[one.project]}], one.key)
				if lost == nil || day.Before(*lost) {
					lost = &day
				}
			}
		}
	}
	if lost == nil {
		return days
	}
	for day := *lost; !day.After(r.today); day = day.AddDate(0, 0, 1) {
		if !building[day] {
			days = append(days, day)
		}
	}
	slices.SortFunc(days, time.Time.Compare)
	return days
}

// containment is a production rule's verdict on one repository's history as the deploy
// repository: the apply of each deploy run that reached production, by its run id, and each pull
// request's first shipping apply, by its index among the history's pull requests, nil for none.
type containment struct {
	applied    map[int64]delivery.Apply
	containing []*delivery.Apply
}

// containmentOf is containment over repo's deploy runs and their jobs, deploys, as the deploy
// repository, of prs, every population pull request, under production.
func containmentOf(repo string, deploys delivery.RunsWithJobs, prs []delivery.DeliveryPullRequest, production delivery.Production) containment {
	applies := delivery.ProductionApplies(deploys.Runs, deploys.Jobs, production).Applies
	judged := containment{applied: make(map[int64]delivery.Apply, len(applies)), containing: make([]*delivery.Apply, len(prs))}
	for _, apply := range applies {
		judged.applied[apply.RunID] = apply
	}
	shipping := delivery.NewShippingIndex(applies)
	for i := range prs {
		judged.containing[i] = shipping.Containing(prs[i], repo)
	}
	return judged
}

// repoHistory is containment over one repository's whole history as the deploy repository
// (historyOf): the production rule's verdict on its runs and its pull requests (containment), the
// start of its history, its deploy runs by id, the pull requests each apply ships first, and its
// deploy runs and push runs on main by the day they started, with their jobs.
type repoHistory struct {
	containment
	repo                      string
	since                     time.Time
	runByID                   map[int64]delivery.DeliveryRun
	shipped                   map[int64][]*delivery.DeliveryPullRequest
	deploysByDay, pushesByDay map[time.Time][]delivery.DeliveryRun
	jobs                      map[int64][]delivery.DeliveryRunJob
}

// historyOf is containmentOf over repo's deploy runs and their jobs, deploys, as the deploy
// repository, of prs, every population pull request, under production, with what a day's rows
// read of the history beside it: its deploy runs by id, the pull requests each apply ships first,
// and its deploy runs and push runs on main by the day they started, with their jobs.
func historyOf(repo keptRepo, deploys delivery.RunsWithJobs, prs []delivery.DeliveryPullRequest, production delivery.Production) repoHistory {
	h := repoHistory{
		containment: containmentOf(repo.name, deploys, prs, production),
		repo:        repo.name, since: repo.since, runByID: make(map[int64]delivery.DeliveryRun, len(deploys.Runs)),
		shipped:      map[int64][]*delivery.DeliveryPullRequest{},
		deploysByDay: map[time.Time][]delivery.DeliveryRun{}, pushesByDay: map[time.Time][]delivery.DeliveryRun{}, jobs: deploys.Jobs,
	}
	for i, apply := range h.containing {
		if apply != nil {
			h.shipped[apply.RunID] = append(h.shipped[apply.RunID], &prs[i])
		}
	}
	for _, run := range deploys.Runs {
		h.runByID[run.RunID] = run
		day := measures.Midnight(run.StartedAt)
		h.deploysByDay[day] = append(h.deploysByDay[day], run)
		if run.Event != nil && *run.Event == "push" {
			h.pushesByDay[day] = append(h.pushesByDay[day], run)
		}
	}
	return h
}

// prsByMergeDay is the index in prs of each pull request merged on each UTC day, by the day.
func prsByMergeDay(prs []delivery.DeliveryPullRequest) map[time.Time][]int {
	byDay := map[time.Time][]int{}
	for i := range prs {
		if prs[i].MergedAt != nil {
			day := measures.Midnight(*prs[i].MergedAt)
			byDay[day] = append(byDay[day], i)
		}
	}
	return byDay
}

// deliveryDay is the repository's DeliveryDay of day, as readDeliveryDays builds it, from the
// history's rows: of prs, every population pull request, those merged on the day (byDay,
// prsByMergeDay), each shipped by its first shipping apply; the deploy runs started on the day,
// each with whether it reached production and the pull requests it ships first; and the push runs
// on main started on the day with the jobs the cards follow; each attributed by attribute. It holds
// no PR-checks run, which the history does not hold.
func (h *repoHistory) deliveryDay(day time.Time, cards []measures.DeployJobCard, prs []delivery.DeliveryPullRequest, byDay map[time.Time][]int,
	attribute attribution) DeliveryDay {
	one := DeliveryDay{Day: day, deployRepo: h.repo, since: h.since, cards: cards}
	for _, i := range byDay[day] {
		// A partial row, whose opening is not known yet, as readDeliveryDays leaves it out.
		if prs[i].CreatedAt != nil {
			one.prs = append(one.prs, attribute.pullRequest(prs[i], h.containing[i], h.runByID))
		}
	}
	for _, run := range h.deploysByDay[day] {
		_, reached := h.applied[run.RunID]
		one.deploys = append(one.deploys, attribute.deploy(run, reached, h.shipped[run.RunID]))
	}
	one.pushes, one.pushJobs = attribute.pushRuns(h.pushesByDay[day], one.deploys), h.jobs
	return one
}

// historyOwners is every owner key (OwnerKey) the rewritten history counts a row for under the
// settings histories were built by, in key order: each pull request's whose attribution list is not
// constant or has no terminal (attribution.of), each deploy run's (attribution.deploy) and the
// push runs on main of a repository that are no deploy run of their day (attribution.pushRuns). A
// day the rewrite builds builds their rows as it builds a project's.
func historyOwners(histories []repoHistory, prs []delivery.DeliveryPullRequest, attribute attribution) []string {
	owners := map[string]bool{}
	for _, pr := range prs {
		if counts := attribute.of(pr); isOwner(counts.project) {
			owners[counts.project] = true
		}
	}
	for _, h := range histories {
		for _, runs := range h.deploysByDay {
			for _, run := range runs {
				_, reached := h.applied[run.RunID]
				for _, counts := range attribute.deploy(run, reached, h.shipped[run.RunID]).projects {
					if isOwner(counts.project) {
						owners[counts.project] = true
					}
				}
			}
		}
		if len(h.pushesByDay) > 0 {
			owners[OwnerKey(h.repo, nil, nil)] = true
		}
	}
	return slices.Sorted(maps.Keys(owners))
}

// deleteStaleSettingsDays is deleteStaleDays over many days at once: it deletes each of the
// computation's stored rows of the evals slugs names and of the deploy repositories deployRepos
// names, on each day of a project the rewrite writes (projects and days, pairwise), that held is
// not among, as deleteStaleDays deletes them. It sends each project's days with that project's
// evals and rows, the rows in statements of at most statementBytes of parameters as
// deleteStaleDays cuts them (staleRuns), each reading the stored rows of its own range of keys,
// and a project's days, every day of the history it rewrites, in cuts where they would pass their
// reserve (writtenReserve, writtenCuts), so the statements together delete what one would. Its
// statements are planned with merge joins off, and the setting then given back the value it held.
func deleteStaleSettingsDays(ctx context.Context, tx pgx.Tx, projects []string, days []time.Time, slugs, deployRepos []string, held []DayRow) error {
	if len(projects) == 0 || len(deployRepos) == 0 {
		return nil
	}
	written := map[string][]time.Time{}
	for i, project := range projects {
		written[project] = append(written[project], days[i])
	}
	rows := map[string][]*DayRow{}
	for i := range held {
		rows[held[i].ProjectKey] = append(rows[held[i].ProjectKey], &held[i])
	}
	// gone matches the stored rows to the days written by their project and day. A merge join
	// matches them by the project alone, which the primary key's order gives it for nothing, and
	// tests each row's day against every day written of its project; the planner takes it by a
	// thin margin of its cost, estimating that the join keeps a few rows where it keeps thousands.
	// A rewrite of 200 days of one project tested 434,218 pairs to keep its 2,182 rows (106 ms on
	// the pod), and the join written as a semi-join, `(project_key, day) in (select … from
	// written)`, is merged on the project as well where the planner estimates the scan of
	// eval_days low. With merge joins off it hashes the days written and matches both (12 to 25
	// ms). The setting is given back its value after the statements, so no other statement of the
	// rewrite's transaction plans without merge joins.
	// TestTheStaleRowsOfASettingsRewriteMatchTheWrittenDaysByProjectAndDay holds it to that plan.
	var mergeJoins string
	if err := tx.QueryRow(ctx, `select current_setting('enable_mergejoin')`).Scan(&mergeJoins); err != nil {
		return fmt.Errorf("read enable_mergejoin before the stale rows of a settings change: %w", err)
	}
	if _, err := tx.Exec(ctx, `select set_config('enable_mergejoin', 'off', true)`); err != nil {
		return fmt.Errorf("turn merge joins off for the stale rows of a settings change: %w", err)
	}
	for _, project := range slices.Sorted(maps.Keys(written)) {
		dates := written[project]
		writtenProjects := make([]string, len(dates))
		for i := range dates {
			writtenProjects[i] = project
		}
		limit := writtenReserve(writtenProjects, nil)
		evalIDs := make([]string, len(slugs))
		base := paramBytes(12, writerComputation) + limit
		for i, slug := range slugs {
			evalIDs[i] = EvalID(project, slug)
			base += 4 + len(evalIDs[i])
		}
		for _, repo := range deployRepos {
			base += 4 + len(repo)
		}
		cuts := writtenCuts(writtenProjects, dates, nil, nil, nil, limit)
		for _, run := range staleRuns(rows[project], base, dayRowKeyBytes) {
			heldProjects, heldEvals, heldDeployRepos, heldSeries, heldRepos, heldDays := dayRowKeys(run.rows)
			for _, cut := range cuts {
				if _, err := tx.Exec(ctx, deleteStaleSettingsDaysStatement, cut.projects, cut.dates, heldProjects, heldEvals, heldSeries, heldRepos, heldDays,
					evalIDs, writerComputation, heldDeployRepos, deployRepos, dayRowKey(run.from), dayRowKey(run.until)); err != nil {
					return fmt.Errorf("delete the day rows of %s a settings change leaves no longer held: %w", project, err)
				}
			}
		}
	}
	if _, err := tx.Exec(ctx, `select set_config('enable_mergejoin', $1, true)`, mergeJoins); err != nil {
		return fmt.Errorf("give enable_mergejoin back its value after the stale rows of a settings change: %w", err)
	}
	return nil
}

// deleteStaleSettingsDaysStatement is deleteStaleSettingsDays' statement: $1 and $2 the projects
// and days written (pairwise), $3, $4, $10, $5, $6 and $7 the rows they hold of the statement's
// range of keys (project, eval, deploy repository, series, repository, day), $8 the evals, $9
// writerComputation, $11 the deploy repositories, and $12 and $13 the range, from $12 up to $13 in
// the byte order of the keys (dayRowKey), null for no bound. gone is the stored rows' keys of the
// range except the given ones, the form every statement that finds what a day no longer holds
// takes (day_rows.go says why).
const deleteStaleSettingsDaysStatement = `
	with written as materialized (
		select * from unnest($1::text[], $2::date[]) as written(project_key, day)
	), gone as materialized (
		select e.project_key, e.eval_id, e.deploy_repo, e.series, e.repo, e.day
		from eval_days e
		join written on written.project_key = e.project_key and written.day = e.day
		where e.eval_id = any($8) and e.writer = $9 and e.deploy_repo = any($11)
			and ($12::text[] is null
				or (e.project_key collate "C", e.eval_id collate "C", e.deploy_repo collate "C", e.series collate "C", e.repo collate "C", e.day)
					>= ($12[1], $12[2], $12[3], $12[4], $12[5], $12[6]::date))
			and ($13::text[] is null
				or (e.project_key collate "C", e.eval_id collate "C", e.deploy_repo collate "C", e.series collate "C", e.repo collate "C", e.day)
					< ($13[1], $13[2], $13[3], $13[4], $13[5], $13[6]::date))
		except
		select * from unnest($3::text[], $4::text[], $10::text[], $5::text[], $6::text[], $7::date[])
			as held(project_key, eval_id, deploy_repo, series, repo, day)
	)
	delete from eval_days e
	using gone
	where e.project_key = gone.project_key and e.eval_id = gone.eval_id and e.writer = $9
		and e.deploy_repo = gone.deploy_repo and e.series = gone.series and e.repo = gone.repo and e.day = gone.day
`

// writeMovedMembers writes, in tx, the members built that moved from where stored holds them: each
// that stored holds on another day, under another repository or shipping run, or not at all,
// upserted; and each stored on a day written (by project) that built does not hold, deleted, on
// that day alone. A member whose row stands is sent to neither statement: a production rule's
// change moves the shipping run of some pull requests and the projects of some deploys, and leaves
// most of a history's members where they are. The members go in the byte order of their keys, in
// statements of at most statementBytes of parameters each (statementChunks), as the day rows do
// (writeDayRows): a repository's first runs bring a member of every pull request merged since.
func writeMovedMembers(ctx context.Context, tx pgx.Tx, stored, built map[memberKey]memberAt, written map[projectDay]bool) error {
	var moved, gone []memberKey
	for key, at := range built {
		if held, found := stored[key]; !found || !held.day.Equal(at.day) || held.repo != at.repo || held.shippedBy != at.shippedBy {
			moved = append(moved, key)
		}
	}
	for key, at := range stored {
		if _, kept := built[key]; !kept && written[projectDay{project: key.project, day: at.day}] {
			gone = append(gone, key)
		}
	}
	slices.SortFunc(moved, compareMemberKeys)
	slices.SortFunc(gone, compareMemberKeys)
	movedBytes := func(key *memberKey) int {
		at := built[*key]
		return 7*4 + len(key.project) + len(key.family) + len(key.deployRepo) + len(key.rowKey) + 8 + len(at.repo) + len(at.shippedBy)
	}
	for chunk := range statementChunks(moved, paramBytes(7), movedBytes) {
		count := len(chunk)
		projects, families, deployRepos, keys := make([]string, count), make([]string, count), make([]string, count), make([]string, count)
		repos, shippedBy, days := make([]string, count), make([]string, count), make([]time.Time, count)
		for i, key := range chunk {
			at := built[key]
			projects[i], families[i], deployRepos[i], keys[i] = key.project, key.family, key.deployRepo, key.rowKey
			days[i], repos[i], shippedBy[i] = at.day, at.repo, at.shippedBy
		}
		if _, err := tx.Exec(ctx, upsertMembersStatement, projects, families, deployRepos, keys, days, repos, shippedBy); err != nil {
			return fmt.Errorf("record the %d members that moved: %w", len(moved), err)
		}
	}
	goneBytes := func(key *memberKey) int {
		return 5*4 + len(key.project) + len(key.family) + len(key.deployRepo) + len(key.rowKey) + 8
	}
	for chunk := range statementChunks(gone, paramBytes(5), goneBytes) {
		count := len(chunk)
		projects, families, deployRepos, keys := make([]string, count), make([]string, count), make([]string, count), make([]string, count)
		days := make([]time.Time, count)
		for i, key := range chunk {
			projects[i], families[i], deployRepos[i], keys[i], days[i] = key.project, key.family, key.deployRepo, key.rowKey, stored[key].day
		}
		if _, err := tx.Exec(ctx, `
			delete from eval_day_members m
			using unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::date[]) as gone(project_key, family, deploy_repo, row_key, day)
			where m.project_key = gone.project_key and m.family = gone.family and m.deploy_repo = gone.deploy_repo
				and m.row_key = gone.row_key and m.day = gone.day
		`, projects, families, deployRepos, keys, days); err != nil {
			return fmt.Errorf("delete the %d members their days no longer hold: %w", len(gone), err)
		}
	}
	return nil
}

// compareMemberKeys orders members by the bytes of their keys: project, family, deploy repository
// and row key.
func compareMemberKeys(a, b memberKey) int {
	return cmp.Or(strings.Compare(a.project, b.project), strings.Compare(a.family, b.family),
		strings.Compare(a.deployRepo, b.deployRepo), strings.Compare(a.rowKey, b.rowKey))
}
