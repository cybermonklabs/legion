// Package legionfacts computes one issue's stored Legion facts and writes them to issue_facts
// (dispatch://INFRA-2915), with the issues Legion sessions filed, which it writes to
// issue_filings (dispatch://INFRA-2916). The facts are computed once and stored as rows, so a
// reader of them reads rows and computes nothing. The board is one reader of them here; the issue
// table and the evals read them under dispatch://INFRA-2916.
//
// The computation holds no loop and no lease: facts.go answers the rows of a key set (Compute)
// or of a whole project (ComputeProject), and store.go writes them (WriteProject). Run is the
// loop that decides when to call either, under one lease and woken by the feeds a row is
// computed from (consumer.go).
//
// The numbers are the metrics package's: Compute and ComputeProject read a population and call
// metrics.ProjectFacts, which is the one computation of the per-issue facts, and the filings are
// metrics.ProjectFilings over the same population. Nothing here states a rule of its own, so a
// stored row can never disagree with the number a page computes. Both readers pass a nil GitHub
// client: a pass that runs every half minute must make no GitHub call, and metrics documents
// that a merge time Dispatch cannot reach leaves that issue with none.
package legionfacts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Querier is what a read or a write in this package needs of its connection: the pool, or a
// transaction already open on it, which a consumer that writes the rows and its cursor in one
// commit holds (store.Pool refuses a second connection to a caller holding one).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Row is one issue's stored Legion facts, the row issue_facts holds. Every field is the column of
// the same name, and the migrations state what each one means (20261111090000 and
// 20261113111231).
//
// HoursByPhase holds the board's column ids, a column with no hours left out, so an issue whose
// work took no measured time holds an empty map and still has a row: a row means Legion worked
// the issue. WaitingReason is "" where the issue waits on nothing, which the column holds as
// null, and Repo is "" where no repository resolves, which the column holds as null too.
// WorkFirstAt and WorkLastAt are nil until the issue has measured work. Bars holds every one of
// the seven bars and IdleByRole every one of the six roles. UpdatedAt is when the row was
// written, never when it was read.
type Row struct {
	Project       string
	Issue         string
	OpenedAt      time.Time
	ClosedAt      *time.Time
	HoursByPhase  map[string]float64
	WorkHours     float64
	ReviewRounds  int
	RootKey       string
	TreeDepth     int
	SlotHours     float64
	WaitingReason string
	WorkFirstAt   *time.Time
	WorkLastAt    *time.Time

	ParkedHours                  float64
	QueuedHours                  float64
	StartedAt                    *time.Time
	MergedAt                     *time.Time
	Bars                         map[string]metrics.BarPart
	IdleByRole                   map[string]float64
	SlotWaitHours                float64
	SentBack                     bool
	ImplementingAfterFirstReview float64
	TestingAfterFirstReview      float64
	Children                     int
	Repo                         string

	UpdatedAt time.Time
}

// Filing is one issue a Legion session filed, the row issue_filings holds (migration
// 20261113082155): the filed issue, the project whose session filed it, when, the role and the
// issue that filed it and the phase that issue was in, the class metric 4 gives the filing, and
// the filed issue's tree root as it stands now. FilerIssue and FilerPhase are "" for a filing by
// the controller, which works no issue.
type Filing struct {
	Project    string
	Issue      string
	FiledAt    time.Time
	FilerRole  string
	FilerIssue string
	FilerPhase string
	Class      string
	RootKey    string
}

// Compute answers the rows of the named issues as of now. It reads the population the facts of
// those issues are computed from (metrics.ReadIssues), which is wider than the keys asked for
// because an issue's hours read its whole tree, and keeps the rows of the keys alone. A key that
// carries no Legion phase work gets no row, as it gets no facts entry.
func Compute(ctx context.Context, pool *store.Pool, project string, keys []string, now time.Time) ([]Row, error) {
	rows, _, err := computeKeys(ctx, pool, project, keys, nil, now)
	return rows, err
}

// ComputeProject answers the rows of every issue of the project with Legion phase work, open or
// closed, as of now: the sweep's own read (metrics.ReadProject).
func ComputeProject(ctx context.Context, pool *store.Pool, project string, now time.Time) ([]Row, error) {
	rows, _, err := computeWhole(ctx, pool, project, now)
	return rows, err
}

// computeKeys is Compute with the filings beside the rows: the filings of the filed issues
// filed names that a Legion session of the project filed, computed over the same population as
// the rows, which reads each filer's issue with them (metrics.ReadIssues). It is what one pass of
// the consumer computes for a project it does not read whole.
func computeKeys(ctx context.Context, pool *store.Pool, project string, keys, filed []string, now time.Time) ([]Row, []Filing, error) {
	if len(keys) == 0 && len(filed) == 0 {
		return nil, nil, nil
	}
	population, err := metrics.ReadIssues(ctx, pool, nil, project, keys, filed, now)
	if err != nil {
		return nil, nil, err
	}
	wanted := make(map[string]bool, len(keys))
	for _, key := range keys {
		wanted[key] = true
	}
	rows, err := rowsOf(ctx, pool, population, project, wanted, now)
	if err != nil {
		return nil, nil, err
	}
	return rows, filingsOf(population, project), nil
}

// computeWhole is ComputeProject with the filings beside the rows: every filing a Legion
// session of the project ever made, from the same population (metrics.ReadProject).
func computeWhole(ctx context.Context, pool *store.Pool, project string, now time.Time) ([]Row, []Filing, error) {
	population, err := metrics.ReadProject(ctx, pool, nil, project, now)
	if err != nil {
		return nil, nil, err
	}
	rows, err := rowsOf(ctx, pool, population, project, nil, now)
	if err != nil {
		return nil, nil, err
	}
	return rows, filingsOf(population, project), nil
}

// rowsOf turns the population's facts into rows, in key order. wanted names the issues to keep,
// and a nil wanted keeps every issue with facts.
//
// The waiting reason is the one card rule (legion.CardWaiting), read through the one waiting
// line (legion.WaitingLine), which the board builds from its own cards. The daemon's admission
// block is read once per computation, because it is one row of the project rather than a fact of
// any issue, and the repositories are read once for every row kept (ReadRepos).
func rowsOf(ctx context.Context, q Querier, population *metrics.Population, project string, wanted map[string]bool, now time.Time) ([]Row, error) {
	admission, err := readAdmission(ctx, q, project)
	if err != nil {
		return nil, err
	}
	line := legion.NewWaitingLine(legion.WaitingLineInput{
		Admission: admission,
		Issue:     func(key string) (legion.LineIssue, bool) { return lineIssue(population, key) },
		Tree:      population.TreeNode,
	})
	facts := metrics.ProjectFacts(population)
	keys := make([]string, 0, len(facts))
	for _, key := range slices.Sorted(maps.Keys(facts)) {
		if wanted == nil || wanted[key] {
			keys = append(keys, key)
		}
	}
	repos, err := ReadRepos(ctx, q, keys)
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(keys))
	for _, key := range keys {
		fact := facts[key]
		rows = append(rows, Row{
			Project:                      project,
			Issue:                        key,
			OpenedAt:                     fact.OpenedAt,
			ClosedAt:                     fact.ClosedAt,
			HoursByPhase:                 fact.PhaseHours,
			WorkHours:                    fact.WorkHours,
			ReviewRounds:                 fact.ReviewRounds,
			RootKey:                      fact.Tree,
			TreeDepth:                    fact.TreeDepth,
			SlotHours:                    fact.SlotHours,
			WaitingReason:                reasonOf(line, population.Issues[key]),
			WorkFirstAt:                  fact.WorkFirstAt,
			WorkLastAt:                   fact.WorkLastAt,
			ParkedHours:                  fact.ParkedHours,
			QueuedHours:                  fact.QueuedHours,
			StartedAt:                    fact.StartedAt,
			MergedAt:                     fact.MergedAt,
			Bars:                         fact.Bars,
			IdleByRole:                   fact.IdleByRole,
			SlotWaitHours:                fact.SlotWaitHours,
			SentBack:                     fact.SentBack,
			ImplementingAfterFirstReview: fact.ImplementingHoursAfterFirstReview,
			TestingAfterFirstReview:      fact.TestingHoursAfterFirstReview,
			Children:                     fact.Children,
			Repo:                         repos[key],
			UpdatedAt:                    now,
		})
	}
	return rows, nil
}

// filingsOf turns the population's filings into rows, classified by metric 4's own rule
// (metrics.ProjectFilings). issue_filings holds one row per filed issue, so a filed issue that
// somehow carries two `issue.created` events by Legion sessions keeps the first, the order the
// population read them in; a second row of one key in one write would fail the whole write.
func filingsOf(population *metrics.Population, project string) []Filing {
	filings := []Filing{}
	seen := map[string]bool{}
	for _, filed := range metrics.ProjectFilings(population) {
		if seen[filed.Key] {
			continue
		}
		seen[filed.Key] = true
		filings = append(filings, Filing{
			Project:    project,
			Issue:      filed.Key,
			FiledAt:    filed.At,
			FilerRole:  filed.Filer,
			FilerIssue: filed.FilerIssue,
			FilerPhase: filed.FilerPhase,
			Class:      filed.Class,
			RootKey:    filed.Root,
		})
	}
	return filings
}

// reposStatement resolves the repository of every issue its one argument names, by the server's
// one rule (refs.IssueRepoTerms), which the issue page reads a page of issues by. The text is
// built once, at start, as refs builds its own statement over the same terms.
var reposStatement = `
	with recursive subjects as (select unnest($1::text[]) as key), ` + refs.IssueRepoTerms + `
	select i.key, ` + refs.IssueRepoResolved + `, ` + refs.IssueRepoTooDeep + `
	from issues i ` + refs.IssueRepoJoin + `
	where i.key = any($1::text[])`

// ReadRepos is the repository each of the keys resolves to, by key, and no entry for an issue
// none resolves for: the repository a row of issue_facts holds, and the one the Legion evals' day
// rows cut a filing by (evals.ReadLegionDay). A parent chain the walk could not finish names no
// answer either: the issue page refuses such an issue rather than answer its project's default,
// which would be a wrong one, and a refusal here would stop every pass of the project's facts for
// good. The row holds no repository for it, which is the answer a chain that names none gets.
func ReadRepos(ctx context.Context, q Querier, keys []string) (map[string]string, error) {
	repos := map[string]string{}
	if len(keys) == 0 {
		return repos, nil
	}
	rows, err := q.Query(ctx, reposStatement, keys)
	if err != nil {
		return nil, fmt.Errorf("resolve the repositories of %d issues: %w", len(keys), err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var repo *string
		var tooDeep bool
		if err := rows.Scan(&key, &repo, &tooDeep); err != nil {
			return nil, fmt.Errorf("resolve the repositories of %d issues: %w", len(keys), err)
		}
		if repo != nil && !tooDeep {
			repos[key] = *repo
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve the repositories of %d issues: %w", len(keys), err)
	}
	return repos, nil
}

// lineIssue is what the waiting line reads of one issue the daemon reports (legion.LineIssue):
// the phase of its newest `phase` observation and the slot of its newest `slot` one. An issue the
// sampler recorded no observation of is an issue the daemon reports nothing of, which is the set
// the board's own line reads (api/legion_board.go's issues map), and the line numbers it in no
// place and draws it no card.
func lineIssue(population *metrics.Population, key string) (legion.LineIssue, bool) {
	issue := population.Issues[key]
	if issue == nil || len(issue.Observed) == 0 {
		return legion.LineIssue{}, false
	}
	phase, _ := newestPhase(issue)
	return legion.LineIssue{Phase: phase, Slot: newestSlot(issue)}, true
}

// reasonOf is why the issue waits, by the one card rule (legion.CardWaiting), and "" where the
// rule answers that it waits for nothing. The rule reads the issue's phase, the column it stands
// in, its place in the daemon's waiting line, the issues a blocker link holds it on, when it
// entered its phase, and the wait the daemon recorded. The line answers the middle three, as it
// answers them for the card the board draws of the same issue. The phase and the wait come from
// the newest observations of the issue, which the population already holds.
//
// An issue the board draws no card for stands in no column, and the rule then answers that it
// waits for nothing: a child its tree has not started is counted on the card above it, and the
// stored row of it says what that card says of it, which is nothing.
func reasonOf(line *legion.WaitingLine, issue *metrics.Issue) string {
	if issue == nil {
		return ""
	}
	phase, phaseSince := newestPhase(issue)
	waiting := legion.CardWaiting(legion.WaitingInput{
		Phase:        phase,
		Column:       line.Column(issue.Key),
		WaitingPlace: line.Place(issue.Key),
		BlockedBy:    line.BlockedBy(issue.Key),
		PhaseSince:   phaseSince,
		WaitingOn:    newestWait(issue),
	})
	if waiting == nil {
		return ""
	}
	return waiting.Reason
}

// newestPhase is the phase the issue is in and when it entered it: the newest `phase` observation
// and the moment it was recorded, since the sampler writes a row only where the value moved. The
// marks are in order, so the newest is the last.
func newestPhase(issue *metrics.Issue) (string, *time.Time) {
	marks := issue.Observed[legion.FieldPhase]
	if len(marks) == 0 {
		return "", nil
	}
	newest := marks[len(marks)-1]
	since := newest.At.UTC()
	return legion.JSONString([]byte(newest.Value)), &since
}

// newestSlot is the slot the issue holds of its own, from its newest `slot` observation, and nil
// where it holds none or where the daemon sent a value of another shape (legion.ParseSlot).
func newestSlot(issue *metrics.Issue) *int {
	marks := issue.Observed[legion.FieldSlot]
	if len(marks) == 0 {
		return nil
	}
	slot, _ := legion.ParseSlot([]byte(marks[len(marks)-1].Value))
	return slot
}

// newestWait is the wait the daemon recorded of the issue, from its newest `waiting_on`
// observation (legion.ParseWait). A value of another shape, and a wait with no kind, read as no
// wait at all, which is the one decode the board reads the same field by.
func newestWait(issue *metrics.Issue) *legion.Wait {
	marks := issue.Observed[legion.FieldWaitingOn]
	if len(marks) == 0 {
		return nil
	}
	wait, _ := legion.ParseWait([]byte(marks[len(marks)-1].Value))
	return wait
}

// readAdmission is the daemon's newest admission block, which the sampler writes as one row of
// the project under legion.AdmissionKey. It is nil for a project whose daemon has never answered
// and for one that sends no block, and nil again where the row holds a value of another shape:
// the daemon owns the wire shape, and a reader of one names what it expects.
func readAdmission(ctx context.Context, q Querier, project string) (*legion.Admission, error) {
	var value []byte
	err := q.QueryRow(ctx, `
		select value
		from legion_observations
		where project = $1 and issue_key = $2 and field = $3
		order by observed_at desc, id desc
		limit 1
	`, project, legion.AdmissionKey, legion.FieldAdmission).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the admission block of %s: %w", project, err)
	}
	var admission legion.Admission
	if err := json.Unmarshal(value, &admission); err != nil {
		return nil, nil
	}
	return &admission, nil
}
