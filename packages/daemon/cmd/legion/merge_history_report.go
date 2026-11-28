package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
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
	candidates := mergeCandidateFlags{}
	cmd.Flags().BoolVar(&candidates.read, "read-candidates", false, "also read the merge candidates of each PARTIAL and NONE issue, as the daemon's merge read does, applying nothing")
	cmd.Flags().StringVar(&candidates.progress, "progress", "", "the private file the candidate reads save their progress in, and resume from (required with --read-candidates)")
	cmd.Flags().StringVar(&candidates.githubAPI, "github-api", "", "GitHub's REST root the candidate reads ask (default: https://api.github.com)")
	cmd.Flags().IntVar(&candidates.maxGets, "max-gets", 0, "the most GETs this run's candidate reads send, at most one pass's eight (default: eight)")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		return runMergeHistoryReport(ctx, *configPath, *project, *repos, candidates, stdout, stderr)
	})
	return cmd
}

// mergeCandidateFlags is the census's candidate mode: whether it reads candidates, the private
// file their progress is saved in, GitHub's REST root, and the GETs this run may send.
type mergeCandidateFlags struct {
	read      bool
	progress  string
	githubAPI string
	maxGets   int
}

// runMergeHistoryReport reads the census and prints it. Each way it can fail to count prints the
// report with every count UNKNOWN and the reason's code on its unknown line, and exits 1: config
// (the file was refused), connect (the database was not reached), read (the read failed) and empty
// (the selection holds no issue). The refusal of the first three goes to stderr, through the URL
// rule (printRefusal), since a DSN can carry a credential. With --read-candidates it then reads
// the candidates of each PARTIAL and NONE issue (readMergeCandidates), and a candidate read it
// could not make prints its lines UNKNOWN and exits 1 (candidates).
func runMergeHistoryReport(ctx context.Context, configPath, project string, repos []string, candidates mergeCandidateFlags, stdout, stderr io.Writer) int {
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
	if candidates.read && candidates.progress == "" {
		return fail("config", errors.New("--read-candidates needs --progress, the private file its reads resume from"))
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
	if !candidates.read {
		return 0
	}
	read, err := readMergeCandidates(ctx, configPath, st.Pool(), rows, candidates)
	if err != nil {
		printRefusal(stderr, name, err)
		read.Unknown = true
	}
	read.write(stdout)
	if read.Unknown {
		return 1
	}
	return 0
}

// readMergeCandidates runs one pass of the candidate reads of each PARTIAL and NONE row
// (daemon.ReadMergeCandidates), with the configuration's Dispatch and GitHub Apps, resuming from
// and saving to the progress file, which it keeps private (0600). It writes nothing to the
// database: the reads take the database's one merge read slot and apply no fact.
func readMergeCandidates(ctx context.Context, configPath string, pool *store.Pool, rows []record.MergeHistoryRow, flags mergeCandidateFlags) (mergeCandidateReport, error) {
	report := mergeCandidateReport{}
	for _, row := range rows {
		if row.Class != record.MergeHistoryAnswered {
			report.issues = append(report.issues, daemon.CensusIssue{Key: row.Key, Repo: row.Repo, Class: row.Class, Evidence: row.Evidence})
		}
	}
	cfg, err := config.Load(configPath, nil)
	if err != nil {
		return report, err
	}
	if cfg.DispatchURL == "" || cfg.DispatchTokenFile == "" {
		return report, errors.New("--read-candidates reads Dispatch with dispatch_url and dispatch_token_file, and the file names neither or not both")
	}
	token, err := config.ReadSecretPointer("dispatch_token_file", cfg.DispatchTokenFile)
	if err != nil {
		return report, err
	}
	progress, err := loadCensusProgress(flags.progress)
	if err != nil {
		return report, err
	}
	read := daemon.CensusRead{
		Dispatch:  dispatch.New(cfg.DispatchURL, dispatch.BearerTransport(token, nil)),
		Tokens:    appauth.New(cfg.GitHubApps, appauth.Options{BaseURL: flags.githubAPI}),
		GitHubAPI: flags.githubAPI,
		Pool:      pool,
		Now:       time.Now,
		MaxGets:   flags.maxGets,
		Save:      func(progress daemon.CensusProgress) error { return saveCensusProgress(flags.progress, progress) },
	}
	progress, outcomes, err := daemon.ReadMergeCandidates(ctx, read, report.issues, progress)
	report.progress, report.outcomes = progress, outcomes
	return report, err
}

// loadCensusProgress reads the progress file, and answers an empty progress where it does not
// exist yet: a census's first run.
func loadCensusProgress(path string) (daemon.CensusProgress, error) {
	var progress daemon.CensusProgress
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return progress, nil
	}
	if err != nil {
		return progress, fmt.Errorf("read the census's progress file %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &progress); err != nil {
		return progress, fmt.Errorf("decode the census's progress file %s: %w", path, err)
	}
	return progress, nil
}

// saveCensusProgress writes progress over the progress file whole: to a private file beside it,
// renamed over it, so a run stopped mid-write leaves the last progress it saved.
func saveCensusProgress(path string, progress daemon.CensusProgress) error {
	data, err := json.Marshal(progress)
	if err != nil {
		return err
	}
	next, err := os.CreateTemp(filepath.Dir(path), ".census-progress-*")
	if err != nil {
		return fmt.Errorf("save the census's progress: %w", err)
	}
	if _, err := next.Write(data); err != nil {
		next.Close()
		os.Remove(next.Name())
		return fmt.Errorf("save the census's progress: %w", err)
	}
	if err := next.Close(); err != nil {
		os.Remove(next.Name())
		return fmt.Errorf("save the census's progress: %w", err)
	}
	if err := os.Rename(next.Name(), path); err != nil {
		os.Remove(next.Name())
		return fmt.Errorf("save the census's progress: %w", err)
	}
	return nil
}

// mergeCandidateReport is what one run of the candidate reads came to: the candidate issues, the
// progress saved, each issue's outcome, and whether the run could not read (Unknown).
type mergeCandidateReport struct {
	issues   []daemon.CensusIssue
	progress daemon.CensusProgress
	outcomes map[string]daemon.CensusOutcome
	Unknown  bool
}

// write prints the candidate reads, one fact a line: for PARTIAL, NONE and their total, the
// candidates, those answering, those completed and unread, and those unfinished, split into those
// whose automatic retries go on (unfinished_active) and those that stopped (unfinished_wait); and
// what the reads cost: the GETs, the retries, the attempts whose answer was never saved
// (uncertain), the passes observed and their lower bound (ceil(gets/8)), the token requests, and
// Dispatch's downstream work, which the daemon cannot see (UNKNOWN). A run that could not read
// prints each count UNKNOWN.
func (r mergeCandidateReport) write(w io.Writer) {
	type tally struct{ candidates, answering, unread, active, wait int }
	tallies := map[record.MergeHistoryClass]*tally{record.MergeHistoryPartial: {}, record.MergeHistoryNone: {}}
	var total tally
	for _, issue := range r.issues {
		outcome := r.outcomes[issue.Key]
		for _, t := range []*tally{tallies[issue.Class], &total} {
			t.candidates++
			switch {
			case outcome.Answering:
				t.answering++
			case outcome.Unread:
				t.unread++
			case outcome.Stopped:
				t.wait++
			default:
				t.active++
			}
		}
	}
	count := func(n int) string {
		if r.Unknown {
			return unknownCount
		}
		return strconv.Itoa(n)
	}
	line := func(label string, t tally) {
		fmt.Fprintf(w, "%s candidates %s answering %s unread %s unfinished %s unfinished_active %s unfinished_wait %s\n", label,
			count(t.candidates), count(t.answering), count(t.unread), count(t.active+t.wait), count(t.active), count(t.wait))
	}
	line("read_PARTIAL", *tallies[record.MergeHistoryPartial])
	line("read_NONE", *tallies[record.MergeHistoryNone])
	line("read_total", total)
	uncertain := 0
	for _, progress := range r.progress.Issues {
		uncertain += progress.Uncertain
	}
	gets := r.progress.Gets
	fmt.Fprintf(w, "read_cost gets %s retries %s uncertain %s passes %s passes_lower_bound %s token_requests %s dispatch_downstream UNKNOWN\n",
		count(gets), count(r.progress.Retries), count(uncertain), count(r.progress.Passes), count((gets+7)/8), count(r.progress.TokenRequests))
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
