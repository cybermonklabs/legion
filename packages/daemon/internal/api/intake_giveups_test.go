package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// giveUpsNow is the clock of the give-up route tests: every give-up is recorded before it, and
// every replay and discard closes its records at it.
var giveUpsNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// giveUpConsumer is the Dispatch durable of the test project, which delivered every give-up here.
const giveUpConsumer = "legion-go-" + testDispatchProject + "-dispatch"

// controllerSession is the session the test project's controller registers as.
const controllerSession = "ses_controller"

// serveGiveUps has h serve the give-up routes over giveUps, beside the grants, the controller
// secret and the claim registration routes a door's caller is authenticated through.
func serveGiveUps(h *harness, giveUps GiveUps) {
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, DispatchProject: testDispatchProject,
		OperatorToken: testOperatorToken, Controller: h.store, Grants: credential.New(nil), Intake: giveUps,
		Log: slog.New(slog.NewTextHandler(h.serverLog, nil)),
	}).Handler
}

// workflowGiveUps is the real replay and discard of the test project's give-ups on h's database,
// applying with the daemon's own handler chain (daemon/workflow.go: the mirror, the engine,
// admission) and a stand-in for the preparation of an issue's repository, which a live delivery
// of a Dispatch issue event runs and which counts its calls in prepared.
func workflowGiveUps(t *testing.T, h *harness) (giveUps *intake.GiveUps, prepared *atomic.Int32) {
	t.Helper()
	records := record.NewStore()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(records, workflow.Config{Project: testDispatchProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quiet)
	prepared = &atomic.Int32{}
	prepare := func(_ context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error) {
		prepared.Add(1)
		return issue, nil
	}
	return giveUpsApplying(t, h, prepare, blockers.NewMirror(records, testDispatchProject), engine,
		admit.New(records, engine, 2, testDispatchProject, quiet)), prepared
}

// giveUpsApplying is the real replay and discard of the test project's give-ups on h's database at
// giveUpsNow, applying with handlers.
func giveUpsApplying(t *testing.T, h *harness, prepare func(context.Context, intake.DispatchIssue) (intake.DispatchIssue, error),
	handlers ...intake.Handler) *intake.GiveUps {
	t.Helper()
	spec := intake.ConsumerSpec{Project: testDispatchProject, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return giveUpsNow }, PrepareRepository: prepare}
	giveUps, err := intake.NewGiveUps(intake.Records{Pool: h.store.Pool(), Spec: spec, Repositories: func() []ghrepo.Repository { return spec.Repositories }},
		handlers...)
	if err != nil {
		t.Fatalf("NewGiveUps: %v", err)
	}
	return giveUps
}

// giveUpEnvelope is a Dispatch issue event of key in backlog, under the event id dispatch-<key>,
// as Envoy stores it on the stream.
func giveUpEnvelope(t *testing.T, key string) []byte {
	t.Helper()
	event, err := json.Marshal(map[string]any{"id": 30, "issue_key": key, "seq": 3, "type": "issue.updated", "notify": true,
		"actor": map[string]string{"kind": "user", "id": "a-person"},
		"payload": map[string]any{"key": key, "status": "backlog", "title": "an event given up", "rank": "A", "parent": nil,
			"labels": []string{}, "resolved_repo": "acme/widgets"}})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{"event_id": "dispatch-" + key, "source": "dispatch", "source_event_id": "30",
		"topic": "issue", "dedupe_key": "dispatch-" + key, "issued_at": 1, "payload_summary": key + " issue updated",
		"payload": string(event), "trace_id": "dispatch-" + key})
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

// giveUpMessage is the Dispatch durable's message of key's issue event at stream sequence seq, as
// project's daemon records it.
func giveUpMessage(t *testing.T, project, key string, seq uint64) record.IntakeMessage {
	t.Helper()
	return record.IntakeMessage{Project: project, Consumer: "legion-go-" + project + "-dispatch",
		Subject: "notifications.dispatch.issue." + key + ".issue.updated", StreamSeq: seq, StoredAt: giveUpsNow.Add(-3 * time.Hour),
		Payload: giveUpEnvelope(t, key), EventSource: "dispatch", EventID: "dispatch-" + key}
}

// givenUp records message as the intake records a decoded message it gives up at its bound
// (NoteFailure, then GiveUpIntakeMessage), and answers the record.
func givenUp(t *testing.T, h *harness, message record.IntakeMessage) record.IntakeFailure {
	t.Helper()
	ctx := context.Background()
	var given record.IntakeFailure
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		failure, err := records.NoteFailure(ctx, tx, message, 21, "the handler failed", giveUpsNow.Add(-2*time.Hour))
		if err != nil {
			return err
		}
		var gaveUp bool
		given, gaveUp, err = records.GiveUpIntakeMessage(ctx, tx, message.Project, failure.ID, record.GiveUpBound, "the handler failed", giveUpsNow.Add(-time.Hour))
		if err == nil && !gaveUp {
			err = fmt.Errorf("message %d had no open failure", message.StreamSeq)
		}
		return err
	}); err != nil {
		t.Fatalf("give up %s's message %d: %v", message.Consumer, message.StreamSeq, err)
	}
	return given
}

// givenUpUndecodable records a message of the test project's Dispatch durable at seq that does not
// decode, as the intake records one at its first delivery (RecordUndecodable).
func givenUpUndecodable(t *testing.T, h *harness, seq uint64) record.IntakeFailure {
	t.Helper()
	ctx := context.Background()
	message := record.IntakeMessage{Project: testDispatchProject, Consumer: giveUpConsumer,
		Subject: "notifications.dispatch.issue.LEGION-999.issue.updated", StreamSeq: seq, StoredAt: giveUpsNow.Add(-3 * time.Hour), Payload: []byte(`{`)}
	var given record.IntakeFailure
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		given, _, err = record.NewStore().RecordUndecodable(ctx, tx, message, 1, "an earlier decoder's error", giveUpsNow.Add(-time.Hour))
		return err
	}); err != nil {
		t.Fatalf("record the undecodable message %d: %v", seq, err)
	}
	return given
}

// giveUpRecord is give-up id as the table keeps it after a replay or a discard, read through the
// record's own read (record.Postgres.IntakeRecords): its error, its claim, its latest close and its
// discard are what the tests here read of it.
func giveUpRecord(t *testing.T, h *harness, id int64) record.IntakeFailure {
	t.Helper()
	ctx := context.Background()
	var records []record.IntakeFailure
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		records, err = record.NewStore().IntakeRecords(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("read give-up %d: %v", id, err)
	}
	for _, f := range records {
		if f.ID == id {
			return f
		}
	}
	t.Fatalf("read give-up %d: the table holds no such record", id)
	return record.IntakeFailure{}
}

// processedEvents is how many facts processed_events holds of eventID.
func processedEvents(t *testing.T, h *harness, eventID string) int {
	t.Helper()
	var count int
	if err := h.store.Pool().QueryRow(context.Background(), `select count(*) from processed_events where event_id = $1`, eventID).Scan(&count); err != nil {
		t.Fatalf("read processed_events: %v", err)
	}
	return count
}

// giveUpDoor is one door onto the give-ups: the operator's, with the operator's bearer, or the
// controller's, with a grant its registered session minted; and who the record keeps as having
// closed a record through it.
type giveUpDoor struct {
	caller  string
	replay  func(selection IntakeSelection) *httptest.ResponseRecorder
	discard func(selection IntakeSelection, reason string) *httptest.ResponseRecorder
}

// giveUpDoorNames are the two doors every [both doors] row runs through.
var giveUpDoorNames = []string{"operator", "controller"}

// giveUpDoorOf is the door named, on h, which serves the give-up routes already.
func giveUpDoorOf(t *testing.T, h *harness, name string) giveUpDoor {
	t.Helper()
	if name == "operator" {
		return giveUpDoor{caller: "operator",
			replay: func(selection IntakeSelection) *httptest.ResponseRecorder {
				return h.operator(http.MethodPost, "/legion/v1/operator/intake/replay", OperatorIntakeReplayRequest{selection})
			},
			discard: func(selection IntakeSelection, reason string) *httptest.ResponseRecorder {
				return h.operator(http.MethodPost, "/legion/v1/operator/intake/discard", OperatorIntakeDiscardRequest{selection, reason})
			}}
	}
	return controllerDoor(t, h, h.controllerSession(), "controller session "+controllerSession)
}

// controllerDoor is the controller's door with grant, whose caller the record keeps as caller.
func controllerDoor(t *testing.T, h *harness, grant, caller string) giveUpDoor {
	t.Helper()
	return giveUpDoor{caller: caller,
		replay: func(selection IntakeSelection) *httptest.ResponseRecorder {
			return h.request(http.MethodPost, "/legion/v1/intake/replay", IntakeReplayRequest{GrantID: grant, IntakeSelection: selection}, nil)
		},
		discard: func(selection IntakeSelection, reason string) *httptest.ResponseRecorder {
			return h.request(http.MethodPost, "/legion/v1/intake/discard",
				IntakeDiscardRequest{GrantID: grant, IntakeSelection: selection, Reason: reason}, nil)
		}}
}

// replayAnswer is a replay's 200 answer.
func replayAnswer(t *testing.T, recorder *httptest.ResponseRecorder) IntakeReplayResponse {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("replay = %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var answer IntakeReplayResponse
	decodeInto(t, recorder, &answer)
	return answer
}

// discardAnswer is a discard's 200 answer.
func discardAnswer(t *testing.T, recorder *httptest.ResponseRecorder) IntakeDiscardResponse {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("discard = %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var answer IntakeDiscardResponse
	decodeInto(t, recorder, &answer)
	return answer
}

// A give-up's id replays it through either door, with the daemon's own handlers: a decoded event
// applies once through the live handling, its issue prepared as a delivery prepares it; a second
// record of the same event answers already applied; a message that holds no fact of this project
// answers no fact; and a message that still does not decode answers failed with its new error.
// The first three close their records as their outcome, by the door's caller, at the replay's
// time; the failed one stays open, keeping the error.
func TestTheGiveUpRoutesReplayARecordThroughBothDoors(t *testing.T) {
	for _, name := range giveUpDoorNames {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			giveUps, prepared := workflowGiveUps(t, h)
			serveGiveUps(h, giveUps)
			door := giveUpDoorOf(t, h, name)
			applied := givenUp(t, h, giveUpMessage(t, testDispatchProject, "LEGION-501", 11))
			again := givenUp(t, h, giveUpMessage(t, testDispatchProject, "LEGION-501", 12))
			noFact := givenUp(t, h, giveUpMessage(t, testDispatchProject, "OTHER-3", 13))
			failed := givenUpUndecodable(t, h, 14)

			for _, row := range []struct {
				name      string
				given     record.IntakeFailure
				outcome   intake.ReplayOutcome
				remaining int
			}{
				{"applied", applied, intake.ReplayApplied, 0},
				{"already applied", again, intake.ReplayAlreadyApplied, 0},
				{"no fact", noFact, intake.ReplayNoFact, 0},
				{"failed", failed, intake.ReplayFailed, 1},
			} {
				answer := replayAnswer(t, door.replay(IntakeSelection{GiveUp: row.given.ID}))
				if len(answer.Replayed) != 1 || answer.Remaining != row.remaining {
					t.Fatalf("%s: the replay answered %+v, want one record and %d remaining", row.name, answer, row.remaining)
				}
				got := answer.Replayed[0]
				if got.ID != row.given.ID || got.Consumer != giveUpConsumer || got.StreamSeq != row.given.StreamSeq || got.Outcome != row.outcome {
					t.Fatalf("%s: the replay answered %+v, want record %d at %d %s", row.name, got, row.given.ID, row.given.StreamSeq, row.outcome)
				}
				kept := giveUpRecord(t, h, row.given.ID)
				if row.outcome == intake.ReplayFailed {
					if !strings.Contains(got.Error, "decode Envoy envelope") || !kept.ClosedAt.IsZero() || kept.LastError != got.Error {
						t.Errorf("%s: answered %+v and kept %+v, want it open with the new decode error", row.name, got, kept)
					}
					continue
				}
				if got.Error != "" || kept.ClosedHow != record.IntakeClosedHow(row.outcome) || kept.ClosedBy != door.caller ||
					!kept.ClosedAt.Equal(giveUpsNow) || !kept.ReplayClaimedAt.IsZero() {
					t.Errorf("%s: the record = %+v, want it closed as %s by %q at %s", row.name, kept, row.outcome, door.caller, giveUpsNow)
				}
			}
			if count := processedEvents(t, h, applied.EventID); count != 1 {
				t.Errorf("processed_events holds %s %d times, want once", applied.EventID, count)
			}
			// The decoded event is prepared for each of its two records, as a live delivery of
			// each would be: the duplicate is found in the fact's transaction, after the
			// preparation.
			if calls := prepared.Load(); calls != 2 {
				t.Errorf("the replays prepared an issue %d times, want twice, once for each record of the decoded event", calls)
			}
		})
	}
}

// A consumer and a stream sequence replay every open record of that consumer at or below the
// sequence through either door, in stream order whatever order the records were written in, going
// on past one that fails, which stays open and is counted among what remains; a record above the
// sequence is not taken.
func TestTheGiveUpRoutesReplayABulkSelectorThroughBothDoors(t *testing.T) {
	for _, name := range giveUpDoorNames {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			giveUps, _ := workflowGiveUps(t, h)
			serveGiveUps(h, giveUps)
			door := giveUpDoorOf(t, h, name)
			third := givenUp(t, h, giveUpMessage(t, testDispatchProject, "LEGION-630", 30))
			first := givenUp(t, h, giveUpMessage(t, testDispatchProject, "LEGION-610", 10))
			above := givenUp(t, h, giveUpMessage(t, testDispatchProject, "LEGION-640", 40))
			second := givenUpUndecodable(t, h, 20)

			answer := replayAnswer(t, door.replay(IntakeSelection{Consumer: giveUpConsumer, ThroughSeq: 30}))
			var outcomes []string
			for _, replayed := range answer.Replayed {
				outcomes = append(outcomes, fmt.Sprintf("%d:%d %s", replayed.StreamSeq, replayed.ID, replayed.Outcome))
			}
			want := []string{fmt.Sprintf("10:%d applied", first.ID), fmt.Sprintf("20:%d failed", second.ID), fmt.Sprintf("30:%d applied", third.ID)}
			if !slices.Equal(outcomes, want) || answer.Remaining != 1 {
				t.Fatalf("the replay answered %v with %d remaining, want %v and the failed one remaining", outcomes, answer.Remaining, want)
			}
			for _, id := range []int64{first.ID, third.ID} {
				if kept := giveUpRecord(t, h, id); kept.ClosedHow != record.IntakeApplied || kept.ClosedBy != door.caller || !kept.ClosedAt.Equal(giveUpsNow) {
					t.Errorf("record %d = %+v, want it closed as applied by %q at %s", id, kept, door.caller, giveUpsNow)
				}
			}
			if kept := giveUpRecord(t, h, second.ID); !kept.ClosedAt.IsZero() || !strings.Contains(kept.LastError, "decode Envoy envelope") {
				t.Errorf("the failed record = %+v, want it open with its new error", kept)
			}
			if kept := giveUpRecord(t, h, above.ID); !kept.ClosedAt.IsZero() || !kept.ReplayClaimedAt.IsZero() {
				t.Errorf("the record above the sequence = %+v, want it open and never claimed", kept)
			}
		})
	}
}

// applyFunc is an intake handler that runs a function of the test's own.
type applyFunc func(context.Context, pgx.Tx, intake.Fact) (intake.Result, error)

func (f applyFunc) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	return f(ctx, tx, fact)
}

// A replay runs to its end when its caller hangs up: the client's connection closes while the
// first record is being applied, and the replay still applies and closes every record it took,
// so none is left to wait for its claim to go stale.
func TestAReplayRunsToItsEndWhenItsCallerHangsUp(t *testing.T) {
	h := newHarness(t)
	asking, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	hungUp := make(chan struct{})
	var applied atomic.Int32
	handler := applyFunc(func(context.Context, pgx.Tx, intake.Fact) (intake.Result, error) {
		if applied.Add(1) == 1 {
			hangUp()
			<-hungUp
		}
		return intake.Result{}, nil
	})
	serveGiveUps(h, giveUpsApplying(t, h, nil, handler))
	server := httptest.NewServer(h.handler)
	t.Cleanup(server.Close)
	var ids []int64
	for seq := uint64(1); seq <= 3; seq++ {
		ids = append(ids, givenUp(t, h, giveUpMessage(t, testDispatchProject, fmt.Sprintf("LEGION-70%d", seq), seq)).ID)
	}

	body, err := json.Marshal(OperatorIntakeReplayRequest{IntakeSelection{Consumer: giveUpConsumer, ThroughSeq: 3}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(asking, http.MethodPost, server.URL+"/legion/v1/operator/intake/replay", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testOperatorToken)
	response, err := server.Client().Do(request)
	close(hungUp)
	if err == nil {
		response.Body.Close()
		t.Fatalf("the replay answered %d to a caller that hung up during its first record", response.StatusCode)
	}
	testwait.Eventually(t, "every record the replay took closed as applied", func() bool {
		for _, id := range ids {
			if giveUpRecord(t, h, id).ClosedHow != record.IntakeApplied {
				return false
			}
		}
		return true
	})
	if calls := applied.Load(); calls != 3 {
		t.Errorf("the handler applied %d facts, want the three the replay took", calls)
	}
}

// A discard through either door closes the records it names without applying them, keeping each
// row with the discard's time, caller and reason, and answers the id of each it closed: one by
// its id, and a consumer's open records at or below a stream sequence, passing over one a running
// replay holds and leaving one above the sequence open. It refuses an id it may not take — a
// record already discarded or one a replay closed, naming how and when, and one a running replay
// holds — and a request that names no reason, a reason longer than 2,048 characters, or a
// selection of neither form or both, each before it writes anything.
func TestTheGiveUpRoutesDiscardThroughBothDoors(t *testing.T) {
	const reason = "the event's issue was deleted in Dispatch"
	for _, name := range giveUpDoorNames {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			giveUps, _ := workflowGiveUps(t, h)
			serveGiveUps(h, giveUps)
			door := giveUpDoorOf(t, h, name)
			replayed := givenUp(t, h, giveUpMessage(t, testDispatchProject, "LEGION-805", 5))
			one := givenUp(t, h, giveUpMessage(t, testDispatchProject, "LEGION-810", 10))
			second := givenUp(t, h, giveUpMessage(t, testDispatchProject, "LEGION-820", 20))
			held := givenUp(t, h, giveUpMessage(t, testDispatchProject, "LEGION-825", 25))
			third := givenUp(t, h, giveUpMessage(t, testDispatchProject, "LEGION-830", 30))
			above := givenUp(t, h, giveUpMessage(t, testDispatchProject, "LEGION-840", 40))

			if answer := discardAnswer(t, door.discard(IntakeSelection{GiveUp: one.ID}, reason)); !slices.Equal(answer.Discarded, []int64{one.ID}) {
				t.Fatalf("the discard of %d answered %v", one.ID, answer.Discarded)
			}
			kept := giveUpRecord(t, h, one.ID)
			if kept.ClosedHow != record.IntakeDiscarded || kept.ClosedBy != door.caller || !kept.ClosedAt.Equal(giveUpsNow) ||
				kept.DiscardedBy != door.caller || !kept.DiscardedAt.Equal(giveUpsNow) || kept.DiscardReason != reason {
				t.Fatalf("the discarded record = %+v, want it closed and discarded by %q at %s for %q", kept, door.caller, giveUpsNow, reason)
			}
			if count := processedEvents(t, h, one.EventID); count != 0 {
				t.Errorf("the discard applied %s %d times, want never", one.EventID, count)
			}
			if sentence := (&record.IntakeGiveUpClosedError{ID: one.ID, How: record.IntakeDiscarded, At: giveUpsNow}).Error(); refused(t,
				door.discard(IntakeSelection{GiveUp: one.ID}, reason), http.StatusConflict, giveUpClosed) != sentence {
				t.Errorf("a second discard of %d is not refused saying how and when it closed, %q", one.ID, sentence)
			}
			// A record a replay closed is refused too, as applied, at the replay's time.
			if answer := replayAnswer(t, door.replay(IntakeSelection{GiveUp: replayed.ID})); answer.Replayed[0].Outcome != intake.ReplayApplied {
				t.Fatalf("the replay of %d answered %+v, want it applied", replayed.ID, answer)
			}
			if sentence := (&record.IntakeGiveUpClosedError{ID: replayed.ID, How: record.IntakeApplied, At: giveUpsNow}).Error(); refused(t,
				door.discard(IntakeSelection{GiveUp: replayed.ID}, reason), http.StatusConflict, giveUpClosed) != sentence {
				t.Errorf("the discard of a replayed record is not refused saying %q", sentence)
			}
			// A replay running now holds one record: its claim is the one a replay takes first.
			if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
				_, err := record.NewStore().ClaimIntakeReplay(ctx, tx, testDispatchProject, record.IntakeSelector{ID: held.ID},
					1, time.Hour, giveUpsNow)
				return err
			}); err != nil {
				t.Fatalf("claim %d for a replay: %v", held.ID, err)
			}
			refused(t, door.discard(IntakeSelection{GiveUp: held.ID}, reason), http.StatusConflict, giveUpBeingReplayed)

			for _, invalid := range []struct {
				name      string
				selection IntakeSelection
				reason    string
			}{
				{"no reason", IntakeSelection{GiveUp: above.ID}, ""},
				{"a blank reason", IntakeSelection{GiveUp: above.ID}, "   "},
				{"a reason longer than 2,048 characters", IntakeSelection{GiveUp: above.ID}, strings.Repeat("é", 2049)},
				{"a selection of neither form", IntakeSelection{}, reason},
				{"a selection of both forms", IntakeSelection{GiveUp: above.ID, Consumer: giveUpConsumer, ThroughSeq: 40}, reason},
			} {
				t.Run(invalid.name, func(t *testing.T) {
					refused(t, door.discard(invalid.selection, invalid.reason), http.StatusBadRequest, giveUpRequestInvalid)
				})
			}

			bulk := discardAnswer(t, door.discard(IntakeSelection{Consumer: giveUpConsumer, ThroughSeq: 30}, reason))
			if want := []int64{second.ID, third.ID}; !slices.Equal(bulk.Discarded, want) {
				t.Fatalf("the bulk discard through 30 answered %v, want %v: the open records at or below it no replay holds, in stream order",
					bulk.Discarded, want)
			}
			for _, open := range []record.IntakeFailure{held, above} {
				if kept := giveUpRecord(t, h, open.ID); !kept.ClosedAt.IsZero() {
					t.Errorf("record %d at %d = %+v, want it open", open.ID, open.StreamSeq, kept)
				}
			}
		})
	}
}

// countingGiveUps is a stand-in for the replay and the discard that records each call and does
// nothing, for the rows that must reach neither.
type countingGiveUps struct {
	mu    sync.Mutex
	calls []string
}

func (g *countingGiveUps) Replay(_ context.Context, selector record.IntakeSelector, caller string) (intake.ReplayAnswer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, fmt.Sprintf("replay %+v by %s", selector, caller))
	return intake.ReplayAnswer{}, nil
}

func (g *countingGiveUps) Discard(_ context.Context, selector record.IntakeSelector, caller, reason string) ([]int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, fmt.Sprintf("discard %+v by %s for %s", selector, caller, reason))
	return nil, nil
}

func (g *countingGiveUps) made() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

// The give-up routes admit the operator, by its bearer, and the controller, by its grant, and no
// one else: a request with no bearer or the wrong one at the operator's door, and at the
// controller's a request with no grant, a grant the daemon never minted, a phase worker's grant
// and an architect's, is refused before the replay or the discard is called.
func TestTheGiveUpRoutesRefuseEveryCallerButTheOperatorAndTheController(t *testing.T) {
	h := newHarness(t)
	stand := &countingGiveUps{}
	serveGiveUps(h, stand)
	worker := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer).grant(t)
	architect := newLiveClaim(t, h, "LEGION-210", claim.RoleArchitect).grant(t)
	selection := IntakeSelection{GiveUp: 41}
	// Joined to its scheme, as the other operator tests here join theirs: the push's leak check reads
	// a scheme and a token written as one literal as a credential.
	wrongToken := "not-the-operator-token"
	wrongBearer := http.Header{"Authorization": {"Bearer " + wrongToken}}

	for _, route := range []struct {
		name                 string
		operator, controller string
		operatorBody         any
		withGrant            func(grant string) any
	}{
		{"replay", "/legion/v1/operator/intake/replay", "/legion/v1/intake/replay", OperatorIntakeReplayRequest{selection},
			func(grant string) any { return IntakeReplayRequest{GrantID: grant, IntakeSelection: selection} }},
		{"discard", "/legion/v1/operator/intake/discard", "/legion/v1/intake/discard", OperatorIntakeDiscardRequest{selection, "a reason"},
			func(grant string) any {
				return IntakeDiscardRequest{GrantID: grant, IntakeSelection: selection, Reason: "a reason"}
			}},
	} {
		t.Run(route.name, func(t *testing.T) {
			for _, row := range []struct {
				name    string
				request func() *httptest.ResponseRecorder
				status  int
				code    string
			}{
				{"no bearer", func() *httptest.ResponseRecorder {
					return h.request(http.MethodPost, route.operator, route.operatorBody, nil)
				}, http.StatusForbidden, ""},
				{"the wrong bearer", func() *httptest.ResponseRecorder {
					return h.request(http.MethodPost, route.operator, route.operatorBody, wrongBearer)
				}, http.StatusForbidden, ""},
				{"no grant", func() *httptest.ResponseRecorder {
					return h.request(http.MethodPost, route.controller, route.withGrant(""), nil)
				}, http.StatusBadRequest, "MISSING_FIELD"},
				{"a grant the daemon never minted", func() *httptest.ResponseRecorder {
					return h.request(http.MethodPost, route.controller, route.withGrant("never-minted"), nil)
				}, http.StatusForbidden, "GRANT_UNAVAILABLE"},
				{"a phase worker's grant", func() *httptest.ResponseRecorder {
					return h.request(http.MethodPost, route.controller, route.withGrant(worker), nil)
				}, http.StatusForbidden, "CONTROLLER_REQUIRED"},
				{"an architect's grant", func() *httptest.ResponseRecorder {
					return h.request(http.MethodPost, route.controller, route.withGrant(architect), nil)
				}, http.StatusForbidden, "CONTROLLER_REQUIRED"},
			} {
				t.Run(row.name, func(t *testing.T) {
					recorder := row.request()
					if row.code == "" {
						wantRefusal(t, recorder, row.status, invalidOperatorToken)
					} else {
						refused(t, recorder, row.status, row.code)
					}
					if calls := stand.made(); len(calls) != 0 {
						t.Fatalf("the refused request reached the give-ups: %v", calls)
					}
				})
			}
		})
	}
	// The stand-in is reachable: the operator's bearer is let through to it.
	discardAnswer(t, h.operator(http.MethodPost, "/legion/v1/operator/intake/discard", OperatorIntakeDiscardRequest{selection, "a reason"}))
	if calls := stand.made(); !slices.Equal(calls, []string{"discard {ID:41 Consumer: ThroughSeq:0} by operator for a reason"}) {
		t.Fatalf("the operator's discard reached the give-ups as %v", calls)
	}
}

// A daemon that runs no intake keeps no give-ups, so the replay and the discard answer 503
// NO_INTAKE through either door; a daemon whose stop has begun refuses both through either door
// with 503, naming the stop; and neither reaches the replay or the discard.
func TestTheGiveUpRoutesRefuseADaemonWithNoIntakeAndADaemonThatIsStopping(t *testing.T) {
	h := newHarness(t)
	stand := &countingGiveUps{}
	// One grant service for every server below, so the controller's grant redeems at each.
	grants := credential.New(nil)
	serve := func(giveUps GiveUps, stopping context.Context) {
		h.handler = NewServer("127.0.0.1", 8437, Options{
			Steps:      config.DesignGateRootIssues.Steps(),
			Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, DispatchProject: testDispatchProject,
			OperatorToken: testOperatorToken, Controller: h.store, Grants: grants, Intake: giveUps, Stopping: stopping,
			Log: slog.New(slog.NewTextHandler(h.serverLog, nil)),
		}).Handler
	}
	serve(stand, context.Background())
	doors := map[string]giveUpDoor{
		"operator":   giveUpDoorOf(t, h, "operator"),
		"controller": controllerDoor(t, h, h.controllerSession(), "controller session "+controllerSession),
	}
	stopping, beginStop := context.WithCancel(context.Background())
	beginStop()
	selection := IntakeSelection{GiveUp: 41}

	for _, daemon := range []struct {
		name     string
		giveUps  GiveUps
		stopping context.Context
		refused  func(t *testing.T, recorder *httptest.ResponseRecorder, request string)
	}{
		{"no intake", nil, context.Background(), func(t *testing.T, recorder *httptest.ResponseRecorder, request string) {
			refusedSaying(t, recorder, http.StatusServiceUnavailable, noIntake, request+" refused: this daemon runs no intake, so it keeps no give-ups")
		}},
		{"a stop begun", stand, stopping, func(t *testing.T, recorder *httptest.ResponseRecorder, request string) {
			wantRefusal(t, recorder, http.StatusServiceUnavailable, request+" refused: the daemon is stopping; ask again once it is back")
		}},
	} {
		t.Run(daemon.name, func(t *testing.T) {
			serve(daemon.giveUps, daemon.stopping)
			for _, name := range giveUpDoorNames {
				t.Run(name, func(t *testing.T) {
					door := doors[name]
					daemon.refused(t, door.replay(selection), "intake replay")
					daemon.refused(t, door.discard(selection, "a reason"), "intake discard")
				})
			}
		})
	}
	if calls := stand.made(); len(calls) != 0 {
		t.Fatalf("a refused request reached the give-ups: %v", calls)
	}
}

// A controller grant the operator's bearer minted names no controller session, so a record closed
// through the controller's door with it is recorded as the operator's; a grant the controller's
// registered session minted is recorded as that session's.
func TestTheGiveUpRoutesRecordTheOperatorForAControllerGrantTheOperatorMinted(t *testing.T) {
	h := newHarness(t)
	giveUps, _ := workflowGiveUps(t, h)
	serveGiveUps(h, giveUps)
	byOperator := givenUp(t, h, giveUpMessage(t, testDispatchProject, "LEGION-901", 1))
	bySession := givenUp(t, h, giveUpMessage(t, testDispatchProject, "LEGION-902", 2))
	// The session registers first: a capability's mint revokes every controller grant minted
	// before it, the operator's among them.
	sessionGrant := h.controllerSession()
	operatorGrant := controllerGrant(t, h).GrantID
	for _, row := range []struct {
		door  giveUpDoor
		given record.IntakeFailure
	}{
		{controllerDoor(t, h, operatorGrant, "operator"), byOperator},
		{controllerDoor(t, h, sessionGrant, "controller session "+controllerSession), bySession},
	} {
		discardAnswer(t, row.door.discard(IntakeSelection{GiveUp: row.given.ID}, "a test discard"))
		if kept := giveUpRecord(t, h, row.given.ID); kept.ClosedBy != row.door.caller || kept.DiscardedBy != row.door.caller {
			t.Errorf("record %d was closed by %q and discarded by %q, want %q", row.given.ID, kept.ClosedBy, kept.DiscardedBy, row.door.caller)
		}
	}
}

// Another project's daemon's give-up, in the same database, is no give-up of this project: its id
// is refused 404 through either door for a replay and a discard, a bulk replay of its consumer
// takes nothing, and the record is left as it was.
func TestTheGiveUpRoutesRefuseAnotherProjectsRecord(t *testing.T) {
	for _, name := range giveUpDoorNames {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			giveUps, _ := workflowGiveUps(t, h)
			serveGiveUps(h, giveUps)
			door := giveUpDoorOf(t, h, name)
			theirs := givenUp(t, h, giveUpMessage(t, "OTHER", "OTHER-5", 5))

			refused(t, door.replay(IntakeSelection{GiveUp: theirs.ID}), http.StatusNotFound, giveUpNotFound)
			refused(t, door.discard(IntakeSelection{GiveUp: theirs.ID}, "not ours"), http.StatusNotFound, giveUpNotFound)
			if answer := replayAnswer(t, door.replay(IntakeSelection{Consumer: theirs.Consumer, ThroughSeq: 100})); len(answer.Replayed) != 0 || answer.Remaining != 0 {
				t.Fatalf("a bulk replay of OTHER's consumer answered %+v, want nothing taken", answer)
			}
			if kept := giveUpRecord(t, h, theirs.ID); !kept.ClosedAt.IsZero() || !kept.ReplayClaimedAt.IsZero() || kept.LastError != "the handler failed" {
				t.Errorf("OTHER's record = %+v, want it as it was", kept)
			}
		})
	}
}

// A plugin of the contract before intakeGiveUpsContract reads the state with a schema that does
// not know `intakeGiveUps`, so the route serves a caller that states that contract the state
// without the key, and says so in its own log; a caller that states the contract is served the
// key, and the rest of the two bodies is the same. Every build from shapedAnswersContract on
// states its contract in the header, and a pod's claims place no caller at 193 or above
// (podContracts), so the header is what places a caller below this band. The older caller's log
// holds one line for each band above its contract (narrowingsExpected), the give-ups' first; a
// later band's own line is that band's test's to name. Both callers read carriedState, the
// golden's state with the keys each entry adds to it, so an entry's band finds its key to take
// and writes the line narrowingsExpected counts for it.
func TestAnOlderPluginContractGetsTheStateWithoutIntakeGiveUps(t *testing.T) {
	if intakeGiveUpsContract > DaemonAPIVersion() {
		t.Fatalf("intakeGiveUpsContract = %d, above DaemonAPIVersion %d: no caller could be served the key", intakeGiveUpsContract, DaemonAPIVersion())
	}
	older, narrowings := stateNarrowedStating(t, carriedState(t), "192.0.2.1:41000", nil, intakeGiveUpsContract-1)
	current := stateStating(t, stateServer(t, carriedState(t), nil), "192.0.2.1:41000", intakeGiveUpsContract)
	var olderBody, currentBody map[string]json.RawMessage
	if err := json.Unmarshal(older, &olderBody); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(current, &currentBody); err != nil {
		t.Fatal(err)
	}
	if served, carried := olderBody["intakeGiveUps"]; carried {
		t.Errorf("a caller at %d is served `intakeGiveUps` %s, which its plugin's schema does not name", intakeGiveUpsContract-1, served)
	}
	want, err := json.Marshal(carriedState(t).IntakeGiveUps)
	if err != nil {
		t.Fatal(err)
	}
	if served := currentBody["intakeGiveUps"]; !bytes.Equal(served, want) {
		t.Errorf("a caller at %d is served `intakeGiveUps` %s, want the state's %s", intakeGiveUpsContract, served, want)
	}
	// The caller stands below every row of stateNarrowings above it, podHoldContract's and each
	// entry's band included, stateSeqContract's among them, so the log holds the line of each of
	// those rows, the give-ups' line among them, and no other.
	expected := narrowingsExpected(carriedState(t), intakeGiveUpsContract-1)
	if !slices.Contains(expected, "intakeGiveUps") {
		t.Fatalf("a caller at %d expects the lines %v, want the give-ups' among them", intakeGiveUpsContract-1, expected)
	}
	wantNarrowings(t, narrowings, intakeGiveUpsContract-1, expected)
	for i, line := range narrowings {
		if line.PluginContract != intakeGiveUpsContract-1 {
			t.Errorf("line %d of a caller at %d names contract %d, want %d",
				i, intakeGiveUpsContract-1, line.PluginContract, intakeGiveUpsContract-1)
		}
	}
	for _, body := range []map[string]json.RawMessage{olderBody, currentBody} {
		delete(body, "intakeGiveUps")
		delete(body, "contract")
	}
	olderRest, _ := json.Marshal(olderBody)
	currentRest, _ := json.Marshal(currentBody)
	if !bytes.Equal(olderRest, currentRest) {
		t.Errorf("the two bodies differ beyond `intakeGiveUps`:\n%s\n%s", olderRest, currentRest)
	}
}

// goldenIntakeReplay is the replay's answer the goldens and the shape records serve: one record of
// each outcome, the failed one with its error, and what remains of the selection.
var goldenIntakeReplay = IntakeReplayResponse{
	Replayed: []ReplayedGiveUpView{
		{ID: 41, Consumer: "legion-go-LEGION-dispatch", StreamSeq: 6044801, Outcome: intake.ReplayApplied},
		{ID: 42, Consumer: "legion-go-LEGION-dispatch", StreamSeq: 6044802, Outcome: intake.ReplayAlreadyApplied},
		{ID: 43, Consumer: "legion-go-LEGION-dispatch", StreamSeq: 6044850, Outcome: intake.ReplayNoFact},
		{ID: 44, Consumer: "legion-go-LEGION-dispatch", StreamSeq: 6044896, Outcome: intake.ReplayFailed,
			Error: "decode Envoy envelope: unexpected end of JSON input"},
	},
	Remaining: 1,
}

// goldenIntakeDiscard is the discard's answer the goldens and the shape records serve: the ids it
// closed, in stream order.
var goldenIntakeDiscard = IntakeDiscardResponse{Discarded: []int64{41, 42}}

// The replay's answer is what the controller's tool and `legion intake replay` read.
func TestIntakeReplayAnswerGolden(t *testing.T) {
	golden(t, "intake-replay.json", goldenIntakeReplay)
}

// The discard's answer is the ids it closed, in stream order.
func TestIntakeDiscardAnswerGolden(t *testing.T) {
	golden(t, "intake-discard.json", goldenIntakeDiscard)
}
