package bus

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
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
	// team topic stored nowhere.
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
	if read := testnats.AwaitSettledStream(t, js, Stream).Config.Subjects; slices.Contains(read, "notifications.team.>") ||
		!slices.Contains(read, "notifications.team.__envoy_move_frozen.>") {
		t.Fatalf("the server reads the stream's subjects as %v, want notifications.team.> narrowed", read)
	}
	if _, err := js.Publish(teamT, []byte("narrowed")); !errors.Is(err, nats.ErrNoStreamResponse) {
		t.Fatalf("a JetStream publish on %s while narrowed: %v, want no stream to answer it", teamT, err)
	}

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
	for _, subject := range []string{teamT, teamOther, teamU, agent} {
		sequence := publishStored(t, owner, subject, "after the restore on "+subject)
		if subject != teamOther {
			want = append(want, subject)
			wantSequences = append(wantSequences, sequence)
		}
	}
	pull := pullSession(t, consumers, session)
	var got []string
	var gotSequences []uint64
	for range want {
		waited, cancel := context.WithTimeout(ctx, 5*time.Second)
		msg, err := pull.Next(waited)
		cancel()
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
