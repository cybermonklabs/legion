package verdict

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The heads of these cases: H the head decided, H0 any other commit.
const (
	h  = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	h0 = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
)

// ciReviewer is the bot login a verdict repository's entry names as its ci_reviewer.
const ciReviewer = "example-ci-review[bot]"

func approvedBy(author string, person bool, at string) Review {
	return Review{Head: at, Author: author, Person: person, State: StateApproved}
}

func refusedBy(author string, person bool, at string) Review {
	return Review{Head: at, Author: author, Person: person, State: StateChangesRequested}
}

func commentedBy(author string, person bool, at string) Review {
	return Review{Head: at, Author: author, Person: person, State: "commented"}
}

// submitted is review as GitHub numbered it and submitted it, minute minutes past ten: what orders
// each author's reviews (record.ReviewOrder), whatever order the daemon recorded them in.
func submitted(review Review, id int64, minute int) Review {
	review.GitHubID, review.SubmittedAt = id, time.Date(2026, 11, 20, 10, minute, 0, 0, time.UTC)
	return review
}

// The sessions the daemon records for the claims of a task's agents: the implementer's, the
// reviewer's, the owner's at its acceptance, and the tester's; and a session that no claim of the
// task records, as a session of a maker outside the task reads.
const (
	implementerSession = "ses-implementer"
	reviewerSession    = "ses-reviewer"
	ownerSession       = "ses-owner"
	testerSession      = "ses-tester"
	outsideSession     = "ses-outside"
)

// approvedAs is a review step whose agent approved H, its claim recording sessions; changes and
// noVerdict are a review step whose agent asked for changes at H, or gave no verdict there.
func approvedAs(sessions ...string) Step {
	return Step{Type: plan.TypeReview, Verdict: ReviewerApprove, Sessions: sessions}
}

var (
	approves  = approvedAs(reviewerSession)
	accepts   = approvedAs(ownerSession)
	changes   = Step{Type: plan.TypeReview, Verdict: ReviewerChanges, Sessions: []string{reviewerSession}}
	noVerdict = Step{Type: plan.TypeReview}
)

// testedAs is a testing step whose agent's claim records sessions.
func testedAs(sessions ...string) Step {
	return Step{Type: plan.TypeTesting, Sessions: sessions}
}

// testingStep is a testing step whose agent no record names, so its claim records no session.
var testingStep = Step{Type: plan.TypeTesting}

// madeBy is a commit of the change whose Omp-Session lines name sessions, none for a commit with no
// such line.
func madeBy(sha string, sessions ...string) Commit {
	return Commit{SHA: sha, Makers: sessions}
}

// implemented is a change of one commit, the implementer's.
var implemented = []Commit{madeBy("c1", implementerSession)}

// allYes is a head's set of two checks, each reading yes at H.
var allYes = []Check{{Name: "k1", State: StateYes}, {Name: "k2", State: StateYes}}

// checks is a head's set of two checks: k1 reading yes at H and k2 reading state ("" for no state
// held).
func checks(state string) []Check {
	return []Check{{Name: "k1", State: StateYes}, {Name: "k2", State: state}}
}

// defaultSteps is a Legion head H of a task with the default steps, planner implementer tester
// reviewer acceptance:owner: the head's checks, the reviewer's step and the owner's acceptance as
// given, a tester whose claim records no session, and the change of the implementer's one commit.
func defaultSteps(held []Check, reviewer, acceptance Step) Inputs {
	return Inputs{Head: h, Legion: true, Checks: held, Steps: []Step{{Type: plan.TypePlanning}, {Type: plan.TypeImplementation},
		testingStep, reviewer, acceptance}, Commits: implemented}
}

// stepsOf is a Legion head H of a task with steps of these types: an implementation step, and
// each of more as given; its change is the implementer's one commit.
func stepsOf(held []Check, more ...Step) Inputs {
	return Inputs{Head: h, Legion: true, Checks: held, Steps: append([]Step{{Type: plan.TypeImplementation}}, more...), Commits: implemented}
}

// withReviews is in with reviews delivered by webhook.
func withReviews(in Inputs, reviews ...Review) Inputs {
	in.Reviews = reviews
	return in
}

// withUnread is in where the daemon could not read every commit of the change.
func withUnread(in Inputs) Inputs {
	in.Unread, in.Commits = true, nil
	return in
}

// withUnanswered is in where GitHub gave no answer to the read of the change's commits.
func withUnanswered(in Inputs) Inputs {
	in.Unanswered, in.Commits = true, nil
	return in
}

// withChange is in with the change of commits.
func withChange(in Inputs, commits ...Commit) Inputs {
	in.Commits = commits
	return in
}

// withTester is in with its testing step's claim recording sessions.
func withTester(in Inputs, sessions ...string) Inputs {
	steps := slices.Clone(in.Steps)
	for i := range steps {
		if steps[i].Type == plan.TypeTesting {
			steps[i].Sessions = sessions
		}
	}
	in.Steps = steps
	return in
}

// Decide is R4's table and refusals, one case per row and per refusal. On a head a Legion issue
// owns, success only when each check of the task reads yes at H whatever steps the plan names,
// every testing and review step passed at H, and each commit of the change has a reader: an agent
// of a review step that approved H, or of a testing step that passed there, whose claim records a
// session and none of whose sessions made the commit (R29, with R4's departures). A commit's makers
// are the sessions its Omp-Session lines name: a commit with no line has no agent maker, and a
// session no claim records is a maker outside the task's claims. A claim that records no session
// reads no commit. Failure on a check no at H, on a review step's changes at H, once every check
// and step passed with a commit no agent read, which Decide names as its reason, and where the
// daemon could not read every commit of the change. Nothing before. A person's approval is never a
// verdict there, and a person's last request for changes at H refuses it. On any other head, the
// ci_reviewer's last review, the side reviewer's verdict where the entry takes side reviews, or a
// person's approval; and on every head a refusal - a failed step, the CI reviewer's request for
// changes, or a person's last review at the head asking for changes - beats every approval. A head
// that holds none of them gets no conclusion, so nothing is written on it. On a Legion head the
// daemon reads an author's write access for a request for changes alone (intake's
// resolveReviewPermission), so an approval there is given here as it arrives: with Person false.
func TestDecideIsR4sTable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		in     Inputs
		want   Conclusion
		reason Reason
	}{
		// A head a Legion issue owns, with the default steps.
		{"a Legion head: every check yes, both review steps approve, the implementer's commit read", defaultSteps(allYes, approves, accepts), Success, ""},
		{"a Legion head: a check no at H, before any review", defaultSteps(checks(StateNo), noVerdict, noVerdict), Failure, ""},
		{"a Legion head: a check no at H beats both approvals", defaultSteps(checks(StateNo), approves, accepts), Failure, ""},
		{"a Legion head: the reviewer's changes", defaultSteps(allYes, changes, noVerdict), Failure, ""},
		{"a Legion head: the owner's changes after the reviewer's approve", defaultSteps(allYes, approves, changes), Failure, ""},
		{"a Legion head: a check with no state at H", defaultSteps(checks(""), approves, accepts), None, ""},
		{"a Legion head: a check unknown at H", defaultSteps(checks(StateUnknown), approves, accepts), None, ""},
		{"a Legion head: no set of checks held at H", defaultSteps(nil, approves, accepts), None, ""},
		{"a Legion head: an empty set of checks at H", defaultSteps([]Check{}, approves, accepts), Success, ""},
		{"a Legion head: the reviewer approves and the owner has not", defaultSteps(allYes, approves, noVerdict), None, ""},
		{"a Legion head: neither review step has a verdict", defaultSteps(allYes, noVerdict, noVerdict), None, ""},
		{"a Legion head: a commit of the reviewer's, which the owner reads", withChange(defaultSteps(allYes, approves, accepts),
			madeBy("c1", reviewerSession)), Success, ""},
		{"a Legion head: a commit of the reviewer's and one of the owner's, each read by the other", withChange(defaultSteps(allYes, approves, accepts),
			madeBy("c1", reviewerSession), madeBy("c2", ownerSession)), Success, ""},
		{"a Legion head: a commit whose lines name the reviewer and the owner, the tester recording no session", withChange(defaultSteps(allYes, approves, accepts),
			madeBy("c1", implementerSession), madeBy("c2", reviewerSession, ownerSession)), Failure, NoReader},
		{"a Legion head: the tester's pass reads a commit of the reviewer and the owner", withTester(withChange(defaultSteps(allYes, approves, accepts),
			madeBy("c2", reviewerSession, ownerSession)), testerSession), Success, ""},
		{"a Legion head: a commit whose lines name all three readers", withTester(withChange(defaultSteps(allYes, approves, accepts),
			madeBy("c2", reviewerSession, ownerSession, testerSession)), testerSession), Failure, NoReader},
		{"a Legion head: the change's commits could not be read", withUnread(defaultSteps(allYes, approves, accepts)), Failure, ChangeUnread},
		{"a Legion head: the change's commits not read at all", withChange(defaultSteps(allYes, approves, accepts)), Failure, ChangeUnread},
		{"a Legion head: GitHub gave no answer to the read of the change", withUnanswered(defaultSteps(allYes, approves, accepts)), Undecided, ""},
		{"a Legion head: a person's request for changes while GitHub gave no answer", withReviews(withUnanswered(defaultSteps(allYes, approves, accepts)),
			refusedBy("a-writer", true, h)), Failure, ""},
		{"a Legion head: no answer to the read, and a check not yet yes", withUnanswered(defaultSteps(checks(""), approves, accepts)), None, ""},
		{"a Legion head: a commit no agent read, and a check not yet yes", withChange(defaultSteps(checks(""), approves, accepts),
			madeBy("c1", reviewerSession, ownerSession)), None, ""},

		// A head a Legion issue owns, with a plan of other steps.
		{"a plan implementer reviewer: every check yes and the reviewer's approve", stepsOf(allYes, approves), Success, ""},
		{"a plan implementer reviewer: a check no, though the plan has no testing step", stepsOf(checks(StateNo), approves), Failure, ""},
		{"a plan implementer reviewer: no set of checks held", stepsOf(nil, approves), None, ""},
		{"a plan implementer reviewer: a commit of the reviewer's", withChange(stepsOf(allYes, approves), madeBy("c1", implementerSession),
			madeBy("c2", reviewerSession)), Failure, NoReader},
		{"a plan implementer reviewer: a commit with no Omp-Session line", withChange(stepsOf(allYes, approves), madeBy("c1")), Success, ""},
		{"a plan implementer reviewer: a commit naming a session no claim records", withChange(stepsOf(allYes, approves),
			madeBy("c1", outsideSession)), Success, ""},
		{"a plan implementer reviewer: the reviewer's claim records no session", stepsOf(allYes, approvedAs()), Failure, NoReader},
		{"a plan implementer tester: every check yes, no commit the tester's", stepsOf(allYes, testedAs(testerSession)), Success, ""},
		{"a plan implementer tester: a commit of the tester's", withChange(stepsOf(allYes, testedAs(testerSession)),
			madeBy("c1", testerSession)), Failure, NoReader},
		{"a plan implementer tester: the tester's claim records no session", stepsOf(allYes, testingStep), Failure, NoReader},
		{"a plan implementer tester: a check with no state", stepsOf(checks(""), testedAs(testerSession)), None, ""},
		{"a plan implementer tester: a check no", stepsOf(checks(StateNo), testedAs(testerSession)), Failure, ""},
		{"a plan with no testing step and no review step: every check yes", stepsOf(allYes), Failure, NoReader},
		{"a plan with no testing step and no review step: a check with no state", stepsOf(checks("")), None, ""},
		{"a plan with no testing step and no review step: a check no", stepsOf(checks(StateNo)), Failure, ""},

		// A person's review of a Legion head.
		{"a Legion head: a person's approval is no verdict", withReviews(defaultSteps(allYes, approves, noVerdict), approvedBy("a-writer", false, h)), None, ""},
		{"a Legion head: a person's request for changes refuses every step's pass", withReviews(defaultSteps(allYes, approves, approves),
			refusedBy("a-writer", true, h)), Failure, ""},
		{"a Legion head: the same person's later approval lifts the refusal", withReviews(defaultSteps(allYes, approves, approves),
			refusedBy("a-writer", true, h), approvedBy("a-writer", false, h)), Success, ""},
		{"a Legion head: another person's approval lifts no refusal", withReviews(defaultSteps(allYes, approves, approves),
			refusedBy("a-writer", true, h), approvedBy("another-writer", false, h)), Failure, ""},
		{"a Legion head: a comment after a request for changes lifts nothing", withReviews(defaultSteps(allYes, approves, approves),
			refusedBy("a-writer", true, h), commentedBy("a-writer", false, h)), Failure, ""},
		{"a Legion head: an approval recorded after the same person's later refusal lifts nothing", withReviews(defaultSteps(allYes, approves, approves),
			submitted(refusedBy("a-writer", true, h), 12, 5), submitted(approvedBy("a-writer", false, h), 11, 1)), Failure, ""},
		{"a Legion head: a request for changes at another head refuses nothing here", withReviews(defaultSteps(allYes, approves, approves),
			refusedBy("a-writer", true, h0)), Success, ""},
		{"a Legion head: an account with no write access refuses nothing", withReviews(defaultSteps(allYes, approves, approves),
			refusedBy("a-reader", false, h)), Success, ""},
		{"a Legion head: a person's request for changes before every step passed", withReviews(defaultSteps(checks(""), noVerdict, noVerdict),
			refusedBy("a-writer", true, h)), Failure, ""},
		{"a Legion head: the CI reviewer's approval is no verdict", func() Inputs {
			in := withReviews(defaultSteps(allYes, approves, noVerdict), approvedBy(ciReviewer, false, h))
			in.CIReviewer = ciReviewer
			return in
		}(), None, ""},
		{"a Legion head: the CI reviewer's request for changes refuses every step's pass", func() Inputs {
			in := withReviews(defaultSteps(allYes, approves, approves), refusedBy(ciReviewer, false, h))
			in.CIReviewer = ciReviewer
			return in
		}(), Failure, ""},
		{"a Legion head: the CI reviewer's later approval lifts its own request for changes", func() Inputs {
			in := withReviews(defaultSteps(allYes, approves, approves), submitted(refusedBy(ciReviewer, false, h), 41, 1),
				submitted(approvedBy(ciReviewer, false, h), 42, 5))
			in.CIReviewer = ciReviewer
			return in
		}(), Success, ""},
		{"a Legion head: the CI reviewer's request for changes at another head refuses nothing here", func() Inputs {
			in := withReviews(defaultSteps(allYes, approves, approves), refusedBy(ciReviewer, false, h0))
			in.CIReviewer = ciReviewer
			return in
		}(), Success, ""},

		// A head no Legion issue owns, on a repository whose entry names a ci_reviewer.
		{"an outside head: the CI reviewer named in another case than GitHub spells it", Inputs{Head: h, CIReviewer: "Example-CI-Review[bot]",
			Reviews: []Review{approvedBy(ciReviewer, false, h)}}, Success, ""},
		{"an outside head: the CI reviewer approves", Inputs{Head: h, CIReviewer: ciReviewer, Reviews: []Review{approvedBy(ciReviewer, false, h)}}, Success, ""},
		{"an outside head: the CI reviewer asks for changes", Inputs{Head: h, CIReviewer: ciReviewer, Reviews: []Review{refusedBy(ciReviewer, false, h)}}, Failure, ""},
		{"an outside head: the CI reviewer's last review stands", Inputs{Head: h, CIReviewer: ciReviewer,
			Reviews: []Review{refusedBy(ciReviewer, false, h), approvedBy(ciReviewer, false, h)}}, Success, ""},
		{"an outside head: a person's request for changes after the CI reviewer's approval", Inputs{Head: h, CIReviewer: ciReviewer,
			Reviews: []Review{approvedBy(ciReviewer, false, h), refusedBy("a-writer", true, h)}}, Failure, ""},
		{"an outside head: the CI reviewer's approval recorded after its later request for changes", Inputs{Head: h, CIReviewer: ciReviewer,
			Reviews: []Review{submitted(refusedBy(ciReviewer, false, h), 22, 5), submitted(approvedBy(ciReviewer, false, h), 21, 1)}}, Failure, ""},
		{"an outside head: a person's approval recorded after their later request for changes", Inputs{Head: h,
			Reviews: []Review{submitted(refusedBy("a-writer", true, h), 32, 5), submitted(approvedBy("a-writer", true, h), 31, 1)}}, Failure, ""},
		{"an outside head: the CI reviewer's approval of another head", Inputs{Head: h, CIReviewer: ciReviewer, Reviews: []Review{approvedBy(ciReviewer, false, h0)}}, None, ""},
		{"an outside head: a bot that is not the CI reviewer", Inputs{Head: h, CIReviewer: ciReviewer,
			Reviews: []Review{approvedBy("another-bot[bot]", false, h), refusedBy("another-bot[bot]", false, h)}}, None, ""},
		{"an outside head: a person's approval", Inputs{Head: h, CIReviewer: ciReviewer, Reviews: []Review{approvedBy("a-writer", true, h)}}, Success, ""},
		{"an outside head: an approval by an account with no write access", Inputs{Head: h, CIReviewer: ciReviewer, Reviews: []Review{approvedBy("a-reader", false, h)}}, None, ""},
		{"an outside head: no plan step is read", Inputs{Head: h, Steps: []Step{approves}, Checks: checks(StateNo),
			Reviews: []Review{approvedBy("a-writer", true, h)}}, Success, ""},

		// A head no Legion issue owns, on a repository whose entry takes side reviews.
		{"an outside head: the side reviewer approves", Inputs{Head: h, SideReviews: true, Side: ReviewerApprove}, Success, ""},
		{"an outside head: the side reviewer asks for changes", Inputs{Head: h, SideReviews: true, Side: ReviewerChanges}, Failure, ""},
		{"an outside head: a person's refusal beats the side reviewer's approve", Inputs{Head: h, SideReviews: true, Side: ReviewerApprove,
			Reviews: []Review{refusedBy("a-writer", true, h)}}, Failure, ""},
		{"an outside head: a side verdict where the entry takes none", Inputs{Head: h, Side: ReviewerApprove}, None, ""},

		// A head no Legion issue owns, on a repository whose entry names neither.
		{"an outside head with neither: a person's approval", Inputs{Head: h, Reviews: []Review{approvedBy("a-writer", true, h)}}, Success, ""},
		{"an outside head with neither: nothing recorded", Inputs{Head: h}, None, ""},
		{"an outside head with neither: a bot's approval", Inputs{Head: h, Reviews: []Review{approvedBy(ciReviewer, false, h)}}, None, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, reason := Decide(tc.in); got != tc.want || reason != tc.reason {
				t.Errorf("Decide(%+v) = %q, %q; want %q, %q", tc.in, got, reason, tc.want, tc.reason)
			}
		})
	}
}

// StepsPassed is whether each check of a Legion head's task reads yes there, whatever steps the
// plan names, and every review step's agent approved the head: only then do the readers of the
// change's commits decide anything, so the writer reads the change's commits only then.
func TestStepsPassedAtTheHead(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Inputs
		want bool
	}{
		{"every check yes and both review steps approve", defaultSteps(allYes, approves, accepts), true},
		{"a check with no state", defaultSteps(checks(""), approves, accepts), false},
		{"a review step with no verdict", defaultSteps(allYes, approves, noVerdict), false},
		{"a review step's changes", defaultSteps(allYes, approves, changes), false},
		{"a plan with no testing step and no set held", stepsOf(nil, approves), false},
		{"a plan with no testing step and every check yes", stepsOf(allYes, approves), true},
		{"a plan with no review step", stepsOf(allYes, testingStep), true},
		{"a plan with no testing step and no review step", stepsOf(allYes), true},
		{"an outside head", Inputs{Head: h, Steps: []Step{approves}, Checks: allYes}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StepsPassed(tc.in); got != tc.want {
				t.Errorf("StepsPassed(%+v) = %t, want %t", tc.in, got, tc.want)
			}
		})
	}
}

// CommitsWithNoReader is each commit of the change no reader read, in the change's order, which the
// run's summary names: a commit whose makers include the session of every agent that read at the
// head. A step that did not pass at the head reads nothing: a review step with no approve there,
// and a testing step whose checks do not all read yes.
func TestCommitsWithNoReaderAreNamedInTheChangesOrder(t *testing.T) {
	in := withTester(withChange(defaultSteps(allYes, approves, accepts),
		madeBy("c1", reviewerSession, ownerSession, testerSession), madeBy("c2", implementerSession), madeBy("c3"),
		madeBy("c4", testerSession, ownerSession, reviewerSession)), testerSession)
	if got := CommitsWithNoReader(in); !slices.Equal(got, []string{"c1", "c4"}) {
		t.Errorf("CommitsWithNoReader = %q, want c1 and c4, the two whose lines name every reader", got)
	}
	waiting := withTester(withChange(defaultSteps(checks(""), approves, noVerdict), madeBy("c1", reviewerSession)), testerSession)
	if got := CommitsWithNoReader(waiting); !slices.Equal(got, []string{"c1"}) {
		t.Errorf("CommitsWithNoReader where the tester's checks are not all yes and the owner gave no verdict = %q, want c1: the reviewer alone reads, and not its own commit", got)
	}
}

// Decide takes each author's last review by the order the record orders reviews by
// (record.ReviewOrder.After): this package writes that rule out, since it imports no record, and
// each pair here, in both directions, is ordered by the two alike. The pairs are record's own:
// a draft submitted late, two reviews of one moment, a review with no time, and one review.
func TestReviewsAreOrderedAsTheRecordOrdersThem(t *testing.T) {
	early := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	late := early.Add(time.Minute)
	for _, pair := range [][2]Review{
		{{SubmittedAt: late, GitHubID: 1}, {SubmittedAt: early, GitHubID: 2}},
		{{SubmittedAt: early, GitHubID: 2}, {SubmittedAt: early, GitHubID: 1}},
		{{GitHubID: 2}, {SubmittedAt: late, GitHubID: 1}},
		{{SubmittedAt: early, GitHubID: 1}, {SubmittedAt: early, GitHubID: 1}},
	} {
		for _, ab := range [][2]Review{pair, {pair[1], pair[0]}} {
			a, b := ab[0], ab[1]
			want := record.ReviewOrder{SubmittedAt: a.SubmittedAt, ID: a.GitHubID}.After(record.ReviewOrder{SubmittedAt: b.SubmittedAt, ID: b.GitHubID})
			if got := after(a, b); got != want {
				t.Errorf("after(%+v, %+v) = %v, want %v as record.ReviewOrder.After answers", a, b, got, want)
			}
		}
	}
}
