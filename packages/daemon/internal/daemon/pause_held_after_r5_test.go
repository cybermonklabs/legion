package daemon

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A role message or a hook prompt that R5 held for its claim's session before a pause carries
// both marks after the resume: the hold's (outbox.held_at, which no later try clears) and the
// pause's (outbox.held_by_pause). Main's rule decides such a row (R4 of the project pause):
// a row a pause owes the role it stopped is ended neither for that claim's release nor for having
// no live holder, and reaches the role once it is back (heldMessageOwed). So the hold's own end —
// the claim no longer keeping its session, which the pause's stop makes it — does not finish the
// row (heldDelivery). The row survives its first attempt after the resume, the role launches once
// under the pause's wake, and the row reaches the role's session once.
//
// The row is held by R5 as a try before the pause would have left it: the test writes its hold
// mark, as retryAt does for a row heldForItsSession holds.
//
// The negative control is heldDelivery asking holdEnded first for such a row, as it did: the row
// is finished undelivered at its first attempt after the resume, "its claim ended", and each case
// fails where it reads the row after that attempt.
func TestARowR5HeldBeforeThePauseIsStillOwedToTheRoleThePauseStopped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload func(worker claim.Token) record.OutboxPayload
		message func(issue string, payload record.OutboxPayload) string
		// listenerWakes is whether the role comes back on the listener's report of the row's
		// publish (a role message's), rather than the daemon's own wake (a hook prompt's).
		listenerWakes bool
	}{
		{
			name: "role message",
			payload: func(worker claim.Token) record.OutboxPayload {
				return record.RoleMessage{Role: string(worker), Body: "The checks of your head settled."}
			},
			message:       func(_ string, p record.OutboxPayload) string { return p.(record.RoleMessage).Body },
			listenerWakes: true,
		},
		{
			name: "hook prompt",
			payload: func(claim.Token) record.OutboxPayload {
				return record.HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Reviewing,
					To: phase.Implementing, Count: 1, Counts: map[string]int{phase.Pair(phase.Reviewing, phase.Implementing): 1},
					Prompt: "Read the review."}
			},
			message: func(issue string, p record.OutboxPayload) string { return p.(record.HookPrompt).Message(issue) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPauseDaemonRig(t, nil)
			issue := r.issue(1)
			worker := r.token(issue, claim.RoleImplementer)
			r.seed(r.root(issue, phase.Implementing), r.running(issue, claim.RoleImplementer, supervise.StateIdle))
			r.envoy.gone(worker)
			r.boot()

			pause := r.pauseProject()
			r.awaitState(worker, supervise.StateSuspended, "the pause stops the idle implementer")
			payload := tc.payload(worker)
			row := r.enqueue(issue, payload)
			if _, err := r.pool.Exec(r.ctx, `update outbox set held_at = $2 where id = $1`, row, r.clock.Now().UTC()); err != nil {
				t.Fatalf("mark the %s held for its claim's session: %v", tc.name, err)
			}
			time.Sleep(time.Second)
			if sent := r.envoy.publishedTo(worker); len(sent) != 0 {
				t.Fatalf("the %s was published %d times while the project was paused, want it held", tc.name, len(sent))
			}

			attempts := heldRowAttempts(t, r, row)
			before := len(r.rt.launches())
			r.resumeProject()
			testwait.Eventually(t, "the held row's first attempt after the resume", func() bool { return attempts() != 0 })
			if attempts() == -1 {
				t.Fatalf("the %s that R5 held before the pause ended at its first attempt after the resume, with %d publishes taken for the role; want it still owed to the role the pause stopped",
					tc.name, len(r.envoy.publishedTo(worker)))
			}
			if held := r.heldByPause(row); held != pause.ID {
				t.Fatalf("the %s's row is marked with pause %d after the resume, want pause %d", tc.name, held, pause.ID)
			}
			if tc.listenerWakes {
				r.applyFact("listener", "r5-held-role-message-gone", intake.ListenerWake{Kind: intake.ListenerWakeRole,
					Role: string(worker), At: r.clock.Now(), Report: "report-of-the-r5-held-role-message"})
			}
			r.runPacing(before, 1, func(launch waveLaunch) {
				if launch.claim == worker {
					r.ready(worker)
					r.envoy.hold(worker)
				}
			}, func(api.State) {})
			testwait.WithinPolling(t, 6*time.Minute, 200*time.Millisecond, "the held row to reach the role after its launch", func() bool {
				return len(r.envoy.publishedTo(worker)) > 0 && attempts() == -1
			})
			time.Sleep(time.Second)

			want := tc.message(issue, payload)
			if sent := r.envoy.publishedTo(worker); len(sent) != 1 || sent[0].message != want {
				t.Fatalf("the %s reached the role %d times (%+v), want once with its words", tc.name, len(sent), sent)
			}
			var launchedAfter int
			for _, launch := range r.rt.launches()[before:] {
				if launch.claim == worker {
					launchedAfter++
				}
			}
			if launchedAfter != 1 {
				t.Fatalf("the implementer launched %d times after the resume, want once", launchedAfter)
			}
		})
	}
}
