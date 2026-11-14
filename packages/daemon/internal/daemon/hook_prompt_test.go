package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// firedHook is the row a hook of the deployment writes when it fires on a move back: the words of
// the hook, the move, the count that fired it and every backward-move count of the issue.
var firedHook = record.HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer,
	From: phase.Reviewing, To: phase.Implementing, Count: 3,
	Counts:      map[string]int{"reviewing-implementing": 3, "testing-implementing": 1},
	PullRequest: "acme/widgets#86", Prompt: "Read the reviewer's thread before you write anything."}

// A hook prompt is the deployment's own words for the round a move back opened, and the role the
// hook named is who they were written for: the row publishes its own message — the daemon's facts
// of the firing and then the hook's words — to that role's Envoy topic, under the row's own key,
// and the row ends there.
func TestAHookPromptReachesItsRolesOwnTopicAndEndsTheRow(t *testing.T) {
	ctx := context.Background()
	publisher := &outboxPublisher{}
	runner, _, pool := hookRunner(t, publisher)
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", firedHook, time.Now().Add(-time.Minute)))

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("drain the hook prompt: %v", err)
	}

	topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleImplementer))
	published := publisher.publishes()
	if len(published) != 1 || published[0].topic != topic {
		t.Fatalf("publishes = %#v, want the one hook prompt to %s", published, topic)
	}
	want := firedHook.Message("LEGION-208")
	if published[0].message != want || published[0].payload != any(want) {
		t.Fatalf("the implementer was told\n\t%q\nwant\n\t%q", published[0].message, want)
	}
	t.Logf("the implementer was told: %s", published[0].message)
	if _, ok := record.ParseOutboxKey(published[0].key); !ok {
		t.Fatalf("the prompt went under key %q, want its row's own (legion-outbox:<id>): a retried row is one delivery", published[0].key)
	}
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left = %d, want the row finished", left)
	}
}

// A role topic with no live holder is the ordinary state for a role an issue has never had, and
// the hook's words are of the round the move back opened: the row ends with one log line rather
// than waiting for a reader that would read them of a round that is over.
func TestAHookPromptNoSessionHoldsEndsTheRow(t *testing.T) {
	ctx := context.Background()
	publisher := &holderPublisher{}
	runner, _, pool := hookRunner(t, publisher)
	topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleImplementer))
	publisher.setAbsent(topic)
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", firedHook, time.Now().Add(-time.Minute)))

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("drain the hook prompt: %v", err)
	}

	attempts, delivered := publisher.snapshot()
	if attempts != 1 || len(delivered) != 0 {
		t.Fatalf("publish attempts = %d with %#v delivered, want the one refused attempt and no delivery", attempts, delivered)
	}
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left = %d, want the prompt finished rather than held", left)
	}
}

// A role whose pod was released holds its session still, and its pod is one wake away: the row
// wakes the pod and waits on its backoff. Published instead, it would meet the no-holder answer
// above and finish undelivered, so the role would never read the words its own round was given.
func TestAHookPromptForAReleasedPodWakesItAndWaits(t *testing.T) {
	ctx := context.Background()
	publisher := &outboxPublisher{}
	runner, sup, pool := hookRunner(t, publisher)
	implementer := releasedClaim(t, sup, claim.RoleImplementer)
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", firedHook, time.Now().Add(-time.Minute)))

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("drain the hook prompt: %v", err)
	}

	if published := publisher.publishes(); len(published) != 0 {
		t.Fatalf("publishes = %#v, want none: the pod is coming back for the prompt", published)
	}
	if left := outboxRows(t, pool); left != 1 {
		t.Fatalf("outbox rows left = %d, want the prompt waiting for the woken pod", left)
	}
	if held := implementer.Claim(); held.Released() {
		t.Fatalf("the implementer is %s by %s, want the wake to have resumed it", held.State, held.SuspendedBy)
	}
	var wakes int
	if err := pool.QueryRow(ctx, `select count(*) from pod_resumes where issue = $1 and role = $2 and kind = $3`,
		"LEGION-208", string(claim.RoleImplementer), pods.WakeDelivery).Scan(&wakes); err != nil {
		t.Fatalf("read the recorded wakes: %v", err)
	}
	if wakes != 1 {
		t.Fatalf("recorded delivery wakes = %d, want the one this row took", wakes)
	}
}

// hookRunner is an outbox runner over an isolated database holding LEGION-208 in implementing,
// the phase a move back to implementing leaves it in, with the admission slot its work holds. It
// drains on the caller's goroutine (RunOnce), as the red's runner does.
func hookRunner(t *testing.T, publisher notify.Publisher) (*outbox, *supervisor, *store.Pool) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, issue)
	holdSlot(t, pool, issue.Key)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{
		dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: outboxTreeStore(t, pool, issue.Tree),
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), notices: publisher,
		now: func() time.Time { return time.Now() }, log: quietLogger(),
	}
	return runner, sup, pool
}

// releasedClaim creates role's claim on LEGION-208 as an idle release left it: suspended with the
// source `released`, its session intact, and nothing but a wake to bring its pod back.
func releasedClaim(t *testing.T, sup *supervisor, role claim.Role) *supervise.Machine {
	t.Helper()
	machine, _, err := sup.Create(context.Background(), supervise.Claim{Token: mustClaimToken(t, "LEGION-208", role),
		Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: role, Generation: 1, ServingGeneration: 1,
		State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased,
		Session: "ses-" + string(role), SessionFile: t.TempDir() + "/session.jsonl"}, "")
	if err != nil {
		t.Fatalf("create the released %s's claim: %v", role, err)
	}
	if held := machine.Claim(); !held.Released() {
		t.Fatalf("the %s is %s by %s, want it released", role, held.State, held.SuspendedBy)
	}
	return machine
}
