package evals

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// renamedRowsStatements read every stored row of the eval registry's tables that names a repository,
// each as one line of its key and its values, with the columns that say when or by which build it
// was written left out: the delivery, CI and Legion day rows, their members, the stored figures
// (their fold key and stamp, which name the rows' write times, left out too) and the repository of
// each issue's stored facts.
var renamedRowsStatements = []string{
	`select concat_ws(' ', 'day', project_key, eval_id, writer, deploy_repo, series, repo, day, listed, n, sum, num, den, max,
		samples::text, last_value, last_at, last_recorded_at, last_reading, counts::text, via_repository) from eval_days`,
	`select concat_ws(' ', 'member', project_key, family, deploy_repo, row_key, day, repo, shipped_by) from eval_day_members`,
	`select concat_ws(' ', 'range', project_key, repo, days, end_day, eval_id, deploy_repo, encode(figures, 'hex')) from eval_range_figures`,
	`select concat_ws(' ', 'day figures', project_key, repo, day, eval_id, deploy_repo, encode(figures, 'hex')) from eval_day_figures`,
	`select concat_ws(' ', 'facts', project_key, issue_key, coalesce(repo, '-')) from issue_facts`,
}

// renamedRows is every line renamedRowsStatements read, sorted.
func (r *refresherRig) renamedRows(t *testing.T) []string {
	t.Helper()
	var lines []string
	for _, statement := range renamedRowsStatements {
		rows, err := r.database.Pool.Query(t.Context(), statement)
		if err != nil {
			t.Fatalf("read %s: %v", statement, err)
		}
		var line string
		if _, err := pgx.ForEachRow(rows, []any{&line}, func() error {
			lines = append(lines, line)
			return nil
		}); err != nil {
			t.Fatalf("read %s: %v", statement, err)
		}
	}
	slices.Sort(lines)
	return lines
}

// renamedDayRows is every day row and member, with just the fields the rename pass moves:
// its owner key, its eval id when it carries that key, and its deploy repository.
func renamedDayRows(lines []string, renamed map[string]string) []string {
	var rows []string
	for _, line := range lines {
		if strings.HasPrefix(line, "day figures ") {
			continue
		}
		fields := strings.Split(line, " ")
		if fields[0] != "day" && fields[0] != "member" {
			continue
		}
		deploy := 3
		if fields[0] == "day" {
			deploy = 4
		}
		key := fields[1]
		if strings.HasPrefix(key, ":") {
			repo, rest, _ := strings.Cut(key[1:], "|")
			if to, ok := renamed[strings.ToLower(repo)]; ok {
				fields[1] = ":" + to + "|" + rest
				if fields[0] == "day" {
					fields[2] = fields[1] + strings.TrimPrefix(fields[2], key)
				}
			}
		}
		if to, ok := renamed[strings.ToLower(fields[deploy])]; ok {
			fields[deploy] = to
		}
		rows = append(rows, strings.Join(fields, " "))
	}
	slices.Sort(rows)
	return rows
}

// A repository renamed by a pair the boot reads is stored and read under its new name once the
// boot's rename pass and the refresher's next passes have run: every day row, member, stored
// figure and stored Legion fact that named the old name names the new one and holds what it held,
// and no row names an old name. The pass writes the delivery rows, the settings and the issues'
// repositories under the new name, and the stored facts' repository (issue_facts.repo); the
// triggers of those tables log each row it wrote as a change, which the refresher reads as any
// other change, so it writes the days, members and figures of those rows again from the renamed
// rows, and deletes those it held under the old name. Three repositories are renamed: the deploy
// repository, which every run and the settings name and every row of the evals that read the
// deploy repository is kept under, a repository of pull requests alone, and the repository the
// Legion issues own. Right after the pass the day rows and members have only their owner keys and
// deploy repositories renamed (renamedDayRows).
// A row the pass deleted by mistake is missing there, even if the refresher would write it again
// from its input.
func TestARenamedRepositorysRowsAreStoredAndReadUnderItsNewName(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	deliveries := deliverytest.Seed(t, pool, deliverytest.Spec{Days: 5})
	legion := legiontest.Seed(t, pool, legiontest.Spec{Trees: 6, Now: deliverytest.DefaultNow})
	const legionRepo = "acme/legion"
	r.exec(t, "give the Legion roots a repository", `update issues set repo = $2 where project_key = $1 and parent_key is null`,
		legion.Project, legionRepo)
	rows, filings, err := legionfacts.ComputeProject(ctx, pool, legion.Project, deliverytest.DefaultNow)
	if err != nil {
		t.Fatalf("compute the facts of %s: %v", legion.Project, err)
	}
	if _, err := legionfacts.WriteProject(ctx, pool, legionfacts.ProjectWrite{Project: legion.Project, Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the facts of %s: %v", legion.Project, err)
	}
	if err := legionfacts.WriteFilings(ctx, pool, legionfacts.FilingsWrite{Project: legion.Project, Filings: filings, Whole: true}); err != nil {
		t.Fatalf("write the filings of %s: %v", legion.Project, err)
	}
	r.settle(t)

	renamed := map[string]string{
		deliveries.DeployRepo: "acme/deliv-core",
		deliveries.Repos[2]:   "acme/deliv-site",
		legionRepo:            "acme/legion-next",
	}
	before := r.renamedRows(t)
	want := make([]string, len(before))
	named := map[string]int{}
	for i, line := range before {
		for from, to := range renamed {
			if strings.Contains(line, from) {
				named[from]++
				line = strings.ReplaceAll(line, from, to)
			}
		}
		want[i] = line
	}
	slices.Sort(want)
	for from := range renamed {
		if named[from] == 0 {
			t.Fatalf("no stored row names %s before the rename, so the test cannot tell its rows move", from)
		}
	}
	t.Logf("rows naming each renamed repository before the pass: %v", named)
	for from, to := range renamed {
		r.exec(t, "declare the rename of "+from, `insert into repo_projects (repo, project, created_by, renamed_to)
			values ($1, $2, '{"kind":"user","id":"alice"}', $3)`, from, deliveries.Project, to)
	}

	pairs, err := store.ReadRepoRenames(ctx, pool)
	if err != nil {
		t.Fatalf("read the rename pairs: %v", err)
	}
	if pass := store.RewriteRenamedRepositories(ctx, pool, pairs); pass.Err() != nil || pass.Failures != 0 {
		t.Fatalf("the boot's rename pass = %+v (%v), want no failure and nothing left", pass, pass.Err())
	}
	if got, want := renamedDayRows(r.renamedRows(t), nil), renamedDayRows(before, renamed); !slices.Equal(got, want) {
		missing, extra := difference(want, got), difference(got, want)
		t.Fatalf("right after the pass the rows and members differ from the rows before under the renamed keys and deploy repositories: %d missing, %d extra\nmissing: %s\nextra: %s",
			len(missing), len(extra), strings.Join(first(missing, 20), "\n  "), strings.Join(first(extra, 20), "\n  "))
	} else if slices.Equal(want, renamedDayRows(before, nil)) {
		t.Fatalf("no day row or member names a renamed repository before the pass")
	}
	passes, _ := r.settle(t)

	after := r.renamedRows(t)
	for _, line := range after {
		for from := range renamed {
			if strings.Contains(line, from+" ") || strings.HasSuffix(line, from) || strings.Contains(line, from+"#") || strings.Contains(line, from+"/") {
				t.Errorf("a row still names the old name %s: %s", from, line)
			}
		}
	}
	if !slices.Equal(after, want) {
		missing, extra := difference(want, after), difference(after, want)
		t.Fatalf("after the pass and %d refresher passes the stored rows are not the rows before under the new names: %d missing, %d extra\nmissing: %s\nextra: %s",
			passes, len(missing), len(extra), strings.Join(first(missing, 20), "\n  "), strings.Join(first(extra, 20), "\n  "))
	}
	t.Logf("after %d refresher passes, %d stored rows equal the %d before under the new names", passes, len(after), len(before))
}

// A project whose Legion issues name no repository holds its Legion rows under ':default'
// (legionfacts.DefaultRepo), never under a name, and a read that names the repository it publishes
// as its default folds them into that repository's cut. The boot's rename pass renames the
// published default with the owner keys, so the refresher sees no move of it, and no change the
// pass's writes log names those rows: none of them moves, only the cut a read finds them in. Right
// after the pass, a read of the new name's cut, and of the old name's, states no eval from the
// stored figures and each answers the fold of its day rows; once the refresher settles, both are
// stated from the stored figures again, each the same fold, and every stored figure is the fold of
// the rows that stand (expectTruth). Red without the pass's marks of the two cuts
// (store.markRenamedCuts): the read of the new name then states the stored figures of a cut that
// holds none of the ':default' rows, the old name's those of rows it no longer reads, and the
// refresher, which nothing wakes for them, leaves both so.
func TestARenamedPublishedDefaultIsReadUnderItsNewNameFromTheRefoldOfItsCut(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	_, legion := r.seedHistories(t)
	project := legion.Project
	const defaultRepo, renamedTo = "acme/legion", "acme/legion-next"
	r.exec(t, "give the Legion project a default repository", `update projects set default_repo = $2 where key = $1`, project, defaultRepo)
	r.settle(t)
	r.expectBuilt(t, "the first whole build", refresherToday)
	window := moveShapes()[0].window
	var named, defaultRows, figures int
	if err := pool.QueryRow(ctx, `select
			(select count(*) from issues where project_key = $1 and repo is not null),
			(select count(*) from eval_days where project_key = $1 and repo = $2 and listed and day >= $4 and day < $5),
			(select count(*) from eval_range_figures where project_key = $1 and repo = $3)`,
		project, legionfacts.DefaultRepo, defaultRepo, window.From, window.To).Scan(&named, &defaultRows, &figures); err != nil {
		t.Fatalf("read %s's rows: %v", project, err)
	}
	if named != 0 || defaultRows == 0 || figures == 0 {
		t.Fatalf("%s holds %d issues that name a repository, %d listed rows under %s over 28 days and %d stored range figures of %s, and the check needs none, some and some",
			project, named, defaultRows, legionfacts.DefaultRepo, figures, defaultRepo)
	}
	r.exec(t, "declare the rename of "+defaultRepo, `insert into repo_projects (repo, project, created_by, renamed_to)
		values ($1, $2, '{"kind":"user","id":"alice"}', $3)`, defaultRepo, project, renamedTo)
	pairs, err := store.ReadRepoRenames(ctx, pool)
	if err != nil {
		t.Fatalf("read the rename pairs: %v", err)
	}
	if pass := store.RewriteRenamedRepositories(ctx, pool, pairs); pass.Err() != nil || pass.Failures != 0 {
		t.Fatalf("the boot's rename pass = %+v (%v), want no failure and nothing left", pass, pass.Err())
	}
	if published, err := PublishedDefaultIn(ctx, pool, project); err != nil || published != renamedTo {
		t.Fatalf("the pass left %s publishing %q as its default (%v), want %s", project, published, err, renamedTo)
	}
	check := func(what string, stale bool) {
		t.Helper()
		for _, repo := range []string{renamedTo, defaultRepo} {
			for _, one := range moveShapes() {
				stated, folded, fromFigures := r.listValues(t, project, []string{repo}, one.window, &one.compare)
				if len(folded) == 0 {
					t.Fatalf("%s: the read of %s's %s over %s states no eval, and the check needs its evals", what, project, repo, one.name)
				}
				for _, id := range slices.Sorted(maps.Keys(folded)) {
					if fromFigures[id] == stale {
						t.Errorf("%s: the read of %s's %s over %s states %s from the stored figures %t, want %t", what, project, repo, one.name, id, fromFigures[id], !stale)
					}
					if stated[id] != folded[id] {
						t.Errorf("%s: the read of %s's %s over %s states %s's values in %d bytes that differ from the fold of its day rows' %d at byte %d",
							what, project, repo, one.name, id, len(stated[id]), len(folded[id]), firstDiffering(stated[id], folded[id]))
					}
				}
			}
		}
	}
	check("right after the rename pass", true)
	r.settle(t)
	check("once the refresher settled", false)
	expectTruth(t, "once the refresher settled", r.storedFigures(t), r.truthFigures(t))
}

// A renamed deploy source whose new name a source of the same project reads already, its runs
// stored under another spelling of case (the spelling GitHub gives the intake), so its rule, its
// rows and its source key stand under that spelling: ALPHA's two sources, under one rule, read
// acme/alpha-ops, whose run 2001 deploys on sourcesDay, and acme/alpha-core, whose runs 5001 and
// 5002, stored as Acme/Alpha-Core, deploy three days before and on sourcesDay, so the new name's
// rule starts first. The boot's rename of acme/alpha-ops to acme/alpha-core, and one refresher
// pass after it, leave one repository, acme/alpha-core in lower case: the runs and their jobs,
// whose moves the change log names, arriving under it and leaving under the name each had; one
// rule, the new name's, its fields and times as they stood, recorded again by no pass; one deploy
// repository a read selects (DeployReposIn), whose rows count each of the three deploys once,
// under it, and no member of another spelling; and no rebuild, no walk begun again and no day row
// written but on the two days the moved runs name, as the rule and its start stand. Red where the
// pass leaves the new name's rule, rows or runs in mixed case beside the old name's moved under the
// new name in lower case: the refresher's pass finds the source's runs under two repositories,
// records a rule for each spelling and builds the new one's days again.
func TestARenamedSourceRepositoryOfAMixedCaseDestinationIsReadAsOneRepository(t *testing.T) {
	t.Parallel()
	const coreRepo, coreSpelled = "acme/alpha-core", "Acme/Alpha-Core"
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	early := sourcesDay.AddDate(0, 0, -3)
	r.exec(t, "the project", `insert into projects (key, name, default_assignee, next_number) values ('ALPHA', 'Alpha', 'alice', 1)`)
	putSources(t, pool, "ALPHA", []string{"dev-ana"},
		deliverytest.SourceOf(ruleOfJob(opsJob), new(alphaOps)), deliverytest.SourceOf(ruleOfJob(opsJob), new(coreRepo)))
	r.deployRun(t, coreSpelled, 5001, early.Add(10*time.Hour), opsJob)
	r.deployRun(t, alphaOps, 2001, sourcesDay.Add(10*time.Hour), opsJob)
	r.deployRun(t, coreSpelled, 5002, sourcesDay.Add(11*time.Hour), opsJob)
	r.settle(t)
	since := func(day time.Time) string { return "ALPHA since " + day.Format(time.DateOnly) }
	if got, want := r.ruleRecords(t), map[string]string{alphaOps: since(sourcesDay), coreSpelled: since(early)}; !maps.Equal(got, want) {
		t.Fatalf("before the rename eval_days_rules records %v, want %v", got, want)
	}
	deploys := func(what string) {
		t.Helper()
		for _, day := range []time.Time{early, sourcesDay} {
			want := defaultTally{n: 1, via: 1}
			if day.Equal(sourcesDay) {
				want = defaultTally{n: 2, via: 2}
			}
			if got := r.deploysOn(t, "ALPHA", day); got != want {
				t.Errorf("%s: ALPHA's deploy-frequency on %s counts %d deploys, %d through a repository; want %d and %d",
					what, day.Format(time.DateOnly), got.n, got.via, want.n, want.via)
			}
		}
	}
	deploys("before the rename")
	// rule is the new name's rule, in any case, but for its repository: its project, its fields, its
	// start and its times.
	const rule = `select 'rule', (to_jsonb(r) - 'repo')::text from eval_days_rules r where lower(r.repo) = 'acme/alpha-core'`
	destination := r.xmins(t, rule)["rule"]
	seq := r.xmins(t, `select 'seq', coalesce(max(seq), 0)::text from delivery_changes`)["seq"]

	r.exec(t, "declare the rename of "+alphaOps, `insert into repo_projects (repo, project, created_by, renamed_to)
		values ($1, 'ALPHA', '{"kind":"user","id":"alice"}', $2)`, alphaOps, coreRepo)
	pairs, err := store.ReadRepoRenames(ctx, pool)
	if err != nil {
		t.Fatalf("read the rename pairs: %v", err)
	}
	if pass := store.RewriteRenamedRepositories(ctx, pool, pairs); pass.Err() != nil || pass.Failures != 0 {
		t.Fatalf("the boot's rename pass = %+v (%v), want no failure and nothing left", pass, pass.Err())
	}
	// The runs and their jobs, and the change log's word of each run's move: its arrival under the
	// new name, and its leaving under the name it had, the old name's as the change log's own line
	// of the pass renames it.
	for query, want := range map[string]string{
		`select 'runs', string_agg(r.repo || '#' || r.run_id || ' ' || (select string_agg(j.repo || ' ' || j.name, ',') from delivery_run_jobs j
			where j.run_id = r.run_id), ', ' order by r.run_id) from delivery_runs r`: "acme/alpha-core#2001 acme/alpha-core ops / release, " +
			"acme/alpha-core#5001 acme/alpha-core ops / release, acme/alpha-core#5002 acme/alpha-core ops / release",
		`select 'runs', string_agg(distinct (repo || '#' || ref || case when t_old is null then ' in' when t_new is null then ' out' else ' moved' end) collate "C", ', '
			order by (repo || '#' || ref || case when t_old is null then ' in' when t_new is null then ' out' else ' moved' end) collate "C")
			from delivery_changes where kind = 'run' and seq > ` + seq: "Acme/Alpha-Core#5001 out, Acme/Alpha-Core#5002 out, acme/alpha-core#2001 in, " +
			"acme/alpha-core#2001 moved, acme/alpha-core#2001 out, acme/alpha-core#5001 in, acme/alpha-core#5002 in",
	} {
		if got := r.xmins(t, query)["runs"]; got != want {
			t.Errorf("after the rename pass %s\n= %s\nwant %s", query, got, want)
		}
	}
	if got := r.xmins(t, rule)["rule"]; got != destination {
		t.Errorf("after the rename pass the new name's rule reads %s, want it as it stood: %s", got, destination)
	}
	const settled = `select 'rule', xmin::text from eval_days_rules where repo = 'acme/alpha-core'`
	moved, rows, rebuilds := r.xmins(t, settled)["rule"], r.xmins(t, dayRowXmins), r.count(t, `select count(*) from eval_rebuilds`)
	walks := r.xmins(t, `select family || ' ' || project_key, xmin::text from eval_days_walks`)

	report, _ := r.pass(t)

	if got, want := r.ruleRecords(t), map[string]string{coreRepo: since(early)}; !maps.Equal(got, want) {
		t.Errorf("after the refresher's pass eval_days_rules records %v, want %v", got, want)
	}
	if got := r.xmins(t, settled)["rule"]; got != moved {
		t.Errorf("the refresher's pass recorded the new name's rule again (xmin %s, the rename pass's %s)", got, moved)
	}
	repos, err := DeployReposIn(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if got := repos.Of("ALPHA"); !slices.Equal(got, []string{coreRepo}) {
		t.Errorf("ALPHA's read of the deploy repository folds %v, want %s alone", got, coreRepo)
	}
	deploys("after the rename and one refresher pass")
	for day, want := range map[time.Time][]string{
		early:      {RunMember(coreRepo, 5001)},
		sourcesDay: {RunMember(coreRepo, 2001), RunMember(coreRepo, 5002)},
	} {
		if got := r.deployItems(t, "ALPHA", day); !slices.Equal(got, want) {
			t.Errorf("ALPHA's deploy evals count %v on %s, want %v", got, day.Format(time.DateOnly), want)
		}
	}
	for what, query := range map[string]string{
		"day rows of another spelling": `select count(*) from eval_days where lower(deploy_repo) in ('acme/alpha-ops', 'acme/alpha-core') and deploy_repo <> 'acme/alpha-core'`,
		"members of another spelling": `select count(*) from eval_day_members where (lower(deploy_repo) in ('acme/alpha-ops', 'acme/alpha-core')
			and deploy_repo <> 'acme/alpha-core') or (row_key like 'delivery_run:%' and row_key not like 'delivery_run:acme/alpha-core#%')`,
		"source keys of another spelling": `select count(*) from eval_day_owners where lists = '{>~}' and owner <> ':acme/alpha-core||>~'`,
	} {
		if n := r.count(t, query); n != 0 {
			t.Errorf("after the rename and one refresher pass %d %s stand", n, what)
		}
	}
	if n := r.count(t, `select count(*) from eval_rebuilds`); n != rebuilds {
		t.Errorf("the refresher's pass began %d rebuilds, want none", n-rebuilds)
	}
	if differ := sameValues(r.xmins(t, `select family || ' ' || project_key, xmin::text from eval_days_walks`), walks, func(xmin string) string { return xmin }); len(differ) > 0 {
		t.Errorf("the refresher's pass began walks again:\n\t%s", strings.Join(differ, "\n\t"))
	}
	moves := map[string]bool{early.Format(time.DateOnly): true, sourcesDay.Format(time.DateOnly): true}
	after := r.xmins(t, dayRowXmins)
	for key := range rows {
		if after[key] != rows[key] && !moves[key[strings.LastIndex(key, " ")+1:]] {
			t.Errorf("the refresher's pass wrote or deleted the day row %s, on no day a moved run names", key)
		}
	}
	for key := range after {
		if _, held := rows[key]; !held && !moves[key[strings.LastIndex(key, " ")+1:]] {
			t.Errorf("the refresher's pass wrote the day row %s, on no day a moved run names", key)
		}
	}
	for day := range report.days {
		if day.family == walkDelivery && !moves[day.day.Format(time.DateOnly)] {
			t.Errorf("the refresher's pass built %s's delivery day %s, which no moved run names", day.project, day.day.Format(time.DateOnly))
		}
	}
	t.Logf("after one refresher pass: %d day rows, %d delivery days built", len(after), len(report.days))
}

// difference is the lines of a that b does not hold, both sorted, each as often as a holds it
// more than b does.
func difference(a, b []string) []string {
	var left []string
	j := 0
	for _, line := range a {
		for j < len(b) && b[j] < line {
			j++
		}
		if j < len(b) && b[j] == line {
			j++
			continue
		}
		left = append(left, line)
	}
	return left
}

// first is at most n of lines.
func first(lines []string, n int) []string {
	return lines[:min(n, len(lines))]
}
