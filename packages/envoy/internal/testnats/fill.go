package testnats

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

// messageNotStored is nats-server's code for a message its stream did not store
// (JSStreamStoreFailedF), and maxBytesExceeded its description when the stream refused the message
// because it holds its byte limit (ErrMaxBytes). nats.go names neither.
const (
	messageNotStored natsgo.ErrorCode = 10077
	maxBytesExceeded                  = "maximum bytes exceeded"
)

// FillStream fills stream, which must have a byte limit, through js until the stream holds too
// much to store one more message of one byte: messages of a kibibyte on subject.<n> until the
// stream refuses one, then messages of one byte on subject.tiny.<n> until the stream's own count
// (its info's bytes against its byte limit) leaves less room than one of them takes. Both subjects
// must be ones the stream captures, and the stream must have room for a one-byte message when the
// fill starts. The stream then refuses every message whose stored size (its subject, headers and
// payload) is no smaller than a one-byte fill message's, which every envelope's is, so the test's
// verdict does not rest on how much room the last fill message left.
//
// A refusal alone does not show the stream full, so the fill reads the stream's count. A clustered
// stream's leader checks a publish against the bytes it holds plus those of every message it has
// proposed and not yet finished applying, and it acknowledges a message before it stops counting
// it so (nats-server 2.15.1: checkMsgHeadersPreClusteredProposal, jetstream_batching.go, adds
// mset.inflight, which applyStreamMsgOp, jetstream_cluster.go, clears only once processJetStreamMsg
// has stored the message and sent its acknowledgement). The publish that follows an
// acknowledgement can therefore be refused while the stream still has room for it, by as much as
// the acknowledged message takes, and a fill that stopped at that refusal left room for the message
// a full-stream test expects refused (dispatch://INFRA-3904). A one-byte message refused at the
// byte limit while the count still shows room for it is sent again, at testwait's pace, until the
// stream stores it or its count shows no room for it. Any other failure of a fill publish fails
// the test, a refusal for another reason (the stream's message count, say) among them.
//
// The size a one-byte message takes is read from the first one the stream stores alone: the
// stream's count before and after its publish, with its acknowledged sequence the one sequence the
// stream added between them. A publish another writer's message shares that span with, or that the
// stream refused, is measured again with the next one-byte message, so another publisher into the
// stream during the fill makes the fill no less full.
func FillStream(t *testing.T, js natsgo.JetStreamContext, stream, subject string) {
	t.Helper()
	ctx := testwait.Context(t)
	read := func() *natsgo.StreamInfo {
		t.Helper()
		info, err := js.StreamInfo(stream, natsgo.Context(ctx))
		if err != nil {
			t.Fatalf("read stream %s: %v", stream, err)
		}
		return info
	}
	limit := read().Config.MaxBytes
	if limit <= 0 {
		t.Fatalf("stream %s has no byte limit, so a fill of it never ends", stream)
	}
	state := func() natsgo.StreamState {
		t.Helper()
		return read().State
	}
	room := func() int64 {
		t.Helper()
		return limit - int64(state().Bytes)
	}
	// full reports that err is the stream's refusal of a fill message at its byte limit, and fails
	// the test on any other error.
	full := func(message string, err error) bool {
		t.Helper()
		var answer natsgo.JetStreamError
		if !errors.As(err, &answer) || answer.APIError() == nil ||
			answer.APIError().ErrorCode != messageNotStored || answer.APIError().Description != maxBytesExceeded {
			t.Fatalf("%s into stream %s: %v, want it stored or refused at the stream's byte limit", message, stream, err)
		}
		return true
	}
	sent := 0
	tiny := func() (*natsgo.PubAck, error) {
		sent++
		return js.Publish(fmt.Sprintf("%s.tiny.%04d", subject, sent), []byte("x"), natsgo.Context(ctx))
	}

	size := testwait.Until(t, fmt.Sprintf("a one-byte fill message that stream %s stored alone, to measure", stream), func() (int64, bool) {
		before := state()
		ack, err := tiny()
		if err != nil {
			full("the one-byte fill message to measure", err)
			return 0, false
		}
		after := state()
		alone := ack.Sequence == before.LastSeq+1 && after.LastSeq == ack.Sequence && after.Msgs == before.Msgs+1
		return int64(after.Bytes) - int64(before.Bytes), alone
	})
	if size <= 0 {
		t.Fatalf("stream %s stored a one-byte message alone and its count grew by %d bytes", stream, size)
	}

	kibibyte := bytes.Repeat([]byte("x"), 1<<10)
	before := room()
	for n := 0; ; n++ {
		if int64(n)<<10 > before {
			t.Fatalf("stream %s stored %d messages of a kibibyte with room for %d bytes", stream, n, before)
		}
		_, err := js.Publish(fmt.Sprintf("%s.%d", subject, n), kibibyte, natsgo.Context(ctx))
		if err == nil {
			continue
		}
		full(fmt.Sprintf("kibibyte fill message %d", n), err)
		break
	}

	left := room()
	for stored := 0; ; stored++ {
		if int64(stored)*size > left {
			t.Fatalf("stream %s stored %d one-byte messages of %d bytes after the kibibytes, with room for %d bytes", stream, stored, size, left)
		}
		err := testwait.Until(t, fmt.Sprintf("a one-byte fill message stored, or stream %s too full to store one (%d bytes)", stream, size),
			func() (error, bool) {
				_, err := tiny()
				return err, err == nil || full("a one-byte fill message", err) && room() < size
			})
		if err != nil {
			return
		}
	}
}
