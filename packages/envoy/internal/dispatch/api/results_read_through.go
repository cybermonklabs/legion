package api

import (
	"context"
	"log/slog"
	"strconv"
	"time"

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
// after the record would never be learned. The readable test is that repository's own.
//
// A read that fails leaves the bases exactly as the stored rows say, which is what a result reads
// with no read-through at all, and marks the commit (readFailed): a GitHub call that did not land
// is no evidence either way, so a result there that the rows leave at another commit is not known
// for model.ReasonReadFailed (commitStands). That holds for each commit GitHub failed, each
// commit the read stopped before, and each one Dispatch's own database could not claim or store
// the answer of.
//
// A claim stands on GitHub's answer only once this read has stored that answer and marked the
// claim answered (delivery.CommitPullReads.AnswerAcrossProcesses, or the settle). A read the
// claim refuses, in any process, reads the commit from the stored rows only where the claim says
// so (HeldAcrossProcesses). A claim that is not answered is a read under way, a read GitHub
// failed, or a read whose process stopped, and none of those has stored anything about the
// commit, so a result there is not known. A commit this process's memory refused on a moment no
// claim holds any more — another process gave the claim back, which that process's give-back
// cannot reach here — is claimed again at once (ReclaimAcrossProcesses), and a read-only caller
// asks GitHub about it.
//
// A read-only caller's read makes the same read and stores nothing, takes no claim, settles and
// marks nothing (delivery.Attributor.AnswerCommitsPulls), and reads the claims as a writer's does.
func (c *comparisonReader) readThroughHeads(ctx context.Context, repo string, results []model.CheckResult) {
	if c.server.deps.GitHub == nil {
		return
	}
	now := c.server.resultsNow()
	failed := func(sha string) { c.readFailed[[2]string{repo, sha}] = true }
	if ctx.Err() != nil {
		// The read's budget for its read-through is spent (learn): it claims nothing more and
		// asks nothing, and each commit here is not known, as one no question asked is.
		for _, result := range results {
			failed(result.CommitSHA)
		}
		return
	}
	var asking, refused []string
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
			held, err := c.server.commitPulls.RefusesAcrossProcesses(ctx, c.server.deps.Store.Pool, repo, result.CommitSHA, now)
			if err != nil {
				slog.Info("dispatch: the claim of a read of a result's commit was not read, so the read asks GitHub nothing and reads the commit as a read that failed",
					"repo", repo, "commit", result.CommitSHA, "error", err)
				failed(result.CommitSHA)
				continue
			}
			if held {
				refused = append(refused, result.CommitSHA)
				continue
			}
			asking = append(asking, result.CommitSHA)
			continue
		}
		claimed, err := c.server.commitPulls.ClaimAcrossProcesses(ctx, c.server.deps.Store.Pool, repo, result.CommitSHA, now)
		if err != nil {
			slog.Info("dispatch: the claim of a read of a result's commit was not taken, so the read asks GitHub nothing and reads the commit as a read that failed",
				"repo", repo, "commit", result.CommitSHA, "error", err)
			failed(result.CommitSHA)
			continue
		}
		if claimed {
			asking = append(asking, result.CommitSHA)
		} else {
			refused = append(refused, result.CommitSHA)
		}
	}
	asking = c.readRefusedClaims(ctx, repo, refused, asking, now)
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
		slog.Info("dispatch: the read of the pull requests of results' commits stopped, so each commit it did not answer reads as a read that failed",
			"repo", repo, "commits", len(asking), "asked", len(read.Asked), "answered", len(read.Reads),
			"failed", len(read.Failed), "error", err)
	}
	// The claims are given back, marked and settled on a context the caller's end cannot cancel,
	// as the read itself runs (delivery's readThroughCommitsPulls): a caller that hangs up after
	// the read must not leave a claim standing that no answer of GitHub's is behind.
	settling := context.WithoutCancel(ctx)
	var answered []string
	for _, sha := range asking {
		learned, stored := read.Reads[sha]
		switch {
		case stored && c.readOnly:
		case stored && learned.Written > 0 && !learned.AtHead:
			if err := c.server.commitPulls.SettleAcrossProcesses(settling, c.server.deps.Store.Pool, repo, sha, now); err != nil {
				// The claim is not marked answered, so a read it refuses would read the commit as
				// not known for the rest of its span: it is given back, and the next read asks again.
				slog.Info("dispatch: the settle of a read of a result's commit was not recorded, so its claim is given back",
					"repo", repo, "commit", sha, "error", err)
				c.giveBack(settling, repo, sha, now)
			}
		case stored:
			answered = append(answered, sha)
		case read.Failed[sha]:
			// GitHub failed this commit, its query, its alias in the query or its own REST read, so
			// it keeps its claim, as the read of one commit GitHub failed keeps it. The claim is
			// not answered, so every read it refuses, in any process, reads the commit as not known
			// for the rest of its span, and none asks GitHub again inside it.
			failed(sha)
		default:
			// No answer of GitHub's stands behind this commit's claim and no failure of GitHub's
			// either: the read stopped before it, in an earlier batch or earlier in its own query's
			// answer, or its answer was not stored, or not in full. So this read reads it as a read
			// that failed, and gives the claim back: the next read asks again at once rather than
			// CommitPullReadStaleAfter later.
			failed(sha)
			if !c.readOnly {
				c.giveBack(settling, repo, sha, now)
			}
		}
	}
	if err := c.server.commitPulls.AnswerAcrossProcesses(settling, c.server.deps.Store.Pool, repo, answered, now); err != nil {
		// A claim not marked answered reads as not known in every read it refuses: the claims are
		// given back, and the next read asks GitHub again and finds the rows this read stored.
		slog.Info("dispatch: the claims of reads GitHub answered were not marked answered, so they are given back",
			"repo", repo, "commits", len(answered), "error", err)
		for _, sha := range answered {
			c.giveBack(settling, repo, sha, now)
		}
	}
}

// readRefusedClaims reads the claim every process shares of each commit a claim refused
// (delivery.CommitPullReads.HeldAcrossProcesses) and answers asking with the commits to ask
// GitHub about after all. A commit whose claim is answered reads from the stored rows, which hold
// GitHub's answer. A commit whose claim is not answered is not known (readFailed): the read behind
// the claim is under way, failed at GitHub, or stopped with its process. A commit with no claim
// held past now was refused on a moment this process's memory took from a claim that is gone —
// another process gave it back — or holds longer than the database does: a writer claims it again
// (ReclaimAcrossProcesses) and asks about it where that is granted, and a read-only caller asks
// about it. A claim another read took between the two statements is read once more, and a commit
// that still has no answered claim then is not known: the read ends there rather than chase it.
func (c *comparisonReader) readRefusedClaims(ctx context.Context, repo string, refused, asking []string, now time.Time) []string {
	failed := func(sha string) { c.readFailed[[2]string{repo, sha}] = true }
	held, err := c.server.commitPulls.HeldAcrossProcesses(ctx, c.server.deps.Store.Pool, repo, refused, now)
	if err != nil {
		slog.Info("dispatch: the claims that refused reads of results' commits were not read, so the read reads each as a read that failed",
			"repo", repo, "commits", len(refused), "error", err)
		for _, sha := range refused {
			failed(sha)
		}
		return asking
	}
	var taken []string
	for _, sha := range refused {
		answered, found := held[sha]
		switch {
		case found && !answered:
			failed(sha)
		case found:
		case c.readOnly:
			asking = append(asking, sha)
		default:
			claimed, err := c.server.commitPulls.ReclaimAcrossProcesses(ctx, c.server.deps.Store.Pool, repo, sha, now)
			switch {
			case err != nil:
				slog.Info("dispatch: the claim of a read of a result's commit was not taken again, so the read asks GitHub nothing and reads the commit as a read that failed",
					"repo", repo, "commit", sha, "error", err)
				failed(sha)
			case claimed:
				asking = append(asking, sha)
			default:
				taken = append(taken, sha)
			}
		}
	}
	if len(taken) == 0 {
		return asking
	}
	held, err = c.server.commitPulls.HeldAcrossProcesses(ctx, c.server.deps.Store.Pool, repo, taken, now)
	for _, sha := range taken {
		if err != nil || !held[sha] {
			failed(sha)
		}
	}
	return asking
}

// giveBack gives back the claim this read took of repo's commit sha at now, and logs a give-back
// that failed: the claim then stands not answered, so every read it refuses reads the commit as not
// known until its moment passes.
func (c *comparisonReader) giveBack(ctx context.Context, repo, sha string, now time.Time) {
	if err := c.server.commitPulls.ReleaseAcrossProcesses(ctx, c.server.deps.Store.Pool, repo, sha, now); err != nil {
		slog.Info("dispatch: the claim of a read of a result's commit was not given back, so a read it refuses reads the commit as not known until its moment passes",
			"repo", repo, "commit", sha, "error", err)
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
