// results_stacked_heads_test.go holds GET /api/v1/issues/{key}/results to the state of each head
// in the shape a stack of pull requests makes: one issue, its own pull request on a branch named
// for it, and a second pull request whose branch is named for the issue too and whose base is the
// first one's branch, carrying other work. Passes stand at the issue's own head and none at the
// stacked one. A row here also holds the attribution that keeps the issue's own pull request the
// issue's when its body names another issue's pull request by number. The file reads the route's
// answer as JSON into types of its own and uses only helpers main holds, so at a commit before
// these rules it builds and fails on its own assertions.
package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The pull requests of these rows, all of acme/widgets: the issue's own, the one stacked on its
// branch, the other issue's pull request a body names, the GitHub issue a body cites, and the one
// pull request of the control's issue.
const (
	stackedOwn       = 51
	stackedOnTop     = 52
	stackedCitedPull = 53
	stackedCitedIs   = 54
	stackedAlone     = 61

	stackedOwnHead   = "5151515151515151515151515151515151515151"
	stackedOnTopHead = "5252525252525252525252525252525252525252"
	stackedCitedHead = "5353535353535353535353535353535353535353"
	stackedAloneHead = "6161616161616161616161616161616161616161"
)

// stackedAnswer is the results read, with only the members this file reads.
type stackedAnswer struct {
	Checks []struct {
		BlockID      string         `json:"block_id"`
		ComparedWith []stackedBasis `json:"compared_with"`
		PassStands   struct {
			Answer   string   `json:"answer"`
			Roles    []string `json:"roles"`
			Subjects []struct {
				Basis   stackedBasis `json:"basis"`
				Answer  string       `json:"answer"`
				Reason  string       `json:"reason"`
				Results []string     `json:"results"`
			} `json:"subjects"`
		} `json:"pass_stands"`
	} `json:"checks"`
}

// stackedBasis is one commit a check compared its results with.
type stackedBasis struct {
	Kind        string `json:"kind"`
	Commit      string `json:"commit"`
	PullRequest *int   `json:"pull_request"`
}

func (b stackedBasis) String() string {
	if b.PullRequest == nil {
		return b.Kind + " " + b.Commit
	}
	return fmt.Sprintf("%s #%d at %s", b.Kind, *b.PullRequest, b.Commit)
}

// stackedPull is one pull request of acme/widgets as GitHub names it for its head commit: its
// head branch is legion/<the issue's key><suffix>, its base branch base, or the issue's own branch
// legion/<the issue's key> where base is empty (a pull request stacked on it), its body body, and
// open, or merged where merged is set.
type stackedPull struct {
	number       int
	head         string
	suffix, base string
	body         string
	merged       bool
}

// stackedWorld is a Dispatch whose GitHub names each pull request of pulls for its own head commit,
// an issue of acme/widgets whose Verification document is siblingChecks, and a second issue of the
// same project, other, which a row links to the pull request or the GitHub issue a body names.
type stackedWorld struct {
	t       *testing.T
	handler http.Handler
	issue   string
	other   string
	// readThrough writes a pull request's head row as production's read-through of its head
	// commit writes it (delivery.Attributor.ReadThroughCommitPulls), attributed from GitHub's
	// answer.
	readThrough func(pull stackedPull)
	// attribution is the issue and the source the head row of the pull request number names.
	attribution func(number int) (string, string)
	// link records, as Dispatch stores a link to the reference acme/widgets#number, both URLs of
	// that number on the other issue.
	link func(number int)
	// pullRequest records the other issue's open pull request number, at head, through the writer
	// a `pull_request` event of it reaches (delivery.Attributor.UpsertPullRequestHead).
	pullRequest func(number int, head string)
	// offered reports whether the reconcile's head pass would read the pull request number again
	// now (delivery.ListStalePullRequestHeads, the batch a pass reads).
	offered func(number int) bool
}

func newStackedWorld(t *testing.T, project string, pulls ...stackedPull) stackedWorld {
	t.Helper()
	handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, project, "acme/widgets", "Stacked pull requests")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecks)
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": project, "title": "Other work", "spec": "A spec\n"}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create the other issue: status=%d body=%s", response.Code, response.Body.String())
	}
	other := decodeBody[model.Issue](t, response).Key
	byHead := map[string]stackedPull{}
	for _, pull := range pulls {
		byHead[pull.head] = pull
	}
	fake.answers(func(repo, sha string) string {
		pull, named := byHead[sha]
		if !named {
			return `[]`
		}
		body, err := json.Marshal(pull.body)
		if err != nil {
			t.Errorf("encode the body of #%d: %v", pull.number, err)
		}
		state, merged := "open", `false,"merged_at":null`
		if pull.merged {
			state, merged = "closed", `true,"merged_at":"2026-11-20T17:00:00Z"`
		}
		return fmt.Sprintf(`[{"number":%d,"title":"A change","body":%s,"html_url":"https://github.com/%s/pull/%d",
			"state":%q,"merged":%s,"updated_at":"2026-11-20T17:07:00Z",
			"head":{"ref":%q,"sha":%q,"repo":{"full_name":%q}},"base":{"ref":%q}}]`,
			pull.number, body, repo, pull.number, state, merged, "legion/"+issue.Key+pull.suffix, sha, repo, cmp.Or(pull.base, "legion/"+issue.Key))
	})
	ctx := context.Background()
	return stackedWorld{
		t: t, handler: handler, issue: issue.Key, other: other,
		readThrough: func(pull stackedPull) {
			t.Helper()
			if written, _, err := served.deps.Attributor.ReadThroughCommitPulls(ctx, database.Pool, served.deps.GitHub, nil,
				"acme/widgets", pull.head, time.Now()); err != nil || written != 1 {
				t.Fatalf("write the head of #%d through the read of its commit: %d written, %v", pull.number, written, err)
			}
		},
		attribution: func(number int) (string, string) {
			t.Helper()
			var key, source string
			if err := database.Pool.QueryRow(ctx, `
				select coalesce(issue_key, ''), attribution_source from pull_request_heads
				where repo = 'acme/widgets' and number = $1`, number).Scan(&key, &source); err != nil {
				t.Fatalf("read the head row of #%d: %v", number, err)
			}
			return key, source
		},
		link: func(number int) {
			t.Helper()
			if _, err := database.Pool.Exec(ctx, `
				insert into issue_external_links (issue_key, url, kind) values
					($1, 'https://github.com/acme/widgets/issues/' || $2, 'github_issue'),
					($1, 'https://github.com/acme/widgets/pull/' || $2, 'url')`, other, strconv.Itoa(number)); err != nil {
				t.Fatalf("link acme/widgets#%d to %s: %v", number, other, err)
			}
		},
		pullRequest: func(number int, head string) {
			t.Helper()
			key := other
			if err := served.deps.Attributor.UpsertPullRequestHead(ctx, database.Pool, nil, delivery.PullRequestHead{
				Repo: "acme/widgets", Number: number, HeadSHA: head, HeadRef: "legion/" + other, BaseRef: "main",
				State: "open", IssueKey: &key, AttributionSource: "branch", AttributionComplete: true,
				Title: "Other work", URL: fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
			}); err != nil {
				t.Fatalf("record #%d, the other issue's pull request: %v", number, err)
			}
		},
		offered: func(number int) bool {
			t.Helper()
			stale, err := delivery.ListStalePullRequestHeads(ctx, database.Pool, delivery.PullRequestHeadBatch, time.Now())
			if err != nil {
				t.Fatalf("list the heads the reconcile's pass reads: %v", err)
			}
			return slices.ContainsFunc(stale, func(head delivery.PullRequestHead) bool {
				return head.Repo == "acme/widgets" && head.Number == number
			})
		},
	}
}

// requireAttribution fails the test unless the pull request number is key's by source.
func (w stackedWorld) requireAttribution(number int, key, source, why string) {
	w.t.Helper()
	if gotKey, gotSource := w.attribution(number); gotKey != key || gotSource != source {
		w.t.Fatalf("#%d is credited to %q by %q, want %s by %s: %s", number, gotKey, gotSource, key, source, why)
	}
}

// passes records the tester's pass of each check of siblingChecks at commit and answers their ids
// by check.
func (w stackedWorld) passes(commit string) map[string]string {
	w.t.Helper()
	ids := map[string]string{}
	for _, check := range []string{"k1", "k3"} {
		response := recordResult(w.t, w.handler, w.issue, resultBody(check, "pass", commit, map[string]any{"role": "tester"}))
		if response.Code != http.StatusCreated {
			w.t.Fatalf("record the tester's pass of %s at %s: status=%d body=%s", check, commit, response.Code, response.Body.String())
		}
		var recorded struct {
			Result struct {
				ID string `json:"id"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &recorded); err != nil || recorded.Result.ID == "" {
			w.t.Fatalf("decode the pass of %s: %v (body %s)", check, err, response.Body.String())
		}
		ids[check] = recorded.Result.ID
	}
	return ids
}

// read is the issue's results as the route answers them.
func (w stackedWorld) read() stackedAnswer {
	w.t.Helper()
	response := dispatchRequest(w.t, w.handler, http.MethodGet, "/api/v1/issues/"+w.issue+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		w.t.Fatalf("read the results of %s: status=%d body=%s", w.issue, response.Code, response.Body.String())
	}
	var read stackedAnswer
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		w.t.Fatalf("decode the results of %s: %v (body %s)", w.issue, err, response.Body.String())
	}
	if len(read.Checks) != 2 {
		w.t.Fatalf("the results of %s hold %d checks, want siblingChecks' two: %s", w.issue, len(read.Checks), response.Body.String())
	}
	return read
}

// requireEachHead fails the test unless every check compared with own and onTop, and reads own's
// head yes from the pass passes holds for it and onTop's head no for no_result.
func (w stackedWorld) requireEachHead(own, onTop stackedPull, passes map[string]string) {
	w.t.Helper()
	for _, check := range w.read().Checks {
		if got, want := stackedCommits(check.ComparedWith), []string{own.head, onTop.head}; !slices.Equal(got, want) {
			w.t.Errorf("%s compared with %v, want both open heads, the issue's own #%d and the stacked #%d", check.BlockID, check.ComparedWith, own.number, onTop.number)
		}
		states := map[string]string{}
		for _, subject := range check.PassStands.Subjects {
			states[subject.Basis.Commit] = subject.Answer + " " + subject.Reason
			switch subject.Basis.Commit {
			case own.head:
				if subject.Answer != "yes" || !slices.Equal(subject.Results, []string{passes[check.BlockID]}) {
					w.t.Errorf("%s at the issue's own head %s reads %q (reason %q) from results %v, want yes from the tester's pass %s",
						check.BlockID, subject.Basis, subject.Answer, subject.Reason, subject.Results, passes[check.BlockID])
				}
			case onTop.head:
				if subject.Answer != "no" || subject.Reason != "no_result" {
					w.t.Errorf("%s at the stacked head %s reads %q (reason %q), want no for no_result: no result is recorded there",
						check.BlockID, subject.Basis, subject.Answer, subject.Reason)
				}
			default:
				w.t.Errorf("%s names a state at %s, which heads no pull request of the issue", check.BlockID, subject.Basis)
			}
		}
		if len(states) != 2 {
			w.t.Errorf("%s names the state of %d head(s) %v beside its pass_stands %q from %v: want the issue's own head #%d yes and the stacked head #%d no for no_result",
				check.BlockID, len(states), states, check.PassStands.Answer, check.PassStands.Roles, own.number, onTop.number)
		}
	}
}

// stackedCommits is the commits a check compared its results with, sorted.
func stackedCommits(bases []stackedBasis) []string {
	commits := make([]string, 0, len(bases))
	for _, basis := range bases {
		commits = append(commits, basis.Commit)
	}
	slices.Sort(commits)
	return commits
}

// The issue's own pull request (head branch legion/<key>, base main) and one stacked on its branch
// (legion/<key>-snippet-sentinels, base legion/<key>) are both the issue's by the key in their
// branch names, and each head has the state of the results at its own commit. Every check passes at
// the issue's own head and has no result at the stacked one: the read says yes at the first, from
// the tester's pass, and no for no_result at the second.
//
// The second row is the same stack, with the own pull request's body naming another issue's pull
// request by number ("#53 renamed the helper"), and that other issue linking acme/widgets#53, which
// Dispatch stores as both .../issues/53 and .../pull/53. #53 is a pull request Dispatch holds, so
// the mention cites no GitHub issue: the own pull request stays the issue's by its branch, and the
// read is the first row's. Before rule 5 the mention outranked the branch, the own pull request was
// credited to the other issue, and the issue's checks compared with the stacked head alone.
//
// The third row is the order a merge can come in: the own pull request, merged, is recorded while
// Dispatch holds no #53, so its mention reads as a citation of a GitHub issue and credits it to the
// other issue, as the rule says of a number Dispatch does not know. #53's own event then records it
// as a pull request, and that write gives the merged row rule 0, so the reconcile's head pass reads
// it again, as a raised rule has that pass read every merged row; the read credits it to the issue
// by its branch. Without that mark nothing reads a merged row again, and it stays the other issue's.
//
// Two controls. A body that cites #54, a GitHub issue the other issue links and no pull request
// Dispatch holds, still credits the pull request to the other issue, by the citation, which ranks
// above the branch. And an issue with one open pull request reads as before each head had a state
// of its own: a pass at its head stands, from the tester, and each check compared with that head
// alone.
func TestAStackedPullRequestsHeadHasItsOwnStateBesideTheIssuesOwnHead(t *testing.T) {
	own := stackedPull{number: stackedOwn, head: stackedOwnHead, base: "main"}
	onTop := stackedPull{number: stackedOnTop, head: stackedOnTopHead, suffix: "-snippet-sentinels"}
	t.Run("two open pull requests, one stacked on the other's branch", func(t *testing.T) {
		world := newStackedWorld(t, "RESSTK", own, onTop)
		for _, pull := range []stackedPull{own, onTop} {
			world.readThrough(pull)
			world.requireAttribution(pull.number, world.issue, "branch", "its branch names the issue")
		}
		world.requireEachHead(own, onTop, world.passes(own.head))
	})
	t.Run("the own pull request's body names another issue's pull request by number", func(t *testing.T) {
		mentioning := own
		mentioning.body = fmt.Sprintf("#%d renamed the helper, so this change calls it by its new name.", stackedCitedPull)
		world := newStackedWorld(t, "RESSTM", mentioning, onTop)
		world.link(stackedCitedPull)
		world.pullRequest(stackedCitedPull, stackedCitedHead)
		for _, pull := range []stackedPull{mentioning, onTop} {
			world.readThrough(pull)
			world.requireAttribution(pull.number, world.issue, "branch",
				fmt.Sprintf("#%d is a pull request Dispatch holds, so the body's #%d cites no GitHub issue and the branch names the issue", stackedCitedPull, stackedCitedPull))
		}
		world.requireEachHead(mentioning, onTop, world.passes(own.head))
	})
	t.Run("the pull request the body names is recorded after the merged pull request that names it", func(t *testing.T) {
		merged := own
		merged.merged = true
		merged.body = fmt.Sprintf("#%d renamed the helper, so this change calls it by its new name.", stackedCitedPull)
		world := newStackedWorld(t, "RESSTL", merged)
		world.link(stackedCitedPull)
		world.readThrough(merged)
		world.requireAttribution(merged.number, world.other, "github_issue_link",
			fmt.Sprintf("Dispatch holds no #%d yet, so the body's #%d reads as a GitHub issue the other issue links", stackedCitedPull, stackedCitedPull))
		if world.offered(merged.number) {
			t.Fatalf("the merged #%d, judged by this build's rule, is offered to the reconcile's head pass before anything changed", merged.number)
		}
		world.pullRequest(stackedCitedPull, stackedCitedHead)
		if !world.offered(merged.number) {
			t.Fatalf("the merged #%d, credited to %s by its body's #%d, is not offered to the reconcile's head pass once Dispatch records #%d as a pull request: nothing reads a merged row again, so it stays the other issue's",
				merged.number, world.other, stackedCitedPull, stackedCitedPull)
		}
		// The pass reads the pull request from GitHub and writes its row through the attribution
		// the read-through writes it with (attributeHead).
		world.readThrough(merged)
		world.requireAttribution(merged.number, world.issue, "branch",
			fmt.Sprintf("#%d is now a pull request Dispatch holds, so the body's #%d cites no GitHub issue and the branch names the issue", stackedCitedPull, stackedCitedPull))
	})
	t.Run("control: a body citing a GitHub issue the other issue links", func(t *testing.T) {
		citing := own
		citing.body = fmt.Sprintf("Fixes #%d.", stackedCitedIs)
		world := newStackedWorld(t, "RESSTC", citing)
		world.link(stackedCitedIs)
		world.readThrough(citing)
		world.requireAttribution(citing.number, world.other, "github_issue_link",
			fmt.Sprintf("#%d is no pull request Dispatch holds, so the body cites the GitHub issue the other issue links, which ranks above the branch", stackedCitedIs))
	})
	t.Run("control: one open pull request", func(t *testing.T) {
		alone := stackedPull{number: stackedAlone, head: stackedAloneHead, base: "main"}
		world := newStackedWorld(t, "RESSTO", alone)
		world.readThrough(alone)
		world.requireAttribution(alone.number, world.issue, "branch", "its branch names the issue")
		world.passes(alone.head)
		for _, check := range world.read().Checks {
			if got := stackedCommits(check.ComparedWith); !slices.Equal(got, []string{alone.head}) {
				t.Errorf("%s compared with %v, want the one open head %s", check.BlockID, check.ComparedWith, alone.head)
			}
			if check.PassStands.Answer != "yes" || !slices.Equal(check.PassStands.Roles, []string{"tester"}) {
				t.Errorf("%s's pass stands %q from %v, want yes from the tester's pass at the one head", check.BlockID,
					check.PassStands.Answer, check.PassStands.Roles)
			}
		}
	})
}
