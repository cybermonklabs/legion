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
// never fail, so this package imports nothing it is compared with, legion/metrics among them: it
// reads `events`, `handoffs` and `legion_observations` itself, with statements of its own, and
// works the numbers out from the requirement's words. Where this reading and the computation
// disagree, one of them is wrong and the check has found it.
//
// It takes the scope and the window as arguments and answers both beside the facts it reads
// (Reading), so a caller can print what it read: the check's test logs them beside the scope and
// window of the computation it compares with, and cmd prints the reading's report as JSON
// (Report), with every count beside the scope and the window, which is what a tester re-runs.
package independent

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Querier is the database connection this reading needs: one that answers a query. A pool, a
// connection or a transaction is one.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Scope is what one reading covers: the project whose issues it reads. A reading names the scope
// it read (Reading.Scope), so a caller can print it.
type Scope struct {
	Project string
}

// String is the scope in one word, the project, as a caller prints it.
func (s Scope) String() string { return s.Project }

// Window is the stretch of time one reading measures over: To is the moment of the computation,
// which bounds what the reading takes from the log (the issues created by To, and their events,
// titles, handoffs and observations as of To) and is where an open issue's work time runs to.
// An issue's parent and its close come from its current `issues` row, as legion/metrics reads
// them, and not as of To: the close counts where the row's closed_at is by To, so an issue closed
// by To and opened again after it reads open at To, and a parent set after To is the parent the
// reading walks.
//
// R1 covers every issue the project has Legion phase work for from the beginning of the project's
// history up to To, so From must be the zero moment; Read refuses a non-zero From rather than
// ignoring it and reporting a window it did not read.
type Window struct {
	From time.Time
	To   time.Time
}

// String is the window in one line, as a caller prints it: two moments, each in UTC, and
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
// of the window's end, read in one repeatable-read snapshot when db can begin a transaction.
func Read(ctx context.Context, db Querier, scope Scope, window Window) (Reading, error) {
	if !window.From.IsZero() {
		return Reading{}, fmt.Errorf("independent.Read covers the whole history up to To; non-zero Window.From %s is unsupported", window.From.UTC().Format(time.RFC3339))
	}
	if window.To.IsZero() {
		return Reading{}, fmt.Errorf("independent.Read requires a non-zero Window.To")
	}
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
