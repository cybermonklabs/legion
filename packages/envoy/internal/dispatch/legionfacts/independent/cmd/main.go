// Command independent prints the independent reading of dispatch://INFRA-2915's R1 for one
// project, as JSON: the scope it covered, the window it used, how many issues it read facts for,
// how many of them have no hours at all, and one entry per issue with each fact.
//
// It is the instrument k1 and k9 store and re-run, so its output is what a reader compares
// against the server's own computation. Every count it prints stands beside the scope and the
// window it was given, because a comparison whose two sides read different scopes proves
// nothing.
//
//	go run ./internal/dispatch/legionfacts/independent/cmd \
//	  -project INFRA -database "$DATABASE_URL" -to 2026-11-11T12:00:00Z
//
// -to defaults to this moment, which is the moment an open issue's work time runs to, and -from
// to the beginning of the project's history. A reading of a whole project takes the default.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/legionfacts/independent"
)

func main() {
	project := flag.String("project", "", "the project to read, which is the scope of the answer")
	database := flag.String("database", os.Getenv("DATABASE_URL"), "the Postgres URL to read, or $DATABASE_URL")
	to := flag.String("to", "", "the moment of the computation, RFC 3339; this moment by default")
	from := flag.String("from", "", "the start of the window, RFC 3339; the beginning of the history by default")
	flag.Parse()
	if err := run(*project, *database, *from, *to); err != nil {
		fmt.Fprintf(os.Stderr, "independent: %v\n", err)
		os.Exit(1)
	}
}

// run reads the project and writes the report to standard output.
func run(project, database, from, to string) error {
	if project == "" {
		return fmt.Errorf("name the project to read with -project")
	}
	if database == "" {
		return fmt.Errorf("name the database to read with -database or $DATABASE_URL")
	}
	window := independent.Window{To: time.Now().UTC()}
	if to != "" {
		at, err := time.Parse(time.RFC3339, to)
		if err != nil {
			return fmt.Errorf("read -to %q as a moment: %w", to, err)
		}
		window.To = at.UTC()
	}
	if from != "" {
		at, err := time.Parse(time.RFC3339, from)
		if err != nil {
			return fmt.Errorf("read -from %q as a moment: %w", from, err)
		}
		window.From = at.UTC()
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, database)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer pool.Close()
	reading, err := independent.Read(ctx, pool, independent.Scope{Project: project}, window)
	if err != nil {
		return err
	}
	report, err := json.MarshalIndent(reading.Report(), "", "  ")
	if err != nil {
		return fmt.Errorf("write the report: %w", err)
	}
	fmt.Printf("%s\n", report)
	return nil
}
