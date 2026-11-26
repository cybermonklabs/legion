package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// results_ci.go answers a check CI proves (validCICheck) on the read of an issue's results. No
// role records a result for such a check (refuseResultOfAnotherRecorder), and nothing is stored
// of the run its `ci_check` names: each read takes the head of each open pull request of the
// issue (delivery.Attributor.HeadsAttributedTo), or where none is open the newest merged one
// (delivery.NewestPullRequestHead), reads each such commit's check runs from GitHub once, in the
// repository of that pull request's own row, and derives a result at each from the run its
// `ci_check` names. A check CI proves holds those derived results beside each result a run of its
// CI job recorded through the job's results token (ci_check_results: role ci, Branch set), newest
// first. The run's conclusion is the whole job's, and a whole job's result decides no check (R12
// of The Spec): at a head where the job recorded a result of its own for a check, the run derives
// that check no result there (jobResultHeads), so a failing test of another check of the job
// leaves this one as the job recorded it. The derived results go through the same
// comparisonReader a recorded result does, so one rule judges where both kinds stand. The branch
// such a recorded result names is read here too, for the comparison of every `before_merge`
// check of the issue: its head, as GitHub answers it on this read (readBranchHead).

// ciReadDeadline bounds the whole read of the check runs of every head of one results read, the
// token mints and every page together, and the read of one pushed branch's head, its token mint
// included (readBranchHead): the route is read inside a role's turn and inside the reviewer's
// gate, and a read that runs out is a failed read the next one repairs. githubapp.RequestTimeout
// bounds one call at twice this, so the bound is the context's (ciReadBound).
const ciReadDeadline = 10 * time.Second

// ciReadBound is ctx bounded by ciReadDeadline, the bound of each read of GitHub a results read
// makes: the reads of every head together (readCIHeads), each head's read inside it (readCIRuns),
// and one pushed branch's head (readBranchHead). A var so a test can end a bound on an event of its
// own, once the reads it waits for have ended, and never on a clock a stopped scheduler runs past.
var ciReadBound = func(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, ciReadDeadline)
}

// ciHeadReadConcurrency is how many heads' check runs one results read reads at one time: the
// number the reconcile reads pull requests with (delivery's reconcilePartialConcurrency), so an
// issue with eleven open pull requests takes two rounds within the one ciReadDeadline.
const ciHeadReadConcurrency = 8

// ciCheckRunPages bounds the pages of one head's check runs, a hundred runs a page, as
// cireconcile bounds its own reads of the same listing: a listing GitHub keeps reporting full
// past it is a failed read rather than a loop.
const ciCheckRunPages = 100

// ciResultRole is the role a derived result names: the producer, never a role that recorded it.
const ciResultRole = "ci"

// ciResultActorKind is the kind of actor a derived result names, whose id is the slug of the
// GitHub App that created the run.
const ciResultActorKind = "github_app"

// ciResultIDPrefix is what a derived result's id begins with, before the check run's own id, so
// no derived id can be read as a stored row's uuid.
const ciResultIDPrefix = "ci-run:"

// ciObservedOutputRunes is how much of the run's output title and summary, taken together, a
// derived result's observed text carries.
const ciObservedOutputRunes = 2000

// ciUnreadDetailRunes is how long the detail of a check whose runs GitHub could not be read for
// is, at most: the bound ciObservedOutputRunes puts on a run's output.
const ciUnreadDetailRunes = ciObservedOutputRunes

// The cases that name why a check CI proves holds no derived result, beside the conclusions GitHub
// writes. A conclusion is its own case: success is a pass; neutral, skipped and cancelled prove
// nothing; failure, timed_out, action_required, startup_failure and stale are a fail, and so is
// a conclusion this list does not name, as GitHub wrote it.
const (
	ciConclusionSuccess   = "success"
	ciConclusionNeutral   = "neutral"
	ciConclusionSkipped   = "skipped"
	ciConclusionCancelled = "cancelled"
	ciCaseNotCompleted    = "not_completed"
	ciCaseAbsent          = "absent"
	ciCaseNoHead          = "no_head"
	ciCaseUnreadable      = "github_unreadable"
	// ciCaseJobRecorded is a run that passed or failed at a head where the check's CI job
	// recorded a result of its own for the check (jobResultHeads): no_result, since the run's
	// conclusion is the whole job's.
	ciCaseJobRecorded = "job_recorded"
	// ciCaseReadFailed is a head at whose commit the check's CI job recorded a result that names
	// no repository, where Dispatch's own read of the repository the issue resolves to did not
	// answer (jobResultHeads): unread, since whether that result or the run decides the head
	// turns on the repository the read would have named.
	ciCaseReadFailed = model.ReasonReadFailed
)

// ciRunCompleted is the status of a check run that has concluded.
const ciRunCompleted = "completed"

// ciRun is one GitHub check run, with the fields a derived result is made of.
type ciRun struct {
	id          int64
	name        string
	status      string
	conclusion  string
	completedAt time.Time
	htmlURL     string
	title       string
	summary     string
	appSlug     string
}

// deriveCIResults answers every check CI proves among checks (validCICheck, its block): its
// `ci` member, the read of the newest head, and, at each head where the run that decides it passed
// or failed, the result derived from that run, merged into the results the read loaded for it,
// newest first (newestResultsFirst). The heads are the head of each open pull request of the issue
// (ciHeadsOf), each read in the repository of its own row, or where none is open the merged one
// that merged last, which fillMergeTimes orders over the reader's memo (comparisonReader.fills).
// Where Dispatch cannot tell which merged one merged last, it reads no head, and each such check's
// `ci` is unread for the case read_failed (ciCaseReadFailed). It answers each check's
// read at each head, by block id and then by the head's repository and commit (ciHeadKey), which
// the comparison gives each subject (comparison.ci): two repositories at one commit each read
// their own runs there, and each derives a result of its own, so a fail in one is never hidden by
// a pass in the other (R8). At a head where the check's
// CI job recorded a result of its own for the check, the run derives the check none
// (jobResultHeads, ciCaseJobRecorded); where the read of the repository that says whether such a
// result is of the head's did not answer, the head is unread for the check (ciCaseReadFailed)
// and the run derives it none either. A head whose read did not end
// within the one ciReadDeadline of the whole read is unread there alone (readCIHeads). A result
// the read loaded for such a check that no run of the job its ci_check names recorded, a role's or
// another job's, counts for nothing and says why (notCountedOnCICheck). An issue holding no check
// CI proves asks nothing of the database or of GitHub. issueKey is the route's issue, whose
// Verification document holds the checks and whose pull request heads they are read at. It runs
// after reader has learned the heads GitHub names (comparisonReader.learn) and reads the heads with
// reader's answers (ciHeadsOf), so a head this read's read-through learned is a head a check CI
// proves is derived at. Only a failed read of Dispatch's own database is an error; whatever GitHub
// answers is a case of the answer.
func (s *server) deriveCIResults(ctx context.Context, reader *comparisonReader, issueKey string, checks []model.CheckBlock) (map[string]map[ciHeadKey]*model.CheckCI, error) {
	var proved []provedCI
	names := map[string]struct{}{}
	for index := range checks {
		if validCICheck(checks[index]) {
			name := docs.CheckAttributesOfBlock(checks[index]).CICheck
			notCountedOnCICheck(&checks[index], name)
			proved = append(proved, provedCI{check: &checks[index], name: name})
			names[name] = struct{}{}
		}
	}
	if len(proved) == 0 {
		return nil, nil
	}
	// Every case below writes a line, and every line names the run, so the rule its words are kept
	// by is read before any of them; an issue holding no check CI proves still reads nothing.
	words, err := s.readLineWords(ctx, s.deps.Store.Pool)
	if err != nil {
		return nil, err
	}
	// The heads are read after learn, so a writer's read finds the rows its own read-through
	// stored, and a read-only caller's read finds the heads its own read-through learned, through
	// the request's answers it passes as every other read of the heads does. A reader that named a
	// commit learns nothing, so its answers are empty and it reads the stored rows alone, whoever
	// reads.
	heads, ordered, err := s.ciHeadsOf(ctx, reader, issueKey)
	if err != nil {
		return nil, err
	}
	if !ordered {
		for _, p := range proved {
			p.check.CI = ciUnconcluded(*p.check, p.name, nil, model.CIStateUnread, ciCaseReadFailed,
				fmt.Sprintf("Dispatch holds no time of merge for a merged pull request of %s, so it cannot tell which of them merged last, and read no head.", issueKey), words)
		}
		return nil, nil
	}
	if len(heads) == 0 {
		for _, p := range proved {
			p.check.CI = ciUnconcluded(*p.check, p.name, nil, model.CIStateNoResult, ciCaseNoHead,
				fmt.Sprintf("Dispatch holds no open or merged pull request head of %s.", issueKey), words)
		}
		return nil, nil
	}
	reads := s.readCIHeads(ctx, heads, names)
	jobs := s.jobResultHeads(ctx, issueKey, proved, heads)
	answers := make(map[string]map[ciHeadKey]*model.CheckCI, len(proved))
	for _, p := range proved {
		atHead := make(map[ciHeadKey]*model.CheckCI, len(heads))
		for index, head := range heads {
			job := jobs[p.check.BlockID][ciHeadKeyOf(head)]
			switch {
			case reads[index].err != nil:
				atHead[ciHeadKeyOf(head)] = ciUnconcluded(*p.check, p.name, &head, model.CIStateUnread, ciCaseUnreadable,
					ciUnreadDetail(head, reads[index].err), words)
				continue
			case job == jobAtHeadUnresolved:
				atHead[ciHeadKeyOf(head)] = ciUnconcluded(*p.check, p.name, &head, model.CIStateUnread, ciCaseReadFailed,
					fmt.Sprintf("Dispatch could not read the repository %s resolves to, so whether the result the check's CI job recorded at %s is of %s, and decides that head in place of the run, is not known.",
						issueKey, head.HeadSHA, head.Repo), words)
				continue
			}
			answer, result := ciAnswerOf(*p.check, p.name, head, reads[index].runs[p.name], job == jobAtHeadRecorded, words)
			atHead[ciHeadKeyOf(head)] = answer
			if result != nil {
				p.check.Results = append(p.check.Results, *result)
			}
		}
		p.check.Results = newestResultsFirst(p.check.Results)
		p.check.CI = atHead[ciHeadKeyOf(heads[0])]
		answers[p.check.BlockID] = atHead
	}
	return answers, nil
}

// ciHeadKey is one head a check CI proves is read at: the repository of its pull request, where
// its check runs are read, and its commit. Two repositories may hold one commit, each with runs of
// its own there, so every step of the read keys a head by both (deriveCIResults, ciHeadsOf,
// eachSubjectStands), never by the commit alone.
type ciHeadKey struct {
	repo   string
	commit string
}

// ciHeadKeyOf is the key of head.
func ciHeadKeyOf(head delivery.NewestHead) ciHeadKey {
	return ciHeadKey{repo: head.Repo, commit: head.HeadSHA}
}

// jobAtHead is what jobResultHeads reads of a check's CI job at one head, in rising order: of two
// readings at one head the higher stands, so a result that is of the head's repository decides the
// head whatever another result's unresolved repository would have said.
type jobAtHead int

const (
	// jobAtHeadNone: no result of the job that counts is of the head's repository at its commit.
	jobAtHeadNone jobAtHead = iota
	// jobAtHeadUnresolved: the job recorded a result that counts at the head's commit and names
	// no repository, so it is of the issue's own, and the read of the repository the issue
	// resolves to did not answer: whether the result is of the head's repository is not known.
	jobAtHeadUnresolved
	// jobAtHeadRecorded: the job recorded a result there that counts, of the head's repository.
	jobAtHeadRecorded
)

// jobResultHeads is, for each check of proved by block id, what the check's CI job recorded at
// each head: a result of its own that counts (jobAtHeadRecorded) is of the job the check's
// ci_check names, since notCountedOnCICheck has marked every other, retracted by nobody, under the
// check's words now, at the head's commit, and of the head's repository. A job's result names its
// repository only where it is another than the issue's (CommitRepo), so the issue's repository is
// resolved, once, and only where such a result is at the commit of a head. An issue that resolves
// to no repository holds no head of its own, so no head takes such a result in place of its run's
// conclusion, and both stand there. A resolve that did not answer, a failed read of Dispatch's own
// database (checkIssueRepository), says neither: each head such a result is at is unresolved
// (jobAtHeadUnresolved), unless a result that names its repository is recorded there.
func (s *server) jobResultHeads(ctx context.Context, issueKey string, proved []provedCI, heads []delivery.NewestHead) map[string]map[ciHeadKey]jobAtHead {
	var issueRepo *string
	resolved, answered := false, false
	at := map[string]map[ciHeadKey]jobAtHead{}
	mark := func(block string, head delivery.NewestHead, job jobAtHead) {
		if at[block] == nil {
			at[block] = map[ciHeadKey]jobAtHead{}
		}
		at[block][ciHeadKeyOf(head)] = max(at[block][ciHeadKeyOf(head)], job)
	}
	for _, p := range proved {
		for _, result := range p.check.Results {
			if result.Branch == "" || result.NotCounted != "" || result.Retracted != nil || result.CheckSHA256 != p.check.SHA256 {
				continue
			}
			for _, head := range heads {
				if head.HeadSHA != result.CommitSHA {
					continue
				}
				repo := result.CommitRepo
				if repo == nil {
					if !resolved {
						issueRepo, answered = s.checkIssueRepository(ctx, s.deps.Store.Pool, issueKey)
						resolved = true
					}
					if !answered {
						mark(p.check.BlockID, head, jobAtHeadUnresolved)
						continue
					}
					repo = issueRepo
				}
				if repo == nil || !strings.EqualFold(*repo, head.Repo) {
					continue
				}
				mark(p.check.BlockID, head, jobAtHeadRecorded)
			}
		}
	}
	return at
}

// ciHeadsOf is the heads a check CI proves is read at: the head of each open pull request of
// issueKey (delivery.Attributor.HeadsAttributedTo), each in its own row's repository and each
// commit of a repository once, the open pull request of the highest number first, whose read is
// the check's `ci`; where none is open, the one head delivery.NewestPullRequestHead answers, the
// final head of the merged pull request that merged last; and none where Dispatch holds neither.
// Where none is open, the merged pull requests are taken in the order they merged only once the
// fill over the reader's memo (fillMergeTimes, comparisonReader.fills) says they can be: where it
// says they cannot, ordered is false and no head is read, since the head of the highest number, or
// of a merge with no time ordered last, may be an older merge's than the newest. Each read of the
// heads takes the reader's answers (comparisonReader.answers), as every read of the heads on the
// results path does.
func (s *server) ciHeadsOf(ctx context.Context, reader *comparisonReader, issueKey string) ([]delivery.NewestHead, bool, error) {
	attributed, err := s.deps.Attributor.HeadsAttributedTo(ctx, s.deps.Store.Pool, issueKey, reader.answers)
	if err != nil {
		return nil, false, err
	}
	var heads []delivery.NewestHead
	for _, head := range attributed {
		read := delivery.NewestHead{Repo: head.Repo, Number: head.Number, HeadSHA: head.HeadSHA}
		if head.Open && !slices.ContainsFunc(heads, func(held delivery.NewestHead) bool {
			return held.Repo == read.Repo && held.HeadSHA == read.HeadSHA
		}) {
			heads = append(heads, read)
		}
	}
	if len(heads) > 0 {
		slices.SortStableFunc(heads, func(a, b delivery.NewestHead) int { return b.Number - a.Number })
		return heads, true, nil
	}
	merged := make([]delivery.MergeTime, len(attributed))
	for index, head := range attributed {
		merged[index] = head.MergeTime()
	}
	if !s.fillMergeTimes(ctx, &reader.fills, merged) {
		return nil, false, nil
	}
	head, found, err := delivery.NewestPullRequestHead(ctx, s.deps.Store.Pool, issueKey, reader.answers)
	if err != nil || !found {
		return nil, true, err
	}
	return []delivery.NewestHead{head}, true, nil
}

// ciHeadRead is GitHub's answer for the check runs at one head: the run of each name, or the
// error that made the read fail.
type ciHeadRead struct {
	runs map[string]*ciRun
	err  error
}

// readCIHeads reads the check runs of each head (readCIRuns), at most ciHeadReadConcurrency at one
// time, all under one ciReadDeadline: a head whose read did not end by then answers its error,
// and every other head keeps its own answer. Each answer is at the index of its head.
func (s *server) readCIHeads(ctx context.Context, heads []delivery.NewestHead, names map[string]struct{}) []ciHeadRead {
	ctx, cancel := ciReadBound(ctx)
	defer cancel()
	reads := make([]ciHeadRead, len(heads))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(ciHeadReadConcurrency)
	for index, head := range heads {
		group.Go(func() error {
			runs, err := s.readCIRuns(groupCtx, head, names)
			reads[index] = ciHeadRead{runs: runs, err: err}
			return nil
		})
	}
	_ = group.Wait()
	return reads
}

// notCountedOnCICheck marks each result the read loaded for check, a check CI proves whose
// ci_check names the job name, that no run of that job recorded, and it counts for nothing
// (standingCommit) and says why:
//
//   - a role's, a row of check_results, which names no branch, where every row of
//     ci_check_results names one (model.ResultNotCountedRoleOnCICheck). Such a row is older than
//     the check's ci_check, which a document write that raced the result gave the block, and
//     stands against it only until the settlement that holds the block to the fourth rule
//     (docs.CICheckRuleIn) takes that ci_check off;
//   - a CI job's whose run, its check run's name, is not name (model.ResultNotCountedAnotherJob).
//     The check's digest covers its ci_check, and the record route reads the job and the digest
//     from one state of the check (resolveCheck), so no route of this build writes such a row;
//     this is the second line of that binding.
//
// It runs before the derived result is merged in, so that one is never marked.
func notCountedOnCICheck(check *model.CheckBlock, name string) {
	for index := range check.Results {
		switch result := &check.Results[index]; {
		case result.Branch == "":
			result.NotCounted = model.ResultNotCountedRoleOnCICheck
		case result.Run != name:
			result.NotCounted = model.ResultNotCountedAnotherJob
		}
	}
}

// newestResultsFirst orders results as the read of the stored ones does (loadCheckResults), by
// newerResultFirst.
func newestResultsFirst(results []model.CheckResult) []model.CheckResult {
	slices.SortStableFunc(results, newerResultFirst)
	return results
}

// newerResultFirst is the one order of a check's results, the order the results read lists them
// in: newest first by CreatedAt, the higher ID first between two of one time. It is below zero
// where a comes before b, so a was recorded after b. Every choice of "the newest" result among
// several reads it (newestResultsFirst, subjectState), so the list and the results that decide a
// state never disagree on which result is newer.
func newerResultFirst(a, b model.CheckResult) int {
	if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
		return order
	}
	return strings.Compare(b.ID, a.ID)
}

// provedCI is a check CI proves and name, the run its `ci_check` names as the rules read it,
// trimmed (docs.CheckAttributesOfBlock): the block as served keeps `ci_check` as written.
type provedCI struct {
	check *model.CheckBlock
	name  string
}

// readCIRuns reads every page of the check runs of head's commit, in head's own repository,
// under one ciReadDeadline, and answers the run of the highest id of each name in names: GitHub
// keeps every attempt of a run, and the newest decides (classify.mergeAttemptSets in the daemon
// keeps one name's run the same way). A row whose repository is no name GitHub allows
// (refs.SplitGitHubRepo) is the error before any GitHub call: a rename stored before the routes
// held names to that rule can make `acme/..` the canonical name of a row, and its halves would go
// into the paths as they are. A deployment holding no App key, a token GitHub would not mint, an
// HTTP error, a rate limit and the deadline are each the error, worded as GitHub answered it. The
// token never reaches it: it is sent in a header, and no answer names it.
func (s *server) readCIRuns(ctx context.Context, head delivery.NewestHead, names map[string]struct{}) (map[string]*ciRun, error) {
	owner, repo, err := refs.SplitGitHubRepo(head.Repo)
	if err != nil {
		return nil, fmt.Errorf("the pull request row's %w", err)
	}
	ctx, cancel := ciReadBound(ctx)
	defer cancel()
	runs, err := s.readCIRunPages(ctx, owner, repo, head.HeadSHA, names)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("no answer within %s: %w", ciReadDeadline, err)
	}
	return runs, err
}

// readCIRunPages is readCIRuns's own read, pages 1..N until the first page that is not full, as
// cireconcile reads the same listing.
func (s *server) readCIRunPages(ctx context.Context, owner, repo, sha string, names map[string]struct{}) (map[string]*ciRun, error) {
	token, err := s.deps.GitHub.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("mint an installation token: %w", err)
	}
	newest := map[string]*ciRun{}
	for number := 1; number <= ciCheckRunPages; number++ {
		page, err := s.deps.GitHub.CommitCheckRuns(ctx, token, owner, repo, sha, number, "")
		if err != nil {
			return nil, err
		}
		for _, object := range page.Objects {
			name, _ := object["name"].(string)
			if _, wanted := names[name]; !wanted {
				continue
			}
			// encoding/json decodes a number into float64, which holds a check run's id exactly:
			// GitHub's ids are far below 2^53.
			id, _ := object["id"].(float64)
			if held, ok := newest[name]; !ok || int64(id) > held.id {
				newest[name] = ciRunOf(int64(id), name, object)
			}
		}
		if !page.Full {
			return newest, nil
		}
	}
	return nil, fmt.Errorf("the check runs did not end within %d pages", ciCheckRunPages)
}

// readBranchHead asks GitHub for the head of one branch of repo, for the comparison of a
// `before_merge` check (comparisonReader.branchHeadOf): the installation token and the exact read
// of the branch (githubapp.Client.Branch) together under one ciReadDeadline, as a read of one
// head's check runs is bounded, for the same reason.
//
// A deployment holding no App key, an App installed on no repository covering this one, and a
// token refused the repository are model.ReasonRepositoryUnreadable. The branch read's 404, 409
// (an empty repository) and 422 are gone, a branch GitHub does not hold: a repository the App
// cannot see answers 404 too, and is told from a deleted branch by the read of the repository
// itself, which nothingReason and commitStands make (defaultBranchOf). Every other answer — a 403,
// which GitHub also sends for a rate limit, a 5xx, the deadline — is model.ReasonReadFailed. The
// token never reaches the log: it is sent in a header, and no error names it.
func (s *server) readBranchHead(ctx context.Context, repo, branch string) branchHeadAnswer {
	ctx, cancel := ciReadBound(ctx)
	defer cancel()
	owner, name, _ := strings.Cut(repo, "/")
	token, err := s.deps.GitHub.RepositoryToken(ctx, owner, name)
	if err != nil {
		if errors.Is(err, githubapp.ErrNoAppKey) || errors.Is(err, githubapp.ErrNoInstallation) || errors.Is(err, githubapp.ErrRepositoryUnreadable) {
			return branchHeadAnswer{reason: model.ReasonRepositoryUnreadable}
		}
		slog.Warn("dispatch: mint a token to read the head of a branch a CI job pushed", "repo", repo, "branch", branch, "error", err)
		return branchHeadAnswer{reason: model.ReasonReadFailed}
	}
	sha, err := s.deps.GitHub.Branch(ctx, token, owner, name, branch)
	switch {
	case err == nil:
		return branchHeadAnswer{sha: sha}
	case errors.Is(err, githubapp.ErrNoBranch):
		return branchHeadAnswer{gone: true}
	}
	slog.Warn("dispatch: read the head of a branch a CI job pushed", "repo", repo, "branch", branch, "error", err)
	return branchHeadAnswer{reason: model.ReasonReadFailed}
}

// ciUnreadDetail is the detail of a check whose runs at head GitHub could not be read for, err
// the read's error, worded by gitHubErrorText: where GitHub refused or rate-limited the read, the
// error's text ends with GitHub's body as githubapp.ShownBody shows it, its first 512 characters
// of a page of check runs that may take 16 MiB, and that gives way to GitHub's `message`, read
// from the whole body, so the status stays. The sentence is then cut to ciUnreadDetailRunes.
func ciUnreadDetail(head delivery.NewestHead, err error) string {
	return text.HeadRunes(fmt.Sprintf("GitHub could not be read for the check runs of %s at %s: %s.",
		head.Repo, head.HeadSHA, gitHubErrorText(err)), ciUnreadDetailRunes)
}

// gitHubErrorText is the text of err, a GitHub read's error. Where GitHub refused the read
// (githubapp.StatusError) or rate-limited it (githubapp.RateLimitError), the body the error's
// text ends with, as githubapp.ShownBody shows it, is replaced by the `message` GitHub writes in
// the JSON it refuses a request with, read from the whole body, and left out where the body holds
// none (withGitHubMessage). Any other error keeps its text.
func gitHubErrorText(err error) string {
	var refused *githubapp.StatusError
	if errors.As(err, &refused) {
		return withGitHubMessage(err.Error(), githubapp.ShownBody(refused.Body), refused.Message())
	}
	if limited, ok := githubapp.AsRateLimit(err); ok {
		return withGitHubMessage(err.Error(), githubapp.ShownBody(limited.Body), limited.Message())
	}
	return err.Error()
}

// gitHubReason is err's text as a refusal sentence carries it: gitHubErrorText, cut to its first
// gitHubReasonBytes at a whole character (text.HeadBytes) where it is longer, saying so. The cut
// is the one bound of every 502 GITHUB_UNREADABLE that names GitHub's error (getAppInstallation)
// on the whole reason: an error GitHub answered with a page that is no JSON carries the page as
// githubapp.ShownBody shows it, its first 512 characters, which may take 2,048 bytes, and its
// size, after the request's own text, and an error of any other package carries whatever text it
// holds.
func gitHubReason(err error) string {
	reason := gitHubErrorText(err)
	head, cut := text.HeadBytes(reason, gitHubReasonBytes)
	if !cut {
		return reason
	}
	return fmt.Sprintf("%s… (GitHub's error, cut to its first %d of %d bytes)", head, len(head), len(reason))
}

// gitHubReasonBytes is the most bytes of GitHub's error gitHubReason keeps, about a kilobyte:
// room for the status and a message as GitHub writes one, while the refusal stays a sentence a
// caller prints whole.
const gitHubReasonBytes = 1024

// withGitHubMessage is reason, an error's text, with body, which ends it after ": ", replaced by
// ": " and message, or by nothing where message is "". A text that does not end so is kept.
func withGitHubMessage(reason, body, message string) string {
	kept, ends := strings.CutSuffix(reason, body)
	if !ends {
		return reason
	}
	if kept, ends = strings.CutSuffix(kept, ": "); !ends {
		return reason
	}
	if message == "" {
		return kept
	}
	return kept + ": " + message
}

// ciRunOf reads the check run id named name out of the object GitHub listed. A field GitHub
// left null or out reads as its zero value: a run not completed carries no conclusion and no
// completion time, and GitHub Actions fills no output for its own jobs.
func ciRunOf(id int64, name string, object map[string]any) *ciRun {
	run := &ciRun{id: id, name: name}
	run.status, _ = object["status"].(string)
	run.conclusion, _ = object["conclusion"].(string)
	run.htmlURL, _ = object["html_url"].(string)
	if completed, ok := object["completed_at"].(string); ok {
		run.completedAt, _ = time.Parse(time.RFC3339, completed)
	}
	if output, ok := object["output"].(map[string]any); ok {
		run.title, _ = output["title"].(string)
		run.summary, _ = output["summary"].(string)
	}
	if app, ok := object["app"].(map[string]any); ok {
		run.appSlug, _ = app["slug"].(string)
	}
	return run
}

// ciAnswerOf decides one check CI proves from run, the run named name at head, nil where head
// carries none: the `ci` member, which names head and, for a pass or a fail, the id of the result
// derived from run, and that derived result, nil for every other case. recorded says that the
// check's CI job recorded a result of its own for the check at head (jobResultHeads): a run that
// passed or failed there is then the case job_recorded and derives nothing, since its conclusion
// is the whole job's. words is the rule the pull-request body line keeps the run's name and the
// check's words by.
func ciAnswerOf(check model.CheckBlock, name string, head delivery.NewestHead, run *ciRun, recorded bool, words lineWords) (*model.CheckCI, *model.CheckResult) {
	at := fmt.Sprintf("%s, the head of %s#%d", head.HeadSHA, head.Repo, head.Number)
	switch {
	case run == nil:
		return ciUnconcluded(check, name, &head, model.CIStateNoResult, ciCaseAbsent,
			fmt.Sprintf("No CI run named %s is on %s.", name, at), words), nil
	case run.status != ciRunCompleted:
		return ciUnconcluded(check, name, &head, model.CIStateNoResult, ciCaseNotCompleted,
			fmt.Sprintf("CI run %s is %s at %s, and has not concluded.", run.name, run.status, at), words), nil
	}
	switch run.conclusion {
	case ciConclusionNeutral, ciConclusionSkipped, ciConclusionCancelled:
		return ciUnconcluded(check, name, &head, model.CIStateNoResult, run.conclusion,
			fmt.Sprintf("CI run %s concluded %s at %s, which proves nothing.", run.name, run.conclusion, at), words), nil
	}
	if recorded {
		return ciUnconcluded(check, name, &head, model.CIStateNoResult, ciCaseJobRecorded,
			fmt.Sprintf("CI run %s concluded %s at %s, and its job recorded its own result of %s there: the conclusion of the whole job decides no check its job records.",
				run.name, run.conclusion, at, check.BlockID), words), nil
	}
	verdict := model.CIStateFail
	if run.conclusion == ciConclusionSuccess {
		verdict = model.CIStatePass
	}
	result := model.CheckResult{
		ID:       ciResultIDPrefix + strconv.FormatInt(run.id, 10),
		IssueKey: check.IssueKey, CheckIssueKey: check.IssueKey,
		CheckArtifactID: check.ArtifactID, CheckBlockID: check.BlockID,
		CheckVersion: check.Version, CheckSHA256: check.SHA256,
		Role: ciResultRole, Surface: check.Surface, Run: run.name, Observed: ciObserved(*run),
		CommitSHA: head.HeadSHA, NegativeControl: check.NegativeControl, Verdict: verdict,
		Screenshots: []string{}, Actor: model.Actor{Kind: ciResultActorKind, ID: run.appSlug},
		CreatedAt: run.completedAt,
	}
	answer := &model.CheckCI{
		Run: run.name, State: verdict, Case: run.conclusion,
		Detail: fmt.Sprintf("CI run %s concluded %s at %s.", run.name, run.conclusion, at),
		Line:   ciVerdictLine(check, name, verdict, head.HeadSHA, words),
		Commit: head.HeadSHA, Repo: head.Repo, Result: result.ID,
	}
	return answer, &result
}

// ciUnconcluded is the `ci` member of a check its run, the one named name, did not decide, for
// the case given, at head, nil where Dispatch holds no head to read at.
func ciUnconcluded(check model.CheckBlock, name string, head *delivery.NewestHead, state, ciCase, detail string, words lineWords) *model.CheckCI {
	ci := &model.CheckCI{
		Run: name, State: state, Case: ciCase, Detail: detail,
		Line: fmt.Sprintf("- Check %s (%s): %s has no result (%s)", check.BlockID, check.When, ciRunOfLine(name, words), ciCase),
	}
	if head != nil {
		ci.Commit, ci.Repo = head.HeadSHA, head.Repo
	}
	return ci
}

// ciRunOfLine is the run a check's ci_check names as a pull-request body line names it: `CI run`
// and the name's words joined by single blanks, when the line drops none of them (lineWords.drops).
// The name is text the check's author wrote, so where the line drops any of its words, or the name
// has none, the line says `the CI run the check names` instead. The words left of a name that lost
// some are a run nobody can find (`0)` of `unit_tests (test_x.py, 0)`), and two such runs can read
// the same.
func ciRunOfLine(name string, words lineWords) string {
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return "the CI run the check names"
	}
	for _, word := range fields {
		if words.drops(word) {
			return "the CI run the check names"
		}
	}
	return "CI run " + strings.Join(fields, " ")
}

// ciVerdictLine is the pull-request body line of a derived result: the recorded line's grammar,
// with the run that proved it (ciRunOfLine), where a recorded line names its recorder, and the
// check's own words where a recorded line carries the role's reading, since no role read anything.
// The words are kept by words and cut as checkResultLine keeps and cuts a reading; a check of whose
// words nothing is left after the cut ends the line at the run, as a check with no words does.
func ciVerdictLine(check model.CheckBlock, name, verdict, sha string, words lineWords) string {
	line := fmt.Sprintf("- Check %s (%s): %s at %s, from %s.", check.BlockID, check.When, verdict, sha, ciRunOfLine(name, words))
	tail := words.cut(words.keepThrough(check.Text, checkResultReadingRunes))
	if tail == "" {
		return line
	}
	return line + " " + tail
}

// ciObserved is what a derived result says was observed, one part a line: the run's conclusion,
// then its output title and summary together cut to ciObservedOutputRunes, then the run's URL. A
// part GitHub left empty is left out.
func ciObserved(run ciRun) string {
	parts := []string{"conclusion: " + run.conclusion}
	output := run.title
	if output != "" && run.summary != "" {
		output += "\n"
	}
	output += run.summary
	if output != "" {
		parts = append(parts, text.HeadRunes(output, ciObservedOutputRunes))
	}
	if run.htmlURL != "" {
		parts = append(parts, run.htmlURL)
	}
	return strings.Join(parts, "\n")
}
