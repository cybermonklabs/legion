package api

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// readThroughHeads teaches Dispatch the pull requests GitHub names for the commits of one
// repository that a read's results stand at, in one question
// (delivery.Attributor.ReadThroughCommitsPulls). A row written at another head can be the basis of
// another result of the same read, and a row of a pull request owed a read settles that read (R5),
// so the bases are read after learn (answer).
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
//
// A read-only caller's read makes the same read and stores nothing, takes no claim and settles
// nothing (delivery.Attributor.AnswerCommitsPulls).
func (c *comparisonReader) readThroughHeads(ctx context.Context, repo string, results []model.CheckResult) {
	if c.server.deps.GitHub == nil {
		return
	}
	now := c.server.resultsNow()
	var asking []string
	// One commit is asked about once, however many results of however many checks stand at it: a
	// writer's second claim of a commit is refused by the claim its first took, and a read-only
	// caller takes no claim, so only this keeps its query from naming the commit twice.
	naming := map[string]struct{}{}
	for _, result := range results {
		if _, named := naming[result.CommitSHA]; named {
			continue
		}
		naming[result.CommitSHA] = struct{}{}
		if c.readOnly {
			// A read-only caller takes no claim: it asks nothing where a writer's claim or settle
			// stands, as a writer's read whose claim is refused asks nothing, and a read of that
			// claim that fails asks nothing either, as a writer's claim that fails grants nothing.
			refused, err := c.server.commitPulls.RefusesAcrossProcesses(ctx, c.server.deps.Store.Pool, repo, result.CommitSHA, now)
			if err != nil {
				slog.Info("dispatch: the claim of a read of a result's commit was not read, so the read asks GitHub nothing",
					"repo", repo, "commit", result.CommitSHA, "error", err)
				continue
			}
			if refused {
				continue
			}
			asking = append(asking, result.CommitSHA)
			continue
		}
		claimed, err := c.server.commitPulls.ClaimAcrossProcesses(ctx, c.server.deps.Store.Pool, repo, result.CommitSHA, now)
		if err != nil {
			slog.Info("dispatch: the claim of a read of a result's commit was not taken, so the read asks GitHub nothing",
				"repo", repo, "commit", result.CommitSHA, "error", err)
			continue
		}
		if claimed {
			asking = append(asking, result.CommitSHA)
		}
	}
	if len(asking) == 0 {
		return
	}
	var read delivery.CommitsPullsAnswer
	var err error
	if c.readOnly {
		read, err = c.server.deps.Attributor.AnswerCommitsPulls(
			ctx, c.server.deps.Store.Pool, c.server.deps.GitHub, &c.answers, repo, asking, now)
	} else {
		read, err = c.server.deps.Attributor.ReadThroughCommitsPulls(
			ctx, c.server.deps.Store.Pool, c.server.deps.GitHub, repo, asking, now)
	}
	if err != nil {
		slog.Info("dispatch: GitHub named no pull request for a result's commit",
			"repo", repo, "commits", len(asking), "asked", len(read.Asked), "answered", len(read.Reads), "error", err)
	}
	// The claims are given back and settled on a context the caller's end cannot cancel, as the
	// read itself runs (delivery's readThroughCommitsPulls): a caller that hangs up after the read
	// must not leave a claim standing that no answer of GitHub's is behind.
	settling := context.WithoutCancel(ctx)
	for _, sha := range asking {
		learned, answered := read.Reads[sha]
		switch {
		case !answered && (!read.Asked[sha] || errors.Is(err, delivery.ErrReadThroughNotStored)):
			if c.readOnly {
				continue
			}
			// No question of the read named this commit, since the read stopped at a failure
			// before it, or its answer was not stored, or not in full: either way its claim
			// protects no GitHub answer, so give it back, and the next read asks again at once
			// rather than CommitPullReadStaleAfter later. A commit a query named that GitHub
			// failed keeps its claim, as the read of one commit GitHub failed keeps it.
			if err := c.server.commitPulls.ReleaseAcrossProcesses(settling, c.server.deps.Store.Pool, repo, sha, now); err != nil {
				slog.Info("dispatch: the claim of a read that stored nothing was not given back, so the next read waits it out",
					"repo", repo, "commit", sha, "error", err)
			}
		case answered && learned.Written > 0 && !learned.AtHead && !c.readOnly:
			if err := c.server.commitPulls.SettleAcrossProcesses(settling, c.server.deps.Store.Pool, repo, sha, now); err != nil {
				slog.Info("dispatch: the settle of a read of a result's commit was not recorded, so a later read asks GitHub again",
					"repo", repo, "commit", sha, "error", err)
			}
		}
	}
}

// readThroughRepository is the repository readThroughHeads reads a result's commit through, and
// whether that read can be made at all, by whose the commit is taken to be (commitRepositoryOf):
// a linked repository, where the App can read it (commitRepository.readable, the one answer
// commitStands decides a `before_merge` result by as well); and else the repository the check's
// issue resolves to, where the App answered that repository's default branch, which also answers
// false for an issue resolving to no repository and for a resolve that did not answer, the memo
// commitStands reads next.
func (c *comparisonReader) readThroughRepository(ctx context.Context, result model.CheckResult) (string, bool) {
	if owner := c.commitRepositoryOf(ctx, result.CheckIssueKey, result.CommitRepo); owner.linked != "" {
		return owner.linked, owner.readable
	}
	if _, readable := c.defaultBranchOf(ctx, result.CheckIssueKey); !readable {
		return "", false
	}
	repo, _ := c.repositoryOf(ctx, result.CheckIssueKey)
	return *repo, true
}

// readLinkedHeads reads from GitHub each pull request the external links of issueKey name whose
// head Dispatch does not hold, and stores it (delivery.Attributor.LinkedHeadsUnread and
// ReadLinkedHead), before the results read reads any head (deriveCIResults, learn). It is the read
// of a link by hand that no result commit can reach: a pull request no event of which Dispatch
// took, linked to an issue whose only `before_merge` check is one CI proves, takes no result from
// any role (409 CHECK_PROVED_BY_CI), so no result write's read-through and no read-through of a
// result commit (readThroughHeads) asks GitHub about it, and the check read no head until the
// reconcile's next pass read the owed link (no_head with the pull request open, linked and
// green). It reads whatever checks the issue holds, and for a link written before links owed a
// read as for one written since.
//
// One linked pull request is read at most once per delivery.CommitPullReadStaleAfter in this
// process however often the results are read (server.linkedHeads), at most
// delivery.LinkedHeadReadsPerRead of them in one read, and a read that fails is claimed with the
// rest: the read leaves the bases as the stored rows say, which the reconcile repairs, so a GitHub
// call that did not land costs the results read nothing but the call.
//
// A read-only caller's read takes no claim: it asks GitHub nothing where a writer's claim stands
// (Refuses), and keeps what it learns for its own request alone
// (delivery.Attributor.AnswerLinkedHead, answers).
func (c *comparisonReader) readLinkedHeads(ctx context.Context, issueKey string) {
	s := c.server
	if s.deps.GitHub == nil || s.deps.Attributor == nil {
		return
	}
	pool := s.deps.Store.Pool
	unread, err := s.deps.Attributor.LinkedHeadsUnread(ctx, pool, issueKey, c.answers)
	if err != nil {
		slog.Warn("dispatch: read the linked pull requests whose head is not held", "issue", issueKey, "error", err)
		return
	}
	now := s.resultsNow()
	for _, link := range unread {
		key := "pull/" + strconv.Itoa(link.Number)
		if c.readOnly {
			if s.linkedHeads.Refuses(link.Repo, key, now) {
				continue
			}
			err = s.deps.Attributor.AnswerLinkedHead(ctx, pool, s.deps.GitHub, &c.answers, link, now)
		} else {
			if !s.linkedHeads.Claim(link.Repo, key, now) {
				continue
			}
			err = s.deps.Attributor.ReadLinkedHead(ctx, pool, s.deps.GitHub, link, now)
		}
		if err != nil {
			slog.Info("dispatch: read a linked pull request's head for an issue's results; the reconcile reads it again",
				"issue", issueKey, "repo", link.Repo, "number", link.Number, "error", err)
		}
	}
}
