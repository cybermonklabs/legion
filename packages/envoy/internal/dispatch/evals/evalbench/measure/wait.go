package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// pollEvery is how often wait reads the consumers' state: more than the longest the refresher
// waits between two passes with work it knows of, one batch of a walk and the next
// (evals.RefresherGap, 10 s; a change's wake runs its pass as soon as the running one ends), so a
// cursor or a walk that stood still for a whole poll had a pass in which to move.
const pollEvery = 15 * time.Second

// atBase reports whether mode measures the base's server, main at the commit this head merges:
// base, or base-failure-day, the base over a history with a failure day added.
func atBase(mode string) bool {
	return mode == "base" || mode == "base-failure-day"
}

// lagPolls is how many polls old the heads a cursor is held to are: each cursor must stand at or
// past where its feed stood lagPolls polls before. The rows the Legion facts consumer writes log
// changes after its own cursor, so its cursor reaches a head only on its next pass, which the
// drain of those changes wakes as soon as the pass that wrote them ends; three polls is 45 s, many
// times a pass.
const lagPolls = 3

// heads is where each feed the consumers read stands.
type heads struct {
	events, observations, changes int64
}

// consumerState is what one poll read of the consumers and their feeds: whether the server runs
// the Legion facts consumer (legionFacts) and, where it does, its cursors and project marks; the
// keyword fill's cursor (search_text_versions_fill.event_id, 0 before the fill writes its row),
// nil where the server runs no keyword fill; and at this head the refresher's cursors (its two
// feeds' and the stored figures' own, by name), its walks, the serving records of its day rows
// (eval_days_serving), how many of its rebuilds are building, its day rows' builds, the projects
// that hold day rows and how far each project's stored figures are built (eval_figures_built).
// This head runs every one of these consumers. The base, main at the commit this head merges,
// runs the facts consumer and the fill where its catalog holds their tables, and the refresher
// where its catalog holds the refresher's cursors: a base before the eval registry runs none, and
// a base that is a commit of this branch before the code evals runs all but the regroup. The base's
// refresher is waited for (registry) only where the wait asks for it (options.baseRefresher, k17's
// control, whose paint reads the base's evals list): capture-base's GET /delivery/timeline reads no
// day row, so k22's capture at its base head waits for the facts and the fill alone (readConsumers),
// as it did before the code evals. A merge of main can change what the base runs. Where the
// database holds code_projects (codeBuilt), the regroup's pass at the server's start has built a
// project's day rows of the code evals when its row holds the digest of its groups. In mode k14 it
// reads the production signals' loop too (prodevents/signals): its three cursors, and each source
// repository whose GitHub deploy rows stand derived under another rule than its own, or under
// none (signalsOwed).
type consumerState struct {
	heads       heads
	legionFacts bool
	registry    bool
	facts       map[string]int64
	projects    []string
	computed    map[string]string
	days        map[string]int64
	walks       []walk
	serving     map[string]string
	rebuilding  int64
	builtBy     map[string]int64
	dayProjects []string
	built       map[string]time.Time
	codeBuilt   map[string]bool
	fill        *int64
	signals     map[string]int64
	signalsOwed []string
}

// walk is one row of eval_days_walks: builtBy is the build whose process last moved it.
type walk struct {
	family, project, builtBy string
	next                     *time.Time
}

// serverBuild is the build the server at url says it is (/healthz's commit), which its consumers
// record against every row they write.
func serverBuild(ctx context.Context, url string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/healthz", nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("read the server's health: %w", err)
	}
	defer response.Body.Close()
	var health struct {
		Commit string `json:"commit"`
	}
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		return "", fmt.Errorf("read the server's health: %w", err)
	}
	// /healthz answers the stamped commit, and an empty one where the binary carries none.
	if health.Commit == "" {
		return "", fmt.Errorf("the server answers no commit: a binary with no stamped commit draws a new build at each start, so its restart would rebuild every family; build it with -ldflags \"-X main.buildCommit=<commit>\"")
	}
	return health.Commit, nil
}

// wait polls until the consumers have caught up (consumerState.caughtUp) on two polls in a row,
// and fails once o.timeout has passed. Where the server runs the keyword fill (this head, and the
// base where its catalog holds the fill's table) it prints, at the first poll that finds the
// fill's cursor at the newest event, how long the fill took to get there: the time from the wait's
// start, which is the moment the server first served, to that poll, so it is long by at most one
// poll (pollEvery). evalbench's load writes the cursor at the newest loaded event, so the fill has
// only the events the server wrote since to follow.
func wait(ctx context.Context, r *report, o options) error {
	database, err := store.Open(ctx, o.database)
	if err != nil {
		return err
	}
	defer database.Pool.Close()
	build, err := serverBuild(ctx, o.url)
	if err != nil {
		return err
	}
	started := time.Now()
	var seen []heads
	held, filled := false, false
	for {
		state, err := readConsumers(ctx, database.Pool, o.mode, o.baseRefresher)
		if err != nil {
			return err
		}
		if !filled && state.filled() {
			filled = true
			r.result("keyword_fill mode=%s seconds=%.0f cursor=%d newest_event=%d poll=%s", o.mode, time.Since(started).Seconds(),
				*state.fill, state.heads.events, pollEvery)
		}
		var previous *heads
		if len(seen) >= lagPolls {
			previous = &seen[len(seen)-lagPolls]
		}
		missing := state.caughtUp(build, previous, o.project)
		fmt.Printf("WAIT %s after %s: %s\n", time.Now().UTC().Format(time.RFC3339), time.Since(started).Round(time.Second), state.summary(build))
		if len(missing) == 0 && held {
			r.result("catchup mode=%s build=%s seconds=%.0f %s", o.mode, build, time.Since(started).Seconds(), state.summary(build))
			return nil
		}
		held = len(missing) == 0
		if !held {
			fmt.Printf("WAIT still owed: %s\n", strings.Join(missing, "; "))
		}
		if time.Since(started) > o.timeout {
			return fmt.Errorf("after %s the consumers had not caught up: %s", o.timeout, strings.Join(missing, "; "))
		}
		seen = append(seen, state.heads)
		time.Sleep(pollEvery)
	}
}

// readConsumers reads the feeds' heads and the consumers' cursors, marks and walks. At the base it
// first reads which of the Legion facts consumer, the keyword fill and the refresher the server
// runs, by whether the catalog holds their cursor tables, and reads those alone, the refresher's
// only where baseRefresher asks for it.
func readConsumers(ctx context.Context, pool *store.Pool, mode string, baseRefresher bool) (consumerState, error) {
	state := consumerState{facts: map[string]int64{}, computed: map[string]string{}, days: map[string]int64{}, builtBy: map[string]int64{}}
	if err := pool.QueryRow(ctx, `
		select coalesce((select max(id) from events), 0), coalesce((select max(id) from legion_observations), 0),
		       coalesce((select max(seq) from delivery_changes), 0)
	`).Scan(&state.heads.events, &state.heads.observations, &state.heads.changes); err != nil {
		return consumerState{}, fmt.Errorf("read the feeds' heads: %w", err)
	}
	state.legionFacts, state.registry = true, true
	fill := true
	if atBase(mode) {
		if err := pool.QueryRow(ctx, `select to_regclass('issue_facts_cursor') is not null, to_regclass('search_text_versions_fill') is not null,
			to_regclass('eval_days_cursor') is not null`).Scan(&state.legionFacts, &fill, &state.registry); err != nil {
			return consumerState{}, fmt.Errorf("read which consumers the base runs: %w", err)
		}
		state.registry = state.registry && baseRefresher
	}
	rows, err := pool.Query(ctx, `select distinct project from legion_observations order by project`)
	if err != nil {
		return consumerState{}, fmt.Errorf("read the projects Legion works: %w", err)
	}
	if state.projects, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return consumerState{}, fmt.Errorf("read the projects Legion works: %w", err)
	}
	var project, build string
	if state.legionFacts {
		if err := collectPairs(ctx, pool, `select name, position from issue_facts_cursor`, state.facts); err != nil {
			return consumerState{}, err
		}
		marks, err := pool.Query(ctx, `select project_key, coalesce(computed_build, '') from issue_facts_computed`)
		if err != nil {
			return consumerState{}, fmt.Errorf("read the Legion facts' marks: %w", err)
		}
		if _, err := pgx.ForEachRow(marks, []any{&project, &build}, func() error {
			state.computed[project] = build
			return nil
		}); err != nil {
			return consumerState{}, fmt.Errorf("read the Legion facts' marks: %w", err)
		}
	}
	// Read after the heads, so a cursor at or past the events' head read above stands at or past
	// every event committed before that read.
	if fill {
		state.fill = new(int64)
		if err := pool.QueryRow(ctx, `select coalesce((select event_id from search_text_versions_fill where id = 1), 0)`).Scan(state.fill); err != nil {
			return consumerState{}, fmt.Errorf("read the keyword fill's cursor: %w", err)
		}
	}
	if !state.registry {
		return state, nil
	}
	if err := collectPairs(ctx, pool, `select name, position from eval_days_cursor`, state.days); err != nil {
		return consumerState{}, err
	}
	walks, err := pool.Query(ctx, `select family, project_key, built_by, next_day from eval_days_walks order by family, project_key`)
	if err != nil {
		return consumerState{}, fmt.Errorf("read the refresher's walks: %w", err)
	}
	var one walk
	if _, err := pgx.ForEachRow(walks, []any{&one.family, &one.project, &one.builtBy, &one.next}, func() error {
		state.walks = append(state.walks, one)
		return nil
	}); err != nil {
		return consumerState{}, fmt.Errorf("read the refresher's walks: %w", err)
	}
	serving, err := pool.Query(ctx, `select family || '/' || project_key, built_by from eval_days_serving`)
	if err != nil {
		return consumerState{}, fmt.Errorf("read what the day rows are served by: %w", err)
	}
	state.serving = map[string]string{}
	var family string
	if _, err := pgx.ForEachRow(serving, []any{&family, &build}, func() error {
		state.serving[family] = build
		return nil
	}); err != nil {
		return consumerState{}, fmt.Errorf("read what the day rows are served by: %w", err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from eval_rebuilds where state = 'building'`).Scan(&state.rebuilding); err != nil {
		return consumerState{}, fmt.Errorf("read the refresher's building rebuilds: %w", err)
	}
	counts, err := pool.Query(ctx, `select built_by, count(*) from eval_days group by built_by`)
	if err != nil {
		return consumerState{}, fmt.Errorf("read the day rows' builds: %w", err)
	}
	var count int64
	if _, err := pgx.ForEachRow(counts, []any{&build, &count}, func() error {
		state.builtBy[build] = count
		return nil
	}); err != nil {
		return consumerState{}, fmt.Errorf("read the day rows' builds: %w", err)
	}
	// One probe of eval_days' primary key for each project, where a distinct read of project_key
	// would scan the whole table on every poll, beside the count above.
	projects, err := pool.Query(ctx, `select key from projects p where exists (select 1 from eval_days d where d.project_key = p.key) order by key`)
	if err != nil {
		return consumerState{}, fmt.Errorf("read the projects that hold day rows: %w", err)
	}
	if state.dayProjects, err = pgx.CollectRows(projects, pgx.RowTo[string]); err != nil {
		return consumerState{}, fmt.Errorf("read the projects that hold day rows: %w", err)
	}
	built, err := pool.Query(ctx, `select project_key, through from eval_figures_built`)
	if err != nil {
		return consumerState{}, fmt.Errorf("read how far the stored figures are built: %w", err)
	}
	state.built = map[string]time.Time{}
	var through time.Time
	if _, err := pgx.ForEachRow(built, []any{&project, &through}, func() error {
		state.built[project] = through.UTC()
		return nil
	}); err != nil {
		return consumerState{}, fmt.Errorf("read how far the stored figures are built: %w", err)
	}
	// The code survival results' regroup: whether each project's day rows of the code evals are
	// built, where the database holds them.
	var coded bool
	if err := pool.QueryRow(ctx, `select to_regclass('code_projects') is not null`).Scan(&coded); err != nil {
		return consumerState{}, fmt.Errorf("read whether the database holds the code survival results: %w", err)
	}
	if coded {
		state.codeBuilt = map[string]bool{}
		regrouped, err := pool.Query(ctx, `select project_key from code_projects where groups_digest is not null`)
		if err != nil {
			return consumerState{}, fmt.Errorf("read the projects whose code day rows are built: %w", err)
		}
		keys, err := pgx.CollectRows(regrouped, pgx.RowTo[string])
		if err != nil {
			return consumerState{}, fmt.Errorf("read the projects whose code day rows are built: %w", err)
		}
		for _, key := range keys {
			state.codeBuilt[key] = true
		}
	}
	if mode == "k14" {
		if err := readSignals(ctx, pool, &state); err != nil {
			return consumerState{}, err
		}
	}
	return state, nil
}

// readSignals reads the production signals' loop's cursors and, for each source repository's
// rule (delivery.ReadSourceRules), whether its GitHub deploy rows stand derived under it
// (production_deploy_rules): its project and the two fields of the rule that decide a deploy.
func readSignals(ctx context.Context, pool *store.Pool, state *consumerState) error {
	state.signals = map[string]int64{}
	if err := collectPairs(ctx, pool, `select name, position from production_signal_cursors`, state.signals); err != nil {
		return err
	}
	rules, err := delivery.ReadSourceRules(ctx, pool)
	if err != nil {
		return err
	}
	for _, rule := range rules.All() {
		var project string
		var names []string
		var step *string
		err := pool.QueryRow(ctx, `select project_key, production_job_names, production_apply_step_name from production_deploy_rules where repo = $1`,
			strings.ToLower(rule.DeployRepo)).Scan(&project, &names, &step)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			state.signalsOwed = append(state.signalsOwed, fmt.Sprintf("the GitHub deploy rows of %s derived under its rule (none recorded)", rule.DeployRepo))
		case err != nil:
			return fmt.Errorf("read the rule the deploy rows of %s stand derived under: %w", rule.DeployRepo, err)
		case project != rule.Project || !slices.Equal(names, rule.ProductionJobNames) || !sameStep(step, rule.ProductionApplyStepName):
			state.signalsOwed = append(state.signalsOwed, fmt.Sprintf("the GitHub deploy rows of %s derived under its rule (derived under %s's %v)", rule.DeployRepo, project, names))
		}
	}
	return nil
}

// sameStep reports whether two optional apply step names are one.
func sameStep(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// collectPairs reads a name and a position a row into into.
func collectPairs(ctx context.Context, pool *store.Pool, query string, into map[string]int64) error {
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return fmt.Errorf("read %q: %w", query, err)
	}
	var name string
	var position int64
	_, err = pgx.ForEachRow(rows, []any{&name, &position}, func() error {
		into[name] = position
		return nil
	})
	return err
}

// caughtUp is what the consumers still owe, none once they have caught up: where the server runs
// the Legion facts consumer, every project Legion works read whole under build; each cursor at or
// past previous, where its feed stood lagPolls polls before (a feed the server's own sweeps keep
// writing is never still, so a cursor is held to a head it has had a whole sweep to reach), the
// stored figures' own cursor among them; where the server runs the keyword fill, its cursor at the
// newest event this poll read (filled); and at this head every walk done (walkDone), whichever
// build's process last moved it, the delivery rows and the Legion rows of every project Legion
// works served by build (eval_days_serving: its walk built them, or its rebuild's flip made them
// its own), no rebuild building, a day row, and every project that holds day rows with its stored
// figures built through the last end day of today (eval_figures_built, the mark the stored
// figures' read reads), since a first paint measured before that would read the day rows, which
// the stored figures are there to spare it. Where the database holds the code survival results
// (codeBuilt), the regroup's pass at the server's start must have built codeProject's day rows of
// the code evals, which its row of code_projects says by the digest of its groups. A day row keeps
// the build that last moved its values, so a release whose rules move no value leaves them as they
// were. Production serves with the fill caught up once its one backfill, at the deploy, is done,
// so evalbench's load writes the cursor as caught up with the loaded log, and this waits for the
// fill to follow the events the server wrote after the load: a paint measured while the fill still
// writes its batches would share the database with them.
func (s consumerState) caughtUp(build string, previous *heads, codeProject string) []string {
	var missing []string
	if previous == nil {
		missing = append(missing, fmt.Sprintf("%d polls, the heads the cursors are held to", lagPolls+1))
	}
	behind := func(consumer string, cursors map[string]int64, name string, head int64) {
		if previous != nil && cursors[name] < head {
			missing = append(missing, fmt.Sprintf("%s's %s cursor at %d, behind %d", consumer, name, cursors[name], head))
		}
	}
	if s.legionFacts {
		if len(s.projects) == 0 {
			missing = append(missing, "a project with Legion observations")
		}
		for _, project := range s.projects {
			if s.computed[project] != build {
				missing = append(missing, fmt.Sprintf("the Legion facts of %s read whole under %s (marked %q)", project, build, s.computed[project]))
			}
		}
		if previous != nil {
			behind("the Legion facts consumer", s.facts, "events", previous.events)
			behind("the Legion facts consumer", s.facts, "legion_observations", previous.observations)
			behind("the Legion facts consumer", s.facts, "delivery_changes", previous.changes)
		}
	}
	if s.fill != nil && !s.filled() {
		missing = append(missing, fmt.Sprintf("the keyword fill's cursor (search_text_versions_fill.event_id) at %d, behind the newest event %d",
			*s.fill, s.heads.events))
	}
	if !s.registry {
		return missing
	}
	if previous != nil {
		behind("the refresher", s.days, "events", previous.events)
		behind("the refresher", s.days, "delivery_changes", previous.changes)
		behind("the stored figures", s.days, "figures", previous.changes)
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	families := map[string]bool{}
	for _, one := range s.walks {
		families[one.family+"/"+one.project] = true
		if want := walkDone(one.family, today); one.next == nil || !one.next.Equal(want) {
			next := "none"
			if one.next != nil {
				next = one.next.Format(time.DateOnly)
			}
			missing = append(missing, fmt.Sprintf("the %s walk of %q through %s (it is at %s, last moved by %q)", one.family, one.project,
				want.AddDate(0, 0, -1).Format(time.DateOnly), next, one.builtBy))
		}
	}
	served := []string{"delivery/"}
	for _, project := range s.projects {
		served = append(served, "legion/"+project)
	}
	want := slices.Clone(served)
	for _, project := range s.dayProjects {
		want = append(want, "figures/"+project)
	}
	for _, family := range want {
		if !families[family] {
			missing = append(missing, "the "+family+" walk")
		}
	}
	for _, family := range served {
		if by, held := s.serving[family]; !held || by != build {
			missing = append(missing, fmt.Sprintf("the %s rows served by %s (served %t by %q)", family, build, held, by))
		}
	}
	if s.rebuilding > 0 {
		missing = append(missing, fmt.Sprintf("%d rebuilds building", s.rebuilding))
	}
	last := today.AddDate(0, 0, evals.FigureLead)
	for _, project := range s.dayProjects {
		if through, held := s.built[project]; !held || through.Before(last) {
			missing = append(missing, fmt.Sprintf("the stored figures of %s built through %s (built %t through %s)", project,
				last.Format(time.DateOnly), held, through.Format(time.DateOnly)))
		}
	}
	if len(s.builtBy) == 0 {
		missing = append(missing, "a day row")
	}
	if s.codeBuilt != nil && !s.codeBuilt[codeProject] {
		missing = append(missing, fmt.Sprintf("the regroup of %s's day rows of the code evals (its row of code_projects holds no digest)", codeProject))
	}
	if s.signals != nil {
		missing = append(missing, s.signalsOwed...)
		if previous != nil {
			behind("the production signals", s.signals, "deploys", previous.changes)
			behind("the production signals", s.signals, "checks", previous.events)
			behind("the production signals", s.signals, "incidents", previous.events)
		}
	}
	return missing
}

// walkDone is where a walk of family stands once it is done as of today: a walk of the day rows at
// tomorrow, once it has built today, and the stored figures' walk (family `figures`) at the day
// after the last end day it folds, evals.FigureLead days after today.
func walkDone(family string, today time.Time) time.Time {
	if family == "figures" {
		return today.AddDate(0, 0, evals.FigureLead+1)
	}
	return today.AddDate(0, 0, 1)
}

// filled is whether the keyword fill's cursor stands at or past the newest event the poll read,
// so every event committed before the poll has its keyword rows. It is false where the server runs
// no keyword fill.
func (s consumerState) filled() bool {
	return s.fill != nil && *s.fill >= s.heads.events
}

// summary is one line of the state.
func (s consumerState) summary(build string) string {
	line := fmt.Sprintf("heads events=%d observations=%d changes=%d legion_projects=%d", s.heads.events, s.heads.observations, s.heads.changes, len(s.projects))
	if s.legionFacts {
		marked := 0
		for _, project := range s.projects {
			if s.computed[project] == build {
				marked++
			}
		}
		line += fmt.Sprintf(" facts_cursors=%v read_whole=%d", s.facts, marked)
	} else {
		line += " legion_facts=none"
	}
	if s.fill != nil {
		line += fmt.Sprintf(" keyword_fill_cursor=%d", *s.fill)
	} else {
		line += " keyword_fill=none"
	}
	if s.days == nil || len(s.walks) == 0 && len(s.days) == 0 {
		return line
	}
	done := 0
	today := time.Now().UTC().Truncate(24 * time.Hour)
	for _, one := range s.walks {
		if one.next != nil && one.next.Equal(walkDone(one.family, today)) {
			done++
		}
	}
	served := 0
	for _, by := range s.serving {
		if by == build {
			served++
		}
	}
	var builds []string
	for other, count := range s.builtBy {
		builds = append(builds, fmt.Sprintf("%s=%d", other, count))
	}
	slices.Sort(builds)
	if s.codeBuilt != nil {
		line += fmt.Sprintf(" code_projects_built=%d", len(s.codeBuilt))
	}
	line += fmt.Sprintf(" refresher_cursors=%v walks=%d done=%d served=%d of %d rebuilding=%d figures_built=%d of %d day_rows_by_build=[%s]",
		s.days, len(s.walks), done, served, len(s.serving), s.rebuilding, len(s.built), len(s.dayProjects), strings.Join(builds, " "))
	if s.signals != nil {
		line += fmt.Sprintf(" signals_cursors=%v signals_rules_owed=%d", s.signals, len(s.signalsOwed))
	}
	return line
}
