package replay

import "testing"

// The replay numbers its own pull request record of each issue (assumption 29) as no other
// issue's in the same repository, the repository compared without case as the daemon finds a pull
// request (lower(repo)), and the same at every run: the number a recorded line names, kept by the
// first issue in key order that a line names it for in that repository; else the issue's own
// key's number, where no issue takes it in that repository already; else the next above every
// number given in its repository, in key order; and for an issue the trace does not hold, or one
// asked for in another repository than the trace records it on, the next there when it is first
// asked for. The test numbers a trace of three repositories. In acme/widgets ACME-9's line names
// 3, ACME-3's key's number; ACME-7's and ACME-10's lines both name 12; ACME-4 and OTHER-4 share
// their key's number. ACME-11's line names 12 in Acme/Gadgets, another repository, and ACME-5's
// names 12 in acme/gadgets, the same repository spelt otherwise. OTHER-3 in acme/tools takes 3,
// which ACME-9 holds in acme/widgets. It then asks for ACME-99, which the trace does not hold,
// and for ACME-9 in acme/tools. It fails for an issue numbered otherwise, and for a number asked
// twice that changes.
func TestEachPullRequestRecordHasANumberOfItsOwn(t *testing.T) {
	t.Parallel()
	const widgets, gadgets, tools = "acme/widgets", "Acme/Gadgets", "acme/tools"
	trace := Trace{Issues: map[string]*IssueTrace{
		"ACME-3":  {Key: "ACME-3", Repo: widgets},
		"ACME-4":  {Key: "ACME-4", Repo: widgets},
		"ACME-5":  {Key: "ACME-5", Repo: "acme/gadgets", PullRequestNumber: 12},
		"ACME-7":  {Key: "ACME-7", Repo: widgets, PullRequestNumber: 12},
		"ACME-9":  {Key: "ACME-9", Repo: widgets, PullRequestNumber: 3},
		"ACME-10": {Key: "ACME-10", Repo: widgets, PullRequestNumber: 12},
		"ACME-11": {Key: "ACME-11", Repo: gadgets, PullRequestNumber: 12},
		"OTHER-3": {Key: "OTHER-3", Repo: tools},
		"OTHER-4": {Key: "OTHER-4", Repo: widgets},
	}}
	numbers := newPullRequestNumbers(trace)
	for _, want := range []struct {
		key, repo string
		number    int
	}{
		{"ACME-10", widgets, 12},
		{"ACME-11", gadgets, 12},
		{"ACME-9", widgets, 3},
		{"OTHER-3", tools, 3},
		{"ACME-5", "acme/gadgets", 5},
		{"ACME-7", widgets, 7},
		{"ACME-4", "ACME/Widgets", 4},
		{"ACME-3", widgets, 13},
		{"OTHER-4", widgets, 14},
		{"ACME-99", widgets, 15},
		{"ACME-99", widgets, 15},
		{"ACME-9", tools, 4},
		{"ACME-9", tools, 4},
		{"ACME-3", widgets, 13},
	} {
		if got := numbers.of(want.key, want.repo); got != want.number {
			t.Errorf("the pull request record of %s in %s is numbered %d, want %d", want.key, want.repo, got, want.number)
		}
	}
}
