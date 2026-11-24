// results_outside_text_test.go holds Dispatch to its rule for text outside the commit that a check
// reads, such as a pull request's body: a pass counts at the commit it read, and Dispatch tracks no
// such text, so an edit of the body changes no check state. It cannot show that the architect reads
// that text at the acceptance, which is what guards it. The edit reaches Dispatch as production
// delivers it: GitHub's pull_request event, normalized as Envoy's webhook normalizes it
// (contracts.GithubEnvelopes) and taken by the delivery intakes (delivery.StartIntakes) on a NATS
// server of the test's own. The rig and its readers are check_states_test.go's, and the intakes'
// start check_states_ci_test.go's.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// outsideTextFixture is GitHub's pull_request event as the webhook's fixtures hold it
// (packages/envoy/scripts), whose repository is the one the rig's project maps.
const outsideTextFixture = "../../../scripts/fixtures/github/pull-request-opened.json"

// The body #70 is opened with, the body its edit gives it, and GitHub's updated_at of each of the
// two events. Neither body names an issue or cites one, so the edit leaves the attribution as the
// title made it.
const (
	outsideTextBody       = "The first description of the change."
	outsideTextEditedBody = "The description of the change, rewritten after the tester's pass."
	outsideTextOpenedAt   = "2026-11-20T10:00:00Z"
	outsideTextEditedAt   = "2026-11-20T10:05:00Z"
)

// TestAnEditOfThePullRequestBodyChangesNoState: GitHub opens #70 of ACME-1 at H1, and the tester's
// pass of k1 at H1 publishes one event, k1 at H1 yes, which shows that the test sees the events a
// change of state publishes. Then GitHub delivers an edited event of #70 that changes its body and
// pushes nothing, and the intake takes it: #70's row carries the edit's time and is still open at
// H1 for ACME-1. Dispatch publishes no check.state_changed event after it, and k1 at H1 still reads
// yes, as published, as Dispatch computes it afresh, and as the results read answers it.
func TestAnEditOfThePullRequestBodyChangesNoState(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	url, publisher := deliveryBus(t)
	startDeliveryIntakes(t, rig, url, publisher)

	outsideTextDeliver(t, publisher, outsideTextEvent(t, rig, "opened", outsideTextBody, outsideTextOpenedAt))
	outsideTextWantHead(t, rig, "#70 opened", outsideTextOpenedAt)
	wantEventSet(t, "#70 opened at H1", settleOwed(t, rig),
		stateWant{"k1", eachHeadH1, "no", "no_result"}, stateWant{"k2", eachHeadH1, "no", "no_result"})

	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	wantEvents(t, "the tester's pass of k1 at H1", settleOwed(t, rig), stateWant{"k1", eachHeadH1, "yes", ""})

	edited := outsideTextEvent(t, rig, "edited", outsideTextEditedBody, outsideTextEditedAt)
	edited["changes"] = map[string]any{"body": map[string]any{"from": outsideTextBody}}
	after := lastEventID(t, rig.database)
	outsideTextDeliver(t, publisher, edited)
	outsideTextWantHead(t, rig, "#70's body edited", outsideTextEditedAt)
	settleOwed(t, rig)
	if events := checkStateEventsAfter(t, rig.database, after); len(events) != 0 {
		t.Fatalf("an edit of #70's body with no push published %v, want no check.state_changed event", eventLines(events))
	}
	wantPublishedIsComputed(t, "k1 at H1 after the edit of #70's body", rig, "k1", eachHeadH1)
	if row, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, eachHeadH1, "k1"); !held || row.state == nil || *row.state != "yes" {
		t.Fatalf("after the edit of #70's body the published state of k1 at H1 is %+v (held %v), want yes", row, held)
	}
	eachHeadWants(t, "k1 at H1 after the edit of #70's body",
		eachHeadAt(t, eachHeadRead(t, rig.handler, rig.issue, "k1", ""), eachHeadH1), "yes", "")
}

// outsideTextEvent is GitHub's pull_request event of action for #70 of the rig's repository, built
// from the webhook's fixture: open at H1 on main at ciJobBaseSHA, from a branch of the repository
// itself, titled after the rig's issue so that the intake attributes it there, with body and
// GitHub's updated_at.
func outsideTextEvent(t *testing.T, rig *checkStatesRig, action, body, updatedAt string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(outsideTextFixture)
	if err != nil {
		t.Fatalf("read the pull_request fixture: %v", err)
	}
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("decode the pull_request fixture: %v", err)
	}
	owner, name, _ := strings.Cut(ciJobRepository, "/")
	event["action"] = action
	event["number"] = 70
	event["repository"] = map[string]any{"name": name, "full_name": ciJobRepository, "owner": map[string]any{"login": owner}}
	pull, ok := event["pull_request"].(map[string]any)
	if !ok {
		t.Fatalf("the pull_request fixture holds no pull_request object: %s", raw)
	}
	pull["number"] = 70
	pull["title"] = rig.issue + " records the results of a check"
	pull["body"] = body
	pull["state"] = "open"
	pull["html_url"] = fmt.Sprintf("https://github.com/%s/pull/70", ciJobRepository)
	pull["head"] = map[string]any{"sha": eachHeadH1, "ref": "legion/work", "repo": map[string]any{"full_name": ciJobRepository}}
	pull["base"] = map[string]any{"ref": "main", "sha": ciJobBaseSHA}
	pull["updated_at"] = updatedAt
	return event
}

// outsideTextDeliver normalizes event as Envoy's webhook normalizes GitHub's pull_request event
// (contracts.GithubEnvelopes, Envelope.Validate), publishes the one envelope that makes, and waits
// until the delivery durable has acknowledged every message the stream holds: the intake writes
// before it acknowledges, so the event's write is in place by then. Each call takes a delivery id
// of its own, since the bus stores a GitHub envelope once per delivery.
func outsideTextDeliver(t *testing.T, publisher *bus.Client, event map[string]any) {
	t.Helper()
	delivery := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
	items := contracts.GithubEnvelopes(contracts.GithubEnvelopeInput{
		Event: "pull_request", Delivery: delivery, Body: event, EventID: "event-" + delivery, TraceID: "trace-" + delivery,
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("GitHub's %v pull_request event makes %d envelopes, want 1", event["action"], len(items))
	}
	if err := items[0].Validate(); err != nil {
		t.Fatalf("the envelope of GitHub's %v pull_request event: %v", event["action"], err)
	}
	if err := publisher.Publish(items[0]); err != nil {
		t.Fatalf("publish %s: %v", items[0].Topic, err)
	}
	stream, err := publisher.JS().StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	awaitDelivery(t, publisher, fmt.Sprintf("the acknowledgement of GitHub's %v event of #70", event["action"]), func(info *natsgo.ConsumerInfo) bool {
		return info.AckFloor.Stream >= stream.State.LastSeq && info.NumAckPending == 0
	})
}

// outsideTextWantHead fails the test unless #70's row of the rig's repository is open at H1,
// attributed to the rig's issue, and carries updatedAt, GitHub's time of the newest event the
// intake wrote: that event's write is in place.
func outsideTextWantHead(t *testing.T, rig *checkStatesRig, what, updatedAt string) {
	t.Helper()
	want, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		t.Fatalf("parse %q: %v", updatedAt, err)
	}
	var head, state, issue string
	var eventAt *time.Time
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select head_sha, state, coalesce(issue_key, ''), event_at from pull_request_heads where repo = $1 and number = 70`,
		ciJobRepository).Scan(&head, &state, &issue, &eventAt); err != nil {
		t.Fatalf("%s: read #70's row: %v", what, err)
	}
	if head != eachHeadH1 || state != "open" || issue != rig.issue || eventAt == nil || !eventAt.Equal(want) {
		t.Fatalf("%s: #70's row is %s at %s for %q with the event time %v, want open at %s for %s with %s",
			what, state, head, issue, eventAt, eachHeadH1, rig.issue, want)
	}
}
