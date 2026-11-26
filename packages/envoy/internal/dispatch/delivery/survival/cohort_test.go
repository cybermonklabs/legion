package survival_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
)

func TestGroupOfTakesTheFirstMatchingGroup(t *testing.T) {
	t.Parallel()
	groups := []survival.Group{
		{Name: "agents", BranchPrefixes: []string{"agent/"}, AuthorLogins: []string{"agent-app[bot]"}},
		{Name: "bots", AuthorLogins: []string{"Helper-Bot[bot]"}},
		{Name: "people", BranchPrefixes: []string{"feature/", "fix/"}},
		{Name: "rest"},
	}
	named := groups[:3]
	rows := []struct {
		name   string
		change survival.ChangeFacts
		groups []survival.Group
		want   string
	}{
		{"a prefix", pr(1, "agent/KEY-1", "someone", ""), groups, "agents"},
		{"a login", pr(2, "main-fix", "agent-app[bot]", ""), groups, "agents"},
		{"both", pr(3, "agent/KEY-2", "agent-app[bot]", ""), groups, "agents"},
		{"a login in another case", pr(4, "chore/x", "AGENT-APP[BOT]", ""), groups, "agents"},
		{"an App's [bot] login", pr(5, "chore/x", "helper-bot[bot]", ""), groups, "bots"},
		{"the login without [bot]", pr(6, "chore/x", "helper-bot", ""), groups, "rest"},
		{"two matching groups", pr(7, "feature/x", "helper-bot[bot]", ""), groups, "bots"},
		{"a second prefix", pr(8, "fix/y", "someone", ""), groups, "people"},
		{"a prefix inside the branch, not at its start", pr(9, "x/agent/y", "someone", ""), groups, "rest"},
		{"a login that only starts with one", pr(10, "chore/x", "agent-app[bot]-2", ""), groups, "rest"},
		{"a prefix in another case", pr(11, "Agent/x", "someone", ""), groups, "rest"},
		{"the catch-all last", pr(12, "chore/x", "someone", ""), groups, "rest"},
		{"no group", pr(13, "chore/x", "someone", ""), named, ""},
		{"a commit with no pull request, beside a catch-all", survival.ChangeFacts{HeadRef: "agent/x", Author: "agent-app[bot]"}, groups, ""},
	}
	for _, row := range rows {
		name, ok := survival.GroupOf(row.change, row.groups)
		if name != row.want || ok != (row.want != "") {
			t.Errorf("%s: GroupOf answers %q, %v; want %q", row.name, name, ok, row.want)
		}
	}
}

func TestAChangesIssueIsAnInputAndDecidesItsKind(t *testing.T) {
	t.Parallel()
	groups := []survival.Group{{Name: "agents", BranchPrefixes: []string{"agent/"}}, {Name: "rest"}}
	adder := pr(1, "agent/KEY-1-work", "agent-app[bot]", "KEY-1")
	rows := []struct {
		name           string
		adder, remover survival.ChangeFacts
		want           survival.Kind
	}{
		{"the same issue", adder, pr(2, "agent/more", "agent-app[bot]", "KEY-1"), survival.KindIteration},
		{"two issues", adder, pr(3, "agent/KEY-1-again", "agent-app[bot]", "KEY-2"), survival.KindRework},
		{"a remover with no issue, its branch naming the adder's", adder, pr(4, "agent/KEY-1", "agent-app[bot]", ""), survival.KindNoIssue},
		{"an adder with no issue, its branch naming one", pr(5, "agent/KEY-7", "agent-app[bot]", ""), pr(6, "agent/y", "agent-app[bot]", "KEY-7"), survival.KindNoIssue},
		{"neither with an issue", pr(7, "agent/x", "agent-app[bot]", ""), pr(8, "agent/y", "agent-app[bot]", ""), survival.KindNoIssue},
		{"another group, the same issue", adder, pr(9, "other/KEY-1", "someone", "KEY-1"), survival.KindOther},
		{"another group, no issue", adder, pr(10, "other/x", "someone", ""), survival.KindOther},
		{"a commit with no pull request", adder, survival.ChangeFacts{}, survival.KindOther},
	}
	for _, row := range rows {
		if got := survival.KindOf("agents", row.adder, row.remover, groups); got != row.want {
			t.Errorf("%s: KindOf answers %q, want %q", row.name, got, row.want)
		}
	}

	// Measure carries each change's issue as it was given, whatever the branch names.
	r := chainOf(
		step{at(0, 9, 0), pr(1, "agent/KEY-9", "agent-app[bot]", "KEY-1"), added("x.txt", 5)},
		step{at(1, 9, 0), pr(2, "agent/KEY-1", "agent-app[bot]", ""), removed("x.txt", 1, 2)},
	)
	out := r.measure(t, days(0, 1))
	if got := changeOf(t, out, sha(0)); got.IssueKey != "KEY-1" {
		t.Errorf("the change's issue reads %q, want the KEY-1 it was given", got.IssueKey)
	}
	if got := changeOf(t, out, sha(1)); got.IssueKey != "" {
		t.Errorf("the change given no issue reads %q", got.IssueKey)
	}
	if len(out.Pairs) != 1 || out.Pairs[0].Remover != r.chain[1].Change {
		t.Fatalf("the pairs are %+v, want the one removal with the remover's facts as given", out.Pairs)
	}
	adderFacts := changeOf(t, out, sha(0)).ChangeFacts
	if got := survival.KindOf("agents", adderFacts, out.Pairs[0].Remover, groups); got != survival.KindNoIssue {
		t.Errorf("the pair reads %q, want %q: the remover was given no issue", got, survival.KindNoIssue)
	}
}
