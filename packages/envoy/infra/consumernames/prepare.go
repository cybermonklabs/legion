package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// prepareThreshold is the inactive threshold prepare raises every fixed name's to, when it has a
// shorter one: long enough that no fixed name a creator stops pulling at the hold is removed for
// inactivity before convert reads it. An update re-arms the delete timer with the new threshold
// (nats-server consumer.go:1515-1533, 2670-2676), and the timer measures the time since the last
// pull against it (:2266-2275).
const prepareThreshold = 7 * 24 * time.Hour

// The hold's cohort record (holdrecord.Client.PutRecord): kind cohort, key cohort, which prepare
// writes and `hold abort` from prepare deletes (holdrecord.Client.DeleteCohort).
const (
	prepareCohortKind = "cohort"
	prepareCohortKey  = "cohort"
)

// prepareCohort is the hold's cohort record: the cutover boundary B and, for each logical name a
// fixed name held on the notification stream when prepare listed it, that fixed name and its ack
// floor. An ack floor never goes down, so a floor read before the hold is a safe bound of the
// logical name's start after it: convert starts a cohort member whose fixed name is gone by then
// at its floor + 1, and create-bucket writes B into the selection bucket's sentinel.
type prepareCohort struct {
	// Boundary is B, max(1, first_seq) of ENVOY_NOTIFICATIONS: the stream's first retained
	// sequence, which covers every older start a consumer created after the listing can carry. An
	// empty stream answers first_seq 0 (nats-server filestore.go:526), and DeliverByStartSequence
	// refuses a start of 0 (consumer.go:957-959), so B is at least 1.
	Boundary uint64 `json:"boundary"`
	// StreamID is the stream's identity (selection.StreamIdentity) when prepare read it.
	StreamID string `json:"stream_id,omitempty"`
	// Members are the fixed names prepare listed, sorted by fixed name.
	Members []prepareCohortMember `json:"members"`
}

// prepareCohortMember is one logical name of the cohort.
type prepareCohortMember struct {
	Family  string `json:"family"`
	Logical string `json:"logical"`
	Fixed   string `json:"fixed"`
	// Floor is the fixed name's ack floor (convertFloor) as prepare read it.
	Floor uint64 `json:"floor"`
	// Threshold is the fixed name's own inactive threshold, its creator's, before prepare raised
	// it (0: none). convert gives the fresh name this one, not prepareThreshold, which only keeps
	// the fixed name through the window.
	Threshold time.Duration `json:"threshold,omitempty"`
}

// member is the cohort's member of fixed name, or nil.
func (c prepareCohort) member(fixed string) *prepareCohortMember {
	i, found := slices.BinarySearchFunc(c.Members, fixed, func(m prepareCohortMember, fixed string) int {
		return strings.Compare(m.Fixed, fixed)
	})
	if !found {
		return nil
	}
	return &c.Members[i]
}

// readPrepareCohort reads hold holdID's cohort record with its gen, which a write of it is
// conditional on (0 when there is none), and false when it has none.
func readPrepareCohort(ctx context.Context, records *holdrecord.Client, holdID string) (prepareCohort, uint64, bool, error) {
	return readHoldRecord[prepareCohort](ctx, records, holdID, prepareCohortKind, prepareCohortKey,
		fmt.Sprintf("hold %s's cohort record does not decode", holdID))
}

// prepareCutover is `consumer-names prepare --hold <id>`, before the hold is set and while every
// client still pulls (§3 step 1): under the prepare row of its hold, it reads ENVOY_NOTIFICATIONS'
// first sequence as the cutover boundary B, lists every fixed name, raises the inactive threshold
// of each that has a shorter one to prepareThreshold through a consumer update, reads each one's
// ack floor, and writes the hold's cohort record. It writes no journal entry: the journal admits
// only `hold set` under a prepare row (holdrecord.Admit).
//
// A rerun writes the cohort again, keeping the lower of each value it read before and reads now
// (B, and each member's floor) and each member it listed before and lists no more: a lower bound
// can only make a logical name receive a message again, never miss one.
func (r *run) prepareCutover(ctx context.Context) error {
	if _, err := r.requirePhase(ctx, natsgrant.OperatorPhasePrepare); err != nil {
		return err
	}
	nc, _, err := r.conn(ctx)
	if err != nil {
		return unknownf(err, "the operator's connection")
	}
	stream, err := streamInfo(ctx, nc, notifications)
	if err != nil {
		return unknownf(err, "%s's state", notifications)
	}
	cohort := prepareCohort{Boundary: max(1, stream.State.FirstSeq), StreamID: selection.StreamIdentity(stream)}
	names, err := consumerNames(ctx, nc, notifications)
	if err != nil {
		return err
	}
	raised := 0
	for _, name := range names {
		m, ok := classify(name)
		if !ok {
			continue
		}
		info, err := consumerInfo(ctx, nc, notifications, name)
		if err != nil {
			return unknownf(err, "the info of %s", name)
		}
		if info == nil {
			// Removed since the listing: no floor of it was read, so it is no member.
			continue
		}
		threshold := info.Config.InactiveThreshold
		if threshold > 0 && threshold < prepareThreshold {
			config := info.Config
			config.InactiveThreshold = prepareThreshold
			updated, err := writeConsumer(ctx, nc, notifications, config, actionUpdateConsumer)
			switch {
			case apiErrorIs(err, jetstream.JSErrCodeConsumerNotFound) || apiErrorIs(err, jetstream.JSErrCodeConsumerDoesNotExist):
				// Deleted since its info, by a creator's sweep, deregistration or reaper, which
				// every client still runs in phase prepare: it needs no raise, and the floor its
				// info answered stays a safe bound of its logical name, since a floor never goes
				// down.
			case err != nil:
				return fmt.Errorf("raise %s's inactive threshold to %s: %w", name, prepareThreshold, err)
			default:
				info = updated
				raised++
			}
		}
		cohort.Members = append(cohort.Members, prepareCohortMember{Family: m.Family, Logical: m.Logical, Fixed: name,
			Floor: convertFloor(info), Threshold: threshold})
	}
	written, err := r.writePrepareCohort(ctx, cohort)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.stdout, "hold %s: cohort of %d fixed names (%d thresholds raised to %s), boundary %d\n",
		r.o.holdID, len(written.Members), raised, prepareThreshold, written.Boundary)
	return nil
}

// writePrepareCohort writes read as the hold's cohort record, merged with the record a run before
// it wrote (mergePrepareCohort), at that record's gen, and reads again after a write that came
// between; it answers the cohort it wrote.
func (r *run) writePrepareCohort(ctx context.Context, read prepareCohort) (prepareCohort, error) {
	for attempt := 1; ; attempt++ {
		earlier, gen, found, err := readPrepareCohort(ctx, r.records, r.o.holdID)
		if err != nil {
			return prepareCohort{}, err
		}
		cohort := read
		if found {
			cohort = mergePrepareCohort(earlier, read)
		}
		slices.SortFunc(cohort.Members, func(a, b prepareCohortMember) int { return strings.Compare(a.Fixed, b.Fixed) })
		value, err := json.Marshal(cohort)
		if err != nil {
			return prepareCohort{}, err
		}
		_, err = r.records.PutRecord(ctx, r.o.holdID, prepareCohortKind, prepareCohortKey, gen, value)
		var moved *holdrecord.RouteError
		if errors.As(err, &moved) && moved.Code == "gen_mismatch" && attempt < 16 {
			continue
		}
		if err != nil {
			return prepareCohort{}, fmt.Errorf("write hold %s's cohort record: %w", r.o.holdID, err)
		}
		return cohort, nil
	}
}

// mergePrepareCohort is the cohort of a rerun: the lower boundary, every member of either, and
// for a member of both, the lower floor (a fixed name deleted and made again by an old build's
// restart can carry a lower start than the one before).
func mergePrepareCohort(earlier, now prepareCohort) prepareCohort {
	merged := prepareCohort{Boundary: min(earlier.Boundary, now.Boundary), StreamID: now.StreamID}
	if merged.Boundary == 0 {
		merged.Boundary = max(earlier.Boundary, now.Boundary)
	}
	byFixed := map[string]prepareCohortMember{}
	for _, m := range earlier.Members {
		byFixed[m.Fixed] = m
	}
	for _, m := range now.Members {
		if before, ok := byFixed[m.Fixed]; ok {
			// The earlier run read the fixed name before any raise: its threshold is the
			// creator's, where a rerun reads the one the earlier run raised.
			m.Threshold = before.Threshold
			if before.Floor < m.Floor {
				m.Floor = before.Floor
			}
		}
		byFixed[m.Fixed] = m
	}
	for _, m := range byFixed {
		merged.Members = append(merged.Members, m)
	}
	return merged
}
