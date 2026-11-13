package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// twoChecks is a Verification document with one pre-merge check and one after-merge check.
const twoChecks = ":::check{#k1 surface=\"cli\" run=\"go test ./...\" negative_control=\"Break it and watch it fail.\"}\n" +
	"The server records a result against a check.\n" +
	":::\n\n" +
	":::check{#k2 when=\"after_merge\"}\n" +
	"Production answers at the merged commit.\n" +
	":::\n"

// afterMergeAndDefaultChecks is a Verification document with one after-merge check and one
// default-branch check, the pair that asks two questions about one commit: an issue whose work
// merged into an integration branch has its after-merge check measured against that branch and
// its default-branch check against the trunk. The after-merge check comes first, so a read
// answers it first and an answer wrongly shared between the two would be the integration
// branch's.
const afterMergeAndDefaultChecks = ":::check{#k4 when=\"after_merge\"}\n" +
	"The branch the work merged into answers at the merged commit.\n" +
	":::\n\n" +
	":::check{#k5 when=\"default_branch\"}\n" +
	"The trunk answers at the merged commit.\n" +
	":::\n"

// siblingChecks is two pre-merge checks, so a rewording of one can be seen to leave the other's
// results alone; siblingChecksReworded changes k1's words and leaves k3's.
const siblingChecks = ":::check{#k1 surface=\"cli\" run=\"go test ./...\"}\n" +
	"The server records a result against a check.\n" +
	":::\n\n" +
	":::check{#k3 surface=\"cli\" run=\"go test ./...\"}\n" +
	"A sibling check nobody reworded.\n" +
	":::\n"

const siblingChecksReworded = ":::check{#k1 surface=\"cli\" run=\"go test ./...\"}\n" +
	"The server records a result against a check, named by its block id.\n" +
	":::\n\n" +
	":::check{#k3 surface=\"cli\" run=\"go test ./...\"}\n" +
	"A sibling check nobody reworded.\n" +
	":::\n"

const (
	headCommit    = "1111111111111111111111111111111111111111"
	newHeadCommit = "2222222222222222222222222222222222222222"
	mergeCommit   = "3333333333333333333333333333333333333333"
	// integrationCommit is on the integration branch an issue's pull request merged into and
	// on no other branch: an after-merge result of that issue at this commit stands at a basis,
	// and a default-branch result of it does not.
	integrationCommit = "4444444444444444444444444444444444444444"
	// strayCommit is on no branch at all: a commit built and never landed.
	strayCommit = "5555555555555555555555555555555555555555"
	// baseMergeCommit is the commit GitHub made when a pull request merged into its base
	// branch, which Dispatch records as on that branch as it records the merge. A repository
	// that merges by squash puts a squash of the work on the default branch and never this
	// commit, so once the base branch is deleted that record is the whole of what can answer
	// for it.
	baseMergeCommit = "6666666666666666666666666666666666666666"
	// nearHeadCommit shares its first twelve characters with headCommit and differs after
	// them: the pair an abbreviated commit cannot tell apart.
	nearHeadCommit = "111111111111aaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type recordedResult struct {
	Result model.CheckResult `json:"result"`
	Line   string            `json:"line"`
}

type issueResults struct {
	Issue                  string             `json:"issue"`
	VerificationArtifactID string             `json:"verification_artifact_id"`
	Checks                 []model.CheckBlock `json:"checks"`
}

// resultBody is a well-formed record body with the given check and verdict.
func resultBody(check, verdict, commit string, fields map[string]any) map[string]any {
	body := map[string]any{"check": check, "role": "implementer", "verdict": verdict, "commit": commit}
	for name, value := range fields {
		body[name] = value
	}
	return body
}

func recordResult(t *testing.T, handler http.Handler, issue string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/results", body, "alice")
}

func readResults(t *testing.T, handler http.Handler, issue string) issueResults {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", issue, response.Code, response.Body.String())
	}
	return decodeBody[issueResults](t, response)
}

// checkOf is the check of the read with that block id.
func checkOf(t *testing.T, read issueResults, blockID string) model.CheckBlock {
	t.Helper()
	for _, check := range read.Checks {
		if check.BlockID == blockID {
			return check
		}
	}
	t.Fatalf("the read holds no check %s: %+v", blockID, read.Checks)
	return model.CheckBlock{}
}

// writeHead stores one pull request head row directly, as the intake's write of a
// `pull_request` envelope would leave it, on the base branch `main` that a call site saying
// nothing about a base branch means. An empty issueKey is a pull request the attribution
// named no issue for.
func writeHead(t *testing.T, database *store.Store, repo string, number int, sha, state, issueKey string) {
	t.Helper()
	writeHeadOnBase(t, database, repo, number, sha, state, issueKey, "main")
}

// writeHeadOnBase stores one pull request head row whose base branch the caller names: the
// row delivery.BaseBranchMergedFor answers an issue's merged branch from, where the row is
// merged. The conflict clause writes every column either caller means to write, so a second
// row for the same pull request leaves none of them as the first write left it.
func writeHeadOnBase(t *testing.T, database *store.Store, repo string, number int, sha, state, issueKey, baseRef string) {
	t.Helper()
	var attributed *string
	if issueKey != "" {
		attributed = &issueKey
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, event_at, checked_at)
		values ($1, $2, $3, 'legion/work', $4, $5, $6, now(), now())
		on conflict (repo, number) do update set head_sha = excluded.head_sha, base_ref = excluded.base_ref,
			state = excluded.state, issue_key = excluded.issue_key
	`, repo, number, sha, baseRef, state, attributed); err != nil {
		t.Fatalf("store the head of %s#%d on %s: %v", repo, number, baseRef, err)
	}
}

// recordMerge records one issue's merged pull request as production records a merge, through
// the writer that seeds the one fact the merge itself proves: the base branch held the merge
// commit, written as a branch_commits yes that is kept for good
// (delivery.UpsertPullRequestHead). The head is headCommit and the merge commit
// baseMergeCommit, which is what every caller here means by them.
func recordMerge(t *testing.T, database *store.Store, repo string, number int, issueKey, baseRef string) {
	t.Helper()
	attributed := issueKey
	if err := delivery.UpsertPullRequestHead(context.Background(), database.Pool, nil, delivery.PullRequestHead{
		Repo: repo, Number: number, HeadSHA: headCommit, HeadRef: "legion/work",
		BaseRef: baseRef, State: "merged", IssueKey: &attributed, AttributionSource: "title",
		AttributionComplete: true, Title: "A change",
		URL:            fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
		MergeCommitSHA: baseMergeCommit,
	}); err != nil {
		t.Fatalf("record the merge of %s#%d into %s: %v", repo, number, baseRef, err)
	}
}

// writeBranchAnswer stores what Dispatch holds about one commit of one branch, as a compare
// read records it (delivery's writeBranchCommit): a yes is kept for good and a no for
// delivery.BranchCommitStaleAfter, and either is read before any call to GitHub. The row is
// keyed by the lowercased repository, as both of delivery's own statements over the table
// are.
func writeBranchAnswer(t *testing.T, database *store.Store, repo, branch, sha string, onBranch bool) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into branch_commits (repo, branch, sha, on_branch, checked_at)
		values (lower($1), $2, $3, $4, now())
		on conflict (repo, branch, sha) do update set on_branch = excluded.on_branch, checked_at = excluded.checked_at
	`, repo, branch, sha, onBranch); err != nil {
		t.Fatalf("store whether %s@%s is on %s: %v", repo, sha, branch, err)
	}
}

// The pull-request body line carries the reading the role recorded, because that is the record
// a reader of the pull request cannot see anywhere else: the check's own words are on the
// issue, and a pass and a fail would carry them alike. A reading over the budget is cut at a
// word boundary and marked, a short one prints whole, and a result with no reading says so.
func TestThePullRequestLineCarriesTheReading(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESL", "Line", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	long := "The rig drove the command against a local Dispatch and the refusal named the flag it wanted, which is the whole of the case."
	for _, row := range []struct {
		name     string
		observed string
		want     string
	}{
		{
			name:     "a reading over the budget is cut at a word boundary",
			observed: long,
			want:     "The rig drove the command against a local Dispatch and the refusal named the…",
		},
		{
			name:     "a reading under the budget prints whole",
			observed: "Every one of the twenty runs printed its listing.",
			want:     "Every one of the twenty runs printed its listing.",
		},
		{
			name:     "a result with no reading says so",
			observed: "",
			want:     "The role recorded no reading.",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			body := map[string]any{"surface": "cli"}
			if row.observed != "" {
				body["observed"] = row.observed
			}
			recorded := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, body))
			if recorded.Code != http.StatusCreated {
				t.Fatalf("record a result: status=%d body=%s", recorded.Code, recorded.Body.String())
			}
			line := decodeBody[recordedResult](t, recorded).Line
			want := "- Check k1 (before_merge): pass at " + headCommit + ", recorded by implementer. " + row.want
			if line != want {
				t.Fatalf("line = %q, want %q", line, want)
			}
			if strings.Contains(line, "The server records a result against a check") {
				t.Fatalf("the line carries the check's own words: %q", line)
			}
		})
	}
}

// A result is recorded against a check of the issue's own Verification document and against a
// check of an ancestor's; the event goes on the check's issue either way, and the answer carries
// the pull-request body line. An id neither holds is refused, and one two of them share is
// refused until the caller names which document it means.
func TestResultRecordsAgainstAChecksOfTheIssueAndOfAnAncestor(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	parent := createInteractionIssue(t, handler, "RES", "Parent", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+parent.Key, "verification.md", twoChecks)
	child := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RES", "title": "Child", "parent": parent.Key, "force": true}, "alice"))

	own := recordResult(t, handler, parent.Key, resultBody("k1", "pass", headCommit, map[string]any{
		"surface": "cli", "run": "go test ./...", "observed": "ok", "negative_control": "broke it",
	}))
	if own.Code != http.StatusCreated {
		t.Fatalf("record a result on the check's own issue: status=%d body=%s", own.Code, own.Body.String())
	}
	recorded := decodeBody[recordedResult](t, own)
	if recorded.Result.CheckIssueKey != parent.Key || recorded.Result.IssueKey != parent.Key {
		t.Fatalf("result = recorded on %s against %s, want both %s", recorded.Result.IssueKey, recorded.Result.CheckIssueKey, parent.Key)
	}
	wantLine := "- Check k1 (before_merge): pass at " + headCommit +
		", recorded by implementer. ok"
	if recorded.Line != wantLine {
		t.Fatalf("line = %q, want %q", recorded.Line, wantLine)
	}
	if strings.Contains(recorded.Line, "dispatch://") || strings.Contains(recorded.Line, testServerURL) {
		t.Fatalf("the pull-request line carries a Dispatch reference: %q", recorded.Line)
	}

	// The child has no Verification document of its own, so its result names its parent's check
	// and is recorded against the parent, whose event log carries it.
	fromChild := recordResult(t, handler, child.Key, resultBody("k1", "fail", newHeadCommit, map[string]any{"failed": 2}))
	if fromChild.Code != http.StatusCreated {
		t.Fatalf("record a result against an ancestor's check: status=%d body=%s", fromChild.Code, fromChild.Body.String())
	}
	childResult := decodeBody[recordedResult](t, fromChild)
	if childResult.Result.IssueKey != child.Key || childResult.Result.CheckIssueKey != parent.Key {
		t.Fatalf("child's result = recorded on %s against %s, want %s against %s",
			childResult.Result.IssueKey, childResult.Result.CheckIssueKey, child.Key, parent.Key)
	}
	event := lastIssueEventPayload(t, database, parent.Key)
	if event["result_id"] != childResult.Result.ID || event["issue"] != child.Key || event["check_issue"] != parent.Key {
		t.Fatalf("result.recorded payload = %+v, want the child's result on the parent's check", event)
	}
	// Both results stand under the parent's check, newest first.
	read := readResults(t, handler, parent.Key)
	if read.VerificationArtifactID == "" || len(read.Checks) != 2 {
		t.Fatalf("the parent's results read = %+v, want its Verification document and two checks", read)
	}
	if results := checkOf(t, read, "k1").Results; len(results) != 2 || results[0].ID != childResult.Result.ID {
		t.Fatalf("k1's results = %+v, want the child's newest of two", results)
	}

	// An id no Verification document in reach holds.
	unknown := recordResult(t, handler, child.Key, resultBody("k9", "pass", headCommit, nil))
	if code, _ := refusalOf(t, unknown); unknown.Code != http.StatusBadRequest || code != "UNKNOWN_CHECK" {
		t.Fatalf("an unknown check: status=%d body=%s", unknown.Code, unknown.Body.String())
	}

	// The child writes a Verification document of its own that repeats k1: the bare id now names
	// two checks, and only the canonical reference decides between them.
	childVerification := uploadDocument(t, handler, "/api/v1/issues/"+child.Key, "verification.md", twoChecks)
	ambiguous := recordResult(t, handler, child.Key, resultBody("k1", "pass", headCommit, nil))
	if code, _ := refusalOf(t, ambiguous); ambiguous.Code != http.StatusConflict || code != "CHECK_AMBIGUOUS" {
		t.Fatalf("an ambiguous check: status=%d body=%s", ambiguous.Code, ambiguous.Body.String())
	}
	named := recordResult(t, handler, child.Key, resultBody(
		"dispatch://"+child.Key+"/artifact/"+childVerification.Artifact.Slug+"#b-k1", "pass", headCommit, nil))
	if named.Code != http.StatusCreated {
		t.Fatalf("record against the named document: status=%d body=%s", named.Code, named.Body.String())
	}
	if against := decodeBody[recordedResult](t, named).Result.CheckIssueKey; against != child.Key {
		t.Fatalf("the named result was recorded against %s, want the child's own document", against)
	}
}

// The shape the incident had: one check with three results, the first recorded at a commit
// whose first twelve characters are the pull request head's and whose remainder is not, then
// two at the head itself. The read answers a check's results newest first, so a reader that
// takes one result per check — `dispatch results` prints results[0] — takes the last recorded
// rather than the first.
func TestCheckResultsComeBackNewestFirst(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESN", "Newest first", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	near := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key,
		resultBody("k1", "fail", nearHeadCommit, map[string]any{"failed": 1})))
	earlier := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key,
		resultBody("k1", "fail", headCommit, map[string]any{"failed": 1})))
	last := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key,
		resultBody("k1", "pass", headCommit, nil)))

	results := checkOf(t, readResults(t, handler, issue.Key), "k1").Results
	if len(results) != 3 {
		t.Fatalf("k1's results = %+v, want the three recorded", results)
	}
	for position, want := range []model.CheckResult{last.Result, earlier.Result, near.Result} {
		if results[position].ID != want.ID {
			t.Fatalf("results[%d] = %s recorded at %s, want %s recorded at %s: the read answers newest first",
				position, results[position].ID, results[position].CreatedAt, want.ID, want.CreatedAt)
		}
	}
	if results[0].Verdict != "pass" {
		t.Fatalf("results[0].verdict = %q, want the last recorded result's pass", results[0].Verdict)
	}
}

// A pass that counts a skipped case contradicts itself.
func TestResultRefusesAPassThatCountsASkippedCase(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESC", "Contradiction", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	refused := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, map[string]any{"skipped": 1}))
	code, message := refusalOf(t, refused)
	if refused.Code != http.StatusBadRequest || code != "RESULT_CONTRADICTS" || !strings.Contains(message, "1 skipped") {
		t.Fatalf("a pass counting a skipped case: status=%d body=%s", refused.Code, refused.Body.String())
	}
	// The same counts with a failing verdict are no contradiction.
	if accepted := recordResult(t, handler, issue.Key, resultBody("k1", "fail", headCommit, map[string]any{"skipped": 1})); accepted.Code != http.StatusCreated {
		t.Fatalf("a fail counting a skipped case: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
}

// A closed issue takes a result for an after-merge check — the production check runs only once
// the merge has closed the issue — and refuses a pre-merge one, while a document write on it
// stays refused. A comment is no such write: a closed issue takes one, and this states that the
// result route's exemption is not what lets it through.
func TestClosedIssueTakesAnAfterMergeResultAndStillRefusesADocumentWrite(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESX", "Closed", "A spec\n")
	owner := "/api/v1/issues/" + issue.Key
	uploadDocument(t, handler, owner, "verification.md", twoChecks)
	closed := dispatchRequest(t, handler, http.MethodPatch, owner,
		map[string]any{"status": "done"}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close the issue: status=%d body=%s", closed.Code, closed.Body.String())
	}

	after := recordResult(t, handler, issue.Key, resultBody("k2", "pass", mergeCommit, nil))
	if after.Code != http.StatusCreated {
		t.Fatalf("record an after-merge result on a closed issue: status=%d body=%s", after.Code, after.Body.String())
	}
	before := recordResult(t, handler, issue.Key, resultBody("k1", "pass", mergeCommit, nil))
	if code, _ := refusalOf(t, before); before.Code != http.StatusConflict || code != "ISSUE_CLOSED" {
		t.Fatalf("a pre-merge result on a closed issue: status=%d body=%s", before.Code, before.Body.String())
	}
	// The exemption lives in this route alone among the writes a closed issue refuses.
	write := dispatchRequest(t, handler, http.MethodPost, owner+"/artifacts",
		map[string]string{"name": "notes.md", "content": "# Notes\n"}, "alice")
	if code, _ := refusalOf(t, write); write.Code != http.StatusConflict || code != "ISSUE_CLOSED" {
		t.Fatalf("a document write on a closed issue: status=%d body=%s", write.Code, write.Body.String())
	}
	comment := dispatchRequest(t, handler, http.MethodPost, owner+"/comments", map[string]any{"body": "A note."}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("a comment on a closed issue: status=%d body=%s, want 201", comment.Code, comment.Body.String())
	}
}

// A pre-merge result stands at the head of an open pull request of its check's issue. A push
// that moves that head leaves the result at another commit with no write of Dispatch's, and a
// result at the new head stands at it. Rewording one check moves its own words and leaves its
// sibling's alone, and restoring the words moves them back.
//
// The two facts are asserted apart at every step, which is what the surface states apart: a
// result of the reworded check still stands at the head Dispatch holds, and what changed is
// its check's words. One word for the two would hide which of them a reader is looking at.
//
// The issue resolves to a repository the App can read, which is what makes "this commit heads
// no pull request I know" another commit rather than an unknown.
func TestBeforeMergeResultFollowsTheHeadAndTheChecksWords(t *testing.T) {
	t.Parallel()
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true},
		func(string, string) string { return "diverged" })
	issue := issueInRepository(t, handler, "RESH", "acme/widgets", "Heads")
	owner := "/api/v1/issues/" + issue.Key
	uploadDocument(t, handler, owner, "verification.md", siblingChecks)
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", issue.Key)

	first := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	stands := standsAt(t, readResults(t, handler, issue.Key), "k1", first.Result.ID,
		"a result at the open head", model.CommitAtBasis, false)
	if stands.Basis == nil || stands.Basis.Kind != model.CheckBasisPullRequest || stands.Basis.Commit != headCommit {
		t.Fatalf("a result at the open head stands at %+v, want the pull request's head %s", stands.Basis, headCommit)
	}

	// A push moves the head: the same result, with no Dispatch write of its own, is at another
	// commit, and a result at the new head stands at it.
	writeHead(t, database, "acme/widgets", 7, newHeadCommit, "open", issue.Key)
	standsAt(t, readResults(t, handler, issue.Key), "k1", first.Result.ID,
		"a result at the old head", model.CommitOtherCommit, false)
	moved := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", newHeadCommit, nil)))
	standsAt(t, readResults(t, handler, issue.Key), "k1", moved.Result.ID,
		"a result at the new head", model.CommitAtBasis, false)

	// A pull request attributed to another issue never answers for this one.
	writeHead(t, database, "acme/widgets", 8, mergeCommit, "open", "")
	stranger := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", mergeCommit, nil)))
	standsAt(t, readResults(t, handler, issue.Key), "k1", stranger.Result.ID,
		"a result at another issue's head", model.CommitOtherCommit, false)

	// Rewording k1 moves its own words and leaves its sibling's alone, and restoring them moves
	// them back. Both checks run before the merge, so the only thing that moves between them is
	// their own words: the reworded check's result stands at the head it always did.
	sibling := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k3", "pass", newHeadCommit, nil)))
	uploadDocument(t, handler, owner, "verification.md", siblingChecksReworded)
	read := readResults(t, handler, issue.Key)
	standsAt(t, read, "k1", moved.Result.ID, "a result of a reworded check", model.CommitAtBasis, true)
	standsAt(t, read, "k3", sibling.Result.ID, "the sibling check's result", model.CommitAtBasis, false)
	// The reworded check's version and the result's own check_version are both on the answer,
	// which is what a reader names the two wordings by.
	if check, result := checkOf(t, read, "k1"), resultRead(t, read, "k1", moved.Result.ID); check.Version == result.CheckVersion {
		t.Fatalf("the reworded check is at version %d and its result recorded version %d, want two versions",
			check.Version, result.CheckVersion)
	}
	uploadDocument(t, handler, owner, "verification.md", siblingChecks)
	standsAt(t, readResults(t, handler, issue.Key), "k1", moved.Result.ID,
		"a result of a check whose words were restored", model.CommitAtBasis, false)
}

// resultRead is one result of one check as the read answered it.
func resultRead(t *testing.T, read issueResults, blockID, resultID string) model.CheckResult {
	t.Helper()
	for _, result := range checkOf(t, read, blockID).Results {
		if result.ID == resultID {
			return result
		}
	}
	t.Fatalf("check %s holds no result %s", blockID, resultID)
	return model.CheckResult{}
}

// standsAt asserts the two facts the surface states apart for one result, and answers where
// its commit stands so a caller can read the basis it matched. stands is model.CommitAtBasis,
// model.CommitOtherCommit or model.CommitUnknown, and moved is whether the check's words moved
// after the result was recorded. what names the case the failure is about.
//
// Both are named at every call on purpose. A result at a compared commit whose check was
// reworded is at_basis with moved words, and a test that asserted one word for the two would
// re-make the defect this surface answers (dispatch://INFRA-1522).
func standsAt(
	t *testing.T, read issueResults, blockID, resultID, what, stands string, moved bool,
) model.ResultCommitStands {
	t.Helper()
	result := resultRead(t, read, blockID, resultID)
	if result.CommitStands == nil || result.CheckWordsMoved == nil {
		t.Fatalf("%s carries commit_stands=%+v and check_words_moved=%v, want both",
			what, result.CommitStands, result.CheckWordsMoved)
	}
	if result.CommitStands.Answer != stands || *result.CheckWordsMoved != moved {
		t.Fatalf("%s stands %q with the check's words moved=%v, want %q and moved=%v",
			what, result.CommitStands.Answer, *result.CheckWordsMoved, stands, moved)
	}
	return *result.CommitStands
}

// comparedNothing asserts that a check names nothing to compare its results with, for that
// reason, and that its pass stands as the gate reads it.
func comparedNothing(t *testing.T, read issueResults, blockID, reason, passStands string) {
	t.Helper()
	check := checkOf(t, read, blockID)
	if len(check.ComparedWith) != 0 || check.ComparedNothing != reason {
		t.Fatalf("check %s compared with %+v for the reason %q, want nothing compared for %q",
			blockID, check.ComparedWith, check.ComparedNothing, reason)
	}
	if check.PassStands.Answer != passStands {
		t.Fatalf("check %s answers pass_stands %q, want %q", blockID, check.PassStands.Answer, passStands)
	}
}

// issueInRepository seeds a project whose default repository is repo and one issue of it, so
// the check's issue resolves to that repository: the pair every answer about a result turns
// on.
func issueInRepository(t *testing.T, handler http.Handler, project, repo, title string) model.Issue {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects",
		map[string]string{"key": project, "name": title}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+repo,
		map[string]any{"project": project, "default": true}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("map %s to %s: status=%d body=%s", repo, project, response.Code, response.Body.String())
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": project, "title": title, "spec": "A spec\n"}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue of %s: status=%d body=%s", project, response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// resultsGitHub is a Dispatch holding an App key whose GitHub answers `handler`, with the
// installation lookup and token mint every read needs already served. Its GitHub holds every
// commit asked about: resultsGitHubHolding is the same fixture with the commits named.
func resultsGitHub(t *testing.T, repos map[string]bool, compare func(base, head string) string) (http.Handler, *store.Store) {
	t.Helper()
	return resultsGitHubHolding(t, repos, nil, compare)
}

// resultsGitHubHolding is resultsGitHub with the commits the repositories hold seeded: held
// drives `GET /repos/{owner}/{repo}/commits/{sha}`, the read refuseUnknownCommit turns on. A
// commit held names is 200, and every other commit is the 422 whose message begins `No commit
// found for SHA`, the one answer that proves absence — the status alone does not, since GitHub
// sends it for a validation failure too. A nil held holds every commit, which is what
// resultsGitHub passes and what every test that never seeded a commit takes.
//
// The route is served here rather than left to the mux's own 404, which this codebase reads as
// "repository unreadable" and stores the result for: a test missing the route would prove
// nothing.
//
// Every repository's default branch is `main`: resultsGitHubWithDefaultBranch is the same
// fixture with that branch named.
func resultsGitHubHolding(t *testing.T, repos, held map[string]bool, compare func(base, head string) string) (http.Handler, *store.Store) {
	t.Helper()
	return resultsGitHubWithDefaultBranch(t, repos, held, "main", compare)
}

// resultsGitHubWithDefaultBranch is resultsGitHubHolding with the branch `GET
// /repos/{owner}/{repo}` answers as every repository's default named, so a test can tell the
// branch Dispatch resolved from a branch a fixture hard-coded. It is a variant rather than a
// widening of resultsGitHubHolding's signature, whose call sites are about commits and
// readable repositories and mean `main` by the default branch.
func resultsGitHubWithDefaultBranch(
	t *testing.T,
	repos, held map[string]bool,
	defaultBranch string,
	compare func(base, head string) string,
) (http.Handler, *store.Store) {
	t.Helper()
	return resultsGitHubWithDeletedBranch(t, repos, held, defaultBranch, noDeletedBranch, compare)
}

// noDeletedBranch is what resultsGitHubWithDeletedBranch is passed where no branch of the
// repository is deleted: no branch is called nothing, so every compare read is answered by
// `compare` and the fixture is resultsGitHubWithDefaultBranch exactly.
const noDeletedBranch = ""

// resultsGitHubWithDeletedBranch is resultsGitHubWithDefaultBranch with one branch GitHub no
// longer holds named: every compare read whose base is that branch answers `404 Not Found`,
// body and all, as a compare against a branch deleted when its pull request merged does —
// which a repository that deletes merged branches does on every merge.
//
// The deleted branch is named here rather than answered by `compare`, whose answer is
// GitHub's compare `status`, because a 404 is a different shape of answer: it says nothing at
// all about the commit, and the client reads it as a repository it cannot see
// (delivery.Branches.CommitOnBranch, which holds the pair off for five minutes and answers
// unknown meanwhile). It is a variant rather than a widening of the signature above for the
// reason that one is a variant of resultsGitHubHolding: every call site of that one is about
// a GitHub holding every branch it is asked about.
func resultsGitHubWithDeletedBranch(
	t *testing.T,
	repos, held map[string]bool,
	defaultBranch, deletedBranch string,
	compare func(base, head string) string,
) (http.Handler, *store.Store) {
	t.Helper()
	return resultsGitHubAnswering(t, repos, held, defaultBranch, func(base, head string) (int, string) {
		if deletedBranch != noDeletedBranch && base == deletedBranch {
			return http.StatusNotFound, ""
		}
		return http.StatusOK, compare(base, head)
	})
}

// resultsGitHubAnswering is the fixture every one above is made of, and the one a test reaches
// for when a compare read must fail with something other than the 404 a deleted branch
// answers: `answer` is given each read's base and head and returns the HTTP status GitHub
// sends and, where that status is 200, GitHub's own compare `status` field. It counts nothing
// of its own, so a test that must count the compare reads counts them inside `answer`, which
// sees the refused reads the compare callback above never does.
func resultsGitHubAnswering(
	t *testing.T,
	repos, held map[string]bool,
	defaultBranch string,
	answer func(base, head string) (int, string),
) (http.Handler, *store.Store) {
	t.Helper()
	handler, database, _ := resultsGitHubServed(t, repos, held, defaultBranch, answer, nil)
	return handler, database
}

// resultsGitHubServed is resultsGitHubAnswering answering also the server its handler mounts,
// for the test that must move what that server holds of its own: the default branch each
// repository was last resolved to, which delivery.Branches keeps for
// delivery.BranchCommitStaleAfter and which a test can reach no other way, since it cannot
// wait five minutes out. readThroughGitHub answers the same for the read-through's cache.
// checkRuns, where it is not nil, answers `GET /repos/{owner}/{repo}/commits/{sha}/check-runs`
// (resultsGitHubCheckRuns); a nil one leaves that read to the mux's 404.
func resultsGitHubServed(
	t *testing.T,
	repos, held map[string]bool,
	defaultBranch string,
	answer func(base, head string) (int, string),
	checkRuns http.HandlerFunc,
) (http.Handler, *store.Store, *server) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if !repos[r.PathValue("owner")+"/"+r.PathValue("repo")] {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_fake_77","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	// The read that says whether Dispatch can see the repository at all
	// (delivery.Branches.DefaultBranchOf) is refused for a repository repos does not hold, as
	// the installation lookup above is. A test takes a repository out of the map to lose it
	// after Dispatch has read it, which this route is what answers: the client caches the
	// installation it resolved and the token it minted, so the lookup alone would go on
	// saying yes.
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("owner") + "/" + r.PathValue("repo")
		if !repos[name] {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprintf(w, `{"full_name":%q,"default_branch":%q}`, name, defaultBranch)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		sha := r.PathValue("sha")
		if held != nil && !held[sha] {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprintf(w, `{"message":"No commit found for SHA: %s"}`, sha)
			return
		}
		fmt.Fprintf(w, `{"sha":%q}`, sha)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead}", func(w http.ResponseWriter, r *http.Request) {
		base, head, _ := strings.Cut(r.PathValue("basehead"), "...")
		status, comparison := answer(base, head)
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"message":%q}`, http.StatusText(status))
			return
		}
		fmt.Fprintf(w, `{"status":%q}`, comparison)
	})
	if checkRuns != nil {
		mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-runs", checkRuns)
	}
	upstream := githubtest.Server(t, githubtest.Spec{Handle: mux.ServeHTTP})
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream,
	})
	return handler, database, served
}

// A result at a commit the repository of the check's issue does not hold is refused, and
// nothing of it is stored. The one answer that proves absence is GitHub's 422 whose message
// begins `No commit found for SHA`, so a commit of the right shape that the repository never
// held — a mistyped head, a commit of a branch nobody pushed — never becomes a row a reader
// takes for the real head.
func TestResultAtACommitTheRepositoryDoesNotHoldIsRefused(t *testing.T) {
	handler, database := resultsGitHubHolding(t, map[string]bool{"acme/widgets": true},
		map[string]bool{headCommit: true}, func(string, string) string { return "diverged" })
	issue := issueInRepository(t, handler, "RESQ", "acme/widgets", "Unknown commit")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	// nearHeadCommit is 40 hex characters the repository holds no commit at: the fixture's
	// GitHub answers it 422, as GitHub itself does.
	refused := recordResult(t, handler, issue.Key, resultBody("k1", "pass", nearHeadCommit, nil))
	code, text := refusalOf(t, refused)
	if refused.Code != http.StatusBadRequest || code != "RESULT_COMMIT_UNKNOWN" {
		t.Fatalf("a result at a commit the repository does not hold: status=%d body=%s",
			refused.Code, refused.Body.String())
	}
	if !strings.Contains(text, nearHeadCommit) || !strings.Contains(text, "acme/widgets") {
		t.Fatalf("the refusal reads %q, want the commit %s and the repository acme/widgets named",
			text, nearHeadCommit)
	}

	// Nothing stored, in both halves of the one transaction the record would have written. The
	// row first: the check's results read empty.
	if results := checkOf(t, readResults(t, handler, issue.Key), "k1").Results; len(results) != 0 {
		t.Fatalf("the refused check holds %+v, want no result", results)
	}
	// Then the event. lastIssueEventPayload reads the newest event's payload alone, and a
	// payload carries no type, so the absence is asked of the events table by type: that says
	// the event was never appended, where the newest payload would say only that it is not the
	// newest one.
	if events := resultEventsOn(t, database, issue.Key); events != 0 {
		t.Fatalf("the check's issue holds %d result.recorded events, want none", events)
	}
}

// resultEventsOn counts the result.recorded events appended on one issue.
func resultEventsOn(t *testing.T, database *store.Store, key string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events where issue_key = $1 and type = 'result.recorded'
	`, key).Scan(&count); err != nil {
		t.Fatalf("count %s's result.recorded events: %v", key, err)
	}
	return count
}

// A result at a commit the repository does hold is stored, as every result was before the
// commit was asked about at all: the row stands under its check and the event is on the
// check's issue.
func TestResultAtACommitTheRepositoryHoldsIsStored(t *testing.T) {
	handler, database := resultsGitHubHolding(t, map[string]bool{"acme/widgets": true},
		map[string]bool{headCommit: true}, func(string, string) string { return "diverged" })
	issue := issueInRepository(t, handler, "RESY", "acme/widgets", "Held commit")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	recorded := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("a result at a commit the repository holds: status=%d body=%s",
			recorded.Code, recorded.Body.String())
	}
	stored := decodeBody[recordedResult](t, recorded)
	if stored.Result.CommitSHA != headCommit {
		t.Fatalf("the stored result is at %s, want %s", stored.Result.CommitSHA, headCommit)
	}
	results := checkOf(t, readResults(t, handler, issue.Key), "k1").Results
	if len(results) != 1 || results[0].ID != stored.Result.ID {
		t.Fatalf("k1's results = %+v, want the one result %s", results, stored.Result.ID)
	}
	if events := resultEventsOn(t, database, issue.Key); events != 1 {
		t.Fatalf("the check's issue holds %d result.recorded events, want one", events)
	}
}

// A check whose issue resolves to a repository the App is not installed on refuses nothing:
// the installation lookup answers 404, so no token is minted and the commit is never asked
// about, which says only that Dispatch cannot tell whether the commit is held. The result is
// stored although the fixture's GitHub holds no such commit anywhere, and the check compares
// with nothing for that reason, since nothing can teach Dispatch that repository's heads
// either.
func TestResultAtAnUnreadableRepositoryIsStoredAndComparesWithNothing(t *testing.T) {
	handler, database := resultsGitHubHolding(t, map[string]bool{"acme/widgets": true},
		map[string]bool{headCommit: true}, func(string, string) string { return "diverged" })
	hidden := issueInRepository(t, handler, "RESZ", "acme/hidden", "Unreadable")
	uploadDocument(t, handler, "/api/v1/issues/"+hidden.Key, "verification.md", twoChecks)

	recorded := recordResult(t, handler, hidden.Key, resultBody("k1", "pass", nearHeadCommit, nil))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("a result of a repository the App cannot read: status=%d body=%s",
			recorded.Code, recorded.Body.String())
	}
	stored := decodeBody[recordedResult](t, recorded)
	read := readResults(t, handler, hidden.Key)
	comparedNothing(t, read, "k1", model.ReasonRepositoryUnreadable, model.PassStandsUnknown)
	stands := standsAt(t, read, "k1", stored.Result.ID,
		"a result of a repository the App cannot read", model.CommitUnknown, false)
	if stands.Reason != model.ReasonRepositoryUnreadable {
		t.Fatalf("a result of a repository the App cannot read is unknown for %q, want %q",
			stands.Reason, model.ReasonRepositoryUnreadable)
	}
	if events := resultEventsOn(t, database, hidden.Key); events != 1 {
		t.Fatalf("the check's issue holds %d result.recorded events, want one", events)
	}
}

// The same refusal for an issue recorded under the name its repository was renamed away from.
// GitHub answers 404 for the old name's installation and knows only the canonical one, so the
// commit is asked about under the name the repository is called today, as the results read
// asks (refs.CanonicalRepo): the commit the repository holds is stored, and the one it does
// not is refused, naming the canonical repository. Asked under the old name, every such result
// would be stored instead, since an unreadable repository refuses nothing.
func TestResultOfAnIssueUnderARepositorysOldNameIsRefusedAtTheCanonicalName(t *testing.T) {
	handler, _ := resultsGitHubHolding(t, map[string]bool{"acme/gadgets": true},
		map[string]bool{headCommit: true}, func(string, string) string { return "diverged" })
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects",
		map[string]string{"key": "RESN", "name": "Renamed"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create RESN: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, mapping := range []struct {
		repo string
		body map[string]any
	}{
		{"acme/gadgets", map[string]any{"project": "RESN", "default": true}},
		{"oldco/gadgets", map[string]any{"project": "RESN", "renamed_to": "acme/gadgets"}},
	} {
		if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+mapping.repo,
			mapping.body, "alice"); response.Code != http.StatusOK {
			t.Fatalf("map %s: status=%d body=%s", mapping.repo, response.Code, response.Body.String())
		}
	}
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESN", "title": "Recorded under the old name", "spec": "A spec\n",
			"repo": "oldco/gadgets"}, "alice"))
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	refused := recordResult(t, handler, issue.Key, resultBody("k1", "pass", nearHeadCommit, nil))
	code, text := refusalOf(t, refused)
	if refused.Code != http.StatusBadRequest || code != "RESULT_COMMIT_UNKNOWN" {
		t.Fatalf("a result of an issue under oldco/gadgets at a commit acme/gadgets does not hold: status=%d body=%s",
			refused.Code, refused.Body.String())
	}
	if !strings.Contains(text, "acme/gadgets") {
		t.Fatalf("the refusal reads %q, want the canonical repository acme/gadgets named", text)
	}
	if recorded := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)); recorded.Code != http.StatusCreated {
		t.Fatalf("a result at a commit acme/gadgets holds: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
}

// An after-merge result of an issue Dispatch holds no pull request of at all stands at the
// repository's default branch for a commit that branch carries, and at another commit for one
// of an unmerged branch. Both answers are stored, so a second read of the same results asks
// GitHub nothing. The fallback itself, with the rows that choose a branch present and naming
// none for this issue, is TestAfterMergeResultFallsBackToTheTrunkWithoutAMergeOfTheIssue
// below.
func TestAfterMergeResultWithNoPullRequestFollowsTheDefaultBranch(t *testing.T) {
	t.Parallel()
	calls := 0
	handler, _ := resultsGitHub(t, map[string]bool{"acme/widgets": true}, func(base, head string) string {
		calls++
		if base != "main" {
			return "diverged"
		}
		if head == mergeCommit {
			return "behind"
		}
		return "diverged"
	})
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "RESD", "name": "Default"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets",
		map[string]any{"project": "RESD", "default": true}, "alice")
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESD", "title": "Default branch", "spec": "A spec\n"}, "alice"))
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	merged := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", mergeCommit, nil)))
	unmerged := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", newHeadCommit, nil)))
	read := readResults(t, handler, issue.Key)
	// The check names the one branch it compares with, under the name delivery resolved.
	if compared := checkOf(t, read, "k2").ComparedWith; len(compared) != 1 ||
		compared[0].Kind != model.CheckBasisBranch || compared[0].Branch != "main" {
		t.Fatalf("the after-merge check compared with %+v, want the default branch main alone", compared)
	}
	stands := standsAt(t, read, "k2", merged.Result.ID,
		"a result at a default-branch commit", model.CommitAtBasis, false)
	if stands.Basis == nil || stands.Basis.Branch != "main" {
		t.Fatalf("a result at a default-branch commit stands at %+v, want the branch main", stands.Basis)
	}
	standsAt(t, read, "k2", unmerged.Result.ID,
		"a result at an unmerged branch's commit", model.CommitOtherCommit, false)
	if stood := checkOf(t, read, "k2").PassStands; stood.Answer != model.PassStandsYes ||
		len(stood.Roles) != 1 || stood.Roles[0] != "implementer" {
		t.Fatalf("the after-merge check answers pass_stands %+v, want yes from the implementer", stood)
	}
	// Neither commit is asked about again: a yes is kept for good, and a no stands for five
	// minutes, so a second read inside them makes no GitHub call at all.
	before := calls
	readResults(t, handler, issue.Key)
	if calls != before {
		t.Fatalf("a second read made %d compare calls, want none (both answers are stored)", calls-before)
	}
}

// An after-merge result is compared with the branch its issue's pull request merged into and
// not the repository's default branch alone: an issue whose work merged into an integration
// branch has reached that branch and no other, so a commit that branch carries is what proves
// it, and a reviewer reading the Verification tab sees the pass it earned rather than a
// result at another commit that waits on the integration branch itself landing.
//
// The same read's default-branch check is compared with the trunk at the very same commit,
// which the integration branch carries and the trunk does not. Two questions about one commit
// have two answers, and both the stored row and the memo one read holds are keyed by the
// branch asked about: a key of the repository and the commit alone would hand the
// default-branch check the integration branch's yes.
func TestAfterMergeResultFollowsTheBranchTheIssueMergedInto(t *testing.T) {
	calls := 0
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true}, func(base, head string) string {
		calls++
		if base == "integration/x" && head == integrationCommit {
			return "behind"
		}
		return "diverged"
	})
	issue := issueInRepository(t, handler, "RESM", "acme/widgets", "Merged into an integration branch")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", afterMergeAndDefaultChecks)
	writeHeadOnBase(t, database, "acme/widgets", 41, headCommit, "merged", issue.Key, "integration/x")

	onBase := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k4", "pass", integrationCommit, nil)))
	offBase := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k4", "pass", strayCommit, nil)))
	trunk := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k5", "pass", integrationCommit, nil)))

	// All three in the one read: the default-branch check comes after the after-merge check in
	// the document, so its question about integrationCommit is asked after that commit has
	// already been answered yes for the integration branch.
	read := readResults(t, handler, issue.Key)
	// The after-merge check names both branches, the merge's own first, and the
	// default-branch check names the trunk alone.
	if compared := checkOf(t, read, "k4").ComparedWith; len(compared) != 2 ||
		compared[0].Branch != "integration/x" || compared[1].Branch != "main" {
		t.Fatalf("the after-merge check compared with %+v, want integration/x and then main", compared)
	}
	if compared := checkOf(t, read, "k5").ComparedWith; len(compared) != 1 || compared[0].Branch != "main" {
		t.Fatalf("the default-branch check compared with %+v, want main alone", compared)
	}
	stands := standsAt(t, read, "k4", onBase.Result.ID,
		"an after-merge result at a commit on the branch the issue merged into", model.CommitAtBasis, false)
	if stands.Basis == nil || stands.Basis.Branch != "integration/x" {
		t.Fatalf("that result stands at %+v, want the branch integration/x", stands.Basis)
	}
	standsAt(t, read, "k4", offBase.Result.ID,
		"an after-merge result at a commit on neither branch", model.CommitOtherCommit, false)
	standsAt(t, read, "k5", trunk.Result.ID,
		"a default-branch result at a commit only the integration branch holds", model.CommitOtherCommit, false)
	if stood := checkOf(t, read, "k5").PassStands; stood.Answer != model.PassStandsNo {
		t.Fatalf("the default-branch check answers pass_stands %+v, want no", stood)
	}
	// Each of the three questions is stored under the branch it asked about — a yes for good,
	// a no for five minutes — so a second read inside them spends no compare call at all.
	before := calls
	readResults(t, handler, issue.Key)
	if calls != before {
		t.Fatalf("a second read made %d compare calls, want none (every answer is stored)", calls-before)
	}
}

// A default-branch check keeps its meaning whatever its issue's pull request merged into: a
// commit the repository's default branch carries stands at it, and one only the integration
// branch holds stands at another commit. That is the whole of the difference between the two
// `when`s, and it is why a check that must speak for the trunk is written `default_branch`
// rather than `after_merge`.
func TestDefaultBranchResultStaysOnTheTrunkForAnIssueMergedElsewhere(t *testing.T) {
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true}, func(base, head string) string {
		switch {
		case base == "integration/x" && head == integrationCommit:
			return "behind"
		case base == "main" && head == mergeCommit:
			return "behind"
		}
		return "diverged"
	})
	issue := issueInRepository(t, handler, "RESJ", "acme/widgets", "Trunk check of an integration-branch issue")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", afterMergeAndDefaultChecks)
	writeHeadOnBase(t, database, "acme/widgets", 42, headCommit, "merged", issue.Key, "integration/x")

	onIntegration := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k5", "pass", integrationCommit, nil)))
	onTrunk := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k5", "pass", mergeCommit, nil)))

	read := readResults(t, handler, issue.Key)
	standsAt(t, read, "k5", onIntegration.Result.ID,
		"a default-branch result at a commit only the branch the issue merged into holds",
		model.CommitOtherCommit, false)
	standsAt(t, read, "k5", onTrunk.Result.ID,
		"a default-branch result at a commit the default branch holds", model.CommitAtBasis, false)
}

// An issue Dispatch holds no merge of is compared with the repository's default branch, as
// every after-merge result was before a branch was chosen at all. The row that chooses the
// branch must be merged and must be the check's issue's own: this issue's pull request is
// still open, and the merge into an integration branch belongs to a neighbour, so neither
// names a branch for this issue. Its result stands at the default branch for a commit that
// branch carries, and at another commit for the neighbour's integration-branch commit, which
// that branch does hold.
func TestAfterMergeResultFallsBackToTheTrunkWithoutAMergeOfTheIssue(t *testing.T) {
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true}, func(base, head string) string {
		switch {
		case base == "main" && head == mergeCommit:
			return "behind"
		case base == "integration/x" && head == integrationCommit:
			return "behind"
		}
		return "diverged"
	})
	neighbour := issueInRepository(t, handler, "RESP", "acme/widgets", "Merged into an integration branch")
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESP", "title": "Still open", "spec": "A spec\n"}, "alice"))
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	writeHeadOnBase(t, database, "acme/widgets", 43, newHeadCommit, "merged", neighbour.Key, "integration/x")
	writeHead(t, database, "acme/widgets", 44, headCommit, "open", issue.Key)

	onTrunk := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", mergeCommit, nil)))
	onNeighboursBranch := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", integrationCommit, nil)))

	read := readResults(t, handler, issue.Key)
	standsAt(t, read, "k2", onTrunk.Result.ID,
		"an after-merge result of an issue Dispatch holds no merge of, at a default-branch commit",
		model.CommitAtBasis, false)
	standsAt(t, read, "k2", onNeighboursBranch.Result.ID,
		"an after-merge result of an issue Dispatch holds no merge of, at a commit only another issue's integration branch holds",
		model.CommitOtherCommit, false)
}

// A merged row carrying no base branch at all — the shape a `pull_request` envelope published
// before Dispatch carried the field leaves behind — is no branch to compare with, and the
// repository's default branch answers instead. The default branch here is `trunk`, so a
// result compared with a hard-coded `main` would stand at another commit and this test would
// say so.
func TestAfterMergeResultFallsBackWhenTheMergedRowNamesNoBaseBranch(t *testing.T) {
	handler, database := resultsGitHubWithDefaultBranch(t, map[string]bool{"acme/widgets": true}, nil, "trunk",
		func(base, head string) string {
			if base == "trunk" && head == mergeCommit {
				return "behind"
			}
			return "diverged"
		})
	issue := issueInRepository(t, handler, "RESV", "acme/widgets", "Merged before the base branch was carried")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	// The empty base branch is named here, which is what the row is for: the column is
	// `not null default ''`, so an envelope published before Dispatch carried the field
	// leaves exactly this.
	writeHeadOnBase(t, database, "acme/widgets", 45, headCommit, "merged", issue.Key, "")

	onTrunk := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", mergeCommit, nil)))
	offTrunk := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", strayCommit, nil)))

	read := readResults(t, handler, issue.Key)
	if compared := checkOf(t, read, "k2").ComparedWith; len(compared) != 1 || compared[0].Branch != "trunk" {
		t.Fatalf("the after-merge check compared with %+v, want the resolved default branch trunk", compared)
	}
	standsAt(t, read, "k2", onTrunk.Result.ID,
		"an after-merge result of an issue whose merged row names no base branch, at a commit the default branch carries",
		model.CommitAtBasis, false)
	standsAt(t, read, "k2", offTrunk.Result.ID,
		"an after-merge result of an issue whose merged row names no base branch, at a commit no branch carries",
		model.CommitOtherCommit, false)
}

// Where the App cannot read the check's issue's repository, the check compares with nothing
// for that reason and its results are unknown. An issue that resolves to no repository at all
// is the other kind of nothing: there is no pull request and no branch, so its results stand
// at another commit and no pass of them stands.
func TestAfterMergeResultIsUnknownWhenTheAppCannotReadTheRepository(t *testing.T) {
	t.Parallel()
	handler, _ := resultsGitHub(t, map[string]bool{}, func(string, string) string { return "behind" })
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "RESU", "name": "Unknown"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/hidden",
		map[string]any{"project": "RESU", "default": true}, "alice")
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESU", "title": "Unreadable", "spec": "A spec\n"}, "alice"))
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", mergeCommit, nil)))
	read := readResults(t, handler, issue.Key)
	comparedNothing(t, read, "k2", model.ReasonRepositoryUnreadable, model.PassStandsUnknown)
	stands := standsAt(t, read, "k2", recorded.Result.ID,
		"a result of a repository the App cannot read", model.CommitUnknown, false)
	if stands.Reason != model.ReasonRepositoryUnreadable {
		t.Fatalf("that result is unknown for %q, want %q", stands.Reason, model.ReasonRepositoryUnreadable)
	}

	// An issue that resolves to no repository at all has nothing to compare with.
	// createInteractionIssue creates the project itself.
	loose := createInteractionIssue(t, handler, "RESN", "No repository", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+loose.Key, "verification.md", twoChecks)
	none := decodeBody[recordedResult](t, recordResult(t, handler, loose.Key, resultBody("k2", "pass", mergeCommit, nil)))
	looseRead := readResults(t, handler, loose.Key)
	comparedNothing(t, looseRead, "k2", model.ReasonNoPullRequestOrBranch, model.PassStandsNo)
	standsAt(t, looseRead, "k2", none.Result.ID,
		"a result of an issue with no repository", model.CommitOtherCommit, false)
}

// The base branch or the default branch, whichever carries the commit. An issue whose pull
// request merged into a branch other than the repository's default stands at that branch for
// a commit it carries — the work reached it, whether or not the branch has landed — and at
// the default branch for a commit that branch carries, since an integration branch that has
// landed has carried the work onto the trunk, where the base branch itself may be gone. A
// commit neither branch carries stands at another commit.
//
// The same read's default-branch check is compared with the trunk alone, at the very commit
// the after-merge check has just placed on the integration branch: an answer cached for one
// branch is never read for another, which the stored row and the memo of one read are both
// keyed by the branch to keep.
func TestAfterMergeResultReadsTheBaseBranchOrTheDefaultBranch(t *testing.T) {
	calls := 0
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true}, func(base, head string) string {
		calls++
		switch {
		case base == "integration/x" && head == integrationCommit:
			return "behind"
		case base == "main" && head == mergeCommit:
			return "behind"
		}
		return "diverged"
	})
	issue := issueInRepository(t, handler, "RESB", "acme/widgets", "Merged into an integration branch that landed")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", afterMergeAndDefaultChecks)
	writeHeadOnBase(t, database, "acme/widgets", 46, headCommit, "merged", issue.Key, "integration/x")

	onBase := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k4", "pass", integrationCommit, nil)))
	onTrunk := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k4", "pass", mergeCommit, nil)))
	onNeither := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k4", "pass", strayCommit, nil)))
	trunkCheck := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k5", "pass", integrationCommit, nil)))

	read := readResults(t, handler, issue.Key)
	onBaseStands := standsAt(t, read, "k4", onBase.Result.ID,
		"an after-merge result at a commit on the branch the issue merged into", model.CommitAtBasis, false)
	if onBaseStands.Basis == nil || onBaseStands.Basis.Branch != "integration/x" {
		t.Fatalf("that result stands at %+v, want the branch integration/x", onBaseStands.Basis)
	}
	onTrunkStands := standsAt(t, read, "k4", onTrunk.Result.ID,
		"an after-merge result at a commit on the repository's default branch", model.CommitAtBasis, false)
	if onTrunkStands.Basis == nil || onTrunkStands.Basis.Branch != "main" {
		t.Fatalf("that result stands at %+v, want the branch main", onTrunkStands.Basis)
	}
	standsAt(t, read, "k4", onNeither.Result.ID,
		"an after-merge result at a commit on neither branch", model.CommitOtherCommit, false)
	standsAt(t, read, "k5", trunkCheck.Result.ID,
		"a default-branch result at a commit only the integration branch holds", model.CommitOtherCommit, false)
	// Every question is stored under the branch it asked about — a yes for good, a no for five
	// minutes — so a second read inside them spends no compare call at all.
	before := calls
	readResults(t, handler, issue.Key)
	if calls != before {
		t.Fatalf("a second read made %d compare calls, want none (every answer is stored)", calls-before)
	}
}

// What the widened rule costs an issue merged the ordinary way, into the repository's own
// default branch: the one compare read per result commit it cost when an after-merge result
// was compared with one branch. The merge's own branch is the default branch under the name
// delivery resolved, so the check names that branch once and the walk asks about it once.
func TestAfterMergeResultOfAnIssueMergedIntoTheTrunkSpendsOneCompareReadACommit(t *testing.T) {
	calls := 0
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true}, func(base, head string) string {
		calls++
		if base == "main" && head == mergeCommit {
			return "behind"
		}
		return "diverged"
	})
	issue := issueInRepository(t, handler, "RESO", "acme/widgets", "Merged into the trunk")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	writeHeadOnBase(t, database, "acme/widgets", 47, headCommit, "merged", issue.Key, "main")

	landed := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", mergeCommit, nil)))
	stray := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", strayCommit, nil)))

	before := calls
	read := readResults(t, handler, issue.Key)
	standsAt(t, read, "k2", landed.Result.ID,
		"an after-merge result of an issue merged into the trunk, at a commit the trunk carries",
		model.CommitAtBasis, false)
	standsAt(t, read, "k2", stray.Result.ID,
		"an after-merge result of an issue merged into the trunk, at a commit no branch carries",
		model.CommitOtherCommit, false)
	if spent := calls - before; spent != 2 {
		t.Fatalf("one read of two result commits made %d compare calls, want one for each commit", spent)
	}
}

// The branch deleted on the merge, as a repository that deletes merged branches leaves every
// base branch once it lands. GitHub answers a compare read against it 404, which this
// codebase reads as a repository it cannot see and so as no answer about the commit at all.
// The rule takes that for "this commit is not on that branch" and lets the default branch
// decide.
//
// So a commit the default branch carries stands at it, and a commit neither branch carries
// stands at another commit rather than being unknown — the whole point of the rule, since a
// reviewer left with a pass nobody could place can only refuse the approval. The pull
// request's own merge commit stands at the base branch although nothing can be read about
// that branch any more and the default branch holds only a squash of the work: Dispatch
// recorded that commit as on its base branch when it recorded the merge, and a yes is kept
// for good (delivery.UpsertPullRequestHead).
func TestAfterMergeResultWithTheBaseBranchDeleted(t *testing.T) {
	handler, database := resultsGitHubWithDeletedBranch(t, map[string]bool{"acme/widgets": true}, nil,
		"main", "integration/x", func(base, head string) string {
			if base == "main" && head == mergeCommit {
				return "behind"
			}
			return "diverged"
		})
	issue := issueInRepository(t, handler, "RESD2", "acme/widgets", "Merged into a branch since deleted")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	recordMerge(t, database, "acme/widgets", 48, issue.Key, "integration/x")

	// mergeCommit is the squash the trunk carries, baseMergeCommit the merge GitHub made on
	// the branch it has since deleted, and strayCommit a commit neither ever carried.
	onTrunk := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", mergeCommit, nil)))
	merge := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", baseMergeCommit, nil)))
	onNeither := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", strayCommit, nil)))

	read := readResults(t, handler, issue.Key)
	standsAt(t, read, "k2", onTrunk.Result.ID,
		"an after-merge result at a default-branch commit, the base branch deleted",
		model.CommitAtBasis, false)
	merged := standsAt(t, read, "k2", merge.Result.ID,
		"an after-merge result at the pull request's own merge commit, the base branch deleted",
		model.CommitAtBasis, false)
	if merged.Basis == nil || merged.Basis.Branch != "integration/x" {
		t.Fatalf("the merge commit stands at %+v, want the base branch integration/x", merged.Basis)
	}
	standsAt(t, read, "k2", onNeither.Result.ID,
		"an after-merge result at a commit neither branch carries, the base branch deleted",
		model.CommitOtherCommit, false)
}

// The same squash-merged issue once the App has lost the repository. The merge commit stands
// at the base branch still: Dispatch recorded that yes when it recorded the merge, a yes is
// kept for good, and the stored row is read before any call to GitHub, so the branch's name
// is the whole of what the answer needs (delivery.Branches.CommitOnBranch). The branch the
// check compares with is named from the default branch this process resolved before the App
// lost the repository, which delivery keeps across a read that can say nothing exactly so
// that this answer survives (delivery/branch.go:261-273).
//
// This pins the reviewer's finding on pull request #3530 (dispatch://INFRA-1522):
// branchBases compared with nothing as soon as the repository read answered unreadable, so
// the branches were never asked and the stored yes was never reached. One failed App read
// turned every `after_merge` and every `default_branch` result of the repository from
// `at_basis` to `unknown` — the squash merge of a deleted base branch among them, which
// nothing else can ever place.
func TestAfterMergeResultStandsAtAStoredBranchAnswerWhileTheAppCannotReadTheRepository(t *testing.T) {
	// The test owns the map the fixture reads per request, so taking the repository out of it
	// is the App losing it: GitHub refuses the read of the repository and every compare read
	// of it from that moment on.
	repos := map[string]bool{"acme/widgets": true}
	handler, database, served := resultsGitHubServed(t, repos, nil, "main",
		func(base, head string) (int, string) {
			if !repos["acme/widgets"] {
				return http.StatusNotFound, ""
			}
			if base == "main" && head == mergeCommit {
				return http.StatusOK, "behind"
			}
			return http.StatusOK, "diverged"
		}, nil)
	issue := issueInRepository(t, handler, "RESD3", "acme/widgets", "Merged, then the App lost the repository")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	recordMerge(t, database, "acme/widgets", 51, issue.Key, "integration/x")

	merge := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", baseMergeCommit, nil)))
	stray := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", strayCommit, nil)))

	// The App loses the repository. delivery.Branches holds what it last read for
	// delivery.BranchCommitStaleAfter, so the read past that span is made here with the clock
	// moved on rather than by waiting five minutes: it learns nothing, keeps the name it had
	// and marks the repository unreadable, which is the state the read below runs in.
	ctx := context.Background()
	served.branches.DefaultBranchOf(ctx, served.deps.GitHub, "acme/widgets", time.Now())
	delete(repos, "acme/widgets")
	branch, readable := served.branches.DefaultBranchOf(ctx, served.deps.GitHub, "acme/widgets",
		time.Now().Add(delivery.BranchCommitStaleAfter))
	if branch != "main" || readable {
		t.Fatalf("the default branch after the App lost the repository = %q, readable=%v; want main and false",
			branch, readable)
	}

	read := readResults(t, handler, issue.Key)
	stands := standsAt(t, read, "k2", merge.Result.ID,
		"an after-merge result at the merge commit of a repository the App can no longer read",
		model.CommitAtBasis, false)
	if stands.Basis == nil || stands.Basis.Branch != "integration/x" {
		t.Fatalf("the merge commit stands at %+v, want the base branch integration/x", stands.Basis)
	}
	if compared := checkOf(t, read, "k2").ComparedWith; len(compared) != 2 ||
		compared[0].Branch != "integration/x" || compared[1].Branch != "main" {
		t.Fatalf("the after-merge check of a repository the App cannot read compared with %+v, want the base branch and the trunk", compared)
	}
	if stood := checkOf(t, read, "k2").PassStands; stood.Answer != model.PassStandsYes {
		t.Fatalf("the check answers pass_stands %+v, want yes: the pass at the merge commit stands", stood)
	}
	// A commit no branch holds a stored answer for is unknown all the same: nothing can be
	// asked while the App cannot read the repository, and that is no evidence about the work.
	// It reads repository_unreadable and not read_failed, which is the word for a read of
	// Dispatch's own that did not answer: the two part what a reader does next, give the App
	// the repository or ask again (model.go, the reasons).
	unplaced := standsAt(t, read, "k2", stray.Result.ID,
		"an after-merge result at a commit nothing is stored for, the App having lost the repository",
		model.CommitUnknown, false)
	if unplaced.Reason != model.ReasonRepositoryUnreadable {
		t.Fatalf("that result is unknown for %q, want %q", unplaced.Reason, model.ReasonRepositoryUnreadable)
	}
}

// The merged issue of a repository this process has never named a default branch for: the
// restart whose first `GET /repos/{owner}/{repo}` failed, which delivery.Branches holds for
// delivery.BranchCommitStaleAfter, and every repository of a deployment holding no App key.
// The branch the work merged into is read from the database (delivery.BaseBranchMergedFor)
// and costs no call to GitHub, so the check compares with that branch alone and the stored
// yes at it places the merge commit.
//
// This pins the second half of the reviewer's finding on pull request #3530
// (dispatch://INFRA-1522): branchBases answered nothing at all as soon as the default branch
// had no name, so a branch nothing about GitHub turns on was never asked, and the squash
// merge of a deleted base branch — which nothing else can ever place — read `unknown`.
//
// A commit no stored row answers for is unknown rather than another commit: the trunk, which
// would have decided, was never asked, and "not on the branch the work merged into" is no
// evidence about it.
func TestAfterMergeResultStandsAtItsMergedBranchWithNoDefaultBranchNamed(t *testing.T) {
	// The issue resolves to acme/hidden, which the App is not installed on, so no read of
	// this process ever names that repository's default branch: server.branches holds no name
	// for it, as it holds none for any repository after a restart whose first read failed.
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true},
		func(string, string) string { return "diverged" })
	issue := issueInRepository(t, handler, "RESD4", "acme/hidden", "Merged, the repository never read")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	recordMerge(t, database, "acme/hidden", 52, issue.Key, "integration/x")

	merge := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", baseMergeCommit, nil)))
	stray := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", strayCommit, nil)))

	read := readResults(t, handler, issue.Key)
	if compared := checkOf(t, read, "k2").ComparedWith; len(compared) != 1 || compared[0].Branch != "integration/x" {
		t.Fatalf("the after-merge check of a repository with no default branch named compared with %+v, want the merged branch alone", compared)
	}
	stands := standsAt(t, read, "k2", merge.Result.ID,
		"an after-merge result at the merge commit, no default branch named for its repository",
		model.CommitAtBasis, false)
	if stands.Basis == nil || stands.Basis.Branch != "integration/x" {
		t.Fatalf("the merge commit stands at %+v, want the base branch integration/x", stands.Basis)
	}
	if stood := checkOf(t, read, "k2").PassStands; stood.Answer != model.PassStandsYes {
		t.Fatalf("the check answers pass_stands %+v, want yes: the pass at the merge commit stands", stood)
	}
	unplaced := standsAt(t, read, "k2", stray.Result.ID,
		"an after-merge result at a commit nothing is stored for, no default branch named",
		model.CommitUnknown, false)
	if unplaced.Reason != model.ReasonRepositoryUnreadable {
		t.Fatalf("that result is unknown for %q, want %q", unplaced.Reason, model.ReasonRepositoryUnreadable)
	}
}

// The same merged issue in a deployment holding no App key, where deps.GitHub is nil, every
// read of GitHub answers githubapp.ErrNoAppKey and no repository has a default branch named.
// The branch the issue merged into is a fact of Dispatch's own database and the yes at it a
// row kept for good, so the merge commit stands at that branch with no GitHub at all — which
// is the whole of what such a deployment can answer, and more than the nothing it answered
// before (dispatch://INFRA-1522).
//
// A commit the stored row says that branch does not carry is unknown and never another
// commit: the walk is short of the default branch, which nothing could name, so the branch
// that would have decided — the trunk an integration branch lands on — was never asked.
func TestAfterMergeResultStandsAtItsMergedBranchWithNoAppKey(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	if deps.GitHub != nil {
		t.Fatalf("the fixture holds a GitHub client, want the deployment that holds no App key")
	}
	issue := issueInRepository(t, handler, "RESD5", "acme/widgets", "Merged, no App key")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	recordMerge(t, database, "acme/widgets", 53, issue.Key, "integration/x")
	// The answer a compare read of the merged branch left behind before the App key went
	// away: that branch does not carry this commit. It is read before any call to GitHub, so
	// the walk ends on it rather than on a read nobody can make (delivery.CommitOnBranch).
	writeBranchAnswer(t, database, "acme/widgets", "integration/x", strayCommit, false)

	merge := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", baseMergeCommit, nil)))
	stray := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", strayCommit, nil)))
	read := readResults(t, handler, issue.Key)
	if compared := checkOf(t, read, "k2").ComparedWith; len(compared) != 1 || compared[0].Branch != "integration/x" {
		t.Fatalf("the after-merge check of a deployment with no App key compared with %+v, want the merged branch alone", compared)
	}
	stands := standsAt(t, read, "k2", merge.Result.ID,
		"an after-merge result at the merge commit, the deployment holding no App key",
		model.CommitAtBasis, false)
	if stands.Basis == nil || stands.Basis.Branch != "integration/x" {
		t.Fatalf("the merge commit stands at %+v, want the base branch integration/x", stands.Basis)
	}
	if stood := checkOf(t, read, "k2").PassStands; stood.Answer != model.PassStandsYes {
		t.Fatalf("the check answers pass_stands %+v, want yes: the pass at the merge commit stands", stood)
	}
	unplaced := standsAt(t, read, "k2", stray.Result.ID,
		"an after-merge result the merged branch is stored as not carrying, with no default branch to ask",
		model.CommitUnknown, false)
	if unplaced.Reason != model.ReasonRepositoryUnreadable {
		t.Fatalf("that result is unknown for %q, want %q", unplaced.Reason, model.ReasonRepositoryUnreadable)
	}
}

// A compare read GitHub failed to answer about the branch an issue merged into leaves the
// result unknown, never at another commit. The branch is still there and GitHub is the one at
// fault, so "the default branch does not carry it" is the whole of what the read learned, and
// a reviewer shown a result placed elsewhere over a 500 can only refuse an approval that
// nothing is wrong with. A base branch GitHub no longer holds is the other case, and places
// the commit elsewhere where the default branch says no: that is
// TestAfterMergeResultWithTheBaseBranchDeleted above, and telling the two apart is the whole
// of this rule (delivery.BranchAnswer).
//
// The next read asks GitHub again rather than being answered from a hold: a failure of a
// second's standing must not hold the branch every result of the issue is compared with for
// five minutes. The default branch's own "no" is stored for that span, as every no is, so the
// second read spends the base branch's call and no other.
func TestAfterMergeResultIsUnknownWhenTheBaseBranchReadFails(t *testing.T) {
	compares := 0
	handler, database := resultsGitHubAnswering(t, map[string]bool{"acme/widgets": true}, nil, "main",
		func(base, _ string) (int, string) {
			compares++
			if base == "integration/x" {
				return http.StatusInternalServerError, ""
			}
			return http.StatusOK, "diverged"
		})
	issue := issueInRepository(t, handler, "RESG", "acme/widgets", "A base branch GitHub failed on")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	writeHeadOnBase(t, database, "acme/widgets", 49, headCommit, "merged", issue.Key, "integration/x")

	stray := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", strayCommit, nil)))

	before := compares
	read := readResults(t, handler, issue.Key)
	stands := standsAt(t, read, "k2", stray.Result.ID,
		"an after-merge result whose base branch GitHub failed on", model.CommitUnknown, false)
	if stands.Reason != model.ReasonReadFailed {
		t.Fatalf("that result is unknown for %q, want %q", stands.Reason, model.ReasonReadFailed)
	}
	// A pass Dispatch cannot place leaves the gate's question unanswered, never answered no.
	if stood := checkOf(t, read, "k2").PassStands; stood.Answer != model.PassStandsUnknown {
		t.Fatalf("the check answers pass_stands %+v, want unknown", stood)
	}
	if spent := compares - before; spent != 2 {
		t.Fatalf("the first read made %d compare calls, want one for the base branch and one for the trunk", spent)
	}

	before = compares
	read = readResults(t, handler, issue.Key)
	standsAt(t, read, "k2", stray.Result.ID, "the same result read again", model.CommitUnknown, false)
	if spent := compares - before; spent != 1 {
		t.Fatalf("the second read made %d compare calls, want the base branch's own: a failure holds nothing, and the trunk's no is stored", spent)
	}
}

// A `before_merge` check of a repository Dispatch cannot see compares with nothing for that
// reason, and its results are unknown: nothing can teach Dispatch that repository's pull
// request heads, so "this commit heads none I know" says nothing about the work, and the
// reviewer reads a pass nobody could place rather than a forced refusal. Where Dispatch can
// read the repository the same commit stands at another commit, which is what makes the
// answer worth reading.
func TestBeforeMergeResultIsUnknownWhenDispatchCannotSeeTheRepository(t *testing.T) {
	t.Parallel()
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true},
		func(string, string) string { return "diverged" })

	hidden := issueInRepository(t, handler, "RESB", "acme/hidden", "Unreadable")
	uploadDocument(t, handler, "/api/v1/issues/"+hidden.Key, "verification.md", twoChecks)
	unreadable := decodeBody[recordedResult](t, recordResult(t, handler, hidden.Key, resultBody("k1", "pass", headCommit, nil)))
	hiddenRead := readResults(t, handler, hidden.Key)
	comparedNothing(t, hiddenRead, "k1", model.ReasonRepositoryUnreadable, model.PassStandsUnknown)
	standsAt(t, hiddenRead, "k1", unreadable.Result.ID,
		"a pre-merge result of a repository the App cannot read", model.CommitUnknown, false)

	// An issue that resolves to no repository at all has nothing to compare with, and nothing
	// left to read: createInteractionIssue creates the project itself, and nothing maps a
	// repository to it.
	loose := createInteractionIssue(t, handler, "RESL", "No repository", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+loose.Key, "verification.md", twoChecks)
	none := decodeBody[recordedResult](t, recordResult(t, handler, loose.Key, resultBody("k1", "pass", headCommit, nil)))
	looseRead := readResults(t, handler, loose.Key)
	comparedNothing(t, looseRead, "k1", model.ReasonNoPullRequestOrBranch, model.PassStandsNo)
	standsAt(t, looseRead, "k1", none.Result.ID,
		"a pre-merge result of an issue with no repository", model.CommitOtherCommit, false)

	// The contrast, in a repository the App reads: with no head stored the check compares with
	// nothing although the repository reads, and a head makes that commit a basis.
	seen := issueInRepository(t, handler, "RESV", "acme/widgets", "Readable")
	uploadDocument(t, handler, "/api/v1/issues/"+seen.Key, "verification.md", twoChecks)
	adrift := decodeBody[recordedResult](t, recordResult(t, handler, seen.Key, resultBody("k1", "pass", headCommit, nil)))
	adriftRead := readResults(t, handler, seen.Key)
	comparedNothing(t, adriftRead, "k1", model.ReasonNoPullRequestOrBranch, model.PassStandsNo)
	standsAt(t, adriftRead, "k1", adrift.Result.ID,
		"a pre-merge result heading no pull request of a readable repository", model.CommitOtherCommit, false)
	writeHead(t, database, "acme/widgets", 31, newHeadCommit, "open", seen.Key)
	atHead := decodeBody[recordedResult](t, recordResult(t, handler, seen.Key, resultBody("k1", "pass", newHeadCommit, nil)))
	standsAt(t, readResults(t, handler, seen.Key), "k1", atHead.Result.ID,
		"a pre-merge result at the open head", model.CommitAtBasis, false)
}

// The same unknown, with a second repository in play: Dispatch reads one of the two, and the
// commit a result of the unreadable repository's issue names is the head of an open pull
// request of the readable one, attributed to the readable repository's own issue. That head
// says nothing about the unreadable repository — Dispatch sees none of its pull requests, so
// it cannot know what that commit is there — so that result stays unknown and is never placed
// at another commit, while the readable issue's own result at the same commit stands at its
// head.
func TestBeforeMergeResultStaysUnknownAtAnotherRepositorysHead(t *testing.T) {
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true},
		func(string, string) string { return "diverged" })
	seen := issueInRepository(t, handler, "RESY", "acme/widgets", "Readable")
	hidden := issueInRepository(t, handler, "RESZ", "acme/hidden", "Unreadable")
	uploadDocument(t, handler, "/api/v1/issues/"+seen.Key, "verification.md", twoChecks)
	uploadDocument(t, handler, "/api/v1/issues/"+hidden.Key, "verification.md", twoChecks)

	// The one pull request Dispatch knows at that commit: open, in the repository it reads, and
	// attributed to that repository's own issue.
	writeHead(t, database, "acme/widgets", 41, headCommit, "open", seen.Key)

	elsewhere := decodeBody[recordedResult](t, recordResult(t, handler, hidden.Key, resultBody("k1", "pass", headCommit, nil)))
	standsAt(t, readResults(t, handler, hidden.Key), "k1", elsewhere.Result.ID,
		"a pre-merge result of acme/hidden's issue at acme/widgets' head, which Dispatch cannot see at all",
		model.CommitUnknown, false)
	atHead := decodeBody[recordedResult](t, recordResult(t, handler, seen.Key, resultBody("k1", "pass", headCommit, nil)))
	standsAt(t, readResults(t, handler, seen.Key), "k1", atHead.Result.ID,
		"acme/widgets' own result at that head", model.CommitAtBasis, false)
}

// Recording a result at a commit that is the head of no pull request Dispatch has an event
// for reads GitHub through the App and stores every pull request it names, so the result
// stands at that head without a webhook ever arriving.
func TestRecordingAResultReadsThroughToAPullRequestDispatchHasNoEventFor(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_fake_77","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	var issueKey string
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[{"number":12,"title":%q,"body":"","html_url":"https://github.com/acme/widgets/pull/12",
			"state":"open","merged":false,"updated_at":"2026-01-01T00:00:00Z",
			"head":{"ref":"legion/x","sha":%q,"repo":{"full_name":"acme/widgets"}},"base":{"ref":"main"}}]`, issueKey+" work", headCommit)
	})
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	handler, database, _ := newTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream.URL,
	})
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "REST", "name": "Through"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets",
		map[string]any{"project": "REST", "default": true}, "alice")
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "REST", "title": "Read through", "spec": "A spec\n"}, "alice"))
	issueKey = issue.Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	var storedIssue *string
	var state string
	if err := database.Pool.QueryRow(context.Background(), `
		select issue_key, state from pull_request_heads where repo = 'acme/widgets' and number = 12
	`).Scan(&storedIssue, &state); err != nil {
		t.Fatalf("read the pull request the read-through stored: %v", err)
	}
	if storedIssue == nil || *storedIssue != issue.Key || state != "open" {
		t.Fatalf("the read-through stored issue=%v state=%q, want %s open", storedIssue, state, issue.Key)
	}
	standsAt(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID,
		"a result at a head the read-through learned", model.CommitAtBasis, false)
}

// The same case at the surface a reviewer reads: a pull request of acme/gadgets whose title
// names acme/widgets' issue before its own. The attribution walks every candidate the five
// sources name (delivery's attributionCandidates) and takes the first one held to the pull
// request's own repository (attributionHeld), so the head the read-through learns is
// attributed to the check's own issue and the result stands at it, although the title names
// another repository's issue first. Judged through the route, which is where a reviewer's
// approval condition is actually read.
func TestAResultAtTheHeadOfAPullRequestTitledAfterAnotherRepositorysIssueStandsAtIt(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_fake_77","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	// The repository is readable, so a result at no attributed head is `other_commit` rather
	// than `unknown`.
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"full_name":%q,"default_branch":"main"}`, r.PathValue("owner")+"/"+r.PathValue("repo"))
	})
	var title string
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("owner") + "/" + r.PathValue("repo")
		fmt.Fprintf(w, `[{"number":33,"title":%q,"body":"","html_url":"https://github.com/%s/pull/33",
			"state":"open","merged":false,"updated_at":"2026-01-02T00:00:00Z",
			"head":{"ref":"legion/x","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}]`,
			title, repo, headCommit, repo)
	})
	// The title names a candidate held to acme/gadgets, so the read-through keeps it without
	// paging the commit messages, the fifth source; the handler answers an empty page for a read
	// that asks anyway.
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	handler, _, _ := newTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream.URL,
	})

	elsewhere := issueInRepository(t, handler, "RESW", "acme/widgets", "Another repository's issue")
	own := issueInRepository(t, handler, "RESG", "acme/gadgets", "This repository's issue")
	title = elsewhere.Key + ": the other repository's issue, and " + own.Key + " (this one)"
	uploadDocument(t, handler, "/api/v1/issues/"+own.Key, "verification.md", twoChecks)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, own.Key, resultBody("k1", "pass", headCommit, nil)))
	standsAt(t, readResults(t, handler, own.Key), "k1", recorded.Result.ID,
		"a result at the head of "+own.Key+"'s own pull request, whose title names "+elsewhere.Key+" first",
		model.CommitAtBasis, false)
}

// The same surface for a repository under two names. An issue is recorded under the name its
// repository was renamed away from, and its pull request is on the name GitHub knows.
// Dispatch holds both — repo_projects maps each, and the old name's row names what it was
// renamed to — so the results route asks the App about the canonical name
// (refs.CanonicalRepo) and the attribution holds the issue to it. Before that pair was read,
// the App was asked about the old name, whose installation GitHub answers 404 for: the
// read-through learned no pull request and Dispatch could see no repository at all, so the
// result was unknown rather than placed at a head.
func TestAResultOfAnIssueRecordedUnderARepositorysOldNameStandsAtItsHead(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	mux := http.NewServeMux()
	// GitHub knows acme/gadgets and nothing of the name it was renamed away from, which is why
	// Dispatch cannot resolve a rename by asking and is told it instead.
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("owner")+"/"+r.PathValue("repo") != "acme/gadgets" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_fake_77","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"full_name":%q,"default_branch":"main"}`, r.PathValue("owner")+"/"+r.PathValue("repo"))
	})
	var title string
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[{"number":44,"title":%q,"body":"","html_url":"https://github.com/acme/gadgets/pull/44",
			"state":"open","merged":false,"updated_at":"2026-03-01T00:00:00Z",
			"head":{"ref":"legion/x","sha":%q,"repo":{"full_name":"acme/gadgets"}},"base":{"ref":"main"}}]`, title, headCommit)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	handler, _, _ := newTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream.URL,
	})

	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects",
		map[string]string{"key": "RENM", "name": "Renamed"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create RENM: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, mapping := range []struct {
		repo string
		body map[string]any
	}{
		{"acme/gadgets", map[string]any{"project": "RENM", "default": true}},
		{"oldco/gadgets", map[string]any{"project": "RENM", "renamed_to": "acme/gadgets"}},
	} {
		if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+mapping.repo,
			mapping.body, "alice"); response.Code != http.StatusOK {
			t.Fatalf("map %s: status=%d body=%s", mapping.repo, response.Code, response.Body.String())
		}
	}
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RENM", "title": "Recorded under the old name", "spec": "A spec\n", "repo": "oldco/gadgets"}, "alice"))
	title = issue.Key + ": the work, on the new name"
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	standsAt(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID,
		"a result of "+issue.Key+", recorded under oldco/gadgets, at the head of its pull request on acme/gadgets",
		model.CommitAtBasis, false)
}

// The answer to a record carries the moment the row was written, not the zero time: created_at
// is a declared field of the contract, and a reader takes it for when the run happened.
func TestRecordedResultAnswersTheTimeItWasStored(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESA", "Stored at", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	if recorded.Result.CreatedAt.IsZero() {
		t.Fatalf("the record's answer carries created_at %s, want the moment the row was written", recorded.Result.CreatedAt)
	}
	var stored time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select created_at from check_results where id = $1::uuid`, recorded.Result.ID).Scan(&stored); err != nil {
		t.Fatalf("read the stored result: %v", err)
	}
	if !recorded.Result.CreatedAt.Equal(stored) {
		t.Fatalf("the record's answer carries created_at %s, want the row's %s", recorded.Result.CreatedAt, stored)
	}
	if read := checkOf(t, readResults(t, handler, issue.Key), "k1").Results; len(read) != 1 || !read[0].CreatedAt.Equal(stored) {
		t.Fatalf("the results read carries %+v, want the one result at %s", read, stored)
	}
}

// R4: a child's pull request is never one of the heads a root check is compared with. A child
// proving its parent's check records the result on the child, and the head of the child's own
// pull request answers for nothing on the parent's tab. What stands there is the parent's own
// result at a head of the parent's own pull request.
func TestChildsResultAgainstAnAncestorsCheckStandsAtNoBasisAtTheChildsHead(t *testing.T) {
	t.Parallel()
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true},
		func(string, string) string { return "diverged" })
	parent := issueInRepository(t, handler, "RESX", "acme/widgets", "Parent")
	uploadDocument(t, handler, "/api/v1/issues/"+parent.Key, "verification.md", siblingChecks)
	child := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESX", "title": "Child", "parent": parent.Key, "force": true}, "alice"))
	// Two open pull requests in the one repository: the child's, and the parent's own.
	writeHead(t, database, "acme/widgets", 21, headCommit, "open", child.Key)
	writeHead(t, database, "acme/widgets", 22, newHeadCommit, "open", parent.Key)

	fromChild := decodeBody[recordedResult](t, recordResult(t, handler, child.Key, resultBody("k1", "pass", headCommit, nil)))
	fromParent := decodeBody[recordedResult](t, recordResult(t, handler, parent.Key, resultBody("k1", "pass", newHeadCommit, nil)))

	read := readResults(t, handler, parent.Key)
	// The parent's check compares with the parent's own head and with no head of the child's.
	if compared := checkOf(t, read, "k1").ComparedWith; len(compared) != 1 || compared[0].Commit != newHeadCommit {
		t.Fatalf("the parent's check compared with %+v, want the parent's own head %s alone", compared, newHeadCommit)
	}
	standsAt(t, read, "k1", fromChild.Result.ID,
		"the child's result at the child's own head", model.CommitOtherCommit, false)
	standsAt(t, read, "k1", fromParent.Result.ID,
		"the parent's own result at the parent's own head", model.CommitAtBasis, false)
}

// The same rule where the child has a repository of its own. The read-through asks GitHub
// about the repository the check's issue resolves to and no other, so the child's own pull
// request is never learned, and the child's result stands at no head on the parent's tab
// although GitHub would name a pull request for that commit in the child's repository.
func TestChildsResultInItsOwnRepositoryStandsAtNoBasisOnTheParentsTab(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_fake_77","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	// Both repositories read, so a commit heading no pull request Dispatch knows is
	// `other_commit` there rather than `unknown`.
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"full_name":%q,"default_branch":"main"}`, r.PathValue("owner")+"/"+r.PathValue("repo"))
	})
	var childKey string
	var askedParent, askedChild bool
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("repo") != "gadgets" {
			askedParent = true
			fmt.Fprint(w, `[]`)
			return
		}
		askedChild = true
		fmt.Fprintf(w, `[{"number":31,"title":%q,"body":"","html_url":"https://github.com/acme/gadgets/pull/31",
			"state":"open","merged":false,"updated_at":"2026-01-01T00:00:00Z",
			"head":{"ref":"legion/child","sha":%q},"base":{"ref":"main"}}]`, childKey+" work", headCommit)
	})
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	handler, database, _ := newTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream.URL,
	})
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "RESR", "name": "Two repos"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets",
		map[string]any{"project": "RESR", "default": true}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/gadgets",
		map[string]any{"project": "RESR"}, "alice")
	parent := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESR", "title": "Parent", "spec": "A spec\n"}, "alice"))
	uploadDocument(t, handler, "/api/v1/issues/"+parent.Key, "verification.md", siblingChecks)
	child := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESR", "title": "Child", "parent": parent.Key, "force": true}, "alice"))
	childKey = child.Key
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key,
		map[string]any{"repo": "acme/gadgets"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("give the child its own repository: status=%d body=%s", response.Code, response.Body.String())
	}

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, child.Key, resultBody("k1", "pass", headCommit, nil)))

	if !askedParent {
		t.Fatalf("the read-through never asked the repository the check's issue resolves to")
	}
	if askedChild {
		t.Fatalf("the read-through asked the recording issue's own repository, which R4 rules out")
	}
	var stored int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from pull_request_heads where repo = 'acme/gadgets'
	`).Scan(&stored); err != nil {
		t.Fatalf("count the pull requests stored for the child's repository: %v", err)
	}
	if stored != 0 {
		t.Fatalf("the read-through stored %d pull requests of the child's repository, want none", stored)
	}
	standsAt(t, readResults(t, handler, parent.Key), "k1", recorded.Result.ID,
		"the child's result at its own repository's head", model.CommitOtherCommit, false)
}

// commitPullsFake is the GitHub a read-time read-through reads (readThroughGitHub): it answers
// GET /repos/{owner}/{repo}/commits/{sha}/pulls from the body its test set last, so a test can
// open a pull request between two reads, and counts every such ask per repository, so a test can
// show what one read of an issue's results spent.
type commitPullsFake struct {
	mu     sync.Mutex
	asked  map[string]int
	answer func(repo, sha string) string
}

// answers sets what GET /repos/{owner}/{repo}/commits/{sha}/pulls answers from now on: the JSON
// array of pull requests GitHub names for that commit. Until a test sets one, GitHub names none.
func (f *commitPullsFake) answers(body func(repo, sha string) string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = body
}

// asks is how often GitHub has been asked for repo's pull requests, at any commit.
func (f *commitPullsFake) asks(repo string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked[repo]
}

func (f *commitPullsFake) serveCommitPulls(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	f.mu.Lock()
	f.asked[repo]++
	answer := f.answer
	f.mu.Unlock()
	if answer == nil {
		fmt.Fprint(w, `[]`)
		return
	}
	fmt.Fprint(w, answer(repo, r.PathValue("sha")))
}

// openPullAt is GitHub's answer for a commit that is the head of one open pull request titled
// after issueKey, in whichever repository was asked about.
func openPullAt(number int, issueKey string) func(repo, sha string) string {
	return func(repo, sha string) string {
		return fmt.Sprintf(`[{"number":%d,"title":%q,"body":"","html_url":"https://github.com/%s/pull/%d",
			"state":"open","merged":false,"updated_at":"2026-11-05T07:15:08Z",
			"head":{"ref":"legion/x","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}]`,
			number, issueKey+" the work", repo, number, sha, repo)
	}
}

// readThroughGitHub is resultsGitHub's twin for the read-time read-through: a Dispatch holding
// an App key, the server its handler mounts — whose commitPulls cache a test moves — and the
// GitHub it reads. The App is installed on repos and on nothing else, every repository it can
// read answers `main` as its default branch (so a commit heading no pull request is
// `other_commit` rather than `unknown`), each pull request's commit messages are an empty
// page, and one commit's pull requests are the fake's, which counts the asks.
func readThroughGitHub(t *testing.T, repos ...string) (http.Handler, *store.Store, *server, *commitPullsFake) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	installed := map[string]bool{}
	for _, repo := range repos {
		installed[repo] = true
	}
	fake := &commitPullsFake{asked: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if !installed[r.PathValue("owner")+"/"+r.PathValue("repo")] {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_fake_77","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"full_name":%q,"default_branch":"main"}`, r.PathValue("owner")+"/"+r.PathValue("repo"))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", fake.serveCommitPulls)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	upstream := githubtest.Server(t, githubtest.Spec{Handle: mux.ServeHTTP})
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream,
	})
	return handler, database, served, fake
}

// INFRA-1240 at the surface a reviewer reads. A `before_merge` result is recorded at a commit
// no pull request carries yet: GitHub names none, so the record's own read-through stores no
// head row, the check compares with nothing and the result stands at another commit. The pull
// request is opened at that head a moment later and nothing tells Dispatch — a deployment
// holding no delivery settings consumes no `pull_request` event at all, the reconcile's head
// pass only re-reads rows that already exist, and the record's read-through has run once and
// will not run again — so the read of the results asks GitHub itself, and the result stands
// at that head.
func TestAResultStandsAtItsCommitOnceAPullRequestOpensThere(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESO", "acme/widgets", "Opened after the record")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	var heads int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from pull_request_heads where repo = 'acme/widgets'`).Scan(&heads); err != nil {
		t.Fatalf("count the stored pull request heads: %v", err)
	}
	if heads != 0 {
		t.Fatalf("recording at a commit GitHub names no pull request for stored %d head rows, want none", heads)
	}
	first := readResults(t, handler, issue.Key)
	comparedNothing(t, first, "k1", model.ReasonNoPullRequestOrBranch, model.PassStandsNo)
	standsAt(t, first, "k1", recorded.Result.ID,
		"a result at a commit no pull request carries", model.CommitOtherCommit, false)

	// The pull request opens at that head, and no event reaches Dispatch.
	fake.answers(openPullAt(91, issue.Key))
	// The read-through the first read spent holds the next one off for
	// delivery.CommitPullReadStaleAfter, so the cache is rewound to its zero value, an empty one:
	// what the clock does on its own between a reviewer's refetches, without the test waiting for
	// it.
	served.commitPulls = delivery.CommitPullReads{}

	read := readResults(t, handler, issue.Key)
	stands := standsAt(t, read, "k1", recorded.Result.ID,
		"a result at the head of a pull request opened after it was recorded, which the read of the results learned from GitHub",
		model.CommitAtBasis, false)
	// The head the read learned is named on the check, with the pull request's number and the
	// time Dispatch learned it.
	if stands.Basis == nil || stands.Basis.PullRequest == nil || *stands.Basis.PullRequest != 91 ||
		stands.Basis.LearnedAt == nil {
		t.Fatalf("the result stands at %+v, want pull request 91 with the time Dispatch learned its head", stands.Basis)
	}
	if stood := checkOf(t, read, "k1").PassStands; stood.Answer != model.PassStandsYes {
		t.Fatalf("the check answers pass_stands %+v, want yes", stood)
	}
}

// The bound on that read. Two reads in a row, while GitHub still names no pull request for the
// commit, spend one read-through between them: the Verification tab refetches an issue's results
// on every result.recorded, and a tab left open would otherwise spend a GitHub call per result
// per refetch for ever, since nothing about such a commit is ever stored
// (delivery.CommitPullReadStaleAfter).
func TestTwoResultReadsSpendOneReadThroughOfACommit(t *testing.T) {
	handler, _, _, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESC", "acme/widgets", "Bounded")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	// The record's own read-through, which the read-time bound neither counts nor is bound by.
	spent := fake.asks("acme/widgets")

	for _, read := range []string{"first", "second"} {
		standsAt(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID,
			"the "+read+" read of a result at a commit no pull request carries", model.CommitOtherCommit, false)
	}
	if asked := fake.asks("acme/widgets") - spent; asked != 1 {
		t.Fatalf("two reads in a row asked GitHub for the commit's pull requests %d times, want 1", asked)
	}
}

// What the read never spends a GitHub call on. A repository Dispatch cannot see is the
// unknown answer whatever GitHub would say about the commit, so it is not read through at
// all, and a commit that already heads a pull request attributed to the check's own issue is
// answered from that row, so neither the record nor the read asks GitHub anything.
func TestAResultsReadReadsThroughNeitherAnUnseenRepositoryNorAKnownHead(t *testing.T) {
	handler, database, _, fake := readThroughGitHub(t, "acme/widgets")
	hidden := issueInRepository(t, handler, "RESJ", "acme/hidden", "Unreadable")
	seen := issueInRepository(t, handler, "RESK", "acme/widgets", "Readable")
	uploadDocument(t, handler, "/api/v1/issues/"+hidden.Key, "verification.md", twoChecks)
	uploadDocument(t, handler, "/api/v1/issues/"+seen.Key, "verification.md", twoChecks)
	// GitHub would name an open pull request of the issue for the commit in either repository,
	// so a read that asked would place the result at that head and the counts below are what
	// decides the test.
	fake.answers(openPullAt(92, hidden.Key))

	unreadable := decodeBody[recordedResult](t, recordResult(t, handler, hidden.Key, resultBody("k1", "pass", headCommit, nil)))
	standsAt(t, readResults(t, handler, hidden.Key), "k1", unreadable.Result.ID,
		"a result of a repository Dispatch cannot see", model.CommitUnknown, false)
	if asked := fake.asks("acme/hidden"); asked != 0 {
		t.Fatalf("acme/hidden's pull requests were asked for %d times, want none: Dispatch sees none of that repository", asked)
	}

	writeHead(t, database, "acme/widgets", 71, newHeadCommit, "open", seen.Key)
	known := decodeBody[recordedResult](t, recordResult(t, handler, seen.Key, resultBody("k1", "pass", newHeadCommit, nil)))
	standsAt(t, readResults(t, handler, seen.Key), "k1", known.Result.ID,
		"a result at a head Dispatch already holds", model.CommitAtBasis, false)
	if asked := fake.asks("acme/widgets"); asked != 0 {
		t.Fatalf("acme/widgets' pull requests were asked for %d times, want none: the stored head row answers", asked)
	}
}

// What a superseded result costs: one GitHub call, once, and not again while the answer stands.
// A check with two results — the newer at the head of a pull request Dispatch holds, the earlier
// at a commit it holds none for — is the state a push past a recorded head leaves behind. The
// superseded commit heads no row Dispatch keeps, so the first read asks GitHub about it, and
// GitHub answers by naming the pull request at the head it has now: an answer about that commit
// that stands until something opens a pull request at it. So the pair is held off for
// delivery.CommitPullSettledFor and no read inside that span asks again
// (delivery.CommitPullReads.Settle, api/results_comparison.go's readThroughHead); without that, a
// tab left open would spend the call again every delivery.CommitPullReadStaleAfter for as long
// as the result is read.
func TestASupersededResultsCommitIsAskedOfGitHubOnceAndNotAgainWithinTheSpan(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESP", "acme/widgets", "Pushed past")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	// The earlier result, recorded while GitHub named no pull request for its commit, so nothing
	// is stored for that commit.
	superseded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	// The push: the pull request's head moves, Dispatch holds that row, and the next result of the
	// same check is recorded there.
	writeHead(t, database, "acme/widgets", 97, newHeadCommit, "open", issue.Key)
	newest := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", newHeadCommit, nil)))

	// GitHub names that pull request for the superseded commit too, carrying the head it has now,
	// which is what it answers for a commit a later push replaced. The cache, rewound to its zero
	// value, holds no read off, so what keeps the second read from asking is the settling and not
	// the span.
	fake.answers(func(repo, _ string) string {
		return fmt.Sprintf(`[{"number":97,"title":%q,"body":"","html_url":"https://github.com/%s/pull/97",
			"state":"open","merged":false,"updated_at":"2026-11-05T07:15:08Z",
			"head":{"ref":"legion/x","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}]`,
			issue.Key+" the work", repo, newHeadCommit, repo)
	})
	served.commitPulls = delivery.CommitPullReads{}

	for _, read := range []struct {
		name  string
		asked int
	}{{"first", 1}, {"second", 0}} {
		spent := fake.asks("acme/widgets")
		answered := readResults(t, handler, issue.Key)
		standsAt(t, answered, "k1", superseded.Result.ID,
			"the "+read.name+" read of the result a later push left behind", model.CommitOtherCommit, false)
		standsAt(t, answered, "k1", newest.Result.ID,
			"the "+read.name+" read of the newest result, at the head Dispatch holds",
			model.CommitAtBasis, false)
		if asked := fake.asks("acme/widgets") - spent; asked != read.asked {
			t.Fatalf("the %s read asked GitHub for the superseded commit's pull requests %d times, want %d",
				read.name, asked, read.asked)
		}
	}
	// Settled, not merely bounded: the pair is refused a claim far past
	// delivery.CommitPullReadStaleAfter, which is what keeps the call off the refetches of the
	// hour that follows. The settling's own moment is the read's, a second or so before this
	// one, so a claim a minute short of the span is inside it.
	if served.commitPulls.Claim("acme/widgets", headCommit, time.Now().Add(delivery.CommitPullSettledFor-time.Minute)) {
		t.Fatal("a claim of the superseded commit inside the settled span was granted, want it refused")
	}
}

// The settling is believed for a span and no longer. GitHub has answered about the commit —
// naming a pull request whose head is another commit — and a pull request opened later can
// still have its head at that commit, which no event tells Dispatch about. So the answer is
// held for delivery.CommitPullSettledFor and then asked again, and a settling kept for the
// life of the process would leave such a result standing at no basis for ever
// (dispatch://INFRA-1240, the round-3 review of pull request #2798).
//
// Four steps: the first read settles the pair, the second pull request opens at the commit, a
// read inside the span places the commit elsewhere and asks GitHub nothing, which is what
// bounds the cost, and a read past the span asks once and stands the result at its head.
func TestAPullRequestOpenedAtASettledCommitIsSeenOnceTheSpanEnds(t *testing.T) {
	handler, _, served, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESS", "acme/widgets", "Opened at a settled commit")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	// The result is recorded while GitHub names no pull request for its commit at all, so its
	// own read-through stores nothing and the commit heads no row Dispatch keeps.
	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	// The pull request the commit is behind, and the one that opens at it later: both titled
	// after the issue, so each is attributed to it.
	pushedPast := func(repo string) string {
		return fmt.Sprintf(`{"number":97,"title":%q,"body":"","html_url":"https://github.com/%s/pull/97",
			"state":"open","merged":false,"updated_at":"2026-11-05T07:15:08Z",
			"head":{"ref":"legion/x","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}`,
			issue.Key+" the work", repo, newHeadCommit, repo)
	}
	openedAtTheCommit := func(repo, sha string) string {
		return fmt.Sprintf(`{"number":98,"title":%q,"body":"","html_url":"https://github.com/%s/pull/98",
			"state":"open","merged":false,"updated_at":"2026-11-05T07:15:08Z",
			"head":{"ref":"legion/y","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}`,
			issue.Key+" the rest", repo, sha, repo)
	}

	// 1. GitHub names the one pull request, whose head is another commit, and the read settles
	// the pair on that answer.
	fake.answers(func(repo, _ string) string { return "[" + pushedPast(repo) + "]" })
	standsAt(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID,
		"the read that settles the commit", model.CommitOtherCommit, false)

	// 2. The second pull request opens with its head at that commit, and nothing tells Dispatch:
	// a deployment holding no delivery settings consumes no `pull_request` event, and the
	// reconcile's head pass only re-reads rows that already exist.
	fake.answers(func(repo, sha string) string {
		return "[" + pushedPast(repo) + "," + openedAtTheCommit(repo, sha) + "]"
	})

	// 3. Inside the span no second call is spent. This step runs within
	// delivery.CommitPullReadStaleAfter of the claim above, so that bound alone refuses the read
	// and the settling is not what this step proves: the settle itself is pinned by
	// TestASupersededResultsCommitIsAskedOfGitHubOnceAndNotAgainWithinTheSpan and by the span
	// tests of delivery/commit_pull_reads_test.go. What it does prove is the cost a tab left
	// open carries: a commit GitHub has answered about is not asked again inside the span.
	spent := fake.asks("acme/widgets")
	standsAt(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID,
		"the read inside the span, where no call is spent and the stored answer stands",
		model.CommitOtherCommit, false)
	if asked := fake.asks("acme/widgets") - spent; asked != 0 {
		t.Fatalf("the read inside the span asked GitHub %d times, want none", asked)
	}

	// 4. Past the span. The cache's moments are its only clock, and the public way to move one
	// is to stamp the pair again from an earlier moment: a settling made
	// delivery.CommitPullSettledFor ago is one whose span has just run out, which is what the
	// clock does on its own between a reviewer's refetches, without the test waiting an hour for
	// it — as the sibling tests rewind the whole cache to its zero value for the shorter span.
	served.commitPulls.Settle("acme/widgets", headCommit, time.Now().Add(-delivery.CommitPullSettledFor))
	spent = fake.asks("acme/widgets")
	standsAt(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID,
		"the read past the settled span, where a pull request has opened at the commit",
		model.CommitAtBasis, false)
	if asked := fake.asks("acme/widgets") - spent; asked != 1 {
		t.Fatalf("the read past the settled span asked GitHub %d times, want 1", asked)
	}
}

// What a child's result on its parent's tab costs: nothing. Its commit heads the child's own
// pull request, attributed to the child, so Dispatch has been told that commit's pull request
// and asking GitHub again teaches it nothing. The result stands at no basis of the parent's
// check by the rule itself, whatever GitHub answers (R4, api/results_comparison.go,
// readThroughHead).
func TestAChildsResultOnTheParentsTabAsksGitHubNothing(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
	parent := issueInRepository(t, handler, "RESN", "acme/widgets", "Parent")
	uploadDocument(t, handler, "/api/v1/issues/"+parent.Key, "verification.md", siblingChecks)
	child := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESN", "title": "Child", "parent": parent.Key, "force": true}, "alice"))
	// The child's own pull request: open at the commit and attributed to the child.
	writeHead(t, database, "acme/widgets", 98, headCommit, "open", child.Key)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, child.Key, resultBody("k1", "pass", headCommit, nil)))
	// GitHub would name that pull request again, so a call that was spent would show as an ask;
	// and the cache, rewound to its zero value, holds no read off.
	fake.answers(openPullAt(98, child.Key))
	served.commitPulls = delivery.CommitPullReads{}
	spent := fake.asks("acme/widgets")

	standsAt(t, readResults(t, handler, parent.Key), "k1", recorded.Result.ID,
		"the child's result at the child's own head", model.CommitOtherCommit, false)
	if asked := fake.asks("acme/widgets") - spent; asked != 0 {
		t.Fatalf("the read asked GitHub for the child's commit's pull requests %d times, want none", asked)
	}
}

// Eligibility is the (issue, commit) pair's, and never one result's. A check holds a result at
// the head of a pull request nothing told Dispatch of and a later result at a commit no pull
// request carries, and a sibling check holds one result, at that same head. Every check of
// the issue is compared with one list of heads (api/results_comparison.go, headBases), so a read
// the eligibility test skipped for one result must not stand as the answer for the rest: both
// results at the head stand at it, whichever check recorded them and whenever.
func TestEveryResultOfAnIssueAtAPullRequestsHeadStandsAtIt(t *testing.T) {
	handler, _, served, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESZ", "acme/widgets", "Two checks at one head")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecks)

	atHead := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	sibling := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k3", "pass", headCommit, nil)))
	// A later result of the first check, at a commit no pull request carries: a run against a
	// working copy, or the commit of a branch nobody has opened a pull request from.
	decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "fail", newHeadCommit, nil)))

	// The pull request opens at the first commit, and nothing tells Dispatch.
	open := openPullAt(91, issue.Key)
	fake.answers(func(repo, sha string) string {
		if sha == headCommit {
			return open(repo, sha)
		}
		return `[]`
	})
	served.commitPulls = delivery.CommitPullReads{}

	read := readResults(t, handler, issue.Key)
	standsAt(t, read, "k3", sibling.Result.ID,
		"the sibling check's only result, at the pull request's head", model.CommitAtBasis, false)
	standsAt(t, read, "k1", atHead.Result.ID,
		"the earlier result of the check that has a later one elsewhere", model.CommitAtBasis, false)
}

// The same, where the later result is a child's. A child records against a check of its parent
// at the head of the child's own pull request, which Dispatch holds. The parent's own results
// sit at the parent's head, which it holds nothing for. The child's commit is eligible for no
// read — Dispatch keeps that row and asking GitHub teaches nothing — and the parent's commit
// is eligible for one, so the parent's results stand at the parent's head and the child's
// commit costs no call.
func TestAParentsResultsStandAtItsHeadThoughAChildRecordedOnTheSameCheck(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
	parent := issueInRepository(t, handler, "RESY", "acme/widgets", "Parent")
	uploadDocument(t, handler, "/api/v1/issues/"+parent.Key, "verification.md", siblingChecks)
	child := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESY", "title": "Child", "parent": parent.Key, "force": true}, "alice"))

	atHead := decodeBody[recordedResult](t, recordResult(t, handler, parent.Key, resultBody("k1", "pass", headCommit, nil)))
	sibling := decodeBody[recordedResult](t, recordResult(t, handler, parent.Key, resultBody("k3", "pass", headCommit, nil)))
	// The child's own pull request, open at its own head, and the child's result against the
	// parent's check there — recorded after the parent's two.
	writeHead(t, database, "acme/widgets", 98, newHeadCommit, "open", child.Key)
	decodeBody[recordedResult](t, recordResult(t, handler, child.Key, resultBody("k1", "pass", newHeadCommit, nil)))

	// The parent's pull request opens at the parent's head, and nothing tells Dispatch.
	open := openPullAt(91, parent.Key)
	fake.answers(func(repo, sha string) string {
		if sha == headCommit {
			return open(repo, sha)
		}
		return `[]`
	})
	served.commitPulls = delivery.CommitPullReads{}
	spent := fake.asks("acme/widgets")

	read := readResults(t, handler, parent.Key)
	standsAt(t, read, "k3", sibling.Result.ID,
		"the sibling check's only result, at the parent's head", model.CommitAtBasis, false)
	standsAt(t, read, "k1", atHead.Result.ID,
		"the parent's result at the parent's head, beside the child's", model.CommitAtBasis, false)
	if asked := fake.asks("acme/widgets") - spent; asked != 1 {
		t.Errorf("the read asked GitHub %d times, want 1: the parent's head alone, since Dispatch keeps the child's row", asked)
	}
}

// The control the two above are read against: one check, one result, at the head of a pull
// request nothing told Dispatch of, and nothing else recorded anywhere. It stands at that
// head, so a failure of either test above is the eligibility test refusing the call and not
// the read-through failing to repair the result (api/results_comparison.go, readThroughHead).
func TestACheckWithOneResultAtAPullRequestsHeadStandsAtIt(t *testing.T) {
	handler, _, served, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESX", "acme/widgets", "One result")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecks)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k3", "pass", headCommit, nil)))
	fake.answers(openPullAt(91, issue.Key))
	served.commitPulls = delivery.CommitPullReads{}

	standsAt(t, readResults(t, handler, issue.Key), "k3", recorded.Result.ID,
		"one result at the head of a pull request opened after it was recorded",
		model.CommitAtBasis, false)
}

// A closed pull request at the commit holds no read off. Dispatch holds a row for a pull
// request closed without merging at the result's commit, and a new pull request is then
// opened from the same head while no event reaches Dispatch. The closed row is no basis and
// no pass of Dispatch's ever reads it again (delivery.ListStalePullRequestHeads), so counting
// it as a pull request Dispatch holds would leave the result standing at no basis for good.
// The read asks GitHub and the result stands at the new head.
func TestAClosedPullRequestsHeadDoesNotHoldOffTheReadThrough(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESW", "acme/widgets", "Opened again from the same head")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecks)
	writeHead(t, database, "acme/widgets", 90, headCommit, "closed", issue.Key)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	standsAt(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID,
		"a result at the head of a closed pull request", model.CommitOtherCommit, false)

	// The new pull request, opened from that same head.
	fake.answers(openPullAt(91, issue.Key))
	served.commitPulls = delivery.CommitPullReads{}

	standsAt(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID,
		"a result at the head of a pull request opened after a closed one", model.CommitAtBasis, false)
}

// ?at=<commit> replaces what every check of the issue is compared with by the one commit the
// reader named, whatever each check's `when`, and makes no read of GitHub for it: the reader
// asked what the results say about one commit, and nothing about that question turns on what
// GitHub holds now. A value that is not a commit of 40 lowercase hex characters is refused.
func TestResultsReadAtACommitComparesEveryCheckWithThatCommit(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESAT", "acme/widgets", "Read at a commit")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	writeHead(t, database, "acme/widgets", 62, headCommit, "open", issue.Key)

	atHead := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	merged := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", mergeCommit, nil)))

	// GitHub would name a pull request for either commit and nothing holds a read-through off,
	// so a read that asked would show as an ask.
	fake.answers(openPullAt(93, issue.Key))
	served.commitPulls = delivery.CommitPullReads{}
	spent := fake.asks("acme/widgets")

	read := readResultsAt(t, handler, issue.Key, mergeCommit)
	for _, blockID := range []string{"k1", "k2"} {
		compared := checkOf(t, read, blockID).ComparedWith
		if len(compared) != 1 || compared[0].Kind != model.CheckBasisNamed || compared[0].Commit != mergeCommit {
			t.Fatalf("check %s compared with %+v, want the one commit %s the reader named", blockID, compared, mergeCommit)
		}
	}
	standsAt(t, read, "k1", atHead.Result.ID,
		"the pre-merge result, at its pull request's head and not at the commit the reader named",
		model.CommitOtherCommit, false)
	standsAt(t, read, "k2", merged.Result.ID,
		"the after-merge result, at the commit the reader named", model.CommitAtBasis, false)
	if stood := checkOf(t, read, "k1").PassStands; stood.Answer != model.PassStandsNo {
		t.Fatalf("the pre-merge check answers pass_stands %+v at that commit, want no", stood)
	}
	if stood := checkOf(t, read, "k2").PassStands; stood.Answer != model.PassStandsYes {
		t.Fatalf("the after-merge check answers pass_stands %+v at that commit, want yes", stood)
	}
	if asked := fake.asks("acme/widgets") - spent; asked != 0 {
		t.Fatalf("the read at one commit asked GitHub %d times, want none", asked)
	}

	for _, at := range []string{"nope", strings.ToUpper(nearHeadCommit), headCommit[:39]} {
		refused := dispatchRequest(t, handler, http.MethodGet,
			"/api/v1/issues/"+issue.Key+"/results?at="+at, nil, "alice")
		if code, _ := refusalOf(t, refused); refused.Code != http.StatusBadRequest || code != "INVALID_QUERY" {
			t.Fatalf("a read at %q: status=%d body=%s", at, refused.Code, refused.Body.String())
		}
	}
}

// readResultsAt is readResults with ?at=<commit>.
func readResultsAt(t *testing.T, handler http.Handler, issue, at string) issueResults {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/issues/"+issue+"/results?at="+at, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s at %s: status=%d body=%s", issue, at, response.Code, response.Body.String())
	}
	return decodeBody[issueResults](t, response)
}

// The four ways a check comes to compare with nothing, and the two answers they part into. An
// issue that resolves to no repository and a `before_merge` check whose issue holds no pull
// request have no pull request and no branch to compare with, which is a fact about the work:
// no pass of either stands. A repository the App cannot read is the other answer, and leaves
// the gate's question unanswered rather than answered no.
//
// The two were one answer before dispatch://INFRA-1522, and a reviewer could not tell a check
// nothing proves from one Dispatch could not read.
func TestACheckComparingWithNothingNamesWhichKindOfNothing(t *testing.T) {
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true},
		func(string, string) string { return "diverged" })

	// 1. An issue that resolves to no repository at all: createInteractionIssue creates the
	// project itself, and nothing maps a repository to it.
	loose := createInteractionIssue(t, handler, "NON1", "No repository", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+loose.Key, "verification.md", twoChecks)
	adrift := decodeBody[recordedResult](t, recordResult(t, handler, loose.Key, resultBody("k1", "pass", headCommit, nil)))
	looseRead := readResults(t, handler, loose.Key)
	comparedNothing(t, looseRead, "k1", model.ReasonNoPullRequestOrBranch, model.PassStandsNo)
	standsAt(t, looseRead, "k1", adrift.Result.ID,
		"a result of an issue that resolves to no repository", model.CommitOtherCommit, false)

	// 2. A `before_merge` check whose issue holds no pull request at all, in a repository the
	// App reads: the branches are not asked about at all, and there is no head to name.
	seen := issueInRepository(t, handler, "NON2", "acme/widgets", "No pull request")
	uploadDocument(t, handler, "/api/v1/issues/"+seen.Key, "verification.md", twoChecks)
	unheld := decodeBody[recordedResult](t, recordResult(t, handler, seen.Key, resultBody("k1", "pass", headCommit, nil)))
	seenRead := readResults(t, handler, seen.Key)
	comparedNothing(t, seenRead, "k1", model.ReasonNoPullRequestOrBranch, model.PassStandsNo)
	standsAt(t, seenRead, "k1", unheld.Result.ID,
		"a result of an issue holding no pull request", model.CommitOtherCommit, false)

	// 3. A repository that resolves and that the App cannot read.
	hidden := issueInRepository(t, handler, "NON3", "acme/hidden", "Unreadable")
	uploadDocument(t, handler, "/api/v1/issues/"+hidden.Key, "verification.md", twoChecks)
	unseen := decodeBody[recordedResult](t, recordResult(t, handler, hidden.Key, resultBody("k2", "pass", mergeCommit, nil)))
	hiddenRead := readResults(t, handler, hidden.Key)
	comparedNothing(t, hiddenRead, "k2", model.ReasonRepositoryUnreadable, model.PassStandsUnknown)
	stands := standsAt(t, hiddenRead, "k2", unseen.Result.ID,
		"a result of a repository the App cannot read", model.CommitUnknown, false)
	if stands.Reason != model.ReasonRepositoryUnreadable {
		t.Fatalf("that result is unknown for %q, want %q", stands.Reason, model.ReasonRepositoryUnreadable)
	}

	// 4. An issue that resolved to a repository, merged into a branch, and resolves to none
	// now, its mapping deleted after the merge — the issue whose project mapping was removed,
	// and the issue whose parent moved. This pins the deep review's finding on #3530: the
	// merged row delivery.BaseBranchMergedFor reads by issue key alone goes on naming
	// `integration/x`, so a branch walk chosen before anything tests the repository reached
	// onNamedBranch with none and answered `unknown`/`read_failed` for every result, against
	// what the surface's own rule says — an issue that resolves to no repository is never
	// unknown, and a retry settles nothing here.
	adopted := issueInRepository(t, handler, "NON4", "acme/widgets", "Mapping deleted after the merge")
	uploadDocument(t, handler, "/api/v1/issues/"+adopted.Key, "verification.md", twoChecks)
	recordMerge(t, database, "acme/widgets", 41, adopted.Key, "integration/x")
	orphaned := decodeBody[recordedResult](t,
		recordResult(t, handler, adopted.Key, resultBody("k2", "pass", mergeCommit, nil)))
	unmapped := dispatchRequest(t, handler, http.MethodDelete,
		"/api/v1/settings/repo-projects/acme/widgets", nil, "alice")
	if unmapped.Code != http.StatusNoContent {
		t.Fatalf("delete the mapping of acme/widgets: status=%d body=%s", unmapped.Code, unmapped.Body.String())
	}
	adoptedRead := readResults(t, handler, adopted.Key)
	comparedNothing(t, adoptedRead, "k2", model.ReasonNoPullRequestOrBranch, model.PassStandsNo)
	standsAt(t, adoptedRead, "k2", orphaned.Result.ID,
		"an after-merge result of an issue whose repository mapping was deleted after its merge",
		model.CommitOtherCommit, false)
}

// A resolve of the check's issue's repository that did not answer is a read of Dispatch's own
// that said nothing, and never a verdict about the work. A parent chain that loops is how
// that resolve fails on demand: refs.IssueRepository refuses a chain it cannot finish rather
// than answer with the project's default mapping (refs.IssueRepoTerms), so every read of the
// repository below it says it could not tell.
//
// This pins the deep review's finding on #3530: repositoryOf answered nil for two worlds —
// an issue that resolves to no repository, and a resolve that failed — and each reader of
// that nil took the first. The three rows here are the three readers. branchBases sent an
// `after_merge` check to `no_pull_request_or_branch`; headBases, through nothingReason, sent
// a `before_merge` check of an issue holding no pull request to the same word; and the
// `before_merge` arm of commitStands read a commit at none of the heads it does hold as
// `other_commit`. All three then answered `pass_stands` no, which is a reviewer's approval
// refused over a database hiccup, against the rule the three reasons are kept apart for
// (model.ReasonReadFailed, "a read Dispatch makes did not answer") and against
// results_comparison.go's own: a database hiccup must not make a reviewer refuse an approval.
//
// The other two states keep the answers they had. An issue that resolves to no repository is
// cases 1 and 4 of TestACheckComparingWithNothingNamesWhichKindOfNothing, which go on reading
// `no_pull_request_or_branch` and `pass_stands` no; a repository that resolves is every other
// test of this file, TestPassStandsNamesTheRolesWhosePassesStand among them.
func TestAResolveThatDidNotAnswerIsUnknownAndNotAVerdict(t *testing.T) {
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true},
		func(string, string) string { return "diverged" })

	// Two issues of one project, each the other's parent below. Both are seeded and recorded
	// against first, while the chain is still whole, so what the loop changes is the read
	// alone: the work is done, the merge is held and the passes are in the table.
	merged := issueInRepository(t, handler, "NONR", "acme/widgets", "Merged, then looped")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "NONR", "title": "No pull request, then looped", "spec": "A spec\n"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the second issue of NONR: status=%d body=%s", created.Code, created.Body.String())
	}
	unheld := decodeBody[model.Issue](t, created)
	for _, issue := range []string{merged.Key, unheld.Key} {
		uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", twoChecks)
	}
	recordMerge(t, database, "acme/widgets", 42, merged.Key, "main")
	atMerge := decodeBody[recordedResult](t,
		recordResult(t, handler, merged.Key, resultBody("k2", "pass", mergeCommit, nil)))
	beforeMerge := decodeBody[recordedResult](t,
		recordResult(t, handler, merged.Key, resultBody("k1", "pass", mergeCommit, nil)))
	atNoHead := decodeBody[recordedResult](t,
		recordResult(t, handler, unheld.Key, resultBody("k1", "pass", headCommit, nil)))

	// The loop, written straight into issues.parent_key as a direct database write or an
	// earlier release's API makes one (refs.IssueRepoTerms): neither chain can be finished, so
	// refs.IssueRepository refuses both as too deep and the resolve answers nothing at all.
	for _, link := range [][2]string{{merged.Key, unheld.Key}, {unheld.Key, merged.Key}} {
		if _, err := database.Pool.Exec(context.Background(),
			`update issues set parent_key = $1 where key = $2`, link[1], link[0]); err != nil {
			t.Fatalf("loop the parent chain of %s: %v", link[0], err)
		}
	}

	looped := readResults(t, handler, merged.Key)
	noHead := readResults(t, handler, unheld.Key)

	// One row per reader of the resolve, each failing on its own: three answers moved, and a
	// run that stopped at the first would say so of one.
	t.Run("the after-merge check of a merged issue compares with nothing it could name", func(t *testing.T) {
		// branchBases. No branch can be named for a repository that cannot be named, and the
		// merged row still naming `main` is no help: there is no repository to ask GitHub or
		// the stored rows about it under. This is the read both reviewers reproduced.
		comparedNothing(t, looped, "k2", model.ReasonReadFailed, model.PassStandsUnknown)
		stands := standsAt(t, looped, "k2", atMerge.Result.ID,
			"an after-merge result whose issue's repository could not be resolved",
			model.CommitUnknown, false)
		if stands.Reason != model.ReasonReadFailed {
			t.Fatalf("that result is unknown for %q, want %q", stands.Reason, model.ReasonReadFailed)
		}
	})

	t.Run("the pre-merge result at none of the heads held is unknown and not another commit", func(t *testing.T) {
		// The `before_merge` arm of commitStands. The same issue's pre-merge check compares
		// with the head of the merged pull request Dispatch holds for it, which the resolve
		// has no part in (delivery.HeadsAttributedTo reads by issue key alone), and the result
		// is at another commit. Whether that commit heads a pull request Dispatch has not
		// learned turns on reading the repository, which is the read that did not answer.
		compared := checkOf(t, looped, "k1").ComparedWith
		if len(compared) != 1 || compared[0].Kind != model.CheckBasisPullRequest || compared[0].Commit != headCommit {
			t.Fatalf("the pre-merge check compared with %+v, want the one head %s Dispatch holds for it",
				compared, headCommit)
		}
		stands := standsAt(t, looped, "k1", beforeMerge.Result.ID,
			"a pre-merge result at no held head whose issue's repository could not be resolved",
			model.CommitUnknown, false)
		if stands.Reason != model.ReasonReadFailed {
			t.Fatalf("that result is unknown for %q, want %q", stands.Reason, model.ReasonReadFailed)
		}
		if stood := checkOf(t, looped, "k1").PassStands; stood.Answer != model.PassStandsUnknown {
			t.Fatalf("the pre-merge check answers pass_stands %+v, want unknown: a resolve that did "+
				"not answer is no reason to refuse an approval", stood)
		}
	})

	t.Run("the pre-merge check of an issue holding no pull request compares with nothing it could name", func(t *testing.T) {
		// headBases, through nothingReason. The heads are every head Dispatch holds, but only
		// an issue whose repository resolves can be said to hold every head it will ever hold.
		comparedNothing(t, noHead, "k1", model.ReasonReadFailed, model.PassStandsUnknown)
		stands := standsAt(t, noHead, "k1", atNoHead.Result.ID,
			"a pre-merge result of an issue holding no pull request whose repository could not be resolved",
			model.CommitUnknown, false)
		if stands.Reason != model.ReasonReadFailed {
			t.Fatalf("that result is unknown for %q, want %q", stands.Reason, model.ReasonReadFailed)
		}
	})
}

// The gate's question over several results of one check. A fail stands for nothing, a pass at
// a commit no basis names does not stand, and the two passes at the head do, so the check
// answers yes and names both roles. Rewording the check takes every one of them away: a pass
// whose check's words moved is no pass that stands (R5), although each result stands at the
// head exactly as it did, which is the separation the two fields keep.
func TestPassStandsNamesTheRolesWhosePassesStand(t *testing.T) {
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true},
		func(string, string) string { return "diverged" })
	issue := issueInRepository(t, handler, "RESPS", "acme/widgets", "Several results")
	owner := "/api/v1/issues/" + issue.Key
	uploadDocument(t, handler, owner, "verification.md", siblingChecks)
	writeHead(t, database, "acme/widgets", 63, headCommit, "open", issue.Key)

	recordResult(t, handler, issue.Key, resultBody("k1", "fail", headCommit, map[string]any{"failed": 1}))
	recordResult(t, handler, issue.Key, resultBody("k1", "pass", newHeadCommit, map[string]any{"role": "tester"}))
	byImplementer := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key,
		resultBody("k1", "pass", headCommit, nil)))
	recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, map[string]any{"role": "reviewer"}))
	recordResult(t, handler, issue.Key, resultBody("k3", "fail", headCommit, map[string]any{"failed": 1}))

	read := readResults(t, handler, issue.Key)
	stood := checkOf(t, read, "k1").PassStands
	roles := slices.Sorted(slices.Values(stood.Roles))
	if stood.Answer != model.PassStandsYes || !slices.Equal(roles, []string{"implementer", "reviewer"}) {
		t.Fatalf("the check answers pass_stands %+v, want yes from the implementer and the reviewer", stood)
	}
	if sibling := checkOf(t, read, "k3").PassStands; sibling.Answer != model.PassStandsNo || len(sibling.Roles) != 0 {
		t.Fatalf("the sibling check, holding one fail, answers pass_stands %+v, want no and no roles", sibling)
	}

	// The words move, and no pass of the check stands any more, although every result stands
	// at the same commit it did before.
	uploadDocument(t, handler, owner, "verification.md", siblingChecksReworded)
	reworded := readResults(t, handler, issue.Key)
	if stood := checkOf(t, reworded, "k1").PassStands; stood.Answer != model.PassStandsNo {
		t.Fatalf("the reworded check answers pass_stands %+v, want no: a pass whose words moved does not stand", stood)
	}
	standsAt(t, reworded, "k1", byImplementer.Result.ID,
		"the implementer's pass at the head, under words that moved since", model.CommitAtBasis, true)
}

// A retracted result counts as no evidence. The read carries the retraction with its reason
// and its actor, the result itself is unchanged and still stands at the head, and the pass it
// recorded no longer stands.
func TestARetractedPassDoesNotStand(t *testing.T) {
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true},
		func(string, string) string { return "diverged" })
	issue := issueInRepository(t, handler, "RESRT", "acme/widgets", "Retracted")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecks)
	writeHead(t, database, "acme/widgets", 64, headCommit, "open", issue.Key)

	pass := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	if stood := checkOf(t, readResults(t, handler, issue.Key), "k1").PassStands; stood.Answer != model.PassStandsYes {
		t.Fatalf("the check answers pass_stands %+v before the retraction, want yes", stood)
	}

	reason := "The run was against the wrong branch."
	if retracted := retractAsPerson(t, handler, pass.Result.ID, reason); retracted.Code != http.StatusCreated {
		t.Fatalf("retract the pass: status=%d body=%s", retracted.Code, retracted.Body.String())
	}

	read := readResults(t, handler, issue.Key)
	result := resultRead(t, read, "k1", pass.Result.ID)
	if result.Retracted == nil || result.Retracted.Reason != reason || result.Retracted.Actor.ID == "" ||
		result.Retracted.CreatedAt.IsZero() {
		t.Fatalf("the retracted result carries %+v, want the reason, the actor and the time", result.Retracted)
	}
	standsAt(t, read, "k1", pass.Result.ID, "a retracted pass", model.CommitAtBasis, false)
	if stood := checkOf(t, read, "k1").PassStands; stood.Answer != model.PassStandsNo {
		t.Fatalf("the check answers pass_stands %+v after the retraction, want no", stood)
	}
}

// A check CI proves takes no result from any role: a tester's and an implementer's are both
// refused, 409 CHECK_PROVED_BY_CI naming the block and the check run, and nothing is stored. The
// refusal comes before the commit is asked of GitHub: the commit here is one the repository does
// not hold, which would otherwise be refused as RESULT_COMMIT_UNKNOWN. The same call against an
// ordinary check of the same document is recorded.
func TestResultAgainstACheckCIProvesIsRefused(t *testing.T) {
	handler, database := resultsGitHubHolding(t, map[string]bool{"acme/widgets": true},
		map[string]bool{headCommit: true}, func(string, string) string { return "diverged" })
	issue := issueInRepository(t, handler, "RESCI", "acme/widgets", "Proved by CI")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", ciCheckBlock("c1", ciCheckKept)+"\n"+twoChecks)

	want := `check c1 is proved by CI run "` + ciCheckRun + `": dispatch results reads its result from that run, and no role records one`
	for _, role := range []string{"tester", "implementer"} {
		refused := recordResult(t, handler, issue.Key, resultBody("c1", "pass", nearHeadCommit, map[string]any{"role": role}))
		if code, text := refusalOf(t, refused); refused.Code != http.StatusConflict || code != "CHECK_PROVED_BY_CI" || text != want {
			t.Fatalf("the %s's result against c1: status=%d body=%s, want 409 CHECK_PROVED_BY_CI %q", role, refused.Code, refused.Body.String(), want)
		}
	}
	if results := checkOf(t, readResults(t, handler, issue.Key), "c1").Results; len(results) != 0 {
		t.Fatalf("the check CI proves holds %+v, want no result", results)
	}
	if events := resultEventsOn(t, database, issue.Key); events != 0 {
		t.Fatalf("the issue holds %d result.recorded events, want none", events)
	}

	if recorded := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, map[string]any{"role": "tester"})); recorded.Code != http.StatusCreated {
		t.Fatalf("the same call against an ordinary check: status=%d body=%s, want 201", recorded.Code, recorded.Body.String())
	}
}

// A result and a ci_check written against one block at once can both commit, and the block then
// holds a recorded result, so it is no check CI proves. Crafted here as a browser's write that
// gives a ci_check to a check already holding a result, before any settlement: the results read
// shows it as an ordinary check carrying its ci_check and its recorded result, and it takes a
// further result. Its sibling, given the same ci_check and holding no result, is CI's. The next
// settlement takes the inert ci_check off the block holding results, with one block.invalid
// naming the fourth rule, and keeps its sibling's.
func TestResultAgainstACICheckHoldingAResultIsRecorded(t *testing.T) {
	documents, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "RESCR", "Raced", "A spec\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		ciCheckBlock("c1", ciCheckOrdinary)+"\n"+ciCheckBlock("c2", ciCheckOrdinary))
	first := recordResult(t, handler, issue.Key, resultBody("c1", "pass", headCommit, nil))
	if first.Code != http.StatusCreated {
		t.Fatalf("record a result against c1: status=%d body=%s", first.Code, first.Body.String())
	}
	writeBrowserDocument(t, documents, verification.Artifact.ID, ciCheckBlock("c1", ciCheckKept)+"\n"+ciCheckBlock("c2", ciCheckKept))

	held := checkOf(t, readResults(t, handler, issue.Key), "c1")
	if held.CICheck != ciCheckRun || len(held.Results) != 1 || held.Results[0].ID != decodeBody[recordedResult](t, first).Result.ID {
		t.Fatalf("c1 reads %+v, want its ci_check and the result recorded against it", held)
	}
	if again := recordResult(t, handler, issue.Key, resultBody("c1", "fail", newHeadCommit, map[string]any{"role": "tester", "failed": 1})); again.Code != http.StatusCreated {
		t.Fatalf("a further result against c1: status=%d body=%s, want 201", again.Code, again.Body.String())
	}
	if results := checkOf(t, readResults(t, handler, issue.Key), "c1").Results; len(results) != 2 {
		t.Fatalf("c1 holds %+v, want both results", results)
	}

	refused := recordResult(t, handler, issue.Key, resultBody("c2", "pass", headCommit, nil))
	if code, _ := refusalOf(t, refused); refused.Code != http.StatusConflict || code != "CHECK_PROVED_BY_CI" {
		t.Fatalf("a result against c2: status=%d body=%s, want 409 CHECK_PROVED_BY_CI", refused.Code, refused.Body.String())
	}

	if err := documents.SettleIssueDocuments(context.Background(), issue.Key); err != nil {
		t.Fatalf("settle the issue's documents: %v", err)
	}
	settled := readResults(t, handler, issue.Key)
	if c1 := checkOf(t, settled, "c1"); c1.CICheck != "" || len(c1.Results) != 2 {
		t.Fatalf("c1 once settled reads %+v, want it without its ci_check and holding both results", c1)
	}
	if c2 := checkOf(t, settled, "c2"); c2.CICheck != ciCheckRun {
		t.Fatalf("c2 once settled reads %+v, want its ci_check %q kept", c2, ciCheckRun)
	}
	want := []ciCheckRefusalEvent{{BlockID: "c1", Reason: ciCheckRecordedRule, BlockType: "check"}}
	if got := ciCheckRefusalEvents(t, handler, issue.Key); !slices.Equal(got, want) {
		t.Fatalf("block.invalid events = %+v, want %+v", got, want)
	}
}

// checkRunsFake is GitHub's listing of each commit's check runs, a hundred to a page, as the
// read of a check CI proves asks for it, and the count of the pages asked for each commit. A
// test sets the runs of a commit, a status and body every read answers in place of the listing,
// or a hold that keeps every read waiting until its caller gives up.
type checkRunsFake struct {
	mu        sync.Mutex
	runs      map[string][]map[string]any
	fault     int
	faultBody string
	hold      bool
	asked     map[string]int
}

func newCheckRunsFake() *checkRunsFake {
	return &checkRunsFake{runs: map[string][]map[string]any{}, asked: map[string]int{}}
}

// set is the runs GitHub lists for sha, in the order given.
func (f *checkRunsFake) set(sha string, runs ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[sha] = runs
}

// failWith makes every read answer status with a message of GitHub's shape, and 0 ends it.
func (f *checkRunsFake) failWith(status int) {
	f.failWithBody(status, "")
}

// failWithBody makes every read answer status with body, or with a message of GitHub's shape
// where body is "", and 0 ends it.
func (f *checkRunsFake) failWithBody(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fault, f.faultBody = status, body
}

// holdEveryRead makes every read wait until its caller gives up.
func (f *checkRunsFake) holdEveryRead() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold = true
}

// reads is how many pages of sha's check runs GitHub has been asked for.
func (f *checkRunsFake) reads(sha string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked[sha]
}

// allReads is how many pages of check runs GitHub has been asked for, at any commit.
func (f *checkRunsFake) allReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, asked := range f.asked {
		total += asked
	}
	return total
}

func (f *checkRunsFake) serve(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 || r.URL.Query().Get("filter") != "all" {
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprintf(w, `{"message":"want filter=all and a page, got %s"}`, r.URL.RawQuery)
		return
	}
	f.mu.Lock()
	f.asked[sha]++
	fault, faultBody, hold, runs := f.fault, f.faultBody, f.hold, f.runs[sha]
	f.mu.Unlock()
	if hold {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		return
	}
	if fault != 0 {
		w.WriteHeader(fault)
		if faultBody != "" {
			fmt.Fprint(w, faultBody)
			return
		}
		fmt.Fprintf(w, `{"message":"Server Error at the check runs of %s"}`, sha)
		return
	}
	from, to := min((page-1)*100, len(runs)), min(page*100, len(runs))
	body, err := json.Marshal(map[string]any{"total_count": len(runs), "check_runs": runs[from:to]})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(body)
}

// resultsGitHubCheckRuns is resultsGitHub over acme/widgets whose check runs fake lists.
func resultsGitHubCheckRuns(t *testing.T) (http.Handler, *store.Store, *checkRunsFake) {
	t.Helper()
	fake := newCheckRunsFake()
	handler, database, _ := resultsGitHubServed(t, map[string]bool{"acme/widgets": true}, nil, "main",
		func(string, string) (int, string) { return http.StatusOK, "diverged" }, fake.serve)
	return handler, database, fake
}

// ciRunCompletedAt is when every concluded run of these tests concluded.
const ciRunCompletedAt = "2026-11-12T10:00:00Z"

// completedRun is a check run GitHub Actions concluded, with no output, as its own jobs carry.
func completedRun(id int64, name, conclusion string) map[string]any {
	return map[string]any{
		"id": id, "name": name, "status": "completed", "conclusion": conclusion,
		"completed_at": ciRunCompletedAt, "html_url": ciRunURL(id),
		"output": map[string]any{"title": nil, "summary": nil},
		"app":    map[string]any{"slug": "github-actions"},
	}
}

// pendingRun is a check run that has not concluded: no conclusion and no completion time.
func pendingRun(id int64, name, status string) map[string]any {
	return map[string]any{
		"id": id, "name": name, "status": status, "conclusion": nil, "completed_at": nil,
		"html_url": ciRunURL(id), "output": map[string]any{"title": nil, "summary": nil},
		"app": map[string]any{"slug": "github-actions"},
	}
}

func ciRunURL(id int64) string {
	return fmt.Sprintf("https://github.com/acme/widgets/runs/%d", id)
}

// issueOfProject is a further issue of project with verification as its Verification document.
func issueOfProject(t *testing.T, handler http.Handler, project, title, verification string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": project, "title": title, "spec": "A spec\n"}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue of %s: status=%d body=%s", project, response.Code, response.Body.String())
	}
	key := decodeBody[model.Issue](t, response).Key
	uploadDocument(t, handler, "/api/v1/issues/"+key, "verification.md", verification)
	return key
}

// shortenCIReadDeadline sets the bound of one head's check-runs read for the test.
func shortenCIReadDeadline(t *testing.T, deadline time.Duration) {
	t.Helper()
	before := ciReadDeadline
	ciReadDeadline = deadline
	t.Cleanup(func() { ciReadDeadline = before })
}

// ciVerdictLineOf is the pull-request body line of a check written by ciCheckBlock whose run
// gave verdict at sha.
func ciVerdictLineOf(blockID, run, verdict, sha string) string {
	return fmt.Sprintf("- Check %s (before_merge): %s at %s, from CI run %s. The server builds at the head.",
		blockID, verdict, sha, run)
}

// ciUnconcludedLineOf is the pull-request body line of a check whose run did not decide it.
func ciUnconcludedLineOf(blockID, run, ciCase string) string {
	return fmt.Sprintf("- Check %s (before_merge): CI run %s has no result (%s)", blockID, run, ciCase)
}

// assertCI asserts what the read says of a check CI proves: its run, state, case and line.
func assertCI(t *testing.T, check model.CheckBlock, run, state, ciCase, line string) {
	t.Helper()
	if check.CI == nil {
		t.Fatalf("check %s carries no ci, want state %s (%s)", check.BlockID, state, ciCase)
	}
	if got := *check.CI; got.Run != run || got.State != state || got.Case != ciCase || got.Line != line || got.Detail == "" {
		t.Fatalf("check %s ci = %+v, want run %q, state %q, case %q, a detail and the line %q",
			check.BlockID, got, run, state, ciCase, line)
	}
}

// assertDerived asserts that the check's one result is the one derived from run id, which
// concluded conclusion at sha, and that it stands at the head it was read at.
func assertDerived(t *testing.T, check model.CheckBlock, id int64, run, conclusion, verdict, sha string) model.CheckResult {
	t.Helper()
	if len(check.Results) != 1 {
		t.Fatalf("check %s holds %+v, want the one result derived from run %d", check.BlockID, check.Results, id)
	}
	result := check.Results[0]
	completedAt, _ := time.Parse(time.RFC3339, ciRunCompletedAt)
	want := model.Actor{Kind: "github_app", ID: "github-actions"}
	if result.ID != fmt.Sprintf("ci-run:%d", id) || result.Role != "ci" || result.Actor.Kind != want.Kind ||
		result.Actor.ID != want.ID || result.Session != "" || result.Run != run || result.Verdict != verdict ||
		result.CommitSHA != sha || !result.CreatedAt.Equal(completedAt) ||
		result.NegativeControl != check.NegativeControl || result.Surface != check.Surface ||
		result.CheckBlockID != check.BlockID || result.CheckArtifactID != check.ArtifactID ||
		result.CheckIssueKey != check.IssueKey || result.CheckSHA256 != check.SHA256 ||
		result.Failed != 0 || result.Skipped != 0 || result.Excluded != 0 || result.EventSeq != nil ||
		result.Screenshots == nil || len(result.Screenshots) != 0 {
		t.Fatalf("check %s's derived result = %+v, want run %d's %s at %s by %+v, completed %s, carrying the check's own fields",
			check.BlockID, result, id, verdict, sha, want, ciRunCompletedAt)
	}
	if observed := "conclusion: " + conclusion + "\n" + ciRunURL(id); result.Observed != observed {
		t.Fatalf("check %s's derived result observed %q, want %q", check.BlockID, result.Observed, observed)
	}
	if result.CommitStands == nil || result.CommitStands.Answer != model.CommitAtBasis ||
		result.CheckWordsMoved == nil || *result.CheckWordsMoved {
		t.Fatalf("check %s's derived result stands %+v with words moved %v, want at the head it was read at, words unmoved",
			check.BlockID, result.CommitStands, result.CheckWordsMoved)
	}
	return result
}

// A check CI proves is answered by the run its ci_check names at the issue's open head, for every
// case R3 names: success is a pass with its one derived result; neutral, skipped, cancelled, a
// run not completed and a head carrying no run of that name are no result, each naming its case;
// timed_out and a conclusion the list does not name are a fail. The named run on page two of a
// head carrying 101 runs is read, because the read pages.
func TestCIResultReadsEachOutcomeOfTheNamedRun(t *testing.T) {
	handler, database, fake := resultsGitHubCheckRuns(t)
	issueInRepository(t, handler, "RESCO", "acme/widgets", "Outcomes")
	others := make([]map[string]any, 100)
	for index := range others {
		others[index] = completedRun(int64(1000+index), fmt.Sprintf("other-%d", index), "success")
	}
	for index, test := range []struct {
		name   string
		runs   []map[string]any
		state  string
		ciCase string
		run    int64
		pages  int
	}{
		{"success", []map[string]any{completedRun(11, ciCheckRun, "success")}, model.CIStatePass, "success", 11, 1},
		{"neutral", []map[string]any{completedRun(12, ciCheckRun, "neutral")}, model.CIStateNoResult, "neutral", 0, 1},
		{"skipped", []map[string]any{completedRun(13, ciCheckRun, "skipped")}, model.CIStateNoResult, "skipped", 0, 1},
		{"cancelled", []map[string]any{completedRun(14, ciCheckRun, "cancelled")}, model.CIStateNoResult, "cancelled", 0, 1},
		{"in progress", []map[string]any{pendingRun(15, ciCheckRun, "in_progress")}, model.CIStateNoResult, "not_completed", 0, 1},
		{"absent", []map[string]any{completedRun(16, "lint", "success")}, model.CIStateNoResult, "absent", 0, 1},
		{"on page two of 101 runs", append(slices.Clone(others), completedRun(17, ciCheckRun, "success")), model.CIStatePass, "success", 17, 2},
		{"timed out", []map[string]any{completedRun(18, ciCheckRun, "timed_out")}, model.CIStateFail, "timed_out", 18, 1},
		{"an unnamed conclusion", []map[string]any{completedRun(19, ciCheckRun, "frobnicated")}, model.CIStateFail, "frobnicated", 19, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			key := issueOfProject(t, handler, "RESCO", test.name, ciCheckBlock("c1", ciCheckKept))
			head := fmt.Sprintf("%040x", index+1)
			writeHead(t, database, "acme/widgets", 100+index, head, "open", key)
			fake.set(head, test.runs...)

			check := checkOf(t, readResults(t, handler, key), "c1")
			if test.run == 0 {
				assertCI(t, check, ciCheckRun, test.state, test.ciCase, ciUnconcludedLineOf("c1", ciCheckRun, test.ciCase))
				if len(check.Results) != 0 || check.PassStands.Answer != model.PassStandsNo {
					t.Fatalf("c1 holds %+v and pass_stands %+v, want no result and no pass", check.Results, check.PassStands)
				}
			} else {
				assertCI(t, check, ciCheckRun, test.state, test.ciCase, ciVerdictLineOf("c1", ciCheckRun, test.state, head))
				assertDerived(t, check, test.run, ciCheckRun, test.ciCase, test.state, head)
				wantStands := model.CheckPassStands{Answer: model.PassStandsNo}
				if test.state == model.CIStatePass {
					wantStands = model.CheckPassStands{Answer: model.PassStandsYes, Roles: []string{"ci"}}
				}
				if check.PassStands.Answer != wantStands.Answer || !slices.Equal(check.PassStands.Roles, wantStands.Roles) {
					t.Fatalf("c1 pass_stands = %+v, want %+v", check.PassStands, wantStands)
				}
			}
			if pages := fake.reads(head); pages != test.pages {
				t.Fatalf("the read asked for %d pages of %s's check runs, want %d", pages, head, test.pages)
			}
		})
	}
}

// assertUnread asserts that a check CI proves reads as GitHub unread, with each of words in its
// detail and no token, and holds no result and a pass standing that is not known: a failed read
// is neither a pass nor a fail.
func assertUnread(t *testing.T, check model.CheckBlock, words ...string) {
	t.Helper()
	assertCI(t, check, ciCheckRun, model.CIStateUnread, "github_unreadable",
		ciUnconcludedLineOf(check.BlockID, ciCheckRun, "github_unreadable"))
	for _, word := range words {
		if !strings.Contains(check.CI.Detail, word) {
			t.Fatalf("c1's detail %q does not carry %q", check.CI.Detail, word)
		}
	}
	if strings.Contains(check.CI.Detail, "ghs_") {
		t.Fatalf("c1's detail %q carries a token", check.CI.Detail)
	}
	if len(check.Results) != 0 || check.PassStands.Answer != model.PassStandsUnknown {
		t.Fatalf("c1 holds %+v and pass_stands %+v, want no result and pass_stands unknown", check.Results, check.PassStands)
	}
}

// A read Dispatch could not make is no verdict: GitHub's 500, a read held past the deadline and
// a deployment holding no App key each leave the check with no result and a detail saying what
// GitHub answered, never a pass and never a fail.
func TestCIResultIsUnreadWhenGitHubCannotBeRead(t *testing.T) {
	handler, database, fake := resultsGitHubCheckRuns(t)
	issue := issueInRepository(t, handler, "RESCU", "acme/widgets", "Unread")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", ciCheckBlock("c1", ciCheckKept))
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", issue.Key)
	fake.set(headCommit, completedRun(21, ciCheckRun, "success"))

	fake.failWith(http.StatusInternalServerError)
	assertUnread(t, checkOf(t, readResults(t, handler, issue.Key), "c1"),
		"status 500", "Server Error at the check runs of "+headCommit)

	fake.failWith(0)
	fake.holdEveryRead()
	shortenCIReadDeadline(t, 300*time.Millisecond)
	started := time.Now()
	assertUnread(t, checkOf(t, readResults(t, handler, issue.Key), "c1"), "no answer within 300ms")
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the held read answered after %s, want it cut at the deadline", elapsed)
	}

	keyless, keylessDatabase, deps := newTestServer(t, testServerOptions{})
	if deps.GitHub != nil {
		t.Fatalf("the fixture holds a GitHub client, want the deployment that holds no App key")
	}
	other := issueInRepository(t, keyless, "RESCK", "acme/widgets", "No App key")
	uploadDocument(t, keyless, "/api/v1/issues/"+other.Key, "verification.md", ciCheckBlock("c1", ciCheckKept))
	writeHead(t, keylessDatabase, "acme/widgets", 7, headCommit, "open", other.Key)
	assertUnread(t, checkOf(t, readResults(t, keyless, other.Key), "c1"), "no private key")
}

// GitHub's refusal of a read carries its whole body in the error, up to the 16 MiB a page of
// check runs may take, and none of it reaches the detail: the detail names the status and the
// `message` of a JSON body, names the status alone for a body that is not JSON, and is cut to
// 2,000 runes where the message itself is longer.
func TestCIResultDetailNamesTheStatusAndMessageOfALargeRefusal(t *testing.T) {
	handler, database, fake := resultsGitHubCheckRuns(t)
	issue := issueInRepository(t, handler, "RESCB", "acme/widgets", "Large refusal")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", ciCheckBlock("c1", ciCheckKept))
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", issue.Key)
	padding := strings.Repeat("x", 1<<20)
	for _, test := range []struct {
		name    string
		status  int
		body    string
		words   []string
		cut     bool
		leftOut string
	}{
		{
			name:   "a JSON body padded past a megabyte",
			status: http.StatusBadGateway,
			body:   `{"message":"Bad gateway at the check runs","documentation_url":"https://docs.github.com/rest","padding":"` + padding + `"}`,
			words:  []string{"status 502: Bad gateway at the check runs."}, leftOut: "xxxxxxxx",
		},
		{
			name:   "a megabyte that is not JSON",
			status: http.StatusServiceUnavailable,
			body:   "<html>" + padding + "</html>",
			words:  []string{"status 503."}, leftOut: "<html>",
		},
		{
			name:   "a message longer than the bound",
			status: http.StatusInternalServerError,
			body:   `{"message":"` + padding + `"}`,
			words:  []string{"status 500: xxxxxxxx"}, cut: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake.failWithBody(test.status, test.body)
			check := checkOf(t, readResults(t, handler, issue.Key), "c1")
			assertUnread(t, check, test.words...)
			runes := len([]rune(check.CI.Detail))
			if runes > 2000 || (test.cut && runes != 2000) {
				t.Fatalf("c1's detail is %d runes, want at most 2000 (exactly 2000 when cut)", runes)
			}
			if test.leftOut != "" && strings.Contains(check.CI.Detail, test.leftOut) {
				t.Fatalf("c1's detail %q carries GitHub's body", check.CI.Detail)
			}
		})
	}
}

// A rate limit carries GitHub's whole body in its error too, and none of it reaches the detail:
// GitHub's 429, which needs no header to be a limit, is named by its status and the `message`
// of its JSON body alone.
func TestCIResultDetailNamesTheStatusAndMessageOfARateLimit(t *testing.T) {
	handler, database, fake := resultsGitHubCheckRuns(t)
	issue := issueInRepository(t, handler, "RESCR", "acme/widgets", "Rate limit")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", ciCheckBlock("c1", ciCheckKept))
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", issue.Key)
	fake.failWithBody(http.StatusTooManyRequests,
		`{"message":"API rate limit exceeded for installation ID 1.","documentation_url":"https://docs.github.com/rest/rate-limit"}`)

	check := checkOf(t, readResults(t, handler, issue.Key), "c1")
	assertUnread(t, check, "(status 429)", "(status 429), next request in 1m0s: API rate limit exceeded for installation ID 1.")
	if strings.Contains(check.CI.Detail, "documentation_url") || strings.Contains(check.CI.Detail, "{") {
		t.Fatalf("c1's detail %q carries GitHub's body", check.CI.Detail)
	}
}

// A head carries every attempt of a run, and the run of the highest id decides, in whichever
// order GitHub lists them: a failure retried to a success is a pass, and a success retried to a
// failure is a fail. The first case lists the lower id first and the second the higher, so
// neither the first match nor the last answers both.
func TestCIResultIsTheRunOfTheHighestID(t *testing.T) {
	handler, database, fake := resultsGitHubCheckRuns(t)
	issue := issueInRepository(t, handler, "RESCH", "acme/widgets", "Retried")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", ciCheckBlock("c1", ciCheckKept))
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", issue.Key)

	fake.set(headCommit, completedRun(31, ciCheckRun, "failure"), completedRun(32, ciCheckRun, "success"))
	check := checkOf(t, readResults(t, handler, issue.Key), "c1")
	assertCI(t, check, ciCheckRun, model.CIStatePass, "success", ciVerdictLineOf("c1", ciCheckRun, "pass", headCommit))
	assertDerived(t, check, 32, ciCheckRun, "success", "pass", headCommit)

	fake.set(headCommit, completedRun(34, ciCheckRun, "failure"), completedRun(33, ciCheckRun, "success"))
	check = checkOf(t, readResults(t, handler, issue.Key), "c1")
	assertCI(t, check, ciCheckRun, model.CIStateFail, "failure", ciVerdictLineOf("c1", ciCheckRun, "fail", headCommit))
	assertDerived(t, check, 34, ciCheckRun, "failure", "fail", headCommit)
}

// A ci_check name written with spaces around it is either refused at the write or answered by
// the run it names without them, never stored as a name no run carries: the spaces cannot be
// seen in the editor, and docs.CarriesCICheck already trims the value, so the write path and the
// read must agree on which run the block names (dispatch://INFRA-3204, tester round 1).
func TestACICheckNamePaddedWithSpacesIsRefusedOrReadAsTheRunItNames(t *testing.T) {
	handler, database, fake := resultsGitHubCheckRuns(t)
	keys := []string{issueInRepository(t, handler, "RESCP", "acme/widgets", "Padded 0").Key}
	for index, padded := range []string{" " + ciCheckRun, ciCheckRun + " ", "\t" + ciCheckRun + " "} {
		if index > 0 {
			created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
				map[string]any{"project": "RESCP", "title": fmt.Sprintf("Padded %d", index), "spec": "A spec\n"}, "alice")
			if created.Code != http.StatusCreated {
				t.Fatalf("create issue %d: status=%d body=%s", index, created.Code, created.Body.String())
			}
			keys = append(keys, decodeBody[model.Issue](t, created).Key)
		}
		issue := model.Issue{Key: keys[index]}
		attributes := `negative_control="` + ciCheckNegativeControl + `" ci_check="` + padded + `"`
		written := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
			"name": "verification.md", "content": ciCheckBlock("c1", attributes),
		}, "alice")
		if written.Code == http.StatusBadRequest && strings.Contains(written.Body.String(), "INVALID_CI_CHECK") {
			continue
		}
		if written.Code != http.StatusCreated {
			t.Fatalf("ci_check %q: write status=%d body=%s, want 201 or 400 INVALID_CI_CHECK", padded, written.Code, written.Body.String())
		}
		head := fmt.Sprintf("%040x", 0xa0d0+index)
		writeHead(t, database, "acme/widgets", 500+index, head, "open", issue.Key)
		fake.set(head, completedRun(int64(600+index), ciCheckRun, "success"))
		check := checkOf(t, readResults(t, handler, issue.Key), "c1")
		if check.CI == nil || check.CI.State != model.CIStatePass || len(check.Results) != 1 {
			t.Fatalf("ci_check %q was stored and reads %+v with results %+v, want the pass of run %q at %s",
				padded, check.CI, check.Results, ciCheckRun, head)
		}
	}
}

// A ci_check with spaces around it names the run without them: the read asks GitHub for that run,
// and its success is a pass with its one derived result, naming the run unpadded in ci.run, the
// line and the result, while the block's own ci_check is served as written. A ci_check of only
// spaces names no run: the check is an ordinary one, with no ci member and no result, and GitHub
// is asked for nothing.
func TestCIResultReadsAPaddedCICheckAsTheRunItNames(t *testing.T) {
	handler, database, fake := resultsGitHubCheckRuns(t)
	issueInRepository(t, handler, "RESCT", "acme/widgets", "Trimmed")

	padded := issueOfProject(t, handler, "RESCT", "Padded",
		ciCheckBlock("c1", `negative_control="`+ciCheckNegativeControl+`" ci_check=" `+ciCheckRun+`"`))
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", padded)
	fake.set(headCommit, completedRun(61, ciCheckRun, "success"))
	check := checkOf(t, readResults(t, handler, padded), "c1")
	if check.CICheck != " "+ciCheckRun {
		t.Fatalf("c1 ci_check = %q, want %q as written", check.CICheck, " "+ciCheckRun)
	}
	assertCI(t, check, ciCheckRun, model.CIStatePass, "success", ciVerdictLineOf("c1", ciCheckRun, "pass", headCommit))
	assertDerived(t, check, 61, ciCheckRun, "success", "pass", headCommit)

	blank := issueOfProject(t, handler, "RESCT", "Blank",
		ciCheckBlock("c1", `negative_control="`+ciCheckNegativeControl+`" ci_check="   "`))
	writeHead(t, database, "acme/widgets", 8, newHeadCommit, "open", blank)
	fake.set(newHeadCommit, completedRun(62, ciCheckRun, "success"))
	before := fake.allReads()
	check = checkOf(t, readResults(t, handler, blank), "c1")
	if check.CICheck != "   " || check.CI != nil || len(check.Results) != 0 {
		t.Fatalf("c1 with a blank ci_check reads ci_check %q, ci %+v and results %+v, want it as written and an ordinary check with none",
			check.CICheck, check.CI, check.Results)
	}
	if asked := fake.allReads() - before; asked != 0 {
		t.Fatalf("a check with a blank ci_check read %d pages of check runs, want none", asked)
	}
}

// The open pull request's head is read over an older merged one's, and once the open one merges
// the merged pull request of the highest number is read: the answer keeps speaking after the
// merge, current at that merged head. The older merged head is never read.
func TestCIResultIsReadAtTheOpenHeadOverAnOlderMergedOne(t *testing.T) {
	handler, database, fake := resultsGitHubCheckRuns(t)
	issue := issueInRepository(t, handler, "RESCM", "acme/widgets", "Open over merged")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", ciCheckBlock("c1", ciCheckKept))
	writeHead(t, database, "acme/widgets", 3, headCommit, "merged", issue.Key)
	writeHead(t, database, "acme/widgets", 5, newHeadCommit, "open", issue.Key)
	fake.set(headCommit, completedRun(41, ciCheckRun, "failure"))
	fake.set(newHeadCommit, completedRun(42, ciCheckRun, "success"))

	check := checkOf(t, readResults(t, handler, issue.Key), "c1")
	assertDerived(t, check, 42, ciCheckRun, "success", "pass", newHeadCommit)

	writeHead(t, database, "acme/widgets", 5, newHeadCommit, "merged", issue.Key)
	check = checkOf(t, readResults(t, handler, issue.Key), "c1")
	result := assertDerived(t, check, 42, ciCheckRun, "success", "pass", newHeadCommit)
	if basis := result.CommitStands.Basis; basis == nil || basis.PullRequest == nil || *basis.PullRequest != 5 {
		t.Fatalf("the derived result at the merged head stands at %+v, want #5's head", basis)
	}
	if check.PassStands.Answer != model.PassStandsYes {
		t.Fatalf("c1 pass_stands = %+v at the merged head, want yes", check.PassStands)
	}
	if older := fake.reads(headCommit); older != 0 {
		t.Fatalf("the older merged head's check runs were read %d times, want none", older)
	}
}

// One answer reads one head's check runs once, however many checks CI proves at it, and an issue
// holding no check CI proves asks GitHub for none at all.
func TestCIResultReadsOneHeadOnceAndAnIssueWithoutOneNotAtAll(t *testing.T) {
	handler, database, fake := resultsGitHubCheckRuns(t)
	issue := issueInRepository(t, handler, "RESC3", "acme/widgets", "Three checks")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		ciCheckBlock("c1", ciCheckKept)+"\n"+
			ciCheckBlock("c2", `negative_control="`+ciCheckNegativeControl+`" ci_check="lint"`)+"\n"+
			ciCheckBlock("c3", `negative_control="`+ciCheckNegativeControl+`" ci_check="build"`))
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", issue.Key)
	fake.set(headCommit, completedRun(51, ciCheckRun, "success"), completedRun(52, "lint", "failure"))

	read := readResults(t, handler, issue.Key)
	assertDerived(t, checkOf(t, read, "c1"), 51, ciCheckRun, "success", "pass", headCommit)
	assertDerived(t, checkOf(t, read, "c2"), 52, "lint", "failure", "fail", headCommit)
	assertCI(t, checkOf(t, read, "c3"), "build", model.CIStateNoResult, "absent", ciUnconcludedLineOf("c3", "build", "absent"))
	if reads := fake.reads(headCommit); reads != 1 {
		t.Fatalf("one answer read the head's check runs %d times, want once for three checks", reads)
	}

	ordinary := issueOfProject(t, handler, "RESC3", "No check CI proves", twoChecks)
	writeHead(t, database, "acme/widgets", 8, newHeadCommit, "open", ordinary)
	before := fake.allReads()
	if check := checkOf(t, readResults(t, handler, ordinary), "k1"); check.CI != nil {
		t.Fatalf("an ordinary check carries ci %+v, want none", check.CI)
	}
	if asked := fake.allReads() - before; asked != 0 {
		t.Fatalf("an issue holding no check CI proves read %d pages of check runs, want none", asked)
	}
}

// The run's output title and summary are carried together cut to 2,000 runes, counted as runes
// and not bytes, between the conclusion and the run's URL, and the answer is not refused.
func TestCIResultCutsTheRunsOutputToTwoThousandRunes(t *testing.T) {
	handler, database, fake := resultsGitHubCheckRuns(t)
	issue := issueInRepository(t, handler, "RESCL", "acme/widgets", "Long summary")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", ciCheckBlock("c1", ciCheckKept))
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", issue.Key)
	run := completedRun(61, ciCheckRun, "success")
	summary := strings.Repeat("é", 2500)
	run["output"] = map[string]any{"title": "Tests", "summary": summary}
	fake.set(headCommit, run)

	result := checkOf(t, readResults(t, handler, issue.Key), "c1").Results
	if len(result) != 1 {
		t.Fatalf("c1 holds %+v, want its derived result", result)
	}
	output := string([]rune("Tests\n" + summary)[:2000])
	if want := "conclusion: success\n" + output + "\n" + ciRunURL(61); result[0].Observed != want {
		t.Fatalf("observed %d runes, want the conclusion, the output cut to 2,000 runes and the URL (%d runes)",
			len([]rune(result[0].Observed)), len([]rune(want)))
	}
}

// An issue Dispatch holds no open or merged pull request head of has no derived result: the check
// reads no_head, and GitHub is asked nothing. So does an issue whose one pull request closed
// without merging, whose head is no head.
func TestCIResultIsNoHeadForAnIssueWithNoPullRequest(t *testing.T) {
	handler, database, fake := resultsGitHubCheckRuns(t)
	issue := issueInRepository(t, handler, "RESCN", "acme/widgets", "No pull request")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", ciCheckBlock("c1", ciCheckKept))
	fake.set(headCommit, completedRun(81, ciCheckRun, "success"))
	assertNoHead := func(what string) {
		t.Helper()
		check := checkOf(t, readResults(t, handler, issue.Key), "c1")
		assertCI(t, check, ciCheckRun, model.CIStateNoResult, "no_head", ciUnconcludedLineOf("c1", ciCheckRun, "no_head"))
		if len(check.Results) != 0 || check.PassStands.Answer == model.PassStandsYes {
			t.Fatalf("%s: c1 holds %+v and pass_stands %+v, want no result and no pass", what, check.Results, check.PassStands)
		}
		if asked := fake.allReads(); asked != 0 {
			t.Fatalf("%s: the read asked for %d pages of check runs, want none", what, asked)
		}
	}

	assertNoHead("an issue with no pull request row")
	writeHead(t, database, "acme/widgets", 7, headCommit, "closed", issue.Key)
	assertNoHead("an issue whose pull request closed unmerged")
}

// The derived result is judged as a recorded result is: current at the head it was read at, and
// at another commit for a reader who names the head GitHub moved to while Dispatch still holds
// the old one. Once Dispatch holds the new head, the run at that head decides, and a head no run
// of that name has reached yet holds no result.
func TestCIResultIsCurrentAtTheHeadAndOlderOnceTheHeadMoves(t *testing.T) {
	handler, database, fake := resultsGitHubCheckRuns(t)
	issue := issueInRepository(t, handler, "RESCC", "acme/widgets", "Currency")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", ciCheckBlock("c1", ciCheckKept))
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", issue.Key)
	fake.set(headCommit, completedRun(71, ciCheckRun, "success"))

	read := readResults(t, handler, issue.Key)
	stands := standsAt(t, read, "c1", "ci-run:71", "the derived result at the head", model.CommitAtBasis, false)
	if stands.Basis == nil || stands.Basis.Commit != headCommit {
		t.Fatalf("the derived result stands at %+v, want the head %s", stands.Basis, headCommit)
	}
	if passed := checkOf(t, read, "c1").PassStands; passed.Answer != model.PassStandsYes || !slices.Equal(passed.Roles, []string{"ci"}) {
		t.Fatalf("c1 pass_stands = %+v, want yes from ci", passed)
	}

	moved := readResultsAt(t, handler, issue.Key, newHeadCommit)
	standsAt(t, moved, "c1", "ci-run:71", "the derived result read at the head GitHub moved to", model.CommitOtherCommit, false)
	if passed := checkOf(t, moved, "c1").PassStands; passed.Answer != model.PassStandsNo {
		t.Fatalf("c1 pass_stands = %+v at the moved head, want no", passed)
	}

	writeHead(t, database, "acme/widgets", 7, newHeadCommit, "open", issue.Key)
	check := checkOf(t, readResults(t, handler, issue.Key), "c1")
	assertCI(t, check, ciCheckRun, model.CIStateNoResult, "absent", ciUnconcludedLineOf("c1", ciCheckRun, "absent"))
	if len(check.Results) != 0 {
		t.Fatalf("c1 holds %+v at a head no run has reached, want no result", check.Results)
	}
}
