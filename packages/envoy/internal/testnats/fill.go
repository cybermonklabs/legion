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
// (JSStreamStoreFailedF), the answer a stream at its byte limit gives a publish. nats.go names no
// constant for it.
const messageNotStored natsgo.ErrorCode = 10077

// FillStream fills stream, which must have a byte limit, through js until the stream holds too
// much to store one more message of one byte: messages of a kibibyte on subject.<n> until the
// stream refuses one, then messages of one byte on subject.tiny.<n> until the stream's own count
// (its info's bytes against its byte limit) leaves less room than one of them takes. Both subjects
// must be ones the stream captures. A message the test then sends into the stream, of any size, is
// refused, so the test's verdict does not rest on how much room the last fill message left.
//
// A refusal alone does not show the stream full, so the fill reads the stream's count. A clustered
// stream's leader checks a publish against the bytes it holds plus those of every message it has
// proposed and not yet finished applying, and it acknowledges a message before it stops counting
// it so (nats-server 2.15.1: checkMsgHeadersPreClusteredProposal, jetstream_batching.go, adds
// mset.inflight, which applyStreamMsgOp, jetstream_cluster.go, clears only once processJetStreamMsg
// has stored the message and sent its acknowledgement). The publish that follows an
// acknowledgement can therefore be refused while the stream still has room for it, by as much as
// the acknowledged message takes, and a fill that stopped at that refusal left room for the message
// a full-stream test expects refused (dispatch://INFRA-3904). A one-byte message refused while the
// count still shows room for it is sent again, at testwait's pace, until the stream stores it or
// its count shows no room for it. Any other failure of a fill publish fails the test.
//
// The first one-byte message, sent before the rest, measures what each takes: every one is the
// same size. Another publisher into the stream during the fill adds its bytes to that measure, so
// the fill can then end with room for a few one-byte messages, but none for a larger one.
func FillStream(t *testing.T, js natsgo.JetStreamContext, stream, subject string) {
	t.Helper()
	ctx := testwait.Context(t)
	room := func() int64 {
		t.Helper()
		info, err := js.StreamInfo(stream, natsgo.Context(ctx))
		if err != nil {
			t.Fatalf("read stream %s: %v", stream, err)
		}
		if info.Config.MaxBytes <= 0 {
			t.Fatalf("stream %s has no byte limit, so a fill of it never ends", stream)
		}
		return info.Config.MaxBytes - int64(info.State.Bytes)
	}
	// refused reports that err is the stream's refusal to store a fill message, and fails the test
	// on any other error.
	refused := func(message string, err error) bool {
		t.Helper()
		var answer natsgo.JetStreamError
		if !errors.As(err, &answer) || answer.APIError() == nil || answer.APIError().ErrorCode != messageNotStored {
			t.Fatalf("%s into stream %s: %v, want it stored or the stream's refusal to store it", message, stream, err)
		}
		return true
	}
	tiny := func(n int) error {
		_, err := js.Publish(fmt.Sprintf("%s.tiny.%04d", subject, n), []byte("x"), natsgo.Context(ctx))
		return err
	}

	before := room()
	if err := tiny(0); err != nil {
		t.Fatalf("the first one-byte fill message into stream %s: %v", stream, err)
	}
	size := before - room()
	if size <= 0 {
		t.Fatalf("stream %s took a one-byte message and its count grew by %d bytes", stream, size)
	}

	kibibyte := bytes.Repeat([]byte("x"), 1<<10)
	for n := 0; ; n++ {
		if int64(n)<<10 > before {
			t.Fatalf("stream %s stored %d messages of a kibibyte with room for %d bytes", stream, n, before)
		}
		_, err := js.Publish(fmt.Sprintf("%s.%d", subject, n), kibibyte, natsgo.Context(ctx))
		if err == nil {
			continue
		}
		refused(fmt.Sprintf("kibibyte fill message %d", n), err)
		break
	}

	left := room()
	for n := 1; ; n++ {
		if int64(n-1)*size > left {
			t.Fatalf("stream %s stored %d one-byte messages of %d bytes after the kibibytes, with room for %d bytes", stream, n-1, size, left)
		}
		err := testwait.Until(t, fmt.Sprintf("one-byte fill message %d stored, or stream %s too full to store one (%d bytes)", n, stream, size),
			func() (error, bool) {
				err := tiny(n)
				return err, err == nil || refused(fmt.Sprintf("one-byte fill message %d", n), err) && room() < size
			})
		if err != nil {
			return
		}
	}
}
