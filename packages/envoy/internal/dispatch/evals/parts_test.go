package evals

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// TestADayRowsPartIsStoredAndReadBackWithNothingLost: a day row is its part (measures.DayPart)
// beside its key and bookkeeping, and a row written with every field of its part set reads back
// holding the same part, both as a stored row (scanDay) and as the window read the folds read
// (DayWindowIn), with its key and bookkeeping kept, so a field added to the part and not to the
// columns the writes and the reads name fails here. The row is an owner key's (owners.go): the
// plain key of acme/web, which the project publishes as its default repository and so reads
// through the repository, the one read that answers a row's via_repository as stored (a project's
// own rows count none through a repository, viaAsRead); the window read answers the row under the
// project's eval id of its slug. The window read is of a `last` series over two repositories, the
// read that sends every field of a part: it sends the write that stored the last value
// (last_recorded_at, last_reading) only where a `last` fold can tie two rows.
func TestADayRowsPartIsStoredAndReadBackWithNothingLost(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	day := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	lastAt, recordedAt := day.Add(17*time.Hour), day.Add(18*time.Hour)
	part := measures.DayPart{
		Series: "minutes", Day: day, N: 3, Sum: new(12.5), Num: new(2.0), Den: new(3.0), Max: new(7.5),
		Samples: []float64{2, 3, 7.5}, Counts: map[string]float64{"success": 2, "failure": 1},
		LastValue: new(7.5), LastAt: &lastAt, LastRecordedAt: &recordedAt, LastReading: "0190c9a4-5c1e-7a00-8000-000000000001",
		ViaRepository: 1,
	}
	value := reflect.ValueOf(part)
	for index := range value.NumField() {
		if value.Field(index).IsZero() {
			t.Fatalf("the part's %s is its zero value, so the test cannot see it lost", value.Type().Field(index).Name)
		}
	}
	const repo = "acme/web"
	if _, err := r.database.Pool.Exec(t.Context(), `insert into eval_days_defaults (project_key, repo) values ($1, $2)`, r.project, repo); err != nil {
		t.Fatalf("publish %s as %s's default repository: %v", repo, r.project, err)
	}
	owner := OwnerKey(repo, nil, nil)
	row := DayRow{ProjectKey: owner, EvalID: EvalID(owner, SlugPRCI), Repo: repo, Listed: true, DayPart: part}
	tx, err := r.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the write: %v", err)
	}
	defer tx.Rollback(t.Context())
	if err := upsertDays(t.Context(), tx, []DayRow{row}, testBuild); err != nil {
		t.Fatalf("write the row: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit the row: %v", err)
	}

	stored := r.day(t, row.EvalID, part.Series, row.Repo, day)
	if !reflect.DeepEqual(stored.DayPart, part) {
		t.Errorf("the stored row's part reads back as\n%+v\nwant\n%+v", stored.DayPart, part)
	}
	if stored.ProjectKey != row.ProjectKey || stored.EvalID != row.EvalID || stored.Repo != row.Repo || !stored.Listed || stored.BuiltBy != testBuild {
		t.Errorf("the stored row's key and bookkeeping are %+v, want those of %+v built by %s", stored, row, testBuild)
	}
	read := EvalID(r.project, SlugPRCI)
	entry := Entry{Definition: Definition{EvalID: read, ProjectKey: r.project, Source: SourceFoldTiming},
		Series: []Series{{EvalID: read, Series: part.Series, SeriesComputation: SeriesComputation{Agg: AggLast}}}}
	window, err := DayWindowIn(t.Context(), r.database.Pool, []string{r.project}, []Entry{entry}, "", []string{repo, "acme/api"}, Range{From: day, To: day.AddDate(0, 0, 1)}, nil)
	if err != nil {
		t.Fatalf("read the row's window: %v", err)
	}
	if len(window) != 1 || window[0].EvalID != read || !reflect.DeepEqual(window[0].Part, part) {
		t.Errorf("the window read answers %+v, want the one row's part under %s\n%+v", window, read, part)
	}
	t.Logf("%d fields of the part kept through the write and both reads", value.NumField())
}
