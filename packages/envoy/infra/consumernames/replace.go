package consumernames

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
)

// replaceMaxName is NATS's limit on a consumer's name (nats-server JSMaxNameLen), and
// replaceFreshSuffix the bytes a fresh name adds to its prefix: a '~' and a 22-character NUID
// (selection.FreshName).
const (
	replaceMaxName     = 255
	replaceFreshSuffix = 1 + 22
)

// replaceListener is `consumer-names replace --listener <machine id> --hold <id>`: it replaces one
// listener's durable by a fresh name at one past its ack floor, through the durable's selection
// record in the session consumers' bucket (selection.DurableKey of bus.ListenerDurable), as the
// selection protocol's Replace does (selection.Names.Replace). It runs only while the hold row is
// held under --hold, so no listener is connected and the durable's floor cannot move under it.
//
// It first reads the record behind the hold's bucket barrier (behindBarrier), as every held reader
// of the bucket does, and settles an operation a crashed run, or a listener before the hold, left
// open on it (Step 0, selection.Names.Select): a candidate the server holds is published, and one it
// does not is retired. Step 0's writes are conditional on the revision it read, so when the
// bucket's leader moved during the read and ReadBucket reads again, the second Select finds them
// written. Then it reserves the replace with start = the current's ack floor + 1, and
// a start past the sequence after the stream's last at the stream's first sequence, as every
// session consumer create clamps one (internal/bus/session_consumers.go, create); creates the
// candidate from the current's own configuration, corrected as the listener's policy needs it (no
// idle heartbeat, no flow control, ack explicit, a new reply inbox) and carrying the durable's
// logical name; publishes it as the current; retires the old name and deletes it. At the stream's
// max_consumers the protocol deletes the retired names the server holds and tries again, and then
// deletes the old name, whose start is saved, before the create. Last, it deletes every retired
// name of the record the server still holds, so a rerun after a crash between the publish and the
// delete finishes the delete. A rerun replaces again from the new current's floor: under the hold
// nothing acknowledges a message, so the new current's floor is its start minus one, and the rerun
// starts at the same sequence.
//
// It is a command of the hold's consumer-cleanup workflow (consumerCleanupAdmit, input listener):
// its intent is its admission, after the held re-read, and its done entry names the durable, the
// name it replaced and the new current with its start. Every exit after its intent, a refusal, an
// UNKNOWN and a done entry whose write failed alike, writes its stop, which records what every run
// of the command in this hold left (admission.stopped): nothing while no run wrote a record, made
// or deleted a consumer, or put this hold's barrier key, which a run puts before it reads the
// record (admission.barrier) and nothing replace does deletes; an UNKNOWN's records what the run
// read and could not read (leftUnknown), never unchanged. A Step 0 that settled an open operation
// wrote the record, and a Replace that failed may have reserved, created or published a name, so a
// stop after either records the record.
func (r *run) replaceListener(ctx context.Context) error {
	if err := core.CheckMachineID("--listener", r.o.listener); err != nil {
		return err
	}
	durable := bus.ListenerDurable(r.o.listener)
	if len(durable)+replaceFreshSuffix > replaceMaxName {
		return fmt.Errorf("--listener: the durable's fresh name, %s~<nuid>, is %d bytes, over NATS's %d-byte consumer-name limit", durable, len(durable)+replaceFreshSuffix, replaceMaxName)
	}
	m, ok := classify(durable)
	if !ok || m.Family != familySession {
		return fmt.Errorf("--listener: %s is no listener durable's name", durable)
	}
	a, err := r.consumerCleanupAdmit(ctx, replaceCommand, map[string]string{"listener": r.o.listener})
	if err != nil {
		return err
	}
	const nothing = "this run wrote no record and made or deleted no consumer"
	nc, _, err := r.conn(ctx)
	if err != nil {
		return a.stopped(ctx, err, true, nothing)
	}
	// The bucket barrier reads the bucket's stream leader first, which a missing bucket answers
	// UNKNOWN; a missing bucket is replace's refusal.
	if _, err := streamInfo(ctx, nc, selectionStreamName); apiErrorIs(err, jetstream.JSErrCodeStreamNotFound) {
		return a.stopped(ctx, fmt.Errorf("the selection bucket %s is missing, so %s has no record to replace by: %w", selection.SessionBucket, durable, err), true, nothing)
	} else if err != nil {
		return a.stopped(ctx, unknownf(err, "the info of %s", selectionStreamName), true, nothing)
	}
	r.read("the info of " + selectionStreamName)
	store, err := r.store(ctx, familySession)
	if err != nil {
		return a.stopped(ctx, err, true, nothing)
	}
	names := replaceNames(nc, store, r.o.holdID)
	var sel selection.Selection
	var selected, read error
	var before selection.Record
	var revision uint64
	a.barrier = true
	if err := behindBarrier(ctx, nc, r.o.holdID, func(ctx context.Context) error {
		before, revision, read = store.Read(ctx, m.Logical)
		sel, selected = names.Select(ctx, m.Logical, nil)
		return nil
	}); err != nil {
		return a.stopped(ctx, err, true, nothing)
	}
	r.read(fmt.Sprintf("%s's record %s behind the bucket barrier", durable, m.Logical))
	// Step 0 writes the record only when it holds an open operation: settled is whether this
	// run's Select may have written it, a record it could not read first counting as one it may.
	settled := (read != nil && !errors.Is(read, selection.ErrNoRecord) && !errors.Is(read, selection.ErrRecordDeleted)) ||
		(read == nil && before.Op != nil && (selected != nil || sel.Revision != revision))
	account := nothing
	if settled {
		account = fmt.Sprintf("this run's Step 0 may have settled the open operation of %s's record %s", durable, m.Logical)
	}
	switch {
	case errors.Is(selected, selection.ErrCutoverNotRun):
		return a.stopped(ctx, fmt.Errorf("%s: the selection bucket's sentinel does not read cutover done, so the listener's durable is its fixed name, which replace does not take: %w", durable, selected), !settled, account)
	case selected != nil:
		return a.stopped(ctx, fmt.Errorf("%s's selection record (%s): %w", durable, m.Logical, selected), !settled, account)
	case sel.Record.Current == "":
		return a.stopped(ctx, fmt.Errorf("%s's selection record (%s) selects no current: the listener's start creates its durable at the record's start", durable, m.Logical), !settled, account)
	case sel.Missing:
		return a.stopped(ctx, fmt.Errorf("%s's selection record (%s) selects %s, which %s does not hold: the listener's start creates it again at the start the record proves", durable, m.Logical, sel.Record.Current, notifications), !settled, account)
	}
	old, info := sel.Record.Current, sel.Info
	stream, err := streamInfo(ctx, nc, notifications)
	if err != nil {
		return a.stopped(ctx, unknownf(err, "the info of %s", notifications), !settled, account)
	}
	r.read("the info of " + notifications)
	start := clampStart(info.AckFloor.Stream+1, stream.State)
	replaced, err := names.Replace(ctx, sel, start, replaceCreate(nc, info.Config, m))
	if err != nil {
		return a.stopped(ctx, fmt.Errorf("replace %s (%s) at %d: %w", old, durable, start, err), false,
			fmt.Sprintf("this run's replace of %s may have reserved, created or published a fresh name in %s's record %s, which its rerun settles (Step 0)", old, durable, m.Logical))
	}
	deleted, err := replaceDeleteRetired(ctx, nc, m, replaced.Record)
	replacedAccount := fmt.Sprintf("this run replaced %s by %s in %s's record %s, and deleted %d of its retired names", old, replaced.Record.Current, durable, m.Logical, deleted)
	if err != nil {
		return a.stopped(ctx, fmt.Errorf("%s is %s's current at %d, and %w", replaced.Record.Current, durable, replaced.Record.Start, err), false, replacedAccount)
	}
	fmt.Fprintf(r.stdout, "replace: %s (record %s): %s replaced by %s at stream sequence %s; %s retired; deleted %d retired name(s) the server held\n",
		durable, m.Logical, old, replaced.Record.Current, formatSeq(replaced.Record.Start), old, deleted)
	outputs := map[string]string{"durable": durable, "replaced": old, "current": replaced.Record.Current,
		"start": formatSeq(replaced.Record.Start), "deleted": strconv.Itoa(deleted)}
	if err := a.done(ctx, outputs); err != nil {
		return a.stopped(ctx, err, false, replacedAccount)
	}
	return nil
}

// replaceNames is the selection protocol over a listener durable's record in store, on the
// operator's connection nc, as the listener runs it for its own durable
// (cmd/listener/listener_durable_names.go, durableNames): the naming of the library's table for a
// durable's key (selection.DurableNaming: the durable's own name as each fresh name's prefix and as
// its fixed name, envoy.logical naming the durable on each of its consumers, which the protocol's
// owner check holds every consumer the record names to), its floor the ack floor, and its key
// DurableKey's.
func replaceNames(nc *nats.Conn, store selection.Store, holdID string) *selection.Names {
	naming := selection.DurableNaming()
	return selection.New(selection.Config{
		Store:  store,
		Server: replaceServer{nc: nc},
		Prefix: naming.Prefix,
		Fixed:  naming.Fixed,
		Floor:  func(info *jetstream.ConsumerInfo) uint64 { return info.AckFloor.Stream },
		StreamID: func(ctx context.Context) (string, error) {
			info, err := streamInfo(ctx, nc, notifications)
			if err != nil {
				return "", err
			}
			return selection.StreamIdentity(info), nil
		},
		CheckKey: selection.CheckDurableKey,
		Metadata: naming.Metadata,
		Owner:    "envoy-infra consumer-names replace, hold " + holdID,
	})
}

// replaceServer is the notification stream's consumers on the operator's connection, as the
// selection protocol reads and deletes them (selection.Server).
type replaceServer struct{ nc *nats.Conn }

func (s replaceServer) Info(ctx context.Context, name string) (*jetstream.ConsumerInfo, error) {
	info, err := consumerInfo(ctx, s.nc, notifications, name)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, fmt.Errorf("%w: %s on %s", selection.ErrNotFound, name, notifications)
	}
	return info, nil
}

func (s replaceServer) Delete(ctx context.Context, name string) error {
	return deleteConsumer(ctx, s.nc, notifications, name)
}

// replaceCreate is the selection.CreateFunc of the replace's candidate: the current's own
// configuration, base, under the candidate's name, carrying m's metadata, at the start the protocol
// gives by start sequence, corrected as packages/envoy/deploy/README.md's recreation corrected it
// (no idle heartbeat, no flow control, ack explicit), and delivering to a new reply inbox, as the
// listener's own create gives one (cmd/listener/durable.go, ensureFixedDurable), so the candidate
// and the name it replaces never deliver to one subject. A refusal at the stream's max_consumers is
// selection.ErrConsumerLimit; the server's other answers stay *jetstream.APIError, which the
// protocol reads as answered, and any other error is a create whose outcome it settles with an
// info.
func replaceCreate(nc *nats.Conn, base jetstream.ConsumerConfig, m member) selection.CreateFunc {
	return func(ctx context.Context, name string, start selection.Start) (*jetstream.ConsumerInfo, error) {
		config := base
		config.Name, config.Durable = name, name
		config.Metadata = m.Metadata()
		config.DeliverSubject = nats.NewInbox()
		config.IdleHeartbeat, config.FlowControl = 0, false
		config.AckPolicy = jetstream.AckExplicitPolicy
		config.DeliverPolicy, config.OptStartSeq, config.OptStartTime = jetstream.DeliverByStartSequencePolicy, start.Seq, nil
		info, err := writeConsumer(ctx, nc, notifications, config, actionCreateConsumer)
		if apiErrorIs(err, jetstream.JSErrCodeMaximumConsumersLimit) {
			return nil, fmt.Errorf("%w: %w", selection.ErrConsumerLimit, err)
		}
		return info, err
	}
}

// replaceDeleteRetired deletes each retired name of record, but its current, that the server still
// holds and that is m's own (deleteIfOwned), and answers how many it deleted. The names stay in the
// record's retired list, which the cleanup command reads.
func replaceDeleteRetired(ctx context.Context, nc *nats.Conn, m member, record selection.Record) (int, error) {
	deleted := 0
	for _, name := range record.Retired {
		if name == record.Current {
			continue
		}
		info, err := consumerInfo(ctx, nc, notifications, name)
		if err != nil {
			return deleted, unknownf(err, "the info of retired %s", name)
		}
		if info == nil {
			continue
		}
		if err := deleteIfOwned(ctx, nc, m, info); err != nil {
			return deleted, fmt.Errorf("the delete of retired %s failed: %w", name, err)
		}
		deleted++
	}
	return deleted, nil
}
