package workflow

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// A check run's name and a settlement's snapshot are CI's own text, and a push's sender is
// GitHub's, so any of them can hold a NUL, which PostgreSQL refuses in a text column (SQLSTATE
// 22021) and, as the JSON escape \u0000, in jsonb (22P05). A settlement or a push the store refused
// would be delivered again for as long as the daemon runs, and the pull request's record would
// never move past it. Each applies instead, and the pull request's row holds U+FFFD where each NUL
// was: in its check runs, its failing checks and its snapshot, and in the push it holds.
func TestAPullRequestsChecksAndPushHoldingNULAreStoredWithReplacementCharacters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: headOne})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	facts := store.NewPool(pool)
	engine := testEngine(config.DesignGateRootIssues, nil)

	for _, step := range []struct {
		id   string
		fact intake.Fact
	}{
		{"checks", intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: headOne,
			CheckRuns: []record.AttemptRun{{Name: "unit\x00tests", ID: 1}}, Generation: 1, Snapshot: "settled\x00" + headOne,
			Failing: []string{"unit\x00tests"}}},
		{"push", intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: headOne, After: headTwo, Pusher: "author", Sender: "au\x00thor"}},
	} {
		result, err := intake.ApplyFact(ctx, facts, "github", step.id, step.fact, engine, admissionStub{})
		if err != nil || result.Refusal != nil {
			t.Errorf("apply the %s fact = %+v, %v; want it applied", step.id, result.Refusal, err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	var checkRunsJSON, failingJSON, pushesJSON []byte
	var snapshot string
	if err := pool.QueryRow(ctx, `select check_runs, failing, snapshot, pushes from pull_requests where issue = $1`, "LEGION-208").
		Scan(&checkRunsJSON, &failingJSON, &snapshot, &pushesJSON); err != nil {
		t.Fatalf("read the pull request of LEGION-208: %v", err)
	}
	var checkRuns []record.AttemptRun
	var failing []string
	var pushes []record.ClassifiedPush
	for _, column := range []struct {
		name string
		data []byte
		into any
	}{{"check_runs", checkRunsJSON, &checkRuns}, {"failing", failingJSON, &failing}, {"pushes", pushesJSON, &pushes}} {
		if err := json.Unmarshal(column.data, column.into); err != nil {
			t.Fatalf("the stored %s %s do not decode: %v", column.name, column.data, err)
		}
	}
	var names []string
	for _, run := range checkRuns {
		names = append(names, run.Name)
	}
	if want := []string{"unit\uFFFDtests"}; !slices.Equal(names, want) {
		t.Errorf("the stored check runs are named %q, want %q", names, want)
	}
	if want := []string{"unit\uFFFDtests"}; !slices.Equal(failing, want) {
		t.Errorf("the stored failing checks are %q, want %q", failing, want)
	}
	if want := "settled\uFFFD" + headOne; snapshot != want {
		t.Errorf("the stored snapshot is %q, want %q", snapshot, want)
	}
	if want := []record.ClassifiedPush{{SHA: headTwo, Before: headOne, Sender: "au\uFFFDthor"}}; !slices.Equal(pushes, want) {
		t.Errorf("the stored pushes are %+v, want %+v", pushes, want)
	}
}

// A person's note on a decline is their own text, and the words beside it are Dispatch's, so
// either can hold a NUL, which PostgreSQL refuses in a text column (SQLSTATE 22021). A decline the
// store refused would be delivered again for as long as the daemon runs, and the root whose
// architect asked would never hear it. The decline applies instead, and the ask's mirror row holds
// U+FFFD where each NUL was, in the note and in the words.
func TestAPersonsDeclineHoldingNULIsMirroredWithReplacementCharacters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := migratedPool(t)
	seedIssue(t, pool, askRoot())
	seedClaimSession(t, pool, "architect-claim", askArchitect.ID)
	seedDeclaredRoot(t, pool)
	seedQuestion(t, pool, "ask-q", 4)
	seedQuestion(t, pool, "ask-second", 5)
	decline := declineOf("ask-q", 8, "The vendor owns that date;\x00 I do not.")
	decline.Decline.Words = "The person chose not to answer this question.\x00 Decide the next step without an answer."

	result, err := intake.ApplyFact(ctx, store.NewPool(pool), "api", "declined", decline, testEngine(config.DesignGateRootIssues, nil), admissionStub{})
	if err != nil || result.Refusal != nil {
		t.Fatalf("apply the decline = %+v, %v; want it applied", result.Refusal, err)
	}

	var declined bool
	var note, words string
	if err := pool.QueryRow(ctx, `select declined, coalesce(declined_note, ''), coalesce(declined_words, '') from root_asks where issue = $1 and ask = $2`,
		"LEGION-208", "ask-q").Scan(&declined, &note, &words); err != nil {
		t.Fatalf("read the mirror row of ask-q: %v", err)
	}
	if !declined {
		t.Errorf("the mirror row of ask-q is not declined, want the person's decline recorded")
	}
	if want := "The vendor owns that date;\uFFFD I do not."; note != want {
		t.Errorf("the mirror row of ask-q holds the note %q, want %q", note, want)
	}
	if want := "The person chose not to answer this question.\uFFFD Decide the next step without an answer."; words != want {
		t.Errorf("the mirror row of ask-q holds Dispatch's words %q, want %q", words, want)
	}
}
