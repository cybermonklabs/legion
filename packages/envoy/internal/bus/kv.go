package bus

import (
	"context"
	"errors"
	"fmt"
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
// a key written through it stays readable, watchable and deletable, and a value the bus refuses for
// its size, past the server's max payload or the bucket's largest value, is ErrTooLarge naming the
// size and the bound (tooLarge). Any other call is held only to the subject it builds itself, so
// a key an earlier build stored past the write bound still lists and deletes, and still reads
// while its get fits: a store serves it or skips it, and a reaper removes it.
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
	// request of a start is (refusedBy): the jetstream API has no context to wrap, so the send is.
	open := func() (jetstream.KeyValue, error) {
		ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
		defer cancel()
		return refusedBy(owner, []string{jsAPI("STREAM.INFO", "KV_"+kv.Bucket())}, func() (jetstream.KeyValue, error) {
			return js.KeyValue(ctx, kv.Bucket())
		})
	}
	if sendAgain {
		send := open
		open = func() (jetstream.KeyValue, error) { return readAgain(ctx, request, readLimitOf(ctx), send) }
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

// jsStreamMessageExceedsMaximum is nats-server's code for a message larger than its stream takes
// (JSStreamMessageExceedsMaximumErr): for a bucket, a value past the bucket's largest value
// (nats.KeyValueConfig.MaxValueSize). nats.go names no constant for it.
const jsStreamMessageExceedsMaximum = 10054

// MaxPayload is the largest message, headers and value together, that the server this handle's
// connection is on takes now (nats.Conn.MaxPayload, from the server's INFO, which a reconnect to
// another server renews). A caller that lays a value out to fit one message reads it at each
// write, so a server that raises or lowers its max payload changes the layout with no new build.
func (kv KeyValue) MaxPayload() int {
	return int(kv.streams.Conn().MaxPayload())
}

// tooLarge names the bus's refusal of a write of value bytes under key for the value's size as the
// ErrTooLarge it is, naming the size and the bound: nats.go's refusal of a message past the
// server's max payload, which it makes before sending anything (nats.ErrMaxPayload), and the
// stream's refusal of a value past the bucket's largest value (jsStreamMessageExceedsMaximum). The
// same write is refused at every retry, so its caller answers it as the caller's own input, a 413,
// rather than as a bus that did not answer. The bucket's bound is read from its stream only here,
// on the refusal, and a read of it that fails leaves it out of the error. Any other error is
// returned as it is.
func (kv KeyValue) tooLarge(key string, value int, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, nats.ErrMaxPayload) {
		return fmt.Errorf("%w: a value of %d bytes for key %q against the server's max payload of %d bytes: %w", ErrTooLarge, value, key, kv.MaxPayload(), err)
	}
	var apiErr *nats.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode != jsStreamMessageExceedsMaximum {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()
	stream, streamErr := kv.streams.Stream(ctx, "KV_"+kv.Bucket())
	if streamErr != nil {
		return fmt.Errorf("%w: a value of %d bytes for key %q past the largest value of bucket %s: %w", ErrTooLarge, value, key, kv.Bucket(), err)
	}
	return fmt.Errorf("%w: a value of %d bytes for key %q past the largest value of bucket %s, %d bytes with its headers: %w",
		ErrTooLarge, value, key, kv.Bucket(), stream.CachedInfo().Config.MaxMsgSize, err)
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
	return revision, kv.tooLarge(key, len(value), invalidKey(key, err))
}

func (kv KeyValue) PutString(key string, value string) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	revision, err := refusedAfterOn(kv, kv.keySubjects(key), func() (uint64, error) { return kv.legacy.PutString(key, value) })
	return revision, kv.tooLarge(key, len(value), invalidKey(key, err))
}

func (kv KeyValue) Create(key string, value []byte) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	revision, err := refusedAfterOn(kv, kv.keySubjects(key), func() (uint64, error) { return kv.legacy.Create(key, value) })
	return revision, kv.tooLarge(key, len(value), invalidKey(key, err))
}

func (kv KeyValue) Update(key string, value []byte, last uint64) (uint64, error) {
	if err := kv.write(key); err != nil {
		return 0, err
	}
	revision, err := refusedAfterOn(kv, kv.keySubjects(key), func() (uint64, error) { return kv.legacy.Update(key, value, last) })
	return revision, kv.tooLarge(key, len(value), invalidKey(key, err))
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
// returns the entry it read and deleted, whose revision the delete was fenced on and whose value a
// caller reads to remove what that value named (store's interest record chunks); nil when there
// was none to read.
//
// A key the read finds missing is deleted at no revision at all, which fences nothing: nats.go
// sends no expected-last-subject-sequence header for revision 0, so that delete would take
// whatever the bucket holds, and a write that landed between the read and it - a registration
// storing the row this delete read none of - would be deleted whole. So no delete is made for it:
// the key is reported absent, with no entry, and there is nothing there to remove.
//
// A key it may not read (ErrRefused) it may not write either, since a write is held to the longest
// bound and nats.go refuses a key outside its alphabet everywhere, so nothing can land in between:
// that key is deleted at no revision, with no entry, which removes a key an earlier build stored
// past the write bound, and a key outside the alphabet is refused by the delete too.
func (kv KeyValue) DeleteAtRead(key string) (nats.KeyValueEntry, error) {
	entry, err := kv.Get(key)
	switch {
	case err == nil:
		return entry, kv.Delete(key, nats.LastRevision(entry.Revision()))
	case errors.Is(err, nats.ErrKeyNotFound):
		return nil, nil
	case errors.Is(err, ErrRefused):
		return nil, kv.Delete(key)
	default:
		return nil, err
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
