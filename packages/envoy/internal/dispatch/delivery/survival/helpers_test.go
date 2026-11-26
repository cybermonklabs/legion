package survival_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/survivaltest"
)

// epoch is the UTC midnight the tests count their days from.
var epoch = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// day is the UTC midnight n days after epoch.
func day(n int) time.Time { return epoch.AddDate(0, 0, n) }

// at is the moment hour:minute of day n.
func at(n, hour, minute int) time.Time {
	return day(n).Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
}

// days is the UTC midnights of days from to to, both included.
func days(from, to int) []time.Time {
	var out []time.Time
	for n := from; n <= to; n++ {
		out = append(out, day(n))
	}
	return out
}

// replayed is a repository's first-parent chain as the computation reads it: each commit with the
// pull request facts facts gives its SHA (none for a commit facts does not name), and each
// commit's patch parsed.
type replayed struct {
	chain   []survival.Commit
	patches [][]survival.FileDiff
}

// replay reads the chain that ends at the repository's head, its patches as git log writes them.
func replay(t *testing.T, repo *survivaltest.Repo, facts map[string]survival.ChangeFacts) replayed {
	t.Helper()
	var out replayed
	for _, commit := range repo.FirstParents(repo.Head()) {
		files, err := survival.ParseDiff(commit.Patch)
		if err != nil {
			t.Fatalf("parse the patch of %s: %v", commit.SHA, err)
		}
		out.chain = append(out.chain, survival.Commit{SHA: commit.SHA, Parents: commit.Parents,
			CommittedAt: commit.CommittedAt, Change: facts[commit.SHA]})
		out.patches = append(out.patches, files)
	}
	return out
}

// measure runs Measure over the replay range of selected in r's chain, every patch read whole.
func (r replayed) measure(t *testing.T, selected []time.Time) survival.MeasureOutput {
	t.Helper()
	index := make(map[string]int, len(r.chain))
	for i, commit := range r.chain {
		index[commit.SHA] = i
	}
	rangeChain := survival.ReplayRange(r.chain, slices.MinFunc(selected, time.Time.Compare), slices.MaxFunc(selected, time.Time.Compare))
	out, err := survival.Measure(survival.MeasureInput{Days: selected, Chain: rangeChain,
		Patch: func(i int) (survival.Patch, error) {
			return survival.Patch{Files: r.patches[index[rangeChain[i].SHA]]}, nil
		}})
	if err != nil {
		t.Fatalf("measure %v: %v", selected, err)
	}
	return out
}

// changeOf is the change of out whose SHA is sha; it fails the test when out holds none.
func changeOf(t *testing.T, out survival.MeasureOutput, sha string) survival.Change {
	t.Helper()
	for _, change := range out.Changes {
		if change.SHA == sha {
			return change
		}
	}
	t.Fatalf("the measurement holds no change %s, of %d changes", sha, len(out.Changes))
	return survival.Change{}
}

// lines is the text of count numbered lines, each "<prefix> <n>\n" for n from 0.
func lines(prefix string, count int) string {
	var text string
	for n := range count {
		text += fmt.Sprintf("%s %d\n", prefix, n)
	}
	return text
}

// pr is the facts of pull request number, opened by author from head, for issue (none when "").
func pr(number int, head, author, issue string) survival.ChangeFacts {
	return survival.ChangeFacts{PR: number, HeadRef: head, Author: author, IssueKey: issue}
}
