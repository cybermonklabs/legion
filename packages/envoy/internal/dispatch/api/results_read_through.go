package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// readThroughHead teaches Dispatch the pull requests GitHub names for a result's commit
// (delivery.ReadThroughCommitPulls). A row written at another head can be the basis of another
// result of the same read, and a row of a pull request owed a read settles that read (R5), so
// the bases are read after learn (answer).
//
// It reads a commit that an open or merged pull request Dispatch holds already heads, whatever
// issue that row is attributed to: GitHub can name a second pull request at the same head,
// whose own event never arrived, and the read stores its row (R6 of the results lookup). That is
// a child's result on its parent's tab too, where the row is the child's own pull request.
//
// listCheckResults places every result of every check, and the Verification tab reads them all
// again on every result.recorded, so the reads are bounded by the repository and the commit,
// never by the result that is being answered:
//
//   - One commit is read through at most once every delivery.CommitPullReadStaleAfter, however
//     many reads ask and however many processes serve them:
//     server.commitPulls.ClaimAcrossProcesses tests that bound and records the read together,
//     first in this process's memory and then in the claim every process shares
//     (commit_pull_reads), so two reads that ask at once spend one call. A commit that a stored
//     row heads costs that one call in each span while its results are read.
//   - Or a read within the last delivery.CommitPullSettledFor has asked GitHub about that
//     repository and commit and been told of a pull request whose head is another commit
//     (delivery.CommitPullReads.SettleAcrossProcesses, which that read records and the claim
//     refuses from). GitHub has answered about this commit for now — its answer put the pull
//     request's head past it —
//     so the answer is believed for that span and nothing a read inside it can learn repairs the
//     result. It is a span and not for ever because a pull request opened later can have its
//     head at that commit, which the next read after the span sees. GitHub naming no pull
//     request at all is settled for no span at all, since one may open at the commit a moment
//     later.
//
// Two tests cost nothing and come first. A deployment holding no App key can ask nothing.
// A repository Dispatch cannot see is the unknown answer whatever GitHub would say, and the
// read of its default branch is the test (readThroughRepository), made once per repository per
// read of the results (defaultBranchIn's memo): for the issue's own repository it is the test
// commitStands makes next, so it decides here first and costs nothing twice. It also answers
// false for an issue resolving to no repository and for a resolve that did not answer, which is
// why the repository read there has one.
//
// The read is against the repository the result's commit is taken to be of (commitRepositoryOf):
// a linked repository the result's row names, under its canonical name, and else the canonical
// repository the check's issue resolves to (repositoryOf), the same issue headBases asks the
// stored rows about. Asked of the issue's own repository, a commit only a linked repository
// holds answers 422 on every read, and a linked pull request whose head reaches that commit
// after the record would never be learned. The readable test is that repository's own. A read
// that fails leaves the bases exactly as the stored rows say, which is what a result reads with
// no read-through at all: a GitHub call that did not land is no evidence either way.
func (c *comparisonReader) readThroughHead(ctx context.Context, result model.CheckResult) {
	if c.server.deps.GitHub == nil {
		return
	}
	repo, readable := c.readThroughRepository(ctx, result)
	if !readable {
		return
	}
	now := time.Now()
	claimed, err := c.server.commitPulls.ClaimAcrossProcesses(ctx, c.server.deps.Store.Pool, repo, result.CommitSHA, now)
	if err != nil {
		slog.Info("dispatch: the claim of a read of a result's commit was not taken, so the read asks GitHub nothing",
			"repo", repo, "commit", result.CommitSHA, "error", err)
		return
	}
	if !claimed {
		return
	}
	written, atHead, err := delivery.ReadThroughCommitPulls(
		ctx, c.server.deps.Store.Pool, c.server.deps.GitHub, repo, result.CommitSHA, now)
	if err != nil {
		slog.Info("dispatch: GitHub named no pull request for a result's commit",
			"repo", repo, "commit", result.CommitSHA, "error", err)
		if errors.Is(err, delivery.ErrReadThroughNotStored) {
			// Nothing was stored, so the claim protects no GitHub answer: give it back, so the
			// next read asks again at once rather than CommitPullReadStaleAfter later.
			if err := c.server.commitPulls.ReleaseAcrossProcesses(ctx, c.server.deps.Store.Pool, repo, result.CommitSHA, now); err != nil {
				slog.Info("dispatch: the claim of a read that stored nothing was not given back, so the next read waits it out",
					"repo", repo, "commit", result.CommitSHA, "error", err)
			}
		}
		return
	}
	if written > 0 && !atHead {
		if err := c.server.commitPulls.SettleAcrossProcesses(ctx, c.server.deps.Store.Pool, repo, result.CommitSHA, now); err != nil {
			slog.Info("dispatch: the settle of a read of a result's commit was not recorded, so a later read asks GitHub again",
				"repo", repo, "commit", result.CommitSHA, "error", err)
		}
	}
}

// readThroughRepository is the repository readThroughHead reads a result's commit through, and
// whether that read can be made at all, by whose the commit is taken to be (commitRepositoryOf):
// a linked repository, where the read of its canonical name answered and the App can read it
// (commitRepository.readable, the one answer commitStands decides a `before_merge` result by as
// well); and else the repository the check's issue resolves to, where the App answered that
// repository's default branch, which also answers false for an issue resolving to no repository
// and for a resolve that did not answer, the memo commitStands reads next.
func (c *comparisonReader) readThroughRepository(ctx context.Context, result model.CheckResult) (string, bool) {
	switch owner := c.commitRepositoryOf(ctx, result.CheckIssueKey, result.CommitRepo); {
	case owner.unread:
		return "", false
	case owner.linked != "":
		return owner.linked, owner.readable
	}
	if _, readable := c.defaultBranchOf(ctx, result.CheckIssueKey); !readable {
		return "", false
	}
	repo, _ := c.repositoryOf(ctx, result.CheckIssueKey)
	return *repo, true
}
