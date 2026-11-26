package evalfixture_test

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/evals/evalfixture"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// refresh writes database's day rows as the server's refresher writes them, as of now
// (evals.Store.RefreshThrough): every project seeded, and every family's rows from its first input
// day through now's. It answers the registry that wrote them, which reads them as the routes do.
func refresh(t *testing.T, database *store.Store) *evals.Store {
	t.Helper()
	broker := events.NewBroker()
	// Settlement is an hour away, so no background settlement writes while a test reads.
	documents := docs.New(docs.Deps{Store: database, Events: broker, Settle: time.Hour})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down the document service: %v", err)
		}
	})
	registry := evals.New(database, documents, broker, delivery.NewAttributor(refs.Renames{}))
	deps := evals.RefresherDeps{Horizon: events.NewHorizon(database.Pool), Build: "evalfixture-test"}
	if err := registry.RefreshThrough(t.Context(), deps, now); err != nil {
		t.Fatalf("refresh the day rows as of %s: %v", now.Format(time.RFC3339), err)
	}
	return registry
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

// TestTheLegionModeLeavesEveryIssueWritableThroughTheAPI: the fixture loads a history into the
// database a running server reads, and the checks of dispatch://INFRA-2916 then write to its issues
// through the API (a status change, a reparent). An event the API writes takes the issue's next
// sequence (issues.last_seq + 1), and events_issue_key_seq_key holds one event to each, so a
// fixture that writes events 1..n and leaves last_seq at 0 makes the next API write of every such
// issue answer 500 on that key. Every issue with events holds last_seq at its highest event seq.
func TestTheLegionModeLeavesEveryIssueWritableThroughTheAPI(t *testing.T) {
	database := storetest.Open(t)
	if _, err := evalfixture.WriteLegion(t.Context(), database.Pool, "LEGK", 240, now); err != nil {
		t.Fatalf("write the Legion history: %v", err)
	}
	withEvents := count(t, database, `select count(distinct issue_key) from events where issue_key like 'LEGK-%'`)
	behind := count(t, database, `
		select count(*) from issues i
		where i.project_key = 'LEGK'
			and i.last_seq < (select coalesce(max(e.seq), 0) from events e where e.issue_key = i.key)`)
	if withEvents == 0 || behind != 0 {
		t.Errorf("%d of the %d issues with events hold a last_seq below their highest event seq, want none", behind, withEvents)
	}
}

// TestWindowsOfRefusesAFirstFinishedDayThatStartsAtTheFirstObservationOrAfter: the window before
// the first observation starts at the midnight of the first finished issue's day, so a history
// whose first issue finished on a day after its first observation, or on the day of an observation
// made at that day's midnight, has no such window and is refused, its refusal saying the day
// starts at or after the first observation; one observed later on the first finished day is not.
func TestWindowsOfRefusesAFirstFinishedDayThatStartsAtTheFirstObservationOrAfter(t *testing.T) {
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for _, refused := range []struct{ observed, finished time.Time }{
		{day.Add(10 * time.Hour), day.AddDate(0, 0, 1).Add(9 * time.Hour)},
		{day, day.Add(9 * time.Hour)},
	} {
		_, err := evalfixture.WindowsOf(legiontest.Fixture{FirstObserved: refused.observed, FirstFinished: refused.finished, To: now})
		if err == nil || !strings.Contains(err.Error(), "starts at or after the first observation") {
			t.Errorf("observed at %s, first finished at %s: answered %v, want a refusal naming a day that starts at or after the first observation",
				refused.observed.Format(time.RFC3339), refused.finished.Format(time.RFC3339), err)
		}
	}
	windows, err := evalfixture.WindowsOf(legiontest.Fixture{FirstObserved: day.Add(10 * time.Hour), FirstFinished: day.Add(9 * time.Hour), To: now})
	if err != nil || !windows.Before.From.Equal(day) {
		t.Errorf("observed later on the first finished day: windows %+v, %v; want one starting at %s", windows, err, day.Format(time.DateOnly))
	}
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

// TestTheAttributionModeWritesK15sCase: -attribution writes k15's case (verification version 26):
// three projects, two of them with a default repository, three pull requests, two deploys and two
// CI jobs, and the read the eval routes fold (evals.DayWindowIn, over the rows the refresher
// builds, evals.Store.RefreshThrough) counts each row for exactly the projects the case's R15
// oracle names and through what it names: pr-open-to-merge counts the pull requests,
// deploy-frequency the deploys and pr-ci the CI jobs, each day row carries how many of them came
// through a default repository, a pull request of the repository that is no project's default
// counts for none, and the third project reads no row of them, "no data". The read records how many
// rows the case attributes, how many day rows it read and each one's repository count, and fails
// where the case attributes no row.
func TestTheAttributionModeWritesK15sCase(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	written, err := evalfixture.WriteAttribution(ctx, database.Pool, "ATTR", now)
	if err != nil {
		t.Fatalf("write k15's case: %v", err)
	}
	day := now.Truncate(24*time.Hour).AddDate(0, 0, -1)
	registry := refresh(t, database)
	type tally struct{ n, via int }
	want := map[string]map[string]tally{}
	attributed := 0
	for _, row := range written.Rows {
		t.Logf("%s counts for %v, through a default repository %v", row.Row, row.CountsFor, row.ViaRepository)
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
		if len(row.CountsFor) > 0 {
			attributed++
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
	if attributed == 0 {
		t.Fatal("the case attributes no row to any project")
	}
	deployRepos, err := evals.DeployReposIn(ctx, database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	window := evals.Range{From: day, To: day.AddDate(0, 0, 1)}
	series := map[string]string{evals.SlugPROpenToMerge: "minutes", evals.SlugDeployFrequency: "deploys", evals.SlugPRCI: "minutes"}
	read := 0
	for _, project := range written.Projects {
		page, err := registry.Current(ctx, []string{project}, "", 200, 0)
		if err != nil {
			t.Fatalf("read %s's evals: %v", project, err)
		}
		var entries []evals.Entry
		for _, entry := range page.Items {
			if _, held := series[entry.Definition.Slug]; held {
				entries = append(entries, entry)
			}
		}
		if len(entries) != len(series) {
			t.Fatalf("%s holds %d of the evals %v", project, len(entries), slices.Sorted(maps.Keys(series)))
		}
		rows, err := evals.DayWindowIn(ctx, database.Pool, []string{project}, entries, deployRepos, nil, window, nil)
		if err != nil {
			t.Fatalf("read %s's day rows of %s: %v", project, day.Format(time.DateOnly), err)
		}
		got, held := map[string]tally{}, 0
		for _, row := range rows {
			slug := strings.TrimPrefix(row.EvalID, project+"/")
			if row.Part.Series != series[slug] {
				continue
			}
			held++
			t.Logf("%s's day row of %s.%s on %s: n %d, %d through a default repository", project, slug, row.Part.Series,
				row.Part.Day.Format(time.DateOnly), row.Part.N, row.Part.ViaRepository)
			got[slug] = tally{n: got[slug].n + row.Part.N, via: got[slug].via + row.Part.ViaRepository}
		}
		read += held
		if project == written.Nothing && held != 0 {
			t.Errorf("%s, the project with nothing, reads %d day rows of %v, want none: \"no data\"", project, held, slices.Sorted(maps.Keys(series)))
		}
		for slug, name := range series {
			if got[slug] != want[project][slug] {
				t.Errorf("%s's %s.%s counts %d rows, %d through a default repository; R15 names %d and %d",
					project, slug, name, got[slug].n, got[slug].via, want[project][slug].n, want[project][slug].via)
			}
		}
	}
	t.Logf("the case attributes %d of its %d rows; the read answered %d day rows of those evals", attributed, len(written.Rows), read)
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
