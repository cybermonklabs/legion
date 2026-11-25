package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// results_linked.go holds what the results route does with a linked repository: one the check's
// issue does not resolve to, in which its external links name a GitHub pull request. An issue
// whose work spans two repositories links the pull request it has in the second one, and a
// result at that pull request's head names a commit of a repository the issue does not resolve
// to. The record route takes such a commit (refuseUnknownCommit) and keeps the repository with
// the row (model.CheckResult.CommitRepo); the results read says whose a result's commit is from
// that row (commitRepositoryOf) and walks a branch check's result in a linked repository
// (linkedBranchBases), where results_comparison.go walks every other result in the repository
// the check's issue resolves to.

// linkedPullRequestReads is the most linked pull requests one record reads the state of from
// GitHub (refuseUnknownCommit, linkedRepositoryHolds). Each read is a GitHub call the record
// makes one after another before it answers, and an issue may hold any number of links, since
// the issue PATCH bounds none: unbounded, one record would spend a call for every link a person
// or an agent ever added, against the App's hourly quota that every other read of Dispatch
// shares. A repository is read only until one of its linked pull requests counts, so the bound
// is also the most linked repositories one record reads, and each of those is asked about the
// commit at most once. Ten leaves room for an issue whose work spans several repositories, or
// carries a few links that name no pull request at all, and holds one record to at most ten such
// calls and ten commit reads. A link past the bound is left unread, and a refusal says how many
// were.
const linkedPullRequestReads = 10

// refuseUnknownCommit refuses a result at a commit that no repository the result may name a
// commit of holds: 400 RESULT_COMMIT_UNKNOWN, naming the commit and every `owner/repo` GitHub
// said holds no such commit. It answers the repository other than repo that the result's commit
// is taken to be of, and nil where the commit is taken to be repo's or there is no repo.
//
// A result may name a commit of three kinds of repository. The first is repo, the repository the
// check's issue resolves to, which readResultRequest resolved for this refusal; a nil repo is
// nothing to ask GitHub about, no refusal and no read-through. A deployment with no App key
// resolves none, so the key is not checked again here. The second is the repository of each open or
// merged pull request of the issue that Dispatch holds (delivery.Attributor.HeadsAttributedTo): a
// pull request an issue links by its URL is that issue's in any repository (attributionHeldFor, R19
// of the spec of several open pull requests), so it may be another repository than repo (R17). That
// covers a link in the `/issues/N` form too, which the statement reads as the pull request and the
// links below do not. The third is a linked repository in which the external links of the check's
// issue name a GitHub pull request that is open or merged (linkedRepositories,
// linkedRepositoryHolds). A closed pull request that did not merge makes no repository count, and
// neither does a link to a pull request GitHub answers 404 or 410 for
// (delivery.ErrPullRequestNotFound): each is an answer, and no retry changes it. A linked
// repository is called by its canonical name, which GitHub can hold: the link rule holds a link's
// own name to GitHub's names, and the boot holds no rename pair whose new name GitHub cannot hold
// (refs.NewRenames).
//
// A commit that is the head of an open or merged pull request of the issue, as Dispatch last read
// it from GitHub (AttributedHead.HeadKnown), is taken with no read of GitHub at all (R17): the
// head's row says which repository holds it, and the answer is that repository where it is not
// repo. Every other commit is asked of repo first, and of the other repositories only when
// GitHub does not say that repo holds it: when it proves repo does not, and when its read of repo
// did not answer — a rate limit, a 5xx — since the commit may then be another repository's, and a
// result stored under the issue's own repository there would be compared where GitHub answers
// 404 for it, unknown for good. A result at a commit of the issue's own repository costs the one
// read it cost before. The repositories of the issue's pull requests are asked next, each once
// in name order, and then the linked ones: at most linkedPullRequestReads linked pull requests
// are read, each distinct repository once, and a linked repository already asked as a pull
// request's is not asked again.
//
// It runs before the transaction opens, so a refused result leaves no check_results row and no
// result.recorded event behind. Dispatch stored a result at a mistyped commit no repository
// held once, and a reader could not tell it from the real head, since both print as twelve
// characters.
//
// It refuses in two ways. Where Dispatch's own list of the links could not be read
// (linkedRepositories), it answers that error, a server error to the caller, and nothing is stored,
// whatever GitHub said of repo: the record cannot tell whether a linked repository holds the
// commit. A failed read of Dispatch's rows of the issue's pull requests
// (delivery.Attributor.HeadsAttributedTo) refuses nothing by itself: it is logged, and the record
// asks GitHub as it would with no such row, so a commit repo holds is still stored. And where
// GitHub proved that repo does not hold the commit and every other repository either does not count
// or, counting, is proved not to hold it either — which delivery.RepositoryHoldsCommit defines as
// the one 422 whose message says GitHub found no commit for the SHA — it is refused 400
// RESULT_COMMIT_UNKNOWN. A refusal with links left unread for the bound says how many, since a link
// past it may name the repository that holds the commit.
//
// Where Dispatch cannot read one of the repositories and no repository is shown to hold the
// commit, it stores the result, taken to be repo's (R17): a read that did not answer — a rate
// limit, a 5xx, a transport failure — of repo or of any other repository, and a repository the
// App is not installed on or whose token is refused it (delivery.RepositoryUnreadable). No such
// answer can block the result a closed issue takes, the production check's after-merge result —
// whose merge commit is on the branch its pull request merged into and is held anyway — and a
// repository Dispatch cannot read records the result as before.
//
// It answers a repository other than repo only where the head's row names it or GitHub said that
// repository holds the commit: the first that does. Every other result it stores is taken to be
// at a commit of repo, and it answers nil for it. recordResultUnderOwnerLocks writes a repository
// it answers with the row (model.CheckResult.CommitRepo), so the results read compares a branch
// check's result in that repository (comparisonReader.onNamedBranch) and reads a `before_merge`
// result's pull requests through it (comparisonReader.readThroughHead); and the read-through after
// the write reads the commit's pull requests through it, or through repo where it answers nil
// (readThroughPullRequests). The repository is under the name it is called today: the heads and
// the links are read under the boot's rename pairs (delivery.Attributor), so the row stores no
// name a repository was renamed away from, as no write of a repository does, and the boot's pass
// rewrites a row stored before (store.RewriteRenamedRepositories, check_results.commit_repo).
func (s *server) refuseUnknownCommit(ctx context.Context, q refs.Queryer, issueKey string, repo *string, sha string) (*string, error) {
	if repo == nil {
		return nil, nil
	}
	heads, err := s.deps.Attributor.HeadsAttributedTo(ctx, q, issueKey, delivery.HeadAnswers{})
	if err != nil {
		slog.Warn("dispatch: read the pull request heads of a recorded result's commit",
			"issue", issueKey, "commit", sha, "error", err)
	}
	if at := slices.IndexFunc(heads, func(head delivery.AttributedHead) bool {
		return head.HeadKnown && head.HeadSHA == sha
	}); at >= 0 {
		return headRepositoryOther(heads[at], *repo), nil
	}
	held, err := delivery.RepositoryHoldsCommit(ctx, s.deps.GitHub, *repo, sha)
	if err == nil && held {
		return nil, nil
	}
	// unread is a repository Dispatch could not read. It refuses nothing by itself (R17): it
	// leaves the commit repo's wherever no other repository is shown to hold it.
	unread := err != nil
	if unread {
		slog.Info("dispatch: GitHub could not say whether a recorded result's commit is held",
			"repo", *repo, "commit", sha, "error", err)
	}
	checked := []string{*repo}
	asked := []string{*repo}
	for _, other := range otherHeadRepositories(heads, *repo) {
		asked = append(asked, other)
		held, err := delivery.RepositoryHoldsCommit(ctx, s.deps.GitHub, other, sha)
		switch {
		case err != nil:
			slog.Info("dispatch: GitHub could not say whether the repository of a pull request of the issue holds a recorded result's commit",
				"issue", issueKey, "repo", other, "commit", sha, "error", err)
			unread = true
		case held:
			return &other, nil
		default:
			checked = append(checked, other)
		}
	}
	links, err := s.linkedRepositories(ctx, q, issueKey)
	if err != nil {
		return nil, err
	}
	reads := linkedReads{left: linkedPullRequestReads}
	for _, link := range links {
		if slices.ContainsFunc(asked, func(name string) bool { return strings.EqualFold(name, link.repo) }) {
			continue
		}
		counts, held, err := s.linkedRepositoryHolds(ctx, link, sha, &reads)
		switch {
		case err != nil:
			slog.Info("dispatch: GitHub could not say whether a linked repository holds a recorded result's commit",
				"issue", issueKey, "repo", link.repo, "commit", sha, "error", err)
			unread = true
		case held:
			return &link.repo, nil
		case counts:
			checked = append(checked, link.repo)
		}
	}
	if unread || len(reads.unreadRepos) > 0 {
		return nil, nil
	}
	return nil, errorf(http.StatusBadRequest, "RESULT_COMMIT_UNKNOWN",
		"%s no commit %s: record the result once the commit is pushed, or link the issue to an open or merged pull request of the repository that holds it%s",
		repositoriesHold(checked), sha, reads.leftUnread())
}

// headRepositoryOther is the repository head is of where it is not repo, the repository the
// check's issue resolves to, and nil where it is: a linked pull request's under the canonical
// name the statement reads it by (delivery.AttributedHead.LinkedRepo), and else the row's own,
// compared without case.
func headRepositoryOther(head delivery.AttributedHead, repo string) *string {
	switch {
	case head.LinkedRepo != "":
		return &head.LinkedRepo
	case !strings.EqualFold(head.Repo, repo):
		return &head.Repo
	}
	return nil
}

// otherHeadRepositories is the repository of each pull request in heads that is not of repo,
// each once, compared without case, in name order: a linked pull request's under the name
// headRepositoryOther gives it, and every other's its row's own. A linked pull request's
// repository is among them, unlike the repositories the links below read, since the statement
// gives the issue a pull request it links in the `/issues/N` form too, which no link of the
// `/pull/N` form names.
func otherHeadRepositories(heads []delivery.AttributedHead, repo string) []string {
	var others []string
	for _, head := range heads {
		other := headRepositoryOther(head, repo)
		if other == nil {
			continue
		}
		if !slices.ContainsFunc(others, func(name string) bool { return strings.EqualFold(name, *other) }) {
			others = append(others, *other)
		}
	}
	slices.Sort(others)
	return others
}

// repositoriesHold is the subject of a refusal's sentence, every repository it checked: "a
// holds" for one, "a and b hold" for two, "a, b and c hold" for more.
func repositoriesHold(repos []string) string {
	if len(repos) == 1 {
		return repos[0] + " holds"
	}
	return joinAnd(repos) + " hold"
}

// joinAnd lists items as a sentence does: "a", "a and b", "a, b and c".
func joinAnd(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// linkedReads is what one record has left of its bound on the linked pull requests it reads
// (linkedPullRequestReads), how many linked pull requests the bound left unread and each
// repository Dispatch cannot read (delivery.RepositoryUnreadable), in the order the record met
// them: unreadRepos names each it cannot read, which stores the result rather than refusing it
// (R17 of the spec of several open pull requests).
type linkedReads struct {
	left, unread int
	unreadRepos  []string
}

// cannotRead keeps link, a repository whose read of a linked pull request's state, or of the
// commit once a pull request read open or merged, answered that Dispatch cannot read it: the
// answer is about the whole repository, so none of its pull requests can be read until someone
// grants the App it.
func (r *linkedReads) cannotRead(link linkedRepository) {
	r.unreadRepos = append(r.unreadRepos, link.repo)
}

// leftUnread is the clause a refusal ends with where the bound left links unread, and "" where
// every link was read or needed no read.
func (r linkedReads) leftUnread() string {
	switch r.unread {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("; 1 linked pull request was left unread, past the %d one record reads", linkedPullRequestReads)
	}
	return fmt.Sprintf("; %d linked pull requests were left unread, past the %d one record reads", r.unread, linkedPullRequestReads)
}

// linkedRepository is one repository an issue's external links name GitHub pull requests in,
// under the canonical name the repository is called today, with the numbers of those pull
// requests in the order the issue lists its links.
type linkedRepository struct {
	repo    string
	numbers []int
}

// linkedRepositories is every repository that the issue does not resolve to and in which the
// external links of issueKey name a GitHub pull request, each once with the numbers of its
// linked pull requests, in the order the issue lists its links. It groups
// delivery.Attributor.LinkedPullRequests, under the rename pairs the boot read. A link to a pull
// request of the issue's own repository names nothing here: that repository is asked about the
// commit before any link is read.
func (s *server) linkedRepositories(ctx context.Context, q refs.Queryer, issueKey string) ([]linkedRepository, error) {
	pulls, err := s.deps.Attributor.LinkedPullRequests(ctx, q, issueKey)
	if err != nil {
		return nil, err
	}
	var linked []linkedRepository
	for _, pull := range pulls {
		at := slices.IndexFunc(linked, func(l linkedRepository) bool { return strings.EqualFold(l.repo, pull.Repo) })
		if at < 0 {
			linked = append(linked, linkedRepository{repo: pull.Repo})
			at = len(linked) - 1
		}
		linked[at].numbers = append(linked[at].numbers, pull.Number)
	}
	return linked, nil
}

// linkedRepositoryHolds answers whether link's repository counts — one of its linked pull
// requests is open or merged, read from GitHub in link order until one is
// (delivery.PullRequestOpenOrMerged) — and, for a repository that counts, whether it holds sha
// (delivery.RepositoryHoldsCommit). Each pull request read spends one of reads, and one the
// bound leaves unread is counted there and read no further.
//
// A repository does not count, with no error, where every linked pull request closed without
// merging or is one GitHub answers 404 or 410 for, and where a read answers that no read of that
// repository can work: either read, where the App is not installed on it or its token is refused
// it (delivery.RepositoryUnreadable: githubapp.ErrNoInstallation, and
// githubapp.ErrRepositoryUnreadable, the 403 that is no rate limit). That 404 or 410
// (delivery.ErrPullRequestNotFound) is an answer and not a failed read: GitHub says there is no
// such pull request, which is a link a person mistyped or one to a pull request since deleted, so
// it counts for nothing, exactly as a closed one that did not merge, and the next linked pull
// request is read. A repository no read of which can work is the same answer about every pull
// request of it, so none of them is read further and its commits are not asked about. No retry
// changes it until someone gives the App the repository, and the result is stored rather than
// refused (linkedReads.cannotRead, R17 of the spec of several open pull requests). Read as a
// failure, it would refuse every record of the issue until someone removed the link.
//
// The error is a read that did not answer — a rate limit, a 5xx, a transport failure — about the
// commit, or about a pull request's state where no other pull request of the repository is open
// or merged, which leaves the result stored where no repository is shown to hold the commit
// (refuseUnknownCommit, R17).
func (s *server) linkedRepositoryHolds(ctx context.Context, link linkedRepository, sha string, reads *linkedReads) (counts, held bool, err error) {
	for index, number := range link.numbers {
		if reads.left == 0 {
			reads.unread += len(link.numbers) - index
			return false, false, err
		}
		reads.left--
		open, readErr := delivery.PullRequestOpenOrMerged(ctx, s.deps.GitHub, link.repo, number)
		switch {
		case delivery.RepositoryUnreadable(readErr):
			reads.cannotRead(link)
			return false, false, nil
		case errors.Is(readErr, delivery.ErrPullRequestNotFound):
			// No such pull request: it counts for nothing, and the next one is read.
		case readErr != nil:
			if err == nil {
				err = readErr
			}
		case open:
			held, err = delivery.RepositoryHoldsCommit(ctx, s.deps.GitHub, link.repo, sha)
			if delivery.RepositoryUnreadable(err) {
				reads.cannotRead(link)
				return false, false, nil
			}
			return true, held, err
		}
	}
	return false, false, err
}

// commitRepository is whose a result's commit is taken to be, as commitRepositoryOf answers it:
// the repository the check's issue resolves to, where linked is "", or the linked repository
// linked names.
type commitRepository struct {
	// linked is the linked repository the commit is of, under the name it is called today, and ""
	// where the commit is of the repository the check's issue resolves to, which no answer about
	// it names.
	linked string
	// readable says the App answered the default branch of linked (defaultBranchIn): the one
	// answer to whether Dispatch can read a linked repository, which commitStands decides a
	// `before_merge` result by and readThroughRepository decides its read-through by. It is false
	// where linked is "", since no linked repository is read then.
	readable bool
}

// commitRepositoryOf is whose the commit of a result of the check of issueKey is taken to be,
// commitRepo being the linked repository the result's row names (model.CheckResult.CommitRepo).
// It is the one place the results read decides it, and commitStands, onNamedBranch and
// readThroughRepository each read it, so no two of them can answer the question apart.
//
// A row that names no repository, which is every result at a commit of the issue's own
// repository and every row recorded before the column existed, is the issue's own repository's,
// and reads nothing, so the ordinary result costs what it always did. A row that names one names
// a repository GitHub said holds the commit when the result was recorded (refuseUnknownCommit),
// under the name it was called then, which is read again under the name it is called today
// (refs.Renames.Canonical over Deps.Renames, the pairs the boot read, so it costs no read): the
// repository can be renamed since, and GitHub answers 404 for the old name's installation, so
// every result of it would read unknown — the reason the repository of the check's issue is read
// under its canonical name (checkIssueRepository).
//
// A name that is the repository the check's issue resolves to now, compared without case, is the
// issue's own repository: a row stored while the issue resolved elsewhere names that repository,
// and once the issue has come to resolve to it, the result is read as every other result of the
// issue is, and no answer about it names a repository.
//
// For a linked repository, it also reads whether the App can read that repository (readable),
// from defaultBranchIn's memo. Every reader of such a repository reads that memo for it next
// anyway, so the answer costs no GitHub call they would not make.
func (c *comparisonReader) commitRepositoryOf(ctx context.Context, issueKey string, commitRepo *string) commitRepository {
	if commitRepo == nil {
		return commitRepository{}
	}
	owner := commitRepository{linked: c.server.deps.Renames.Canonical(*commitRepo)}
	if own, answered := c.repositoryOf(ctx, issueKey); answered && own != nil && strings.EqualFold(owner.linked, *own) {
		owner.linked = ""
	}
	if owner.linked != "" {
		_, owner.readable = c.defaultBranchIn(ctx, owner.linked)
	}
	return owner
}

// linkedBranchBases is branchBases for a result whose commit is taken to be of repo, a linked
// repository (commitRepositoryOf) GitHub said holds the commit when the result was recorded: the
// branches of repo that result is compared with, chosen by the rule the issue's own repository's
// are chosen by (measuredBranches), the `after_merge` check's merged branch being the one the
// issue's linked pull request in repo merged into (baseBranchMergedIn). The walk asks them in
// this order (onNamedBranch), and the check goes on naming its own repository's branches as its
// bases: these are one result's, not the check's. Each names repo, so a reader can tell the
// branch a result stands at from the issue's own repository's branch of the same name.
//
// A comparison naming no branch here is always unknown for the result, never another commit,
// so its reason is one of the two a commit's standing carries. A merge read that failed is
// model.ReasonReadFailed. No branch at all is a repository no read of this process ever named
// a default branch for, with no merge of the issue's there for an `after_merge` check to name
// in its place, and that is the App's failed read of repo, since
// delivery.Branches.DefaultBranchOf answers no name only with no:
// model.ReasonRepositoryUnreadable.
func (c *comparisonReader) linkedBranchBases(ctx context.Context, check model.CheckBlock, repo string) comparison {
	defaultBranch, _ := c.defaultBranchIn(ctx, repo)
	branches, chosen := c.measuredBranches(check, defaultBranch, func() (string, bool) {
		return c.baseBranchMergedIn(ctx, check.IssueKey, repo)
	})
	if !chosen {
		return noBases(model.ReasonReadFailed)
	}
	if len(branches) == 0 {
		return noBases(model.ReasonRepositoryUnreadable)
	}
	compared := branchesNamed(branches)
	for index := range compared.bases {
		compared.bases[index].Repo = repo
	}
	return compared
}

// baseBranchMergedIn is the branch the newest pull request the check's issue links in repo, a
// linked repository, merged into (delivery.Attributor.BaseBranchMergedIn), read once per issue and
// repository per read of the results, and whether the read answered at all. The attribution
// gives the issue a pull request it links in any repository, and baseBranchMergedFor reads the
// pull requests of the repository the issue resolves to alone, so this reads its merge there. The
// key is the issue and the repository: an issue whose work spans two repositories merged once in
// each. The memo keeps a failed read as baseBranchMergedFor's does, for the same reason. It reads
// the caller's request's answers as baseBranchMergedFor does: a merged pull request a read-only
// caller's read-through of a linked repository's commit learned is a row a writer's read-through
// would have stored, which its read here finds.
func (c *comparisonReader) baseBranchMergedIn(ctx context.Context, issueKey, repo string) (string, bool) {
	memo := [2]string{issueKey, repo}
	if answer, memoized := c.linkedBaseBranches[memo]; memoized {
		return answer.branch, answer.read
	}
	branch, err := c.server.deps.Attributor.BaseBranchMergedIn(ctx, c.server.deps.Store.Pool, issueKey, repo, c.answers)
	if err != nil {
		slog.Warn("dispatch: read the branch a result's issue merged into", "issue", issueKey, "repo", repo, "error", err)
	}
	answer := baseBranchAnswer{branch: branch, read: err == nil}
	c.linkedBaseBranches[memo] = answer
	return answer.branch, answer.read
}
