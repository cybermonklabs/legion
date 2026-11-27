package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// implementationPullRequest is the implementer's completion's read of GitHub: in implementing, a
// completion moves its issue to its task's next step (testing, in the default steps) only on a
// pull request recorded open or merged for it (workflow's TriggerImplementationReady), and only
// the opened webhook, or the daemon's per-issue read standing in for one that webhook lost
// (readIssuePullRequest), records one open. So before
// the completion is applied, the open pull request on the issue's branch (claim.IssueBranch) is
// read as the implement App, minted for the owner of the issue's repository in GitHub's spelling
// (record.Issue.GitHubRepo, which the webhook's facts name it by), and it says whether the
// completion goes on:
//
//   - one open: when the record does not already hold it open, the intake.PullRequestOpened its
//     webhook would have carried is applied first, a reopen when the record holds the same pull
//     request closed, under the read's stable id (intake.OpenedRead). The completion then
//     moves the issue to its task's next step. A failure to apply it is FACT_APPLY_FAILED, with
//     the completion not applied.
//   - none, with the record holding the pull request merged: the completion is applied and moves
//     the issue to its task's next step. The work landed, its merge told the architect
//     (pr-merged), and the workflow runs on.
//   - none otherwise: HANDOFF_NO_PULL_REQUEST, 409, and nothing is applied, so the completion at
//     the same commit is applied once the pull request is open. When the record holds the pull
//     request closed without merging, the refusal names it: the close told the architect, which
//     decides the phase.
//   - unreadable (a repository the record cannot name, a failed token mint or a failed read,
//     GitHub's rate limit among them): logged WARN, and the completion is applied as it was before
//     this read, so GitHub's trouble never blocks it; the per-issue read records the pull request
//     when it finds one.
//
// A completion the workflow refuses whatever GitHub shows reads nothing: the route calls this only
// for one that is not already applied (eventID, its key, read with intake.Applied), whose tree does
// not linger and whose handoff record is not the one the implementer named for its previous phase
// (workflow's HANDOFF_NOT_NEW). It returns false when it wrote the answer.
func (s *server) implementationPullRequest(w http.ResponseWriter, r *http.Request, issue *record.Issue, recorded *record.PullRequest, eventID string) bool {
	ctx := r.Context()
	applied, err := intake.Applied(ctx, s.pool, "api", []string{eventID})
	if err != nil {
		s.logRequestFailure(r, err, "api: read whether a completion is already applied", "issue", issue.Key)
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read whether this completion was already received")
		return false
	}
	if applied[eventID] {
		return true
	}
	if s.tokens == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "GITHUB_TOKEN_SOURCE_UNAVAILABLE", "GitHub token source is unavailable")
		return false
	}
	branch := claim.IssueBranch(issue.Key)
	logUnread := func(err error) {
		attributes := []any{"issue", issue.Key, "repo", issue.GitHubRepo, "branch", branch, "error", err}
		var answer *githubrest.Answer
		if errors.As(err, &answer) {
			attributes = append(attributes, "status", answer.Status)
		}
		s.log.Warn("api: read the open pull request on a completing implementer's branch; the completion is applied unread, and the per-issue read records the pull request it finds", attributes...)
	}
	recordedRepo, err := ghrepo.Parse("the GitHub repository of "+issue.Key, issue.GitHubRepo)
	if err != nil {
		logUnread(err)
		return true
	}
	// This read is GitHub's own, so it is minted for and addressed at the repository GitHub holds
	// the recorded one under now (ghrepo.Renames.Serving, whose doc says why both halves are
	// taken): the token is the half the old owner cannot supply, since after a transfer the
	// installation covering the repository is the owner it moved to's. The log line above and the
	// fact applied below go on naming issue.GitHubRepo, the name the record is held under.
	repository := s.serving(recordedRepo)
	lease, err := s.tokens.Token(ctx, appauth.Implement, repository.Owner())
	if err != nil {
		logUnread(fmt.Errorf("mint the implement App token for %s: %w", repository, err))
		return true
	}
	github := githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(s.githubAPI, repository)}
	// The owner this asks under is the resolved repository's, and it has to be: the read is the
	// `head=<owner>:<branch>` query, and GitHub translates no part of it across a transfer, so a
	// query naming the owner a record was written under answers an empty list on the repository
	// that moved rather than the pull request on it. That empty list is the production refusal
	// this rule was changed for: every implementer handoff of a tree opened before the transfer
	// was refused HANDOFF_NO_PULL_REQUEST below.
	pull, found, err := github.OpenPullRequestOn(ctx, repository.Owner(), branch)
	if err != nil {
		logUnread(err)
		return true
	}
	if !found {
		// This read is GitHub's own answer on an open pull request, so of the states that let the
		// implementation move on (record.LetsImplementationMove) only merged still does here: a
		// record holding the pull request open is one GitHub closed or merged since the daemon last
		// heard, and the completion waits for whichever of the two the daemon records next.
		if record.LetsImplementationMove(recorded) && recorded.State != record.PullRequestOpen {
			return true
		}
		message := fmt.Sprintf("no open pull request on %s %s; nothing was recorded. Open the pull request and complete again.", issue.GitHubRepo, branch)
		if recorded != nil && recorded.State == record.PullRequestClosed {
			message += fmt.Sprintf(" pull request #%d is closed; your architect was told and decides this issue's phase.", recorded.Number)
		}
		s.writeFailure(w, r, http.StatusConflict, "HANDOFF_NO_PULL_REQUEST", message)
		return false
	}
	if recorded != nil && recorded.State == record.PullRequestOpen && recorded.Number == pull.Number {
		return true
	}
	fact, read := intake.OpenedRead(issue.Project, issue.GitHubRepo, branch, pull, issue.Generation, recorded)
	if _, err := s.apply(ctx, "github", read, fact); err != nil {
		s.logRequestFailure(r, err, "api: apply the open pull request a completing implementer's branch shows", "issue", issue.Key, "pullRequest", pull.Number)
		s.writeFailure(w, r, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply the open pull request read from GitHub; this completion changed nothing")
		return false
	}
	return true
}
