package legion_test

// admission_registration_test.go holds the tests of the registration of the admission eval and its
// gate in the intake (dispatch://INFRA-2918, R22 "The registration", D18; checks k27 and k29): the
// intake registers a project's eval `<KEY>/admission` through the register write, in that write's
// own transaction, before the transaction of the first state it applies that serves either record
// of the daemon (`capChanges` or `signalChanges`), once; a state that does not apply, a failed whole
// read and a state that serves neither record register nothing. Like admission_test.go's, the tests
// are outside package legion, since they run the writer dispatch/evals builds (export_test.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/evals/charts"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// registerCalls counts the calls of the Register of a rig's admissions, by project.
type registerCalls struct {
	mu    sync.Mutex
	calls map[string]int
}

// of is how many times Register was called for project.
func (c *registerCalls) of(project string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[project]
}

// countRegisters has every intake the rig starts from here on, and the rig's whole reads
// (readWhole), run the rig's admissions with each call of their Register counted.
func (r *admissionRig) countRegisters() *registerCalls {
	counted := &registerCalls{calls: map[string]int{}}
	register := r.admissions.Register
	r.admissions.Register = func(ctx context.Context, project string) error {
		counted.mu.Lock()
		counted.calls[project]++
		counted.mu.Unlock()
		return register(ctx, project)
	}
	r.Admit(r.admissions)
	return counted
}

// registeredVersion is one version of a project's admission eval as eval_versions holds it.
type registeredVersion struct {
	Version                                                       int
	Name, Question, NumberRule, Decision, Unit, OwnerRole, Person string
	Kind, Source, DocBlock, WrittenVia                            string
	Direction, IssueKey                                           *string
	Target                                                        *float64
	FailureModes                                                  []evals.FailureMode
	Charts                                                        []byte
}

// versionsOf is every version of project's admission eval, oldest first.
func versionsOf(t *testing.T, database *store.Store, project string) []registeredVersion {
	t.Helper()
	rows, err := database.Pool.Query(t.Context(), `
		select version, name, question, number_rule, decision, unit, owner_role, person, kind, source, doc_block, written_via,
			direction, issue_key, target, failure_modes, charts
		from eval_versions where eval_id = $1 order by version
	`, project+"/admission")
	if err != nil {
		t.Fatalf("read the versions of the admission eval of %s: %v", project, err)
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (registeredVersion, error) {
		var v registeredVersion
		err := row.Scan(&v.Version, &v.Name, &v.Question, &v.NumberRule, &v.Decision, &v.Unit, &v.OwnerRole, &v.Person, &v.Kind, &v.Source,
			&v.DocBlock, &v.WrittenVia, &v.Direction, &v.IssueKey, &v.Target, &v.FailureModes, &v.Charts)
		return v, err
	})
	if err != nil {
		t.Fatalf("read the versions of the admission eval of %s: %v", project, err)
	}
	return versions
}

// registeredSeries is one version of a series of a project's admission eval as eval_series holds
// it.
type registeredSeries struct {
	Series, Unit, Role, Agg string
	Version                 int
	IsPrimary, Listed       bool
}

// seriesOf is every version of every series of project's admission eval, by series and version.
func seriesOf(t *testing.T, database *store.Store, project string) []registeredSeries {
	t.Helper()
	rows, err := database.Pool.Query(t.Context(), `
		select series, unit, role, agg, version, is_primary, listed from eval_series where eval_id = $1 order by series, version
	`, project+"/admission")
	if err != nil {
		t.Fatalf("read the series of the admission eval of %s: %v", project, err)
	}
	series, err := pgx.CollectRows(rows, pgx.RowToStructByPos[registeredSeries])
	if err != nil {
		t.Fatalf("read the series of the admission eval of %s: %v", project, err)
	}
	return series
}

// newestRead is whether project's newest whole read is a good one, and its error.
func newestRead(t *testing.T, database *store.Store, project string) (bool, *string) {
	t.Helper()
	var ok bool
	var failure *string
	if err := database.Pool.QueryRow(t.Context(), `
		select ok, error from legion_reads where project = $1 and kind = 'read' order by id desc limit 1
	`, project).Scan(&ok, &failure); err != nil {
		t.Fatalf("read the newest whole read of %s: %v", project, err)
	}
	return ok, failure
}

// refuseAdmissionReadings has Postgres refuse every insert of a reading with an exception, an
// error outside SQLSTATE classes 22 and 23, which a retry may write; takeAdmissionReadings undoes
// it.
func refuseAdmissionReadings(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(), `
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
}

func takeAdmissionReadings(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(), `drop trigger refuse_admission_readings on eval_readings`); err != nil {
		t.Fatalf("take the readings again: %v", err)
	}
}

// requireStepCharts fails unless declared, the charts of the admission eval's version 1, are the
// root's: one xy chart for each of the eight series, which the validator accepts, each a step line
// over measured time (charts.Steps) of one series, in its col; the five value series' and the
// holds' grouped by col; and the holds' and the switch's y axis from 0 to 1, and no other's bounded.
func requireStepCharts(t *testing.T, declared []byte) {
	t.Helper()
	if refusals, err := charts.Validate(declared); err != nil || len(refusals) != 0 {
		t.Fatalf("the validator refuses the declared charts %s: %v %v", declared, refusals, err)
	}
	steps, err := charts.Steps(declared)
	if err != nil {
		t.Fatalf("read the step lines of %s: %v", declared, err)
	}
	all := []string{"admission-cap", "admission-idle", "admission-queue", "admission-headroom", "admission-kills", "admission-approvals",
		"admission-holds", "admission-switch"}
	want := make([]charts.Step, len(all))
	for index, series := range all {
		want[index] = charts.Step{Series: series, Group: charts.GroupCol}
	}
	if !slices.Equal(steps, want) {
		t.Errorf("the declared charts draw the step lines %v, want one over measured time for each series, in its col: %v", steps, want)
	}
	var list []struct {
		Kind, Curve, Group string
		Y                  struct {
			Series   string
			Min, Max *float64
		}
	}
	if err := json.Unmarshal(declared, &list); err != nil {
		t.Fatalf("decode the declared charts %s: %v", declared, err)
	}
	if len(list) != len(all) {
		t.Fatalf("the eval declares %d charts, want %d, one for each series: %s", len(list), len(all), declared)
	}
	grouped := []string{"admission-idle", "admission-queue", "admission-headroom", "admission-kills", "admission-approvals", "admission-holds"}
	bounded := []string{"admission-holds", "admission-switch"}
	for index, chart := range list {
		series := chart.Y.Series
		if chart.Kind != "xy" || chart.Curve != "step" {
			t.Errorf("chart %d of %s is a %s chart of curve %q, want an xy step chart", index, series, chart.Kind, chart.Curve)
		}
		if slices.Contains(grouped, series) && chart.Group != charts.GroupCol {
			t.Errorf("the chart of %s is grouped by %q, want col", series, chart.Group)
		}
		zeroToOne := chart.Y.Min != nil && *chart.Y.Min == 0 && chart.Y.Max != nil && *chart.Y.Max == 1
		if unbounded := chart.Y.Min == nil && chart.Y.Max == nil; slices.Contains(bounded, series) != zeroToOne || !zeroToOne && !unbounded {
			t.Errorf("the chart of %s draws y from %v to %v, want 0 to 1 for the holds and the switch alone", series, chart.Y.Min, chart.Y.Max)
		}
	}
}

// requireSection fails unless version's doc_block names the method block of a section of INFRA's
// `evals-md` headed by the eval's slug, which holds the eval's question.
func (r *admissionRig) requireSection(version registeredVersion) {
	r.t.Helper()
	var id string
	if err := r.Database().Pool.QueryRow(r.t.Context(), `
		select id::text from artifacts where project_key = $1 and issue_key is null and slug = 'evals-md'
	`, infra).Scan(&id); err != nil {
		r.t.Fatalf("find INFRA's evals-md: %v", err)
	}
	text, blocks, err := r.documents.TextWithBlocks(r.t.Context(), id)
	if err != nil {
		r.t.Fatalf("read INFRA's evals-md: %v", err)
	}
	headings := 0
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) == "## admission" {
			headings++
		}
	}
	held := slices.ContainsFunc(blocks, func(block model.ArtifactBlock) bool {
		return block.ID == version.DocBlock || slices.Contains(block.DescendantIDs, version.DocBlock)
	})
	if headings != 1 || !held || !strings.Contains(text, version.Question) {
		r.t.Errorf("INFRA's evals-md holds %d sections of admission, its block %q %t, and the question %t, want one section whose method holds the question:\n%s",
			headings, version.DocBlock, held, strings.Contains(text, version.Question), text)
	}
}

// TestTheIntakeRegistersTheAdmissionEvalOnce (k27 (a)): a project whose starting state serves
// neither record holds no row of its admission eval. The first state the intake applies that
// carries `capChanges`, a batch, registers the eval through the register write, before the batch's
// transaction: the registry row is version 1 with the root's definition (kind legion, source
// logged, no target, no direction, no issue, the unit issues, owner role controller, the project's
// default assignee as its person, a section of the project's `evals-md` as its document block, and
// a name, question, number rule, decision and failure modes each read back non-empty), its eight
// series, admission-cap its primary, each folded by last, and a step chart over measured time for
// each (requireStepCharts). The batch writes a cap_change reading of each entry, with its
// attributes and its note. Two later states with the same entries, a batch and a whole read, leave
// version 1, and the register function is called once.
func TestTheIntakeRegistersTheAdmissionEvalOnce(t *testing.T) {
	rig, daemon := newAdmissionRig(t, stateAt(t, 10, map[string]any{}), false)
	calls := rig.countRegisters()
	const person = "owner@example.com"
	if _, err := rig.Database().Pool.Exec(t.Context(), `update projects set default_assignee = $2 where key = $1`, infra, person); err != nil {
		t.Fatalf("name INFRA's default assignee: %v", err)
	}
	rig.Start()
	rig.AwaitStartingRead(infra)
	if n, versions := calls.of(infra), versionsOf(t, rig.Database(), infra); n != 0 || len(versions) != 0 {
		t.Fatalf("a starting state that serves neither record called the register function %d times and left %d versions, want none", n, len(versions))
	}

	block := map[string]any{"cap": 5, "capChanges": goldenCaps(t)}
	rig.Publish(infra, legion.AdmissionEvent(11, block))
	rig.Settled()
	want := goldenCapReadings(t)
	rig.requireReadings("the first state that carries capChanges", want...)
	rig.Publish(infra, legion.AdmissionEvent(12, block))
	rig.Settled()
	daemon.Serves(stateAt(t, 12, block))
	rig.readWhole(daemon.URL, legion.ReadReasonRetry)
	rig.requireReadings("two later states with the same entries", want...)
	rig.requireLastSeq(12)
	if n := calls.of(infra); n != 1 {
		t.Errorf("the register function was called %d times for INFRA, want once", n)
	}

	versions := versionsOf(t, rig.Database(), infra)
	if len(versions) != 1 {
		t.Fatalf("the admission eval holds %d versions, want version 1 alone", len(versions))
	}
	v := versions[0]
	t.Logf("version 1: name %q, question %q, number rule %q, decision %q, failure modes %+v", v.Name, v.Question, v.NumberRule, v.Decision, v.FailureModes)
	if v.Version != 1 || v.Kind != evals.KindLegion || v.Source != evals.SourceLogged || v.Target != nil || v.Direction != nil || v.IssueKey != nil ||
		v.Unit != "issues" || v.OwnerRole != "controller" || v.Person != person || v.WrittenVia != evals.ViaRegister {
		t.Errorf("the registry row is %+v, want version 1 of kind legion, source logged, no target, no direction and no issue, in issues, "+
			"owned by the controller and %s, through the register write", v, person)
	}
	if v.Name == "" || v.Question == "" || v.NumberRule == "" || v.Decision == "" || len(v.FailureModes) == 0 {
		t.Errorf("the registry row's name %q, question %q, number rule %q, decision %q or failure modes %v is empty", v.Name, v.Question, v.NumberRule, v.Decision, v.FailureModes)
	}
	for index, mode := range v.FailureModes {
		if mode.Mode == "" || mode.Guard == "" {
			t.Errorf("failure mode %d is %+v, want a mode and its guard", index, mode)
		}
	}
	control := func(series, unit string) registeredSeries {
		return registeredSeries{Series: series, Unit: unit, Role: evals.RoleControl, Agg: evals.AggLast, Version: 1, Listed: true}
	}
	wantSeries := []registeredSeries{
		control("admission-approvals", "count"),
		{Series: "admission-cap", Unit: "issues", Role: evals.RoleHeadline, Agg: evals.AggLast, Version: 1, IsPrimary: true, Listed: true},
		control("admission-headroom", "share of limit"),
		control("admission-holds", ""),
		control("admission-idle", "share"),
		control("admission-kills", "count"),
		control("admission-queue", "hours"),
		control("admission-switch", ""),
	}
	if got := seriesOf(t, rig.Database(), infra); !slices.Equal(got, wantSeries) {
		t.Errorf("the admission eval's series are\n%+v\nwant\n%+v", got, wantSeries)
	}
	requireStepCharts(t, v.Charts)
	rig.requireSection(v)
}

// TestAStateWithNoCapChangesRegistersNothing (k27 (b)): beside INFRA, whose state that carries
// capChanges registers its eval, LINUX's daemon serves a cap and neither record, in its starting
// state and in an admission event: LINUX holds no row of its admission eval and no reading, the
// register function is never called for it, nothing of its admission eval is logged, and its
// state applies.
func TestAStateWithNoCapChangesRegistersNothing(t *testing.T) {
	rig, _ := newAdmissionRig(t, stateAt(t, 10, map[string]any{}), false)
	calls := rig.countRegisters()
	linux := legion.NewFakeDaemon(t, stateIn(t, "LINUX", 20, legion.RigStateID, legion.RigRun, map[string]any{"cap": 3}))
	legion.SeedLegionProject(t, rig.Database(), "LINUX", linux.URL)
	rig.Start()
	rig.AwaitWholeReads(2)
	for _, project := range []string{infra, "LINUX"} {
		if ok, failure := newestRead(t, rig.Database(), project); !ok {
			t.Fatalf("%s's starting read failed with %v", project, failure)
		}
	}
	rig.Publish(infra, legion.AdmissionEvent(11, map[string]any{"cap": 5, "capChanges": goldenCaps(t)}))
	rig.Publish("LINUX", legion.AdmissionEvent(21, map[string]any{"cap": 4, "active": []string{"LINUX-1"}}))
	rig.Settled()

	rig.requireReadings("INFRA's state that carries capChanges", goldenCapReadings(t)...)
	if n := calls.of(infra); n != 1 {
		t.Errorf("the register function was called %d times for INFRA, want once", n)
	}
	if n, versions, readings := calls.of("LINUX"), versionsOf(t, rig.Database(), "LINUX"), readingsOf(t, rig.Database(), "LINUX"); n != 0 || len(versions) != 0 || len(readings) != 0 {
		t.Errorf("LINUX's states called the register function %d times and left %d versions and the readings %v, want none", n, len(versions), readingTexts(readings))
	}
	for _, line := range []string{evals.AdmissionSkippedLine, evals.AdmissionUnknownLine, evals.AdmissionRefusedLine} {
		for _, record := range rig.LogRecords(line) {
			if record["project"] == "LINUX" {
				t.Errorf("the writer logged %v for LINUX, want no line", record)
			}
		}
	}
	if seq := rig.LastSeq("LINUX"); seq == nil || *seq != 21 {
		t.Errorf("LINUX's cursor stands at %v, want its admission event's 21", seq)
	}
	if admission := rig.Current("LINUX")["/admission"]; !strings.Contains(admission, "LINUX-1") {
		t.Errorf("LINUX's admission observation is %s, want its event's block", admission)
	}
}

// TestAWholeReadWhoseReadingsWriteFailsIsRecordedFailedAndReadAgain (k27 (c)): a good whole read
// whose state carries the golden's two cap entries, whose readings cannot be written for an error a
// retry may fix (a trigger's exception, outside SQLSTATE classes 22 and 23), is recorded as a failed
// read whose error starts `record:`, with none of its observations and no reading, and the read is
// owed again; the registration, made in a transaction of its own before the read's, stands. The
// next read, once the readings can be written, records as good and writes the two readings, and a
// read after it adds none.
func TestAWholeReadWhoseReadingsWriteFailsIsRecordedFailedAndReadAgain(t *testing.T) {
	rig, daemon := newAdmissionRig(t, stateAt(t, 10, map[string]any{"cap": 5, "capChanges": goldenCaps(t), "active": []string{"INFRA-1"}}), false)
	calls := rig.countRegisters()
	refuseAdmissionReadings(t, rig.Database())
	rig.readWhole(daemon.URL, legion.ReadReasonStart)
	if ok, failure := newestRead(t, rig.Database(), infra); ok || failure == nil || !strings.HasPrefix(*failure, "record:") {
		t.Fatalf("the read whose readings write failed is recorded good %t with the error %v, want a failed read whose error starts record:", ok, failure)
	}
	if current := rig.Current(infra); len(current) != 0 {
		t.Errorf("the failed read recorded the observations %v, want none", current)
	}
	rig.requireReadings("the failed read")
	if !rig.ReadOwed(infra) {
		t.Errorf("INFRA's cursor owes no read after the failed one, want it owed again")
	}
	if n, versions := calls.of(infra), versionsOf(t, rig.Database(), infra); n != 1 || len(versions) != 1 {
		t.Errorf("the register function was called %d times and the eval holds %d versions, want the registration made once and standing", n, len(versions))
	}

	takeAdmissionReadings(t, rig.Database())
	rig.readWhole(daemon.URL, legion.ReadReasonRetry)
	if ok, failure := newestRead(t, rig.Database(), infra); !ok {
		t.Fatalf("the next read is recorded failed with %v, want it good", failure)
	}
	want := goldenCapReadings(t)
	rig.requireReadings("the next read", want...)
	if rig.ReadOwed(infra) {
		t.Errorf("INFRA's cursor still owes a read after the good one")
	}
	if admission := rig.Current(infra)["/admission"]; !strings.Contains(admission, "INFRA-1") {
		t.Errorf("INFRA's admission observation is %s, want the good read's block", admission)
	}
	rig.readWhole(daemon.URL, legion.ReadReasonRetry)
	rig.requireReadings("a read after it", want...)
	if n := calls.of(infra); n != 1 {
		t.Errorf("the register function was called %d times, want once", n)
	}
}

// TestAnEntryTheDatabaseRefusesIsLoggedAndTheOthersWritten (k27 (d)): in the state that registers
// the eval, an entry whose reason holds a NUL, which Postgres stores in no text and no jsonb, so
// that it refuses the reading for good (SQLSTATE class 22), has no reading and is logged at WARN
// with its code and row key; the other entries of the state have their readings, and the project's
// observations go on: the next message applies.
func TestAnEntryTheDatabaseRefusesIsLoggedAndTheOthersWritten(t *testing.T) {
	rig, _ := newAdmissionRig(t, stateAt(t, 10, map[string]any{}), false)
	rig.Start()
	rig.AwaitStartingRead(infra)
	refused := capChange(3, 8, 5, time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond), "a reason that holds a NUL \x00 here")
	rig.Publish(infra, legion.AdmissionEvent(11, map[string]any{"cap": 5, "capChanges": append([]map[string]any{refused}, goldenCaps(t)...)}))
	rig.Publish(infra, legion.PhaseEvent(12, "INFRA-1", "implementing"))
	rig.Settled()
	rig.requireReadings("a state with an entry the database refuses", goldenCapReadings(t)...)
	skipped := rig.skippedLines()
	if len(skipped) != 1 || !strings.HasPrefix(skipped[0], "cap_change "+key(3)+" 22") {
		t.Errorf("the writer logged the skipped readings %v, want the refused entry %s alone, with its SQLSTATE of class 22", skipped, key(3))
	}
	t.Logf("the refused entry: %v", skipped)
	rig.requireLastSeq(12)
	if phase := rig.Current(infra)["INFRA-1/phase"]; phase != `"implementing"` {
		t.Errorf("INFRA-1's phase is %s, want the next message's implementing", phase)
	}
}

// TestAStateWithANewSeqAndTheSameCapAddsNoReading (k27 (e)): after the state that registered the
// eval and wrote the golden's two cap_change readings, whose newest cap is 5, a state at a new seq
// whose cap is 5 adds no reading: an admission event with neither record, one with the same
// entries, and a whole read of a later state.
func TestAStateWithANewSeqAndTheSameCapAddsNoReading(t *testing.T) {
	rig, daemon := newAdmissionRig(t, stateAt(t, 10, map[string]any{}), false)
	rig.Start()
	rig.AwaitStartingRead(infra)
	block := map[string]any{"cap": 5, "capChanges": goldenCaps(t)}
	rig.Publish(infra, legion.AdmissionEvent(11, block))
	rig.Settled()
	want := goldenCapReadings(t)
	rig.requireReadings("the state that registered the eval", want...)

	rig.Publish(infra, legion.AdmissionEvent(12, map[string]any{"cap": 5, "active": []string{"INFRA-1"}}))
	rig.Settled()
	rig.requireReadings("an event at seq 12 with the same cap and neither record", want...)
	rig.Publish(infra, legion.AdmissionEvent(13, block))
	rig.Settled()
	rig.requireReadings("an event at seq 13 with the same cap and entries", want...)
	daemon.Serves(stateAt(t, 14, block))
	rig.readWhole(daemon.URL, legion.ReadReasonRetry)
	rig.requireReadings("a whole read at seq 14 with the same cap and entries", want...)
	rig.requireLastSeq(14)
}

// TestAMessageThatDoesNotApplyRegistersNothing: messages that carry capChanges and do not apply
// register nothing: one of another store, which calls for a read of the daemon that finds it in the
// cursor's store and marks the other store as no daemon of INFRA; that store's next message, which
// the intake forgets; and a part of a transaction with more of it to come, which is held. The
// transaction's last part then applies the held part's admission event, and registers the eval,
// once.
func TestAMessageThatDoesNotApplyRegistersNothing(t *testing.T) {
	rig, _ := newAdmissionRig(t, stateAt(t, 10, map[string]any{}), false)
	calls := rig.countRegisters()
	rig.Start()
	rig.AwaitStartingRead(infra)
	block := map[string]any{"cap": 5, "capChanges": goldenCaps(t)}
	requireNone := func(why string) {
		t.Helper()
		if n, versions := calls.of(infra), versionsOf(t, rig.Database(), infra); n != 0 || len(versions) != 0 {
			t.Fatalf("%s: the register function was called %d times and the eval holds %d versions, want none", why, n, len(versions))
		}
		rig.requireReadings(why)
		rig.requireLastSeq(10)
	}

	rig.PublishIn(infra, otherRun, legion.OtherStateID, time.Now().UTC().Truncate(time.Microsecond), legion.AdmissionEvent(4, block))
	rig.Settled()
	rig.AwaitWholeReads(2)
	requireNone("a message of another store")
	rig.PublishIn(infra, otherRun, legion.OtherStateID, time.Now().UTC().Truncate(time.Microsecond), legion.AdmissionEvent(5, block))
	rig.Settled()
	requireNone("a message of the store marked as no daemon of INFRA")
	rig.PublishPart(infra, legion.AdmissionEvent(11, block))
	rig.Settled()
	requireNone("a part with more of its transaction to come")

	rig.Publish(infra, legion.PhaseEvent(12, "INFRA-1", "implementing"))
	rig.Settled()
	rig.requireReadings("the transaction's last part, after its held part's admission event", goldenCapReadings(t)...)
	rig.requireLastSeq(12)
	if n := calls.of(infra); n != 1 {
		t.Errorf("the register function was called %d times, want once, for the transaction that applied", n)
	}
}

// TestAFailedWholeReadRegistersNothing: a whole read that fails registers nothing, whatever its
// state carries: a state of the project with both records and no dispatch_url
// (legion.ErrNoDispatchURL), another project's state with both records, and a daemon that refuses
// the read. A good read of the state then registers the eval, once, and writes its readings.
func TestAFailedWholeReadRegistersNothing(t *testing.T) {
	block := map[string]any{"cap": 5, "capChanges": goldenCaps(t), "signalChanges": goldenSignals(t)}
	good := stateAt(t, 10, block)
	noURL := strings.Replace(good, `"dispatch_url":"http://dispatch.internal.example",`, "", 1)
	if noURL == good {
		t.Fatal("the failed read's state still carries a dispatch_url")
	}
	rig, daemon := newAdmissionRig(t, noURL, false)
	calls := rig.countRegisters()
	for _, read := range []struct {
		name  string
		serve func()
	}{
		{"a state with no dispatch_url", func() { daemon.Serves(noURL) }},
		{"another project's state", func() { daemon.Serves(stateIn(t, "LINUX", 10, legion.RigStateID, legion.RigRun, block)) }},
		{"a daemon that refuses the read", daemon.RefusesUnavailable},
	} {
		read.serve()
		rig.readWhole(daemon.URL, legion.ReadReasonStart)
		if ok, _ := newestRead(t, rig.Database(), infra); ok {
			t.Fatalf("%s was recorded as a good read", read.name)
		}
		if n, versions := calls.of(infra), versionsOf(t, rig.Database(), infra); n != 0 || len(versions) != 0 {
			t.Fatalf("%s called the register function %d times and left %d versions, want none", read.name, n, len(versions))
		}
		rig.requireReadings(read.name)
	}

	daemon.Serves(good)
	rig.readWhole(daemon.URL, legion.ReadReasonRetry)
	rig.requireReadings("a good read of the state", append(goldenCapReadings(t), goldenSignalReadings(t)...)...)
	if n := calls.of(infra); n != 1 {
		t.Errorf("the register function was called %d times, want once, for the good read", n)
	}
}

// TestAFirstStateWithSignalChangesAloneRegistersTheEval (k29): the first state that carries
// `signalChanges` and no `capChanges` registers the eval, once, and each entry has its reading,
// beside the cap the state reports, which no reading of admission-cap holds (the plan's departure
// 12). The same state again registers nothing more and adds no reading.
func TestAFirstStateWithSignalChangesAloneRegistersTheEval(t *testing.T) {
	rig, _ := newAdmissionRig(t, stateAt(t, 10, map[string]any{}), false)
	calls := rig.countRegisters()
	rig.Start()
	rig.AwaitStartingRead(infra)
	block := map[string]any{"cap": 5, "signalChanges": goldenSignals(t)}
	committed := rig.Publish(infra, legion.AdmissionEvent(11, block))
	rig.Settled()
	want := append(goldenSignalReadings(t), observedReading(11, 5, committed))
	rig.requireReadings("the first state with signalChanges alone", want...)
	rig.Publish(infra, legion.AdmissionEvent(12, block))
	rig.Settled()
	rig.requireReadings("the same state again", want...)
	rig.requireLastSeq(12)
	if n, versions := calls.of(infra), versionsOf(t, rig.Database(), infra); n != 1 || len(versions) != 1 {
		t.Errorf("the register function was called %d times and the eval holds %d versions, want version 1 registered once", n, len(versions))
	}
}

// TestAWholeReadThroughReadWholeWritesTheAdmissionReadings: legion.ReadWhole, the whole read its
// callers outside the intake make (the api package's tests of the Legion board among them), run
// with the writer dispatch/evals builds, registers the eval at a good read whose state serves both
// records, once, and writes a reading of each entry; a second read adds nothing.
func TestAWholeReadThroughReadWholeWritesTheAdmissionReadings(t *testing.T) {
	database := storetest.Open(t)
	registry, _ := newRegistry(t, database)
	admissions := evals.AdmissionWriter(registry, testBuild)
	daemon := legion.NewFakeDaemon(t, stateAt(t, 10, map[string]any{"cap": 5, "capChanges": goldenCaps(t), "signalChanges": goldenSignals(t)}))
	legion.SeedLegionProject(t, database, infra, daemon.URL)
	want := readingTexts(append(goldenCapReadings(t), goldenSignalReadings(t)...))
	for _, reason := range []string{legion.ReadReasonStart, legion.ReadReasonRetry} {
		if err := legion.ReadWhole(t.Context(), database.Pool, admissions, legion.Settings{Project: infra, StateURL: daemon.URL}, reason); err != nil {
			t.Fatalf("read INFRA's daemon whole for %s: %v", reason, err)
		}
		if got := readingTexts(readingsOf(t, database, infra)); !slices.Equal(got, want) {
			t.Fatalf("after the read for %s the admission readings are\n%s\nwant\n%s", reason, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if versions := versionsOf(t, database, infra); len(versions) != 1 {
			t.Fatalf("after the read for %s the eval holds %d versions, want version 1", reason, len(versions))
		}
	}
}

// TestARepeatedStateLogsItsSkippedEntriesOnce: each state the intake applies carries the daemon's
// whole record, so an entry the writer skips comes again with every state: an entry whose reading
// R29's rules refuse (a setAt before evals.MeasuredAtFloor) and a signal entry of a what it does not
// know. The writer logs each once in its process: a second state with the same record logs
// neither again, and a third that adds one more of each logs those two alone, once each.
func TestARepeatedStateLogsItsSkippedEntriesOnce(t *testing.T) {
	rig, _ := newAdmissionRig(t, stateAt(t, 10, map[string]any{}), false)
	rig.Start()
	rig.AwaitStartingRead(infra)
	early := evals.MeasuredAtFloor.Add(-24 * time.Hour)
	unknown := func(id int) map[string]any {
		return map[string]any{"id": id, "at": "2026-11-26T07:30:00Z", "signal": "idle", "part": nil, "what": "other", "value": 1,
			"absent": nil, "setBy": nil, "reason": nil}
	}
	before := []map[string]any{capChange(3, 8, 5, early, "an entry before the floor")}
	record := map[string]any{"cap": 5, "capChanges": append(slices.Clone(before), goldenCaps(t)...), "signalChanges": []map[string]any{unknown(8)}}
	rig.Publish(infra, legion.AdmissionEvent(11, record))
	rig.Publish(infra, legion.AdmissionEvent(12, record))
	rig.Settled()
	unknownLines := func() []string {
		var lines []string
		for _, line := range rig.LogRecords(evals.AdmissionUnknownLine) {
			if line["level"] == "WARN" && line["project"] == infra {
				lines = append(lines, fmt.Sprint(line["row_key"]))
			}
		}
		slices.Sort(lines)
		return lines
	}
	refusedLine := func(id int) string { return "cap_change " + key(id) + " " + evals.CodeMeasuredAtOutOfRange }
	if skipped, want := rig.skippedLines(), []string{refusedLine(3)}; !slices.Equal(skipped, want) {
		t.Errorf("after two states of one record the writer logged the skipped readings %v, want %v", skipped, want)
	}
	if logged, want := unknownLines(), []string{key(8)}; !slices.Equal(logged, want) {
		t.Errorf("after two states of one record the writer logged the unknown entries %v, want %v", logged, want)
	}

	record = map[string]any{"cap": 5, "capChanges": append([]map[string]any{capChange(4, 5, 5, early, "another entry before the floor")}, record["capChanges"].([]map[string]any)...),
		"signalChanges": []map[string]any{unknown(9), unknown(8)}}
	rig.Publish(infra, legion.AdmissionEvent(13, record))
	rig.Settled()
	if skipped, want := rig.skippedLines(), []string{refusedLine(3), refusedLine(4)}; !slices.Equal(skipped, want) {
		t.Errorf("after a state that adds a refused entry the writer logged the skipped readings %v, want %v", skipped, want)
	}
	if logged, want := unknownLines(), []string{key(8), key(9)}; !slices.Equal(logged, want) {
		t.Errorf("after a state that adds an unknown entry the writer logged the unknown entries %v, want %v", logged, want)
	}
	rig.requireReadings("three states whose records each skip entries", goldenCapReadings(t)...)
	rig.requireLastSeq(13)
}
