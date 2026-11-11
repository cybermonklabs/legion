// Package legionfacts computes one issue's stored Legion facts and writes them to issue_facts
// (dispatch://INFRA-2915). The facts are computed once and stored as rows, so the board, the
// issue table and every eval read rows and compute nothing.
//
// The computation holds no loop and no lease: facts.go answers the rows of a key set (Compute)
// or of a whole project (ComputeProject), and store.go writes them (Write). Run is the loop
// that decides when to call either, under one lease and woken by the feeds a row is computed
// from (consumer.go).
//
// The numbers are the metrics package's: Compute and ComputeProject read a population and call
// metrics.ProjectFacts, which is the one computation of the per-issue facts. Nothing here states
// a rule of its own, so a stored row can never disagree with the number a page computes. Both
// readers pass a nil GitHub client: a pass that runs every half minute must make no GitHub call,
// and metrics documents that a merge time Dispatch cannot reach leaves that issue with none.
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
// the same name, and the migration states what each one means.
//
// HoursByPhase holds the board's column ids, a column with no hours left out, so an issue whose
// work took no measured time holds an empty map and still has a row: a row means Legion worked
// the issue. WaitingReason is "" where the issue waits on nothing, which the column holds as
// null. WorkFirstAt and WorkLastAt are nil until the issue has measured work. UpdatedAt is when
// the row was written, never when it was read.
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
	UpdatedAt     time.Time
}

// Compute answers the rows of the named issues as of now. It reads the population the facts of
// those issues are computed from (metrics.ReadIssues), which is wider than the keys asked for
// because an issue's hours read its whole tree, and keeps the rows of the keys alone. A key that
// carries no Legion phase work gets no row, as it gets no facts entry.
func Compute(ctx context.Context, pool *store.Pool, project string, keys []string, now time.Time) ([]Row, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	population, err := metrics.ReadIssues(ctx, pool, nil, project, keys, now)
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]bool, len(keys))
	for _, key := range keys {
		wanted[key] = true
	}
	return rowsOf(ctx, pool, population, project, wanted, now)
}

// ComputeProject answers the rows of every issue of the project with Legion phase work, open or
// closed, as of now: the sweep's own read (metrics.ReadProject).
func ComputeProject(ctx context.Context, pool *store.Pool, project string, now time.Time) ([]Row, error) {
	population, err := metrics.ReadProject(ctx, pool, nil, project, now)
	if err != nil {
		return nil, err
	}
	return rowsOf(ctx, pool, population, project, nil, now)
}

// rowsOf turns the population's facts into rows, in key order. wanted names the issues to keep,
// and a nil wanted keeps every issue with facts.
//
// The waiting reason is the one card rule (legion.CardWaiting), which reads the daemon's own
// admission block: the block is read once per computation, because it is one row of the project
// rather than a fact of any issue.
func rowsOf(ctx context.Context, q Querier, population *metrics.Population, project string, wanted map[string]bool, now time.Time) ([]Row, error) {
	admission, err := readAdmission(ctx, q, project)
	if err != nil {
		return nil, err
	}
	waits := newWaitingLine(population, admission)
	facts := metrics.ProjectFacts(population)
	rows := make([]Row, 0, len(facts))
	for _, key := range slices.Sorted(maps.Keys(facts)) {
		if wanted != nil && !wanted[key] {
			continue
		}
		fact := facts[key]
		rows = append(rows, Row{
			Project:       project,
			Issue:         key,
			OpenedAt:      fact.OpenedAt,
			ClosedAt:      fact.ClosedAt,
			HoursByPhase:  fact.PhaseHours,
			WorkHours:     fact.WorkHours,
			ReviewRounds:  fact.ReviewRounds,
			RootKey:       fact.Tree,
			TreeDepth:     fact.TreeDepth,
			SlotHours:     fact.SlotHours,
			WaitingReason: waits.reasonOf(population.Issues[key]),
			WorkFirstAt:   fact.WorkFirstAt,
			WorkLastAt:    fact.WorkLastAt,
			UpdatedAt:     now,
		})
	}
	return rows, nil
}

// waitingLine is what the daemon's admission block says of each issue: its place in the waiting
// line from 1, and the issues a blocker link holds it on. read is whether a block was recorded
// at all, which the column rule answers differently from a block that names the issue nowhere
// (legion.Queued): a project whose daemon has never answered has no admission row, and the board
// queues its unstarted issues by Dispatch's rank.
type waitingLine struct {
	read     bool
	places   map[string]int
	blockers map[string][]string
}

// newWaitingLine numbers the waiting line and takes the blocker links, as the board numbers them
// (api/legion_board.go's legionBoardCompute): each key once, in the daemon's own order, and a key
// the population does not hold is numbered by neither reader.
func newWaitingLine(population *metrics.Population, admission *legion.Admission) *waitingLine {
	line := &waitingLine{places: map[string]int{}, blockers: map[string][]string{}}
	if admission == nil {
		return line
	}
	line.read = true
	for _, key := range admission.Waiting {
		if population.Issues[key] == nil {
			continue
		}
		if _, numbered := line.places[key]; numbered {
			continue
		}
		line.places[key] = len(line.places) + 1
	}
	for _, blocked := range admission.Blocked {
		line.blockers[blocked.Issue] = blocked.BlockedBy
	}
	return line
}

// queuedBy is what this line knows of one issue, in the shape the column rule reads
// (legion.Queued): nothing at all where no read recorded an admission block, and otherwise
// whether the line names the issue or a blocker link holds it.
func (l *waitingLine) queuedBy(key string, blockedBy []string) legion.Queued {
	switch {
	case !l.read:
		return legion.QueuedUnread
	case l.places[key] > 0 || len(blockedBy) > 0:
		return legion.QueuedYes
	default:
		return legion.QueuedNo
	}
}

// reasonOf is why the issue waits, by the one card rule (legion.CardWaiting), and "" where the
// rule answers that it waits for nothing. The rule reads the issue's phase, the column it stands
// in (legion.Column, the column the board draws it in), its place in the daemon's waiting line,
// the issues a blocker link holds it on, when it entered its phase, and the wait the daemon
// recorded. The first five come from the newest observations of the issue, which the population
// already holds, and from what the line knows of the issue (queuedBy).
func (l *waitingLine) reasonOf(issue *metrics.Issue) string {
	if issue == nil {
		return ""
	}
	phase, phaseSince := newestPhase(issue)
	blockedBy := l.blockers[issue.Key]
	var place *int
	if numbered, named := l.places[issue.Key]; named {
		place = &numbered
	}
	waiting := legion.CardWaiting(legion.WaitingInput{
		Phase:        phase,
		Column:       legion.Column(phase, newestSlot(issue), l.queuedBy(issue.Key, blockedBy)),
		WaitingPlace: place,
		BlockedBy:    blockedBy,
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
