package roles

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// readTimeout bounds one read of Buckets, the bucket's open included, within the caller's own
// context: the caller is answered once it passes (within). nats.go's KV calls take no context and
// wait out the JetStream MaxWait (10 s), so a session read that has not answered by then is left
// to finish on its own, and so is a bucket's open (bucket says why it takes the caller's
// context); a claim read from the stream's leader takes the bound as its context and ends with it.
const readTimeout = 2 * time.Second

// JetStream is the client Buckets reads through: its JetStream context on the current connection
// (bus.Client.JS) and that connection itself (bus.Client.Connection), which a bucket handle's
// watch side is opened on (bus.OpenKeyValue).
type JetStream interface {
	JS() nats.JetStreamContext
	Connection() *nats.Conn
}

// Buckets is a ClaimStore and a SessionLookup that read the role bucket and the session bucket
// themselves, one read per call, with no watcher and no cache: what Dispatch's outbox resolves a
// role-routed event's holder through. A claim is read from the role bucket stream's leader, a
// session record by a direct get. Each bucket is opened on the client's connection at its first
// read, and again once the client has replaced that connection. A missing role bucket holds no
// claim; a missing session bucket is a read that fails, since it says nothing of whether a holder
// is there.
//
// It writes nothing: a role's claim is the listener's to release, and only once its holder is
// gone (the package comment states the rule). The Windows it carries are what a holder's record
// is judged by, and they are the only measure of a holder here: how long the session bucket keeps
// a record is a storage window and no verdict on the session, so a record the bucket still holds
// past its maximum age is judged by its own last registration, as every other reader judges one.
type Buckets struct {
	Windows

	client JetStream

	mu       sync.Mutex
	js       nats.JetStreamContext
	roles    *bus.KeyValue
	sessions *bus.KeyValue
}

// NewBuckets returns Buckets reading through client and judging a holder's record by windows. It
// takes the windows rather than defaulting them, so no caller reaches the package's compiled
// windows by forgetting to say which it means: a process judging a holder on windows its
// deployment did not set calls a holder the listener still calls unresponsive gone, and marks
// its event published with nothing sent. A caller that does mean the compiled windows passes a
// zero Windows, whose own zero fields are session.LiveWindow and session.GoneHorizon.
func NewBuckets(client JetStream, windows Windows) *Buckets {
	return &Buckets{Windows: windows, client: client}
}

// openBucketHook, when set, runs before each bucket open (bucket). It is nil outside tests.
var openBucketHook atomic.Pointer[func()]

// bucket returns the handle of bucket on the client's current connection, opening it within ctx
// when it is not open on that connection yet.
//
// b.mu guards the handles alone, and the open is made with it released: the open sends a
// STREAM.INFO, which waits for nats.go's connection lock, and nats.go holds that lock through a
// reconnect's dial and handshake. Held across the open, b.mu kept every other read of Buckets
// waiting behind that handshake. The read that makes the open answers its caller at its own bound
// (within); an open still waiting then goes on to ctx's end or the JetStream MaxWait, holding
// nothing. Two reads that both find a handle missing each open one, and the first to finish keeps
// its handle; an open reads the bucket's stream (two STREAM.INFO requests, the legacy handle's and
// the jetstream handle's) and leaves nothing behind.
func (b *Buckets) bucket(ctx context.Context, name string) (bus.KeyValue, error) {
	js := b.client.JS()
	b.mu.Lock()
	if js != b.js {
		b.js, b.roles, b.sessions = js, nil, nil
	}
	handle := &b.roles
	if name == bus.SessionBucket {
		handle = &b.sessions
	}
	if *handle != nil {
		kv := **handle
		b.mu.Unlock()
		return kv, nil
	}
	b.mu.Unlock()
	conn := b.client.Connection()
	if hook := openBucketHook.Load(); hook != nil {
		(*hook)()
	}
	kv, err := bus.OpenKeyValue(ctx, conn, js, name)
	if err != nil {
		return bus.KeyValue{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if js == b.js && *handle == nil {
		*handle = &kv
	}
	return kv, nil
}

// within runs read, waiting for it no longer than ctx and at most readTimeout (bus.Bounded), and
// hands read that bound as its own context, for a read that takes one. A read that did not answer
// in time is named by what.
func within[T any](ctx context.Context, what string, read func(bounded context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	value, answered, err := bus.Bounded(ctx, func() (T, error) { return read(ctx) })
	if !answered {
		return value, fmt.Errorf("%s: %w", what, err)
	}
	return value, err
}

// RoleClaim reads role's claim from the role bucket stream's leader within ctx
// (bus.KeyValue.ReadFromLeader), decoded as the listener decodes every claim it reads to decide
// which session a role's message reaches (store.DecodeLeaderRoleClaim): a zero claim without an
// error when there is no role bucket, when the bucket holds no claim for the role and when its
// newest message is a delete or purge marker; store.ErrMalformedRoleClaim for a message the server
// wrote under the role's key, whatever its body names, and for a record no claim decodes from; and
// the claim stamped with the sequence the leader stores it at otherwise. A direct get would read a
// message the server wrote as the claim of whichever session its body names, and the outbox would
// forward the role's events to that session, and the reply check would take its reply for the
// holder's (Dispatch's api.lookUpRoleHolderReply).
func (b *Buckets) RoleClaim(ctx context.Context, role string) (store.RoleClaim, error) {
	return within(ctx, "read role "+role+"'s claim", func(bounded context.Context) (store.RoleClaim, error) {
		// The open takes the caller's ctx, not the bound: an open still waiting when the bound
		// passes goes on and keeps the handle for the next read (bucket).
		kv, err := b.bucket(ctx, bus.RoleClaimBucket)
		if errors.Is(err, nats.ErrBucketNotFound) {
			return store.RoleClaim{}, nil
		}
		if err != nil {
			return store.RoleClaim{}, err
		}
		leader, err := kv.ReadFromLeader(bounded, role)
		if err != nil {
			return store.RoleClaim{}, err
		}
		return store.DecodeLeaderRoleClaim(role, leader)
	})
}

// Holder reads sessionID's record from the session bucket, whatever state that record is in:
// judging it is Windows.EntryState's, so a record the bucket still holds past its maximum age is
// answered here and judged by its own last registration. A key the bucket does not hold, one no
// read may name (bus.ErrRefused), a value that does not decode and one that is not sessionID's
// registration (session.DecodeEntry: another session's record copied under the key, a JetStream
// answer) are nats.ErrKeyNotFound: no record stands for the session. A record an earlier build
// wrote, naming no key, is read as sessionID's, as every listener but the edge reads it. Any
// other error is a read that did not answer.
func (b *Buckets) Holder(ctx context.Context, sessionID string) (session.SessionEntry, error) {
	entry, err := within(ctx, "read session "+sessionID, func(context.Context) (nats.KeyValueEntry, error) {
		kv, err := b.bucket(ctx, bus.SessionBucket)
		if err != nil {
			return nil, err
		}
		return kv.Get(sessionID)
	})
	if errors.Is(err, bus.ErrRefused) {
		return session.SessionEntry{}, fmt.Errorf("%w: %w", nats.ErrKeyNotFound, err)
	}
	if err != nil {
		return session.SessionEntry{}, err
	}
	holder, err := session.DecodeEntry(sessionID, entry.Value())
	if err != nil {
		return session.SessionEntry{}, fmt.Errorf("%w: session %s's record is not read as its registration: %w", nats.ErrKeyNotFound, sessionID, err)
	}
	return holder, nil
}

// LastSeen is 0 for every session: Dispatch reads the session bucket and keeps no memory of a
// record that has gone, so a holder whose record the bucket no longer holds is one nothing here
// can date (Resolution.LastSeen says what a caller makes of that).
func (b *Buckets) LastSeen(string) int64 { return 0 }
