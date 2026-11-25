package workflow

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

const (
	modelA = "stand-in/model-a"
	modelB = "stand-in/model-b"
)

// totalsOf is what a shim counted of one launch: its set model, and each model with its calls.
func totalsOf(setModel string, counts ...shimwire.ModelCount) *shimwire.Totals {
	return &shimwire.Totals{Read: 1, SetModel: setModel, Counts: counts}
}

// calls is model with n calls, in the shim's count list.
func calls(model string, n uint64) shimwire.ModelCount {
	return shimwire.ModelCount{Model: model, Calls: n}
}

// answered is the answers of a record, each a model and its calls.
func answered(answers ...record.ModelCalls) *[]record.ModelCalls {
	if answers == nil {
		answers = []record.ModelCalls{}
	}
	return &answers
}

// fromZero is the start of a launch that began during the step.
var fromZero = &shimwire.Totals{}

// Each case of a step's calls is a set of launch spans, and countStep gives its set model and its
// answers. A start or an end not known, a name outside the form, or 17 models leave the counts
// unknown. Two set models name none. A step with no call knows an empty list. A step that starts
// from a newer read a committed fact kept counts from that read.
func TestCountStepGivesEachRowsArithmetic(t *testing.T) {
	t.Parallel()
	seventeen := make([]shimwire.ModelCount, 0, 17)
	for i := range 17 {
		seventeen = append(seventeen, calls(fmt.Sprintf("stand-in/m-%02d", i), 1))
	}
	outside := totalsOf(modelA, calls(modelA, 2))
	outside.OutsideForm = 1
	overflowed := totalsOf(modelA, calls(modelA, 2))
	overflowed.Overflowed = true
	for _, tc := range []struct {
		name     string
		spans    []launchSpan
		setModel string
		answers  *[]record.ModelCalls
	}{
		{"one launch from zero makes A 3 and B 1", []launchSpan{{setModel: modelA, start: fromZero, end: totalsOf(modelA, calls(modelA, 3), calls(modelB, 1))}},
			modelA, answered(record.ModelCalls{Model: modelA, Calls: 3}, record.ModelCalls{Model: modelB, Calls: 1})},
		{"A 5 in an earlier step, then A 2", []launchSpan{{setModel: modelA, start: totalsOf(modelA, calls(modelA, 5)), end: totalsOf(modelA, calls(modelA, 7))}},
			modelA, answered(record.ModelCalls{Model: modelA, Calls: 2})},
		{"A 5 before, A 2 in it, a restart, A 1 more", []launchSpan{{setModel: modelA, start: totalsOf(modelA, calls(modelA, 5)), end: totalsOf(modelA, calls(modelA, 8))}},
			modelA, answered(record.ModelCalls{Model: modelA, Calls: 3})},
		{"the first launch's final totals never came", []launchSpan{
			{setModel: modelA, start: totalsOf(modelA, calls(modelA, 5))},
			{setModel: modelA, start: fromZero, end: totalsOf(modelA, calls(modelA, 1))}}, modelA, nil},
		{"the first launch's final totals came", []launchSpan{
			{setModel: modelA, start: totalsOf(modelA, calls(modelA, 5)), end: totalsOf(modelA, calls(modelA, 6))},
			{setModel: modelA, start: fromZero, end: totalsOf(modelA, calls(modelA, 2))}}, modelA, answered(record.ModelCalls{Model: modelA, Calls: 3})},
		{"no connection at the end", []launchSpan{{setModel: modelA, start: totalsOf(modelA, calls(modelA, 5))}}, modelA, nil},
		{"a launch with no answer at the end", []launchSpan{{setModel: modelA, start: totalsOf(modelA, calls(modelA, 5))}}, modelA, nil},
		{"a model outside the form answered", []launchSpan{{setModel: modelA, start: fromZero, end: outside}}, modelA, nil},
		{"17 models answered", []launchSpan{{setModel: modelA, start: fromZero, end: totalsOf(modelA, seventeen...)}}, modelA, nil},
		{"two launches set to A and to B", []launchSpan{
			{setModel: modelA, start: totalsOf(modelA, calls(modelA, 5)), end: totalsOf(modelA, calls(modelA, 6))},
			{setModel: modelB, start: fromZero, end: totalsOf(modelB, calls(modelB, 1))}}, "", answered(record.ModelCalls{Model: modelA, Calls: 1}, record.ModelCalls{Model: modelB, Calls: 1})},
		{"no call", []launchSpan{{setModel: modelA, start: totalsOf(modelA, calls(modelA, 5)), end: totalsOf(modelA, calls(modelA, 5))}}, modelA, answered()},
		{"relaunched between the read and the start", []launchSpan{{setModel: modelA, end: totalsOf(modelA, calls(modelA, 1))}}, modelA, nil},
		{"a step ended by a newer read holds A 2", []launchSpan{{setModel: modelA, start: totalsOf(modelA, calls(modelA, 1)), end: totalsOf(modelA, calls(modelA, 3))}},
			modelA, answered(record.ModelCalls{Model: modelA, Calls: 2})},
		{"the next step starts from that kept read and gives A 1", []launchSpan{{setModel: modelA, start: totalsOf(modelA, calls(modelA, 3)), end: totalsOf(modelA, calls(modelA, 4))}},
			modelA, answered(record.ModelCalls{Model: modelA, Calls: 1})},
		{"a settle that read nothing", []launchSpan{{setModel: modelA, start: totalsOf(modelA, calls(modelA, 5))}}, modelA, nil},
		{"two launches set to A give A", []launchSpan{
			{setModel: modelA, start: fromZero, end: totalsOf(modelA)},
			{setModel: modelA, start: fromZero, end: totalsOf(modelA)}}, modelA, answered()},
		{"A and a launch whose set model is not known give none", []launchSpan{
			{setModel: modelA, start: fromZero, end: totalsOf(modelA)},
			{start: fromZero, end: totalsOf("")}}, "", answered()},
		{"no launch is no set model and no call", nil, "", answered()},
		{"a count that fell is not known", []launchSpan{{setModel: modelA, start: totalsOf(modelA, calls(modelA, 5)), end: totalsOf(modelA, calls(modelA, 4))}}, modelA, nil},
		{"a model gone from the end is a count that fell", []launchSpan{{setModel: modelA, start: totalsOf(modelA, calls(modelB, 1)), end: totalsOf(modelA, calls(modelA, 1))}}, modelA, nil},
		{"totals that overflowed are not known", []launchSpan{{setModel: modelA, start: fromZero, end: overflowed}}, modelA, nil},
		{"answers run most calls first, then by model", []launchSpan{{setModel: modelA, start: fromZero, end: totalsOf(modelA, calls(modelB, 2), calls(modelA, 2), calls("stand-in/model-c", 4))}},
			modelA, answered(record.ModelCalls{Model: "stand-in/model-c", Calls: 4}, record.ModelCalls{Model: modelA, Calls: 2}, record.ModelCalls{Model: modelB, Calls: 2})},
		{"a count list naming a model outside the form is not known", []launchSpan{{setModel: modelA, start: fromZero, end: totalsOf(modelA, calls("stand-in/model with space", 1))}},
			modelA, nil},
		{"a set model outside the form is none", []launchSpan{{setModel: "a b", start: fromZero, end: totalsOf("a b")}}, "", answered()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setModel, answers := countStep(tc.spans)
			if setModel != tc.setModel || !reflect.DeepEqual(answers, tc.answers) {
				t.Fatalf("countStep = %q, %s; want %q, %s", setModel, shownAnswers(answers), tc.setModel, shownAnswers(tc.answers))
			}
		})
	}
}

// A fact of a pull request names the issue its handlers apply it to, and reads that issue's
// agents. Its pull request is found under a renamed repository's other name too. A pull request
// whose issue is on another repository names no issue, since the fact applies to none.
func TestAFactOfAPullRequestNamesTheIssueItsHandlersApplyItTo(t *testing.T) {
	t.Parallel()
	issueOf := func(t *testing.T, pool *pgxpool.Pool, engine *Engine, fact intake.Fact) string {
		t.Helper()
		var issue string
		seedRecord(t, pool, func(tx pgx.Tx) error {
			var err error
			issue, err = engine.factIssue(context.Background(), tx, fact)
			return err
		})
		return issue
	}
	t.Run("a pull request recorded under a renamed repository's old name", func(t *testing.T) {
		t.Parallel()
		pool := migratedPool(t)
		seedRenamedTree(t, pool, "sjawhar/legion")
		engine := renamedEngine(renamedLegion)
		for _, fact := range []intake.Fact{
			intake.PullRequestReview{Repo: "cybermonklabs/legion", Number: 42},
			intake.Push{Repo: "cybermonklabs/legion", Branch: "legion/LEGION-209"},
		} {
			if got := issueOf(t, pool, engine, fact); got != "LEGION-209" {
				t.Fatalf("%T naming cybermonklabs/legion names issue %q, want LEGION-209, whose pull request is recorded on sjawhar/legion", fact, got)
			}
		}
	})
	t.Run("a pull request whose issue is on another repository", func(t *testing.T) {
		t.Parallel()
		pool := migratedPool(t)
		openTree(t, pool)
		parent := "LEGION-208"
		seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent,
			Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "V", LastDispatchSeq: 1})
		seedPR(t, pool, record.PullRequest{Issue: "LEGION-209", Repo: "acme/other", Number: 42, Branch: "legion/LEGION-209", HeadSHA: "head",
			HeadUpdatedAt: time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC), Failing: []string{}, Generation: 1, State: record.PullRequestOpen})
		engine := testEngine(config.DesignGateRootIssues, nil)
		for _, fact := range []intake.Fact{
			intake.PullRequestReview{Repo: "acme/other", Number: 42},
			intake.Push{Repo: "acme/other", Branch: "legion/LEGION-209"},
		} {
			if got := issueOf(t, pool, engine, fact); got != "" {
				t.Fatalf("%T naming acme/other names issue %q, want none: the issue is on acme/widgets, so the fact applies to no issue", fact, got)
			}
		}
	})
}

// shownAnswers is answers as a test failure names them: unknown for nil, else each model and its
// calls. So an empty list and nil read apart.
func shownAnswers(answers *[]record.ModelCalls) string {
	if answers == nil {
		return "unknown"
	}
	return fmt.Sprintf("%v", *answers)
}

// A step's launches are the start's live launch, and each launch from the first that counts from
// zero (record.StartTotals.FromZero) to the claim's generation now. A launch still starting is one
// too. A launch absent at the start, whose row the store wrote only after it, counts from zero. A
// live launch's end is the fact's read, or the newer read a committed fact kept. An ended launch's
// end is its final totals. A launch still starting at the end has no end.
func TestTheLaunchesOfAStepAreItsStartsAndEachLaunchFromZero(t *testing.T) {
	t.Parallel()
	agent := claim.Token("legion-legion-legion-208-implementer")
	read := func(n uint64, counts ...shimwire.ModelCount) *record.LaunchRead {
		totals := totalsOf(modelA, counts...)
		totals.Read = n
		return &record.LaunchRead{Totals: *totals, At: time.Unix(int64(n), 0).UTC()}
	}
	key := func(generation uint64) record.LaunchKey {
		return record.LaunchKey{Claim: agent, Generation: generation}
	}
	for _, tc := range []struct {
		name     string
		began    record.StartTotals
		now      record.AgentLaunches
		starting bool
		reads    FactReads
		want     []launchSpan
	}{
		{name: "a launch absent at the start counts from zero",
			began: record.StartTotals{Generation: 0, FromZero: 0},
			now:   record.AgentLaunches{Generation: 1, Live: true, Launches: []record.Launch{{Generation: 1}}},
			reads: FactReads{key(1): *read(2, calls(modelA, 3))},
			want:  []launchSpan{{setModel: modelA, start: fromZero, end: &read(2, calls(modelA, 3)).Totals}}},
		{name: "a live launch's end is the newer kept read",
			began: record.StartTotals{Generation: 2, Live: true, Read: read(1, calls(modelA, 1)), FromZero: 3},
			now:   record.AgentLaunches{Generation: 2, Live: true, Launches: []record.Launch{{Generation: 2, Newest: read(5, calls(modelA, 4))}}},
			reads: FactReads{key(2): *read(4, calls(modelA, 3))},
			want:  []launchSpan{{setModel: modelA, start: &read(1, calls(modelA, 1)).Totals, end: &read(5, calls(modelA, 4)).Totals}}},
		{name: "a live launch the fact did not read has no end",
			began: record.StartTotals{Generation: 2, Live: true, Read: read(1, calls(modelA, 1)), FromZero: 3},
			now:   record.AgentLaunches{Generation: 2, Live: true, Launches: []record.Launch{{Generation: 2, Newest: read(5, calls(modelA, 4))}}},
			want:  []launchSpan{{setModel: modelA, start: &read(1, calls(modelA, 1)).Totals}}},
		{name: "an ended launch ends at its final totals, and the next counts from zero",
			began: record.StartTotals{Generation: 2, Live: true, Read: read(1, calls(modelA, 1)), FromZero: 3},
			now: record.AgentLaunches{Generation: 3, Live: true, Launches: []record.Launch{
				{Generation: 2, Final: read(3, calls(modelA, 2))}, {Generation: 3}}},
			reads: FactReads{key(3): *read(1, calls(modelA, 1))},
			want: []launchSpan{
				{setModel: modelA, start: &read(1, calls(modelA, 1)).Totals, end: &read(3, calls(modelA, 2)).Totals},
				{setModel: modelA, start: fromZero, end: &read(1, calls(modelA, 1)).Totals}}},
		{name: "a launch that ended before the step is not the step's",
			began: record.StartTotals{Generation: 2, FromZero: 3},
			now:   record.AgentLaunches{Generation: 3, Live: true, Launches: []record.Launch{{Generation: 2, Final: read(3)}, {Generation: 3}}},
			reads: FactReads{key(3): *read(1)},
			want:  []launchSpan{{setModel: modelA, start: fromZero, end: &read(1).Totals}}},
		{name: "a launch still starting at the end has no end",
			began:    record.StartTotals{Generation: 0, FromZero: 0},
			now:      record.AgentLaunches{Generation: 1},
			starting: true,
			want:     []launchSpan{{start: fromZero}}},
		{name: "a generation with no row and no launch starting is no launch",
			began: record.StartTotals{Generation: 0, FromZero: 0},
			now:   record.AgentLaunches{Generation: 1}},
		{name: "a launch still starting after one that ended has no end",
			began: record.StartTotals{Generation: 2, Live: true, Read: read(1, calls(modelA, 1)), FromZero: 3},
			now: record.AgentLaunches{Generation: 3, Launches: []record.Launch{
				{Generation: 2, Final: read(3, calls(modelA, 2))}}},
			starting: true,
			want: []launchSpan{
				{setModel: modelA, start: &read(1, calls(modelA, 1)).Totals, end: &read(3, calls(modelA, 2)).Totals},
				{start: fromZero}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := stepSpans(agent, tc.began, tc.now, tc.starting, tc.reads)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("stepSpans = %+v, want %+v", got, tc.want)
			}
		})
	}
}
