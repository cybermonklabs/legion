package benchtime

import (
	"strings"
	"testing"
	"time"
)

// samplePlan is an EXPLAIN (ANALYZE, BUFFERS) answer as Postgres writes one, with the two time
// lines this package reads indented as they are under a plan node's summary.
const samplePlan = `Index Scan using asks_issue_key on asks a  (cost=0.43..8.45 rows=1 width=16) (actual time=0.021..0.023 rows=2 loops=1)
  Index Cond: (issue_key = 'SEED1-1'::text)
  Buffers: shared hit=4
Planning Time: 0.184 ms
Execution Time: 1.250 ms`

func TestPlanTimeIsPlanningPlusExecution(t *testing.T) {
	took, err := planTime(samplePlan)
	if err != nil {
		t.Fatalf("read the plan's times: %v", err)
	}
	if want := 1434 * time.Microsecond; took != want {
		t.Fatalf("planning plus execution = %v, want %v", took, want)
	}
}

// A series run's time is the sum of its statements' planning and execution, and its plan text
// is each statement's plan in the order the run sent them, each under a line naming its place
// and its own time; a run of one statement is that statement's plan alone, as Pair has always
// logged it.
func TestASeriesRunIsTheSumOfItsStatements(t *testing.T) {
	second := "Result  (cost=0.00..0.01 rows=1 width=4) (actual time=0.001..0.001 rows=1 loops=1)\nPlanning Time: 0.016 ms\nExecution Time: 0.050 ms"
	taken, err := runReading([]string{samplePlan, second})
	if err != nil {
		t.Fatalf("read a run of two statements: %v", err)
	}
	if want := 1500 * time.Microsecond; taken.took != want {
		t.Errorf("the run took %v, want the sum of its statements' times, %v", taken.took, want)
	}
	if want := "statement 1 of 2, planning plus execution 1.434ms:\n" + samplePlan + "\nstatement 2 of 2, planning plus execution 66µs:\n" + second; taken.plan != want {
		t.Errorf("the run's plan text is\n%s\nwant\n%s", taken.plan, want)
	}
	one, err := runReading([]string{samplePlan})
	if err != nil {
		t.Fatalf("read a run of one statement: %v", err)
	}
	if one.took != 1434*time.Microsecond || one.plan != samplePlan {
		t.Errorf("a run of one statement read %v/%q, want 1.434ms and its plan alone", one.took, one.plan)
	}
	if _, err := runReading([]string{samplePlan, "Result\nPlanning Time: 0.016 ms"}); err == nil || !strings.Contains(err.Error(), "statement 2 of 2") {
		t.Errorf("a run whose second plan carries no execution time read %v, want a failure naming statement 2 of 2", err)
	}
}

// PairWithin's bound is the before median times 1.10 plus 0.505 ms. Its absolute term is the
// spread of the block loader at 256 ids against itself, 3.871 ms against 3.366 ms, so a before
// median of 3.366 ms bounds the after median at 4.2076 ms.
func TestPairWithinBoundsTheAfterMedianByATenthAndTheSpread(t *testing.T) {
	if got, want := withinBound(3366*time.Microsecond), 4207600*time.Nanosecond; got != want {
		t.Fatalf("withinBound(3.366ms) = %v, want %v", got, want)
	}
}

// A plan with no time at all is what EXPLAIN without ANALYZE answers, and one with only a
// planning time is what a cancelled run leaves. Either must fail the measurement: a missing time
// read as zero would be the fastest run of the eleven and would decide the median.
func TestPlanTimeRefusesAPlanMissingATime(t *testing.T) {
	for _, plan := range []struct{ name, text string }{
		{"no times", "Seq Scan on issues i  (cost=0.00..18.10 rows=810 width=36)"},
		{"planning alone", "Seq Scan on issues i\nPlanning Time: 0.100 ms"},
		{"execution alone", "Seq Scan on issues i\nExecution Time: 0.100 ms"},
	} {
		t.Run(plan.name, func(t *testing.T) {
			if took, err := planTime(plan.text); err == nil {
				t.Fatalf("read %v from a plan carrying %s", took, plan.name)
			}
		})
	}
}

// The median is a whole reading, so the plan a run prints beside a number is the run that gave
// that number rather than whichever run finished last.
func TestMedianIsTheMiddleReadingWithItsPlan(t *testing.T) {
	readings := []reading{
		{took: 5 * time.Millisecond, plan: "fifth"},
		{took: 1 * time.Millisecond, plan: "first"},
		{took: 3 * time.Millisecond, plan: "third"},
		{took: 2 * time.Millisecond, plan: "second"},
		{took: 4 * time.Millisecond, plan: "fourth"},
	}
	middle := median(readings)
	if middle.took != 3*time.Millisecond || middle.plan != "third" {
		t.Fatalf("the median reading is %v/%q, want 3ms/\"third\"", middle.took, middle.plan)
	}
	if readings[0].plan != "fifth" {
		t.Fatalf("median reordered its caller's readings: %q leads them", readings[0].plan)
	}
}

// The runs the median is taken over are eleven: runs 2 to 12, the first left out.
func TestElevenRunsDecideTheMedian(t *testing.T) {
	if counted := runs - discarded; counted != 11 {
		t.Fatalf("%d runs decide the median, want 11", counted)
	}
}

// Every statement-timing test is gated on this one variable, and no other variable turns a
// measurement on: a CI job that sets none runs none.
func TestTheGateIsTheBenchDatabaseURL(t *testing.T) {
	if !strings.HasPrefix(DatabaseURLEnv, "DISPATCH_BENCH_") {
		t.Fatalf("the gate is %q, which is not the bench database variable CI leaves unset", DatabaseURLEnv)
	}
}

// The generic plan's EXECUTE takes each argument as a literal of the type the statement is
// prepared with, so the generic plan is read for the values the plan for its values binds: a quote
// in a string is doubled, an array keeps its order and may be empty, a time keeps its
// microseconds, in UTC, and a time that is nil is null of the type a time takes. A value of any
// other type is refused rather than written as a guess.
func TestTheGenericPlanBindsTheArgumentsAsLiterals(t *testing.T) {
	at := time.Date(2026, 12, 29, 1, 2, 3, 456789000, time.FixedZone("", 3600))
	for _, want := range []struct {
		argument         any
		parameter, value string
	}{
		{"it's", "text", `'it''s'`},
		{[]string{"SEED1-1", "o'clock"}, "text[]", `array['SEED1-1', 'o''clock']::text[]`},
		{[]string{}, "text[]", `array[]::text[]`},
		{at, "timestamptz", `'2026-12-29T00:02:03.456789Z'::timestamptz`},
		{&at, "timestamptz", `'2026-12-29T00:02:03.456789Z'::timestamptz`},
		{(*time.Time)(nil), "timestamptz", `null::timestamptz`},
		{true, "boolean", `true`},
		{false, "boolean", `false`},
		{[]int16{0, 3}, "smallint[]", `array[0, 3]::smallint[]`},
		{[]int16{}, "smallint[]", `array[]::smallint[]`},
	} {
		parameter, value, err := sqlLiteral(want.argument)
		if err != nil || parameter != want.parameter || value != want.value {
			t.Errorf("sqlLiteral(%#v) = %q, %q, %v, want %q, %q", want.argument, parameter, value, err, want.parameter, want.value)
		}
	}
	if _, _, err := sqlLiteral(42); err == nil {
		t.Error("sqlLiteral(42) answers a literal, want a refusal of an int")
	}
}
