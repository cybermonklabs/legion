package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A topic that a session's consumer filter leaves out by its fourth or fifth kind
// (sessionConsumerFilter) never leaves it silently. The registration's answer names it as not
// registered, with the reason; the listener logs one WARN line naming the session and the topic;
// and the interest row does not hold it, so no publish counts the session as a recipient for it
// (recipientCounts, which reads the rows and the known-session records, the latter written from
// the same row). Before, such a topic stayed in the row and the registration was answered 200 with
// no word of it: a message on it was stored, counted as reaching the session, and never delivered
// by the session's consumer.
//
// The fourth kind is *.team.foo, which does not begin with the literal notifications. and matches
// notifications.team.foo, and which a registration plans (ReachesNoSession is false for it). The
// fifth is the pair notifications.envoy.exceptions.foo.* and notifications.envoy.exceptions.*.bar,
// which the filter's reduction merges into notifications.envoy.exceptions.>, every session's failed
// deliveries. No subject this build compiles captures either of the pair, so a registration splits
// each off as uncaptured before it plans (splitUncapturedTopics); a row holds them when an earlier
// build stored them, or a newer build's subjects capture them. So the later steps store such a row:
// a rebuild of the filter from it logs the same WARN line; before the session's next registration,
// while the row and the known-session record an earlier build left still hold them, a publish
// counts the session as no recipient through them; and that registration names all three and takes
// them out of the row, though its body names none of them.
func TestATopicTheFilterLeavesOutIsNamedLoggedAndKeptOutOfTheRow(t *testing.T) {
	// Not parallel: it swaps the clock and memory of registrationLeftOutLogs, the one left-out
	// limiter every registration handler in the process shares (leftOutLogLimiter.isolate), and
	// asserts on the WARN lines that limiter gates.
	const (
		outside  = "*.team.foo"
		pairFoo  = "notifications.envoy.exceptions.foo.*"
		pairBar  = "notifications.envoy.exceptions.*.bar"
		merged   = "notifications.envoy.exceptions.>"
		kept     = "notifications.team.deploy"
		leftLine = "listener left a topic out of the session's consumer filter"
		sesA     = "ses_left_out_a"
		sesB     = "ses_left_out_b"
		sesC     = "ses_left_out_c"
		// counted follows notifications.team.foo as a registration of this build stores it, so a
		// publish on that subject has one recipient the count must find.
		counted = "ses_left_out_counted"
	)
	// A subject a publish can carry that each left-out topic matches, and the part of its warning
	// that says why it went.
	matched := map[string]string{outside: "notifications.team.foo", pairFoo: "notifications.envoy.exceptions.foo.bar", pairBar: "notifications.envoy.exceptions.foo.bar"}
	reason := map[string]string{outside: "begin with the literal notifications.", pairFoo: merged, pairBar: merged}
	for topic, subject := range matched {
		if !bus.SubjectContains(topic, subject) {
			t.Fatalf("%s does not match %s, so the recipient check below could not see it", topic, subject)
		}
	}
	if !bus.StreamCaptures(matched[outside]) {
		t.Fatalf("no stream subject captures %s, so a publish on it is refused and carries no count", matched[outside])
	}

	// The registration line is logged once per session and topic an hour (registrationLeftOutLogs);
	// each run reads a limiter of its own, so a run after the first is told the same.
	t.Cleanup(registrationLeftOutLogs.isolate(nil))
	rig := newRecipientRig(t)
	registry := rig.harness.registry
	register := func(t *testing.T, sessionID string, topics ...string) interestSubscribeAnswer {
		t.Helper()
		var answer interestSubscribeAnswer
		if err := json.Unmarshal([]byte(rig.post(t, "/v1/interests/subscribe", consumerRegistration(sessionID, "e", topics...))), &answer); err != nil {
			t.Fatalf("decode the registration's answer: %v", err)
		}
		return answer
	}
	// named checks that answer names each of leftOut once, as not registered and why, and names
	// the kept topic in no warning.
	named := func(t *testing.T, answer interestSubscribeAnswer, leftOut ...string) {
		t.Helper()
		for _, topic := range leftOut {
			var found []string
			for _, warning := range answer.Warnings {
				if strings.HasPrefix(warning, topic+" was not registered: ") {
					found = append(found, warning)
				}
			}
			if len(found) != 1 || !strings.Contains(found[0], reason[topic]) {
				t.Errorf("warnings %q name %s %d times, want once, as not registered because of %s", answer.Warnings, topic, len(found), reason[topic])
			}
		}
		for _, warning := range answer.Warnings {
			if strings.HasPrefix(warning, kept+" ") {
				t.Errorf("warnings %q name %s, which the filter keeps", answer.Warnings, kept)
			}
		}
	}
	// logged checks that logs, the listener's log since a step began, holds one WARN line for
	// sessionID and each of leftOut, written by the path from names, with the topic's kind and,
	// for the fifth kind alone, the merged filter that made it go.
	logged := func(t *testing.T, logs, sessionID, from string, leftOut ...string) {
		t.Helper()
		for _, topic := range leftOut {
			lines := linesIn(logs, leftLine, map[string]any{"session_id": sessionID, "topic": topic, "from": from})
			if len(lines) != 1 || lines[0]["level"] != "WARN" {
				t.Errorf("the log holds %v for %s and %s from %s, want one WARN line %q:\n%s", lines, sessionID, topic, from, leftLine, logs)
				continue
			}
			wantKind, wantMerged := "merges_into_private", any(merged)
			if topic == outside {
				wantKind, wantMerged = "no_notifications_prefix", nil
			}
			if lines[0]["kind"] != wantKind || lines[0]["merged"] != wantMerged {
				t.Errorf("the WARN line for %s from %s has kind %v and merged %v, want %s and %v", topic, from, lines[0]["kind"], lines[0]["merged"], wantKind, wantMerged)
			}
		}
	}
	// recipients is how many sessions a publish on subject from recipientSender counts
	// (recipientCounts, read as the publish route reads it).
	recipients := func(t *testing.T, subject string) int {
		t.Helper()
		total, _ := recipientCounts(t.Context(), rig.deps, subject, recipientSender)
		if total == nil {
			t.Fatalf("a publish on %s carries no count, so the count cannot be read", subject)
		}
		return *total
	}
	// outOfRow checks that sessionID's interest row holds none of leftOut and holds keptTopic, and
	// that a publish on a subject one of leftOut matches counts the session as no recipient, while
	// one on keptTopic, which the session alone follows, counts it, so the count sees the session.
	outOfRow := func(t *testing.T, sessionID, keptTopic string, leftOut ...string) {
		t.Helper()
		row, _, err := registry.RefreshAt(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("read the interest row of %s: %v", sessionID, err)
		}
		for _, topic := range leftOut {
			if slices.Contains(row.Topics, topic) {
				t.Errorf("the interest row of %s holds %v, %s among them", sessionID, row.Topics, topic)
			}
		}
		if !slices.Contains(row.Topics, keptTopic) {
			t.Errorf("the interest row of %s holds %v, without %s", sessionID, row.Topics, keptTopic)
		}
		for _, topic := range leftOut {
			if slices.ContainsFunc(registry.MatchAllMachines(matched[topic]), func(interest store.Interest) bool { return interest.SessionID == sessionID }) {
				t.Errorf("a publish on %s finds %s's row through %s", matched[topic], sessionID, topic)
			}
		}
		if got := recipients(t, keptTopic); got != 1 {
			t.Errorf("a publish on %s, which %s alone follows, counts %d recipients, want 1", keptTopic, sessionID, got)
		}
	}

	t.Run("a registration that names a topic with a wildcard first token", func(t *testing.T) {
		const keptA = kept + "-a"
		before := rig.harness.logs.Len()
		answer := register(t, sesA, outside, keptA)
		named(t, answer, outside)
		logged(t, rig.harness.logs.String()[before:], sesA, "registration", outside)
		outOfRow(t, sesA, keptA, outside)
		if got := recipients(t, matched[outside]); got != 0 {
			t.Errorf("a publish on %s counts %d recipients, want none: only %s's %s matches it", matched[outside], got, sesA, outside)
		}
		filters := rig.filters(t, sesA)
		if slices.Contains(filters, outside) || !slices.Contains(filters, keptA) {
			t.Errorf("the consumer of %s filters on %v, want %s and not %s", sesA, filters, keptA, outside)
		}
	})

	t.Run("an earlier build's row that holds both kinds", func(t *testing.T) {
		const keptB = kept + "-b"
		leftOut := []string{outside, pairFoo, pairBar}
		register(t, sesB, keptB)
		kv, err := rig.harness.client.JS().KeyValue(bus.InterestBucket)
		if err != nil {
			t.Fatalf("open the interest bucket: %v", err)
		}
		earlier, err := json.Marshal(store.Interest{SessionID: sesB, MachineID: "test-machine", UpdatedAt: contracts.NowMillis(), Topics: append([]string{contracts.LegacyAgentSubject(sesB), keptB}, leftOut...)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := kv.Put(sesB, earlier); err != nil {
			t.Fatalf("store %s's row as an earlier build stored it: %v", sesB, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		before := rig.harness.logs.Len()
		if _, outcome, _ := rig.deps.consumers.storedFilterStep(ctx, registry, sesB, "left-out test"); outcome != filterWriteLanded {
			t.Errorf("storedFilterStep for %s answered %v, want a write that landed", sesB, outcome)
		}
		logged(t, rig.harness.logs.String()[before:], sesB, "stored_row", leftOut...)

		before = rig.harness.logs.Len()
		answer := register(t, sesB, keptB)
		named(t, answer, leftOut...)
		logged(t, rig.harness.logs.String()[before:], sesB, "registration", leftOut...)
		outOfRow(t, sesB, keptB, leftOut...)
	})

	t.Run("an earlier build's row and record before the session's next registration", func(t *testing.T) {
		const keptC = kept + "-c"
		earlier := []string{contracts.LegacyAgentSubject(sesC), keptC, outside, pairFoo, pairBar}
		rig.register(t, counted, []string{matched[outside]})
		// The row and the record as an earlier build left them: a merging write, which no plan of
		// this build's registration shapes, and the record written from that row.
		if _, err := registry.Upsert(store.Interest{SessionID: sesC, MachineID: "other-machine"}, earlier); err != nil {
			t.Fatalf("store %s's row as an earlier build stored it: %v", sesC, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := rig.known.RecordWithin(ctx, session.KnownSession{SessionID: sesC, Title: "an earlier build's session", Topics: earlier}); err != nil {
			t.Fatalf("store %s's known-session record as an earlier build stored it: %v", sesC, err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			row, cached := registry.CachedInterest(sesC)
			record, known := rig.known.Get(sesC)
			if cached && known && slices.Contains(row.Topics, outside) && slices.Contains(record.Topics, outside) &&
				slices.Contains(row.Topics, pairFoo) && slices.Contains(row.Topics, pairBar) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the caches hold %s's row %v (%t) and record %v (%t), want both with %s, %s and %s: the earlier build's window is not there to test",
					sesC, row.Topics, cached, record.Topics, known, outside, pairFoo, pairBar)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if got := recipients(t, keptC); got != 1 {
			t.Errorf("a publish on %s, which %s alone follows, counts %d recipients, want 1, so the count does not see the session", keptC, sesC, got)
		}
		// No subject of this build captures the pair's lanes, so the publish route refuses a
		// publish on one; the count it would carry is read on its own.
		if got := recipients(t, matched[pairFoo]); got != 0 {
			t.Errorf("a publish on %s counts %d recipients, want none: only %s's %s and %s match it", matched[pairFoo], got, sesC, pairFoo, pairBar)
		}
		// A real publish on notifications.team.foo counts the session that follows it as this build
		// stores it, and not the one whose row and record match it only through *.team.foo.
		assertCounts(t, rig.publish(t, matched[outside], "left-out-count"), 1, 1)
	})
}

// A pair of the fifth kind reaches a registration of this build when a newer build's deployed
// subjects capture it: the deployed stream carries notifications.envoy.exceptions.*.*, which
// captures notifications.envoy.exceptions.foo.* and notifications.envoy.exceptions.*.bar read
// literally, so the registration's split keeps both (splitUncapturedTopics, ReachesNoSession is
// false) and its plan is what leaves them out (planWithoutLeftOut). The answer names each with the
// merge that made it go, and not as uncaptured; the listener logs one WARN line for each, of the
// fifth kind with the merged filter; the row holds neither; and a real publish on a subject the pair
// matches, which the deployed stream stores, counts the session as no recipient.
func TestAFifthKindPairANewerBuildsSubjectsCaptureIsLeftOutOfTheRegistration(t *testing.T) {
	// Not parallel: it swaps the clock and memory of registrationLeftOutLogs, the one left-out
	// limiter every registration handler in the process shares (leftOutLogLimiter.isolate), and
	// asserts on the WARN lines that limiter gates.
	const (
		newer    = "notifications.envoy.exceptions.*.*"
		pairFoo  = "notifications.envoy.exceptions.foo.*"
		pairBar  = "notifications.envoy.exceptions.*.bar"
		merged   = "notifications.envoy.exceptions.>"
		subject  = "notifications.envoy.exceptions.foo.bar"
		kept     = "notifications.team.deploy-newer"
		leftLine = "listener left a topic out of the session's consumer filter"
		sesNewer = "ses_left_out_newer"
	)
	pair := []string{pairFoo, pairBar}
	for _, topic := range append([]string{subject}, pair...) {
		if bus.StreamCaptures(topic) {
			t.Fatalf("this build captures %s itself, so no newer build's subject is what keeps it", topic)
		}
	}
	url := testnats.URL(t)
	deployNewerBuildsStream(t, url, newer)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1), bus.WithStreamSubjectsRefresh(100*time.Millisecond))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	for _, topic := range append([]string{subject}, pair...) {
		bustest.AwaitDeployedCapture(t, client, topic, true, 15*time.Second)
		if client.ReachesNoSession(topic) {
			t.Fatalf("the listener's client reads %s as reaching no session, so the registration would split it off as uncaptured", topic)
		}
	}
	t.Cleanup(registrationLeftOutLogs.isolate(nil))
	rig := &recipientRig{knownSessionRig: newKnownSessionRigOn(t, client, url)}

	before := rig.harness.logs.Len()
	var answer interestSubscribeAnswer
	if err := json.Unmarshal([]byte(rig.post(t, "/v1/interests/subscribe", consumerRegistration(sesNewer, "e", pairFoo, pairBar, kept))), &answer); err != nil {
		t.Fatalf("decode the registration's answer: %v", err)
	}
	logs := rig.harness.logs.String()[before:]
	for _, topic := range pair {
		var found []string
		for _, warning := range answer.Warnings {
			if strings.HasPrefix(warning, topic+" was not registered: ") {
				found = append(found, warning)
			}
		}
		if len(found) != 1 || !strings.Contains(found[0], merged) || found[0] == contracts.UncapturedTopicRefusal(topic, sesNewer) {
			t.Errorf("warnings %q name %s %d times, want once, with the merge into %s", answer.Warnings, topic, len(found), merged)
		}
		lines := linesIn(logs, leftLine, map[string]any{"session_id": sesNewer, "topic": topic, "from": "registration"})
		if len(lines) != 1 || lines[0]["level"] != "WARN" || lines[0]["kind"] != "merges_into_private" || lines[0]["merged"] != merged {
			t.Errorf("the log holds %v for %s, want one WARN line %q of kind merges_into_private, merged %s:\n%s", lines, topic, leftLine, merged, logs)
		}
	}
	row, _, err := rig.harness.registry.RefreshAt(context.Background(), sesNewer)
	if err != nil {
		t.Fatalf("read the interest row of %s: %v", sesNewer, err)
	}
	for _, topic := range pair {
		if slices.Contains(row.Topics, topic) {
			t.Errorf("the interest row of %s holds %v, %s among them", sesNewer, row.Topics, topic)
		}
	}
	if !slices.Contains(row.Topics, kept) {
		t.Errorf("the interest row of %s holds %v, without %s", sesNewer, row.Topics, kept)
	}
	// The count sees the session at all: a publish on the topic it alone keeps counts it.
	assertCounts(t, rig.publish(t, kept, "newer-kept"), 1, 1)
	assertCounts(t, rig.publish(t, subject, "newer-pair"), 0, 0)
}
