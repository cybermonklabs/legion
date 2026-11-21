package consumernames

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"
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
	// Restore is a rollback in progress, which the rollback command writes; Names never writes it.
	Restore *Restore `json:"restore,omitempty"`
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

// StoreState is what a store's sentinel holds.
type StoreState struct {
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

// Store keeps the selection records of one family.
type Store interface {
	// State reads the store's sentinel. A store that is missing is ErrNoStore, a sentinel that is
	// missing ErrNoSentinel.
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
	// ErrNoSentinel is a store whose sentinel is missing, so a lost record cannot be told from a
	// new key.
	ErrNoSentinel = errors.New("consumer names: the selection store's sentinel is missing")
	// ErrRecordDeleted is a key whose last write is a delete or a purge marker.
	ErrRecordDeleted = errors.New("consumer names: the selection record was deleted")
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
)

// IsSelectionStateLost reports whether err refuses a call because selection state is lost: a
// missing store, sentinel or key, or a key whose last write is a delete.
func IsSelectionStateLost(err error) bool {
	return errors.Is(err, ErrNoStore) || errors.Is(err, ErrNoSentinel) || errors.Is(err, ErrRecordDeleted)
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

// CheckDurableKey refuses a key DurableKey did not make (ErrInvalidKey): a family keyed by
// DurableKey passes it as Config.CheckKey.
func CheckDurableKey(key string) error {
	name, ok := strings.CutPrefix(key, durableKeyPrefix)
	if !ok || name == "" || strings.ContainsFunc(name, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '=' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9'))
	}) {
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
	if logical == "" || strings.ContainsAny(logical, `.*>/\`) || strings.ContainsFunc(logical, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("%w: %q", ErrInvalidKey, logical)
	}
	if len(logical) > maxFixedNameKey {
		return fmt.Errorf("create consumer s-%s: %w (10102)", logical, ErrKeyTooLong)
	}
	return nil
}

// FreshName is a physical name never made before: prefix, '~' and a NUID. A '~' is no character
// of a fixed name's family prefix, so no fresh name is a fixed one.
func FreshName(prefix string) string {
	return prefix + "~" + nuid.Next()
}

// LogicalOf is the logical name of a physical one: the name cut at its last '~', or the name
// itself when it holds none (a fixed name).
func LogicalOf(physical string) string {
	if i := strings.LastIndexByte(physical, '~'); i >= 0 {
		return physical[:i]
	}
	return physical
}
