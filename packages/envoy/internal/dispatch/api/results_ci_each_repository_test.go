// results_ci_each_repository_test.go holds GET /api/v1/issues/{key}/results to R8 where two
// repositories hold one commit: Dispatch derives one result at the head of each open pull
// request, from that pull request's own repository, so two linked repositories at one commit give
// two results, and each head carries the read of its own repository. It also holds the members of
// a CI read that name its head and the result it derived (model.CheckCI: commit, repo, result). The
// tests read the route's answer as JSON into the types of this file, so they build at a commit
// before the rule and fail there on their own assertions.
package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// ciRepositoryShared is the commit both #70 of acme/widgets and #9 of acme/infra are open at.
const ciRepositoryShared = "c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4"

// ciRepositoryRead is c1 as the results read answers it, with only the members these tests read.
// Each CI read is a map, so a test sees a member the answer leaves out as absent.
type ciRepositoryRead struct {
	CI         map[string]any       `json:"ci"`
	Results    []ciRepositoryResult `json:"results"`
	PassStands struct {
		Answer   string `json:"answer"`
		Reason   string `json:"reason"`
		Subjects []struct {
			Basis struct {
				Commit      string `json:"commit"`
				PullRequest *int   `json:"pull_request"`
				Repo        string `json:"repo"`
			} `json:"basis"`
			Answer  string         `json:"answer"`
			Reason  string         `json:"reason"`
			Results []string       `json:"results"`
			CI      map[string]any `json:"ci"`
		} `json:"subjects"`
	} `json:"pass_stands"`
}

// ciRepositoryResult is one result of c1, with only the members these tests read.
type ciRepositoryResult struct {
	ID           string `json:"id"`
	Verdict      string `json:"verdict"`
	CommitSHA    string `json:"commit_sha"`
	NotCounted   string `json:"not_counted"`
	CommitStands struct {
		Answer string `json:"answer"`
		Basis  *struct {
			PullRequest *int   `json:"pull_request"`
			Repo        string `json:"repo"`
		} `json:"basis"`
	} `json:"commit_stands"`
}

// ciRepositoryRig is a Dispatch whose issue lives in acme/widgets and links pull requests of
// acme/infra, whose Verification document is ciEachHeadVerification (c1, ci_check go-checks), and
// whose GitHub lists check runs by repository and commit (ciEachHeadGitHub).
type ciRepositoryRig struct {
	handler http.Handler
	github  *ciEachHeadGitHub
	issue   string
	write   func(repo string, number int, sha string)
}

func newCIRepositoryRig(t *testing.T) ciRepositoryRig {
	t.Helper()
	github := newCIEachHeadGitHub()
	handler, database, _ := resultsGitHubServed(t, map[string]bool{"acme/widgets": true, "acme/infra": true}, nil, "main",
		func(string, string) (int, string) { return http.StatusOK, "diverged" }, github.serve)
	issue := issueInRepository(t, handler, "RESRP", "acme/widgets", "A check CI proves in two repositories")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", ciEachHeadVerification)
	return ciRepositoryRig{handler: handler, github: github, issue: issue.Key, write: func(repo string, number int, sha string) {
		writeHead(t, database, repo, number, sha, "open", issue.Key)
	}}
}

// read is c1 as the results read answers it.
func (r ciRepositoryRig) read(t *testing.T) ciRepositoryRead {
	t.Helper()
	response := dispatchRequest(t, r.handler, http.MethodGet, "/api/v1/issues/"+r.issue+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", r.issue, response.Code, response.Body.String())
	}
	var read struct {
		Checks []struct {
			BlockID string `json:"block_id"`
			ciRepositoryRead
		} `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the results of %s: %v", r.issue, err)
	}
	for _, check := range read.Checks {
		if check.BlockID == "c1" {
			return check.ciRepositoryRead
		}
	}
	t.Fatalf("the read of %s holds no check c1: %s", r.issue, response.Body.String())
	return ciRepositoryRead{}
}

// ciReadWants fails the test unless ci, a CI read as the answer carries it, has each member of
// want with its value, and holds no member want maps to nil.
func ciReadWants(t *testing.T, what string, ci map[string]any, want map[string]any) {
	t.Helper()
	for member, value := range want {
		got, held := ci[member]
		switch {
		case value == nil && held:
			t.Errorf("%s: the CI read %v holds %s=%v, want it left out", what, ci, member, got)
		case value != nil && got != value:
			t.Errorf("%s: the CI read %v holds %s=%v, want %v", what, ci, member, got, value)
		}
	}
}

// TestEachRepositoryAtOneCommitDerivesItsOwnCIResult: R8 with two linked repositories at one
// commit. #70 of acme/widgets and #9 of acme/infra are open at ciRepositoryShared; go-checks
// passed there in acme/widgets and failed there in acme/infra. Each head's read derives a result
// of its own, so c1 holds the pass and the fail, both counted, and the commit holds a pass and a
// fail (R4): each head reads not known and names the two, and each carries the read of its own
// repository, so #9's reads the failure.
func TestEachRepositoryAtOneCommitDerivesItsOwnCIResult(t *testing.T) {
	t.Parallel()
	rig := newCIRepositoryRig(t)
	rig.write("acme/widgets", 70, ciRepositoryShared)
	rig.write("acme/infra", 9, ciRepositoryShared)
	rig.github.set("acme/widgets", ciRepositoryShared, completedRun(61, "go-checks", "success"))
	rig.github.set("acme/infra", ciRepositoryShared, completedRun(62, "go-checks", "failure"))
	read := rig.read(t)

	if widgets, infra := rig.github.requests("acme/widgets", ciRepositoryShared), rig.github.requests("acme/infra", ciRepositoryShared); widgets != 1 || infra != 1 {
		t.Fatalf("the commit was read %d times in acme/widgets and %d in acme/infra, want once in each", widgets, infra)
	}
	verdicts := map[string]string{}
	for _, result := range read.Results {
		if result.CommitSHA == ciRepositoryShared && result.NotCounted == "" {
			verdicts[result.ID] = result.Verdict
		}
	}
	if want := map[string]string{"ci-run:61": "pass", "ci-run:62": "fail"}; len(verdicts) != 2 || verdicts["ci-run:61"] != "pass" || verdicts["ci-run:62"] != "fail" {
		t.Fatalf("c1 counts the results %v at %s, want %v: one derived from each repository's run", verdicts, ciRepositoryShared, want)
	}
	if read.PassStands.Answer != "unknown" || read.PassStands.Reason != "pass_and_fail" {
		t.Fatalf("c1 reads %q (reason %q), want unknown for pass_and_fail", read.PassStands.Answer, read.PassStands.Reason)
	}
	ownRead := map[string]map[string]any{
		"acme/widgets": {"state": "pass", "repo": "acme/widgets", "commit": ciRepositoryShared, "result": "ci-run:61"},
		"acme/infra":   {"state": "fail", "repo": "acme/infra", "commit": ciRepositoryShared, "result": "ci-run:62"},
	}
	seen := map[string]bool{}
	for _, subject := range read.PassStands.Subjects {
		repo := subject.Basis.Repo
		if repo == "" {
			repo = "acme/widgets"
		}
		seen[repo] = true
		if subject.Answer != "unknown" || subject.Reason != "pass_and_fail" || !slices.Equal(subject.Results, []string{"ci-run:61", "ci-run:62"}) {
			t.Errorf("the head of %s#%d reads %q (reason %q) with results %v, want unknown for pass_and_fail, naming the pass ci-run:61 and the fail ci-run:62",
				repo, *subject.Basis.PullRequest, subject.Answer, subject.Reason, subject.Results)
		}
		ciReadWants(t, "the head of "+repo, subject.CI, ownRead[repo])
	}
	if len(read.PassStands.Subjects) != 2 || !seen["acme/widgets"] || !seen["acme/infra"] {
		t.Fatalf("c1's subjects are %+v, want the head of #70 of acme/widgets and of #9 of acme/infra", read.PassStands.Subjects)
	}
}

// TestEachHeadAtOneCommitNamesTheResultItsOwnRunDerived: #70 of acme/widgets and #9 of acme/infra
// are open at one commit, and go-checks concluded alike in both, acme/widgets' run the later. Each
// head reads the state of the results at the commit, and names as the result that decides it the
// one its own repository's run derived, never the other repository's newer one; each derived
// result stands at the pull request of its own head. The case runs with two passes and two fails.
func TestEachHeadAtOneCommitNamesTheResultItsOwnRunDerived(t *testing.T) {
	t.Parallel()
	for _, each := range []struct{ conclusion, answer, reason string }{
		{"success", "yes", ""},
		{"failure", "no", "fail"},
	} {
		t.Run(each.conclusion, func(t *testing.T) {
			rig := newCIRepositoryRig(t)
			rig.write("acme/widgets", 70, ciRepositoryShared)
			rig.write("acme/infra", 9, ciRepositoryShared)
			widgets, infra := completedRun(71, "go-checks", each.conclusion), completedRun(72, "go-checks", each.conclusion)
			widgets["completed_at"], infra["completed_at"] = "2026-11-12T11:00:00Z", "2026-11-12T10:00:00Z"
			rig.github.set("acme/widgets", ciRepositoryShared, widgets)
			rig.github.set("acme/infra", ciRepositoryShared, infra)
			read := rig.read(t)

			own := map[string]string{"acme/widgets": "ci-run:71", "acme/infra": "ci-run:72"}
			number := map[string]int{"ci-run:71": 70, "ci-run:72": 9}
			if len(read.PassStands.Subjects) != 2 {
				t.Fatalf("c1's subjects are %+v, want the head of #70 of acme/widgets and of #9 of acme/infra", read.PassStands.Subjects)
			}
			for _, subject := range read.PassStands.Subjects {
				repo := subject.Basis.Repo
				if repo == "" {
					repo = "acme/widgets"
				}
				if subject.Answer != each.answer || subject.Reason != each.reason || !slices.Equal(subject.Results, []string{own[repo]}) {
					t.Errorf("the head of %s reads %q (reason %q) decided by %v, want %q (reason %q) decided by its own run's %s",
						repo, subject.Answer, subject.Reason, subject.Results, each.answer, each.reason, own[repo])
				}
			}
			for _, result := range read.Results {
				want, derived := number[result.ID]
				if !derived {
					continue
				}
				if basis := result.CommitStands.Basis; result.CommitStands.Answer != "at_basis" || basis == nil || basis.PullRequest == nil || *basis.PullRequest != want {
					t.Errorf("the result %s stands %+v at %+v, want at_basis at #%d, the head its run was read at", result.ID, result.CommitStands.Answer, basis, want)
				}
				delete(number, result.ID)
			}
			if len(number) != 0 {
				t.Errorf("c1's results hold no result %v", number)
			}
		})
	}
}

// TestTheCIReadNamesItsHeadAndTheResultItDerived: each CI read names the head it is of, its commit
// and its repository, and the read whose run passed or failed names the result it derived, so a
// reader finds that result by its id. #70 of acme/widgets passed, #71 of acme/widgets has a run
// still going, and #9 of acme/infra GitHub cannot be read for: the three name their heads, and
// only #70's names a result. An issue with no pull request names no head at all.
func TestTheCIReadNamesItsHeadAndTheResultItDerived(t *testing.T) {
	t.Parallel()
	rig := newCIRepositoryRig(t)
	rig.write("acme/widgets", 70, ciEachHeadH1)
	rig.write("acme/widgets", 71, ciEachHeadH2)
	rig.write("acme/infra", 9, ciEachHeadH3)
	rig.github.set("acme/widgets", ciEachHeadH1, completedRun(63, "go-checks", "success"))
	rig.github.set("acme/widgets", ciEachHeadH2, map[string]any{"id": 64, "name": "go-checks", "status": "in_progress", "conclusion": nil})
	read := rig.read(t)

	if !slices.ContainsFunc(read.Results, func(result ciRepositoryResult) bool {
		return result.ID == "ci-run:63" && result.CommitSHA == ciEachHeadH1
	}) {
		t.Fatalf("c1's results %+v hold no result ci-run:63 at %s", read.Results, ciEachHeadH1)
	}
	want := map[string]map[string]any{
		ciEachHeadH1: {"state": "pass", "commit": ciEachHeadH1, "repo": "acme/widgets", "result": "ci-run:63"},
		ciEachHeadH2: {"state": "no_result", "case": "not_completed", "commit": ciEachHeadH2, "repo": "acme/widgets", "result": nil},
		ciEachHeadH3: {"state": "unread", "commit": ciEachHeadH3, "repo": "acme/infra", "result": nil},
	}
	for _, subject := range read.PassStands.Subjects {
		wanted, named := want[subject.Basis.Commit]
		if !named {
			t.Fatalf("c1 names a subject at %s, which no pull request of the test is open at", subject.Basis.Commit)
		}
		ciReadWants(t, "the head at "+subject.Basis.Commit, subject.CI, wanted)
		delete(want, subject.Basis.Commit)
	}
	if len(want) != 0 {
		t.Fatalf("c1 names no subject at %v", want)
	}
	// The check's own read is the open pull request's of the highest number, #71.
	ciReadWants(t, "c1's own read", read.CI, map[string]any{"state": "no_result", "commit": ciEachHeadH2, "repo": "acme/widgets", "result": nil})

	t.Run("no head", func(t *testing.T) {
		rig := newCIRepositoryRig(t)
		read := rig.read(t)
		ciReadWants(t, "c1's read with no pull request", read.CI,
			map[string]any{"state": "no_result", "case": "no_head", "commit": nil, "repo": nil, "result": nil})
	})
}
