package delivery

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// A pull request linked by hand, whose body is then edited, is compared at its head before any
// result names it. In production on 20 November, cybermonklabs/legion#4612, linked by hand to its
// issue, had a body edit and then a reconcile pass, and no head was compared for 14 minutes,
// until a `dispatch result` at its head read it through (ReadThroughCommitPulls): a head read only
// when a result names it. That deployment carries no release of this change, and its intake binds
// no durable without a delivery_settings row. Here, with no delivery settings, the same order runs:
// the link by hand (OweLinkedPullRequestReads), the `edited` envelope through the intake's own
// delivery (Intake.deliver and its route, with the settings production holds: none; the intake
// reads the pull request from GitHub and writes its head, R1 and R2), one reconcile pass, and then
// the results' read of the issue's heads (HeadsAttributedTo), with no result recorded and no
// read-through. The issue's head is #4612 at GitHub's head, read. The second case is the same
// order with the edit's event lost: the pass reads the read the link owed (R5) and writes the
// head. The control is a pull request whose `opened` event arrived, which reads as before. Each
// case runs with GitHub serving the pull request, and with GitHub's reads counted, so a head that
// came from a read-through would show as a read made after the pass.
func TestALinkedPullRequestWhoseBodyIsEditedIsComparedBeforeAnyResult(t *testing.T) {
	for _, c := range []struct {
		name   string
		linked bool
		action string // "" sends no event
	}{
		{name: "linked_then_edited_then_a_pass", linked: true, action: "edited"},
		{name: "linked_and_the_edit_lost_then_a_pass", linked: true},
		{name: "control_opened_then_a_pass", action: "opened"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			const key, number = "ATTR-920", 4612
			url := fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number)
			links := map[string][]string{}
			if c.linked {
				links[key] = []string{url}
			}
			seedAttributionIssues(t, ctx, pool, []string{key}, links)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			head := fakeSHA("46", number)
			body := "A change"
			if !c.linked {
				body = "Dispatch: " + key
			}
			github.set("acme/widgets", number, "open", head, "topic", body, at)
			client := fake.newTestClient()
			if c.linked {
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if err := unrenamed().OweLinkedPullRequestReads(ctx, tx, []string{url}); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if c.action != "" {
				intake := newIntake(tuningFor(t), nil, pool, nil, client, nil, newSettingsCache(pool, nil), unrenamed())
				payload, err := json.Marshal(prEnvelope("acme/widgets", number, c.action, head, "topic", body, at))
				if err != nil {
					t.Fatal(err)
				}
				intake.deliver(deliveryWithNoNATS(ctx), intakeMessage{
					msg:      &natsgo.Msg{Subject: "notifications.github.acme.widgets.pr." + strconv.Itoa(number)},
					envelope: &contracts.Envelope{Payload: string(payload)},
				})
			}
			NewReconcile(pool, client, events.NewBroker(), unrenamed()).runOnce(ctx)
			readsAfterPass := github.reads.Load()

			heads, err := unrenamed().HeadsAttributedTo(ctx, pool, key, HeadAnswers{})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("heads of %s after the pass, before any result: %+v; GitHub pull reads by then %d, after the read %d",
				key, heads, readsAfterPass, github.reads.Load())
			if len(heads) != 1 || heads[0].Number != number || !heads[0].HeadKnown || heads[0].HeadSHA != head {
				t.Fatalf("after the pass and before any result, %s's heads are %+v; want #%d known at GitHub's head %s", key, heads, number, head)
			}
			if reads := github.reads.Load(); reads != readsAfterPass {
				t.Errorf("the results' read of the heads read GitHub %d times, want none: the head was stored before any result", reads-readsAfterPass)
			}
		})
	}
}
