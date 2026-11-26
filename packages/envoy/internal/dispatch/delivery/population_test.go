package delivery

import (
	"cmp"
	"slices"
	"testing"
	"time"
)

// testRules is one source repository's rule, acme/widgets', with the population that reads it.
func testRules() *SourceRules {
	return testRulesOf(DeliverySettings{
		DeployRepo:        "acme/widgets",
		PopulationAuthors: []string{"octocat", "octocat-agent", "octocat-agent[bot]"},
		ExcludedRepos:     []string{"acme/dojo", "acme/widgets-smoke"},
	})
}

// testRulesOf is the SourceRules of rules and their populations' union, as the settings cache
// holds them: what NewSourceRules makes of the projects that saved them, without the database.
func testRulesOf(rules ...DeliverySettings) *SourceRules {
	var authors, excluded []string
	for _, rule := range rules {
		authors = append(authors, rule.PopulationAuthors...)
		excluded = append(excluded, rule.ExcludedRepos...)
	}
	ordered := slices.SortedFunc(slices.Values(rules), func(a, b DeliverySettings) int { return cmp.Compare(a.DeployRepo, b.DeployRepo) })
	return newSourceRules(ordered, Population{Authors: sortedSet(authors), ExcludedRepos: sortedSet(excluded)}, nil)
}

func testWindow() TimeWindow {
	return TimeWindow{
		Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 29, 0, 0, 0, 0, time.UTC),
	}
}

func TestIsPopulationPR(t *testing.T) {
	t.Parallel()
	rules := testRules()
	window := testWindow()
	inWindow := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		pr      RawPullRequest
		want    bool
		wantErr bool
	}{
		{
			name: "excluded repo wins even for a population author",
			pr: RawPullRequest{
				Repo: "acme/dojo", Author: "octocat", MergedAt: inWindow,
			},
			want: false,
		},
		{
			name: "author not in the list",
			pr: RawPullRequest{
				Repo: "acme/widgets", Author: "someone-else", MergedAt: inWindow,
			},
			want: false,
		},
		{
			name: "before window start",
			pr: RawPullRequest{
				Repo: "acme/widgets", Author: "octocat",
				MergedAt: window.Start.Add(-time.Second),
			},
			want: false,
		},
		{
			name: "at window end exactly is excluded (End is exclusive)",
			pr: RawPullRequest{
				Repo: "acme/widgets", Author: "octocat", MergedAt: window.End,
			},
			want: false,
		},
		{
			name: "at window start exactly is included (Start is inclusive)",
			pr: RawPullRequest{
				Repo: "acme/widgets", Author: "octocat", MergedAt: window.Start,
				Labels: []string{"non-task"},
			},
			want: true,
		},
		{
			name: "deploy-repo PR with task label is excluded",
			pr: RawPullRequest{
				Repo: "acme/widgets", Author: "octocat", MergedAt: inWindow,
				Labels: []string{"task"},
			},
			want: false,
		},
		{
			name: "deploy-repo PR with non-task label is included",
			pr: RawPullRequest{
				Repo: "acme/widgets", Author: "octocat", MergedAt: inWindow,
				Labels: []string{"non-task"},
			},
			want: true,
		},
		{
			name: "deploy-repo PR with neither label propagates the error",
			pr: RawPullRequest{
				Repo: "acme/widgets", Author: "octocat", MergedAt: inWindow,
				Labels: nil,
			},
			wantErr: true,
		},
		{
			name: "non-deploy-repo PR by a population author is never excluded as a task PR",
			pr: RawPullRequest{
				Repo: "acme/other", Author: "octocat", MergedAt: inWindow,
				Labels: nil,
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := IsPopulationPR(tt.pr, rules, window)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("IsPopulationPR() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A pull request is judged a task pull request by the rule of its own source repository
// (dispatch://INFRA-978, D1): with two sources in two repositories, a task-labelled merge of
// either is a task pull request and an unlabelled merge of either is unclassified, while a merge
// of a repository no source reads is neither, whatever its labels.
func TestATaskPullRequestIsJudgedAgainstItsOwnDeployRepository(t *testing.T) {
	t.Parallel()
	rules := testRulesOf(
		DeliverySettings{DeployRepo: "acme/widgets", PopulationAuthors: []string{"octocat"}},
		DeliverySettings{DeployRepo: "acme/gadgets", PopulationAuthors: []string{"octocat"}},
	)
	merged := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	for _, one := range []struct {
		name    string
		repo    string
		labels  []string
		want    bool
		wantErr bool
	}{
		{name: "the second source's task pull request is a task pull request", repo: "acme/gadgets", labels: []string{"task"}},
		{name: "the first source's task pull request is a task pull request", repo: "acme/widgets", labels: []string{"task"}},
		{name: "the second source's non-task pull request is in the population", repo: "acme/gadgets", labels: []string{"non-task"}, want: true},
		{name: "the second source's unlabelled pull request is unclassified", repo: "acme/gadgets", wantErr: true},
		{name: "a repository no source reads is never a task pull request", repo: "acme/other", want: true},
	} {
		t.Run(one.name, func(t *testing.T) {
			pr := RawPullRequest{Repo: one.repo, Number: 7, Author: "octocat", Labels: one.labels, MergedAt: merged}
			got, err := IsPopulationPR(pr, rules, testWindow())
			if one.wantErr {
				if err == nil {
					t.Fatalf("IsPopulationPR of %s = %v, want the unclassified error of its own repository's rule", one.repo, got)
				}
				return
			}
			if err != nil || got != one.want {
				t.Fatalf("IsPopulationPR of %s%v = %v (%v), want %v", one.repo, one.labels, got, err, one.want)
			}
		})
	}
}

func TestIsRework(t *testing.T) {
	t.Parallel()
	tests := []struct {
		title string
		want  bool
	}{
		{"fix #42", true},
		{"Fix #42", true},
		{"FIX #42", true},
		{"revert #42", true},
		{"hotfix #42", true},
		{"prefix something", false},
		{"feat: add widget", false},
	}
	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			if got := IsRework(tt.title); got != tt.want {
				t.Errorf("IsRework(%q) = %v, want %v", tt.title, got, tt.want)
			}
		})
	}
}

func TestRevertKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		title string
		want  string
	}{
		{"Revert #42", "revert"},
		{"Fix #42", "fix"},
		{"Hotfix #42", "fix"},
	}
	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			if got := RevertKind(tt.title); got != tt.want {
				t.Errorf("RevertKind(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}
