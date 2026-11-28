package intake

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// replayNow is the clock of the replay tests: the give-ups are recorded before it, and every
// replay and discard happens at it.
var replayNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// replaySpec is consumerSpec at replayNow, for the replay tests that t runs. Its acknowledgement
// window is a hang guard, not the subject: it bounds each statement a replay makes, which
// consumerSpec's 200 ms cuts on a loaded runner and any fixed span lets the machine decide, so it
// is what is left of the test binary's deadline less 30 s (testwait.Remaining, docs/testing.md).
func replaySpec(t *testing.T, logs *lockedBuffer) ConsumerSpec {
	spec := consumerSpec(logs)
	spec.AckWait = testwait.Remaining(t)
	spec.Now = func() time.Time { return replayNow }
	return spec
}

// giveUpsOf is the replay and the discard of CAPTURE's give-ups on pool, applying with handlers.
func giveUpsOf(t *testing.T, pool store.TxPool, spec ConsumerSpec, handlers ...Handler) *GiveUps {
	t.Helper()
	giveUps, err := NewGiveUps(Records{Pool: pool, Spec: spec, Repositories: func() []ghrepo.Repository { return spec.Repositories }}, handlers...)
	if err != nil {
		t.Fatalf("NewGiveUps: %v", err)
	}
	return giveUps
}

// storedGiveUp records data, the message consumer delivered on subject at stream sequence seq, as
// the intake records a decoded message it gives up at its bound after deliveries deliveries
// (NoteFailure, then GiveUpIntakeMessage), with the fact's source and event id, and answers the
// record.
func storedGiveUp(t *testing.T, pool *pgxpool.Pool, consumer, subject string, data []byte, seq uint64, deliveries int) record.IntakeFailure {
	t.Helper()
	decoded, err := decodeMessage(subject, "CAPTURE", capturedRepositories, nil, data)
	if err != nil {
		t.Fatalf("decode the message to give up: %v", err)
	}
	message := intakeTestMessage(consumer, subject, data, seq)
	message.EventSource, message.EventID = decoded.Source, decoded.EventID
	ctx := context.Background()
	var given record.IntakeFailure
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		records := record.NewStore()
		failure, err := records.NoteFailure(ctx, tx, message, deliveries, "the handler failed", replayNow.Add(-time.Hour))
		if err != nil {
			return err
		}
		var gaveUp bool
		given, gaveUp, err = records.GiveUpIntakeMessage(ctx, tx, "CAPTURE", failure.ID, record.GiveUpBound, "the handler failed", replayNow.Add(-time.Minute))
		if err == nil && !gaveUp {
			err = errors.New("it had no open failure to give up")
		}
		return err
	}); err != nil {
		t.Fatalf("give up %s's message %d: %v", consumer, seq, err)
	}
	return given
}

// storedUndecodable records data, the message consumer delivered on subject at stream sequence
// seq, as given up at its first delivery for not decoding, with decodeError as its error
// (RecordUndecodable), and answers the record.
func storedUndecodable(t *testing.T, pool *pgxpool.Pool, consumer, subject string, data []byte, seq uint64, decodeError string) record.IntakeFailure {
	t.Helper()
	ctx := context.Background()
	var given record.IntakeFailure
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var recorded bool
		var err error
		given, recorded, err = record.NewStore().RecordUndecodable(ctx, tx, intakeTestMessage(consumer, subject, data, seq), 1, decodeError, replayNow.Add(-time.Minute))
		if err == nil && !recorded {
			err = errors.New("it was recorded already")
		}
		return err
	}); err != nil {
		t.Fatalf("record %s's undecodable message %d: %v", consumer, seq, err)
	}
	return given
}

func intakeTestMessage(consumer, subject string, data []byte, seq uint64) record.IntakeMessage {
	return record.IntakeMessage{Project: "CAPTURE", Consumer: consumer, Subject: subject, StreamSeq: seq,
		StoredAt: replayNow.Add(-2 * time.Hour), Payload: data}
}

// replayIssueEnvelope is a Dispatch event on CAPTURE issue key, under the event id replay-<key>.
func replayIssueEnvelope(t *testing.T, key string) []byte {
	t.Helper()
	return envelopeJSON(t, "replay-"+key, "dispatch", fmt.Sprintf(`{"id":1,"issue_key":%q,"seq":1,"notify":true,"type":"issue.updated",`+
		`"actor":{"kind":"user","id":"someone"},"payload":{"key":%q,"status":"backlog","title":"a replayed event","parent":null,"rank":"A",`+
		`"labels":[],"resolved_repo":"sjawhar/legion"}}`, key, key))
}

// replayIssueSubject is the subject of replayIssueEnvelope's event on key.
func replayIssueSubject(key string) string {
	return "notifications.dispatch.issue." + key + ".issue.updated"
}

// replayedRecord is give-up id as the record keeps it after a replay or a discard, read through the
// record's own read (intakeRecords): its error, its claim, its latest close and its discard are
// what the tests here read of it.
func replayedRecord(t *testing.T, pool *pgxpool.Pool, id int64) record.IntakeFailure {
	t.Helper()
	for _, f := range intakeRecords(t, pool) {
		if f.ID == id {
			return f
		}
	}
	t.Fatalf("read give-up %d: the table holds no such record", id)
	return record.IntakeFailure{}
}

// processedFacts is how many facts processed_events holds, and how many of source's eventID.
func processedFacts(t *testing.T, pool *pgxpool.Pool, source, eventID string) (all, these int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `select count(*), count(*) filter (where source = $1 and event_id = $2) from processed_events`,
		source, eventID).Scan(&all, &these); err != nil {
		t.Fatalf("read processed_events: %v", err)
	}
	return all, these
}

// keysHandler is a handler that keeps the issue key of each Dispatch issue fact it is handed, in
// order, and fails the keys in fail.
type keysHandler struct {
	mu   sync.Mutex
	keys []string
	fail map[string]bool
}

func (h *keysHandler) Apply(_ context.Context, _ pgx.Tx, fact Fact) (Result, error) {
	issue, _ := fact.(DispatchIssue)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.keys = append(h.keys, issue.Key)
	if h.fail[issue.Key] {
		return Result{}, fmt.Errorf("%s still fails", issue.Key)
	}
	return Result{}, nil
}

func (h *keysHandler) handled() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.keys)
}

// A message given up while its handler failed is applied, once the cause is fixed, by a replay of
// its record: the stored message runs through the handling a live delivery takes, the
// preparation of its issue among it, and its fact is applied once, under its own event id. Nothing
// is published: the stream holds what it held. The record closes as applied by its caller, in the
// fact's own transaction, on the daemon's guarded pool (store.Pool), which would refuse the close a
// second connection while the fact's transaction holds one.
func TestAReplayAppliesTheStoredMessageOnceThroughTheLiveHandling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := migratedPool(t)
	pool := store.NewPool(raw)
	js, stream := testJetStream(t)
	spec := boundSpec(&lockedBuffer{}, 1, time.Nanosecond)
	spec.AckWait = testwait.Remaining(t)
	var prepared atomic.Int32
	spec.PrepareRepository = func(_ context.Context, issue DispatchIssue) (DispatchIssue, error) {
		prepared.Add(1)
		return issue, nil
	}
	consumers, err := OpenConsumers(ctx, js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	var fixed atomic.Bool
	var applied atomic.Int32
	handler := handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) {
		if !fixed.Load() {
			return Result{}, errAlwaysFails
		}
		applied.Add(1)
		return Result{}, nil
	})
	stop := runConsumers(t, consumers, handler)
	sent := publish(t, js, replayIssueSubject("CAPTURE-3"), capturedIssueUpdatedEnvelope(t))
	dispatch := dispatchConsumerName(spec.Project)
	given := waitGivenUp(t, raw, dispatch, sent.Sequence)
	stop()

	fixed.Store(true)
	preparedBefore := prepared.Load()
	giveUps, err := NewGiveUps(consumers.Records(), handler)
	if err != nil {
		t.Fatalf("NewGiveUps: %v", err)
	}
	answer, err := giveUps.Replay(ctx, record.IntakeSelector{ID: given.ID}, "operator")
	if err != nil {
		t.Fatalf("replay give-up %d: %v", given.ID, err)
	}
	want := ReplayAnswer{Replayed: []ReplayedGiveUp{{ID: given.ID, Consumer: dispatch, StreamSeq: sent.Sequence, Outcome: ReplayApplied}}}
	if !slices.Equal(answer.Replayed, want.Replayed) || answer.Remaining != 0 {
		t.Fatalf("the replay answered %+v, want %+v", answer, want)
	}
	if calls := applied.Load(); calls != 1 {
		t.Errorf("the handler applied the fact %d times, want once", calls)
	}
	if all, these := processedFacts(t, raw, given.EventSource, given.EventID); all != 1 || these != 1 {
		t.Errorf("processed_events holds %d facts, %d of them %s/%s, want that one fact once", all, these, given.EventSource, given.EventID)
	}
	if calls := prepared.Load() - preparedBefore; calls != 1 {
		t.Errorf("the replay prepared the issue %d times, want once, as a live delivery prepares it", calls)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if info.State.LastSeq != sent.Sequence {
		t.Errorf("the stream's last sequence is %d after the replay, want %d: a replay publishes nothing", info.State.LastSeq, sent.Sequence)
	}
	closed := replayedRecord(t, raw, given.ID)
	if closed.ClosedHow != record.IntakeApplied || closed.ClosedBy != "operator" || closed.ClosedAt.IsZero() || !closed.ReplayClaimedAt.IsZero() {
		t.Errorf("the record = %+v, want it closed as applied by the operator, with no claim left", closed)
	}
}

// A replay of a second record of a fact applied already, the same event given up twice as two
// stream messages, answers already applied: no handler runs, and the duplicate line is written,
// naming the record's consumer, stream sequence and delivery count and the record it replays, so
// a reader tells the replay from a redelivery.
func TestASecondReplayAnswersAlreadyAppliedAndWritesTheDuplicateLine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := migratedPool(t)
	logs := &lockedBuffer{}
	handler := &keysHandler{}
	giveUps := giveUpsOf(t, pool, replaySpec(t, logs), handler)
	dispatch := dispatchConsumerName("CAPTURE")
	data := replayIssueEnvelope(t, "CAPTURE-3")
	first := storedGiveUp(t, pool, dispatch, replayIssueSubject("CAPTURE-3"), data, 11, 21)
	second := storedGiveUp(t, pool, dispatch, replayIssueSubject("CAPTURE-3"), data, 12, 23)

	if answer, err := giveUps.Replay(ctx, record.IntakeSelector{ID: first.ID}, "operator"); err != nil || answer.Replayed[0].Outcome != ReplayApplied {
		t.Fatalf("the first replay answered %+v (%v), want applied", answer, err)
	}
	answer, err := giveUps.Replay(ctx, record.IntakeSelector{ID: second.ID}, "controller session ses-controller")
	if err != nil {
		t.Fatalf("replay give-up %d: %v", second.ID, err)
	}
	if want := (ReplayedGiveUp{ID: second.ID, Consumer: dispatch, StreamSeq: 12, Outcome: ReplayAlreadyApplied}); len(answer.Replayed) != 1 || answer.Replayed[0] != want {
		t.Fatalf("the second replay answered %+v, want %+v", answer, want)
	}
	if handled := handler.handled(); !slices.Equal(handled, []string{"CAPTURE-3"}) {
		t.Errorf("the handler was handed %v, want the fact once, by the first replay", handled)
	}
	if closed := replayedRecord(t, pool, second.ID); closed.ClosedHow != record.IntakeAlreadyApplied || closed.ClosedBy != "controller session ses-controller" || closed.ClosedAt != replayNow {
		t.Errorf("the second record = %+v, want it closed as already applied by the controller's session at %s", closed, replayNow)
	}

	written := logs.String()
	if said := strings.Count(written, droppedDuplicateLine); said != 1 {
		t.Fatalf("%d lines say a fact was dropped as already applied, want one, the second replay's, in:\n%s", said, written)
	}
	line := logLine(written, droppedDuplicateLine)
	for key, want := range map[string]string{"consumer": dispatch, "stream_seq": "12", "delivered": "23", "replay": fmt.Sprint(second.ID),
		"source": "dispatch", "event_id": "replay-CAPTURE-3"} {
		if said := logValue(line, key); said != want {
			t.Errorf("the duplicate line says %s=%s, want %s: %s", key, said, want, line)
		}
	}
	if !strings.Contains(line, "level=INFO") || !strings.Contains(line, "subject=") {
		t.Errorf("the duplicate line is not at info with its subject: %s", line)
	}
}

// A message the live handling acknowledges with no transaction closes, replayed, as no fact: a
// review of a pull request the daemon does not record, a GitHub event of a repository the daemon
// no longer hears (the replay decodes against the repositories GitHub intake hears now, as a live
// delivery would), and a message that decodes to no fact at all. No handler runs, nothing is
// marked processed, and the record closes by its caller.
func TestAReplayOfAMessageHandledWithNoTransactionAnswersNoFact(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name     string
		consumer string
		subject  string
		data     func(t *testing.T) []byte
		// heard is the repositories GitHub intake hears at the replay, the spec's when nil; the
		// message was given up while sjawhar/legion was heard (storedGiveUp).
		heard []ghrepo.Repository
	}{
		{name: "a review of a pull request the daemon does not record", consumer: githubConsumerName("CAPTURE"),
			subject: "notifications.github.sjawhar.legion.pr.42.review", data: func(t *testing.T) []byte { return capturedGitHubEnvelope(t, "review.json") }},
		{name: "a GitHub event of a repository the daemon no longer hears", consumer: githubConsumerName("CAPTURE"),
			subject: "notifications.github.sjawhar.legion.pr.42", data: func(t *testing.T) []byte { return capturedGitHubEnvelope(t, "pr-opened.json") },
			heard: []ghrepo.Repository{ghrepo.MustParse("acme/widgets")}},
		{name: "another project's issue event, which decodes to no fact", consumer: dispatchConsumerName("CAPTURE"),
			subject: replayIssueSubject("OTHER-3"), data: func(t *testing.T) []byte { return replayIssueEnvelope(t, "OTHER-3") }},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool := migratedPool(t)
			spec := replaySpec(t, &lockedBuffer{})
			if row.heard != nil {
				spec.Repositories = row.heard
			}
			handler := &keysHandler{}
			given := storedGiveUp(t, pool, row.consumer, row.subject, row.data(t), 7, 21)
			answer, err := giveUpsOf(t, pool, spec, handler).Replay(context.Background(), record.IntakeSelector{ID: given.ID}, "operator")
			if err != nil {
				t.Fatalf("replay give-up %d: %v", given.ID, err)
			}
			if want := (ReplayedGiveUp{ID: given.ID, Consumer: row.consumer, StreamSeq: 7, Outcome: ReplayNoFact}); len(answer.Replayed) != 1 || answer.Replayed[0] != want || answer.Remaining != 0 {
				t.Fatalf("the replay answered %+v, want %+v and none remaining", answer, want)
			}
			if handled := handler.handled(); len(handled) != 0 {
				t.Errorf("the handler was handed %d facts, want none", len(handled))
			}
			if all, _ := processedFacts(t, pool, "", ""); all != 0 {
				t.Errorf("processed_events holds %d facts, want none", all)
			}
			if closed := replayedRecord(t, pool, given.ID); closed.ClosedHow != record.IntakeNoFact || closed.ClosedBy != "operator" || closed.ClosedAt != replayNow {
				t.Errorf("the record = %+v, want it closed as no fact by the operator at %s", closed, replayNow)
			}
		})
	}
}

// A replay that fails again answers failed with the new error, keeps that error on the record,
// gives back its claim and leaves the record open, so it is counted among what remains: a handler
// whose cause is not fixed yet, and a message that still does not decode.
func TestAReplayThatFailsKeepsTheRecordOpenWithItsNewError(t *testing.T) {
	t.Parallel()
	dispatch := dispatchConsumerName("CAPTURE")
	for _, row := range []struct {
		name  string
		given func(t *testing.T, pool *pgxpool.Pool) record.IntakeFailure
		want  string
	}{
		{name: "the handling fails again", want: "CAPTURE-3 still fails", given: func(t *testing.T, pool *pgxpool.Pool) record.IntakeFailure {
			return storedGiveUp(t, pool, dispatch, replayIssueSubject("CAPTURE-3"), replayIssueEnvelope(t, "CAPTURE-3"), 7, 21)
		}},
		{name: "the message still does not decode", want: "decode Envoy envelope", given: func(t *testing.T, pool *pgxpool.Pool) record.IntakeFailure {
			return storedUndecodable(t, pool, dispatch, replayIssueSubject("CAPTURE-3"), []byte(`{`), 7, "an earlier decoder's error")
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool := migratedPool(t)
			given := row.given(t, pool)
			handler := &keysHandler{fail: map[string]bool{"CAPTURE-3": true}}
			answer, err := giveUpsOf(t, pool, replaySpec(t, &lockedBuffer{}), handler).Replay(context.Background(), record.IntakeSelector{ID: given.ID}, "operator")
			if err != nil {
				t.Fatalf("replay give-up %d: %v", given.ID, err)
			}
			if len(answer.Replayed) != 1 || answer.Replayed[0].Outcome != ReplayFailed || !strings.Contains(answer.Replayed[0].Error, row.want) || answer.Remaining != 1 {
				t.Fatalf("the replay answered %+v, want it failed with an error holding %q and the record remaining", answer, row.want)
			}
			kept := replayedRecord(t, pool, given.ID)
			if !kept.ClosedAt.IsZero() || !kept.ReplayClaimedAt.IsZero() || kept.LastError != answer.Replayed[0].Error {
				t.Errorf("the record = %+v, want it open with no claim and the replay's error %q", kept, answer.Replayed[0].Error)
			}
		})
	}
}

// A bulk replay takes a consumer's open records at or below a stream sequence in stream order,
// whatever order their ids are in, goes on past one that fails, which stays open, and leaves one
// above the sequence untouched. It takes at most a hundred a call and says how many open records
// of its selector are left.
func TestABulkReplayAppliesAConsumersRecordsInStreamOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dispatch := dispatchConsumerName("CAPTURE")
	t.Run("five records, the third failing, and one above the sequence", func(t *testing.T) {
		pool := migratedPool(t)
		ids := map[uint64]int64{}
		for _, seq := range []uint64{50, 30, 10, 40, 20, 60} {
			key := fmt.Sprintf("CAPTURE-%d", seq)
			ids[seq] = storedGiveUp(t, pool, dispatch, replayIssueSubject(key), replayIssueEnvelope(t, key), seq, 21).ID
		}
		handler := &keysHandler{fail: map[string]bool{"CAPTURE-30": true}}
		answer, err := giveUpsOf(t, pool, replaySpec(t, &lockedBuffer{}), handler).Replay(ctx, record.IntakeSelector{Consumer: dispatch, ThroughSeq: 50}, "operator")
		if err != nil {
			t.Fatalf("replay %s through 50: %v", dispatch, err)
		}
		var outcomes []string
		for _, replayed := range answer.Replayed {
			outcomes = append(outcomes, fmt.Sprintf("%d:%d %s", replayed.StreamSeq, replayed.ID, replayed.Outcome))
		}
		want := []string{fmt.Sprintf("10:%d applied", ids[10]), fmt.Sprintf("20:%d applied", ids[20]), fmt.Sprintf("30:%d failed", ids[30]),
			fmt.Sprintf("40:%d applied", ids[40]), fmt.Sprintf("50:%d applied", ids[50])}
		if !slices.Equal(outcomes, want) || answer.Remaining != 1 {
			t.Fatalf("the replay answered %v with %d remaining, want %v and the failed one remaining", outcomes, answer.Remaining, want)
		}
		if handled := handler.handled(); !slices.Equal(handled, []string{"CAPTURE-10", "CAPTURE-20", "CAPTURE-30", "CAPTURE-40", "CAPTURE-50"}) {
			t.Errorf("the facts were handled in the order %v, want stream order", handled)
		}
		if failed := replayedRecord(t, pool, ids[30]); !failed.ClosedAt.IsZero() || failed.LastError != "CAPTURE-30 still fails" {
			t.Errorf("the failed record = %+v, want it open with its new error", failed)
		}
		if above := replayedRecord(t, pool, ids[60]); !above.ClosedAt.IsZero() || !above.ReplayClaimedAt.IsZero() {
			t.Errorf("the record above the sequence = %+v, want it open and never claimed", above)
		}
	})
	t.Run("a hundred and one records", func(t *testing.T) {
		pool := migratedPool(t)
		for seq := uint64(1); seq <= replayBatch+1; seq++ {
			key := fmt.Sprintf("CAPTURE-%d", seq)
			storedGiveUp(t, pool, dispatch, replayIssueSubject(key), replayIssueEnvelope(t, key), seq, 21)
		}
		answer, err := giveUpsOf(t, pool, replaySpec(t, &lockedBuffer{}), &keysHandler{}).Replay(ctx, record.IntakeSelector{Consumer: dispatch, ThroughSeq: 1000}, "operator")
		if err != nil {
			t.Fatalf("replay %s through 1000: %v", dispatch, err)
		}
		if len(answer.Replayed) != replayBatch || answer.Remaining != 1 {
			t.Fatalf("the replay took %d records with %d remaining, want %d and 1", len(answer.Replayed), answer.Remaining, replayBatch)
		}
		if last := answer.Replayed[replayBatch-1]; last.StreamSeq != replayBatch || last.Outcome != ReplayApplied {
			t.Fatalf("the last record replayed = %+v, want stream sequence %d applied, the 101st left for the next call", last, replayBatch)
		}
	})
}

// A record a replay running now has claimed is refused to a second replay of its id and to a
// discard of its id, and a bulk discard passes over it; once that replay ends, the record is the
// first replay's. A claim older than HandlerLimit, a replay that stopped without ending, holds
// nothing: a replay takes the record again.
func TestARecordAReplayClaimsIsNeitherReplayedAgainNorDiscarded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := migratedPool(t)
	dispatch := dispatchConsumerName("CAPTURE")
	held := storedGiveUp(t, pool, dispatch, replayIssueSubject("CAPTURE-10"), replayIssueEnvelope(t, "CAPTURE-10"), 10, 21)
	stale := storedGiveUp(t, pool, dispatch, replayIssueSubject("CAPTURE-20"), replayIssueEnvelope(t, "CAPTURE-20"), 20, 21)
	entered, release := make(chan struct{}), make(chan struct{})
	handler := handlerFunc(func(_ context.Context, _ pgx.Tx, fact Fact) (Result, error) {
		if issue, _ := fact.(DispatchIssue); issue.Key == "CAPTURE-10" {
			close(entered)
			<-release
		}
		return Result{}, nil
	})
	giveUps := giveUpsOf(t, pool, replaySpec(t, &lockedBuffer{}), handler)
	limit := giveUps.spec.HandlerLimit

	type replayed struct {
		answer ReplayAnswer
		err    error
	}
	running := make(chan replayed, 1)
	go func() {
		answer, err := giveUps.Replay(ctx, record.IntakeSelector{ID: held.ID}, "operator")
		running <- replayed{answer, err}
	}()
	<-entered
	if _, err := giveUps.Replay(ctx, record.IntakeSelector{ID: held.ID}, "operator"); !errors.Is(err, record.ErrIntakeGiveUpBeingReplayed) {
		t.Errorf("a second replay of the claimed record answered %v, want %v", err, record.ErrIntakeGiveUpBeingReplayed)
	}
	if _, err := giveUps.Discard(ctx, record.IntakeSelector{ID: held.ID}, "operator", "it can never apply"); !errors.Is(err, record.ErrIntakeGiveUpBeingReplayed) {
		t.Errorf("a discard of the claimed record answered %v, want %v", err, record.ErrIntakeGiveUpBeingReplayed)
	}
	if ids, err := giveUps.Discard(ctx, record.IntakeSelector{Consumer: dispatch, ThroughSeq: 10}, "operator", "it can never apply"); err != nil || len(ids) != 0 {
		t.Errorf("a bulk discard over the claimed record closed %v (%v), want none", ids, err)
	}
	close(release)
	if first := <-running; first.err != nil || len(first.answer.Replayed) != 1 || first.answer.Replayed[0].Outcome != ReplayApplied {
		t.Fatalf("the running replay answered %+v (%v), want the record applied", first.answer, first.err)
	}

	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := record.NewStore().ClaimIntakeReplay(ctx, tx, "CAPTURE", record.IntakeSelector{ID: stale.ID}, 1, limit,
			replayNow.Add(-limit-time.Second))
		return err
	}); err != nil {
		t.Fatalf("claim give-up %d as a replay that stopped: %v", stale.ID, err)
	}
	answer, err := giveUps.Replay(ctx, record.IntakeSelector{ID: stale.ID}, "operator")
	if err != nil || len(answer.Replayed) != 1 || answer.Replayed[0].Outcome != ReplayApplied {
		t.Fatalf("a replay of a record under a claim older than HandlerLimit answered %+v (%v), want it taken and applied", answer, err)
	}
}

// beginCountingPool counts the transactions it begins, which a refusal made before any statement
// begins none of.
type beginCountingPool struct {
	*pgxpool.Pool
	begun atomic.Int32
}

func (p *beginCountingPool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	p.begun.Add(1)
	return p.Pool.BeginTx(ctx, options)
}

// A discard names its reason: none, a blank one and one longer than 2,048 characters are refused
// before any statement, and the record stays open. One within the bound closes the record as
// discarded, keeping who discarded it, when and why, applies nothing, keeps the row and answers
// its id.
func TestADiscardNeedsAReasonAndClosesTheRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := migratedPool(t)
	pool := &beginCountingPool{Pool: raw}
	handler := &keysHandler{}
	giveUps := giveUpsOf(t, pool, replaySpec(t, &lockedBuffer{}), handler)
	given := storedGiveUp(t, raw, dispatchConsumerName("CAPTURE"), replayIssueSubject("CAPTURE-3"), replayIssueEnvelope(t, "CAPTURE-3"), 7, 21)
	selector := record.IntakeSelector{ID: given.ID}

	for _, reason := range []string{"", " \n\t", strings.Repeat("é", discardReasonRunes+1)} {
		ids, err := giveUps.Discard(ctx, selector, "operator", reason)
		var refused *RequestError
		if !errors.As(err, &refused) || ids != nil {
			t.Errorf("a discard for a reason of %d characters answered %v (%v), want a refusal", len([]rune(reason)), ids, err)
		}
	}
	if begun := pool.begun.Load(); begun != 0 {
		t.Errorf("the refused discards began %d transactions, want none", begun)
	}
	if open := replayedRecord(t, raw, given.ID); !open.ClosedAt.IsZero() {
		t.Fatalf("the record = %+v after the refused discards, want it open", open)
	}

	reason := strings.Repeat("é", discardReasonRunes)
	ids, err := giveUps.Discard(ctx, selector, "operator", reason)
	if err != nil || !slices.Equal(ids, []int64{given.ID}) {
		t.Fatalf("the discard answered %v (%v), want the record's id", ids, err)
	}
	discarded := replayedRecord(t, raw, given.ID)
	if discarded.ClosedHow != record.IntakeDiscarded || discarded.ClosedBy != "operator" || discarded.ClosedAt != replayNow ||
		discarded.DiscardedBy != "operator" || discarded.DiscardedAt != replayNow || discarded.DiscardReason != reason {
		t.Errorf("the record = %+v, want it discarded by the operator at %s for the reason given", discarded, replayNow)
	}
	if handled := handler.handled(); len(handled) != 0 {
		t.Errorf("the discard handed the handler %v, want nothing applied", handled)
	}
	if all, _ := processedFacts(t, raw, "", ""); all != 0 {
		t.Errorf("processed_events holds %d facts after the discard, want none", all)
	}
}

// A bulk discard closes every open record of one consumer at or below a stream sequence, and
// answers their ids in stream order. It leaves the consumer's records above the sequence, another
// consumer's, and a record closed already, whose discard stays the one it had.
func TestABulkDiscardClosesAConsumersOpenRecordsAtOrBelowASequence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := migratedPool(t)
	dispatch, github := dispatchConsumerName("CAPTURE"), githubConsumerName("CAPTURE")
	giveUps := giveUpsOf(t, pool, replaySpec(t, &lockedBuffer{}), &keysHandler{})
	ids := map[uint64]int64{}
	for _, seq := range []uint64{20, 10, 30, 5} {
		key := fmt.Sprintf("CAPTURE-%d", seq)
		ids[seq] = storedGiveUp(t, pool, dispatch, replayIssueSubject(key), replayIssueEnvelope(t, key), seq, 21).ID
	}
	other := storedGiveUp(t, pool, github, "notifications.github.sjawhar.legion.pr.42.review", capturedGitHubEnvelope(t, "review.json"), 15, 21)
	if _, err := giveUps.Discard(ctx, record.IntakeSelector{ID: ids[5]}, "operator", "the first discard"); err != nil {
		t.Fatalf("discard give-up %d: %v", ids[5], err)
	}

	closed, err := giveUps.Discard(ctx, record.IntakeSelector{Consumer: dispatch, ThroughSeq: 20}, "controller session ses-controller", "GitHub sent events the listener should not publish")
	if err != nil || !slices.Equal(closed, []int64{ids[10], ids[20]}) {
		t.Fatalf("the bulk discard closed %v (%v), want %v, in stream order", closed, err, []int64{ids[10], ids[20]})
	}
	for _, seq := range []uint64{10, 20} {
		if discarded := replayedRecord(t, pool, ids[seq]); discarded.ClosedHow != record.IntakeDiscarded || discarded.DiscardedBy != "controller session ses-controller" {
			t.Errorf("record %d = %+v, want it discarded by the controller's session", seq, discarded)
		}
	}
	for name, id := range map[string]int64{"the record above the sequence": ids[30], "another consumer's record": other.ID} {
		if open := replayedRecord(t, pool, id); !open.ClosedAt.IsZero() {
			t.Errorf("%s = %+v, want it open", name, open)
		}
	}
	if first := replayedRecord(t, pool, ids[5]); first.DiscardReason != "the first discard" || first.DiscardedBy != "operator" {
		t.Errorf("the record discarded before = %+v, want its first discard kept", first)
	}
}

// A discard can be undone: a replay of a discarded record by its id that applies it marks it
// replayed and keeps the discard's time, caller and reason; one that fails leaves it discarded,
// keeping the new error, and counts it among no open records; and a bulk replay takes no discarded
// record.
func TestADiscardedRecordCanBeReplayedAndKeepsItsDiscard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := migratedPool(t)
	dispatch := dispatchConsumerName("CAPTURE")
	handler := &keysHandler{fail: map[string]bool{"CAPTURE-20": true}}
	giveUps := giveUpsOf(t, pool, replaySpec(t, &lockedBuffer{}), handler)
	ids := map[uint64]int64{}
	for _, seq := range []uint64{10, 20, 30} {
		key := fmt.Sprintf("CAPTURE-%d", seq)
		ids[seq] = storedGiveUp(t, pool, dispatch, replayIssueSubject(key), replayIssueEnvelope(t, key), seq, 21).ID
	}
	if closed, err := giveUps.Discard(ctx, record.IntakeSelector{Consumer: dispatch, ThroughSeq: 30}, "operator", "a wrong discard"); err != nil || len(closed) != 3 {
		t.Fatalf("the discard closed %v (%v), want the three records", closed, err)
	}

	if answer, err := giveUps.Replay(ctx, record.IntakeSelector{Consumer: dispatch, ThroughSeq: 30}, "operator"); err != nil || len(answer.Replayed) != 0 || answer.Remaining != 0 {
		t.Errorf("a bulk replay over discarded records answered %+v (%v), want none taken and none remaining", answer, err)
	}

	answer, err := giveUps.Replay(ctx, record.IntakeSelector{ID: ids[10]}, "controller session ses-controller")
	if err != nil || len(answer.Replayed) != 1 || answer.Replayed[0].Outcome != ReplayApplied {
		t.Fatalf("the replay of a discarded record answered %+v (%v), want it applied", answer, err)
	}
	replayed := replayedRecord(t, pool, ids[10])
	if replayed.ClosedHow != record.IntakeApplied || replayed.ClosedBy != "controller session ses-controller" ||
		replayed.DiscardedAt != replayNow || replayed.DiscardedBy != "operator" || replayed.DiscardReason != "a wrong discard" {
		t.Errorf("the replayed record = %+v, want it closed as applied by the controller's session with its discard kept", replayed)
	}

	answer, err = giveUps.Replay(ctx, record.IntakeSelector{ID: ids[20]}, "operator")
	if err != nil || len(answer.Replayed) != 1 || answer.Replayed[0].Outcome != ReplayFailed || answer.Remaining != 0 {
		t.Fatalf("the failing replay of a discarded record answered %+v (%v), want it failed with none remaining", answer, err)
	}
	failed := replayedRecord(t, pool, ids[20])
	if failed.ClosedHow != record.IntakeDiscarded || failed.DiscardReason != "a wrong discard" || failed.LastError != "CAPTURE-20 still fails" || !failed.ReplayClaimedAt.IsZero() {
		t.Errorf("the record whose replay failed = %+v, want it discarded still, with the new error and no claim", failed)
	}
	if handled := handler.handled(); !slices.Equal(handled, []string{"CAPTURE-10", "CAPTURE-20"}) {
		t.Errorf("the handler was handed %v, want the two replays by id alone", handled)
	}
}

// A replay of a record a replay closed, a discard of it, and a discard of a discarded record are
// refused, naming how the record closed and when.
func TestAClosedRecordIsRefusedWithHowAndWhenItClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := migratedPool(t)
	dispatch := dispatchConsumerName("CAPTURE")
	giveUps := giveUpsOf(t, pool, replaySpec(t, &lockedBuffer{}), &keysHandler{})
	replayed := storedGiveUp(t, pool, dispatch, replayIssueSubject("CAPTURE-10"), replayIssueEnvelope(t, "CAPTURE-10"), 10, 21)
	discarded := storedGiveUp(t, pool, dispatch, replayIssueSubject("CAPTURE-20"), replayIssueEnvelope(t, "CAPTURE-20"), 20, 21)
	if answer, err := giveUps.Replay(ctx, record.IntakeSelector{ID: replayed.ID}, "operator"); err != nil || answer.Replayed[0].Outcome != ReplayApplied {
		t.Fatalf("replay give-up %d answered %+v (%v), want it applied", replayed.ID, answer, err)
	}
	if _, err := giveUps.Discard(ctx, record.IntakeSelector{ID: discarded.ID}, "operator", "it can never apply"); err != nil {
		t.Fatalf("discard give-up %d: %v", discarded.ID, err)
	}

	for _, row := range []struct {
		name string
		act  func() error
		id   int64
		how  record.IntakeClosedHow
	}{
		{name: "a replay of a replayed record", id: replayed.ID, how: record.IntakeApplied, act: func() error {
			_, err := giveUps.Replay(ctx, record.IntakeSelector{ID: replayed.ID}, "operator")
			return err
		}},
		{name: "a discard of a replayed record", id: replayed.ID, how: record.IntakeApplied, act: func() error {
			_, err := giveUps.Discard(ctx, record.IntakeSelector{ID: replayed.ID}, "operator", "too late")
			return err
		}},
		{name: "a discard of a discarded record", id: discarded.ID, how: record.IntakeDiscarded, act: func() error {
			_, err := giveUps.Discard(ctx, record.IntakeSelector{ID: discarded.ID}, "operator", "again")
			return err
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			err := row.act()
			var closed *record.IntakeGiveUpClosedError
			if !errors.As(err, &closed) || closed.ID != row.id || closed.How != row.how || !closed.At.Equal(replayNow) {
				t.Fatalf("answered %v, want give-up %d refused as closed %s at %s", err, row.id, row.how, replayNow)
			}
			if text := err.Error(); !strings.Contains(text, string(row.how)) || !strings.Contains(text, replayNow.Format(time.RFC3339)) {
				t.Errorf("the refusal says %q, want how and when the record closed", text)
			}
		})
	}
}
