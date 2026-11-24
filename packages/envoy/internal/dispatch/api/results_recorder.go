package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// results_recorder.go says who may record a result of a check on POST
// /api/v1/issues/{key}/results, and what each recorder's result is: a role, whose body names what
// it ran, or the CI job a check's ci_check names, whose results token Dispatch reads its identity
// from (refusalFor, refuseResultOfAnotherRecorder). It holds the refusals each recorder's body and
// credential earn, the check run a job's token was minted in, and the row each recorder's result
// becomes (roleCheckResult, ciJobCheckResult). The route itself is results.go's, and the tables the
// rows are written to and read from are results_rows.go's.

// codeRefNotABranch refuses a results token whose run is of a ref that names no branch: a CI
// job records a result for the push of a branch, and its row names that branch.
const codeRefNotABranch = "REF_NOT_A_BRANCH"

// codeRepositoryNotTheIssues refuses a results token whose run is of another repository than the
// one the check's issue resolves to.
const codeRepositoryNotTheIssues = "REPOSITORY_NOT_THE_ISSUES"

// codeBranchNotTheIssues refuses a CI job's result on an issue whose change the branch its run
// pushed is not (refuseBranchOfAnotherIssue).
const codeBranchNotTheIssues = "BRANCH_NOT_THE_ISSUES"

// branchRefPrefix is what the ref of a branch push begins with.
const branchRefPrefix = "refs/heads/"

// ciJobResultInput is every member a CI job's results token may send on POST
// /api/v1/issues/{key}/results: what its run observed, and nothing about who the job is, which
// Dispatch reads from the token and from GitHub. refusalFor names each other member a job's
// body carries.
type ciJobResultInput struct {
	Check    string `json:"check"`
	Commit   string `json:"commit"`
	Verdict  string `json:"verdict"`
	Observed string `json:"observed"`
}

// refusalFor is every refusal a result's own fields earn before anything is read, for the
// credential that sends them and body, the bytes they were decoded from. A CI job's results
// token (model.Credential.ResultsOnly) is held to its run (ciJobRefusal); every other credential
// records a role's result, held to the rules below. The three counts are each a non-negative
// number check_results keeps in an integer column, so a count past that column
// (fitsIntegerColumn) is refused here, before the query that would fail to encode it.
func (input checkResultInput) refusalFor(credential *model.Credential, body []byte) error {
	if credential != nil && credential.ResultsOnly {
		return input.ciJobRefusal(credential.CI, body)
	}
	if strings.TrimSpace(input.Check) == "" {
		return errorf(http.StatusBadRequest, "RESULT_INPUT", "check names the check block the result proves")
	}
	if !checkResultRole.MatchString(input.Role) {
		return errorf(http.StatusBadRequest, "RESULT_INPUT",
			"role is required and is at most 32 characters of a-z and -, the word the pull-request line prints")
	}
	if input.Verdict != "pass" && input.Verdict != "fail" {
		return errorf(http.StatusBadRequest, "RESULT_INPUT", "verdict is pass or fail")
	}
	if !resultCommitSHA.MatchString(input.Commit) {
		return errorf(http.StatusBadRequest, "RESULT_INPUT", "commit is the 40-character lowercase hex commit the check ran at")
	}
	for _, count := range []struct {
		name  string
		cases int
	}{{"failed", input.Failed}, {"skipped", input.Skipped}, {"excluded", input.Excluded}} {
		if count.cases < 0 || !fitsIntegerColumn(count.cases) {
			return errorf(http.StatusBadRequest, "RESULT_INPUT", "%s counts cases, from 0 to %d", count.name, maxIntegerColumn)
		}
	}
	if input.Verdict == "pass" && (input.Failed > 0 || input.Skipped > 0 || input.Excluded > 0) {
		return errorf(http.StatusBadRequest, "RESULT_CONTRADICTS",
			"a pass counts no failed, skipped or excluded case; this one counts %d failed, %d skipped and %d excluded",
			input.Failed, input.Skipped, input.Excluded)
	}
	return nil
}

// ciJobRefusal is every refusal the body of a CI job's result earns before anything is read, run
// being the run its results token was minted for. The body names what the run observed and
// nothing about who the job is, which Dispatch reads from the token and from GitHub, so a member
// beyond ciJobResultInput's four is refused by name rather than ignored; a body that names one
// is a caller that thinks it sets it. The commit is the run's, and the run is of a branch push,
// whose branch its row names (ciJobCheckResult).
func (input checkResultInput) ciJobRefusal(run *oauthwire.CIRun, body []byte) error {
	if undeclared := undeclaredMembers(&ciJobResultInput{}, body); len(undeclared) > 0 {
		return errorf(http.StatusBadRequest, codeResultInput,
			"a CI job's result takes check, commit, verdict and observed alone, and names %s: Dispatch reads who recorded a result from its token, never from its body",
			strings.Join(undeclared, ", "))
	}
	if strings.TrimSpace(input.Check) == "" {
		return errorf(http.StatusBadRequest, codeResultInput, "check names the check block the result proves")
	}
	if input.Verdict != "pass" && input.Verdict != "fail" {
		return errorf(http.StatusBadRequest, codeResultInput, "verdict is pass or fail")
	}
	if !resultCommitSHA.MatchString(input.Commit) {
		return errorf(http.StatusBadRequest, codeResultInput, "commit is the 40-character lowercase hex commit the job tested")
	}
	if run == nil {
		// brokerTokenActor carries the run of every results token it accepts.
		return errors.New("a results token's credential carries no run")
	}
	if input.Commit != run.SHA {
		return commitNotTheRuns(input.Commit, run.SHA)
	}
	if pushedBranch(*run) == "" {
		return errorf(http.StatusForbidden, codeRefNotABranch,
			"this token's run is of %q, which names no branch: a CI job records a result only for the push of a branch",
			run.Ref)
	}
	return nil
}

// pushedBranch is the branch whose push run ran on: its ref without refs/heads/, and "" for a ref
// that names no branch.
func pushedBranch(run oauthwire.CIRun) string {
	branch, ok := strings.CutPrefix(run.Ref, branchRefPrefix)
	if !ok {
		return ""
	}
	return branch
}

// refuseResultOfAnotherRecorder refuses a result whose recorder is not the one that does the work
// of its check (R12: nobody writes a result for work that it did not do), in this order:
//
//  1. A check CI proves (docs.CICheckRule answers "") takes no result from any credential but
//     a CI job's results token: 409 CHECK_PROVED_BY_CI, naming the block and the job.
//  2. A results token records only the result of a check CI proves whose ci_check, trimmed, is
//     the name of the token's check run: 409 CHECK_NOT_THIS_JOBS, naming the check, its
//     ci_check (or that it carries none) and the check run.
//  3. The token's check run is GitHub's at the token's commit (403 CHECK_RUN_NOT_THE_RUNS), and
//     the token's repository is the one the check's issue resolves to, compared without case
//     (server.checkIssueRepository; 403 REPOSITORY_NOT_THE_ISSUES), each naming both. refusalFor
//     has held the body's commit to the token's already.
//
// Any other credential's result for any other check is judged by the rest of the route. Whether
// the check is CI's is its block's alone (validCICheck), and q reads the check's repository: the
// pool before the transaction opens, and the transaction itself under the locks. run is the
// token's check run (ciJobCheckRun), and nil for every other credential.
func (s *server) refuseResultOfAnotherRecorder(ctx context.Context, q queryer, check resolvedCheck, credential *model.Credential, run *githubapp.CheckRun) error {
	carries := docs.CarriesCICheck(check.Attributes)
	rule := ""
	if carries {
		rule = docs.CICheckRule(check.Attributes)
	}
	provedByCI := carries && rule == ""
	if credential == nil || !credential.ResultsOnly {
		if provedByCI {
			return errorf(http.StatusConflict, "CHECK_PROVED_BY_CI",
				"check %s names CI job %q: only a run of that job records its result, through the secrets broker",
				check.BlockID, check.Attributes.CICheck)
		}
		return nil
	}
	if run == nil || credential.CI == nil {
		return fmt.Errorf("judge a CI job's result for check %s: the token's check run was not read", check.BlockID)
	}
	if !provedByCI || check.Attributes.CICheck != run.Name {
		named := "carries no ci_check"
		switch {
		case carries && !provedByCI:
			named = fmt.Sprintf("names CI job %q and is no check CI proves (%s)", check.Attributes.CICheck, rule)
		case carries:
			named = fmt.Sprintf("names CI job %q", check.Attributes.CICheck)
		}
		return errorf(http.StatusConflict, codeCheckNotThisJobs,
			"check %s %s, and this token's check run is %q: a job records results only for the checks whose ci_check names its check run",
			check.BlockID, named, run.Name)
	}
	if run.HeadSHA != credential.CI.SHA {
		return checkRunNotTheRuns(run.HeadSHA, run.ID, credential.CI.SHA)
	}
	repo, answered := s.checkIssueRepository(ctx, q, check.IssueKey)
	if !answered {
		return fmt.Errorf("judge a CI job's result for check %s: the repository of %s did not resolve", check.BlockID, check.IssueKey)
	}
	if repo == nil || !strings.EqualFold(*repo, credential.CI.Repository) {
		resolves := "no repository"
		if repo != nil {
			resolves = *repo
		}
		return errorf(http.StatusForbidden, codeRepositoryNotTheIssues,
			"this token's run is of %s, and issue %s resolves to %s: a CI job records a result only for an issue of the repository it ran in",
			credential.CI.Repository, check.IssueKey, resolves)
	}
	return nil
}

// refuseBranchOfAnotherIssue refuses a CI job's result on issueKey's route when the branch whose
// push its run ran on does not belong to that issue (delivery.BranchOfIssue): a job's result
// counts for an issue only on a branch of that issue's change, so a job of one issue's branch
// never makes a pass stand for another's (403 BRANCH_NOT_THE_ISSUES). The branch is quoted, so
// a name holding a line break prints on one line.
func refuseBranchOfAnotherIssue(ctx context.Context, q refs.Queryer, issueKey, branch string) error {
	belongs, err := delivery.BranchOfIssue(ctx, q, issueKey, branch)
	if err != nil {
		return err
	}
	if !belongs {
		return errorf(http.StatusForbidden, codeBranchNotTheIssues,
			"this token's run pushed branch %q, which is no branch of issue %s: a branch belongs to an issue when it is the head branch of a pull request of that issue, or when the first issue key its name holds is that issue's, as Dispatch attributes a pull request by its branch",
			branch, issueKey)
	}
	return nil
}

// ciJobCheckRun is the check run a results token was minted in, as GitHub names it in the run's
// repository, through the cache the project's results route reads it through (specCheckRun). A
// GitHub that does not answer leaves the result unjudged: 503 CHECK_RUN_UNREADABLE, and the job
// writes it again.
func (s *server) ciJobCheckRun(ctx context.Context, ci oauthwire.CIRun) (githubapp.CheckRun, error) {
	id, err := strconv.ParseInt(ci.CheckRunID, 10, 64)
	if err != nil {
		// The verifier admits no other (ciphertoken.shapeOf), so this is Dispatch's own fault.
		return githubapp.CheckRun{}, fmt.Errorf("read the check run of a results token: %w", err)
	}
	owner, name, _ := strings.Cut(ci.Repository, "/")
	run, err := s.specCheckRun(ctx, &repositoryReader{s: s, owner: owner, name: name}, ci.Repository, id)
	if err != nil {
		return githubapp.CheckRun{}, errorf(http.StatusServiceUnavailable, codeCheckRunUnreadable,
			"GitHub did not answer the read of check run %d of %s, so the result was not judged; write it again: %v",
			id, ci.Repository, err)
	}
	return run, nil
}

// roleCheckResult is the row a role's result is stored as: each field as the role recorded it,
// under the actor the request resolved to.
func roleCheckResult(input checkResultInput, check resolvedCheck, actor model.Actor, issueKey string) model.CheckResult {
	return model.CheckResult{
		ID: uuid.NewString(), IssueKey: issueKey, CheckIssueKey: check.IssueKey,
		CheckArtifactID: check.ArtifactID, CheckBlockID: check.BlockID,
		CheckVersion: check.Version, CheckSHA256: check.SHA256,
		Role: input.Role, Surface: input.Surface, Run: input.Run, Observed: input.Observed,
		CommitSHA: input.Commit, NegativeControl: input.NegativeControl,
		Failed: input.Failed, Skipped: input.Skipped, Excluded: input.Excluded,
		Verdict: input.Verdict, Screenshots: nonNilStrings(input.Screenshots),
		Session: sessionOf(actor), Actor: actor,
	}
}

// ciJobActor is the actor a CI job's result and its event are recorded under: the session of the
// run its results token proved, and no person. The person the broker's result entry names ran
// nothing, so it stays inside the server, as on the project's results route
// (model.ResultRecorder): the airlock authenticated the run (R12).
func ciJobActor(actor model.Actor) model.Actor {
	actor.Owner = nil
	return actor
}

// ciJobCheckResult is the row of ci_check_results a CI job's result is stored as
// (insertCIJobCheckResult): what its run observed, under the identity its token proved and
// nothing its body named. Its role is ci and its run the check run's name, as on the result
// derived from that run (ciAnswerOf); what it observed is kept by the rule of the project's
// results route (cutObserved); its surface and negative control are the check's; its commit is
// the run's, which ciJobRefusal held the body's to; its session and actor are the run's
// (ciJobActor); and Branch is the branch whose push the run ran on. Attestation is the in-toto
// statement of its run (attestedResult.statement), as the project's results route builds one:
// attempt is the run's attempt (runNumbersOf), and principal the broker's principal of the run
// (resultPrincipal).
func ciJobCheckResult(input checkResultInput, check resolvedCheck, actor model.Actor, run githubapp.CheckRun, ci oauthwire.CIRun, attempt int, principal string, issueKey string) model.CheckResult {
	session, observed := sessionOf(actor), cutObserved(input.Observed)
	statement := attestedResult{
		predicateType: checkBlockResultPredicateType, project: *actor.Project, check: check.Ref,
		procedureDigest: check.SHA256,
		test:            model.ResultTest{Job: check.Attributes.CICheck},
		verdict:         input.Verdict, observed: observed, checkRunName: run.Name,
		run: ci, attempt: attempt, session: session, principal: principal,
	}.statement()
	return model.CheckResult{
		ID: uuid.NewString(), IssueKey: issueKey, CheckIssueKey: check.IssueKey,
		CheckArtifactID: check.ArtifactID, CheckBlockID: check.BlockID,
		CheckVersion: check.Version, CheckSHA256: check.SHA256,
		Role: ciResultRole, Surface: check.Surface, Run: run.Name, Observed: observed,
		CommitSHA: ci.SHA, Branch: pushedBranch(ci), NegativeControl: check.Attributes.NegativeControl,
		Verdict: input.Verdict, Screenshots: []string{},
		Session: session, Actor: actor, Attestation: &statement,
	}
}
