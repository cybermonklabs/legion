package evals

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliveryseed"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legionseed"
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

// ownerRows is each line of lines (renamedRows) of a day row or a member that stands under an owner
// key (OwnerKey), with the key renamed where its repository is a name renamed holds, compared
// without case as the boot's rename pass compares it, and the eval id of a day row with it: what the
// pass writes of those rows, and nothing else of them.
func ownerRows(lines []string, renamed map[string]string) []string {
	var owned []string
	for _, line := range lines {
		fields := strings.Split(line, " ")
		if (fields[0] != "day" && fields[0] != "member") || !strings.HasPrefix(fields[1], ":") {
			continue
		}
		key := fields[1]
		repo, rest, _ := strings.Cut(key[1:], "|")
		for from, to := range renamed {
			if strings.EqualFold(repo, from) {
				fields[1] = ":" + to + "|" + rest
				if fields[0] == "day" {
					fields[2] = fields[1] + strings.TrimPrefix(fields[2], key)
				}
			}
		}
		owned = append(owned, strings.Join(fields, " "))
	}
	slices.Sort(owned)
	return owned
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
// Legion issues own. Right after the pass, before a refresher pass writes a row again, the day
// rows and members of the owner keys are the rows before with each key of an old name renamed
// (ownerRows), the whole of what the pass writes of them: a row the pass deleted by mistake is
// missing there, where the refresher's passes after it would write it again from its input.
func TestARenamedRepositorysRowsAreStoredAndReadUnderItsNewName(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	deliveries := deliverytest.Seed(t, pool, deliveryseed.Spec{Days: 5})
	legion := legiontest.Seed(t, pool, legionseed.Spec{Trees: 6, Now: deliveryseed.DefaultNow})
	const legionRepo = "acme/legion"
	r.exec(t, "give the Legion roots a repository", `update issues set repo = $2 where project_key = $1 and parent_key is null`,
		legion.Project, legionRepo)
	rows, filings, err := legionfacts.ComputeProject(ctx, pool, legion.Project, deliveryseed.DefaultNow)
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
	if got, want := ownerRows(r.renamedRows(t), nil), ownerRows(before, renamed); !slices.Equal(got, want) {
		missing, extra := difference(want, got), difference(got, want)
		t.Fatalf("right after the pass the rows of the owner keys are not the rows before under the renamed keys: %d missing, %d extra\nmissing: %s\nextra: %s",
			len(missing), len(extra), strings.Join(first(missing, 20), "\n  "), strings.Join(first(extra, 20), "\n  "))
	} else if slices.Equal(want, ownerRows(before, nil)) {
		t.Fatalf("no row of an owner key names a renamed repository before the pass, so the test cannot tell the pass's rows")
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
