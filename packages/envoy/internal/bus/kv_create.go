// The create of a KV bucket's stream, nats.go's CreateKeyValue made one request at a time, beside
// EnsureKeyValue, which creates every bucket a start finds missing through it.

package bus

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// createBucketStream creates the stream of the bucket config names as nats.go's CreateKeyValue
// does (nats.go@v1.55.0 kv.go:422-551), making the same requests in the same order, but one at a
// time through js, so that each is read for the server's refusal of its own subject alone
// (readsRefusals): the account's JetStream info (AccountInfo), the stream's create (AddStream),
// and, when the server answers that the stream's name is already in use, a read of that stream
// (StreamInfo) and, when the stream holds the configuration asked for but for its discard policy
// and its direct gets, an update of it to this configuration (UpdateStream). nats.go makes them
// all in one call, so read for refusals together, a request the call had not reached yet took
// another send's refusal of its subject while the call still waited on an earlier request
// (TestABucketCreateTakesOnlyTheRefusalOfTheStepItIsOn).
//
// It answers the errors CreateKeyValue answers, and no handle: nats.go builds the handle from the
// stream's info with a function it does not export, so EnsureKeyValue looks the bucket up once the
// stream is there (awaitBucketApplied). The checks nats.go makes before it sends anything are made
// first, in its order, and answer its errors; the server's version is read where nats.go reads it.
// No request is sent once ctx has ended (addBucketStream).
func createBucketStream(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, config *nats.KeyValueConfig) error {
	stream, err := addBucketStream(ctx, conn, js, config)
	if errors.Is(err, nats.ErrStreamNameAlreadyInUse) && ctx.Err() == nil {
		// nats.go drops the read's own error: a stream it cannot read stays "already in use".
		if info, _ := js.StreamInfo(stream.Name); info != nil && ctx.Err() == nil {
			info.Config.Discard = stream.Discard
			info.Config.AllowDirect = stream.AllowDirect
			if reflect.DeepEqual(&info.Config, stream) {
				_, err = js.UpdateStream(stream)
			}
		}
	}
	return err
}

// addBucketStream is createBucketStream up to its create and no further: nats.go's checks, the
// account's JetStream info and the stream's create, answering the stream it asked for with the
// create's answer. A create the server answers "stream name already in use" is answered so, with
// no read of that stream and no update of it, whatever configuration the server reports for it:
// a create-only open's create (createOnly), which leaves a bucket another client made as it is.
//
// ctx is read before each request, and none is sent once it has ended: a caller that stops waiting
// (sentAgain, through Bounded) leaves this call running on its own goroutine, and without the read
// that call would still send the create after its caller had answered, such as an operator's
// `envoy-infra buckets create` interrupted while the account's info was in flight.
func addBucketStream(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, config *nats.KeyValueConfig) (*nats.StreamConfig, error) {
	if !natsServerMinVersion(conn.ConnectedServerVersion(), 2, 6, 2) {
		return nil, errKeyValueServerTooOld
	}
	if config == nil {
		return nil, nats.ErrKeyValueConfigRequired
	}
	if !natsBucketName.MatchString(config.Bucket) {
		return nil, nats.ErrInvalidBucketName
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := js.AccountInfo(); err != nil {
		return nil, err
	}
	stream, err := bucketStreamConfig(config, conn.ConnectedServerVersion())
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return stream, err
	}
	_, err = js.AddStream(stream)
	return stream, err
}

// errKeyValueServerTooOld is the error nats.go's CreateKeyValue answers, before it sends anything,
// on a connection whose server reports a version before 2.6.2, or none (nats.go@v1.55.0
// kv.go:423-425).
var errKeyValueServerTooOld = errors.New("nats: key-value requires at least server version 2.6.2")

// natsBucketName copies nats.go's validBucketRe (nats.go@v1.55.0 kv.go:391), the names
// CreateKeyValue takes for a bucket.
var natsBucketName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// bucketStreamConfig is the stream configuration nats.go's CreateKeyValue builds for config, on a
// server reporting serverVersion (nats.go@v1.55.0 kv.go:436-523), copied field for field because
// nats.go does not export it: TestABucketStreamIsCreatedAsNatsGoCreatesIt has both creates sent and
// compares the requests. The one difference is that a mirror or a source config names is copied
// before it is changed, where nats.go changes the caller's own sources.
func bucketStreamConfig(config *nats.KeyValueConfig, serverVersion string) (*nats.StreamConfig, error) {
	history := int64(1)
	if config.History > 0 {
		if config.History > nats.KeyValueMaxHistory {
			return nil, nats.ErrHistoryToLarge
		}
		history = int64(config.History)
	}
	replicas := config.Replicas
	if replicas == 0 {
		replicas = 1
	}
	maxBytes := config.MaxBytes
	if maxBytes == 0 {
		maxBytes = -1
	}
	maxMsgSize := config.MaxValueSize
	if maxMsgSize == 0 {
		maxMsgSize = -1
	}
	duplicates := 2 * time.Minute
	if config.TTL > 0 && config.TTL < duplicates {
		duplicates = config.TTL
	}
	var compression nats.StoreCompression
	if config.Compression {
		compression = nats.S2Compression
	}
	stream := &nats.StreamConfig{
		Name:              "KV_" + config.Bucket,
		Description:       config.Description,
		MaxMsgsPerSubject: history,
		MaxBytes:          maxBytes,
		MaxAge:            config.TTL,
		MaxMsgSize:        maxMsgSize,
		Storage:           config.Storage,
		Replicas:          replicas,
		Placement:         config.Placement,
		AllowRollup:       true,
		DenyDelete:        true,
		Duplicates:        duplicates,
		MaxMsgs:           -1,
		MaxConsumers:      -1,
		AllowDirect:       true,
		RePublish:         config.RePublish,
		Compression:       compression,
	}
	if config.Mirror != nil {
		mirror := copiedSource(config.Mirror)
		if !strings.HasPrefix(mirror.Name, "KV_") {
			mirror.Name = "KV_" + mirror.Name
		}
		stream.Mirror = mirror
		stream.MirrorDirect = true
	} else {
		for _, source := range config.Sources {
			source = copiedSource(source)
			bucket, prefixed := strings.CutPrefix(source.Name, "KV_")
			if !prefixed {
				source.Name = "KV_" + bucket
			}
			if source.External == nil || bucket != config.Bucket {
				source.SubjectTransforms = []nats.SubjectTransformConfig{{Source: fmt.Sprintf("$KV.%s.>", bucket), Destination: fmt.Sprintf("$KV.%s.>", config.Bucket)}}
			}
			stream.Sources = append(stream.Sources, source)
		}
		stream.Subjects = []string{fmt.Sprintf("$KV.%s.>", config.Bucket)}
	}
	if natsServerMinVersion(serverVersion, 2, 7, 2) {
		stream.Discard = nats.DiscardNew
	}
	return stream, nil
}

// copiedSource copies nats.go's StreamSource.copy (nats.go@v1.55.0 jsm.go:302-314), which it
// does not export: source with its start time and its external stream copied too.
func copiedSource(source *nats.StreamSource) *nats.StreamSource {
	copied := *source
	if source.OptStartTime != nil {
		start := *source.OptStartTime
		copied.OptStartTime = &start
	}
	if source.External != nil {
		external := *source.External
		copied.External = &external
	}
	return &copied
}
