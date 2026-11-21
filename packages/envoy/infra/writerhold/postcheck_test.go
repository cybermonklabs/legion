package writerhold

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// archive is a stream's backup as STREAM.SNAPSHOT streamed it: its chunks, and the configuration
// and state the snapshot's answer stated, which a restore sends back as they came.
type archive struct {
	chunks [][]byte
	config json.RawMessage
	state  json.RawMessage
	ArchiveState
}

// snapshotStream backs stream up on nc, as envoy-infra buckets backup does through jsm.go: a
// STREAM.SNAPSHOT delivered to nc's own inbox, each chunk acknowledged on its reply, until the
// server's end-of-snapshot message.
func snapshotStream(t *testing.T, nc *nats.Conn, stream string) archive {
	t.Helper()
	inbox := nc.NewInbox()
	chunks, err := nc.SubscribeSync(inbox)
	require.NoError(t, err)
	defer func() { _ = chunks.Unsubscribe() }()
	var answer struct {
		Error  *jetstream.APIError `json:"error,omitempty"`
		Config json.RawMessage     `json:"config"`
		State  json.RawMessage     `json:"state"`
	}
	request := map[string]any{"deliver_subject": inbox, "no_consumers": true}
	require.NoError(t, apiRequest(t.Context(), nc, fmt.Sprintf(natsserver.JSApiStreamSnapshotT, stream), request, &answer))
	require.Nil(t, answer.Error, "the snapshot of %s", stream)
	backup := archive{config: answer.Config, state: answer.State}
	var state struct {
		LastSeq  uint64 `json:"last_seq"`
		Messages uint64 `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(answer.State, &state))
	backup.ArchiveState = ArchiveState{LastSeq: state.LastSeq, Messages: state.Messages}
	for {
		chunk, err := chunks.NextMsg(30 * time.Second)
		require.NoError(t, err, "the snapshot's next chunk of %s", stream)
		if len(chunk.Data) == 0 {
			// The end of the snapshot: status 204 once a chunk with a reply was sent, and no
			// header at all when the whole archive was one short chunk; 408 or 500 is a failure.
			status := chunk.Header.Get("Status")
			require.Contains(t, []string{"", "204"}, status, "the snapshot of %s ended %s %q", stream, status, chunk.Header.Get("Description"))
			require.NotEmpty(t, backup.chunks, "the snapshot of %s sent no chunk", stream)
			return backup
		}
		backup.chunks = append(backup.chunks, chunk.Data)
		// The last chunk read short carries no reply (jetstream_api.go's streamSnapshot): only the
		// others take an acknowledgement.
		if chunk.Reply != "" {
			require.NoError(t, chunk.Respond(nil), "acknowledge a chunk of %s", stream)
		}
	}
}

// restoreStream restores stream from backup on nc, as envoy-infra buckets restore does through
// jsm.go: a STREAM.RESTORE, each chunk sent as a request to the subject its answer names, then
// the empty message that ends it, whose answer is the restored stream or the restore's error.
func restoreStream(t *testing.T, nc *nats.Conn, stream string, backup archive) {
	t.Helper()
	var answer struct {
		Error          *jetstream.APIError `json:"error,omitempty"`
		DeliverSubject string              `json:"deliver_subject"`
	}
	request := map[string]json.RawMessage{"config": backup.config, "state": backup.state}
	require.NoError(t, apiRequest(t.Context(), nc, fmt.Sprintf(natsserver.JSApiStreamRestoreT, stream), request, &answer))
	require.Nil(t, answer.Error, "the restore of %s", stream)
	for _, chunk := range backup.chunks {
		reply, err := nc.Request(answer.DeliverSubject, chunk, 30*time.Second)
		require.NoError(t, err, "send a chunk of %s", stream)
		require.Empty(t, reply.Data, "the restore's answer to a chunk of %s", stream)
	}
	done, err := nc.Request(answer.DeliverSubject, nil, time.Minute)
	require.NoError(t, err, "end the restore of %s", stream)
	var restored struct {
		Error *jetstream.APIError `json:"error,omitempty"`
	}
	require.NoError(t, json.Unmarshal(done.Data, &restored))
	require.Nil(t, restored.Error, "the restore of %s", stream)
}

// deleteStream deletes stream on nc, a raw STREAM.DELETE.
func deleteStream(t *testing.T, nc *nats.Conn, stream string) {
	t.Helper()
	var answer struct {
		Error   *jetstream.APIError `json:"error,omitempty"`
		Success bool                `json:"success"`
	}
	require.NoError(t, apiRequest(t.Context(), nc, fmt.Sprintf(natsserver.JSApiStreamDeleteT, stream), nil, &answer))
	require.Nil(t, answer.Error, "delete %s", stream)
	require.True(t, answer.Success, "delete %s", stream)
}

// createStream creates a stream of config on nc, a raw STREAM.CREATE, leaving out the server's own
// _nats. metadata that an info's configuration carries.
func createStream(t *testing.T, nc *nats.Conn, config jetstream.StreamConfig) {
	t.Helper()
	metadata := map[string]string{}
	for key, value := range config.Metadata {
		if !strings.HasPrefix(key, "_nats.") {
			metadata[key] = value
		}
	}
	config.Metadata = metadata
	var answer struct {
		Error *jetstream.APIError `json:"error,omitempty"`
	}
	require.NoError(t, apiRequest(t.Context(), nc, fmt.Sprintf(natsserver.JSApiStreamCreateT, config.Name), config, &answer))
	require.Nil(t, answer.Error, "create %s", config.Name)
}

// TestHoldThePostcheck shows the postcheck per stream incarnation and per operation (§3a, red
// team R6, F7), its commands run under exactly natsgrant.Operator(OperatorPhaseHeld): a CAS write
// to the selection bucket whose answer is dropped, then read back from its record and so counted,
// passes on a kept stream; a recreated copied bucket passes with a new created and its own one
// write; and a restored one passes on the archive's last_seq and message count plus its one write
// after the restore, its created not compared. The plan says a restore keeps the archive's created
// (stream.go:9622-9623, the V1 restore); on nats-server 2.15.1 the snapshot is the V2 archive,
// whose restore sets a new created, and the row measures that. Control: a restore check that
// compares created stops the correct restore: on 2.15.1 the one that demands step 1's created,
// while the plan's, which demands a new one, passes it here. Control: one write by another raw
// client, in each case, stops the command naming the stream. The row fails if the postcheck
// compares a restored stream's created, or stops comparing last_seq and the message count with
// what the command states.
func TestHoldThePostcheck(t *testing.T) {
	ctx := t.Context()
	selection := kvStreamPrefix + bus.SessionConsumersBucket
	interests := kvStreamPrefix + bus.InterestBucket
	roles := kvStreamPrefix + bus.RoleClaimBucket
	hub := startGrantHub(t)
	_, js, legacy := hub.admin(t)
	seedProtected(t, legacy)
	bucket := func(name string) nats.KeyValue {
		kv, err := legacy.KeyValue(name)
		require.NoError(t, err)
		return kv
	}
	selectionKV, interestsKV, rolesKV := bucket(bus.SessionConsumersBucket), bucket(bus.InterestBucket), bucket(bus.RoleClaimBucket)
	var expected uint64
	for _, key := range []string{"a", "b", "c"} {
		revision, err := selectionKV.PutString("session."+key, "v1")
		require.NoError(t, err)
		if key == "a" {
			expected = revision
		}
		_, err = interestsKV.PutString(key, "v1")
		require.NoError(t, err)
		_, err = rolesKV.PutString(key, "v1")
		require.NoError(t, err)
	}
	held := hub.operator(t, natsgrant.OperatorPhaseHeld)
	// The backup of the roles bucket, taken outside the hold, before step 1.
	backup := snapshotStream(t, held, roles)
	require.Equal(t, ArchiveState{LastSeq: 3, Messages: 3}, backup.ArchiveState)
	_, err := rolesKV.PutString("d", "after the backup")
	require.NoError(t, err)

	prepare := hub.operator(t, natsgrant.OperatorPhasePrepare)
	step1, err := ReadProtectedStreams(ctx, prepare, nil)
	require.NoError(t, err)
	interestsConfig, err := js.Stream(ctx, interests)
	require.NoError(t, err)

	// The command. A CAS write to the selection bucket whose answer is dropped: a publish with no
	// reply, so no answer comes. The command reads its record back, finds its own value at a
	// sequence after the expected one, and counts the write.
	record := "$KV." + bus.SessionConsumersBucket + ".session.a"
	require.NotZero(t, expected, "session.a's revision")
	cas := nats.NewMsg(record)
	cas.Header.Set(natsserver.JSExpectedLastSubjSeq, fmt.Sprint(expected))
	cas.Data = []byte("v2")
	require.NoError(t, held.PublishMsg(cas))
	var readBack struct {
		Error   *jetstream.APIError `json:"error,omitempty"`
		Message struct {
			Sequence uint64 `json:"seq"`
			Data     []byte `json:"data"`
		} `json:"message"`
	}
	testwait.Eventually(t, "the CAS write's record to read back", func() bool {
		readBack.Error = nil
		err := apiRequest(ctx, held, fmt.Sprintf(natsserver.JSApiMsgGetT, selection), map[string]string{"last_by_subj": record}, &readBack)
		return err == nil && readBack.Error == nil && string(readBack.Message.Data) == "v2"
	})
	require.Greater(t, readBack.Message.Sequence, expected)
	// The interests bucket recreated with its configuration, and its barrier record written.
	deleteStream(t, held, interests)
	createStream(t, held, interestsConfig.CachedInfo().Config)
	holdBarrier(t, held, bus.InterestBucket)
	// The roles bucket restored from its backup, and its barrier record written.
	deleteStream(t, held, roles)
	restoreStream(t, held, roles, backup)
	holdBarrier(t, held, bus.RoleClaimBucket)

	changes := map[string]Change{
		selection: {Kind: ChangeKept, Writes: 1},
		interests: {Kind: ChangeRecreated, Writes: 1},
		roles:     {Kind: ChangeRestored, Writes: 1, Archive: backup.ArchiveState},
	}
	after, err := Postcheck(ctx, held, step1, changes)
	require.NoError(t, err)
	require.NotEqual(t, readingOfStream(t, step1, interests).Created, readingOfStream(t, after, interests).Created, "the recreated bucket's created")
	// On 2.15.1 the restore gives the stream a new created: the snapshot is the V2 archive
	// (stream.go:9477), whose restore, stream_backup.go's RestoreStreamV2, sets no created time.
	require.NotEqual(t, readingOfStream(t, step1, roles).Created, readingOfStream(t, after, roles).Created, "the restored bucket's created on 2.15.1")
	t.Logf("the postcheck passed: %s kept, last_seq %d; %s recreated, created %s, last_seq %d; %s restored, created %s (step 1's %s), last_seq %d, %d messages, from the archive's %+v",
		selection, readingOfStream(t, after, selection).LastSeq,
		interests, readingOfStream(t, after, interests).Created.Format(time.RFC3339Nano), readingOfStream(t, after, interests).LastSeq,
		roles, readingOfStream(t, after, roles).Created.Format(time.RFC3339Nano), readingOfStream(t, step1, roles).Created.Format(time.RFC3339Nano),
		readingOfStream(t, after, roles).LastSeq, readingOfStream(t, after, roles).Messages, backup.ArchiveState)

	// Control: a restore check that compares created stops the correct restore. On 2.15.1 that is
	// the check demanding step 1's created, as the kept check does; the plan's control, which
	// demands a new created, passes this restore, and stops one only on a server whose restore
	// keeps the archive's created (2.10.29's V1 restore).
	_, err = postcheck(ctx, held, step1, changes, streamChecks{restoreSameCreated: true})
	requireRefusal(t, err, StepPostcheck, roles, "created moved from")
	t.Logf("control, a restore check demanding step 1's created: %v", err)
	_, err = postcheck(ctx, held, step1, changes, streamChecks{restoreNewCreated: true})
	require.NoError(t, err, "the plan's control, a new created demanded of a 2.15.1 restore")
	t.Logf("the plan's control, a restore check demanding a new created, passes this 2.15.1 restore")

	// Control: one write by another raw client stops the command naming the stream, in each case,
	// taken in the reverse of the order the postcheck reads the streams, so each refusal is the
	// newest write's.
	for _, extra := range []struct {
		stream string
		kv     nats.KeyValue
		want   string
	}{
		{roles, rolesKV, "the archive's 3 plus the command's 1 writes after the restore is 4"},
		{interests, interestsKV, "the command states 1 writes to the stream it created"},
		{selection, selectionKV, "plus the command's 1 writes"},
	} {
		_, err := extra.kv.PutString("raw", "another client")
		require.NoError(t, err)
		_, err = Postcheck(ctx, held, step1, changes)
		requireRefusal(t, err, StepPostcheck, extra.stream, extra.want)
		t.Logf("control, one write by another raw client: %v", err)
	}
}

// TestHoldTheResidualByRawClients records the residual the coordinator accepted (§3a, t-10's
// dependencies): under exactly natsgrant.Operator(OperatorPhaseReleased), outside any hold, the
// grant refuses a raw client's direct publish onto $KV.envoy_sessions.redirect, yet its snapshot
// of KV_envoy_sessions with that deliver_subject succeeds, since nats-server checks a snapshot's
// deliver subject against nothing but its own form, and the snapshot's chunks land in the bucket
// as the record of key redirect. The row passes while the capability is as recorded, so a change
// to it, in the grant or the server, fails the row and is seen.
func TestHoldTheResidualByRawClients(t *testing.T) {
	ctx := t.Context()
	sessions := kvStreamPrefix + bus.SessionBucket
	redirect := "$KV." + bus.SessionBucket + ".redirect"
	hub := startGrantHub(t)
	admin, js, legacy := hub.admin(t)
	seedProtected(t, legacy)
	sessionsKV, err := legacy.KeyValue(bus.SessionBucket)
	require.NoError(t, err)
	_, err = sessionsKV.PutString("ses_live", "registered")
	require.NoError(t, err)
	refused := make(chan error, 8)
	released := hub.operator(t, natsgrant.OperatorPhaseReleased, nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
		refused <- err
	}))

	// The grant holds no publish onto the bucket: the direct write is the server's permissions
	// violation, and no record lands.
	require.NoError(t, released.Publish(redirect, []byte("direct")))
	require.NoError(t, released.Flush())
	select {
	case err := <-refused:
		require.True(t, errors.Is(err, nats.ErrPermissionViolation), "the direct publish's refusal %v", err)
		require.Contains(t, err.Error(), redirect)
		t.Logf("the grant refuses the direct publish: %v", err)
	case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
		t.Fatal("the grant did not refuse the direct publish onto " + redirect)
	}
	_, err = sessionsKV.Get("redirect")
	require.ErrorIs(t, err, nats.ErrKeyNotFound)

	// The snapshot aimed at the same subject succeeds, and what it sends is written into the
	// bucket as the record of redirect: a chunk of the archive, then the snapshot's empty end
	// message. The bucket keeps one message a key, so the end replaces the chunk; the admin's
	// subscription to the subject shows the chunk's bytes, and the bucket's sequence shows both
	// were stored.
	stream, err := js.Stream(ctx, sessions)
	require.NoError(t, err)
	before := stream.CachedInfo().State.LastSeq
	sent, err := admin.SubscribeSync(redirect)
	require.NoError(t, err)
	require.NoError(t, admin.Flush())
	var answer struct {
		Error  *jetstream.APIError `json:"error,omitempty"`
		Config *struct {
			Name string `json:"name"`
		} `json:"config"`
	}
	request := map[string]any{"deliver_subject": redirect, "no_consumers": true}
	require.NoError(t, apiRequest(ctx, released, fmt.Sprintf(natsserver.JSApiStreamSnapshotT, sessions), request, &answer))
	require.Nil(t, answer.Error, "the snapshot with deliver_subject %s", redirect)
	require.NotNil(t, answer.Config)
	require.Equal(t, sessions, answer.Config.Name)
	var landed struct {
		Error   *jetstream.APIError `json:"error,omitempty"`
		Message struct {
			Sequence uint64 `json:"seq"`
			Header   []byte `json:"hdrs"`
		} `json:"message"`
	}
	chunk, err := sent.NextMsg(testwait.Bounded(t, time.Minute, time.Second))
	require.NoError(t, err, "the snapshot's first message onto %s", redirect)
	require.NotEmpty(t, chunk.Data, "the snapshot's first message onto %s is a chunk of the archive", redirect)
	testwait.Eventually(t, "the snapshot's chunk and its end to land in "+sessions+" as the record of redirect", func() bool {
		landed.Error = nil
		err := apiRequest(ctx, admin, fmt.Sprintf(natsserver.JSApiMsgGetT, sessions), map[string]string{"last_by_subj": redirect}, &landed)
		return err == nil && landed.Error == nil && landed.Message.Sequence >= before+2
	})
	info, err := stream.Info(ctx)
	require.NoError(t, err)
	require.Equal(t, landed.Message.Sequence, info.State.LastSeq, "the record of redirect is the bucket's last message")
	t.Logf("the snapshot under Operator(released) with deliver_subject %s succeeded: a %d-byte chunk was published there, and %s stored the record of redirect at sequence %d (last_seq before %d)", redirect, len(chunk.Data), sessions, landed.Message.Sequence, before)
}
