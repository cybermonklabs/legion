package daemon

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// roleWaitCase is one of the nine outbox kinds that waits for a role (record.OutboxKind.WaitsForRole),
// with the role it targets on LEGION-208, its payload, and how a role wait treats it:
//   - endsNoClaim: the kind ends undelivered with no publish when its role has no claim at all
//     (dispatch://INFRA-4131/spec#b-r-wait-ready, P4).
//   - wakesReleased: the kind wakes a released claim of a wakeable issue itself (wakeForDelivery /
//     deliveryWake) rather than publishing on the capped waits.
//   - endsOnClaimEnd: the kind ends undelivered when its role's claim fails or retires (false for
//     controller_notice under `controller: daemon`, whose failed or retired controller claim
//     controllerKeeper retries).
//   - endsAtHour: hook_prompt and phase_ended, whose kept-message hour bounds their wait.
//   - updateRound: non-empty for update_ask, whose claim must carry this round.
//   - controllerLaunched: true for controller_notice under `controller: daemon`.
type roleWaitCase struct {
	kind               record.OutboxKind
	role               claim.Role
	endsNoClaim        bool
	wakesReleased      bool
	endsOnClaimEnd     bool
	endsAtHour         bool
	updateRound        string
	controllerLaunched bool
}

// roleWaitCases is all nine kinds of record.OutboxKind.WaitsForRole, in one order, for every
// per-kind test of the role-wait state table.
var roleWaitCases = []roleWaitCase{
	{kind: record.OutboxKindNotice, role: claim.RoleArchitect, endsNoClaim: true, wakesReleased: true, endsOnClaimEnd: true},
	{kind: record.OutboxKindControllerNotice, role: claim.RoleController, controllerLaunched: true},
	{kind: record.OutboxKindMergeQueueRemoval, role: claim.RoleMerger, wakesReleased: true, endsOnClaimEnd: true},
	{kind: record.OutboxKindMergeHoldsChanged, role: claim.RoleMerger, wakesReleased: true, endsOnClaimEnd: true},
	{kind: record.OutboxKindMergeForward, role: claim.RoleMerger, wakesReleased: true, endsOnClaimEnd: true},
	{kind: record.OutboxKindHookPrompt, role: claim.RoleImplementer, wakesReleased: true, endsOnClaimEnd: true, endsAtHour: true},
	{kind: record.OutboxKindPhaseEnded, role: claim.RoleImplementer, wakesReleased: true, endsOnClaimEnd: true, endsAtHour: true},
	{kind: record.OutboxKindRoleMessage, role: claim.RoleImplementer, endsNoClaim: true, endsOnClaimEnd: true},
	{kind: record.OutboxKindUpdateAsk, role: claim.RoleImplementer, endsNoClaim: true, endsOnClaimEnd: true, updateRound: "pod:uid-1"},
}

// roleWaitPublisher is a notify.Publisher that counts every publish attempt (Publish and
// PublishKept), records each publish with the clock moment it arrived at, and answers each with
// err (or takes it with holder when err is nil).
type roleWaitPublisher struct {
	clock     *heldClock
	holder    func() string
	err       error
	publishes []roleWaitPublish
}

type roleWaitPublish struct {
	at      time.Time
	topic   string
	message string
	key     string
	keep    bool
}

func (p *roleWaitPublisher) Publish(_ context.Context, topic, message string, _ any, key string) (string, error) {
	p.publishes = append(p.publishes, roleWaitPublish{at: p.clock.Now(), topic: topic, message: message, key: key})
	if p.err != nil {
		return "", p.err
	}
	if p.holder != nil {
		return p.holder(), nil
	}
	return "", nil
}

func (p *roleWaitPublisher) PublishKept(_ context.Context, topic, message string, _ any, key string) (bool, error) {
	p.publishes = append(p.publishes, roleWaitPublish{at: p.clock.Now(), topic: topic, message: message, key: key, keep: true})
	if p.err != nil {
		return false, p.err
	}
	return false, nil
}

func (p *roleWaitPublisher) count() int { return len(p.publishes) }

// roleWaitHarness is one isolated database, outbox runner, supervisor, publisher and fake clock
// seeded for LEGION-208 in merging with an open pull request #86 at sha-1 holding a merger
// merge-forward ask: a state where every one of the nine kinds is live and owed.
type roleWaitHarness struct {
	t         *testing.T
	tc        roleWaitCase
	clock     *heldClock
	start     time.Time
	pool      *store.Pool
	records   record.Store
	sup       *supervisor
	trees     *store.Store
	publisher *roleWaitPublisher
	runner    *outbox
	token     claim.Token
	topic     string
}

func newRoleWaitHarness(t *testing.T, tc roleWaitCase, refusal error) *roleWaitHarness {
	t.Helper()
	clock := roleWaitClock()
	start := clock.Now()
	publisher := &roleWaitPublisher{clock: clock, err: refusal}
	runner, sup, pool := runnerIn(t, phase.Merging, "retro", publisher, quietLogger())
	sup.deps.Clock = clock
	runner.now = clock.Now
	runner.controllerLaunched = tc.controllerLaunched
	holdSlot(t, pool, "LEGION-208")
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		return runner.records.PutPullRequest(ctx, tx, record.PullRequest{
			State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "acme/widgets", Number: 86,
			Branch: "legion/LEGION-208", HeadSHA: "sha-1", HeadUpdatedAt: start,
			Failing: []string{}, CheckRuns: []record.AttemptRun{}, Base: "main",
			MergeForward: &record.MergeForward{Head: "sha-1", Base: "main", Role: claim.RoleMerger,
				Phase: phase.Merging, Why: record.ConflictMergeNext, AskedAt: start},
		})
	}); err != nil {
		t.Fatalf("seed LEGION-208's pull request: %v", err)
	}
	var token claim.Token
	var topic string
	if tc.kind == record.OutboxKindControllerNotice {
		token = claim.ControllerToken("legion")
		topic = notify.ControllerTopic("legion")
	} else {
		token = mustClaimToken(t, "LEGION-208", tc.role)
		topic = notify.RoleTopicPrefix + string(token)
	}
	trees, ok := runner.trees.(*store.Store)
	if !ok {
		t.Fatalf("runner.trees is %T, want *store.Store", runner.trees)
	}
	return &roleWaitHarness{
		t: t, tc: tc, clock: clock, start: start, pool: pool, records: runner.records,
		sup: sup, trees: trees, publisher: publisher, runner: runner, token: token, topic: topic,
	}
}

// payload builds a valid payload of h.tc's kind for LEGION-208.
func (h *roleWaitHarness) payload() record.OutboxPayload {
	switch h.tc.kind {
	case record.OutboxKindNotice:
		return record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "review_round_cap"}
	case record.OutboxKindControllerNotice:
		return record.ControllerNotice{Kind: "held", Role: claim.RolePlanner, Phase: phase.Planning}
	case record.OutboxKindMergeQueueRemoval:
		return record.MergeQueueRemoval{Number: 86, Head: "sha-1"}
	case record.OutboxKindMergeHoldsChanged:
		return record.MergeHoldsChanged{Project: "LEGION"}
	case record.OutboxKindMergeForward:
		return record.MergeForwardMessage{Role: claim.RoleMerger, Step: record.MergeForwardAsk, Head: "sha-1",
			Base: "main", Phase: phase.Merging, PullRequest: 86, Why: record.ConflictMergeNext}
	case record.OutboxKindHookPrompt:
		return record.HookPrompt{Hook: "review", Role: claim.RoleImplementer, From: phase.Reviewing,
			To: phase.Implementing, Count: 1, Counts: map[string]int{"review": 1}, Prompt: "Address the review."}
	case record.OutboxKindPhaseEnded:
		return record.PhaseEnded{Role: claim.RoleImplementer, From: phase.Implementing, To: phase.AwaitingChildren,
			Reason: "The parent moved to awaiting_children."}
	case record.OutboxKindRoleMessage:
		return record.RoleMessage{Role: string(h.token), Body: "CI settled at head sha-1"}
	case record.OutboxKindUpdateAsk:
		return record.UpdateAsk{Role: claim.RoleImplementer, Round: h.tc.updateRound, Pod: "pod-1", Text: "Finish your turn for the pod update."}
	default:
		h.t.Fatalf("unexpected kind %s", h.tc.kind)
		return nil
	}
}

// queue enqueues one row of h.tc's kind at h.clock.Now() and returns its rig.
func (h *roleWaitHarness) queue() *roleWaitRig {
	h.t.Helper()
	row := mustOutboxRow(h.t, "LEGION-208", h.payload(), h.clock.Now())
	id := queueRow(h.t, h.pool, h.records, row)
	return newRoleWaitRig(h.t, h.runner, h.pool, h.clock, id)
}

// claim creates h.token's claim in state, with optional overrides through tune.
func (h *roleWaitHarness) claim(state supervise.ClaimState, tune func(*supervise.Claim)) *supervise.Machine {
	h.t.Helper()
	c := supervise.Claim{
		Token: h.token, Project: "legion", Role: h.tc.role, Generation: 1, ServingGeneration: 1,
		State: state, UpdateRound: h.tc.updateRound,
	}
	if h.tc.role != claim.RoleController {
		c.Tree, c.Issue = "LEGION-208", "LEGION-208"
	}
	if !supervise.Unregistered(state) {
		c.Session = "ses-" + string(h.tc.role)
		c.SessionFile = h.t.TempDir() + "/session.jsonl"
	}
	if state != supervise.StateFailed && state != supervise.StateRetired && state != supervise.StateSuspended && state != supervise.StateQueued {
		c.Locator = &runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: h.token, Incarnation: "1:1",
			Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"}}
	}
	if supervise.SaidReady(state) {
		c.LastReady = h.start.Add(-10 * time.Minute)
		c.LastTurnStart = h.start.Add(-5 * time.Minute)
	}
	if tune != nil {
		tune(&c)
	}
	h.sup.deps.Conns.(*fake.Conns).Register(h.token, fake.NewConn())
	machine := roleWaitClaim(h.t, h.sup, c)
	if h.tc.role != claim.RoleController {
		if err := h.trees.PutClaim(context.Background(), machine.Claim()); err != nil {
			h.t.Fatalf("record %s in store: %v", h.token, err)
		}
	}
	h.publisher.holder = func() string { return machine.View().Session }
	return machine
}

// refusalError builds the listener error for each refusal answer of R2.
func refusalError(name string) error {
	switch name {
	case "unclaimed":
		return fmt.Errorf(`publish notice: listener returned 404 Not Found: {"reason":"unclaimed"}: %w`, notify.ErrNoHolder)
	case "holder_lapsed_gone":
		return fmt.Errorf(`publish notice: listener returned 404 Not Found: {"reason":"holder_lapsed","holder":"ses-old","holder_state":"gone"}: %w`, notify.ErrNoHolder)
	case "unresponsive":
		return fmt.Errorf(`publish notice: listener returned 404 Not Found: {"reason":"holder_lapsed","holder":"ses-1","holder_state":"unresponsive"}: %w`, notify.ErrHolderUnresponsive)
	case "receipt_timeout":
		return confirmedAnswer(notify.ReasonReceiptTimeout, "ses-1")
	case "delivery_failed":
		return confirmedAnswer(notify.ReasonDeliveryFailed, "ses-1")
	case "capability":
		return fmt.Errorf(`publish notice: listener returned 403 Forbidden: {"reason":"capability","holder":"ses-1"}: %w`, notify.ErrRefusedByHolder{Holder: "ses-1"})
	case "denied":
		return fmt.Errorf(`publish notice: listener returned 403 Forbidden: {"error":"envoy: the message bus denied the publish"}: %w`,
			notify.ErrPublishDenied{Message: "envoy: the message bus denied the publish under this listener's own permissions"})
	default:
		panic("unknown refusal " + name)
	}
}

// S1 (R1): while a role's claim does not exist in the daemon's record, a waiting kind publishes
// nothing across 20 runs and stays queued, and an ending kind (notice, role_message with no pause,
// update_ask) publishes nothing and ends undelivered at its first run.
func TestRoleWaitNoClaimPublishesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range roleWaitCases {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			h := newRoleWaitHarness(t, tc, refusalError("unclaimed"))
			rig := h.queue()
			if tc.endsNoClaim {
				rig.endsAtItsNextRun()
				if got := h.publisher.count(); got != 0 {
					t.Fatalf("ending kind %s with no claim published %d times, want 0", tc.kind, got)
				}
				return
			}
			rig.runTo(roleWaitRuns)
			if got := h.publisher.count(); got != 0 {
				t.Fatalf("waiting kind %s with no claim published %d times across %d runs, want 0", tc.kind, got, roleWaitRuns)
			}
			if _, _, queued := rig.state(); !queued {
				t.Fatalf("waiting kind %s left the outbox with no claim, want it waiting", tc.kind)
			}
		})
	}
}

// S3 (R1): a claim whose launch has not registered its session (queued, launch_uncertain,
// launching, shim_connected) receives no publish from any of the nine kinds.
func TestRoleWaitLaunchPublishesNothingBeforeItRegisters(t *testing.T) {
	t.Parallel()
	states := []supervise.ClaimState{
		supervise.StateQueued,
		supervise.StateLaunchUncertain,
		supervise.StateLaunching,
		supervise.StateShimConnected,
	}
	for _, tc := range roleWaitCases {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			for _, state := range states {
				t.Run(string(state), func(t *testing.T) {
					t.Parallel()
					h := newRoleWaitHarness(t, tc, refusalError("unclaimed"))
					h.claim(state, nil)
					rig := h.queue()
					rig.runTo(5)
					if got := h.publisher.count(); got != 0 {
						t.Fatalf("kind %s to a %s claim published %d times across 5 runs, want 0", tc.kind, state, got)
					}
					if _, _, queued := rig.state(); !queued {
						t.Fatalf("kind %s to a %s claim left the outbox, want it waiting", tc.kind, state)
					}
				})
			}
		})
	}
}

// S4 (R1): a claim that launches three times (generations 1, 2, 3), registering each time without
// saying it is ready, receives at most one publish per launch — three in all across repeated runs
// in each generation.
func TestRoleWaitLaunchPublishesAtMostOncePerLaunch(t *testing.T) {
	t.Parallel()
	for _, tc := range roleWaitCases {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			// Unresponsive keeps every one of the nine kinds queued on a refusal (role_message and
			// update_ask end on ErrNoHolder), so the test exercises the once-per-launch gate across
			// all three launches for every kind.
			h := newRoleWaitHarness(t, tc, refusalError("unresponsive"))
			machine := h.claim(supervise.StateRegistered, nil)
			rig := h.queue()

			for gen := uint64(1); gen <= 3; gen++ {
				if got := machine.Claim().Generation; got != gen {
					t.Fatalf("generation = %d, want %d", got, gen)
				}
				// Three runs in this generation: the first publishes once and stores wait_launch;
				// the next two wait without publishing.
				before := h.publisher.count()
				rig.run()
				rig.run()
				rig.run()
				if got := h.publisher.count() - before; got != 1 {
					t.Fatalf("kind %s at registered generation %d published %d times across 3 runs, want 1", tc.kind, gen, got)
				}
				if gen < 3 {
					// Move the claim into its next launch (generation gen+1), keeping UpdateRound
					// where update_ask needs it, and leaving the new launch unregistered first.
					relaunchToUnregistered(t, machine, tc)
					unregBefore := h.publisher.count()
					rig.run()
					if got := h.publisher.count() - unregBefore; got != 0 {
						t.Fatalf("kind %s at unregistered generation %d published %d times, want 0", tc.kind, gen+1, got)
					}
					comesUp(t, machine, false)
				}
			}
			if got := h.publisher.count(); got != 3 {
				t.Fatalf("kind %s across 3 launches published %d times in all, want 3", tc.kind, got)
			}
		})
	}
}

// relaunchToUnregistered advances machine's claim to its next launch (Generation+1) in an
// unregistered launching state. Root architects and the daemon controller cannot be retired via
// operator RequestStop / RequestAskedRetry; a suspend+resume advances every claim cleanly and
// keeps UpdateRound on update_ask claims.
func relaunchToUnregistered(t *testing.T, machine *supervise.Machine, tc roleWaitCase) {
	t.Helper()
	ctx := context.Background()
	before := machine.Claim()
	if err := machine.Handle(ctx, supervise.RequestSuspend{Claim: before.Token, By: supervise.SuspendedByOperator}); err != nil {
		t.Fatalf("suspend %s at generation %d: %v", tc.kind, before.Generation, err)
	}
	machine.Wait()
	if err := machine.Handle(ctx, supervise.RequestResume{Claim: before.Token, By: supervise.SuspendedByOperator}); err != nil {
		t.Fatalf("resume %s past generation %d: %v", tc.kind, before.Generation, err)
	}
	machine.Wait()
	if got := machine.Claim(); !supervise.Unregistered(got.State) || got.Generation != before.Generation+1 {
		t.Fatalf("after suspend+resume %s is %s at generation %d, want unregistered generation %d",
			tc.kind, got.State, got.Generation, before.Generation+1)
	}
}

// S6 (R1): a released claim with no launch in progress:
//   - kinds that wake the role themselves (notice, the three deliverHeld kinds, hook_prompt,
//     phase_ended) ask the daemon's wake (deliveryWake), publish nothing while the launch comes up,
//     and the launch comes even with the listener's wake report dropped (the publisher reports
//     nothing);
//   - a controller_notice under `controller: daemon` (no wake of its own) and a pause-held
//     role_message publish on R2's capped waits, so a dropped wake report is followed by another
//     publish within 5 minutes;
//   - a role_message no pause holds and an update_ask end undelivered with no publish.
func TestRoleWaitReleasedRoleIsWokenOrKeepsTheCappedWaits(t *testing.T) {
	t.Parallel()
	for _, tc := range roleWaitCases {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			h := newRoleWaitHarness(t, tc, refusalError("unclaimed"))
			machine := h.claim(supervise.StateSuspended, func(c *supervise.Claim) {
				c.SuspendedBy = supervise.SuspendedByReleased
			})
			rig := h.queue()

			switch {
			case tc.wakesReleased:
				rig.run()
				machine.Wait()
				if got := h.publisher.count(); got != 0 {
					t.Fatalf("waking kind %s published %d times on its wake run, want 0", tc.kind, got)
				}
				if held := machine.Claim(); held.Released() || held.Generation < 2 {
					t.Fatalf("waking kind %s left claim %s by %s at gen %d, want it woken into a new launch",
						tc.kind, held.State, held.SuspendedBy, held.Generation)
				}
				// Even with no listener wake report ever delivered, the launch is under way and
				// further runs before registration publish nothing.
				rig.run()
				if got := h.publisher.count(); got != 0 {
					t.Fatalf("waking kind %s published %d times while the woken launch has not registered, want 0", tc.kind, got)
				}
			case tc.kind == record.OutboxKindControllerNotice:
				// No wake of its own: publishes on R2's capped waits.
				rig.runTo(12)
				if got := h.publisher.count(); got != 12 {
					t.Fatalf("released controller_notice published %d times across 12 runs, want 12 on R2's waits", got)
				}
				times := publishOffsets(h.publisher.publishes, h.start)
				if got := times[11] - times[10]; got != record.RoleWaitBackoffMost {
					t.Fatalf("gap at cap = %s, want %s", got, record.RoleWaitBackoffMost)
				}
			default:
				// role_message (no pause) and update_ask end undelivered with no publish.
				rig.endsAtItsNextRun()
				if got := h.publisher.count(); got != 0 {
					t.Fatalf("released %s published %d times, want 0", tc.kind, got)
				}
			}
		})
	}
	t.Run("role_message_held_by_pause", func(t *testing.T) {
		t.Parallel()
		tc := roleWaitCase{kind: record.OutboxKindRoleMessage, role: claim.RoleImplementer, endsOnClaimEnd: true}
		h := newRoleWaitHarness(t, tc, refusalError("holder_lapsed_gone"))
		h.claim(supervise.StateSuspended, func(c *supervise.Claim) {
			c.SuspendedBy = supervise.SuspendedByReleased
		})
		// Record a project pause that stopped this claim, and mark the row held by that pause,
		// so heldMessageOwed answers that pause and the row publishes on R2's capped waits.
		pauseID := seedPauseStop(t, h.pool, "LEGION", "LEGION-208", h.token, h.start.Add(-time.Minute))
		row := mustOutboxRow(t, "LEGION-208", h.payload(), h.clock.Now())
		row.HeldByPause = pauseID
		id := queueRow(t, h.pool, h.records, row)
		if _, err := h.pool.Exec(context.Background(), `update outbox set held_by_pause = $1 where id = $2`, pauseID, id); err != nil {
			t.Fatalf("mark row %d held by pause %d: %v", id, pauseID, err)
		}
		rig := newRoleWaitRig(t, h.runner, h.pool, h.clock, id)

		rig.runTo(12)
		if got := h.publisher.count(); got != 12 {
			t.Fatalf("pause-held role_message published %d times across 12 runs, want 12 on R2's waits", got)
		}
		times := publishOffsets(h.publisher.publishes, h.start)
		if got := times[11] - times[10]; got != record.RoleWaitBackoffMost {
			t.Fatalf("pause-held role_message gap at cap = %s, want %s", got, record.RoleWaitBackoffMost)
		}
	})
}

// seedPauseStop inserts a completed project pause and a stopped pause-stop row for token so
// store.Store.StoppedByAPause returns the pause id.
func seedPauseStop(t *testing.T, pool *store.Pool, project, issue string, token claim.Token, at time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	var pauseID int64
	if err := pool.QueryRow(ctx, `insert into project_pauses (project, began_at, by_kind, reason, ended_at, ended_by_kind, ended_by_session)
		values ($1, $2, 'operator', 'test', $3, 'operator', '') returning id`,
		project, at.Add(-time.Minute), at).Scan(&pauseID); err != nil {
		t.Fatalf("insert project_pauses: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into project_pause_stops (pause_id, project, claim, issue, role, asked_at, ended_at, ended_as)
		values ($1, $2, $3, $4, $5, $6, $6, $7)`,
		pauseID, project, string(token), issue, string(claim.RoleImplementer), at, string(store.PauseStopStopped)); err != nil {
		t.Fatalf("insert project_pause_stops: %v", err)
	}
	return pauseID
}

// publishOffsets is each publish's elapsed duration since start.
func publishOffsets(publishes []roleWaitPublish, start time.Time) []time.Duration {
	out := make([]time.Duration, len(publishes))
	for i, p := range publishes {
		out[i] = p.at.Sub(start)
	}
	return out
}

// S9 (R2): a ready claim (with a turn started since its ready) whose every publish is refused on
// every refusal that keeps its kind's row doubles from 1 s to 5 minutes: publish times 0, 1, 3, 7,
// 15, 31, 63, 127, 255, 511 s then every 300 s (at most 12 in an hour once at the cap); never
// ended while the claim stands (except hook_prompt and phase_ended, which end at their hour, as
// today); and ended once the claim is retired (except controller_notice under `controller: daemon`,
// whose failed/retired controller is retried by controllerKeeper).
// Task t7 adds the notice/receipt_timeout subtest once P9 lands.
func TestRoleWaitCapDoublesToFiveMinutesForEveryRefusal(t *testing.T) {
	t.Parallel()
	type capSubtest struct {
		tc       roleWaitCase
		refusals []string
	}
	// Every refusal that keeps a row of each kind, per plan ## Readings (task t7 adds
	// notice/receipt_timeout).
	cases := []capSubtest{
		{tc: roleWaitCases[0], refusals: []string{"unclaimed", "holder_lapsed_gone", "unresponsive", "delivery_failed"}},
		{tc: roleWaitCases[1], refusals: []string{"unclaimed", "holder_lapsed_gone", "unresponsive", "receipt_timeout", "delivery_failed", "capability", "denied"}},
		{tc: roleWaitCases[2], refusals: []string{"unclaimed", "holder_lapsed_gone", "unresponsive", "receipt_timeout", "delivery_failed", "capability", "denied"}},
		{tc: roleWaitCases[3], refusals: []string{"unclaimed", "holder_lapsed_gone", "unresponsive", "receipt_timeout", "delivery_failed", "capability", "denied"}},
		{tc: roleWaitCases[4], refusals: []string{"unclaimed", "holder_lapsed_gone", "unresponsive", "receipt_timeout", "delivery_failed", "capability", "denied"}},
		{tc: roleWaitCases[5], refusals: []string{"unclaimed", "holder_lapsed_gone", "unresponsive", "receipt_timeout", "delivery_failed", "capability", "denied"}},
		{tc: roleWaitCases[6], refusals: []string{"unclaimed", "holder_lapsed_gone", "unresponsive", "receipt_timeout", "delivery_failed", "capability", "denied"}},
		{tc: roleWaitCases[7], refusals: []string{"unresponsive", "receipt_timeout", "delivery_failed"}},
		{tc: roleWaitCases[8], refusals: []string{"unresponsive", "receipt_timeout", "delivery_failed", "capability", "denied"}},
	}
	wantFirstTen := []time.Duration{
		0, 1 * time.Second, 3 * time.Second, 7 * time.Second, 15 * time.Second,
		31 * time.Second, 63 * time.Second, 127 * time.Second, 255 * time.Second, 511 * time.Second,
	}
	for _, group := range cases {
		t.Run(string(group.tc.kind), func(t *testing.T) {
			t.Parallel()
			for _, refusal := range group.refusals {
				t.Run(refusal, func(t *testing.T) {
					t.Parallel()
					h := newRoleWaitHarness(t, group.tc, refusalError(refusal))
					machine := h.claim(supervise.StateWorking, nil)
					rig := h.queue()

					if group.tc.endsAtHour {
						// Kept messages (hook_prompt, phase_ended) run up to their hour: 10
						// doubling publishes by 511 s, then 10 at the 300 s cap (811..3511 s),
						// and end at the 21st run at 3811 s (>= 3600 s).
						rig.runTo(20)
						want := append(slices.Clone(wantFirstTen),
							811*time.Second, 1111*time.Second, 1411*time.Second, 1711*time.Second, 2011*time.Second,
							2311*time.Second, 2611*time.Second, 2911*time.Second, 3211*time.Second, 3511*time.Second)
						if got := publishOffsets(h.publisher.publishes, h.start); !slices.Equal(got, want) {
							t.Fatalf("publish offsets = %v, want %v", got, want)
						}
						rig.endsAtItsNextRun()
						if got := h.publisher.count(); got != 20 {
							t.Fatalf("past-its-hour run published %d times in all, want 20 (no publish on the ending run)", got)
						}
						return
					}

					// Every other kind is never ended while its claim stands: drive 25 runs
					// (well past an hour at the 5-minute cap).
					rig.runTo(25)
					got := publishOffsets(h.publisher.publishes, h.start)
					if len(got) != 25 {
						t.Fatalf("publishes = %d, want 25", len(got))
					}
					if !slices.Equal(got[:10], wantFirstTen) {
						t.Fatalf("first 10 publish offsets = %v, want %v", got[:10], wantFirstTen)
					}
					for i := 10; i < len(got); i++ {
						if gap := got[i] - got[i-1]; gap != record.RoleWaitBackoffMost {
							t.Fatalf("gap between publish %d and %d = %s, want %s", i-1, i, gap, record.RoleWaitBackoffMost)
						}
					}
					// In any 60-minute window starting at the cap (from publish 10 onward),
					// at most 12 publishes occur (every 300 s).
					capStart := got[10]
					inHour := 0
					for _, at := range got[10:] {
						if at < capStart+time.Hour {
							inHour++
						}
					}
					if inHour > 12 {
						t.Fatalf("publishes in an hour at the cap = %d, want <= 12", inHour)
					}

					// Once the claim retires, every kind with a claim-end rule ends undelivered
					// with no further publish. A tree root's architect retires via RequestTreeClose
					// (a root claim refuses RequestStop); every other claim retires via RequestStop.
					if group.tc.endsOnClaimEnd {
						var retire supervise.Event = supervise.RequestStop{Claim: h.token, By: supervise.SuspendedByOperator}
						if group.tc.role == claim.RoleArchitect {
							retire = supervise.RequestTreeClose{Claim: h.token}
						}
						if err := machine.Handle(context.Background(), retire); err != nil {
							t.Fatalf("retire %s: %v", h.token, err)
						}
						machine.Wait()
						before := h.publisher.count()
						rig.endsAtItsNextRun()
						if h.publisher.count() != before {
							t.Fatalf("kind %s published on its retired-claim run, want 0", group.tc.kind)
						}
					}
				})
			}
		})
	}
}

// S12 (R2): a row refused while its role is ready and then taken ends at the publish that was
// taken.
func TestRoleWaitTakenEndsTheRowAtThePublishThatWasTaken(t *testing.T) {
	t.Parallel()
	for _, tc := range roleWaitCases {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			h := newRoleWaitHarness(t, tc, refusalError("unresponsive"))
			h.claim(supervise.StateWorking, nil)
			rig := h.queue()

			rig.runTo(5)
			if got := h.publisher.count(); got != 5 {
				t.Fatalf("refused publishes = %d, want 5", got)
			}
			h.publisher.err = nil
			rig.endsAtItsNextRun()
			if got := h.publisher.count(); got != 6 {
				t.Fatalf("total publishes = %d, want 6 (5 refused + 1 taken)", got)
			}
		})
	}
}

// S13 (R1): a claim that fails or retires ends the row undelivered with no publish for every kind
// that ends on a failed or retired claim (and holds a controller_notice under `controller: daemon`
// with no publish while controllerKeeper brings the controller back).
func TestRoleWaitLaunchThatFailsOrRetiresEndsTheRow(t *testing.T) {
	t.Parallel()
	for _, tc := range roleWaitCases {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			for _, state := range []supervise.ClaimState{supervise.StateFailed, supervise.StateRetired} {
				t.Run(string(state), func(t *testing.T) {
					t.Parallel()
					h := newRoleWaitHarness(t, tc, refusalError("unclaimed"))
					h.claim(state, nil)
					rig := h.queue()
					if tc.endsOnClaimEnd {
						rig.endsAtItsNextRun()
						if got := h.publisher.count(); got != 0 {
							t.Fatalf("kind %s to a %s claim published %d times, want 0", tc.kind, state, got)
						}
						return
					}
					// controller_notice under `controller: daemon`: waits with no publish.
					rig.runTo(5)
					if got := h.publisher.count(); got != 0 {
						t.Fatalf("controller_notice to a %s claim published %d times, want 0", state, got)
					}
					if _, _, queued := rig.state(); !queued {
						t.Fatalf("controller_notice to a %s claim left the outbox, want it waiting for controllerKeeper's retry", state)
					}
				})
			}
		})
	}
}

// S14 (R2): a controller_notice under `controller: operator`, whose topic is held to no claim of
// this daemon, publishes on R2's capped waits when the listener refuses it.
func TestRoleWaitUnheldControllerTopicKeepsTheCappedWaits(t *testing.T) {
	t.Parallel()
	tc := roleWaitCase{kind: record.OutboxKindControllerNotice, role: claim.RoleController, controllerLaunched: false}
	h := newRoleWaitHarness(t, tc, refusalError("unclaimed"))
	rig := h.queue()

	rig.runTo(15)
	got := publishOffsets(h.publisher.publishes, h.start)
	wantFirstTen := []time.Duration{
		0, 1 * time.Second, 3 * time.Second, 7 * time.Second, 15 * time.Second,
		31 * time.Second, 63 * time.Second, 127 * time.Second, 255 * time.Second, 511 * time.Second,
	}
	if !slices.Equal(got[:10], wantFirstTen) {
		t.Fatalf("first 10 publish offsets = %v, want %v", got[:10], wantFirstTen)
	}
	for i := 10; i < len(got); i++ {
		if gap := got[i] - got[i-1]; gap != record.RoleWaitBackoffMost {
			t.Fatalf("gap between publish %d and %d = %s, want %s", i-1, i, gap, record.RoleWaitBackoffMost)
		}
	}
	h.publisher.err = nil
	rig.endsAtItsNextRun()
}

// R1: a role_message no pause holds, whose role's claim is ready, ends undelivered at its first
// 404 unclaimed from the listener.
func TestRoleWaitReadyRoleMessageEndsAtNoHolder(t *testing.T) {
	t.Parallel()
	tc := roleWaitCase{kind: record.OutboxKindRoleMessage, role: claim.RoleImplementer, endsNoClaim: true, endsOnClaimEnd: true}
	h := newRoleWaitHarness(t, tc, refusalError("unclaimed"))
	h.claim(supervise.StateWorking, nil)
	rig := h.queue()

	rig.endsAtItsNextRun()
	if got := h.publisher.count(); got != 1 {
		t.Fatalf("ready role_message refused unclaimed published %d times, want 1", got)
	}
}

// R2: every publish of a waiting row carries the row's own key, legion-outbox:<row>.
func TestRoleWaitCapEveryPublishCarriesTheRowsKey(t *testing.T) {
	t.Parallel()
	for _, tc := range roleWaitCases {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			h := newRoleWaitHarness(t, tc, refusalError("unresponsive"))
			h.claim(supervise.StateWorking, nil)
			rig := h.queue()

			rig.runTo(6)
			h.publisher.err = nil
			rig.endsAtItsNextRun()
			wantKey := record.OutboxKey(rig.id)
			if len(h.publisher.publishes) != 7 {
				t.Fatalf("publishes = %d, want 7", len(h.publisher.publishes))
			}
			for i, p := range h.publisher.publishes {
				if p.key != wantKey {
					t.Fatalf("publish %d carried key %q, want %q", i, p.key, wantKey)
				}
			}
		})
	}
}

// S15 (R1): wait_launch is stored on the row, so a daemon restarted after publishing once to a
// registered, not-ready launch does not publish a second time to that same launch.
func TestRoleWaitLaunchMarkSurvivesARestart(t *testing.T) {
	t.Parallel()
	tc := roleWaitCase{kind: record.OutboxKindMergeForward, role: claim.RoleMerger, wakesReleased: true, endsOnClaimEnd: true}
	h := newRoleWaitHarness(t, tc, refusalError("unclaimed"))
	h.claim(supervise.StateRegistered, nil)
	rig := h.queue()

	// First run publishes once to generation 1 and stores wait_launch = 1.
	rig.run()
	if got := h.publisher.count(); got != 1 {
		t.Fatalf("before restart: publishes = %d, want 1", got)
	}
	var launch int64
	if err := h.pool.QueryRow(context.Background(), `select wait_launch from outbox where id = $1`, rig.id).Scan(&launch); err != nil {
		t.Fatalf("read wait_launch: %v", err)
	}
	if launch != 1 {
		t.Fatalf("wait_launch = %d, want 1", launch)
	}

	// Restart: a fresh outbox over the same store and claim machine.
	restarted := &outbox{
		dispatchProject: "LEGION", pool: h.pool, records: h.records, supervisor: h.sup, trees: h.trees,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), notices: h.publisher,
	}
	again := newRoleWaitRig(t, restarted, h.pool, h.clock, rig.id)
	again.run()
	again.run()
	if got := h.publisher.count(); got != 1 {
		t.Fatalf("after restart on the same launch: publishes = %d, want 1 (no second publish to generation 1)", got)
	}
}
