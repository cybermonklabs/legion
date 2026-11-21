package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
)

// The daemon's durables on fresh names (ConsumerSpec.Names, consumernames.ModeFresh). Each of the
// three durables keeps its fixed name, legion-go-<project>-<kind>, as its logical name, and every
// creation of it is a physical name never made before, <logical>~<nuid>, which the logical name's
// selection record in the daemon's own Postgres selects (intake_consumers, consumernames.PGStore).
// A consumer state write that nats-server finishes after the consumer's delete then lands in a
// directory no consumer reads, never in the state of the next consumer of that name: on fixed names
// such a write can fail the next create (10012) or hand it the deleted consumer's state, and a
// restart then skips messages.
//
// The protocol is consumernames.Names: every call is refused until the operator's convert has
// written the cutover marker "done" into the database's sentinel, and a missing table, sentinel or
// record is a refusal, never a fall back to the fixed names. The daemon adds three rules of its
// own. A logical name with a checkpoint (consumer_positions) and no record stops, since a lost
// record is not a new project (missingRecord). A new project starts at the next message, DeliverNew
// as today, with the stream's end saved as the start a recovery creates at (first). And the daemon
// deletes nothing: its NATS grant has no consumer delete, so a name it retires after a create
// nobody answered stays on the server until the operator's `envoy-infra consumer-names cleanup`
// deletes it (durableServer.Delete).

// nameNotSettledLine is the message of the ERROR line a failed name step writes (durableNames.failed).
const nameNotSettledLine = "intake: a durable consumer's physical name could not be settled"

// operationUpdate is the daemon's own step beside consumernames' operations: the update of a
// selected consumer's configuration at each boot and each repositories change.
const operationUpdate consumernames.Operation = "update"

// errCheckpointWithoutRecord is a logical name whose checkpoint the daemon keeps and whose
// selection record is missing (missingRecord).
var errCheckpointWithoutRecord = errors.New("intake: a durable consumer has a checkpoint and no selection record")

// errStoreConverted is the refusal of the fixed names on a database the operator's convert has
// marked converted (refuseConvertedStore).
var errStoreConverted = errors.New("intake_consumers says this database's durable consumers were converted to fresh names (cutover done), and legion.yaml's consumer_names is fixed: set consumer_names: fresh, since a converted store must not run the fixed names")

// refuseConvertedStore refuses the fixed names on a database whose sentinel the operator's convert
// has marked converted (cutover "done"), at the boot and after each reconnect of the daemon's
// connection (Consumers.rereadMarker): its durables are fresh names now, and creating the fixed
// ones again would run the race the cutover removed. What says there is no marker is
// today's code, which opens the fixed names: no table, no sentinel, a read the database refuses
// (insufficient_privilege: a login that may not read the table is a deployment that never ran the
// cutover on it), a sentinel before any cutover, and one a rollback wrote ("rolled_back", which
// restored the fixed names). A read that says nothing either way, no connection or a timeout, is
// made again, from a quarter second doubling to five, until it answers or ctx ends, and then the
// caller ends on its error: the fixed names never run on a marker the daemon did not read.
func refuseConvertedStore(ctx context.Context, pool Pool) error {
	store := consumernames.NewPGStore(pool)
	wait := markerReadWait
	for {
		state, err := store.State(ctx)
		switch {
		case err == nil && state.Cutover == consumernames.CutoverDone:
			return errStoreConverted
		case err == nil, errors.Is(err, consumernames.ErrNoStore), errors.Is(err, consumernames.ErrNoSentinel), readRefused(err):
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("read the cutover marker of intake_consumers, which the fixed names never start without: %w", err)
		case <-time.After(wait):
		}
		wait = min(2*wait, markerReadWaitMost)
	}
}

// The waits between the boot's reads of the cutover marker (refuseConvertedStore).
const (
	markerReadWait     = 250 * time.Millisecond
	markerReadWaitMost = 5 * time.Second
)

// readRefused reports whether err is a read the database refused for want of a privilege.
func readRefused(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "42501"
}

// durableNames is the daemon's side of the protocol for its three durables on fresh names.
//
// Every call on a physical name, <logical>~<generation>, goes on a NATS connection of its own,
// named <logical>/<generation> (GenerationConnection), which the auth service may grant exactly
// that consumer's create, info, pull and acknowledgements: a grant held to the daemon's projects
// names only the fixed names, and a fresh name is no fixed one. A create opens its candidate's
// connection once the protocol has reserved it (consumernames.Config.Reserved), before the create
// is sent; an info of a name opens that name's; and the consumer each durable delivers from is
// bound on its current's connection, which Run consumes on. The connection of a name the record no
// longer selects is closed once the open that settled the record ends. Nothing here reads the
// stream: a grant held to the daemon's projects holds no stream info, so a first create keeps its
// native policy and saves the created consumer's own position (consumernames'
// NativeFirstCreate), and a last floor is tagged with the stream identity the operator recorded
// in the sentinel (streamID).
type durableNames struct {
	names   *consumernames.Names
	store   *consumernames.PGStore
	pool    Pool
	logger  *slog.Logger
	connect func(ctx context.Context, name string) (*nats.Conn, error)

	mu sync.Mutex
	// generations is JetStream on each open connection, by the physical name the connection is
	// named for; Conn is that connection.
	generations map[string]jetstream.JetStream
}

// GenerationConnection is the name of the NATS connection the daemon makes every call on physical,
// <logical>~<generation>, on: <logical>/<generation>. The auth service reads the generation, and
// the logical name's project and kind, back from it.
func GenerationConnection(physical string) string {
	logical := consumernames.LogicalOf(physical)
	return logical + "/" + strings.TrimPrefix(physical, logical+"~")
}

func newDurableNames(pool Pool, spec ConsumerSpec) *durableNames {
	store := consumernames.NewPGStore(pool)
	d := &durableNames{store: store, pool: pool, logger: spec.Logger, connect: spec.Connect, generations: map[string]jetstream.JetStream{}}
	d.names = consumernames.New(consumernames.Config{
		Store:             store,
		Server:            durableServer{d},
		Prefix:            func(logical string) string { return logical },
		Floor:             func(info *jetstream.ConsumerInfo) uint64 { return info.AckFloor.Stream },
		StreamID:          d.streamID,
		MissingRecord:     d.missingRecord,
		NativeFirstCreate: true,
		Owner:             fmt.Sprintf("legion daemon %s, pid %d", spec.Project, os.Getpid()),
		Now:               spec.Now,
		Reserved: func(ctx context.Context, _ string, op consumernames.Op) error {
			_, err := d.on(ctx, op.Candidate)
			return err
		},
	})
	return d
}

// on is JetStream on physical's connection, opened now if it is not open. The dial runs outside
// the lock, so a reader of another name never waits on it; of two dials of one name at once, the
// later closes its own and takes the first's.
func (d *durableNames) on(ctx context.Context, physical string) (jetstream.JetStream, error) {
	d.mu.Lock()
	open, ok := d.generations[physical]
	d.mu.Unlock()
	if ok {
		return open, nil
	}
	if consumernames.LogicalOf(physical) == physical {
		return nil, fmt.Errorf("consumer %s is no fresh name, which no generation's connection is granted", physical)
	}
	name := GenerationConnection(physical)
	conn, err := d.connect(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", name, err)
	}
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("open JetStream on %s: %w", name, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if open, ok := d.generations[physical]; ok {
		conn.Close()
		return open, nil
	}
	d.generations[physical] = js
	return js, nil
}

// keep closes the connection of every name of logical but current's.
func (d *durableNames) keep(logical, current string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for physical, open := range d.generations {
		if physical != current && consumernames.LogicalOf(physical) == logical {
			open.Conn().Close()
			delete(d.generations, physical)
		}
	}
}

// close closes every generation's connection.
func (d *durableNames) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for physical, open := range d.generations {
		open.Conn().Close()
		delete(d.generations, physical)
	}
}

// open settles config's durable, whose Durable is its logical name, and returns the consumer the
// record selects, with this boot's configuration (config's filters and acknowledgement window),
// bound on that consumer's own connection: it creates one when the record selects none, or the one
// selected is gone (consumernames.Names.Ensure), and updates the one selected otherwise, keeping
// the start it was created with, as openConsumer does for a fixed name. It then writes the
// consumer's floor as the record's last floor, and closes the connections of the logical name's
// other generations. A failed Ensure closes none: what the record selects is not known then, and
// the consumer Run reads from stays bound to its own connection, so a failed repositories change
// stops nothing and its caller retries it.
func (d *durableNames) open(ctx context.Context, config jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	logical := config.Durable
	sel, _, err := d.names.Ensure(ctx, logical, consumernames.CreateRequest{Create: d.create(config), First: d.first})
	if err != nil {
		return nil, d.failed(logical, sel, consumernames.OperationCreate, err)
	}
	defer d.keep(logical, sel.Record.Current)
	current := sel.Record.Current
	info := sel.Info
	if info == nil {
		if info, err = (durableServer{d}).Info(ctx, current); err != nil {
			return nil, d.failed(logical, sel, consumernames.OperationSelect, err)
		}
	}
	js, err := d.on(ctx, current)
	if err != nil {
		return nil, d.failed(logical, sel, operationUpdate, err)
	}
	selected := physicalConfig(config, current)
	selected.DeliverPolicy, selected.OptStartSeq, selected.OptStartTime = info.Config.DeliverPolicy, info.Config.OptStartSeq, info.Config.OptStartTime
	consumer, err := js.UpdateConsumer(ctx, notificationStream, selected)
	if err != nil {
		return nil, d.failed(logical, sel, operationUpdate, fmt.Errorf("update %s: %w", current, err))
	}
	if err := d.recordLastFloor(ctx, logical, current, consumer.CachedInfo()); err != nil {
		// The last floor only narrows what a missing current replays; without it the record's
		// start, which is lower, governs. The line is the operator's, and the boot goes on.
		d.failed(logical, sel, consumernames.OperationLastFloor, err)
	}
	return consumer, nil
}

// recordLastFloor writes info's ack floor as logical's last floor, read from current on this
// stream.
func (d *durableNames) recordLastFloor(ctx context.Context, logical, current string, info *jetstream.ConsumerInfo) error {
	id, err := d.streamID(ctx)
	if err != nil {
		return err
	}
	return d.names.RecordLastFloor(ctx, logical, current, info.AckFloor.Stream, id)
}

// info is the info of the consumer logical's record selects, read fresh on its own connection, for
// a reader: the boot's reconcile (Consumers.DispatchPosition) and the state route (Consumers.Lag).
// It creates nothing, and closes no connection: a repositories change may be settling the same
// logical name beside it (open), whose candidate's connection it must not close.
func (d *durableNames) info(ctx context.Context, logical string) (*jetstream.ConsumerInfo, error) {
	sel, err := d.names.Select(ctx, logical, nil)
	switch {
	case err != nil:
		return nil, d.failed(logical, sel, consumernames.OperationSelect, err)
	case sel.Record.Current == "":
		return nil, d.failed(logical, sel, consumernames.OperationSelect, fmt.Errorf("the selection record of %s selects no consumer", logical))
	case sel.Missing:
		return nil, d.failed(logical, sel, consumernames.OperationSelect, fmt.Errorf("the selection record of %s selects %s, which the stream does not hold", logical, sel.Record.Current))
	}
	return sel.Info, nil
}

// failed writes the ERROR line of a failed name step on logical and returns err. The step and the
// physical name are the protocol's own when err names them (consumernames.StepError); a refusal
// before anything was read is the admission's; otherwise they are call's and the name sel's record
// names.
func (d *durableNames) failed(logical string, sel consumernames.Selection, call consumernames.Operation, err error) error {
	operation, physical := call, sel.Record.Current
	if physical == "" && sel.Record.Op != nil {
		physical = sel.Record.Op.Candidate
	}
	var step *consumernames.StepError
	switch {
	case errors.As(err, &step):
		operation, physical = step.Operation, step.Physical
	case errors.Is(err, consumernames.ErrNoSafeStart):
		operation = consumernames.OperationMissingCurrent
	case errors.Is(err, consumernames.ErrCutoverNotRun), consumernames.IsSelectionStateLost(err), errors.Is(err, errCheckpointWithoutRecord):
		operation = consumernames.OperationSelect
	}
	d.logger.Error(nameNotSettledLine, "consumer", logical, "physical", physical, "op", string(operation), "error", err.Error())
	return err
}

// first is the daemon's first-create rule: a durable whose logical name never existed starts at
// the next message, DeliverNew, as it always has, whatever boundary the sentinel holds, so a new
// kind of durable is never handed the events since the cutover. It reads nothing:
// NativeFirstCreate saves the created consumer's own delivered sequence + 1 as the record's start
// for a recovery, and a create that never reached the server is made natively again, as today's
// daemon makes it a moment later.
func (d *durableNames) first(context.Context) (consumernames.Start, error) {
	return consumernames.Start{Native: consumernames.NativeNew}, nil
}

// errNoStreamID is a sentinel that records no stream identity: a store the operator's convert or
// enroll did not write, on which a last floor could not be tagged (streamID).
var errNoStreamID = errors.New("intake_consumers' sentinel records no stream identity (stream_id): the operator's convert or enroll writes it, with the identity of ENVOY_NOTIFICATIONS")

// requireStreamID refuses a boot on fresh names whose sentinel says the cutover is done and records
// no stream identity (errNoStreamID), with the ERROR line, before any consumer call. A sentinel the
// protocol itself refuses (no store, no sentinel, no cutover) is left to its admission, which names
// its own reason.
func (d *durableNames) requireStreamID(ctx context.Context, logical string) error {
	state, err := d.store.State(ctx)
	if err != nil || state.Cutover != consumernames.CutoverDone || state.StreamID != "" {
		return nil
	}
	return d.failed(logical, consumernames.Selection{Logical: logical}, consumernames.OperationSelect, errNoStreamID)
}

// missingRecord refuses logical when the daemon keeps a checkpoint for it (consumer_positions,
// keyed by the logical name) and no selection record: the record was lost, and taking the name
// for a new project would start it at the next message and skip what it had not applied. A name
// with no checkpoint is a new project's.
func (d *durableNames) missingRecord(ctx context.Context, logical string) error {
	var seq int64
	err := d.pool.QueryRow(ctx, `select stream_seq from consumer_positions where consumer = $1`, logical).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the checkpoint of durable consumer %s: %w", logical, err)
	}
	return fmt.Errorf("%w: %s applied through stream sequence %d, and intake_consumers holds no row for it; the operator's `envoy-infra consumer-names replay` gives it a start",
		errCheckpointWithoutRecord, logical, seq)
}

// streamID is the identity of ENVOY_NOTIFICATIONS as the operator recorded it in the sentinel
// (convert, enroll, and every operator command that makes the stream again), which each last floor
// carries and must match to count: a held daemon may not read the stream's own. A sentinel that
// records none refuses (errNoStreamID).
func (d *durableNames) streamID(ctx context.Context) (string, error) {
	state, err := d.store.State(ctx)
	if err != nil {
		return "", fmt.Errorf("read the stream identity intake_consumers' sentinel records: %w", err)
	}
	if state.StreamID == "" {
		return "", errNoStreamID
	}
	return state.StreamID, nil
}

// create is the consumernames.CreateFunc of config's durable: a create of the physical name, on
// that name's own connection, with config's filters and window, the logical name in its metadata,
// at start: its native policy for a new project's first create, else by start sequence. A refusal
// at the stream's max_consumers is consumernames.ErrConsumerLimit.
func (d *durableNames) create(config jetstream.ConsumerConfig) consumernames.CreateFunc {
	return func(ctx context.Context, name string, start consumernames.Start) (*jetstream.ConsumerInfo, error) {
		js, err := d.on(ctx, name)
		if err != nil {
			return nil, err
		}
		physical := physicalConfig(config, name)
		switch start.Native {
		case consumernames.NativeNew:
			physical.DeliverPolicy = jetstream.DeliverNewPolicy
		case consumernames.NativeAll:
			physical.DeliverPolicy = jetstream.DeliverAllPolicy
		default:
			physical.DeliverPolicy, physical.OptStartSeq = jetstream.DeliverByStartSequencePolicy, start.Seq
		}
		consumer, err := js.CreateConsumer(ctx, notificationStream, physical)
		if err != nil {
			var api *jetstream.APIError
			if errors.As(err, &api) && api.ErrorCode == jetstream.JSErrCodeMaximumConsumersLimit {
				return nil, fmt.Errorf("%w: %w", consumernames.ErrConsumerLimit, err)
			}
			return nil, err
		}
		return consumer.CachedInfo(), nil
	}
}

// physicalConfig is config, whose Durable is a logical name, for the physical consumer name: its
// Durable the physical name and its metadata naming the logical one.
func physicalConfig(config jetstream.ConsumerConfig, name string) jetstream.ConsumerConfig {
	logical := config.Durable
	config.Durable = name
	config.Metadata = maps.Clone(config.Metadata)
	if config.Metadata == nil {
		config.Metadata = map[string]string{}
	}
	config.Metadata[consumernames.LogicalMetadataKey] = logical
	return config
}

// durableServer is the daemon's consumers as its own NATS grant reaches them, each on its own
// connection (durableNames.on): info, and no delete.
type durableServer struct{ d *durableNames }

// Info reads consumer name on name's own connection, through a handle of its own, as the readers
// of a fixed name do.
func (s durableServer) Info(ctx context.Context, name string) (*jetstream.ConsumerInfo, error) {
	js, err := s.d.on(ctx, name)
	if err != nil {
		return nil, err
	}
	consumer, err := js.Consumer(ctx, notificationStream, name)
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		return nil, fmt.Errorf("%w: %s", consumernames.ErrNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	return consumer.CachedInfo(), nil
}

// Delete refuses: the daemon's grant has no consumer delete (natsgrant's daemon entries), so the
// request would draw no answer. A retired daemon name is the operator's cleanup's to delete.
func (s durableServer) Delete(_ context.Context, name string) error {
	return fmt.Errorf("delete consumer %s: the daemon's NATS grant has no consumer delete; `envoy-infra consumer-names cleanup` deletes a retired daemon name with the operator's credential", name)
}
