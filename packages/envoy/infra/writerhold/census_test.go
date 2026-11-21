package writerhold

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// TestHoldWritersInsideTheServer shows the writer census (§3a step 2, plan E's reviewer, gate 2,
// red team F5), under exactly natsgrant.Operator(OperatorPhaseHeld): a republish rule onto
// $KV.envoy_session_consumers.K, set on another stream before the hold, stops the hold at the
// census naming that stream; so do a republish onto a subject of ENVOY_NOTIFICATIONS as its listed
// configuration holds them, a push consumer whose deliver_subject is
// $KV.envoy_session_consumers.x, and a stream that sources into KV_envoy_sessions. Control: a
// republish onto an unrelated literal subject passes, and without the check the census passes and
// the republish writes K's record during the hold. The row fails if the census stops reading
// republish destinations, push consumers' deliver subjects or a protected stream's sources.
func TestHoldWritersInsideTheServer(t *testing.T) {
	ctx := t.Context()
	recordK := "$KV." + bus.SessionConsumersBucket + ".K"
	hub := startGrantHub(t)
	_, js, legacy := hub.admin(t)
	seedProtected(t, legacy)
	held := hub.operator(t, natsgrant.OperatorPhaseHeld)
	audit := func(republish string) {
		t.Helper()
		_ = js.DeleteStream(ctx, "AUDIT")
		config := jetstream.StreamConfig{Name: "AUDIT", Subjects: []string{"audit.>"}, Storage: jetstream.MemoryStorage}
		if republish != "" {
			config.RePublish = &jetstream.RePublish{Source: "audit.k", Destination: republish}
		}
		_, err := js.CreateStream(ctx, config)
		require.NoError(t, err, "create AUDIT republishing to %q", republish)
	}

	// Control: a republish onto an unrelated literal subject passes, and the census read it.
	audit("audit-copy.k")
	census, err := TakeCensus(ctx, held, ProtectedStreams())
	require.NoError(t, err)
	listed := false
	for _, info := range census.Streams {
		listed = listed || (info.Config.Name == "AUDIT" && info.Config.RePublish != nil && info.Config.RePublish.Destination == "audit-copy.k")
	}
	require.True(t, listed, "the census lists AUDIT with its republish")

	// The republish onto K's record stops the hold, naming the stream.
	audit(recordK)
	_, err = TakeCensus(ctx, held, ProtectedStreams())
	requireRefusal(t, err, StepCensus, "AUDIT", recordK, "$KV.>")
	t.Logf("with the check: %v", err)

	// Control: without the check the census passes, and the republish writes K's record during
	// the hold. The message AUDIT stores is the admin's publish here, standing for one the server
	// stores with no client (a source's or a schedule's); the republish puts it on
	// $KV.envoy_session_consumers.K, which the selection bucket stores as K's record.
	_, err = takeCensus(ctx, held, StepCensus, ProtectedStreams(), streamChecks{noDestinations: true})
	require.NoError(t, err)
	_, err = js.Publish(ctx, "audit.k", []byte("republished"))
	require.NoError(t, err)
	selection, err := legacy.KeyValue(bus.SessionConsumersBucket)
	require.NoError(t, err)
	testwait.Eventually(t, "the republish to write K's record into KV_"+bus.SessionConsumersBucket, func() bool {
		entry, err := selection.Get("K")
		return err == nil && string(entry.Value()) == "republished"
	})
	t.Logf("control, the census without the destination check passed, and the republish wrote K's record %q into KV_%s", "republished", bus.SessionConsumersBucket)

	// A republish onto a subject of ENVOY_NOTIFICATIONS, as its listed configuration holds them.
	audit("notifications.agent.x")
	_, err = TakeCensus(ctx, held, ProtectedStreams())
	requireRefusal(t, err, StepCensus, "AUDIT", "notifications.agent.x", "notifications.agent.>")
	t.Logf("with the check: %v", err)

	// A push consumer whose deliver subject is a key of the selection bucket.
	audit("")
	_, err = legacy.AddConsumer("AUDIT", &nats.ConsumerConfig{Durable: "relay", DeliverSubject: "$KV." + bus.SessionConsumersBucket + ".x", AckPolicy: nats.AckNonePolicy})
	require.NoError(t, err)
	_, err = TakeCensus(ctx, held, ProtectedStreams())
	requireRefusal(t, err, StepCensus, "push consumer relay of stream AUDIT", "$KV."+bus.SessionConsumersBucket+".x")
	t.Logf("with the check: %v", err)
	require.NoError(t, js.DeleteConsumer(ctx, "AUDIT", "relay"))

	// A stream that sources into KV_envoy_sessions.
	sessions := kvStreamPrefix + bus.SessionBucket
	updateStream(t, js, sessions, func(config *jetstream.StreamConfig) {
		config.Sources = []*jetstream.StreamSource{{Name: "AUDIT"}}
	})
	_, err = TakeCensus(ctx, held, ProtectedStreams())
	requireRefusal(t, err, StepCensus, sessions, "sources from AUDIT")
	t.Logf("with the check: %v", err)
}

// TestHoldASubstitutionIsNotLiteral shows the literal-destination check (§3a step 2, red team F5),
// under exactly natsgrant.Operator(OperatorPhaseHeld): a republish from in.*.*.* to $1.$2.$3, and
// the same with {{wildcard(n)}}, stops the hold as not literal, since its output is built from the
// source subject's tokens. Control: a check of the destination as it is spelled passes it (no
// token of $1.$2.$3 is $KV), and a retained message in.$KV.envoy_sessions.K is republished onto
// $KV.envoy_sessions.K, K's record in KV_envoy_sessions, during the hold. The row fails if the
// census judges a destination by its spelling alone.
func TestHoldASubstitutionIsNotLiteral(t *testing.T) {
	ctx := t.Context()
	hub := startGrantHub(t)
	_, js, legacy := hub.admin(t)
	seedProtected(t, legacy)
	held := hub.operator(t, natsgrant.OperatorPhaseHeld)
	in := func(dest string) {
		t.Helper()
		_ = js.DeleteStream(ctx, "IN")
		_, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "IN", Subjects: []string{"in.>"}, Storage: jetstream.MemoryStorage,
			RePublish: &jetstream.RePublish{Source: "in.*.*.*", Destination: dest}})
		require.NoError(t, err, "create IN republishing to %q", dest)
	}

	for _, dest := range []string{"{{wildcard(1)}}.{{wildcard(2)}}.{{wildcard(3)}}", "$1.$2.$3"} {
		in(dest)
		_, err := TakeCensus(ctx, held, ProtectedStreams())
		requireRefusal(t, err, StepCensus, "IN", dest, "not a literal subject")
		t.Logf("with the check: %v", err)
	}

	// Control: the destination's spelling overlaps no protected subject, so a check of the
	// spelling passes it, and the republish writes K's record into KV_envoy_sessions.
	_, err := takeCensus(ctx, held, StepCensus, ProtectedStreams(), streamChecks{rawSpelling: true})
	require.NoError(t, err)
	retained := "in.$KV." + bus.SessionBucket + ".K"
	_, err = js.Publish(ctx, retained, []byte("substituted"))
	require.NoError(t, err)
	stream, err := js.Stream(ctx, "IN")
	require.NoError(t, err)
	_, err = stream.GetLastMsgForSubject(ctx, retained)
	require.NoError(t, err, "IN retains %s", retained)
	sessions, err := legacy.KeyValue(bus.SessionBucket)
	require.NoError(t, err)
	testwait.Eventually(t, "the republish to write K's record into KV_"+bus.SessionBucket, func() bool {
		entry, err := sessions.Get("K")
		return err == nil && string(entry.Value()) == "substituted"
	})
	t.Logf("control, the census checking the spelling passed, and the republish of %s wrote K's record %q into KV_%s", retained, "substituted", bus.SessionBucket)
}

// listResponder answers the census's three list requests, STREAM.LIST, STREAM.NAMES and
// CONSUMER.LIST.<stream>, on a server with no JetStream, from the entries the row sets, limit
// entries a page, and edit changes each page before it is sent: nats-server cannot be made to
// answer `missing` on one server, so a crafted page stands for clustered CONSUMER.LIST's.
type listResponder struct {
	mu        sync.Mutex
	streams   []jetstream.StreamInfo
	consumers map[string][]jetstream.ConsumerInfo
	limit     int
	edit      func(subject string, offset int, page map[string]any)
	asked     []string
}

func newListResponder(t *testing.T, url string) *listResponder {
	t.Helper()
	responder := &listResponder{limit: 1, consumers: map[string][]jetstream.ConsumerInfo{}}
	conn, err := testnats.ConnectSeed(t, url, "")
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	for _, subject := range []string{natsserver.JSApiStreamList, natsserver.JSApiStreams, natsserver.JSApiConsumerList} {
		_, err := conn.Subscribe(subject, responder.answer)
		require.NoError(t, err)
	}
	require.NoError(t, conn.Flush())
	return responder
}

func (r *listResponder) answer(msg *nats.Msg) {
	var request pageRequest
	if len(msg.Data) > 0 {
		if err := json.Unmarshal(msg.Data, &request); err != nil {
			panic(err)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, fmt.Sprintf("%s@%d", msg.Subject, request.Offset))
	var entries []any
	key := "streams"
	switch {
	case msg.Subject == natsserver.JSApiStreamList:
		for _, info := range r.streams {
			entries = append(entries, info)
		}
	case msg.Subject == natsserver.JSApiStreams:
		for _, info := range r.streams {
			entries = append(entries, info.Config.Name)
		}
	default:
		key = "consumers"
		for _, info := range r.consumers[strings.TrimPrefix(msg.Subject, fmt.Sprintf(natsserver.JSApiConsumerListT, ""))] {
			entries = append(entries, info)
		}
	}
	end := min(request.Offset+r.limit, len(entries))
	page := map[string]any{"total": len(entries), "offset": request.Offset, "limit": r.limit, key: entries[min(request.Offset, end):end]}
	if r.edit != nil {
		r.edit(msg.Subject, request.Offset, page)
	}
	body, err := json.Marshal(page)
	if err != nil {
		panic(err)
	}
	_ = msg.Respond(body)
}

// set replaces the responder's edit.
func (r *listResponder) set(edit func(subject string, offset int, page map[string]any)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.edit = edit
	r.asked = nil
}

// TestHoldIncompleteLists shows the census's complete-answer rule (§3a step 2, t-e-58, A22): a
// CONSUMER.LIST answer with a non-empty `missing`, and a page that adds no entry before
// offset+len reaches total, each stop the hold as UNKNOWN, as do a STREAM.LIST with `missing` or
// `offline`, a STREAM.LIST page that adds no entry, a total that changes between pages and a
// STREAM.NAMES that lacks a stream of STREAM.LIST, and so is a complete listing that lacks a
// stream the caller's STREAM.INFO found present. nats-server cannot be made to answer `missing`,
// so a responder on a server with no JetStream answers crafted pages, one entry a page, to the
// census's own connection. Control: a complete answer passes, every page asked for; the push
// consumer the incomplete answers leave out delivers onto $KV.envoy_sessions.x, which the complete
// answer shows and refuses; and a census that takes the incomplete answers as whole passes with
// that writer unseen. The row fails if the census takes a partial list as the whole.
func TestHoldIncompleteLists(t *testing.T) {
	ctx := t.Context()
	_, url := testnats.StartConfig(t, "", nil, true)
	lists := newListResponder(t, url)
	census, err := testnats.ConnectSeed(t, url, "", nats.CustomInboxPrefix(natsgrant.OperatorInbox))
	require.NoError(t, err)
	t.Cleanup(census.Close)
	sessions := kvStreamPrefix + bus.SessionBucket
	lists.mu.Lock()
	lists.streams = []jetstream.StreamInfo{
		{Config: jetstream.StreamConfig{Name: "WORK", Subjects: []string{"work.>"}}},
		{Config: jetstream.StreamConfig{Name: sessions, Subjects: []string{"$KV." + bus.SessionBucket + ".>"}}},
	}
	lists.consumers["WORK"] = []jetstream.ConsumerInfo{
		{Name: "pull", Stream: "WORK", Config: jetstream.ConsumerConfig{Durable: "pull"}},
		{Name: "push", Stream: "WORK", Config: jetstream.ConsumerConfig{Durable: "push", DeliverSubject: "work-out.x"}},
	}
	lists.mu.Unlock()

	// Control: the complete answer passes, and the census asked for every page.
	listing, err := TakeCensus(ctx, census, nil)
	require.NoError(t, err)
	require.Len(t, listing.Streams, 2)
	require.Len(t, listing.Consumers["WORK"], 2)
	lists.mu.Lock()
	asked := lists.asked
	lists.mu.Unlock()
	require.Contains(t, asked, natsserver.JSApiStreamList+"@1")
	require.Contains(t, asked, fmt.Sprintf(natsserver.JSApiConsumerListT, "WORK")+"@1")

	// A complete answer that lacks a stream the caller's own STREAM.INFO read found present is
	// UNKNOWN: a listing blind to it cannot read as clean.
	dpop := kvStreamPrefix + bus.DPoPProofBucket
	_, err = TakeCensus(ctx, census, []string{sessions, dpop})
	requireUnknown(t, err, StepCensus, "STREAM.LIST does not hold "+dpop)

	// The second consumer is a writer: its complete answer stops the hold naming it.
	lists.mu.Lock()
	lists.consumers["WORK"][1].Config.DeliverSubject = "$KV." + bus.SessionBucket + ".x"
	lists.mu.Unlock()
	_, err = TakeCensus(ctx, census, nil)
	requireRefusal(t, err, StepCensus, "push consumer push of stream WORK", "$KV."+bus.SessionBucket+".x")
	t.Logf("the complete answer shows the writer: %v", err)

	consumerList := fmt.Sprintf(natsserver.JSApiConsumerListT, "WORK")
	incomplete := []struct {
		name string
		edit func(subject string, offset int, page map[string]any)
		want []string
		// hidesWriter is set for the answers that leave the push consumer out.
		hidesWriter bool
	}{
		{
			name: "a CONSUMER.LIST answer with missing",
			edit: func(subject string, offset int, page map[string]any) {
				if subject == consumerList {
					page["total"] = 1
					page["missing"] = []string{"push"}
				}
			},
			want:        []string{consumerList, "missing [push]"},
			hidesWriter: true,
		},
		{
			name: "a CONSUMER.LIST page that adds no entry",
			edit: func(subject string, offset int, page map[string]any) {
				if subject == consumerList && offset == 1 {
					page["consumers"] = []any{}
				}
			},
			want:        []string{consumerList, "added no entry", "1 of a total of 2"},
			hidesWriter: true,
		},
		{
			name: "a STREAM.LIST answer with missing",
			edit: func(subject string, offset int, page map[string]any) {
				if subject == natsserver.JSApiStreamList {
					page["missing"] = []string{sessions}
				}
			},
			want: []string{natsserver.JSApiStreamList, "missing [" + sessions + "]"},
		},
		{
			name: "a STREAM.LIST answer with offline",
			edit: func(subject string, offset int, page map[string]any) {
				if subject == natsserver.JSApiStreamList {
					page["offline"] = map[string]string{sessions: "unsupported"}
				}
			},
			want: []string{natsserver.JSApiStreamList, "offline"},
		},
		{
			name: "a STREAM.LIST page that adds no entry",
			edit: func(subject string, offset int, page map[string]any) {
				if subject == natsserver.JSApiStreamList && offset == 1 {
					page["streams"] = []any{}
				}
			},
			want: []string{natsserver.JSApiStreamList, "added no entry"},
		},
		{
			name: "a total that changes between pages",
			edit: func(subject string, offset int, page map[string]any) {
				if subject == natsserver.JSApiStreamList && offset == 1 {
					page["total"] = 3
				}
			},
			want: []string{natsserver.JSApiStreamList, "total changed from 2 to 3"},
		},
		{
			name: "a STREAM.NAMES answer that lacks a stream of STREAM.LIST",
			edit: func(subject string, offset int, page map[string]any) {
				if subject == natsserver.JSApiStreams {
					page["total"] = 1
				}
			},
			want: []string{"STREAM.LIST and STREAM.NAMES do not hold the same streams", sessions},
		},
	}
	for _, answer := range incomplete {
		lists.set(answer.edit)
		_, incompleteErr := TakeCensus(ctx, census, nil)
		requireUnknown(t, incompleteErr, StepCensus, answer.want...)
		t.Logf("%s: %v", answer.name, incompleteErr)
		if answer.hidesWriter {
			// Control: a census that takes the answer as the whole list passes, with the writer
			// the complete answer refused left unseen.
			listing, err := takeCensus(ctx, census, StepCensus, nil, streamChecks{acceptIncomplete: true})
			require.NoError(t, err, "%s, taken as whole", answer.name)
			require.Len(t, listing.Consumers["WORK"], 1, "%s, taken as whole", answer.name)
			t.Logf("control, %s taken as whole: the census passed, listing %d consumer of WORK, the writer push unseen", answer.name, len(listing.Consumers["WORK"]))
		}
	}
}
