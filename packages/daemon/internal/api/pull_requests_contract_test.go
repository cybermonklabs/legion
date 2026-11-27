package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// pullRequestsProject is the Dispatch project the issues below are recorded under,
// pullRequestsNow the instant their state is projected at, and pullRequestsCaller the address its
// callers ask from, in no pod, so each is served the contract it states.
const (
	pullRequestsProject = "LEGION"
	pullRequestsCaller  = "192.0.2.1:41000"
)

var pullRequestsNow = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

// pullRequestsState is the daemon's own state source over a real store: the state the projection
// builds in the route's own snapshot, with no claim, as the daemon's source answers it.
type pullRequestsState struct{}

func (pullRequestsState) State(ctx context.Context, tx pgx.Tx) (api.State, []supervise.Claim, error) {
	state, err := projection.Project(ctx, projection.Inputs{Tx: tx, Store: record.NewStore(), Project: pullRequestsProject, Now: pullRequestsNow})
	return state, nil, err
}

func (pullRequestsState) ObservedState(ctx context.Context, tx pgx.Tx) (api.State, []supervise.Claim, error) {
	observed, err := projection.Observe(ctx, projection.ObservedInputs{Tx: tx, Store: record.NewStore(), Project: pullRequestsProject, Now: pullRequestsNow})
	return observed, nil, err
}

// pullRequestsEntry is the number this build gives the contract entry that added each issue's
// `pullRequests`, the hold reason `pull-requests` and its held word `pull_requests`.
func pullRequestsEntry(t *testing.T) int {
	t.Helper()
	for _, entry := range api.NumberedEntries() {
		if entry.Slug == "pull-requests" {
			return entry.Number
		}
	}
	t.Fatal("this build registers no contract entry pull-requests")
	return 0
}

// servedPullRequests is what one served issue carries of its pull requests: `pullRequest` as
// owner/name#number, and each of `pullRequests` the same way with its task and whether the issue
// links it, nil where the issue carries no `pullRequests` key.
type servedPullRequests struct {
	pullRequest string
	set         []string
}

// pullRequestsOf reads servedPullRequests off one served issue.
func pullRequestsOf(t *testing.T, issue map[string]json.RawMessage) servedPullRequests {
	t.Helper()
	type pull struct {
		Repo   string `json:"repo"`
		Number int    `json:"number"`
		Task   string `json:"task"`
		Linked *bool  `json:"linked"`
	}
	var served servedPullRequests
	var one pull
	if raw, ok := issue["pullRequest"]; ok {
		if err := json.Unmarshal(raw, &one); err != nil {
			t.Fatalf("decode the pull request %s: %v", raw, err)
		}
		served.pullRequest = fmt.Sprintf("%s#%d", one.Repo, one.Number)
	}
	raw, carried := issue["pullRequests"]
	if !carried {
		return served
	}
	var each []pull
	if err := json.Unmarshal(raw, &each); err != nil {
		t.Fatalf("decode the pull requests %s: %v", raw, err)
	}
	served.set = []string{}
	for _, pr := range each {
		if pr.Linked == nil {
			t.Fatalf("the pull request %s#%d of the set carries no `linked`, which every pull request of it carries", pr.Repo, pr.Number)
		}
		served.set = append(served.set, fmt.Sprintf("%s#%d task=%q linked=%t", pr.Repo, pr.Number, pr.Task, *pr.Linked))
	}
	return served
}

// stateServedAt is the answer of GET target, the state or an issue's record, asked from
// pullRequestsCaller stating contract in the plugin's contract header.
func stateServedAt(t *testing.T, handler http.Handler, target string, contract int) []byte {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.RemoteAddr = pullRequestsCaller
	request.Header.Set("Legion-Plugin-Contract", strconv.Itoa(contract))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s stating %d = %d, want 200; body %s", target, contract, recorder.Code, recorder.Body)
	}
	return recorder.Body.Bytes()
}

// A caller below the contract entry that added `pullRequests` (dispatch://INFRA-5910 D8, decisions
// 13 and 14) reads the newest open pull request of an issue's set as `pullRequest`, as every caller
// does, and is served no `pullRequests`, a key its strict reader does not name. The state is the
// daemon's own projection over a real store, served through the route each plugin reads.
//
// LEGION-401 has no plan rows and two open pull requests: acme/widgets#40 on its own branch, the
// issue's merge, and acme/gadgets#7, which the issue links, which GitHub changed after #40 and whose
// number is the lower. Each caller reads acme/gadgets#7 as `pullRequest`: the newest by GitHub's
// clock (record.PullRequest.HeadUpdatedAt), neither the issue's merge nor the higher number. A
// caller at the entry reads both under `pullRequests`, in the set's order of repositories and
// numbers, each marked linked or not, with no task, since the issue has no plan rows. The record
// route serves the issue as the state does.
//
// LEGION-402 is held from merging for pull-requests, where no merge was in hand to wait with. A
// caller at the entry reads the reason and the held word `pull_requests`; a caller below it reads
// the issue held from merging with no reason, as a hold of no reason is served
// (withoutUnadmittedHoldReason), and the held row as `unclassified`, as every word its reader does
// not admit is served (withoutUnadmittedHeldWords).
//
// The fault it catches is a body below the entry that carries `pullRequests`, which that reader
// refuses whole, and a `pullRequest` that is the issue's merge while a newer one of its set is open.
func TestACallerBelowTheContractSeesTheNewestOpenPullRequest(t *testing.T) {
	entry := pullRequestsEntry(t)
	ctx := context.Background()
	st := testpg.CloneStore(t, "legion_api_test")
	records := record.NewStore()
	token, err := claim.ProjectToken(pullRequestsProject)
	if err != nil {
		t.Fatalf("project token of %s: %v", pullRequestsProject, err)
	}
	const open, held = "LEGION-401", "LEGION-402"
	root := func(key string, at phase.Phase, status, rank string) record.Issue {
		return record.Issue{Key: key, Tree: key, Project: pullRequestsProject, Title: key, Phase: at, Generation: 1, Status: status,
			Rank: rank, HandedOver: true, LastDispatchSeq: 1, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	}
	heldIssue := root(held, phase.Held, "needs_review", "B")
	heldIssue.Hold = &record.Hold{From: phase.Merging, Reason: record.HoldForPullRequests}
	if err := store.Transact(ctx, st.Pool(), func(_ context.Context, tx pgx.Tx) error {
		for _, issue := range []record.Issue{root(open, phase.Implementing, "in_progress", "A"), heldIssue} {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
			if _, err := records.OpenTreeLifecycle(ctx, tx, token, issue.Key, treelifecycle.AuthorityWorkflow); err != nil {
				return err
			}
			// The person's approval of each root's spec, the step every tree requires
			// (record.TreeGate.Required), so neither root's tree is held by its gate.
			if err := records.PutGate(ctx, tx, record.DesignGate{Issue: issue.Key, Step: record.DesignStepSpec, ArtifactID: "artifact-" + issue.Key,
				LatestVersion: 1, ApprovedVersion: new(1), EverOpened: true}); err != nil {
				return err
			}
		}
		for _, pr := range []record.PullRequest{
			{Issue: open, Repo: "acme/widgets", Number: 40, Branch: "legion/" + open, Base: "main", HeadSHA: "4040404040404040404040404040404040404040",
				State: record.PullRequestOpen, HeadUpdatedAt: pullRequestsNow.Add(-2 * time.Hour), Generation: 1, Failing: []string{}},
			{Issue: open, Repo: "acme/gadgets", Number: 7, Branch: "fix/widgets-401", Base: "main", HeadSHA: "0707070707070707070707070707070707070707",
				State: record.PullRequestOpen, HeadUpdatedAt: pullRequestsNow.Add(-time.Hour), Generation: 1, Failing: []string{}},
		} {
			if err := records.PutPullRequest(ctx, tx, pr); err != nil {
				return err
			}
		}
		return records.ReplacePullRequestLinks(ctx, tx, open, []record.PullRequestLink{{Issue: open, Repo: "acme/gadgets", Number: 7}})
	}); err != nil {
		t.Fatalf("record the issues: %v", err)
	}

	handler := api.NewServer("127.0.0.1", 8437, api.Options{State: pullRequestsState{}, StateTransactions: st.Pool()}).Handler
	for _, tc := range []struct {
		name       string
		contract   int
		set        []string
		holdReason string
		heldWord   record.HeldReason
	}{
		{name: "a caller stating the entry", contract: entry,
			set:        []string{`acme/gadgets#7 task="" linked=true`, `acme/widgets#40 task="" linked=false`},
			holdReason: string(record.HoldForPullRequests), heldWord: record.HeldPullRequests},
		{name: "a caller stating the contract before it", contract: entry - 1, heldWord: record.HeldUnclassified},
	} {
		var body struct {
			Issues    map[string]map[string]json.RawMessage `json:"issues"`
			Admission struct {
				Held []api.HeldIssue `json:"held"`
			} `json:"admission"`
		}
		if err := json.Unmarshal(stateServedAt(t, handler, "/legion/v1/state", tc.contract), &body); err != nil {
			t.Fatalf("%s: decode the state: %v", tc.name, err)
		}
		want := servedPullRequests{pullRequest: "acme/gadgets#7", set: tc.set}
		if got := pullRequestsOf(t, body.Issues[open]); got.pullRequest != want.pullRequest || !slices.Equal(got.set, want.set) || (got.set == nil) != (want.set == nil) {
			t.Errorf("%s: the state serves %s the pull request %s and the set %q (key carried %t), want %s and %q (key carried %t)",
				tc.name, open, got.pullRequest, got.set, got.set != nil, want.pullRequest, want.set, want.set != nil)
		}
		var issueRecord map[string]json.RawMessage
		if err := json.Unmarshal(stateServedAt(t, handler, "/legion/v1/issues/"+open+"/record", tc.contract), &issueRecord); err != nil {
			t.Fatalf("%s: decode the record of %s: %v", tc.name, open, err)
		}
		if got := pullRequestsOf(t, issueRecord); got.pullRequest != want.pullRequest || !slices.Equal(got.set, want.set) || (got.set == nil) != (want.set == nil) {
			t.Errorf("%s: the record of %s serves the pull request %s and the set %q (key carried %t), want what the state serves, %s and %q",
				tc.name, open, got.pullRequest, got.set, got.set != nil, want.pullRequest, want.set)
		}

		stands, from, reason := wordOf(t, body.Issues[held], "phase"), wordOf(t, body.Issues[held], "heldFrom"), wordOf(t, body.Issues[held], "holdReason")
		if stands != string(phase.Held) || from != string(phase.Merging) || reason != tc.holdReason {
			t.Errorf("%s: %s is served at %q held from %q for %q, want held from merging for %q", tc.name, held, stands, from, reason, tc.holdReason)
		}
		if row, named := heldRow(body.Admission.Held, held); !named || row.Reason != tc.heldWord {
			t.Errorf("%s: admission.held names %s as %+v, want the word %s", tc.name, held, body.Admission.Held, tc.heldWord)
		}
	}
}

// wordOf is the string issue carries under key, "" where it carries none.
func wordOf(t *testing.T, issue map[string]json.RawMessage, key string) string {
	t.Helper()
	raw, carried := issue[key]
	if !carried {
		return ""
	}
	var word string
	if err := json.Unmarshal(raw, &word); err != nil {
		t.Fatalf("decode %s %s: %v", key, raw, err)
	}
	return word
}
