package workflow

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// ClearGeneration empties the facts one generation of issue owns: a merged or closed pull
// request, the last generation's reading of a still-open one (the next generation runs on the
// same branch and pull request), its design gates, and each role's handoff, review rounds,
// verdict and summary. Each role keeps its claim and the handoff it last named, so a commit an
// earlier generation reported is never new again. If the clear deletes gate rows of issue, the
// empty set of design-gate documents is queued for it, due at now, because the set Dispatch
// holds for it is the one those rows recorded. issue is the record as tx holds it: the decisions
// asked first are those its tree keeps on it.
//
// It and ClearTreeGeneration are the only way to clear a generation: one issue's, for an adopted
// member and a re-entered child, or a whole tree's, for a re-admitted root. Each forgets before
// it clears, because the clear deletes the design gates, and a gate is the only record of the
// approval that superseded a decision kept for the tree. A start or catch-up that names the
// decision after the clear asks a gate that is gone or new, and the decision would read as the
// person's word again: a person who requested changes and then approved the latest version as
// their hand-over would send the new architect to revise the spec they approved. So every
// decision kept on a cleared issue is asked while its gates still stand, and one a later
// approval superseded is deleted (ForgetSuperseded). A row a queued task holds is deleted too,
// since that task is asked again at its send (the daemon's rootTask) and its gates are gone by
// then. A row that an outbox row claims stays (record.Store.ForgetKeptDecisions): its text is
// already written, and that row settles it.
//
// The statements of the clear are unexported in this package, not methods of record.Store, so
// that no clear can skip the forget: a caller in another package has only these two methods, and
// the compiler refuses any other route.
func (e *Engine) ClearGeneration(ctx context.Context, tx pgx.Tx, issue record.Issue, now time.Time) error {
	describe := fmt.Sprintf("clear the generation of %s", issue.Key)
	if err := e.forgetBeforeClear(ctx, tx, issue.Tree, issue.Key, describe); err != nil {
		return err
	}
	return clearGeneration(ctx, tx, e.store, "issue = $1", issue.Key, describe, now)
}

// ClearTreeGeneration empties them for every issue of tree, and forgets first as ClearGeneration
// says. A root set back to todo starts a new generation of the whole tree, and its children
// carried the old one's pull request, gate and handoffs into it — a child re-entered at the new
// generation read a handoff of the last.
func (e *Engine) ClearTreeGeneration(ctx context.Context, tx pgx.Tx, tree string, now time.Time) error {
	describe := fmt.Sprintf("clear the generation of tree %s", tree)
	if err := e.forgetBeforeClear(ctx, tx, tree, "", describe); err != nil {
		return err
	}
	return clearGeneration(ctx, tx, e.store, "issue in (select key from issues where tree = $1)", tree, describe, now)
}

// forgetBeforeClear deletes in tx each decision tree keeps on issue that a later approval
// superseded (ForgetSuperseded), or each one it keeps on any of its issues when issue is "". Only
// the cleared issues' decisions are asked: a decision is asked of the gates of the issue it was
// made on, and every other issue keeps its gates.
func (e *Engine) forgetBeforeClear(ctx context.Context, tx pgx.Tx, tree, issue, describe string) error {
	decisions, err := e.store.ParkedDecisions(ctx, tx, tree)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	var owed record.KeptDecisions
	for _, d := range decisions {
		if issue == "" || d.Issue == issue {
			owed.Decisions = append(owed.Decisions, record.DecisionMark{Ref: d.Ref, At: d.At, Issue: d.Issue,
				Decision: d.Decision, Actor: d.Actor, Seq: d.Seq})
		}
	}
	if _, err := ForgetSuperseded(ctx, tx, e.store, e.cfg.Steps, tree, owed); err != nil {
		return fmt.Errorf("%s: forget what a later approval superseded: %w", describe, err)
	}
	return nil
}

// clearGeneration runs each statement with key as $1, where selecting the cleared issues' rows by
// their issue column. The design gates go with them, in a statement of their own (clearGates).
func clearGeneration(ctx context.Context, tx pgx.Tx, store record.Store, where, key, describe string, now time.Time) error {
	for _, statement := range []string{
		"delete from pull_requests where " + where + " and state <> 'open'",
		// An open pull request is kept across a new generation, but the last one's reading of it is
		// not: its checks settlement is of a head nobody has judged since, and the
		// review round's decision goes with the phases below, and so does its head run, whose
		// replay would put the old generation's judgement back. Its pushes stay, each a fact about
		// two commits, and so does its newest review, which orders every review it will have, and
		// the checks its base branch requires, which belong to the base, not the generation. So
		// does conflicting_head, the head the daemon last judged conflicting with its base: like
		// mergeability beside it, it is GitHub's answer about that head and that base, which a new
		// generation of the issue's work does not make false. A head that still does not merge
		// still does not merge, and the count it excuses starts again with the rounds below, so
		// nothing of the old generation's counting survives it.
		"update pull_requests set " +
			"head_run = null, planned_red = false, checked_head = '', failing = '[]'::jsonb, cancelled = '[]'::jsonb, " +
			"check_runs = '[]'::jsonb where " + where,
		// The entry's task goes with the run, and the entry itself stays where it stands: the
		// number is never reset, so a worker still holding the old entry's task is behind the row
		// rather than level with it, and the re-admitted tree's promotion raises the entry again
		// and writes the task of that entry beside it (admit's startMidPhaseMembers). The time
		// the entry was raised stays with the number it belongs to. The counts go with the
		// rounds: the new generation counts its moves back from nothing, and the cap mark goes
		// with them, so the architect is told again at the new generation's own first crossing of
		// the cap.
		"update phases set handoff_commit = '', handoff_record = '', handoff_version = 0, rounds = 0, conflict_rounds = 0, " +
			"backward_moves = '{}'::jsonb, cap_told = 0, verdict = '', summary = '', decision = null, " +
			"entry_task = '' where " + where,
	} {
		if _, err := tx.Exec(ctx, statement, key); err != nil {
			return fmt.Errorf("%s: %w", describe, err)
		}
	}
	return clearGates(ctx, tx, store, where, key, describe, now)
}

// clearGates deletes the design gates of every issue the clear covers and queues one publication
// of the empty set for each issue it emptied: those gates recorded a document until this
// statement, and Dispatch holds the set they recorded until something tells it otherwise. The
// delete names the issues it emptied rather than the clear naming every issue it covers, so an
// issue that held no gate row — the ordinary child of a re-entry, and every member of a
// re-admitted tree whose architect registered nothing — costs no Dispatch write. A row comes back
// for each step, so an issue gated on several steps is published once.
//
// The publication is queued through the store, which knows nothing of the lanes beside it, so no
// lane is woken for it: the row waits on its issue's board lane for the next sweep, among the
// status writes the clearing pass queues with it.
func clearGates(ctx context.Context, tx pgx.Tx, store record.Store, where, key, describe string, now time.Time) error {
	rows, err := tx.Query(ctx, "delete from design_gates where "+where+" returning issue", key)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	emptied, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	slices.Sort(emptied)
	for _, issue := range slices.Compact(emptied) {
		if err := store.EnqueueDesignGates(ctx, tx, record.DesignGatesPublish{Issue: issue, Gates: record.GateDocuments(nil)}, now); err != nil {
			return fmt.Errorf("%s: %w", describe, err)
		}
	}
	return nil
}
