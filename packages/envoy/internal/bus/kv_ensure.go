// A start's open of a KV bucket it needs before it can serve (EnsureKeyValue): the lookup, the
// create or the raise of the bucket's stream, and the waits that follow each, until the bucket
// answers reads and writes.

package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// namedRequest is err, a JetStream request's answer, with request, the request that drew it, in
// front of it: `KV_envoy_webhook_redelivery STREAM.INFO: context deadline exceeded`. A caller
// wraps the whole of EnsureKeyValue in one line of its own (`open envoy_webhook_redelivery KV
// bucket: ...`, internal/dispatch/redeliver/sweep.go:117), and that line names the bucket but not
// which of the bucket's requests went unanswered, which is the one thing a start that timed out
// is read for. The answer is wrapped with %w, so every errors.Is through the name still reads the
// server's own error: EnsureKeyValue's switch reads nats.ErrBucketNotFound and
// nats.ErrStreamNameAlreadyInUse through it, and so does a caller of its own. nil is returned as
// it is, so a call that answered is named nothing.
func namedRequest(request string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", request, err)
}

// EnsureKeyValue opens the KV bucket config names on conn, through js, creating it with config when
// it does not exist. config.Replicas is the deployment's replica count (ReplicasEnvVar): a new
// bucket is created at it, and an existing bucket with fewer copies is raised to it
// (raiseStream, after the read of the bucket's stream every start makes of a bucket it finds).
// A refused create or raise of more than one copy names the setting
// (refusedSettings), and is returned as soon as the server answers it: nothing a server refuses is
// waited out. A bucket a ConnectOwningStream client names (WithKeptBuckets) is raised to
// the deployment's count again by that client's keeper from then on, which also ends a lowering by
// a concurrent start with a lower count; the count holds while every program of the deployment
// carries the same one.
//
// A bucket this start created, and one a concurrent start created while this one looked (the
// server answers that create "stream name already in use"), is waited for on the member answering
// before the handle is opened on it (awaitBucketApplied), since a clustered create is answered
// before every member has applied it. A bucket this start raised is waited for until its copies
// are current (awaitReplicasCurrent). A bucket this start created at more than one copy, one a
// concurrent start created, and one it found with no leader, at any copy count of its own, are
// waited for until the stream has a leader (awaitStreamLeader), since a write to a stream with no
// leader draws no answer.
//
// Every wait of this call is on the cluster, and ends on what the cluster says: the fact it waits
// for, or the server's own error, which is returned at once. Nothing else ends it but ctx, the
// caller's context, or the start's hang guard (startHangGuard), which ctx is bounded by here: a
// cluster electing a leader, or a member catching up, takes as long as the machine makes it take,
// and no shorter clock may decide that a start failed. The error at the end of ctx or of the guard
// names the step that was waiting and what it last read, with ctx's cause wrapped
// (context.DeadlineExceeded, context.Canceled or the guard's errHangGuard).
//
// A lookup or a create the server leaves unanswered is sent again rather than waited out for
// longer (sentAgain): nats.go sends each request once, and a member answers neither while its
// metadata group is younger than lostQuorumInterval or while it has not yet seen its metadata
// leader go, and a member that is not an existing bucket's stream leader leaves its lookup
// unanswered while that stream's leader is gone (sentAgain lists the paths). The create of a
// bucket of more than one copy is answered only once its stream's group has elected its first
// leader, so on a cluster whose election is slow every send of it can run out unanswered until
// then. A member that knows it has no leader answers an error instead, which unanswered() does
// not match and this call still ends on. Every production caller opens its JetStream context with
// nats.MaxWait(10 * time.Second) (brokerTokens.open in cmd/listener/brokerauth.go, cistore.Open,
// session.OpenSessionRegistry, store.Open, and the client's own context, Client.install), and
// legionagents.Open opens its own with nats.MaxWait(openTimeout), the same ten seconds; the
// known-session bucket's open waits 2 s (internal/session/known.go, knownBucketOpenWait). That
// wait bounds one send and not one step of this call: a step is sent again until it is answered,
// ctx ends or the guard passes, and a send in flight then is abandoned with its answer dropped
// (Bounded). So where the server answers nothing at all, the call costs startHangGuard and ends
// naming the step that drew no answer and how many sends it made.
//
// A request the server refuses under the connection's grant is not sent again, since it would be
// refused again: the server answers such a request with nothing and reports the refusal on the
// connection instead, where the client that owns conn records it (recordPublishDenial), and the
// send reads that record rather than take the silence for a member still working (refusedBy).
// A step whose request is refused therefore costs one send and ends with an error naming
// ErrPublishDenied and the subject, `KV_legion_agents STREAM.INFO: bus: NATS denied the publish
// to "$JS.API.STREAM.INFO.KV_legion_agents": ...`, where it would otherwise be sent again until
// the hang guard passed. That reading needs the client conn belongs to: a connection opened
// outside a Client records nothing, so a request of its own the server refuses is sent again
// until ctx ends or the guard passes.
//
// The error names the bucket's stream and the step that drew it:
// `KV_envoy_webhook_redelivery STREAM.INFO: ...` for the lookup of the bucket, which this call
// makes first and makes again while it waits a member out, and
// `KV_envoy_webhook_redelivery create: ...` for the create. A name is the step and not the
// requests inside it, because which requests a send of the create makes depends on the answers it
// draws (the comment on the create below lists them): a list of them would name requests the
// server answered and leave out the one it did not (namedRequest). The read of a found bucket's
// stream names the step it serves and the request, `read KV_<bucket>: KV_<bucket> STREAM.INFO:
// ...` at one copy and `raise KV_<bucket>: KV_<bucket> STREAM.INFO: ...` above it; a raise names
// itself (raiseStream), and a refused create names the setting behind it after the step:
// `KV_envoy_interests create: ENVOY_NATS_REPLICAS=3: ...`.
//
// The handle open this function ends with is sent again too when the server leaves it unanswered
// (wrapKeyValue, sentAgain), on both of its paths: a bucket this start created and one it found
// already there, which is the ordinary start, a restart of a deployment whose buckets exist.
// EnsureKeyValue is how a program opens a bucket it needs before it can serve: the listener's
// start, where losing that answer ends the program for the buckets it must hold before it
// serves, while the legion agent registry's open logs one ERROR, leaves the registry
// unavailable and is retried in the background (openLegionAgentRegistry), and Dispatch's
// webhook redelivery sweep (redeliver.OpenState), which logs the failure and tries again on its
// next pass. A caller that waits on a budget of its own, such as a request or a watcher's
// repair, opens a bucket through OpenKeyValue or WrapKeyValue instead, which send it once.
//
// ctx is the context the caller runs under, so a stop that ends it ends a start that waits: the
// listener's start, which a signal ends at once, runs under no other; the legion agent registry's
// retry passes the retry's context, and the webhook redelivery sweep the context of its pass.
//
// Its lines, the WARNs of its waits and of a request it sends again, go where the client that owns
// conn writes, and where slog.Default() points at each line when no client owns it (loggerOwning).
func EnsureKeyValue(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, config *nats.KeyValueConfig) (KeyValue, error) {
	return ensureKeyValue(ctx, loggerOwning(conn), startHangGuard, conn, js, config)
}

// ensureKeyValue is EnsureKeyValue with guard, its hang guard, named by the caller: a test of the
// guard shortens it, and every other caller is EnsureKeyValue, which passes startHangGuard. Its
// lines go to logger.
func ensureKeyValue(ctx context.Context, logger *slog.Logger, guard time.Duration, conn *nats.Conn, js nats.JetStreamContext, config *nats.KeyValueConfig) (KeyValue, error) {
	ctx, cancel := hangGuard(ctx, guard)
	defer cancel()
	// One run of reads for every read of this open, so a delay one step measured holds for the
	// next (withReadLimit).
	ctx = withReadLimit(ctx)
	stream := "KV_" + config.Bucket
	lookup := stream + " STREAM.INFO"
	create := stream + " create"
	// Every request of this call, the lookup, the create, the read of a found bucket's stream, its
	// raise and the reads of the waits below, goes through js read for the server's refusal
	// (readsRefusals), so a request the grant refuses ends its step at once naming the refusal.
	js = readsRefusals(conn, js)
	// lookUpBucket is the one lookup this call makes, here and again while it waits a member out,
	// so the two are the same request, and it names its own error rather than leave each caller
	// to name it. The name wraps the server's answer with %w (namedRequest), so awaitBucketApplied
	// still reads the member's "bucket not found" through it (bucketMissing) and the switch below
	// still reads nats.ErrBucketNotFound.
	lookUpBucket := func() (nats.KeyValue, error) {
		kv, err := readAgain(ctx, logger, lookup, readLimitOf(ctx), func() (nats.KeyValue, error) { return js.KeyValue(config.Bucket) })
		return kv, namedRequest(lookup, err)
	}
	kv, err := lookUpBucket()
	created, createdElsewhere := false, false
	switch {
	case errors.Is(err, nats.ErrBucketNotFound):
		// A create the server leaves unanswered is sent again, where an unanswered update is not
		// (updateStream, internal/bus/stream.go). A second identical create finds the
		// stream assignment the first one proposed, or left inflight, compares the two
		// configurations and, when they are equal, proposes that same assignment again, so the
		// answer the first send never drew goes out (server/jetstream_cluster.go:11053-11063,
		// :11109-11116). A create the server does answer "stream name already in use" is settled
		// by the branch below, which reads the bucket until the member answering has applied it.
		//
		// Sending the step again sends more than the create: the step is nats.go's
		// CreateKeyValue made one request at a time (createBucketStream), which asks the account
		// for its JetStream info first and, on "stream name already in use", reads the stream and,
		// when the stream's configuration equals the one asked for, sends a STREAM.UPDATE of it.
		// That update goes again with the rest of the step where updateStream leaves an
		// unanswered update alone, because it carries the configuration the stream already holds:
		// it changes nothing when it lands twice, while the replica or age change updateStream
		// guards is a write another one can race. A request of the step the server refuses under
		// the connection's grant ends the step at that send instead (refusedBy): it would be
		// refused again.
		_, err = sentAgain(ctx, logger, create, func() (struct{}, error) { return struct{}{}, createBucketStream(conn, js, config) })
		switch {
		case err == nil || errors.Is(err, nats.ErrStreamNameAlreadyInUse):
			// The stream is there, and the member answering may not have applied its create:
			// the create's answer came from the metadata group. So the bucket is looked up until
			// that member holds it, which is also how the start gets its handle on the bucket,
			// since nats.go builds a handle only from a lookup or its own create.
			//
			// "Stream name already in use" says the bucket was created between this start's
			// lookup and its create, by another start of this deployment. The copies are not
			// raised here, as they are for a bucket this start found: the start that created it
			// just now carries the deployment's count too. That start may carry more copies than
			// this one, so the leader is waited for below whatever this start's count is.
			//
			// Each of those lookups is sent again when the server leaves it unanswered
			// (sentAgain). A bucket the member has not applied and a request that drew no answer
			// are two different answers, and each is waited out in its own way: the first is read
			// again until the member holds it, the second is sent again. A lost send here would
			// end the start the create above has just carried through.
			createdElsewhere = err != nil
			kv, err = awaitBucketApplied(ctx, logger, config.Bucket, bucketMissing, lookUpBucket)
		case unanswered(err) || ctx.Err() != nil:
			// No answer, or the wait ended: the server refused nothing, so no setting is named.
			err = namedRequest(create, err)
		default:
			err = namedRequest(create, refusedSettings(err, nil, &nats.StreamConfig{Replicas: config.Replicas}))
		}
		created = err == nil
	case err == nil:
		// The bucket is there: its stream is read for its copies and its leader, sent again while
		// the server leaves the read unanswered as the lookup is (nats.go's lookup answers a
		// handle, not the stream's info), and raised when it has fewer than the deployment's
		// count. The read is made at one copy too: a start at one copy can find a stream another
		// program, configured with more, has just raised, whose new group has no leader yet
		// (leaderless below). The raise is a write the server answers at once, sent again while
		// the server leaves it unanswered and bounded by ctx: a raise in flight when ctx ends is
		// abandoned (updateStream), and a raise that lands after that is one the keeper would have
		// made. A start at one copy raises nothing (raiseStream).
		step := "read"
		if config.Replicas > 1 {
			step = "raise"
		}
		var info *nats.StreamInfo
		info, err = readAgain(ctx, logger, lookup, readLimitOf(ctx), func() (*nats.StreamInfo, error) { return js.StreamInfo(stream) })
		if err != nil {
			err = fmt.Errorf("%s %s: %w", step, stream, namedRequest(lookup, err))
			break
		}
		before, raiseErr := raiseStream(ctx, logger, js, info, config.Replicas)
		switch {
		case raiseErr != nil:
			err = raiseErr
		case before > 0:
			// The copies this raise added are behind until they have caught up with the bucket's
			// stream, and a KV read is a direct get, which nats-server hands a copy the queue
			// group at 90% caught up rather than when it is current (server/jetstream_cluster.go:
			// 2622-2641, v2.10.29): a read taken now can be answered "key not found" by a copy
			// that does not hold the key yet. The caller reads keys as soon as this call answers
			// (the webhook redelivery sweep reads its cursor and its index), and a stale miss
			// there asks GitHub for a delivery it has already redelivered, so the start waits for
			// the copies and fails rather than reading early.
			err = awaitReplicasCurrent(ctx, logger, js, stream, config.Replicas)
		case leaderless(info):
			// Another program raised or created the bucket's stream just before this start read
			// it, and the stream's new group has not elected its leader yet: a write sent now draws
			// no answer. The start waits for the leader, the fact another program's own start
			// waits for, and not for every copy current, so a bucket with a member offline still
			// opens (awaitStreamLeader).
			err = awaitStreamLeader(ctx, logger, js, afterOpen, stream)
		}
	}
	if err != nil {
		return KeyValue{}, err
	}
	handle, err := wrapKeyValue(ctx, conn, kv, created, startSendsAgain)
	if err == nil && created && (config.Replicas > 1 || createdElsewhere) {
		// A create of more than one copy can be answered before the bucket's stream has a leader:
		// the first send of it is answered at the stream's first leader election, but a send made
		// again while that election runs is an equal create, which the server answers at once
		// (measured with nats-server v2.15.1, the election held 11 s and 30 s: the second send of
		// a create through a 2 s wait answered, and the first write to the bucket then failed
		// "no response from stream"). The caller writes as soon as this call answers, so the
		// start waits for the stream's leader here (awaitStreamLeader), and for a create another
		// start made, whose count this start does not know, at one copy too. This read follows
		// the handle open, which has waited for the member to apply the create.
		err = awaitStreamLeader(ctx, logger, js, afterCreate, stream)
	}
	if err != nil {
		return KeyValue{}, err
	}
	return handle, nil
}

// awaitBucketApplied is read, a read of the bucket a start has just created, taken again every
// bucketAppliedInterval while the member answering reports the bucket missing (missing) and ctx
// lasts. It returns the first answer that is not that, and once ctx ends the last miss with ctx's
// cause wrapped beside it, so its caller wraps one error however the wait ended: between two reads,
// or during a read that ctx cut. Any other error, a refusal included, is returned at once: only a
// member that has not caught up is waited out. The first miss is logged to logger, so a start that
// waited says which bucket it waited for.
func awaitBucketApplied[T any](ctx context.Context, logger *slog.Logger, bucket string, missing func(error) bool, read func() (T, error)) (T, error) {
	var lastMiss error
	for attempt := 1; ; attempt++ {
		value, err := read()
		if err != nil && !missing(err) && lastMiss != nil && ctx.Err() != nil {
			return value, fmt.Errorf("%w; the member had not applied the bucket when the wait ended: %w", lastMiss, err)
		}
		if err == nil || !missing(err) {
			return value, err
		}
		lastMiss = err
		if attempt == 1 {
			logger.Warn("envoy nats kv bucket not applied yet, waiting",
				slog.String("bucket", bucket),
				slog.String("error", err.Error()),
				slog.String("retry_in", bucketAppliedInterval.String()))
		}
		if ended := pause(ctx, bucketAppliedInterval); ended != nil {
			return value, fmt.Errorf("%w; the member had not applied the bucket when the wait ended: %w", err, ended)
		}
	}
}

// bucketMissing reports whether err is a member's answer that the bucket's stream is not there,
// however the legacy or the jetstream package names it: the answer a member that has not applied
// a create of it gives (bucketAppliedInterval).
func bucketMissing(err error) bool {
	return errors.Is(err, nats.ErrBucketNotFound) || errors.Is(err, jetstream.ErrBucketNotFound) ||
		errors.Is(err, nats.ErrStreamNotFound) || errors.Is(err, jetstream.ErrStreamNotFound)
}

// raiseReplicas raises the stream of the existing bucket to replicas copies when it has fewer, and
// returns the copies it had then, 0 when it left the bucket alone: the keeper's raise, made again
// at its next pass (raiseStream). Its read of the stream is a read of a stream's state, sent again
// while the server leaves it unanswered, each send up to its read limit, for as long as ctx lasts
// (readAgain), as updateStream's read after an update is: a cluster that answers no read within
// the JetStream context's wait has said nothing of the bucket, so a read sent once would log a
// raise that failed over a question nobody answered. One copy needs no read: every stream has at
// least one.
func raiseReplicas(ctx context.Context, logger *slog.Logger, js nats.JetStreamContext, bucket string, replicas int) (int, error) {
	if replicas <= 1 {
		return 0, nil
	}
	stream := "KV_" + bucket
	info, err := readAgain(ctx, logger, stream+" STREAM.INFO", readLimitOf(ctx), func() (*nats.StreamInfo, error) { return js.StreamInfo(stream) })
	if err != nil {
		return 0, fmt.Errorf("raise %s: %w", stream, err)
	}
	return raiseStream(ctx, logger, js, info, replicas)
}

// raiseStream raises the stream info describes to replicas copies when it has fewer, and returns
// the copies it had then, 0 when it left the stream alone. The update writes back the stream's
// deployed configuration with only its replicas changed, so the bucket's TTL, history, discard
// policy, direct get, rollup, delete settings, metadata and the rest stay as they are. A stream
// with as many copies or more is left alone. As for the notification stream
// (ensureStreamWithConfig), two starts raising at once each write the deployment's one count. A
// raise whose answer never came succeeds when the stream, read again, has the copies, and is sent
// again, raised from that read, while it does not; one the server refused as overlapping the
// bucket's own stream is sent again until the stream has them or its sends run out
// (updateStream). Its error names the stream and, for a refused raise, the setting
// (refusedSettings): `raise KV_envoy_interests: ENVOY_NATS_REPLICAS=3: ...`, so the keeper's line
// says which bucket it could not raise.
func raiseStream(ctx context.Context, logger *slog.Logger, js nats.JetStreamContext, info *nats.StreamInfo, replicas int) (int, error) {
	if info.Config.Replicas >= replicas {
		return 0, nil
	}
	raise := func(current *nats.StreamInfo) (nats.StreamConfig, bool) {
		raised := current.Config
		raised.Replicas = replicas
		return raised, true
	}
	update, err := updateStream(ctx, logger, js, info, raise, func(again *nats.StreamInfo) bool { return again.Config.Replicas >= replicas })
	if err != nil {
		return 0, fmt.Errorf("raise %s: %w", info.Config.Name, refusedSettings(err, &update.from.Config, &update.sent))
	}
	return info.Config.Replicas, nil
}

// awaitReplicasCurrent waits until stream reports replicas copies, each one current with its
// leader and none of them offline or lagging (copiesBehind), for as long as ctx lasts.
// EnsureKeyValue runs it on a bucket's stream after a raise that added copies, and nothing else
// needs it: a bucket nobody raised has no copy that is behind.
//
// A raise of a one-copy stream is answered at once, by the member that held its one copy
// (processClusterUpdateStream, "If we were a single node being promoted", v2.15.1
// server/jetstream_cluster.go), and the stream then has no leader until the new group elects one:
// that election is what this waits out, with the new copies' catch-up after it. The end of ctx is
// an error naming the bucket's stream, the copies it asked for, what each copy that was not
// current had to say and ctx's cause, the way a refused raise names its setting, and the caller
// fails its start on it. Carrying on instead would hand the caller a bucket whose direct gets a
// copy that is 90% caught up may answer "key not found" for a key that is there
// (server/jetstream_cluster.go:2622-2641, v2.10.29), and a start that reads one wrong answer is
// worse than a start that does not come up: the webhook redelivery sweep reads its records the
// moment this call answers, and a stale miss there asks GitHub for a redelivery it already made.
func awaitReplicasCurrent(ctx context.Context, logger *slog.Logger, js nats.JetStreamContext, stream string, replicas int) error {
	return awaitStream(ctx, logger, js, afterRaise, stream, fmt.Sprintf("%d copies were not current", replicas), "envoy nats kv raised copies not current yet, waiting", func(info *nats.StreamInfo) string {
		return copiesBehind(info, replicas)
	})
}

// copiesBehind says what keeps the stream info describes short of replicas copies that are all
// current, and is empty when none does: the stream has a leader, as many copies as were asked for,
// and every copy of it is current, online and not lagging. Each copy that is not is named with
// what it reported, so a start that gave up says which member it waited for.
func copiesBehind(info *nats.StreamInfo, replicas int) string {
	if info.Cluster == nil || info.Cluster.Leader == "" {
		return "the stream has no leader yet"
	}
	var behind []string
	if copies := 1 + len(info.Cluster.Replicas); copies != replicas {
		behind = append(behind, fmt.Sprintf("%d copies of %d", copies, replicas))
	}
	for _, peer := range info.Cluster.Replicas {
		switch {
		case peer.Offline:
			behind = append(behind, peer.Name+" offline")
		case !peer.Current:
			behind = append(behind, fmt.Sprintf("%s not current, lag %d", peer.Name, peer.Lag))
		case peer.Lag > 0:
			behind = append(behind, fmt.Sprintf("%s lag %d", peer.Name, peer.Lag))
		}
	}
	return strings.Join(behind, ", ")
}

// SetKeyValueMaxAge sets the maximum age of the existing bucket's stream to maxAge when it carries
// another, and returns the age the bucket carries once it has: maxAge, whether it was written now
// or already there, and 0 only when no age was asked for and the bucket was left alone - so a
// caller that must know the age it is expiring against is answered by this call and makes no
// second read of its own. As raiseStream does for the copies, the update writes back the
// stream's deployed configuration with only its MaxAge changed, so the bucket's replicas,
// history, discard policy, direct get, rollup, delete settings, metadata and the rest stay as
// they are. A non-positive maxAge sets none: a caller whose setting is unset leaves the bucket's
// own age standing (ParseSessionMaxAge), as it leaves one copy alone.
// An age an answer never came for succeeds when the stream, read again, carries it, and is sent
// again, set on that read, while it does not; one the server refused as overlapping the bucket's
// own stream is sent again (updateStream). Its error names the bucket's stream:
// `set KV_envoy_sessions max age: ...`.
//
// The session registry's start makes this call right after it opens the bucket, and a failed call
// fails that start, so it waits on the cluster as EnsureKeyValue does: the read is sent again
// while the server leaves it unanswered (sentAgain), and the read and the update end when ctx
// ends or the start's hang guard passes (startHangGuard), the update in flight then abandoned
// (updateStream). The session reaper also makes this call on each pass, under a context of one
// RequestTimeout, since a pass has no start behind it and its next tick asks again. Its lines go
// where EnsureKeyValue's go: to the logger of the client that owns conn (loggerOwning).
func SetKeyValueMaxAge(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, bucket string, maxAge time.Duration) (time.Duration, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	ctx, cancel := hangGuard(ctx, startHangGuard)
	defer cancel()
	ctx = withReadLimit(ctx)
	js = readsRefusals(conn, js)
	logger := loggerOwning(conn)
	stream := "KV_" + bucket
	read := stream + " STREAM.INFO"
	info, err := readAgain(ctx, logger, read, readLimitOf(ctx), func() (*nats.StreamInfo, error) { return js.StreamInfo(stream) })
	if err != nil {
		return 0, fmt.Errorf("set %s max age: %w", stream, namedRequest(read, err))
	}
	if info.Config.MaxAge == maxAge {
		return maxAge, nil
	}
	age := func(current *nats.StreamInfo) (nats.StreamConfig, bool) {
		aged := current.Config
		aged.MaxAge = maxAge
		return aged, true
	}
	if _, err := updateStream(ctx, logger, js, info, age, func(again *nats.StreamInfo) bool { return again.Config.MaxAge == maxAge }); err != nil {
		return 0, fmt.Errorf("set %s max age: %w", stream, err)
	}
	return maxAge, nil
}

// RaiseMaxAge raises the stream of the existing bucket to keep a key's latest value for age when
// it keeps it for less. A bucket EnsureKeyValue creates is created with the age in its config;
// this is for one an earlier build created with a shorter one, since a create that finds the
// bucket there changes nothing. A bucket that keeps values as long or longer is left alone, and
// so is one that keeps them forever, which JetStream writes as a MaxAge of 0: an operator's
// longer age stands, unlimited included. As SetKeyValueMaxAge does, the update writes back the
// stream's deployed configuration with only its MaxAge changed, so the bucket's replicas,
// history, discard policy, direct get, rollup, delete settings, metadata and the rest stay as
// they are; an age an answer never came for succeeds when the stream, read again, keeps values as
// long, and is sent again, raised from that read, while it does not (updateStream). Its error
// names the bucket's stream: `raise KV_envoy_listener's max age: ...`.
//
// A start makes this call right after it opens the bucket, so it waits on the cluster as
// SetKeyValueMaxAge does: the read is sent again while the server leaves it unanswered (readAgain),
// a request the grant refuses ends it at once (readsRefusals), and the read and the update end
// when ctx ends or the start's hang guard passes (startHangGuard), the update in flight then
// abandoned. Its lines go where SetKeyValueMaxAge's go: to the logger of the client that owns conn
// (loggerOwning).
func RaiseMaxAge(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, bucket string, age time.Duration) error {
	ctx, cancel := hangGuard(ctx, startHangGuard)
	defer cancel()
	ctx = withReadLimit(ctx)
	js = readsRefusals(conn, js)
	logger := loggerOwning(conn)
	stream := "KV_" + bucket
	read := stream + " STREAM.INFO"
	enough := func(config *nats.StreamConfig) bool { return config.MaxAge == 0 || config.MaxAge >= age }
	info, err := readAgain(ctx, logger, read, readLimitOf(ctx), func() (*nats.StreamInfo, error) { return js.StreamInfo(stream) })
	if err != nil {
		return fmt.Errorf("raise %s's max age: %w", stream, namedRequest(read, err))
	}
	if enough(&info.Config) {
		return nil
	}
	raise := func(current *nats.StreamInfo) (nats.StreamConfig, bool) {
		raised := current.Config
		raised.MaxAge = age
		return raised, true
	}
	if _, err := updateStream(ctx, logger, js, info, raise, func(again *nats.StreamInfo) bool { return enough(&again.Config) }); err != nil {
		return fmt.Errorf("raise %s's max age: %w", stream, err)
	}
	return nil
}
