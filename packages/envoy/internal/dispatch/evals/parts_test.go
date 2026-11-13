package evals

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// TestADayRowAndItsPartConvertWithNothingLost: a day row read back as a part and written again is
// the same row, and a part written into a row and read back is the same part, with every field of
// measures.DayPart set, so a field a conversion leaves out (or one added to the part and not to
// the conversion) fails here.
func TestADayRowAndItsPartConvertWithNothingLost(t *testing.T) {
	day := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	lastAt := day.Add(17 * time.Hour)
	part := measures.DayPart{
		Series: "minutes", Day: day, N: 3, Sum: new(12.5), Num: new(2.0), Den: new(3.0), Max: new(7.5),
		Samples: []float64{2, 3, 7.5}, Counts: map[string]float64{"success": 2, "failure": 1},
		LastValue: new(7.5), LastAt: &lastAt, ViaRepository: 1,
	}
	value := reflect.ValueOf(part)
	for index := range value.NumField() {
		if value.Field(index).IsZero() {
			t.Fatalf("the part's %s is its zero value, so the test cannot see it lost", value.Type().Field(index).Name)
		}
	}
	key := DayRow{
		Range: RangeWindow, ProjectKey: "EVAL", EvalID: "EVAL/pr-ci", Repo: "acme/web", Listed: true,
		BuiltBy: testBuild, UpdatedAt: day.Add(20 * time.Hour),
	}

	row := key.WithPart(part)
	if got := row.Part(); !reflect.DeepEqual(got, part) {
		t.Errorf("the part written into a row reads back as\n%+v\nwant\n%+v", got, part)
	}
	if got := row.WithPart(row.Part()); !reflect.DeepEqual(got, row) {
		t.Errorf("the row read as a part and written again is\n%+v\nwant\n%+v", got, row)
	}
	if row.Range != key.Range || row.ProjectKey != key.ProjectKey || row.EvalID != key.EvalID || row.Repo != key.Repo ||
		row.Listed != key.Listed || row.BuiltBy != key.BuiltBy || !row.UpdatedAt.Equal(key.UpdatedAt) {
		t.Errorf("writing a part replaced the row's key or bookkeeping: %+v, want those of %+v", row, key)
	}
	t.Logf("%d fields of the part kept through both conversions", value.NumField())
}
