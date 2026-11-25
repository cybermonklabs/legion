package api

import "testing"

// A person's message to a role nobody holds, whose newest registry entry is a finished agent's,
// is redirected to that agent's session only so the agent's daemon can answer it. When no daemon
// answers (the project's daemon runs without `answers`, or has stopped), main's listener takes the
// redirected send as it takes any send to a session Envoy still knows: 200, stored for a session
// that will not run again, state stopped (#2643). That is not an answer, so the message gets
// what it got before the registry existed: the listener's own answer for the role, failed with no
// holder, the same for a role message and a role mention. Recording it queued would tell the
// person it reached the role while it sits on a dead session's subject until it expires.
//
// sjawhar/legion#2230's proof run 43920318131 (in the deployment repository) showed it under A13
// with answers removed: the branch recorded the role message queued for the finished architect's
// session, where the base commit recorded it failed, no holder for role.
func TestARoleMessageTheListenerOnlyStoresForAFinishedSessionKeepsTheNoHolderFailure(t *testing.T) {
	t.Parallel()
	listener := newStoppedAgentListener(t)
	listener.storeSends.Store(true)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Stored, not answered", "before")
	noHolder := "no holder for role " + registeredRole

	message := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Who holds this?", "target": "role:" + registeredRole, "delivery": "btw",
	}, "alice")
	if got := message.Deliveries; len(got) != 1 || got[0].State != "failed" || got[0].SessionID != "" ||
		got[0].Error == nil || *got[0].Error != noHolder {
		t.Fatalf("a role message the listener stored for the finished session %s, answered by no daemon: deliveries = %#v, want failed %q with no session",
			stoppedNewest, got, noHolder)
	}

	roleTarget := "role:" + registeredRole
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body":     "Mentioning a finished role.",
		"mentions": []map[string]any{{"target": roleTarget}},
	}))
	if len(comment.Deliveries) != 1 {
		t.Fatalf("mention deliveries = %#v, want one", comment.Deliveries)
	}
	for _, delivery := range settledCommentDeliveries(t, database, comment.ID) {
		if delivery.State != "failed" || delivery.SessionID != nil || delivery.Error == nil || *delivery.Error != noHolder {
			t.Fatalf("%s mention delivery = %#v (session %v, error %q), want failed %q with no session",
				delivery.Target, delivery, deref(delivery.SessionID), deref(delivery.Error), noHolder)
		}
	}
}
