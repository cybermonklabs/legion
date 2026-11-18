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

// KeyValue is a KV handle that checks a key before nats.go builds a subject from it, since nats.go
// checks only the characters a key may hold: a key whose subject would reach past maxSubjectBytes is
// ErrTooLarge, and one NATS would not accept in a subject (an empty token, `a..b`, which nats.go
// allows, or whitespace) is ErrInvalidSubject, each refused before anything is sent. nats.go's own
// refusal of a key outside its alphabet is named the ErrInvalidKey it is (invalidKey), so every
// key a call refuses however often it is named is an ErrRefused. A write (Put, PutString, Create,
// Update) is held to the longest subject any call on its key builds, a watcher's create request, so
// a key written through it stays readable, watchable and deletable. Any other call is held only to
// the subject it builds itself, so a key an earlier build stored past the write bound still lists
// and deletes, and still reads while its get fits: a store serves it or skips it, and a reaper
// removes it.
//
// Every read and write goes through the bucket's legacy nats.KeyValue, except what nats.go runs as
// a watcher and the reads beside them: a watch of the bucket (WatchKeys), a key's history
// (ReadHistory) and its stream's state (ReadStreamState) go through the same bucket's
// jetstream.KeyValue instead, and the leader-answered read of one key (GetFromLeader) makes
// nats.go's legacy GetLastMsg call on that jetstream context's connection. nats.go's legacy KV
// watcher, which its legacy Watch, History and Keys all run, arms an init timer while its scan is
// pending whose callback can send on the channels the subscription's end has just closed, a "send
// on closed channel" panic in a goroutine nats.go owns (the `initDoneTimer.Reset` of
// `(*kvs).WatchFiltered`, unfixed through v1.55.0), while the jetstream package's watcher has no
// such timer and closes its updates only on the subscription goroutine's own exit (its own
// `(*kvs).WatchFiltered`). So the legacy handle is held unexported and KeyValue forwards only the
// calls it checks: nothing can reach the legacy handle's Watch, WatchAll, WatchFiltered, History,
// Keys or ListKeys through a KeyValue, and a call to one does not compile. A legacy handle cannot
// name the connection it was opened on, so every constructor takes it.
//
// EnsureKeyValue and OpenKeyValue are how the listener and Dispatch open every bucket, and
// WrapKeyValue pairs a legacy handle a caller already holds with its connection. A test that
// injects faults builds its handle with NewKeyValue, from the legacy handle (Legacy), the jetstream
// one (WatchHandle) and the jetstream context (Streams) it wraps. A zero KeyValue has none of them
// and is a programming error: every call on it panics.
type KeyValue struct {
	// legacy is the bucket's legacy handle, which every checked read and write goes through. It is
	// not embedded, so none of its watcher calls is promoted.
	legacy nats.KeyValue
	// watch is the same bucket as a jetstream handle, the one every watch, history and stream read
	// goes through.
	watch jetstream.KeyValue
	// streams is the same connection's jetstream context, which every read of the bucket's stream
	// goes through (ReadStreamState), and whose connection the leader read takes (GetFromLeader):
	// each read fetches the stream afresh. The jetstream handle's own Status stores the info it
	// read in the handle's shared stream value with no lock (the `s.info = &cached` of nats.go's
	// `(*stream).Info`, which `(*kvs).Status` calls on the stream `mapStreamToKVS` built that
	// handle over), so two reads of one handle at once -- a watcher's stream probe beside its
	// completion check -- race on it.
	streams jetstream.JetStream
	// owner is the client that owns the connection streams is on, found once when the handle is
	// built (clientOwning), whose record of the server's refusals the handle's calls read
	// (refusedOn, refusedAfterOn); nil on a connection no client owns.
	owner *Client
}

// RequestTimeout bounds one send of a read through the jetstream handle whose caller holds no
// deadline of its own: the STREAM.INFO that opens the handle, a read of the bucket's stream, a
// key's history. It is the JetStream MaxWait the listener and Dispatch open their legacy handles
// with, so a read moved off the legacy handle waits no longer than it did there, and no less,
// whatever MaxWait the legacy handle carries. A start that sends a request again because the
// server left it unanswered takes that bound once per send (sentAgain, wrapKeyValue for a
// start's caller), so the bound is one send's and not one step's: what a step of EnsureKeyValue
// costs is counted in that function's doc.
const RequestTimeout = 10 * time.Second

// A create a clustered server answers is committed to the JetStream metadata group, not applied
// by every member yet: a member that has not applied the new stream assignment answers a read of
// the bucket's stream "stream not found", the member the create was sent through included. A probe
// over 25 fresh three-member clusters asked every member for a bucket's stream the instant
// CreateKeyValue answered, and 67 of the 75 member reads still said so, the slowest for 206 ms. So
// a read of a bucket this start has just created, or that a concurrent start created while this
// one looked, is taken again every bucketAppliedInterval until the member answers for it.
//
// bucketAppliedInterval is short against that 206 ms, so the wait ends within a small fraction of
// the catch-up it waits out, and each miss is one STREAM.INFO a member answers from memory. It is
// also the pause between two sends of a request the server left unanswered (sentAgain) and between
// two reads of a raised stream's copies (awaitReplicasCurrent).
const bucketAppliedInterval = 25 * time.Millisecond

// startHangGuard bounds one step of a start that waits on a fact of the cluster the start cannot
// hurry: an answer the server gives only once a stream's group has elected a leader (the create of
// a bucket or of the stream with more than one copy, a lookup at a member while the stream's leader
// is gone), a member applying a create (awaitBucketApplied), a stream's leader after a create or a
// raise (awaitStreamLeader), or the copies of a raise becoming current (awaitReplicasCurrent).
// Each of those waits ends on the fact, on the server's own error, or when the caller's context
// ends, whichever comes first; the guard ends the wait only on a cluster that never settles, so
// that start fails with an error naming what it waited for (errHangGuard) rather than hanging with
// its port answering "starting". No shorter bound decides the result: how long an election takes
// depends on the machine and on the election's own timers.
//
// The guard is three times the slowest such wait measured under load, the sizing docs/testing.md
// gives a bound of this kind. On 2026-11-17, on a dev box with 288 CPUs at a one-minute load of
// 1,375 to 2,125, fresh three-member clusters of nats-server v2.15.1 were raised and created 64 at a
// time. In 400 raises of a one-copy bucket to three copies, the stream had a leader and every copy
// was current at most 5.1 s after the raise was answered, and a create of a three-copy bucket was
// answered, at its first election, within 3.3 s. In 160 more, whose preferred member did not
// campaign at once, the group elected on its election timers alone (4 s to 9 s,
// minElectionTimeoutDefault and maxElectionTimeoutDefault in server/raft.go), which is the election
// a group holds when the preferred member's first campaign is lost: a leader and every copy current
// came at most 11.9 s after the raise was answered (one in ten took more than 10 s), and a create
// was answered within 8.7 s. Three times 11.9 s is 36 s. A step the guard ends fails the start at
// once, well inside the listener's start-up probe of 300 s
// (infra/gcp/testdata/dispatch-listener-pod.golden).
const startHangGuard = 36 * time.Second

// errHangGuard is what a start's wait ends with, wrapped, when its hang guard passes before the
// fact it waits on holds (hangGuard).
var errHangGuard = errors.New("the start's hang guard passed")

// hangGuard is ctx bounded by guard as well. When guard passes first, ctx's cause is an error that
// wraps both errHangGuard, naming guard, and context.DeadlineExceeded, so a caller that reads a
// deadline off a start's error reads one whichever of the two ended the wait, and one that reads
// errHangGuard is told it was the guard.
func hangGuard(ctx context.Context, guard time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, guard, fmt.Errorf("%w after %s: %w", errHangGuard, guard, context.DeadlineExceeded))
}

// pause waits d out and reports nil, or reports ctx's cause when ctx ends first, which is what a
// wait between two reads of a fact ends with: the caller's context, or the start's hang guard.
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

// awaitBucketApplied is read, a read of the bucket a start has just created, taken again every
// bucketAppliedInterval while the member answering reports the bucket missing (missing) and ctx
// lasts. It returns the first answer that is not that, and once ctx ends the last miss with ctx's
// cause wrapped beside it, so its caller wraps one error however the wait ended: between two reads,
// or during a read that ctx cut. Any other error, a refusal included, is returned at once: only a
// member that has not caught up is waited out. The first miss is logged, as bindListenerDurable
// logs its first refusal, so a start that waited says which bucket it waited for.
func awaitBucketApplied[T any](ctx context.Context, bucket string, missing func(error) bool, read func() (T, error)) (T, error) {
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
			slog.Warn("envoy nats kv bucket not applied yet, waiting",
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
// nats.MaxWait(10 * time.Second) (cmd/listener/brokerauth.go:149,
// internal/cistore/cistore.go:357, internal/session/registry.go:396, internal/store/kv.go:94,
// internal/bus/nats.go:511), and internal/legionagents/store.go:75 opens its own with
// nats.MaxWait(openTimeout), the same ten seconds (store.go:29); the known-session bucket's open
// waits 2 s (internal/session/known.go, knownBucketOpenWait). That wait bounds one send and not
// one step of this call: a step is sent again until it is answered, ctx ends or the guard passes,
// and a send in flight then is abandoned with its answer dropped (Bounded). So where the server
// answers nothing at all, the call costs startHangGuard and ends naming the step that drew no
// answer and how many sends it made.
//
// A request the server refuses under the connection's grant is not sent again, since it would be
// refused again: the server answers such a request with nothing and reports the refusal on the
// connection instead, where the client that owns conn records it (recordPublishDenial), and the
// send reads that record rather than take the silence for a member still working (refusedSend).
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
func EnsureKeyValue(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, config *nats.KeyValueConfig) (KeyValue, error) {
	return ensureKeyValue(ctx, startHangGuard, conn, js, config)
}

// ensureKeyValue is EnsureKeyValue with guard, its hang guard, named by the caller: a test of the
// guard shortens it, and every other caller is EnsureKeyValue, which passes startHangGuard.
func ensureKeyValue(ctx context.Context, guard time.Duration, conn *nats.Conn, js nats.JetStreamContext, config *nats.KeyValueConfig) (KeyValue, error) {
	ctx, cancel := hangGuard(ctx, guard)
	defer cancel()
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
	lookupLimits := newReadLimit(ctx)
	lookUpBucket := func() (nats.KeyValue, error) {
		kv, err := readAgain(ctx, lookup, lookupLimits, func() (nats.KeyValue, error) { return js.KeyValue(config.Bucket) })
		return kv, namedRequest(lookup, err)
	}
	kv, err := lookUpBucket()
	created, createdElsewhere := false, false
	switch {
	case errors.Is(err, nats.ErrBucketNotFound):
		// A create the server leaves unanswered is sent again, where an unanswered update is not
		// (updateStream, internal/bus/stream.go:748-754). A second identical create finds the
		// stream assignment the first one proposed, or left inflight, compares the two
		// configurations and, when they are equal, proposes that same assignment again, so the
		// answer the first send never drew goes out (server/jetstream_cluster.go:11053-11063,
		// :11109-11116). A create the server does answer "stream name already in use" is settled
		// by the branch below, which reads the bucket until the member answering has applied it.
		//
		// Sending the step again sends more than the create: nats.go asks the account for its
		// JetStream info first (nats.go@v1.53.1 kv.go:432-434), and on "stream name already in
		// use" it reads the stream and, when the stream's configuration equals the one asked for,
		// sends a STREAM.UPDATE of it (kv.go:534-545). That update goes again with the rest of
		// the step where updateStream leaves an unanswered update alone, because it carries the
		// configuration the stream already holds: it changes nothing when it lands twice, while
		// the replica or age change updateStream guards is a write another one can race. A
		// request of the step the server refuses under the connection's grant ends the step at
		// that send instead (refusedSend): it would be refused again.
		kv, err = sentAgain(ctx, create, func() (nats.KeyValue, error) { return js.CreateKeyValue(config) })
		if errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
			// The bucket was created between this start's lookup and its create, by another start
			// of this deployment, and the member answering may not have applied that create
			// either: the lookup is taken again until it has. The copies are not raised here, as
			// they are for a bucket this start found: the start that created it just now carries
			// the deployment's count too. That start may carry more copies than this one, so the
			// leader is waited for below whatever this start's count is.
			//
			// Each of those lookups is sent again when the server leaves it unanswered
			// (sentAgain). A bucket the member has not applied and a request that drew no answer
			// are two different answers, and each is waited out in its own way: the first is read
			// again until the member holds it, the second is sent again. A lost send here would
			// end the start the create above has just carried through.
			createdElsewhere = true
			kv, err = awaitBucketApplied(ctx, config.Bucket, bucketMissing, lookUpBucket)
		} else if unanswered(err) || ctx.Err() != nil {
			// No answer, or the wait ended: the server refused nothing, so no setting is named.
			err = namedRequest(create, err)
		} else {
			err = namedRequest(create, refusedSettings(err, nil, &nats.StreamConfig{Replicas: config.Replicas}))
		}
		created = err == nil
	case err == nil:
		// The bucket is there: its stream is read for its copies and its leader, sent again while
		// the server leaves the read unanswered as the lookup is (nats.go's lookup answers a
		// handle, not the stream's info), and raised when it has fewer than the deployment's
		// count. The read is made at one copy too: a start at one copy can find a stream another
		// program, configured with more, has just raised, whose new group has no leader yet
		// (leaderless below). The raise is a write the server answers at once, and the wait is
		// bounded by ctx: a raise in flight when ctx ends is abandoned (Bounded), and a raise that
		// lands after that is one the keeper would have made. A start at one copy raises nothing
		// (raiseStream).
		step := "read"
		if config.Replicas > 1 {
			step = "raise"
		}
		var info *nats.StreamInfo
		info, err = readAgain(ctx, lookup, lookupLimits, func() (*nats.StreamInfo, error) { return js.StreamInfo(stream) })
		if err != nil {
			err = fmt.Errorf("%s %s: %w", step, stream, namedRequest(lookup, err))
			break
		}
		before, answered, raiseErr := Bounded(ctx, func() (int, error) { return raiseStream(ctx, js, info, config.Replicas) })
		switch {
		case !answered:
			err = fmt.Errorf("raise %s: the raise drew no answer when the wait ended: %w", stream, context.Cause(ctx))
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
			err = awaitReplicasCurrent(ctx, js, stream, config.Replicas)
		case leaderless(info):
			// Another program raised or created the bucket's stream just before this start read
			// it, and the stream's new group has not elected its leader yet: a write sent now draws
			// no answer. The start waits for the leader, the fact another program's own start
			// waits for, and not for every copy current, so a bucket with a member offline still
			// opens (awaitStreamLeader).
			err = awaitStreamLeader(ctx, js, "open", stream)
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
		err = awaitStreamLeader(ctx, js, "create", stream)
	}
	if err != nil {
		return KeyValue{}, err
	}
	return handle, nil
}

// raiseReplicas raises the stream of the existing bucket to replicas copies when it has fewer, and
// returns the copies it had then, 0 when it left the bucket alone: the keeper's raise, which reads
// the stream once and is made again at its next pass (raiseStream). One copy needs no read: every
// stream has at least one.
func raiseReplicas(ctx context.Context, js nats.JetStreamContext, bucket string, replicas int) (int, error) {
	if replicas <= 1 {
		return 0, nil
	}
	stream := "KV_" + bucket
	info, err := js.StreamInfo(stream)
	if err != nil {
		return 0, fmt.Errorf("raise %s: %w", stream, err)
	}
	return raiseStream(ctx, js, info, replicas)
}

// raiseStream raises the stream info describes to replicas copies when it has fewer, and returns
// the copies it had then, 0 when it left the stream alone. The update writes back the stream's
// deployed configuration with only its replicas changed, so the bucket's TTL, history, discard
// policy, direct get, rollup, delete settings, metadata and the rest stay as they are. A stream
// with as many copies or more is left alone. As for the notification stream
// (ensureStreamWithConfig), two starts raising at once each write the deployment's one count. A
// raise whose answer never came succeeds when the stream, read again, has the copies, and one the
// server refused as overlapping the bucket's own stream is sent again until the stream has them
// or its sends run out (updateStream). Its error names the stream and, for a refused raise, the
// setting (refusedSettings): `raise KV_envoy_interests: ENVOY_NATS_REPLICAS=3: ...`, so the
// keeper's line says which bucket it could not raise.
func raiseStream(ctx context.Context, js nats.JetStreamContext, info *nats.StreamInfo, replicas int) (int, error) {
	if info.Config.Replicas >= replicas {
		return 0, nil
	}
	raised := info.Config
	raised.Replicas = replicas
	if err := updateStream(ctx, js, &raised, func(again *nats.StreamInfo) bool { return again.Config.Replicas >= replicas }); err != nil {
		return 0, fmt.Errorf("raise %s: %w", info.Config.Name, refusedSettings(err, &info.Config, &raised))
	}
	return info.Config.Replicas, nil
}

// leaderless reports whether info describes a stream of more than one copy that has no leader:
// one whose group has not elected one yet, after a create or a raise. A stream of one copy is its
// one member's, and needs no election.
func leaderless(info *nats.StreamInfo) bool {
	return info.Config.Replicas > 1 && (info.Cluster == nil || info.Cluster.Leader == "")
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
func awaitReplicasCurrent(ctx context.Context, js nats.JetStreamContext, stream string, replicas int) error {
	return awaitStream(ctx, js, "raise", stream, fmt.Sprintf("%d copies were not current", replicas), "envoy nats kv raised copies not current yet, waiting", func(info *nats.StreamInfo) string {
		return copiesBehind(info, replicas)
	})
}

// awaitStreamLeader waits until stream reports a leader, for as long as ctx lasts, after step: a
// create of more than one copy that this start made or that a concurrent start made ("create"),
// or a raise of the notification stream's copies ("raise"). A create sent again while the
// stream's first election runs is answered before that election ends, and a raise of a one-copy
// stream at once (EnsureKeyValue, ensureStreamWithConfig); a stream with no leader answers no
// write, and a publish sent then draws no answer even once the leader is there. It asks for the
// leader alone and not for every copy current: a new stream holds no message a copy could be
// behind on, a create with one member of three stopped lands with that member offline and serves
// on the other two (TestOneServerStoppedACreateAtThreeCopiesLandsAndTheStoppedMemberCatchesUp),
// and the notification stream is read through consumers, which its leader serves.
func awaitStreamLeader(ctx context.Context, js nats.JetStreamContext, step, stream string) error {
	return awaitStream(ctx, js, step, stream, "the stream had no leader", "envoy nats stream has no leader yet, waiting", func(info *nats.StreamInfo) string {
		if info.Cluster == nil || info.Cluster.Leader == "" {
			return "the stream has no leader yet"
		}
		return ""
	})
}

// awaitStream reads stream every bucketAppliedInterval until short, given what a read answered,
// reports nothing short, or ctx ends: the wait after a raise (awaitReplicasCurrent) and after a
// create (awaitStreamLeader), step naming which. Its first read that is short logs waiting, the
// WARN that says a start waited and for which stream. The end of ctx is an error naming step, the
// stream, want (what was still short), the last thing short reported and ctx's cause.
//
// A read the server answers with an error is returned as it is, since the stream is there: this
// runs only after a raise or a create of it answered. The one exception is a member that has not
// yet applied a create, which answers "stream not found" for it (bucketAppliedInterval): after a
// create that answer is read again. A read the server leaves unanswered (unanswered) says nothing
// of the stream, so it is read again, as sentAgain sends a request again. Each read is waited for
// up to its read limit and then abandoned, its answer dropped (Bounded), so one lost read costs the
// start a fraction of a second rather than the caller's whole ten second wait after the fact it
// waits for is true; a read in flight when ctx ends is abandoned too. The limit starts at
// firstReadLimit, doubles after each read left unanswered, so a server that answers late is still
// waited for, and after each read the server answered is three times that read's time and never
// under firstReadLimit (readLimit), so lost reads early in a wait do not make a lost read later in
// it cost more, and a server that answers every read late is not cut and read again.
func awaitStream(ctx context.Context, js nats.JetStreamContext, step, stream, want, waiting string, short func(*nats.StreamInfo) string) error {
	behind := "no read of the stream answered yet"
	limits := newReadLimit(ctx)
	for attempt := 1; ; attempt++ {
		wait := limits.limit
		readCtx, cancelRead := context.WithTimeout(ctx, wait)
		sent := time.Now()
		info, answered, err := Bounded(readCtx, func() (*nats.StreamInfo, error) { return js.StreamInfo(stream) })
		took := time.Since(sent)
		cancelRead()
		switch {
		case !answered && ctx.Err() != nil:
			return fmt.Errorf("%s %s: %s when the wait ended, a read in flight: %s: %w", step, stream, want, behind, context.Cause(ctx))
		case !answered:
			behind = fmt.Sprintf("the stream's read drew no answer within %s", wait)
			limits.lost()
		case unanswered(err):
			behind = "the stream's read drew no answer: " + err.Error()
			limits.lost()
		case step == "create" && bucketMissing(err):
			behind = "the member has not applied the create yet: " + err.Error()
			limits.answered(took)
		case err != nil:
			return fmt.Errorf("%s %s: read the stream: %w", step, stream, err)
		default:
			behind = short(info)
			limits.answered(took)
		}
		if behind == "" {
			return nil
		}
		if attempt == 1 {
			slog.Warn(waiting,
				slog.String("stream", stream),
				slog.String("behind", behind),
				slog.String("retry_in", bucketAppliedInterval.String()))
		}
		if ended := pause(ctx, bucketAppliedInterval); ended != nil {
			return fmt.Errorf("%s %s: %s when the wait ended: %s: %w", step, stream, want, behind, ended)
		}
	}
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
// An age an answer never came for succeeds when the stream, read again, carries it, and one the
// server refused as overlapping the bucket's own stream is sent again (updateStream). Its error
// names the bucket's stream: `set KV_envoy_sessions max age: ...`.
//
// The session registry's start makes this call right after it opens the bucket, and a failed call
// fails that start, so it waits on the cluster as EnsureKeyValue does: the read is sent again
// while the server leaves it unanswered (sentAgain), and the read and the update end when ctx
// ends or the start's hang guard passes (startHangGuard), the update in flight then abandoned
// (Bounded). The session reaper also makes this call on each pass, under a context of one
// RequestTimeout, since a pass has no start behind it and its next tick asks again.
func SetKeyValueMaxAge(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, bucket string, maxAge time.Duration) (time.Duration, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	ctx, cancel := hangGuard(ctx, startHangGuard)
	defer cancel()
	js = readsRefusals(conn, js)
	stream := "KV_" + bucket
	read := stream + " STREAM.INFO"
	info, err := readAgain(ctx, read, newReadLimit(ctx), func() (*nats.StreamInfo, error) { return js.StreamInfo(stream) })
	if err != nil {
		return 0, fmt.Errorf("set %s max age: %w", stream, namedRequest(read, err))
	}
	if info.Config.MaxAge == maxAge {
		return maxAge, nil
	}
	aged := info.Config
	aged.MaxAge = maxAge
	_, answered, err := Bounded(ctx, func() (struct{}, error) {
		return struct{}{}, updateStream(ctx, js, &aged, func(again *nats.StreamInfo) bool { return again.Config.MaxAge == maxAge })
	})
	if !answered {
		return 0, fmt.Errorf("set %s max age: the update drew no answer when the wait ended: %w", stream, context.Cause(ctx))
	}
	if err != nil {
		return 0, fmt.Errorf("set %s max age: %w", stream, err)
	}
	return maxAge, nil
}

// OpenKeyValue opens the existing KV bucket named bucket on conn, through js, and creates nothing,
// as a rebuild does. It raises nothing either: the start that opened the bucket with EnsureKeyValue
// did that. Both of its requests end when ctx does: the jetstream handle's open is made under ctx,
// and the legacy lookup (js.KeyValue, whose STREAM.INFO nats.go gives no context: the call takes
// none and reads js's MaxWait alone) is waited for only until ctx ends, its answer then dropped
// (Bounded; the lookup changes nothing, so a dropped one leaves nothing behind). A reconnect hook
// gives it the run's context, so a lookup sent on a connection that then drops ends the hook at
// the drop.
func OpenKeyValue(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, bucket string) (KeyValue, error) {
	js = readsRefusals(conn, js)
	kv, _, err := Bounded(ctx, func() (nats.KeyValue, error) { return js.KeyValue(bucket) })
	if err != nil {
		return KeyValue{}, err
	}
	return wrapKeyValue(ctx, conn, kv, false, oneSend)
}

// WrapKeyValue pairs kv, a legacy handle opened on conn, with the jetstream handle of the same
// bucket on conn that its watches go through. Opening that handle is one STREAM.INFO, bounded by
// RequestTimeout, and one send of it. The bucket is read as it is: a caller that holds a handle
// on it has had an answer for it already. A start opens its handles through EnsureKeyValue
// instead, which sends its handle open again (wrapKeyValue).
func WrapKeyValue(conn *nats.Conn, kv nats.KeyValue) (KeyValue, error) {
	return wrapKeyValue(context.Background(), conn, kv, false, oneSend)
}

// startSendsAgain and oneSend are wrapKeyValue's sendAgain, named at the call so the two readings
// of an unanswered handle open are told apart where they are chosen: a program's start sends the
// open again (sentAgain), and every other caller sends it once.
const (
	startSendsAgain = true
	oneSend         = false
)

// wrapKeyValue is WrapKeyValue, with two things its callers choose, under ctx, which bounds the
// handle's open as well as RequestTimeout does.
//
// created says this start has just created the bucket, so the STREAM.INFO that opens the
// jetstream handle is taken again until the member answering has applied that create
// (awaitBucketApplied): the create's own answer came from the metadata group, and this read goes
// to whichever member conn is on, which may not have applied it yet.
//
// sendAgain says the caller is EnsureKeyValue, so each send of that read is sent again when the
// server leaves it unanswered (readAgain), which a member whose meta leader is changing does. It
// is the caller and not created that decides this: a start whose buckets are already there
// creates nothing, which is every restart of a deployment, and it loses an answer exactly as the
// start that creates them does.
//
// A bucket missing and a request unanswered are two different answers and are waited out
// differently: the first is a member that has not applied the create and is read again until it
// has, the second is a request that drew nothing and is sent again. So a start's send is waited
// for up to its read limit, 630 ms first and twice that after each send left unanswered, up to
// RequestTimeout (readAgain); a start's waits, on the member and on an answer, last as long as its
// ctx, which EnsureKeyValue bounds by the start's hang guard (startHangGuard); and any other
// caller is one send, bounded by RequestTimeout as a lone read is. One send is right there because
// such a caller waits on a budget of its own: roles.Buckets.bucket, for one, opens a bucket again
// under its own lock, on a request whose budget is two seconds, and a read that kept it waiting
// longer would stop every role and session read of the process rather than save a start.
func wrapKeyValue(ctx context.Context, conn *nats.Conn, kv nats.KeyValue, created, sendAgain bool) (KeyValue, error) {
	js, err := jetstream.New(conn)
	if err != nil {
		return KeyValue{}, err
	}
	request := "KV_" + kv.Bucket() + " handle open"
	owner := clientOwning(conn)
	// The open is one STREAM.INFO of the bucket's stream, read for the server's refusal as every
	// request of a start is (refusedSend): the jetstream API has no context to wrap, so the send is.
	open := func() (jetstream.KeyValue, error) {
		ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
		defer cancel()
		return refusedBy(owner, []string{jsAPI("STREAM.INFO", "KV_"+kv.Bucket())}, func() (jetstream.KeyValue, error) {
			return js.KeyValue(ctx, kv.Bucket())
		})
	}
	if sendAgain {
		send := open
		openLimits := newReadLimit(ctx)
		open = func() (jetstream.KeyValue, error) { return readAgain(ctx, request, openLimits, send) }
	}
	var watch jetstream.KeyValue
	if created {
		watch, err = awaitBucketApplied(ctx, kv.Bucket(), bucketMissing, open)
	} else {
		watch, err = open()
	}
	if err != nil {
		return KeyValue{}, fmt.Errorf("open the jetstream handle of KV bucket %s: %w", kv.Bucket(), err)
	}
	return KeyValue{legacy: kv, watch: watch, streams: js, owner: owner}, nil
}

// NewKeyValue pairs kv with watch, a jetstream handle of the same bucket, and streams, the jetstream
// context its stream is read through, without a round trip: for a test that wraps any of them to
// inject a fault.
func NewKeyValue(kv nats.KeyValue, watch jetstream.KeyValue, streams jetstream.JetStream) KeyValue {
	var owner *Client
	if streams != nil {
		owner = clientOwning(streams.Conn())
	}
	return KeyValue{legacy: kv, watch: watch, streams: streams, owner: owner}
}

// refusedOn is call, a request kv makes once in a while of its bucket's stream (a watch's create,
// a read of the stream), read for the server's refusal of subjects as it arrives (refusedBy), in
// the record of the client that owns kv's connection, found when kv was built. A handle on a
// connection no client owns sends as call does.
func refusedOn[T any](kv KeyValue, subjects []string, call func() (T, error)) (T, error) {
	return refusedBy(kv.owner, subjects, call)
}

// refusedAfterOn is call, a read or a write of one key, which the bus makes on every request it
// serves, made on the caller's goroutine and read for the server's refusal of subjects only when
// the server left it unanswered (refusedAfter): a call the server answers costs exactly what it
// cost before, and a refused one names the refusal after its own wait, where it named only the
// silence. Only the subjects the call sends are listed: a call takes the refusal of any subject it
// lists, and one it does not send is another call's to answer.
func refusedAfterOn[T any](kv KeyValue, subjects func() []string, call func() (T, error)) (T, error) {
	return refusedAfter(kv.owner, subjects, call)
}

// keySubjects is the subject a write of key publishes to: "$KV.<bucket>.<key>" (nats.go@v1.53.1
// kv.go, the default KV prefix).
func (kv KeyValue) keySubjects(key string) func() []string {
	return func() []string { return []string{"$KV." + kv.Bucket() + "." + key} }
}

// getSubjects are the requests a read of key's last value can send: a direct get of the key's last
// message, on a bucket that allows direct gets, and a get from the stream's leader on one that
// does not (nats.go@v1.53.1 kv.go getRevision, js.go apiDirectMsgGetLastBySubjectT).
func (kv KeyValue) getSubjects(key string) func() []string {
	return func() []string {
		stream := "KV_" + kv.Bucket()
		return []string{jsAPI("DIRECT.GET", stream, "$KV."+kv.Bucket()+"."+key), jsAPI("STREAM.MSG.GET", stream)}
	}
}

// revisionSubjects are the requests a read of one revision can send: a direct get by sequence and
// a get from the stream's leader (nats.go@v1.53.1 kv.go getRevision, js.go apiDirectMsgGetT).
func (kv KeyValue) revisionSubjects() []string {
	stream := "KV_" + kv.Bucket()
	return []string{jsAPI("DIRECT.GET", stream), jsAPI("STREAM.MSG.GET", stream)}
}

// watchSubjects are the requests a watch sends to create its consumer, whose name nats.go picks as
// it sends (jetstream ordered consumers): every consumer create under the bucket's stream.
func (kv KeyValue) watchSubjects() []string {
	return []string{jsAPI("CONSUMER.CREATE", "KV_"+kv.Bucket(), ">")}
}

// Legacy is the legacy handle kv's checked reads and writes go through, for a test that wraps it to
// inject a fault (NewKeyValue). Production code calls kv's own methods, which check the key first.
func (kv KeyValue) Legacy() nats.KeyValue {
	return kv.legacy
}

// Opened reports whether kv is a handle on a bucket rather than the zero value, for a caller that
// opens its bucket once and keeps it (the webhook redelivery sweep's openState).
func (kv KeyValue) Opened() bool {
	return kv.legacy != nil
}

// WatchHandle is the jetstream handle kv's watches go through.
func (kv KeyValue) WatchHandle() jetstream.KeyValue {
	return kv.watch
}

// Streams is the jetstream context kv's stream is read through.
func (kv KeyValue) Streams() jetstream.JetStream {
	return kv.streams
}

// Bucket is the name of the bucket kv is a handle on.
func (kv KeyValue) Bucket() string {
	return kv.legacy.Bucket()
}

// WatchKeys watches the keys matching keys (jetstream.AllKeys for every key) through kv's jetstream
// handle, checking keys against the subject a watcher's create request builds. nats.go ties the
// subscription's life to ctx, which it also uses for the consumer's create request (js.go:
// 1951-1952, 2052-2058): the watcher ends at ctx's deadline, so one that follows the bucket takes a
// ctx without one, and the caller cancels ctx once it has stopped the watcher, or the goroutine
// nats.go parks on ctx.Done() outlives the watch. A ctx without a deadline still bounds the create,
// by the JetStream request wait of the handle's legacy push context, 5 s (jsm.go:1791-1795,
// js.go:299). The watcher sends a nil entry once it has delivered every existing key, and closes
// its updates only once its subscription has ended; whoever stops it keeps reading it until then,
// since nats.go's delivery blocks on a full 256-entry buffer.
func (kv KeyValue) WatchKeys(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	if err := kv.check(keys, watchOverhead(kv.Bucket())); err != nil {
		return nil, err
	}
	watcher, err := refusedOn(kv, kv.watchSubjects(), func() (jetstream.KeyWatcher, error) { return kv.watch.Watch(ctx, keys, opts...) })
	return watcher, invalidKey(keys, err)
}

// WatchFilteredKeys is WatchKeys over several key patterns at once, the watch a caller that reads
// a window of a time-keyed bucket needs (the webhook redelivery index): one watcher, one
// consumer, and the subjects of every pattern on it. Each pattern is checked as WatchKeys checks
// its own, since each becomes a filter subject of the same create request.
func (kv KeyValue) WatchFilteredKeys(ctx context.Context, keys []string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	for _, key := range keys {
		if err := kv.check(key, watchOverhead(kv.Bucket())); err != nil {
			return nil, err
		}
	}
	watcher, err := refusedOn(kv, kv.watchSubjects(), func() (jetstream.KeyWatcher, error) { return kv.watch.WatchFiltered(ctx, keys, opts...) })
	return watcher, invalidKey(keys, err)
}

// The subjects nats.go builds from a key of bucket, each as the bytes it puts around the key. A put
// or a delete publishes to `$KV.<bucket>.<key>`; a get is a direct get,
// `$JS.API.DIRECT.GET.KV_<bucket>.$KV.<bucket>.<key>`; a watch or a history creates a watcher on the
// key, `$JS.API.CONSUMER.CREATE.KV_<bucket>.<name>.$KV.<bucket>.<key>`, whose consumer name nats.go
// makes eight bytes, the longest of the three. A get by revision names no key in its subject.
func putOverhead(bucket string) int {
	return len("$KV..") + len(bucket)
}

func getOverhead(bucket string) int {
	return len("$JS.API.DIRECT.GET.KV_.") + len(bucket) + putOverhead(bucket)
}

func watchOverhead(bucket string) int {
	return len("$JS.API.CONSUMER.CREATE.KV_..00000000") + len(bucket) + putOverhead(bucket)
}

// check refuses key when the subject a call puts overhead bytes around it would be refused.
func (kv KeyValue) check(key string, overhead int) error {
	if limit := maxSubjectBytes - overhead; len(key) > limit {
		return fmt.Errorf("%w: a key of %d bytes, past %d", ErrTooLarge, len(key), limit)
	}
	if !validSubject(key) {
		return fmt.Errorf("%w: key %q", ErrInvalidSubject, key)
	}
	return nil
}

func (kv KeyValue) write(key string) error {
	return kv.check(key, watchOverhead(kv.Bucket()))
}

// invalidKey names nats.go's refusal of a key outside its key alphabet (nats.ErrInvalidKey, or
// jetstream.ErrInvalidKey on a watch through the jetstream handle) as the ErrInvalidKey it is,
// naming key. nats.go checks the alphabet before it sends anything (an exact key on a read, write
// or delete, a search key on a watch), so every call through the handle reads its answer rather
// than repeat its check. RefusesRead is the one exception: it answers without calling nats.go, so
// it repeats the check as natsKeyValid, which must follow nats.go's validKeyRe on every upgrade.
func invalidKey(key any, err error) error {
	if errors.Is(err, nats.ErrInvalidKey) || errors.Is(err, jetstream.ErrInvalidKey) {
		return fmt.Errorf("%w: key %q: %w", ErrInvalidKey, key, err)
	}
	return err
}

// Get reads key's latest value from the bucket.
//
// The read is a direct get, as every read of this build's buckets is: nats.go sends a KV get
// to the stream's $JS.API.DIRECT.GET subject whenever that stream carries AllowDirect, and every
// bucket here carries it. CreateKeyValue sets AllowDirect on the stream it creates and
// nats.KeyValueConfig holds no field that unsets it; mapStreamToKVS, which both CreateKeyValue
// and KeyValue return a handle through, reads that stream's AllowDirect back as the handle's
// useDirect; and kvs.get passes nats.DirectGet to the GetLastMsg behind every read that flag is
// set on. Neither KV surface takes a per-read option for it: the legacy nats.KeyValue.Get takes a
// key alone and jetstream.KeyValue.Get a context and a key, and the jetstream side builds the
// same read (prepareKeyValueConfig sets AllowDirect, its own mapStreamToKVS sets useDirect, and
// stream.getMsg sends the direct get an AllowDirect stream asks for). So with ENVOY_NATS_REPLICAS
// above 1 a copy answers the read, one the queue group hands it at 90% caught up rather than when
// it is current (EnsureKeyValue above), and what it answers stands up to one write behind the
// bucket's leader. No test here reads a lagging copy: that takes a cluster, and these tests run
// one server.
//
// A read the stream's leader answers exists, and this build makes none: js.GetLastMsg(stream,
// subject) without nats.DirectGet is a STREAM.MSG.GET, which the leader answers, and the
// listener's grant holds $JS.API.STREAM.MSG.GET.KV_<bucket> for every bucket it opens
// (infra/core's bucketPermissions), so it is this build's choice and not its permissions. What
// that read answers is a raw stream message rather than a KV entry, so each caller would decode
// the KV operation markers nats.go's KV decodes for it today -- the KV-Operation header's DEL and
// PURGE, which the legacy kvs.get and the jetstream package's own kvs.get both answer
// ErrKeyDeleted for -- to tell a deleted record from a live one, in each of the two reads the
// repair makes (session.SessionRegistry.Refresh, store.Registry.RefreshAt). The window the direct
// get leaves is one write wide and is closed without that decoding: a delete and a registration
// each repair the session's consumer from the bucket's newest record and row (cmd/listener's
// registeredTarget, which states what a copy behind costs that repair), and the session's next
// heartbeat registers again.
func (kv KeyValue) Get(key string) (nats.KeyValueEntry, error) {
	if err := kv.check(key, getOverhead(kv.Bucket())); err != nil {
		return nil, err
	}
	entry, err := refusedAfterOn(kv, kv.getSubjects(key), func() (nats.KeyValueEntry, error) { return kv.legacy.Get(key) })
	return entry, invalidKey(key, err)
}

// RefusesRead answers the refusal a Get of key through this handle would answer before anything is
// sent, and nil when a read of key would reach the bucket. It sends nothing itself.
//
// It is for a caller holding a key it got from a scan of the bucket rather than from a read of the
// key: a listing names keys this build may be unable to read, and such a caller holds no refusal to
// classify them by, so it asks this instead and tells a key this build cannot read from an ordinary
// one without a round trip. What it answers is exactly the refusal Get answers before sending, so
// the two cannot drift silently: Get's own check first, then nats.go's key alphabet, which this
// package copies as natsKeyValid because nats.go's keyValid is unexported.
// TestRefusesReadAnswersTheSameRefusalAGetOfTheKeyAnswers is what pins that copy, against the
// refusal a real Get answers.
func (kv KeyValue) RefusesRead(key string) error {
	if err := kv.check(key, getOverhead(kv.Bucket())); err != nil {
		return err
	}
	if !natsKeyValid(key) {
		return invalidKey(key, nats.ErrInvalidKey)
	}
	return nil
}

// natsKeyValid copies nats.go's keyValid, which every KV call there runs on
// an exact key, refusing it locally without sending anything: a key is valid when it is not empty,
// neither starts nor ends in a dot, and holds only `-`, `/`, `_`, `=`, `.`, a letter or a digit
// (its `validKeyRe`). The copy exists because keyValid is unexported, so a caller that asks
// what a read would be refused for (RefusesRead) cannot ask nats.go itself.
func natsKeyValid(key string) bool {
	if key == "" || key[0] == '.' || key[len(key)-1] == '.' {
		return false
	}
	for i := range len(key) {
		switch c := key[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '/', c == '_', c == '=', c == '.':
		default:
			return false
		}
	}
	return true
}

// GetRevision gets key by revision, which names the revision rather than the key in its subject, so
// only nats.go's alphabet check refuses a key here.
func (kv KeyValue) GetRevision(key string, revision uint64) (nats.KeyValueEntry, error) {
	entry, err := refusedAfterOn(kv, kv.revisionSubjects, func() (nats.KeyValueEntry, error) { return kv.legacy.GetRevision(key, revision) })
	return entry, invalidKey(key, err)
}

func (kv KeyValue) Put(key string, value []byte) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	revision, err := refusedAfterOn(kv, kv.keySubjects(key), func() (uint64, error) { return kv.legacy.Put(key, value) })
	return revision, invalidKey(key, err)
}

func (kv KeyValue) PutString(key string, value string) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	revision, err := refusedAfterOn(kv, kv.keySubjects(key), func() (uint64, error) { return kv.legacy.PutString(key, value) })
	return revision, invalidKey(key, err)
}

func (kv KeyValue) Create(key string, value []byte) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	revision, err := refusedAfterOn(kv, kv.keySubjects(key), func() (uint64, error) { return kv.legacy.Create(key, value) })
	return revision, invalidKey(key, err)
}

func (kv KeyValue) Update(key string, value []byte, last uint64) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	revision, err := refusedAfterOn(kv, kv.keySubjects(key), func() (uint64, error) { return kv.legacy.Update(key, value, last) })
	return revision, invalidKey(key, err)
}

// IsCASConflict reports whether err is a compare-and-swap revision conflict
// (as opposed to a real transport/storage error). Create returns ErrKeyExists
// when the key already exists; Update returns a "wrong last sequence" API error
// when the revision moved.
func IsCASConflict(err error) bool {
	return errors.Is(err, nats.ErrKeyExists) || strings.Contains(err.Error(), "wrong last sequence")
}

func (kv KeyValue) Delete(key string, opts ...nats.DeleteOpt) error {
	if err := kv.check(key, putOverhead(kv.Bucket())); err != nil {
		return err
	}
	_, err := refusedAfterOn(kv, kv.keySubjects(key), func() (struct{}, error) { return struct{}{}, kv.legacy.Delete(key, opts...) })
	return invalidKey(key, err)
}

// DeleteAtRead deletes key at the revision it reads, so a write that lands in between is kept, and
// returns that revision, 0 when there was none to read.
//
// A key the read finds missing is deleted at no revision at all, which fences nothing: nats.go
// sends no expected-last-subject-sequence header for revision 0, so that delete would take
// whatever the bucket holds, and a write that landed between the read and it - a registration
// storing the row this delete read none of - would be deleted whole. So no delete is made for it:
// the key is reported absent, at revision 0, and there is nothing there to remove.
//
// A key it may not read (ErrRefused) it may not write either, since a write is held to the longest
// bound and nats.go refuses a key outside its alphabet everywhere, so nothing can land in between:
// that key is deleted at no revision, which removes a key an earlier build stored past the write
// bound, and a key outside the alphabet is refused by the delete too.
func (kv KeyValue) DeleteAtRead(key string) (uint64, error) {
	entry, err := kv.Get(key)
	switch {
	case err == nil:
		revision := entry.Revision()
		return revision, kv.Delete(key, nats.LastRevision(revision))
	case errors.Is(err, nats.ErrKeyNotFound):
		return 0, nil
	case errors.Is(err, ErrRefused):
		return 0, kv.Delete(key)
	default:
		return 0, err
	}
}

func (kv KeyValue) Purge(key string, opts ...nats.DeleteOpt) error {
	if err := kv.check(key, putOverhead(kv.Bucket())); err != nil {
		return err
	}
	_, err := refusedAfterOn(kv, kv.keySubjects(key), func() (struct{}, error) { return struct{}{}, kv.legacy.Purge(key, opts...) })
	return invalidKey(key, err)
}

// Status reads the bucket's status through the legacy handle, which waits out that handle's
// JetStream MaxWait: the open-time read of a bucket's TTL and a store's health probe, whose
// openers chose that wait. A read held to RequestTimeout instead is ReadStreamState.
func (kv KeyValue) Status() (nats.KeyValueStatus, error) {
	return refusedOn(kv, []string{jsAPI("STREAM.INFO", "KV_"+kv.Bucket())}, kv.legacy.Status)
}

// Bounded is call, a bucket call that takes no context, as a caller holding ctx waits for it:
// nats.go's KV reads, writes and deletes take none and wait out the JetStream MaxWait (10 s), so
// the wait ends at the earlier of call's answer and ctx's end. answered is false when ctx ended
// first, and err is then ctx's error; the abandoned call runs on to its own end and its answer is
// dropped, so a write or a delete it sent may still land after the caller has answered (each
// caller says why that is safe). A ctx that has already ended starts no call, and a ctx that can
// never end (context.Background, which a caller with no request behind it passes) is simply
// call, in this goroutine. A background loop is not such a caller: the role claim reaper passes
// its own loop's ctx, which the listener's shutdown ends under a pass in flight
// (store.StartRoleClaimReaper).
func Bounded[T any](ctx context.Context, call func() (T, error)) (value T, answered bool, err error) {
	if ctx.Done() == nil {
		value, err = call()
		return value, true, err
	}
	if err := ctx.Err(); err != nil {
		return value, false, err
	}
	type answer struct {
		value T
		err   error
	}
	answers := make(chan answer, 1)
	go func() {
		value, err := call()
		answers <- answer{value: value, err: err}
	}()
	select {
	case got := <-answers:
		return got.value, true, got.err
	case <-ctx.Done():
		return value, false, ctx.Err()
	}
}

// StreamState is what one STREAM.INFO read says about the stream behind a KV bucket: its name, the
// sequence space it holds, how many messages are in it, when it was created and how long it keeps
// a message (its MaxAge, the bucket's TTL; 0 keeps them for good). A KV revision is a stream
// sequence, so a caller comparing revisions against the bucket's sequence space reads them from
// here.
type StreamState struct {
	Name     string
	FirstSeq uint64
	LastSeq  uint64
	Msgs     uint64
	Created  time.Time
	MaxAge   time.Duration
}

// ReadStreamState reads the stream behind kv (one STREAM.INFO) through its jetstream context,
// within RequestTimeout and ctx, fetching the stream afresh so no read shares state with another
// (KeyValue.streams): the read before every watcher a kvwatch.Watcher installs, its stream probe,
// gate, completion check and Check, and each read of the interest marker collector. The legacy
// handle's Status would wait out that handle's JetStream MaxWait, which its opener chose for its
// own reads, however short, so this read bounds itself. A stream that is gone is
// nats.ErrStreamNotFound, however the jetstream package names it.
func (kv KeyValue) ReadStreamState(ctx context.Context) (StreamState, error) {
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	stream, err := refusedOn(kv, []string{jsAPI("STREAM.INFO", "KV_"+kv.Bucket())}, func() (jetstream.Stream, error) {
		return kv.streams.Stream(ctx, "KV_"+kv.Bucket())
	})
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return StreamState{}, fmt.Errorf("read the stream of KV bucket %s: %w: %w", kv.Bucket(), nats.ErrStreamNotFound, err)
	}
	if err != nil {
		return StreamState{}, err
	}
	info := stream.CachedInfo()
	return StreamState{
		Name:     info.Config.Name,
		FirstSeq: info.State.FirstSeq,
		LastSeq:  info.State.LastSeq,
		Msgs:     info.State.Msgs,
		Created:  info.Created,
		MaxAge:   info.Config.MaxAge,
	}, nil
}

// ReadHistory reads every revision of key the bucket holds, oldest first, through a watch of the key
// with its history included (WatchKeys), the watch nats.go's History runs on the legacy watcher
// (KeyValue says why that one never runs). ctx bounds the whole read, the watch's create included:
// nats.go ends the watch's subscription when ctx ends. A key with no revision is nats.ErrKeyNotFound,
// as nats.go's History answers. A watch that ends before it has delivered every revision -- ctx
// ended, or its subscription did -- is an error rather than the revisions that arrived, which is
// what nats.go's History would return: a caller that takes the last one for the key's latest would
// take an older revision for it. Once complete, the watch is stopped and read until it closes, since
// nats.go's delivery blocks on a full 256-entry buffer holding the watcher's lock.
func (kv KeyValue) ReadHistory(ctx context.Context, key string) ([]jetstream.KeyValueEntry, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watcher, err := kv.WatchKeys(ctx, key, jetstream.IncludeHistory())
	if err != nil {
		return nil, err
	}
	var entries []jetstream.KeyValueEntry
	for entry := range watcher.Updates() {
		if entry != nil {
			entries = append(entries, entry)
			continue
		}
		_ = watcher.Stop()
		go DrainWatcher(watcher)
		if len(entries) == 0 {
			return nil, fmt.Errorf("read the history of key %s: %w", key, nats.ErrKeyNotFound)
		}
		return entries, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read the history of key %s of KV bucket %s: %w", key, kv.Bucket(), err)
	}
	return nil, fmt.Errorf("read the history of key %s of KV bucket %s: the watch ended after %d revisions, before it had delivered them all", key, kv.Bucket(), len(entries))
}

// DrainWatcher reads watcher's updates until nats.go closes them, which it does once the
// subscription a stop ended has delivered what it held: nats.go's delivery blocks holding the
// watcher's lock on a full 256-entry buffer, so a watcher stopped and left unread parks its
// subscription's goroutine for good. ReadHistory, kvwatch's scans and the watchers kvwatch stops
// all read theirs through it.
func DrainWatcher(watcher jetstream.KeyWatcher) {
	for range watcher.Updates() {
	}
}
