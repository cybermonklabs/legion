package writerhold

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// grantHub is a JetStream server whose one account, $G, holds the admin, its no_auth_user, which
// sets each fixture up and plays every other client, and one static nkey user per operator phase,
// each holding exactly natsgrant.Operator(phase)'s lists, so a step run as that user runs under
// exactly that phase's grant. It holds one more user, oldReleased: released's grant as it stood
// before #4679 moved every snapshot to held, the snapshot of KV_envoy_sessions and its chunks'
// acknowledgements added, the control of the residual the move closed.
type grantHub struct {
	server      *testnats.Server
	url         string
	seeds       map[natsgrant.OperatorPhase]string
	oldReleased string
}

// oldReleasedGrant is natsgrant.Operator(OperatorPhaseReleased) with the snapshot of
// KV_envoy_sessions released held before #4679's reads-only grant (nats-server jetstream_api.go's
// STREAM.SNAPSHOT and its $JS.SNAPSHOT.ACK chunk acknowledgements).
func oldReleasedGrant() natsgrant.Permissions {
	grant := natsgrant.Operator(natsgrant.OperatorPhaseReleased)
	sessions := kvStreamPrefix + bus.SessionBucket
	grant.Publish = append(append([]string{}, grant.Publish...), fmt.Sprintf(natsserver.JSApiStreamSnapshotT, sessions), "$JS.SNAPSHOT.ACK."+sessions+".>")
	return grant
}

// startGrantHub starts a grantHub, shut down when t ends.
func startGrantHub(t *testing.T) *grantHub {
	t.Helper()
	hub := &grantHub{seeds: map[natsgrant.OperatorPhase]string{}}
	var users []string
	for _, phase := range []natsgrant.OperatorPhase{natsgrant.OperatorPhaseReleased, natsgrant.OperatorPhasePrepare, natsgrant.OperatorPhaseHeld} {
		seed, public := testnats.User(t)
		hub.seeds[phase] = seed
		users = append(users, fmt.Sprintf("{ nkey: %q, permissions: %s }", public, permissionsConfig(natsgrant.Operator(phase))))
	}
	seed, public := testnats.User(t)
	hub.oldReleased = seed
	users = append(users, fmt.Sprintf("{ nkey: %q, permissions: %s }", public, permissionsConfig(oldReleasedGrant())))
	_, admin := testnats.User(t)
	users = append(users, fmt.Sprintf("{ nkey: %q }", admin))
	config := fmt.Sprintf("jetstream {}\nauthorization {\n  users = [\n    %s\n  ]\n}\nno_auth_user: %q\n", strings.Join(users, ",\n    "), admin)
	hub.server, hub.url = testnats.StartConfig(t, config, []string{"-js"}, true)
	return hub
}

// oldReleasedOperator connects as the oldReleased user, as operator connects a phase's user.
func (h *grantHub) oldReleasedOperator(t *testing.T, opts ...nats.Option) *nats.Conn {
	t.Helper()
	conn, err := testnats.ConnectSeed(t, h.url, h.oldReleased, append([]nats.Option{nats.CustomInboxPrefix(natsgrant.OperatorInbox)}, opts...)...)
	require.NoError(t, err, "connect as the old released grant")
	t.Cleanup(conn.Close)
	return conn
}

// permissionsConfig is grant as nats-server's configuration writes a user's permissions.
func permissionsConfig(grant natsgrant.Permissions) string {
	direction := func(allow, deny []string) string {
		parts := []string{"allow: [" + quoted(allow) + "]"}
		if len(deny) > 0 {
			parts = append(parts, "deny: ["+quoted(deny)+"]")
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	}
	return fmt.Sprintf("{ publish: %s, subscribe: %s }", direction(grant.Publish, grant.PublishDeny), direction(grant.Subscribe, grant.SubscribeDeny))
}

func quoted(subjects []string) string {
	out := make([]string, len(subjects))
	for i, subject := range subjects {
		out[i] = fmt.Sprintf("%q", subject)
	}
	return strings.Join(out, ", ")
}

// operator connects as phase's operator user, with the operator's inbox prefix, as every operator
// connection is made, closed when t ends.
func (h *grantHub) operator(t *testing.T, phase natsgrant.OperatorPhase, opts ...nats.Option) *nats.Conn {
	t.Helper()
	conn, err := testnats.ConnectSeed(t, h.url, h.seeds[phase], append([]nats.Option{nats.CustomInboxPrefix(natsgrant.OperatorInbox)}, opts...)...)
	require.NoError(t, err, "connect as Operator(%s)", phase)
	t.Cleanup(conn.Close)
	return conn
}

// admin connects as the admin once JetStream answers, closed when t ends, with both JetStream
// APIs on it.
func (h *grantHub) admin(t *testing.T) (*nats.Conn, jetstream.JetStream, nats.JetStreamContext) {
	t.Helper()
	conn := testnats.Connect(t, h.url)
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	require.NoError(t, err)
	legacy, err := conn.JetStream()
	require.NoError(t, err)
	return conn, js, legacy
}

// seedProtected creates every protected stream the preflight reads (ProtectedStreams) but skip,
// as the hub holds them: the notification stream with its carried subjects, the copied buckets
// with the configuration envoy-infra buckets creates them with, and the other buckets, each on
// file storage, so a restart of the server keeps them as the hub's restart does.
func seedProtected(t *testing.T, legacy nats.JetStreamContext, skip ...string) {
	t.Helper()
	skipped := func(stream string) bool {
		for _, name := range skip {
			if name == stream {
				return true
			}
		}
		return false
	}
	if !skipped(bus.Stream) {
		_, err := legacy.AddStream(&nats.StreamConfig{Name: bus.Stream, Subjects: bus.StreamCarriedSubjects(), Storage: nats.FileStorage})
		require.NoError(t, err, "create %s", bus.Stream)
	}
	buckets := []nats.KeyValueConfig{{Bucket: bus.SessionConsumersBucket, Storage: nats.FileStorage}}
	for _, bucket := range bus.CopiedBuckets() {
		config, ok := bus.CopiedBucketConfig(bucket, 1)
		require.True(t, ok, bucket)
		buckets = append(buckets, config)
	}
	for _, bucket := range []string{bus.LegionAgentBucket, bus.DPoPProofBucket, bus.WebhookRedeliveryBucket} {
		buckets = append(buckets, nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage})
	}
	for _, config := range buckets {
		if skipped(kvStreamPrefix + config.Bucket) {
			continue
		}
		_, err := legacy.CreateKeyValue(&config)
		require.NoError(t, err, "create bucket %s", config.Bucket)
	}
}

// readingOfStream is the reading of stream among readings.
func readingOfStream(t *testing.T, readings []StreamReading, stream string) StreamReading {
	t.Helper()
	for _, reading := range readings {
		if reading.Name == stream {
			return reading
		}
	}
	t.Fatalf("no reading of %s among %+v", stream, readings)
	return StreamReading{}
}

// requireRefusal requires err to be a Refusal of step whose finding names each of names.
func requireRefusal(t *testing.T, err error, step string, names ...string) {
	t.Helper()
	var refusal *Refusal
	require.ErrorAs(t, err, &refusal, "want a refusal of %s naming %v", step, names)
	require.Equal(t, step, refusal.Step, "the refusal %v", refusal)
	for _, name := range names {
		require.Contains(t, refusal.What, name, "the refusal %v", refusal)
	}
}

// requireUnknown requires err to be an Unknown of step whose text names each of names.
func requireUnknown(t *testing.T, err error, step string, names ...string) *Unknown {
	t.Helper()
	var unknown *Unknown
	require.ErrorAs(t, err, &unknown, "want UNKNOWN of %s naming %v", step, names)
	require.Equal(t, step, unknown.Step, "the UNKNOWN %v", unknown)
	for _, name := range names {
		require.Contains(t, unknown.Error(), name, "the UNKNOWN %v", unknown)
	}
	return unknown
}

// holdBarrier is a held command's write of its barrier record into bucket (hold.barrier.<hold id>,
// §3a step 6), under the held grant, which waits for the bucket's acknowledgement.
func holdBarrier(t *testing.T, held *nats.Conn, bucket string) {
	t.Helper()
	holdID := strings.TrimPrefix(nats.NewInbox(), nats.InboxPrefix)
	answer, err := held.Request("$KV."+bucket+".hold.barrier."+holdID, nil, jsRequest)
	require.NoError(t, err, "the barrier write into %s", bucket)
	var ack struct {
		Error  *jetstream.APIError `json:"error,omitempty"`
		Stream string              `json:"stream"`
	}
	require.NoError(t, json.Unmarshal(answer.Data, &ack))
	require.Nil(t, ack.Error, "the barrier write into %s", bucket)
	require.Equal(t, kvStreamPrefix+bucket, ack.Stream)
}

// updateStream changes stream's configuration as change makes it, as a user holding
// STREAM.UPDATE of it does.
func updateStream(t *testing.T, js jetstream.JetStream, stream string, change func(*jetstream.StreamConfig)) {
	t.Helper()
	current, err := js.Stream(t.Context(), stream)
	require.NoError(t, err, "read %s", stream)
	config := current.CachedInfo().Config
	change(&config)
	_, err = js.UpdateStream(t.Context(), config)
	require.NoError(t, err, "update %s", stream)
}

// TestHoldPreflightAnAbsentBucket shows the preflight's stream reads aware of the workflow (§3a
// step 1, red team G5), under exactly natsgrant.Operator(OperatorPhasePrepare): `stream not found`
// is expected absence only for a target the hold's workflow declares absent. In a cutover hold the
// selection bucket's absence is recorded and passes, while an absent copied bucket stops the hold
// naming it; the same selection bucket absent in a buckets-create hold for another bucket stops
// it; and an unreadable stream info, the hub answering no JetStream or an error other than 10059,
// is UNKNOWN naming the stream. The row fails if the preflight takes any `stream not found` as
// absence, or reads a failed info as anything but UNKNOWN.
func TestHoldPreflightAnAbsentBucket(t *testing.T) {
	ctx := t.Context()
	// The prepare grant holds the STREAM.INFO of every stream the preflight reads, so every
	// answer below is the hub's, not the grant's refusal.
	prepareGrant := natsgrant.Operator(natsgrant.OperatorPhasePrepare).Publish
	for _, name := range ProtectedStreams() {
		require.Contains(t, prepareGrant, fmt.Sprintf(natsserver.JSApiStreamInfoT, name))
	}
	selection := kvStreamPrefix + bus.SessionConsumersBucket
	interests := kvStreamPrefix + bus.InterestBucket
	hub := startGrantHub(t)
	_, _, legacy := hub.admin(t)
	seedProtected(t, legacy, selection)
	prepare := hub.operator(t, natsgrant.OperatorPhasePrepare)

	// The cutover's first create-bucket declares the selection bucket absent: its absence is
	// recorded, and every other stream is read present.
	readings, err := ReadProtectedStreams(ctx, prepare, []string{selection})
	require.NoError(t, err)
	require.Len(t, readings, len(ProtectedStreams()))
	require.True(t, readingOfStream(t, readings, selection).Absent)
	for _, reading := range readings {
		if reading.Name != selection {
			require.False(t, reading.Absent, reading.Name)
			require.False(t, reading.Created.IsZero(), "%s's created", reading.Name)
		}
	}
	// Control: the same absence in a hold whose workflow does not declare it, a buckets-create of
	// envoy_interests, or one that declares nothing, stops the hold naming it.
	_, err = ReadProtectedStreams(ctx, prepare, []string{interests})
	requireRefusal(t, err, StepPreflight, selection, "does not declare it absent")
	t.Logf("control, the selection bucket's absence in a buckets-create hold of envoy_interests: %v", err)
	_, err = ReadProtectedStreams(ctx, prepare, nil)
	requireRefusal(t, err, StepPreflight, selection)

	// An absent copied bucket in a cutover hold stops the hold, naming it.
	require.NoError(t, legacy.DeleteKeyValue(bus.InterestBucket))
	_, err = ReadProtectedStreams(ctx, prepare, []string{selection})
	requireRefusal(t, err, StepPreflight, interests, "does not declare it absent")
	t.Logf("an absent copied bucket in a cutover hold: %v", err)
	// In the buckets-create hold that declares it, once the selection bucket exists, it passes.
	_, err = legacy.CreateKeyValue(&nats.KeyValueConfig{Bucket: bus.SessionConsumersBucket, Storage: nats.FileStorage})
	require.NoError(t, err)
	readings, err = ReadProtectedStreams(ctx, prepare, []string{interests})
	require.NoError(t, err)
	require.True(t, readingOfStream(t, readings, interests).Absent)
	require.False(t, readingOfStream(t, readings, selection).Absent)

	// A declared target the preflight does not read is UNKNOWN, never a silent pass.
	_, err = ReadProtectedStreams(ctx, prepare, []string{"KV_not_protected"})
	requireUnknown(t, err, StepPreflight, "KV_not_protected")

	// An unreadable stream info is UNKNOWN, naming the stream: a hub with no JetStream answers no
	// request at all.
	_, plainURL := testnats.StartConfig(t, "", nil, true)
	plain, err := testnats.ConnectSeed(t, plainURL, "", nats.CustomInboxPrefix(natsgrant.OperatorInbox))
	require.NoError(t, err)
	t.Cleanup(plain.Close)
	_, err = ReadProtectedStreams(ctx, plain, nil)
	unknown := requireUnknown(t, err, StepPreflight, bus.Stream)
	require.ErrorIs(t, unknown, nats.ErrNoResponders)
	t.Logf("a hub with no JetStream: %v", unknown)

	// And a hub that answers one stream's info with an error other than 10059, or with a body
	// that does not decode. A responder on the same plain server answers every other info as a
	// clean present stream; its control, with no stream answered wrong, passes the preflight, so
	// the UNKNOWN is the wrong answer's.
	infos := newInfoResponder(t, plainURL)
	_, err = ReadProtectedStreams(ctx, plain, nil)
	require.NoError(t, err, "the responder's clean answers")
	agents := kvStreamPrefix + bus.LegionAgentBucket
	infos.answer(agents, `{"type":"io.nats.jetstream.api.v1.stream_info_response","error":{"code":503,"err_code":10008,"description":"JetStream system temporarily unavailable"}}`)
	_, err = ReadProtectedStreams(ctx, plain, nil)
	unknown = requireUnknown(t, err, StepPreflight, agents, "10008")
	var apiErr *jetstream.APIError
	require.ErrorAs(t, unknown, &apiErr)
	require.Equal(t, jetstream.ErrorCode(10008), apiErr.ErrorCode)
	t.Logf("an info answered 10008: %v", unknown)
	infos.answer(agents, `{"config":`)
	_, err = ReadProtectedStreams(ctx, plain, nil)
	requireUnknown(t, err, StepPreflight, agents, "does not decode")
	// The responder's 10059 is absence, which the workflow must declare, as on the real hub.
	infos.answer(agents, `{"type":"io.nats.jetstream.api.v1.stream_info_response","error":{"code":404,"err_code":10059,"description":"stream not found"}}`)
	_, err = ReadProtectedStreams(ctx, plain, nil)
	requireRefusal(t, err, StepPreflight, agents)
	readings, err = ReadProtectedStreams(ctx, plain, []string{agents})
	require.NoError(t, err)
	require.True(t, readingOfStream(t, readings, agents).Absent)
}

// infoResponder answers STREAM.INFO on a server with no JetStream: each stream as a present clean
// stream, but for the streams answer sets, which it answers with the body given.
type infoResponder struct {
	mu     sync.Mutex
	bodies map[string]string
}

func newInfoResponder(t *testing.T, url string) *infoResponder {
	t.Helper()
	responder := &infoResponder{bodies: map[string]string{}}
	conn, err := testnats.ConnectSeed(t, url, "")
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	created := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	_, err = conn.Subscribe(fmt.Sprintf(natsserver.JSApiStreamInfoT, "*"), func(msg *nats.Msg) {
		name := strings.TrimPrefix(msg.Subject, fmt.Sprintf(natsserver.JSApiStreamInfoT, ""))
		responder.mu.Lock()
		body, set := responder.bodies[name]
		responder.mu.Unlock()
		if !set {
			info, err := json.Marshal(jetstream.StreamInfo{Config: jetstream.StreamConfig{Name: name}, Created: created, State: jetstream.StreamState{LastSeq: 1, Msgs: 1}})
			if err != nil {
				panic(err)
			}
			body = string(info)
		}
		_ = msg.Respond([]byte(body))
	})
	require.NoError(t, err)
	require.NoError(t, conn.Flush())
	return responder
}

// answer has the responder answer stream's info with body.
func (r *infoResponder) answer(stream, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies[stream] = body
}

// TestHoldSources shows the sources check at both ends of a held command (§3a step 1 and the
// postcheck): a source added to a copied bucket before the hold stops the hold at step 1, under
// exactly the prepare grant, naming the bucket; and a last_seq that moved beyond the command's own
// writes stops the command after its mutation, under exactly the held grant, naming the stream,
// whether or not the source is still there. Control: without the step-1 check the hold passes and
// the source writes K's record into the bucket during the hold. The row fails if the preflight or
// the postcheck stops reading sources, or if the postcheck stops comparing last_seq with step 1's
// plus the command's writes.
func TestHoldSources(t *testing.T) {
	ctx := t.Context()
	sessions := kvStreamPrefix + bus.SessionBucket
	hub := startGrantHub(t)
	_, js, legacy := hub.admin(t)
	seedProtected(t, legacy)
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "FEED", Subjects: []string{"feed.>"}, Storage: jetstream.MemoryStorage})
	require.NoError(t, err)
	prepare := hub.operator(t, natsgrant.OperatorPhasePrepare)
	held := hub.operator(t, natsgrant.OperatorPhaseHeld)
	changes := map[string]Change{sessions: {Kind: ChangeKept, Writes: 1}}

	// Control first: with no source, step 1 passes, and after a command whose one write to the
	// bucket is its barrier record, the postcheck passes.
	step1, err := ReadProtectedStreams(ctx, prepare, nil)
	require.NoError(t, err)
	holdBarrier(t, held, bus.SessionBucket)
	_, err = Postcheck(ctx, held, step1, changes)
	require.NoError(t, err, "the postcheck of a command that made its one stated write")

	// A source added to the bucket before the hold: FEED's messages, written into the bucket as
	// its records.
	updateStream(t, js, sessions, func(config *jetstream.StreamConfig) {
		config.Sources = []*jetstream.StreamSource{{Name: "FEED", SubjectTransforms: []jetstream.SubjectTransformConfig{{Source: "feed.*", Destination: "$KV." + bus.SessionBucket + ".{{wildcard(1)}}"}}}}
	})
	_, err = ReadProtectedStreams(ctx, prepare, nil)
	requireRefusal(t, err, StepPreflight, sessions, "sources from FEED")
	t.Logf("step 1 with the check: %v", err)

	// Control: without the check step 1 passes, and during the hold the source writes K's record.
	step1, err = readProtectedStreams(ctx, prepare, nil, streamChecks{noSources: true})
	require.NoError(t, err)
	_, err = js.Publish(ctx, "feed.K", []byte("sourced"))
	require.NoError(t, err)
	sessionsKV, err := legacy.KeyValue(bus.SessionBucket)
	require.NoError(t, err)
	testwait.Eventually(t, "the source to write K's record into "+sessions, func() bool {
		entry, err := sessionsKV.Get("K")
		return err == nil && string(entry.Value()) == "sourced"
	})
	t.Logf("control, step 1 without the check passed, and the source wrote K's record %q into %s", "sourced", sessions)
	holdBarrier(t, held, bus.SessionBucket)

	// After the command's mutation: the postcheck refuses the source, and its last_seq check stops
	// the command on its own, with the sources check removed and again once the source is gone.
	_, err = Postcheck(ctx, held, step1, changes)
	requireRefusal(t, err, StepPostcheck, sessions, "sources from FEED")
	_, err = postcheck(ctx, held, step1, changes, streamChecks{noSources: true})
	requireRefusal(t, err, StepPostcheck, sessions, "last_seq", "plus the command's 1 writes")
	t.Logf("the postcheck's last_seq check, the sources check removed: %v", err)
	updateStream(t, js, sessions, func(config *jetstream.StreamConfig) { config.Sources = nil })
	_, err = Postcheck(ctx, held, step1, changes)
	requireRefusal(t, err, StepPostcheck, sessions, "last_seq", "plus the command's 1 writes")
	t.Logf("the postcheck after the source is gone: %v", err)
}

// scheduleSubject is where TestHoldMessageSchedules persists its schedule: a key of the bucket
// itself, since a schedule's target must be one of its own stream's subjects.
const scheduleSubject = "$KV." + bus.SessionBucket + ".schedule"

// The message-schedule headers, written out because this test binary also builds against
// nats-server 2.10.29 (forced-order.sh -server 2.10.29), which has no message schedules and
// declares none of them. nats-server 2.15.1 declares them in server/stream.go: JSSchedulePattern
// at :759, JSScheduleTarget at :763 and JSScheduler at :775.
const (
	schedulePatternHeader = "Nats-Schedule"
	scheduleTargetHeader  = "Nats-Schedule-Target"
	schedulerHeader       = "Nats-Scheduler"
)

// TestHoldMessageSchedules shows the message-schedule check (§3a step 2, gate 2), on nats-server
// 2.15.1, the linked server: a protected copied bucket with allow_msg_schedules and a schedule
// persisted in it, repeating every second so it fires after the restart and after the census,
// stops the hold at the preflight, under exactly the prepare grant, and at the census, under
// exactly the held grant, naming the bucket. Control: without the check the preflight passes, the
// server restarts on its store, the census passes, and the server writes the schedule's target,
// K's record, into the bucket after the census, with no client. The row fails if the preflight or
// the census stops reading allow_msg_schedules on a protected stream.
func TestHoldMessageSchedules(t *testing.T) {
	ctx := t.Context()
	sessions := kvStreamPrefix + bus.SessionBucket
	hub := startGrantHub(t)
	_, js, legacy := hub.admin(t)
	seedProtected(t, legacy)
	// A listener holding STREAM.UPDATE on the copied bucket allows message schedules on it, which
	// nats-server takes only with discard old, and persists a schedule whose target is K.
	updateStream(t, js, sessions, func(config *jetstream.StreamConfig) {
		config.AllowMsgSchedules = true
		config.Discard = jetstream.DiscardOld
	})
	schedule := nats.NewMsg(scheduleSubject)
	schedule.Header.Set(schedulePatternHeader, "@every 1s")
	schedule.Header.Set(scheduleTargetHeader, "$KV."+bus.SessionBucket+".K")
	schedule.Data = []byte("scheduled")
	_, err := js.PublishMsg(ctx, schedule)
	require.NoError(t, err, "persist the schedule")

	prepare := hub.operator(t, natsgrant.OperatorPhasePrepare)
	_, err = ReadProtectedStreams(ctx, prepare, nil)
	requireRefusal(t, err, StepPreflight, sessions, "allow_msg_schedules")
	held := hub.operator(t, natsgrant.OperatorPhaseHeld)
	_, err = TakeCensus(ctx, held, nil)
	requireRefusal(t, err, StepCensus, sessions, "allow_msg_schedules")
	t.Logf("with the check, the preflight and the census each refuse: %v", err)

	// Control: without the check the preflight passes; the hold's restart stops the server and
	// starts it again on its own store, which keeps the schedule; the census passes; and then the
	// server writes K's record into the bucket, for the schedule, with no client connected but the
	// operator.
	unchecked := streamChecks{noSchedules: true}
	step1, err := readProtectedStreams(ctx, prepare, nil, unchecked)
	require.NoError(t, err)
	hub.server.Stop(t)
	hub.server.Start(t)
	_, js, _ = hub.admin(t)
	held = hub.operator(t, natsgrant.OperatorPhaseHeld)
	census, err := takeCensus(ctx, held, StepCensus, presentStreams(step1), unchecked)
	require.NoError(t, err)
	var atCensus uint64
	for _, info := range census.Streams {
		if info.Config.Name == sessions {
			atCensus = info.State.LastSeq
		}
	}
	require.NotZero(t, atCensus, "the census's reading of %s", sessions)
	stream, err := js.Stream(ctx, sessions)
	require.NoError(t, err)
	var written *jetstream.RawStreamMsg
	testwait.Eventually(t, "the schedule to write K's record into "+sessions+" after the census", func() bool {
		msg, err := stream.GetLastMsgForSubject(ctx, "$KV."+bus.SessionBucket+".K")
		if err != nil {
			return false
		}
		written = msg
		return msg.Sequence > atCensus
	})
	require.Equal(t, "scheduled", string(written.Data))
	require.Equal(t, scheduleSubject, written.Header.Get(schedulerHeader), "the server wrote K for the schedule")
	t.Logf("control, without the check: after the restart and the census (%s last_seq %d), the server wrote K's record %q at sequence %d for the schedule %s", sessions, atCensus, written.Data, written.Sequence, written.Header.Get(schedulerHeader))
}
