package bus

import (
	"errors"
	"maps"
	"testing"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Client.StreamID answers the stream's identity (StreamIDMetadataKey), and ErrNoStreamID once the
// stream carries none, as SessionConsumers.StreamID answers it: a caller that names a cursor by
// the identity never takes "" for one. The same client answers the identity before the key is
// removed, which is the control that the read sees a stream's metadata at all. With "" answered for
// a stream with no identity, as the identity, the second read fails this test.
func TestClientStreamIDAnswersErrNoStreamIDForAStreamWithNoIdentity(t *testing.T) {
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	ctx := testwait.Context(t)
	js := client.JS()
	info, err := js.StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	want := info.Config.Metadata[StreamIDMetadataKey]
	if id, err := client.StreamID(ctx); err != nil || id == "" || id != want {
		t.Fatalf("StreamID = %q, %v; want the stream's identity %q", id, err, want)
	}

	unidentified := info.Config
	unidentified.Metadata = maps.Clone(info.Config.Metadata)
	delete(unidentified.Metadata, StreamIDMetadataKey)
	if _, err := js.UpdateStream(&unidentified); err != nil {
		t.Fatalf("remove the stream's identity: %v", err)
	}
	if id, err := client.StreamID(ctx); !errors.Is(err, ErrNoStreamID) || id != "" {
		t.Fatalf("StreamID of a stream with no identity = %q, %v; want \"\" and ErrNoStreamID", id, err)
	}
}
