// The reading of the server's refusals for the calls a KeyValue makes: the record of the client
// that owns the handle's connection, read for a call as it arrives or once the call is left
// unanswered, the subjects each call lists, and the shape of the create a watch sends.

package bus

import (
	"reflect"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// refusedOn is call, a request kv makes once in a while of its bucket's stream (a read of the
// stream), read for the server's refusal of subjects as it arrives (refusedBy), in the record of
// the client that owns kv's connection, found when kv was built. A handle on a connection no
// client owns sends as call does.
func refusedOn[T any](kv KeyValue, subjects []string, call func() (T, error)) (T, error) {
	return refusedBy(kv.owner, subjects, call)
}

// watchRefused is watch, the construction of a watcher whose consumer create has the shape create
// describes, read for the server's refusal of a create of that shape as it arrives
// (refusedByRecord), in the record of the client that owns kv's connection: a watch the grant
// refuses ends at once naming its refusal, and one the grant allows returns its own watcher
// whatever other request on the connection the server refuses meanwhile. A refusal that answers
// the watch before its construction returns leaves that construction running to its own end, and
// a watcher it then builds is stopped and read until it closes, since no caller holds it (a grant
// that refuses a consumer by name is the one way to get there: watchCreate.matches). A handle on a
// connection no client owns watches as watch does.
func (kv KeyValue) watchRefused(create watchCreate, watch func() (jetstream.KeyWatcher, error)) (jetstream.KeyWatcher, error) {
	if kv.owner == nil {
		return watch()
	}
	denied := func(sentAt time.Time) error { return kv.owner.deniedPublishMatching(create.matches, sentAt) }
	return refusedByRecord(kv.owner, denied, watch, func(watcher jetstream.KeyWatcher) {
		_ = watcher.Stop()
		DrainWatcher(watcher)
	})
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

// keySubjects is the subject a write of key publishes to: "$KV.<bucket>.<key>" (nats.go@v1.55.0
// kv.go:383-385, the default KV prefix).
func (kv KeyValue) keySubjects(key string) func() []string {
	return func() []string { return []string{"$KV." + kv.Bucket() + "." + key} }
}

// getSubjects is the one request a read of key's last value sends: a direct get of the key's last
// message through a handle that reads directly, and a get from the stream's leader through one
// that does not (readsDirect; nats.go@v1.55.0 kv.go:642-686, js.go:210-217, jsm.go:1304-1332).
// The other is not listed: a read takes the refusal of any subject it lists, and a refusal of a
// request it never sends is another read's to answer.
func (kv KeyValue) getSubjects(key string) func() []string {
	return func() []string {
		stream := "KV_" + kv.Bucket()
		if readsDirect(kv.legacy) {
			return []string{jsAPI("DIRECT.GET", stream, "$KV."+kv.Bucket()+"."+key)}
		}
		return []string{jsAPI("STREAM.MSG.GET", stream)}
	}
}

// revisionSubjects is the one request a read of one revision sends: a direct get by sequence
// through a handle that reads directly, and a get from the stream's leader through one that does
// not (readsDirect; nats.go@v1.55.0 kv.go:642-668, js.go:210-214, jsm.go:1304-1332).
func (kv KeyValue) revisionSubjects() []string {
	stream := "KV_" + kv.Bucket()
	if readsDirect(kv.legacy) {
		return []string{jsAPI("DIRECT.GET", stream)}
	}
	return []string{jsAPI("STREAM.MSG.GET", stream)}
}

// natsPackage is the import path of nats.go, whose legacy KV handle readsDirect reads.
var natsPackage = reflect.TypeFor[nats.Conn]().PkgPath()

// readsDirect reports whether legacy, a legacy KV handle, sends its reads of a key and of a
// revision as direct gets, which any copy of the bucket answers, rather than as gets from the
// stream's leader. A handle this package wraps around nats.go's states it (statedReads), and is
// asked first: an edge listener's (streamMessageReads, WithStreamMessageReads) sends every read
// as a get from the stream's leader whatever its stream allows, so the nats.go handle inside it,
// built for a stream that allows direct gets, would answer for a request it never sends. nats.go
// decides that for its own handle once, as it builds the handle, from its stream's
// AllowDirect, keeps it in the handle's unexported useDirect, and reports it through no method
// (nats.go@v1.55.0 kv.go:562-574, 642-668, 1353-1364), so it is read off the handle: a read of the
// stream now would cost a request, and could answer otherwise than the read the handle was built
// from. Only an unanswered read asks this (refusedAfter), so a read the server answers pays
// nothing for it.
//
// Any other handle nats.go did not build, a test's double (NewKeyValue), states nothing and is
// taken to read directly, whatever handle it wraps, as nats.go's handle on every bucket this build
// creates does (Get). So is a nats.go handle that holds no such field:
// TestAReadTakesOnlyTheRefusalOfTheRequestItsHandleSends fails on it, since its handle on a stream
// without AllowDirect would then be read as a direct one.
func readsDirect(legacy nats.KeyValue) bool {
	if stated, ok := legacy.(statedReads); ok {
		return stated.readsDirectly()
	}
	handle := reflect.ValueOf(legacy)
	if handle.Kind() != reflect.Pointer || handle.IsNil() {
		return true
	}
	kvs := handle.Elem()
	if kvs.Kind() != reflect.Struct || kvs.Type().PkgPath() != natsPackage {
		return true
	}
	useDirect := kvs.FieldByName("useDirect")
	if useDirect.Kind() != reflect.Bool {
		return true
	}
	return useDirect.Bool()
}

// statedReads is a legacy KV handle that states how it sends its reads of a key and of a revision,
// which readsDirect asks before it reads nats.go's handle: one this package wraps around nats.go's
// whose reads are not the requests nats.go's handle would send (streamMessageReads).
type statedReads interface {
	// readsDirectly reports whether the handle sends its reads as direct gets.
	readsDirectly() bool
}

// watchCreate is the shape of the request a watch sends to create its consumer, as nats.go builds
// it (nats.go@v1.55.0 jetstream/kv.go:1356-1361, js.go:1877 and 1963-1969, jsm.go:505-525):
// prefix, `$JS.API.CONSUMER.CREATE.KV_<bucket>.`, then the consumer's name, then, for a watch of
// one key, a dot and filter, `$KV.<bucket>.<key>` (every key, `$KV.<bucket>.>`, for a watch given
// none, jetstream/kv.go:1268-1274). A watch of several keys sends their filters in the request's
// body and none in its subject, so its filter is empty.
type watchCreate struct {
	prefix, filter string
}

// watchCreate is the shape of the create a watch of keys through kv sends.
func (kv KeyValue) watchCreate(keys []string) watchCreate {
	create := watchCreate{prefix: jsAPI("CONSUMER.CREATE", "KV_"+kv.Bucket()) + "."}
	switch len(keys) {
	case 0:
		create.filter = "$KV." + kv.Bucket() + "." + jetstream.AllKeys
	case 1:
		create.filter = "$KV." + kv.Bucket() + "." + keys[0]
	}
	return create
}

// matches reports whether subject is a create of this shape: prefix, one token for the name, and
// then a dot and the filter, or nothing after the name when there is no filter. nats.go picks the
// name at random as it sends the create (getHash(nuid.Next()), js.go:1968), so a grant cannot
// tell two creates of one shape apart by it: the refusal of a create of this shape is the refusal
// this watch's own create meets. A create of the bucket's stream of another shape -- a watch of
// another key, or a watch of one key beside one of several -- is another request, which a grant can
// refuse while it allows this watch's, so it never answers this watch. The one exception is a grant
// that refuses one consumer by name, as it can a durable's: the refusal of that consumer's create
// answers a watch of the same shape too, before the watch's own create, which the grant allows, has
// built its watcher, and watchRefused stops that watcher.
func (create watchCreate) matches(subject string) bool {
	rest, ok := strings.CutPrefix(subject, create.prefix)
	if !ok {
		return false
	}
	name, filter, filtered := strings.Cut(rest, ".")
	if name == "" {
		return false
	}
	if create.filter == "" {
		return !filtered
	}
	return filtered && filter == create.filter
}
