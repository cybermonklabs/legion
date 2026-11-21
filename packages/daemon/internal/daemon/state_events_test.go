package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// stateTestRun is the run id the state rows these tests write carry, as a daemon run's own rows
// carry its id (record.Postgres.PublishState).
const stateTestRun = "5f0c7a3e-2b9d-4e61-8c47-1d3a9b6e2f80"

// stateKinds is the state lane's kinds, the list the project's state lane drains.
var stateKinds = record.LaneKinds(record.LaneState)

// stateRig is the outbox of project LEGION, token legion, over a database of its own: it writes
// through a record store whose state events are on and wake nothing, publishes through the
// package's recording publisher (outboxPublisher) and logs as JSON into logged. stateURL is where
// GET /legion/v1/state is served over the same database, once serveState serves it.
type stateRig struct {
	pool      *store.Pool
	records   *record.Postgres
	publisher *outboxPublisher
	runner    *outbox
	logged    *syncBuffer
	stateURL  string
}

func newStateRig(t *testing.T) *stateRig {
	t.Helper()
	r := &stateRig{pool: isolatedOutboxPool(t), records: record.NewStore(), publisher: &outboxPublisher{}, logged: &syncBuffer{}}
	r.records.PublishState(stateTestRun, nil)
	r.runner = &outbox{pool: r.pool, records: r.records, notices: r.publisher, project: "legion", dispatchProject: "LEGION",
		log: slog.New(slog.NewJSONHandler(r.logged, nil)), now: time.Now}
	return r
}

// seed writes the records a case starts from through a record store with no state events, as a
// daemon's records stand before a boot turns them on and as the admission tests seed theirs: what
// is published is what the writes after it change.
func (r *stateRig) seed(t *testing.T, write func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		return write(context.Background(), tx, record.NewStore())
	}); err != nil {
		t.Fatalf("seed the records: %v", err)
	}
}

// serveState serves GET /legion/v1/state over the rig's database through the daemon's own state
// source (source), at stateURL: view, with the claim store it opens here, the rig's records,
// project LEGION and its token legion, and the deployment and reports of a daemon that runs
// nothing filled in. It answers the claim store, which the route reads the claims through.
func (r *stateRig) serveState(t *testing.T, view source) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), r.pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	view.store, view.records, view.project, view.projectToken = st, r.records, "LEGION", "legion"
	view.deployment = func() capabilities.Deployment { return capabilities.Deployment{} }
	view.reports = func() map[claim.Token]capabilities.Report { return nil }
	server := httptest.NewServer(api.NewServer("127.0.0.1", 0, api.Options{State: &view, StateTransactions: st, Log: quietLogger()}).Handler)
	t.Cleanup(server.Close)
	r.stateURL = server.URL
	return st
}

// getState is the body GET /legion/v1/state serves now (serveState), which a case expects served.
func (r *stateRig) getState(t *testing.T) []byte {
	t.Helper()
	response, err := http.Get(r.stateURL + "/legion/v1/state")
	if err != nil {
		t.Fatalf("GET the state: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("GET the state = %d %s (%v), want the state", response.StatusCode, body, err)
	}
	return body
}

// stateWrite is one write a test commits through records.
type stateWrite func(ctx context.Context, tx pgx.Tx, records record.Store) error

// commit runs write through records in a transaction of its own, under the commit scope the
// daemon's facts run in (intake.WithCommitHooks), and answers the ids of the state rows it queued,
// read inside the transaction while they still carry no sequence: every committed state row has
// one (record.OutboxRow.StateSeq).
func (r *stateRig) commit(t *testing.T, records record.Store, write stateWrite) []int64 {
	t.Helper()
	scoped, committed := intake.WithCommitHooks(context.Background())
	var ids []int64
	if err := pgx.BeginFunc(scoped, r.pool, func(tx pgx.Tx) error {
		if err := write(scoped, tx, records); err != nil {
			return err
		}
		rows, err := tx.Query(scoped, `select id from outbox where kind = any($1) and state_seq is null order by id`, stateKindNames())
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		return err
	}); err != nil {
		t.Fatalf("commit the write: %v", err)
	}
	committed()
	return ids
}

// stateKindNames is stateKinds as the outbox stores them.
func stateKindNames() []string {
	names := make([]string, len(stateKinds))
	for i, kind := range stateKinds {
		names[i] = string(kind)
	}
	return names
}

// drain runs the project's state lane until its list finds nothing it may run, as the lane does
// each time it is woken (outbox.drain).
func (r *stateRig) drain(t *testing.T) {
	t.Helper()
	if drained, _ := r.runner.drain(store.ForConcurrentWorker(context.Background()), "LEGION", stateKinds); !drained {
		t.Fatalf("the state lane did not drain; log:\n%s", r.logged.String())
	}
}

// stateRow is what a state row carries that these tests read: its id, the sequence and the time
// its commit gave it, and what its runs left on it.
type stateRow struct {
	id, seq    int64
	at         time.Time
	attempts   int
	leaseToken string
}

// stateRows is every state row still in the outbox, in sequence order.
func (r *stateRig) stateRows(t *testing.T) []stateRow {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), `select id, state_seq, state_at, attempts, lease_token from outbox
		where kind = any($1) order by state_seq`, stateKindNames())
	if err != nil {
		t.Fatalf("read the state rows: %v", err)
	}
	defer rows.Close()
	var found []stateRow
	for rows.Next() {
		var row stateRow
		if err := rows.Scan(&row.id, &row.seq, &row.at, &row.attempts, &row.leaseToken); err != nil {
			t.Fatalf("read a state row: %v", err)
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the state rows: %v", err)
	}
	return found
}

// refuseWith makes every later publish answer err; nil takes each publish.
func (p *outboxPublisher) refuseWith(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

// publishedState is one publish on a state topic as Dispatch reads it: the envelope's topic,
// summary and key, and its payload decoded into the type the executor publishes (stateMessage),
// each event decoded on its own (stateEvent).
type publishedState struct {
	topic, summary, key string
	message             stateMessage
	events              []stateEvent
}

// statesPublished is every publish the rig's publisher took from the from-th on, failed attempts
// included, each on the state topic of project legion and decoded as Dispatch reads it
// (decodeState).
func statesPublished(t *testing.T, publisher *outboxPublisher, from int) []publishedState {
	t.Helper()
	var states []publishedState
	for _, published := range publisher.publishes()[from:] {
		if published.topic != notify.StateTopic("legion") {
			t.Fatalf("a publish went to %q, want the project's state topic %q", published.topic, notify.StateTopic("legion"))
		}
		encoded, err := json.Marshal(published.payload)
		if err != nil {
			t.Fatalf("encode the published payload as the listener's publisher does: %v", err)
		}
		states = append(states, decodeState(t, published.topic, published.message, published.key, encoded))
	}
	return states
}

// decodeState decodes one state message strictly, refusing any key the Go types do not name, and
// holds it to the contract's rules (legionStateMessage, packages/contracts/src/legion-api.ts): a
// project key (claim.IsProjectKey), a run and a store id that are UUIDs, the commit time in UTC
// with its microseconds, events that run from firstSeq to seq one sequence each, and each event of
// one of the three kinds carrying its kind's fields and no other's. The summary is the one line the
// executor writes for it.
func decodeState(t *testing.T, topic, summary, key string, encoded []byte) publishedState {
	t.Helper()
	strict := func(raw []byte, into any) {
		t.Helper()
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(into); err != nil {
			t.Fatalf("decode the state message %s: %v", raw, err)
		}
	}
	published := publishedState{topic: topic, summary: summary, key: key}
	strict(encoded, &published.message)
	message := published.message
	if !claim.IsProjectKey(message.Project) {
		t.Fatalf("the state message names the project %q, which is no Dispatch project key", message.Project)
	}
	if _, err := uuid.Parse(message.Run); err != nil {
		t.Fatalf("the state message names the run %q, which is no UUID: %v", message.Run, err)
	}
	if _, err := uuid.Parse(message.StateID); err != nil {
		t.Fatalf("the state message names the store id %q, which is no UUID: %v", message.StateID, err)
	}
	if at, err := time.Parse(time.RFC3339Nano, message.At); err != nil || at.UTC().Format(stateAtLayout) != message.At {
		t.Fatalf("the state message's at is %q, want RFC 3339 in UTC with its microseconds (%v)", message.At, err)
	}
	if message.FirstSeq < 1 || message.Seq < message.FirstSeq || int64(len(message.Events)) != message.Seq-message.FirstSeq+1 {
		t.Fatalf("the state message runs from %d to %d with %d events, want one event for each sequence from 1 on",
			message.FirstSeq, message.Seq, len(message.Events))
	}
	if want := fmt.Sprintf("%s state %d-%d", message.Project, message.FirstSeq, message.Seq); summary != want {
		t.Fatalf("the state message's summary is %q, want %q", summary, want)
	}
	for i, raw := range message.Events {
		var event stateEvent
		strict(raw, &event)
		if event.Seq != message.FirstSeq+int64(i) {
			t.Fatalf("event %d of the message from %d carries sequence %d, want %d", i, message.FirstSeq, event.Seq, message.FirstSeq+int64(i))
		}
		var whole bool
		switch event.Kind {
		case "change":
			whole = claim.IsIssueKey(event.Issue) && len(event.Changes) > 0 && event.Admission == nil && event.Settings == nil && event.SchemaVersion == nil
		case "admission":
			whole = isJSONObject(event.Admission) && event.Issue == "" && event.Changes == nil && event.Settings == nil && event.SchemaVersion == nil
		case "boot":
			whole = isJSONObject(event.Settings) && event.SchemaVersion != nil && *event.SchemaVersion >= 0 && event.Issue == "" && event.Changes == nil && event.Admission == nil
		}
		if !whole {
			t.Fatalf("event %s is no change, admission or boot carrying its own fields and no other's", raw)
		}
		published.events = append(published.events, event)
	}
	return published
}

// isJSONObject says whether raw is one JSON object.
func isJSONObject(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

// stateIssue is the one issue the sequence tests move from write to write.
func stateIssue() record.Issue {
	return record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-1", Title: "states", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true}
}

// The project's state events are published on its state topic (notify.StateTopic, the project
// token's legionStateSubject), one message for each committed transaction, in the order of their
// sequences, which run on from transaction to transaction with no gap, each message under the key
// of its transaction's first row (record.OutboxKey), so a message sent again is one the stream
// already holds. The lane runs as the daemon runs it: a write through the workflow's records wakes
// the project's state lane as the write commits, and a row nothing woke is run by the next sweep.
// A publish that fails is tried again on the outbox's backoff with the sequence it had, and holds
// every later state row of the project until it goes. A publish the listener refuses the same way
// however often it is sent, its own grant denying the subject or the message itself refused,
// finishes its row with an ERROR line naming the sequence it drops, and the next row goes out one
// above the gap. The dropped events reach Dispatch at its next whole read of the project. Every
// message carries the store's one id (record.StatePublication.StateID), the id GET
// /legion/v1/state serves as `daemon.stateId`.
func TestStateEventsArePublishedOnTheProjectsStateSubjectInSequence(t *testing.T) {
	r := newStateRig(t)
	topic := notify.StateTopic("legion")
	if topic != "notifications.legion.legion.state" {
		t.Fatalf("the state topic of project legion is %q, want notifications.legion.legion.state", topic)
	}
	issue := stateIssue()
	moveTo := func(at phase.Phase) stateWrite {
		return func(ctx context.Context, tx pgx.Tx, records record.Store) error {
			issue.Phase = at
			return records.PutIssue(ctx, tx, issue)
		}
	}
	one := func(ids []int64) int64 {
		t.Helper()
		if len(ids) != 1 {
			t.Fatalf("the write queued %d state rows %v, want one", len(ids), ids)
		}
		return ids[0]
	}

	// The lanes run on a held clock, so no sweep runs but the one the test fires.
	clock := &heldClock{}
	stop := runLanes(t, r.runner, clock)
	// The writes go through a record store whose state events the daemon's own switch turned on
	// (publishRecordStates), which wakes the project's state lane as each write that queued a row
	// commits.
	w := &workflowRuntime{recordStore: record.NewStore(), dispatchProject: "LEGION", log: quietLogger()}
	w.outboxWake.Store(r.runner)
	publishRecordStates(w, stateTestRun)
	var heads []int64
	for _, write := range []stateWrite{
		moveTo(phase.Admitted),
		func(ctx context.Context, tx pgx.Tx, records record.Store) error {
			return records.PutSlot(ctx, tx, record.Slot{Issue: issue.Key, Index: 0, AdmittedAt: time.Now()})
		},
		moveTo(phase.Planning),
	} {
		heads = append(heads, one(r.commit(t, w.recordStore, write)))
	}
	testwait.Eventually(t, "the three woken transactions published", func() bool {
		return len(r.publisher.publishes()) == 3 && len(r.stateRows(t)) == 0
	})
	if _, sweeps := clock.armed(outboxLease); sweeps != 1 {
		t.Fatalf("sweep timers armed = %d, want the one Run armed at boot and never fired: each commit's wake ran the lane", sweeps)
	}
	testwait.Eventually(t, "the state lane exited with nothing left to run", func() bool { return liveLanes(r.runner) == 0 })
	// A row written through the rig's store, whose state events wake nothing, wakes nothing, and
	// with the clock held nothing else can start the lane until the sweep's timer fires.
	heads = append(heads, one(r.commit(t, r.records, moveTo(phase.Implementing))))
	if published, lanes := len(r.publisher.publishes()), liveLanes(r.runner); published != 3 || lanes != 0 {
		t.Fatalf("after a write that woke nothing: %d publishes and %d lanes, want 3 and none", published, lanes)
	}
	fireSweep(t, clock)
	testwait.Eventually(t, "the sweep ran the row nothing woke", func() bool {
		return len(r.publisher.publishes()) == 4 && len(r.stateRows(t)) == 0
	})
	stop()
	for i, state := range statesPublished(t, r.publisher, 0) {
		seq := int64(i + 1)
		if state.key != record.OutboxKey(heads[i]) || state.message.FirstSeq != seq || state.message.Seq != seq {
			t.Fatalf("message %d went out under %q carrying %d-%d, want %q carrying %d alone: the sequence runs on across transactions with no gap",
				i, state.key, state.message.FirstSeq, state.message.Seq, record.OutboxKey(heads[i]), seq)
		}
		if state.message.Project != "LEGION" || state.message.Run != stateTestRun {
			t.Fatalf("message %d names project %q and run %q, want LEGION and %s", i, state.message.Project, state.message.Run, stateTestRun)
		}
	}

	// The lane's own drain from here on, the clock still held: a publish that fails holds its row
	// and the later one behind it, and the row keeps its sequence.
	r.publisher.refuseWith(errors.New(`publish notice to notifications.legion.legion.state: listener returned 503 Service Unavailable: {"error":"envoy: the message bus did not answer: nats: timeout"}`))
	failing := one(r.commit(t, r.records, moveTo(phase.Testing)))
	held := one(r.commit(t, r.records, moveTo(phase.Reviewing)))
	r.drain(t)
	attempts := statesPublished(t, r.publisher, 4)
	if len(attempts) != 1 || attempts[0].key != record.OutboxKey(failing) || attempts[0].message.FirstSeq != 5 || attempts[0].message.Seq != 5 {
		t.Fatalf("the failing publish's attempts = %+v, want one attempt of sequence 5 alone under %q", attempts, record.OutboxKey(failing))
	}
	rows := r.stateRows(t)
	if len(rows) != 2 || rows[0].id != failing || rows[0].seq != 5 || rows[0].attempts != 1 || rows[0].leaseToken != "" ||
		rows[1].id != held || rows[1].seq != 6 || rows[1].attempts != 0 || rows[1].leaseToken != "" {
		t.Fatalf("the state rows after the failed publish = %+v, want row %d at sequence 5 tried once and row %d at sequence 6 untried", rows, failing, held)
	}
	// Until its backoff has passed the failed row is not due, and the row after it may not run.
	r.drain(t)
	if published := len(r.publisher.publishes()); published != 5 {
		t.Fatalf("a drain inside the failed row's backoff published %d times in all, want the 5 before it", published)
	}
	r.publisher.refuseWith(nil)
	clock.advance(time.Minute)
	r.drain(t)
	retried := statesPublished(t, r.publisher, 5)
	if len(retried) != 2 || retried[0].key != record.OutboxKey(failing) || retried[0].message.FirstSeq != 5 || retried[0].message.Seq != 5 ||
		retried[1].key != record.OutboxKey(held) || retried[1].message.FirstSeq != 6 || retried[1].message.Seq != 6 {
		t.Fatalf("the publishes after the backoff = %+v, want sequence 5 again under its own key, then 6", retried)
	}
	if rows := r.stateRows(t); len(rows) != 0 {
		t.Fatalf("state rows left after the retry = %+v, want none", rows)
	}
	if failed := loggedLines(t, r.logged.String(), `"msg":"outbox row failed"`); len(failed) != 1 {
		t.Fatalf("logged %d failed rows, want the one failed publish", len(failed))
	}

	// What the listener refuses however often it is sent finishes the row, logged, and the row
	// after it goes out one above the gap.
	for i, refusal := range []error{
		fmt.Errorf("publish notice to %s: listener returned 403 Forbidden: %w", topic, notify.ErrPublishDenied{
			Message: `envoy: the message bus denied the publish under this listener's own permissions: bus: NATS denied the publish to "` + topic + `"`}),
		fmt.Errorf("publish notice to %s: listener returned 413 Request Entity Too Large: %w", topic, notify.ErrMessageRefused{
			Status: http.StatusRequestEntityTooLarge, Message: "bus: the message is too large: nats: maximum payload exceeded"}),
	} {
		r.publisher.refuseWith(refusal)
		dropped := one(r.commit(t, r.records, moveTo([]phase.Phase{phase.Accepting, phase.Merging}[i])))
		r.drain(t)
		if rows := r.stateRows(t); len(rows) != 0 {
			t.Fatalf("after %v the state rows = %+v, want the refused row finished: it would hold the lane for ever", refusal, rows)
		}
		lines := loggedLines(t, r.logged.String(), "outbox state event refused by the listener and dropped")
		if len(lines) != i+1 || lines[i]["level"] != "ERROR" || lines[i]["row"] != float64(dropped) || lines[i]["seq"] != float64(7+i) ||
			!strings.Contains(lines[i]["error"].(string), refusal.Error()) {
			t.Fatalf("the refusal lines = %v, want an ERROR naming row %d, sequence %d and %v", lines, dropped, 7+i, refusal)
		}
	}
	r.publisher.refuseWith(nil)
	next := one(r.commit(t, r.records, moveTo(phase.Done)))
	r.drain(t)
	if after := statesPublished(t, r.publisher, 9); len(after) != 1 || after[0].key != record.OutboxKey(next) || after[0].message.FirstSeq != 9 || after[0].message.Seq != 9 {
		t.Fatalf("the publishes after the refusals = %+v, want sequence 9 alone under %q, above the two refused rows' gap", after, record.OutboxKey(next))
	}
	if failed := loggedLines(t, r.logged.String(), `"msg":"outbox row failed"`); len(failed) != 1 {
		t.Fatalf("logged %d failed rows, want the refusals logged as refusals and not as failures", len(failed))
	}

	// Every message the lane published, the failed and the refused attempts among them, carries
	// the one id of the store its sequences are counted in, and GET /legion/v1/state serves that
	// same id beside the sequence, so a reader that took its starting point from the route applies
	// these messages and no message of another store.
	r.serveState(t, source{runtime: "tmux", admissionCap: 1, startedAt: time.Now()})
	var served struct {
		Daemon struct {
			StateSeq *int64 `json:"stateSeq"`
			StateID  string `json:"stateId"`
		} `json:"daemon"`
	}
	if body := r.getState(t); json.Unmarshal(body, &served) != nil || served.Daemon.StateID == "" || served.Daemon.StateSeq == nil || *served.Daemon.StateSeq != 9 {
		t.Fatalf("the state serves %s, want daemon.stateSeq 9 and the store's daemon.stateId", body)
	}
	for i, state := range statesPublished(t, r.publisher, 0) {
		if state.message.StateID != served.Daemon.StateID {
			t.Fatalf("publish %d carries stateId %q, want the store's one id %q, which the state serves", i, state.message.StateID, served.Daemon.StateID)
		}
	}
}

// The state events one transaction commits go out as one message, in the order the transaction
// wrote them and with the one time it committed at: a pass that takes an issue's slot away and
// grants it another, and moves its phase, in one fact is one message of three events, so Dispatch
// holds only the slot that committed. A transaction whose events would pass stateMessageBound as
// the listener's envelope carries them goes out as consecutive messages, split between its events,
// each under the bound and with no gap between them.
func TestTheStateEventsOfOneTransactionArePublishedAsOneMessage(t *testing.T) {
	t.Run("a slot released and granted again and a phase moved", func(t *testing.T) {
		r := newStateRig(t)
		issue := stateIssue()
		r.commit(t, r.records, func(ctx context.Context, tx pgx.Tx, records record.Store) error {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
			return records.PutSlot(ctx, tx, record.Slot{Issue: issue.Key, Index: 0, AdmittedAt: time.Now()})
		})
		r.drain(t)
		before := len(r.publisher.publishes())
		ids := r.commit(t, r.records, func(ctx context.Context, tx pgx.Tx, records record.Store) error {
			if err := records.ReleaseSlot(ctx, tx, issue.Key); err != nil {
				return err
			}
			if err := records.PutSlot(ctx, tx, record.Slot{Issue: issue.Key, Index: 1, AdmittedAt: time.Now()}); err != nil {
				return err
			}
			issue.Phase = phase.Planning
			return records.PutIssue(ctx, tx, issue)
		})
		rows := r.stateRows(t)
		if len(ids) != 3 || len(rows) != 3 {
			t.Fatalf("the transaction queued %v and the outbox holds %+v, want three state rows", ids, rows)
		}
		for i, row := range rows {
			if row.id != ids[i] || row.seq != rows[0].seq+int64(i) || !row.at.Equal(rows[0].at) {
				t.Fatalf("the transaction's rows = %+v, want the rows %v in the order written, with consecutive sequences and one time", rows, ids)
			}
		}
		r.drain(t)
		published := statesPublished(t, r.publisher, before)
		if len(published) != 1 {
			t.Fatalf("the transaction went out as %d messages %+v, want one", len(published), published)
		}
		message := published[0]
		if message.key != record.OutboxKey(ids[0]) || message.message.FirstSeq != rows[0].seq || message.message.Seq != rows[2].seq ||
			message.message.At != rows[0].at.UTC().Format(stateAtLayout) || message.message.More {
			t.Fatalf("the message = %+v, want %d-%d at %s under %q, with no more: the transaction fits in one", message, rows[0].seq, rows[2].seq, rows[0].at.UTC().Format(stateAtLayout), record.OutboxKey(ids[0]))
		}
		want := []stateEvent{
			{Seq: rows[0].seq, Kind: "change", Issue: issue.Key, Changes: []record.StateFieldChange{
				{Field: record.StateFieldSlot, Old: json.RawMessage(`0`), New: json.RawMessage(`null`)}}},
			{Seq: rows[1].seq, Kind: "change", Issue: issue.Key, Changes: []record.StateFieldChange{
				{Field: record.StateFieldSlot, Old: json.RawMessage(`null`), New: json.RawMessage(`1`)}}},
			{Seq: rows[2].seq, Kind: "change", Issue: issue.Key, Changes: []record.StateFieldChange{
				{Field: record.StateFieldPhase, Old: json.RawMessage(`"admitted"`), New: json.RawMessage(`"planning"`)}}},
		}
		if !reflect.DeepEqual(message.events, want) {
			t.Fatalf("the message's events = %+v, want the release, the grant and the phase in that order, %+v", message.events, want)
		}
		if rows := r.stateRows(t); len(rows) != 0 {
			t.Fatalf("state rows left = %+v, want every row of the message finished with it", rows)
		}
	})

	t.Run("a transaction too large for one message is split between its events", func(t *testing.T) {
		r := newStateRig(t)
		// Each block holds 30,000 keys in quotes: 330 KB of JSON, and 390 KB as the listener's
		// envelope carries it, every quote escaped there, so two blocks fit under the bound and a
		// third does not.
		block, err := json.Marshal(map[string]any{"cap": 1, "waiting": slices.Repeat([]string{"LEGION-1"}, 30_000)})
		if err != nil {
			t.Fatal(err)
		}
		ids := r.commit(t, r.records, func(ctx context.Context, tx pgx.Tx, records record.Store) error {
			for range 4 {
				row, err := record.NewOutboxRow("LEGION", record.StateAdmission{Project: "LEGION", Run: stateTestRun, Admission: block}, time.Now())
				if err != nil {
					return err
				}
				if err := records.Enqueue(ctx, tx, row); err != nil {
					return err
				}
			}
			return nil
		})
		rows := r.stateRows(t)
		if len(ids) != 4 || len(rows) != 4 {
			t.Fatalf("the transaction queued %v and the outbox holds %d state rows, want four", ids, len(rows))
		}
		r.drain(t)
		published := statesPublished(t, r.publisher, 0)
		if len(published) != 2 {
			t.Fatalf("the transaction went out as %d messages, want two of two events each", len(published))
		}
		var events []json.RawMessage
		for i, state := range published {
			first := rows[2*i]
			if state.key != record.OutboxKey(first.id) || state.message.FirstSeq != first.seq || state.message.Seq != first.seq+1 ||
				state.message.At != rows[0].at.UTC().Format(stateAtLayout) {
				t.Fatalf("message %d went out under %q carrying %d-%d at %s, want %q carrying %d-%d at the transaction's %s",
					i, state.key, state.message.FirstSeq, state.message.Seq, state.message.At,
					record.OutboxKey(first.id), first.seq, first.seq+1, rows[0].at.UTC().Format(stateAtLayout))
			}
			// Every message of the split transaction but its last says the transaction goes on in
			// the next message, and the last says nothing: the key is absent from it, never false.
			encoded, err := json.Marshal(r.publisher.publishes()[i].payload)
			if err != nil {
				t.Fatal(err)
			}
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &keys); err != nil {
				t.Fatal(err)
			}
			more, carried := keys["more"]
			if last := i == len(published)-1; last && carried {
				t.Fatalf("the transaction's last message carries more = %s, want the key absent", more)
			} else if !last && (!carried || string(more) != "true") {
				t.Fatalf("message %d of the split transaction carries more = %s (on the wire = %v), want true: the transaction goes on in the next message", i, more, carried)
			}
			if carried := stateCarried(encoded); carried > stateMessageBound {
				t.Fatalf("message %d takes %d bytes as the envelope carries it, past the bound of %d", i, carried, stateMessageBound)
			} else {
				t.Logf("message %d carries events %d-%d in %d bytes as the envelope carries it, more = %v", i, state.message.FirstSeq, state.message.Seq, carried, state.message.More)
			}
			events = append(events, state.message.Events...)
		}
		whole, err := json.Marshal(stateMessage{Project: "LEGION", Run: stateTestRun, StateID: published[0].message.StateID,
			FirstSeq: rows[0].seq, Seq: rows[3].seq, At: rows[0].at.UTC().Format(stateAtLayout), Events: events})
		if err != nil {
			t.Fatal(err)
		}
		if carried := stateCarried(whole); carried <= stateMessageBound {
			t.Fatalf("the transaction's four events take %d bytes in one message, within the bound of %d: the case splits nothing", carried, stateMessageBound)
		}
		if rows := r.stateRows(t); len(rows) != 0 {
			t.Fatalf("state rows left = %d, want every row finished with its message", len(rows))
		}
	})
}

// admissionStateRig is a stateRig with the daemon's handlers over its records, in the order
// openWorkflow runs a fact through them (the mirror, the engine, admission, cap 1), so a fact
// applied here writes the admission block's event as a fact applied by the daemon does; and with
// GET /legion/v1/state served over the same database by the daemon's own state source.
type admissionStateRig struct {
	*stateRig
	admission *admit.Admission
	handlers  []intake.Handler
}

func newAdmissionStateRig(t *testing.T) *admissionStateRig {
	t.Helper()
	r := &admissionStateRig{stateRig: newStateRig(t)}
	log := quietLogger()
	engine := workflow.New(r.records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, log)
	r.admission = admit.New(r.records, engine, 1, "LEGION", log)
	r.handlers = []intake.Handler{blockers.NewMirror(r.records, "LEGION"), engine, r.admission}
	r.serveState(t, source{runtime: "tmux", admissionCap: 1, startedAt: time.Now()})
	return r
}

// root is a root of its own tree recorded at admitted, handed to Legion, at rank and status.
func (r *admissionStateRig) root(key, rank, status string) record.Issue {
	return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Title: key, Tree: key, Phase: phase.Admitted, Generation: 1, Status: status, Rank: rank, HandedOver: true}
}

// waiting records key as a root waiting for a slot.
func (r *admissionStateRig) waiting(t *testing.T, key, rank string) {
	t.Helper()
	r.seed(t, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		return records.PutIssue(ctx, tx, r.root(key, rank, "todo"))
	})
}

// running records key as a root at work: its record, the cap's one slot and its tree lifecycle,
// open under the workflow, as promotion writes them.
func (r *admissionStateRig) running(t *testing.T, key, rank string) {
	t.Helper()
	r.seed(t, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		if err := records.PutIssue(ctx, tx, r.root(key, rank, "in_progress")); err != nil {
			return err
		}
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: 0, AdmittedAt: time.Now()}); err != nil {
			return err
		}
		_, err := records.OpenTreeLifecycle(ctx, tx, "legion", key, treelifecycle.AuthorityWorkflow)
		return err
	})
}

// mirror writes what Dispatch says of key, its status and the keys it waits on, as the mirror
// writes it from an event, at sequence 1.
func (r *admissionStateRig) mirror(t *testing.T, key, status string, waits ...string) {
	t.Helper()
	r.seed(t, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		return records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: key, Project: "LEGION", Seq: 1,
			Status: record.Carry(status), Parent: record.Carry(""), Blockers: record.Carry(waits)})
	})
}

// apply applies fact through the daemon's handlers, as the intake does.
func (r *admissionStateRig) apply(t *testing.T, source, eventID string, fact intake.Fact) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), r.pool, source, eventID, fact, r.handlers...); err != nil {
		t.Fatalf("apply %s: %v", eventID, err)
	}
}

// publishedBlocks is the block of every admission event published so far, in order, decoded.
func (r *admissionStateRig) publishedBlocks(t *testing.T) []any {
	t.Helper()
	var blocks []any
	for _, state := range statesPublished(t, r.publisher, 0) {
		for _, event := range state.events {
			if event.Kind == "admission" {
				blocks = append(blocks, decodedJSON(t, event.Admission))
			}
		}
	}
	return blocks
}

// servedBlock is the `admission` GET /legion/v1/state serves now, decoded.
func (r *admissionStateRig) servedBlock(t *testing.T) any {
	t.Helper()
	body := r.getState(t)
	var state map[string]json.RawMessage
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	return decodedJSON(t, state["admission"])
}

// decodedJSON is raw as plain JSON values, which compare equal whatever order raw writes its keys in.
func decodedJSON(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return value
}

// The admission event the state lane publishes carries the block GET /legion/v1/state serves of the
// same records, field for field, after each change the admission pass's own test makes of it
// (admit's TestAnAdmissionPassQueuesTheBlockOnlyWhenItChanges), driven here through the daemon's
// own handlers: a slot a tick grants, a rank a Dispatch event moves and a blocker link a Dispatch
// event adds. Each change publishes one admission event, whose block is not the one before it.
func TestTheAdmissionEventCarriesTheBlockTheStateServes(t *testing.T) {
	const run, first, second, blocker = "LEGION-100", "LEGION-101", "LEGION-102", "LEGION-199"
	for _, tc := range []struct {
		name   string
		seed   func(t *testing.T, r *admissionStateRig)
		change func(t *testing.T, r *admissionStateRig)
	}{
		{
			name: "a slot granted",
			seed: func(t *testing.T, r *admissionStateRig) {
				r.waiting(t, first, "A")
				r.mirror(t, first, "todo")
			},
			change: func(t *testing.T, r *admissionStateRig) { r.apply(t, "controller", "tick", intake.ControllerTick{}) },
		},
		{
			name: "a Dispatch rank moved",
			seed: func(t *testing.T, r *admissionStateRig) {
				r.running(t, run, "0")
				r.mirror(t, run, "in_progress")
				r.waiting(t, first, "A")
				r.waiting(t, second, "B")
				r.mirror(t, first, "todo")
				r.mirror(t, second, "todo")
			},
			change: func(t *testing.T, r *admissionStateRig) {
				r.apply(t, "dispatch", "rank", intake.DispatchIssue{Key: second, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
					Status: "todo", Title: second, Rank: "0", HandedOver: true})
			},
		},
		{
			name: "a blocker link added",
			seed: func(t *testing.T, r *admissionStateRig) {
				r.running(t, run, "0")
				r.mirror(t, run, "in_progress")
				r.waiting(t, first, "A")
				r.mirror(t, first, "todo")
				r.mirror(t, blocker, "in_progress")
			},
			change: func(t *testing.T, r *admissionStateRig) {
				r.apply(t, "dispatch", "link", intake.DispatchIssue{Key: first, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
					Status: "todo", Title: first, Rank: "A", HandedOver: true, Blockers: []string{blocker}})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAdmissionStateRig(t)
			tc.seed(t, r)
			r.apply(t, "entry-sweep", "sweep", intake.EntrySweep{})
			r.drain(t)
			before := r.publishedBlocks(t)
			if len(before) == 0 {
				t.Fatal("the first pass published no admission event")
			}
			if got, want := before[len(before)-1], r.servedBlock(t); !reflect.DeepEqual(got, want) {
				t.Fatalf("the first pass's event carries the block %v, want the block the state serves, %v", got, want)
			}
			tc.change(t, r)
			r.drain(t)
			after := r.publishedBlocks(t)
			if len(after) != len(before)+1 {
				t.Fatalf("the change published %d admission events, want one", len(after)-len(before))
			}
			got := after[len(after)-1]
			if reflect.DeepEqual(got, before[len(before)-1]) {
				t.Fatalf("the change's event carries the block %v it carried before the change", got)
			}
			if want := r.servedBlock(t); !reflect.DeepEqual(got, want) {
				t.Fatalf("the change's event carries the block %v, want the block the state serves, %v", got, want)
			}
		})
	}
}

// listenerEnvelope is the envelope the Envoy listener's publish route stores for one publish
// (contracts.MessageEnvelope, packages/envoy/internal/contracts/message_envelope.go, whose fields
// are contracts.Envelope's under its JSON names): the daemon's publish names no source, so the
// route writes agent, and no session; its message is one line, so the summary is that line whole;
// its payload rides as the string the daemon sent; and the three ids the route mints are 32 hex
// digits each (id.New).
type listenerEnvelope struct {
	EventID        string `json:"event_id"`
	Source         string `json:"source"`
	SourceEventID  string `json:"source_event_id"`
	Topic          string `json:"topic"`
	DedupeKey      string `json:"dedupe_key"`
	IssuedAt       int64  `json:"issued_at"`
	PayloadSummary string `json:"payload_summary"`
	Payload        string `json:"payload,omitempty"`
	TraceID        string `json:"trace_id"`
}

// storedEnvelope is the envelope the listener stores for published, encoded.
func storedEnvelope(t *testing.T, published listenerPublish) []byte {
	t.Helper()
	minted := hex.EncodeToString(make([]byte, 16))
	encoded, err := json.Marshal(listenerEnvelope{EventID: minted, Source: "agent", SourceEventID: minted, Topic: published.Topic,
		DedupeKey: published.DedupeKey, IssuedAt: time.Now().UnixMilli(), PayloadSummary: published.Message, Payload: published.Payload, TraceID: minted})
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	return encoded
}

// seedBootStore seeds by SQL the store cfg's daemon boots over: issue, which the project shipped,
// and with earlier the state row of its last write, which the run earlierRun wrote and never
// published. It answers the sequence that row's commit gave it, 0 without it. The store is migrated
// first, so the boot applies no migration.
func seedBootStore(t *testing.T, cfg config.Config, issue string, earlier bool, earlierRun string) int64 {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := st.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch)
		values ($1, $1, $2, 'shipped', 'done', 1, 'done', 'A', 1, 'acme/widgets', 'main')`, issue, cfg.Project); err != nil {
		t.Fatalf("seed the issue: %v", err)
	}
	if !earlier {
		return 0
	}
	payload := fmt.Sprintf(`{"project":%q,"run":%q,"issue":%q,"changes":[{"field":"phase","old":"merging","new":"done"}]}`, cfg.Project, earlierRun, issue)
	if _, err := st.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ($1, $2, $3, 0, now(), '')`, string(record.OutboxKindStateChange), cfg.Project, payload); err != nil {
		t.Fatalf("seed the earlier run's state row: %v", err)
	}
	var seq int64
	if err := st.Pool().QueryRow(ctx, `select state_seq from outbox where issue = $1 and kind = $2`,
		cfg.Project, string(record.OutboxKindStateChange)).Scan(&seq); err != nil {
		t.Fatalf("read the earlier run's state row: %v", err)
	}
	if seq != 1 {
		t.Fatalf("the earlier run's row took sequence %d at its commit, want the project's first", seq)
	}
	return seq
}

// A booted daemon's first state event is its boot event (record.StateBoot), published on the
// project's state topic through the Envoy listener: one message of its own, carrying the settings
// and the schema version GET /legion/v1/state serves, the run id the boot logged, the sequence its
// commit took and the store id the state serves, and nothing of the state itself, its envelope as
// the listener stores it well under the 1 MiB a NATS server takes whole. The state serves that run
// id as `daemon.stateRun`, the run of every message this daemon run publishes, and not the run of
// a message an earlier run left. A state row an earlier run left unpublished holds a lower
// sequence, so the lane publishes it first, and the boot event follows it one sequence above.
func TestABootedDaemonPublishesItsBootEventFirst(t *testing.T) {
	const earlierRun = "1c9e4b7a-6d2f-4a83-9b05-e8f7d3c2a160"
	for _, tc := range []struct {
		name    string
		earlier bool
	}{
		{name: "a project with no state event yet"},
		{name: "a state event an earlier run left unpublished", earlier: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := workflowConfig(t, workflowNATS(t))
			listener := newNoticeListener()
			envoy := httptest.NewServer(listener)
			t.Cleanup(envoy.Close)
			cfg.EnvoyURL = envoy.URL
			token, err := claim.ProjectToken(cfg.Project)
			if err != nil {
				t.Fatal(err)
			}
			topic := notify.StateTopic(token)

			issue := cfg.Project + "-1"
			earlierSeq := seedBootStore(t, cfg, issue, tc.earlier, earlierRun)

			var logs syncBuffer
			o := fakeRuntime(fake.NewRuntime(), &built{})
			o.workflowTokens = &workflowTokenRecorder{}
			d := startDaemonLogging(t, cfg, o, slog.New(slog.NewJSONHandler(&logs, nil)))
			messages := 1
			if tc.earlier {
				messages = 2
			}
			testwait.Eventually(t, "the boot event published on the state topic", func() bool {
				return len(listener.acceptedOn(topic)) >= messages
			})
			accepted := listener.acceptedOn(topic)
			var states []publishedState
			for _, published := range accepted[:messages] {
				states = append(states, decodeState(t, published.Topic, published.Message, published.DedupeKey, []byte(published.Payload)))
			}
			if tc.earlier {
				left := states[0]
				if left.message.Run != earlierRun || left.message.FirstSeq != earlierSeq || left.message.Seq != earlierSeq ||
					left.events[0].Kind != "change" || left.events[0].Issue != issue {
					t.Fatalf("the first message = %+v, want the earlier run's change of %s at sequence %d", left, issue, earlierSeq)
				}
			}
			boot := states[messages-1]
			lines := loggedLines(t, logs.String(), "legion daemon state events on")
			if len(lines) != 1 {
				t.Fatalf("the boot logged %d lines naming its run, want one", len(lines))
			}
			if run, _ := lines[0]["run"].(string); boot.message.Run != run || boot.message.Project != cfg.Project {
				t.Fatalf("the boot event names run %q of project %q, want the run the boot logged, %q, of %s", boot.message.Run, boot.message.Project, run, cfg.Project)
			}
			if boot.message.FirstSeq != earlierSeq+1 || boot.message.Seq != earlierSeq+1 || boot.events[0].Kind != "boot" {
				t.Fatalf("the boot's message carries %d-%d, first event %q, want the boot event alone at sequence %d, one above every row before this run",
					boot.message.FirstSeq, boot.message.Seq, boot.events[0].Kind, earlierSeq+1)
			}
			// Nothing of the state rides with it: the event carries its sequence, its kind, the
			// settings and the store's schema version, and no other key.
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(boot.message.Events[0], &keys); err != nil {
				t.Fatal(err)
			}
			if got := slices.Sorted(maps.Keys(keys)); !reflect.DeepEqual(got, []string{"kind", "schemaVersion", "seq", "settings"}) {
				t.Fatalf("the boot event carries the keys %v, want seq, kind, settings and schemaVersion alone", got)
			}
			status, body := d.request(http.MethodGet, "/legion/v1/state", nil, false)
			if status != http.StatusOK {
				t.Fatalf("GET the state = %d %s", status, body)
			}
			var state struct {
				Settings json.RawMessage `json:"settings"`
				Daemon   struct {
					SchemaVersion int64  `json:"schemaVersion"`
					StateSeq      *int64 `json:"stateSeq"`
					StateID       string `json:"stateId"`
					StateRun      string `json:"stateRun"`
				} `json:"daemon"`
			}
			if err := json.Unmarshal(body, &state); err != nil {
				t.Fatalf("decode the state: %v", err)
			}
			if got, want := decodedJSON(t, boot.events[0].Settings), decodedJSON(t, state.Settings); !reflect.DeepEqual(got, want) {
				t.Fatalf("the boot event's settings = %s, want the settings the state serves, %s", boot.events[0].Settings, state.Settings)
			}
			// The schema version is the one the store records after the boot's migrations, which
			// the state serves and the boot's line names.
			if logged, _ := lines[0]["schemaVersion"].(float64); state.Daemon.SchemaVersion < 1 || *boot.events[0].SchemaVersion != state.Daemon.SchemaVersion ||
				int64(logged) != state.Daemon.SchemaVersion {
				t.Fatalf("the boot event carries schema version %d and the boot logged %v, want the state's daemon.schemaVersion %d",
					*boot.events[0].SchemaVersion, lines[0]["schemaVersion"], state.Daemon.SchemaVersion)
			}
			if state.Daemon.StateSeq == nil || *state.Daemon.StateSeq < boot.message.Seq {
				t.Fatalf("the state serves daemon.stateSeq %v, want at least the boot event's %d, which committed before the API served", state.Daemon.StateSeq, boot.message.Seq)
			}
			// The state serves the run that published the boot, which a reader takes as its
			// starting point's run: a boot of that run at or below the state's stateSeq is one
			// the state already holds.
			if state.Daemon.StateRun != boot.message.Run {
				t.Fatalf("the state serves daemon.stateRun %q, want the run of the boot event this daemon published, %q", state.Daemon.StateRun, boot.message.Run)
			}
			// Every message counted in this store carries its one id, the earlier run's left row
			// among them, and the state serves the same id.
			for _, published := range states {
				if published.message.StateID != state.Daemon.StateID {
					t.Fatalf("a message of the store carries stateId %q, want the daemon.stateId %q the state serves", published.message.StateID, state.Daemon.StateID)
				}
			}
			envelope := storedEnvelope(t, accepted[messages-1])
			if len(envelope) >= 1<<20 {
				t.Fatalf("the boot event's envelope takes %d bytes as the listener stores it, want under 1 MiB", len(envelope))
			}
			t.Logf("the boot event's envelope, as the listener's publish route stores it, takes %d bytes", len(envelope))
		})
	}
}
