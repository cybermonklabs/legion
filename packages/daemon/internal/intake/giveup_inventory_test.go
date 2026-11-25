package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// inventoryEnvelope is one message of the inventory: the consumer that hears its subject, the
// subject, the envelope, and the fact type it decodes to, "" for a message that does not decode.
type inventoryEnvelope struct {
	consumer string
	subject  string
	data     []byte
	fact     string
}

// dispatchEventEnvelope is the Envoy envelope of Dispatch's event eventType on CAPTURE-3, with
// payload as the event's own.
func dispatchEventEnvelope(t *testing.T, id int, eventType string, payload map[string]any) []byte {
	t.Helper()
	event, err := json.Marshal(map[string]any{"id": id, "issue_key": "CAPTURE-3", "project": "CAPTURE", "seq": id, "type": eventType,
		"actor": map[string]string{"kind": "user", "id": "someone"}, "notify": true, "created_at": "2026-11-20T00:00:00Z", "payload": payload})
	if err != nil {
		t.Fatalf("marshal the Dispatch event: %v", err)
	}
	return envelopeJSON(t, fmt.Sprintf("dispatch-inventory-%d", id), "dispatch", string(event))
}

// githubEventEnvelope is the Envoy envelope of a GitHub event of sjawhar/legion as the listener
// normalizes it, with fields as its payload.
func githubEventEnvelope(t *testing.T, id string, fields map[string]any) []byte {
	t.Helper()
	fields["repo"] = "sjawhar/legion"
	payload, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal the GitHub payload: %v", err)
	}
	return envelopeJSON(t, "github-inventory-"+id, "github", string(payload))
}

// No fact type keeps redelivery without a bound: one message of each of the 18 fact types
// decodeMessage returns, on the Dispatch, GitHub and wake subjects, and one message that does not
// decode, each failing where its handling can fail, is terminated at the bound once a given-up
// record of it has committed. Each fails in the handler; a review and a checks settlement name a
// pull request the daemon records, since one of any other is acknowledged with no transaction.
// The count at the end holds the table to the 18 distinct fact types decode.go returns today; it
// counts the table, not decode.go, so a fact type decode.go returns later is added to the table,
// and to the count, by hand.
func TestEveryFactTypeTheDecoderReturnsIsGivenUpAtTheBound(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	recordPullRequest(t, pool, "LEGION-208", 42)
	dispatch, github, wake := dispatchConsumerName("CAPTURE"), githubConsumerName("CAPTURE"), wakeConsumerName("CAPTURE")
	const issue = "notifications.dispatch.issue.CAPTURE-3."
	const repo = "notifications.github.sjawhar.legion."
	pr := func(action string, extra map[string]any) map[string]any {
		fields := map[string]any{"kind": "pr", "action": action, "number": "42", "head_ref": "legion/LEGION-208", "head_sha": "head-2", "base_ref": "main"}
		for key, value := range extra {
			fields[key] = value
		}
		return fields
	}
	wakeReport, err := json.Marshal(map[string]any{"kind": ListenerWakeReady, "at": time.Now().UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	envelopes := []inventoryEnvelope{
		{dispatch, issue + "issue.updated", dispatchEventEnvelope(t, 1, "issue.updated", map[string]any{
			"key": "CAPTURE-3", "status": "todo", "title": "widgets", "rank": "A", "labels": []string{}, "resolved_repo": "sjawhar/legion",
			"blocked_by": []string{}, "claim": nil}), "intake.DispatchIssue"},
		{dispatch, issue + "artifact.approved", dispatchEventEnvelope(t, 2, "artifact.approved", map[string]any{"artifact_id": "plan-md", "version": 2}),
			"intake.DispatchArtifact"},
		{dispatch, issue + "ask.opened", dispatchEventEnvelope(t, 3, "ask.opened", map[string]any{"id": "ask-1", "kind": "question", "restored": true}),
			"intake.DispatchAskRestored"},
		{dispatch, issue + "ask.opened", dispatchEventEnvelope(t, 4, "ask.opened", map[string]any{"id": "ask-2", "kind": "question",
			"author": map[string]string{"kind": "user", "id": "someone"}}), "intake.DispatchAskOpened"},
		{dispatch, issue + "ask.answered", dispatchEventEnvelope(t, 5, "ask.answered", map[string]any{"id": "ask-3", "kind": "question"}),
			"intake.DispatchAskClosed"},
		{dispatch, issue + "comment.created", dispatchEventEnvelope(t, 6, "comment.created", map[string]any{"id": "comment-1", "ask_id": "ask-4",
			"ask_state": "open", "ask_waiting_on": "human"}), "intake.DispatchAskReply"},
		{dispatch, issue + "ask.edited", dispatchEventEnvelope(t, 7, "ask.edited", map[string]any{"id": "ask-5", "kind": "approval",
			"approval": map[string]any{"artifact_id": "plan-md", "version": 3, "requested_version": 2}}), "intake.DispatchApprovalMoved"},
		{dispatch, issue + "issue.claimed", dispatchEventEnvelope(t, 8, "issue.claimed", map[string]any{
			"claim": map[string]any{"actor": map[string]string{"kind": "session", "id": "ses-1"}}}), "intake.DispatchClaim"},
		{github, repo + "pr.42", githubEventEnvelope(t, "opened", pr("opened", nil)), "intake.PullRequestOpened"},
		{github, repo + "pr.42", githubEventEnvelope(t, "synchronize", pr("synchronize", map[string]any{"before": "head-1"})), "intake.PullRequestSynchronized"},
		{github, repo + "pr.42", githubEventEnvelope(t, "merged", pr("closed", map[string]any{"merged": "true", "merge_commit_sha": "merge-1"})), "intake.PullRequestMerged"},
		{github, repo + "pr.42", githubEventEnvelope(t, "closed", pr("closed", map[string]any{"merged": "false"})), "intake.PullRequestClosed"},
		{github, repo + "pr.42", githubEventEnvelope(t, "dequeued", pr("dequeued", nil)), "intake.PullRequestDequeued"},
		{github, repo + "pr.42", githubEventEnvelope(t, "review_requested", pr("review_requested", nil)), "intake.PullRequestReviewRequests"},
		{github, repo + "pr.42.review", githubEventEnvelope(t, "review", map[string]any{"kind": "review", "action": "submitted", "number": "42",
			"state": "approved", "author": "maintainer", "commit_id": "head-2", "head_sha": "head-2", "review_id": "7001",
			"submitted_at": "2026-11-20T00:00:00Z"}), "intake.PullRequestReview"},
		{github, repo + "push.branch.main", githubEventEnvelope(t, "base-push", map[string]any{"kind": "push", "ref": "refs/heads/main",
			"before": "base-1", "after": "base-2"}), "intake.BasePush"},
		{github, repo + "push.branch.legion_LEGION-208", githubEventEnvelope(t, "push", map[string]any{"kind": "push",
			"ref": "refs/heads/legion/LEGION-208", "before": "head-2", "after": "head-3", "pusher": "someone"}), "intake.Push"},
		{github, repo + "pr.42.checks", githubEventEnvelope(t, "checks", map[string]any{"kind": "checks", "number": 42, "sha": "head-2",
			"check_runs": []map[string]any{{"name": "ci", "id": 1}}, "generation": 1, "snapshot": "snapshot-1",
			"failed": map[string]any{"count": 0, "checks": []string{}}, "cancelled": map[string]any{"count": 0, "checks": []string{}}}),
			"intake.PullRequestChecks"},
		{wake, legionWakeSubject, envelopeJSON(t, "envoy-inventory-wake", "envoy", string(wakeReport)), "intake.ListenerWake"},
		{dispatch, issue + "issue.updated", []byte(`{`), ""},
	}

	logs := &lockedBuffer{}
	spec, err := normalizedSpec(ConsumerSpec{Project: "CAPTURE", Repositories: capturedRepositories, NakDelay: time.Second,
		Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatalf("normalizedSpec: %v", err)
	}
	// The bound at its least: the first failed delivery reaches the count, and no hour is waited.
	spec.GiveUpDeliveries, spec.GiveUpAfter = 1, 0
	run := consumerRun{spec: spec, repositories: spec.Repositories, pool: pool, maxAge: notificationStreamMaxAge, names: &issueNames{},
		remade: &remadeFailures{}, dropped: newDroppedSettlements(), noHeadRepo: &pacedCount{}}
	failing := handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) {
		return Result{}, errors.New("the handler fails")
	})

	types := map[string]bool{}
	for i, envelope := range envelopes {
		name := envelope.fact
		if name == "" {
			name = "a message that does not decode"
		}
		t.Run(name, func(t *testing.T) {
			decoded, decodeErr := decodeMessage(envelope.subject, spec.Project, spec.Repositories, envelope.data)
			switch {
			case envelope.fact == "" && decodeErr == nil:
				t.Fatalf("decoded %#v, want a message that does not decode", decoded.Fact)
			case envelope.fact != "" && (decodeErr != nil || fmt.Sprintf("%T", decoded.Fact) != envelope.fact):
				t.Fatalf("decoded %T (%v), want %s", decoded.Fact, decodeErr, envelope.fact)
			}
			if envelope.fact != "" {
				types[envelope.fact] = true
			}
			seq := uint64(1000 + i)
			delivery := newDelivery(t, envelope.consumer, envelope.subject, envelope.data, seq, time.Now().Add(-time.Minute))
			var committed bool
			var readErr error
			delivery.onTerm = func() {
				committed, readErr = committedGiveUp(context.Background(), pool, envelope.consumer, seq)
			}
			consumeDelivery(context.Background(), delivery, run, failing)
			acks, terms, naks := delivery.asked()
			if terms != 1 || acks != 0 || len(naks) != 0 {
				t.Fatalf("asked %d acks, %d terms and naks %v of JetStream, want the message terminated at the bound:\n%s", acks, terms, naks, logs)
			}
			if readErr != nil || !committed {
				t.Fatalf("at the termination a committed give-up record was there: %t (%v), want one before the message is terminated", committed, readErr)
			}
		})
	}
	if len(types) != 19 {
		t.Fatalf("the inventory holds %d fact types, want the 19 decodeMessage returns", len(types))
	}
}
