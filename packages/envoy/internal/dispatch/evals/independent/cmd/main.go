// Command independent prints the independent reading of dispatch://INFRA-2916's six Legion
// measures for one project over one window, as JSON: the project and the window it covered, how
// many issues finished in the window, and for each measure and each cut (all, roots, children)
// the cut's issue count and every series by the name and the statistics the eval list route
// answers them with (GET /api/v1/projects/{key}/evals?kind=legion), null where the measure reads
// "no data".
//
// It is the instrument k6 records and re-runs, so its output is what a reader compares, value
// by value, against the eval routes over the same window. The routes read whole UTC days, so
// -from and -to are UTC midnights in a comparison.
//
//	go run ./internal/dispatch/evals/independent/cmd -project LEGK -database "$DATABASE_URL" \
//	  -from 2026-10-28T00:00:00Z -to 2026-11-04T00:00:00Z
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/evals/independent"
)

func main() {
	project := flag.String("project", "", "the project to read")
	database := flag.String("database", os.Getenv("DATABASE_URL"), "the Postgres URL to read, or $DATABASE_URL")
	from := flag.String("from", "", "the start of the window, RFC 3339")
	to := flag.String("to", "", "the end of the window, RFC 3339, not included")
	flag.Parse()
	if err := run(*project, *database, *from, *to); err != nil {
		fmt.Fprintf(os.Stderr, "independent: %v\n", err)
		os.Exit(1)
	}
}

// run reads the project over the window and writes the reading to standard output.
func run(project, database, from, to string) error {
	if project == "" {
		return errors.New("name the project to read with -project")
	}
	if database == "" {
		return errors.New("name the database to read with -database or $DATABASE_URL")
	}
	var window independent.Window
	for _, one := range []struct {
		name, value string
		into        *time.Time
	}{{"-from", from, &window.From}, {"-to", to, &window.To}} {
		if one.value == "" {
			return fmt.Errorf("name the window with -from and -to")
		}
		at, err := time.Parse(time.RFC3339, one.value)
		if err != nil {
			return fmt.Errorf("read %s %q as a moment: %w", one.name, one.value, err)
		}
		*one.into = at.UTC()
	}
	if !window.From.Before(window.To) {
		return fmt.Errorf("-from %s must be before -to %s", from, to)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, database)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer pool.Close()
	reading, err := independent.Read(ctx, pool, project, window)
	if err != nil {
		return err
	}
	report, err := json.MarshalIndent(reading, "", "  ")
	if err != nil {
		return fmt.Errorf("write the reading: %w", err)
	}
	fmt.Printf("%s\n", report)
	return nil
}
