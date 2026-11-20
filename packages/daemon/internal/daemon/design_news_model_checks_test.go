package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The checks of TestTheNewsRecordHoldsToWhatTheHolderWasReallyTold (design_news_model_test.go):
// what the model says the claim store credits each told row with, read from the writes the store
// let through, and the rules each told row is held to after every step.

// credits is what the copy carried credits row with, by the model's rule: each item to row as a
// link, and each opening it says opened, by its number, counting only what was rendered for the run
// the issue stands in now (design_news_current).
func (m *newsModel) credits(row *newsModelRow, carried []byte) ([]newsModelLink, map[int]bool) {
	m.t.Helper()
	var news record.DeliveryNews
	if err := json.Unmarshal(carried, &news); err != nil {
		m.fail("decode the copy %s a delivery of the %s holds: %v", carried, row.name, err)
	}
	gen, treeGen := m.gens(row)
	var links []newsModelLink
	for _, item := range news.Items {
		if item.Issue == row.issue && item.Audience == row.audience && item.Step == record.DesignStepSpec &&
			item.Generation == gen && item.TreeGeneration == treeGen {
			links = append(links, newsModelLinkOf(item))
		}
	}
	opens := map[int]bool{}
	for _, opening := range news.Openings {
		if opening.Issue == row.issue && opening.Audience == row.audience && !opening.Closed &&
			opening.Generation == gen && opening.TreeGeneration == treeGen {
			opens[opening.Seq] = true
		}
	}
	return links, opens
}

// standing is the credit row's claims hold in the claim store as stored reads it (storedClaims):
// the copy each confirmed delivery whose turn's result has not said it ran holds (credits).
func (m *newsModel) standing(row *newsModelRow, stored map[claim.Token]*supervise.Delivery) ([]newsModelLink, map[int]bool) {
	m.t.Helper()
	var links []newsModelLink
	opens := map[int]bool{}
	for _, a := range row.agents {
		if a.machine == nil {
			continue
		}
		d := stored[a.token]
		if d == nil || d.ConfirmedAt.IsZero() || d.Ran || len(d.Carried) == 0 {
			continue
		}
		credited, opened := m.credits(row, d.Carried)
		links = append(links, credited...)
		maps.Copy(opens, opened)
	}
	return links, opens
}

// owesOpening says whether row owes the opening that stands, by the model's rule: no role message
// told it, no final credit's fold settled it, and no standing credit (opens) carried it.
func (m *newsModel) owesOpening(row *newsModelRow, opens map[int]bool) bool {
	return !row.told[row.openSeq] && !row.cleared[row.openSeq] && !opens[row.openSeq]
}

// drain reads every write of a delivery the claim store took since the last drain, in order. A
// write whose copy is new is the acknowledgement of a prompt, and the copy is held to that prompt's
// text (invariant 1). A write of a confirmed delivery whose turn's result said it ran folds its copy
// into the row its claim credits (the claim store's settleDesignNews): each credit of the run the
// issue stands in is final, and an opening it says opened is settled where the row still holds that
// number (design_news_opening_told).
func (m *newsModel) drain() {
	m.t.Helper()
	for _, w := range m.claims.drain() {
		a := m.agentOf(w.token)
		if a == nil {
			continue
		}
		if w.delivery == nil {
			a.carried = nil
			continue
		}
		d, row := *w.delivery, a.row
		if len(d.Carried) > 0 && !bytes.Equal(d.Carried, a.carried) {
			if p := row.prompts[d.ID]; p != nil {
				if why := m.namesExactly(row, p, d.Carried); why != "" {
					m.fail("invariant 1: the copy the claim store holds of the %s's prompt %s, which its confirmation credits, is not the news that prompt carried: %s",
						a.name, d.ID, why)
				}
			}
		}
		a.carried = d.Carried
		if d.ConfirmedAt.IsZero() || !d.Ran || len(d.Carried) == 0 {
			continue
		}
		a.finalized[d.ID] = true
		links, opens := m.credits(row, d.Carried)
		row.final = append(row.final, links...)
		if opens[row.openSeq] {
			row.cleared[row.openSeq] = true
		}
	}
}

// truth is the version of the spec the model says row's holder took of prompts in the run its
// issue stands in: from the version that run started from (base), through every pair they told,
// and, where closed says so, through every version since that changed only decision blocks, which a
// holder knows without being told. It is what the record is held never to pass with the prompts
// the holders were told (closed), and to reach with the prompts whose credit stands (not closed).
func (m *newsModel) truth(row *newsModelRow, prompts []*newsModelPrompt, closed bool) int {
	var links []newsModelLink
	for _, p := range prompts {
		links = append(links, p.pairs...)
	}
	took := row.base
	for moved := true; moved; {
		moved = false
		if reached, _ := newsModelClosure(took, links); reached != took {
			took, moved = reached, true
		}
		for closed && took < m.latest && m.decisionOnly[took+1] {
			took, moved = took+1, true
		}
	}
	return took
}

// owes says whether row's holders have not been told the opening that stands by the role messages
// or by any of prompts.
func (m *newsModel) owes(row *newsModelRow, prompts []*newsModelPrompt) bool {
	if row.told[row.openSeq] {
		return false
	}
	for _, p := range prompts {
		if p.opening == row.openSeq {
			return false
		}
	}
	return true
}

// lagging says whether the claim store, as stored reads it (storedClaims), holds a's delivery
// otherwise than its machine does: a refused write the machine still owes. A restart does not end
// that. The booted machine reads the stored delivery, so the two agree, yet what the stopped
// machine had decided of it in memory — a provider error's take-back, say — was never written, and
// the booted machine's own first write of it can be refused too. So a store that lagged at a
// restart lags until the stored delivery is another than it was then (behind).
func (m *newsModel) lagging(a *newsModelAgent, stored map[claim.Token]*supervise.Delivery) bool {
	m.t.Helper()
	if a.machine == nil {
		return false
	}
	held := storedFrom(stored[a.token])
	if a.behind != nil {
		if held == *a.behind {
			return true
		}
		a.behind = nil
	}
	return held != storedFrom(a.machine.Claim().Pending)
}

// check reads every write the claim store took since the last check and holds each told row, as
// every reader reads it, to the model (checkRow), counting the step ambiguous where a claim store
// lags its machine. It reads the claim store and the told rows once, after the writes: nothing moves
// them while it checks. A prompt whose turn runs while its claim holds another task, or none, was
// given back: a newer task took its place, which ends its credit, and its turn's result, when it
// comes, is another delivery's. Only the steps since the last check that make such a replacement
// can have made it (replaced), and any other fails the run: a rule that keeps a running turn's task
// in place (supervise's Machine.queue) is broken.
func (m *newsModel) check() {
	m.t.Helper()
	m.drain()
	for _, a := range m.agents() {
		if a.turn == "" || a.machine == nil || a.superseded[a.turn] {
			continue
		}
		p := a.machine.Claim().Pending
		if p != nil && p.ID == a.turn {
			continue
		}
		if !m.replaced(a, p) {
			held := "nothing"
			if p != nil {
				held = fmt.Sprintf("task %s of entry %d (news alone %t)", p.ID, p.Entry, p.NewsOnly)
			}
			m.fail("the %s's claim holds %s while the turn of %s runs, and no step since the last check makes that replacement",
				a.name, held, a.turn)
		}
		a.superseded[a.turn] = true
	}
	m.superseding, m.entryStarts = "", false
	_, stored := m.storedClaims()
	told := m.toldRows()
	ambiguous := false
	for _, row := range m.rows() {
		if m.checkRow(row, stored, told[toldKey{row.issue, row.audience}]) {
			ambiguous = true
		}
	}
	m.ambiguity.step(ambiguous)
	m.ran["checks"]++
}

// replaced says whether the steps since the last check can have put pending, the task a's claim
// holds (nil for none), in the place of the task whose turn a's agent runs. Two things do. The
// supersede step's newer task leaves the reviewer's claim holding that task, or none where the
// retirement landed and the write of the task was refused. The start of a newer entry, which a
// re-entry, a re-admission, a hand-over or the sweep raises (newsModelEntryStarts), leaves the
// claim holding a task of an entry above the one whose turn runs, or none on the same refusal. A
// task of news alone carries no entry, so it never takes that place.
func (m *newsModel) replaced(a *newsModelAgent, pending *supervise.Delivery) bool {
	if a == m.reviewer && m.superseding != "" && (pending == nil || pending.ID == m.superseding) {
		return true
	}
	if !m.entryStarts {
		return false
	}
	var entry int64
	if turn := a.row.prompts[a.turn]; turn != nil {
		entry = turn.entry
	}
	return pending == nil || pending.Entry > entry
}

// checkRow holds row's told row, told, to the model, with the claim store as stored reads it, and
// answers whether the claim store lags the machine of a claim that credits it.
//
// After a re-admission of the tree nothing says what the child's holder stood on, and the row knows
// no version, until the reconcile reads the review history: the version approved before the one
// that stands, which is the one below it (newNewsModel). The model takes that as the run's base at
// the reconcile that reads it (settled), and holds the row to it from that step on.
func (m *newsModel) checkRow(row *newsModelRow, stored map[claim.Token]*supervise.Delivery, told record.DesignTold) bool {
	m.t.Helper()
	took, known := told.Took(record.DesignStepSpec)
	lagging := false
	for _, a := range row.agents {
		lagging = m.lagging(a, stored) || lagging
	}
	m.settleFinals(row, lagging)
	links, opens := m.standing(row, stored)
	credited := append(slices.Clone(row.final), links...)
	if row.base != newsModelUnknown && !known {
		m.fail("exactly: the %s's row knows no version of the spec, but its run started from version %d: %+v", row.name, row.base, told)
	}
	if upper := m.truth(row, row.toldPrompts(lagging), true); known && row.base != newsModelUnknown && took > upper {
		m.fail("never ahead of what it was told: the record says the %s took version %d, but it was told no further than %d", row.name, took, upper)
	}
	if want, wantKnown := newsModelClosure(row.base, credited); known != wantKnown || (known && took != want) {
		m.fail("exactly: the record says the %s took %s, but what the claim store credits gives %s: from %s through the credits %v (final %v, standing %v)",
			row.name, versionWords(took, known), versionWords(want, wantKnown), versionWords(row.base, row.base != newsModelUnknown), credited, row.final, links)
	}
	if !lagging && row.base != newsModelUnknown {
		if lower := m.truth(row, row.creditedPrompts(), false); took < lower {
			m.fail("level with what it read: the record says the %s took version %d, but it read the news up to %d", row.name, took, lower)
		}
	}
	if !told.OpeningOwed && m.owes(row, row.toldPrompts(lagging)) {
		m.fail("never ahead of what it was told: the record owes the %s no opening, but no message or read prompt told it opening %d", row.name, row.openSeq)
	}
	if want := m.owesOpening(row, opens); told.OpeningOwed != want {
		m.fail("exactly: the record owes the %s the opening %d: %t, but the role messages and what the claim store credits say %t (told by a message %t, settled by a final credit %t, carried by a standing credit %t)",
			row.name, row.openSeq, told.OpeningOwed, want, row.told[row.openSeq], row.cleared[row.openSeq], opens[row.openSeq])
	}
	if !lagging {
		if owed := m.owes(row, row.creditedPrompts()); told.OpeningOwed != owed {
			m.fail("level with what it read: the record owes the %s the opening %d: %t, but by the messages and the prompts whose credit stands it is %t",
				row.name, row.openSeq, told.OpeningOwed, owed)
		}
	}
	return lagging
}

// settleFinals settles each prompt of row's claims whose turn's result came and whose final credit
// the claim store has not landed (ranAt): once the store has landed it, or the prompt is of a run
// that ended, there is nothing to wait for; while the store lags a machine, the write that makes it
// final may still come; and once the store has caught up without it, a refused write of that claim
// since that result lost it (lost). Only the reviewer's writes are ever refused
// (newsModelStore.refusing), so a credit of any other claim the store never made final, and one of
// the reviewer's with no write refused since its result, fails the run.
func (m *newsModel) settleFinals(row *newsModelRow, lagging bool) {
	m.t.Helper()
	for _, a := range row.agents {
		for _, id := range slices.Sorted(maps.Keys(a.ranAt)) {
			spent := a.ranAt[id]
			switch p := row.prompts[id]; {
			case a.finalized[id] || p == nil || p.run != row.run:
				delete(a.ranAt, id)
			case lagging:
			case a.token == m.claims.refusing && m.claims.spentCount() > spent:
				a.lost[id] = true
				delete(a.ranAt, id)
			default:
				m.fail("level with what it read: the %s's turn on %s ran and its result came, but the claim store never made its credit final, and none of its writes was refused since",
					a.name, id)
			}
		}
	}
}
