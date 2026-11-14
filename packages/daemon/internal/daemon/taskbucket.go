package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/legiontasks"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// taskBucketCallTimeout bounds each call the daemon makes on the task bucket's stream outside a
// claim's own decision — the open, the create, the reads of its epoch — as Envoy's own clients bound
// one key-value request (nats.MaxWait of ten seconds, packages/envoy/internal/bus/kv.go). A call the
// server refuses under the daemon's grant draws no answer at all, only the refusal on the
// connection (natsauth.RefusedPermission), so this is also how long a refused boot takes to say so.
const taskBucketCallTimeout = 10 * time.Second

// taskBucketCheckEvery is how often the watch reads the bucket's stream to find it gone or created
// again (taskBucket.moved), and how long it waits before it opens a watch that ended again: a watch
// of a bucket that was deleted hands over nothing more, and says nothing of it either.
const taskBucketCheckEvery = 30 * time.Second

// taskBucket is the daemon's task bucket, legiontasks.Bucket on Envoy's NATS: each claim's key holds
// the number of the task the claim's record owes it, which the listener relays to the session
// holding the claim's role (dispatch://INFRA-2811). It is supervise.TaskValues, the bucket every
// machine writes its claim's value through, and it keeps the bucket the record's delivered copy
// (watch): the record is the truth, so the bucket never decides anything.
//
// Every project's daemon shares the bucket, and the deployment's daemons share one NATS user, so
// this daemon writes, repairs and deletes the keys of its own project alone (legiontasks.OwnedBy
// with project) and leaves every other key as it stands.
type taskBucket struct {
	js      jetstream.JetStream
	conn    *nats.Conn
	project string
	// nats is the user the connection authenticates as, which a refusal names when it is the pane
	// seed's (refused).
	nats natsConnection
	log  *slog.Logger
	// recheck asks the watch to read the bucket's stream now rather than at its next check: a write
	// the bucket did not take may be a write to a bucket that is gone.
	recheck chan struct{}

	mu sync.RWMutex
	kv jetstream.KeyValue
	// epoch is the bucket's epoch, the creation time of its stream (legiontasks.Epoch), which every
	// revision this daemon reports is carried with.
	epoch string
}

var _ supervise.TaskValues = (*taskBucket)(nil)

// openTaskBucket opens the task bucket on conn, creating it when it does not exist (ensure), for
// the daemon of project token project connected as nc.
func openTaskBucket(ctx context.Context, conn *nats.Conn, project string, nc natsConnection, log *slog.Logger) (*taskBucket, error) {
	js, err := jetstream.New(conn)
	if err != nil {
		return nil, fmt.Errorf("open Envoy JetStream for the task bucket %s: %w", legiontasks.Bucket, err)
	}
	b := &taskBucket{js: js, conn: conn, project: project, nats: nc, log: log, recheck: make(chan struct{}, 1)}
	if err := b.ensure(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

// ensure opens the bucket, or creates it where it does not exist, and reads its epoch, the creation
// time of its stream. The bucket is created as decision 4 of INFRA-2811's plan shapes it: one
// revision of each key, kept on file, with no TTL, at the replica count of Envoy's notification
// stream, so it has as many copies as the stream every daemon already reads. The deployment's
// daemons create it concurrently, so a create the server answers "already in use" is another
// daemon's that came first, and the bucket is opened, as Envoy's own bus.EnsureKeyValue opens it.
//
// A call the server refuses under the daemon's grant is refused here naming the bucket and the
// subject (refused), and a boot refuses on it: no wait gives a user a grant it lacks.
func (b *taskBucket) ensure(ctx context.Context) error {
	created := false
	kv, err := b.lookUp(ctx)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		kv, created, err = b.create(ctx)
		if err != nil {
			return b.refused("create", err)
		}
	}
	if err != nil {
		return b.refused("open", err)
	}
	stream, err := b.stream(ctx, legiontasks.Stream)
	if err != nil {
		return b.refused("read the epoch of", err)
	}
	epoch := legiontasks.Epoch(stream.CachedInfo().Created)
	b.mu.Lock()
	b.kv, b.epoch = kv, epoch
	b.mu.Unlock()
	b.log.Info("legion daemon opened the task bucket", "bucket", legiontasks.Bucket, "epoch", epoch, "created", created)
	return nil
}

// create creates the bucket, and reports whether this daemon's create made it: false where another
// daemon's create came first and the bucket was opened instead.
func (b *taskBucket) create(ctx context.Context) (jetstream.KeyValue, bool, error) {
	notifications, err := b.stream(ctx, intake.NotificationStream)
	if err != nil {
		return nil, false, fmt.Errorf("read the replica count of %s, which the bucket is created at: %w", intake.NotificationStream, err)
	}
	bounded, cancel := context.WithTimeout(ctx, taskBucketCallTimeout)
	defer cancel()
	kv, err := b.js.CreateKeyValue(bounded, jetstream.KeyValueConfig{
		Bucket:   legiontasks.Bucket,
		History:  1,
		Storage:  jetstream.FileStorage,
		Replicas: notifications.CachedInfo().Config.Replicas,
	})
	if errors.Is(err, jetstream.ErrBucketExists) {
		kv, err = b.lookUp(ctx)
		return kv, false, err
	}
	return kv, err == nil, err
}

// lookUp opens the bucket's handle, bounded by taskBucketCallTimeout.
func (b *taskBucket) lookUp(ctx context.Context) (jetstream.KeyValue, error) {
	bounded, cancel := context.WithTimeout(ctx, taskBucketCallTimeout)
	defer cancel()
	return b.js.KeyValue(bounded, legiontasks.Bucket)
}

// stream reads the stream named name, bounded by taskBucketCallTimeout.
func (b *taskBucket) stream(ctx context.Context, name string) (jetstream.Stream, error) {
	bounded, cancel := context.WithTimeout(ctx, taskBucketCallTimeout)
	defer cancel()
	return b.js.Stream(bounded, name)
}

// refused is err, the failure of step on the bucket, as the daemon refuses it: naming the bucket,
// and, where the server refused the daemon's user a subject — a request the grant does not allow
// draws no answer, only the server's permissions violation on the connection
// (natsauth.RefusedPermission) — the subject its grant is missing, which daemonpermissions.Grant
// gives a Legion daemon. A daemon connected as the pane seed's user, the user every session of the
// deployment shares, is told so: that user is granted nothing of the bucket, by design, so such a
// daemon never boots until it is given a user of its own. The refusal carries the server's own
// error, so natsauth.Unreachable reads it as the misconfiguration it is.
func (b *taskBucket) refused(step string, err error) error {
	err = fmt.Errorf("%s the task bucket %s: %w", step, legiontasks.Bucket, err)
	last := b.conn.LastError()
	operation, subject, denied := natsauth.RefusedPermission(last)
	if !denied {
		return err
	}
	why := fmt.Sprintf("NATS refused the daemon's user (%s) the %s of %q, which the grant of a Legion daemon holds (daemonpermissions.Grant)",
		b.nats.user, operation, subject)
	if b.nats.paneUser {
		why += fmt.Sprintf("; the daemon connects as the pane seed's user (%s), the session user every pane shares, which holds no grant on the task bucket: give the daemon a user of its own (%s)",
			natsauth.SeedFileVariable, natsauth.DaemonSeedFileKey)
	}
	return fmt.Errorf("%w: %s (%w)", err, why, last)
}

// handle is the bucket as the daemon last opened it, and its epoch.
func (b *taskBucket) handle() (jetstream.KeyValue, string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.kv, b.epoch
}

// owns refuses a key of another project: a daemon writes and deletes its own project's keys alone.
func (b *taskBucket) owns(token claim.Token) error {
	if !legiontasks.OwnedBy(string(token), b.project) {
		return fmt.Errorf("the task bucket's key %s is not a key of project %s", token, b.project)
	}
	return nil
}

// Write puts number as token's value, and answers the bucket's epoch and the write's revision. A
// write the bucket did not take has the watch read the bucket's stream at once (recheck), since
// the bucket may be gone.
func (b *taskBucket) Write(ctx context.Context, token claim.Token, number uint64) (string, uint64, error) {
	if err := b.owns(token); err != nil {
		return "", 0, err
	}
	data, err := legiontasks.Value{Task: number}.Encode()
	if err != nil {
		return "", 0, err
	}
	kv, epoch := b.handle()
	revision, err := kv.Put(ctx, string(token), data)
	if err != nil {
		b.checkSoon()
		return "", 0, fmt.Errorf("write the task value of %s: %w", token, err)
	}
	return epoch, revision, nil
}

// Delete deletes token's value.
func (b *taskBucket) Delete(ctx context.Context, token claim.Token) error {
	if err := b.owns(token); err != nil {
		return err
	}
	kv, _ := b.handle()
	if err := kv.Delete(ctx, string(token)); err != nil {
		b.checkSoon()
		return fmt.Errorf("delete the task value of %s: %w", token, err)
	}
	return nil
}

// Read is token's current value, and false where the bucket holds none. It reads the key's latest
// entry (a direct get of the key's subject, which the daemon's grant allows), never a revision of
// it, whose get the grant does not allow.
func (b *taskBucket) Read(ctx context.Context, token claim.Token) (supervise.TaskValue, bool, error) {
	kv, epoch := b.handle()
	entry, err := kv.Get(ctx, string(token))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return supervise.TaskValue{}, false, nil
	}
	if err != nil {
		return supervise.TaskValue{}, false, fmt.Errorf("read the task value of %s: %w", token, err)
	}
	value, err := legiontasks.Decode(entry.Value())
	if err != nil {
		return supervise.TaskValue{}, false, err
	}
	return supervise.TaskValue{Number: value.Task, Epoch: epoch, Revision: entry.Revision()}, true, nil
}

// checkSoon asks the watch to read the bucket's stream now (recheck), once however many ask.
func (b *taskBucket) checkSoon() {
	select {
	case b.recheck <- struct{}{}:
	default:
	}
}

// errTaskBucketMoved ends a watch whose bucket is gone, or was created again under another epoch.
var errTaskBucketMoved = errors.New("the task bucket is gone or was created again")

// watch keeps the bucket the delivered copy of the record for as long as ctx lasts (R3): it watches
// the whole bucket and hands each claim of sup the value its key holds (supervise.TaskValueRead),
// whose machine writes the record's value again where the two differ. The first watch is the
// boot's, whose reads carry supervise.TaskValueBoot; every later one reads the bucket whole again
// under supervise.TaskValueRepair. A watch ends where the bucket is gone or was created again
// (moved), or where its own consumer ended; the bucket is then opened again, created again where it
// is gone (ensure), and watched again, so every owed value is written into the new one. every is
// how often the stream is read for that (taskBucketCheckEvery), and how long a watch that ended
// for any other reason, or an open that failed, waits before it is tried again.
func (b *taskBucket) watch(ctx context.Context, sup *supervisor, every time.Duration) {
	cause := supervise.TaskValueBoot
	for {
		err := b.watchOnce(ctx, sup, cause, every)
		if ctx.Err() != nil {
			return
		}
		cause = supervise.TaskValueRepair
		b.log.Warn("legion daemon watches the task bucket again", "bucket", legiontasks.Bucket, "reason", err)
		if !errors.Is(err, errTaskBucketMoved) && !sleepFor(ctx, every) {
			return
		}
		for {
			err := b.ensure(ctx)
			if err == nil {
				break
			}
			b.log.Error("legion daemon could not open the task bucket again; trying again", "bucket", legiontasks.Bucket,
				"after", every.String(), "error", err)
			if !sleepFor(ctx, every) {
				return
			}
		}
	}
}

// sleepFor waits d, and reports false when ctx ended first.
func sleepFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// watchOnce is one watch of the bucket as handle opened it. Its first part reads the bucket whole —
// the watch hands over the latest entry of every key, then marks the end of them — and hands every
// claim of sup its key's entry, or none: a key with no entry is read at the bucket's last revision
// before the watch began, so a write the claim's machine made after it is not taken for one the
// bucket lost. Then it hands each change of a key of this daemon's project to that key's claim, and
// reads the bucket's stream every interval, ending where it is gone or under another epoch.
//
// A key of this project that no claim of sup holds is left as it stands, and logged once: this
// daemon's store holds every claim of its project, so no record of its own owes that key anything
// to write, and only another daemon of the same project would be writing it.
func (b *taskBucket) watchOnce(ctx context.Context, sup *supervisor, cause string, every time.Duration) error {
	kv, epoch := b.handle()
	stream, err := b.stream(ctx, legiontasks.Stream)
	if err != nil {
		return fmt.Errorf("read the task bucket's stream: %w", err)
	}
	if legiontasks.Epoch(stream.CachedInfo().Created) != epoch {
		return errTaskBucketMoved
	}
	last := stream.CachedInfo().State.LastSeq
	watching, stop := context.WithCancel(ctx)
	defer stop()
	watcher, err := kv.WatchAll(watching)
	if err != nil {
		return fmt.Errorf("watch the task bucket: %w", err)
	}
	defer func() { _ = watcher.Stop() }()
	strays := map[string]bool{}
	held := map[claim.Token]jetstream.KeyValueEntry{}
	for replayed := false; !replayed; {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case entry, ok := <-watcher.Updates():
			switch {
			case !ok:
				return errors.New("the task bucket's watch ended")
			case entry == nil:
				replayed = true
			case legiontasks.OwnedBy(entry.Key(), b.project):
				held[claim.Token(entry.Key())] = entry
			}
		}
	}
	for _, m := range sup.supervised() {
		token := m.View().Token
		read := supervise.TaskValueRead{Claim: token, Value: supervise.TaskValue{Epoch: epoch, Revision: last}, Cause: cause}
		if entry, ok := held[token]; ok {
			read = taskValueRead(entry, epoch, cause)
			delete(held, token)
		}
		sup.post(token, read)
	}
	for token := range held {
		b.stray(strays, token)
	}
	b.log.Info("legion daemon read the task bucket whole", "bucket", legiontasks.Bucket, "epoch", epoch,
		"claims", len(sup.supervised()), "cause", cause)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case entry, ok := <-watcher.Updates():
			if !ok {
				return errors.New("the task bucket's watch ended")
			}
			if entry == nil || !legiontasks.OwnedBy(entry.Key(), b.project) {
				continue
			}
			token := claim.Token(entry.Key())
			if _, supervised := sup.Machine(token); !supervised {
				b.stray(strays, token)
				continue
			}
			sup.post(token, taskValueRead(entry, epoch, supervise.TaskValueRepair))
		case <-ticker.C:
			if err := b.moved(ctx, epoch); err != nil {
				return err
			}
		case <-b.recheck:
			if err := b.moved(ctx, epoch); err != nil {
				return err
			}
		}
	}
}

// stray logs, once per watch, a key of this project no claim of this daemon holds.
func (b *taskBucket) stray(logged map[string]bool, token claim.Token) {
	if logged[string(token)] {
		return
	}
	logged[string(token)] = true
	b.log.Warn("legion daemon leaves a task value of its project that no claim of its own holds", "bucket", legiontasks.Bucket, "key", token)
}

// moved is errTaskBucketMoved where the bucket's stream is gone or was created again under another
// epoch than the watch's, and nil otherwise: a read the server did not answer is logged and taken
// again at the next check, since a bucket that cannot be read cannot be repaired either.
func (b *taskBucket) moved(ctx context.Context, epoch string) error {
	stream, err := b.stream(ctx, legiontasks.Stream)
	switch {
	case errors.Is(err, jetstream.ErrStreamNotFound):
		b.log.Warn("legion daemon found the task bucket gone; it creates it again and writes every owed value", "bucket", legiontasks.Bucket)
		return errTaskBucketMoved
	case err != nil:
		if ctx.Err() == nil {
			b.log.Warn("legion daemon could not read the task bucket's stream; reading it again at the next check", "bucket", legiontasks.Bucket, "error", err)
		}
		return nil
	case legiontasks.Epoch(stream.CachedInfo().Created) != epoch:
		b.log.Warn("legion daemon found the task bucket created again; it writes every owed value into it", "bucket", legiontasks.Bucket,
			"epoch", legiontasks.Epoch(stream.CachedInfo().Created), "was", epoch)
		return errTaskBucketMoved
	}
	return nil
}

// taskValueRead is entry, an entry of the bucket at epoch, as its claim's machine is handed it. A
// put whose value is no task value (legiontasks.Decode) is held with no number, which the record's
// own number never matches.
func taskValueRead(entry jetstream.KeyValueEntry, epoch, cause string) supervise.TaskValueRead {
	read := supervise.TaskValueRead{Claim: claim.Token(entry.Key()), Cause: cause,
		Value: supervise.TaskValue{Epoch: epoch, Revision: entry.Revision()}}
	if entry.Operation() == jetstream.KeyValuePut {
		read.Held = true
		if value, err := legiontasks.Decode(entry.Value()); err == nil {
			read.Value.Number = value.Task
		}
	}
	return read
}
