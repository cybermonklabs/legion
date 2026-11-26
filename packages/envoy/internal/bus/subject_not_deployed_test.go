package bus

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// frozenGeneration is the generation a hub move's freeze stamps on the stream it narrows, above
// every build's, so that no owning client widens the stream again (ensureStreamWithConfig leaves
// a stream of a higher generation as it is).
const frozenGeneration = "1000000000"

// narrowStreamAsTheQuiesce narrows the deployed stream through js as a hub move's quiesce does
// (narrowedAsTheQuiesce, stamped frozenGeneration), and returns the configuration it replaced, for
// the restore.
func narrowStreamAsTheQuiesce(t *testing.T, js nats.JetStreamContext) nats.StreamConfig {
	t.Helper()
	before := deployedStream(t, js).Config
	narrowed := before
	narrowed.Subjects = narrowedAsTheQuiesce(before.Subjects)
	narrowed.Metadata = map[string]string{}
	for key, value := range before.Metadata {
		narrowed.Metadata[key] = value
	}
	narrowed.Metadata[generationKey] = frozenGeneration
	if _, err := js.UpdateStream(&narrowed); err != nil {
		t.Fatalf("narrow the stream as the quiesce does: %v", err)
	}
	return before
}

// awaitDeployedCapture waits until client's last read of the deployed subjects answers captures
// for topic: the control that a publish after it is judged on the list the test just deployed, and
// not on the one the client held before.
func awaitDeployedCapture(t *testing.T, client *Client, topic string, captures bool) {
	t.Helper()
	testwait.Until(t, "the client to read a deployed list where "+topic+" is captured="+strconv.FormatBool(captures),
		func() (bool, bool) {
			read := client.DeployedStreamCaptures(topic)
			return read, read == captures
		})
}

// A topic this build stores, which the deployed stream does not capture, is sent nowhere: each of
// the five publish functions returns ErrSubjectNotDeployed, which names the stream and the topic,
// and neither a core subscriber of the topic nor the stream receives anything. Before the refusal
// the publish went out over core NATS, the subscriber was handed it, nothing stored it, and the
// publish answered nil: the send route's 200 for a message nobody received.
//
// The subscriber holds notifications.>, so it receives a core publish of any of these topics and
// the request of a JetStream publish too: its silence in step 2 is no blindness, which step 3 shows
// on two topics that still take core NATS.
func TestNothingIsSentForATopicThisBuildStoresWhileTheDeployedStreamLacksIt(t *testing.T) {
	uri := testnats.URL(t)
	client, err := ConnectOwningStream([]string{uri}, WithStreamSubjectsRefresh(100*time.Millisecond))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js := rawJetStream(t, uri)
	const session = "ses_a"
	agentTopic := contracts.AgentTopicPrefix + session

	original := narrowStreamAsTheQuiesce(t, js)
	awaitDeployedCapture(t, client, agentTopic, false)

	subscriber := testnats.Connect(t, uri)
	t.Cleanup(subscriber.Close)
	received, err := subscriber.SubscribeSync(NotificationTopicPrefix + ">")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	before := deployedStream(t, js).State.LastSeq

	envelope := func(topic, id string) contracts.Envelope {
		return contracts.Envelope{
			EventID:        "evt-" + id,
			Source:         "agent",
			SourceEventID:  "source-" + id,
			Topic:          topic,
			DedupeKey:      "publish." + id,
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: "a message the narrowed stream cannot store",
			TraceID:        "trace-" + id,
		}
	}
	publishes := []struct {
		name    string
		publish func(contracts.Envelope) error
	}{
		{"Publish", client.Publish},
		{"PublishWithin", func(item contracts.Envelope) error { return client.PublishWithin(context.Background(), item) }},
		{"PublishReportingDuplicate", func(item contracts.Envelope) error {
			_, err := client.PublishReportingDuplicate(item)
			return err
		}},
		{"PublishReportingDuplicateWithin", func(item contracts.Envelope) error {
			_, err := client.PublishReportingDuplicateWithin(context.Background(), item)
			return err
		}},
		{"PublishAckWithin", func(item contracts.Envelope) error {
			_, err := client.PublishAckWithin(context.Background(), item)
			return err
		}},
	}
	// Step 1: every publish function refuses every topic this build stores.
	for _, publish := range publishes {
		for _, topic := range []string{agentTopic, "notifications.team.t", ExceptionTopicPrefix + agentTopic} {
			err := publish.publish(envelope(topic, publish.name+"-"+topic))
			if !errors.Is(err, ErrSubjectNotDeployed) {
				t.Errorf("%s of %s: %v, want ErrSubjectNotDeployed", publish.name, topic, err)
				continue
			}
			for _, other := range []error{ErrRefused, ErrUnacknowledged, ErrUnavailable, ErrStreamFull, ErrPublishDenied} {
				if errors.Is(err, other) {
					t.Errorf("%s of %s: %v also matches %v", publish.name, topic, err, other)
				}
			}
			for _, part := range []string{Stream, topic, "nothing was sent"} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("%s of %s: %q does not name %q", publish.name, topic, err, part)
				}
			}
		}
	}

	// Step 2: nothing reached the wire, and the stream stored nothing.
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush the subscriber: %v", err)
	}
	if msg, err := received.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("the subscriber was handed %s (reply %q); a refused publish sends nothing", msg.Subject, msg.Reply)
	} else if !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("read the subscriber: %v", err)
	}
	if after := deployedStream(t, js).State.LastSeq; after != before {
		t.Fatalf("the stream's last sequence moved from %d to %d; a refused publish stores nothing", before, after)
	}

	// Step 3, the control: a topic that still takes core NATS reaches the same subscriber.
	for _, topic := range []string{"notifications.lxspec.probe.all", contracts.RoleTopicPrefix + "reviewer"} {
		if _, err := client.PublishAckWithin(context.Background(), envelope(topic, "control-"+topic)); err != nil {
			t.Fatalf("the control publish of %s: %v", topic, err)
		}
		msg, err := received.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("the subscriber received nothing of the control publish of %s: %v", topic, err)
		}
		if msg.Subject != topic {
			t.Fatalf("the subscriber received %s, want the control publish of %s", msg.Subject, topic)
		}
	}

	// Step 4: once the stream captures the topic again and the client has read that, the same
	// publish is stored, once.
	original.Metadata = deployedStream(t, js).Config.Metadata
	if _, err := js.UpdateStream(&original); err != nil {
		t.Fatalf("restore the stream's subjects: %v", err)
	}
	awaitDeployedCapture(t, client, agentTopic, true)
	restored := envelope(agentTopic, "restored")
	if _, err := client.PublishAckWithin(context.Background(), restored); err != nil {
		t.Fatalf("publish of %s after the restore: %v", agentTopic, err)
	}
	info, err := js.StreamInfo(Stream, &nats.StreamInfoRequest{SubjectsFilter: agentTopic})
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if got := info.State.Subjects[agentTopic]; got != 1 {
		t.Fatalf("the stream holds %d messages on %s after the restore, want 1", got, agentTopic)
	}
}
