package bustest

import (
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testwait"
)

// FrozenGeneration is the generation a hub move's freeze stamps on the stream it narrows
// (the stream metadata dispatch.stream-generation), above every build's, so that no owning client
// widens the stream again: a start or a keeper leaves a stream of a higher generation as it is.
const FrozenGeneration = "1000000000"

// generationKey is the metadata key the generation is stamped under. bus keeps its own unexported,
// and every build reads it under this name.
const generationKey = "dispatch.stream-generation"

// NarrowedAsTheQuiesce is subjects as a hub move's quiesce narrows them, so that the stream stores
// nothing a client publishes: the first wildcard token of each subject becomes
// __envoy_move_frozen, a `>` becoming __envoy_move_frozen.> (notifications.agent.> becomes
// notifications.agent.__envoy_move_frozen.>, notifications.x.* becomes
// notifications.x.__envoy_move_frozen). A subject with no wildcard is left as it is. The bus's own
// tests keep a copy (narrowedAsTheQuiesce), since package bus cannot import this one.
func NarrowedAsTheQuiesce(subjects []string) []string {
	narrowed := make([]string, len(subjects))
	for i, subject := range subjects {
		tokens := strings.Split(subject, ".")
		for j, token := range tokens {
			if token == "*" {
				tokens[j] = "__envoy_move_frozen"
				break
			}
			if token == ">" {
				tokens[j] = "__envoy_move_frozen.>"
				break
			}
		}
		narrowed[i] = strings.Join(tokens, ".")
	}
	return narrowed
}

// NarrowStream narrows the deployed notifications stream through js as a hub move's quiesce does:
// each subject that keep does not name is narrowed (NarrowedAsTheQuiesce), and the stream is
// stamped FrozenGeneration. keep nil narrows every subject. It returns the configuration it
// replaced, which RestoreStream puts back.
func NarrowStream(t testing.TB, js natsgo.JetStreamContext, keep func(subject string) bool) natsgo.StreamConfig {
	t.Helper()
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read %s to narrow it: %v", bus.Stream, err)
	}
	original := info.Config
	narrowed := original
	narrowed.Subjects = make([]string, len(original.Subjects))
	for i, subject := range original.Subjects {
		narrowed.Subjects[i] = subject
		if keep == nil || !keep(subject) {
			narrowed.Subjects[i] = NarrowedAsTheQuiesce([]string{subject})[0]
		}
	}
	narrowed.Metadata = maps.Clone(original.Metadata)
	if narrowed.Metadata == nil {
		narrowed.Metadata = map[string]string{}
	}
	narrowed.Metadata[generationKey] = FrozenGeneration
	if _, err := js.UpdateStream(&narrowed); err != nil {
		t.Fatalf("narrow %s as the quiesce does: %v", bus.Stream, err)
	}
	return original
}

// RestoreStream puts original's subjects back on the deployed notifications stream through js, as
// the move's restore does, keeping the generation the stream holds now, so that no owning client
// rewrites it in between.
func RestoreStream(t testing.TB, js natsgo.JetStreamContext, original natsgo.StreamConfig) {
	t.Helper()
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read %s to restore it: %v", bus.Stream, err)
	}
	restored := info.Config
	restored.Subjects = original.Subjects
	if _, err := js.UpdateStream(&restored); err != nil {
		t.Fatalf("restore the subjects of %s: %v", bus.Stream, err)
	}
}

// AwaitDeployedCapture waits until client's last read of the deployed stream's subjects answers
// captures for topic (bus.Client.DeployedStreamCaptures), and fails the test when it does not
// within bound. It is the control that what a test does next is judged on the list it just
// deployed: a client that still holds the list before a narrowing publishes through JetStream, and
// a test of the refusal after a narrowing proves nothing until the client has read it. The client
// reads the subjects every bus.WithStreamSubjectsRefresh, which a test sets short.
func AwaitDeployedCapture(t *testing.T, client *bus.Client, topic string, captures bool, bound time.Duration) {
	t.Helper()
	testwait.WithinPolling(t, testwait.Bounded(t, bound, time.Second), 20*time.Millisecond,
		"the client to read a deployed list where "+topic+" is captured="+strconv.FormatBool(captures),
		func() bool { return client.DeployedStreamCaptures(topic) == captures })
}
