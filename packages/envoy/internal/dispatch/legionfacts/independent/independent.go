// Package independent is a second reading of the per-issue Legion facts, written from the words
// of dispatch://INFRA-2915's R1 and not from the code that serves them:
//
//	One exported computation gives every issue's Legion facts: hours by phase, work time, review
//	rounds, root, and tree root. It covers every issue for which the project has Legion phase
//	work, open or closed, and keeps an issue whose hours are all zero. For an open issue, work
//	time runs to the moment of the computation.
//
// It is the second side of that requirement's check (k1 and k9), which compares every fact of
// every issue against a reading of the requirement. A check whose two sides are one function can
// never fail, so this package imports neither legion/metrics nor legionfacts: it reads `events`,
// `handoffs` and `legion_observations` itself, with statements of its own, and works the numbers
// out from the requirement's words. Where this reading and the computation disagree, one of them
// is wrong and the check has found it.
//
// It takes the scope and the window as arguments and answers both beside every count it reports
// (Report), because a check whose two sides read different scopes proves nothing. cmd prints
// that report as JSON, which is what a tester re-runs.
package independent

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Querier is the database connection this reading needs: one that answers a query. A pool, a
// connection or a transaction is one.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Scope is what one reading covers: the project whose issues it reads. Every count it reports
// names it, so two sides of a comparison cannot read two scopes and read as one.
type Scope struct {
	Project string
}

// String is the scope in one word, which is what a report prints and a check compares.
func (s Scope) String() string { return s.Project }

// Window is the stretch of time one reading measures over: To is the moment of the computation,
// which is where an open issue's work time runs to, and From is where the reading starts.
//
// R1 covers every issue the project has Legion phase work for, open or closed, so From narrows
// no issue out of the answer: it is the start of the history the reading takes in, and a reading
// of the whole project is From the zero moment. It is reported because the two sides of a
// comparison must be read to have measured the same stretch.
type Window struct {
	From time.Time
	To   time.Time
}

// String is the window as a reader of a report compares it: two moments, each in UTC, and
// `beginning` for a From that names no moment.
func (w Window) String() string {
	from := "beginning"
	if !w.From.IsZero() {
		from = w.From.UTC().Format(time.RFC3339)
	}
	return from + " to " + w.To.UTC().Format(time.RFC3339)
}

// Facts is one issue's five facts, as R1 names them.
type Facts struct {
	// PhaseHours is the hours by phase, by the board's column id, with a column the issue spent
	// no time in left out. An issue whose work took no measured time holds none at all, and
	// keeps its entry: a result means Legion worked the issue.
	PhaseHours map[string]float64 `json:"phase_hours"`
	// WorkHours is the work time those hours are shared out of, in hours.
	WorkHours float64 `json:"work_hours"`
	// ReviewRounds is how many times the issue entered reviewing.
	ReviewRounds int `json:"review_rounds"`
	// Root is whether the issue is the root of its own tree, and Tree is the tree it is in.
	Root bool   `json:"root"`
	Tree string `json:"tree"`
}

// ZeroHours is whether the issue's hours are all zero: no work time and no phase holding any of
// it. R1 keeps such an issue, so a reading that holds none over a history that has one has
// dropped it.
func (f Facts) ZeroHours() bool {
	if f.WorkHours != 0 {
		return false
	}
	for _, hours := range f.PhaseHours {
		if hours != 0 {
			return false
		}
	}
	return true
}

// Reading is one run of this reading: the scope and the window it covered, and the facts of
// every issue the scope has Legion phase work for.
type Reading struct {
	Scope  Scope
	Window Window
	Issues map[string]Facts
}

// Read answers the facts of every issue the scope has Legion phase work for, open or closed, as
// of the window's end.
func Read(ctx context.Context, db Querier, scope Scope, window Window) (Reading, error) {
	held, err := readPopulation(ctx, db, scope, window)
	if err != nil {
		return Reading{}, err
	}
	measured := newReading(held)
	issues := map[string]Facts{}
	for _, key := range held.keys {
		if !held.issues[key].worked() {
			continue
		}
		issues[key] = measured.factsOf(key)
	}
	return Reading{Scope: scope, Window: window, Issues: issues}, nil
}

// Report is the reading as a reader compares it: the scope, the window, the counts, and one
// entry per issue with each fact. The scope and the window stand beside the counts, so a report
// read on its own says what it covered.
type Report struct {
	Scope      string           `json:"scope"`
	WindowFrom string           `json:"window_from"`
	WindowTo   string           `json:"window_to"`
	Issues     int              `json:"issues"`
	ZeroHours  int              `json:"zero_hour_issues"`
	Facts      map[string]Facts `json:"facts"`
}

// Report is the report of this reading.
func (r Reading) Report() Report {
	from := ""
	if !r.Window.From.IsZero() {
		from = r.Window.From.UTC().Format(time.RFC3339)
	}
	report := Report{
		Scope:      r.Scope.String(),
		WindowFrom: from,
		WindowTo:   r.Window.To.UTC().Format(time.RFC3339),
		Issues:     len(r.Issues),
		Facts:      r.Issues,
	}
	for _, facts := range r.Issues {
		if facts.ZeroHours() {
			report.ZeroHours++
		}
	}
	return report
}
