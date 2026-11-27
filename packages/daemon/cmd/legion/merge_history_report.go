package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// unknownCount is what the report prints for a count it could not read. A read that failed and a
// selection of no issue both print it, so neither reads as a population of zeros.
const unknownCount = "UNKNOWN"

// newMergeHistoryReportCommand is `legion merge-history-report`, the operator's census of what the
// record shows of each issue's merge. It reads the database --config's postgres_dsn names, in one
// read-only transaction over one snapshot, through the evidence projection every door reads
// (record.Store.MergeHistoryRows), and migrates and writes nothing: it is run against a deployment's
// database, whose schema may be the release before this one. It prints the selection, the
// snapshot's time, the keys it counted, and for each class the issues, the done ones, those a
// request stands on and those whose known merge misses branch metadata.
func newMergeHistoryReportCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "merge-history-report",
		Short: "count a project's recorded issues by what the record shows of each merge (ANSWERED, PARTIAL or NONE), reading the database only",
		Args:  configOnly,
	}
	configPath := cmd.Flags().String("config", defaultConfigPath, "path to legion.yaml, whose postgres_dsn names the database to read")
	project := cmd.Flags().String("project", "", "the project whose recorded issues to count (default: the file's project)")
	repos := cmd.Flags().StringArray("repo", nil, "count only the issues on this repository, compared without case (repeat it for each, or give none for every repository)")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		return runMergeHistoryReport(ctx, *configPath, *project, *repos, stdout, stderr)
	})
	return cmd
}

// runMergeHistoryReport reads the census and prints it. Each way it can fail to count prints the
// report with every count UNKNOWN and the reason's code on its unknown line, and exits 1: config
// (the file was refused), connect (the database was not reached), read (the read failed) and empty
// (the selection holds no issue). The refusal of the first three goes to stderr, through the URL
// rule (printRefusal), since a DSN can carry a credential.
func runMergeHistoryReport(ctx context.Context, configPath, project string, repos []string, stdout, stderr io.Writer) int {
	const name = "legion merge-history-report"
	report := mergeHistoryReport{Project: project, Repos: repos}
	fail := func(reason string, err error) int {
		if err != nil {
			printRefusal(stderr, name, err)
		}
		report.Unknown = reason
		report.write(stdout)
		return 1
	}
	cfg, err := config.LoadForValidation(configPath, nil)
	if err != nil {
		return fail("config", err)
	}
	if report.Project == "" {
		report.Project = cfg.Project
	}
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		return fail("connect", err)
	}
	defer st.Close()
	var (
		at   time.Time
		rows []record.MergeHistoryRow
	)
	// Repeatable read: every statement reads the snapshot of the first, so the counts and the time
	// printed are of one moment of the database.
	if err := store.TransactWith(ctx, st.Pool(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, "select now()").Scan(&at); err != nil {
				return fmt.Errorf("read the snapshot's time: %w", err)
			}
			var err error
			rows, err = record.NewStore().MergeHistoryRows(ctx, tx, report.Project, repos)
			return err
		}); err != nil {
		return fail("read", err)
	}
	report.SnapshotAt = at
	if len(rows) == 0 {
		return fail("empty", nil)
	}
	report.Rows = rows
	report.write(stdout)
	return 0
}

// mergeHistoryReport is what one census read: the selection (Project, Repos), the snapshot's time,
// the rows it counted, and the code of what kept it from counting (Unknown), empty where it counted.
type mergeHistoryReport struct {
	Project    string
	Repos      []string
	SnapshotAt time.Time
	Rows       []record.MergeHistoryRow
	Unknown    string
}

// mergeHistoryTally is one class's counts, or the whole selection's.
type mergeHistoryTally struct {
	issues, done, requested, missingMetadata int
}

// add counts row once in t.
func (t *mergeHistoryTally) add(row record.MergeHistoryRow) {
	t.issues++
	if row.Phase == phase.Done {
		t.done++
	}
	if row.Requested {
		t.requested++
	}
	if row.MissingMetadata {
		t.missingMetadata++
	}
}

// write prints the report, one fact a line, each line its name and its values: project, repos
// (all for every repository), snapshot_at, selected (the count, then each key), one line for each
// class and a total line, each with issues, done, requested and missing_metadata, and, where it
// did not count, unknown and its code. A count it did not read is UNKNOWN.
func (r mergeHistoryReport) write(w io.Writer) {
	repos := "all"
	if len(r.Repos) > 0 {
		repos = strings.Join(r.Repos, " ")
	}
	fmt.Fprintf(w, "project %s\nrepos %s\n", r.Project, repos)
	snapshot := unknownCount
	if !r.SnapshotAt.IsZero() {
		snapshot = r.SnapshotAt.UTC().Format(time.RFC3339Nano)
	}
	fmt.Fprintf(w, "snapshot_at %s\n", snapshot)
	keys := make([]string, 0, len(r.Rows))
	for _, row := range r.Rows {
		keys = append(keys, row.Key)
	}
	selected := strconv.Itoa(len(keys))
	if r.Unknown != "" && r.SnapshotAt.IsZero() {
		selected = unknownCount
	}
	fmt.Fprintln(w, strings.TrimSpace("selected "+selected+" "+strings.Join(keys, " ")))
	classes := map[record.MergeHistoryClass]*mergeHistoryTally{}
	var total mergeHistoryTally
	for _, class := range record.MergeHistoryClasses {
		classes[class] = &mergeHistoryTally{}
	}
	for _, row := range r.Rows {
		classes[row.Class].add(row)
		total.add(row)
	}
	line := func(label string, t mergeHistoryTally) {
		count := func(n int) string {
			if r.Unknown != "" {
				return unknownCount
			}
			return strconv.Itoa(n)
		}
		fmt.Fprintf(w, "%s issues %s done %s requested %s missing_metadata %s\n", label, count(t.issues), count(t.done), count(t.requested),
			count(t.missingMetadata))
	}
	for _, class := range record.MergeHistoryClasses {
		line(string(class), *classes[class])
	}
	line("total", total)
	if r.Unknown != "" {
		fmt.Fprintf(w, "unknown %s\n", r.Unknown)
	}
}
