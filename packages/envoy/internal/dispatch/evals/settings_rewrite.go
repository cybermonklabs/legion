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
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// settings_rewrite.go is the refresher's answer to a change of a deploy repository's rule
// (dispatch://INFRA-2916, R14; dispatch://INFRA-978, D1): one pass rewrites, in one transaction,
// the days of the delivery and CI evals whose rows of that repository the change moves, and of no
// other repository, and begins no walk. The rule each repository's stored delivery and CI rows
// stand built under, and the project whose source reads it, are recorded beside them
// (eval_days_rules, migration 20261123020640), and a page compares them with the rules it reads
// (delivery.DeployRules) before it writes a day (syncSettings): field by field, since a settings
// change in the log names no field, and since rows can stand built under other rules than the
// change before the page's cursor implies (a page whose pass read the settings after a later
// change committed).
//
// A change that moves no eval writes nothing and begins no walk. One of a setting no day row reads
// (a workflow path, the population rule's authors and excluded repositories, the roll-up job), or
// one that leaves the production rule and every deploy card following the same names with the same
// step (the order of the production names, a staging job renamed between two names the production
// rule follows), moves no eval by settingsMoves, and the pass reads nothing. A source that names
// another repository, or reads a moved default repository, keeps or stops keeping repositories
// (syncKept): a read chooses among them by the repositories recorded with the rows (deploy_repos.go,
// DeployReposIn). A production job name no run holds moves the rule's names but its verdict on no
// run and no card's part, so the rewrite reads the history and, once the delivery walk has written
// today, builds no day: it builds only the days whose rows the change can move
// (movedDeliveryDays). Until then it builds every day (rewriteSettings says why). Each of its
// writes sends a row, a member or a figure only where its value moved.

// settingsMoves is the delivery and CI evals (by slug) whose day rows of one repository a change of
// its rule from prev to next can move, and the families of eval_day_members it can move, read from
// the code that builds the rows (readDeliveryDays, DeliveryDay.Rows and the measures they call). It
// moves the rows of that repository alone, where it holds a run (rewriteSettings):
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
//   - Project, the delivery-on project whose source reads the repository: a deploy run that ships
//     no pull request counts for it and for no other project (attribution.deploy, INFRA-978 R12),
//     so deploy-frequency, deploy-run-success and the delivery family, and each deploy card either
//     rule makes, with the CI family, whose push runs count for the projects of their deploy.
//   - deploy_repo: none. It names the repository, which a change of the rule keeps.
//   - deploy_workflow_path, pr_checks_workflow_path, population_authors, excluded_repos and
//     pr_checks_rollup_job_name: none. The first four decide what the intake and the reconcile
//     store, each store a change of its own, and no read of a day names them; the roll-up job is
//     read at read time (the Pipeline's failing jobs, pr-ci's items) and by no day row.
func settingsMoves(prev, next delivery.DeliverySettings) (slugs, families []string) {
	moved, family := map[string]bool{}, map[string]bool{}
	before, after := cardParts(prev), cardParts(next)
	production := partOf(prev.ProductionJobNames, prev.ProductionApplyStepName) != partOf(next.ProductionJobNames, next.ProductionApplyStepName)
	if production {
		for _, slug := range leadTimeSlugs {
			moved[slug] = true
		}
	}
	if production || prev.Project != next.Project {
		moved[SlugDeployFrequency], moved[SlugDeployRunSuccess], family[KindDelivery] = true, true, true
		for card, slug := range cardSlugs {
			_, was := before[card]
			_, is := after[card]
			if was || is {
				moved[slug], family[KindCI] = true, true
			}
		}
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

// sameBuiltRule reports whether a and b agree on every field of a repository's rule
// eval_days_rules records, each exactly as stored: the four a day row reads, and the project whose
// source reads the repository, which a deploy that ships no pull request counts for and whose read
// selects the repository's rows (DeployReposIn).
func sameBuiltRule(a, b delivery.DeliverySettings) bool {
	return slices.Equal(a.ProductionJobNames, b.ProductionJobNames) &&
		sameName(a.ProductionApplyStepName, b.ProductionApplyStepName) && sameName(a.StagingE2EJobName, b.StagingE2EJobName) &&
		sameName(a.StagingApplyJobName, b.StagingApplyJobName) && a.Project == b.Project
}

// sameName reports whether two optional names are one.
func sameName(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// builtRule is one repository's row of eval_days_rules: the rule its stored delivery and CI rows
// stand built under (rule.DeployRepo the repository as its rows are keyed, rule.Project the project
// whose source reads it, and the four fields a day row reads), and the start of its history the
// rows are kept from (keptRepo.since), nil where it holds no delivery run and so no row.
type builtRule struct {
	rule  delivery.DeliverySettings
	since *time.Time
}

// readBuiltUnder reads the rules the stored delivery and CI rows stand built under (builtUnderIn).
func (s *Store) readBuiltUnder(ctx context.Context) (map[string]builtRule, error) {
	return builtUnderIn(ctx, s.database.Pool)
}

// builtUnderIn reads, in q, the rule each repository's stored delivery and CI rows stand built
// under (eval_days_rules), the fields recorded there alone (sameBuiltRule), by ruleKey: empty where
// none is recorded.
func builtUnderIn(ctx context.Context, q delivery.Querier) (map[string]builtRule, error) {
	rows, err := q.Query(ctx, `
		select repo, project_key, production_job_names, production_apply_step_name, staging_e2e_job_name,
			staging_apply_job_name, kept_since
		from eval_days_rules
	`)
	if err != nil {
		return nil, fmt.Errorf("read the rules the day rows stand built under: %w", err)
	}
	built := map[string]builtRule{}
	for rows.Next() {
		var one builtRule
		if err := rows.Scan(&one.rule.DeployRepo, &one.rule.Project, &one.rule.ProductionJobNames, &one.rule.ProductionApplyStepName,
			&one.rule.StagingE2EJobName, &one.rule.StagingApplyJobName, &one.since); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the rules the day rows stand built under: %w", err)
		}
		built[ruleKey(one.rule.DeployRepo)] = one
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the rules the day rows stand built under: %w", err)
	}
	return built, nil
}

// keptOf is the repositories built records rows of (those with a start), each with its start and
// its rule, in the byte order of their names: the set the rows stand built for (syncKept).
func keptOf(built map[string]builtRule) []keptRepo {
	var kept []keptRepo
	for _, one := range built {
		if one.since != nil {
			kept = append(kept, keptRepo{name: one.rule.DeployRepo, since: *one.since, rule: one.rule})
		}
	}
	slices.SortFunc(kept, func(a, b keptRepo) int { return cmp.Compare(a.name, b.name) })
	return kept
}

// BuiltSettingsIn is, read in q, the rule each of repos' stored delivery and CI rows answer under,
// in the order of repos: the rule of the source that reads it now (delivery.DeployRules over
// delivery.ListProjectSettings), with each field eval_days_rules records (sameBuiltRule: the four a
// day row reads and the source's project) as the refresher recorded it with the rows
// (recordRules), and the repository spelled as its rows are keyed; the recorded fields alone where
// no source reads it now; nil where neither holds it. Between a save of the settings and the
// refresher's commit that brings the rows to them, a reader of the evals then reads the rows and
// the rule they were built under together, the old one whole, and from that commit on the new one
// whole. Where no pass has recorded the repository's rule (a source the refresher has not reached
// yet), it is the saved one. It reads the settings and the record once, whatever the number of
// repos, and nothing where repos is empty.
func BuiltSettingsIn(ctx context.Context, q delivery.Querier, repos []string) ([]*delivery.DeliverySettings, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	projects, err := delivery.ListProjectSettings(ctx, q)
	if err != nil {
		return nil, err
	}
	built, err := builtUnderIn(ctx, q)
	if err != nil {
		return nil, err
	}
	rules := delivery.DeployRules(projects)
	answers := make([]*delivery.DeliverySettings, len(repos))
	for i, repo := range repos {
		var settings *delivery.DeliverySettings
		if at := slices.IndexFunc(rules, func(rule delivery.DeliverySettings) bool { return ruleKey(rule.DeployRepo) == ruleKey(repo) }); at >= 0 {
			settings = new(rules[at])
		}
		recorded, held := built[ruleKey(repo)]
		switch {
		case held && settings != nil:
			settings.ProductionJobNames, settings.ProductionApplyStepName = recorded.rule.ProductionJobNames, recorded.rule.ProductionApplyStepName
			settings.StagingE2EJobName, settings.StagingApplyJobName = recorded.rule.StagingE2EJobName, recorded.rule.StagingApplyJobName
			settings.Project, settings.DeployRepo = recorded.rule.Project, recorded.rule.DeployRepo
		case held:
			settings = &recorded.rule
		}
		answers[i] = settings
	}
	return answers, nil
}

// recordRules records, in q, built as the rules the stored delivery and CI rows stand built under
// (eval_days_rules): one row per repository, each written where a field moved, and every row of a
// repository built does not hold deleted. A pass's page writes it with its cursors (commitPage).
// Where it moves a rule a read of the rows answers under (BuiltSettingsIn: a field of a recorded
// rule that moved, or a rule deleted) it logs, in the same transaction, that the rules are
// recorded (logRulesRecorded), as the release before logs a record of its settings. A repository's
// first rule moves no such read, which answers the saved rule of a repository with none recorded,
// and neither does a move of where a repository's rows start alone (kept_since, syncKept), as the
// release before logs no record of the repositories kept: a page whose record moves only those
// logs nothing.
func recordRules(ctx context.Context, q pgx.Tx, built map[string]builtRule) error {
	repos := make([]string, 0, len(built))
	moved := false
	for _, key := range slices.Sorted(maps.Keys(built)) {
		one := built[key]
		repos = append(repos, one.rule.DeployRepo)
		// The statement answers a row only where it wrote one, and whether it moved a field of a rule
		// recorded before it (held is the row as it stood before the statement, none for a first one).
		var ruleMoved bool
		err := q.QueryRow(ctx, `
			with held as (
				select project_key, production_job_names, production_apply_step_name, staging_e2e_job_name, staging_apply_job_name
				from eval_days_rules where repo = $1
			)
			insert into eval_days_rules (repo, project_key, production_job_names, production_apply_step_name,
				staging_e2e_job_name, staging_apply_job_name, kept_since)
			values ($1, $2, $3, $4, $5, $6, $7::date)
			on conflict (repo) do update
			set project_key = excluded.project_key, production_job_names = excluded.production_job_names,
				production_apply_step_name = excluded.production_apply_step_name,
				staging_e2e_job_name = excluded.staging_e2e_job_name, staging_apply_job_name = excluded.staging_apply_job_name,
				kept_since = excluded.kept_since, updated_at = now()
			where (eval_days_rules.project_key, eval_days_rules.production_job_names, eval_days_rules.production_apply_step_name,
					eval_days_rules.staging_e2e_job_name, eval_days_rules.staging_apply_job_name, eval_days_rules.kept_since)
				is distinct from (excluded.project_key, excluded.production_job_names, excluded.production_apply_step_name,
					excluded.staging_e2e_job_name, excluded.staging_apply_job_name, excluded.kept_since)
			returning exists (
				select 1 from held
				where (held.project_key, held.production_job_names, held.production_apply_step_name, held.staging_e2e_job_name,
						held.staging_apply_job_name)
					is distinct from (eval_days_rules.project_key, eval_days_rules.production_job_names,
						eval_days_rules.production_apply_step_name, eval_days_rules.staging_e2e_job_name,
						eval_days_rules.staging_apply_job_name))
		`, one.rule.DeployRepo, one.rule.Project, nonNil(one.rule.ProductionJobNames), one.rule.ProductionApplyStepName,
			one.rule.StagingE2EJobName, one.rule.StagingApplyJobName, one.since).Scan(&ruleMoved)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("record the rule the day rows of %s stand built under: %w", one.rule.DeployRepo, err)
		}
		moved = moved || ruleMoved
	}
	deleted, err := q.Exec(ctx, `delete from eval_days_rules where repo <> all($1::text[])`, repos)
	if err != nil {
		return fmt.Errorf("delete the rules of the repositories the day rows no longer stand built for: %w", err)
	}
	if !moved && deleted.RowsAffected() == 0 {
		return nil
	}
	return logRulesRecorded(ctx, q)
}

// recordRule records, in q, rule as the rule the stored delivery and CI rows of its repository
// (rule.DeployRepo, as recorded) stand built under, its start left as it is, and logs in the same
// transaction that the rules are recorded (logRulesRecorded): the rewrite of that repository's
// change (rewriteSettings) writes it in its own transaction.
func recordRule(ctx context.Context, q pgx.Tx, rule delivery.DeliverySettings) error {
	if _, err := q.Exec(ctx, `
		update eval_days_rules
		set project_key = $2, production_job_names = $3, production_apply_step_name = $4,
			staging_e2e_job_name = $5, staging_apply_job_name = $6, updated_at = now()
		where repo = $1
	`, rule.DeployRepo, rule.Project, nonNil(rule.ProductionJobNames), rule.ProductionApplyStepName,
		rule.StagingE2EJobName, rule.StagingApplyJobName); err != nil {
		return fmt.Errorf("record the rule the day rows of %s stand built under: %w", rule.DeployRepo, err)
	}
	return logRulesRecorded(ctx, q)
}

// logRulesRecorded logs in q one eval_days change of no project that the rules the stored delivery
// and CI rows stand built under are recorded (delivery.SettingsRecorded: Ref
// delivery.SettingsRecordedRef, carrying the rewrite's mark), recorded in the change log's outbox
// as every eval change is (delivery_change_log_pending). The delivery routes and the live hub's
// delivery views answer under the recorded rules (BuiltSettingsIn), so the hub marks every delivery
// view for it and recomputes it at once (delivery.EvalDaysImmediate), whether or not the commit
// moved a day row; it marks no evals view, which names no project of it and is no SettingsApplied.
// Its callers are the rewrite's transaction (recordRule, rewriteSettings) and the page's commit
// that records rules whose difference moved no eval (recordRules, commitPage); a save of the same
// values records nothing (syncSettings), and so logs nothing.
func logRulesRecorded(ctx context.Context, q pgx.Tx) error {
	if _, err := q.Exec(ctx, `select delivery_change_log_pending($1, $2, null, $3, null, null)`,
		delivery.ChangeKindEvalDays, delivery.EvalDaysWrittenBySettingsRewrite, delivery.SettingsRecordedRef); err != nil {
		return fmt.Errorf("log that the rules the day rows stand built under are recorded: %w", err)
	}
	return nil
}

// syncSettings brings the stored delivery and CI rows of each source repository to its rule as the
// pass reads it (r.rules) before the page writes a day under them, on every page, so no row is
// written under a rule the rows do not stand built under:
//
//   - where a repository's recorded rule (eval_days_rules, read once a pass) agrees with it on every
//     field recorded there (sameBuiltRule), nothing;
//   - where they differ, it rewrites that repository's days of the evals the difference moves whose
//     rows it can move (settingsMoves, rewriteSettings), which records the rule in its own
//     transaction, or, where the difference moves no eval or the repository holds no deploy run,
//     has the page record it with its cursors (w.record);
//   - a repository no rule is recorded for is a new source: the page records its rule, and, where it
//     holds a delivery run, builds its rows as a repository whose first runs arrived (syncKept); a
//     recorded one no source reads any more goes from the record, and its rows go (syncKept);
//   - where no rule is recorded for any repository (a database no pass of this release has brought
//     to its rules, or one a bench reset), the rows' rules are unknown, so it has the page record
//     them with its cursors and, in the same transaction, begin a rebuild of every served family
//     (rebuild.go), which verifies every day under the rules it reads aside while the stored rows
//     keep serving, or, where no delivery rows are served yet, begin the delivery walk again, which
//     builds every day under them in the open.
//
// The commit that records the rules says that the rows stand at them (markSettingsApplied), which
// recomputes every evals view, where the record gains or loses a repository a read chooses among
// (a source repository: eval_days_rules, DeployReposIn), or where none was recorded and the page
// read a change of the settings row, and says nothing otherwise: the rewrite's eval_days changes,
// one of each eval whose rows it moved, recompute the evals views of their projects, and a
// difference that moves no row and no repository moves nothing an evals view reads. A move of a
// source alone moves no repository's rule, so it writes no row of a repository kept before and
// after it: the page records the new selection with its cursors, in the commit that says the rows
// stand at the settings, from which on a read chooses its rows (DeployReposIn).
func (r *refresh) syncSettings(ctx context.Context, w *pageWork) error {
	if !r.builtRead {
		built, err := r.store.readBuiltUnder(ctx)
		if err != nil {
			return err
		}
		r.builtUnder, r.keptUnder, r.builtRead = built, keptOf(built), true
	}
	if len(r.rules) == 0 && len(r.builtUnder) == 0 {
		return nil
	}
	if len(r.builtUnder) == 0 {
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
		for key, rule := range r.rules {
			r.builtUnder[key] = builtRule{rule: rule}
		}
		w.record, w.settingsApplied = true, w.settings
		return nil
	}
	kept, err := r.keptRepos(ctx)
	if err != nil {
		return err
	}
	for _, key := range slices.Sorted(maps.Keys(r.rules)) {
		next := r.rules[key]
		recorded, held := r.builtUnder[key]
		if !held {
			r.builtUnder[key], w.record, w.settingsApplied = builtRule{rule: next}, true, true
			continue
		}
		next.DeployRepo = recorded.rule.DeployRepo
		if sameBuiltRule(recorded.rule, next) {
			continue
		}
		r.builtUnder[key] = builtRule{rule: next, since: recorded.since}
		at := slices.IndexFunc(kept, func(repo keptRepo) bool { return ruleKey(repo.name) == key && repo.deploys })
		slugs, families := settingsMoves(recorded.rule, next)
		if len(slugs) == 0 || at < 0 {
			w.record = true
			continue
		}
		if err := r.rewriteSettings(ctx, recorded.rule, kept[at], slugs, families); err != nil {
			return err
		}
	}
	for key := range r.builtUnder {
		if _, current := r.rules[key]; !current {
			delete(r.builtUnder, key)
			w.record, w.settingsApplied = true, true
		}
	}
	return nil
}

// syncKept compares the repositories kept now (keptRepos, read once the page has read its changes:
// the source repositories that hold a delivery run, and the events repositories that hold a posted
// deploy) with those the delivery and CI rows stand built for (keptUnder, the recorded rules with a
// start), after syncSettings, so the record exists or the page's commit makes it. Where they differ
// the page records the new set with its cursors (w.record, each repository's start on its rule),
// and, where the stored delivery rows are whole through a day (eval_days_serving), builds in the
// same transaction the days of each repository that differs (w.kept, w.keptWas,
// refresh.writeNamed): a new repository's from its start on, the days between a moved one's two
// starts, and none of a gone one's, whose rows and members go. A repository becomes new where its
// first runs arrive or a source begins to read it, and gone where its runs go or no source reads it
// any more. Those are the rows and members a whole walk over the same runs holds, where the change
// that brought a repository's first runs names the days of its runs alone. A crash before the
// commit leaves the record as it was, so the next page finds the same difference.
func (r *refresh) syncKept(ctx context.Context, w *pageWork) error {
	if len(r.rules) == 0 && len(r.keptUnder) == 0 {
		return nil
	}
	kept, err := r.keptRepos(ctx)
	if err != nil {
		return err
	}
	if sameKept(kept, r.keptUnder) {
		return nil
	}
	w.kept, w.keptWas, w.record = kept, r.keptUnder, true
	r.keptUnder = kept
	for key, one := range r.builtUnder {
		one.since = nil
		r.builtUnder[key] = one
	}
	for _, repo := range kept {
		one := r.builtUnder[ruleKey(repo.name)]
		since := repo.since
		one.rule.DeployRepo, one.since = repo.name, &since
		r.builtUnder[ruleKey(repo.name)] = one
	}
	return nil
}

// sameKept reports whether a and b, each in the byte order of the names, are the same
// repositories with the same starts.
func sameKept(a, b []keptRepo) bool {
	return slices.EqualFunc(a, b, func(x, y keptRepo) bool { return x.name == y.name && x.since.Equal(y.since) })
}

// rewriteWorkers is the most goroutines rewriteSettings runs at once, each computing one
// repository's containment under one rule, building one day or folding one stored figure: the
// CPUs the process may use, up to this many. Every open evals view the rewrite moves waits for its
// commit (R14's 2 s), and none is recomputed before it.
const rewriteWorkers = 16

// memberParts is how many parts of the days rewriteSettings reads the stored members of, each
// on a connection of its own: a production rule's rewrite reads those of every day of the
// history, which read whole took longer than the build beside it.
const memberParts = 4

// rewriteHistoryReaders is how many statements read the jobs of the history's deploy runs at once,
// each of rewriteRunsPerRead runs, on a connection of its own (delivery.ReadDeployHistory).
const (
	rewriteHistoryReaders = 4
	rewriteRunsPerRead    = 500
)

// seriesOf is the zero rows of next, and those of prev of a series next holds none of: the
// series either settings make, each once.
func seriesOf(next, prev []DayRow) []DayRow {
	type series struct{ eval, deployRepo, name string }
	held := make(map[series]bool, len(next))
	for _, row := range next {
		held[series{row.EvalID, row.DeployRepo, row.Series}] = true
	}
	for _, row := range prev {
		if one := (series{row.EvalID, row.DeployRepo, row.Series}); !held[one] {
			held[one], next = true, append(next, row)
		}
	}
	return next
}

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

// rewriteSettings rewrites, under repo's rule as the pass reads it (repo.rule, refresh.rules), the
// days of the evals slugs names and the members of families whose rows of repo the change from
// prev, the rule those rows stand built under, can move, as the whole delivery walk under the new
// rule writes them, in one pass that begins no walk. It rewrites repo's rows alone, a kept
// repository (keptRepos) that holds a deploy run, which the production rule and the deploy cards
// judge; every other repository's rows stand built under their own rules, which the change does
// not move. A repository that holds no deploy run moves with no field of its rule, and its rows
// and members are left as they are:
//
//   - the reads, side by side: every deploy run on main of each repository rewritten with its jobs
//     of the names either rule judges by and each card the change moves follows under either
//     settings (delivery.ReadDeployHistory); every population pull request, read apart, and then
//     their attribution lists (readAttributionSideBySide), which no deploy run moves; the stored
//     rows of the moved evals of every project and owner key, by project and day
//     (readSettingsRows); the stored members of the families of the repositories rewritten, in
//     parts (readMembers, memberParts), none where it builds none, of the CI family the push runs'
//     alone: the days build no PR-checks run, whose project its pull request's issue decides
//     (attribution.check), which no setting moves, so its members stand as stored; the walk's
//     first input day (deliveryInputStart, over every kept repository), each project's first days
//     of each series either settings make (firstDays), the record of the walks (readWalks) and the
//     serving record (servingNext);
//   - containment over each repository's history as the deploy repository (historyOf): the new
//     production rule's verdict on each run (ProductionApplies, which judges a run by its own jobs
//     alone, and by its jobs of the judged names as by every job), each pull request's first
//     shipping apply (delivery.ShippingIndex), and each deploy's shipped set, the pull requests
//     whose first shipping apply it is (Deploys.ShippedBy's rule over every day at once); and,
//     where it builds the days the change can move alone (below), the old rule's verdict and first
//     shipping applies (containmentOf), the new rule's where the rule did not change. Each
//     repository's under each rule runs on a goroutine of its own, as many at once as
//     rewriteWorkers allows;
//   - the days to build, from the delivery walk's first day through today: those whose rows the
//     change can move (movedDeliveryDays), none for a change that moves no verdict and no card's
//     part, and every day after one of them that was a moved eval's first input day and holds no
//     input of it under the new settings (lostFirstDays); or every day, where the rows may not
//     stand built under prev: no delivery rows are served yet (eval_days_serving), or the delivery
//     walk is to begin or has days left (a family's first walk begins it), and a day it has not
//     reached holds rows of the settings before those its walk began under, which no comparison of
//     prev with the new settings finds;
//   - then, in one transaction, begun before the days are built: those days in day order a chunk
//     at a time (rewriteDays), each chunk's days built side by side, each repository's DeliveryDay
//     as readDeliveryDays builds it from those rows and each project's rows of the day
//     (deliveryRead.Rows); then, in day order, each project's day as the walk writes it
//     (writeDelivery, dropUnshipped, zerosFrom and noteBuilt), by the first input days the walk
//     builds by: those the build itself finds, day by day from the first, where it builds every
//     day, and otherwise the store's, which the walk built, but those lostFirstDays drops; with the
//     zero rows of each series from its first input day on where the stored delivery rows are
//     whole through the day (servingNext), and with none where they are not (writeNamed's rule);
//     and of the days written, those holding a row or that can hold a stored row of a moved eval
//     (on or after its first stored first input day), the rows of the evals slugs names and the
//     members of families. A day built that moves a series' first input day earlier adds the days
//     after it up to the old one, of that project alone (firstMoved). Each chunk's rows are
//     compared at once with the stored rows of its days (diffDay): each whose stored row holds
//     other values or is missing upserted (upsertDays, which cuts them into statements of at most
//     statementBytes of parameters), and each stored row of a moved eval of the repositories
//     rewritten on a day written that the day no longer holds deleted by its key (deleteDayRows),
//     and no row that stands sent, by a writer that runs beside the build of the next chunk; the
//     members of families of those repositories whose day, repository or shipping run moved, or
//     that are new, upserted, and those of the days written that the days no longer hold deleted,
//     and no other member sent, once the last chunk is built; the stored figures of every day whose
//     rows moved folded again, over each eval's span of the days whose rows moved or went
//     (foldFiguresInTx, whose quantiles read each rank across the days' sorted samples,
//     measures.SortedSamples); and the rule recorded as the one the repository's rows stand built
//     under, the repositories they stand built for left as they are (syncKept's).
//     Its eval_days changes are one per eval, as every transaction's are (migration
//     20261117100014), each under the project that reads the eval (an owner key's under each
//     project that reads the key), so the evals views of each project whose rows moved hear of the
//     whole change once, and each carries the rewrite's mark (MarkSettingsRewrite), so the view is
//     recomputed for them at once. It says no word that the rows stand at the settings
//     (markSettingsApplied): the repository it rewrites is one a read chooses before and after, so
//     no view of a project whose rows did not move hears of it. With no day to build it writes no
//     row, member or figure: the rule recorded and a building rebuild begun again (replaceRebuild)
//     alone.
//
// A day it does not build holds the rows it would build: its inputs, its rows' first days and the
// settings its rows read are what they were built from. It leaves eval_days_owed as it is: it
// writes the moved evals of a day alone, and a day owed is owed the rewrite of every eval of its
// families (writeOwed). It records each day it wrote in the pass's report, as the walk does, and
// the first days it moved (noteBuilt), but no day as done (refresh.done): a change of the same
// page that names a day writes that day whole after it. Once it has committed it logs what each of
// its parts took and what it wrote (rewriteTimes).
func (r *refresh) rewriteSettings(ctx context.Context, prev delivery.DeliverySettings, repo keptRepo, slugs, families []string) error {
	var times rewriteTimes
	times.start()
	next := repo.rule
	pool := r.store.database.Pool
	end := r.today.AddDate(0, 0, 1)
	prevCards, cards := measures.DeployCards(prev), measures.DeployCards(next)
	prevProduction, production := prev.Production(), next.Production()
	kept, err := r.keptRepos(ctx)
	if err != nil {
		return err
	}
	rewritten := []keptRepo{repo}
	names := deployRepoNames(rewritten)
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

	var deploys map[string]delivery.RunsWithJobs
	var prs []delivery.DeliveryPullRequest
	var attribute attribution
	var input, served *time.Time
	var walks map[walkKey]walkState
	// owners is the delivery family's owners (refresh.owners), and the owner keys the rewritten
	// history counts rows for that the store holds none of yet, added below once it is attributed.
	owners := slices.Clone(r.owners)
	var firsts map[string]map[evalKey]time.Time
	// The events repositories' posted deploys, which ship a pull request first where one finishes
	// before the repository's own deploy does (firstShipped).
	var events []keptRepo
	for _, one := range kept {
		if IsEventsRepo(one.name) {
			events = append(events, one)
		}
	}
	posted := make([]postedRecord, len(events))
	var storedByDay map[projectDay][]DayRow
	// memberDays is every day a stored member of the repositories rewritten can stand on: from the
	// first of their histories (keptRepo.since, before which none is kept) through today.
	var memberDays []time.Time
	if len(rewritten) > 0 {
		from := rewritten[0].since
		for _, repo := range rewritten {
			if repo.since.Before(from) {
				from = repo.since
			}
		}
		for day := from; !day.After(r.today); day = day.AddDate(0, 0, 1) {
			memberDays = append(memberDays, day)
		}
	}
	parts := min(memberParts, len(memberDays))
	partTimes := make([]time.Duration, parts)
	// The reads side by side, each on a connection of its own. The pull requests are read apart
	// from the deploy runs, so their attribution, which reads them alone, is read beside the deploy
	// runs and their jobs, the slowest read (delivery.ReadDeployHistory, which reads the jobs by
	// the runs' ids and not by a subselect over every run of the repository). The stored rows of
	// the moved evals are read of every day (readSettingsRows), which the days the rewrite writes
	// are among, whichever it finds to build, and the stored members of every day they can stand
	// on, in parts of the days (readMembers): a member stands on one day, so no two parts read it,
	// and one stored on a day the rewrite does not build is read and left alone. Only the refresher
	// writes the computation's rows and members, one pass at a time (RefresherLease), so what it
	// reads here is what its transaction then writes over, as a span's write reads it
	// (compareSpan).
	reads := []func(context.Context) error{
		func(ctx context.Context) (err error) {
			began := time.Now()
			deploys, err = delivery.ReadDeployHistory(ctx, pool, names, judged, rewriteHistoryReaders, rewriteRunsPerRead)
			times.deploysRead = time.Since(began)
			return err
		},
		func(ctx context.Context) (err error) {
			began := time.Now()
			if prs, err = delivery.ReadPullRequestsMergedBefore(ctx, pool, end); err != nil {
				return err
			}
			times.pullRequestsRead = time.Since(began)
			attribute, err = readAttributionSideBySide(ctx, r.store, prs)
			times.attributionRead = time.Since(began) - times.pullRequestsRead
			return err
		},
		func(ctx context.Context) (err error) {
			began := time.Now()
			storedByDay, err = r.store.readSettingsRows(ctx, owners, slugs, names)
			times.rowsRead = time.Since(began)
			return err
		},
		func(ctx context.Context) (err error) {
			input, err = r.store.deliveryInputStart(ctx, kept)
			return err
		},
		func(ctx context.Context) (err error) {
			// Each series either settings make: one the new settings unmake (a deploy card) holds
			// rows the rewrite deletes from its first day on (lostFirstDays).
			began := time.Now()
			zeros := make(map[string][]DayRow, len(owners))
			rules := keptRules(kept)
			// The rules before the change: the rewritten repository's under prev.
			prevRules := slices.Clone(rules)
			if at := slices.IndexFunc(prevRules, func(rule delivery.DeliverySettings) bool { return ruleKey(rule.DeployRepo) == ruleKey(repo.name) }); at >= 0 {
				prevRules[at] = prev
			}
			for _, project := range owners {
				zeros[project] = seriesOf(ZeroDeliveryDay(project, r.today, rules).Rows, ZeroDeliveryDay(project, r.today, prevRules).Rows)
			}
			firsts, err = r.store.firstDays(ctx, zeros, nil)
			times.firstDaysRead = time.Since(began)
			return err
		},
		func(ctx context.Context) (err error) {
			walks, err = r.store.readWalks(ctx)
			return err
		},
		func(ctx context.Context) (err error) {
			served, err = r.servingNext(ctx, walkDelivery, "")
			return err
		},
		func(ctx context.Context) (err error) {
			for i, one := range events {
				if posted[i], err = postedDeploys(ctx, pool, one.rule.Project, time.Time{}); err != nil {
					return err
				}
			}
			return nil
		},
	}
	stored := map[memberKey]memberAt{}
	var gathering sync.Mutex
	for i := range parts {
		part := memberDays[i*len(memberDays)/parts : (i+1)*len(memberDays)/parts]
		reads = append(reads, func(ctx context.Context) error {
			began := time.Now()
			held, err := r.store.readMembers(ctx, owners, families, names, part)
			if err != nil {
				return err
			}
			partTimes[i] = time.Since(began)
			gathering.Lock()
			defer gathering.Unlock()
			maps.Copy(stored, held)
			return nil
		})
	}
	if err := store.ReadSideBySide(ctx, reads...); err != nil {
		return fmt.Errorf("read the delivery history for a settings change: %w", err)
	}
	times.reads = times.lap()
	if parts > 0 {
		times.membersRead = slices.Max(partTimes)
	}
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
	// heldFrom is the first day each project can hold a stored row of a moved eval of the
	// repository on: the earliest first input day the store holds of one (every row of a series
	// lies on or after it). A day built from it on is written though it builds no row, so its stale
	// rows go.
	heldFrom := map[string]time.Time{}
	for project, first := range firsts {
		for key, since := range first {
			if from, held := heldFrom[project]; ids[key.eval] && key.deployRepo == repo.name && (!held || since.Before(from)) {
				heldFrom[project] = since
			}
		}
	}
	prsByDay := prsByMergeDay(prs)
	start := earliestOf(input, earliest)
	walk, found := walks[walkKey{family: walkDelivery}]
	// The days the change can move alone are built where the stored delivery rows are whole and the
	// delivery walk has written today (no walk begun again rebuilds them), and they need the old
	// rule's verdict beside the new one's. A change of the rule's project moves which project each
	// deploy that ships no pull request counts for (attribution.deploy), which no verdict shows, so
	// it builds every day.
	movedOnly := start != nil && served != nil && found && walk.next != nil && walk.next.After(r.today) && prev.Project == next.Project
	// The first input days the rows are built by are the whole walk's. Where every day is built,
	// they are those the build itself finds, day by day from the first (noteBuilt): a series of the
	// repository begins on the first day it holds input under the new rule, whatever rows the store
	// held of it, and every other repository's series keeps its own. Where the days the change can
	// move are built alone, the walk has written today, and the store's first days are the ones it
	// built (refresh.firstOf), but a first input day the change leaves with no input (lost, below).
	for _, project := range owners {
		first := firsts[project]
		if !movedOnly {
			first = maps.Clone(first)
			if first == nil {
				first = map[evalKey]time.Time{}
			}
			maps.DeleteFunc(first, func(key evalKey, _ time.Time) bool { return key.deployRepo == repo.name })
		}
		r.first[walkKey{family: walkDelivery, project: project}] = first
	}
	// Those first input days are the rewrite's own, and hold none of a repository it does not
	// rewrite where it builds every day: once it ends, a later write of the pass builds by the
	// store's (storedFirst), which hold the rewrite's from its commit on, so each owner's goes
	// from the pass's (r.first), the owner keys the history adds below among them. A day written
	// by the pass's own first days, emptied here, would leave out the zero rows of every series
	// of a repository the rewrite did not build, and its write would delete them (R12).
	defer func() {
		for _, project := range owners {
			delete(r.first, walkKey{family: walkDelivery, project: project})
		}
	}()
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
	// Each repository's containment under each rule on a goroutine of its own, each writing its own
	// slot.
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(min(runtime.GOMAXPROCS(0), rewriteWorkers))
	for k, repo := range rewritten {
		group.Go(func() error {
			if err := groupCtx.Err(); err != nil {
				return err
			}
			histories[k] = historyOf(repo, deploys[repo.name], prs)
			return nil
		})
		if movedOnly && !sameRule {
			group.Go(func() error {
				if err := groupCtx.Err(); err != nil {
					return err
				}
				was[k] = containmentOf(repo.name, deploys[repo.name], prs, prevProduction)
				return nil
			})
		}
	}
	if err := group.Wait(); err != nil {
		return err
	}
	times.containment = times.lap()
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
		days = movedDeliveryDays(histories, was, prs, prevCards, cards, slugs, *start, r.today)
	} else if start != nil {
		for day := *start; !day.After(r.today); day = day.AddDate(0, 0, 1) {
			days = append(days, day)
		}
	}
	// dayOf is each project's rows of day: the repository's DeliveryDay of it, as readDeliveryDays
	// builds it, from the history's rows under the new rule (repoHistory.deliveryDay), each pull
	// request's lead times kept where its first deploy is the repository's (firstShipped).
	dayOf := func(day time.Time) []ProjectDay {
		read := deliveryRead{Day: day, rules: []delivery.DeliverySettings{next}, days: make([]DeliveryDay, len(histories))}
		var merged []delivery.DeliveryPullRequest
		for _, i := range prsByDay[day] {
			merged = append(merged, prs[i])
		}
		for k := range histories {
			compared := []DeliveryDay{histories[k].deliveryDay(day, prs, prsByDay, attribute)}
			for i, eventsRepo := range events {
				compared = append(compared, postedDayAs(day, merged, posted[i], eventsRepo))
			}
			firstShipped(compared)
			read.days[k] = compared[0]
		}
		rows := make([]ProjectDay, len(owners))
		for p, project := range owners {
			rows[p] = read.Rows(project)
		}
		return rows
	}
	if movedOnly {
		began := time.Now()
		days = r.lostFirstDays(days, owners, firsts, ids, names, dayOf)
		times.lost = time.Since(began)
	}
	times.days = times.lap()

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
	members := map[memberKey]memberAt{}
	// written is each project's day whose members the rewrite writes.
	written := map[projectDay]bool{}
	// zeros is each project's zero rows of a day of no input under the new settings
	// (ZeroDeliveryDay), of today: a day's are the same rows on that day (zeroRowsOn).
	zeros := map[string][]DayRow{}
	// moved is, by project and eval, the span of the days whose rows moved or went: what the stored
	// figures are folded again over.
	moved := map[string]map[string]daySpan{}
	note := func(row *DayRow) {
		if moved[row.ProjectKey] == nil {
			moved[row.ProjectKey] = map[string]daySpan{}
		}
		moved[row.ProjectKey][row.EvalID] = moved[row.ProjectKey][row.EvalID].widen(measures.Midnight(row.Day))
	}

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
	before, err := readRewriteCounts(ctx, tx)
	if err != nil {
		return err
	}
	times.bookkeeping = times.lap()

	// Each chunk of days is built, compared with the stored rows of its days (diffDay) and handed to
	// a goroutine that writes what moved in the transaction, beside the build of the next chunk: a
	// transaction that waits for the whole build before its first write stands idle for it. Only
	// the refresher writes the computation's rows, one pass at a time (RefresherLease), so the
	// chunks' writes, in day order and not in the key order of the whole, wait for no other writer.
	// The members are written after the last chunk, and then the figures.
	type dayWrite struct{ changed, gone []DayRow }
	writes, wrote := make(chan dayWrite, 4), make(chan error, 1)
	go func() {
		var err error
		for one := range writes {
			if err != nil {
				continue
			}
			began := time.Now()
			if err = upsertDays(ctx, tx, one.changed, r.build); err == nil {
				err = deleteDayRows(ctx, tx, one.gone)
			}
			times.dayRows += time.Since(began)
		}
		wrote <- err
	}()
	builtDays, builtRows, sent := len(days), 0, 0
	buildErr := func() error {
		// pending is the days left to build, in day order; reached is, of each day a moved first
		// day reaches that is no other day to build, the projects whose series it begins.
		pending, scheduled := slices.Clone(days), make(map[time.Time]bool, len(days))
		for _, day := range days {
			scheduled[day] = true
		}
		reached := map[time.Time]map[string]bool{}
		for len(pending) > 0 {
			chunk := pending[:min(rewriteDays, len(pending))]
			pending = pending[len(chunk):]
			// The chunk's days built side by side.
			built := make([][]ProjectDay, len(chunk))
			group, groupCtx := errgroup.WithContext(ctx)
			group.SetLimit(min(runtime.GOMAXPROCS(0), rewriteWorkers))
			chunkBegan := time.Now()
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
			times.buildDays += time.Since(chunkBegan)

			// The chunk's days the walk writes, in day order, as it moves each eval's first input
			// day. A day of a project that builds no row and can hold a stored row of a moved eval
			// (heldFrom) is written all the same, so its stale rows and members go. A day a moved
			// first day reaches is built once the day that moved it is, and can be after a later
			// moved day: its inputs being what they were, it moves no first day, and so no row of
			// a day after it.
			var added []time.Time
			var write dayWrite
			for index, day := range chunk {
				only := reached[day]
				for p, project := range owners {
					if only != nil && !only[project] {
						continue
					}
					own := built[index][p]
					first := r.first[walkKey{family: walkDelivery, project: project}]
					own.Rows = dropUnshipped(own.Rows, first, day)
					if movedOnly {
						reach := firstMoved(own.Rows, first, ids, day, r.today)
						for one := reach.first; !reach.first.IsZero() && !one.After(reach.last); one = one.AddDate(0, 0, 1) {
							if !scheduled[one] {
								scheduled[one], reached[one] = true, map[string]bool{}
								added = append(added, one)
							}
							if reached[one] != nil {
								reached[one][project] = true
							}
						}
					}
					full := own
					if computed(day) && begunBy(first, day) {
						if zeros[project] == nil {
							zeros[project] = ZeroDeliveryDay(project, r.today, []delivery.DeliverySettings{next}).Rows
						}
						full = own.WithZeros(zeroRowsOn(zerosFrom(zeros[project], first, day), day))
					}
					if from, held := heldFrom[project]; len(full.Rows) == 0 && (!held || day.Before(from)) {
						continue
					}
					var rows []DayRow
					for _, row := range full.Rows {
						if ids[row.EvalID] {
							rows = append(rows, row)
						}
					}
					diffBegan := time.Now()
					changed, gone := diffDay(storedByDay[projectDay{project: project, day: day}], rows)
					times.diff += time.Since(diffBegan)
					builtRows += len(rows)
					for i := range changed {
						note(&changed[i])
					}
					for i := range gone {
						note(&gone[i])
					}
					write.changed, write.gone = append(write.changed, changed...), append(write.gone, gone...)
					if only == nil {
						written[projectDay{project: project, day: day}] = true
						for _, member := range full.Members {
							if slices.Contains(families, member.Family) {
								key := memberKey{project: project, family: member.Family, deployRepo: member.DeployRepo, rowKey: member.RowKey}
								members[key] = memberAt{day: day, repo: member.Repo, shippedBy: member.ShippedBy}
							}
						}
					}
					r.noteBuilt(walkDelivery, project, day, own.Rows)
					r.report.days[builtDay{family: walkDelivery, project: project, day: day}] = true
				}
			}
			if len(write.changed)+len(write.gone) > 0 {
				sent += len(write.changed)
				select {
				case writes <- write:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if len(added) > 0 {
				builtDays += len(added)
				pending = slices.SortedFunc(slices.Values(slices.Concat(pending, added)), time.Time.Compare)
			}
		}
		return nil
	}()
	close(writes)
	if err := <-wrote; err != nil {
		return err
	}
	if buildErr != nil {
		return buildErr
	}
	times.build = times.lap()
	if err := writeMovedMembers(ctx, tx, stored, members, written); err != nil {
		return err
	}
	times.memberWrites = times.lap()
	// The rewrite folds on as many goroutines as it builds days on: the views that wait for its
	// commit wait for its fold, and no view reads beside it what the fold takes CPUs from.
	if err := foldFiguresInTx(withFoldWorkers(withFigureTimes(ctx, &times.figures), rewriteWorkers), tx, r.build, moved); err != nil {
		return err
	}
	times.figureScope = times.lap() - times.figures.moved - times.figures.plan - times.figures.fold
	if err := recordRule(ctx, tx, next); err != nil {
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
	times.log(after.minus(before), builtDays, builtRows, sent)
	return nil
}

// markSettingsApplied logs, in tx, that the day rows stand at the delivery settings once tx
// commits: one eval_days change of no eval (delivery.SettingsApplied), carrying the settings
// rewrite's mark, recorded in the change log's outbox as every eval change is
// (delivery_change_log_pending, migration 20261117100001). The refresher logs it in the page's
// commit (commitPage) that records a set of source repositories other than the one recorded with
// the rows, or the first record of the rules (syncSettings), never in a rewrite of one repository's
// rows (rewriteSettings). The Delivery page's live hub recomputes every evals view for it, at once:
// a read of any project's evals chooses among the repositories recorded with the rows
// (DeployReposIn), so a view is recomputed when the repositories and the rows both stand, and not
// at the change of the settings row, whose recompute would answer the new settings over rows the
// old ones built. A change that moves rows and not the repositories logs none: its rewrite's
// eval_days changes wake the views of the projects whose rows moved, and no other view's answer
// moves.
func markSettingsApplied(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `select delivery_change_log_pending($1, $2, null, null, null, null)`,
		delivery.ChangeKindEvalDays, delivery.EvalDaysWrittenBySettingsRewrite); err != nil {
		return fmt.Errorf("log that the day rows stand at the delivery settings: %w", err)
	}
	return nil
}

// rewriteTimes is how long each part of one rewrite of a delivery settings change took
// (rewriteSettings), read off a stopwatch it starts as it begins, so every moment of the rewrite
// falls in exactly one part, in the order it runs them, and how long some reads within a part
// took of their own:
//
//   - reads, the history's deploy runs and their jobs (deploysRead), its pull requests
//     (pullRequestsRead) and then their attribution (attributionRead), each project's first days
//     (firstDaysRead), the stored rows of the moved evals (rowsRead), the stored members, read in
//     parts (membersRead, the slowest part's), the walk's first input day, the walks' records, the
//     serving record and the events repositories' posted deploys, side by side: the wall time of
//     the slowest;
//   - containment, each repository's containment under each rule;
//   - days, the owner keys the history adds and the days to build (movedDeliveryDays, and
//     lostFirstDays of its own, lost);
//   - bookkeeping, the transaction begun and marked, the rule recorded, a building rebuild begun
//     again and the two reads of what the transaction wrote (rewriteCountsStatement);
//   - build, the days built chunk by chunk (buildDays, the chunks' days built side by side), each
//     chunk compared with the stored rows of its days (diff) and its day rows sent upserted and
//     the gone ones deleted by a writer beside the build of the next chunk (dayRows, the
//     writer's own time): the wall time of the whole, which ends with the last write;
//   - memberWrites, the members written;
//   - the figures' refold (foldFiguresInTx): figureScope, its reads of the projects that read
//     the owner keys written, of each project's figure scope and of its evals, and whatever it
//     does between its timed parts, and figures, its reads of the moved days, of the rows it folds
//     and its fold and write, and of that the statements' own time (figureTimes);
//   - commit, and drain, the change log's outbox moved into the log once it has committed.
type rewriteTimes struct {
	stopwatch
	reads, containment, days, build                time.Duration
	deploysRead, pullRequestsRead, attributionRead time.Duration
	firstDaysRead, rowsRead, lost                  time.Duration
	membersRead, buildDays, diff, dayRows          time.Duration
	bookkeeping, memberWrites                      time.Duration
	figureScope                                    time.Duration
	figures                                        figureTimes
	commit, drain                                  time.Duration
}

// log logs the rewrite's parts in the order it ran them, each followed by the reads of its own
// within it, then what it wrote, how many days it built and how many day rows it built and sent:
// the line a measurement of a settings write's way to the open pages reads.
func (t *rewriteTimes) log(wrote rewriteCounts, days, built, sent int) {
	slog.Info("dispatch: the eval day-row refresher rewrote what a delivery settings change moved",
		"reads_ms", t.reads.Milliseconds(), "deploys_read_ms", t.deploysRead.Milliseconds(),
		"pull_requests_read_ms", t.pullRequestsRead.Milliseconds(), "attribution_read_ms", t.attributionRead.Milliseconds(),
		"first_days_read_ms", t.firstDaysRead.Milliseconds(), "rows_read_ms", t.rowsRead.Milliseconds(),
		"containment_ms", t.containment.Milliseconds(), "days_ms", t.days.Milliseconds(), "lost_ms", t.lost.Milliseconds(),
		"members_read_ms", t.membersRead.Milliseconds(),
		"build_ms", t.build.Milliseconds(), "build_days_ms", t.buildDays.Milliseconds(), "diff_ms", t.diff.Milliseconds(),
		"day_rows_ms", t.dayRows.Milliseconds(), "member_writes_ms", t.memberWrites.Milliseconds(),
		"figure_scope_ms", t.figureScope.Milliseconds(),
		"moved_figure_days_ms", t.figures.moved.Milliseconds(), "figure_plan_ms", t.figures.plan.Milliseconds(),
		"figure_plan_reads", t.figures.reads, "figure_plan_rows", t.figures.rows, "fold_and_write_ms", t.figures.fold.Milliseconds(),
		"figure_writes_ms", t.figures.writes.Milliseconds(), "bookkeeping_ms", t.bookkeeping.Milliseconds(),
		"commit_ms", t.commit.Milliseconds(), "drain_ms", t.drain.Milliseconds(), "total_ms", t.total().Milliseconds(),
		"days_built", days, "day_rows_built", built, "day_rows_sent", sent, "day_rows_written", wrote.dayRows,
		"day_rows_deleted", wrote.dayRowsGone, "members_written", wrote.members, "evals_moved", wrote.evals,
		"figures_written", wrote.figures)
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
//     card is made or unmade.
//
// Every other day builds the rows it holds from its inputs, its evals' first days and what it
// reads of the settings, which are what its rows were built from, but a day after a moved one that
// moves a series' first input day earlier (noteBuilt), up to the old first day: such a day holds
// the series' zero row, and a lead time's row that counts no deployed pull request, from the new
// first day on (zerosFrom, dropUnshipped). Which series a moved day begins earlier is known once
// it is built, so rewriteSettings adds those days as it builds (firstMoved), and a series that
// begins on no moved day adds none.
func movedDeliveryDays(histories []repoHistory, was []containment, prs []delivery.DeliveryPullRequest,
	before, after []measures.DeployJobCard, slugs []string, start, today time.Time) []time.Time {
	var cards []measures.DeployJobCardID
	for _, slug := range slugs {
		if card, ok := CardOf(slug); ok {
			cards = append(cards, card)
		}
	}
	moved := map[time.Time]bool{}
	for k, h := range histories {
		note := func(day time.Time) {
			if !day.Before(start) && !day.After(today) {
				moved[day] = true
			}
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
	}
	return slices.SortedFunc(maps.Keys(moved), time.Time.Compare)
}

// firstMoved is the days a built day of a project moves to write besides itself: for each of
// rows, the project's rows of day as the walk keeps them (dropUnshipped), that is an
// all-repositories row of a moved eval (ids) whose series' first input day, of the pass's first
// days (first, before noteBuilt moves it), is after day or none, every day after day up to the day
// before that first day, or through today where it has none. Those days' inputs did not move, and
// they hold the series' zero rows and its unshipped lead times from day on.
func firstMoved(rows []DayRow, first map[evalKey]time.Time, ids map[string]bool, day, today time.Time) daySpan {
	var reach daySpan
	for i := range rows {
		if rows[i].Repo != "" || !ids[rows[i].EvalID] {
			continue
		}
		last := today
		if since, held := first[keyOf(rows[i])]; held {
			if !day.Before(since) {
				continue
			}
			last = since.AddDate(0, 0, -1)
		}
		if last.After(day) {
			reach = reach.widen(day.AddDate(0, 0, 1)).widen(last)
		}
	}
	return reach
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
// project and the deploy cards of its rule, the start of its history, its deploy runs by id, the
// pull requests each apply ships first, and its deploy runs and push runs on main by the day they
// started, with their jobs.
type repoHistory struct {
	containment
	repo, project             string
	cards                     []measures.DeployJobCard
	since                     time.Time
	runByID                   map[int64]delivery.DeliveryRun
	shipped                   map[int64][]*delivery.DeliveryPullRequest
	deploysByDay, pushesByDay map[time.Time][]delivery.DeliveryRun
	jobs                      map[int64][]delivery.DeliveryRunJob
}

// historyOf is containmentOf over repo's deploy runs and their jobs, deploys, as the deploy
// repository, of prs, every population pull request, under its rule's production rule, with what a
// day's rows read of the history beside it: its deploy runs by id, the pull requests each apply
// ships first, and its deploy runs and push runs on main by the day they started, with their jobs.
func historyOf(repo keptRepo, deploys delivery.RunsWithJobs, prs []delivery.DeliveryPullRequest) repoHistory {
	h := repoHistory{
		containment: containmentOf(repo.name, deploys, prs, repo.rule.Production()),
		repo:        repo.name, project: repo.rule.Project, cards: measures.DeployCards(repo.rule), since: repo.since,
		runByID:      make(map[int64]delivery.DeliveryRun, len(deploys.Runs)),
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

// deliveryDay is the repository's DeliveryDay of day under its rule, as readDeliveryDays builds it,
// from the history's rows: of prs, every population pull request, those merged on the day (byDay,
// prsByMergeDay), each shipped by its first shipping apply; the deploy runs started on the day,
// each with whether it reached production and the pull requests it ships first, one that ships
// none counted for the rule's project (attribution.deploy); and the push runs on main started on
// the day with the jobs the rule's cards follow; each attributed by attribute. It holds no
// PR-checks run, which the history does not hold.
func (h *repoHistory) deliveryDay(day time.Time, prs []delivery.DeliveryPullRequest, byDay map[time.Time][]int, attribute attribution) DeliveryDay {
	one := DeliveryDay{Day: day, deployRepo: h.repo, since: h.since, cards: h.cards}
	for _, i := range byDay[day] {
		// A partial row, whose opening is not known yet, as readDeliveryDays leaves it out.
		if prs[i].CreatedAt != nil {
			one.prs = append(one.prs, attribute.pullRequest(prs[i], h.containing[i], h.runByID))
		}
	}
	for _, run := range h.deploysByDay[day] {
		_, reached := h.applied[run.RunID]
		one.deploys = append(one.deploys, attribute.deploy(run, reached, h.shipped[run.RunID], h.project))
	}
	one.pushes, one.pushJobs = attribute.pushRuns(h.pushesByDay[day], one.deploys), h.jobs
	return one
}

// historyOwners is every owner key (OwnerKey) the rewritten history counts a row for under the
// settings histories were built by, in key order: each pull request's whose attribution list is not
// constant or has no terminal (attribution.of), each deploy run's (attribution.deploy, under the
// project of its repository's rule, which a deploy that ships no pull request counts for) and the
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
				for _, counts := range attribute.deploy(run, reached, h.shipped[run.RunID], h.project).projects {
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

// attributionParts is how many parts readAttributionSideBySide cuts a history's pull requests
// into, each read on a connection of its own.
const attributionParts = 8

// readAttributionSideBySide is readAttribution of prs, every population pull request of the
// history, which holds no PR-checks run: prs cut into attributionParts parts of about one size,
// each part's attribution read side by side with the others', and their lists together. A pull
// request's list is made of its own candidates or its own issue alone (readAttribution), so a
// part's lists are the lists one read of every pull request answers of that part's.
func readAttributionSideBySide(ctx context.Context, s *Store, prs []delivery.DeliveryPullRequest) (attribution, error) {
	parts := min(attributionParts, len(prs))
	if parts < 2 {
		return readAttribution(ctx, s, prs, nil)
	}
	read := make([]attribution, parts)
	reads := make([]func(context.Context) error, parts)
	for i := range parts {
		part := prs[i*len(prs)/parts : (i+1)*len(prs)/parts]
		reads[i] = func(ctx context.Context) (err error) {
			read[i], err = readAttribution(ctx, s, part, nil)
			return err
		}
	}
	if err := store.ReadSideBySide(ctx, reads...); err != nil {
		return attribution{}, err
	}
	lists := make(map[string]attributionList, len(prs))
	for _, one := range read {
		maps.Copy(lists, one.lists)
	}
	return attribution{lists: lists}, nil
}

// settingsRowParts is how many parts of its pairs of project and eval readSettingsRows reads side
// by side, each on a connection of its own.
const settingsRowParts = 4

// readSettingsRows reads the computation's stored rows of the evals slugs names of each of
// projects (every project and owner key the rewrite builds), of the deploy repositories
// deployRepos, of every day, by project and day: every row a rewrite of a settings change can
// write over or delete (rewriteSettings), which compares them with the rows it builds of each day
// (diffDay). Each pair of a project and an eval is one range of eval_days' primary key (a lateral
// read of each, so no plan reads the table whole), the pairs dealt in turn into settingsRowParts
// parts read side by side, each in statements of at most statementBytes (statementChunks): the
// projects grow with the owner keys.
func (s *Store) readSettingsRows(ctx context.Context, projects, slugs, deployRepos []string) (map[projectDay][]DayRow, error) {
	type pair struct{ project, evalID string }
	if len(projects) == 0 || len(slugs) == 0 || len(deployRepos) == 0 {
		return nil, nil
	}
	parts := make([][]pair, min(settingsRowParts, len(projects)*len(slugs)))
	index := 0
	for _, project := range projects {
		for _, slug := range slugs {
			parts[index%len(parts)] = append(parts[index%len(parts)], pair{project: project, evalID: EvalID(project, slug)})
			index++
		}
	}
	base := paramBytes(3, writerComputation)
	for i := range deployRepos {
		base += textBytes(&deployRepos[i])
	}
	size := func(one *pair) int { return 2*4 + len(one.project) + len(one.evalID) }
	read := make([][]DayRow, len(parts))
	reads := make([]func(context.Context) error, len(parts))
	for i, part := range parts {
		reads[i] = func(ctx context.Context) error {
			for chunk := range statementChunks(part, base, size) {
				named, evalIDs := make([]string, len(chunk)), make([]string, len(chunk))
				for j, one := range chunk {
					named[j], evalIDs[j] = one.project, one.evalID
				}
				rows, err := s.database.Pool.Query(ctx, `
					select d.* from unnest($1::text[], $2::text[]) as given(project_key, eval_id)
					cross join lateral (
						select `+dayColumns+` from eval_days
						where project_key = given.project_key and eval_id = given.eval_id and writer = $3 and deploy_repo = any($4)
						offset 0
					) d
				`, named, evalIDs, writerComputation, deployRepos)
				if err != nil {
					return fmt.Errorf("read the stored rows a settings change can move: %w", err)
				}
				if read[i], err = pgx.AppendRows(read[i], rows, func(row pgx.CollectableRow) (DayRow, error) { return scanDay(row) }); err != nil {
					return fmt.Errorf("read the stored rows a settings change can move: %w", err)
				}
			}
			return nil
		}
	}
	if err := store.ReadSideBySide(ctx, reads...); err != nil {
		return nil, err
	}
	byDay := map[projectDay][]DayRow{}
	for _, part := range read {
		for _, row := range part {
			at := projectDay{project: row.ProjectKey, day: measures.Midnight(row.Day)}
			byDay[at] = append(byDay[at], row)
		}
	}
	return byDay, nil
}

// diffDay is what built, the rows of the moved evals one project's day builds, moves of stored,
// the stored rows of the same project, evals and day (readSettingsRows), each by its key: each
// built row whose stored row holds other values or is missing (changed), and each stored row no
// built row holds (gone). It is diffDayRows over one day, by a key that is no string.
func diffDay(stored, built []DayRow) (changed, gone []DayRow) {
	type key struct{ eval, deployRepo, series, repo string }
	held := make(map[key]*DayRow, len(stored))
	for i := range stored {
		held[key{stored[i].EvalID, stored[i].DeployRepo, stored[i].Series, stored[i].Repo}] = &stored[i]
	}
	for i := range built {
		at := key{built[i].EvalID, built[i].DeployRepo, built[i].Series, built[i].Repo}
		was, found := held[at]
		delete(held, at)
		if !found || !sameDayValues(&built[i], was) {
			changed = append(changed, built[i])
		}
	}
	for _, row := range held {
		gone = append(gone, *row)
	}
	return changed, gone
}

// zeroRowsOn is zeros, zero rows of a day of no input (ZeroDeliveryDay), on day: each the same
// row with day as its day, which is all a zero row holds of its day. The rows share their counts,
// which nothing that writes or compares a row moves.
func zeroRowsOn(zeros []DayRow, day time.Time) []DayRow {
	day = measures.Midnight(day)
	on := make([]DayRow, len(zeros))
	for i, zero := range zeros {
		on[i] = zero
		on[i].Day = day
	}
	return on
}

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
