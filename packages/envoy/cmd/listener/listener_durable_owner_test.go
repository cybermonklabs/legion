package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/natsgrant"
)

// The listener's durable record takes a consumer only when that consumer's own metadata names the
// durable: a record naming a <durable>~<random> consumer whose envoy.logical is another's (made by a
// principal that can create consumers and write the bucket) is refused as a malformed record by the
// bind and by every reader of the durable, and the consumer is neither corrected to the listener's
// policy nor bound. The control is the same record naming a consumer whose metadata names the
// durable, which the bind corrects and binds.
func TestADurableRecordNamingAnotherOwnersConsumerIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		logical func(r *durableRig) string
		served  bool
	}{
		{name: "a consumer of another logical name", logical: func(*durableRig) string { return "ses_someone_else" }},
		{name: "control: the durable's own consumer", logical: func(r *durableRig) string { return r.durable }, served: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDurableRig(t, consumernames.ModeFresh)
			ctx := context.Background()
			js, err := r.conn.JetStream()
			if err != nil {
				t.Fatal(err)
			}
			name := r.durable + "~FORGEDBYAPRINCIPAL0000"
			if _, err := js.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
				Durable:        name,
				DeliverSubject: natsgo.NewInbox(),
				AckPolicy:      natsgo.AckExplicitPolicy,
				Metadata:       map[string]string{consumernames.LogicalMetadataKey: tc.logical(r)},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := r.client.SelectionStore().Write(ctx, consumernames.DurableKey(r.durable), consumernames.Record{Current: name, Start: 1}, 0); err != nil {
				t.Fatal(err)
			}
			sub, err := startListenerSubscription(r.client, r.durable, func(msg *natsgo.Msg) { _ = msg.Ack() })
			if sub != nil {
				t.Cleanup(func() { _ = sub.Unsubscribe() })
			}
			info, infoErr := js.ConsumerInfo(bus.Stream, name)
			if infoErr != nil {
				t.Fatal(infoErr)
			}
			if tc.served {
				if err != nil || info.Config.MaxAckPending != consumerMaxAckPending {
					t.Fatalf("control: the bind = %v, max ack pending %d; want it bound and corrected to %d", err, info.Config.MaxAckPending, consumerMaxAckPending)
				}
				return
			}
			if !errors.Is(err, consumernames.ErrMalformedRecord) {
				t.Fatalf("the bind over another's consumer = %v, want ErrMalformedRecord", err)
			}
			if info.Config.MaxAckPending == consumerMaxAckPending || info.PushBound {
				t.Fatalf("the refused bind touched %s: max ack pending %d, push bound %t", name, info.Config.MaxAckPending, info.PushBound)
			}
			if _, err := durableInfo(ctx, r.client, r.durable); !errors.Is(err, consumernames.ErrMalformedRecord) {
				t.Fatalf("durableInfo over another's consumer = %v, want ErrMalformedRecord", err)
			}
		})
	}
}

// A candidate an open create of the listener's durable names that someone else made, with the
// durable's own metadata and start, is adopted only as the listener creates it. One with a setting
// the listener's create never gives and no update changes (replay at the original rate, a deliver
// subject that is no reply inbox, no deliver subject at all) is the listener's refusal of the
// durable (errListenerDurableRefused), an ErrUnexpectedConsumer naming it and that setting as the
// server names it, and is neither published nor bound. An earlier adoption checked only push, the
// heartbeat and the ack policy, so it bound the first two, and refused the pull one with an error
// the bind retried until its deadline. The control is the candidate as the listener makes it,
// adopted and bound. A setting an update changes is corrected instead, and a pause is refused too
// (TestAPrecreatedDurableCandidateThatWouldNotDeliverIsCorrectedOrRefused).
func TestAPrecreatedDurableCandidateIsAdoptedOnlyAsTheListenerCreatesIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		made    func(*natsgo.ConsumerConfig)
		setting string
	}{
		{name: "replay at the original rate", made: func(c *natsgo.ConsumerConfig) { c.ReplayPolicy = natsgo.ReplayOriginalPolicy }, setting: `replay_policy "original", not "instant"`},
		{name: "a deliver subject that is no reply inbox", made: func(c *natsgo.ConsumerConfig) { c.DeliverSubject = "elsewhere.deliver" }, setting: `deliver_subject "elsewhere.deliver", not a reply inbox`},
		{name: "no deliver subject", made: func(c *natsgo.ConsumerConfig) { c.DeliverSubject = "" }, setting: "deliver_subject none, not a reply inbox"},
		{name: "control: as the listener creates it", made: func(*natsgo.ConsumerConfig) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDurableRig(t, consumernames.ModeFresh)
			ctx := context.Background()
			js, err := r.conn.JetStream()
			if err != nil {
				t.Fatal(err)
			}
			name := r.durable + "~PRECREATEDBYAPRINCIPAL"
			made := listenerDurableConfig(r.client, name, bus.StreamSubjects())
			made.DeliverPolicy, made.OptStartSeq = natsgo.DeliverByStartSequencePolicy, 1
			made.Metadata = map[string]string{consumernames.LogicalMetadataKey: r.durable}
			tc.made(&made)
			if _, err := js.AddConsumer(bus.Stream, &made); err != nil {
				t.Fatal(err)
			}
			op := &consumernames.Op{Kind: consumernames.OpCreate, Owner: "another process", Candidate: name, ReservedAt: time.Now().Add(-time.Minute)}
			if _, err := r.client.SelectionStore().Write(ctx, consumernames.DurableKey(r.durable), consumernames.Record{Start: 1, Op: op}, 0); err != nil {
				t.Fatal(err)
			}
			sub, err := startListenerSubscription(r.client, r.durable, func(msg *natsgo.Msg) { _ = msg.Ack() })
			if sub != nil {
				t.Cleanup(func() { _ = sub.Unsubscribe() })
			}
			if tc.setting == "" {
				if err != nil || r.current(t) != name {
					t.Fatalf("control: the bind = %v, current %q; want %s adopted and bound", err, r.current(t), name)
				}
				return
			}
			if !errors.Is(err, errListenerDurableRefused) || !errors.Is(err, consumernames.ErrUnexpectedConsumer) ||
				!strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), tc.setting) {
				t.Fatalf("the bind over %s = %v; want the listener's refusal, ErrUnexpectedConsumer naming it and %s", name, err, tc.setting)
			}
			if current := r.current(t); current != "" {
				t.Fatalf("the refused candidate was published: current %q", current)
			}
			if info, err := js.ConsumerInfo(bus.Stream, name); err != nil || info.PushBound {
				t.Fatalf("the refused candidate = %+v, %v; want it held and bound to no one", info, err)
			}
		})
	}
}

// An edge listener's durable delivers to its connection's own inbox, under its machine's prefix
// (natsgrant.EdgeInboxPrefix, edgeGrant.connectOptions). So a candidate of its durable that an open
// create names, made exactly as its create makes it (durableCreateConfig on its client, at the
// record's start), is adopted by the fresh mode's create (ensureFreshDurable): the candidate a
// crash between the create and the publish leaves, or a create whose answer was lost. A candidate
// delivering under another prefix, nats.go's default, under which a shared listener's durable
// delivers, or another edge machine's, is the listener's refusal of the durable
// (errListenerDurableRefused), an ErrUnexpectedConsumer naming its deliver subject, and is not
// published. Before, the adoption took only an _INBOX. deliver subject, so the edge listener
// refused its own candidate and exited at every start until an operator replaced the durable. The
// control is a shared listener, with nats.go's default prefix, adopting its own candidate.
func TestAnEdgeListenerAdoptsItsOwnDurableCandidateOnly(t *testing.T) {
	edge := &edgeGrant{inboxPrefix: natsgrant.EdgeInboxPrefix("m")}
	for _, tc := range []struct {
		name    string
		edge    *edgeGrant
		deliver string
		adopted bool
	}{
		{name: "the edge listener's own candidate", edge: edge, adopted: true},
		{name: "control: a shared listener's own candidate", adopted: true},
		{name: "a shared listener's candidate, at the edge", edge: edge, deliver: "_INBOX.PRECREATED"},
		{name: "another edge machine's candidate, at the edge", edge: edge, deliver: natsgrant.EdgeInboxPrefix("n") + ".PRECREATED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDurableRig(t, consumernames.ModeFresh, tc.edge.connectOptions()...)
			ctx := context.Background()
			js, err := r.conn.JetStream()
			if err != nil {
				t.Fatal(err)
			}
			name := r.durable + "~PRECREATEDBYTHELISTENER"
			made := durableCreateConfig(r.client, name, r.durable, bus.StreamSubjects())
			if tc.edge != nil && !strings.HasPrefix(made.DeliverSubject, tc.edge.inboxPrefix+".") {
				t.Fatalf("setup: the edge listener's durable delivers to %s, not under %s", made.DeliverSubject, tc.edge.inboxPrefix)
			}
			if tc.deliver != "" {
				made.DeliverSubject = tc.deliver
			}
			made.DeliverPolicy, made.OptStartSeq = natsgo.DeliverByStartSequencePolicy, 1
			if _, err := js.AddConsumer(bus.Stream, &made); err != nil {
				t.Fatal(err)
			}
			op := &consumernames.Op{Kind: consumernames.OpCreate, Owner: "the listener before its crash", Candidate: name, ReservedAt: time.Now().Add(-time.Minute)}
			if _, err := r.client.SelectionStore().Write(ctx, consumernames.DurableKey(r.durable), consumernames.Record{Start: 1, Op: op}, 0); err != nil {
				t.Fatal(err)
			}
			current, err := ensureFreshDurable(ctx, r.client, r.durable, bus.StreamSubjects())
			if tc.adopted {
				if err != nil || current != name || r.current(t) != name {
					t.Fatalf("ensureFreshDurable over its own candidate delivering to %s = %q, %v, record current %q; want %s adopted", made.DeliverSubject, current, err, r.current(t), name)
				}
				return
			}
			if !errors.Is(err, errListenerDurableRefused) || !errors.Is(err, consumernames.ErrUnexpectedConsumer) ||
				!strings.Contains(err.Error(), `deliver_subject "`+made.DeliverSubject+`"`) {
				t.Fatalf("ensureFreshDurable over %s delivering to %s = %q, %v; want the listener's refusal, ErrUnexpectedConsumer naming the deliver subject", name, made.DeliverSubject, current, err)
			}
			if current := r.current(t); current != "" {
				t.Fatalf("the refused candidate was published: current %q", current)
			}
		})
	}
}

// A candidate of the listener's durable an open create names that someone else made, with the
// durable's own metadata and start, and a setting the listener's create never gives that would
// keep the listener from receiving it, is corrected or refused before the bind. Headers only,
// which an update changes, is corrected to the whole configuration the durable's create gives, so
// the listener then receives a message whole. A pause a day ahead, which nats-server keeps across
// every update, is the listener's refusal of the durable (errListenerDurableRefused), an
// ErrUnexpectedConsumer naming pause_until, and is neither published nor bound. Before, the
// correction applied the listener's policy to the candidate's own configuration, which keeps both:
// the paused durable was bound and delivered nothing, and the headers-only one was refused. The
// control is that correction (correctListenerDurable) applied to each such candidate and bound,
// over which the listener receives nothing whole.
func TestAPrecreatedDurableCandidateThatWouldNotDeliverIsCorrectedOrRefused(t *testing.T) {
	dayAhead := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339Nano)
	for _, tc := range []struct {
		name    string
		setting map[string]any
		refused string
	}{
		{name: "paused a day ahead", setting: map[string]any{"pause_until": dayAhead}, refused: "pause_until"},
		{name: "headers only", setting: map[string]any{"headers_only": true}},
	} {
		for _, control := range []bool{false, true} {
			name := tc.name
			if control {
				name = "control: today's correction of one " + tc.name
			}
			t.Run(name, func(t *testing.T) {
				r := newDurableRig(t, consumernames.ModeFresh)
				ctx := context.Background()
				js, err := r.conn.JetStream()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := js.Publish("notifications.team.durable-candidate", []byte("the payload")); err != nil {
					t.Fatal(err)
				}
				candidate := r.durable + "~PRECREATEDUPDATABLE"
				made := listenerDurableConfig(r.client, candidate, bus.StreamSubjects())
				made.DeliverPolicy, made.OptStartSeq = natsgo.DeliverByStartSequencePolicy, 1
				made.Metadata = map[string]string{consumernames.LogicalMetadataKey: r.durable}
				createRawConsumer(t, r.conn, made, tc.setting)
				received := make(chan *natsgo.Msg, 16)
				handler := func(msg *natsgo.Msg) {
					received <- msg
					_ = msg.Ack()
				}
				var sub *natsgo.Subscription
				if control {
					info, err := js.ConsumerInfo(bus.Stream, candidate)
					if err != nil {
						t.Fatal(err)
					}
					if err := correctListenerDurable(r.client, info, bus.StreamSubjects()); err != nil {
						t.Fatal(err)
					}
					sub, err = r.client.Subscribe("", handler, natsgo.Bind(bus.Stream, candidate), natsgo.ManualAck())
				} else {
					op := &consumernames.Op{Kind: consumernames.OpCreate, Owner: "another process", Candidate: candidate, ReservedAt: time.Now().Add(-time.Minute)}
					if _, err := r.client.SelectionStore().Write(ctx, consumernames.DurableKey(r.durable), consumernames.Record{Start: 1, Op: op}, 0); err != nil {
						t.Fatal(err)
					}
					sub, err = startListenerSubscription(r.client, r.durable, handler)
					if tc.refused != "" {
						if !errors.Is(err, errListenerDurableRefused) || !errors.Is(err, consumernames.ErrUnexpectedConsumer) ||
							!strings.Contains(err.Error(), candidate) || !strings.Contains(err.Error(), tc.refused) {
							t.Fatalf("the bind over a candidate %s = %v; want the listener's refusal naming it and %s", tc.name, err, tc.refused)
						}
						if current := r.current(t); current != "" {
							t.Fatalf("the refused candidate was published: current %q", current)
						}
						return
					}
					if err == nil && r.current(t) != candidate {
						err = errors.New("the candidate was not published")
					}
				}
				if err != nil {
					t.Fatalf("bind %s: %v", candidate, err)
				}
				t.Cleanup(func() { _ = sub.Unsubscribe() })
				var whole bool
				select {
				case msg := <-received:
					whole = string(msg.Data) == "the payload"
				case <-time.After(3 * time.Second):
				}
				if control {
					if whole {
						t.Fatalf("control: today's correction of a candidate %s delivered it whole", tc.name)
					}
					return
				}
				if !whole {
					t.Fatalf("the listener bound over the adopted candidate %s received nothing whole within 3 s", candidate)
				}
				info, err := js.ConsumerInfo(bus.Stream, candidate)
				if err != nil || info.Config.HeadersOnly {
					t.Fatalf("the adopted candidate after the bind = %+v, %v; want it whole", info, err)
				}
			})
		}
	}
}

// createRawConsumer creates config on the stream with setting's fields added to its JSON, for a
// setting nats.go's nats package does not type (pause_until), as a principal holding the create
// can.
func createRawConsumer(t *testing.T, conn *natsgo.Conn, config natsgo.ConsumerConfig, setting map[string]any) {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{}
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	maps.Copy(fields, setting)
	request, err := json.Marshal(map[string]any{"stream_name": bus.Stream, "config": fields, "action": "create"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := conn.Request("$JS.API.CONSUMER.CREATE."+bus.Stream+"."+config.Durable, request, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var answer struct {
		Config map[string]any `json:"config"`
		Error  *struct {
			Description string `json:"description"`
		} `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &answer); err != nil || answer.Error != nil {
		t.Fatalf("create %s with %v: %s, %v", config.Durable, setting, reply.Data, err)
	}
	for key := range setting {
		if _, ok := answer.Config[key]; !ok {
			t.Fatalf("control: the server made %s without %s: %s", config.Durable, key, reply.Data)
		}
	}
}

// The listener's own durable, as the server stored it from the listener's create, needs no
// correction: the shared correction compares each setting after the server's own defaulting
// (bus.CorrectedConfig), and the listener's create sets every setting the server defaults
// (ack wait, max ack pending, max deliver, inactive threshold; a push consumer has no max waiting),
// so no bind of it sends an update. The control is the same candidate stored with max deliver 5,
// which the correction brings back to the listener's 20.
func TestTheListenersStoredDurableNeedsNoCorrection(t *testing.T) {
	r := newDurableRig(t, consumernames.ModeFresh)
	ctx := context.Background()
	for _, c := range []struct {
		name, suffix string
		setting      map[string]any
		changed      bool
	}{
		{name: "as the listener creates it", suffix: "AS"},
		{name: "control: stored with max deliver 5", suffix: "CONTROL", setting: map[string]any{"max_deliver": 5}, changed: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			candidate := r.durable + "~STORED" + c.suffix
			made := durableCreateConfig(r.client, candidate, r.durable, bus.StreamSubjects())
			made.DeliverPolicy, made.OptStartSeq = natsgo.DeliverByStartSequencePolicy, 1
			createRawConsumer(t, r.conn, made, c.setting)
			info, err := r.client.ConsumerInfo(ctx, candidate)
			if err != nil {
				t.Fatal(err)
			}
			create, err := jetstreamConfig(made)
			if err != nil {
				t.Fatal(err)
			}
			next, changed := bus.CorrectedConfig(info.Config, create)
			if changed != c.changed {
				t.Fatalf("the correction of %s as the server stored it = changed %t, want %t:\nstored %+v\nnext   %+v", candidate, changed, c.changed, info.Config, next)
			}
			if c.changed && next.MaxDeliver != 20 {
				t.Fatalf("the correction writes max deliver %d, want the listener's 20", next.MaxDeliver)
			}
		})
	}
}
