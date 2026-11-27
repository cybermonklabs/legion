//go:build survivalref

package survival_test

// The reference run of dispatch://INFRA-3206's check k5: the survival package's replay compared,
// over cybermonklabs/legion's 3,356 commits to c0c4997971c1 and a second repository's 2,518, with
// the outputs of the reference scripts (dispatch://INFRA-3117/artifact/survival-scripts-tar-gz
// version 5) and with the figures dispatch://INFRA-3117/artifact/method-md publishes. The inputs
// are the directory testdata/reference/prepare.sh writes, which SURVIVAL_REF_DIR names
// (testdata/reference/README.md); every test fails, naming the variable, where it is unset. Each
// test streams the patches from their files, one at a time.
//
//	DISPATCH_TEST_DATABASE_URL=… SURVIVAL_REF_DIR=<dir> go test -tags survivalref -timeout 60m \
//	  -run 'TestReference' -v ./internal/dispatch/delivery/survival/

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/survivaltest"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestMain gives TestReferenceIssuesAreAttributionsSourcesThreeAndFour its database.
func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// The replay reads, for every commit of the chain, the lines replay.py git read: each commit's
// added lines and each (adder, remover) pair's lines equal lg/r_git_added.tsv and
// lg/r_git_deaths.tsv, both as prepare.sh's step 4 wrote them and as the second-repository
// archive holds them, and the totals of r_git.json: 0 of 3,356 commits and 0 of 27,967 pairs
// differ.
func TestReferenceLegionPerCommit(t *testing.T) {
	l := refLegionChain(t)
	replay := refReplayEvery(t, l.chain, l.ref.PatchPath)
	got := refCountsOf(replay, l.chain)
	for _, source := range []struct {
		name   string
		prefix []string
	}{
		{"step 4's replay.py git", []string{"lg", "r_git"}},
		{"the archive's lg/r_git", append(slices.Clone(refSecondRepo), "lg", "r_git")},
	} {
		prefix := l.ref.Path(source.prefix...)
		c := refCompare(t, len(l.chain), refReadCounts(t, prefix), got)
		if c.commitsDiffer != 0 || c.pairsDiffer != 0 || c.commits != 3356 || c.pairs != 27967 {
			t.Errorf("beside %s: %d of %d commits and %d of %d pairs differ, want 0 of 3356 and 0 of 27967:\n%s",
				source.name, c.commitsDiffer, c.commits, c.pairsDiffer, c.pairs, strings.Join(c.first, "\n"))
		}
		var totals struct {
			Added   int `json:"added_lines"`
			Deaths  int `json:"death_lines"`
			AtTip   int `json:"tracked_lines_at_tip"`
			Commits int `json:"commits_seen"`
		}
		refJSON(t, prefix+".json", &totals)
		if totals.Added != c.addedOther || totals.Deaths != c.deathsOther || totals.AtTip != replay.Lines() || totals.Commits != len(l.chain) {
			t.Errorf("beside %s.json: the replay adds %d lines, kills %d and holds %d at the tip over %d commits, want %+v",
				source.name, c.addedOther, c.deathsOther, replay.Lines(), len(l.chain), totals)
		}
		t.Logf("beside %s: %d commits compared, %d differ; %d pairs compared, %d differ; %d lines added, %d killed, %d held at the tip",
			source.name, c.commits, c.commitsDiffer, c.pairs, c.pairsDiffer, c.addedOther, c.deathsOther, replay.Lines())
	}
}

// Under the reference's code groups, every commit's group is lg/commits.tsv's fifth column, the
// cohort's (905 legion, 1,057 other, 1,394 none), and its fourth says it merged a pull request
// exactly when the commit carries one's facts.
func TestReferenceLegionGroups(t *testing.T) {
	l := refLegionChain(t)
	path := l.ref.Path(append(slices.Clone(refSecondRepo), "lg", "commits.tsv")...)
	rows := refRows(t, path)
	if len(rows) != len(l.chain) {
		t.Fatalf("%s holds %d commits, the chain %d", path, len(rows), len(l.chain))
	}
	counts, differ := map[string]int{}, []string{}
	for at, commit := range l.chain {
		row := rows[at]
		if len(row) != 5 || row[1] != commit.SHA {
			t.Fatalf("%s row %d is %v, not commit %s", path, at+1, row, commit.SHA)
		}
		group := "none"
		if name, ok := survival.GroupOf(commit.Change, l.groups); ok {
			group = refColumn[name]
		}
		counts[group]++
		if merged := commit.Change.PR != 0; group != row[4] || merged != (row[3] == "1") {
			differ = append(differ, fmt.Sprintf("%s (PR #%d, head %q, author %q): group %s, merge %v; the cohort says %s, %s",
				commit.SHA, commit.Change.PR, commit.Change.HeadRef, commit.Change.Author, group, merged, row[4], row[3]))
		}
	}
	if len(differ) > 0 || counts["legion"] != 905 || counts["other"] != 1057 || counts["none"] != 1394 {
		t.Errorf("%d of %d commits are in another group than the cohort's, and the groups hold %v, want 905 legion, 1057 other, 1394 none:\n%s",
			len(differ), len(l.chain), counts, refFirst(differ, 10))
	}
	t.Logf("%d commits, %d in another group than the cohort's: %d legion, %d other, %d none", len(l.chain), len(differ),
		counts["legion"], counts["other"], counts["none"])
}

// Attribution's own code, over each merged pull request's head branch, title and body alone,
// gives the issue lg/pr_issue.tsv holds for it: step 3's issues and their projects are loaded into
// the test's database with no link, so sources 1 and 2 hold nothing, and the GitHub the
// attribution reads refuses every request, so a pull request its sources 3 and 4 attribute sends
// none (no commit message was read) and one they leave unattributed is the one refused read of
// its commit messages, which IssueOf answers as no issue.
func TestReferenceIssuesAreAttributionsSourcesThreeAndFour(t *testing.T) {
	ref := survivaltest.OpenReference(t, os.Getenv(survivaltest.ReferenceDirEnv))
	if os.Getenv(storetest.DatabaseURLEnv) == "" {
		t.Fatalf("%s is unset: this test loads step 3's issues into a database of its own on that server", storetest.DatabaseURLEnv)
	}
	database := storetest.Open(t)
	ctx := context.Background()

	issues := ref.Issues(t)
	keys, projects, repos := make([]string, len(issues)), make([]string, len(issues)), make([]string, len(issues))
	numbers := make([]int32, len(issues))
	held := map[string]bool{}
	for at, issue := range issues {
		keys[at], projects[at], numbers[at], repos[at] = issue.Key, issue.Project, issue.Number, issue.Repo
		held[issue.Key] = true
	}
	made := 0
	named := slices.Compact(slices.Sorted(slices.Values(slices.Clone(projects))))
	for _, project := range named {
		tag, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ($1, $1, 'survivalref') on conflict (key) do nothing`, project)
		if err != nil {
			t.Fatalf("make the project %s: %v", project, err)
		}
		made += int(tag.RowsAffected())
	}
	tag, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, status, created_by, rank, repo)
		select u.key, u.project, u.number, u.key, 'todo', '{"kind":"system","id":"survivalref"}'::jsonb, u.key, nullif(u.repo, '')
		from unnest($1::text[], $2::text[], $3::int[], $4::text[]) as u(key, project, number, repo)`, keys, projects, numbers, repos)
	if err != nil || tag.RowsAffected() != int64(len(keys)) {
		t.Fatalf("load the %d issues of lg/issues.tsv: %v (%d loaded)", len(keys), err, tag.RowsAffected())
	}

	// The GitHub the attribution reads: every request refused, and counted.
	var asked atomic.Int64
	server := githubtest.Server(t, githubtest.Spec{
		Record: func(*http.Request) { asked.Add(1) },
		Handle: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"the reference reads no commit message"}`))
		},
	})
	client := refAppClient(t, server)
	token := func() (string, error) { return "survivalref", nil }
	attributor := delivery.NewAttributor(refs.Renames{})
	// IssueOf logs each candidate it passes over through slog.Default; the test's output keeps
	// its own lines alone.
	logtest.SetDefault(t, slog.DiscardHandler)

	want := map[int]string{}
	for _, issue := range ref.PullIssues(t) {
		want[issue.Number] = issue.Issue
	}
	var differ []string
	attributed, refused, merged := 0, 0, 0
	for _, pull := range ref.Pulls(t) {
		if !pull.Merged() {
			continue
		}
		merged++
		before := asked.Load()
		got, pages, err := attributor.IssueOf(ctx, database.Pool, client, token, delivery.PullRequestFacts{
			Repo: survivaltest.ReferenceRepo, Number: pull.Number,
			URL:   fmt.Sprintf("https://github.com/%s/pull/%d", survivaltest.ReferenceRepo, pull.Number),
			Title: pull.Title, Body: pull.Body, HeadRef: pull.HeadRef, HeadRepo: survivaltest.ReferenceRepo,
		})
		reads := asked.Load() - before
		var status *githubapp.StatusError
		switch {
		case err == nil && got != "" && pages == 0 && reads == 0:
			attributed++
		case err != nil && errors.As(err, &status) && status.Status == http.StatusForbidden && pages == 1 && reads == 1:
			refused++
			got = ""
		default:
			t.Fatalf("PR #%d: IssueOf answered %q after %d commit pages and %d requests: %v", pull.Number, got, pages, reads, err)
		}
		if wanted, ok := want[pull.Number]; !ok || got != wanted {
			differ = append(differ, fmt.Sprintf("PR #%d: attribution gives %q, pr_issue.tsv %q (listed %v)", pull.Number, got, wanted, ok))
		}
		if got != "" && !held[got] {
			t.Errorf("PR #%d is attributed %s, an issue the test did not load", pull.Number, got)
		}
	}
	if len(differ) > 0 || merged != len(want) || attributed == 0 || merged != 3105 {
		t.Errorf("%d of %d merged pull requests are attributed otherwise than pr_issue.tsv's %d lines, want 0 of 3105:\n%s",
			len(differ), merged, len(want), refFirst(differ, 10))
	}
	if total := asked.Load(); total != int64(refused) {
		t.Errorf("GitHub was asked %d times, want once for each of the %d pull requests sources 3 and 4 leave unattributed", total, refused)
	}
	t.Logf("%d issues of %d projects loaded (%d projects made, %d held already); %d merged pull requests, %d differ: %d attributed by sources 3 and 4 with no request, %d unattributed after the one refused read of their commit messages",
		len(keys), len(named), made, len(named)-made, merged, len(differ), attributed, refused)
}

// The replay's per-change output, read with the reference's own selection (scripts/16_window.py
// and scripts/replaylib.py: changes at least 30 days old at the tip, each read at its own mark,
// and the weeks counted back from the tip), gives the published figures exactly: over the five
// weeks 2026-09-09 to 2026-10-14 (weeks 4 to 8), 264,957 of 488,483 lines (54.241 %, 396
// changes) for Legion and 532,842 of 725,685 (73.426 %, 657) for the other group, and over the
// whole window 315,351 of 607,759 and 533,607 of 728,784 (12_analysis.txt). Fails below 400
// changes read.
func TestReferenceLegionSurvival(t *testing.T) {
	l := refLegionChain(t)
	out := l.onePass(t)
	tip := l.tip()
	span, whole := map[string]refFigure{}, map[string]refFigure{}
	for _, change := range out.Changes {
		name, ok := survival.GroupOf(change.ChangeFacts, l.groups)
		if !ok {
			t.Fatalf("the change %s (PR #%d) is in no group", change.SHA, change.PR)
		}
		age := tip.Sub(change.CommittedAt)
		if age < 30*refDay {
			continue
		}
		add := func(figures map[string]refFigure) {
			f := figures[name]
			f.changes++
			f.added += change.Added
			f.alive += change.AliveAtMark
			figures[name] = f
		}
		add(whole)
		if week := int(age / (7 * refDay)); week >= 4 && week <= 8 {
			add(span)
		}
	}
	if read := span["Legion"].changes + span["Other"].changes; read < 400 {
		t.Fatalf("the five weeks read %d changes, fewer than 400", read)
	}
	// The figures as 16_window.py wrote them, in the pass archive and in step 4's run of it.
	windows := map[string]map[string]json.RawMessage{}
	for _, source := range [][]string{append(slices.Clone(refPassOutput), "16_window.json"), {"lg", "an", "git", "out", "16_window.json"}} {
		var window map[string]json.RawMessage
		refJSON(t, l.ref.Path(source...), &window)
		windows[strings.Join(source, "/")] = window
	}
	for _, row := range []struct {
		figures map[string]refFigure
		group   string
		key     string
		want    refFigure
		share   string
	}{
		{span, "Legion", "span:legion:30d_alive", refFigure{396, 488483, 264957}, "54.241"},
		{span, "Other", "span:other:30d_alive", refFigure{657, 725685, 532842}, "73.426"},
		{whole, "Legion", "all:legion:30d_alive", refFigure{456, 607759, 315351}, "51.888"},
		{whole, "Other", "all:other:30d_alive", refFigure{658, 728784, 533607}, "73.219"},
	} {
		got := row.figures[row.group]
		if got != row.want || got.share() != row.share {
			t.Errorf("%s: %d of %d lines alive (%s %%) over %d changes, want %d of %d (%s %%) over %d",
				row.key, got.alive, got.added, got.share(), got.changes, row.want.alive, row.want.added, row.share, row.want.changes)
		}
		for source, window := range windows {
			var figure struct {
				Merges int `json:"merges"`
				Added  int `json:"added"`
				N      int `json:"n"`
			}
			if err := json.Unmarshal(window[row.key], &figure); err != nil {
				t.Fatalf("%s of %s: %v", row.key, source, err)
			}
			if (refFigure{figure.Merges, figure.Added, figure.N}) != got {
				t.Errorf("%s of %s reads %+v, the replay %+v", row.key, source, figure, got)
			}
		}
		t.Logf("%s: %d of %d lines alive at their marks, %s %%, over %d changes, as 16_window.json", row.key, got.alive, got.added, got.share(), got.changes)
	}
}

// Every pair rework30.py keeps on step 3's issues (a Legion merge's lines killed by a later Legion
// merge at most 30 days after it) is the replay's pair of kind rework, iteration or no issue under
// the reference's groups, in the same case, with the same lines and gap; and rework30.py with its
// 30-day line off, on 14_keys.py's keys, reads as 15_rework.py: buckets 198,399 / 11,144 / 94,655
// and 4,819 distinct pairs. Fails at any differing pair or at zero pairs. k5's negative control,
// a sub-test: the last day of the window dropped from the replay alone makes the comparison fail,
// at the pairs of that day and at no others.
func TestReferenceLegionReworkPairs(t *testing.T) {
	l := refLegionChain(t)
	out := l.onePass(t)
	script := refScriptPairs(t, l.ref.Path("lg", "an", "k5", "out", "rework30_pairs.tsv"))
	if len(script) == 0 {
		t.Fatal("rework30.py kept no pair on step 3's issues")
	}
	replay := refReplayPairs(out, "Legion", l.groups)
	if diffs := refComparePairs(script, replay); len(diffs) > 0 {
		t.Errorf("%d of %d pairs differ between rework30.py and the replay:\n%s", len(diffs), len(script), refFirst(slices.Collect(maps.Values(diffs)), 10))
	}
	cases := map[string][2]int{}
	for _, pair := range replay {
		c := cases[pair.kase]
		cases[pair.kase] = [2]int{c[0] + 1, c[1] + pair.lines}
	}
	t.Logf("rework30.py keeps %d pairs on step 3's issues, the replay %d, 0 differ: same %d pairs %d lines, different %d pairs %d lines, unresolved %d pairs %d lines",
		len(script), len(replay), cases["same"][0], cases["same"][1], cases["different"][0], cases["different"][1], cases["unresolved"][0], cases["unresolved"][1])

	type rework struct {
		Buckets  map[string][2]int `json:"buckets"`
		Distinct int               `json:"distinct_pairs"`
	}
	var off, published rework
	refJSON(t, l.ref.Path("lg", "an", "git", "rework30-off", "rework30.json"), &off)
	refJSON(t, l.ref.Path(append(slices.Clone(refPassOutput), "15_rework.json")...), &published)
	if off.Buckets["different"][0] != 198399 || off.Buckets["same"][0] != 11144 || off.Buckets["unresolved"][0] != 94655 ||
		off.Distinct != 4819 || fmt.Sprint(off) != fmt.Sprint(published) {
		t.Errorf("rework30.py --no-30d on 14_keys.py's keys reads %+v, want buckets 198399 / 11144 / 94655 and 4819 distinct pairs, as 15_rework.json's %+v", off, published)
	}
	t.Logf("rework30.py --no-30d on 14_keys.py's keys: buckets different %d / same %d / unresolved %d, distinct_pairs %d, as 15_rework.json",
		off.Buckets["different"][0], off.Buckets["same"][0], off.Buckets["unresolved"][0], off.Distinct)

	t.Run("the last day dropped from the replay fails the comparison", func(t *testing.T) {
		last := l.window[len(l.window)-1]
		dropped := map[string]bool{}
		var chain []survival.Commit
		for _, commit := range l.chain {
			if survival.DayOf(commit.CommittedAt).Equal(last) {
				dropped[commit.SHA] = true
				continue
			}
			chain = append(chain, commit)
		}
		doctored, _ := l.measure(t, l.window[:len(l.window)-1], chain)
		diffs := refComparePairs(script, refReplayPairs(doctored, "Legion", l.groups))
		ofTheDay, lines := 0, 0
		for key, pair := range script {
			if dropped[key.adder] || dropped[key.remover] {
				ofTheDay++
				lines += pair.lines
			}
		}
		if len(diffs) == 0 {
			t.Fatalf("with %s's %d commits dropped from the replay the comparison still passes: %d pairs alike", last.Format(time.DateOnly), len(dropped), len(script))
		}
		for key, diff := range diffs {
			if !dropped[key.adder] && !dropped[key.remover] {
				t.Errorf("a pair of no commit of %s differs: %s", last.Format(time.DateOnly), diff)
			}
		}
		if len(diffs) != ofTheDay {
			t.Errorf("%d pairs differ, want the %d pairs of the script's with a commit of %s", len(diffs), ofTheDay, last.Format(time.DateOnly))
		}
		t.Logf("with %s dropped from the replay alone (%d commits), the comparison FAILS as it must: %d of %d pairs differ, %d lines, each a pair with a commit of that day; the first: %s",
			last.Format(time.DateOnly), len(dropped), len(diffs), len(script), lines, refFirst(slices.Collect(maps.Values(diffs)), 1))
	})
}

// The window measured in runs of 28 days, and of 7, each over its own replay range, gives every
// change and every pair of the single pass, and nothing else.
func TestReferenceBatchedRunsEqualOnePass(t *testing.T) {
	l := refLegionChain(t)
	pass := l.onePass(t)
	for _, size := range []int{28, 7} {
		var union survival.MeasureOutput
		runs, reads := 0, 0
		var bytes int64
		for start := 0; start < len(l.window); start += size {
			out, reader := l.measure(t, l.window[start:min(start+size, len(l.window))], l.chain)
			union.Changes = append(union.Changes, out.Changes...)
			union.Pairs = append(union.Pairs, out.Pairs...)
			runs++
			reads += reader.Reads()
			bytes += reader.Total()
			for _, day := range out.Days {
				if day.State != survival.DayMeasured {
					t.Fatalf("a run of %d days left %s %s", size, day.Day.Format(time.DateOnly), day.State)
				}
			}
		}
		if diffs := refDiffChanges(pass, union); len(diffs) > 0 {
			t.Errorf("runs of %d days differ from the single pass at %d changes and pairs:\n%s", size, len(diffs), refFirst(diffs, 10))
		}
		t.Logf("runs of %d days: %d runs, %d patches and %d bytes read; %d changes and %d pairs, as the single pass's %d and %d",
			size, runs, reads, bytes, len(union.Changes), len(union.Pairs), len(pass.Changes), len(pass.Pairs))
	}
}

// Over the second repository's 2,518 diffs, as GitHub's diff route served them (prepare.sh's
// step 5) and ParseDiff reads them, every commit's added lines and every pair's lines equal the
// archive's ai/r_diff_added.tsv and ai/r_diff_deaths.tsv, and differ from local git's
// ai/r_git_* by the 15 lines of 1,497,629 over 23 commits measured there, and at the pairs the
// archive's ai/compare_git_diff.json counts.
func TestReferenceSecondRepositoryPerCommit(t *testing.T) {
	ref := survivaltest.OpenReference(t, os.Getenv(survivaltest.ReferenceDirEnv))
	ai := append(slices.Clone(refSecondRepo), "ai")
	path := ref.Path(append(slices.Clone(ai), "commits.tsv")...)
	rows := refRows(t, path)
	shas := refRows(t, ref.Path("ai", "shas.txt"))
	if len(rows) != 2518 || len(shas) != len(rows) {
		t.Fatalf("%s holds %d commits and ai/shas.txt %d, want 2518 each", path, len(rows), len(shas))
	}
	var chain []survival.Commit
	for at, row := range rows {
		if len(row) != 5 || row[1] != shas[at][0] || refInt(t, path, row[0]) != at+1 {
			t.Fatalf("%s row %d is %v, where ai/shas.txt names %v", path, at+1, row, shas[at])
		}
		committed, err := time.Parse(time.RFC3339, row[2])
		if err != nil {
			t.Fatalf("%s row %d: %v", path, at+1, err)
		}
		chain = append(chain, survival.Commit{SHA: row[1], CommittedAt: committed.UTC()})
	}
	replay := refReplayEvery(t, chain, func(sha string) string { return ref.Path("ai", "diffs", sha[:2], sha+".diff.gz") })
	got := refCountsOf(replay, chain)

	route := refCompare(t, len(chain), refReadCounts(t, ref.Path(append(slices.Clone(ai), "r_diff")...)), got)
	if route.commitsDiffer != 0 || route.pairsDiffer != 0 {
		t.Errorf("beside ai/r_diff_*: %d of %d commits and %d of %d pairs differ, want none:\n%s",
			route.commitsDiffer, route.commits, route.pairsDiffer, route.pairs, strings.Join(route.first, "\n"))
	}
	t.Logf("beside ai/r_diff_*, the diff route's replay.py: %d commits compared, %d differ; %d pairs compared, %d differ; %d lines added, %d killed",
		route.commits, route.commitsDiffer, route.pairs, route.pairsDiffer, route.addedOther, route.deathsOther)

	git := refCompare(t, len(chain), refReadCounts(t, ref.Path(append(slices.Clone(ai), "r_git")...)), got)
	var measured struct {
		Commits int `json:"commits_with_added_diff"`
		All     map[string]struct {
			Base  int `json:"added_base"`
			Other int `json:"added_other"`
		} `json:"all_commits"`
		Deaths struct {
			Base       int `json:"death_lines_base"`
			Other      int `json:"death_lines_other"`
			OnlyBase   int `json:"pairs_only_in_base"`
			OnlyOther  int `json:"pairs_only_in_other"`
			BothDiffer int `json:"pairs_in_both_with_different_counts"`
		} `json:"deaths"`
	}
	refJSON(t, ref.Path(append(slices.Clone(ai), "compare_git_diff.json")...), &measured)
	if git.commitsDiffer != 23 || git.added != 1497629 || git.addedOther-git.added != 15 ||
		git.commitsDiffer != measured.Commits || git.added != measured.All["ALL"].Base || git.addedOther != measured.All["ALL"].Other ||
		git.deaths != measured.Deaths.Base || git.deathsOther != measured.Deaths.Other ||
		git.pairsDiffer != measured.Deaths.OnlyBase+measured.Deaths.OnlyOther+measured.Deaths.BothDiffer {
		t.Errorf("beside local git's ai/r_git_*: %d commits and %d pairs differ, and the replay adds %d lines and kills %d where git adds %d and kills %d; want 23 commits and 15 lines more than 1497629, as ai/compare_git_diff.json's %+v",
			git.commitsDiffer, git.pairsDiffer, git.addedOther, git.deathsOther, git.added, git.deaths, measured)
	}
	t.Logf("beside local git's ai/r_git_*: %d of %d commits differ in added lines, %d lines added against git's %d (%+d); %d of %d pairs differ, %d lines killed against git's %d",
		git.commitsDiffer, git.commits, git.addedOther, git.added, git.addedOther-git.added, git.pairsDiffer, git.pairs, git.deathsOther, git.deaths)
}

// One 28-day run of the window, the first, streamed from the files, holds at most eight times its
// largest patch plus 64 MiB of patch bytes at once (R4's bound), and its live heap after garbage
// collection, sampled through the run, stays at most 1 GiB.
func TestReferenceReplayCost(t *testing.T) {
	l := refLegionChain(t)
	days := l.window[:28]
	runtime.GC()
	stop := refHeapSampler()
	started := time.Now()
	out, reader := l.measure(t, days, l.chain)
	heap := stop()
	bound := 8*reader.Largest() + 64<<20
	if reader.PeakHeld() > bound {
		t.Errorf("the run held %d patch bytes at once, past the bound of %d (8 × its largest patch of %d + 64 MiB)", reader.PeakHeld(), bound, reader.Largest())
	}
	if heap > 1<<30 {
		t.Errorf("the live heap after GC reached %d bytes, past 1 GiB", heap)
	}
	t.Logf("one 28-day run, %s to %s: %d patches, %d bytes, in %s; largest patch %d bytes; peak patch bytes held %d (bound %d); live heap after GC at most %d bytes (%.1f MiB, bound 1 GiB); peak %d tracked lines; %d changes, %d pairs",
		days[0].Format(time.DateOnly), days[len(days)-1].Format(time.DateOnly), reader.Reads(), reader.Total(), time.Since(started).Round(time.Millisecond),
		reader.Largest(), reader.PeakHeld(), bound, heap, float64(heap)/(1<<20), out.PeakLines, len(out.Changes), len(out.Pairs))
}

// refJSON decodes the JSON file at path into v.
func refJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

// refAppClient is an App client of server, signed with a key made for the test, built as the
// server builds its own (githubapp.New with the base URL).
func refAppClient(t *testing.T, server string) *githubapp.Client {
	t.Helper()
	_, encoded := githubapptest.Key(t)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.survivalref", PEM: encoded}, server)
	if err != nil || client == nil {
		t.Fatalf("the App client: %v", err)
	}
	return client
}
