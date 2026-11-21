package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"

	"github.com/jackc/pgx/v5"
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
//
// Each failed name step is one ERROR line, nameNotSettledLine, naming the logical name, the
// physical name the step was on ("" when none was read), the step and the error. There is no
// metric for it.

// nameNotSettledLine is the message of the ERROR line a failed name step writes (durableNames.failed).
const nameNotSettledLine = "intake: a durable consumer's physical name could not be settled"

// operationUpdate is the daemon's own step beside consumernames' operations: the update of a
// selected consumer's configuration at each boot and each repositories change.
const operationUpdate consumernames.Operation = "update"

// errCheckpointWithoutRecord is a logical name whose checkpoint the daemon keeps and whose
// selection record is missing (missingRecord).
var errCheckpointWithoutRecord = errors.New("intake: a durable consumer has a checkpoint and no selection record")

// durableNames is the daemon's side of the protocol for its three durables on fresh names.
type durableNames struct {
	names  *consumernames.Names
	stream jetstream.Stream
	pool   Pool
	logger *slog.Logger
}

func newDurableNames(stream jetstream.Stream, pool Pool, spec ConsumerSpec) *durableNames {
	d := &durableNames{stream: stream, pool: pool, logger: spec.Logger}
	d.names = consumernames.New(consumernames.Config{
		Store:         consumernames.NewPGStore(pool),
		Server:        durableServer{stream: stream},
		Prefix:        func(logical string) string { return logical },
		Floor:         func(info *jetstream.ConsumerInfo) uint64 { return info.AckFloor.Stream },
		StreamID:      d.streamID,
		MissingRecord: d.missingRecord,
		Owner:         fmt.Sprintf("legion daemon %s, pid %d", spec.Project, os.Getpid()),
		Now:           spec.Now,
	})
	return d
}

// open settles config's durable, whose Durable is its logical name, and returns the consumer the
// record selects, with this boot's configuration (config's filters and acknowledgement window): it
// creates one when the record selects none, or the one selected is gone (consumernames.Names.Ensure),
// and updates the one selected otherwise, keeping the start it was created with, as openConsumer
// does for a fixed name. It then writes the consumer's floor as the record's last floor.
func (d *durableNames) open(ctx context.Context, config jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	logical := config.Durable
	sel, _, err := d.names.Ensure(ctx, logical, consumernames.CreateRequest{Create: d.create(config), First: d.first})
	if err != nil {
		return nil, d.failed(logical, sel, consumernames.OperationCreate, err)
	}
	current := sel.Record.Current
	info := sel.Info
	if info == nil {
		if info, err = (durableServer{stream: d.stream}).Info(ctx, current); err != nil {
			return nil, d.failed(logical, sel, consumernames.OperationSelect, err)
		}
	}
	selected := physicalConfig(config, current)
	selected.DeliverPolicy, selected.OptStartSeq, selected.OptStartTime = info.Config.DeliverPolicy, info.Config.OptStartSeq, info.Config.OptStartTime
	consumer, err := d.stream.UpdateConsumer(ctx, selected)
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

// info is the info of the consumer logical's record selects, read fresh, for a reader: the boot's
// reconcile (Consumers.DispatchPosition) and the state route (Consumers.Lag). It creates nothing.
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

// first is the daemon's first-create rule: a new project's durable starts at the next message,
// DeliverNew, as it always has, and the record saves the stream's end at the reservation as the
// start a recovery creates at by start sequence, so a message published between the reservation
// and the create is never skipped by a consumer created again in its place.
func (d *durableNames) first(ctx context.Context) (consumernames.Start, error) {
	info, err := d.stream.Info(ctx)
	if err != nil {
		return consumernames.Start{}, fmt.Errorf("read %s info: %w", notificationStream, err)
	}
	return consumernames.Start{Seq: info.State.LastSeq + 1, Native: consumernames.NativeNew}, nil
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

// streamID is the notification stream's identity, which a last floor carries and must match to
// count (consumernames.StreamIdentity): the listener writes it when it creates the stream.
func (d *durableNames) streamID(ctx context.Context) (string, error) {
	info, err := d.stream.Info(ctx)
	if err != nil {
		return "", fmt.Errorf("read %s info: %w", notificationStream, err)
	}
	return consumernames.StreamIdentity(info), nil
}

// create is the consumernames.CreateFunc of config's durable: a create of the physical name with
// config's filters and window, the logical name in its metadata, at start: its native policy for a
// new project's first create, else by start sequence. A refusal at the stream's max_consumers is
// consumernames.ErrConsumerLimit.
func (d *durableNames) create(config jetstream.ConsumerConfig) consumernames.CreateFunc {
	return func(ctx context.Context, name string, start consumernames.Start) (*jetstream.ConsumerInfo, error) {
		physical := physicalConfig(config, name)
		switch start.Native {
		case consumernames.NativeNew:
			physical.DeliverPolicy = jetstream.DeliverNewPolicy
		case consumernames.NativeAll:
			physical.DeliverPolicy = jetstream.DeliverAllPolicy
		default:
			physical.DeliverPolicy, physical.OptStartSeq = jetstream.DeliverByStartSequencePolicy, start.Seq
		}
		consumer, err := d.stream.CreateConsumer(ctx, physical)
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

// durableServer is the daemon's consumers as its own NATS grant reaches them: info, and no delete.
type durableServer struct{ stream jetstream.Stream }

// Info reads consumer name through a handle of its own (stream.Consumer), as the readers of a
// fixed name do.
func (s durableServer) Info(ctx context.Context, name string) (*jetstream.ConsumerInfo, error) {
	consumer, err := s.stream.Consumer(ctx, name)
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
