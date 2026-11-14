package evalfixture_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/evals/evalfixture"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// now is the moment every history of these tests ends at.
var now = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// count is one number a statement answers.
func count(t *testing.T, database *store.Store, statement string, args ...any) int {
	t.Helper()
	var answer int
	if err := database.Pool.QueryRow(t.Context(), statement, args...).Scan(&answer); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
	return answer
}

// TestTheLegionModeWritesAHistoryObservedFromPartway: -legion writes at least the finished issues
// it is asked for, observations that begin partway through the history (some issues observed, some
// not, the first observation the one the result names), a root that waited for its slot, finished
// issues that sat parked in `icebox` and in `backlog`, a merged pull request for each issue the
// merger closed, filings by a worker into its tree, by a worker outside any tree and by the
// controller, the project's legion_settings row, and two whole-day windows on either side of the
// first observation that hold every finished issue between them.
func TestTheLegionModeWritesAHistoryObservedFromPartway(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	loaded, err := evalfixture.WriteLegion(ctx, database.Pool, "LEGK", 240, now)
	if err != nil {
		t.Fatalf("write the Legion history: %v", err)
	}
	if loaded.Finished < 240 || loaded.Issues != count(t, database, `select count(*) from issues where project_key = 'LEGK'`) {
		t.Errorf("the result names %d finished of %d issues, want at least 240 finished and every issue stored", loaded.Finished, loaded.Issues)
	}
	finished := count(t, database, `
		select count(*) from issues i
		where i.project_key = 'LEGK' and i.status = 'done' and i.closed_at is not null
			and (exists (select 1 from handoffs h where h.issue_key = i.key)
				or exists (select 1 from events e where e.issue_key = i.key and e.actor->'origin'->>'session_title' like '% · ' || i.key))`)
	if finished != loaded.Finished {
		t.Errorf("the database holds %d finished issues with Legion phase work, the result names %d", finished, loaded.Finished)
	}
	var first time.Time
	if err := database.Pool.QueryRow(ctx, `select min(observed_at) from legion_observations where project = 'LEGK'`).Scan(&first); err != nil {
		t.Fatalf("read the first observation: %v", err)
	}
	observed := count(t, database, `select count(distinct issue_key) from legion_observations where project = 'LEGK'`)
	worked := count(t, database, `select count(distinct issue_key) from handoffs h join issues i on i.key = h.issue_key where i.project_key = 'LEGK'`)
	if !first.Equal(loaded.FirstObserved) || observed == 0 || loaded.Unobserved == 0 {
		t.Errorf("the first observation is %s (the result names %s), %d issues observed of %d trees with %d unobserved",
			first, loaded.FirstObserved, observed, loaded.Trees, loaded.Unobserved)
	}
	queued := count(t, database, `
		select count(distinct p.issue_key) from legion_observations p
		where p.project = 'LEGK' and p.field = 'phase' and p.value = '"admitted"'
			and p.observed_at < (select min(s.observed_at) from legion_observations s
				where s.project = p.project and s.issue_key = p.issue_key and s.field = 'slot')`)
	if queued == 0 {
		t.Error("no issue was admitted before it took its slot, so slot wait reads zero for every issue")
	}
	if url := count(t, database, `select count(*) from legion_settings where project = 'LEGK' and state_url = $1`, evalfixture.StateURL); url != 1 {
		t.Errorf("%d legion_settings rows name %s, want one", url, evalfixture.StateURL)
	}
	parked := func(status string) int {
		return count(t, database, `
			select count(distinct e.issue_key) from events e join issues i on i.key = e.issue_key
			where i.project_key = 'LEGK' and i.status = 'done' and e.payload->>'status' = $1`, status)
	}
	icebox, backlog := parked("icebox"), parked("backlog")
	merged := count(t, database, `
		select count(*) from delivery_pull_requests p join issues i on i.key = p.issue_key
		where i.project_key = 'LEGK' and p.merged_at is not null and p.merged_at < i.closed_at`)
	filed := func(title string) int {
		return count(t, database, `
			select count(*) from events e join issues i on i.key = e.issue_key
			where i.project_key = 'LEGK' and e.type = 'issue.created' and e.actor->'origin'->>'session_title' like $1`, title)
	}
	later, outside, controller := filed("Legion implementer · %"), filed("Legion reviewer · %"), filed("Legion controller · LEGK")
	if icebox == 0 || backlog == 0 || merged != loaded.Trees*3 || later == 0 || outside == 0 || controller == 0 {
		t.Errorf("%d finished issues sat in icebox and %d in backlog, %d merged pull requests (want %d), filings by an implementer %d, a reviewer %d and the controller %d: want some of each",
			icebox, backlog, merged, loaded.Trees*3, later, outside, controller)
	}

	before, after := loaded.Windows.Before, loaded.Windows.After
	if !before.From.Before(loaded.FirstObserved) || before.To != after.From || after.From.Before(loaded.FirstObserved) {
		t.Errorf("the windows %s..%s and %s..%s do not fall on either side of the first observation %s",
			before.From, before.To, after.From, after.To, loaded.FirstObserved)
	}
	in := func(window evalfixture.Window) int {
		return count(t, database, `select count(*) from issues where project_key = 'LEGK' and status = 'done' and closed_at >= $1 and closed_at < $2
			and key in (select issue_key from handoffs union select issue_key from events where actor->'origin'->>'session_title' like '% · ' || issue_key)`,
			window.From, window.To)
	}
	early, late := in(before), in(after)
	if early == 0 || late == 0 || early+late != loaded.Finished || before.Days() > 84 || after.Days() > 84 {
		t.Errorf("the windows hold %d and %d of %d finished issues over %d and %d days, want both some, every one between them, and at most 84 days each",
			early, late, loaded.Finished, before.Days(), after.Days())
	}
	t.Logf("%s: %d issues, %d finished (%d before, %d after), %d of %d with phase work observed, %d waited for a slot, %d sat in icebox and %d in backlog, %d merged pull requests, filings by an implementer %d, a reviewer %d and the controller %d; first observation %s; windows %s..%s and %s..%s",
		loaded.Project, loaded.Issues, loaded.Finished, early, late, observed, worked, queued, icebox, backlog, merged, later, outside, controller, loaded.FirstObserved.Format(time.RFC3339),
		before.From.Format(time.DateOnly), before.To.Format(time.DateOnly), after.From.Format(time.DateOnly), after.To.Format(time.DateOnly))
}

// TestTheBaseModeWritesTheSameHistoryIntoTheBaseSchema: -base refuses a database of this head,
// writes into one without this head's tables (this head's database with them dropped, which is
// the base's Legion schema) exactly the rows -legion writes into this head's with the same -now,
// and -legion refuses that database.
func TestTheBaseModeWritesTheSameHistoryIntoTheBaseSchema(t *testing.T) {
	ctx := t.Context()
	head, base := storetest.Open(t), storetest.Open(t)
	if _, err := evalfixture.WriteBase(ctx, head.Pool, "LEGK", 240, now); err == nil || !strings.Contains(err.Error(), "eval_days") {
		t.Fatalf("-base into this head's database answered %v, want a refusal naming eval_days", err)
	}
	if _, err := base.Pool.Exec(ctx, `drop trigger events_session_title on events; drop function events_session_title(); drop table `+
		strings.Join(evalfixture.BaseLacks, ", ")+` cascade`); err != nil {
		t.Fatalf("drop this head's tables: %v", err)
	}
	if _, err := evalfixture.WriteLegion(ctx, base.Pool, "LEGK", 240, now); err == nil || !strings.Contains(err.Error(), "-base") {
		t.Fatalf("-legion into the base's schema answered %v, want a refusal that names -base", err)
	}
	if _, err := evalfixture.WriteLegion(ctx, head.Pool, "LEGK", 240, now); err != nil {
		t.Fatalf("write the Legion history into this head's database: %v", err)
	}
	if _, err := evalfixture.WriteBase(ctx, base.Pool, "LEGK", 240, now); err != nil {
		t.Fatalf("write the Legion history into the base's schema: %v", err)
	}
	// Each table as the rows it holds, ids and the moment of the write left out.
	digests := map[string]string{
		"projects":               `select md5(string_agg(key || next_number, ',' order by key)) from projects`,
		"issues":                 `select md5(string_agg(key || coalesce(parent_key, '') || status || created_at || coalesce(closed_at::text, ''), ',' order by key)) from issues`,
		"events":                 `select md5(string_agg(issue_key || seq || type || actor::text || payload::text || created_at, ',' order by issue_key, seq)) from events`,
		"handoffs":               `select md5(string_agg(issue_key || phase || created_at, ',' order by issue_key, created_at, phase)) from handoffs`,
		"legion_observations":    `select md5(string_agg(issue_key || field || value::text || observed_at, ',' order by issue_key, field, observed_at)) from legion_observations`,
		"legion_settings":        `select md5(string_agg(project || state_url, ',' order by project)) from legion_settings`,
		"delivery_pull_requests": `select md5(string_agg(repo || number || title || coalesce(issue_key, '') || coalesce(merged_at::text, ''), ',' order by repo, number)) from delivery_pull_requests`,
	}
	for _, table := range evalfixture.LegionTables {
		var mine, theirs string
		if err := head.Pool.QueryRow(ctx, digests[table]).Scan(&mine); err != nil {
			t.Fatalf("digest %s of this head's database: %v", table, err)
		}
		if err := base.Pool.QueryRow(ctx, digests[table]).Scan(&theirs); err != nil {
			t.Fatalf("digest %s of the base's: %v", table, err)
		}
		if mine != theirs {
			t.Errorf("%s differs between -legion's database (%s) and -base's (%s)", table, mine, theirs)
		}
	}
	t.Logf("-base wrote the %d tables %v as -legion does, into a database without %v", len(evalfixture.LegionTables), evalfixture.LegionTables, evalfixture.BaseLacks)
}

// TestTheAttributionModeWritesK15sCase: -attribution writes its three projects, two mapped
// repositories, two pull requests, two deploys and a CI job, and each row counts, in the day rows
// the refresher builds from them, for the projects the result names and through what it names
// (D1): pr-open-to-merge counts the pull requests, deploy-frequency the deploys and pr-ci the CI
// run, and the third project holds no row.
func TestTheAttributionModeWritesK15sCase(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	written, err := evalfixture.WriteAttribution(ctx, database.Pool, "ATTR", now)
	if err != nil {
		t.Fatalf("write k15's case: %v", err)
	}
	settings, err := delivery.GetSettings(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	registry := evals.New(database, nil, nil)
	day := now.Truncate(24*time.Hour).AddDate(0, 0, -1)
	if _, err := registry.WriteDeliveryDay(ctx, settings, day, "", "evalfixture-test"); err != nil {
		t.Fatalf("write the delivery rows of %s: %v", day.Format(time.DateOnly), err)
	}
	type tally struct{ n, via int }
	want := map[string]map[string]tally{}
	for _, row := range written.Rows {
		if row.Day != day.Format(time.DateOnly) {
			t.Fatalf("%s falls on %s, want every row on %s", row.Row, row.Day, day.Format(time.DateOnly))
		}
		eval := evals.SlugPRCI
		switch {
		case strings.HasPrefix(row.Row, "pull request"):
			eval = evals.SlugPROpenToMerge
		case strings.HasPrefix(row.Row, "deploy run"):
			eval = evals.SlugDeployFrequency
		}
		for index, project := range row.CountsFor {
			if want[project] == nil {
				want[project] = map[string]tally{}
			}
			held := want[project][eval]
			held.n++
			if row.ViaRepository[index] {
				held.via++
			}
			want[project][eval] = held
		}
	}
	series := map[string]string{evals.SlugPROpenToMerge: "minutes", evals.SlugDeployFrequency: "deploys", evals.SlugPRCI: "minutes"}
	for _, project := range written.Projects {
		for eval, name := range series {
			got := tally{}
			if err := database.Pool.QueryRow(ctx, `
				select coalesce(sum(n), 0), coalesce(sum(via_repository), 0) from eval_days
				where project_key = $1 and eval_id = $2 and series = $3 and repo = ''`,
				project, evals.EvalID(project, eval), name).Scan(&got.n, &got.via); err != nil {
				t.Fatalf("read %s's %s rows: %v", project, eval, err)
			}
			if got != want[project][eval] {
				t.Errorf("%s's %s.%s counts %d rows, %d through a repository; the case names %d and %d",
					project, eval, name, got.n, got.via, want[project][eval].n, want[project][eval].via)
			}
		}
	}
	if rows := count(t, database, `select count(*) from eval_days where project_key = $1`, written.Nothing); rows != 0 {
		t.Errorf("%s, the project with nothing, holds %d day rows", written.Nothing, rows)
	}
	for _, row := range written.Rows {
		t.Logf("%s counts for %v, through a repository %v", row.Row, row.CountsFor, row.ViaRepository)
	}
}

// TestTheDeliveryModeJoinsTheLegionProject: -delivery after -legion into one project writes its
// issues numbered after the Legion history's, keeps the project's row, and the settled next number
// is past every issue.
func TestTheDeliveryModeJoinsTheLegionProject(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	loaded, err := evalfixture.WriteLegion(ctx, database.Pool, "MIXD", 40, now)
	if err != nil {
		t.Fatalf("write the Legion history: %v", err)
	}
	written, err := evalfixture.WriteDelivery(ctx, database.Pool, "MIXD", 7, 1, now)
	if err != nil {
		t.Fatalf("write the delivery history beside it: %v", err)
	}
	if err := evalfixture.SettleNumbers(ctx, database.Pool, []string{"MIXD"}); err != nil {
		t.Fatal(err)
	}
	ours := written.Fixture.Issues["MIXD"]
	issues := count(t, database, `select count(*) from issues where project_key = 'MIXD'`)
	next := count(t, database, `select next_number from projects where key = 'MIXD'`)
	highest := count(t, database, `select max(number) from issues where project_key = 'MIXD'`)
	lowest := count(t, database, `select min(number) from issues where key = any($1)`, ours)
	if len(ours) == 0 || len(written.Fixture.PullRequests) == 0 || issues != loaded.Issues+len(ours) || lowest <= loaded.Issues || next != highest+1 {
		t.Errorf("MIXD holds %d issues (%d Legion, %d delivery from number %d), %d pull requests, next number %d after %d",
			issues, loaded.Issues, len(ours), lowest, len(written.Fixture.PullRequests), next, highest)
	}
	t.Logf("%s; the delivery issues number from %d, after the Legion history's %d; next number %d", written.Summary, lowest, loaded.Issues, next)
}

// TestMergePRMergesOnePullRequestAtTheMomentNamed: -merge-pr moves one pull request's merge to the
// moment named, logs one change of it for the refresher, names the two days it moved, and refuses
// a pull request no row holds, a malformed name, and a moment before the pull request opened.
func TestMergePRMergesOnePullRequestAtTheMomentNamed(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	written, err := evalfixture.WriteDelivery(ctx, database.Pool, "MRG", 7, 1, now)
	if err != nil {
		t.Fatalf("write the delivery history: %v", err)
	}
	changes := `select count(*) from delivery_changes where kind = 'pull_request'`
	before := count(t, database, changes)
	at := now.Add(time.Hour)
	merged, err := evalfixture.MergePR(ctx, database.Pool, written.Newest, at)
	if err != nil {
		t.Fatalf("merge %s: %v", written.Newest, err)
	}
	repo, number, _ := strings.Cut(written.Newest, "#")
	var stored time.Time
	if err := database.Pool.QueryRow(ctx, `select merged_at from delivery_pull_requests where repo = $1 and number = $2::int`, repo, number).Scan(&stored); err != nil {
		t.Fatalf("read %s: %v", written.Newest, err)
	}
	if !stored.Equal(at) || merged.MergeDay != at.Format(time.DateOnly) || merged.WasMergeDay == nil ||
		!slices.Contains(merged.DaysAffected, *merged.WasMergeDay) || !slices.Contains(merged.DaysAffected, merged.MergeDay) {
		t.Fatalf("%s merged at %s, the result %+v, want %s and both of its days", written.Newest, stored, merged, at)
	}
	if after := count(t, database, changes); after <= before {
		t.Errorf("the merge logged %d pull-request changes, want one at least", after-before)
	}
	for _, refused := range []struct {
		ref string
		at  time.Time
	}{
		{"acme/none#1", at},
		{"acme-no-number", at},
		{written.Newest, written.From.Add(-24 * time.Hour)},
	} {
		if _, err := evalfixture.MergePR(ctx, database.Pool, refused.ref, refused.at); err == nil {
			t.Errorf("merging %s at %s was accepted, want a refusal", refused.ref, refused.at)
		}
	}
	t.Logf("merged %s at %s, on %s; it had merged on %s; days affected %v", written.Newest, at.Format(time.RFC3339), merged.MergeDay, *merged.WasMergeDay, merged.DaysAffected)
}
