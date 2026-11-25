package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// refusedLine is the message of the consumer's line for a checks settlement the decoder refused.
const refusedLine = "intake: a checks settlement was dropped: the decoder refused it"

// A checks settlement of a repository the daemon hears that lacks a field the daemon orders or
// judges it by is no fact, and the decoder says which field and what was wrong with it, one row
// for each way a field can be wrong, so the consumer can name it rather than drop the settlement
// in silence. The head, generation and snapshot its payload gives are carried with the reason,
// each only where it reads as its type, so the line names the settlement even when one of them is
// the field refused. A payload of a repository the daemon does not hear, or of another kind, is no
// checks settlement of this daemon: no fact, and nothing refused.
func TestDecodeChecksNamesTheFieldOfARefusedSettlement(t *testing.T) {
	runs := []record.AttemptRun{{Name: "build", ID: 1}, {Name: "unit", ID: 2}}
	set := func(key string, value any) func(map[string]any) {
		return func(payload map[string]any) { payload[key] = value }
	}
	drop := func(key string) func(map[string]any) {
		return func(payload map[string]any) { delete(payload, key) }
	}
	run := func(name string, id int64) map[string]any { return map[string]any{"name": name, "id": id} }
	for _, tc := range []struct {
		name    string
		change  func(map[string]any)
		refused string
	}{
		{"no number", drop("number"), "number is absent or not a pull request number"},
		{"a number that is no number", set("number", "forty-two"), "number is absent or not a pull request number"},
		{"the number of another pull request than the subject's", set("number", "43"), "number 43 is not the pull request its subject names"},
		{"no sha", drop("sha"), "sha is absent or not a string"},
		{"a sha that is no string", set("sha", 7), "sha is absent or not a string"},
		{"an empty sha", set("sha", ""), "sha is empty"},
		{"no check runs", drop("check_runs"), "check_runs is absent"},
		{"check runs that do not parse", set("check_runs", "build"), "check_runs does not parse as a list of check runs, each a name and an id"},
		{"a run whose id is no number", set("check_runs", []any{map[string]any{"name": "build", "id": "7"}}), "check_runs does not parse as a list of check runs, each a name and an id"},
		{"an empty list of check runs", set("check_runs", []any{}), "check_runs is an empty list"},
		{"a null list of check runs", set("check_runs", nil), "check_runs is an empty list"},
		{"a run with no name", set("check_runs", []any{run("", 1)}), "check_runs names a run with no name"},
		{"a run of id 0", set("check_runs", []any{run("build", 0)}), `check_runs names "build" with the id 0, not above 0`},
		{"a run of a negative id", set("check_runs", []any{run("build", -4)}), `check_runs names "build" with the id -4, not above 0`},
		{"a run named twice", set("check_runs", []any{run("build", 1), run("unit", 2), run("build", 3)}), `check_runs names "build" twice`},
		{"no generation", drop("generation"), "generation is absent or not a whole number"},
		{"a generation that is a string", set("generation", "3"), "generation is absent or not a whole number"},
		{"a generation that is no whole number", set("generation", 1.5), "generation is absent or not a whole number"},
		{"a negative generation", set("generation", -1), "generation is -1, below 0"},
		{"no snapshot", drop("snapshot"), "snapshot is absent or not a string"},
		{"a snapshot that is no string", set("snapshot", 3), "snapshot is absent or not a string"},
		{"an empty snapshot", set("snapshot", ""), "snapshot is empty"},
		{"no failed group", drop("failed"), "failed is absent"},
		{"a failed group that is no group", set("failed", "unit"), "failed is not a group of check names"},
		{"a failed group whose count is not its names", set("failed", map[string]any{"count": 2, "checks": []string{"unit"}}), "failed counts 2 checks and names 1"},
		{"a failed group naming a check with no name", set("failed", checkGroup([]string{""})), "failed names a check with no name"},
		{"no cancelled group", drop("cancelled"), "cancelled is absent"},
		{"a cancelled group whose count is not its names", set("cancelled", map[string]any{"count": 1, "checks": []string{}}), "cancelled counts 1 checks and names 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := settlementPayload(runs, nil, nil)
			tc.change(payload)
			decoded := decodeSettlement(t, payload)
			if decoded.Fact != nil || decoded.Refused == nil || decoded.Refused.reason != tc.refused {
				t.Fatalf("decoded %#v, refused %+v; want no fact, refused %q", decoded.Fact, decoded.Refused, tc.refused)
			}
			if want := readFromPayload(payload); !reflect.DeepEqual(decoded.Refused.settlement, want) {
				t.Fatalf("the refused settlement reads as %v, want %v: each of sha, generation and snapshot that reads as its type",
					decoded.Refused.settlement.attributes(), want.attributes())
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"a repository the daemon does not hear", set("repo", "example-org/example-repo")},
		{"a payload of another kind", set("kind", "comment")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := settlementPayload(runs, nil, nil)
			payload["number"] = "forty-two"
			tc.change(payload)
			if decoded := decodeSettlement(t, payload); decoded.Fact != nil || decoded.Refused != nil {
				t.Fatalf("decoded %#v, refused %+v; want no fact and nothing refused: it is no checks settlement of this daemon", decoded.Fact, decoded.Refused)
			}
		})
	}
	t.Run("a settlement that decodes", func(t *testing.T) {
		decoded := decodeSettlement(t, settlementPayload(runs, nil, nil))
		if decoded.Refused != nil {
			t.Fatalf("refused %+v, want nothing refused", decoded.Refused)
		}
		settledChecks(t, decoded)
	})
}

// readFromPayload is what a refused settlement's identity should read as, taken from the payload
// as the test built it, before any JSON: each of sha and snapshot that is a string and a
// generation that is a whole number.
func readFromPayload(payload map[string]any) settlementRead {
	var read settlementRead
	if sha, ok := payload["sha"].(string); ok {
		read.SHA = &sha
	}
	if generation, ok := payload["generation"].(int); ok {
		whole := int64(generation)
		read.Generation = &whole
	}
	if snapshot, ok := payload["snapshot"].(string); ok {
		read.Snapshot = &snapshot
	}
	return read
}

// deliveredMessage is one JetStream delivery of data on subject, its first, to a handling its row
// expects to acknowledge it: it counts its acknowledgements in acks, takes every progress report
// and answers its metadata, and every other call fails the row by the call's name
// (unexpectedMessage), so a handling that naks or terminates it fails the test saying which.
type deliveredMessage struct {
	unexpectedMessage
	subject string
	data    []byte
	acks    *atomic.Int64
}

var _ jetstream.Msg = deliveredMessage{}

// settlementDelivery is payload delivered under event id on statusesSubject to the row t runs, with
// its acknowledgements counted in acks.
func settlementDelivery(t *testing.T, id string, payload map[string]any, acks *atomic.Int64) deliveredMessage {
	t.Helper()
	return deliveredMessage{unexpectedMessage: unexpectedMessage{t: t}, subject: statusesSubject, data: settlementEnvelope(t, id, payload), acks: acks}
}

func (m deliveredMessage) Subject() string { return m.subject }
func (m deliveredMessage) Data() []byte    { return m.data }
func (m deliveredMessage) Ack() error      { m.acks.Add(1); return nil }
func (deliveredMessage) InProgress() error { return nil }

func (deliveredMessage) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{Sequence: jetstream.SequencePair{Stream: 7, Consumer: 7}, NumDelivered: 1}, nil
}

// settlementEnvelope is payload as the listener publishes it, under event id.
func settlementEnvelope(t *testing.T, id string, payload map[string]any) []byte {
	t.Helper()
	text, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return envelopeJSON(t, id, "github", string(text))
}

// dropConsumer is the consumer's spec over logs, with a consumer's memory of the drops it logged,
// as one durable consumer holds them for as long as it runs (consumeConsumer), and the context
// each of the row's handlings runs under.
//
// Its AckWait is a hang guard, not the subject. The rows count acknowledgements, lines and
// transactions, and AckWait bounds the one read a settlement's handling makes before any of them,
// whether its pull request is recorded (recordedPullRequest, which cuts the read at AckWait). On
// the unrecorded row's pool, new to the row, that read is the pool's first and opens its first
// connection. Any fixed window loses that race on a machine slow enough: at consumerSpec's 200 ms
// a loaded runner's connection outran it, and at one minute a connection made in 61 s did, under
// a -timeout of 4 minutes, so the read was cut, the delivery naked and the row's verdict was the
// machine's. The window is what is left of the test binary's deadline less 30 s
// (testwait.Remaining), so a read however slow is waited out. A window is a span from each read's
// start, though, and a row hands several messages to the consumer one after another: after a read
// that took most of it, the next read's window would run past the binary's deadline. So the
// handlings also run under the returned context, whose deadline is that same moment, the
// binary's deadline less 30 s, fixed when the spec is made: only a read that never answers is
// cut, there, failing the row by its nak (deliveredMessage) before the binary's own timeout
// would. The progress keeper reports nothing: it ticks at a third of the window (spec.AckWait/3
// in consume.go), and each handling here ends long before that.
func dropConsumer(t *testing.T, logs *lockedBuffer) (context.Context, ConsumerSpec, *issueNames, *droppedSettlements) {
	t.Helper()
	spec := consumerSpec(logs)
	spec.AckWait = testwait.Remaining(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(spec.AckWait))
	t.Cleanup(cancel)
	spec, err := normalizedSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, spec, &issueNames{}, newDroppedSettlements()
}

// consumeDropped runs consumeMessage on message under ctx as consumeConsumer does, for the
// consumer and the context dropConsumer gives: its metadata parsed once and handed on with it,
// over pool, with no handler.
func consumeDropped(ctx context.Context, message jetstream.Msg, spec ConsumerSpec, pool store.TxPool, names *issueNames, dropped *droppedSettlements) bool {
	metadata, _ := message.Metadata()
	run := consumerRun{spec: spec, repositories: capturedRepositories, pool: pool, names: names, remade: &remadeFailures{}, dropped: dropped,
		noHeadRepo: &pacedCount{}}
	return consumeMessage(ctx, message, metadata, run, nil)
}

// A checks settlement the decoder refuses is acknowledged, as every message of no fact is, and
// the consumer logs one WARN line for it, naming the field refused and the head, generation and
// snapshot the payload gives, by which the listener's line that published the settlement is
// found. The same settlement delivered three times writes the one line, and is acknowledged each
// time; another refused settlement, under an event id of its own, writes a line of its own.
//
// Negative control: at ac57d598 the decoder answers such a settlement with no fact and no reason,
// and the consumer acknowledges it with no line.
func TestAConsumedSettlementTheDecoderRefusesLogsOneLine(t *testing.T) {
	logs := &lockedBuffer{}
	ctx, spec, names, dropped := dropConsumer(t, logs)
	payload := settlementPayload([]record.AttemptRun{{Name: "build", ID: 1}, {Name: "unit", ID: 2}}, []string{"unit"}, nil)
	payload["failed"] = map[string]any{"count": 2, "checks": []string{"unit"}}
	var acks atomic.Int64
	message := settlementDelivery(t, "checks-refused", payload, &acks)
	for delivery := 1; delivery <= 3; delivery++ {
		if !consumeDropped(ctx, message, spec, nil, names, dropped) {
			t.Fatalf("delivery %d was not handled, want it acknowledged; the consumer's log:\n%s", delivery, logs)
		}
		if got := acks.Load(); got != int64(delivery) {
			t.Fatalf("after delivery %d the message was acknowledged %d times, want once for each delivery", delivery, got)
		}
	}
	if got := strings.Count(logs.String(), refusedLine); got != 1 {
		t.Fatalf("%d lines for the refused settlement delivered three times, want 1:\n%s", got, logs)
	}
	requireLogLine(t, logs, "level=WARN", `msg="`+refusedLine+`"`, "event_id=checks-refused", `reason="failed counts 2 checks and names 1"`,
		"sha="+statusesHead, "generation=3", "snapshot=snapshot-3", "subject="+statusesSubject)

	// A settlement refused for its generation is named by the head and the snapshot it gives, and
	// by no generation at all.
	other := settlementPayload([]record.AttemptRun{{Name: "build", ID: 1}}, nil, nil)
	other["generation"] = "4"
	if !consumeDropped(ctx, settlementDelivery(t, "checks-refused-again", other, &acks), spec, nil, names, dropped) {
		t.Fatalf("the second refused settlement was not handled, want it acknowledged; the consumer's log:\n%s", logs)
	}
	if got := strings.Count(logs.String(), refusedLine); got != 2 {
		t.Fatalf("%d lines after a second refused settlement, want 2:\n%s", got, logs)
	}
	line := logLine(logs.String(), refusedLine, "event_id=checks-refused-again")
	if line == "" || !strings.Contains(line, `reason="generation is absent or not a whole number"`) || !strings.Contains(line, "sha="+statusesHead) ||
		!strings.Contains(line, "snapshot=snapshot-3") || strings.Contains(line, "generation=") {
		t.Fatalf("the second refused settlement's line is %q, want its reason, its sha and snapshot, and no generation:\n%s", line, logs)
	}
}

// A checks settlement of a pull request the daemon does not record is acknowledged with no
// transaction, as before, and the consumer logs one INFO line for it, the one the workflow writes
// for a settlement no recorded pull request matches (NoRecordedPullRequestLine), naming the
// repository, the number, and the settlement's head, generation and snapshot. The same
// settlement delivered three times writes the one line and takes no transaction each time;
// another settlement of the same head, at a new generation, writes a line of its own.
//
// Negative control: at ac57d598 such a settlement is acknowledged with a DEBUG line alone.
func TestAConsumedSettlementOfAnUnrecordedPullRequestLogsOneLine(t *testing.T) {
	pool := migratedPool(t)
	locks := &factLocks{}
	traced := pool.Config().Copy()
	traced.ConnConfig.Tracer = locks
	counted, err := pgxpool.NewWithConfig(context.Background(), traced)
	if err != nil {
		t.Fatalf("open the counted pool: %v", err)
	}
	t.Cleanup(counted.Close)
	logs := &lockedBuffer{}
	ctx, spec, names, dropped := dropConsumer(t, logs)
	settlement := func(generation int) map[string]any {
		payload := settlementPayload([]record.AttemptRun{{Name: "build", ID: 1}}, nil, nil)
		payload["generation"], payload["snapshot"] = generation, fmt.Sprintf("snapshot-%d", generation)
		return payload
	}
	var acks atomic.Int64
	message := settlementDelivery(t, "checks-unrecorded", settlement(3), &acks)
	for delivery := 1; delivery <= 3; delivery++ {
		if !consumeDropped(ctx, message, spec, counted, names, dropped) {
			t.Fatalf("delivery %d was not handled, want it acknowledged; the consumer's log:\n%s", delivery, logs)
		}
		if got := acks.Load(); got != int64(delivery) {
			t.Fatalf("after delivery %d the message was acknowledged %d times, want once for each delivery", delivery, got)
		}
	}
	if got := strings.Count(logs.String(), NoRecordedPullRequestLine); got != 1 {
		t.Fatalf("%d lines for the settlement delivered three times, want 1:\n%s", got, logs)
	}
	requireLogLine(t, logs, "level=INFO", `msg="`+NoRecordedPullRequestLine+`"`, "repo=sjawhar/legion", "number=42", " head="+statusesHead,
		"generation=3", "snapshot=snapshot-3", "subject="+statusesSubject)

	next := settlementDelivery(t, "checks-unrecorded-next", settlement(4), &acks)
	if !consumeDropped(ctx, next, spec, counted, names, dropped) {
		t.Fatalf("the next settlement was not handled, want it acknowledged; the consumer's log:\n%s", logs)
	}
	if got := strings.Count(logs.String(), NoRecordedPullRequestLine); got != 2 {
		t.Fatalf("%d lines after the head's next settlement, want 2:\n%s", got, logs)
	}
	requireLogLine(t, logs, NoRecordedPullRequestLine, "generation=4", "snapshot=snapshot-4")
	if taken := locks.taken.Load(); taken != 0 {
		t.Fatalf("the settlements took the fact lock %d times, want never: no transaction was to open", taken)
	}
	if processed := processedEvents(t, pool); processed != 0 {
		t.Fatalf("%d processed events, want none", processed)
	}
}

// Each set of the drop lines remembers the last DroppedSettlementsKept settlements it logged, and
// no more: the oldest is forgotten first, and a settlement forgotten logs once more.
func TestTheDropLinesRememberTheLastSettlementsTheyLogged(t *testing.T) {
	if DroppedSettlementsKept != 4096 {
		t.Fatalf("DroppedSettlementsKept = %d, want the 4,096 the plan names", DroppedSettlementsKept)
	}
	refused := newDroppedSettlements().refused
	for i := range DroppedSettlementsKept {
		if !refused.Add(fmt.Sprintf("event-%d", i)) {
			t.Fatalf("event-%d is held before it was added", i)
		}
	}
	if refused.Add("event-0") {
		t.Fatal("the first event id is forgotten with the set at its bound, want it held")
	}
	if !refused.Add("event-past-the-bound") {
		t.Fatal("an event id past the bound is held before it was added")
	}
	if !refused.Add("event-0") {
		t.Fatal("the oldest event id is held after one past the bound, want it forgotten")
	}
	// event-0 came back in event-1's place, the oldest then, and event-2 is still held.
	if refused.Add("event-2") || !refused.Add("event-1") {
		t.Fatal("event-2 is forgotten or event-1 held after event-0 came back, want the oldest forgotten first")
	}
}
