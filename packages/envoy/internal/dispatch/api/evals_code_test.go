package api

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/fold"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// methodLink is a link a method text gives to an issue's document: dispatch://<key>/artifact/<slug>.
var methodLink = regexp.MustCompile(`dispatch://([A-Z][A-Z0-9]*-[0-9]+)/artifact/([a-z0-9]+(?:-[a-z0-9]+)*)`)

// TestBothCodeEvalsAreInEveryProject (dispatch://INFRA-3206 k1, R1): two projects created through
// the API, one whose code days the code writer measured and one with none, each hold both code
// evals, which the list read of the project's delivery evals answers: kind delivery, the source
// naming the fold that computes each (computed:delivery/survival/fold.FoldSurvival and
// .FoldRework), version 1 by the seed, and no stored field restating the rule (no number rule, no
// target, no judge), with the status series listed and, for the measured project, each code
// group's series and the agents' share. Each one's doc_block is a block of the project's evals-md,
// the detail read answers that block's method, and the method links
// dispatch://INFRA-3117/artifact/method-md and dispatch://INFRA-3117/artifact/analysis-md, both of
// which the issues route follows to a document. It fails below the four rows, two evals in two
// projects.
func TestBothCodeEvalsAreInEveryProject(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	ctx := t.Context()
	measured, waiting := "MEASURED", "WAITING"
	createTestProject(t, handler, measured, waiting, "INFRA")
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	// The issue whose documents the methods link, with both documents.
	exec("write INFRA-3117", `
		insert into issues (key, project_key, number, title, status, rank, priority, created_by)
		values ('INFRA-3117', 'INFRA', 3117, 'Code survival, measured', 'done', 'U3117', 2, '{"kind":"user","id":"alice"}')`)
	for _, name := range []string{"method.md", "analysis.md"} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/INFRA-3117/artifacts", map[string]string{
			"name": name, "content": "# " + name + "\n\nThe document the code evals' methods link.\n",
		}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("upload %s to INFRA-3117: status=%d body=%s", name, response.Code, response.Body.String())
		}
	}

	// The measured project's three days, under two code groups, as the code writer writes them.
	first := measures.Midnight(time.Now()).AddDate(0, 0, -40)
	groups := []survival.Group{{Name: "agents", BranchPrefixes: []string{"agent/"}}, {Name: "others"}}
	change := func(n, number int, head, issue string, added, alive int) fold.Change {
		issueProject := ""
		if issue != "" {
			issueProject = measured
		}
		return fold.Change{Repo: "acme/code", IssueProject: issueProject, Change: survival.Change{
			SHA: fmt.Sprintf("%040x", number), Day: first.AddDate(0, 0, n), CommittedAt: first.AddDate(0, 0, n).Add(9 * time.Hour),
			ChangeFacts: survival.ChangeFacts{PR: number, HeadRef: head, Author: "someone", IssueKey: issue}, Added: added, AliveAtMark: alive,
		}}
	}
	input := fold.BuildInput{Project: fold.Project{Key: measured, DefaultRepo: "acme/code"}, Groups: groups,
		Changes: []fold.Change{change(0, 1, "agent/one", measured+"-1", 10, 8), change(1, 2, "other/two", "", 4, 4)}}
	days := []time.Time{first, first.AddDate(0, 0, 1), first.AddDate(0, 0, 2)}
	for _, day := range days {
		input.Days = append(input.Days, fold.Day{Day: day, State: survival.DayMeasured})
	}
	survivalParts, reworkParts := fold.BuildDays(input)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := evals.WriteCodeGroupSeriesTx(ctx, tx, measured, []string{"agents", "others"}); err != nil {
		t.Fatalf("write %s's code group series: %v", measured, err)
	}
	ids := []string{evals.EvalID(measured, evals.SlugCodeSurvival), evals.EvalID(measured, evals.SlugPostMergeRework)}
	if err := evals.WriteCodeDaysTx(ctx, tx, measured, ids, days, evals.CodeDayRows(measured, "", survivalParts, reworkParts), testServerBuild); err != nil {
		t.Fatalf("write %s's code days: %v", measured, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	delivery.DrainAfterCommit(ctx, database.Pool, "the code days' changes could not be drained")

	window := url.Values{"from": {first.Format(time.RFC3339)}, "to": {first.AddDate(0, 0, 28).Format(time.RFC3339)}}
	query := url.Values{"kind": {evals.KindDelivery}, "limit": {"200"}, "from": window["from"], "to": window["to"]}.Encode()
	type want struct {
		slug, source, direction, fold string
		bases                         []string
	}
	codeEvals := []want{
		{evals.SlugCodeSurvival, evals.SourceFoldSurvival, evals.DirectionHigherBetter, "fold.FoldSurvival", []string{fold.SeriesSurvival}},
		{evals.SlugPostMergeRework, evals.SourceFoldPostMergeRework, evals.DirectionLowerBetter, "fold.FoldRework",
			[]string{fold.SeriesRework, fold.SeriesIteration, fold.SeriesNoIssue, fold.SeriesOther}},
	}
	rows, followed := 0, 0
	for _, project := range []string{measured, waiting} {
		list := evalGet[evalListResponse](t, handler, "/api/v1/projects/"+project+"/evals?"+query)
		document := evalGet[struct {
			Markdown string                `json:"markdown"`
			Blocks   []model.ArtifactBlock `json:"blocks"`
		}](t, handler, "/api/v1/projects/"+project+"/artifacts/"+evals.DocumentSlug+"/text?blocks=true")
		for _, code := range codeEvals {
			item := findEval(t, list.Items, project, code.slug)
			definition := item.Definition
			what := definition.EvalID
			if definition.Kind != evals.KindDelivery || definition.Source != code.source || !definition.Computed() {
				t.Errorf("%s is of kind %s with source %s, want kind delivery and source %s", what, definition.Kind, definition.Source, code.source)
			}
			if definition.NumberRule != "" || definition.Target != nil || len(definition.TargetJudges) != 0 || definition.TargetInclusive {
				t.Errorf("%s stores number rule %q, target %v and judges %v, want none: its source is its rule", what, definition.NumberRule,
					definition.Target, definition.TargetJudges)
			}
			if definition.Version != 1 || definition.WrittenVia != evals.ViaSeed || definition.State != evals.StateActive || definition.Direction != code.direction ||
				definition.Unit != "ratio" || definition.Name == "" || definition.Question == "" || definition.Decision == "" || definition.IssueKey != "" ||
				len(definition.Links) != 0 || definition.Answer != "" {
				t.Errorf("%s holds %+v, want version 1 by the seed, active, %s, in ratio, with its name, question and decision and nothing a person sets",
					what, definition, code.direction)
			}
			if len(definition.FailureModes) == 0 || slices.ContainsFunc(definition.FailureModes, func(mode evals.FailureMode) bool { return mode.Mode == "" || mode.Guard == "" }) {
				t.Errorf("%s holds the failure modes %+v, want each with its guard", what, definition.FailureModes)
			}
			var series []string
			for _, one := range item.Series {
				series = append(series, one.Series.Series)
				if !one.Listed || one.IsPrimary {
					t.Errorf("%s's series %s is listed %t and primary %t, want listed and not primary", what, one.Series.Series, one.Listed, one.IsPrimary)
				}
			}
			wantSeries := []string{fold.SeriesStatus}
			if project == measured {
				for _, group := range []string{"agents", "others"} {
					for _, base := range code.bases {
						wantSeries = append(wantSeries, fold.GroupSeries(base, group))
					}
				}
			}
			slices.Sort(series)
			if slices.Sort(wantSeries); !slices.Equal(series, wantSeries) {
				t.Errorf("%s states the series %v, want %v", what, series, wantSeries)
			}
			status := seriesValues(t, item, fold.SeriesStatus)
			switch project {
			case measured:
				if measuredDays := status.Window["measured_days"]; measuredDays == nil || *measuredDays != 3 {
					t.Errorf("%s's status reads measured_days %v, want 3", what, measuredDays)
				}
				share := seriesValues(t, item, fold.GroupSeries(code.bases[0], "agents")).Window["share"]
				if wantShare := map[string]float64{evals.SlugCodeSurvival: 0.8, evals.SlugPostMergeRework: 0}[code.slug]; share == nil || *share != wantShare {
					t.Errorf("%s reads the agents' share %v, want %v", what, share, wantShare)
				}
			default:
				if status.Window["measured_days"] != nil || status.DataSince != nil {
					t.Errorf("%s, of a project with no measured day, reads measured_days %v since %v, want no data", what, status.Window["measured_days"], status.DataSince)
				}
			}

			if !slices.ContainsFunc(document.Blocks, func(block model.ArtifactBlock) bool {
				return block.ID == definition.DocBlock || slices.Contains(block.DescendantIDs, definition.DocBlock)
			}) {
				t.Errorf("%s's doc_block %q is no block of %s's %s", what, definition.DocBlock, project, evals.DocumentSlug)
			}
			detail := evalGet[evalDetailResponse](t, handler, "/api/v1/projects/"+project+"/evals/"+code.slug+"?"+window.Encode())
			if detail.DocText == nil {
				t.Errorf("%s's detail read answers no method, want the block %s", what, definition.DocBlock)
				continue
			}
			method := *detail.DocText
			if !strings.Contains(method, code.fold) || !strings.Contains(document.Markdown, strings.TrimSpace(strings.SplitN(method, "\n", 2)[0])) {
				t.Errorf("%s's method %.200q names no %s, or its section in %s does not hold it", what, method, code.fold, evals.DocumentSlug)
			}
			var links []string
			for _, link := range methodLink.FindAllStringSubmatch(method, -1) {
				if slices.Contains(links, link[0]) {
					continue
				}
				links = append(links, link[0])
				target := "/api/v1/issues/" + link[1] + "/artifacts/" + link[2]
				response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
				if response.Code != http.StatusOK {
					t.Errorf("%s links %s, and GET %s answers %d %s", what, link[0], target, response.Code, response.Body.String())
					continue
				}
				if read := decodeBody[artifactRead](t, response); read.Slug != link[2] || read.Kind != "doc" {
					t.Errorf("%s links %s, which reads the artifact %s of kind %s", what, link[0], read.Slug, read.Kind)
				}
				followed++
			}
			slices.Sort(links)
			if wantLinks := []string{"dispatch://INFRA-3117/artifact/analysis-md", "dispatch://INFRA-3117/artifact/method-md"}; !slices.Equal(links, wantLinks) {
				t.Errorf("%s's method links %v, want %v", what, links, wantLinks)
			}
			rows++
		}
	}
	if rows < 4 || followed < 8 {
		t.Fatalf("checked %d code evals and followed %d links, want both evals in both projects, four rows, and both links of each, eight", rows, followed)
	}
	t.Logf("%d code evals in 2 projects, each computed with no stored rule, its method block in the project's %s, and %d links followed", rows,
		evals.DocumentSlug, followed)
}
