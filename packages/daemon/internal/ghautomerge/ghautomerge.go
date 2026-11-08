// Package ghautomerge submits and disarms a pull request's merge through GitHub's GraphQL API: the
// way the daemon merges an accepted READY head under the repository's own rules, and makes sure a
// pull request armed for one READY head never merges another.
package ghautomerge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// Disarmed is what Disarm found and turned off.
type Disarmed struct {
	// Missing is GitHub answering NOT_FOUND for the pull request itself: there is nothing to disarm.
	Missing bool
	// Kept is the pull request's head being the READY head Disarm was told to keep: whatever is
	// armed there is that READY's, and is left as it is.
	Kept bool
	// AutoMerge is an auto-merge Disarm disabled.
	AutoMerge bool
	// Queue is a merge-queue entry Disarm removed.
	Queue bool
}

const read = `query PullRequestAutoMerge($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    autoMergeAllowed
    pullRequest(number: $number) {
      id state headRefOid mergeStateStatus updatedAt isMergeQueueEnabled
      autoMergeRequest { enabledAt } mergeQueueEntry { enqueuedAt state baseCommit { oid } }
      baseRef { name target { oid } }
    }
  }
}`

const (
	disable = `mutation DisableAutoMerge($id: ID!) { disablePullRequestAutoMerge(input: {pullRequestId: $id}) { clientMutationId } }`
	dequeue = `mutation DequeuePullRequest($id: ID!) { dequeuePullRequest(input: {id: $id}) { clientMutationId } }`
	// The enable and the merge pass no commit headline and no commit body, so each repository's own
	// squash settings decide the message a merge lands with, and this package decides none of it.
	//
	// On a merge-queue base GitHub ignores any commit message input anyway
	// (EnablePullRequestAutoMergeInput says so).
	enable = `mutation EnableAutoMerge($id: ID!, $head: GitObjectID!, $method: PullRequestMergeMethod!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id, expectedHeadOid: $head, mergeMethod: $method}) { clientMutationId }
}`
	merge = `mutation MergeREADYHead($id: ID!, $head: GitObjectID!, $method: PullRequestMergeMethod!) {
  mergePullRequest(input: {pullRequestId: $id, expectedHeadOid: $head, mergeMethod: $method}) { clientMutationId }
}`
)

// Outcome is how Submit ended: a submission it asks to run again is a wait it returns instead.
type Outcome string

const (
	// Missing is GitHub answering NOT_FOUND for the pull request itself: there is nothing to merge.
	Missing Outcome = "missing"
	// Stale is a submission the record no longer covers: the issue no longer awaits READY's head.
	Stale Outcome = "stale"
	// Already is a pull request GitHub already merged at READY's head.
	Already Outcome = "already"
	// Merged is GitHub merging READY's head now.
	Merged Outcome = "merged"
)

// Keep answers the READY head an arm is legitimate for, empty when none is. Disarm asks it only
// after reading GitHub.
type Keep func(context.Context) (string, error)

// Disarm turns off what would merge pull request number of repo without a person, through github,
// a GraphQL client (githubrest.GraphQLAPI): its auto-merge where GitHub has it enabled, then its
// merge-queue entry where it has one. Auto-merge goes first because GitHub enqueues again a pull
// request it dequeues while auto-merge is still enabled; the dequeue is attempted even when the
// disable fails, so a refused disable never leaves the entry standing, and both failures are
// returned. It then reads the pull request once more and fails, so its row retries, when anything
// not kept is still armed: a mutation GitHub answered is not proof the merge is off.
//
// It decides by the pull request's state when it runs, never by when anything was armed: GitHub
// enqueues an armed pull request only once its checks pass, so a queue entry can postdate the arm
// it belongs to. It reads GitHub first and then asks keep for the READY head an arm is legitimate
// for: an arm is made only once its READY is recorded, so an arm the read finds is covered by what
// keep answers after it, where asked the other way round a READY recorded between the two would
// have its arm turned off. While the pull request's head on GitHub is that head, nothing is
// touched (Kept). A pull request with nothing armed is left alone, so a retried Disarm changes
// nothing. A NOT_FOUND at the pull request is Missing, not an error; one anywhere else, such as a
// repository the token cannot see, is an error.
func Disarm(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int, keep Keep) (Disarmed, error) {
	pull, err := readPull(ctx, github, repo, number)
	if err != nil {
		return Disarmed{}, err
	}
	if pull == nil {
		return Disarmed{Missing: true}, nil
	}
	if !pull.armed() {
		return Disarmed{}, nil
	}
	head, err := keep(ctx)
	if err != nil {
		return Disarmed{}, err
	}
	if pull.kept(head) {
		return Disarmed{Kept: true}, nil
	}
	var disarmed Disarmed
	var failures []error
	if pull.AutoMergeRequest != nil {
		if err := github.GraphQL(ctx, disable, map[string]any{"id": pull.ID}, nil); err != nil {
			failures = append(failures, fmt.Errorf("disable the auto-merge of pull request #%d of %s: %w", number, repo, err))
		} else {
			disarmed.AutoMerge = true
		}
	}
	if pull.MergeQueueEntry != nil {
		if err := github.GraphQL(ctx, dequeue, map[string]any{"id": pull.ID}, nil); err != nil {
			failures = append(failures, fmt.Errorf("take pull request #%d of %s out of the merge queue: %w", number, repo, err))
		} else {
			disarmed.Queue = true
		}
	}
	if len(failures) != 0 {
		return disarmed, errors.Join(failures...)
	}
	after, err := readPull(ctx, github, repo, number)
	if err != nil || after == nil || !after.armed() || after.kept(head) {
		return disarmed, err
	}
	return disarmed, fmt.Errorf("pull request #%d of %s is still armed after its disarm (auto-merge enabled: %t, in the merge queue: %t)",
		number, repo, after.AutoMergeRequest != nil, after.MergeQueueEntry != nil)
}

// FailingRun is one check that failed on the commit the merge queue built for a pull request its
// merge queue took out: its Name, how it ended (Result: a check run's conclusion as Legion's rule
// for one reads it, classify.RunResult, or a commit status's state), and URL, the run it failed
// in. JobID is the Actions job the check ran as, GitHub's databaseId for a check run its workflow
// run is named with, and zero for everything that is no Actions job — a third-party App's check
// run and a commit status among them — neither of which has an Actions log to read.
//
// Log is what the log of that job said, read after the removal is decided and only where the row
// is about to tell of it (ReadFailingTests, the daemon's readyMerge): the tests its end states
// failed with the first line stating one, or which way the daemon could not read them, written as
// the end of this run's own line. A run whose log has not been read carries none, and its line is
// what it would have been before any log was read: deciding a removal reads no log at all.
type FailingRun struct {
	Name, Result, URL string
	JobID             int64
	Log               string
}

// String is the one line a removal names this run with: the check, how it ended and the run it
// failed in, with what its log said after them where it has been read.
//
// Name, Result and URL come out of GitHub's own check list, and this is the one place any of
// them is written, so all three pass through cutLine here: that is the rule every string of
// GitHub's this package renders is held to, one cut each at one place (jobFailures). Log
// arrives cut, by the read that wrote it. The cut costs GitHub's own run link nothing, since
// cutLine leaves a string of failureLineUnits runes or fewer exactly as it stands and that
// link is of a fixed shape and some seventy characters. What it holds is the link of no fixed
// shape: a third-party App's check run is named by the details URL that App wrote and a commit
// status by its target URL (failingRuns), each as long as whoever posted the check made it,
// and the first run of a removal is named however long its line is (FailingRuns).
func (f FailingRun) String() string {
	return fmt.Sprintf("%s (%s, %s)%s", cutLine(f.Name), cutLine(f.Result), cutLine(f.URL), f.Log)
}

// failingRunsUnits is how long the runs of one removal are named for before the rest are
// counted ("and 12 more runs"), measured in the bytes of the text, which is never fewer than
// its UTF-16 units.
//
// Every sentence this package writes is rendered into one block of a Dispatch post — the
// implementer's task and the architect's checks-red notice a failed merge sends, the READY
// withdrawal's post, the re-submission's, and merge-removed — and Dispatch refuses a block over
// DISPATCH_BLOCK_UNITS, 32,000 UTF-16 units (packages/contracts), naming it; the daemon then
// tells the architect its message was refused, which is the failure this bound exists to spare
// it. One job's line is bounded in itself (failingTestsNamed, failureLineUnits): some 2,400
// units of names and prose as the read writes them, and 22,103 once they are quoted — 22,121
// with the tail that counts the names it did not state — which is the count a Dispatch post is
// measured in. The run's own name, result and link are cut here and never quoted
// (FailingRun.String), so each costs at most 2 × failureLineUnits + 1 = 401 units, a rune
// outside the BMP, an emoji in a check's name among them, being two UTF-16 units, which is
// 3 × 401 + 5 = 1,208 for the three of them with the " (", ", " and ")" around them. A merge
// group a broken base fails across twenty jobs names twenty such lines. Sixteen thousand holds
// every removal's runs to half that block, which leaves the sentence they stand in, and the
// prose around it, the other half. The one line this bound gives way to is the first run's,
// which is named however long it is: a first line of that quoted worst case runs to
// 22,121 + 1,208 = 23,329 units and every run after it is counted, so a removal's runs stay
// inside the block whichever way they run.
const failingRunsUnits = 16000

// FailingRuns is runs as every removal that carries them names them, and as the daemon's own
// messages of one do: each run's line, "; " between them, up to failingRunsUnits of text, with
// the rest counted after them ("and 12 more runs"). The first run is always named, however long
// its own line is: a bound that named none would leave a removal saying nothing of what failed.
func FailingRuns(runs []FailingRun) string {
	var named strings.Builder
	for i, run := range runs {
		line := run.String()
		if i > 0 && named.Len()+len(line) > failingRunsUnits {
			fmt.Fprintf(&named, " and %d more runs", len(runs)-i)
			break
		}
		if i > 0 {
			named.WriteString("; ")
		}
		named.WriteString(line)
	}
	return named.String()
}

// Submit asks GitHub to merge pull request number of repo at head, the head an accepted READY
// named, through github, a GraphQL client. It reads GitHub first and then asks keep for the READY
// head the record still awaits, so a READY withdrawn or left between the two submits nothing. A
// base that requires a merge queue gets the pull request enqueued once GitHub reports it mergeable
// now, a pull request GitHub reports mergeable now is merged, and a repository that allows
// auto-merge gets it armed; every mutation carries head as GitHub's expected head. A base with a
// merge queue waits until GitHub reports the pull request mergeable (Unmergeable), a required check
// still running among what holds it back: there GitHub's enable arms auto-merge on a pull request
// it does not report mergeable rather than enqueueing it, and a required check may judge the arm,
// so Submit never arms one. A repository that allows none of those waits the same way. GitHub's
// own rules decide each answer; Submit bypasses none.
//
// Only GitHub merging the pull request at head ends a submission, or the record leaving that READY
// (Stale). An arm or a merge-queue entry at head is a wait (Armed), not the end. armed says the row
// has seen one at head before this run: an open pull request at head with nothing armed after that
// is a removal someone made, which Submit reads from GitHub's timeline (removal) and never answers
// with a second submission, which for a merge the queue failed would queue the same failure again
// and rebuild every entry behind it. The merge queue taking the pull request out because its merge
// failed is MergeFailed, and any other removal, a person's dequeue or turning auto-merge off, is
// Removed; only a removal that restarts the submission submits again: the submitting App's own, the
// daemon's disarm landing late, and auto-merge a close turned off, undone by the reopen at head.
// The queue's removal for a conflict is a ConflictRemoval, that same removal with the merge state
// this read answered for the pull request (pull.MergeStateStatus, which GitHub computes once the
// entry is no longer queued): the conflict can be with the base itself or with an entry ahead of
// it in the queue, and only that state tells them apart, so Submit leaves the two to its caller
// rather than ending the submission on either.
// While the record still awaits head, a pull request GitHub shows closed, merged at another head, or
// at another head is a wait too (Lagging): the webhook that moves the record has not been applied
// yet, or GitHub's read is the one behind, and the next run decides again. GitHub always answers
// updatedAt, so a read without it fails rather than leaving the wait unbounded.
//
// A mutation GitHub refuses is read again: when GitHub still stands where it refused it (the same
// open pull request at head, nothing armed, the same submission chosen), a retry would meet the
// same refusal, and Submit returns it as *Refused. When that read fails, its error is returned,
// the refusal as its context, so a rate limit it meets reaches the caller (githubrest.RetryAfter).
// Any other failure, an HTTP or network one included, is returned as it is, for the row to retry.
//
// github is GitHub's GraphQL route, and the one client Submit calls: deciding a submission reads
// and changes the pull request and nothing else, so a removal it reads names each failing run
// with its Actions job (MergeFailed, Removed, ConflictRemoval) and leaves the log of that job to
// its caller, which reads it once the decision is made and only where it tells of it
// (ReadFailingTests). A read of a 54 MB log cannot finish inside a merge row's own budget, so
// deciding a removal must never wait on one.
func Submit(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int, head string, armed bool, keep Keep) (Outcome, error) {
	answer, err := readRepository(ctx, github, repo, number)
	if err != nil {
		return "", err
	}
	pull := answer.PullRequest
	switch {
	case pull == nil:
		return Missing, nil
	case pull.State == "MERGED" && pull.HeadRefOid == head:
		return Already, nil
	}
	kept, err := keep(ctx)
	if err != nil {
		return "", err
	}
	switch {
	case kept != head:
		return Stale, nil
	case !pull.openAt(head) && pull.UpdatedAt.IsZero():
		return "", fmt.Errorf("GitHub's read of pull request #%d of %s carries no updatedAt to bound its disagreement with the record", number, repo)
	case !pull.openAt(head):
		return "", &Lagging{Number: number, Head: head, State: pull.State, GitHubHead: pull.HeadRefOid, Since: pull.UpdatedAt}
	case pull.armed():
		return "", pull.armedWait(repo, number, head)
	case armed:
		if err := removal(ctx, github, repo, number, head, pull.MergeStateStatus); err != nil {
			return "", err
		}
	}
	chosen, ok := choose(answer)
	if !ok {
		return "", &Unmergeable{Number: number, Repo: repo, Head: head, State: pull.MergeStateStatus, Queue: pull.IsMergeQueueEnabled}
	}
	variables := map[string]any{"id": pull.ID, "head": head, "method": "SQUASH"}
	sent := github.GraphQL(ctx, chosen.mutation, variables, nil)
	switch {
	case sent == nil && chosen.merges:
		return Merged, nil
	case sent == nil:
		// pull is the read before the mutation, where nothing was armed, so it carries no
		// merge-queue entry at all: the entry's state and base commit stay empty rather than
		// invented, and the next run reads the entry GitHub has made of this arm.
		return "", pull.armedWait(repo, number, head)
	}
	failed := fmt.Errorf("%s pull request #%d of %s at READY's head %s: %w", chosen.action, number, repo, head, sent)
	var refusal *githubrest.GraphQLError
	if !errors.As(sent, &refusal) || !terminalMutationRefusal(refusal) {
		return "", failed
	}
	after, err := readRepository(ctx, github, repo, number)
	if err != nil {
		return "", fmt.Errorf("read pull request #%d of %s again after GitHub's refusal to %v: %w", number, repo, failed, err)
	}
	if after.PullRequest == nil || !after.PullRequest.openAt(head) || after.PullRequest.armed() {
		return "", failed
	}
	if again, ok := choose(after); !ok || again != chosen {
		return "", failed
	}
	return "", &Refused{Number: number, Head: head, Action: chosen.action, Message: refusal.Error()}
}

// terminalMutationRefusal reports GraphQL mutation errors that no backoff can repair. GitHub's
// rate-limit, service and unknown error types retry: only a confirmed unchanged pull request plus
// one of these terminal rejections becomes Refused.
func terminalMutationRefusal(refusal *githubrest.GraphQLError) bool {
	if len(refusal.Failures) == 0 {
		return false
	}
	for _, failure := range refusal.Failures {
		switch failure.Type {
		case "FORBIDDEN", "UNPROCESSABLE", "VALIDATION_FAILED":
		default:
			return false
		}
	}
	return true
}

// Refused is GitHub refusing Submit's mutation where a retry would meet the same refusal: Action is
// what Submit asked of pull request Number at READY's Head, and Message is GitHub's answer.
type Refused struct {
	Number                int
	Head, Action, Message string
}

func (r *Refused) Error() string {
	return fmt.Sprintf("GitHub refused to %s pull request #%d at READY's head %s and still stands where it refused, so a retry meets the same refusal: %s",
		r.Action, r.Number, r.Head, r.Message)
}

// LagBound is how long GitHub's last change to a pull request stands before a disagreement with the
// record is a webhook the daemon never received rather than one still on its way: far longer than
// GitHub takes to deliver one.
const LagBound = 10 * time.Minute

// Lagging is GitHub showing pull request Number in State at GitHubHead while the record still awaits
// its merge at READY's Head: a close or a push whose webhook the record has not applied yet, or a
// GitHub read behind the record. Since is GitHub's last change to the pull request, from the same
// read. It is a wait (wait.ErrWaiting): the next run reads both again, until it is Stalled.
type Lagging struct {
	Number                  int
	Head, State, GitHubHead string
	Since                   time.Time
}

func (l *Lagging) Error() string {
	return fmt.Sprintf("GitHub shows pull request #%d %s at %s while the record still awaits its merge at READY's head %s: waiting for the two to agree",
		l.Number, l.State, l.GitHubHead, l.Head)
}

func (l *Lagging) Is(target error) bool { return target == wait.ErrWaiting }

// Stalled says whether at now the disagreement has stood on GitHub longer than LagBound: GitHub's
// own clock bounds it, never how often the row has tried.
func (l *Lagging) Stalled(now time.Time) bool { return now.Sub(l.Since) > LagBound }

// Unmergeable is GitHub reporting pull request Number of Repo in merge State at READY's Head, not
// mergeable yet, on a base that requires a merge queue (Queue), where enabling auto-merge now would
// arm it rather than enqueue it, or in a repository that allows no auto-merge: Submit enqueues or
// merges it once GitHub reports it mergeable. It is a wait (wait.ErrWaiting).
type Unmergeable struct {
	Number      int
	Repo        ghrepo.Repository
	Head, State string
	Queue       bool
}

func (u *Unmergeable) Error() string {
	if u.Queue {
		return fmt.Sprintf("pull request #%d of %s is %s at READY's head %s, and its base requires a merge queue, where enabling auto-merge before GitHub reports it mergeable arms it instead of enqueueing it: the daemon enqueues it once GitHub reports it mergeable",
			u.Number, u.Repo, u.State, u.Head)
	}
	return fmt.Sprintf("pull request #%d of %s is %s at READY's head %s, and the repository allows no auto-merge: the daemon merges it once GitHub reports it mergeable",
		u.Number, u.Repo, u.State, u.Head)
}

func (u *Unmergeable) Is(target error) bool { return target == wait.ErrWaiting }

// Routine says whether the repository's own rules end the wait without anyone acting for Legion: a
// required check still running or a required review (BLOCKED), or GitHub still computing the merge
// state (UNKNOWN). A branch to update (BEHIND), a draft (DRAFT) or a conflict (DIRTY) waits on
// someone.
func (u *Unmergeable) Routine() bool { return u.State == "BLOCKED" || u.State == "UNKNOWN" }

// Armed is pull request Number of Repo armed for auto-merge, or in its merge queue, at READY's
// Head: GitHub merges it under the repository's own rules. It is a wait (wait.ErrWaiting) that
// ends once GitHub reports the pull request merged at Head; an arm or entry that disappears
// before then is a removal (MergeFailed, Removed), never submitted again unless the submitting App
// made it.
type Armed struct {
	Number int
	Repo   ghrepo.Repository
	Head   string
	// QueueState is the state GitHub's merge queue has the entry in (QUEUED, AWAITING_CHECKS,
	// MERGEABLE, UNMERGEABLE, LOCKED), and is empty where the read found no entry: an auto-merge
	// arm alone, or an entry GitHub made after the read.
	QueueState string
	// QueueBase is the commit the merge queue built the entry's merge on, and is empty where the
	// read found no entry or the queue had built nothing for it yet. For an entry behind another
	// it is the commit the queue built for the entry ahead, whose own parent is the base branch's
	// tip — never that entry's pull-request head. Measured 2026-11-08 00:43:57Z on this
	// repository: main stood at 1f46e1cc76f4, entry 1 was #2234 AWAITING_CHECKS on baseCommit
	// 1f46e1cc76f4, and entry 2 was #2859 UNMERGEABLE on baseCommit b66b78d6d9f3, which is no
	// pull request's head — #2234's head was 7c4463b8ac8b — but the commit the queue built for
	// #2234, carrying its title and the one parent 1f46e1cc76f4.
	QueueBase string
	// BaseName is the branch the pull request merges into, as the same read named it.
	BaseName string
	// BaseTip is the commit that branch was at in the same read, which the queue moves on as it
	// merges what stands before the entry.
	BaseTip string
}

func (a *Armed) Error() string {
	return fmt.Sprintf("pull request #%d of %s is armed for auto-merge or in its merge queue at READY's head %s: the daemon waits for GitHub to merge it",
		a.Number, a.Repo, a.Head)
}

func (a *Armed) Is(target error) bool { return target == wait.ErrWaiting }

// Routine is always true: only the repository's own rules hold an armed pull request back.
func (a *Armed) Routine() bool { return true }

// UnmergeableOnBaseTip says whether GitHub's merge queue reports the entry unmergeable
// (QueueState UNMERGEABLE) while it has built that entry on the tip of the pull request's own
// base branch (QueueBase, BaseTip, both from the one read): the one state in which nothing ahead
// of the entry in the queue can be what the queue cannot merge, so the head itself is. An entry
// with something ahead of it carries the commit the queue built for the entry ahead instead,
// which is the base branch's tip with that entry's merge on top and so never equal to the tip. An
// entry the queue has built nothing for yet answers false, as every other state does.
func (a *Armed) UnmergeableOnBaseTip() bool {
	return a.QueueState == "UNMERGEABLE" && a.QueueBase != "" && a.QueueBase == a.BaseTip
}

// MergeFailed is the merge queue taking pull request Number out at READY's Head because its merge
// failed: GitHub's removal for Reason failed_checks, or for a reason GitHub gives no other meaning
// with a run that failed on Commit, the commit the queue built for it (its merge_group runs ran on
// gh-readonly-queue/<base>/pr-<n>-…, a branch GitHub deletes with the entry). Runs names each check
// that failed there, with its run's URL and the Actions job it ran as, and what that job's log
// says failed where its caller has read it (ReadFailingTests); Event is GitHub's id for the
// removal, and At when it was made. The submission is over: the issue goes back to implementing
// on it.
type MergeFailed struct {
	Number               int
	Head, Reason, Commit string
	Runs                 []FailingRun
	Event                string
	At                   time.Time
}

func (f *MergeFailed) Error() string {
	text := fmt.Sprintf("the merge queue took pull request #%d out at READY's head %s at %s (%s)", f.Number, f.Head, f.At.UTC().Format(time.RFC3339), f.Reason)
	switch {
	case len(f.Runs) > 0:
		return text + fmt.Sprintf(": its merge_group run on %s, the commit the queue built for it, failed: %s", f.Commit, FailingRuns(f.Runs))
	case f.Commit != "":
		return text + fmt.Sprintf(": GitHub reported no failing run on %s, the commit the queue built for it", f.Commit)
	default:
		return text + ": GitHub named no commit the queue built for it"
	}
}

// Removed is pull request Number taken out of the merge queue (Queue), or its auto-merge turned
// off, at READY's Head without a merge, not because its merge failed (MergeFailed) and not by the
// submitting App's own disarm: by a person, Actor, the account GitHub names ("" when it names none),
// or by GitHub on its own (ByGitHub), which names the App that submitted the merge as Actor of its
// own disables (a merge method the repository blocks, a base switch). Reason is GitHub's reason: a
// queue removal's, or an auto-merge disable's reason code, with Detail its sentence for it (none for
// a queue removal). At is when it was made. Runs names each check that failed on the commit the
// queue built for a queue removal, with its run's URL, the Actions job it ran as and what that
// job's log says failed where its caller has read it (ReadFailingTests), from the page of that
// commit's checks GitHub's timeline answer carried (removal); it is empty where nothing failed,
// which is what a removal GitHub explains by itself usually leaves. The submission is over: the
// removal stands, and the row ends rather than undo it.
type Removed struct {
	Number                      int
	Head, Actor, Reason, Detail string
	Queue, ByGitHub             bool
	Runs                        []FailingRun
	At                          time.Time
}

func (r *Removed) Error() string {
	at := r.At.UTC().Format(time.RFC3339)
	text := fmt.Sprintf("turned off the auto-merge of pull request #%d at READY's head %s at %s (%s: %s)", r.Number, r.Head, at, r.Reason, r.Detail)
	if r.Queue {
		text = fmt.Sprintf("took pull request #%d out of the merge queue at READY's head %s at %s (%s)", r.Number, r.Head, at, r.Reason)
	}
	switch {
	case r.ByGitHub:
		text = fmt.Sprintf("GitHub %s on its own, naming %s, the App that submitted it,", text, r.Actor)
	case r.Actor == "":
		text = "someone " + text
	default:
		text = r.Actor + " " + text
	}
	return text + " without merging it: the daemon submits it no further, and the issue stays in awaiting_merge" + r.failures()
}

// failures is Failures as a sentence of its own after the text a removal is told in, and nothing
// at all where nothing failed: the two errors this package renders a removal as both end with it.
func (r *Removed) failures() string {
	if failures := r.Failures(); failures != "" {
		return ". " + failures
	}
	return ""
}

// Failures is the sentence a removal's failing runs are told in, wherever one is told of: the
// checks that failed on the commit the merge queue built for the pull request, each as a line of
// its own (FailingRuns). It is empty where that commit failed no run at all, which is what a
// removal GitHub explains by itself usually leaves.
//
// The sentence is written here and nowhere else: this removal's own Error carries it, and so does
// the message the daemon posts when it submits the merge of such a removal once more (its
// readyMergeResubmitted), which is the same fact told on the issue instead of to the architect.
func (r *Removed) Failures() string {
	if len(r.Runs) == 0 {
		return ""
	}
	return fmt.Sprintf("These checks failed on the commit the queue built for it: %s", FailingRuns(r.Runs))
}

// ConflictRemoval is the merge queue taking a pull request out at READY's head because the merge
// conflicted (GitHub's reason merge_conflict): Removal is that removal as every other one is
// reported, and MergeState is GitHub's merge state for the pull request from the read that found
// nothing armed, which GitHub computes freely once the entry is no longer queued (it answers
// UNKNOWN while one is). The two conflicts GitHub gives the one reason are told apart by that
// state alone: a head that conflicts with the base itself reads DIRTY, where the merge is over
// and the removal stands, and a head GitHub still merges cleanly conflicted with an entry ahead
// of it in the queue, which usually failed no check and which nothing in Legion would ever submit
// again. Deciding between them is the daemon's (its conflictRemoval), so this is deliberately no
// *Removed and answers errors.As no *Removed either: the row's own rule that a *Removed ends the
// row (the daemon's permanentRefusal) must not reach it. Ended is the ending the daemon chooses,
// as the *Removed its architect's notice is written from.
type ConflictRemoval struct {
	Removal    Removed
	MergeState string
}

func (c *ConflictRemoval) Error() string {
	return fmt.Sprintf("the merge queue took pull request #%d out at READY's head %s at %s (%s) while GitHub reports its merge state %s: the daemon decides by that state whether the head conflicts with the base itself or with an entry ahead of it in the queue",
		c.Removal.Number, c.Removal.Head, c.Removal.At.UTC().Format(time.RFC3339), c.Removal.Reason, c.MergeState) + c.Removal.failures()
}

// Ended is the removal as the end of the submission, for the daemon that decides the merge is
// over: the *Removed every other removal of an arm is reported as, which the architect's notice
// of a merge row ended without a merge is written from.
func (c *ConflictRemoval) Ended() *Removed {
	removal := c.Removal
	return &removal
}

// WithBase says whether GitHub's merge state has the head conflicting with the base itself
// (DIRTY), the one of the two conflicts GitHub's single reason covers that no further submission
// can merge.
func (c *ConflictRemoval) WithBase() bool { return c.MergeState == "DIRTY" }

// Uncomputed says whether GitHub has computed no merge state for the pull request yet (UNKNOWN),
// which tells the two conflicts apart not at all: GitHub answers it while the entry is still
// queued, and computes the state freely once it is out.
func (c *ConflictRemoval) Uncomputed() bool { return c.MergeState == "UNKNOWN" }

// Merging is the merge queue having taken pull request Number of Repo out at READY's Head to merge
// it, after the row saw it queued there, while GitHub has yet to show it merged. It is a wait
// (wait.ErrWaiting) on GitHub's own merge.
type Merging struct {
	Number int
	Repo   ghrepo.Repository
	Head   string
}

func (m *Merging) Error() string {
	return fmt.Sprintf("the merge queue took pull request #%d of %s out at READY's head %s to merge it: the daemon waits for GitHub to show it merged",
		m.Number, m.Repo, m.Head)
}

func (m *Merging) Is(target error) bool { return target == wait.ErrWaiting }

// Routine is always true: only GitHub's own merge holds the wait back.
func (m *Merging) Routine() bool { return true }

// Disappeared is pull request Number of Repo open at READY's Head with nothing armed after the row
// saw it armed or queued there, while GitHub's timeline names no removal since: none at all yet, or
// GitHub's read of the pull request ahead of its timeline. It is a wait (wait.ErrWaiting), and not a
// routine one: a removal the timeline never explains waits on someone.
type Disappeared struct {
	Number int
	Repo   ghrepo.Repository
	Head   string
}

func (d *Disappeared) Error() string {
	return fmt.Sprintf("pull request #%d of %s is no longer armed or queued at READY's head %s, and its timeline names no removal since: the daemon waits for GitHub to record one",
		d.Number, d.Repo, d.Head)
}

func (d *Disappeared) Is(target error) bool { return target == wait.ErrWaiting }

// submission is the mutation Submit sends for a pull request, what it asks GitHub to do, and
// whether GitHub's acceptance of it merges the pull request now.
type submission struct {
	mutation, action string
	merges           bool
}

// choose is the submission GitHub's answer calls for: an enqueue on a base that requires a merge
// queue once GitHub reports the pull request mergeable now, a merge of a pull request mergeable now
// anywhere else, an arm in a repository that allows auto-merge, and none otherwise. A merge-queue
// base gets no arm: GitHub's enable enqueues a pull request it reports mergeable now and arms
// auto-merge on any other.
func choose(answer repository) (submission, bool) {
	pull := answer.PullRequest
	switch {
	case pull.IsMergeQueueEnabled && mergeableNow(pull.MergeStateStatus):
		return submission{enable, "enqueue", false}, true
	case pull.IsMergeQueueEnabled:
		return submission{}, false
	case mergeableNow(pull.MergeStateStatus):
		return submission{merge, "merge", true}, true
	case answer.AutoMergeAllowed:
		return submission{enable, "enable auto-merge for", false}, true
	default:
		return submission{}, false
	}
}

// mergeableNow is GitHub's merge states in which the pull request merges without waiting, the
// states gh pr merge --auto merges at once instead of arming.
func mergeableNow(state string) bool {
	switch state {
	case "CLEAN", "HAS_HOOKS", "UNSTABLE":
		return true
	default:
		return false
	}
}

// repository is what Disarm and Submit read of a repository and its pull request.
type repository struct {
	AutoMergeAllowed bool         `json:"autoMergeAllowed"`
	PullRequest      *pullRequest `json:"pullRequest"`
}

// pullRequest is what Disarm and Submit read of a pull request: its node id, state, head, merge
// state, GitHub's last change to it, whether its base requires a merge queue, whether auto-merge
// is enabled and what entry it has in the merge queue, and the branch it merges into with that
// branch's tip. Nothing of the squash commit's message is read, since Submit passes none; the
// enable and merge mutations above say what decides it.
type pullRequest struct {
	ID                  string           `json:"id"`
	State               string           `json:"state"`
	HeadRefOid          string           `json:"headRefOid"`
	MergeStateStatus    string           `json:"mergeStateStatus"`
	UpdatedAt           time.Time        `json:"updatedAt"`
	IsMergeQueueEnabled bool             `json:"isMergeQueueEnabled"`
	AutoMergeRequest    *struct{}        `json:"autoMergeRequest"`
	MergeQueueEntry     *mergeQueueEntry `json:"mergeQueueEntry"`
	// BaseRef is the branch the pull request merges into, nil where GitHub has none for it (a base
	// branch someone deleted), with Target the commit that branch is at, nil the same way.
	BaseRef *struct {
		Name   string `json:"name"`
		Target *struct {
			Oid string `json:"oid"`
		} `json:"target"`
	} `json:"baseRef"`
}

// mergeQueueEntry is the pull request's place in its base's merge queue: when GitHub enqueued it,
// the state the queue has it in (QUEUED, AWAITING_CHECKS, MERGEABLE, UNMERGEABLE, LOCKED), and
// baseCommit, the commit the queue built this entry's merge on, nil until the queue has built
// one. For the first entry that commit is the base branch's own tip; for an entry behind another
// it is the commit the queue built for the entry ahead, whose parent is that tip — it is no pull
// request's head, and reading it as one would name the wrong commit in every notice. Measured
// 2026-11-08 00:43:57Z on this repository: entry 2, #2859, carried baseCommit b66b78d6d9f3, which
// is the queue's own build for entry 1 (#2234, head 7c4463b8ac8b) on main's tip 1f46e1cc76f4. An
// entry at all says the pull request is queued, whatever its state, which is what armed answers.
type mergeQueueEntry struct {
	EnqueuedAt time.Time `json:"enqueuedAt"`
	State      string    `json:"state"`
	BaseCommit *struct {
		Oid string `json:"oid"`
	} `json:"baseCommit"`
}

func (p *pullRequest) armed() bool { return p.AutoMergeRequest != nil || p.MergeQueueEntry != nil }

// armedWait is the wait for GitHub's own merge of the pull request at head, from this one read:
// the merge-queue entry's state and base commit where the read found an entry, and the base
// branch's name and tip where it found a base branch.
func (p *pullRequest) armedWait(repo ghrepo.Repository, number int, head string) *Armed {
	armed := &Armed{Number: number, Repo: repo, Head: head}
	if entry := p.MergeQueueEntry; entry != nil {
		armed.QueueState = entry.State
		if entry.BaseCommit != nil {
			armed.QueueBase = entry.BaseCommit.Oid
		}
	}
	if base := p.BaseRef; base != nil {
		armed.BaseName = base.Name
		if base.Target != nil {
			armed.BaseTip = base.Target.Oid
		}
	}
	return armed
}

// kept says whether the pull request's head is head, the READY head an arm is legitimate for.
func (p *pullRequest) kept(head string) bool { return head != "" && p.HeadRefOid == head }

// openAt says whether the pull request is open at head.
func (p *pullRequest) openAt(head string) bool { return p.State == "OPEN" && p.HeadRefOid == head }

// readPull reads pull request number of repo, nil when GitHub answers NOT_FOUND at the pull
// request itself.
func readPull(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int) (*pullRequest, error) {
	answer, err := readRepository(ctx, github, repo, number)
	return answer.PullRequest, err
}

// readRepository reads repo and pull request number, with a nil pull request when GitHub answers
// NOT_FOUND at the pull request itself.
func readRepository(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int) (repository, error) {
	var answer struct {
		Repository repository `json:"repository"`
	}
	err := github.GraphQL(ctx, read, map[string]any{"owner": repo.Owner(), "name": repo.Name(), "number": number}, &answer)
	var failed *githubrest.GraphQLError
	if errors.As(err, &failed) && failed.NotFoundAt("repository", "pullRequest") {
		return repository{}, nil
	}
	if err != nil {
		return repository{}, fmt.Errorf("read the merge state of pull request #%d of %s: %w", number, repo, err)
	}
	return answer.Repository, nil
}
