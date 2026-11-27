package bus

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A clustered three-copy stream takes a session consumer whose filter names a subject the stream
// does not capture while a narrowing lasts, on its create and on its update, keeps that filter as
// sent, and delivers on it in stream order once the stream captures the subject again. That is what
// the listener's consumer filter rests on: it keeps every topic of a session's interest row whatever
// the deployed stream captures (cmd/listener's sessionConsumerFilter), so a narrowing takes no topic
// out of it and a message stored after the restore is delivered. The hub runs three servers and a
// three-copy stream, where a consumer's create and update go through the stream's Raft group rather
// than one server's checks, so the case is run there and not only on a lone server.
//
// The update's control is the filter main built while narrowed, the session's agent subject alone:
// with it the consumer delivers neither notifications.team.t nor notifications.team.u after the
// restore, and the delivery step fails.
func TestASessionConsumerKeepsANarrowedFilterSubjectOnAThreeCopyStream(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	ctx := context.Background()
	const session = "ses_a"
	agent := contracts.LegacyAgentSubject(session)
	const teamT, teamU, teamOther = "notifications.team.t", "notifications.team.u", "notifications.team.other"

	// Step 1: the stream, at three copies with a leader.
	owner := startOnCluster(t, cluster, WithReplicas(3))
	js := cluster.JetStream(t)
	info := testnats.AwaitSettledStream(t, js, Stream)
	if info.Config.Replicas != 3 || info.Cluster == nil || info.Cluster.Leader == "" {
		t.Fatalf("the stream holds %d copies with cluster %+v, want three copies and a leader", info.Config.Replicas, info.Cluster)
	}

	// Step 2: notifications.team.> narrowed as the quiesce narrows it, and a JetStream publish on a
	// team topic stored nowhere. The stream's leader drops its subscription to the subject before
	// it answers the update, and the other members learn that over the cluster's routes later: a
	// member that has not yet learned it routes the publish to the leader, which drops it without
	// an answer, so the publish can time out (nats.ErrTimeout) rather than be answered no-responders
	// (nats.ErrNoStreamResponse). A server that stored the message and never answered would time out
	// too, so the publish's result decides nothing: the step reads the stream's last sequence before
	// and after it, and the publish keeps its bounded wait (Cluster.JetStream's).
	original := info.Config
	narrowed := original
	narrowed.Subjects = slices.Clone(original.Subjects)
	for i, subject := range narrowed.Subjects {
		if subject == "notifications.team.>" {
			narrowed.Subjects[i] = narrowedAsTheQuiesce([]string{subject})[0]
		}
	}
	narrowed.Metadata = map[string]string{}
	for key, value := range original.Metadata {
		narrowed.Metadata[key] = value
	}
	narrowed.Metadata[generationKey] = frozenGeneration
	if _, err := js.UpdateStream(&narrowed); err != nil {
		t.Fatalf("narrow notifications.team.>: %v", err)
	}
	settled := testnats.AwaitSettledStream(t, js, Stream)
	if read := settled.Config.Subjects; slices.Contains(read, "notifications.team.>") ||
		!slices.Contains(read, "notifications.team.__envoy_move_frozen.>") {
		t.Fatalf("the server reads the stream's subjects as %v, want notifications.team.> narrowed", read)
	}
	_, err := js.Publish(teamT, []byte("narrowed"))
	if last := testnats.AwaitSettledStream(t, js, Stream).State.LastSeq; last != settled.State.LastSeq {
		t.Fatalf("a JetStream publish on %s while narrowed returned %v, and the stream's last sequence moved from %d to %d: the stream stored it",
			teamT, err, settled.State.LastSeq, last)
	}
	t.Logf("a JetStream publish on %s while narrowed returned %v, and the stream's last sequence stayed %d", teamT, err, settled.State.LastSeq)

	// Step 3: the consumer's create and update, each naming the narrowed subject, each kept as sent.
	consumers, err := ConnectSessionConsumers(cluster.URLs(), 0, nil)
	if err != nil {
		t.Fatalf("connect session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	requireFilters := func(step string, subjects []string) {
		t.Helper()
		got, err := consumers.Info(ctx, session)
		if err != nil {
			t.Fatalf("%s: read the consumer: %v", step, err)
		}
		want := SessionConsumerFilters(subjects)
		if sorted := slices.Sorted(slices.Values(got.Config.FilterSubjects)); !slices.Equal(sorted, slices.Sorted(slices.Values(want))) {
			t.Fatalf("%s: the server holds the filter %v, want %v", step, sorted, want)
		}
		if !slices.Contains(got.Config.FilterSubjects, teamT) {
			t.Fatalf("%s: the server holds the filter %v, without %s", step, got.Config.FilterSubjects, teamT)
		}
	}
	created := []string{agent, teamT}
	if _, err := consumers.Ensure(ctx, session, created, 0); err != nil {
		t.Fatalf("create the consumer of %s with %v while narrowed: %v", session, created, err)
	}
	requireFilters("after the create", created)
	updated := []string{agent, teamT, teamU}
	if err := consumers.UpdateFilter(ctx, session, updated); err != nil {
		t.Fatalf("update the consumer of %s to %v while narrowed: %v", session, updated, err)
	}
	requireFilters("after the update", updated)

	// Step 4: the restore, and delivery on the filter in stream order.
	restored := original
	restored.Metadata = narrowed.Metadata
	if _, err := js.UpdateStream(&restored); err != nil {
		t.Fatalf("restore the stream's subjects: %v", err)
	}
	if read := testnats.AwaitSettledStream(t, js, Stream).Config.Subjects; !slices.Contains(read, "notifications.team.>") {
		t.Fatalf("the server reads the restored subjects as %v, want notifications.team.> back", read)
	}
	var want []string
	var wantSequences []uint64
	for i, subject := range []string{teamT, teamOther, teamU, agent} {
		sequence := publishStored(t, owner, subject, "after the restore on "+subject)
		// The stream stores its publishes in the order it takes them, so a narrowed publish it stored
		// after step 2's read would put this one past the sequence after that read.
		if i == 0 && sequence != settled.State.LastSeq+1 {
			t.Fatalf("the first publish after the restore was stored at %d, want %d, the sequence after the narrowed stream's last: the stream stored something since step 2", sequence, settled.State.LastSeq+1)
		}
		if subject != teamOther {
			want = append(want, subject)
			wantSequences = append(wantSequences, sequence)
		}
	}
	pull := pullSession(t, consumers, session)
	var got []string
	var gotSequences []uint64
	for range want {
		// No span of its own: a delivery can come late on a loaded machine with nothing wrong, and
		// one that never comes ends the pull at the test binary's deadline less testwait's margin.
		msg, err := pull.Next(testwait.Context(t))
		if err != nil {
			break
		}
		got, gotSequences = append(got, msg.Subject), append(gotSequences, msg.StreamSeq)
		if err := msg.Ack(); err != nil {
			t.Fatalf("ack %s: %v", msg.Subject, err)
		}
	}
	if !slices.Equal(got, want) || !slices.Equal(gotSequences, wantSequences) {
		t.Fatalf("the consumer delivered [%s] at %v, want [%s] at %v",
			strings.Join(got, ", "), gotSequences, strings.Join(want, ", "), wantSequences)
	}
	requireNoMessage(t, pull, 500*time.Millisecond)
}
