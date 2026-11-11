package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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
	handler, database, _ := resultsGitHubServed(t, repos, held, defaultBranch, answer)
	return handler, database
}

// resultsGitHubServed is resultsGitHubAnswering answering also the server its handler mounts,
// for the test that must move what that server holds of its own: the default branch each
// repository was last resolved to, which delivery.Branches keeps for
// delivery.BranchCommitStaleAfter and which a test can reach no other way, since it cannot
// wait five minutes out. readThroughGitHub answers the same for the read-through's cache.
func resultsGitHubServed(
	t *testing.T,
	repos, held map[string]bool,
	defaultBranch string,
	answer func(base, head string) (int, string),
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
	// Recorded as production records a merge, through the writer that seeds the one fact the
	// merge itself proves: the base branch held the merge commit.
	attributed := issue.Key
	if err := delivery.UpsertPullRequestHead(context.Background(), database.Pool, nil, delivery.PullRequestHead{
		Repo: "acme/widgets", Number: 48, HeadSHA: headCommit, HeadRef: "legion/work",
		BaseRef: "integration/x", State: "merged", IssueKey: &attributed, AttributionSource: "title",
		AttributionComplete: true, Title: "A change", URL: "https://github.com/acme/widgets/pull/48",
		MergeCommitSHA: baseMergeCommit,
	}); err != nil {
		t.Fatalf("record the merge of the issue's pull request: %v", err)
	}

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
		})
	issue := issueInRepository(t, handler, "RESD3", "acme/widgets", "Merged, then the App lost the repository")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	// Recorded as production records a merge, through the writer that seeds the one fact the
	// merge itself proves: the base branch held the merge commit.
	attributed := issue.Key
	if err := delivery.UpsertPullRequestHead(context.Background(), database.Pool, nil, delivery.PullRequestHead{
		Repo: "acme/widgets", Number: 51, HeadSHA: headCommit, HeadRef: "legion/work",
		BaseRef: "integration/x", State: "merged", IssueKey: &attributed, AttributionSource: "title",
		AttributionComplete: true, Title: "A change", URL: "https://github.com/acme/widgets/pull/51",
		MergeCommitSHA: baseMergeCommit,
	}); err != nil {
		t.Fatalf("record the merge of the issue's pull request: %v", err)
	}

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
	unplaced := standsAt(t, read, "k2", stray.Result.ID,
		"an after-merge result at a commit nothing is stored for, the App having lost the repository",
		model.CommitUnknown, false)
	if unplaced.Reason != model.ReasonReadFailed {
		t.Fatalf("that result is unknown for %q, want %q", unplaced.Reason, model.ReasonReadFailed)
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

// The three ways a check comes to compare with nothing, and the two answers they part into. An
// issue that resolves to no repository and a `before_merge` check whose issue holds no pull
// request have no pull request and no branch to compare with, which is a fact about the work:
// no pass of either stands. A repository the App cannot read is the other answer, and leaves
// the gate's question unanswered rather than answered no.
//
// The two were one answer before dispatch://INFRA-1522, and a reviewer could not tell a check
// nothing proves from one Dispatch could not read.
func TestACheckComparingWithNothingNamesWhichKindOfNothing(t *testing.T) {
	handler, _ := resultsGitHub(t, map[string]bool{"acme/widgets": true},
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
