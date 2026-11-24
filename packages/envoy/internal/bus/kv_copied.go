package bus

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"weak"

	"github.com/nats-io/nats.go"
)

// fixtureConns are the connections a test fixture marked as its own (CreateCopiedBucketsOn), each
// held by a weak pointer, so a connection a test is done with is collected with all it references,
// and its entry removed then, rather than kept for the rest of the test process.
var fixtureConns sync.Map

// CreateCopiedBucketsOn marks conn as a test fixture's connection to a server of its own: an open
// through it (EnsureKeyValue) creates a copied bucket it finds missing, with the caller's
// configuration, as it creates every other bucket. Only a test's fixture calls it
// (internal/bus/bustest); no program of a deployment does, so no listener creates a bucket it
// copies.
func CreateCopiedBucketsOn(conn *nats.Conn) {
	key := weak.Make(conn)
	if _, marked := fixtureConns.LoadOrStore(key, struct{}{}); !marked {
		runtime.AddCleanup(conn, func(key weak.Pointer[nats.Conn]) { fixtureConns.Delete(key) }, key)
	}
}

// ErrBucketMissing is a bucket a listener copies that an open found missing (EnsureKeyValue). It
// wraps nats.ErrBucketNotFound, so a caller that reads that error reads this one too.
var ErrBucketMissing = fmt.Errorf("bucket is missing; only `envoy-infra buckets create` creates it, with `--break-glass` until the auth service's cutover, and needs no writer stopped: %w", nats.ErrBucketNotFound)

// ErrBucketExists is a bucket a create-only open found there (CreateMissingCopiedBucket): at its
// lookup, or in the answer "stream name already in use" to its create, when another client made
// the bucket between the two. The open leaves it as it is.
var ErrBucketExists = errors.New("bucket exists; a create-only open creates only a missing bucket, and leaves one that is there as it is")

// CreateCopiedBucket creates bucket, one a listener copies, with its configuration at replicas
// copies (CopiedBucketConfig), or opens it as it is when it exists: `envoy-infra buckets
// recreate`'s create after its delete, and a test fixture's (internal/bus/bustest), on a server of
// its own. Its waits end with ctx, the caller's, or at the start's hang guard (EnsureKeyValue).
func CreateCopiedBucket(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, bucket string, replicas int) (KeyValue, error) {
	return openCopiedBucket(ctx, conn, js, bucket, replicas, openOrCreate)
}

// CreateMissingCopiedBucket creates bucket, one a listener copies, with its configuration at
// replicas copies (CopiedBucketConfig), only when the server does not hold it: a bucket its lookup
// finds, or one another client makes between that lookup and its create, is ErrBucketExists, and is
// neither opened, raised nor updated, and it sends no STREAM.UPDATE on any server (createOnly). As
// CreateCopiedBucket does, it sends a lookup or a create the server leaves unanswered again
// (sentAgain), looks the bucket up until the member answering has applied the create, and opens the
// handle it answers with (awaitBucketApplied, wrapKeyValue). Its waits end with ctx, and it sends
// no request once ctx has ended (addBucketStream). `envoy-infra buckets create` calls it.
func CreateMissingCopiedBucket(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, bucket string, replicas int) (KeyValue, error) {
	return openCopiedBucket(ctx, conn, js, bucket, replicas, createOnly)
}

// openCopiedBucket opens bucket, one a listener copies, with its configuration at replicas copies
// (CopiedBucketConfig), as mode says: CreateCopiedBucket's and CreateMissingCopiedBucket's one body.
func openCopiedBucket(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, bucket string, replicas int, mode bucketOpen) (KeyValue, error) {
	config, ok := CopiedBucketConfig(bucket, replicas)
	if !ok {
		return KeyValue{}, fmt.Errorf("%s is no bucket a listener copies", bucket)
	}
	return openKeyValue(ctx, startHangGuard, conn, js, &config, mode)
}

// CopiedBucketStreamConfig is the stream a create of bucket, one a listener copies, at replicas
// copies makes on a server reporting serverVersion: CopiedBucketConfig made into a stream as
// nats.go's CreateKeyValue makes it (bucketStreamConfig), so its subjects, discard policy and
// direct get among the rest. `envoy-infra buckets create` compares a bucket the server holds against
// it. It is false when bucket is no copied bucket.
func CopiedBucketStreamConfig(bucket string, replicas int, serverVersion string) (nats.StreamConfig, bool) {
	config, ok := CopiedBucketConfig(bucket, replicas)
	if !ok {
		return nats.StreamConfig{}, false
	}
	// bucketStreamConfig refuses only a history above nats.KeyValueMaxHistory, which no copied
	// bucket's configuration holds (TestEveryCopiedBucketHasAStreamConfig).
	stream, err := bucketStreamConfig(&config, serverVersion)
	if err != nil {
		return nats.StreamConfig{}, false
	}
	return *stream, true
}
