// Package verdict decides what the required check legion-verdict concludes on one head, from what
// the daemon recorded of that head: R4's table and its refusals. It reads nothing and writes
// nothing, so the writer (the daemon's writeVerdict), the merge-group write and the sweep all
// decide a head by this one rule.
//
// On a head a Legion issue owns, the verdict follows the checks of the task, the steps of its plan
// (Steps) and the change's commits, as R4 states them:
// the head concludes success only when each check of the task reads yes at that head, whatever
// steps the plan names; every testing step passed there and every review step's agent approved
// there; and each commit of the change has a reader (readsCommit): the agent of a review step that
// approved the head, or of a testing step that passed there, whose claim records a session and
// none of whose sessions made that commit. A commit's makers are the sessions its Omp-Session lines
// name, so a commit with no such line has no agent maker, and a session no claim of the task records
// is a maker outside the task's claims; a claim that records no session reads no commit. Each of
// those is read at the head alone, so a step's pass at an earlier commit counts at no later head;
// the daemon reads no diff. Once every check and step passed with a commit no agent read, or with
// commits the daemon could not read, the head concludes failure, for a reason Decide names, so the
// merger can tell the owner. A person's approval of such a head is no verdict, and neither is the
// CI reviewer's.
// On any other head, the verdict is the last review at the head of the entry's ci_reviewer, the
// side reviewer's verdict where the entry takes side reviews, or a person's approval. A person is
// an account of type User with write access to the repository.
//
// A refusal at a head beats every approval there: a check of the task reading no, a review step's
// changes, the CI reviewer's request for changes, or a person's last review at the head
// asking for changes. A person's refusal stands on every kind of head until that person approves
// the same head, and a comment lifts nothing. A head that holds no verdict and no refusal
// concludes nothing, and nothing is written on it, so it cannot merge. A Legion head where each
// check and step passed, no refusal stands, and GitHub gave no answer to the read of the change's
// commits is undecided: no decision yet, so the daemon writes no run of its own there, and the run
// it wrote before stands, until GitHub answers or the writer's bound on no answer makes the change
// unread (the daemon's boundUnanswered).
package verdict

import (
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/plan"
)

// CheckName is the name of the required check the daemon writes, as a check run and, over a commit
// status of the same name another writer of the one App left, as a commit status.
const CheckName = "legion-verdict"

// Conclusion is what legion-verdict concludes on a head: success, failure, None, which writes no
// check, or Undecided, which writes no run of the daemon's own and leaves its earlier one as it
// stands.
type Conclusion string

const (
	None    Conclusion = ""
	Success Conclusion = "success"
	Failure Conclusion = "failure"
	// Undecided is no decision yet: on a Legion head where each check reads yes and every step
	// passed, GitHub gave no answer at all to the read of the change's commits (Inputs.Unanswered),
	// and no refusal stands. It is never written as a conclusion: the daemon's run written before
	// stands until a read is answered, and a head the writers' sweeps find so on their bound's
	// passes in a row is decided as one whose change cannot be read (ChangeUnread).
	Undecided Conclusion = "undecided"
)

// The review states that decide, as intake spells a GitHub review's state; every other state (a
// comment, a dismissal) decides nothing.
const (
	StateApproved         = "approved"
	StateChangesRequested = "changes_requested"
)

// The states of a check at a head, as Dispatch publishes them (check.state_changed): a testing
// step passes on yes alone, fails on no, and waits on any other.
const (
	StateYes     = "yes"
	StateNo      = "no"
	StateUnknown = "unknown"
)

// Word is a review step's verdict on a head, its agent's, or the side reviewer's a controller
// named. The store records it as it is (store.VerdictWord).
type Word string

const (
	ReviewerApprove Word = "approve"
	ReviewerChanges Word = "changes"
)

// Reason is why Decide concludes failure on a Legion head where no check and no step failed: each
// check reads yes and every testing and review step passed at the head, and a commit of the change
// has no reader there. "" for every other conclusion. The writer names it in the run's title and
// summary (the daemon's verdictOutput), which the merger reads to tell the owner.
type Reason string

const (
	// NoReader is a head where a commit of the change has no reader (CommitsWithNoReader names
	// each), a plan with no testing step and no review step among them, whose commits no agent reads.
	NoReader Reason = "a commit with no reader"
	// ChangeUnread is a head where GitHub answered that the daemon cannot read every commit of the
	// change (Inputs.Unread), so no commit has a reader, or where the writers' sweeps found no
	// answer at all on their bound's passes in a row. One read with no answer is no such answer,
	// and decides nothing yet (Undecided).
	ChangeUnread Reason = "change unread"
)

// Step is one step of the task's plan on a Legion head, as the writer reads it at Inputs.Head: its
// type (#4611's plan.Type), for a step of the type review its agent's verdict at the head, and for
// a review or testing step the sessions the daemon records for its agent's claim, by which it reads
// the change's commits (readsCommit).
type Step struct {
	Type plan.Type
	// Verdict is the review step's agent's verdict at the head, "" for none.
	Verdict Word
	// Sessions are the sessions the daemon records for the claim of the step's agent
	// (claim_sessions): none where the step's record names no agent or its claim records none, and
	// such a step reads no commit, since the daemon cannot tell whether a line names it.
	Sessions []string
}

// Commit is one commit of the change at a Legion head: its id, and its makers, the sessions its
// Omp-Session lines name, wherever a line stands in its message; none for a commit with no line.
type Commit struct {
	SHA    string
	Makers []string
}

// Check is the state of one check of the head's set at the head, "" where the daemon holds none.
type Check struct {
	Name, State string
}

// Review is one review of the pull request, delivered by webhook: the head it read, its author,
// whether its author is a person (an account of type User with write access), its state, and
// GitHub's id of it and when GitHub says it was submitted, by which it is ordered among the
// author's reviews (record.ReviewOrder).
type Review struct {
	Head        string
	Author      string
	Person      bool
	State       string
	GitHubID    int64
	SubmittedAt time.Time
}

// Inputs is what the daemon recorded of one head of a verdict repository's pull request.
type Inputs struct {
	// Head is the commit decided.
	Head string
	// Legion is whether a Legion issue of this daemon's project owns the head (a legion/<KEY> head,
	// or a task branch legion/<KEY>.<slug>); Steps, Checks, Commits, Unread and Unanswered are that
	// issue's, and read on no other head.
	Legion bool
	// Steps are the steps of the task's plan, in its order.
	Steps []Step
	// Checks is each check of the task's set with its state at the head, nil where the daemon holds
	// no set there. Each must read yes at the head, whatever steps the plan names, and a testing step
	// passes at a head when they all do: they are the issue's at the head, not one step's.
	Checks []Check
	// Commits is each commit of the change, in its order, as the daemon read them from GitHub. None
	// where it has not read them, which is no reading of the change: a change has a commit.
	Commits []Commit
	// Unread is whether GitHub answered that the daemon cannot read every commit of the change, so
	// no commit has a reader. Unanswered is whether GitHub gave no answer at all to the read, which
	// decides nothing yet (Undecided). The two stay apart: no answer is no answer that the commits
	// cannot be read, until the writer's bound on no answer (ChangeUnread).
	Unread, Unanswered bool
	// CIReviewer is the bot login the repository's entry names as its ci_reviewer, "" for none.
	CIReviewer string
	// SideReviews is whether the entry takes side reviews, and Side the side reviewer's verdict at
	// the head ("" for none).
	SideReviews bool
	Side        Word
	// Reviews is every review of the pull request the daemon recorded, in the order it recorded
	// them, which can differ from GitHub's when a delivery comes again later; only those at Head
	// are read, and each author's last is taken by GitHub's order (later).
	Reviews []Review
}

// Decide is legion-verdict's conclusion on in.Head, and on a Legion head the reason of a failure no
// step refused (Reason).
func Decide(in Inputs) (Conclusion, Reason) {
	refused, approved := personsAt(in)
	// The CI reviewer's request for changes refuses every kind of head; its approval is a verdict on
	// a head no Legion issue owns alone.
	ci := ciReviewerAt(in)
	refused = refused || ci == StateChangesRequested
	var reason Reason
	if in.Legion {
		// A person's approval is no verdict on a Legion head: only the steps of its plan are.
		var failed bool
		failed, approved, reason = stepsAt(in)
		refused = refused || failed
	} else {
		approved = approved || ci == StateApproved
		if in.SideReviews {
			refused = refused || in.Side == ReviewerChanges
			approved = approved || in.Side == ReviewerApprove
		}
	}
	switch {
	case refused:
		return Failure, reason
	case approved:
		return Success, ""
	case in.Legion && in.Unanswered && StepsPassed(in):
		return Undecided, ""
	}
	return None, ""
}

// stepsAt is what the checks, the steps and the change's commits of a Legion head say at the head:
// failed where a check reads no there or a review step's agent asked for changes there, or where
// each check reads yes and every step passed and a commit of the change has no reader, or GitHub
// answered that the daemon cannot read them, which reason then names; neither where every step
// passed and GitHub gave no answer to the read of the commits; and passed where each check reads
// yes, every step passed and each commit has a reader.
func stepsAt(in Inputs) (failed, passed bool, reason Reason) {
	failed = in.checkReads(StateNo)
	for _, step := range in.Steps {
		failed = failed || step.Type == plan.TypeReview && step.Verdict == ReviewerChanges
	}
	switch {
	case failed || !StepsPassed(in):
		return failed, false, ""
	case in.Unanswered:
		return false, false, ""
	case in.Unread || len(in.Commits) == 0:
		return true, false, ChangeUnread
	case len(CommitsWithNoReader(in)) > 0:
		return true, false, NoReader
	}
	return false, true, ""
}

// StepsPassed is whether each check of a Legion head's task reads yes at the head, whatever steps
// the plan names, and every review step's agent approved the head, so every testing step passed
// there too. Only then do the readers of the change's commits decide anything, so the writer reads
// the change's commits only then. A head no Legion issue owns has no such steps, and answers false.
func StepsPassed(in Inputs) bool {
	if !in.Legion || in.Checks == nil || !in.everyCheckYes() {
		return false
	}
	for _, step := range in.Steps {
		if step.Type == plan.TypeReview && step.Verdict != ReviewerApprove {
			return false
		}
	}
	return true
}

// CommitsWithNoReader is the id of each commit of the change that no agent reads at the head, in
// the change's order (readsCommit).
func CommitsWithNoReader(in Inputs) []string {
	var unread []string
	for _, commit := range in.Commits {
		if !slices.ContainsFunc(in.Steps, func(step Step) bool { return in.readsCommit(step, commit) }) {
			unread = append(unread, commit.SHA)
		}
	}
	return unread
}

// readsCommit is whether step's agent reads commit at the head (the reader rule of R4, with its
// departures): the step is a review step whose agent approved the head, or a testing step that
// passed there, as it does when each check of the task reads yes; its claim records a session; and
// none of those sessions made the commit. A commit with no Omp-Session line has no agent maker, so
// each such agent reads it, as it reads a commit whose line names a session no claim records.
func (in Inputs) readsCommit(step Step, commit Commit) bool {
	passed := step.Type == plan.TypeReview && step.Verdict == ReviewerApprove ||
		step.Type == plan.TypeTesting && in.Checks != nil && in.everyCheckYes()
	return passed && len(step.Sessions) > 0 &&
		!slices.ContainsFunc(step.Sessions, func(session string) bool { return slices.Contains(commit.Makers, session) })
}

// everyCheckYes is whether each check of the head's set reads yes at the head: an empty set does.
func (in Inputs) everyCheckYes() bool {
	for _, check := range in.Checks {
		if check.State != StateYes {
			return false
		}
	}
	return true
}

// checkReads is whether a check of the head's set reads state at the head.
func (in Inputs) checkReads(state string) bool {
	for _, check := range in.Checks {
		if check.State == state {
			return true
		}
	}
	return false
}

// personsAt is whether a person's last deciding review at the head refuses it, and whether one
// approves it. Each author's last approval or request for changes at the head is that author's
// word, so a person's refusal is lifted by the same login's later approval alone; the CI
// reviewer's reviews are read apart (ciReviewerAt).
func personsAt(in Inputs) (refused, approved bool) {
	last := map[string]Review{}
	for _, review := range in.Reviews {
		if review.Head != in.Head || !deciding(review.State) || isCIReviewer(in, review.Author) {
			continue
		}
		if kept, ok := last[review.Author]; !ok || later(review, kept) {
			last[review.Author] = review
		}
	}
	for _, review := range last {
		refused = refused || review.Person && review.State == StateChangesRequested
		approved = approved || review.Person && review.State == StateApproved
	}
	return refused, approved
}

// ciReviewerAt is the state of the entry's ci_reviewer's last deciding review at the head, "" for
// none.
func ciReviewerAt(in Inputs) string {
	var kept Review
	found := false
	for _, review := range in.Reviews {
		if review.Head == in.Head && deciding(review.State) && isCIReviewer(in, review.Author) && (!found || later(review, kept)) {
			kept, found = review, true
		}
	}
	return kept.State
}

// later is whether review replaces kept as its author's last: GitHub submitted it after kept, or
// neither is ordered before the other and the daemon recorded it after.
func later(review, kept Review) bool {
	return !after(kept, review)
}

// after is whether GitHub submitted a after b, by record.ReviewOrder.After's rule: the submission
// times decide where both reviews carry one and they differ, and GitHub's ids otherwise. The rule
// is written out here because this package imports no record: the store reads its words and
// conclusions, and record's own tests read the store. TestReviewsAreOrderedAsTheRecordOrdersThem
// holds the two rules alike.
func after(a, b Review) bool {
	if !a.SubmittedAt.IsZero() && !b.SubmittedAt.IsZero() && !a.SubmittedAt.Equal(b.SubmittedAt) {
		return a.SubmittedAt.After(b.SubmittedAt)
	}
	return a.GitHubID > b.GitHubID
}

// isCIReviewer is whether author is the entry's ci_reviewer, compared without case as GitHub
// compares logins: the configuration keeps the login as an operator spelled it. Its reviews are
// read apart from a person's on every kind of head (ciReviewerAt): on a Legion head its request
// for changes refuses the head and its approval decides nothing (Decide).
func isCIReviewer(in Inputs, author string) bool {
	return in.CIReviewer != "" && strings.EqualFold(author, in.CIReviewer)
}

// deciding is whether a review in state decides anything.
func deciding(state string) bool {
	return state == StateApproved || state == StateChangesRequested
}
