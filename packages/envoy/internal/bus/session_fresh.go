package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
)

// SessionConsumerNamePrefix begins the physical name of every session consumer the fresh mode
// creates (consumernames.FreshName): session~<random>, the family table's (consumernames.SessionPrefix).
// An earlier build's sweep lists only names that begin s-, so it never takes one of these for a
// session.
const SessionConsumerNamePrefix = consumernames.SessionPrefix

// SessionIDMetadataKey is the metadata every session consumer of the fresh mode carries, naming
// its session, beside consumernames.LogicalMetadataKey: the family table's
// (consumernames.SessionIDMetadataKey).
const SessionIDMetadataKey = consumernames.SessionIDMetadataKey

// freshSessions is the selection record of every session, and what reads it.
type freshSessions struct {
	names *consumernames.Names
	store *consumernames.KVStore
	// observe is the counters' callback a caller set (ObserveNames), or nil.
	observe atomic.Pointer[func(consumernames.Operation, consumernames.Outcome)]
	// storeRefusal is the refusal the bucket, its sentinel or the cutover marker last gave a call
	// or a check (a missing bucket or sentinel, a marker that is not done), nil once one admitted a
	// call; SelectionHealth reads the sentinel again while it is set, so a bucket created after
	// the start clears it with no session call.
	storeRefusal atomic.Pointer[error]
	// key is the last refusal of one session's key (a key whose last write is a delete), kept
	// until a call for that same session is admitted: another session's success says nothing of
	// it.
	key atomic.Pointer[keyRefusal]
}

// keyRefusal is a session whose key a call found lost or holding no record of its own, and what
// that call answered.
type keyRefusal struct {
	sessionID string
	err       error
}

// clientRequester is a consumernames.Requester over the client's confirmed requests, so a read
// or write of the selection bucket the connection's user may not make returns ErrPublishDenied
// at once.
type clientRequester struct{ client *Client }

func (r clientRequester) Request(ctx context.Context, msg *nats.Msg) (*nats.Msg, error) {
	return r.client.requestConfirmedMsg(ctx, msg)
}

// sessionNamesServer is the session consumers as consumernames.Server.
type sessionNamesServer struct{ s *SessionConsumers }

func (v sessionNamesServer) Info(ctx context.Context, name string) (*jetstream.ConsumerInfo, error) {
	info, err := v.s.info(ctx, name)
	if errors.Is(err, ErrSessionConsumerNotFound) {
		return nil, fmt.Errorf("%w: %w", consumernames.ErrNotFound, err)
	}
	return info, err
}

func (v sessionNamesServer) Delete(ctx context.Context, name string) error {
	return v.s.delete(ctx, name)
}

// newFreshSessions is the fresh mode of s.
func newFreshSessions(s *SessionConsumers) *freshSessions {
	fresh := &freshSessions{store: s.client.SelectionStore()}
	host, _ := os.Hostname()
	// The naming, the metadata and the key rule are the session family's own
	// (consumernames.SessionNaming), the ones an operator's reader judges a key, a record and a
	// consumer by (consumernames.CheckFamily, consumernames.OwnsFamily).
	naming := consumernames.SessionNaming()
	fresh.names = consumernames.New(consumernames.Config{
		Store:  fresh.store,
		Server: sessionNamesServer{s},
		Prefix: naming.Prefix,
		Fixed:  naming.Fixed,
		Floor:  ackFloor,
		StreamID: func(ctx context.Context) (string, error) {
			state, err := s.streamState(ctx)
			return state.id, err
		},
		RecordFloor: s.recordResumeFloorAt,
		Owner:       fmt.Sprintf("%s/%s/%d", s.client.name, host, os.Getpid()),
		Observe: func(op consumernames.Operation, outcome consumernames.Outcome) {
			if observe := fresh.observe.Load(); observe != nil {
				(*observe)(op, outcome)
			}
		},
		Metadata: naming.Metadata,
		CheckKey: naming.CheckKey,
		// Each create is a consumer request with no deadline of its own, which request bounds by
		// storedMessageWait: the bound a create's candidate is kept named for once retired.
		CreateBound: storedMessageWait,
		// The candidate as the server answers it, against the whole configuration a session
		// consumer's create gives (freshConfig, ConsumerAdoptable): one that differs in a setting no
		// update changes is refused. Its start Names checks, and every setting an update changes is
		// corrected in place by the call that adopted it (published, updated) before it delivers.
		Adopt: func(ctx context.Context, sessionID string, info *jetstream.ConsumerInfo) error {
			return s.client.ConsumerAdoptable(ctx, info.Name, s.freshConfig(info.Name, sessionID, info.Config.OptStartSeq, nil))
		},
	})
	return fresh
}

// sessionConsumerMetadata is the metadata every fresh-mode consumer of sessionID carries, which its
// create writes (freshCreate): the family table's (consumernames.SessionNaming), the one the owner
// check requires, so a key added to one is in the other.
var sessionConsumerMetadata = consumernames.SessionNaming().Metadata

// SelectionStore is the session family's selection bucket (consumernames.SessionBucket) through c's
// confirmed requests, so a read or write c's user may not make answers ErrPublishDenied at once.
// The session consumers keep their records in it, and the listener its own durable's
// (consumernames.DurableKey).
func (c *Client) SelectionStore() *consumernames.KVStore {
	return consumernames.NewKVStore(clientRequester{c})
}

// ConsumerNames is how c's session consumers are named (WithConsumerNames): consumernames.ModeFixed
// unless c was connected in the fresh mode, and for a nil client.
func (c *Client) ConsumerNames() consumernames.Mode {
	if c != nil && c.consumerNames == consumernames.ModeFresh {
		return consumernames.ModeFresh
	}
	return consumernames.ModeFixed
}

// ReadMarker is, in the fixed mode, the read of the selection bucket's cutover marker on the
// session consumers' connection (Client.ReadMarker), which opens their marker gate (GateOnMarker)
// for that connection when the cutover did not convert the store. In the fresh mode it answers nil:
// that mode refuses every call until the marker says done.
func (s *SessionConsumers) ReadMarker(ctx context.Context) error {
	if s.fresh != nil {
		return nil
	}
	return s.client.ReadMarker(ctx)
}

// GateOnMarker gives the session consumers' connection, in the fixed mode, its gate on the
// selection bucket's cutover marker (Client.GateOnMarker): every consumer call of theirs (Ensure,
// UpdateFilter, RestartAtFloor, Delete, Info and the listing), and KeepListed, is refused with
// ErrMarkerUnread until a read of the marker on the connection it would go out on answers that the
// cutover did not convert the store. It does nothing in the fresh mode.
func (s *SessionConsumers) GateOnMarker(converted func(error)) {
	if s.fresh == nil {
		s.client.GateOnMarker(converted)
	}
}

// Fresh reports whether s runs in the fresh mode.
func (s *SessionConsumers) Fresh() bool { return s.fresh != nil }

// ObserveNames sets the counters' callback of the fresh mode's selection steps. The listener counts
// them in envoy_session_consumer_names_total. It does nothing in the fixed mode.
func (s *SessionConsumers) ObserveNames(observe func(consumernames.Operation, consumernames.Outcome)) {
	if s.fresh != nil {
		s.fresh.observe.Store(&observe)
	}
}

// SelectionHealth is, in the fresh mode, why session-consumer calls are refused, naming what is
// missing: the selection bucket, its sentinel, or the cutover marker, read again here while one
// of them refused the last check or call, so a bucket the operator creates after the listener
// starts clears it at the next probe; else the last session whose key was found deleted or holding
// no record of its own (consumernames.ErrMalformedRecord), until a call for that session is
// admitted. nil when neither stands, and always nil in the fixed mode.
// The listener's /healthz names it, within ctx.
func (s *SessionConsumers) SelectionHealth(ctx context.Context) error {
	if s.fresh == nil {
		return nil
	}
	if s.fresh.storeRefusal.Load() != nil {
		if err := s.CheckSelection(ctx); err != nil {
			return err
		}
	}
	if refusal := s.fresh.key.Load(); refusal != nil {
		return refusal.err
	}
	return nil
}

// CheckSelection reads the selection bucket's sentinel and admits it, recording the answer for
// SelectionHealth; nil in the fixed mode, which reads no bucket.
func (s *SessionConsumers) CheckSelection(ctx context.Context) error {
	if s.fresh == nil {
		return nil
	}
	state, err := s.fresh.store.State(ctx)
	if err == nil {
		err = consumernames.Admit(state)
	}
	switch {
	case err == nil:
		s.fresh.storeRefusal.Store(nil)
	case errors.Is(err, consumernames.ErrCutoverNotRun), consumernames.IsSelectionStateLost(err):
		s.fresh.storeRefusal.Store(&err)
	}
	return err
}

// freshAnswer records err for SelectionHealth and returns it in this package's vocabulary. Only an
// answer that proves the store admitted the call clears a refusal: a call that succeeded clears the
// store's and its own session's key's, and a deleted key or one holding no record of its own,
// which the store read to find, clears the store's and names that key. Any other failure leaves
// both as they were, since it may never have reached the store (a refused key, a lost connection),
// and SelectionHealth reads the sentinel again while the store's refusal stands.
func (s *SessionConsumers) freshAnswer(sessionID string, err error) error {
	switch {
	case err == nil:
		s.fresh.storeRefusal.Store(nil)
		if refusal := s.fresh.key.Load(); refusal != nil && refusal.sessionID == sessionID {
			s.fresh.key.CompareAndSwap(refusal, nil)
		}
		return nil
	case errors.Is(err, consumernames.ErrRecordDeleted), errors.Is(err, consumernames.ErrMalformedRecord):
		s.fresh.storeRefusal.Store(nil)
		s.fresh.key.Store(&keyRefusal{sessionID: sessionID, err: err})
	case errors.Is(err, consumernames.ErrCutoverNotRun), consumernames.IsSelectionStateLost(err):
		s.fresh.storeRefusal.Store(&err)
	}
	switch {
	case errors.Is(err, consumernames.ErrInvalidKey):
		return fmt.Errorf("%w: %q", ErrNoSessionConsumerName, sessionID)
	case errors.Is(err, consumernames.ErrNotFound) && !errors.Is(err, ErrSessionConsumerNotFound):
		return fmt.Errorf("%w: %w", ErrSessionConsumerNotFound, err)
	case errors.Is(err, ErrPublishDenied) && !errors.Is(err, ErrConsumerNotGranted):
		return fmt.Errorf("%w: %w", ErrConsumerNotGranted, err)
	}
	return err
}

// freshTarget checks sessionID as a key and reduces subjects to its filters.
func freshTarget(sessionID string, subjects []string) ([]string, error) {
	if err := consumernames.CheckKey(sessionID); err != nil {
		if errors.Is(err, consumernames.ErrInvalidKey) {
			return nil, fmt.Errorf("%w: %q", ErrNoSessionConsumerName, sessionID)
		}
		return nil, err
	}
	filters := SessionConsumerFilters(subjects)
	if len(filters) == 0 {
		return nil, fmt.Errorf("bus: no filter subject for the consumer of session %q", sessionID)
	}
	return filters, nil
}

// freshCreate is the consumernames.CreateFunc of sessionID's consumers on filters: create's
// consumer, at the start Names gives, clamped past the stream's end to its first sequence as every
// create is (pastStreamEnd, firstStart), carrying the session in its metadata. Each create the
// server confirmed writes what it did to *result, and logs the clamp and a start the resume record
// lowered (lowered, set by the first-create rule) as create does.
func (s *SessionConsumers) freshCreate(sessionID string, filters []string, result *SessionConsumerStart, lowered *loweredStart) consumernames.CreateFunc {
	return func(ctx context.Context, name string, start consumernames.Start) (*jetstream.ConsumerInfo, error) {
		state, err := s.streamState(ctx)
		if err != nil {
			return nil, err
		}
		startSeq := start.Seq
		bound, clamped, err := s.pastStreamEnd(ctx, state, startSeq)
		if err != nil {
			return nil, err
		}
		if clamped {
			startSeq = bound.firstStart()
		}
		config := s.freshConfig(name, sessionID, startSeq, filters)
		s.pendingMu.Lock()
		fault := s.createFault
		s.pendingMu.Unlock()
		if fault != nil {
			if err := fault(name); err != nil {
				return nil, err
			}
		}
		info, err := s.upsert(ctx, name, config, "create")
		if errors.Is(err, ErrConsumerLimitReached) {
			return nil, fmt.Errorf("%w: %w", consumernames.ErrConsumerLimit, err)
		}
		if err != nil {
			return nil, err
		}
		if lowered != nil && lowered.lowered && lowered.start == start.Seq {
			s.logger.Info("session consumer created at its resume record's floor, below the start the caller asked",
				slog.String("session_id", sessionID),
				slog.String("consumer", name),
				slog.Uint64("asked_start", lowered.asked),
				slog.Uint64("record_floor", lowered.floor),
				slog.String("stream_id", state.id))
		}
		if clamped {
			s.logger.Warn("session consumer start past the stream's end, created at the stream's first sequence",
				slog.String("session_id", sessionID),
				slog.String("consumer", name),
				slog.Uint64("asked_start", start.Seq),
				slog.Uint64("stream_last_seq", bound.lastSeq),
				slog.String("stream_id", bound.id),
				slog.Uint64("consumer_start", startSeq))
		}
		*result = SessionConsumerStart{Created: true, Name: name, StartSeq: startSeq, Gap: startSeq < bound.firstSeq, StreamID: bound.id, AckFloor: startSeq - 1}
		return info, nil
	}
}

// freshConfig is the whole configuration the fresh mode creates sessionID's consumer name with, at
// stream sequence startSeq, filtering on filters: today's settings (applySettings), the session's
// metadata (sessionConsumerMetadata), and its start. The create sends it (freshCreate), the
// adoption check compares a candidate with it (ConsumerAdoptable), and the correction writes it
// (updated, CorrectedConfig).
func (s *SessionConsumers) freshConfig(name, sessionID string, startSeq uint64, filters []string) jetstream.ConsumerConfig {
	config := jetstream.ConsumerConfig{
		Durable:       name,
		DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:   startSeq,
		Metadata:      sessionConsumerMetadata(sessionID),
	}
	s.applySettings(&config, filters)
	return config
}

// loweredStart is what the first-create rule did to a caller's start, for create's INFO line.
type loweredStart struct {
	lowered             bool
	asked, floor, start uint64
}

// freshFirst is the session family's first-create rule, create's own: the caller's start
// lowered to the resume record's floor plus one; with no caller's start that floor plus one; with
// no record the first sequence the stream holds on the session's own agent subject, else the
// sequence after the stream's last. Names starts a key with no record no later than the cutover
// boundary.
func (s *SessionConsumers) freshFirst(sessionID string, startSeq uint64, lowered *loweredStart) func(context.Context) (consumernames.Start, error) {
	return func(ctx context.Context) (consumernames.Start, error) {
		state, err := s.streamState(ctx)
		if err != nil {
			return consumernames.Start{}, err
		}
		floor, recorded, err := s.resumeFloor(ctx, sessionID, state.id)
		if err != nil {
			return consumernames.Start{}, err
		}
		switch {
		case startSeq > 0:
			if recorded && floor+1 < startSeq {
				*lowered = loweredStart{lowered: true, asked: startSeq, floor: floor, start: floor + 1}
				return consumernames.Start{Seq: floor + 1}, nil
			}
			return consumernames.Start{Seq: startSeq}, nil
		case recorded:
			return consumernames.Start{Seq: floor + 1}, nil
		}
		first, err := s.firstOwnMessage(ctx, sessionID, state.lastSeq+1)
		return consumernames.Start{Seq: first}, err
	}
}

// published is what a call that published sel's current answers: the create's own result when
// this call created it; else the current as Names adopted it (Step 0, or a create whose outcome
// was unknown), which is first brought to the whole configuration the family's create gives, this
// call's filters among them (update, updated), so nothing is delivered from it under a filter or a
// setting this call did not give it. Names adopted it only with its owner and start as the family
// creates them, and every setting no update changes as the family's create gives it
// (ConsumerAdoptable).
func (s *SessionConsumers) published(ctx context.Context, sel consumernames.Selection, result SessionConsumerStart, filters []string) (SessionConsumerStart, error) {
	if result.Name == sel.Record.Current {
		return result, nil
	}
	start := SessionConsumerStart{Created: true, Name: sel.Record.Current}
	if sel.Info == nil {
		return start, nil
	}
	start.StartSeq = sel.Info.Config.OptStartSeq
	start.AckFloor = ackFloor(sel.Info)
	changed, err := s.update(ctx, sel.Record.Current, sel.Info.Config, filters)
	start.Updated = changed
	return start, err
}

// freshEnsure is Ensure in the fresh mode: the session's current consumer, its filters and
// settings updated in place, or a fresh one at the record's start, the caller's lower start, or
// for a session with no record the first-create rule (freshFirst). A current the server no
// longer holds is created again at the start its record proves safe, or refused.
func (s *SessionConsumers) freshEnsure(ctx context.Context, sessionID string, subjects []string, startSeq uint64) (SessionConsumerStart, error) {
	filters, err := freshTarget(sessionID, subjects)
	if err != nil {
		return SessionConsumerStart{}, err
	}
	var result SessionConsumerStart
	var lowered loweredStart
	sel, created, err := s.fresh.names.Ensure(ctx, sessionID, consumernames.CreateRequest{
		Create: s.freshCreate(sessionID, filters, &result, &lowered),
		First:  s.freshFirst(sessionID, startSeq, &lowered),
		Lower:  startSeq,
	})
	if err := s.freshAnswer(sessionID, err); err != nil {
		return SessionConsumerStart{}, err
	}
	if created {
		return s.published(ctx, sel, result, filters)
	}
	changed, err := s.update(ctx, sel.Record.Current, sel.Info.Config, filters)
	return SessionConsumerStart{Name: sel.Record.Current, StartSeq: sel.Info.Config.OptStartSeq, AckFloor: ackFloor(sel.Info), Updated: changed}, err
}

// freshCurrent is sessionID's current consumer and its info, or ErrSessionConsumerNotFound when
// the record selects none or the server no longer holds it. It creates nothing.
func (s *SessionConsumers) freshCurrent(ctx context.Context, sessionID string) (consumernames.Selection, error) {
	if err := consumernames.CheckKey(sessionID); err != nil {
		return consumernames.Selection{}, s.freshAnswer(sessionID, err)
	}
	sel, err := s.fresh.names.Select(ctx, sessionID, nil)
	if err := s.freshAnswer(sessionID, err); err != nil {
		return sel, err
	}
	if sel.Record.Current == "" || sel.Missing {
		return sel, fmt.Errorf("%w: session %s", ErrSessionConsumerNotFound, sessionID)
	}
	return sel, nil
}

// freshUpdateFilter is UpdateFilter in the fresh mode.
func (s *SessionConsumers) freshUpdateFilter(ctx context.Context, sessionID string, subjects []string) error {
	filters, err := freshTarget(sessionID, subjects)
	if err != nil {
		return err
	}
	sel, err := s.freshCurrent(ctx, sessionID)
	if err != nil {
		return err
	}
	_, err = s.update(ctx, sel.Record.Current, sel.Info.Config, filters)
	return err
}

// freshInfo is Info in the fresh mode.
func (s *SessionConsumers) freshInfo(ctx context.Context, sessionID string) (*jetstream.ConsumerInfo, error) {
	sel, err := s.freshCurrent(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return sel.Info, nil
}

// freshRestartAtFloor is RestartAtFloor in the fresh mode. A current with nothing awaiting an ack
// is kept as keepAtFloor keeps it; any other is replaced by a fresh consumer at the sequence after
// its floor, which Names saves before anything is deleted, and the predecessor is deleted only
// after the replacement is published. A session whose record selects no current is
// ErrSessionConsumerNotFound; a current the server no longer holds is created again at its safe
// start.
func (s *SessionConsumers) freshRestartAtFloor(ctx context.Context, sessionID string, subjects []string) (SessionConsumerStart, error) {
	filters, err := freshTarget(sessionID, subjects)
	if err != nil {
		return SessionConsumerStart{}, err
	}
	var result SessionConsumerStart
	create := s.freshCreate(sessionID, filters, &result, nil)
	for conflicts := 0; ; {
		stream := s.joinStreamRead()
		sel, err := s.fresh.names.Select(ctx, sessionID, create)
		if err := s.freshAnswer(sessionID, err); err != nil {
			return SessionConsumerStart{}, err
		}
		if sel.Record.Current == "" {
			return SessionConsumerStart{}, fmt.Errorf("%w: session %s", ErrSessionConsumerNotFound, sessionID)
		}
		if sel.Created {
			return s.published(ctx, sel, result, filters)
		}
		name, info := sel.Record.Current, sel.Info
		if info.NumAckPending == 0 {
			start, kept, err := s.keepAtFloor(ctx, name, info, filters, stream)
			if err != nil || kept {
				start.Name = name
				return start, err
			}
		}
		replaced, err := s.fresh.names.Replace(ctx, sel, ackFloor(info)+1, create)
		if errors.Is(err, consumernames.ErrConflict) {
			if conflicts++; conflicts <= consumernames.MaxConflicts {
				continue
			}
		}
		if err := s.freshAnswer(sessionID, err); err != nil {
			return SessionConsumerStart{}, err
		}
		return s.published(ctx, replaced, result, filters)
	}
}

// freshKeepListed is KeepListed in the fresh mode: info is kept only when it is the session's
// current as its record names it, by the rules KeepListed applies.
func (s *SessionConsumers) freshKeepListed(ctx context.Context, sessionID string, subjects []string, info *jetstream.ConsumerInfo) (SessionConsumerStart, bool, error) {
	filters, err := freshTarget(sessionID, subjects)
	if err != nil {
		return SessionConsumerStart{}, false, err
	}
	if info == nil || info.NumAckPending != 0 {
		return SessionConsumerStart{}, false, nil
	}
	if _, changed := s.updated(info.Config, filters); changed {
		return SessionConsumerStart{}, false, nil
	}
	sel, err := s.fresh.names.Resolve(ctx, sessionID)
	if err := s.freshAnswer(sessionID, err); err != nil {
		return SessionConsumerStart{}, false, err
	}
	if sel.Record.Current != info.Name {
		return SessionConsumerStart{}, false, nil
	}
	if err := s.freshAnswer(sessionID, s.fresh.names.Owns(sessionID, info)); err != nil {
		return SessionConsumerStart{}, false, err
	}
	state, err := s.streamState(ctx)
	if err != nil {
		return SessionConsumerStart{}, false, err
	}
	startSeq := ackFloor(info) + 1
	if startSeq > state.lastSeq+1 {
		return SessionConsumerStart{}, false, nil
	}
	return SessionConsumerStart{Name: info.Name, StartSeq: startSeq, Gap: startSeq < state.firstSeq, StreamID: state.id}, true, nil
}

// freshDelete is Delete in the fresh mode: the delete through the session's record, which records
// the floor on the resume lane and in the record's start before it deletes the current.
func (s *SessionConsumers) freshDelete(ctx context.Context, sessionID string, listed *jetstream.ConsumerInfo) (uint64, error) {
	if consumernames.CheckKey(sessionID) != nil {
		return 0, nil
	}
	floor, err := s.fresh.names.Delete(ctx, sessionID, listed)
	return floor, s.freshAnswer(sessionID, err)
}

// RecordLastFloor writes floor, read from consumer name on the stream streamID, as sessionID's
// last floor in its selection record: the relay calls it at each run. It does nothing in the fixed
// mode, which keeps no record.
func (s *SessionConsumers) RecordLastFloor(ctx context.Context, sessionID, name string, floor uint64, streamID string) error {
	if s.fresh == nil || streamID == "" {
		return nil
	}
	return s.freshAnswer(sessionID, s.fresh.names.RecordLastFloor(ctx, sessionID, name, floor, streamID))
}

// CleanRetired deletes sessionID's retired consumers the server still holds (consumernames
// Names.Cleanup) and answers how many it deleted; the listener's sweep calls it. It does nothing
// in the fixed mode.
func (s *SessionConsumers) CleanRetired(ctx context.Context, sessionID string) (int, error) {
	if s.fresh == nil {
		return 0, nil
	}
	deleted, err := s.fresh.names.Cleanup(ctx, sessionID)
	return deleted, s.freshAnswer(sessionID, err)
}

// EndReplay ends the selection bucket's replay mode once none of the orphans its sentinel lists is
// a consumer the stream holds (consumernames.KVStore.EndReplay): the listener's sweep calls it
// with the names it listed. It does nothing in the fixed mode.
func (s *SessionConsumers) EndReplay(ctx context.Context, held func(name string) bool) error {
	if s.fresh == nil {
		return nil
	}
	_, err := s.fresh.store.EndReplay(ctx, held)
	return err
}

// SessionConsumerSets lists every session consumer of the fresh mode the stream holds, by the
// session its metadata names, current and retired alike, with the paged consumer list; in the
// fixed mode each session's one consumer, as ListSessionConsumers reads it, so the listener's sweep
// lists both modes through this one call.
func (s *SessionConsumers) SessionConsumerSets(ctx context.Context) (map[string][]*jetstream.ConsumerInfo, error) {
	if s.fresh == nil {
		listed, err := s.ListSessionConsumers(ctx)
		if err != nil {
			return nil, err
		}
		sets := make(map[string][]*jetstream.ConsumerInfo, len(listed))
		for sessionID, info := range listed {
			sets[sessionID] = []*jetstream.ConsumerInfo{info}
		}
		return sets, nil
	}
	sets := map[string][]*jetstream.ConsumerInfo{}
	err := s.eachListedConsumer(ctx, func(info *jetstream.ConsumerInfo) {
		if !strings.HasPrefix(info.Name, SessionConsumerNamePrefix+"~") {
			return
		}
		if sessionID := info.Config.Metadata[SessionIDMetadataKey]; sessionID != "" {
			sets[sessionID] = append(sets[sessionID], info)
		}
	})
	if err != nil {
		return nil, err
	}
	return sets, nil
}

// freshList is ListSessionConsumers in the fresh mode: for each session, the consumer of its set
// the server created last (NewestOfEach).
func (s *SessionConsumers) freshList(ctx context.Context) (map[string]*jetstream.ConsumerInfo, error) {
	sets, err := s.SessionConsumerSets(ctx)
	if err != nil {
		return nil, err
	}
	return NewestOfEach(sets), nil
}

// NewestOfEach is, for each session of sets (SessionConsumerSets), the consumer the server created
// last, which is its current unless a replacement is in flight. A caller that keeps a consumer
// from it checks it against the record (KeepListed, Delete), which use it only when it is the
// current.
func NewestOfEach(sets map[string][]*jetstream.ConsumerInfo) map[string]*jetstream.ConsumerInfo {
	listed := make(map[string]*jetstream.ConsumerInfo, len(sets))
	for sessionID, set := range sets {
		listed[sessionID] = slices.MaxFunc(set, func(a, b *jetstream.ConsumerInfo) int { return a.Created.Compare(b.Created) })
	}
	return listed
}
