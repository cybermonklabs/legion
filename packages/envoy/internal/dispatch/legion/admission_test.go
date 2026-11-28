package legion_test

// admission_test.go holds the tests of the admission eval's writer in the intake's transactions
// (dispatch://INFRA-2918, R22, checks k26 and k29): each feeds the intake as the intake's own tests
// do, a daemon stand-in's state documents and messages on the state lane, with the writer
// dispatch/evals builds (evals.AdmissionWriter), which is why they are outside package legion
// (export_test.go). The intake registers the eval at the first state that serves either record of
// the daemon (admission_registration_test.go); a test whose states serve neither registers it by
// hand, through the register write, in its setup.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// infra is the project every test's daemon serves, and admissionID its admission eval.
const (
	infra       = "INFRA"
	admissionID = infra + "/admission"
)

// testBuild is the build every readings write of these tests records on its day rows.
const testBuild = "admission-test-build"

// otherRun is the daemon run that serves the store a daemon's store was replaced by.
const otherRun = "8a41c6e2-3d7f-4b95-9e20-5c7b1f6d4a38"

// retryLine is the intake's log line of a database step it tries again in place (untilDone).
const retryLine = "legion intake: a database step failed; trying it again"

var alice = model.Actor{Kind: "user", ID: "alice"}

// newRegistry is the eval registry over database, with the document service and broker a server
// hands it, and that document service.
func newRegistry(t *testing.T, database *store.Store) (*evals.Store, *docs.Service) {
	t.Helper()
	broker := events.NewBroker()
	// Settlement is an hour away, so no background settlement writes while a test reads.
	documents := docs.New(docs.Deps{Store: database, Events: broker, Settle: time.Hour})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down the document service: %v", err)
		}
	})
	return evals.New(database, documents, broker, delivery.NewAttributor(refs.Renames{})), documents
}

// registerAdmission registers project's admission eval through the register write: a logged eval
// of kind legion whose series admission-cap folds by its last value.
func registerAdmission(t *testing.T, registry *evals.Store, project string) {
	t.Helper()
	if _, err := registry.Register(t.Context(), evals.Registration{
		Project: project, Slug: "admission", Caller: alice,
		Name: evals.Set("Admission"), Kind: evals.Set(evals.KindLegion), Unit: evals.Set("issues"),
		Series: []evals.SeriesChange{{Series: "admission-cap", Unit: evals.Set("issues"), Agg: evals.Set(evals.AggLast)}},
	}); err != nil {
		t.Fatalf("register the admission eval of %s: %v", project, err)
	}
}

// admissionRig is one test's intake (legion.IntakeRig), whose intakes run the admission writer
// over the registry of its database, and the document service that registry writes `evals-md`
// through.
type admissionRig struct {
	*legion.IntakeRig
	t          *testing.T
	registry   *evals.Store
	documents  *docs.Service
	admissions legion.Admissions
}

// newAdmissionRig is a rig whose intakes run the admission writer, with INFRA named to a daemon
// that serves state, and INFRA's admission eval registered by hand where register is set.
func newAdmissionRig(t *testing.T, state string, register bool) (*admissionRig, *legion.FakeDaemon) {
	t.Helper()
	rig := legion.NewIntakeRig(t)
	registry, documents := newRegistry(t, rig.Database())
	admissions := evals.AdmissionWriter(registry, testBuild)
	rig.Admit(admissions)
	daemon := legion.NewFakeDaemon(t, state)
	legion.SeedLegionProject(t, rig.Database(), infra, daemon.URL)
	if register {
		registerAdmission(t, registry, infra)
	}
	return &admissionRig{IntakeRig: rig, t: t, registry: registry, documents: documents, admissions: admissions}, daemon
}

// readWhole reads INFRA's daemon at url whole for reason, with the rig's admission writer
// (legion.ReadWhole).
func (r *admissionRig) readWhole(url, reason string) {
	r.t.Helper()
	if err := legion.ReadWhole(r.t.Context(), r.Database().Pool, r.admissions, legion.Settings{Project: infra, StateURL: url}, reason); err != nil {
		r.t.Fatalf("read INFRA's daemon whole: %v", err)
	}
}

// requireLastSeq fails unless INFRA's cursor stands at want.
func (r *admissionRig) requireLastSeq(want int64) {
	r.t.Helper()
	if got := r.LastSeq(infra); got == nil || *got != want {
		stands := "no sequence"
		if got != nil {
			stands = strconv.FormatInt(*got, 10)
		}
		r.t.Fatalf("INFRA's cursor stands at %s, want %d", stands, want)
	}
}

// encode is value's JSON.
func encode(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %v: %v", value, err)
	}
	return string(encoded)
}

// stateIn is a state document of project's daemon at stateSeq in the store stateID under run,
// whose settings carry a dispatch_url, with admission as its admission block and no issue.
func stateIn(t *testing.T, project string, stateSeq int, stateID, run string, admission map[string]any) string {
	t.Helper()
	return encode(t, map[string]any{
		"daemon":    map[string]any{"project": project, "schemaVersion": legion.StateSchema, "stateSeq": stateSeq, "stateId": stateID, "stateRun": run},
		"settings":  map[string]any{"project": project, "dispatch_url": "http://dispatch.internal.example"},
		"admission": admission,
		"issues":    map[string]any{},
	})
}

// stateAt is stateIn for INFRA in the rig's store under the rig's run.
func stateAt(t *testing.T, stateSeq int, admission map[string]any) string {
	t.Helper()
	return stateIn(t, infra, stateSeq, legion.RigStateID, legion.RigRun, admission)
}

// statePayload is the payload of one state message of project under the rig's run and store,
// committed at at, carrying events, with `more` where more of its transaction is to come.
func statePayload(t *testing.T, project string, at time.Time, more bool, events ...map[string]any) []byte {
	t.Helper()
	return []byte(encode(t, map[string]any{
		"project": project, "run": legion.RigRun, "stateId": legion.RigStateID,
		"firstSeq": events[0]["seq"], "seq": events[len(events)-1]["seq"],
		"at": at.Format(time.RFC3339Nano), "more": more, "events": events,
	}))
}

// goldenEntries is the record (`capChanges` or `signalChanges`) of the admission block of file, a
// copy of the daemon's own golden (testdata/README.md), each entry as the daemon serves it.
func goldenEntries(t *testing.T, file, record string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatalf("read the daemon's golden %s: %v", file, err)
	}
	var document struct {
		Admission map[string]json.RawMessage `json:"admission"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode the daemon's golden %s: %v", file, err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(document.Admission[record], &entries); err != nil || len(entries) == 0 {
		t.Fatalf("decode %s of the daemon's golden %s: %v, %d entries", record, file, err, len(entries))
	}
	return entries
}

// goldenCaps is INFRA-5616's golden `capChanges`, newest first: entry 2, a cap the controller set,
// and entry 1, a clear the operator made (its `cap` absent).
func goldenCaps(t *testing.T) []map[string]any {
	t.Helper()
	return goldenEntries(t, "state-capchanges.json", "capChanges")
}

// goldenSignals is INFRA-5899's golden `signalChanges`, newest first: a hold (5), a value (4), a
// limit the controller set (3), a value the daemon cannot compute (2) and the switch (1).
func goldenSignals(t *testing.T) []map[string]any {
	t.Helper()
	return goldenEntries(t, "state-signalchanges.json", "signalChanges")
}

// capChange is an entry of `capChanges` in the daemon's shape: the cap set from before to after
// under ceiling 8, by the operator, at setAt, for reason.
func capChange(id, before, after int, setAt time.Time, reason string) map[string]any {
	return map[string]any{"id": id, "cap": after, "capBefore": before, "capAfter": after, "ceiling": 8,
		"setBy": map[string]any{"kind": "operator"}, "setAt": setAt.Format(time.RFC3339Nano), "reason": reason}
}

// reading is one reading of INFRA's admission eval as eval_readings holds it.
type reading struct {
	RowKind, RowKey, Series, Col, Unit, Absent, Note string
	Value                                            *float64
	MeasuredAt                                       time.Time
	Attributes                                       map[string]any
}

// String is the reading in every field the tests read, its attributes as sorted JSON.
func (r reading) String() string {
	value := "none"
	if r.Value != nil {
		value = strconv.FormatFloat(*r.Value, 'g', -1, 64)
	}
	attributes, _ := json.Marshal(r.Attributes)
	return fmt.Sprintf("{%s %s series=%s col=%q unit=%q at=%s value=%s absent=%q attributes=%s note=%q}", r.RowKind, r.RowKey, r.Series, r.Col,
		r.Unit, r.MeasuredAt.UTC().Format(time.RFC3339Nano), value, r.Absent, attributes, r.Note)
}

// readingTexts is readings as their texts, in the order of their row kind and row key.
func readingTexts(readings []reading) []string {
	slices.SortFunc(readings, func(a, b reading) int {
		return cmp.Or(cmp.Compare(a.RowKind, b.RowKind), cmp.Compare(a.RowKey, b.RowKey))
	})
	texts := make([]string, len(readings))
	for index, reading := range readings {
		texts[index] = reading.String()
	}
	return texts
}

// readingsOf is every reading of project's admission eval.
func readingsOf(t *testing.T, database *store.Store, project string) []reading {
	t.Helper()
	rows, err := database.Pool.Query(t.Context(), `
		select row_kind, row_key, series, col, unit, value_absent_reason, note, value, measured_at, attributes
		from eval_readings where eval_id = $1
	`, project+"/admission")
	if err != nil {
		t.Fatalf("read the admission readings of %s: %v", project, err)
	}
	readings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (reading, error) {
		var r reading
		err := row.Scan(&r.RowKind, &r.RowKey, &r.Series, &r.Col, &r.Unit, &r.Absent, &r.Note, &r.Value, &r.MeasuredAt, &r.Attributes)
		return r, err
	})
	if err != nil {
		t.Fatalf("read the admission readings of %s: %v", project, err)
	}
	return readings
}

// requireReadings fails unless INFRA's admission eval holds want and nothing else.
func (r *admissionRig) requireReadings(why string, want ...reading) {
	r.t.Helper()
	if got, wanted := readingTexts(readingsOf(r.t, r.Database(), infra)), readingTexts(want); !slices.Equal(got, wanted) {
		r.t.Fatalf("%s: the admission readings are\n%s\nwant\n%s", why, strings.Join(got, "\n"), strings.Join(wanted, "\n"))
	}
}

// key is the row key of an entry or a state of the rig's store.
func key(id int) string { return fmt.Sprintf("%s:%d", legion.RigStateID, id) }

// at is a time of the daemon's goldens.
func at(t *testing.T, text string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatalf("parse %s: %v", text, err)
	}
	return parsed
}

// The cap_change readings of the two entries of INFRA-5616's golden, in the rig's store.
func goldenCapReadings(t *testing.T) []reading {
	t.Helper()
	return []reading{
		{RowKind: "cap_change", RowKey: key(1), Series: "admission-cap", Unit: "issues", Value: new(8.0), MeasuredAt: at(t, "2026-09-21T18:00:00Z"),
			Attributes: map[string]any{"cap_before": 3, "ceiling": 8, "set_by": map[string]any{"kind": "operator"}, "reason": "the provider outage is over"},
			Note:       "cap 3 to 8, its setting cleared, ceiling 8, set by operator: the provider outage is over"},
		{RowKind: "cap_change", RowKey: key(2), Series: "admission-cap", Unit: "issues", Value: new(5.0), MeasuredAt: at(t, "2026-09-22T09:00:00Z"),
			Attributes: map[string]any{"cap_before": 8, "ceiling": 8, "set_by": map[string]any{"kind": "controller", "session": "ses_controller"},
				"reason": "the merge queue waits 52 minutes, above its limit of 45"},
			Note: "cap 8 to 5, ceiling 8, set by controller (session ses_controller): the merge queue waits 52 minutes, above its limit of 45"},
	}
}

// The signal_change readings of the five entries of INFRA-5899's golden, in the rig's store.
func goldenSignalReadings(t *testing.T) []reading {
	t.Helper()
	pass, earlier, first := at(t, "2026-11-26T07:00:00Z"), at(t, "2026-11-26T06:00:00Z"), at(t, "2026-11-26T05:00:00Z")
	none := map[string]any{"set_by": nil, "reason": nil}
	return []reading{
		{RowKind: "signal_change", RowKey: key(1), Series: "admission-switch", Value: new(1.0), MeasuredAt: first,
			Attributes: map[string]any{"set_by": map[string]any{"kind": "operator"}, "reason": "the owner turned admission by signals on"},
			Note:       "switch 1, set by operator: the owner turned admission by signals on"},
		{RowKind: "signal_change", RowKey: key(2), Series: "admission-queue", Col: "value", Unit: "hours", MeasuredAt: first,
			Absent: "the daemon has not begun keeping the merge queue's events", Attributes: none,
			Note: "queue value none (the daemon has not begun keeping the merge queue's events)"},
		{RowKind: "signal_change", RowKey: key(3), Series: "admission-kills", Col: "limit", Unit: "count", Value: new(2.0), MeasuredAt: earlier,
			Attributes: map[string]any{"set_by": map[string]any{"kind": "controller", "session": "ses_controller"}, "reason": "the owner set the limit in the weekly plan"},
			Note:       "kills limit 2, set by controller (session ses_controller): the owner set the limit in the weekly plan"},
		{RowKind: "signal_change", RowKey: key(4), Series: "admission-kills", Col: "value", Unit: "count", Value: new(3.0), MeasuredAt: pass,
			Attributes: none, Note: "kills value 3"},
		{RowKind: "signal_change", RowKey: key(5), Series: "admission-holds", Col: "kills", Value: new(1.0), MeasuredAt: pass,
			Attributes: none, Note: "kills holds 1"},
	}
}

// observedReading is the cap_observed reading of the cap reported by the state at seq of the rig's
// store, measured then.
func observedReading(seq, reported int, measured time.Time) reading {
	return reading{RowKind: "cap_observed", RowKey: key(seq), Series: "admission-cap", Unit: "issues", Value: new(float64(reported)),
		MeasuredAt: measured.UTC().Truncate(time.Microsecond), Attributes: map[string]any{"set_by": map[string]any{"kind": "daemon"}},
		Note: fmt.Sprintf("cap %d, as the daemon's state at sequence %d reports it", reported, seq)}
}

// newestReadAt is the time of INFRA's newest whole read (legion_reads.read_at), which a cap a
// whole read observes is measured at.
func newestReadAt(t *testing.T, database *store.Store) time.Time {
	t.Helper()
	var readAt time.Time
	if err := database.Pool.QueryRow(t.Context(), `
		select read_at from legion_reads where project = $1 and kind = 'read' order by id desc limit 1
	`, infra).Scan(&readAt); err != nil {
		t.Fatalf("read INFRA's newest whole read: %v", err)
	}
	return readAt
}

// skippedLines is each record of evals.AdmissionSkippedLine the rig's intakes logged, as
// "<row_kind> <row_key> <code>".
func (r *admissionRig) skippedLines() []string {
	var lines []string
	for _, record := range r.LogRecords(evals.AdmissionSkippedLine) {
		if record["level"] == "WARN" && record["project"] == infra {
			lines = append(lines, fmt.Sprintf("%v %v %v", record["row_kind"], record["row_key"], record["code"]))
		}
	}
	slices.Sort(lines)
	return lines
}

// TestAnAdmissionEventWritesOneReadingForEachCapEntry: an admission event that carries the two
// entries of INFRA-5616's golden writes one cap_change reading of each, keyed `<stateId>:<id>`, at
// its setAt, of its capAfter (a clear's too, whose entry carries no cap), with the cap before, the
// ceiling, who set it and why as its attributes and in its note. The same event again adds no
// reading, and an event that carries a third entry beside them adds that one alone: three
// readings. Each state's cap is its newest entry's, so no cap is observed.
func TestAnAdmissionEventWritesOneReadingForEachCapEntry(t *testing.T) {
	rig, _ := newAdmissionRig(t, stateAt(t, 10, map[string]any{}), false)
	rig.Start()
	rig.AwaitStartingRead(infra)
	rig.requireReadings("a starting state with no cap and no record")

	caps := goldenCaps(t)
	rig.Publish(infra, legion.AdmissionEvent(11, map[string]any{"cap": 5, "capChanges": caps}))
	rig.Settled()
	want := goldenCapReadings(t)
	rig.requireReadings("an event with the golden's two entries", want...)

	rig.Publish(infra, legion.AdmissionEvent(12, map[string]any{"cap": 5, "capChanges": caps}))
	rig.Settled()
	rig.requireReadings("the same event again", want...)

	third := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	rig.Publish(infra, legion.AdmissionEvent(13, map[string]any{"cap": 6,
		"capChanges": append([]map[string]any{capChange(3, 5, 6, third, "the release needs one more slot")}, caps...)}))
	rig.Settled()
	want = append(want, reading{RowKind: "cap_change", RowKey: key(3), Series: "admission-cap", Unit: "issues", Value: new(6.0), MeasuredAt: third,
		Attributes: map[string]any{"cap_before": 5, "ceiling": 8, "set_by": map[string]any{"kind": "operator"}, "reason": "the release needs one more slot"},
		Note:       "cap 5 to 6, ceiling 8, set by operator: the release needs one more slot"})
	rig.requireReadings("an event with a third entry beside the two", want...)
	rig.requireLastSeq(13)
	if skipped := rig.skippedLines(); len(skipped) != 0 {
		t.Errorf("the writer skipped %v, want no reading skipped", skipped)
	}
}

// TestAWholeReadOfAWrittenStateAddsNoReading: a whole read of the state whose entries a batch
// wrote, its cap the newest entry's, adds no reading: every row kind and row key it gives is held.
func TestAWholeReadOfAWrittenStateAddsNoReading(t *testing.T) {
	rig, daemon := newAdmissionRig(t, stateAt(t, 10, map[string]any{}), false)
	rig.Start()
	rig.AwaitStartingRead(infra)
	block := map[string]any{"cap": 5, "capChanges": goldenCaps(t)}
	rig.Publish(infra, legion.AdmissionEvent(11, block))
	rig.Settled()
	want := goldenCapReadings(t)
	rig.requireReadings("the batch", want...)

	daemon.Serves(stateAt(t, 11, block))
	rig.readWhole(daemon.URL, legion.ReadReasonRetry)
	rig.AwaitWholeReads(2)
	rig.requireReadings("a whole read of the state the batch wrote", want...)
}

// TestTheFirstStateOfAProjectWritesTheCapItObserves: the first state a project's intake applies,
// here its starting read, whose cap no reading of the eval holds, writes one cap_observed reading of
// that cap, set by the daemon, keyed `<stateId>:<stateSeq>`, at the time Dispatch read the state,
// though its record of changes is empty.
func TestTheFirstStateOfAProjectWritesTheCapItObserves(t *testing.T) {
	rig, _ := newAdmissionRig(t, stateAt(t, 10, map[string]any{"cap": 2, "capChanges": []any{}}), false)
	rig.Start()
	rig.AwaitStartingRead(infra)
	rig.requireReadings("the starting read", observedReading(10, 2, newestReadAt(t, rig.Database())))
}

// TestACapNoEntryExplainsIsObserved: a state whose cap differs from the newest admission-cap
// reading once its entries are written, as after a boot that read another admission_cap, writes a
// cap_observed reading of that cap at the message's time, keyed by its seq.
func TestACapNoEntryExplainsIsObserved(t *testing.T) {
	set := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	entries := []map[string]any{capChange(1, 2, 4, set, "the provider recovered")}
	rig, _ := newAdmissionRig(t, stateAt(t, 10, map[string]any{"cap": 4, "capChanges": entries}), false)
	rig.Start()
	rig.AwaitStartingRead(infra)
	entry := reading{RowKind: "cap_change", RowKey: key(1), Series: "admission-cap", Unit: "issues", Value: new(4.0), MeasuredAt: set,
		Attributes: map[string]any{"cap_before": 2, "ceiling": 8, "set_by": map[string]any{"kind": "operator"}, "reason": "the provider recovered"},
		Note:       "cap 2 to 4, ceiling 8, set by operator: the provider recovered"}
	rig.requireReadings("a starting state whose cap its entry explains", entry)

	committed := rig.Publish(infra, legion.AdmissionEvent(11, map[string]any{"cap": 6, "capChanges": entries}))
	rig.Settled()
	rig.requireReadings("a state whose cap 6 no entry explains", entry, observedReading(11, 6, committed))
}

// TestAnObservedSeqAndAnEntryIdOfOneNumberEachHaveAReading: the state at seq 11 carries an entry of
// id 11 and a cap that entry does not explain, so its cap_change and its cap_observed readings are
// both keyed `<stateId>:11`: each is written, since a reading stands for another only where both
// its row kind and its row key are the other's. The next state, whose cap the newest reading holds,
// adds neither again.
func TestAnObservedSeqAndAnEntryIdOfOneNumberEachHaveAReading(t *testing.T) {
	rig, _ := newAdmissionRig(t, stateAt(t, 10, map[string]any{}), false)
	rig.Start()
	rig.AwaitStartingRead(infra)
	set := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	entries := []map[string]any{capChange(11, 3, 5, set, "the queue is short")}
	committed := rig.Publish(infra, legion.AdmissionEvent(11, map[string]any{"cap": 7, "capChanges": entries}))
	rig.Settled()
	want := []reading{
		{RowKind: "cap_change", RowKey: key(11), Series: "admission-cap", Unit: "issues", Value: new(5.0), MeasuredAt: set,
			Attributes: map[string]any{"cap_before": 3, "ceiling": 8, "set_by": map[string]any{"kind": "operator"}, "reason": "the queue is short"},
			Note:       "cap 3 to 5, ceiling 8, set by operator: the queue is short"},
		observedReading(11, 7, committed),
	}
	rig.requireReadings("entry 11 and the cap state 11 observes", want...)

	rig.Publish(infra, legion.AdmissionEvent(12, map[string]any{"cap": 7, "capChanges": entries}))
	rig.Settled()
	rig.requireReadings("the next state, of the same entry and cap", want...)
}

// TestAReplacedStoresEntriesEachHaveAReading: a daemon whose store was replaced counts its entries'
// ids from 1 again. Its first state in the new store, read whole for the message of that store that
// calls for the read, writes a reading of each of its cap and signal entries keyed by the new
// store's id, beside the old store's readings of the same ids.
func TestAReplacedStoresEntriesEachHaveAReading(t *testing.T) {
	rig, daemon := newAdmissionRig(t, stateAt(t, 10, map[string]any{"cap": 5, "capChanges": goldenCaps(t), "signalChanges": goldenSignals(t)}), false)
	rig.Start()
	rig.AwaitStartingRead(infra)
	before := append(goldenCapReadings(t), goldenSignalReadings(t)...)
	rig.requireReadings("the old store's state", before...)

	now := time.Now().UTC().Truncate(time.Microsecond)
	early, late := now.Add(-2*time.Hour), now.Add(-time.Hour)
	daemon.Serves(stateIn(t, infra, 3, legion.OtherStateID, otherRun, map[string]any{"cap": 4,
		"capChanges": []map[string]any{capChange(2, 3, 4, late, "the replaced store's second change"), capChange(1, 5, 3, early, "the replaced store's first change")},
		"signalChanges": []map[string]any{
			{"id": 2, "at": late.Format(time.RFC3339Nano), "signal": "kills", "part": nil, "what": "value", "value": 1, "absent": nil, "setBy": nil, "reason": nil},
			{"id": 1, "at": early.Format(time.RFC3339Nano), "signal": "idle", "part": nil, "what": "value", "value": 0.5, "absent": nil, "setBy": nil, "reason": nil},
		}}))
	rig.PublishIn(infra, otherRun, legion.OtherStateID, now, legion.PhaseEvent(4, "INFRA-1", "implementing"))
	rig.Settled()
	rig.AwaitWholeReads(2)
	replaced := func(id int) string { return fmt.Sprintf("%s:%d", legion.OtherStateID, id) }
	none := map[string]any{"set_by": nil, "reason": nil}
	rig.requireReadings("the replaced store's first state, read whole", append(before,
		reading{RowKind: "cap_change", RowKey: replaced(1), Series: "admission-cap", Unit: "issues", Value: new(3.0), MeasuredAt: early,
			Attributes: map[string]any{"cap_before": 5, "ceiling": 8, "set_by": map[string]any{"kind": "operator"}, "reason": "the replaced store's first change"},
			Note:       "cap 5 to 3, ceiling 8, set by operator: the replaced store's first change"},
		reading{RowKind: "cap_change", RowKey: replaced(2), Series: "admission-cap", Unit: "issues", Value: new(4.0), MeasuredAt: late,
			Attributes: map[string]any{"cap_before": 3, "ceiling": 8, "set_by": map[string]any{"kind": "operator"}, "reason": "the replaced store's second change"},
			Note:       "cap 3 to 4, ceiling 8, set by operator: the replaced store's second change"},
		reading{RowKind: "signal_change", RowKey: replaced(1), Series: "admission-idle", Col: "value", Unit: "share", Value: new(0.5), MeasuredAt: early,
			Attributes: none, Note: "idle value 0.5"},
		reading{RowKind: "signal_change", RowKey: replaced(2), Series: "admission-kills", Col: "value", Unit: "count", Value: new(1.0), MeasuredAt: late,
			Attributes: none, Note: "kills value 1"},
	)...)
	rig.requireLastSeq(4)
}

// TestAFailedReadingsWriteRollsTheBatchBackAndTheRetryAppliesItOnce: a readings write that fails
// with an error a retry may fix (a trigger's exception, SQLSTATE class P0, outside classes 22 and
// 23) inside the batch's transaction fails the batch: none of its observations and no reading is
// written, the cursor does not move, and the intake tries the message again in place. Once the
// write goes through, the retry applies the batch once: its phase, its readings and one legion_reads
// row of kind `events`.
func TestAFailedReadingsWriteRollsTheBatchBackAndTheRetryAppliesItOnce(t *testing.T) {
	rig, _ := newAdmissionRig(t, stateAt(t, 10, map[string]any{}), false)
	rig.Retry(20*time.Millisecond, 100*time.Millisecond)
	rig.Start()
	rig.AwaitStartingRead(infra)
	if _, err := rig.Database().Pool.Exec(t.Context(), `
		create function refuse_admission_readings() returns trigger language plpgsql as $$
		begin
			raise exception 'the test refuses every admission reading';
		end;
		$$;
		create trigger refuse_admission_readings before insert on eval_readings
			for each statement execute function refuse_admission_readings();
	`); err != nil {
		t.Fatalf("refuse the readings: %v", err)
	}
	rig.Publish(infra, legion.PhaseEvent(11, "INFRA-1", "implementing"), legion.AdmissionEvent(12, map[string]any{"cap": 5, "capChanges": goldenCaps(t)}))
	retried := func() int {
		count := 0
		for _, line := range rig.LogRecords(retryLine) {
			if line["project"] == infra && strings.Contains(fmt.Sprint(line["error"]), "the test refuses every admission reading") {
				count++
			}
		}
		return count
	}
	rig.WaitFor(20*time.Second, func() bool { return retried() >= 3 }, func() string {
		return fmt.Sprintf("the batch whose readings write fails was tried %d times, want it tried again in place", retried())
	})
	rig.requireLastSeq(10)
	if phase, observed := rig.Current(infra)["INFRA-1/phase"]; observed {
		t.Fatalf("INFRA-1's phase is %s while the readings write fails, want none: the batch rolls back whole", phase)
	}
	rig.requireReadings("while the readings write fails")

	if _, err := rig.Database().Pool.Exec(t.Context(), `drop trigger refuse_admission_readings on eval_readings`); err != nil {
		t.Fatalf("take the readings again: %v", err)
	}
	rig.Settled()
	rig.requireLastSeq(12)
	if phase := rig.Current(infra)["INFRA-1/phase"]; phase != `"implementing"` {
		t.Errorf("INFRA-1's phase is %s, want the retried batch's implementing", phase)
	}
	rig.requireReadings("after the retry", goldenCapReadings(t)...)
	var batches int
	if err := rig.Database().Pool.QueryRow(t.Context(), `select count(*) from legion_reads where project = $1 and kind = 'events'`, infra).Scan(&batches); err != nil {
		t.Fatalf("count INFRA's batches: %v", err)
	}
	if batches != 1 {
		t.Errorf("%d batches recorded, want the message applied once", batches)
	}
}

// TestARefusedEntryIsLoggedAndSkipped: an entry whose reading R29's rules refuse, here one whose
// setAt is before the earliest time a reading may hold (evals.MeasuredAtFloor), is logged at WARN
// with its code and row key and skipped; the state's other entries are written, and the intake goes
// on to its next message.
func TestARefusedEntryIsLoggedAndSkipped(t *testing.T) {
	rig, _ := newAdmissionRig(t, stateAt(t, 10, map[string]any{}), true)
	rig.Start()
	rig.AwaitStartingRead(infra)
	early := evals.MeasuredAtFloor.Add(-24 * time.Hour)
	rig.Publish(infra, legion.AdmissionEvent(11, map[string]any{"cap": 5,
		"capChanges": append([]map[string]any{capChange(3, 8, 5, early, "an entry before the floor")}, goldenCaps(t)...)}))
	rig.Publish(infra, legion.PhaseEvent(12, "INFRA-1", "implementing"))
	rig.Settled()
	rig.requireReadings("a state with one refused entry", goldenCapReadings(t)...)
	if skipped, want := rig.skippedLines(), []string{"cap_change " + key(3) + " " + evals.CodeMeasuredAtOutOfRange}; !slices.Equal(skipped, want) {
		t.Errorf("the writer logged the skipped readings %v, want %v", skipped, want)
	}
	rig.requireLastSeq(12)
	if phase := rig.Current(infra)["INFRA-1/phase"]; phase != `"implementing"` {
		t.Errorf("INFRA-1's phase is %s, want the next message's implementing", phase)
	}
}

// TestAReadingOfAnEvalNoRowHoldsIsLoggedAndSkipped: a project that holds no row of its admission
// eval, since the register write refuses it (its `evals-md` is a file,
// evals.CodeDocumentNotDocument), and whose state carries `capChanges`, has the refusal logged with
// its code (evals.AdmissionRefusedLine) and each reading logged at WARN as one of an eval no row
// holds (evals.CodeEvalNotFound), with its row key, and skipped; the writer registers no eval, and
// the batch applies.
func TestAReadingOfAnEvalNoRowHoldsIsLoggedAndSkipped(t *testing.T) {
	rig, _ := newAdmissionRig(t, stateAt(t, 10, map[string]any{}), false)
	if _, err := rig.Database().Pool.Exec(t.Context(), `
		insert into artifacts (project_key, slug, name, kind, created_by) values ($1, 'evals-md', 'evals.md', 'file', '{"kind":"user","id":"alice"}')
	`, infra); err != nil {
		t.Fatalf("make INFRA's evals-md a file: %v", err)
	}
	rig.Start()
	rig.AwaitStartingRead(infra)
	rig.Publish(infra, legion.AdmissionEvent(11, map[string]any{"cap": 5, "capChanges": goldenCaps(t), "active": []string{"INFRA-1"}}))
	rig.Settled()
	rig.requireReadings("a state of a project with no admission eval")
	want := []string{
		"cap_change " + key(1) + " " + evals.CodeEvalNotFound,
		"cap_change " + key(2) + " " + evals.CodeEvalNotFound,
		"cap_observed " + key(11) + " " + evals.CodeEvalNotFound,
	}
	if skipped := rig.skippedLines(); !slices.Equal(skipped, want) {
		t.Errorf("the writer logged the skipped readings %v, want %v", skipped, want)
	}
	var refused []string
	for _, record := range rig.LogRecords(evals.AdmissionRefusedLine) {
		if record["level"] == "WARN" && record["project"] == infra {
			refused = append(refused, fmt.Sprint(record["code"]))
		}
	}
	if want := []string{evals.CodeDocumentNotDocument}; !slices.Equal(refused, want) {
		t.Errorf("the writer logged the refused registrations %v, want %v", refused, want)
	}
	var versions int
	if err := rig.Database().Pool.QueryRow(t.Context(), `select count(*) from eval_versions where eval_id = $1`, admissionID).Scan(&versions); err != nil {
		t.Fatalf("count the admission eval's versions: %v", err)
	}
	if versions != 0 {
		t.Errorf("the admission eval holds %d versions, want none: the writer registers no eval", versions)
	}
	rig.requireLastSeq(11)
	if admission := rig.Current(infra)["/admission"]; !strings.Contains(admission, "INFRA-1") {
		t.Errorf("INFRA's admission observation is %s, want the batch's block", admission)
	}
}

// TestAFailedWholeReadWritesNoReading: a whole read that fails, here of the project's own state
// with a stateId and a stateSeq whose settings carry no dispatch_url (legion.ErrNoDispatchURL), with
// a cap no reading holds and the golden's entries, writes no reading, though it records the state's
// observations. The starting read, whose record of the cap is empty, registers the eval and writes
// the cap it observes.
func TestAFailedWholeReadWritesNoReading(t *testing.T) {
	rig, daemon := newAdmissionRig(t, stateAt(t, 10, map[string]any{"cap": 3, "capChanges": []any{}}), false)
	rig.Start()
	rig.AwaitStartingRead(infra)
	want := observedReading(10, 3, newestReadAt(t, rig.Database()))
	rig.requireReadings("the starting read", want)

	failed := strings.Replace(stateAt(t, 20, map[string]any{"cap": 6, "capChanges": goldenCaps(t)}), `"dispatch_url":"http://dispatch.internal.example",`, "", 1)
	if failed == stateAt(t, 20, map[string]any{"cap": 6, "capChanges": goldenCaps(t)}) {
		t.Fatal("the failed read's state still carries a dispatch_url")
	}
	daemon.Serves(failed)
	rig.readWhole(daemon.URL, legion.ReadReasonRetry)
	var ok bool
	var failure *string
	if err := rig.Database().Pool.QueryRow(t.Context(), `
		select ok, error from legion_reads where project = $1 and kind = 'read' order by id desc limit 1
	`, infra).Scan(&ok, &failure); err != nil {
		t.Fatalf("read INFRA's newest whole read: %v", err)
	}
	if ok || failure == nil || !strings.Contains(*failure, legion.ErrNoDispatchURL.Error()) {
		t.Fatalf("INFRA's newest whole read is ok %t with the error %v, want it failed for %q", ok, failure, legion.ErrNoDispatchURL)
	}
	if admission := rig.Current(infra)["/admission"]; !strings.Contains(admission, `"cap": 6`) {
		t.Errorf("INFRA's admission observation is %s, want the failed read's cap 6: the state is the project's own", admission)
	}
	rig.requireReadings("a failed whole read", want)
}

// TestEachSignalEntryWritesOneSignalReading: each entry of a state's `signalChanges` writes one
// signal_change reading in its series, `col` and unit, keyed `<stateId>:<id>`, at its `at`: every
// what of INFRA-5899's golden (a hold, a value, a limit, a value the daemon cannot compute, whose
// reading has none and its `absent` as the reason, and the switch), and, added by hand, a limit a
// person cleared, whose reading has none and `limit cleared` as the reason. Its attributes are who
// set it and why (null where the entry names neither), and its note names the signal, what changed
// and the new value, and for a limit or the switch who set it and why.
func TestEachSignalEntryWritesOneSignalReading(t *testing.T) {
	cleared := map[string]any{"id": 6, "at": "2026-11-26T08:00:00Z", "signal": "queue", "part": nil, "what": "limit", "value": nil,
		"absent": nil, "setBy": map[string]any{"kind": "operator"}, "reason": "the owner cleared the queue's limit"}
	rig, _ := newAdmissionRig(t, stateAt(t, 10, map[string]any{"signalChanges": append([]map[string]any{cleared}, goldenSignals(t)...)}), false)
	rig.Start()
	rig.AwaitStartingRead(infra)
	rig.requireReadings("a state with every what", append(goldenSignalReadings(t),
		reading{RowKind: "signal_change", RowKey: key(6), Series: "admission-queue", Col: "limit", Unit: "hours", MeasuredAt: at(t, "2026-11-26T08:00:00Z"),
			Absent: "limit cleared", Attributes: map[string]any{"set_by": map[string]any{"kind": "operator"}, "reason": "the owner cleared the queue's limit"},
			Note: "queue limit cleared, set by operator: the owner cleared the queue's limit"})...)
}

// TestARepeatedSignalEventAddsNoReading: the same signal entries again, in a later event and in a
// whole read of the state that carries them, add no reading.
func TestARepeatedSignalEventAddsNoReading(t *testing.T) {
	rig, daemon := newAdmissionRig(t, stateAt(t, 10, map[string]any{}), false)
	rig.Start()
	rig.AwaitStartingRead(infra)
	block := map[string]any{"signalChanges": goldenSignals(t)}
	rig.Publish(infra, legion.AdmissionEvent(11, block))
	rig.Settled()
	want := goldenSignalReadings(t)
	rig.requireReadings("the first event", want...)

	rig.Publish(infra, legion.AdmissionEvent(12, block))
	rig.Settled()
	rig.requireReadings("the same event again", want...)
	daemon.Serves(stateAt(t, 12, block))
	rig.readWhole(daemon.URL, legion.ReadReasonRetry)
	rig.requireReadings("a whole read of the same state", want...)
	rig.requireLastSeq(12)
}

// TestABlockWithNoSignalChangesAddsNoneAndFailsNothing: after the golden's signal readings, an
// admission event whose block carries no `signalChanges`, and whose cap is the newest admission-cap
// reading's, adds no reading and skips none, and the batch applies.
func TestABlockWithNoSignalChangesAddsNoneAndFailsNothing(t *testing.T) {
	rig, _ := newAdmissionRig(t, stateAt(t, 10, map[string]any{"cap": 5, "signalChanges": goldenSignals(t)}), false)
	rig.Start()
	rig.AwaitStartingRead(infra)
	want := append(goldenSignalReadings(t), observedReading(10, 5, newestReadAt(t, rig.Database())))
	rig.requireReadings("the starting read", want...)

	rig.Publish(infra, legion.AdmissionEvent(11, map[string]any{"cap": 5, "active": []string{"INFRA-1"}}))
	rig.Settled()
	rig.requireReadings("a block with no signalChanges", want...)
	if skipped := rig.skippedLines(); len(skipped) != 0 {
		t.Errorf("the writer skipped %v, want nothing", skipped)
	}
	rig.requireLastSeq(11)
	if admission := rig.Current(infra)["/admission"]; !strings.Contains(admission, "INFRA-1") {
		t.Errorf("INFRA's admission observation is %s, want the batch's block", admission)
	}
}

// TestAnUnknownSignalEntryIsLoggedAndSkipped: an entry whose signal, part or what the writer does
// not know, added by hand beside the golden's value of kills (a `provider` entry of the part
// `refusals`, which the record no longer holds, and a `what` of `other`), is logged at WARN with its
// row key and skipped, and the state's other entry is written.
func TestAnUnknownSignalEntryIsLoggedAndSkipped(t *testing.T) {
	rig, _ := newAdmissionRig(t, stateAt(t, 10, map[string]any{}), true)
	rig.Start()
	rig.AwaitStartingRead(infra)
	value := goldenSignals(t)[1]
	rig.Publish(infra, legion.AdmissionEvent(11, map[string]any{"signalChanges": []map[string]any{
		{"id": 8, "at": "2026-11-26T07:30:00Z", "signal": "idle", "part": nil, "what": "other", "value": 1, "absent": nil, "setBy": nil, "reason": nil},
		{"id": 7, "at": "2026-11-26T07:30:00Z", "signal": "provider", "part": "refusals", "what": "value", "value": 2, "absent": nil, "setBy": nil, "reason": nil},
		value,
	}}))
	rig.Settled()
	rig.requireReadings("a state with two unknown entries", goldenSignalReadings(t)[3])
	var unknown []string
	for _, record := range rig.LogRecords(evals.AdmissionUnknownLine) {
		if record["level"] == "WARN" && record["project"] == infra {
			unknown = append(unknown, fmt.Sprint(record["row_key"]))
		}
	}
	slices.Sort(unknown)
	if want := []string{key(7), key(8)}; !slices.Equal(unknown, want) {
		t.Errorf("the writer logged the unknown entries %v, want %v", unknown, want)
	}
	rig.requireLastSeq(11)
}

// lockTrace records the statements of each transaction run on the connections of the pool it
// traces, in the order each ran, a pipeline's (pgx.Batch) among them.
type lockTrace struct {
	mu     sync.Mutex
	open   map[*pgx.Conn][]string
	closed [][]string
}

func (l *lockTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	l.record(conn, data.SQL)
	return ctx
}

func (l *lockTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (l *lockTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (l *lockTrace) TraceBatchQuery(_ context.Context, conn *pgx.Conn, data pgx.TraceBatchQueryData) {
	l.record(conn, data.SQL)
}

func (l *lockTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// record adds statement to the transaction open on conn: a `begin` opens one, and a `commit` or a
// `rollback` closes it; a statement outside a transaction is no transaction's.
func (l *lockTrace) record(conn *pgx.Conn, sql string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	statement := strings.TrimSpace(sql)
	switch {
	case strings.HasPrefix(statement, "begin"):
		l.open[conn] = []string{}
	case statement == "commit" || statement == "rollback":
		if transaction, open := l.open[conn]; open {
			l.closed = append(l.closed, transaction)
			delete(l.open, conn)
		}
	default:
		if transaction, open := l.open[conn]; open {
			l.open[conn] = append(transaction, statement)
		}
	}
}

// take answers each transaction that ended since the last take.
func (l *lockTrace) take() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	taken := l.closed
	l.closed = nil
	return taken
}

// evalLock reports whether statement takes an eval's advisory lock (lockEvals' `hashtext('eval:'
// || id)`).
func evalLock(statement string) bool {
	return strings.Contains(statement, "pg_advisory_xact_lock") && strings.Contains(statement, "'eval:'")
}

// legionLock reports whether statement locks a row of legion_settings or legion_event_cursors:
// a read of one `for share` or `for update`, or a write of one.
func legionLock(statement string) bool {
	if !strings.Contains(statement, "legion_settings") && !strings.Contains(statement, "legion_event_cursors") {
		return false
	}
	for _, locks := range []string{"for share", "for update", "update legion_", "insert into legion_", "delete from legion_"} {
		if strings.Contains(statement, locks) {
			return true
		}
	}
	return false
}

// TestEveryIntakeTransactionTakesTheEvalLockFirst: the plan's lock order (D19, "The lock order").
// Each transaction of the intake, run under a pgx tracer, takes an eval's advisory lock only as its
// first statement, before any lock of a row of legion_settings or legion_event_cursors: applyBatch
// for an applied message and recordWhole for a good read take the admission eval's lock first, and
// applyBatch for a held part, recordWhole for a failed read, judgeCrowded and a write of the
// setting that resets the cursor (writeSetting, resetCursor) take none. The observations' lock
// (legion.ObservationsLockKey, an advisory lock of no eval, which no eval write takes) is not an
// eval's lock, and follows the cursor's in every batch and read by design. Each project's admission
// eval is registered by its first whole read, whose state serves capChanges, through the intake's
// gate. Then an admission batch, a readings write and a register write of one eval run at once, 50
// times, and none of them is refused with a deadlock (40P01).
func TestEveryIntakeTransactionTakesTheEvalLockFirst(t *testing.T) {
	trace := &lockTrace{open: map[*pgx.Conn][]string{}}
	database := storetest.Open(t)
	traced := storetest.TracedStore(t, database, trace)
	registry, _ := newRegistry(t, database)
	admissions := evals.AdmissionWriter(registry, testBuild)
	daemon := legion.NewFakeDaemon(t, stateAt(t, 10, map[string]any{"cap": 5, "capChanges": goldenCaps(t)}))
	legion.SeedLegionProject(t, database, infra, daemon.URL)
	other := legion.NewFakeDaemon(t, stateAt(t, 10, map[string]any{}))
	setting := legion.Settings{Project: infra, StateURL: daemon.URL}
	ctx, pool := t.Context(), traced.Pool
	now := time.Now().UTC()

	for _, step := range []struct {
		name string
		run  func() error
		// marks is what each statement of the step's transaction holds, and lockFirst whether the
		// transaction takes the admission eval's lock as its first statement or no eval lock at all.
		marks     []string
		lockFirst bool
	}{
		{name: "a good whole read (recordWhole)", lockFirst: true, marks: []string{"insert into legion_reads"},
			run: func() error { return legion.ReadWhole(ctx, pool, admissions, setting, legion.ReadReasonStart) }},
		{name: "an applied message (applyBatch)", lockFirst: true, marks: []string{"set last_seq = $2"},
			run: func() error {
				return legion.ApplyMessage(ctx, pool, admissions, statePayload(t, infra, now, false,
					legion.AdmissionEvent(11, map[string]any{"cap": 5, "capChanges": append([]map[string]any{capChange(3, 5, 5, now, "a cap set again")}, goldenCaps(t)...)})))
			}},
		{name: "a held part (applyBatch)", marks: []string{"insert into legion_event_parts"},
			run: func() error {
				return legion.ApplyMessage(ctx, pool, admissions, statePayload(t, infra, now, true, legion.AdmissionEvent(12, map[string]any{"cap": 6})))
			}},
		{name: "a message past the cap (judgeCrowded)", marks: []string{"from legion_event_cursors where project = $1 for update"},
			run: func() error {
				return legion.JudgeCrowded(ctx, pool, admissions, statePayload(t, infra, now, false, legion.PhaseEvent(14, "INFRA-1", "implementing")))
			}},
		{name: "a failed whole read (recordWhole)", marks: []string{"insert into legion_reads"},
			run: func() error {
				daemon.RefusesUnavailable()
				return legion.ReadWhole(ctx, pool, admissions, setting, legion.ReadReasonRetry)
			}},
		{name: "a write of the setting that resets the cursor (writeSetting, resetCursor)",
			marks: []string{"from legion_settings where project = $1 for update", "from legion_event_cursors where project = $1 for update"},
			run: func() error {
				_, err := legion.PutSettings(ctx, pool, infra, other.URL, alice)
				return err
			}},
	} {
		trace.take()
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		transactions := trace.take()
		found := false
		for _, transaction := range transactions {
			locked := ""
			for index, statement := range transaction {
				if evalLock(statement) && locked != "" {
					t.Errorf("%s: a transaction takes an eval's lock (statement %d) after it locked a Legion row with %q:\n%s",
						step.name, index, locked, strings.Join(transaction, "\n---\n"))
				}
				if legionLock(statement) && locked == "" {
					locked = statement
				}
			}
			if !slices.ContainsFunc(step.marks, func(mark string) bool {
				return !slices.ContainsFunc(transaction, func(statement string) bool { return strings.Contains(statement, mark) })
			}) {
				found = true
				first := len(transaction) > 0 && evalLock(transaction[0])
				takes := slices.ContainsFunc(transaction, evalLock)
				switch {
				case step.lockFirst && !first:
					t.Errorf("%s: its transaction does not take the admission eval's lock first:\n%s", step.name, strings.Join(transaction, "\n---\n"))
				case !step.lockFirst && takes:
					t.Errorf("%s: its transaction takes an eval's lock, want none:\n%s", step.name, strings.Join(transaction, "\n---\n"))
				}
			}
		}
		if !found {
			t.Errorf("%s: none of its %d transactions holds %q", step.name, len(transactions), step.marks)
		}
	}

	// An admission batch, a readings write and a register write of LINUX's admission eval, at once.
	linux := legion.NewFakeDaemon(t, stateIn(t, "LINUX", 20, legion.RigStateID, legion.RigRun, map[string]any{"cap": 3, "capChanges": []any{}}))
	legion.SeedLegionProject(t, database, "LINUX", linux.URL)
	if err := legion.ReadWhole(ctx, database.Pool, admissions, legion.Settings{Project: "LINUX", StateURL: linux.URL}, legion.ReadReasonStart); err != nil {
		t.Fatalf("read LINUX's daemon whole: %v", err)
	}
	const rounds = 50
	for round := range rounds {
		seq := 21 + round
		set := time.Now().UTC()
		payload := statePayload(t, "LINUX", set, false, legion.AdmissionEvent(seq, map[string]any{"cap": 4,
			"capChanges": []map[string]any{capChange(round+1, 3, 4, set, fmt.Sprintf("round %d", round))}}))
		row := encode(t, map[string]any{"series": "admission-cap", "row_kind": "manual", "row_key": strconv.Itoa(round),
			"measured_at": set.Format(time.RFC3339Nano), "value": round})
		writes := []func() error{
			func() error { return legion.ApplyMessage(ctx, database.Pool, admissions, payload) },
			func() error {
				_, err := registry.WriteReadings(ctx, evals.Batch{Project: "LINUX", Slug: "admission", Rows: []json.RawMessage{json.RawMessage(row)}, Caller: alice, BuiltBy: testBuild})
				return err
			},
			func() error {
				_, err := registry.Register(ctx, evals.Registration{Project: "LINUX", Slug: "admission", Caller: alice, Answer: evals.Set(fmt.Sprintf("round %d", round))})
				return err
			},
		}
		start := make(chan struct{})
		failures := make([]error, len(writes))
		var all sync.WaitGroup
		for index, write := range writes {
			all.Go(func() {
				<-start
				failures[index] = write()
			})
		}
		close(start)
		all.Wait()
		for index, err := range failures {
			var refusal *pgconn.PgError
			if errors.As(err, &refusal) && refusal.Code == "40P01" {
				t.Fatalf("round %d: write %d of the admission batch, the readings write and the register write deadlocked: %v", round, index, err)
			}
			if err != nil {
				t.Fatalf("round %d: write %d of the admission batch, the readings write and the register write: %v", round, index, err)
			}
		}
	}
	var changes, manual int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) filter (where row_kind = 'cap_change'), count(*) filter (where row_kind = 'manual')
		from eval_readings where eval_id = 'LINUX/admission'
	`).Scan(&changes, &manual); err != nil {
		t.Fatalf("count LINUX's admission readings: %v", err)
	}
	if changes != rounds || manual != rounds {
		t.Errorf("LINUX's admission eval holds %d cap_change and %d manual readings, want %d of each", changes, manual, rounds)
	}
}
