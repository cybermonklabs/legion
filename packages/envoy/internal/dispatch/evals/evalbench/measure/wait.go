package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// pollEvery is how often wait reads the consumers' state: more than the refresher's least gap
// between two passes (evals.RefresherGap, 10 s), so a cursor that stood still for a whole poll
// had a pass in which to move.
const pollEvery = 15 * time.Second

// lagPolls is how many polls old the heads a cursor is held to are: each cursor must stand at or
// past where its feed stood lagPolls polls before. The Legion facts consumer sweeps every open
// issue once every legionfacts.SweepEvery (30 s) and the rows it writes log changes after its own
// cursor, so its cursor reaches a head only on its next sweep; three polls is 45 s, a sweep and a
// poll.
const lagPolls = 3

// heads is where each feed the consumers read stands.
type heads struct {
	events, observations, changes int64
}

// consumerState is what one poll read of the consumers and their feeds.
type consumerState struct {
	heads    heads
	facts    map[string]int64
	projects []string
	computed map[string]string
	days     map[string]int64
	walks    []walk
	builtBy  map[string]int64
}

// walk is one row of eval_days_walks.
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
// and fails once o.timeout has passed.
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
	held := false
	for {
		state, err := readConsumers(ctx, database.Pool, o.mode)
		if err != nil {
			return err
		}
		var previous *heads
		if len(seen) >= lagPolls {
			previous = &seen[len(seen)-lagPolls]
		}
		missing := state.caughtUp(o.mode, build, previous)
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

// readConsumers reads the feeds' heads and the consumers' cursors, marks and walks.
func readConsumers(ctx context.Context, pool *store.Pool, mode string) (consumerState, error) {
	state := consumerState{facts: map[string]int64{}, computed: map[string]string{}, days: map[string]int64{}, builtBy: map[string]int64{}}
	if err := pool.QueryRow(ctx, `
		select coalesce((select max(id) from events), 0), coalesce((select max(id) from legion_observations), 0),
		       coalesce((select max(seq) from delivery_changes), 0)
	`).Scan(&state.heads.events, &state.heads.observations, &state.heads.changes); err != nil {
		return consumerState{}, fmt.Errorf("read the feeds' heads: %w", err)
	}
	if err := collectPairs(ctx, pool, `select name, position from issue_facts_cursor`, state.facts); err != nil {
		return consumerState{}, err
	}
	rows, err := pool.Query(ctx, `select distinct project from legion_observations order by project`)
	if err != nil {
		return consumerState{}, fmt.Errorf("read the projects Legion works: %w", err)
	}
	if state.projects, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return consumerState{}, fmt.Errorf("read the projects Legion works: %w", err)
	}
	marks, err := pool.Query(ctx, `select project_key, coalesce(computed_build, '') from issue_facts_computed`)
	if err != nil {
		return consumerState{}, fmt.Errorf("read the Legion facts' marks: %w", err)
	}
	var project, build string
	if _, err := pgx.ForEachRow(marks, []any{&project, &build}, func() error {
		state.computed[project] = build
		return nil
	}); err != nil {
		return consumerState{}, fmt.Errorf("read the Legion facts' marks: %w", err)
	}
	if mode == "base" {
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
	return state, nil
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

// caughtUp is what the consumers still owe, none once they have caught up: every project Legion
// works read whole under build; each cursor at or past previous, where its feed stood lagPolls
// polls before (a feed the server's own sweeps keep writing is never still, so a cursor is held
// to a head it has had a whole sweep to reach); and at this head every walk done through today
// under build, and every day row written by build.
func (s consumerState) caughtUp(mode, build string, previous *heads) []string {
	var missing []string
	if previous == nil {
		missing = append(missing, fmt.Sprintf("%d polls, the heads the cursors are held to", lagPolls+1))
	}
	if len(s.projects) == 0 {
		missing = append(missing, "a project with Legion observations")
	}
	for _, project := range s.projects {
		if s.computed[project] != build {
			missing = append(missing, fmt.Sprintf("the Legion facts of %s read whole under %s (marked %q)", project, build, s.computed[project]))
		}
	}
	behind := func(consumer string, cursors map[string]int64, name string, head int64) {
		if previous != nil && cursors[name] < head {
			missing = append(missing, fmt.Sprintf("%s's %s cursor at %d, behind %d", consumer, name, cursors[name], head))
		}
	}
	if previous != nil {
		behind("the Legion facts consumer", s.facts, "events", previous.events)
		behind("the Legion facts consumer", s.facts, "legion_observations", previous.observations)
		behind("the Legion facts consumer", s.facts, "delivery_changes", previous.changes)
	}
	if mode == "base" {
		return missing
	}
	if previous != nil {
		behind("the refresher", s.days, "events", previous.events)
		behind("the refresher", s.days, "delivery_changes", previous.changes)
	}
	tomorrow := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	families := map[string]bool{}
	for _, one := range s.walks {
		families[one.family+"/"+one.project] = true
		if one.builtBy != build || one.next == nil || !one.next.Equal(tomorrow) {
			next := "none"
			if one.next != nil {
				next = one.next.Format(time.DateOnly)
			}
			missing = append(missing, fmt.Sprintf("the %s walk of %q under %s through %s (it is %q at %s)", one.family, one.project, build, tomorrow.Format(time.DateOnly), one.builtBy, next))
		}
	}
	want := []string{"delivery/"}
	for _, project := range s.projects {
		want = append(want, "legion/"+project)
	}
	for _, family := range want {
		if !families[family] {
			missing = append(missing, "the "+family+" walk")
		}
	}
	for other, count := range s.builtBy {
		if other != build {
			missing = append(missing, fmt.Sprintf("%d day rows built by %q", count, other))
		}
	}
	if s.builtBy[build] == 0 {
		missing = append(missing, "a day row")
	}
	return missing
}

// summary is one line of the state.
func (s consumerState) summary(build string) string {
	marked := 0
	for _, project := range s.projects {
		if s.computed[project] == build {
			marked++
		}
	}
	line := fmt.Sprintf("heads events=%d observations=%d changes=%d facts_cursors=%v legion_projects=%d read_whole=%d",
		s.heads.events, s.heads.observations, s.heads.changes, s.facts, len(s.projects), marked)
	if s.days == nil || len(s.walks) == 0 && len(s.days) == 0 {
		return line
	}
	done := 0
	tomorrow := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	for _, one := range s.walks {
		if one.builtBy == build && one.next != nil && one.next.Equal(tomorrow) {
			done++
		}
	}
	var builds []string
	for other, count := range s.builtBy {
		builds = append(builds, fmt.Sprintf("%s=%d", other, count))
	}
	slices.Sort(builds)
	return line + fmt.Sprintf(" refresher_cursors=%v walks=%d done=%d day_rows_by_build=[%s]", s.days, len(s.walks), done, strings.Join(builds, " "))
}
