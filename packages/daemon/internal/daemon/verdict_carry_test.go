package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	planwords "github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/verdict"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// pushedTo is a push that moves pull request 86 from one head to another: GitHub moves its head,
// which joins its commits, and its synchronize reaches the daemon.
func (rig *verdictRig) pushedTo(t *testing.T, from, to string) {
	t.Helper()
	rig.github.mu.Lock()
	rig.github.head86 = to
	rig.github.history = append(rig.github.history, to)
	rig.github.mu.Unlock()
	rig.apply(t, intake.PullRequestSynchronized{Repo: "acme/widgets", Number: 86, Branch: "legion/CAPTURE-1", Base: "main", Before: from, HeadSHA: to})
}

// mergeState is GitHub's merge state of pull request 86 at the head it stands at, as the merger
// reads it before it merges: `blocked` while the required legion-verdict there is not success.
func (rig *verdictRig) mergeState(t *testing.T) string {
	t.Helper()
	response, err := http.Get(rig.w.githubAPI + "/repos/acme/widgets/pulls/86")
	if err != nil {
		t.Fatalf("read pull request 86: %v", err)
	}
	defer response.Body.Close()
	var pull struct {
		State string `json:"mergeable_state"`
	}
	if err := json.NewDecoder(response.Body).Decode(&pull); err != nil {
		t.Fatalf("decode pull request 86: %v", err)
	}
	return pull.State
}

// noCompare fails the test where the daemon asked GitHub for a compare of the change's base and a
// head: it reads the change's commits only once each check reads yes and every step passed there.
func (rig *verdictRig) noCompare(t *testing.T) {
	t.Helper()
	if _, events := rig.github.recorded(); slices.ContainsFunc(events, func(e string) bool { return strings.HasPrefix(e, "compared") }) {
		t.Fatalf("GitHub saw %v, want no compare call: a pass counts at the head it was given at alone, and the change's commits are read only once every check and step passed there", events)
	}
}

// nothingWrittenOn fails the test where legion-verdict has a run, a status or a recorded write at
// head.
func (rig *verdictRig) nothingWrittenOn(t *testing.T, head string) {
	t.Helper()
	if runs := rig.github.legionRuns(head); len(runs) != 0 {
		t.Fatalf("legion-verdict runs on %s = %+v, want none", head, runs)
	}
	if recorded := verdictWrites(t, rig.pool, head); len(recorded) != 0 {
		t.Fatalf("the store records writes %+v on %s, want none", recorded, head)
	}
}

// verdictPastReview is the reviewer's verdict past review at commit, as the review-verdict route
// queues it.
func verdictPastReview(word, commit string) intake.ReviewVerdict {
	return intake.ReviewVerdict{Issue: "CAPTURE-1", Claim: captureReviewer, Verdict: word, Commit: commit, Summary: "read the push"}
}

// ownersPastReview is the owner's verdict past review at commit, from the architect that ran its
// acceptance, as the review-verdict route queues it.
func ownersPastReview(word, commit string) intake.ReviewVerdict {
	return intake.ReviewVerdict{Issue: "CAPTURE-1", Claim: captureArchitect, Verdict: word, Commit: commit, Summary: "read the push"}
}

// C2's push case, on CAPTURE-1's legion/ head on the default steps: every step passed at code (H)
// writes success there. After a push to code-2 (H2), the reviewer's approve and the owner's
// acceptance at H2 write nothing while a check has no yes state at H2, though both read yes at H:
// a step's pass at an earlier commit counts at no later head. The fake GitHub records no compare
// call while a check has no yes state at H2. Once each check reads yes at H2, Dispatch's state
// there asks for the write, and success is written on H2.
func TestLegionVerdictAStepsPassAtHCountsAtNoLaterHead(t *testing.T) {
	rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, acceptingAtCode)
	rig.acceptedAndWritten(t)
	rig.github.mu.Lock()
	rig.github.events = nil
	rig.github.mu.Unlock()
	rig.pushedTo(t, "code", "code-2")
	rig.apply(t, verdictPastReview("approve", "code-2"))
	rig.apply(t, ownersPastReview("approve", "code-2"))
	rig.checkState(t, "code-2", "k1", verdict.StateYes)
	rig.written(t)
	rig.nothingWrittenOn(t, "code-2")
	rig.noCompare(t)
	rig.checkState(t, "code-2", "k2", verdict.StateYes)
	rig.written(t)
	rig.soleRun(t, "code-2", "success")
	if asked := rig.askedHeads(); !slices.Equal(asked, []string{"code", "code-2", "code-2", "code-2", "code-2"}) {
		t.Fatalf("the workflow asked for writes on %v, want the acceptance's on code, then one on code-2 for each verdict and each check state", asked)
	}
}

// reviewerApprovedCode is CAPTURE-1's reviewer row as its approve of code, which ended the round,
// left it under the claim review-claim.
var reviewerApprovedCode = record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleReviewer, Claim: "review-claim",
	HandoffCommit: "code", HandoffRecord: "review-1", HandoffVersion: 1, Summary: "reviewed",
	Decision: &record.ReviewDecision{State: "approved", Body: "reviewed", Head: "code"}}

// verdictAsks is every ask of a verdict past review CAPTURE-1's outbox holds (record's
// MergeForwardReview rows), in the order written.
func verdictAsks(t *testing.T, pool *store.Pool) []record.MergeForwardMessage {
	t.Helper()
	rows, err := pool.Query(context.Background(), "select payload from outbox where issue = 'CAPTURE-1' and kind = $1 order by id", string(record.OutboxKindMergeForward))
	if err != nil {
		t.Fatalf("read the merge-forward rows: %v", err)
	}
	defer rows.Close()
	asks := []record.MergeForwardMessage{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scan a merge-forward row: %v", err)
		}
		var message record.MergeForwardMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			t.Fatalf("decode the merge-forward row %s: %v", payload, err)
		}
		if message.Step == record.MergeForwardReview {
			asks = append(asks, message)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the merge-forward rows: %v", err)
	}
	return asks
}

// C57: each time the head moves while the issue stands past its review, the daemon asks the
// round's recorded reviewer for a verdict at the new head, with no merge forward asked: in retro,
// in accepting and in merging. GitHub holds the new head, which has no legion-verdict, so the
// merger does not merge it. Every step passed at the new head then writes success there: each check
// reading yes at code-2, the reviewer's approve, and the owner's (its verdict past review in retro
// and in merging, its sign-off in accepting); GitHub then answers it clean, and the merger merges
// it. A push in reviewing re-asks the round as on main and asks for no verdict past review. The ask
// reaches the reviewer's own topic while the head it names stands, and a later push, which asks
// again, ends it undelivered.
func TestAHeadThatMovesPastReviewAsksTheRecordedReviewer(t *testing.T) {
	for _, tc := range []struct {
		at     phase.Phase
		status string
		after  phase.Phase
	}{{phase.Retro, "retro", phase.Retro}, {phase.Accepting, "needs_review", phase.Retro}, {phase.Merging, "retro", phase.Merging}} {
		t.Run(string(tc.at), func(t *testing.T) {
			accepted := tc.at != phase.Accepting
			rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, func(t *testing.T, pool *store.Pool) {
				seedCapture(t, pool, tc.at, tc.status, testerPassAtCode, reviewerApprovedCode)
				seedAgents(t, pool, accepted)
				seedChecks(t, pool, "code", bothYes)
				seedStepVerdict(t, pool, planwords.DefaultTask, reviewerStep, "code", verdict.ReviewerApprove, captureReviewer)
				if accepted {
					seedStepVerdict(t, pool, planwords.DefaultTask, acceptanceStep, "code", verdict.ReviewerApprove, captureArchitect)
				}
			})
			rig.pushedTo(t, "code", "code-2")
			want := []record.MergeForwardMessage{{Role: claim.RoleReviewer, Step: record.MergeForwardReview, Head: "code", Landed: "code-2",
				Base: "main", Phase: tc.at, PullRequest: 86}}
			if asks := verdictAsks(t, rig.pool); !slices.Equal(asks, want) {
				t.Fatalf("the push in %s asked %+v, want %+v", tc.at, asks, want)
			}
			if asked := rig.askedHeads(); len(asked) != 0 {
				t.Fatalf("the push asked for writes on %v, want none: no verdict names code-2", asked)
			}
			if state := rig.mergeState(t); state != "blocked" {
				t.Fatalf("GitHub answers the merger %q for code-2 before the review steps' verdicts, want blocked: legion-verdict there is not success", state)
			}

			rig.checkState(t, "code-2", "k1", verdict.StateYes)
			rig.checkState(t, "code-2", "k2", verdict.StateYes)
			rig.apply(t, verdictPastReview("approve", "code-2"))
			if accepted {
				rig.apply(t, ownersPastReview("approve", "code-2"))
			} else {
				rig.apply(t, rig.signOff(t))
			}
			rig.written(t)
			rig.soleRun(t, "code-2", "success")
			if state := rig.mergeState(t); state != "clean" {
				t.Fatalf("GitHub answers the merger %q for code-2 after every step passed there, want clean", state)
			}
			if got, _ := capturePhase(t, rig.pool); got != tc.after {
				t.Fatalf("after the review steps' verdicts the issue is in %s, want %s", got, tc.after)
			}
		})
	}

	t.Run("a push in reviewing re-asks the round and asks for no verdict past review", func(t *testing.T) {
		rig := newVerdictRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, func(t *testing.T, pool *store.Pool) {
			seedCapture(t, pool, phase.Reviewing, "needs_review", testerPassAtCode, reviewerApprovedCode)
			clearCaptureMergeState(t, pool)
		})
		rig.pushedTo(t, "code", "code-2")
		rig.written(t)
		if got, pr := capturePhase(t, rig.pool); got != phase.Reviewing || pr.HeadSHA != "code-2" {
			t.Fatalf("after the push the issue is in %s at %s, want reviewing at code-2: the approve of code approves no other head", got, pr.HeadSHA)
		}
		if asks := verdictAsks(t, rig.pool); len(asks) != 0 {
			t.Fatalf("the push in reviewing asked %+v, want no verdict past review", asks)
		}
	})

	t.Run("the ask reaches the reviewer while its head stands", func(t *testing.T) {
		ask := record.MergeForwardMessage{Role: claim.RoleReviewer, Step: record.MergeForwardReview, Head: "sha-1", Landed: "sha-2", Base: "main",
			Phase: phase.Retro, PullRequest: 86}
		reviewer := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleReviewer))
		for _, head := range []string{"sha-2", "sha-3"} {
			t.Run("the pull request at "+head, func(t *testing.T) {
				publisher := &holderPublisher{}
				logs := &syncBuffer{}
				runner, _, pool := runnerIn(t, phase.Retro, "retro", publisher, slog.New(slog.NewJSONHandler(logs, nil)))
				if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
					return runner.records.PutPullRequest(context.Background(), tx, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208",
						Repo: "acme/widgets", Number: 86, Branch: "legion/LEGION-208", HeadSHA: head, HeadUpdatedAt: time.Now(), Failing: []string{},
						CheckRuns: []record.AttemptRun{}, Base: "main"})
				}); err != nil {
					t.Fatalf("seed the pull request: %v", err)
				}
				enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", ask, time.Now().Add(-time.Minute)))
				drainAt(t, runner, 0, "the ask's attempt")
				_, delivered := publisher.snapshot()
				if head == "sha-2" {
					if len(delivered) != 1 || delivered[0].topic != reviewer || delivered[0].message != ask.Message("LEGION-208", record.PullRequest{Repo: "acme/widgets"}) {
						t.Fatalf("delivered %+v, want the ask once to %s", delivered, reviewer)
					}
					return
				}
				if len(delivered) != 0 || outboxRows(t, pool) != 0 {
					t.Fatalf("delivered %+v with %d rows left, want the ask finished undelivered", delivered, outboxRows(t, pool))
				}
				if lines := loggedLines(t, logs.String(), mergeForwardLeftLine); len(lines) != 1 || !strings.Contains(fmt.Sprint(lines[0]["why"]), "moved on to sha-3") {
					t.Fatalf("the row logged %v, want one line naming the head it moved on to", lines)
				}
			})
		}
	})
}

// R5, "The verdict past review", with the architect's claim released before the push: an idle
// release (workflow's IdleRelease) leaves every claim of an idle pod released, the architect that
// ran the owner's acceptance among them. LEGION-208 stands in retro on the default steps, its
// reviewer's step and its owner's acceptance passed at sha-1, and a push moves its pull request to
// sha-2. The push asks the architect, the agent the acceptance step's record names, for its verdict
// at sha-2 by a role message to its claim. That message is the one way the owner hears of the push,
// and with the default steps legion-verdict at sha-2 waits on the owner's verdict there, so the row
// is held for the released claim: its first attempt wakes the claim (one recorded wake) and
// publishes nothing, and its next attempt, once the woken session holds its role, delivers the ask
// to the architect's topic once. The negative control is roleMessage at 8bafec63091, which ends the
// row before any publish as a message to a released role "not worth a wake": the row is gone after
// its first attempt, nothing is woken, and the owner is never asked.
func TestAHeadThatMovesPastReviewAsksAReleasedOwner(t *testing.T) {
	publisher := &holderPublisher{}
	runner, sup, pool := runnerIn(t, phase.Retro, "retro", publisher, quietLogger())
	holdSlot(t, pool, "LEGION-208")
	architect := mustClaimToken(t, "LEGION-208", claim.RoleArchitect)
	reviewer := mustClaimToken(t, "LEGION-208", claim.RoleReviewer)
	inStore(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		if err := records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "acme/widgets",
			Number: 86, Branch: "legion/LEGION-208", Base: "main", HeadSHA: "sha-1", HeadUpdatedAt: time.Now(), Failing: []string{},
			CheckRuns: []record.AttemptRun{}}); err != nil {
			return err
		}
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: reviewer,
			HandoffCommit: "sha-1", HandoffRecord: "review-1", HandoffVersion: 1, Summary: "reviewed",
			Decision: &record.ReviewDecision{State: "approved", Body: "reviewed", Head: "sha-1"}}); err != nil {
			return err
		}
		for place, agent := range map[int]claim.Token{reviewerStep: reviewer, acceptanceStep: architect} {
			at := record.StepPlace{Task: planwords.DefaultTask, Kind: record.PlaceStep, Place: place}
			started := time.Date(2026, 11, 21, 5, place, 0, 0, time.UTC)
			if _, err := records.RaiseStepStarts(ctx, tx, "LEGION-208", at, func(int64) record.StepRecord {
				return record.StepRecord{Task: at.Task, Step: at.Place, Kind: at.Kind, Agent: string(agent), StartedAt: &started}
			}); err != nil {
				return err
			}
			if _, _, err := records.EndStepStart(ctx, tx, "LEGION-208", at); err != nil {
				return err
			}
		}
		_, err := store.RecordVerdictTx(ctx, tx, store.Verdict{Repository: widgets, Number: 86, Head: "sha-1", Issue: "LEGION-208",
			Task: planwords.DefaultTask, Step: acceptanceStep, Verdict: verdict.ReviewerApprove, Session: string(architect), RecordedAt: recordedAt})
		return err
	})
	machine := releasedClaim(t, sup, claim.RoleArchitect)
	topic := architectTopic(t, "LEGION-208")
	publisher.setAbsent(topic)

	engine := workflow.New(record.NewStore(), workflow.Config{Project: "LEGION", Repositories: testRepositories}, quietLogger())
	applyFact(t, pool, "test", "push-sha-2", intake.PullRequestSynchronized{Repo: "acme/widgets", Number: 86, Branch: "legion/LEGION-208",
		Base: "main", Before: "sha-1", HeadSHA: "sha-2"}, engine)
	asks := roleMessageRows(t, pool, "LEGION-208")
	if len(asks) != 1 {
		t.Fatalf("the push queued %d role messages for LEGION-208, want the one ask of the owner: this test proves nothing without it", len(asks))
	}
	var ask record.RoleMessage
	if err := json.Unmarshal(asks[0].Payload, &ask); err != nil || ask.Role != string(architect) || !strings.Contains(ask.Body, "the new head is sha-2") {
		t.Fatalf("the push's role message = %+v (%v), want the ask of %s for its verdict at sha-2", ask, err, architect)
	}

	toArchitect := func() []outboxPublish {
		_, delivered := publisher.snapshot()
		return slices.DeleteFunc(delivered, func(p outboxPublish) bool { return p.topic != topic })
	}
	drainAt(t, runner, 0, "the ask's first attempt")
	if left := len(roleMessageRows(t, pool, "LEGION-208")); left != 1 {
		t.Fatalf("role messages of LEGION-208 left after the ask's first attempt = %d, want the ask held for the released owner: it ended undelivered", left)
	}
	if held := machine.Claim(); held.Released() {
		t.Fatalf("the architect is %s by %s after the ask's first attempt, want the ask to have woken it", held.State, held.SuspendedBy)
	}
	if wakes := podResumes(t, pool, "LEGION-208"); wakes != 1 {
		t.Fatalf("recorded wakes = %d, want the one the ask took", wakes)
	}
	if got := toArchitect(); len(got) != 0 {
		t.Fatalf("the first attempt delivered %+v to the architect, want nothing before its woken session holds its role", got)
	}

	// The woken session holds its role now.
	publisher.setAbsent()
	drainAt(t, runner, 2*time.Minute, "the ask's attempt once the woken session holds its role")
	if got := toArchitect(); len(got) != 1 || got[0].message != ask.Body || got[0].key != record.OutboxKey(asks[0].ID) {
		t.Fatalf("delivered to the architect %+v, want the ask once under its row's key", got)
	}
	if left := len(roleMessageRows(t, pool, "LEGION-208")); left != 0 {
		t.Fatalf("role messages of LEGION-208 left after the delivery = %d, want none", left)
	}
	if wakes := podResumes(t, pool, "LEGION-208"); wakes != 1 {
		t.Fatalf("recorded wakes = %d after the delivery, want still the one", wakes)
	}
}
