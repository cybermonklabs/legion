package consumernames

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nats-io/nats.go"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// The cutover families, as the broker's cutover rows name them (holdrecord.Client.PutCutover):
// the session consumers and every listener's durable, whose records are both in the selection
// bucket; Dispatch's two durables; and each Legion daemon, by its project key.
const (
	familySession      = "session"
	familyDispatch     = "dispatch"
	familyDaemonPrefix = "daemon/"
)

// daemonFamily is the cutover family of the daemon of project.
func daemonFamily(project string) string { return familyDaemonPrefix + project }

// selectionStreamName is the stream of the selection bucket (selection.SessionBucket), which holds
// the session family's records.
const selectionStreamName = "KV_" + selection.SessionBucket

// pgFamilies are the families whose store is a Postgres intake_consumers table: Dispatch, and each
// daemon that --daemon-database, the broker's cutover rows or a consumer of names (status passes
// the notification stream's; cleanup none) names, in order.
func pgFamilies(o options, cutovers []holdrecord.CutoverRow, names ...string) []string {
	daemons := map[string]bool{}
	for _, project := range o.daemonProjects() {
		daemons[daemonFamily(project)] = true
	}
	for _, c := range cutovers {
		if strings.HasPrefix(c.Family, familyDaemonPrefix) {
			daemons[c.Family] = true
		}
	}
	for _, name := range names {
		if m, ok := classify(selection.LogicalOf(name)); ok && m.Project != "" {
			daemons[m.Family] = true
		}
	}
	return append([]string{familyDispatch}, slices.Sorted(maps.Keys(daemons))...)
}

// The fixed names' forms.
const (
	sessionFixedPrefix  = "s-"
	sessionFreshPrefix  = "session"
	listenerFixedPrefix = "listener-"
	daemonFixedPrefix   = "legion-go-"
)

// daemonKinds are the kinds of consumer a Legion daemon's grant names per project,
// legion-go-<project>-<kind> (natsgrant newHeldScope: dispatch, wake, ready, github); the daemon
// itself opens dispatch, github and wake (packages/daemon/internal/intake, dispatchConsumerName,
// githubConsumerName, wakeConsumerName).
var daemonKinds = []string{"dispatch", "github", "wake", "ready"}

// member is one logical name of a family as the operator's commands meet it on the stream.
type member struct {
	// Family is its cutover family.
	Family string
	// Logical is its record's key in its family's store: the session id; the listener durable's
	// selection.DurableKey; Dispatch's or the daemon's durable name.
	Logical string
	// Fixed is its fixed name, which every release before the fresh names created.
	Fixed string
	// Prefix is the part of each fresh name before its '~' (selection.FreshName).
	Prefix string
	// SessionID is the session a session consumer delivers to, "" for any other member.
	SessionID string
	// Project is the daemon's project key, "" for any other member.
	Project string
}

// Metadata is the metadata every fresh consumer of the member carries: the logical name it is
// selected under (selection.LogicalMetadataKey), and a session consumer's session
// (bus.SessionIDMetadataKey), which the listener's session listing selects by.
func (m member) Metadata() map[string]string {
	metadata := map[string]string{selection.LogicalMetadataKey: m.logicalMetadata()}
	if m.SessionID != "" {
		metadata[bus.SessionIDMetadataKey] = m.SessionID
	}
	return metadata
}

// logicalMetadata is the logical name a fresh consumer's envoy.logical carries: the session id
// or the durable's own name, as the families' creators write it.
func (m member) logicalMetadata() string {
	if m.SessionID != "" {
		return m.SessionID
	}
	return m.Fixed
}

// classify is the member a consumer named name on the notification stream belongs to, by its
// fixed name, and false for a name that is no family's fixed name: every fresh name holds a '~',
// which no fixed name does.
func classify(name string) (member, bool) {
	if strings.Contains(name, "~") || name == "" {
		return member{}, false
	}
	if id, ok := strings.CutPrefix(name, sessionFixedPrefix); ok && id != "" {
		return member{Family: familySession, Logical: id, Fixed: name, Prefix: sessionFreshPrefix, SessionID: id}, true
	}
	if strings.HasPrefix(name, listenerFixedPrefix) && len(name) > len(listenerFixedPrefix) {
		return member{Family: familySession, Logical: selection.DurableKey(name), Fixed: name, Prefix: name}, true
	}
	if slices.Contains(dispatchDurables(), name) {
		return member{Family: familyDispatch, Logical: name, Fixed: name, Prefix: name}, true
	}
	if rest, ok := strings.CutPrefix(name, daemonFixedPrefix); ok {
		cut := strings.LastIndexByte(rest, '-')
		if cut > 0 && slices.Contains(daemonKinds, rest[cut+1:]) {
			project := rest[:cut]
			return member{Family: daemonFamily(project), Logical: name, Fixed: name, Prefix: name, Project: project}, true
		}
	}
	return member{}, false
}

// dispatchDurables are Dispatch's two durables' names (bus.DispatchDeliveryDurable,
// bus.DispatchJobDurable), the names its intakes bind.
func dispatchDurables() []string {
	return []string{bus.DispatchDeliveryDurable, bus.DispatchJobDurable}
}

// store is family's selection store on the run's reach: the selection bucket for the session
// family, through the operator's connection (newSessionStore), and the family's intake_consumers
// rows for Dispatch and each daemon (newPGFamilyStore).
func (r *run) store(ctx context.Context, family string) (familyStore, error) {
	if family == familySession {
		nc, _, err := r.conn(ctx)
		if err != nil {
			return nil, err
		}
		return newSessionStore(nc), nil
	}
	if family != familyDispatch && !strings.HasPrefix(family, familyDaemonPrefix) {
		return nil, fmt.Errorf("no family %q", family)
	}
	db, err := r.database(ctx, family)
	if err != nil {
		return nil, err
	}
	store, err := newPGFamilyStore(family, db)
	if err != nil {
		return nil, err
	}
	return store, nil
}

// pgStoreOf is family's Postgres store (pgStore) on the run's database of it, a failed open of
// the database naming the family's store.
func (r *run) pgStoreOf(ctx context.Context, family string) (*selection.PGStore, error) {
	db, err := r.database(ctx, family)
	if err != nil {
		return nil, fmt.Errorf("%s's store: %w", family, err)
	}
	return pgStore(family, db)
}

// pgStore is family's Postgres store on db (selection.NewPGStore), its refusal of a family no
// store keeps (selection.ErrInvalidFamily) naming the family.
func pgStore(family string, db Database) (*selection.PGStore, error) {
	store, err := selection.NewPGStore(family, db)
	if err != nil {
		return nil, fmt.Errorf("the %s family's store: %w", family, err)
	}
	return store, nil
}

// familyStore is a family's store with its sentinel's write, which every store the operator
// writes has (selection.KVStore, selection.PGStore), and its sentinel's read with the revision a
// write of it is conditional on, which the library's State does not answer.
type familyStore interface {
	selection.Store
	WriteSentinel(ctx context.Context, state selection.StoreState, revision uint64) (uint64, error)
	// sentinel is the store's sentinel, read through the store's own check (its State), and its
	// revision, read before it, and found false for a store that holds none yet. A sentinel that
	// is not exactly the store's own is the store's selection.ErrMalformedSentinel, never none.
	sentinel(ctx context.Context) (selection.StoreState, uint64, bool, error)
}

// sessionStore is the session family's store (selection.KVStore) with nc, the connection the
// revision of its sentinel is read on.
type sessionStore struct {
	*selection.KVStore
	nc *nats.Conn
}

// newSessionStore is the session family's store on nc.
func newSessionStore(nc *nats.Conn) sessionStore {
	return sessionStore{KVStore: selection.NewKVStore(selection.ConnRequester{Conn: nc}), nc: nc}
}

// sentinel implements familyStore: the sequence of the sentinel key's last message, read from the
// stream's leader (replayLeaderRead), then the sentinel through selection.KVStore.State. A key with
// no message, or whose last is a delete or purge marker, holds none. A write between the two reads
// is after the sequence, so a write conditional on it is refused and read again.
func (s sessionStore) sentinel(ctx context.Context) (selection.StoreState, uint64, bool, error) {
	last, found, err := replayLeaderRead(ctx, s.nc, selection.SessionBucket, selection.SentinelKey)
	if err != nil || !found || last.Op != "" {
		return selection.StoreState{}, 0, false, err
	}
	state, err := s.State(ctx)
	switch {
	case errors.Is(err, selection.ErrNoSentinel):
		return selection.StoreState{}, 0, false, nil
	case err != nil:
		return selection.StoreState{}, 0, false, err
	}
	return state, last.Seq, true, nil
}

// pgFamilyStore is Dispatch's or a daemon's store (selection.PGStore) of family on db.
type pgFamilyStore struct {
	*selection.PGStore
	db     Database
	family string
}

// newPGFamilyStore is family's store on db (pgStore).
func newPGFamilyStore(family string, db Database) (pgFamilyStore, error) {
	store, err := pgStore(family, db)
	if err != nil {
		return pgFamilyStore{}, err
	}
	return pgFamilyStore{PGStore: store, db: db, family: family}, nil
}

// sentinel implements familyStore (readPGSentinel).
func (s pgFamilyStore) sentinel(ctx context.Context) (selection.StoreState, uint64, bool, error) {
	return readPGSentinel(ctx, s.db, s.family)
}

// pgUndefinedTable is Postgres's SQLSTATE for a relation that does not exist: a store whose table
// no migration made.
const pgUndefinedTable = "42P01"

// pgNoTable reports whether err is Postgres's answer for a table that does not exist.
func pgNoTable(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == pgUndefinedTable
}

// readPGSentinel reads family's sentinel row of the intake_consumers table on db
// (selection.PGSentinelKey) with its revision, which selection.PGStore.State does not answer and
// the conditional write needs (selection.PGStore.WriteSentinel), and found false for a table that
// holds no such row. The revision is read first and the state after it through the family's store
// (selection.PGStore.State), whose check refuses a row that is not exactly the family's own
// sentinel (another key's, another family's or none, a field this build does not know) as
// selection.ErrMalformedSentinel, never a missing row: a write between the two reads is after the
// revision, so a write conditional on it is refused and read again. A row gone between the two
// reads is none. Its error is the query's or the store's, which pgNoTable reads for a table that
// does not exist.
func readPGSentinel(ctx context.Context, db Database, family string) (state selection.StoreState, revision uint64, found bool, err error) {
	store, err := pgStore(family, db)
	if err != nil {
		return selection.StoreState{}, 0, false, err
	}
	stored, err := pgRevision(ctx, db, selection.PGSentinelKey(family))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return selection.StoreState{}, 0, false, nil
	case err != nil:
		return selection.StoreState{}, 0, false, err
	}
	state, err = store.State(ctx)
	switch {
	case errors.Is(err, selection.ErrNoSentinel):
		return selection.StoreState{}, 0, false, nil
	case err != nil:
		return selection.StoreState{}, 0, false, err
	}
	return state, uint64(stored), true, nil
}

// pgRevision is the revision of the intake_consumers row keyed key on db, its query's error as it
// is: readPGSentinel reads pgx.ErrNoRows as no sentinel, and replayRevision any error as UNKNOWN.
func pgRevision(ctx context.Context, db Database, key string) (int64, error) {
	var revision int64
	err := db.QueryRow(ctx, `select revision from `+selection.IntakeConsumersTable+` where logical = $1`, key).Scan(&revision)
	return revision, err
}

// behindBarrier runs read behind the hold's bucket barrier on the selection bucket, the one path
// every held reader of the bucket takes (writerhold.ReadBucket: the barrier put
// hold.barrier.<hold id>, then read, with the stream's leader read before the barrier and after
// the read, and both made again when it changed), its UNKNOWN answered as this package's. read's
// own error comes back as it is. The bucket must exist: ReadBucket's leader read of a stream the
// server does not hold is UNKNOWN, so a reader that answers a missing bucket otherwise reads its
// stream first.
func behindBarrier(ctx context.Context, nc *nats.Conn, holdID string, read func(context.Context) error) error {
	return hubUnknown(writerhold.ReadBucket(ctx, nc, selection.SessionBucket, holdID, read),
		"the bucket barrier of "+selection.SessionBucket)
}

// errEscapeCut is a selection.DurableKey key that ends inside an escape (durableOf).
var errEscapeCut = errors.New("ends inside an escape")

// durableOf is the durable name a selection.DurableKey key names: the key's prefix cut, and each
// '=' and the two hex digits after it written back as the byte they stand for. A key that ends
// inside an escape is errEscapeCut, and one holding an escape that is not two hex digits is refused
// too, each error naming what is wrong for its caller to name the key before it. It does not hold
// key to the form DurableKey writes: replay and restore compare the member's key with it
// (replayMember, restoreMember), and the census reads a key in another form as no durable's
// (censusDurableOf).
func durableOf(key string) (string, error) {
	escaped, _ := strings.CutPrefix(key, selection.DurableKey(""))
	var name strings.Builder
	for rest := escaped; rest != ""; {
		before, after, found := strings.Cut(rest, "=")
		name.WriteString(before)
		if !found {
			break
		}
		if len(after) < 2 {
			return "", errEscapeCut
		}
		b, err := strconv.ParseUint(after[:2], 16, 8)
		if err != nil {
			return "", fmt.Errorf("holds the escape =%s, which is not '=' and two hex digits", after[:2])
		}
		name.WriteByte(byte(b))
		rest = after[2:]
	}
	return name.String(), nil
}
