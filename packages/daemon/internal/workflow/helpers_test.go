package workflow

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// refusalCode is the code of a fact's refusal, or "" when it was not refused.
func refusalCode(result intake.Result) string {
	if result.Refusal == nil {
		return ""
	}
	return result.Refusal.Code
}

// A review the review App submits decides a round only when the Legion footer it was written
// with names the pull request's current head, and a footer names a head only when it spells it
// as GitHub does - 40 lowercase hexadecimal characters (intake.PullRequestReview.LegionHead,
// refusesRound). So every fixture whose round a review decides seeds its heads from these, one
// per head of the branch, and writes its reviews with reviewBody.
const (
	// headEarlier is a head the branch left before headOne, for a fixture that needs a head no
	// round of it stands at.
	headEarlier = "9999999999999999999999999999999999999999"
	headOne     = "1111111111111111111111111111111111111111"
	headTwo     = "2222222222222222222222222222222222222222"
	headThree   = "3333333333333333333333333333333333333333"
)

// reviewBody is a review's body as a Legion role writes it: what the role said, then the Legion
// footer naming the session that wrote it and the head it read (prompts/roles/reviewer.md).
// The session it names is the one the shared fixtures record on the reviewer's claim
// (seedRoundOneRows), so it is the reviewer's own review, and a fixture about another session
// writes its own body. It names no entry, so it decides its round and moves no commit the reviewer
// read (record.PhaseRow.ReviewedHead): a fixture whose proof reads that commit writes its review
// with roundReviewBody.
func reviewBody(said, head string) string {
	return said + "\n\n<!-- legion: {\"session\":\"ses-reviewer\",\"phase\":\"review\",\"head\":\"" + head + "\"} -->"
}

// reviewBodyOfEntry is a review's body whose Legion footer names session, head and entry as the
// entry of the round the review is of, the number the reviewer's task names (reviewround.EntryLabel),
// written as the reviewer's texts have an agent write it, a quoted number (`"entry":"<n>"`). A
// fixture about a review of the round that stands writes roundReviewBody, which reads that number;
// one about a review of another round names that round's here.
func reviewBodyOfEntry(said, session, head string, entry int64) string {
	return said + "\n\n<!-- legion: {\"session\":\"" + session + "\",\"phase\":\"review\",\"head\":\"" + head + "\",\"entry\":\"" + strconv.FormatInt(entry, 10) + "\"} -->"
}

// roundReviewBody is the body of session's review of the round that stands, as the reviewer
// writes it for the task it was given: its footer names the entry LEGION-208's reviewer row holds
// when the review is built (record.PhaseRow.Entry), the number that task names. It is the one
// helper of this package through which a fixture of the reviewer's own review ("ses-reviewer",
// the session seedRoundOneRows records on the reviewer's claim) names its entry, and through which
// a fixture of another session's review names the same entry, so that only the session tells the
// two apart. Build the review after the round's entry is raised.
func roundReviewBody(t *testing.T, pool *pgxpool.Pool, session, said, head string) string {
	t.Helper()
	return reviewBodyOfEntry(said, session, head, phaseRowOf(t, pool, claim.RoleReviewer).Entry)
}

// commentBody is reviewBody for the reviewer's COMMENT: its Legion footer names the reviewer's
// session and no head, since a comment reads no head the round could be decided on.
func commentBody(said string) string {
	return said + "\n\n<!-- legion: {\"session\":\"ses-reviewer\",\"phase\":\"review\"} -->"
}

// reviewBodyOf is reviewBody for a review the review App submitted and what the author said for
// anyone else's: a person writes no Legion footer, and their review decides on the commit_id the
// webhook carried it with (refusesRound).
func reviewBodyOf(author, said, head string) string {
	if author == testReviewApp {
		return reviewBody(said, head)
	}
	return said
}

// mergingEngine is an engine of a gated project with a fixed clock, which the tests that drive an
// issue through its later phases share.
func mergingEngine() *Engine {
	return mergingEngineAt(time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC))
}

// mergingEngineAt is mergingEngine with its clock at now.
func mergingEngineAt(now time.Time) *Engine {
	return New(record.NewStore(), Config{
		Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, ReviewAppLogin: testReviewApp, Repositories: testRepositories,
		// The project opens its production check with the tester, the role a project that names
		// no production_check gets (INFRA-809); an engine with no role opens no check at all.
		ProductionCheckRole: claim.RoleTester,
		Clock:               func() time.Time { return now },
	}, nil)
}

// applyFacts applies each fact to pool through engine, failing the test on an error, and returns
// the fact's result. The boot's open of each run no start counted comes before each fact
// (bootBefore), as it comes before any fact the daemon applies, and what the fact left is kept
// for the next pass's check (factLeft).
func applyFacts(t *testing.T, pool *pgxpool.Pool, engine *Engine) func(id string, fact intake.Fact) intake.Result {
	return func(id string, fact intake.Fact) intake.Result {
		t.Helper()
		bootBefore(t, pool, engine)
		result, err := intake.ApplyFact(context.Background(), pool, "api", id, fact, engine, admissionStub{})
		if err != nil {
			t.Fatalf("ApplyFact %s: %v", id, err)
		}
		factLeft(t, pool)
		return result
	}
}

// messageBodies is every Dispatch message the outbox holds, in order.
func messageBodies(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'body' from outbox where kind = 'dispatch_message' order by id")
	if err != nil {
		t.Fatalf("read Dispatch messages: %v", err)
	}
	defer rows.Close()
	bodies := []string{}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatalf("scan Dispatch message: %v", err)
		}
		bodies = append(bodies, body)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate Dispatch messages: %v", err)
	}
	return bodies
}
