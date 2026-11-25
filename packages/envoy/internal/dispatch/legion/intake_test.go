package legion

//ci:reads packages/contracts/fixtures/daemon-api/state-events.json TestAnApplyThatFailsIsRetriedBeforeAnyLaterMessage os.ReadFile (daemonAPIFixture, golden_state_test.go:17, ../../../../contracts/fixtures/daemon-api): the daemon's golden of its state lane, which packages/daemon/internal/daemon/state_events_golden_test.go writes and TestEventsWriteTheRowsAWholeReadOfTheSameStatesWrites applies

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// intakeRig is one test's intake: its database, the NATS client that publishes as the daemon's
// listener does, the lease pool reservation the intake takes its durable's hold on, the settings
// wake, the intake's clock and the log every intake of the test writes.
type intakeRig struct {
	t         *testing.T
	database  *store.Store
	publisher *bus.Client
	leases    delivery.DurableLeases
	wake      chan struct{}
	clock     *intakeClock
	logs      *logtest.Buffer
	// run is the daemon run the rig's messages name unless a test names another.
	run string
	// published numbers the rig's messages, so each has an id and a dedupe key of its own.
	published int

	// intake is the intake running now, stop stops it, and subject is the deliver subject its
	// take-over bound the durable to.
	intake  *Intake
	stop    func()
	subject string
}

// intakeClock is the intake's clock (Intake.now): the wall clock, moved forward by what the test
// has advanced it, and by jump more at each reading once the test sets a jump (jumps).
type intakeClock struct {
	mu     sync.Mutex
	offset time.Duration
	jump   time.Duration
	// done ends with the test (testing.T.Context), and with it every call after still waits to make.
	done <-chan struct{}
}

func (c *intakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += c.jump
	return time.Now().Add(c.offset)
}

func (c *intakeClock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += by
}

// jumps has each reading of the clock from here on move it forward by jump first. A whole read's
// time is one reading, and the read rule's decision after the read is the next (readFor), so a
// jump of ReadRetryAfter less holdAtLeast sends a message that holds a read that failed back to the
// stream for holdAtLeast rather than a minute, and the read is due again at its redelivery.
func (c *intakeClock) jumps(jump time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.jump = jump
}

// after runs f on a goroutine of its own once the clock reads d past its reading now: once the wall
// clock and the test's advances together have moved it that far. It is the rig's Intake.after, so
// a starting point a wake deferred (Intake.startLater) runs when the test moves the clock past the
// minute. It reads the clock without its jump, so it moves nothing. A call still waiting when the
// test ends (done) is never made, so no deferred call reaches an intake its test stopped.
func (c *intakeClock) after(d time.Duration, f func()) {
	at := c.peek().Add(d)
	go func() {
		for c.peek().Before(at) {
			select {
			case <-c.done:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
		f()
	}()
}

// peek is the clock's reading without the jump (jumps).
func (c *intakeClock) peek() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

// newIntakeRig opens the test's database and NATS account, and sends the default logger to a
// buffer the test reads. It starts no intake (start).
func newIntakeRig(t *testing.T) *intakeRig {
	t.Helper()
	logs := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewJSONHandler(logs, nil))
	database := storetest.Open(t)
	publisher, err := bus.ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(publisher.Close)
	callers, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the lease pool: %v", err)
	}
	leases, err := ReserveLeases(callers)
	if err != nil {
		t.Fatalf("reserve the intake's callers of the lease pool: %v", err)
	}
	return &intakeRig{
		t: t, database: database, publisher: publisher, leases: leases, wake: make(chan struct{}, 1),
		clock: &intakeClock{done: t.Context().Done()}, logs: logs, run: rigRun,
	}
}

// start runs an intake on a NATS client of its own until the test ends or stop is called, and
// waits for its take-over to have bound the durable on a deliver subject of its own.
func (r *intakeRig) start() {
	r.t.Helper()
	r.startOn(r.clock.now)
}

// startOn is start with now as the intake's clock rather than the rig's. The intake's deferred
// calls (Intake.after) run on the rig's clock either way.
func (r *intakeRig) startOn(now func() time.Time) {
	r.t.Helper()
	client, err := bus.ConnectOwningStream([]string{testnats.URL(r.t)})
	if err != nil {
		r.t.Fatalf("connect the intake: %v", err)
	}
	r.t.Cleanup(client.Close)
	intake := NewIntake(client, r.database.Pool, r.leases, r.wake)
	intake.now, intake.after = now, r.clock.after
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		intake.Run(ctx)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	r.t.Cleanup(stop)
	previous := r.subject
	var info *natsgo.ConsumerInfo
	r.waitFor(20*time.Second, func() bool {
		info, err = r.publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchLegionStateDurable)
		return err == nil && info.PushBound && info.Config.DeliverSubject != previous
	}, func() string { return fmt.Sprintf("the intake never bound its durable: %+v, %v", info, err) })
	r.intake, r.stop, r.subject = intake, stop, info.Config.DeliverSubject
}

// waitFor polls done until it holds, and fails the test with failure's message when within passes.
func (r *intakeRig) waitFor(within time.Duration, done func() bool, failure func() string) {
	r.t.Helper()
	if !testwait.Holds(within, 25*time.Millisecond, done) {
		r.t.Fatal(failure())
	}
}

// settled waits for the intake to have acknowledged every message the stream holds: it
// acknowledges a message once its batch has committed, or once it was skipped or dropped.
func (r *intakeRig) settled() {
	r.t.Helper()
	stream, err := r.publisher.JS().StreamInfo(bus.Stream)
	if err != nil {
		r.t.Fatalf("read the stream: %v", err)
	}
	last := stream.State.LastSeq
	var info *natsgo.ConsumerInfo
	r.waitFor(20*time.Second, func() bool {
		info, err = r.publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchLegionStateDurable)
		return err == nil && info.AckFloor.Stream >= last && info.NumAckPending == 0 && info.NumPending == 0
	}, func() string {
		return fmt.Sprintf("the intake has not acknowledged through stream sequence %d: %+v, %v", last, info, err)
	})
}

// settledHolding waits for the intake to have answered every message the stream holds, held of
// them by sending it back to the stream unacknowledged, each holding its project's owed whole read
// (heldLine), and every other one by acknowledging it. The held messages are the consumer's only
// messages waiting for an ack, and each has logged that it went back.
func (r *intakeRig) settledHolding(held int) {
	r.t.Helper()
	stream, err := r.publisher.JS().StreamInfo(bus.Stream)
	if err != nil {
		r.t.Fatalf("read the stream: %v", err)
	}
	last := stream.State.LastSeq
	var info *natsgo.ConsumerInfo
	r.waitFor(20*time.Second, func() bool {
		info, err = r.publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchLegionStateDurable)
		return err == nil && info.Delivered.Stream >= last && info.NumPending == 0 && info.NumAckPending == held &&
			len(r.logRecords(heldLine)) >= held
	}, func() string {
		return fmt.Sprintf("the intake has not answered through stream sequence %d with %d messages held (%d logged): %+v, %v",
			last, held, len(r.logRecords(heldLine)), info, err)
	})
}

// lastStreamSeq is the stream sequence of the message published last.
func (r *intakeRig) lastStreamSeq() uint64 {
	r.t.Helper()
	stream, err := r.publisher.JS().StreamInfo(bus.Stream)
	if err != nil {
		r.t.Fatalf("read the stream: %v", err)
	}
	return stream.State.LastSeq
}

// consumerInfo is the state of the intake's durable.
func (r *intakeRig) consumerInfo() *natsgo.ConsumerInfo {
	r.t.Helper()
	info, err := r.publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchLegionStateDurable)
	if err != nil {
		r.t.Fatalf("read the durable: %v", err)
	}
	return info
}

// awaitWholeReads waits for want whole reads of any project to be recorded.
func (r *intakeRig) awaitWholeReads(want int) {
	r.t.Helper()
	got := 0
	r.waitFor(20*time.Second, func() bool {
		if err := r.database.Pool.QueryRow(context.Background(),
			`select count(*) from legion_reads where kind = 'read'`).Scan(&got); err != nil {
			r.t.Fatalf("count the whole reads: %v", err)
		}
		return got >= want
	}, func() string { return fmt.Sprintf("%d whole reads recorded, want %d", got, want) })
}

// The store ids of the rig's messages and states (the state route's daemon.stateId, which every
// state message carries as stateId): rigStateID is the store of every state daemonState builds and
// of the rig's own run, and otherStateID the store of any other run (publishOf), or of a message a
// test names another store for (publishIn). rigRun is the rig's own run (intakeRig.run), the
// daemon.stateRun every state daemonState builds serves.
const (
	rigStateID   = "3f6b2c8e-91d4-4a7b-b5e0-6c2d8f1a9e47"
	otherStateID = "c2a94f17-5e3b-4d08-9b6a-7e1f0d8c5b32"
	rigRun       = "5f0c7a3e-2b9d-4e61-8c47-1d3a9b6e2f80"
)

// stateSchema is the schema version of every state daemonState builds (daemon.schemaVersion), and
// of a boot event whose store has not moved its schema since.
const stateSchema = 20261122163303

// publish publishes events as one message of project's state lane, at the wall clock's moment,
// under the rig's run, and answers that moment.
func (r *intakeRig) publish(project string, events ...map[string]any) time.Time {
	r.t.Helper()
	at := time.Now().UTC().Truncate(time.Microsecond)
	r.publishOf(project, r.run, at, events...)
	return at
}

// publishOf publishes events as the daemon's outbox publishes one committed transaction, under run
// and its store: the rig's own store for the rig's own run, which is the store of every state
// daemonState builds, and another store for any other run.
func (r *intakeRig) publishOf(project, run string, at time.Time, events ...map[string]any) {
	r.t.Helper()
	stateID := rigStateID
	if run != r.run {
		stateID = otherStateID
	}
	r.publishIn(project, run, stateID, at, false, events...)
}

// publishIn publishes events as the daemon's outbox publishes one message, through the listener's
// publish route: an envelope on notifications.legion.<token>.state whose payload is the state
// message (packages/contracts/fixtures/daemon-api/state-events.json holds such envelopes), under
// run and the store stateID, and with `more: true` where more is set: a part of a transaction the
// daemon split, with more of it to come.
func (r *intakeRig) publishIn(project, run, stateID string, at time.Time, more bool, events ...map[string]any) {
	r.t.Helper()
	message := map[string]any{
		"project": project, "run": run, "stateId": stateID, "firstSeq": events[0]["seq"], "seq": events[len(events)-1]["seq"],
		"at": at.Format("2006-01-02T15:04:05.000000Z"), "events": events,
	}
	if more {
		message["more"] = true
	}
	r.publishMessage(project, message)
}

// publishMessage publishes message, a state message's payload as given, on project's state lane
// through the listener's publish route, each with an id and a dedupe key of its own.
func (r *intakeRig) publishMessage(project string, message map[string]any) {
	r.t.Helper()
	payload, err := json.Marshal(message)
	if err != nil {
		r.t.Fatalf("encode the state message: %v", err)
	}
	r.published++
	id := fmt.Sprintf("%s-%d-%d", r.t.Name(), r.published, time.Now().UnixNano())
	if err := r.publisher.Publish(contracts.Envelope{
		EventID: id, Source: "agent", SourceEventID: id,
		Topic:          contracts.LegionStateSubject(strings.ToLower(project)),
		DedupeKey:      "legion-outbox:" + id,
		IssuedAt:       time.Now().UnixMilli(),
		PayloadSummary: fmt.Sprintf("%s state %v-%v", project, message["firstSeq"], message["seq"]),
		Payload:        string(payload),
		TraceID:        id,
	}); err != nil {
		r.t.Fatalf("publish the state message: %v", err)
	}
}

// change is a change event of issue naming each change.
func change(seq int, issue string, changes ...map[string]any) map[string]any {
	return map[string]any{"seq": seq, "kind": "change", "issue": issue, "changes": changes}
}

// moved is one field's change to value.
func moved(field string, value any) map[string]any {
	return map[string]any{"field": field, "old": nil, "new": value}
}

// claimMoved is one role's claim changing to state.
func claimMoved(role string, state any) map[string]any {
	return map[string]any{"field": "claim", "role": role, "old": nil, "new": state}
}

// admissionMoved is an admission event carrying block whole, cap and held included.
func admissionMoved(seq int, block map[string]any) map[string]any {
	return map[string]any{"seq": seq, "kind": "admission", "admission": block}
}

// booted is a boot event whose settings are INFRA's: the project, a dispatch_url and admissionCap
// as admission_cap, and whose store's schema version is schemaVersion.
func booted(seq int, schemaVersion int64, admissionCap int) map[string]any {
	return map[string]any{"seq": seq, "kind": "boot", "schemaVersion": schemaVersion, "settings": map[string]any{
		"project": "INFRA", "dispatch_url": "http://dispatch.internal.example", "admission_cap": admissionCap,
	}}
}

// daemonState is a state document of project's daemon whose daemon block serves stateSeq,
// stateSchema, the rig's store (rigStateID) and the rig's run (rigRun), whose settings carry a
// dispatch_url, and whose issues are the members given.
func daemonState(project string, stateSeq int, issues string) string {
	return fmt.Sprintf(`{"daemon":{"project":%[1]q,"schemaVersion":%[4]d,"stateSeq":%[2]d,"stateId":%[5]q,"stateRun":%[6]q},
		"settings":{"project":%[1]q,"dispatch_url":"http://dispatch.internal.example","admission_cap":2},
		"admission":{"cap":2,"active":["%[1]s-1"],"waiting":[],"trees":["%[1]s-1"],"blocked":[],"held":[]},
		"issues":{%[3]s}}`, project, stateSeq, issues, stateSchema, rigStateID, rigRun)
}

// planning is issue <project>-1 of a daemonState, planning in slot 0 under its own tree, its
// architect and its planner working.
func planning(project string) string {
	return fmt.Sprintf(`"%[1]s-1":{"key":"%[1]s-1","phase":"planning","tree":"%[1]s-1","status":"doing",
		"slot":{"index":0,"admittedAt":"2026-11-18T16:00:00Z"},
		"architect":{"session":"s-0","state":"working"},
		"workers":{"planner":{"claim":{"session":"s-1","state":"working"},"rounds":1}}}`, project)
}

// currentValues is project's legion_current, "<issue>/<field>" to the value's text.
func currentValues(t *testing.T, database *store.Store, project string) map[string]string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select issue_key || '/' || field, value::text from legion_current where project = $1`, project)
	if err != nil {
		t.Fatalf("read legion_current: %v", err)
	}
	defer rows.Close()
	values := map[string]string{}
	for rows.Next() {
		var field, value string
		if err := rows.Scan(&field, &value); err != nil {
			t.Fatalf("scan legion_current: %v", err)
		}
		values[field] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read legion_current: %v", err)
	}
	return values
}

// history is one field's values in legion_observations, oldest first, each as its JSON text.
func history(t *testing.T, database *store.Store, project, issue, field string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select value::text from legion_observations
		where project = $1 and issue_key = $2 and field = $3 order by observed_at
	`, project, issue, field)
	if err != nil {
		t.Fatalf("read the history of %s/%s: %v", issue, field, err)
	}
	values, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the history of %s/%s: %v", issue, field, err)
	}
	return values
}

// readRecord is one legion_reads row with the columns of migration 20261118212431.
type readRecord struct {
	kind   string
	reason *string
	ok     bool
	error  *string
	bytes  int
	readAt time.Time
}

func (r readRecord) String() string {
	reason, failure := "", ""
	if r.reason != nil {
		reason = *r.reason
	}
	if r.error != nil {
		failure = *r.error
	}
	return fmt.Sprintf("{%s reason=%q ok=%v error=%q bytes=%d at=%s}", r.kind, reason, r.ok, failure, r.bytes, r.readAt.Format(time.RFC3339Nano))
}

// readRecords is project's legion_reads, oldest first.
func readRecords(t *testing.T, database *store.Store, project string) []readRecord {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select kind, reason, ok, error, bytes, read_at from legion_reads where project = $1 order by id
	`, project)
	if err != nil {
		t.Fatalf("read legion_reads: %v", err)
	}
	defer rows.Close()
	var listed []readRecord
	for rows.Next() {
		var row readRecord
		if err := rows.Scan(&row.kind, &row.reason, &row.ok, &row.error, &row.bytes, &row.readAt); err != nil {
			t.Fatalf("scan legion_reads: %v", err)
		}
		listed = append(listed, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read legion_reads: %v", err)
	}
	return listed
}

// requireWholeRead fails unless row is a whole read made for reason, good or failed as ok says.
func requireWholeRead(t *testing.T, row readRecord, reason string, ok bool) {
	t.Helper()
	if row.kind != readKindRead || row.reason == nil || *row.reason != reason || row.ok != ok {
		t.Fatalf("legion_reads row = %s, want a whole read (kind %q) for reason %q with ok %v", row, readKindRead, reason, ok)
	}
}

// cursorRow is project's legion_event_cursors row.
type cursorRow struct {
	stateURL         string
	lastSeq          *int64
	readOwed         bool
	run              *string
	stateID          *string
	readReasons      []string
	readHolder       *int64
	readHolds        int
	previousStateID  *string
	previousStateURL *string
	foreignStateIDs  []string
	releasedHolders  []int64
}

func (r cursorRow) String() string {
	return fmt.Sprintf("{stateUrl=%q lastSeq=%s readOwed=%v readReasons=%v readHolder=%s readHolds=%d run=%s stateId=%s previousStateId=%s previousStateUrl=%s foreignStateIds=%v releasedHolders=%v}",
		r.stateURL, orNone(r.lastSeq), r.readOwed, r.readReasons, orNone(r.readHolder), r.readHolds, orNone(r.run), orNone(r.stateID), orNone(r.previousStateID),
		orNone(r.previousStateURL), r.foreignStateIDs, r.releasedHolders)
}

func cursorOf(t *testing.T, database *store.Store, project string) cursorRow {
	t.Helper()
	var row cursorRow
	if err := database.Pool.QueryRow(context.Background(),
		`select state_url, last_seq, read_owed, run, state_id, read_reasons, read_holder, read_holds, previous_state_id,
			previous_state_url, foreign_state_ids, released_holders
		from legion_event_cursors where project = $1`, project).Scan(
		&row.stateURL, &row.lastSeq, &row.readOwed, &row.run, &row.stateID, &row.readReasons, &row.readHolder, &row.readHolds,
		&row.previousStateID, &row.previousStateURL, &row.foreignStateIDs, &row.releasedHolders); err != nil {
		t.Fatalf("read the cursor of %s: %v", project, err)
	}
	return row
}

func requireLastSeq(t *testing.T, database *store.Store, project string, want int64) {
	t.Helper()
	if got := cursorOf(t, database, project).lastSeq; got == nil || *got != want {
		t.Fatalf("the cursor of %s is at %s, want %d", project, orNone(got), want)
	}
}

// logRecords is every record the rig's intakes logged with msg, in order.
func (r *intakeRig) logRecords(msg string) []map[string]any {
	var records []map[string]any
	for line := range strings.Lines(r.logs.String()) {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == msg {
			records = append(records, record)
		}
	}
	return records
}

// awaitLogRecords waits for at least want records the rig's intakes logged with msg, and answers
// every one logged by then. The intake logs a whole read after the read's transaction has
// committed and its announcement has gone out (finishWhole), and a dropped message after its
// acknowledgement (deliver): what a test waits on before it reads a log line - the read's
// legion_reads row, the durable's ack floor - can be in place a moment before the line is.
func (r *intakeRig) awaitLogRecords(msg string, want int) []map[string]any {
	r.t.Helper()
	var records []map[string]any
	r.waitFor(5*time.Second, func() bool {
		records = r.logRecords(msg)
		return len(records) >= want
	}, func() string { return fmt.Sprintf("logged %d %q lines %v, want %d", len(records), msg, records, want) })
	return records
}

// awaitStartingRead waits for the take-over's whole read of project's daemon, and fails unless it
// is a good read for ReadReasonStart, in legion_reads and in its log line. Whole reads of a project
// start ReadRetryAfter apart whatever their reasons, the read right after a good starting read
// included (read-rule-cases v11), so it then moves the intake's clock past that read's minute: the
// next read a message, a write or a take-over calls for is due at once, as the tests that start
// from a good starting read expect. A test of that minute itself waits with awaitWholeReads.
func (r *intakeRig) awaitStartingRead(project string) {
	r.t.Helper()
	r.awaitWholeReads(1)
	reads := readRecords(r.t, r.database, project)
	if len(reads) == 0 {
		r.t.Fatalf("no whole read of %s is recorded", project)
	}
	requireWholeRead(r.t, reads[0], ReadReasonStart, true)
	lines := r.awaitLogRecords(wholeReadLine, 1)
	if line := lines[0]; line["level"] != "INFO" || line["project"] != project || line["reason"] != ReadReasonStart || line["ok"] != true {
		r.t.Fatalf("the starting read's line = %v, want INFO for project %s, reason %s and ok", line, project, ReadReasonStart)
	}
	r.clock.advance(ReadRetryAfter + time.Second)
}

// requireWholeReadLines fails unless the whole reads logged are, in order, one per reason given,
// each at INFO naming its project, its reason, its stateSeq, its bytes and its duration.
func (r *intakeRig) requireWholeReadLines(project string, reasons ...string) []map[string]any {
	r.t.Helper()
	lines := r.awaitLogRecords(wholeReadLine, len(reasons))
	if len(lines) != len(reasons) {
		r.t.Fatalf("logged %d whole reads %v, want %d for %v", len(lines), lines, len(reasons), reasons)
	}
	for index, line := range lines {
		if line["level"] != "INFO" || line["project"] != project || line["reason"] != reasons[index] {
			r.t.Fatalf("whole read line %d = %v, want INFO for project %s and reason %s", index, line, project, reasons[index])
		}
		for _, attribute := range []string{"stateSeq", "bytes", "duration"} {
			if _, present := line[attribute]; !present {
				r.t.Fatalf("whole read line %d = %v, want it to name %s", index, line, attribute)
			}
		}
	}
	return lines
}

// TestAnEventLeavesEveryFieldItDoesNotNameAsItWas: a whole read observes every field of the state
// and writes `null` for a field it no longer carries; an event names only what moved, and every
// field it does not name keeps the value Dispatch observed last, `null` row or not.
func TestAnEventLeavesEveryFieldItDoesNotNameAsItWas(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	before := currentValues(t, rig.database, "INFRA")
	for _, field := range []string{"/admission", "/settings", "INFRA-1/phase", "INFRA-1/tree", "INFRA-1/slot", "INFRA-1/claim.architect", "INFRA-1/claim.planner"} {
		if _, observed := before[field]; !observed {
			t.Fatalf("the starting read observed %v, want %s among them", before, field)
		}
	}
	rows := len(observations(t, rig.database, "INFRA"))

	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
	rig.publish("INFRA", change(12, "INFRA-1", claimMoved("planner", nil), claimMoved("implementer", "launching")))
	rig.publish("INFRA", admissionMoved(13, map[string]any{"cap": 3, "active": []string{"INFRA-1"}, "waiting": []string{}, "held": []string{}}))
	rig.settled()

	want := maps.Clone(before)
	want["INFRA-1/phase"] = `"implementing"`
	want["INFRA-1/claim.planner"] = `null`
	want["INFRA-1/claim.implementer"] = `"launching"`
	want["/admission"] = `{"cap": 3, "active": ["INFRA-1"]}`
	if after := currentValues(t, rig.database, "INFRA"); !maps.Equal(after, want) {
		t.Errorf("legion_current = %v, want %v: each event moves the fields it names and leaves the tree, the slot, the architect's claim and the settings as the read left them", after, want)
	}
	if written := len(observations(t, rig.database, "INFRA")) - rows; written != 4 {
		t.Errorf("the three messages wrote %d rows, want 4: one per field they name", written)
	}
	if served := daemon.served(); served != 1 {
		t.Errorf("the daemon served %d reads, want the starting one alone", served)
	}
}

// TestEachAppliedBatchRecordsAReadAtItsNewestEvent: each message the intake applies records one
// legion_reads row of kind `events`, at the message's `at` (the time its events committed, which
// they share), with the bytes it carried and no reason, and its observations are observed at that
// moment.
func TestEachAppliedBatchRecordsAReadAtItsNewestEvent(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	first := rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")), change(12, "INFRA-1", moved("slot", nil)))
	rig.settled()
	second := rig.publish("INFRA", change(13, "INFRA-1", moved("phase", "testing")))
	rig.settled()

	reads := readRecords(t, rig.database, "INFRA")
	if len(reads) != 3 {
		t.Fatalf("legion_reads = %v, want the starting read and one row per message", reads)
	}
	requireWholeRead(t, reads[0], ReadReasonStart, true)
	for index, at := range []time.Time{first, second} {
		row := reads[index+1]
		if row.kind != readKindEvents || row.reason != nil || !row.ok || row.error != nil || row.bytes == 0 || !row.readAt.Equal(at) {
			t.Errorf("legion_reads row %d = %s, want an `events` row at %s with its bytes and no reason", index+1, row, at.Format(time.RFC3339Nano))
		}
	}
	var observedAt time.Time
	if err := rig.database.Pool.QueryRow(t.Context(), `
		select observed_at from legion_current where project = 'INFRA' and issue_key = 'INFRA-1' and field = 'slot'
	`).Scan(&observedAt); err != nil {
		t.Fatalf("read the slot's newest row: %v", err)
	}
	if !observedAt.Equal(first) {
		t.Errorf("the slot released at %s is observed at %s, want the message's at", first.Format(time.RFC3339Nano), observedAt.Format(time.RFC3339Nano))
	}
	requireLastSeq(t, rig.database, "INFRA", 13)
}

// TestEachAppliedBatchWakesTheProjectsBoard: each batch the intake applies is announced on
// StateChannel with its project, which is what wakes an open Legion board (api/delivery_live_hub.go),
// as a whole read is.
func TestEachAppliedBatchWakesTheProjectsBoard(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	announced := announcements(t, rig.database, "INFRA")

	rig.start()
	announced("the starting read")
	rig.awaitStartingRead("INFRA")
	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
	announced("a batch of state events")
	rig.publish("INFRA", change(12, "INFRA-1", claimMoved("planner", "idle")))
	announced("a second batch")
}

// TestAStoppedIntakeReplaysWhatItMissed: the messages published while no intake ran stay with the
// durable, and the next intake's take-over creates it again at its ack floor, so they are applied
// in order and nothing reads the daemon again: the cursor is the starting point.
func TestAStoppedIntakeReplaysWhatItMissed(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
	rig.settled()

	rig.stop()
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "testing")))
	rig.publish("INFRA", change(13, "INFRA-1", claimMoved("tester", "working")))
	if phase := currentValues(t, rig.database, "INFRA")["INFRA-1/phase"]; phase != `"implementing"` {
		t.Fatalf("the phase is %s with no intake running, want the implementing the running one applied", phase)
	}

	rig.start()
	rig.settled()
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); !slices.Equal(got, []string{`"planning"`, `"implementing"`, `"testing"`}) {
		t.Errorf("the phase's history is %v, want the read's, then each message's in order", got)
	}
	if claim := currentValues(t, rig.database, "INFRA")["INFRA-1/claim.tester"]; claim != `"working"` {
		t.Errorf("the tester's claim is %q, want the missed message's working", claim)
	}
	requireLastSeq(t, rig.database, "INFRA", 13)
	if served := daemon.served(); served != 1 {
		t.Errorf("the daemon served %d reads, want the starting one alone: the second take-over has the cursor to start from", served)
	}
	rig.requireWholeReadLines("INFRA", ReadReasonStart)
}

// TestAGapTheStreamNoLongerHoldsMakesOneWholeRead: a message whose first sequence is past the
// cursor's next one carries events the stream no longer holds, so the project is read whole once,
// the cursor takes the read's stateSeq, and the messages after it are applied with no other read.
func TestAGapTheStreamNoLongerHoldsMakesOneWholeRead(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	// Sequences 11 to 13 never reach Dispatch; the daemon's state holds them.
	daemon.serves(daemonState("INFRA", 15, strings.Replace(planning("INFRA"), `"planning"`, `"reviewing"`, 1)))
	rig.publish("INFRA", change(14, "INFRA-1", moved("phase", "testing")), change(15, "INFRA-1", moved("phase", "reviewing")))
	rig.settled()

	if served := daemon.served(); served != 2 {
		t.Fatalf("the daemon served %d reads, want the starting one and one for the gap", served)
	}
	reads := readRecords(t, rig.database, "INFRA")
	if len(reads) != 2 {
		t.Fatalf("legion_reads = %v, want the starting read and the gap's, and no batch: the message is at or below the read's sequence", reads)
	}
	requireWholeRead(t, reads[1], ReadReasonGap, true)
	requireLastSeq(t, rig.database, "INFRA", 15)
	lines := rig.requireWholeReadLines("INFRA", ReadReasonStart, ReadReasonGap)
	if lines[1]["stateSeq"] != float64(15) {
		t.Errorf("the gap's read logged stateSeq %v, want 15", lines[1]["stateSeq"])
	}

	rig.publish("INFRA", change(16, "INFRA-1", moved("phase", "accepting")))
	rig.settled()
	if phase := currentValues(t, rig.database, "INFRA")["INFRA-1/phase"]; phase != `"accepting"` {
		t.Errorf("the phase after the gap is %s, want the next message's accepting", phase)
	}
	if served := daemon.served(); served != 2 {
		t.Errorf("the daemon served %d reads, want no more once the gap was read", served)
	}
}

// TestAWholeReadAfterEventsAheadOfDispatchsClockIsTheNewestState: an event's rows are observed at
// the daemon's commit time, and the daemon's clock can run ahead of Dispatch's. A whole read made
// after such events is the newer state all the same, so each row it writes, a `null` for a field
// it no longer carries included, lands after the field's newest row, and legion_current (what
// ReadCurrent answers) holds the read's values rather than the older events'. Every row the read
// writes is at one moment, 1 µs after the project's newest row, though some of its fields have
// older newest rows than others: that one moment is how the metrics tell an issue a read dropped
// from a slot released (metrics' slotSpans). The read's own legion_reads row keeps the read's time.
func TestAWholeReadAfterEventsAheadOfDispatchsClockIsTheNewestState(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	// The daemon's clock is an hour ahead of Dispatch's. Message 12 commits a second after 11, so
	// the architect's claim has a newer row than the phase and the planner's claim.
	ahead := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	rig.publishOf("INFRA", rig.run, ahead,
		change(11, "INFRA-1", moved("phase", "implementing"), claimMoved("planner", "idle")))
	latest := ahead.Add(time.Second)
	rig.publishOf("INFRA", rig.run, latest, change(12, "INFRA-1", claimMoved("architect", "idle")))
	rig.settled()
	if current := currentValues(t, rig.database, "INFRA"); current["INFRA-1/phase"] != `"implementing"` ||
		current["INFRA-1/claim.planner"] != `"idle"` || current["INFRA-1/claim.architect"] != `"idle"` {
		t.Fatalf("legion_current after the events = %v, want their implementing and idle claims", current)
	}

	// Sequence 13 never reaches Dispatch. The daemon's state has moved the phase on, dropped the
	// planner's claim and put the architect back to work, which the gap's whole read finds.
	daemon.serves(daemonState("INFRA", 15, `"INFRA-1":{"key":"INFRA-1","phase":"reviewing","tree":"INFRA-1",
		"slot":{"index":0},"architect":{"session":"s-0","state":"working"}}`))
	rig.publishOf("INFRA", rig.run, latest.Add(time.Second),
		change(14, "INFRA-1", moved("phase", "testing")), change(15, "INFRA-1", moved("phase", "reviewing")))
	rig.settled()
	reads := readRecords(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonGap, true)
	if readAt := reads[len(reads)-1].readAt; !readAt.Before(ahead) {
		t.Fatalf("the gap's read is recorded at %s, want its own time on Dispatch's clock, before the events' %s",
			readAt.Format(time.RFC3339Nano), ahead.Format(time.RFC3339Nano))
	}

	current, err := ReadCurrent(t.Context(), rig.database.Pool, "INFRA")
	if err != nil {
		t.Fatalf("read the current state: %v", err)
	}
	newest := map[string]string{}
	for _, one := range current.Newest {
		newest[one.IssueKey+"/"+one.Field] = string(one.Value)
	}
	if phase := newest["INFRA-1/phase"]; phase != `"reviewing"` {
		t.Errorf("ReadCurrent's phase is %s, want the whole read's reviewing: the read is newer than the events whatever their clock says", phase)
	}
	if claim, held := newest["INFRA-1/claim.planner"]; held {
		t.Errorf("ReadCurrent holds the planner's claim %s, want none: the whole read no longer carries it", claim)
	}
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); !slices.Equal(got, []string{`"planning"`, `"implementing"`, `"reviewing"`}) {
		t.Errorf("the phase's history is %v, want the starting read's, the event's, then the gap's read's", got)
	}

	// The read wrote three rows - the phase, the planner's claim it no longer carries and the
	// architect's claim - and every one of them is 1 µs after message 12's, the project's newest
	// row, though the phase's and the planner's own newest rows were a second older.
	written := map[string]time.Time{}
	for _, row := range observedRows(t, rig.database, "INFRA") {
		if row.at.After(latest) {
			written[row.issue+"/"+row.field] = row.at
		}
	}
	moment := latest.Add(time.Microsecond)
	if len(written) != 3 {
		t.Errorf("the gap's read wrote %v after the events, want the phase and the two claims", written)
	}
	for field, at := range written {
		if !at.Equal(moment) {
			t.Errorf("the gap's read wrote %s at %s, want every row of the read at %s, 1 µs after the project's newest row",
				field, at.Format(time.RFC3339Nano), moment.Format(time.RFC3339Nano))
		}
	}
}

// TestABootThatDoesNotFollowMakesOneWholeRead: a boot event whose sequence does not follow the
// cursor, one at or below it under a run the cursor has not followed (a daemon of the same store
// whose counter went back), and one whose schema version is not the starting point's each read the
// project whole once; a boot that follows reads nothing, under the cursor's run or another, and
// neither does a boot replayed under the run the cursor followed last.
func TestABootThatDoesNotFollowMakesOneWholeRead(t *testing.T) {
	const otherRun = "a7d1c0e2-6b8f-4c3a-9e51-0f2b8d4c6a19"
	for _, tc := range []struct {
		name string
		// before is published and applied first; boot is the boot's message.
		before func(rig *intakeRig)
		boot   func(rig *intakeRig)
		// stateSeq is what the daemon serves at the boot's read, and reason the read's reason, ""
		// for none.
		stateSeq int
		reason   string
	}{
		{
			name:     "a boot past the next sequence",
			boot:     func(rig *intakeRig) { rig.publish("INFRA", booted(13, stateSchema, 2)) },
			stateSeq: 13, reason: ReadReasonBoot,
		},
		{
			name: "a boot at or below the cursor under another run",
			before: func(rig *intakeRig) {
				rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
			},
			boot: func(rig *intakeRig) {
				rig.publishIn("INFRA", otherRun, rigStateID, time.Now().UTC().Truncate(time.Microsecond), false, booted(4, stateSchema, 2))
			},
			stateSeq: 4, reason: ReadReasonBoot,
		},
		{
			name:     "a boot whose schema version is not the starting point's",
			boot:     func(rig *intakeRig) { rig.publish("INFRA", booted(11, stateSchema+1, 2)) },
			stateSeq: 11, reason: ReadReasonSchema,
		},
		{
			name:     "a boot that follows",
			boot:     func(rig *intakeRig) { rig.publish("INFRA", booted(11, stateSchema, 3)) },
			stateSeq: 11,
		},
		{
			// The cursor follows the rig's run, and the daemon boots again under another run
			// of the same store: its boot is the next sequence.
			name: "a boot that follows under another run than the cursor's",
			before: func(rig *intakeRig) {
				rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
			},
			boot: func(rig *intakeRig) {
				rig.publishIn("INFRA", otherRun, rigStateID, time.Now().UTC().Truncate(time.Microsecond), false, booted(12, stateSchema, 3))
			},
			stateSeq: 12,
		},
		{
			// The boot and a change after it are applied under the rig's run, and the stream
			// delivers the boot again: at or below the cursor, under the cursor's own run.
			name: "a boot replayed under the cursor's own run",
			before: func(rig *intakeRig) {
				rig.publish("INFRA", booted(11, stateSchema, 3))
				rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "implementing")))
			},
			boot:     func(rig *intakeRig) { rig.publish("INFRA", booted(11, stateSchema, 3)) },
			stateSeq: 12,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIntakeRig(t)
			daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
			seedLegionProject(t, rig.database, "INFRA", daemon.URL)
			rig.start()
			rig.awaitStartingRead("INFRA")
			if tc.before != nil {
				tc.before(rig)
				rig.settled()
			}
			daemon.serves(daemonState("INFRA", tc.stateSeq, planning("INFRA")))
			tc.boot(rig)
			rig.settled()

			reads := readRecords(t, rig.database, "INFRA")
			wholeReads := 0
			for _, row := range reads {
				if row.kind == readKindRead {
					wholeReads++
				}
			}
			if tc.reason == "" {
				if served := daemon.served(); served != 1 || wholeReads != 1 {
					t.Fatalf("the daemon served %d reads and legion_reads holds %v, want the starting read alone: the boot follows", served, reads)
				}
				if settings := currentValues(t, rig.database, "INFRA")["/settings"]; !strings.Contains(settings, `"admission_cap": 3`) {
					t.Errorf("the settings are %s, want the boot's", settings)
				}
				requireLastSeq(t, rig.database, "INFRA", int64(tc.stateSeq))
				rig.requireWholeReadLines("INFRA", ReadReasonStart)
				return
			}
			if served := daemon.served(); served != 2 || wholeReads != 2 {
				t.Fatalf("the daemon served %d reads and legion_reads holds %v, want the starting read and one for the boot", served, reads)
			}
			requireWholeRead(t, reads[len(reads)-1], tc.reason, true)
			requireLastSeq(t, rig.database, "INFRA", int64(tc.stateSeq))
			lines := rig.requireWholeReadLines("INFRA", ReadReasonStart, tc.reason)
			if lines[1]["stateSeq"] != float64(tc.stateSeq) {
				t.Errorf("the boot's read logged stateSeq %v, want %d", lines[1]["stateSeq"], tc.stateSeq)
			}
		})
	}
}

// TestABootOfTheRunTheStartingReadServedMakesNoRead: a daemon commits its boot event before it
// serves its state, so the starting read can hold the boot before the boot's message reaches
// Dispatch. The read takes the run of the daemon that served it (daemon.stateRun) as the cursor's
// run, so that boot, of the same run and at or below the read's stateSeq, is one the read already
// holds: it writes nothing, moves no cursor and makes no second read.
func TestABootOfTheRunTheStartingReadServedMakesNoRead(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	rows := observations(t, rig.database, "INFRA")

	// The boot's message arrives after the read: the daemon's own run, at the read's stateSeq,
	// with settings the read does not hold, so a boot the intake applied would show.
	rig.publish("INFRA", booted(10, stateSchema, 3))
	rig.settled()

	reads := readRecords(t, rig.database, "INFRA")
	if served := daemon.served(); served != 1 || len(reads) != 1 {
		t.Fatalf("the daemon served %d reads and legion_reads holds %v, want the starting read alone: the boot is one the read already holds", served, reads)
	}
	requireWholeRead(t, reads[0], ReadReasonStart, true)
	if after := observations(t, rig.database, "INFRA"); !slices.Equal(after, rows) {
		t.Errorf("observations = %v, want the starting read's %v alone", after, rows)
	}
	requireLastSeq(t, rig.database, "INFRA", 10)
	if run := cursorOf(t, rig.database, "INFRA").run; run == nil || *run != rig.run {
		t.Errorf("the cursor follows the run %s, want %s, the run that served the starting read", orNone(run), rig.run)
	}
	if line := rig.requireWholeReadLines("INFRA", ReadReasonStart)[0]; line["stateRun"] != rig.run {
		t.Errorf("the starting read's line = %v, want it to name the run that served it, %s", line, rig.run)
	}
}

// TestABootOfAnotherRunAtOrBelowTheStartingReadMakesOneRead: a boot at or below the starting
// read's stateSeq under another run than the one that served the read is a daemon of the same
// store whose sequence went back (a database restore), which no event carries. It makes one whole
// read (ReadReasonBoot), and the cursor then follows the run that served that read.
func TestABootOfAnotherRunAtOrBelowTheStartingReadMakesOneRead(t *testing.T) {
	const restoredRun = "9e3c5a71-2f4d-4b86-a0c9-6d1e8b7f3a52"
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	// The daemon's database is restored to sequence 9 and the daemon boots again under a new run:
	// its boot takes sequence 10 again, and the state it serves is that run's.
	implementing := strings.Replace(planning("INFRA"), `"planning"`, `"implementing"`, 1)
	daemon.serves(strings.Replace(daemonState("INFRA", 10, implementing), rigRun, restoredRun, 1))
	rig.publishIn("INFRA", restoredRun, rigStateID, time.Now().UTC().Truncate(time.Microsecond), false, booted(10, stateSchema, 2))
	rig.settled()

	reads := readRecords(t, rig.database, "INFRA")
	if served := daemon.served(); served != 2 || len(reads) != 2 {
		t.Fatalf("the daemon served %d reads and legion_reads holds %v, want the starting read and one for the boot", served, reads)
	}
	requireWholeRead(t, reads[1], ReadReasonBoot, true)
	requireLastSeq(t, rig.database, "INFRA", 10)
	if phase := currentValues(t, rig.database, "INFRA")["INFRA-1/phase"]; phase != `"implementing"` {
		t.Errorf("INFRA-1's phase is %s, want the implementing the boot's read found", phase)
	}
	if run := cursorOf(t, rig.database, "INFRA").run; run == nil || *run != restoredRun {
		t.Errorf("the cursor follows the run %s, want %s, the run that served the boot's read", orNone(run), restoredRun)
	}
	if line := rig.requireWholeReadLines("INFRA", ReadReasonStart, ReadReasonBoot)[1]; line["stateRun"] != restoredRun {
		t.Errorf("the boot's read's line = %v, want it to name the run that served it, %s", line, restoredRun)
	}
}

// TestAnEventAtOrBelowTheReadsSequenceWritesNothing: the whole read's stateSeq says every event at
// or below it is in the state it read, so a message the read already holds writes no observation,
// no legion_reads row and no move of the cursor.
func TestAnEventAtOrBelowTheReadsSequenceWritesNothing(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	rows := observations(t, rig.database, "INFRA")

	rig.publish("INFRA", change(9, "INFRA-1", moved("phase", "admitted")), change(10, "INFRA-1", moved("phase", "planning"), moved("slot", nil)))
	rig.publish("INFRA", change(10, "INFRA-1", moved("phase", "implementing")))
	rig.settled()

	if after := observations(t, rig.database, "INFRA"); !slices.Equal(after, rows) {
		t.Errorf("observations = %v, want the starting read's %v alone", after, rows)
	}
	if reads := readRecords(t, rig.database, "INFRA"); len(reads) != 1 {
		t.Errorf("legion_reads = %v, want the starting read alone", reads)
	}
	requireLastSeq(t, rig.database, "INFRA", 10)
	if served := daemon.served(); served != 1 {
		t.Errorf("the daemon served %d reads, want the starting one alone", served)
	}
}

// TestAProjectWithoutAStartingPointIsReadOnceAndNothingReadsItAgain: a project with a setting and
// no starting point is read whole once at each of the three moments it has none - the durable's
// first creation and a new setting, with no cursor, and a PUT of the same URL while no message
// holds a read, which leaves the cursor following no daemon (settingWritten), at the settings wake
// - and nothing else reads a daemon: a run of events, a wake that changed nothing, all apply with
// no read.
func TestAProjectWithoutAStartingPointIsReadOnceAndNothingReadsItAgain(t *testing.T) {
	rig := newIntakeRig(t)
	infra := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	linux := newFakeDaemon(t, daemonState("LINUX", 20, planning("LINUX")))
	seedLegionProject(t, rig.database, "INFRA", infra.URL)
	seedProject(t, rig.database, "LINUX")
	actor := model.Actor{Kind: "user", ID: "alice@example.com"}

	// The durable's first creation.
	rig.start()
	rig.awaitStartingRead("INFRA")
	for seq := 11; seq <= 13; seq++ {
		rig.publish("INFRA", change(seq, "INFRA-1", moved("slot", seq-10)))
	}
	rig.settled()

	// A new setting, which the settings wake hands the intake.
	if _, err := PutSettings(t.Context(), rig.database.Pool, "LINUX", linux.URL, actor); err != nil {
		t.Fatalf("set LINUX's daemon: %v", err)
	}
	rig.wake <- struct{}{}
	rig.awaitWholeReads(2)
	rig.publish("LINUX", change(21, "LINUX-1", moved("phase", "implementing")))
	rig.publish("LINUX", change(22, "LINUX-1", moved("phase", "testing")))
	rig.settled()

	// A PUT of the same URL, while no message holds a read, forgets INFRA's starting point: the
	// cursor keeps no sequence and no store. The daemon's state now holds the three events the
	// intake applied, as a daemon's would.
	infra.serves(daemonState("INFRA", 13, planning("INFRA")))
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", infra.URL, actor); err != nil {
		t.Fatalf("set INFRA's daemon again: %v", err)
	}
	if c := cursorOf(t, rig.database, "INFRA"); c.lastSeq != nil || c.stateID != nil || c.readOwed || c.readHolder != nil {
		t.Fatalf("INFRA's cursor after the PUT of the same URL is %s, want no sequence, no store, nothing owed and no holder", c)
	}
	rig.wake <- struct{}{}
	rig.awaitWholeReads(3)

	// A settings change that leaves both cursors, then more events: no read. The check is queued
	// on each project's runner before the messages reach it, so the settle covers it.
	rig.intake.SettingsChanged(t.Context())
	rig.publish("INFRA", change(14, "INFRA-1", moved("phase", "implementing")))
	rig.publish("LINUX", change(23, "LINUX-1", moved("phase", "reviewing")))
	rig.settled()

	if served := infra.served(); served != 2 {
		t.Errorf("INFRA's daemon served %d reads, want two: the first creation's and the PUT's", served)
	}
	if served := linux.served(); served != 1 {
		t.Errorf("LINUX's daemon served %d reads, want one: its new setting's", served)
	}
	infraReads, linuxReads := readRecords(t, rig.database, "INFRA"), readRecords(t, rig.database, "LINUX")
	var wholeReads []readRecord
	for _, row := range slices.Concat(infraReads, linuxReads) {
		if row.kind == readKindRead {
			wholeReads = append(wholeReads, row)
		}
	}
	if len(wholeReads) != 3 {
		t.Fatalf("whole reads = %v, want three", wholeReads)
	}
	for _, row := range wholeReads {
		requireWholeRead(t, row, ReadReasonStart, true)
	}
	if phase := currentValues(t, rig.database, "LINUX")["LINUX-1/phase"]; phase != `"reviewing"` {
		t.Errorf("LINUX's phase is %s, want its last message's reviewing", phase)
	}
	requireLastSeq(t, rig.database, "INFRA", 14)
	requireLastSeq(t, rig.database, "LINUX", 23)
	lines := rig.awaitLogRecords(wholeReadLine, 3)
	var projects []string
	for _, line := range lines {
		if line["reason"] != ReadReasonStart || line["level"] != "INFO" {
			t.Errorf("whole read line %v, want INFO with reason %s", line, ReadReasonStart)
		}
		projects = append(projects, fmt.Sprint(line["project"]))
	}
	if want := []string{"INFRA", "LINUX", "INFRA"}; !slices.Equal(projects, want) {
		t.Errorf("whole reads logged for %v, want %v", projects, want)
	}
}

// TestAFailedWholeReadIsTriedAgainAtTheNextMessageAndNoSooner: a starting read the daemon answers
// with a state that is no starting point, or with another project's, records a failed read naming
// why and owes the read; a state of the project's own still records its observations. The cursor
// then holds no store, so the messages that arrive meanwhile are not applied and the cursor stays
// where the failed read left it; neither a settings change nor the next message reads again inside
// ReadRetryAfter, and the first message after it does. The settings change comes while no message
// holds the owed read, so the minute alone holds it back. The first message inside the minute holds
// the owed read, so it goes back to the stream until the read is due and logs only that, and the
// second is acknowledged and its drop logged.
func TestAFailedWholeReadIsTriedAgainAtTheNextMessageAndNoSooner(t *testing.T) {
	issue := planning("INFRA")
	for _, tc := range []struct {
		name     string
		state    string
		status   int
		failure  string
		observed bool
	}{
		{
			name: "a state with no stateSeq",
			state: `{"daemon":{"project":"INFRA","schemaVersion":20261117000000},
				"settings":{"dispatch_url":"http://dispatch.internal.example"},"issues":{` + issue + `}}`,
			failure: ErrNoStateSeq.Error(), observed: true,
		},
		{
			name:    "a state whose stateSeq is 0",
			state:   daemonState("INFRA", 0, issue),
			failure: ErrStateSeqZero.Error(), observed: true,
		},
		{
			name:    "a state whose settings carry no dispatch_url",
			state:   strings.Replace(daemonState("INFRA", 4, issue), `"dispatch_url":"http://dispatch.internal.example",`, ``, 1),
			failure: ErrNoDispatchURL.Error(), observed: true,
		},
		{
			name:    "a state whose daemon block serves no stateId",
			state:   strings.Replace(daemonState("INFRA", 4, issue), `,"stateId":"`+rigStateID+`"`, ``, 1),
			failure: ErrNoStateID.Error(), observed: true,
		},
		{
			name:    "a state of another project",
			state:   strings.Replace(daemonState("INFRA", 4, issue), `"daemon":{"project":"INFRA"`, `"daemon":{"project":"LINUX"`, 1),
			failure: ErrStateOfAnotherProject.Error(),
		},
		{
			name:    "a daemon that refuses",
			status:  http.StatusServiceUnavailable,
			failure: ErrUnavailable.Error(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIntakeRig(t)
			daemon := newFakeDaemon(t, tc.state)
			if tc.status != 0 {
				daemon.refuses(tc.status)
			}
			seedLegionProject(t, rig.database, "INFRA", daemon.URL)
			rig.start()
			rig.awaitWholeReads(1)

			reads := readRecords(t, rig.database, "INFRA")
			requireWholeRead(t, reads[0], ReadReasonStart, false)
			if reads[0].error == nil || !strings.Contains(*reads[0].error, tc.failure) {
				t.Fatalf("the failed read's error is %v, want it to name %q", reads[0].error, tc.failure)
			}
			phase, observed := currentValues(t, rig.database, "INFRA")["INFRA-1/phase"]
			if observed != tc.observed || (observed && phase != `"planning"`) {
				t.Fatalf("after the failed read the phase is %q (observed %v), want observed %v", phase, observed, tc.observed)
			}
			if current := cursorOf(t, rig.database, "INFRA"); !current.readOwed || current.lastSeq != nil {
				t.Fatalf("the cursor after the failed read is %+v, want a read owed and no sequence", current)
			}
			line := rig.requireWholeReadLines("INFRA", ReadReasonStart)[0]
			if line["ok"] != false || !strings.Contains(fmt.Sprint(line["error"]), tc.failure) {
				t.Fatalf("the failed read's line is %v, want ok false and its error", line)
			}

			// Inside ReadRetryAfter, a settings change and the next message read nothing. The
			// settings change is queued on INFRA's runner ahead of the messages, while no message
			// holds the owed read. No good read has said which store the daemon counts in, so
			// neither message is applied: the first goes back to the stream until the read is due
			// and logs that alone, and the second is acknowledged, its drop logged once.
			before := currentValues(t, rig.database, "INFRA")
			rig.clock.advance(ReadRetryAfter / 2)
			rig.intake.SettingsChanged(t.Context())
			rig.publish("INFRA", change(5, "INFRA-1", moved("phase", "implementing")))
			rig.settledHolding(1)
			rig.publish("INFRA", change(6, "INFRA-1", claimMoved("planner", "idle")))
			rig.settledHolding(1)
			if served := daemon.served(); served != 1 {
				t.Fatalf("the daemon served %d reads inside a minute of the failed one, want none more", served)
			}
			if after := currentValues(t, rig.database, "INFRA"); !maps.Equal(after, before) {
				t.Fatalf("legion_current while the cursor holds no store = %v, want the failed read's %v: no message is applied", after, before)
			}
			if current := cursorOf(t, rig.database, "INFRA"); !current.readOwed || current.lastSeq != nil || current.stateID != nil {
				t.Fatalf("the cursor is %+v, want no sequence, no store and the read still owed", current)
			}
			if drops := rig.awaitLogRecords(noStoreLine, 1); len(drops) != 1 || drops[0]["project"] != "INFRA" || drops[0]["seq"] != float64(6) {
				t.Fatalf("the drops logged = %v, want one naming INFRA and the second message's sequence: the first went back to the stream", drops)
			}
			if held := rig.logRecords(heldLine); len(held) != 1 || held[0]["seq"] != float64(5) || held[0]["delivery"] != float64(1) {
				t.Fatalf("the messages sent back = %v, want the first message once, at its first delivery", held)
			}

			// Past ReadRetryAfter, the next message reads the daemon again, and this time the
			// read is good: the cursor takes its stateSeq and owes nothing. The first message
			// comes back from the stream only once its own delay has passed.
			daemon.serves(daemonState("INFRA", 7, strings.Replace(issue, `"planning"`, `"testing"`, 1)))
			rig.clock.advance(ReadRetryAfter/2 + time.Second)
			rig.publish("INFRA", change(7, "INFRA-1", moved("phase", "testing")))
			rig.settledHolding(1)
			if served := daemon.served(); served != 2 {
				t.Fatalf("the daemon served %d reads, want one more once the minute had passed", served)
			}
			reads = readRecords(t, rig.database, "INFRA")
			requireWholeRead(t, reads[len(reads)-1], ReadReasonRetry, true)
			if current := cursorOf(t, rig.database, "INFRA"); current.readOwed || current.lastSeq == nil || *current.lastSeq != 7 {
				t.Fatalf("the cursor after the good read is %+v, want it at the read's 7 and nothing owed", current)
			}
			rig.requireWholeReadLines("INFRA", ReadReasonStart, ReadReasonRetry)
		})
	}
}

// TestAMessageForAProjectWithNoSettingIsDropped: a message of a project no legion_settings row
// names is acknowledged and writes nothing, and the drop is logged once per project per
// dropLogEvery: LINUX's second and third messages log nothing, and its first message after
// dropLogEvery has passed on the intake's clock logs again. The intake then remembers only the
// projects it logged in the last dropLogEvery, so GHOST, logged before it, is forgotten.
func TestAMessageForAProjectWithNoSettingIsDropped(t *testing.T) {
	rig := newIntakeRig(t)
	seedProject(t, rig.database, "LINUX")
	rig.start()
	rig.publish("LINUX", change(1, "LINUX-1", moved("phase", "planning")))
	// A project Dispatch does not hold at all.
	rig.publish("GHOST", change(1, "GHOST-1", moved("phase", "planning")))
	rig.publish("LINUX", change(2, "LINUX-1", moved("phase", "implementing")))
	rig.publish("LINUX", change(3, "LINUX-1", moved("phase", "testing")))
	rig.settled()

	for _, project := range []string{"LINUX", "GHOST"} {
		if rows := observations(t, rig.database, project); len(rows) != 0 {
			t.Errorf("%s's observations = %v, want none", project, rows)
		}
		if reads := readRecords(t, rig.database, project); len(reads) != 0 {
			t.Errorf("%s's legion_reads = %v, want none", project, reads)
		}
	}
	var cursors int
	if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from legion_event_cursors`).Scan(&cursors); err != nil {
		t.Fatalf("count the cursors: %v", err)
	}
	if cursors != 0 {
		t.Errorf("%d cursors, want none", cursors)
	}

	// Each drop is logged after its message is acknowledged. LINUX's runner takes its messages in
	// turn, so once the third is acknowledged the first two have logged what they log.
	logged := func() map[string]int {
		counts := map[string]int{}
		for _, line := range rig.logRecords(droppedLine) {
			counts[fmt.Sprint(line["project"])]++
		}
		return counts
	}
	var counts map[string]int
	rig.waitFor(5*time.Second, func() bool {
		counts = logged()
		return counts["LINUX"] > 0 && counts["GHOST"] > 0
	}, func() string { return fmt.Sprintf("drops logged %v, want LINUX and GHOST", counts) })
	if want := map[string]int{"LINUX": 1, "GHOST": 1}; !maps.Equal(counts, want) {
		t.Fatalf("drops logged %v, want %v: one line per project inside dropLogEvery", counts, want)
	}

	rig.clock.advance(dropLogEvery)
	rig.publish("LINUX", change(4, "LINUX-1", moved("phase", "reviewing")))
	rig.settled()
	rig.waitFor(5*time.Second, func() bool {
		counts = logged()
		return counts["LINUX"] >= 2
	}, func() string {
		return fmt.Sprintf("drops logged %v, want a second LINUX line once dropLogEvery passed", counts)
	})
	if want := map[string]int{"LINUX": 2, "GHOST": 1}; !maps.Equal(counts, want) {
		t.Errorf("drops logged %v, want %v", counts, want)
	}
	rig.intake.mu.Lock()
	var remembered []string
	for key := range rig.intake.reported {
		if key.line == droppedLine {
			remembered = append(remembered, key.project)
		}
	}
	rig.intake.mu.Unlock()
	slices.Sort(remembered)
	if !slices.Equal(remembered, []string{"LINUX"}) {
		t.Errorf("the intake remembers the drops of %v, want LINUX alone: GHOST's entry is older than dropLogEvery", remembered)
	}
}

// TestTheIntakePrunesTheReadsAtMostOnceAnHour: the intake prunes legion_reads (PruneReads) after a
// batch it applied, and then not again until pruneEvery has passed on its own clock. A read past
// ReadRetention written after a prune stays through the batches inside pruneEvery, and goes at the
// first batch after it.
func TestTheIntakePrunesTheReadsAtMostOnceAnHour(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	aged := func() int64 {
		t.Helper()
		var id int64
		if err := rig.database.Pool.QueryRow(t.Context(), `
			insert into legion_reads (project, read_at, ok, bytes) values ('INFRA', $1, true, 1) returning id
		`, time.Now().Add(-ReadRetention-time.Hour)).Scan(&id); err != nil {
			t.Fatalf("write a read past the retention: %v", err)
		}
		return id
	}
	held := func(id int64) bool {
		t.Helper()
		var exists bool
		if err := rig.database.Pool.QueryRow(t.Context(),
			`select exists (select 1 from legion_reads where id = $1)`, id).Scan(&exists); err != nil {
			t.Fatalf("read legion_reads: %v", err)
		}
		return exists
	}
	prunedBy := func(id int64, what string) {
		t.Helper()
		rig.waitFor(10*time.Second, func() bool { return !held(id) }, func() string {
			return fmt.Sprintf("the read past the retention is still held after %s, want it pruned", what)
		})
	}

	first := aged()
	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
	rig.settled()
	prunedBy(first, "the first batch")

	// The runner takes INFRA's messages in turn and prunes after a batch is acknowledged, so once
	// 13 is acknowledged, 12's prune has been decided.
	second := aged()
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "testing")))
	rig.publish("INFRA", change(13, "INFRA-1", moved("phase", "reviewing")))
	rig.settled()
	if !held(second) {
		t.Fatal("a batch inside pruneEvery of the last prune pruned again, want no prune until pruneEvery has passed")
	}

	rig.clock.advance(pruneEvery)
	rig.publish("INFRA", change(14, "INFRA-1", moved("phase", "accepting")))
	rig.settled()
	prunedBy(second, "the first batch past pruneEvery")
}

// TestAnApplyThatFailsIsRetriedBeforeAnyLaterMessage: a batch the database refuses is tried again
// in place, and the project's later message waits behind it, so once the database takes the
// writes again both are applied in sequence order and the history holds each value the daemon
// committed.
func TestAnApplyThatFailsIsRetriedBeforeAnyLaterMessage(t *testing.T) {
	first, last := applyFirstRetry, applyLastRetry
	applyFirstRetry, applyLastRetry = 20*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { applyFirstRetry, applyLastRetry = first, last })
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	if _, err := rig.database.Pool.Exec(t.Context(), `
		create function refuse_legion_events() returns trigger language plpgsql as $$
		begin
			raise exception 'the test refuses every batch of state events';
		end;
		$$;
		create trigger refuse_legion_events before insert on legion_reads
			for each row when (new.kind = 'events') execute function refuse_legion_events();
	`); err != nil {
		t.Fatalf("refuse the batches: %v", err)
	}
	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "testing")))
	retried := func() int {
		count := 0
		for _, line := range rig.logRecords("legion intake: a database step failed; trying it again") {
			if line["project"] == "INFRA" {
				count++
			}
		}
		return count
	}
	rig.waitFor(20*time.Second, func() bool { return retried() >= 3 }, func() string {
		return fmt.Sprintf("the refused batch was tried %d times, want it tried again in place", retried())
	})
	if phase := currentValues(t, rig.database, "INFRA")["INFRA-1/phase"]; phase != `"planning"` {
		t.Fatalf("the phase is %s while the batches are refused, want the read's planning", phase)
	}
	requireLastSeq(t, rig.database, "INFRA", 10)

	if _, err := rig.database.Pool.Exec(t.Context(), `drop trigger refuse_legion_events on legion_reads`); err != nil {
		t.Fatalf("take the batches again: %v", err)
	}
	rig.settled()
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); !slices.Equal(got, []string{`"planning"`, `"implementing"`, `"testing"`}) {
		t.Errorf("the phase's history is %v, want the read's planning, then 11's implementing, then 12's testing", got)
	}
	requireLastSeq(t, rig.database, "INFRA", 12)
	events := 0
	for _, row := range readRecords(t, rig.database, "INFRA") {
		if row.kind == readKindEvents {
			events++
		}
	}
	if events != 2 {
		t.Errorf("%d batches recorded, want the two messages once each", events)
	}
}

// goldenStep is one step of the state lane's golden: what the daemon did; the one message its lane
// published for it, with the run, the last sequence and the commit time its payload names; and the
// state document the daemon served after it.
type goldenStep struct {
	name     string
	envelope contracts.Envelope
	run      string
	seq      int64
	at       time.Time
	state    json.RawMessage
}

// readStateEventsGolden reads the Legion daemon's golden of its state lane, state-events.json,
// which the daemon's own TestStateEventsGolden writes
// (packages/daemon/internal/daemon/state_events_golden_test.go): a daemon of project LEGION
// driven step by step over a seeded store, each step with every message its state lane published,
// as the Envoy listener's publish route stores the envelope, and the document GET /legion/v1/state
// served once they were published. It fails unless each step is one message and a state whose
// stateSeq is that message's last sequence: one committed transaction, and the state a whole read
// made right after it reads.
func readStateEventsGolden(t *testing.T) []goldenStep {
	t.Helper()
	raw, err := os.ReadFile(daemonAPIFixture("state-events.json"))
	if err != nil {
		t.Fatalf("read the daemon's state events golden: %v", err)
	}
	var golden struct {
		Steps []struct {
			Name     string               `json:"name"`
			Messages []contracts.Envelope `json:"messages"`
			State    json.RawMessage      `json:"state"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("decode the daemon's state events golden: %v", err)
	}
	if len(golden.Steps) == 0 {
		t.Fatal("the daemon's state events golden holds no step, so there is nothing to compare")
	}
	steps := make([]goldenStep, 0, len(golden.Steps))
	for _, step := range golden.Steps {
		if len(step.Messages) != 1 {
			t.Fatalf("step %q holds %d messages, want one: a step's state is compared with the rows of one committed transaction", step.Name, len(step.Messages))
		}
		var message struct {
			Run string    `json:"run"`
			Seq int64     `json:"seq"`
			At  time.Time `json:"at"`
		}
		if err := json.Unmarshal([]byte(step.Messages[0].Payload), &message); err != nil {
			t.Fatalf("decode the message of step %q: %v", step.Name, err)
		}
		var state struct {
			Daemon struct {
				StateSeq int64 `json:"stateSeq"`
			} `json:"daemon"`
		}
		if err := json.Unmarshal(step.State, &state); err != nil {
			t.Fatalf("decode the state of step %q: %v", step.Name, err)
		}
		if state.Daemon.StateSeq != message.Seq {
			t.Fatalf("the state of step %q serves stateSeq %d and its message ends at %d: the state must hold the events up to the message's last and none after it",
				step.Name, state.Daemon.StateSeq, message.Seq)
		}
		steps = append(steps, goldenStep{
			name: step.Name, envelope: step.Messages[0], run: message.Run, seq: message.Seq, at: message.At, state: step.State,
		})
	}
	return steps
}

// observedRow is one legion_observations row: its issue and field, its value as jsonb writes it,
// and when it was observed.
type observedRow struct {
	issue, field, value string
	at                  time.Time
}

func (r observedRow) String() string {
	return fmt.Sprintf("%s/%s=%s@%s", r.issue, r.field, r.value, r.at.UTC().Format(time.RFC3339Nano))
}

// observedRows is project's legion_observations, by issue, field and time.
func observedRows(t *testing.T, database *store.Store, project string) []observedRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select issue_key, field, value::text, observed_at from legion_observations
		where project = $1 order by issue_key, field, observed_at
	`, project)
	if err != nil {
		t.Fatalf("read the observations of %s: %v", project, err)
	}
	listed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (observedRow, error) {
		var observed observedRow
		err := row.Scan(&observed.issue, &observed.field, &observed.value, &observed.at)
		return observed, err
	})
	if err != nil {
		t.Fatalf("read the observations of %s: %v", project, err)
	}
	return listed
}

// rowsOutside is each of rows that others does not hold: the same issue, field, value and time.
func rowsOutside(rows, others []observedRow) []observedRow {
	held := make(map[string]bool, len(others))
	for _, row := range others {
		held[row.String()] = true
	}
	var outside []observedRow
	for _, row := range rows {
		if !held[row.String()] {
			outside = append(outside, row)
		}
	}
	return outside
}

// currentDifference is each issue and field whose legion_current row differs between two
// projects' currentValues, in key order.
func currentDifference(events, reads map[string]string) []string {
	keys := slices.Collect(maps.Keys(events))
	for key := range reads {
		if _, held := events[key]; !held {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	text := func(values map[string]string, key string) string {
		if value, held := values[key]; held {
			return value
		}
		return "no row"
	}
	var differing []string
	for _, key := range keys {
		if fromEvents, fromReads := text(events, key), text(reads, key); fromEvents != fromReads {
			differing = append(differing, fmt.Sprintf("%s: %s from the events, %s from the reads", key, fromEvents, fromReads))
		}
	}
	return differing
}

// orNone is *value's text, or "none" for nil.
func orNone[T any](value *T) string {
	if value == nil {
		return "none"
	}
	return fmt.Sprint(*value)
}

// TestEventsWriteTheRowsAWholeReadOfTheSameStatesWrites: the rows the daemon's state events write
// are the rows a whole read of the same states writes. The daemon's own golden of its state lane
// (readStateEventsGolden) is applied twice over. Project LEGION, the golden's own, takes each
// step's message through the intake from a real NATS stream, published as the daemon's listener
// stored it; project READS's daemon serves each step's state document, the daemon block naming
// READS, and the intake's own whole read (Intake.readWhole) records it. After each step the two
// hold the same legion_current, `null` rows included, and the same history: a row of LEGION only
// where READS has one, with the same value, at the same moment, which is the message's `at`.
//
// Both start from the golden's first state: the intake's starting read of LEGION's daemon is served
// it, and READS's first read reads it. The intake's clock stands at the commit time of the step it
// applies (its message's `at`), the moment READS's read of the step's state is made at, so the
// starting read is stamped at the boot's commit. A batch's row is stamped 1 µs after its field's
// newest row, and a whole read's rows 1 µs after the project's newest row, where that is later
// than their own time (recordEvents, recordObservations), but each step of the golden commits a
// minute after the one before, so that rule moves no row of either project here: every row a step
// writes is at its message's `at`, and the test fails on one that is not.
//
// The boot's message is at the starting read's stateSeq, under the run the first state serves as
// daemon.stateRun, which the starting read takes as the cursor's run (recordWhole): the intake
// knows the boot as one the read already holds, and reads nothing more (stateMessage.unfollowed).
// LEGION's daemon is read once, at the start, and each step's rows are its message's.
func TestEventsWriteTheRowsAWholeReadOfTheSameStatesWrites(t *testing.T) {
	const events, reads = "LEGION", "READS"
	steps := readStateEventsGolden(t)
	rig := newIntakeRig(t)
	var clock atomic.Pointer[time.Time]
	clock.Store(&steps[0].at)
	daemon := newFakeDaemon(t, string(steps[0].state))
	seedLegionProject(t, rig.database, events, daemon.URL)
	rig.startOn(func() time.Time { return *clock.Load() })
	rig.awaitStartingRead(events)
	// READS has a setting too, written once the take-over has read the settings, and nothing
	// queues a starting point of it (no take-over, no settings wake) nor delivers it a message, so
	// its daemon is read only where the test reads it, on the test's goroutine.
	readsDaemon := newFakeDaemon(t, `{}`)
	seedLegionProject(t, rig.database, reads, readsDaemon.URL)
	readsSetting := Settings{Project: reads, StateURL: readsDaemon.URL}

	wholeReads := func() []string {
		var reasons []string
		for _, line := range rig.logRecords(wholeReadLine) {
			if line["project"] != events {
				continue
			}
			if line["ok"] != true {
				t.Fatalf("a whole read of %s failed: %v", events, line)
			}
			reasons = append(reasons, fmt.Sprint(line["reason"]))
		}
		return reasons
	}
	var before []observedRow
	var firstReads []string
	var firstServed, fromEvents int
	for index, step := range steps {
		name := fmt.Sprintf("step %d (%s)", index+1, step.name)
		clock.Store(&step.at)
		daemon.serves(string(step.state))
		if err := rig.publisher.Publish(step.envelope); err != nil {
			t.Fatalf("%s: publish its message: %v", name, err)
		}
		rig.settled()
		readsDaemon.serves(stateOfProject(t, step.state, reads))
		var pending *wholeRead
		if _, err := rig.intake.readWhole(t.Context(), readOf(readsSetting, ReadReasonStart), &pending); err != nil {
			t.Fatalf("%s: read its state whole for %s: %v", name, reads, err)
		}
		if last := readRecords(t, rig.database, reads); len(last) == 0 || !last[len(last)-1].ok || !last[len(last)-1].readAt.Equal(step.at) {
			t.Fatalf("%s: the whole reads of %s are %v, want a good one at the step's %s", name, reads, last, step.at.Format(time.RFC3339Nano))
		}

		if differing := currentDifference(currentValues(t, rig.database, events), currentValues(t, rig.database, reads)); len(differing) > 0 {
			t.Fatalf("%s: legion_current differs between the events and the reads:\n%s", name, strings.Join(differing, "\n"))
		}
		eventsRows, readsRows := observedRows(t, rig.database, events), observedRows(t, rig.database, reads)
		if onlyEvents, onlyReads := rowsOutside(eventsRows, readsRows), rowsOutside(readsRows, eventsRows); len(onlyEvents)+len(onlyReads) > 0 {
			t.Fatalf("%s: the histories differ\nrows only the events wrote: %v\nrows only the reads wrote: %v", name, onlyEvents, onlyReads)
		}
		written := rowsOutside(eventsRows, before)
		for _, row := range written {
			if !row.at.Equal(step.at) {
				t.Errorf("%s: the row %s is not at the message's at %s", name, row, step.at.Format(time.RFC3339Nano))
			}
		}
		before = eventsRows
		current := cursorOf(t, rig.database, events)
		if current.lastSeq == nil || *current.lastSeq != step.seq || current.readOwed {
			t.Fatalf("%s: the cursor is at %s with a read owed %v, want it at %d with none owed", name, orNone(current.lastSeq), current.readOwed, step.seq)
		}
		if current.run == nil || *current.run != step.run {
			t.Fatalf("%s: the cursor follows the run %s, want the message's %s", name, orNone(current.run), step.run)
		}

		if index == 0 {
			firstReads, firstServed = wholeReads(), daemon.served()
			if firstServed != 1 || !slices.Equal(firstReads, []string{ReadReasonStart}) {
				t.Fatalf("%s: the daemon served %d reads and the whole reads logged are %v, want the starting read alone: the boot is of the run the starting read served, at its stateSeq",
					name, firstServed, firstReads)
			}
			t.Logf("%s: seq %d at %s; the whole reads %v wrote %d rows", name, step.seq, step.at.Format(time.RFC3339Nano), firstReads, len(written))
			continue
		}
		if served, reasons := daemon.served(), wholeReads(); served != firstServed || !slices.Equal(reasons, firstReads) {
			t.Fatalf("%s: the daemon served %d reads and the whole reads logged are %v, want the first step's %d and %v: a step's rows must be its message's",
				name, served, reasons, firstServed, firstReads)
		}
		var batches int
		if err := rig.database.Pool.QueryRow(t.Context(), `
			select count(*) from legion_reads where project = $1 and kind = $2 and read_at = $3
		`, events, readKindEvents, step.at).Scan(&batches); err != nil {
			t.Fatalf("%s: count its batches: %v", name, err)
		}
		if batches != 1 {
			t.Fatalf("%s: %d batches of events are recorded at its message's at, want one", name, batches)
		}
		fromEvents += len(written)
		t.Logf("%s: seq %d at %s; its message wrote %d rows", name, step.seq, step.at.Format(time.RFC3339Nano), len(written))
	}

	if fromEvents == 0 {
		t.Error("the messages wrote no row, so nothing past the starting point was compared")
	}
	if !slices.Contains(slices.Collect(maps.Values(currentValues(t, rig.database, events))), "null") {
		t.Error("legion_current holds no null row, so no null row was compared")
	}
	if served := readsDaemon.served(); served != len(steps) {
		t.Errorf("the daemon of %s served %d reads, want one per step, %d: its rows must be its whole reads' alone", reads, served, len(steps))
	}
}

// stateOfProject is state, a state document of the golden's daemon, with its daemon block naming
// project, so a whole read made for project takes it as its own; every other member is as the
// daemon served it.
func stateOfProject(t *testing.T, state json.RawMessage, project string) string {
	t.Helper()
	var document, daemon map[string]json.RawMessage
	if err := json.Unmarshal(state, &document); err != nil {
		t.Fatalf("decode the state: %v", err)
	}
	if err := json.Unmarshal(document["daemon"], &daemon); err != nil {
		t.Fatalf("decode the state's daemon block: %v", err)
	}
	daemon["project"], _ = json.Marshal(project)
	var err error
	if document["daemon"], err = json.Marshal(daemon); err != nil {
		t.Fatalf("encode the state's daemon block: %v", err)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode the state: %v", err)
	}
	return string(encoded)
}

// heldPartCount counts project's held parts of split transactions (legion_event_parts).
func heldPartCount(t *testing.T, database *store.Store, project string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from legion_event_parts where project = $1`, project).Scan(&count); err != nil {
		t.Fatalf("count the held parts of %s: %v", project, err)
	}
	return count
}

// wholeReadReasons is the reason of each whole read logged for project, in order, each a good one
// unless the test fails.
func (r *intakeRig) wholeReadReasons(project string) []string {
	r.t.Helper()
	var reasons []string
	for _, line := range r.logRecords(wholeReadLine) {
		if line["project"] != project {
			continue
		}
		if line["ok"] != true {
			r.t.Fatalf("a whole read of %s failed: %v", project, line)
		}
		reasons = append(reasons, fmt.Sprint(line["reason"]))
	}
	return reasons
}

// TestAMessageOfAnotherStoreWritesNothingAndMakesOneRead: R1 applies only a message whose stateId
// is the starting point's daemon.stateId. A message of another store (a daemon the setting no
// longer names, or a sender that is no daemon) writes no observation and moves no cursor, though it
// is at the cursor's next sequence. The starting read's minute is over (awaitStartingRead), so it
// makes one whole read (ReadReasonStore) at once, logged with its reason, which finds the store the
// daemon counts in now. The daemon still serves the starting point's store, so the read marks the
// message's store as no daemon of INFRA (foreign_state_ids, row 3.1 of the read rule's cases), the
// message is dropped, and the drop is logged once, with both ids and the message's sequences. A
// second message of that store inside ReadRetryAfter of the read is dropped the same way: it makes
// no read and holds none, and the daemon's own next message is applied.
func TestAMessageOfAnotherStoreWritesNothingAndMakesOneRead(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	rows := observations(t, rig.database, "INFRA")

	at := time.Now().UTC().Truncate(time.Microsecond)
	rig.publishIn("INFRA", rig.run, otherStateID, at, false, change(11, "INFRA-1", moved("phase", "merging")))
	rig.publishIn("INFRA", rig.run, otherStateID, at, false, change(12, "INFRA-1", moved("phase", "merging")))
	rig.settled()

	if after := observations(t, rig.database, "INFRA"); !slices.Equal(after, rows) {
		t.Errorf("observations = %v, want the starting read's %v alone: a message of another store writes nothing", after, rows)
	}
	if current := cursorOf(t, rig.database, "INFRA"); current.readOwed || !slices.Equal(current.foreignStateIDs, []string{otherStateID}) ||
		current.lastSeq == nil || *current.lastSeq != 10 {
		t.Errorf("the cursor is %+v, want it at the starting read's 10 with no read owed and the other store marked as no daemon of INFRA", current)
	}
	reads := readRecords(t, rig.database, "INFRA")
	if len(reads) != 2 {
		t.Fatalf("legion_reads = %v, want the starting read and the one read of the other store's first message", reads)
	}
	requireWholeRead(t, reads[1], ReadReasonStore, true)
	if served := daemon.served(); served != 2 {
		t.Errorf("the daemon served %d reads, want the starting one and one for the other store", served)
	}
	lines := rig.requireWholeReadLines("INFRA", ReadReasonStart, ReadReasonStore)
	if lines[1]["stateSeq"] != float64(10) || lines[1]["stateId"] != rigStateID {
		t.Errorf("the store's read logged %v, want stateSeq 10 and the daemon's store %s", lines[1], rigStateID)
	}
	drop := rig.awaitLogRecords(otherStoreLine, 1)[0]
	if drop["project"] != "INFRA" || drop["stateId"] != otherStateID || drop["cursorStateId"] != rigStateID ||
		drop["run"] != rig.run || drop["firstSeq"] != float64(11) || drop["seq"] != float64(11) {
		t.Errorf("the drop's line = %v, want INFRA, both stores and the first message's run and sequences", drop)
	}

	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
	rig.settled()
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); !slices.Equal(got, []string{`"planning"`, `"implementing"`}) {
		t.Errorf("the phase's history is %v, want the starting read's planning, then the daemon's own message's implementing", got)
	}
	requireLastSeq(t, rig.database, "INFRA", 11)
	if served := daemon.served(); served != 2 {
		t.Errorf("the daemon served %d reads, want no more: the second message of the other store came inside ReadRetryAfter", served)
	}
	if drops := rig.logRecords(otherStoreLine); len(drops) != 1 {
		t.Errorf("logged %d drops of the other store %v, want one per dropLogEvery", len(drops), drops)
	}
}

// TestAMessagePastTheSequenceOfTheReadItCalledForWritesNothing: R1 applies no message whose
// sequence is beyond the daemon's own stateSeq, as a whole read finds it, plus what the stream has
// delivered since. The starting read is at 10, and the stream delivers 11. A message at 13 is a
// gap, so it makes one whole read (ReadReasonGap), logged with its reason and the sequence it
// read. The daemon serves 11, and it commits each event before it publishes it, so the message is
// no event of the daemon: it writes nothing and moves no cursor, and the drop is logged with its
// run, its sequences and the read's stateSeq. The daemon's next event, 12, is applied.
func TestAMessagePastTheSequenceOfTheReadItCalledForWritesNothing(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
	rig.settled()

	daemon.serves(daemonState("INFRA", 11, strings.Replace(planning("INFRA"), `"planning"`, `"implementing"`, 1)))
	rig.publish("INFRA", change(13, "INFRA-1", moved("phase", "merging")))
	rig.settled()

	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); !slices.Equal(got, []string{`"planning"`, `"implementing"`}) {
		t.Errorf("the phase's history is %v, want planning and implementing: the message past the read's sequence writes nothing", got)
	}
	requireLastSeq(t, rig.database, "INFRA", 11)
	reads := readRecords(t, rig.database, "INFRA")
	if len(reads) != 3 {
		t.Fatalf("legion_reads = %v, want the starting read, 11's batch and the gap's read", reads)
	}
	requireWholeRead(t, reads[2], ReadReasonGap, true)
	lines := rig.requireWholeReadLines("INFRA", ReadReasonStart, ReadReasonGap)
	if lines[1]["stateSeq"] != float64(11) {
		t.Errorf("the gap's read logged stateSeq %v, want 11", lines[1]["stateSeq"])
	}
	drop := rig.awaitLogRecords(beyondLine, 1)[0]
	if drop["project"] != "INFRA" || drop["run"] != rig.run || drop["firstSeq"] != float64(13) ||
		drop["seq"] != float64(13) || drop["stateSeq"] != float64(11) {
		t.Errorf("the drop's line = %v, want INFRA, the message's run and sequences 13, and the read's stateSeq 11", drop)
	}

	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "testing")))
	rig.settled()
	if phase := currentValues(t, rig.database, "INFRA")["INFRA-1/phase"]; phase != `"testing"` {
		t.Errorf("the phase is %s, want the daemon's next event's testing", phase)
	}
	requireLastSeq(t, rig.database, "INFRA", 12)
	if served := daemon.served(); served != 2 {
		t.Errorf("the daemon served %d reads, want the starting one and the gap's", served)
	}
}

// TestNoStartingPointIsTakenWhileAnEarlierBuildIsInTheFleet: R3 takes a project's first starting
// point only once no Dispatch process of an earlier build is in the fleet, since such a build's
// sampler reads every daemon whole each minute and its rows could land after the starting point. A
// server_processes row of the build before this one (legion_state_intake at its default, 0) keeps
// the take-over's starting point from being taken and the next message's too: the message writes
// nothing, makes no cursor and is finished, and the line is logged once. Once that process has
// deregistered, the next message takes the starting point and is applied on it.
func TestNoStartingPointIsTakenWhileAnEarlierBuildIsInTheFleet(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	// A process of the build before this one: it writes the owner lock protocol and no Legion
	// state intake.
	var earlier string
	if err := rig.database.Pool.QueryRow(t.Context(), `
		insert into server_processes (instance, commit, dashboard_build, signing_keys, owner_lock_protocol)
		values (gen_random_uuid(), 'c0', 'build-c0', $1, 1)
		returning instance::text`, []string{"k0"}).Scan(&earlier); err != nil {
		t.Fatalf("register a process of the build before this one: %v", err)
	}

	rig.start()
	rig.awaitLogRecords(earlierBuildLine, 1)
	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
	rig.settled()
	if served := daemon.served(); served != 0 {
		t.Errorf("the daemon served %d reads while an earlier build is in the fleet, want none", served)
	}
	if reads := readRecords(t, rig.database, "INFRA"); len(reads) != 0 {
		t.Errorf("legion_reads = %v, want none", reads)
	}
	if rows := observations(t, rig.database, "INFRA"); len(rows) != 0 {
		t.Errorf("observations = %v, want none", rows)
	}
	var cursors int
	if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from legion_event_cursors`).Scan(&cursors); err != nil {
		t.Fatalf("count the cursors: %v", err)
	}
	if cursors != 0 {
		t.Errorf("%d cursors, want none: no starting point is taken", cursors)
	}
	if lines := rig.logRecords(earlierBuildLine); len(lines) != 1 || lines[0]["project"] != "INFRA" {
		t.Errorf("logged %v, want one line for INFRA per dropLogEvery", lines)
	}

	if _, err := rig.database.Pool.Exec(t.Context(), `delete from server_processes where instance = $1`, earlier); err != nil {
		t.Fatalf("deregister the process of the earlier build: %v", err)
	}
	daemon.serves(daemonState("INFRA", 11, strings.Replace(planning("INFRA"), `"planning"`, `"implementing"`, 1)))
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "testing")))
	rig.settled()
	if reasons := rig.wholeReadReasons("INFRA"); !slices.Equal(reasons, []string{ReadReasonStart}) {
		t.Errorf("whole reads %v, want the starting point the first message after the fleet cleared took", reasons)
	}
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); !slices.Equal(got, []string{`"implementing"`, `"testing"`}) {
		t.Errorf("the phase's history is %v, want the starting read's implementing, then the message's testing", got)
	}
	requireLastSeq(t, rig.database, "INFRA", 12)
}

// TestASplitTransactionIsAppliedOnlyWhole: R3 applies a transaction the daemon split over several
// messages only once every part has arrived. The parts with `more` write no observation, no
// legion_reads row and no move of the cursor, and a part delivered again is held once. The last
// part applies every event of the transaction in one batch: all of its rows at once, at the
// transaction's commit time, one legion_reads row, the cursor at the last part's sequence, and no
// part held after it.
func TestASplitTransactionIsAppliedOnlyWhole(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	rows := observations(t, rig.database, "INFRA")

	at := time.Now().UTC().Truncate(time.Microsecond)
	first := []map[string]any{change(11, "INFRA-1", moved("phase", "implementing")), change(12, "INFRA-1", claimMoved("implementer", "launching"))}
	rig.publishIn("INFRA", rig.run, rigStateID, at, true, first...)
	rig.publishIn("INFRA", rig.run, rigStateID, at, true, first...)
	rig.publishIn("INFRA", rig.run, rigStateID, at, true, change(13, "INFRA-1", moved("slot", nil)))
	rig.settled()
	if after := observations(t, rig.database, "INFRA"); !slices.Equal(after, rows) {
		t.Errorf("observations before the last part = %v, want the starting read's %v alone", after, rows)
	}
	if reads := readRecords(t, rig.database, "INFRA"); len(reads) != 1 {
		t.Errorf("legion_reads before the last part = %v, want the starting read alone", reads)
	}
	requireLastSeq(t, rig.database, "INFRA", 10)
	if held := heldPartCount(t, rig.database, "INFRA"); held != 2 {
		t.Errorf("%d parts held before the last part, want the two parts, the first one once", held)
	}

	rig.publishIn("INFRA", rig.run, rigStateID, at, false, change(14, "INFRA-1", claimMoved("planner", nil)))
	rig.settled()
	var written []string
	for _, row := range observedRows(t, rig.database, "INFRA") {
		if row.at.Equal(at) {
			written = append(written, row.issue+"/"+row.field+"="+row.value)
		}
	}
	slices.Sort(written)
	if want := []string{`INFRA-1/claim.implementer="launching"`, `INFRA-1/claim.planner=null`, `INFRA-1/phase="implementing"`, `INFRA-1/slot=null`}; !slices.Equal(written, want) {
		t.Errorf("the transaction wrote %v at its commit time, want %v", written, want)
	}
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); !slices.Equal(got, []string{`"planning"`, `"implementing"`}) {
		t.Errorf("the phase's history is %v, want the read's planning and the transaction's implementing once", got)
	}
	reads := readRecords(t, rig.database, "INFRA")
	if len(reads) != 2 || reads[1].kind != readKindEvents || !reads[1].readAt.Equal(at) || reads[1].bytes == 0 {
		t.Fatalf("legion_reads = %v, want the starting read and one `events` row at the transaction's commit time", reads)
	}
	requireLastSeq(t, rig.database, "INFRA", 14)
	if held := heldPartCount(t, rig.database, "INFRA"); held != 0 {
		t.Errorf("%d parts held after the last part, want none", held)
	}
	if served := daemon.served(); served != 1 {
		t.Errorf("the daemon served %d reads, want the starting one alone", served)
	}
}

// TestAPartOfASplitTransactionThatIsNotCompletedWritesNothing: the parts of a split transaction
// the intake holds are a transaction it cannot complete once a message does not continue them: a
// gap after them, or a part of another run. Either makes one whole read (ReadReasonGap), which
// deletes the parts, and no row ever holds a value of the parts.
func TestAPartOfASplitTransactionThatIsNotCompletedWritesNothing(t *testing.T) {
	const otherRun = "e4b8a2d6-0c7f-4a1e-8d35-9f6b2c1a7e04"
	for _, tc := range []struct {
		name string
		// next is the message after the part, and stateSeq what the daemon serves at its read.
		next     func(rig *intakeRig, at time.Time)
		stateSeq int
	}{
		{
			name: "a gap after the part",
			next: func(rig *intakeRig, at time.Time) {
				rig.publishIn("INFRA", rig.run, rigStateID, at, false, change(14, "INFRA-1", moved("phase", "testing")))
			},
			stateSeq: 14,
		},
		{
			name: "a part of another run",
			next: func(rig *intakeRig, at time.Time) {
				rig.publishIn("INFRA", otherRun, rigStateID, at, false, change(13, "INFRA-1", moved("phase", "testing")))
			},
			stateSeq: 13,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIntakeRig(t)
			daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
			seedLegionProject(t, rig.database, "INFRA", daemon.URL)
			rig.start()
			rig.awaitStartingRead("INFRA")

			at := time.Now().UTC().Truncate(time.Microsecond)
			rig.publishIn("INFRA", rig.run, rigStateID, at, true,
				change(11, "INFRA-1", moved("phase", "merging")), change(12, "INFRA-1", claimMoved("implementer", "launching")))
			rig.settled()
			daemon.serves(daemonState("INFRA", tc.stateSeq, strings.Replace(planning("INFRA"), `"planning"`, `"testing"`, 1)))
			tc.next(rig, at.Add(time.Second))
			rig.settled()

			for _, value := range history(t, rig.database, "INFRA", "INFRA-1", FieldPhase) {
				if value == `"merging"` {
					t.Errorf("the phase's history %v holds the part's merging", history(t, rig.database, "INFRA", "INFRA-1", FieldPhase))
				}
			}
			if claim, held := currentValues(t, rig.database, "INFRA")["INFRA-1/claim.implementer"]; held {
				t.Errorf("the implementer's claim is %s, want none: only the part named it", claim)
			}
			if phase := currentValues(t, rig.database, "INFRA")["INFRA-1/phase"]; phase != `"testing"` {
				t.Errorf("the phase is %s, want the read's testing", phase)
			}
			reads := readRecords(t, rig.database, "INFRA")
			if len(reads) != 2 {
				t.Fatalf("legion_reads = %v, want the starting read and the gap's", reads)
			}
			requireWholeRead(t, reads[1], ReadReasonGap, true)
			rig.requireWholeReadLines("INFRA", ReadReasonStart, ReadReasonGap)
			requireLastSeq(t, rig.database, "INFRA", int64(tc.stateSeq))
			if held := heldPartCount(t, rig.database, "INFRA"); held != 0 {
				t.Errorf("%d parts held after the gap's read, want none", held)
			}
		})
	}
}

// TestAMessageTheDatabaseRefusesForGoodIsFinishedAndReadsItsProjectWhole: R3 finishes a message
// the database refuses for good, which no retry can write, rather than trying it again in place.
// Here it is a wait whose text carries a lone surrogate escape, `\ud800`: Dispatch keeps a wait as
// the bytes the message carried, and Postgres's jsonb refuses it as no JSON (SQLSTATE 22P02, class
// 22, a data exception). The refusal is logged with the project, the sequences and the SQLSTATE,
// the message is finished, and one whole read of the project (ReadReasonRefused) puts its rows at
// the daemon's state. INFRA's next message is applied after it, and LINUX's message, published
// behind the refused one, is applied meanwhile.
func TestAMessageTheDatabaseRefusesForGoodIsFinishedAndReadsItsProjectWhole(t *testing.T) {
	rig := newIntakeRig(t)
	infra := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	linux := newFakeDaemon(t, daemonState("LINUX", 20, planning("LINUX")))
	seedLegionProject(t, rig.database, "INFRA", infra.URL)
	seedLegionProject(t, rig.database, "LINUX", linux.URL)
	rig.start()
	rig.awaitWholeReads(2)
	// The starting reads' minute passes, so the read a message calls for is due at once.
	rig.clock.advance(ReadRetryAfter + time.Second)

	// The daemon's state once the event committed, as its own encoder writes the wait.
	infra.serves(daemonState("INFRA", 11, `"INFRA-1":{"key":"INFRA-1","phase":"awaiting_person","tree":"INFRA-1",
		"waitingOn":{"kind":"ask","detail":"which name"}}`))
	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "awaiting_person"),
		moved("waitingOn", json.RawMessage(`{"kind":"ask","detail":"\ud800"}`))))
	rig.publish("LINUX", change(21, "LINUX-1", moved("phase", "implementing")))
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "implementing")))
	rig.settled()

	refusal := rig.awaitLogRecords(refusedLine, 1)
	if len(refusal) != 1 || refusal[0]["level"] != "WARN" || refusal[0]["project"] != "INFRA" || refusal[0]["firstSeq"] != float64(11) ||
		refusal[0]["seq"] != float64(11) || refusal[0]["sqlstate"] != "22P02" || refusal[0]["error"] == "" {
		t.Errorf("the refusal's lines = %v, want one WARN naming INFRA, the sequences 11, SQLSTATE 22P02 and its message", refusal)
	}
	for _, line := range rig.logRecords("legion intake: a database step failed; trying it again") {
		if line["project"] == "INFRA" {
			t.Errorf("the refused message was tried again in place: %v", line)
		}
	}
	if reasons := rig.wholeReadReasons("INFRA"); !slices.Equal(reasons, []string{ReadReasonStart, ReadReasonRefused}) {
		t.Errorf("INFRA's whole reads %v, want the starting read and the refused message's one", reasons)
	}
	reads := readRecords(t, rig.database, "INFRA")
	if len(reads) != 3 {
		t.Fatalf("INFRA's legion_reads = %v, want the starting read, the refused message's read and 12's batch", reads)
	}
	requireWholeRead(t, reads[1], ReadReasonRefused, true)
	current := currentValues(t, rig.database, "INFRA")
	if wait := current["INFRA-1/"+FieldWaitingOn]; wait != `{"kind": "ask", "detail": "which name"}` {
		t.Errorf("INFRA-1's wait is %s, want the read's", wait)
	}
	if phase := current["INFRA-1/phase"]; phase != `"implementing"` {
		t.Errorf("INFRA-1's phase is %s, want the next message's implementing", phase)
	}
	requireLastSeq(t, rig.database, "INFRA", 12)
	if served := infra.served(); served != 2 {
		t.Errorf("INFRA's daemon served %d reads, want the starting read and the refused message's", served)
	}
	if phase := currentValues(t, rig.database, "LINUX")["LINUX-1/phase"]; phase != `"implementing"` {
		t.Errorf("LINUX-1's phase is %s, want its message's implementing", phase)
	}
	requireLastSeq(t, rig.database, "LINUX", 21)
}

// TestAStateMessageWhoseEventsDoNotRunFromFirstSeqToSeqDoesNotDecode: the contract's rule for a
// state message (legionStateMessage's refine) is that its events run from firstSeq to seq, one
// sequence each, in that order. A message that breaks it does not decode, whichever way it breaks
// it, and one that keeps it decodes with its events in order.
func TestAStateMessageWhoseEventsDoNotRunFromFirstSeqToSeqDoesNotDecode(t *testing.T) {
	phase := func(seq int) map[string]any { return change(seq, "INFRA-1", moved("phase", "testing")) }
	for _, tc := range []struct {
		name          string
		firstSeq, seq int64
		events        []map[string]any
		decodes       bool
	}{
		{name: "one event at each sequence", firstSeq: 11, seq: 13, events: []map[string]any{phase(11), phase(12), phase(13)}, decodes: true},
		{name: "a seq past its last event", firstSeq: 11, seq: 1 << 62, events: []map[string]any{phase(11)}},
		{name: "an event missing between the two", firstSeq: 11, seq: 13, events: []map[string]any{phase(11), phase(13)}},
		{name: "events out of order", firstSeq: 11, seq: 12, events: []map[string]any{phase(12), phase(11)}},
		{name: "one sequence twice", firstSeq: 11, seq: 12, events: []map[string]any{phase(11), phase(11)}},
		{name: "an event before firstSeq", firstSeq: 11, seq: 11, events: []map[string]any{phase(10)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{
				"project": "INFRA", "run": rigRun, "stateId": rigStateID, "firstSeq": tc.firstSeq, "seq": tc.seq,
				"at": "2026-11-19T12:00:00.000000Z", "events": tc.events,
			})
			if err != nil {
				t.Fatalf("encode the message: %v", err)
			}
			message, err := decodeStateMessage(&contracts.Envelope{Payload: string(payload)}, nil)
			if !tc.decodes {
				if err == nil || !strings.Contains(err.Error(), "one sequence each") {
					t.Fatalf("the message decoded to %+v (error %v), want it refused for its events", message, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode the message: %v", err)
			}
			var sequences []int64
			for _, event := range message.Events {
				sequences = append(sequences, event.Seq)
			}
			if !slices.Equal(sequences, []int64{11, 12, 13}) {
				t.Errorf("the events decoded at %v, want 11, 12 and 13", sequences)
			}
		})
	}
}

// TestAMessageWhoseSequenceItsEventsDoNotReachMovesNoCursor: a message with one event at the
// cursor's next sequence, the daemon's own store and run, and a seq of 2^62 breaks the contract's
// rule for its events, so it does not decode: it is acknowledged and logged, writes nothing and
// leaves the cursor where it was. Taken, it would move the cursor to 2^62, past every event the
// daemon will ever publish, with no whole read to follow. The daemon's real next message then
// follows the cursor and is applied, with no read.
func TestAMessageWhoseSequenceItsEventsDoNotReachMovesNoCursor(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	rows := observations(t, rig.database, "INFRA")

	rig.publishMessage("INFRA", map[string]any{
		"project": "INFRA", "run": rig.run, "stateId": rigStateID, "firstSeq": 11, "seq": int64(1) << 62,
		"at":     time.Now().UTC().Format("2006-01-02T15:04:05.000000Z"),
		"events": []map[string]any{change(11, "INFRA-1", moved("phase", "merging"))},
	})
	rig.settled()
	if after := observations(t, rig.database, "INFRA"); !slices.Equal(after, rows) {
		t.Errorf("observations = %v, want the starting read's %v alone: the message writes nothing", after, rows)
	}
	requireLastSeq(t, rig.database, "INFRA", 10)
	refusal := rig.awaitLogRecords("legion intake: decode a state message; acknowledging it", 1)
	if len(refusal) != 1 || refusal[0]["level"] != "ERROR" || !strings.Contains(fmt.Sprint(refusal[0]["error"]), "one sequence each") {
		t.Errorf("the refusal's lines = %v, want one ERROR naming the events' rule", refusal)
	}

	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
	rig.settled()
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); !slices.Equal(got, []string{`"planning"`, `"implementing"`}) {
		t.Errorf("the phase's history is %v, want the read's planning, then the daemon's next message's implementing", got)
	}
	requireLastSeq(t, rig.database, "INFRA", 11)
	if served := daemon.served(); served != 1 {
		t.Errorf("the daemon served %d reads, want the starting one alone", served)
	}
}

// TestARepointedSettingWhoseFirstReadFailsAppliesNoMessageOfTheDaemonItNamedBefore: R3 applies no
// message of the daemon a setting named before, once the setting names another. The write that
// names another daemon records the store the cursor held (previous_state_id). Here the first read
// of the newly named daemon fails, since it does not answer yet, so the cursor follows the new
// daemon with no store, while the daemon named before still publishes under the same project, in
// its own store and run, at its next sequences. None of its messages is applied, none gives the
// cursor its store, none holds a read, and the drop is logged once: each is finished at its first
// delivery, and inside the minute none makes a read. R3 tries a failed read again at the project's
// next message, so the one that arrives a minute on, once the owed read is due, calls for it: it
// is finished unapplied at once all the same, and the project's runner then makes the read as a
// call no message makes. The read takes the named daemon's state and store, and the named
// daemon's own next event is applied on it.
func TestARepointedSettingWhoseFirstReadFailsAppliesNoMessageOfTheDaemonItNamedBefore(t *testing.T) {
	const namedRun, namedStore = "2c7e9a14-5b3d-4f86-a1e0-8d6c4b2f9e37", "6e1f3a8c-2d4b-4c97-b5a0-9f7e1d3c8b26"
	rig := newIntakeRig(t)
	previous := newFakeDaemon(t, daemonState("INFRA", 2000000, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", previous.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	named := newFakeDaemon(t, "")
	named.refuses(http.StatusServiceUnavailable)
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", named.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("point INFRA at the named daemon: %v", err)
	}
	rig.wake <- struct{}{}
	rig.awaitWholeReads(2)
	reads := readRecords(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonStart, false)
	before := currentValues(t, rig.database, "INFRA")

	// The daemon named before publishes on, under the rig's run and store, which are its own.
	rig.publish("INFRA", change(2000001, "INFRA-1", moved("phase", "merging")))
	rig.publish("INFRA", change(2000002, "INFRA-1", moved("phase", "merging")), change(2000003, "INFRA-1", claimMoved("planner", "idle")))
	rig.settled()
	if after := currentValues(t, rig.database, "INFRA"); !maps.Equal(after, before) {
		t.Errorf("legion_current = %v, want %v: no message of the daemon the setting named before is applied", after, before)
	}
	if c := cursorOf(t, rig.database, "INFRA"); !c.readOwed || c.lastSeq != nil || c.stateID != nil || c.readHolder != nil || c.readHolds != 0 {
		t.Errorf("the cursor is %s, want no sequence, no store, the read owed and no holder", c)
	}
	if served := named.served(); served != 1 {
		t.Errorf("the named daemon served %d reads inside a minute of its failed one, want that one alone", served)
	}

	// A minute on, the owed read is due, and the named daemon answers, in a store and under a run
	// of its own. The daemon named before publishes again: its message calls for the owed read,
	// holds none, and is not applied. The project's runner makes the read after the message is
	// acknowledged.
	implementing := strings.Replace(planning("INFRA"), `"planning"`, `"implementing"`, 1)
	named.serves(strings.Replace(strings.Replace(daemonState("INFRA", 5, implementing), rigStateID, namedStore, 1), rigRun, namedRun, 1))
	rig.clock.advance(ReadRetryAfter + time.Second)
	rig.publish("INFRA", change(2000004, "INFRA-1", moved("phase", "merging")))
	rig.settled()
	rig.awaitWholeReads(3)
	if served := named.served(); served != 2 {
		t.Errorf("the named daemon served %d reads, want the failed one and the one the message a minute on called for", served)
	}
	whole := wholeReadsOf(t, rig.database, "INFRA")
	if len(whole) != 3 {
		t.Fatalf("whole reads = %v, want the first daemon's, the named daemon's failed one and its good one", whole)
	}
	requireWholeRead(t, whole[2], ReadReasonRetry, true)
	if c := cursorOf(t, rig.database, "INFRA"); c.readOwed || c.lastSeq == nil || *c.lastSeq != 5 || c.stateID == nil || *c.stateID != namedStore || c.readHolder != nil {
		t.Errorf("the cursor after the good read is %s, want the named daemon's sequence 5 and store %s, nothing owed and no holder", c, namedStore)
	}
	if drops := rig.awaitLogRecords(forgottenLine, 1); len(drops) != 1 || drops[0]["project"] != "INFRA" || drops[0]["stateId"] != rigStateID {
		t.Errorf("the drops logged = %v, want one naming INFRA and the message's store", drops)
	}
	if held := rig.logRecords(heldLine); len(held) != 0 {
		t.Errorf("the messages sent back = %v, want none: no message of the daemon the setting named before holds a read", held)
	}

	// The named daemon's own next event is applied on the read.
	rig.publishIn("INFRA", namedRun, namedStore, time.Now().UTC().Truncate(time.Microsecond), false, change(6, "INFRA-1", moved("phase", "testing")))
	rig.settled()
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); !slices.Equal(got, []string{`"planning"`, `"implementing"`, `"testing"`}) {
		t.Errorf("the phase's history is %v, want the first daemon's read's planning, the named daemon's read's implementing and its event's testing, and no merging", got)
	}
	requireLastSeq(t, rig.database, "INFRA", 6)
	if served := named.served(); served != 2 {
		t.Errorf("the named daemon served %d reads, want the failed one and the good one", served)
	}
}

// TestAFailedReadKeepsTheHeldPartsAndTheirTransactionIsAppliedWhole: R3 applies a transaction the
// daemon split over several messages whole, once every part has arrived. A whole read that fails
// holds none of the parts' events, so it keeps the parts: here a gap's read fails, and the gap goes
// back to the stream until the retry is due; the first part of a transaction that follows the
// cursor is held while the read is owed, and the retry the gap's redelivery makes a minute on fails
// as well. The transaction's last part then applies all of it, every row at its commit time, and no
// part is held after it.
func TestAFailedReadKeepsTheHeldPartsAndTheirTransactionIsAppliedWhole(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	// A gap whose read the daemon refuses: the read is owed, the gap is not applied, and it goes
	// back to the stream until the read is due.
	daemon.refuses(http.StatusServiceUnavailable)
	rig.publish("INFRA", change(15, "INFRA-1", moved("phase", "merging")))
	rig.settledHolding(1)
	if c := cursorOf(t, rig.database, "INFRA"); !c.readOwed || c.lastSeq == nil || *c.lastSeq != 10 {
		t.Fatalf("the cursor after the refused gap read is %+v, want it at 10 with the read owed", c)
	}

	at := time.Now().UTC().Truncate(time.Microsecond)
	rig.publishIn("INFRA", rig.run, rigStateID, at, true,
		change(11, "INFRA-1", moved("phase", "implementing")), change(12, "INFRA-1", claimMoved("implementer", "launching")))
	rig.settledHolding(1)
	if held := heldPartCount(t, rig.database, "INFRA"); held != 1 {
		t.Fatalf("%d parts held, want the transaction's first", held)
	}

	// A minute on, the gap's redelivery makes the owed read, and the daemon refuses it again. The
	// gap went back to the stream for that minute. A take-over creates the durable again at its ack
	// floor and delivers the gap again at once, so the test does not wait the minute out; the
	// take-over's own call for INFRA's read joins the read the gap holds and makes none.
	rig.clock.advance(ReadRetryAfter + time.Second)
	rig.stop()
	rig.start()
	rig.awaitWholeReads(3)
	reads := readRecords(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonRetry, false)
	if held := heldPartCount(t, rig.database, "INFRA"); held != 1 {
		t.Fatalf("%d parts held after the failed retry, want the first part still: a failed read holds none of its events", held)
	}

	rig.publishIn("INFRA", rig.run, rigStateID, at, false, change(13, "INFRA-1", moved("slot", nil)))
	rig.settledHolding(1)
	var written []string
	for _, row := range observedRows(t, rig.database, "INFRA") {
		if row.at.Equal(at) {
			written = append(written, row.issue+"/"+row.field+"="+row.value)
		}
	}
	slices.Sort(written)
	if want := []string{`INFRA-1/claim.implementer="launching"`, `INFRA-1/phase="implementing"`, `INFRA-1/slot=null`}; !slices.Equal(written, want) {
		t.Errorf("the transaction wrote %v at its commit time, want %v: every part of it", written, want)
	}
	requireLastSeq(t, rig.database, "INFRA", 13)
	if held := heldPartCount(t, rig.database, "INFRA"); held != 0 {
		t.Errorf("%d parts held after the last part, want none", held)
	}
	for _, value := range history(t, rig.database, "INFRA", "INFRA-1", FieldPhase) {
		if value == `"merging"` {
			t.Errorf("the phase's history %v holds the gap's merging", history(t, rig.database, "INFRA", "INFRA-1", FieldPhase))
		}
	}
}

// TestABurstOfMessagesThatCallForReadsMakesOneReadAMinute: a project's whole reads start at least
// ReadRetryAfter apart, whatever calls for them, and its messages are not held behind reads. 300
// messages of INFRA that each start past the cursor's next sequence (gaps, in the daemon's own store
// and run, as forged messages or a bug's would), and a boot among them that does not follow, make
// one whole read in the first minute after the starting read's: the first message's, made at once
// since the starting read's minute is over (awaitStartingRead). INFRA's daemon answers it late and
// holds it, and LINUX's messages are delivered and applied while it is in flight. The first
// message past stateMaxWaiting since that read began is the first that holds the next read
// (read-rule-cases row 8.1): it goes back to the stream until
// the read is due, the one message of the durable's window INFRA holds. Each later message of the
// burst is finished as soon as the cursor records its reason, so none waits on more than that one
// read and none is applied: 300 reads at the daemon's 200 ms would take a minute, past what
// rig.settledHolding waits. LINUX's message behind the burst is applied too. A minute on, INFRA's
// daemon's own next event makes the owed read, logged with both reasons and the first as its
// reason, and is applied after it: one read more.
func TestABurstOfMessagesThatCallForReadsMakesOneReadAMinute(t *testing.T) {
	const burst = 300
	rig := newIntakeRig(t)
	infra := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	linux := newFakeDaemon(t, daemonState("LINUX", 20, planning("LINUX")))
	seedLegionProject(t, rig.database, "INFRA", infra.URL)
	seedLegionProject(t, rig.database, "LINUX", linux.URL)
	rig.start()
	rig.awaitWholeReads(2)
	// The starting reads' minute passes, so the burst's first read is due at once.
	rig.clock.advance(ReadRetryAfter + time.Second)

	infra.slows(200 * time.Millisecond)
	release := infra.holds(t)
	linuxAt := map[int]string{40: "implementing", 120: "testing", 200: "reviewing"}
	linuxSeq := 20
	// The burst's first message makes its read, and the rest are published once that read is in
	// flight: a message judged beside the runner before the read began would owe the gap that read
	// is then made for, so the read would clear it, and the boot would come first in the reasons
	// the read leaves owed.
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "merging")))
	rig.waitFor(20*time.Second, func() bool { return infra.served() >= 2 }, func() string {
		return fmt.Sprintf("INFRA's daemon was asked for %d reads, want the burst's after the starting one", infra.served())
	})
	for i := 1; i < burst; i++ {
		if i == burst/2 {
			rig.publish("INFRA", booted(1000, stateSchema, 3))
		}
		rig.publish("INFRA", change(12+i, "INFRA-1", moved("phase", "merging")))
		if phase, due := linuxAt[i]; due {
			linuxSeq++
			rig.publish("LINUX", change(linuxSeq, "LINUX-1", moved("phase", phase)))
		}
	}
	rig.publish("LINUX", change(linuxSeq+1, "LINUX-1", moved("phase", "accepting")))

	// INFRA's one read is in flight, held by its daemon: LINUX's messages in the window are applied
	// meanwhile, and INFRA's daemon is asked for no other read.
	var linuxLast int64
	rig.waitFor(20*time.Second, func() bool {
		err := rig.database.Pool.QueryRow(t.Context(), `select coalesce(last_seq, 0) from legion_event_cursors where project = 'LINUX'`).Scan(&linuxLast)
		return err == nil && linuxLast >= int64(linuxSeq)
	}, func() string {
		return fmt.Sprintf("LINUX's cursor is at %d while INFRA's read is held, want its messages through %d applied", linuxLast, linuxSeq)
	})
	if served := infra.served(); served != 2 {
		t.Fatalf("INFRA's daemon was asked for %d reads while the burst's is held, want the starting read and that one", served)
	}
	release()
	rig.settledHolding(1)

	if served := infra.served(); served != 2 {
		t.Errorf("INFRA's daemon served %d reads, want the starting read and one for the whole burst", served)
	}
	if reasons := rig.wholeReadReasons("INFRA"); !slices.Equal(reasons, []string{ReadReasonStart, ReadReasonGap}) {
		t.Errorf("INFRA's whole reads %v, want the starting read and the burst's one", reasons)
	}
	if c := cursorOf(t, rig.database, "INFRA"); !c.readOwed || !slices.Equal(c.readReasons, []string{ReadReasonGap, ReadReasonBoot}) ||
		c.lastSeq == nil || *c.lastSeq != 10 {
		t.Errorf("INFRA's cursor is %+v, want it at 10 with a read owed for the gaps and then the boot", c)
	}
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); !slices.Equal(got, []string{`"planning"`}) {
		t.Errorf("INFRA-1's phase history is %v, want the read's planning alone: no message of the burst follows the cursor", got)
	}
	if drops := rig.logRecords(unfollowedLine); len(drops) != 1 || drops[0]["project"] != "INFRA" || drops[0]["reason"] != ReadReasonGap {
		t.Errorf("the drops logged = %v, want one naming INFRA and the gap", drops)
	}
	if got := history(t, rig.database, "LINUX", "LINUX-1", FieldPhase); !slices.Equal(got,
		[]string{`"planning"`, `"implementing"`, `"testing"`, `"reviewing"`, `"accepting"`}) {
		t.Errorf("LINUX-1's phase history is %v, want the read's and each of its messages'", got)
	}
	if served := linux.served(); served != 1 {
		t.Errorf("LINUX's daemon served %d reads, want the starting one alone", served)
	}

	rig.clock.advance(ReadRetryAfter)
	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
	rig.settledHolding(1)
	if served := infra.served(); served != 3 {
		t.Errorf("INFRA's daemon served %d reads, want one more once the minute had passed", served)
	}
	var last map[string]any
	for _, line := range rig.logRecords(wholeReadLine) {
		if line["project"] == "INFRA" {
			last = line
		}
	}
	if last["reason"] != ReadReasonGap || last["reasons"] != ReadReasonGap+","+ReadReasonBoot || last["ok"] != true {
		t.Errorf("the owed read's line = %v, want reason gap and reasons gap,boot", last)
	}
	if c := cursorOf(t, rig.database, "INFRA"); c.readOwed || len(c.readReasons) != 0 || c.lastSeq == nil || *c.lastSeq != 11 {
		t.Errorf("INFRA's cursor is %+v, want it at 11 with nothing owed", c)
	}
	if phase := currentValues(t, rig.database, "INFRA")["INFRA-1/phase"]; phase != `"implementing"` {
		t.Errorf("INFRA-1's phase is %s, want the daemon's own next event's implementing", phase)
	}
}

// TestAProjectThatOwesAReadAndGoesQuietIsReadWithinTheMinute: R3 makes an owed read within the
// minute. INFRA's gap read is good, and a second gap reaches Dispatch inside the minute after it,
// with little of the minute left (the intake's clock moved on); then INFRA's daemon goes quiet. The
// second gap's message goes back to the stream unacknowledged, with a delay of the time left in the
// minute, and its redelivery makes the owed read once the minute has passed, with no other message
// of INFRA: the read comes within the time left and one redelivery. While it waits, INFRA's message
// is the one message of the durable's window that waits for an ack, and LINUX's events are applied
// throughout. That redelivery counts toward no drop: the durable's MaxDeliver is -1, no limit, and
// the delay is below its ack wait (consumerConfig).
func TestAProjectThatOwesAReadAndGoesQuietIsReadWithinTheMinute(t *testing.T) {
	// left is what is left of INFRA's minute when its second gap arrives.
	const left = 8 * time.Second
	rig := newIntakeRig(t)
	infra := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	linux := newFakeDaemon(t, daemonState("LINUX", 20, planning("LINUX")))
	seedLegionProject(t, rig.database, "INFRA", infra.URL)
	seedLegionProject(t, rig.database, "LINUX", linux.URL)
	rig.start()
	rig.awaitWholeReads(2)
	// The starting reads' minute passes, so the first gap's read is due at once.
	rig.clock.advance(ReadRetryAfter + time.Second)

	// A gap past 10 whose read is good: INFRA's next read waits ReadRetryAfter after it.
	infra.serves(daemonState("INFRA", 12, strings.Replace(planning("INFRA"), `"planning"`, `"implementing"`, 1)))
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "implementing")))
	rig.settled()
	reads := readRecords(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonGap, true)
	// The minute runs from the start of the read (last_read_started_at), not from read_at, the time
	// the daemon answered it: an owed read may begin before read_at's minute ends, by the time the
	// first read took.
	minuteEnds := lastReadStartedAt(t, rig, "INFRA").Add(ReadRetryAfter)

	// left before the minute ends, a second gap: 13 never reaches Dispatch. Its read is owed, and
	// INFRA publishes nothing after it.
	rig.clock.advance(ReadRetryAfter - left)
	infra.serves(daemonState("INFRA", 14, strings.Replace(planning("INFRA"), `"planning"`, `"testing"`, 1)))
	rig.publish("INFRA", change(14, "INFRA-1", moved("phase", "testing")))
	owed := time.Now()

	// LINUX's events are applied while INFRA's read is owed. Once each is acknowledged, the window
	// holds what waits for an ack, which is INFRA's message alone.
	var waiting []int
	for index, phase := range []string{"implementing", "testing", "reviewing"} {
		seq := 21 + index
		rig.publish("LINUX", change(seq, "LINUX-1", moved("phase", phase)))
		var info *natsgo.ConsumerInfo
		var applied int64
		rig.waitFor(5*time.Second, func() bool {
			var err error
			if info, err = rig.publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchLegionStateDurable); err != nil {
				return false
			}
			err = rig.database.Pool.QueryRow(t.Context(), `select coalesce(last_seq, 0) from legion_event_cursors where project = 'LINUX'`).Scan(&applied)
			return err == nil && applied >= int64(seq) && info.NumPending == 0 && info.NumAckPending <= 1
		}, func() string {
			return fmt.Sprintf("LINUX's cursor is at %d while INFRA's read is owed, want its message %d applied and acknowledged: %+v", applied, seq, info)
		})
		waiting = append(waiting, info.NumAckPending)
	}

	// The owed read comes with no further message of INFRA: the second gap's redelivery makes it
	// once the minute has passed.
	within := left + 10*time.Second
	rig.waitFor(time.Until(owed.Add(within)), func() bool { return infra.served() >= 3 }, func() string {
		return fmt.Sprintf("INFRA's daemon served %d reads in the %s after the owed gap, want its owed read made with no further message, within the %s left of the minute and one redelivery",
			infra.served(), time.Since(owed).Round(time.Second), left)
	})
	rig.awaitWholeReads(4)
	rig.settled()

	reads = readRecords(t, rig.database, "INFRA")
	retried := reads[len(reads)-1]
	requireWholeRead(t, retried, ReadReasonGap, true)
	if started := lastReadStartedAt(t, rig, "INFRA"); started.Before(minuteEnds) {
		t.Errorf("the owed read began at %s, want it once the minute after the first gap's read began ended, at %s",
			started.Format(time.RFC3339Nano), minuteEnds.Format(time.RFC3339Nano))
	}
	if served := infra.served(); served != 3 {
		t.Errorf("INFRA's daemon served %d reads, want the starting read, the first gap's and the owed one", served)
	}
	if c := cursorOf(t, rig.database, "INFRA"); c.readOwed || c.lastSeq == nil || *c.lastSeq != 14 {
		t.Errorf("INFRA's cursor is %+v, want it at the owed read's 14 with nothing owed", c)
	}
	if phase := currentValues(t, rig.database, "INFRA")["INFRA-1/phase"]; phase != `"testing"` {
		t.Errorf("INFRA-1's phase is %s, want the owed read's testing", phase)
	}
	holds := rig.logRecords(heldLine)
	if len(holds) != 1 || holds[0]["project"] != "INFRA" || holds[0]["seq"] != float64(14) {
		t.Fatalf("the messages sent back = %v, want INFRA's second gap once: its one redelivery made the read", holds)
	}
	if delay, _ := holds[0]["delay"].(float64); time.Duration(delay) < holdAtLeast || time.Duration(delay) > left {
		t.Errorf("the second gap went back for %s, want the %s or less left of the minute", time.Duration(delay), left)
	}
	if !slices.Equal(waiting, []int{1, 1, 1}) {
		t.Errorf("the messages waiting for an ack after each of LINUX's were %v, want INFRA's one each time", waiting)
	}
	if got := history(t, rig.database, "LINUX", "LINUX-1", FieldPhase); !slices.Equal(got,
		[]string{`"planning"`, `"implementing"`, `"testing"`, `"reviewing"`}) {
		t.Errorf("LINUX-1's phase history is %v, want the read's and each of its messages'", got)
	}
	info, err := rig.publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchLegionStateDurable)
	if err != nil {
		t.Fatalf("read the durable: %v", err)
	}
	if info.Config.MaxDeliver != -1 || info.Config.AckWait != stateAckWait || left >= info.Config.AckWait {
		t.Errorf("the durable's MaxDeliver is %d and its AckWait %s, want -1, no limit, and %s, past the %s delay",
			info.Config.MaxDeliver, info.Config.AckWait, stateAckWait, left)
	}
}

// wholeReadsOf is project's legion_reads rows of kind `read`, oldest first.
func wholeReadsOf(t *testing.T, database *store.Store, project string) []readRecord {
	t.Helper()
	var reads []readRecord
	for _, row := range readRecords(t, database, project) {
		if row.kind == readKindRead {
			reads = append(reads, row)
		}
	}
	return reads
}

// heldValues is the value of attribute each heldLine names, in order, and fails unless each names
// the message at stream sequence holder: "delivery", how many times the durable then running had
// delivered it, or "handBack", which of the hold's hand-backs the cursor counted it as.
func (r *intakeRig) heldValues(holder uint64, attribute string) []any {
	r.t.Helper()
	var values []any
	for _, line := range r.logRecords(heldLine) {
		if line["streamSeq"] != float64(holder) {
			r.t.Fatalf("a hand-back names %v, want the message at stream sequence %d alone", line, holder)
		}
		values = append(values, line[attribute])
	}
	return values
}

// oneTo is 1 to n as a log line's JSON numbers, in order.
func oneTo(n int) []any {
	values := make([]any, 0, n)
	for value := range n {
		values = append(values, float64(value+1))
	}
	return values
}

// TestAHeldReadThatKeepsFailingLetsItsMessageGoAfterTenHandBacks: R3 bounds how long one message
// holds its project's owed read while the project's reads fail, and so how long it holds the
// durable's ack floor below it. INFRA's daemon refuses every read after its good starting read. A
// gap makes a read, which fails, and goes back to the stream holding the read, owed for retry; each
// redelivery makes the read again, and each read fails. The message goes back 10 times, R3-i's
// bound (stateMaxHolds), once after each of its first 10 failed reads, the cursor counting each
// hand-back, and its delivery after them, the 11th, whose read is the 11th to fail, is
// acknowledged and logged at WARN: nothing of INFRA waits for an ack, the ack floor passes the
// message, the cursor names no holder and counts no hand-back, and the read stays owed for retry.
// INFRA's next message makes that read, and the daemon answers it. The intake's clock jumps
// ReadRetryAfter less holdAtLeast at each reading (intakeClock.jumps), so each hand-back is
// holdAtLeast long rather than a minute, and each redelivery finds the read due.
//
// The test catches a bound other than 10 hand-backs: a lower one lets the message go before its
// 11th read, and a higher one sends it back after its 11th failed read.
func TestAHeldReadThatKeepsFailingLetsItsMessageGoAfterTenHandBacks(t *testing.T) {
	const failures = 11
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	daemon.refuses(http.StatusServiceUnavailable)
	rig.clock.jumps(ReadRetryAfter - holdAtLeast)
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "merging")))
	holder := rig.lastStreamSeq()
	var info *natsgo.ConsumerInfo
	rig.waitFor(time.Minute, func() bool {
		info = rig.consumerInfo()
		return info.AckFloor.Stream >= holder && info.NumAckPending == 0 && info.NumPending == 0
	}, func() string {
		return fmt.Sprintf("the message that holds INFRA's read is not acknowledged after %d reads and %d hand-backs: %+v",
			daemon.served()-1, len(rig.logRecords(heldLine)), info)
	})

	if served := daemon.served(); served != 1+failures {
		t.Errorf("INFRA's daemon served %d reads, want the starting read and %d that failed", served, failures)
	}
	var reasons []string
	for _, read := range wholeReadsOf(t, rig.database, "INFRA")[1:] {
		if read.ok || read.reason == nil {
			t.Fatalf("INFRA's whole read %s after the starting read, want a failed one", read)
		}
		reasons = append(reasons, *read.reason)
	}
	want := []string{ReadReasonGap}
	for range failures - 1 {
		want = append(want, ReadReasonRetry)
	}
	if !slices.Equal(reasons, want) {
		t.Errorf("INFRA's failed reads were for %v, want %v: the gap's, then the retries its message made", reasons, want)
	}
	if got := rig.heldValues(holder, "delivery"); !slices.Equal(got, oneTo(10)) {
		t.Errorf("the message went back at its deliveries %v, want %v: once after each failed read but the last", got, oneTo(10))
	}
	if got := rig.heldValues(holder, "handBack"); !slices.Equal(got, oneTo(10)) {
		t.Errorf("the cursor counted the hand-backs %v, want %v", got, oneTo(10))
	}
	released := rig.awaitLogRecords(releasedLine, 1)
	if len(released) != 1 || released[0]["level"] != "WARN" || released[0]["project"] != "INFRA" ||
		released[0]["streamSeq"] != float64(holder) || released[0]["delivery"] != float64(failures) ||
		released[0]["handBacks"] != float64(10) {
		t.Errorf("the release lines = %v, want one WARN naming INFRA, the message, its delivery 11 and its 10 hand-backs",
			released)
	}
	if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.readReasons, []string{ReadReasonRetry}) || c.readHolder != nil ||
		c.readHolds != 0 || c.lastSeq == nil || *c.lastSeq != 10 {
		t.Errorf("INFRA's cursor is %s, want it at 10 with the read owed for retry, no holder and no hand-back counted", c)
	}

	// INFRA's next message makes the owed read, the minute after the last failed one being over.
	daemon.serves(daemonState("INFRA", 13, strings.Replace(planning("INFRA"), `"planning"`, `"testing"`, 1)))
	rig.publish("INFRA", change(13, "INFRA-1", moved("phase", "testing")))
	rig.settled()
	if served := daemon.served(); served != 2+failures {
		t.Errorf("INFRA's daemon served %d reads, want one more for INFRA's next message", served)
	}
	reads := wholeReadsOf(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonRetry, true)
	if c := cursorOf(t, rig.database, "INFRA"); c.readOwed || c.readHolder != nil || c.lastSeq == nil || *c.lastSeq != 13 {
		t.Errorf("INFRA's cursor is %s, want it at the read's 13 with nothing owed and no holder", c)
	}
	if phase := currentValues(t, rig.database, "INFRA")["INFRA-1/phase"]; phase != `"testing"` {
		t.Errorf("INFRA-1's phase is %s, want the read's testing", phase)
	}
	if got := len(rig.logRecords(heldLine)); got != 10 {
		t.Errorf("%d hand-backs logged, want the held message's 10 and none for the next message", got)
	}
}

// TestAHeldReadThatFailsTwiceIsMadeByItsThirdRedelivery: a message that holds its project's owed
// read makes it at each redelivery, and goes back again while the read fails. INFRA's gap read is
// good, and a second gap reaches Dispatch with left of the minute after that read to go, so it goes
// back to the stream until the read is due. INFRA's daemon refuses the reads its first two
// redeliveries make, and it goes back after each; its third redelivery makes the read, the daemon
// answers it, and the message is acknowledged with the read's state applied. The clock jumps from
// the first hand-back on, as in TestAHeldReadThatKeepsFailingLetsItsMessageGoAfterTenHandBacks.
func TestAHeldReadThatFailsTwiceIsMadeByItsThirdRedelivery(t *testing.T) {
	const left = 3 * time.Second
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	// A gap past 10 whose read is good: INFRA's next read waits ReadRetryAfter after it.
	daemon.serves(daemonState("INFRA", 12, strings.Replace(planning("INFRA"), `"planning"`, `"implementing"`, 1)))
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "implementing")))
	rig.settled()

	// left before the minute ends, a second gap, whose read is owed: it holds the read.
	daemon.serves(daemonState("INFRA", 14, strings.Replace(planning("INFRA"), `"planning"`, `"testing"`, 1)))
	daemon.refusesFor(2, http.StatusServiceUnavailable)
	rig.clock.advance(ReadRetryAfter - left)
	rig.publish("INFRA", change(14, "INFRA-1", moved("phase", "testing")))
	holder := rig.lastStreamSeq()
	rig.settledHolding(1)
	rig.clock.jumps(ReadRetryAfter - holdAtLeast)
	rig.settled()

	if served := daemon.served(); served != 5 {
		t.Errorf("INFRA's daemon served %d reads, want the starting read, the first gap's and three the second gap's redeliveries made", served)
	}
	reads := wholeReadsOf(t, rig.database, "INFRA")
	if len(reads) != 5 {
		t.Fatalf("INFRA's whole reads are %v, want five", reads)
	}
	requireWholeRead(t, reads[2], ReadReasonGap, false)
	requireWholeRead(t, reads[3], ReadReasonRetry, false)
	requireWholeRead(t, reads[4], ReadReasonRetry, true)
	if got := rig.heldValues(holder, "delivery"); !slices.Equal(got, oneTo(3)) {
		t.Errorf("the message went back at its deliveries %v, want 1, 2 and 3: its fourth delivery, the third redelivery, made the good read", got)
	}
	if got := rig.heldValues(holder, "handBack"); !slices.Equal(got, oneTo(3)) {
		t.Errorf("the cursor counted the hand-backs %v, want 1, 2 and 3", got)
	}
	if released := rig.logRecords(releasedLine); len(released) != 0 {
		t.Errorf("release lines %v, want none: the message went back three times", released)
	}
	if c := cursorOf(t, rig.database, "INFRA"); c.readOwed || c.readHolder != nil || c.readHolds != 0 || c.lastSeq == nil || *c.lastSeq != 14 {
		t.Errorf("INFRA's cursor is %s, want it at the good read's 14 with nothing owed, no holder and no hand-back counted", c)
	}
	if phase := currentValues(t, rig.database, "INFRA")["INFRA-1/phase"]; phase != `"testing"` {
		t.Errorf("INFRA-1's phase is %s, want the good read's testing", phase)
	}
}

// TestATakeOverDuringAHoldReplaysOnlyTheMessagesSinceTheHolder: while a message holds its
// project's owed read, the durable's ack floor stays just below it, and a take-over creates the
// durable again at that floor (delivery's takeOverConfig). So the next intake is delivered the
// holder again and the messages after it, LINUX's two that the first intake had applied and
// acknowledged, and none of the messages before it. Those it replays write nothing and make no
// read: each is at or below its project's cursor. stateMaxHolds bounds the replay, since it bounds
// how long one message holds the floor: the holder's delivery count starts again at 1 with the new
// durable, and the cursor's count of its hand-backs goes on from 1 to 2.
func TestATakeOverDuringAHoldReplaysOnlyTheMessagesSinceTheHolder(t *testing.T) {
	rig := newIntakeRig(t)
	infra := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	linux := newFakeDaemon(t, daemonState("LINUX", 20, planning("LINUX")))
	seedLegionProject(t, rig.database, "INFRA", infra.URL)
	seedLegionProject(t, rig.database, "LINUX", linux.URL)
	rig.start()
	rig.awaitWholeReads(2)
	// The starting reads' minute passes, so the first gap's read is due at once.
	rig.clock.advance(ReadRetryAfter + time.Second)

	// Before the holder: LINUX's 21, and a gap of INFRA's past 10 whose read is good, after which
	// INFRA's next read waits ReadRetryAfter.
	rig.publish("LINUX", change(21, "LINUX-1", moved("phase", "implementing")))
	infra.serves(daemonState("INFRA", 12, strings.Replace(planning("INFRA"), `"planning"`, `"implementing"`, 1)))
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "implementing")))
	rig.settled()

	// The holder, a second gap inside the minute, and after it LINUX's 22 and 23, which are applied
	// and acknowledged while it waits.
	rig.publish("INFRA", change(14, "INFRA-1", moved("phase", "testing")))
	holder := rig.lastStreamSeq()
	rig.publish("LINUX", change(22, "LINUX-1", moved("phase", "testing")))
	rig.publish("LINUX", change(23, "LINUX-1", moved("phase", "reviewing")))
	rig.settledHolding(1)
	requireLastSeq(t, rig.database, "LINUX", 23)
	last := rig.lastStreamSeq()
	if floor := rig.consumerInfo().AckFloor.Stream; floor != holder-1 {
		t.Fatalf("the durable's ack floor is %d while the message at %d holds INFRA's read, want %d", floor, holder, holder-1)
	}

	rig.stop()
	rig.start()
	var info *natsgo.ConsumerInfo
	rig.waitFor(20*time.Second, func() bool {
		info = rig.consumerInfo()
		return info.Delivered.Stream >= last && info.NumPending == 0 && info.NumAckPending == 1 && len(rig.logRecords(heldLine)) >= 2
	}, func() string {
		return fmt.Sprintf("the next intake has not answered through stream sequence %d with the holder sent back again: %+v", last, info)
	})
	if info.Config.OptStartSeq != holder || info.Delivered.Consumer != last-holder+1 {
		t.Errorf("the take-over's durable starts at %d and delivered %d messages, want the holder's %d and the %d from it on",
			info.Config.OptStartSeq, info.Delivered.Consumer, holder, last-holder+1)
	}
	if got := rig.heldValues(holder, "delivery"); !slices.Equal(got, []any{float64(1), float64(1)}) {
		t.Errorf("the holder went back at its deliveries %v, want its first to each intake's durable", got)
	}
	if got := rig.heldValues(holder, "handBack"); !slices.Equal(got, oneTo(2)) {
		t.Errorf("the cursor counted the holder's hand-backs %v, want 1 and 2: the count goes on across the take-over", got)
	}
	if c := cursorOf(t, rig.database, "INFRA"); c.readHolder == nil || uint64(*c.readHolder) != holder || c.readHolds != 2 {
		t.Errorf("INFRA's cursor is %s, want the holder at %d with 2 hand-backs counted", c, holder)
	}
	if got := history(t, rig.database, "LINUX", "LINUX-1", FieldPhase); !slices.Equal(got,
		[]string{`"planning"`, `"implementing"`, `"testing"`, `"reviewing"`}) {
		t.Errorf("LINUX-1's phase history is %v, want the read's and each message's once", got)
	}
	if served := linux.served(); served != 1 {
		t.Errorf("LINUX's daemon served %d reads, want the starting one alone", served)
	}
	if served := infra.served(); served != 2 {
		t.Errorf("INFRA's daemon served %d reads, want the starting read and the first gap's", served)
	}
}

// TestAHoldsHandBacksAreCountedAcrossATakeOver: R3 bounds how long one message holds its
// project's owed read, and so how long it holds the durable's ack floor and how much a take-over
// replays: stateMaxHolds hand-backs at the most in all, then the message is acknowledged. The
// cursor counts the hand-backs, so a take-over in the middle of the count, which creates the
// durable again and starts the message's own delivery count again at 1, does not start the count
// again. INFRA's daemon refuses every read after its good starting read, and a gap's message holds
// the read and goes back after each read it makes that fails; the clock jumps as in
// TestAHeldReadThatKeepsFailingLetsItsMessageGoAfterTenHandBacks. After at least before
// hand-backs the intake stops and a second intake takes the durable over. The message goes back
// stateMaxHolds times in all, the second intake counting on from where the first stopped, and its
// next delivery is acknowledged and logged at WARN, with no holder and no count left on the cursor.
func TestAHoldsHandBacksAreCountedAcrossATakeOver(t *testing.T) {
	// before is how many hand-backs the first intake makes at the least before it stops.
	const before = 4
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	daemon.refuses(http.StatusServiceUnavailable)
	rig.clock.jumps(ReadRetryAfter - holdAtLeast)
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "merging")))
	holder := rig.lastStreamSeq()
	rig.waitFor(30*time.Second, func() bool { return len(rig.logRecords(heldLine)) >= before }, func() string {
		return fmt.Sprintf("the message went back %d times, want %d before the take-over", len(rig.logRecords(heldLine)), before)
	})
	rig.stop()
	first := len(rig.logRecords(heldLine))
	if first >= stateMaxHolds {
		t.Fatalf("the message went back %d times before the take-over, want fewer than %d", first, stateMaxHolds)
	}

	rig.start()
	var info *natsgo.ConsumerInfo
	rig.waitFor(time.Minute, func() bool {
		info = rig.consumerInfo()
		return info.AckFloor.Stream >= holder && info.NumAckPending == 0 && info.NumPending == 0
	}, func() string {
		return fmt.Sprintf("the message that holds INFRA's read is not acknowledged after %d hand-backs, %d of them before the take-over: %+v",
			len(rig.logRecords(heldLine)), first, info)
	})

	deliveries := rig.heldValues(holder, "delivery")
	if len(deliveries) != stateMaxHolds {
		t.Fatalf("the message went back %d times in all, %d of them before the take-over, want %d: the count goes on across the take-over",
			len(deliveries), first, stateMaxHolds)
	}
	// The second durable delivers the message from 1 again: its deliveries are two runs from 1.
	split := slices.Index(deliveries[1:], any(float64(1))) + 1
	if want := append(oneTo(split), oneTo(stateMaxHolds-split)...); split == 0 || !slices.Equal(deliveries, want) {
		t.Errorf("the message went back at its deliveries %v, want two runs from 1, one to each durable", deliveries)
	}
	if got := rig.heldValues(holder, "handBack"); !slices.Equal(got, oneTo(stateMaxHolds)) {
		t.Errorf("the cursor counted the hand-backs %v, want %v across the take-over", got, oneTo(stateMaxHolds))
	}
	released := rig.awaitLogRecords(releasedLine, 1)
	if len(released) != 1 || released[0]["level"] != "WARN" || released[0]["streamSeq"] != float64(holder) ||
		released[0]["handBacks"] != float64(stateMaxHolds) || released[0]["delivery"] != float64(stateMaxHolds-split+1) {
		t.Errorf("the release lines = %v, want one WARN naming the message, its %d hand-backs and its delivery %d to the second durable",
			released, stateMaxHolds, stateMaxHolds-split+1)
	}
	if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.readReasons, []string{ReadReasonRetry}) || c.readHolder != nil || c.readHolds != 0 {
		t.Errorf("INFRA's cursor is %s, want the read owed for retry, no holder and no hand-back counted", c)
	}
}

// TestOnlyAGoodWholeReadClearsTheHolderOfTheOwedRead: the message that holds a project's owed read
// and the cursor's count of its hand-backs go only with a good whole read or with the message
// (recordWhole). A failed read recorded while the cursor follows another daemon than the setting
// names keeps them, as the cursor comes to follow the setting's daemon; a failed read while it
// follows that daemon keeps them too; and a good read clears them. No step of the intake leaves a
// holder on a cursor that follows another daemon, since a write of the setting that names another
// daemon clears it, so the test writes INFRA's cursor so by hand: it follows another URL, owes the
// read for retry, and names stream sequence 42 as the holder, gone back 3 times.
func TestOnlyAGoodWholeReadClearsTheHolderOfTheOwedRead(t *testing.T) {
	daemon := newFakeDaemon(t, "")
	daemon.refuses(http.StatusServiceUnavailable)
	database, setting := configured(t, daemon.URL)
	if _, err := database.Pool.Exec(t.Context(), `
		insert into legion_event_cursors (project, state_url, read_reasons, last_read_started_at, read_holder, read_holds)
		values ('INFRA', 'http://another-daemon.example', '{retry}', now() - interval '2 minutes', 42, 3)
	`); err != nil {
		t.Fatalf("write INFRA's cursor: %v", err)
	}
	stateURL := func() string {
		var url string
		if err := database.Pool.QueryRow(t.Context(), `select state_url from legion_event_cursors where project = 'INFRA'`).Scan(&url); err != nil {
			t.Fatalf("read the URL INFRA's cursor follows: %v", err)
		}
		return url
	}
	requireHolder := func(after string) {
		t.Helper()
		if c := cursorOf(t, database, "INFRA"); c.readHolder == nil || *c.readHolder != 42 || c.readHolds != 3 ||
			!slices.Equal(c.readReasons, []string{ReadReasonRetry}) {
			t.Errorf("INFRA's cursor after %s is %s, want the read owed for retry and held by 42, gone back 3 times", after, c)
		}
	}

	if err := ReadWhole(t.Context(), database.Pool, setting, ReadReasonRetry); err != nil {
		t.Fatalf("read INFRA's daemon whole: %v", err)
	}
	if url := stateURL(); url != daemon.URL {
		t.Fatalf("INFRA's cursor follows %q after the failed read, want the setting's %s", url, daemon.URL)
	}
	requireHolder("a failed read while it followed another daemon")

	if err := ReadWhole(t.Context(), database.Pool, setting, ReadReasonRetry); err != nil {
		t.Fatalf("read INFRA's daemon whole again: %v", err)
	}
	requireHolder("a failed read while it followed the setting's daemon")

	daemon.serves(daemonState("INFRA", 10, planning("INFRA")))
	if err := ReadWhole(t.Context(), database.Pool, setting, ReadReasonRetry); err != nil {
		t.Fatalf("read INFRA's daemon whole a third time: %v", err)
	}
	if c := cursorOf(t, database, "INFRA"); c.readOwed || c.readHolder != nil || c.readHolds != 0 || c.lastSeq == nil || *c.lastSeq != 10 {
		t.Errorf("INFRA's cursor after a good read is %s, want it at the read's 10 with nothing owed, no holder and no hand-back counted", c)
	}
	if reads := wholeReadsOf(t, database, "INFRA"); len(reads) != 3 || reads[0].ok || reads[1].ok || !reads[2].ok {
		t.Errorf("INFRA's whole reads are %v, want two failed reads and a good one", reads)
	}
}

// TestAHeldReadThatTimesOutHoldsTheFloorAboutElevenMinutes: R3's minute between two whole reads of
// a project counts from the start of the first, so a held read against a daemon that accepts the
// connection and never answers holds the durable's ack floor for about 11 minutes at the most, not
// for RequestTimeout more at each read. After INFRA's good starting read its daemon stops
// answering: each read the intake makes waits until the client's window ends, window in the rig,
// and the daemon moves the intake's clock RequestTimeout on at each read, as a read that waits out
// the production window takes. A gap's message makes a read that times out and goes back to the
// stream holding the read. That read began RequestTimeout before it failed, so its minute is over
// and the hand-back is holdAtLeast long, and the redelivery makes the read again. The message goes
// back stateMaxHolds times, each holdAtLeast long, and is then acknowledged: on the intake's clock
// it held the floor for its 11 reads of RequestTimeout each and 10 hand-backs of holdAtLeast, about
// 11 minutes.
func TestAHeldReadThatTimesOutHoldsTheFloorAboutElevenMinutes(t *testing.T) {
	const failures = stateMaxHolds + 1
	// window is the client's window in the rig: every Client shares the default transport, which
	// waits that long for the daemon's answer, so a read of a daemon that never answers ends then.
	const window = 200 * time.Millisecond
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = window
	shared := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() {
		http.DefaultTransport = shared
		transport.CloseIdleConnections()
	})
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	daemon.stalls(func() { rig.clock.advance(RequestTimeout) })
	from := rig.clock.now()
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "merging")))
	holder := rig.lastStreamSeq()

	// The first hand-back follows a read that began RequestTimeout before it failed.
	first := rig.awaitLogRecords(heldLine, 1)[0]
	if delay, _ := first["delay"].(float64); time.Duration(delay) != holdAtLeast {
		t.Fatalf("after a read that timed out the message went back for %s, want holdAtLeast (%s): the read began RequestTimeout (%s) before, and the minute counts from the read's start",
			time.Duration(delay), holdAtLeast, RequestTimeout)
	}
	var info *natsgo.ConsumerInfo
	rig.waitFor(time.Minute, func() bool {
		info = rig.consumerInfo()
		return info.AckFloor.Stream >= holder && info.NumAckPending == 0 && info.NumPending == 0
	}, func() string {
		return fmt.Sprintf("the message that holds INFRA's read is not acknowledged after %d reads and %d hand-backs: %+v",
			daemon.served()-1, len(rig.logRecords(heldLine)), info)
	})
	held := rig.clock.now().Sub(from)

	if served := daemon.served(); served != 1+failures {
		t.Errorf("INFRA's daemon served %d reads, want the starting read and %d that timed out", served, failures)
	}
	for _, line := range rig.logRecords(heldLine) {
		if delay, _ := line["delay"].(float64); time.Duration(delay) != holdAtLeast {
			t.Errorf("a hand-back after a read that timed out is %s long, want holdAtLeast (%s): %v", time.Duration(delay), holdAtLeast, line)
		}
	}
	if got := rig.heldValues(holder, "handBack"); !slices.Equal(got, oneTo(stateMaxHolds)) {
		t.Errorf("the cursor counted the hand-backs %v, want %v", got, oneTo(stateMaxHolds))
	}
	if released := rig.awaitLogRecords(releasedLine, 1); len(released) != 1 || released[0]["streamSeq"] != float64(holder) {
		t.Errorf("the release lines = %v, want one naming the message", released)
	}
	// 11 reads of RequestTimeout each, and 10 hand-backs of holdAtLeast with the rig's own time
	// within a minute: about 11 minutes. A minute counted from each read's end would add
	// ReadRetryAfter for each hand-back, about 21 minutes in all.
	reads := time.Duration(failures) * RequestTimeout
	if held < reads || held >= reads+time.Minute {
		t.Errorf("on the intake's clock the message held the ack floor for %s, want at least the %s of its %d reads and less than a minute more",
			held.Round(time.Second), reads, failures)
	}
}

// TestAMessageThatGoesBackToTheStreamIsLoggedAsHeldAlone: a message that holds its project's owed
// read is not applied, and the delivery that sends it back to the stream logs that hand-back alone
// (heldLine, naming the read the message calls for as its reason): not the line of a message
// acknowledged unapplied that it would log otherwise, a gap's, a message of another store's or
// one of a cursor with no store, and not the refusal of a message the database refuses for good.
func TestAMessageThatGoesBackToTheStreamIsLoggedAsHeldAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		// failedStart is a starting read the daemon refuses, after which the cursor holds no store;
		// otherwise a good read of a gap past 10 sets the cursor at 12 first.
		failedStart bool
		publish     func(rig *intakeRig)
		reason      string
	}{
		{
			name:    "a gap",
			publish: func(rig *intakeRig) { rig.publish("INFRA", change(14, "INFRA-1", moved("phase", "testing"))) },
			reason:  ReadReasonGap,
		},
		{
			name: "a message of another store",
			publish: func(rig *intakeRig) {
				rig.publishIn("INFRA", rig.run, otherStateID, time.Now().UTC().Truncate(time.Microsecond), false,
					change(13, "INFRA-1", moved("phase", "testing")))
			},
			reason: ReadReasonStore,
		},
		{
			name: "a message the database refuses for good",
			publish: func(rig *intakeRig) {
				rig.publish("INFRA", change(13, "INFRA-1", moved("waitingOn", json.RawMessage(`{"kind":"ask","detail":"\ud800"}`))))
			},
			reason: ReadReasonRefused,
		},
		{
			name:        "a message while the cursor holds no store",
			failedStart: true,
			publish:     func(rig *intakeRig) { rig.publish("INFRA", change(5, "INFRA-1", moved("phase", "testing"))) },
			reason:      "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIntakeRig(t)
			daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
			if tc.failedStart {
				daemon.refuses(http.StatusServiceUnavailable)
			}
			seedLegionProject(t, rig.database, "INFRA", daemon.URL)
			rig.start()
			rig.awaitWholeReads(1)
			if !tc.failedStart {
				// The starting read's minute passes, so the gap's read is due at once.
				rig.clock.advance(ReadRetryAfter + time.Second)
				daemon.serves(daemonState("INFRA", 12, planning("INFRA")))
				rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "planning")))
				rig.settled()
			}
			tc.publish(rig)
			rig.settledHolding(1)
			held := rig.logRecords(heldLine)
			if len(held) != 1 || held[0]["project"] != "INFRA" || held[0]["reason"] != tc.reason || held[0]["delivery"] != float64(1) {
				t.Fatalf("the hand-backs logged = %v, want one naming INFRA, the reason %q and the first delivery", held, tc.reason)
			}
			for _, line := range []string{unfollowedLine, noStoreLine, otherStoreLine, refusedLine} {
				if records := rig.logRecords(line); len(records) != 0 {
					t.Errorf("the held message also logged %v", records)
				}
			}
		})
	}
}
