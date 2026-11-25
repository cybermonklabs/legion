package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// defaultKeepInterval is the keeper's period (keep): how long, at most, a stream or bucket a
// concurrent start or an operator lowered stays below the deployment's settings.
const defaultKeepInterval = 30 * time.Second

// keeperNoGrantBackoff is how long the keeper leaves a bucket alone once the server refused a
// request of its raise for want of a grant (refusedBuckets); the first pass after it asks once
// more. A listener whose NATS user lacks a bucket's grant - the known-session bucket on a hub whose
// users' render predates envoy-infra 10.33.0 - is refused every ask, and an ask each pass logged a
// permissions violation on the hub, an async error on this connection and a failed raise here,
// twice a minute, for an answer the keeper already had. It is ten minutes, the listener's
// noGrantBackoff for session consumers (cmd/listener), which spares those calls the same: a grant
// an operator adds is read within it, and nothing is lost meanwhile, since the server refuses the
// same request every time until the grant changes.
const keeperNoGrantBackoff = 10 * time.Minute

// WithKeepInterval sets the keeper's period in place of defaultKeepInterval, so a test sees a
// period pass without waiting one. It is exported for the tests of the programs that connect
// through this package, as WithReconnectErrClock is: no deployment sets it, and no caller outside
// a test may.
func WithKeepInterval(interval time.Duration) ConnectOption {
	return clientOption(func(o *connectOpts) { o.keepInterval = interval })
}

// withKeeperStepGuard sets the hang guard of each step of the keeper's passes in place of
// startHangGuard. It is a seam for this package's tests whose own work runs inside a step: a test
// that holds the keeper's update until its own read of the stream has come back
// (holdKeeperUpdates) gives the step what is left of its deadline, so the guard does not count the
// test's hold. No program sets it.
func withKeeperStepGuard(guard time.Duration) ConnectOption {
	return clientOption(func(o *connectOpts) { o.keeperStepGuard = guard })
}

// startKeeper starts the keeper of a client whose start reconciled the stream with cfg
// (ensureStreamWithConfig), which keeps buckets (WithKeptBuckets) at cfg's copies too: the
// constructor's start once install has returned, and the first connection's on a client that
// connects in the background (ensureStreamOnce), which a Close or Drain can stop first. A client
// already stopped starts none. The client's stop ends it (Close and Drain wait for it:
// awaitKeeper). The check and keeperDone's write are one step under mu, and Close and Drain stop
// the client before awaitKeeper reads keeperDone under mu, so a keeper either sees the stop or is
// waited for.
func (c *Client) startKeeper(cfg nats.StreamConfig, buckets []string, interval time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped() {
		return
	}
	c.keeperDone = make(chan struct{})
	go c.keep(c.keeperDone, cfg, buckets, interval)
}

// keep is the keeper: every interval it puts back what the deployment's settings and this binary
// ask of the stream (keepStream) and raises each of buckets, the program's own (WithKeptBuckets),
// back to the deployment's copies (keepBuckets), until the client stops. A start's reconcile can
// lose to a concurrent start with other settings or subjects, since JetStream's stream update has
// no compare-and-swap (ensureStreamWithConfig), and an operator can lower either by hand; the
// keeper ends both within one period. A pass waits for a live connection: a pass while the client
// is not connected is skipped, and none starts once the client has stopped, though a tick and the
// stop came together. A pass that fails is logged and the next period tries again: the keeper never
// stops the client or the process. A failure once the client has stopped is the stop's, and is not
// logged. Each pass that ran is counted once it has ended (keepPasses); a tick skipped while the
// client is not connected counts nothing.
func (c *Client) keep(done chan struct{}, cfg nats.StreamConfig, buckets []string, interval time.Duration) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	refused := refusedBuckets{}
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
		}
		if c.stopped() {
			return
		}
		if c.Connected() {
			c.keepPass(&cfg, buckets, refused)
			c.keepPasses.Add(1)
		}
	}
}

// keepPass is one pass of the keeper, through the client's current JetStream context: the stream
// (keepStream), then each of buckets (keepBuckets). Each pass reads the live connection's server
// version for itself, once the connection reports one (connectedDefaultsAnUnsetCap), so a stream
// with no consumer cap, which an older server leaves unlimited and this binary therefore leaves
// alone, is raised at the first pass after the server it is connected to has been upgraded to one
// that caps such a stream at a default of its own. A pass whose connection reports no version
// until the client stops or natsversion.VersionWait passes keeps nothing of the stream that pass
// and logs why, since an empty version is no reading of the server; a pass runs only while the
// client is connected (keep).
func (c *Client) keepPass(cfg *nats.StreamConfig, buckets []string, refused refusedBuckets) {
	ctx, cancel := c.untilStopped()
	defer cancel()
	defaultsAnUnsetCap, err := connectedDefaultsAnUnsetCap(ctx, c.Connection())
	js := c.JS()
	var restored []string
	if err == nil {
		err = c.keeperStep(ctx, func(ctx context.Context) (err error) {
			restored, err = keepStream(ctx, c.log(), js, cfg, defaultsAnUnsetCap)
			return err
		})
	}
	switch {
	case err != nil && c.stopped():
		return
	case err != nil:
		c.log().Warn("envoy nats keeper could not restore the stream", slog.String("name", c.name), slog.String("stream", Stream), slog.String("error", err.Error()))
	case len(restored) > 0:
		c.log().Warn("envoy nats keeper restored the stream", slog.String("name", c.name), slog.String("stream", Stream), slog.String("restored", strings.Join(restored, ", ")))
	}
	c.keepBuckets(ctx, js, buckets, cfg.Replicas, refused)
}

// keeperStep is one step of a keeper pass. Each step's first read of the stream or a bucket, and
// the read after an update that drew no answer, is sent again while the server leaves it
// unanswered (readAgain, updateStream), and those reads end when the client stops (ctx) or the
// step's hang guard passes, the one a start's step takes (startHangGuard), or a test's
// (withKeeperStepGuard). The step's update is one send under the JetStream context's own wait,
// which the connection's close at a stop ends too (awaitKeeper); the next pass, 30 s later, makes
// the step again either way.
func (c *Client) keeperStep(ctx context.Context, run func(ctx context.Context) error) error {
	guard := startHangGuard
	if c.keeperStepGuard > 0 {
		guard = c.keeperStepGuard
	}
	ctx, cancel := hangGuard(ctx, guard)
	defer cancel()
	return run(ctx)
}

// keepBuckets raises each of buckets back to replicas copies, one keeper step each: the read of
// the bucket's stream (readBucketStream), then the update that raises it when the read found
// fewer copies (raiseStream); and logs what each raise came to. One copy needs no read, since every
// stream has at least one, so a keeper of one copy asks nothing. A bucket that does not exist yet,
// one the program opens only for a feature it has not configured, is skipped without a line: the
// program creates it at the deployment's copies when it opens it.
//
// A request of a raise the server refuses for want of a grant (grantRefused), the read or the
// update, holds that bucket's next ask for keeperNoGrantBackoff (refusedBuckets), while every
// other bucket is asked each pass. The refusal that starts an episode is logged, with the request
// it names and the time to the next ask, and so is a refusal of another request than the
// episode's; the refused asks that end each backoff after it are not. The episode ends at any
// answer the server gives the request it refused (answeredRequests): a read's answer, a bucket
// that is not there and every other API error included, or an update's, an API error included;
// that logs that the grant now covers the bucket, and then the raise's own line, or its failure's
// unless the answer is that the bucket is not there. An ask that sends no such request leaves the
// episode as it is: a read that finds the bucket at its copies, or not there, sends no update and
// says nothing of a refused one, which a later lowering sends again. So does an ask that draws no
// answer, a lost one say: it logs its failure as any bucket's does, says nothing of the grant, and
// the next pass asks again. The episode's backoff is timed on the client's clock (now,
// WithReconnectErrClock).
func (c *Client) keepBuckets(ctx context.Context, js nats.JetStreamContext, buckets []string, replicas int, refused refusedBuckets) {
	if replicas <= 1 {
		return
	}
	for _, bucket := range buckets {
		if c.stopped() {
			return
		}
		if !refused.asksAgain(bucket, c.now()) {
			continue
		}
		var info *nats.StreamInfo
		var from int
		err := c.keeperStep(ctx, func(ctx context.Context) (err error) {
			if info, err = readBucketStream(ctx, c.log(), js, bucket); err != nil {
				return err
			}
			from, err = raiseStream(ctx, c.log(), js, info, replicas)
			return err
		})
		if err != nil && c.stopped() {
			return
		}
		if grantRefused(err) {
			subject, _ := refusedPublishSubject(err)
			if refused.refuse(bucket, subject, c.now()) {
				c.log().Warn("envoy nats keeper may not raise a bucket under its grant; it asks again after the backoff",
					slog.String("name", c.name), slog.String("bucket", bucket), slog.String("error", err.Error()),
					slog.String("probe_again_in", keeperNoGrantBackoff.String()))
			}
			continue
		}
		if refused.answered(bucket, answeredRequests("KV_"+bucket, info, from, err)) {
			c.log().Info("envoy nats keeper's grant now covers a bucket the server refused it", slog.String("name", c.name), slog.String("bucket", bucket))
		}
		switch {
		case errors.Is(err, nats.ErrStreamNotFound):
		case err != nil:
			c.log().Warn("envoy nats keeper could not raise a bucket", slog.String("name", c.name), slog.String("bucket", bucket), slog.String("error", err.Error()))
		case from > 0:
			c.log().Warn("envoy nats keeper raised a bucket", slog.String("name", c.name), slog.String("bucket", bucket), slog.String("replicas", fmt.Sprintf("%d -> %d", from, replicas)))
		}
	}
}

// answeredRequests is the subjects of the requests of one raise of stream the server answered,
// from what the raise came to: info, the read's answer, nil when the read failed; from, the copies
// raiseStream returned, above 0 exactly when it raised the stream; and err, the raise's error, the
// read's when info is nil and the update's otherwise, since raiseStream returns an error only
// after it sent the update. The read is answered when it read the stream, or its error is the
// server's answer (serverAnswered); the update is answered when it raised the stream, or its error
// is the server's answer.
func answeredRequests(stream string, info *nats.StreamInfo, from int, err error) []string {
	var answered []string
	if info != nil || serverAnswered(err) {
		answered = append(answered, jsAPI("STREAM.INFO", stream))
	}
	if info != nil && (from > 0 || serverAnswered(err)) {
		answered = append(answered, jsAPI("STREAM.UPDATE", stream))
	}
	return answered
}

// serverAnswered reports whether err is the server's own answer to a JetStream request: an API
// error, a stream that is not there among them (nats.ErrStreamNotFound unwraps to its
// *nats.APIError). A wait that ran out (unanswered) is no answer, and neither is any other error a
// client makes up for itself.
func serverAnswered(err error) bool {
	var apiErr *nats.APIError
	return !unanswered(err) && errors.As(err, &apiErr)
}

// grantRefused reports whether err is the server's refusal of a keeper request under the
// connection's grant: the server's permissions violation. A client names a refusal it recorded
// under the request's own subject ErrPublishDenied (deniedPublish, readAgain's one send of a
// refused read), and deniedPublishError, the one spelling of that error, wraps the violation it is
// given, which both of its callers take only as a nats.ErrPermissionViolation (publishDenial, and
// deniedPublish through onAsyncError's record), so the one test covers both.
func grantRefused(err error) bool {
	return errors.Is(err, nats.ErrPermissionViolation)
}

// refusedBuckets is the keeper's record of the buckets one of whose raise's requests the server
// refused for want of a grant (grantRefused), each with the refused request's subject and when the
// bucket may be asked about again, keeperNoGrantBackoff after its last refusal. A bucket is in it
// from the refusal that starts its episode until the server answers the refused request
// (keepBuckets). Only the keeper's goroutine reads and writes it (keep), so it takes no lock.
type refusedBuckets map[string]refusedRequest

// refusedRequest is one bucket's episode: the subject of the request the server refused, "" when
// its refusal named none (refusedPublishSubject), and the time from which the bucket is asked
// about again.
type refusedRequest struct {
	subject string
	until   time.Time
}

// asksAgain reports whether the keeper may ask about bucket at now: when no episode runs for it,
// or its backoff has passed.
func (r refusedBuckets) asksAgain(bucket string, now time.Time) bool {
	episode, refused := r[bucket]
	return !refused || !now.Before(episode.until)
}

// refuse records the server's refusal, at now, of the request of bucket's raise on subject, which
// holds its next ask for keeperNoGrantBackoff, and reports whether the refusal is logged: one that
// starts an episode, or one of another request than the episode's.
func (r refusedBuckets) refuse(bucket, subject string, now time.Time) (logged bool) {
	episode, refused := r[bucket]
	r[bucket] = refusedRequest{subject: subject, until: now.Add(keeperNoGrantBackoff)}
	return !refused || episode.subject != subject
}

// answered ends bucket's episode when the request its refusal named is among answered, the
// subjects of the requests an ask about it sent and the server answered, and reports whether it
// ended one. An episode whose refusal named no request ends at no answer, since no subject is "".
func (r refusedBuckets) answered(bucket string, answered []string) bool {
	episode, refused := r[bucket]
	if !refused || !slices.Contains(answered, episode.subject) {
		return false
	}
	delete(r, bucket)
	return true
}

// awaitKeeper waits for the keeper of a stopped client to end, so it never outlives the client.
// The caller has closed the connection, or is about to have nats.go close it, which ends a
// request a pass has in flight at once. A client that never started one, as a client connecting
// in the background that stopped before its first reconcile, has none to wait for.
func (c *Client) awaitKeeper() {
	c.mu.Lock()
	done := c.keeperDone
	c.mu.Unlock()
	if done != nil {
		<-done
	}
}

// keepStream puts back what cfg, this binary's stream with the deployment's settings, asks of the
// stream and a concurrent start or an operator took away. It writes keptStream: the stream as it
// read it, with copies below cfg's count raised to it, a byte limit below cfg's, or none, raised to
// cfg's when cfg sets one, a positive consumer cap below cfg's raised to cfg's
// (streamMaxConsumers), a deployed -1 or 0 raised to it as well where the connected server caps
// such a stream at a default of its own and left exactly as it is on every server before that
// (serverDefaultsAnUnsetCap), a missing StreamIDMetadataKey added, the age, the duplicate window
// and discard set to cfg's (the same in every release, ownedStream), and each of cfg's subjects the
// stream lacks added unless it overlaps a deployed subject (keptSubjects). It does not check cfg's
// subjects for one that captures a role lane, because no subject this binary compiles captures
// one, which TestStreamConfigExcludesRoleLanes in stream_config_test.go guarantees. It never
// lowers the copies, the byte limit or the consumer cap, so an operator's raise of any of them
// stands, and never removes or replaces a subject: only a start does, since two releases with
// different subject lists would otherwise switch the stream back and forth every period. Its write
// still carries every other field as it read it, the other subjects included, so a concurrent
// write that lands between its read and its write is lost again; the keeper of the program whose
// write it was puts its part back a period later. One part never comes back that way: the keeper
// skips a subject this binary's start put in place of an overlapping one while that overlapping
// subject is back (keptSubjects), so a stale write that put the replaced subject back stands until
// this binary's next start. It returns each field it restored, old and new value, none when the
// stream already held them, in which case it sends nothing. A stream whose subjects capture the
// role lanes it leaves to a start, sending nothing and returning an error when it needs anything:
// only a start moves the role lanes off the stream, their consumers' filters first
// (migrateRoleLanesOffStream), and a write of the list the keeper read could put them back after a
// start removed them. A stream stamped with a generation higher than streamConfigGeneration
// belongs to a newer build and is left exactly as it is, as a start leaves it
// (ensureStreamWithConfig), so the two builds a release runs at once never put their own settings
// back in turn. Its first read is sent again while the server leaves it unanswered, each send up
// to its read limit (readAgain), so a cluster slow to answer one read is not logged as a stream the
// keeper could not restore. An update whose answer never came succeeds when the stream, read
// again, needs nothing more, and one the server refused as overlapping the stream itself is sent
// again until the stream needs nothing more or its sends run out (updateStream). Each update sent
// again is kept from the read before it, and none is sent when that read finds the stream a newer
// build's or carrying the role lanes, the two streams a first read leaves alone; the fields it
// returns are those of the last update against the read it was made from, so a field another
// writer set between two sends is not reported as one this pass restored. The lines of its reads
// sent again go to logger.
func keepStream(ctx context.Context, logger *slog.Logger, js nats.JetStreamContext, cfg *nats.StreamConfig, defaultsAnUnsetCap bool) ([]string, error) {
	info, err := readAgain(ctx, logger, Stream+" STREAM.INFO", readLimitOf(ctx), func() (*nats.StreamInfo, error) { return js.StreamInfo(Stream) })
	if err != nil {
		return nil, err
	}
	if newerBuild(info) {
		return nil, nil
	}
	kept := keptStream(info, cfg, defaultsAnUnsetCap)
	if reflect.DeepEqual(kept, info.Config) {
		return nil, nil
	}
	if err := roleLanesLeftToAStart(info); err != nil {
		return nil, err
	}
	keep := func(current *nats.StreamInfo) (nats.StreamConfig, bool) {
		if newerBuild(current) || roleLanesLeftToAStart(current) != nil {
			return nats.StreamConfig{}, false
		}
		return keptStream(current, cfg, defaultsAnUnsetCap), true
	}
	update, err := updateStream(ctx, logger, js, info, keep, func(again *nats.StreamInfo) bool {
		kept, write := keep(again)
		return write && reflect.DeepEqual(kept, again.Config)
	})
	switch {
	case err != nil:
		return nil, refusedSettings(err, &update.from.Config, &update.sent)
	case update.declined && !newerBuild(update.from):
		return nil, roleLanesLeftToAStart(update.from)
	case update.declined:
		return nil, nil
	}
	return restoredFields(&update.from.Config, &update.sent), nil
}

// roleLanesLeftToAStart is the keeper's refusal of a stream whose subjects capture the role lanes,
// which only a start moves off it (keepStream), naming those subjects; nil when none does.
func roleLanesLeftToAStart(info *nats.StreamInfo) error {
	roleLanes := slices.DeleteFunc(slices.Clone(info.Config.Subjects), func(subject string) bool { return !subjectCapturesRoleLanes(subject) })
	if len(roleLanes) == 0 {
		return nil
	}
	return fmt.Errorf("the stream carries subjects that capture the role lanes, which only a start moves off it: %s", strings.Join(roleLanes, ", "))
}

// restoredFields names each field kept differs in from deployed, with its old and new value:
// `replicas 1 -> 3`, `max_bytes -1 -> 2000000` or `max_bytes 1000000 -> 2000000`,
// `max_consumers -1 -> 24000` or `max_consumers 1000 -> 24000`,
// `max_age 73h0m0s -> 72h0m0s`, `duplicate_window 1m0s -> 72h0m0s`,
// `discard DiscardOld -> DiscardNew`, `envoy_stream_id added as <id>`,
// `subjects added notifications.x.>`. The copies, the byte limit and the consumer cap only ever
// rise (ownedStream), and kept's subjects are deployed's with the keeper's additions after them
// (keptSubjects). Without the cap's line an operator who lowered the cap would see it back within
// a keeper period with nothing saying so.
func restoredFields(deployed, kept *nats.StreamConfig) []string {
	var restored []string
	if kept.Replicas != deployed.Replicas {
		restored = append(restored, fmt.Sprintf("replicas %d -> %d", deployed.Replicas, kept.Replicas))
	}
	if kept.MaxBytes != deployed.MaxBytes {
		restored = append(restored, fmt.Sprintf("max_bytes %d -> %d", deployed.MaxBytes, kept.MaxBytes))
	}
	if kept.MaxConsumers != deployed.MaxConsumers {
		restored = append(restored, fmt.Sprintf("max_consumers %d -> %d", deployed.MaxConsumers, kept.MaxConsumers))
	}
	if kept.MaxAge != deployed.MaxAge {
		restored = append(restored, fmt.Sprintf("max_age %s -> %s", deployed.MaxAge, kept.MaxAge))
	}
	if kept.Duplicates != deployed.Duplicates {
		restored = append(restored, fmt.Sprintf("duplicate_window %s -> %s", deployed.Duplicates, kept.Duplicates))
	}
	if kept.Discard != deployed.Discard {
		restored = append(restored, fmt.Sprintf("discard %s -> %s", deployed.Discard, kept.Discard))
	}
	if id := kept.Metadata[StreamIDMetadataKey]; id != deployed.Metadata[StreamIDMetadataKey] {
		restored = append(restored, fmt.Sprintf("%s added as %s", StreamIDMetadataKey, id))
	}
	if added := kept.Subjects[len(deployed.Subjects):]; len(added) > 0 {
		restored = append(restored, "subjects added "+strings.Join(added, " "))
	}
	return restored
}
