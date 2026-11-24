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
// first, in its order, and answer its errors, but a connection reporting no server version, where
// nats.go answers a server too old, answers ErrNoServerVersion instead, so a caller tells no version
// from a server before 2.6.2 without reading the connection again (addBucketStream). No request is
// sent once ctx has ended.
// It answers whether it sent a STREAM.CREATE, which its caller carries across the sends of one
// create (openKeyValue, ErrStreamCreateSent): a create sent and left unanswered may have landed,
// whatever the send after it answers.
func createBucketStream(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, config *nats.KeyValueConfig) (bool, error) {
	stream, sent, err := addBucketStream(ctx, conn, js, config)
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
	return sent, err
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
//
// The server's version is read twice, where nats.go reads it. The first read, before anything is
// sent, refuses a server a bucket cannot take: a connection that reports no version - not connected,
// reconnecting - ends the call with ErrNoServerVersion, and a connected server before 2.6.2 with
// nats.go's own error (errKeyValueServerTooOld), so a caller tells the two apart without reading the
// connection again. The second read, after the account's info, chooses the stream's discard policy,
// and a connection can drop and start reconnecting between the two: one that is not connected there
// reports no version, which nats.go reads as a server too old for a discard policy of new and so
// nothing more. Each read is before every send, so a call that ends at one sent nothing; the
// second return value says whether this call sent its STREAM.CREATE, and a send the server left
// unanswered may still have landed.
func addBucketStream(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, config *nats.KeyValueConfig) (*nats.StreamConfig, bool, error) {
	version, err := ServerVersion(conn)
	if err != nil {
		return nil, false, err
	}
	if !natsServerMinVersion(version, 2, 6, 2) {
		return nil, false, errKeyValueServerTooOld
	}
	if config == nil {
		return nil, false, nats.ErrKeyValueConfigRequired
	}
	if !natsBucketName.MatchString(config.Bucket) {
		return nil, false, nats.ErrInvalidBucketName
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if _, err := js.AccountInfo(); err != nil {
		return nil, false, err
	}
	version, err = ServerVersion(conn)
	if err != nil {
		return nil, false, err
	}
	stream, err := bucketStreamConfig(config, version)
	if err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return stream, false, err
	}
	_, err = js.AddStream(stream)
	return stream, true, err
}

// ErrNoServerVersion is a connection that reports no server version where a configuration is
// chosen by the version (ServerVersion).
var ErrNoServerVersion = errors.New("the connection reports no server version, as one that is not connected does")

// ErrStreamCreateSent marks the error of a create that sent a STREAM.CREATE whose answer it never
// read: the stream may be there. openKeyValue wraps a failed create with it when a send of that
// call ended unanswered or on a connection that dropped under it, so a caller says what the hub
// may hold rather than that nothing was created. A create the server answered, with a refusal
// included, carries no mark: a refusal made no stream. A create the server left unanswered is sent
// again (sentAgain), and the send after it can end before it sends anything, such as on a
// connection that is reconnecting (ErrNoServerVersion).
var ErrStreamCreateSent = errors.New("a STREAM.CREATE of this create was sent, and may have landed")

// ServerVersion is the version of the server conn is connected to, for a read that chooses a
// configuration by it: the stream a bucket's create makes (addBucketStream) and the one
// `envoy-infra buckets create` judges a bucket against (bus.CopiedBucketStreamConfig). A connection
// that is not connected - reconnecting, or closed - reports none (nats.go@v1.55.0
// nats.go:2820-2832), and nats.go and natsServerMinVersion read none as a version older than any
// minimum, so a configuration chosen by it would be one made for a server older than any the
// deployment runs. No version is ErrNoServerVersion instead.
func ServerVersion(conn *nats.Conn) (string, error) {
	if version := conn.ConnectedServerVersion(); version != "" {
		return version, nil
	}
	return "", ErrNoServerVersion
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
