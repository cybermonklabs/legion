package bus

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// anotherProgramsStream is a stream another program of the deployment keeps beside the
// notifications stream, which no session consumer reads.
const anotherProgramsStream = "ANOTHER_PROGRAMS_STREAM"

// storedPublish is one kind of stored send of the bus, made to subject. unanswered is the error of
// the send's own words that a send nothing answered still carries beside ErrStreamLacksSubject,
// since its callers read it: a publish's nats.ErrNoStreamResponse, a window's ErrNotAcknowledged,
// which Dispatch's outbox backs a send off by, and a forward's ErrForwardNotStored, which the
// listener reports as delivery_failed.
type storedPublish struct {
	name       string
	publish    func(c *Client, subject string) error
	unanswered error
}

// storedPublishes is every kind of stored send of the bus: the publish of an envelope on its own
// topic and on a subject the caller names, the cursor write, a window's two sends and its request,
// and a forward.
func storedPublishes() []storedPublish {
	envelope := func(subject string) contracts.Envelope {
		return contracts.Envelope{
			EventID:   "evt-" + subject,
			Source:    "agent",
			Topic:     subject,
			DedupeKey: "agent.stored." + subject,
			IssuedAt:  contracts.NowMillis(),
		}
	}
	return []storedPublish{
		{"Publish", func(c *Client, subject string) error {
			return c.Publish(envelope(subject))
		}, nats.ErrNoStreamResponse},
		{"PublishStoredAck", func(c *Client, subject string) error {
			_, err := c.PublishStoredAck(context.Background(), subject, envelope(subject), "stored-ack."+subject)
			return err
		}, nats.ErrNoStreamResponse},
		{"PublishCursorAt", func(c *Client, subject string) error {
			_, err := c.PublishCursorAt(context.Background(), subject, []byte(`{"cursor":1}`), 0)
			return err
		}, nats.ErrNoStreamResponse},
		{"a window's PublishAsync", func(c *Client, subject string) error {
			return <-c.WindowUntil(nil).PublishAsync(envelope(subject))
		}, ErrNotAcknowledged},
		{"a window's PublishStoredAsync", func(c *Client, subject string) error {
			return <-c.WindowUntil(nil).PublishStoredAsync(subject, envelope(subject), "stored-async."+subject)
		}, ErrNotAcknowledged},
		{"a window's Request", func(c *Client, subject string) error {
			return <-c.WindowUntil(nil).Request(subject, envelope(subject), RequestOptions{MsgID: "request." + subject, Timeout: storedPublishWindow})
		}, ErrNotAcknowledged},
		{"a forward", func(c *Client, subject string) error {
			forward, err := c.StartRequestStoredTo(subject, envelope(subject), "forward."+subject, storedPublishWindow)
			if err != nil {
				return err
			}
			return forward.Wait()
		}, ErrForwardNotStored},
	}
}

// storedPublishWindow is how long a window's Request and a forward of storedPublishes wait for the
// stream's answer.
const storedPublishWindow = 2 * time.Second

// Every stored send of the bus counts as stored only on the acknowledgement of the notifications
// stream (Stream), the one stream the session consumers read, and a send that stream did not take
// because, as the client read it, the deployed stream captures no subject of the send's is named
// ErrStreamLacksSubject, with the subject and the stream's generation, as soon as the server says
// so. A newer build's notifications stream, which this build leaves as it is, can drop a subject
// this build stores: then no stream answers the send, or another stream of the account captures
// the subject, whose acknowledgement would be of a copy no session consumer reads, so it refuses
// the send that names the notifications stream and neither stream holds the message. So a caller
// answers it as what it is and at once, and does not retry it as a send nothing answered: the
// listener's role send stops after one forward instead of forty, and a window's Request stops at
// the server's answer instead of at its timeout. A send nothing answered keeps the words its
// callers read beside it (storedPublish.unanswered). Each kind of stored send (storedPublishes) is
// a row over each of three streams, and no row leaves a copy in the other stream. The negative
// control is the stream that captures the subject: nil and one copy each, so an error over the
// other two is the stream's doing and not the row's.
func TestEveryStoredPublishTheDeployedStreamLeavesOutIsNamedErrStreamLacksSubjectAtOnce(t *testing.T) {
	agentSubjects := contracts.AgentTopicPrefix + ">"
	omitsAgent := slices.DeleteFunc(slices.Clone(streamCarriedSubjects), func(s string) bool { return s == agentSubjects })
	if len(omitsAgent) == len(streamCarriedSubjects) {
		t.Fatalf("this build carries no %s to drop", agentSubjects)
	}
	for _, stream := range []struct {
		name     string
		subjects []string
		another  bool
	}{
		{"the control: the notifications stream captures the subject", streamCarriedSubjects, false},
		{"the notifications stream dropped the subject and no stream captures it", omitsAgent, false},
		{"the notifications stream dropped the subject and another stream captures it", omitsAgent, true},
	} {
		t.Run(stream.name, func(t *testing.T) {
			uri := testnats.URL(t)
			js := rawJetStream(t, uri)
			if _, err := js.AddStream(&nats.StreamConfig{
				Name:      Stream,
				Subjects:  stream.subjects,
				Retention: nats.LimitsPolicy,
				Discard:   nats.DiscardNew,
				Storage:   nats.FileStorage,
				Metadata:  map[string]string{StreamIDMetadataKey: "stream-of-a-newer-build", generationKey: strconv.Itoa(streamConfigGeneration + 1)},
			}); err != nil {
				t.Fatalf("the newer build creates %s: %v", Stream, err)
			}
			if stream.another {
				if _, err := js.AddStream(&nats.StreamConfig{Name: anotherProgramsStream, Subjects: []string{agentSubjects}, Storage: nats.FileStorage}); err != nil {
					t.Fatalf("another program creates %s: %v", anotherProgramsStream, err)
				}
			}
			client, err := ConnectOwningStream([]string{uri})
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			captures := len(stream.subjects) == len(streamCarriedSubjects)
			for i, p := range storedPublishes() {
				subject := fmt.Sprintf("%sses_lacks_%d", contracts.AgentTopicPrefix, i)
				started := time.Now()
				err := p.publish(client, subject)
				elapsed := time.Since(started)
				held := storedIn(t, js, Stream, subject)
				elsewhere := uint64(0)
				if stream.another {
					elsewhere = storedIn(t, js, anotherProgramsStream, subject)
				}
				t.Logf("%s on %s: err=%v, %d in %s, %d in %s, after %s", p.name, subject, err, held, Stream, elsewhere, anotherProgramsStream, elapsed)
				if elsewhere != 0 {
					t.Errorf("%s on %s left %d copies in %s, which no session consumer reads, want none", p.name, subject, elsewhere, anotherProgramsStream)
				}
				if captures {
					if err != nil || held != 1 {
						t.Errorf("control: %s on %s = %v with %d copies, want nil and 1", p.name, subject, err, held)
					}
					continue
				}
				if !errors.Is(err, ErrStreamLacksSubject) || held != 0 {
					t.Errorf("%s on %s = %v with %d copies, want ErrStreamLacksSubject and none", p.name, subject, err, held)
					continue
				}
				if want := strconv.Quote(subject); !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "generation "+strconv.Itoa(streamConfigGeneration+1)) {
					t.Errorf("%s on %s = %q, want it to name %s and the stream's generation", p.name, subject, err, want)
				}
				if !stream.another && !errors.Is(err, p.unanswered) {
					t.Errorf("%s on %s = %v, which no longer says what its callers read of a send nothing answered (%v)", p.name, subject, err, p.unanswered)
				}
				if elapsed >= storedPublishWindow {
					t.Errorf("%s on %s was answered after %s, its whole wait of %s: the server's answer did not settle it", p.name, subject, elapsed, storedPublishWindow)
				}
			}
		})
	}
}

// A stored publish the notifications stream did not take is named ErrStreamLacksSubject, with its
// subject and the generation of the stream the client read, exactly when the subject is one this
// build stores, the server's answer says that stream did not take it - no stream answered the
// subject, or another stream refused a send that named the notifications stream - and the client's
// last read of the deployed stream carries no subject capturing it. Every other failure keeps its
// own error, so the listener answers it in its own words: a bus that did not answer, a full
// stream, a refusal the read cannot explain, a stream that went away after a read that still
// carried the subject, and an answer to a subject this build never stores, a role lane among
// them, which no newer build dropped.
func TestAStoredPublishTheDeployedStreamLeavesOutIsNamedWithItsSubjectAndGeneration(t *testing.T) {
	const (
		agent = "notifications.agent.ses_left_out"
		role  = "notifications.role.left-out"
	)
	if !StreamCaptures(agent) || StreamCaptures(role) {
		t.Fatalf("StreamCaptures(%s) = %t and StreamCaptures(%s) = %t, want true and false", agent, StreamCaptures(agent), role, StreamCaptures(role))
	}
	leavesOut := &deployedRead{subjects: []string{"notifications.github.>"}, generation: 7}
	carries := &deployedRead{subjects: []string{"notifications.agent.>"}, generation: 7}
	mismatch := &nats.APIError{Code: 400, ErrorCode: jsStreamNotMatch, Description: "expected stream does not match"}
	full := streamFull(&nats.APIError{Code: 503, ErrorCode: jsStreamStoreFailed, Description: maxBytesExceeded})
	for _, tc := range []struct {
		name     string
		subject  string
		deployed *deployedRead
		err      error
		lacks    bool
	}{
		{"no stream answered and the read leaves the subject out", agent, leavesOut, nats.ErrNoStreamResponse, true},
		{"nothing answered a window's send or a forward and the read leaves the subject out", agent, leavesOut, fmt.Errorf("bus: %w: %w", ErrNotAcknowledged, nats.ErrNoResponders), true},
		{"another stream refused and the read leaves the subject out", agent, leavesOut, mismatch, true},
		{"no stream answered and the read carries the subject", agent, carries, nats.ErrNoStreamResponse, false},
		{"nothing answered a window's send or a forward and the read carries the subject", agent, carries, nats.ErrNoResponders, false},
		{"no stream answered and the client has read no stream", agent, nil, nats.ErrNoStreamResponse, false},
		{"the bus did not answer", agent, leavesOut, context.DeadlineExceeded, false},
		{"the stream is full", agent, leavesOut, full, false},
		{"nothing answered a request to a role lane, which this build never stores", role, leavesOut, fmt.Errorf("bus: %w: %w", ErrNotAcknowledged, nats.ErrNoResponders), false},
		{"another stream refused a role lane, which this build never stores", role, leavesOut, mismatch, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{}
			if tc.deployed != nil {
				client.deployed.Store(tc.deployed)
			}
			got := client.lacksSubject(tc.subject, tc.err)
			if errors.Is(got, ErrStreamLacksSubject) != tc.lacks {
				t.Fatalf("lacksSubject(%v) = %v; ErrStreamLacksSubject %t, want %t", tc.err, got, !tc.lacks, tc.lacks)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("lacksSubject(%v) = %v, which no longer matches the server's answer", tc.err, got)
			}
			if tc.lacks && (!strings.Contains(got.Error(), strconv.Quote(tc.subject)) || !strings.Contains(got.Error(), "generation 7")) {
				t.Fatalf("lacksSubject(%v) = %q, want it to name %s and generation 7", tc.err, got, tc.subject)
			}
		})
	}
}

// storedIn is how many messages stream holds on subject.
func storedIn(t *testing.T, js nats.JetStreamContext, stream, subject string) uint64 {
	t.Helper()
	info, err := js.StreamInfo(stream, &nats.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("read what %s holds on %s: %v", stream, subject, err)
	}
	return info.State.Subjects[subject]
}
