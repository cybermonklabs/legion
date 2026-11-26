package replay

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ruleTables is where the replay's admission pass reads and keeps what a rule the daemon does not
// run records beyond the daemon's own tables (admit.RuleTables), over the tables the replay makes
// in its own database (replayTables): the merger's ready marks the rule legion-v19.57.3 gives the
// slot back at, which the replay's handler writes (ReadyMark), the covers that stand under the
// rule cover, which the pass itself keeps, and the first slots and the arrivals the rules
// returned-by-first-slot, rework-first, rework-first-keep and arrival-order place the line by,
// which the pass's read of its facts keeps. A rule that reads none of them reads nothing here.
type ruleTables struct{}

var _ admit.RuleTables = ruleTables{}

// readsFirstSlots says whether rule places the line by each issue's first slot in its current
// generation — the returned part of record.LineReturnedByFirstSlot and of record.LineReworkFirst,
// and an issue with no arrival kept under a rule that places by arrival (readsArrivals) — which
// the replay keeps in a table of its own (replay_first_slots), since the daemon keeps no slot's
// moment once the slot goes back.
func readsFirstSlots(rule record.SlotRule) bool {
	return rule.OrMain().Order == record.LineReturnedByFirstSlot || readsArrivals(rule)
}

// readsArrivals says whether rule places issues by the moment each arrived in the line in its
// current generation (record.LineReworkFirst, record.LineArrival), which the replay keeps in a
// table of its own (replay_arrivals), since the daemon keeps no such moment.
func readsArrivals(rule record.SlotRule) bool {
	order := rule.OrMain().Order
	return order == record.LineReworkFirst || order == record.LineArrival
}

// ReadRuleFacts adds the ready marks, the first slots, the arrivals, the waits beneath and the
// covers the facts' rule reads to facts. A ready mark reads for an issue standing in merging at the
// generation and the merger's phase entry it was made at, so a mark of an earlier visit to merging
// reads nothing once the issue has left it and come back (record.LineFacts.Ready). Under a rule
// that places the line by first slots, the slot each holder holds now is first kept as its
// generation's first where the table holds none for that generation (keepFirstSlots), and each
// issue's first slot at the generation it stands at is then read (record.LineFacts.FirstSlots).
// Under a rule that places by arrival, each of issues is first kept as arriving at the moment its
// record last moved where the table holds no arrival for its generation (keepArrivals), and each
// arrival is then read the same way (record.LineFacts.Arrivals). A read in a transaction that may
// write nothing — the start state's view (admit.Admission.View), which the replay reads in a
// read-only transaction — keeps neither, and reads what the passes and the start state kept. Under
// a rule whose start may take a cover, an issue standing in accepting whose sign-off the workflow
// held at its generation and its architect's entry (HeldSignOff), and an issue standing in merging
// whose pull request record is merged (readMerged), wait on the issues beneath them still in the
// workflow, the set both of the workflow's holds read (record.OpenBeneath;
// record.LineFacts.BeneathWaits). The covers are the table's whole (record.LineFacts.Covers): a
// replay runs one project.
func (ruleTables) ReadRuleFacts(ctx context.Context, tx pgx.Tx, issues []record.Issue, facts *record.LineFacts) error {
	rule := facts.Rule.OrMain()
	if rule.ReadsReadyMark() {
		ready, err := readMarks(ctx, tx, `select m.issue, m.generation from replay_ready_marks m
			left join phases p on p.issue = m.issue and p.role = $2
			where m.issue = any($1) and m.entry = coalesce(p.entry, 0)`, standingIn(issues, phase.Merging), string(claim.RoleMerger))
		if err != nil {
			return fmt.Errorf("read the merger's ready marks: %w", err)
		}
		if facts.Ready == nil {
			facts.Ready = make(map[string]struct{})
		}
		for key := range ready {
			facts.Ready[key] = struct{}{}
		}
	}
	if readsFirstSlots(rule) {
		var readOnly string
		if err := tx.QueryRow(ctx, `select current_setting('transaction_read_only')`).Scan(&readOnly); err != nil {
			return fmt.Errorf("read whether the transaction may write: %w", err)
		}
		keeps := readOnly != "on"
		if keeps {
			if err := keepFirstSlots(ctx, tx); err != nil {
				return err
			}
		}
		first, err := readFirstSlots(ctx, tx, issues)
		if err != nil {
			return err
		}
		facts.FirstSlots = first
		if readsArrivals(rule) {
			if keeps {
				if err := keepArrivals(ctx, tx, issues); err != nil {
					return err
				}
			}
			arrived, err := readGenerationTimes(ctx, tx, "replay_arrivals", "arrived_at", issues)
			if err != nil {
				return fmt.Errorf("read the arrivals: %w", err)
			}
			facts.Arrivals = arrived
		}
	}
	if rule.Take != record.TakeCover {
		return nil
	}
	held, err := readMarks(ctx, tx, `select s.issue, s.generation from replay_held_signoffs s
		left join phases p on p.issue = s.issue and p.role = $2
		where s.issue = any($1) and s.entry = coalesce(p.entry, 0)`, standingIn(issues, phase.Accepting), string(claim.RoleArchitect))
	if err != nil {
		return fmt.Errorf("read the held sign-offs: %w", err)
	}
	merged, err := readMerged(ctx, tx, slices.Sorted(maps.Keys(standingIn(issues, phase.Merging))))
	if err != nil {
		return fmt.Errorf("read the merged pull requests: %w", err)
	}
	facts.BeneathWaits = make(map[string][]string)
	for _, issue := range issues {
		_, signs := held[issue.Key]
		_, closes := merged[issue.Key]
		if !signs && !closes {
			continue
		}
		if open := record.OpenBeneath(issue, issues); len(open) > 0 {
			facts.BeneathWaits[issue.Key] = open
		}
	}
	rows, err := tx.Query(ctx, `select waiter, holder from replay_covers`)
	if err != nil {
		return fmt.Errorf("read the covers that stand: %w", err)
	}
	defer rows.Close()
	facts.Covers = make(map[string]string)
	for rows.Next() {
		var waiter, holder string
		if err := rows.Scan(&waiter, &holder); err != nil {
			return fmt.Errorf("read a cover: %w", err)
		}
		facts.Covers[waiter] = holder
	}
	return rows.Err()
}

// standingIn is the generation of each issue of issues that stands in at (record.StandsAt), by
// key.
func standingIn(issues []record.Issue, at phase.Phase) map[string]uint64 {
	generations := make(map[string]uint64)
	for _, issue := range issues {
		if record.StandsAt(issue) == at {
			generations[issue.Key] = issue.Generation
		}
	}
	return generations
}

// readMerged is the issues of keys, the issues standing in merging, whose pull request record is
// merged: the merge the close of each waits on, read as the engine reads it (workflow's awaitsMerge
// and closeAwaitingMerge), by the issue alone. The record's generation is the generation of its CI
// settlement (record.PullRequest.Generation), no generation of the issue's, so it is not compared.
func readMerged(ctx context.Context, tx pgx.Tx, keys []string) (map[string]struct{}, error) {
	merged := make(map[string]struct{})
	if len(keys) == 0 {
		return merged, nil
	}
	rows, err := tx.Query(ctx, `select issue from pull_requests where issue = any($1) and state = $2`, keys, string(record.PullRequestMerged))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		merged[key] = struct{}{}
	}
	return merged, rows.Err()
}

// readMarks is the issues of generations that query, given their keys and arg, answers at the
// generation each stands at: query selects an issue's key and a generation, and a row of another
// generation is no mark of the one the issue stands at.
func readMarks(ctx context.Context, tx pgx.Tx, query string, generations map[string]uint64, arg string) (map[string]struct{}, error) {
	marked := make(map[string]struct{})
	if len(generations) == 0 {
		return marked, nil
	}
	rows, err := tx.Query(ctx, query, slices.Sorted(maps.Keys(generations)), arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var generation int64
		if err := rows.Scan(&key, &generation); err != nil {
			return nil, err
		}
		if uint64(generation) == generations[key] {
			marked[key] = struct{}{}
		}
	}
	return marked, rows.Err()
}

// keepFirstSlots keeps the slot each holder holds now as the first slot of the generation its
// issue stands at, where the table holds none for that issue and generation: every pass reads its
// facts before it moves a slot, so the first pass after a grant keeps it before any pass can give
// it back, and a slot taken again later in the generation leaves the first one standing. The
// moment is the slot's admitted_at, which the pass that granted it wrote from its clock, the
// replay's simulated one.
func keepFirstSlots(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `insert into replay_first_slots (issue, generation, slot_at)
		select s.issue, i.generation, s.admitted_at from slots s join issues i on i.key = s.issue
		on conflict (issue, generation) do nothing`); err != nil {
		return fmt.Errorf("keep the first slots: %w", err)
	}
	return nil
}

// keepFirstSlot keeps at as issue's first slot in the generation it stands at, where the table
// holds none for that issue and generation: the start state's first slot of an issue in the run it
// stands in at the window's start, as the record shows it (driver.firstSlotBefore), which no slot
// row of the start state carries once the issue gave that slot back.
func keepFirstSlot(ctx context.Context, tx pgx.Tx, issue record.Issue, at time.Time) error {
	if _, err := tx.Exec(ctx, `insert into replay_first_slots (issue, generation, slot_at) values ($1, $2, $3)
		on conflict (issue, generation) do nothing`, issue.Key, int64(issue.Generation), at); err != nil {
		return fmt.Errorf("keep the first slot of %s: %w", issue.Key, err)
	}
	return nil
}

// readFirstSlots is the first slot of each of issues in the generation it stands at, by key, from
// the table keepFirstSlots and the start state (keepFirstSlot) keep (readGenerationTimes).
func readFirstSlots(ctx context.Context, tx pgx.Tx, issues []record.Issue) (map[string]time.Time, error) {
	first, err := readGenerationTimes(ctx, tx, "replay_first_slots", "slot_at", issues)
	if err != nil {
		return nil, fmt.Errorf("read the first slots: %w", err)
	}
	return first, nil
}

// keepArrivals keeps the moment each of issues arrived in the line in the generation it stands at,
// where the table holds none for that issue and generation: the moment its record last moved, read
// at the first pass after the record was written in that generation, which is the moment it was
// written (a store built with the replay's clock stamps it there), so the first pass that reads an
// issue keeps its arrival before any pass can place it. The start state's issues are kept before
// the first pass from the record's own entries (keepArrival, driver.arrivalBefore).
func keepArrivals(ctx context.Context, tx pgx.Tx, issues []record.Issue) error {
	if len(issues) == 0 {
		return nil
	}
	keys := make([]string, 0, len(issues))
	for _, issue := range issues {
		keys = append(keys, issue.Key)
	}
	if _, err := tx.Exec(ctx, `insert into replay_arrivals (issue, generation, arrived_at)
		select key, generation, moved_at from issues where key = any($1)
		on conflict (issue, generation) do nothing`, keys); err != nil {
		return fmt.Errorf("keep the arrivals: %w", err)
	}
	return nil
}

// keepArrival keeps at as issue's arrival in the line in the generation it stands at, where the
// table holds none: the start state's arrival of an issue, as the record's entry of the run it
// stands in shows it (driver.arrivalBefore), which the issue's record, written at the window's
// start, does not carry.
func keepArrival(ctx context.Context, tx pgx.Tx, issue record.Issue, at time.Time) error {
	if _, err := tx.Exec(ctx, `insert into replay_arrivals (issue, generation, arrived_at) values ($1, $2, $3)
		on conflict (issue, generation) do nothing`, issue.Key, int64(issue.Generation), at); err != nil {
		return fmt.Errorf("keep the arrival of %s: %w", issue.Key, err)
	}
	return nil
}

// readGenerationTimes is the moment table's column holds for each of issues in the generation it
// stands at, by key, matched to each issue's generation in the query; an issue the table holds no
// row of at that generation has none. The table and the column are the replay's own names, never
// an input's.
func readGenerationTimes(ctx context.Context, tx pgx.Tx, table, column string, issues []record.Issue) (map[string]time.Time, error) {
	times := make(map[string]time.Time)
	if len(issues) == 0 {
		return times, nil
	}
	keys := make([]string, 0, len(issues))
	generations := make([]int64, 0, len(issues))
	for _, issue := range issues {
		keys, generations = append(keys, issue.Key), append(generations, int64(issue.Generation))
	}
	rows, err := tx.Query(ctx, `select t.issue, t.`+pgx.Identifier{column}.Sanitize()+` from `+pgx.Identifier{table}.Sanitize()+` t
		join unnest($1::text[], $2::bigint[]) as i(issue, generation) on i.issue = t.issue and i.generation = t.generation`, keys, generations)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var at time.Time
		if err := rows.Scan(&key, &at); err != nil {
			return nil, err
		}
		times[key] = at.UTC()
	}
	return times, rows.Err()
}

// KeepCovers makes covers the table's covers: the ones that ended go and the new ones are written,
// in one statement.
func (ruleTables) KeepCovers(ctx context.Context, tx pgx.Tx, covers map[string]string) error {
	if _, err := tx.Exec(ctx, `delete from replay_covers`); err != nil {
		return fmt.Errorf("clear the covers: %w", err)
	}
	if len(covers) == 0 {
		return nil
	}
	waiters := slices.Sorted(maps.Keys(covers))
	holders := make([]string, len(waiters))
	for i, waiter := range waiters {
		holders[i] = covers[waiter]
	}
	if _, err := tx.Exec(ctx, `insert into replay_covers (waiter, holder) select * from unnest($1::text[], $2::text[])`, waiters, holders); err != nil {
		return fmt.Errorf("keep the covers %v of %v: %w", holders, waiters, err)
	}
	return nil
}
