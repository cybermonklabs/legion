package consumernames

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/nats-io/nats.go/jetstream"
)

// LogicalMetadataKey is the consumer metadata every physical consumer carries, naming its logical
// name, so a list of the stream's consumers maps each one to its record without a name pattern.
const LogicalMetadataKey = "envoy.logical"

// StreamIdentityMetadataKey is the stream metadata that holds a stream's identity (the listener
// writes it on ENVOY_NOTIFICATIONS when it creates the stream). A last floor counts only on the
// stream whose identity it carries, so every family passes the same identity (StreamIdentity).
const StreamIdentityMetadataKey = "envoy_stream_id"

// StreamIdentity is info's stream identity (StreamIdentityMetadataKey), "" when it carries none.
func StreamIdentity(info *jetstream.StreamInfo) string {
	if info == nil {
		return ""
	}
	return info.Config.Metadata[StreamIdentityMetadataKey]
}

// The cutover markers a store's sentinel holds (StoreState.Cutover).
const (
	// CutoverDone is written by the cutover's convert once every fixed name of the family is
	// converted. It is the one marker under which Names serves a call (Admit).
	CutoverDone = "done"
	// CutoverRolledBack is written by the rollback's restore, so a later build in the fresh mode
	// waits for the next convert.
	CutoverRolledBack = "rolled_back"
)

// The operations a record holds in flight (Op.Kind).
type OpKind string

const (
	OpCreate  OpKind = "create"
	OpReplace OpKind = "replace"
	OpDelete  OpKind = "delete"
)

// Native is a native deliver policy a first create keeps (Start.Native).
type Native string

const (
	// NativeNew is jetstream.DeliverNewPolicy.
	NativeNew Native = "new"
	// NativeAll is jetstream.DeliverAllPolicy.
	NativeAll Native = "all"
)

// Record is one logical name's selection record.
type Record struct {
	// Key is the store key the record belongs to: in the selection bucket the logical name, or a
	// durable's key (DurableKey); in Postgres the family's key of the logical name (PGKey,
	// <family>/<logical>). A store's Write sets it and its Read refuses a record whose key is not
	// the key it read (DecodeRecord), so a record of one key, or of another family, copied whole
	// under another is no record there.
	Key string `json:"key"`
	// Current is the selected physical name, or "".
	Current string `json:"current,omitempty"`
	// Op is the operation in flight, or nil.
	Op *Op `json:"op,omitempty"`
	// Start is the saved start: written before any destructive step, and, after a publish, the
	// start Current was created at. 0 when none is saved.
	Start uint64 `json:"start,omitempty"`
	// LastFloor is the last ack floor read of Current, with the name it was read from and the
	// stream's identity. A publish clears it, so Start governs until the new consumer's own floor
	// is written.
	LastFloor *Floor `json:"last_floor,omitempty"`
	// Retired are names never selected again; cleanup deletes only these (Names.Cleanup).
	Retired []string `json:"retired,omitempty"`
	// Abandoned are the retired candidates whose create may still have been in flight when they
	// were retired (Step 0's or a retry's), each with the time until which its client still waits
	// for it: the retirement and the create's own bound (Op.CreateWithin), on the retirer's clock.
	// Cleanup drops one the server does not hold only once it reads it absent at or after that time
	// and its own settle age (Config.SettleAge, the clock skew it allows), so a create that lands
	// within its bound, after the candidate was retired, leaves a consumer the record still names,
	// which the next cleanup deletes. One the server applies past that bound, which is only the
	// client's deadline, leaves a consumer no record names: a stated limit (README, "Lifetime").
	// Each is also in Retired.
	Abandoned []Abandoned `json:"abandoned,omitempty"`
	// Restore is a rollback in progress, which the rollback command writes; Names never writes it.
	Restore *Restore `json:"restore,omitempty"`
}

// Abandoned is a retired candidate whose create its client may still wait for until Until.
type Abandoned struct {
	Name  string    `json:"name"`
	Until time.Time `json:"until"`
}

// Op is an operation in flight.
type Op struct {
	Kind OpKind `json:"kind"`
	// Owner names the process that reserved it.
	Owner string `json:"owner,omitempty"`
	// Candidate is the physical name a create or a replace creates.
	Candidate string `json:"candidate,omitempty"`
	// Old is the name a replace or a delete takes away.
	Old string `json:"old,omitempty"`
	// FloorRecorded is set once a delete wrote Old's floor into Record.Start.
	FloorRecorded bool `json:"floor_recorded,omitempty"`
	// StartPending marks a replace reserved with no start (Names.Replace at start 0): its start is
	// read from Old's floor after the reservation (saveReplaceStart), and until then Record.Start is
	// still the start the record held before, which a call that closes the replace without creating
	// keeps.
	StartPending bool `json:"start_pending,omitempty"`
	// CreateWithin is how long the reserving process's create of Candidate can run (its
	// Config.CreateBound): a process that retires the candidate without seeing its create end
	// keeps it named for that long after (Record.Abandoned).
	CreateWithin time.Duration `json:"create_within,omitempty"`
	// Native is the native policy the candidate's create keeps, for a native first create, whose
	// Record.Start is then the numeric boundary recovery creates at instead, or 0 where the first
	// create read no stream (Config.NativeFirstCreate): a retry then keeps the native policy too.
	Native Native `json:"native,omitempty"`
	// ReservedAt is when the operation was reserved, on the reserving process's clock.
	ReservedAt time.Time `json:"reserved_at"`
}

// Floor is an ack floor and where it was read.
type Floor struct {
	Floor    uint64 `json:"floor"`
	Name     string `json:"name"`
	StreamID string `json:"stream_id"`
}

// Restore is a rollback in progress (the rollback command's).
type Restore struct {
	Target string `json:"target"`
	Start  uint64 `json:"start"`
	Phase  string `json:"phase"`
}

// clone is a copy of r that shares nothing with it.
func (r Record) clone() Record {
	c := r
	if r.Op != nil {
		op := *r.Op
		c.Op = &op
	}
	if r.LastFloor != nil {
		floor := *r.LastFloor
		c.LastFloor = &floor
	}
	if r.Restore != nil {
		restore := *r.Restore
		c.Restore = &restore
	}
	c.Retired = slices.Clone(r.Retired)
	c.Abandoned = slices.Clone(r.Abandoned)
	return c
}

// retire is r with name added to Retired, once.
func (r Record) retire(names ...string) Record {
	for _, name := range names {
		if name != "" && !slices.Contains(r.Retired, name) {
			r.Retired = append(slices.Clone(r.Retired), name)
		}
	}
	return r
}

// abandon is r with candidate retired (retire) and kept until until, the time its create can no
// longer land (Record.Abandoned).
func (r Record) abandon(candidate string, until time.Time) Record {
	if candidate == "" {
		return r
	}
	r = r.retire(candidate)
	r.Abandoned = append(slices.DeleteFunc(slices.Clone(r.Abandoned), func(a Abandoned) bool { return a.Name == candidate }),
		Abandoned{Name: candidate, Until: until})
	return r
}

// DropRetired is r with name, one of its retired names, dropped from Retired and Abandoned
// together: the one rule Names.Cleanup drops a retired name by, for a reader that deletes retired
// names itself (the operator's cleanup), so the record it writes stays one the stores read (each
// abandoned name is also retired). deleted says the caller deleted the consumer, else it read the
// name absent. An abandoned candidate read absent while now is before its until (Abandoned.Until)
// and the settle age after it (defaultSettleAge, 2 s) is one whose create its client may still
// wait for, and the consumer that create makes must stay named, so it is kept: the answer is then
// r as it was and false, as it is for a name r does not retire. The settle age is the skew allowed
// between the clock that wrote until, the retirer's, and now's, the dropper's, which can be
// another host's. r itself is never changed.
func (r Record) DropRetired(name string, deleted bool, now time.Time) (Record, bool) {
	return r.dropRetired(name, deleted, now, defaultSettleAge)
}

// dropRetired is DropRetired with skew, the dropper's Config.SettleAge, as the clock skew it
// allows beside an abandoned name's until.
func (r Record) dropRetired(name string, deleted bool, now time.Time, skew time.Duration) (Record, bool) {
	if !slices.Contains(r.Retired, name) {
		return r, false
	}
	if until, ok := r.abandonedUntil(name); ok && !deleted && now.Before(until.Add(skew)) {
		return r, false
	}
	r.Retired = slices.DeleteFunc(slices.Clone(r.Retired), func(retired string) bool { return retired == name })
	if len(r.Retired) == 0 {
		r.Retired = nil
	}
	r.Abandoned = slices.DeleteFunc(slices.Clone(r.Abandoned), func(a Abandoned) bool { return a.Name == name })
	if len(r.Abandoned) == 0 {
		r.Abandoned = nil
	}
	return r, true
}

// abandonedUntil is the time until which the client of r's retired name's create may still wait
// for it, and false for a name no create of which was in flight when it was retired.
func (r Record) abandonedUntil(name string) (time.Time, bool) {
	for _, a := range r.Abandoned {
		if a.Name == name {
			return a.Until, true
		}
	}
	return time.Time{}, false
}

// The families a store's sentinel names (StoreState.Family): the session family's bucket,
// Dispatch's store, and a Legion daemon's store for one project (DaemonFamily).
const (
	SessionFamily  = "session"
	DispatchFamily = "dispatch"
	// daemonFamilyPrefix begins a daemon's family, which DaemonFamily completes with its project.
	daemonFamilyPrefix = "daemon/"
)

// DaemonFamily is the family of a Legion daemon's store for project: "daemon/<project>". A project
// that is empty or holds a '/', white space or a control character makes no family a store keeps
// (storeFamily): its keys could begin another project's, so it is refused as ErrInvalidFamily,
// naming the family.
func DaemonFamily(project string) (string, error) {
	family := daemonFamilyPrefix + project
	if err := CheckStoreFamily(family); err != nil {
		return "", err
	}
	return family, nil
}

// CheckStoreFamily refuses, as ErrInvalidFamily naming it, a family no store keeps (storeFamily):
// a store made for one could reach another family's rows. Every store's constructor calls it
// (pgstore.New among them).
func CheckStoreFamily(family string) error {
	if !storeFamily(family) {
		return fmt.Errorf("%w: %q is none of %s, %s or %s<project>, with a project that is not empty and holds no '/', white space or control character",
			ErrInvalidFamily, family, SessionFamily, DispatchFamily, daemonFamilyPrefix)
	}
	return nil
}

// StoreState is what a store's sentinel holds.
type StoreState struct {
	// Key is the sentinel's own key (SentinelKey in the selection bucket, PGSentinelKey(family) in
	// Postgres), which the store's WriteSentinel sets and its State requires, as a record's
	// (Record.Key).
	Key string `json:"key"`
	// Family is the store's family, required: SessionFamily for the bucket's sentinel,
	// DispatchFamily for Dispatch's store, DaemonFamily(project) for a daemon's. Key alone cannot
	// tell two stores apart, so each store reads only a sentinel that names its own family: the
	// session family's for KVStore, the one pgstore.New was given for the Postgres store. Each
	// store's WriteSentinel writes its own family itself, and refuses a state that names another
	// (EncodeSentinel).
	Family string `json:"family"`
	// Cutover is the family's cutover marker: CutoverDone, CutoverRolledBack, or "" before any.
	Cutover string `json:"cutover,omitempty"`
	// Boundary is the cutover boundary B: a session key with no record starts no later than it.
	// 0 where the family has none.
	Boundary uint64 `json:"boundary,omitempty"`
	// StreamID is the identity of ENVOY_NOTIFICATIONS as the operator recorded it when it wrote
	// the sentinel (the stream's StreamIdentityMetadataKey): the cutover's convert and enroll and a
	// hub restore's finish write it. A family that does not read the stream's info tags each last
	// floor with it instead. "" where none was recorded.
	StreamID string `json:"stream_id,omitempty"`
	// ReplayFrom, while set, is where a key with no record starts, never at its first-create rule
	// (a recovery after a lost store).
	ReplayFrom uint64 `json:"replay_from,omitempty"`
	// Orphans are the consumers no record selects that the recovery listed; replay mode ends once
	// none of them exists.
	Orphans []string `json:"orphans,omitempty"`
	// BackupSHA256 and Hold are the recovery's inputs.
	BackupSHA256 string `json:"backup_sha256,omitempty"`
	Hold         string `json:"hold,omitempty"`
}

// ownSentinel is state as the store of family writes it under key: naming family, which it sets
// when state names none, and carrying key. A state that names another family is refused as
// ErrSentinelFamily naming both, so no caller writes another family's sentinel, or one its store
// cannot read, by mistake.
func ownSentinel(state StoreState, key, family string) (StoreState, error) {
	switch state.Family {
	case "":
		state.Family = family
	case family:
	default:
		return StoreState{}, fmt.Errorf("%w: a sentinel of the family %q cannot be written as the %q store's", ErrSentinelFamily, state.Family, family)
	}
	state.Key = key
	return state, nil
}

// EncodeSentinel is state as the store of family writes it under key (ownSentinel), and its
// JSON: naming family, which it sets when state names none, and carrying key. A state that names
// another family is refused as ErrSentinelFamily naming both, and nothing is encoded. Each store's
// WriteSentinel writes what it answers.
func EncodeSentinel(state StoreState, key, family string) ([]byte, error) {
	state, err := ownSentinel(state, key, family)
	if err != nil {
		return nil, err
	}
	return json.Marshal(state)
}

// Store keeps the selection records of one family.
type Store interface {
	// State reads the store's sentinel. A store that is missing is ErrNoStore, a sentinel that is
	// absent ErrNoSentinel, and a sentinel key holding anything but the store's own exact sentinel
	// ErrMalformedSentinel.
	State(ctx context.Context) (StoreState, error)
	// Read reads logical's record and its revision. A key never written is ErrNoRecord; a key
	// whose last write is a delete is ErrRecordDeleted; a missing store is ErrNoStore.
	Read(ctx context.Context, logical string) (Record, uint64, error)
	// Write writes logical's record if its revision is still revision (0: never written), and
	// returns the new revision. A refused condition is ErrConflict.
	Write(ctx context.Context, logical string, record Record, revision uint64) (uint64, error)
}

// Server is the family's consumers on the server.
type Server interface {
	// Info reads consumer name; one the server does not hold is ErrNotFound.
	Info(ctx context.Context, name string) (*jetstream.ConsumerInfo, error)
	// Delete deletes consumer name; one the server does not hold is no error.
	Delete(ctx context.Context, name string) error
}

// Start is where a consumer is created.
type Start struct {
	// Seq is the stream sequence the consumer starts at, by start sequence. For a native create
	// it is the numeric boundary a recovery creates at instead, or 0 where the first-create rule
	// read no stream (Config.NativeFirstCreate), and the publish saves the start the consumer reports.
	Seq uint64
	// Native, when set, is the native policy the create keeps.
	Native Native
}

// CreateFunc creates consumer name at start, with the family's configuration and the metadata
// LogicalMetadataKey, and answers its info. A refusal for the stream's max_consumers wraps
// ErrConsumerLimit. An error that is a *jetstream.APIError or wraps ErrConsumerLimit is an answer
// from the server; any other error is a create whose outcome is unknown, which Names settles
// with an info.
type CreateFunc func(ctx context.Context, name string, start Start) (*jetstream.ConsumerInfo, error)

var (
	// ErrCutoverNotRun refuses every call in the fresh mode until the store's sentinel holds
	// CutoverDone (Admit).
	ErrCutoverNotRun = errors.New("consumer names: the cutover has not run")
	// ErrNoStore is a selection store that is missing: the KV bucket or the table.
	ErrNoStore = errors.New("consumer names: the selection store is missing")
	// ErrNoSentinel is a store whose sentinel is absent: its key holds nothing, so a lost record
	// cannot be told from a new key. A creator in the fixed mode may read it as no marker for a
	// Postgres store (consumernames/pgstore) alone, as Dispatch and the daemons do. In the session
	// bucket an absent sentinel, a delete or purge marker among them, is never no marker: the
	// listener reads it as a failed read and reads again (bus.Client.ReadMarker), and only a bucket
	// that does not exist is no marker. A sentinel key holding anything else is
	// ErrMalformedSentinel, in either store.
	ErrNoSentinel = errors.New("consumer names: the selection store's sentinel is missing")
	// ErrMalformedSentinel is a store whose sentinel key holds something other than the store's own
	// exact sentinel (DecodeSentinel): another JSON document, a field this build does not know (a
	// newer build's sentinel among them), a sentinel under another key, or one that names another
	// family (ErrSentinelFamily). It is never a missing sentinel: a creator in the fixed mode reads
	// it as a failed read, so it never runs fixed names over a store whose marker it cannot read,
	// and the fresh mode refuses every call over it. Only an operator's rewrite of the sentinel ends
	// it.
	ErrMalformedSentinel = errors.New("consumer names: the selection store's sentinel key holds no sentinel of its own")
	// ErrRecordDeleted is a key whose last write is a delete or a purge marker.
	ErrRecordDeleted = errors.New("consumer names: the selection record was deleted")
	// ErrMalformedRecord is a key whose value is not that key's selection record: another JSON
	// document, a field no record has, data after the record, a field that is not well formed, or a
	// record carrying another key. Any principal holding a JetStream API grant can have the server
	// store its answer under any key of the bucket through a request's reply subject, and one
	// holding a direct get can copy another key's message there whole, so a value is never read as
	// a record on its shape alone (DecodeRecord). It fails closed for that key alone: nothing is
	// created or bound from it, and only an operator's repair of the key ends it. Under the
	// sentinel's key such a value is ErrMalformedSentinel instead.
	ErrMalformedRecord = errors.New("consumer names: the key holds no selection record of its own")
	// ErrNoRecord is a key never written.
	ErrNoRecord = errors.New("consumer names: no selection record")
	// ErrConflict is a conditional write the store refused.
	ErrConflict = errors.New("consumer names: the selection record changed under the write")
	// ErrNoSafeStart refuses a missing current whose record proves no start.
	ErrNoSafeStart = errors.New("consumer names: no safe start is known")
	// ErrNotFound is a consumer the server does not hold.
	ErrNotFound = errors.New("consumer names: no such consumer")
	// ErrConsumerLimit is a create the server refused at the stream's max_consumers.
	ErrConsumerLimit = errors.New("consumer names: the stream holds as many consumers as its max_consumers allows")
	// ErrInvalidKey is a logical name that cannot be a key or a fixed consumer name.
	ErrInvalidKey = errors.New("consumer names: a logical name no consumer can be named after")
	// ErrKeyTooLong is a logical name whose fixed consumer name passes NATS's 255-byte limit.
	ErrKeyTooLong = errors.New("consumer name is too long, maximum allowed is 255")
	// ErrBusy is an operation another process holds in flight that this call could not settle
	// before its context ended.
	ErrBusy = errors.New("consumer names: another process's operation is in flight")
	// ErrSentinelFamily is a well-formed sentinel under a store's sentinel key that names another
	// family than the store's own (KVStore's SessionFamily, the family pgstore.New was given): a
	// malformed sentinel, ErrMalformedSentinel.
	ErrSentinelFamily = errors.New("consumer names: the sentinel names another store's family")
	// ErrInvalidFamily is a family no store keeps: none of SessionFamily, DispatchFamily and
	// DaemonFamily of a project that is not empty and holds no '/', white space or control
	// character. CheckStoreFamily, pgstore.New and DaemonFamily refuse it, so no store reads,
	// writes or lists the rows of a family it does not keep: a store of "daemon" alone would list
	// every project's records.
	ErrInvalidFamily = errors.New("consumer names: no store keeps that family")
	// ErrUnexpectedConsumer is an operation's candidate the server holds that is not as the family
	// creates it: its start differs from what the operation reserved, or a setting no correction in
	// place reaches from what the family's create gives (Names.adoptable, Config.Adopt). Such a
	// candidate is refused, naming it, and never published.
	ErrUnexpectedConsumer = errors.New("consumer names: the consumer is not as its family creates it")
)

// IsSelectionStateLost reports whether err refuses a call because selection state is lost: a
// missing store, sentinel or key, a sentinel key holding no sentinel of its own, a key whose last
// write is a delete, or a key whose value is not its record.
func IsSelectionStateLost(err error) bool {
	return errors.Is(err, ErrNoStore) || errors.Is(err, ErrNoSentinel) || errors.Is(err, ErrMalformedSentinel) ||
		errors.Is(err, ErrRecordDeleted) || errors.Is(err, ErrMalformedRecord)
}

// DecodeRecord reads data as the record of key: one JSON object of Record's fields and nothing
// after it (decodeStrict), whose own key is key and whose every field is well formed (check).
// Anything else is ErrMalformedRecord. Every store's read of a record decodes it here.
func DecodeRecord(key string, data []byte) (Record, error) {
	var record Record
	if err := decodeStrict(data, &record); err != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrMalformedRecord, err)
	}
	if err := record.check(key); err != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrMalformedRecord, err)
	}
	return record, nil
}

// EncodeRecord is record's JSON, as a store writes it under its own key (Record.Key), which the
// caller sets. A record every read of that key would refuse (check) is refused as
// ErrMalformedRecord naming the key, and nothing is encoded: a store writes no record its readers
// cannot read, so a writer outside Names that builds one, an operator's cleanup that drops a
// retired name from Retired alone among them, fails at its own write and leaves the key as it was
// for every reader. Every store's write of a record encodes it here.
func EncodeRecord(record Record) ([]byte, error) {
	if err := record.check(record.Key); err != nil {
		return nil, fmt.Errorf("%w: the record of %s is not written: %w", ErrMalformedRecord, record.Key, err)
	}
	return json.Marshal(record)
}

// DecodeSentinel reads data as the sentinel under key of a store of family, as DecodeRecord reads
// a record: anything else, a sentinel of another family among them (ErrSentinelFamily), is
// ErrMalformedSentinel, which no store answers as a missing sentinel. Every store's read of its
// sentinel decodes it here.
func DecodeSentinel(key, family string, data []byte) (StoreState, error) {
	var state StoreState
	if err := decodeStrict(data, &state); err != nil {
		return StoreState{}, fmt.Errorf("%w: %w", ErrMalformedSentinel, err)
	}
	if err := state.check(key, family); err != nil {
		return StoreState{}, fmt.Errorf("%w: %w", ErrMalformedSentinel, err)
	}
	return state, nil
}

// DecodeKVValue reads data, the value of key in the session bucket (SessionBucket), as KVStore
// reads it: under SentinelKey the session family's sentinel (DecodeSentinel, isSentinel true),
// under any other key that key's record (DecodeRecord). A value that is not that key's own record
// or the bucket's own sentinel is ErrMalformedRecord or ErrMalformedSentinel, naming the key: the
// answer KVStore's Read and State give, which call it, so a reader of the bucket's raw messages
// decodes them as the store does. It reads no delete or purge marker: a reader that sees the
// message's KV-Operation header answers ErrRecordDeleted, or ErrNoSentinel under SentinelKey,
// itself, as KVStore does, before it decodes anything.
func DecodeKVValue(key string, data []byte) (record Record, state StoreState, isSentinel bool, err error) {
	if key == SentinelKey {
		if state, err = DecodeSentinel(key, SessionFamily, data); err != nil {
			return Record{}, StoreState{}, true, fmt.Errorf("consumer names: the key %s: %w", key, err)
		}
		return Record{}, state, true, nil
	}
	if record, err = DecodeRecord(key, data); err != nil {
		return Record{}, StoreState{}, false, fmt.Errorf("consumer names: the key %s: %w", key, err)
	}
	return record, StoreState{}, false, nil
}

// decodeStrict decodes data into v, which is a struct: data must be one JSON object, with no field
// v does not have, and nothing but space after it.
func decodeStrict(data []byte, v any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("the value is no JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("the value holds more after its JSON object")
	}
	return nil
}

// consumerName reports whether name can name a consumer: nats-server refuses an empty name, one
// longer than 255 bytes, and one holding '.', '*', '>', a path separator or whitespace.
func consumerName(name string) bool {
	return name != "" && len(name) <= 255 && !refusedNameChars(name)
}

// refusedNameChars reports whether s holds a character no consumer name or key token holds: '.',
// '*', '>', a path separator, white space or a control character. consumerName and CheckKey keep
// their own length limits beside it.
func refusedNameChars(s string) bool {
	return strings.ContainsAny(s, `.*>/\`) || strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

// check refuses a record that is not key's or sets a field that is not well formed: a name no
// consumer can carry, an operation of no kind this package writes, one reserved at no time, or one
// missing the name its kind acts on.
func (r Record) check(key string) error {
	if r.Key != key {
		return fmt.Errorf("its key is %q, not %q", r.Key, key)
	}
	if r.Current != "" && !consumerName(r.Current) {
		return fmt.Errorf("its current %q names no consumer", r.Current)
	}
	if r.Op != nil {
		if err := r.Op.check(); err != nil {
			return err
		}
	}
	if r.LastFloor != nil && !consumerName(r.LastFloor.Name) {
		return fmt.Errorf("its last floor's name %q names no consumer", r.LastFloor.Name)
	}
	for _, name := range r.Retired {
		if !consumerName(name) {
			return fmt.Errorf("its retired name %q names no consumer", name)
		}
	}
	for _, a := range r.Abandoned {
		if !slices.Contains(r.Retired, a.Name) || a.Until.IsZero() {
			return fmt.Errorf("its abandoned candidate %+v is no retired name with a time", a)
		}
	}
	if r.Restore != nil && (!consumerName(r.Restore.Target) || r.Restore.Phase == "") {
		return fmt.Errorf("its restore %+v names no target or no phase", *r.Restore)
	}
	return nil
}

func (op Op) check() error {
	switch {
	case op.ReservedAt.IsZero():
		return fmt.Errorf("its %s operation was reserved at no time", op.Kind)
	case op.Native != "" && op.Native != NativeNew && op.Native != NativeAll:
		return fmt.Errorf("its operation keeps the native policy %q, which is none", op.Native)
	case op.CreateWithin < 0:
		return fmt.Errorf("its operation's create bound %s is negative", op.CreateWithin)
	}
	switch op.Kind {
	case OpCreate:
		if !consumerName(op.Candidate) {
			return fmt.Errorf("its create's candidate %q names no consumer", op.Candidate)
		}
	case OpReplace:
		if !consumerName(op.Candidate) || !consumerName(op.Old) {
			return fmt.Errorf("its replace of %q by %q names no consumer", op.Old, op.Candidate)
		}
	case OpDelete:
		if !consumerName(op.Old) {
			return fmt.Errorf("its delete of %q names no consumer", op.Old)
		}
	default:
		return fmt.Errorf("its operation is of the kind %q, which is none", op.Kind)
	}
	return nil
}

// check refuses a sentinel that is not key's, names no family or one of no form this package names,
// names another family than family, the store's (ErrSentinelFamily), or sets a field that is not
// well formed: a cutover marker this package does not know, or an orphan no consumer can be named.
func (s StoreState) check(key, family string) error {
	if s.Key != key {
		return fmt.Errorf("its key is %q, not %q", s.Key, key)
	}
	if !storeFamily(s.Family) {
		return fmt.Errorf("its family %q is none of %s, %s or %s<project>", s.Family, SessionFamily, DispatchFamily, daemonFamilyPrefix)
	}
	if s.Family != family {
		return fmt.Errorf("%w: it names the family %q, not %q", ErrSentinelFamily, s.Family, family)
	}
	if s.Cutover != "" && s.Cutover != CutoverDone && s.Cutover != CutoverRolledBack {
		return fmt.Errorf("its cutover marker %q is none", s.Cutover)
	}
	for _, name := range s.Orphans {
		if !consumerName(name) {
			return fmt.Errorf("its orphan %q names no consumer", name)
		}
	}
	return nil
}

// storeFamily reports whether family is one a store keeps and a sentinel may name: SessionFamily,
// DispatchFamily, or DaemonFamily of a project name with no space, control character or '/'.
func storeFamily(family string) bool {
	if family == SessionFamily || family == DispatchFamily {
		return true
	}
	project, ok := strings.CutPrefix(family, daemonFamilyPrefix)
	return ok && project != "" && !strings.ContainsFunc(project, func(r rune) bool { return r == '/' || unicode.IsSpace(r) || unicode.IsControl(r) })
}

// Admit decides, from the store's sentinel, what a consumer call does in the fresh mode: it is
// served when the cutover is done, and refused with ErrCutoverNotRun before the cutover and after
// a rollback. It is the one place that decision is made.
func Admit(state StoreState) error {
	if state.Cutover != CutoverDone {
		return ErrCutoverNotRun
	}
	return nil
}

// maxFixedNameKey is the longest logical name whose fixed name, s-<key>, fits NATS's 255-byte
// consumer name limit (nats-server JSMaxNameLen).
const maxFixedNameKey = 253

// durableKeyPrefix begins every key DurableKey makes. A '.' is in no session key (CheckKey), so a
// durable's record never meets a session's, whatever the session's id.
const durableKeyPrefix = "durable."

// DurableKey is the selection key, in a bucket the session records share, of the durable consumer
// named durable: "durable." and the name with each byte outside [-_a-zA-Z0-9] written as '=' and
// two uppercase hex digits, so the key is in the KV key alphabet ([-/_=.a-zA-Z0-9]) and two names
// never share one. The listener's own durable is kept so, under its name bus.ListenerDurable.
func DurableKey(durable string) string {
	var key strings.Builder
	key.WriteString(durableKeyPrefix)
	for i := range len(durable) {
		switch c := durable[i]; {
		case c == '-' || c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9'):
			key.WriteByte(c)
		default:
			fmt.Fprintf(&key, "=%02X", c)
		}
	}
	return key.String()
}

// CheckDurableKey refuses a key DurableKey did not make (ErrInvalidKey): one whose escapes
// durableOfKey cannot read back, or that DurableKey would not write for the durable it reads back
// (a lowercase escape, an escape of a byte DurableKey writes as itself), so every key it passes
// names one durable, the prefix its naming reads (DurableNaming). A family keyed by DurableKey
// passes it as Config.CheckKey.
func CheckDurableKey(key string) error {
	if durable, ok := durableOfKey(key); !ok || durable == "" || DurableKey(durable) != key {
		return fmt.Errorf("%w: %q is no durable's key", ErrInvalidKey, key)
	}
	return nil
}

// CheckKey refuses a logical name before anything is written, as the fixed names refused it at
// their create: an empty name, one holding a '.', '*', '>', '/', '\' or any white space or control
// character, which no consumer name or key token holds (ErrInvalidKey), and one of 254 bytes or
// more, whose fixed name the server refused with its 10102 answer (ErrKeyTooLong). So no key a
// family writes holds a '.', and every key that does is a system key (SentinelKey).
func CheckKey(logical string) error {
	if logical == "" || refusedNameChars(logical) {
		return fmt.Errorf("%w: %q", ErrInvalidKey, logical)
	}
	if len(logical) > maxFixedNameKey {
		return fmt.Errorf("create consumer s-%s: %w (10102)", logical, ErrKeyTooLong)
	}
	return nil
}

// FreshName is a physical name never made before: prefix, '~' and a random suffix (freshSuffix).
// A '~' is no character of a fixed name's family prefix, so no fresh name is a fixed one.
func FreshName(prefix string) string {
	return prefix + "~" + freshSuffix()
}

// freshAlphabet is the characters of a fresh name's suffix, the ones freshOf takes: [0-9A-Za-z].
const freshAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// freshSuffixLength is how many characters a fresh name's suffix holds: 22 of freshAlphabet, about
// 131 bits, the length a NUID had, so no name grows.
const freshSuffixLength = 22

// freshSuffix is freshSuffixLength characters of freshAlphabet, each drawn uniformly from
// crypto/rand: a byte of 248 or more (4 × 62) is drawn again, so every character is equally likely.
// No suffix says anything of another, so a principal that sees one process's names or reply
// inboxes cannot predict its next candidate and create that name first.
func freshSuffix() string {
	var suffix [freshSuffixLength]byte
	var random [32]byte
	for n := 0; n < len(suffix); {
		// crypto/rand's Read never returns an error, and always fills the buffer.
		_, _ = rand.Read(random[:])
		for _, b := range random {
			if b >= 4*byte(len(freshAlphabet)) {
				continue
			}
			suffix[n] = freshAlphabet[int(b)%len(freshAlphabet)]
			if n++; n == len(suffix) {
				break
			}
		}
	}
	return string(suffix[:])
}

// LogicalOf is the logical name of a physical one, for a family whose Config.Prefix is the logical
// name itself, such as a durable's (the listener's durable, Dispatch's and a daemon's): the name cut
// at its last '~', or the name itself when it holds none (a fixed name). It does not hold for the
// session family, whose prefix is the constant "session": LogicalOf("session~<random>") is
// "session", and a session consumer's logical name is its LogicalMetadataKey metadata.
func LogicalOf(physical string) string {
	if i := strings.LastIndexByte(physical, '~'); i >= 0 {
		return physical[:i]
	}
	return physical
}
