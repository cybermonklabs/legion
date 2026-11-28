// Command independent prints the independent reading of the per-issue Legion facts for one
// project, as JSON: the project, the moment of the reading, how many issues it read facts for,
// how many of them it read by a plan whose steps are not the default steps, and one entry for
// every issue with Legion phase work, with its facts.
//
// It is the instrument of k7, which runs it against production's database after the deploy and
// joins each entry with the issue's stored row of issue_facts, grown to the report's moment: so
// it leaves no issue out, and the count of issues read by a plan of other steps says how many of
// the issues compared the plan rule could move hours of. Its own run over the facts' test history,
// with one tree that ran such a plan, is k28's (main_test.go). Each entry names the columns of the
// row it is compared with.
//
//	go run ./internal/dispatch/legionfacts/independent/cmd \
//	  -project INFRA -database <url> -to 2026-11-11T12:00:00Z
//
// -to defaults to this moment, which is the moment an open issue's work time runs to. The reading
// covers the project's whole history up to it, so the command takes no start.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/legionfacts/independent"
)

func main() {
	project := flag.String("project", "", "the project to read, which is the scope of the answer")
	database := flag.String("database", "", "the Postgres URL to read")
	to := flag.String("to", "", "the moment of the reading, RFC 3339; this moment by default")
	flag.Parse()
	if err := run(context.Background(), os.Stdout, *project, *database, *to); err != nil {
		fmt.Fprintf(os.Stderr, "independent: %v\n", err)
		os.Exit(1)
	}
}

// report is what the command prints: the project and the moment of the reading, the number of
// issues it read facts for and how many of them it read by a plan whose steps are not the default
// steps (independent.Reading's OtherSteps), and one entry for each of those issues, in key order.
type report struct {
	Project          string    `json:"project"`
	Moment           time.Time `json:"moment"`
	Issues           int       `json:"issues"`
	ReadByOtherSteps int       `json:"read_by_other_steps"`
	Facts            []entry   `json:"facts"`
}

// entry is one issue's facts, each under the name of the issue_facts column it is compared with:
// the hours by phase, the work time, the review rounds and the tree root, beside whether the issue
// is that root.
type entry struct {
	IssueKey     string             `json:"issue_key"`
	HoursByPhase map[string]float64 `json:"hours_by_phase"`
	WorkHours    float64            `json:"work_hours"`
	ReviewRounds int                `json:"review_rounds"`
	Root         bool               `json:"root"`
	RootKey      string             `json:"root_key"`
}

// run reads the project at the moment to names, this moment where it names none, and writes the
// report to out.
func run(ctx context.Context, out io.Writer, project, database, to string) error {
	if project == "" {
		return fmt.Errorf("name the project to read with -project")
	}
	if database == "" {
		return fmt.Errorf("name the database to read with -database <url>")
	}
	window := independent.Window{To: time.Now().UTC()}
	if to != "" {
		at, err := time.Parse(time.RFC3339, to)
		if err != nil {
			return fmt.Errorf("read -to %q as a moment: %w", to, err)
		}
		window.To = at.UTC()
	}
	pool, err := pgxpool.New(ctx, database)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer pool.Close()
	reading, err := independent.Read(ctx, pool, independent.Scope{Project: project}, window)
	if err != nil {
		return err
	}
	printed := report{
		Project:          reading.Scope.Project,
		Moment:           reading.Window.To,
		Issues:           len(reading.Issues),
		ReadByOtherSteps: len(reading.OtherSteps),
		Facts:            make([]entry, 0, len(reading.Issues)),
	}
	for _, key := range slices.Sorted(maps.Keys(reading.Issues)) {
		facts := reading.Issues[key]
		printed.Facts = append(printed.Facts, entry{
			IssueKey: key, HoursByPhase: facts.PhaseHours, WorkHours: facts.WorkHours,
			ReviewRounds: facts.ReviewRounds, Root: facts.Root, RootKey: facts.Tree,
		})
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(printed); err != nil {
		return fmt.Errorf("write the report: %w", err)
	}
	return nil
}
