package daemon

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// wantReadyReleasedLine is the one line a ready's release writes once it commits, naming the
// claims that said they are ready and the rows it made due (dispatch://INFRA-4131/spec#b-r-wait-ready).
const wantReadyReleasedLine = "outbox rows released: their role is ready"

// readyHooks is the workflow runtime of h's runner as the daemon builds it for the two hooks a
// waiting row is released by: the ready route's (claimReady, released by releaseReadied) and,
// where changes is set, the claims' machines' own (claimChanged, released by releaseChanged),
// which h's supervisor hands every machine it makes after this call. Neither release runs until
// the test runs it: loops runs both as the daemon does, and release runs one pass of each.
func (h *roleWaitHarness) readyHooks(changes bool) *workflowRuntime {
	h.t.Helper()
	w := &workflowRuntime{outbox: h.runner, log: quietLogger(), readied: newWakeSet[claim.Token](), changed: newWakeSet[string]()}
	if changes {
		h.sup.deps.ClaimChanged = w.claimChanged
	}
	return w
}

// loops runs w's two release loops until the test ends, each on a goroutine of its own under a
// hold mark of its own, as the workflow runs them.
func (h *roleWaitHarness) loops(w *workflowRuntime) {
	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	for _, loop := range []func(context.Context){w.releaseReadied, w.releaseChanged} {
		running.Go(func() { loop(store.ForConcurrentWorker(ctx)) })
	}
	h.t.Cleanup(func() {
		cancel()
		running.Wait()
	})
}

// release runs one pass of each of w's two releases over what its hooks recorded since the last
// pass, on the test's goroutine, as each loop's pass runs it: the rows it makes due are due once it
// returns, so none of its releases lands inside a run the test makes after it.
func (h *roleWaitHarness) release(w *workflowRuntime) {
	h.t.Helper()
	ctx := context.Background()
	if w.readied.waiting() > 0 {
		tokens, _ := w.readied.take(ctx)
		if err := w.outbox.releaseReady(ctx, tokens...); err != nil {
			h.t.Fatalf("release the rows waiting for %v: %v", tokens, err)
		}
	}
	if w.changed.waiting() > 0 {
		keys, _ := w.changed.take(ctx)
		if err := w.outbox.releaseChanged(ctx, keys); err != nil {
			h.t.Fatalf("release the rows waiting for %v: %v", keys, err)
		}
	}
}

// ready is machine's launch saying it is ready as api's ready route takes it: the machine takes
// the ready (supervise.RequestReady), and the route hands the claim it left to the workflow's hook
// (claimReady). It answers the moment of the ready.
func (h *roleWaitHarness) ready(w *workflowRuntime, machine *supervise.Machine) time.Time {
	h.t.Helper()
	c := machine.Claim()
	if err := machine.Handle(context.Background(), supervise.RequestReady{Claim: c.Token, Generation: c.Generation, Session: c.Session}); err != nil {
		h.t.Fatalf("the ready of %s's launch %d: %v", c.Token, c.Generation, err)
	}
	machine.Wait()
	w.claimReady(machine.Claim())
	return h.clock.Now()
}

// queueWaiting enqueues a second row of h.tc's kind, due at at, that waits for key as a row a run
// of its own left waiting does (record.OutboxWait.WaitFor), and answers its id.
func (h *roleWaitHarness) queueWaiting(key string, at time.Time) int64 {
	h.t.Helper()
	row := mustOutboxRow(h.t, "LEGION-208", h.payload(), at)
	row.Wait.WaitFor = key
	return queueRow(h.t, h.pool, h.records, row)
}

// rowWait is row id's wait as the store holds it: when it is due, its step, its key, and whether
// an event woke it while a lane held it.
func rowWait(t *testing.T, pool *store.Pool, id int64) (due time.Time, step int, key string, woken bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), "select next_at, wait_step, wait_for, wait_woken from outbox where id = $1", id).
		Scan(&due, &step, &key, &woken); err != nil {
		t.Fatalf("read the wait of row %d: %v", id, err)
	}
	return due, step, key, woken
}

// dueAtOnce waits for the release of the ready that came at to commit, which makes row id due at
// that moment.
func dueAtOnce(t *testing.T, pool *store.Pool, id int64, at time.Time, what string) {
	t.Helper()
	testwait.Eventually(t, what, func() bool {
		due, _, _, _ := rowWait(t, pool, id)
		return !due.After(at)
	})
}

// publishesOf is each publish under row id's key, as its moment's offset from start.
func publishesOf(publishes []roleWaitPublish, id int64, start time.Time) []time.Duration {
	var at []time.Duration
	for _, p := range publishes {
		if p.key == record.OutboxKey(id) {
			at = append(at, p.at.Sub(start))
		}
	}
	return at
}

// relaunchedReady is machine's claim relaunched (relaunchToUnregistered) and its new launch brought
// up through its registration, ready to say it is ready: the ready that moves the claim's
// LastReady.
func relaunchedReady(t *testing.T, machine *supervise.Machine, tc roleWaitCase) {
	t.Helper()
	relaunchToUnregistered(t, machine, tc)
	comesUp(t, machine, false)
}

// resumedReady is machine's suspended claim resumed by the operator, whose resume lifts any
// suspension (supervise.Claim.HeldAgainst), and its next launch brought up through its
// registration, ready to say it is ready.
func resumedReady(t *testing.T, machine *supervise.Machine) {
	t.Helper()
	suspended := machine.Claim()
	if err := machine.Handle(context.Background(), supervise.RequestResume{Claim: suspended.Token, By: supervise.SuspendedByOperator}); err != nil {
		t.Fatalf("resume %s, suspended by %s: %v", suspended.Token, suspended.SuspendedBy, err)
	}
	machine.Wait()
	if got := machine.Claim(); !supervise.Unregistered(got.State) || got.Generation != suspended.Generation+1 {
		t.Fatalf("after the resume %s is %s at generation %d, want its next launch, %d, unregistered",
			got.Token, got.State, got.Generation, suspended.Generation+1)
	}
	comesUp(t, machine, false)
}

// S2 (R1): a row of each waiting kind to a role whose claim the daemon does not record publishes
// nothing; once the claim is made and its first launch says it is ready, the ready route's release
// makes the row due at once, and the first run after the ready publishes it.
func TestRoleWaitNoClaimIsPublishedAtTheFirstReady(t *testing.T) {
	t.Parallel()
	for _, tc := range roleWaitCases {
		if tc.endsNoClaim {
			continue
		}
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			h := newRoleWaitHarness(t, tc, nil)
			rig := h.queue()
			w := h.readyHooks(false)
			h.loops(w)

			rig.runTo(5)
			if got := h.publisher.count(); got != 0 {
				t.Fatalf("%s with no claim published %d times across 5 runs, want 0", tc.kind, got)
			}
			machine := h.claim(supervise.StateLaunching, func(c *supervise.Claim) {
				c.Session, c.SessionFile = "ses-"+string(tc.role), t.TempDir()+"/session.jsonl"
			})
			comesUp(t, machine, false)
			readyAt := h.ready(w, machine)
			dueAtOnce(t, h.pool, rig.id, readyAt, "the row due at its claim's first ready")

			rig.endsAtItsNextRun()
			if got, want := publishOffsets(h.publisher.publishes, h.start), []time.Duration{readyAt.Sub(h.start)}; !slices.Equal(got, want) {
				t.Fatalf("%s published at %v, want once, at the ready (%v)", tc.kind, got, want)
			}
		})
	}
}

// S5 (R1): a row of each kind to a claim suspended by the operator, the controller, the daemon, the
// agent's exit or an unknown source publishes at most once while the claim stays suspended; the
// claim's next launch's ready makes it due at once, and the first run after that ready publishes
// it. An update_ask to a claim the daemon passes over (supervise.Claim.PassedOverByRound: every
// source but the operator and the controller) ends at its first run without a publish, as it ends
// today.
func TestRoleWaitSuspendedPublishesAtMostOnceUntilTheNextReady(t *testing.T) {
	t.Parallel()
	sources := []supervise.SuspendSource{
		supervise.SuspendedByOperator, supervise.SuspendedByController, supervise.SuspendedByDaemon,
		supervise.SuspendedByExit, supervise.SuspendedByUnknown,
	}
	for _, tc := range roleWaitCases {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			for _, by := range sources {
				t.Run(string(by), func(t *testing.T) {
					t.Parallel()
					h := newRoleWaitHarness(t, tc, refusalError("unresponsive"))
					rig := h.queue()
					w := h.readyHooks(false)
					h.loops(w)
					machine := h.claim(supervise.StateSuspended, func(c *supervise.Claim) { c.SuspendedBy = by })

					if tc.kind == record.OutboxKindUpdateAsk && machine.Claim().PassedOverByRound() {
						rig.endsAtItsNextRun()
						if got := h.publisher.count(); got != 0 {
							t.Fatalf("update_ask to a claim suspended by %s published %d times, want 0", by, got)
						}
						return
					}
					rig.runTo(5)
					suspended := h.publisher.count()
					if suspended > 1 {
						t.Fatalf("%s to a claim suspended by %s published %d times across 5 runs, want at most 1", tc.kind, by, suspended)
					}

					resumedReady(t, machine)
					readyAt := h.ready(w, machine)
					dueAtOnce(t, h.pool, rig.id, readyAt, "the row due at the claim's next ready")
					rig.run()
					got := publishOffsets(h.publisher.publishes, h.start)
					if len(got) != suspended+1 || got[len(got)-1] != readyAt.Sub(h.start) {
						t.Fatalf("%s to a claim suspended by %s published at %v, want one publish more at the next ready (%s)",
							tc.kind, by, got, readyAt.Sub(h.start))
					}
				})
			}
		})
	}
}

// S7 (R1): a claim's ready makes due at once, in one release, every row that waits for that claim:
// a row on the capped waits of a refusal while the claim said it was ready (record.WaitForReady)
// and a row the gate holds for any change of the claim (record.WaitForClaim). Each starts its
// backoff again (wait_step 0), the release writes one line naming the claim and both rows, and the
// first run after the ready publishes. The claim's machines tell the outbox nothing here, so the
// ready route's release alone makes the rows due.
func TestRoleWaitReadyMakesEveryWaitingRowDue(t *testing.T) {
	t.Parallel()
	for _, tc := range roleWaitCases {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			h := newRoleWaitHarness(t, tc, refusalError("unresponsive"))
			rig := h.queue()
			held := h.queueWaiting(record.WaitForClaim(h.token), h.start.Add(time.Hour))
			w := h.readyHooks(false)
			h.loops(w)
			machine := h.claim(supervise.StateIdle, nil)

			rig.runTo(6)
			if _, step, key, _ := rowWait(t, h.pool, rig.id); step != 6 || key != record.WaitForReady(h.token) {
				t.Fatalf("after 6 refusals the row waits at step %d for %q, want step 6 for %q", step, key, record.WaitForReady(h.token))
			}
			relaunchedReady(t, machine, tc)
			readyAt := h.ready(w, machine)
			dueAtOnce(t, h.pool, rig.id, readyAt, "the refused row due at the ready")
			dueAtOnce(t, h.pool, held, readyAt, "the held row due at the ready")
			for _, id := range []int64{rig.id, held} {
				if _, step, _, _ := rowWait(t, h.pool, id); step != 0 {
					t.Fatalf("row %d waits at step %d after the ready, want 0", id, step)
				}
			}
			lines := loggedMessages(t, rig.logs.String(), wantReadyReleasedLine)
			if len(lines) != 1 || !slices.Contains(lines[0]["claims"].([]any), any(string(h.token))) ||
				!slices.Equal(lines[0]["rows"].([]any), []any{float64(rig.id), float64(held)}) {
				t.Fatalf("release lines %v, want one naming claim %s and rows %d and %d", lines, h.token, rig.id, held)
			}

			before := len(publishesOf(h.publisher.publishes, rig.id, h.start))
			rig.run()
			if got := publishesOf(h.publisher.publishes, rig.id, h.start); len(got) != before+1 || got[before] != readyAt.Sub(h.start) {
				t.Fatalf("the refused row published at %v, want one publish more at the ready (%s)", got, readyAt.Sub(h.start))
			}
		})
	}
}

// S7 (R1): a catch-up the wake pacing holds to a moment still ahead of the ready keeps that moment
// (record.Notice.PacedAt), even where it waits for the claim, while a notice beside it that waits
// for the same claim is due at once; the catch-up then goes at its moment.
func TestRoleWaitReadyLeavesAPacedCatchUpAtItsMoment(t *testing.T) {
	t.Parallel()
	h := newRoleWaitHarness(t, roleWaitCases[0], nil)
	paced := h.start.Add(5 * time.Minute)
	catchUp := pacedCatchUp(t, h.pool, h.records, "LEGION-208", 1, h.start, paced)
	if _, err := h.pool.Exec(context.Background(), "update outbox set wait_for = $2 where id = $1", catchUp, record.WaitForClaim(h.token)); err != nil {
		t.Fatalf("make the catch-up wait for its claim: %v", err)
	}
	rig := newRoleWaitRig(t, h.runner, h.pool, h.clock, catchUp)
	beside := h.queueWaiting(record.WaitForClaim(h.token), h.start.Add(time.Hour))
	w := h.readyHooks(false)
	h.loops(w)
	machine := h.claim(supervise.StateRegistered, nil)

	readyAt := h.ready(w, machine)
	dueAtOnce(t, h.pool, beside, readyAt, "the notice beside the catch-up due at the ready")
	if due, _, _, _ := rowWait(t, h.pool, catchUp); !due.Equal(paced) {
		t.Fatalf("the paced catch-up is due at %s after the ready, want its moment %s", due, paced)
	}
	rig.run()
	if got := publishesOf(h.publisher.publishes, catchUp, h.start); !slices.Equal(got, []time.Duration{paced.Sub(h.start)}) {
		t.Fatalf("the paced catch-up published at %v, want once, at its moment %s", got, paced.Sub(h.start))
	}
}

// roleWaitNoHolderCases is every kind that keeps its row when no session holds its role
// (notify.ErrNoHolder): all of the nine but a role_message no pause holds and an update_ask, which
// end there.
func roleWaitNoHolderCases() []roleWaitCase {
	return slices.DeleteFunc(slices.Clone(roleWaitCases), func(tc roleWaitCase) bool {
		return tc.kind == record.OutboxKindRoleMessage || tc.kind == record.OutboxKindUpdateAsk
	})
}

// firstTurnRig is a row of tc's kind whose first publish, at its claim's ready, no session took:
// the launched session has not taken its role's name yet (roleNameBound). The claims' machines
// tell the outbox each change, and each release runs on the test's goroutine.
func firstTurnRig(t *testing.T, tc roleWaitCase) (*roleWaitHarness, *roleWaitRig, *workflowRuntime, *supervise.Machine) {
	t.Helper()
	h := newRoleWaitHarness(t, tc, refusalError("unclaimed"))
	rig := h.queue()
	w := h.readyHooks(true)
	machine := h.claim(supervise.StateRegistered, nil)
	readyAt := h.ready(w, machine)
	h.release(w)
	if !readyAt.Equal(h.start) {
		t.Fatalf("the ready came at %s, want the row's own moment %s", readyAt, h.start)
	}
	rig.run()
	due, step, key, _ := rowWait(t, h.pool, rig.id)
	if want := h.start.Add(roleNameBound); !due.Equal(want) || step != 0 || key != record.WaitForClaim(h.token) {
		t.Fatalf("after the refusal at the ready the row is due at %s, at step %d, waiting for %q; want %s, step 0, %q",
			due, step, key, want, record.WaitForClaim(h.token))
	}
	return h, rig, w, machine
}

// S8 (R1, P8): a row refused because no session holds its role, at its claim's ready with no turn
// since, waits for the role's name: its claim's first turn, which starts 5 s after the ready here,
// makes it due at once, its step not raised in the wait, and from the run at the turn on it waits
// on the backoff from its first step.
func TestRoleWaitFirstTurnRunsTheRowAtTheTurnStart(t *testing.T) {
	t.Parallel()
	for _, tc := range roleWaitNoHolderCases() {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			h, rig, w, machine := firstTurnRig(t, tc)

			h.clock.advance(5 * time.Second)
			if err := machine.Handle(context.Background(), supervise.StreamTurnStart{Claim: h.token}); err != nil {
				t.Fatalf("the claim's first turn: %v", err)
			}
			machine.Wait()
			h.release(w)
			if due, step, _, _ := rowWait(t, h.pool, rig.id); !due.Equal(h.clock.Now()) || step != 0 {
				t.Fatalf("after the turn start the row is due at %s at step %d, want at the turn (%s) at step 0", due, step, h.clock.Now())
			}
			rig.run()
			if got, want := publishOffsets(h.publisher.publishes, h.start), []time.Duration{0, 5 * time.Second}; !slices.Equal(got, want) {
				t.Fatalf("published at %v, want %v: at the ready and at the turn's start", got, want)
			}
			if due, step, _, _ := rowWait(t, h.pool, rig.id); !due.Equal(h.clock.Now().Add(record.RoleWaitBackoff(0))) || step != 1 {
				t.Fatalf("after the run at the turn the row is due at %s at step %d, want the first step's wait (%s) at step 1",
					due.Sub(h.start), step, (5*time.Second + record.RoleWaitBackoff(0)))
			}
		})
	}
}

// S8 (R1, P8): a row refused because no session holds its role, at its claim's ready with no turn
// since, runs again at the end of the pane's first-turn bound (roleNameBound, 30 s) when no turn
// comes, its step not raised in the wait; refused then, it is on the backoff from its first step,
// at 31 s and 33 s, and never loops at the bound.
func TestRoleWaitFirstTurnBoundRunsTheRowAt30Seconds(t *testing.T) {
	t.Parallel()
	for _, tc := range roleWaitNoHolderCases() {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			h, rig, _, _ := firstTurnRig(t, tc)

			rig.run()
			rig.run()
			rig.run()
			want := []time.Duration{0, 30 * time.Second, 31 * time.Second, 33 * time.Second}
			if got := publishOffsets(h.publisher.publishes, h.start); !slices.Equal(got, want) {
				t.Fatalf("published at %v, want %v", got, want)
			}
		})
	}
}

// S10 (R1, P7): a row on the capped waits of a refusal while its claim said it was ready waits for
// the claim's next ready alone (record.WaitForReady): a turn start of the ready claim makes it no
// sooner due, and the ready of the claim's next launch, which moves its LastReady, makes it due at
// once with its step back at 0, so its first refusal after that ready waits the backoff's first
// step. A ready whose release a restart lost restarts the step all the same, at the row's next run:
// the run reads a ready other than the one its step counts from.
func TestRoleWaitReadyRestartsTheBackoff(t *testing.T) {
	t.Parallel()
	tc := roleWaitCase{kind: record.OutboxKindMergeForward, role: claim.RoleMerger, wakesReleased: true, endsOnClaimEnd: true}
	// refusedReady is a merge forward to an idle merger, ready since before the row was written,
	// whose every publish is refused, run until its next run is 64 s ahead.
	refusedReady := func(t *testing.T) (*roleWaitHarness, *roleWaitRig, *workflowRuntime, *supervise.Machine) {
		t.Helper()
		h := newRoleWaitHarness(t, tc, refusalError("unresponsive"))
		rig := h.queue()
		w := h.readyHooks(true)
		machine := h.claim(supervise.StateIdle, nil)
		h.release(w)
		rig.runTo(6)
		return h, rig, w, machine
	}

	t.Run("released at the ready", func(t *testing.T) {
		t.Parallel()
		h, rig, w, machine := refusedReady(t)
		waiting, _, _, _ := rowWait(t, h.pool, rig.id)
		// A row the gate holds for any change of the merger's claim, beside it.
		held := h.queueWaiting(record.WaitForClaim(h.token), h.start.Add(time.Hour))

		h.clock.advance(time.Second)
		for _, ev := range []supervise.Event{supervise.StreamTurnStart{Claim: h.token}, supervise.StreamTurnEnd{Claim: h.token}} {
			if err := machine.Handle(context.Background(), ev); err != nil {
				t.Fatalf("handle %T: %v", ev, err)
			}
		}
		machine.Wait()
		h.release(w)
		if due, _, _, _ := rowWait(t, h.pool, held); !due.Equal(h.clock.Now()) {
			t.Fatalf("the row waiting for any change of the claim is due at %s after the turn, want at once (%s)", due, h.clock.Now())
		}
		if due, _, _, _ := rowWait(t, h.pool, rig.id); !due.Equal(waiting) {
			t.Fatalf("the refused row is due at %s after the turn, want still at its wait (%s)", due, waiting)
		}
		if _, err := h.pool.Exec(context.Background(), "delete from outbox where id = $1", held); err != nil {
			t.Fatalf("drop the held row: %v", err)
		}
		relaunchedReady(t, machine, tc)
		readyAt := h.ready(w, machine)
		h.release(w)
		if due, step, _, _ := rowWait(t, h.pool, rig.id); !due.Equal(readyAt) || step != 0 {
			t.Fatalf("after the next launch's ready the row is due at %s at step %d, want at the ready (%s) at step 0", due, step, readyAt)
		}
		rig.run()
		if due, _, _, _ := rowWait(t, h.pool, rig.id); !due.Equal(readyAt.Add(record.RoleWaitBackoff(0))) {
			t.Fatalf("after its refusal at the ready the row is due at %s, want the first step's wait after the ready (%s)",
				due, readyAt.Add(record.RoleWaitBackoff(0)))
		}
	})

	t.Run("a ready the release lost", func(t *testing.T) {
		t.Parallel()
		h, rig, w, machine := refusedReady(t)
		waiting, _, _, _ := rowWait(t, h.pool, rig.id)

		relaunchedReady(t, machine, tc)
		h.ready(w, machine)
		// The restart takes what the hooks recorded with it: no release runs.
		w.readied.take(context.Background())
		w.changed.take(context.Background())
		if due, _, _, _ := rowWait(t, h.pool, rig.id); !due.Equal(waiting) {
			t.Fatalf("the refused row is due at %s with the ready's release lost, want still at its wait (%s)", due, waiting)
		}
		rig.run()
		if due, _, _, _ := rowWait(t, h.pool, rig.id); !due.Equal(waiting.Add(record.RoleWaitBackoff(0))) {
			t.Fatalf("after its first run since the lost ready the row is due at %s, want the first step's wait after that run (%s)",
				due, waiting.Add(record.RoleWaitBackoff(0)))
		}
	})
}

// S11 (R1): a ready that lands while a lane holds the row, its publish in flight, marks the row
// woken, so the run's retry makes it due at the moment of that retry with its step back at 0
// rather than at the backoff the run asked for; the run after it publishes to the ready claim.
// An update_ask is published under its claim's own lock (supervise.Machine.PublishUpdateAsk), so
// no ready can land while its publish is in flight, and it has no such case.
func TestRoleWaitReadyDuringARunMakesTheRowDueAtOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range roleWaitCases {
		if tc.kind == record.OutboxKindUpdateAsk {
			continue
		}
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			h := newRoleWaitHarness(t, tc, refusalError("unresponsive"))
			rig := h.queue()
			w := h.readyHooks(false)
			h.loops(w)
			machine := h.claim(supervise.StateRegistered, nil)
			h.publisher.during = func() {
				h.publisher.during = nil
				h.ready(w, machine)
				testwait.Eventually(t, "the ready's release to wake the row its lane holds", func() bool {
					_, _, _, woken := rowWait(t, h.pool, rig.id)
					return woken
				})
			}

			rig.run()
			if due, step, _, _ := rowWait(t, h.pool, rig.id); !due.Equal(h.clock.Now()) || step != 0 {
				t.Fatalf("after the run the ready landed in, the row is due at %s at step %d, want at once (%s) at step 0", due, step, h.clock.Now())
			}
			rig.run()
			if got, want := publishOffsets(h.publisher.publishes, h.start), []time.Duration{0, 0}; !slices.Equal(got, want) {
				t.Fatalf("published at %v, want %v: the run the ready landed in and the one right after it", got, want)
			}
		})
	}
}
