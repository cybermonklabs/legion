package blockers

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// AdmissionJudgement is one judgement of a project's admission (JudgeAdmission): the block the
// state route serves under `admission` and a state_admission event carries, beside the parts of the
// judgement the route reads again for each issue's own view — the line each member's
// `waitingLine` is read from, and the mirror rows and the facts each issue's `why` is read from.
type AdmissionJudgement struct {
	Block record.AdmissionBlock
	Line  []record.LineEntry
	Rows  map[string]Row
	Facts record.LineFacts
}

// JudgeAdmission is project's admission block, judged from one read of tx: the project's live
// records (record.Store.LiveIssues), its own slots among slots, every project's as the caller read
// them in tx (record.Store.Slots, record.OwnSlots), the pass's facts (record.ReadLineFacts, the
// Dispatch chains among them) under the design-gate steps the deployment gates, and the line those
// leave (JudgeLine), from which AdmissionOf builds the block under cap. It reads no clock: the line
// is judged from the records and the chains alone.
//
// It is the state route's judgement of the block, for the whole state (projection.Project) and for
// the observed view (projection.Observe) alike; AdmissionOf says which judgement each reader of
// the block builds it over. The admission pass reads the live records too (admit's reread), which
// leave out a done record holding no slot, so the route judges the block from that read as well,
// and the two cannot differ for any record of the project. The records a live read leaves out
// still reach the judgement where it needs them: an issue on a chain is read by the chain read of
// ReadLineFacts (record.ReadChains), whatever its phase.
func JudgeAdmission(ctx context.Context, tx pgx.Tx, store record.Store, project string, steps []record.DesignStep,
	slots []record.Slot, cap int,
) (AdmissionJudgement, error) {
	issues, err := store.LiveIssues(ctx, tx, project)
	if err != nil {
		return AdmissionJudgement{}, fmt.Errorf("judge the admission of %s: %w", project, err)
	}
	facts, err := record.ReadLineFacts(ctx, tx, store, project, issues, steps)
	if err != nil {
		return AdmissionJudgement{}, fmt.Errorf("judge the admission of %s: %w", project, err)
	}
	own := record.OwnSlots(issues, slots)
	rows, facts, line, dropped, err := JudgeLine(ctx, tx, store, project, issues, own, facts, nil)
	if err != nil {
		return AdmissionJudgement{}, fmt.Errorf("judge the admission of %s: %w", project, err)
	}
	return AdmissionOf(issues, own, rows, facts, line, dropped, cap), nil
}

// AdmissionOf is the admission judgement of one judgement of the line (JudgeLine), which reads
// nothing more: issues are the project's live records and slots the project's own among them
// (record.OwnSlots), both as the line was judged from them, and rows, facts, line and dropped are
// what JudgeLine answered over them. Under cap: `p0Holders`, the holders of those slots admission
// treats as P0, counted as the cap's readers count them (record.HoldersOf); `active`, the issues
// holding those slots in slot order; `waiting`, the line in its order; `trees`, the roots in
// Legion's work by key (R22 of dispatch://INFRA-3320): each record in Legion's work with no issue
// above it in work on a known Dispatch chain (record.InLegionsWork, record.Chains.IsRoot);
// `blocked`, what waits on another issue (Blocked); and `held`, what the daemon holds in Legion's
// work and runs nothing of (record.HeldList). Both readers of the block build it with this
// function. The state route builds it over the JudgeLine of JudgeAdmission. The admission pass
// builds it over its final judgement (admit's finalJudgement), so the block costs no judgement of
// its own where the pass's last judgement still stands.
func AdmissionOf(issues []record.Issue, slots []record.Slot, rows map[string]Row, facts record.LineFacts,
	line []record.LineEntry, dropped []record.HeldIssue, cap int,
) AdmissionJudgement {
	active := make([]string, len(slots))
	for i, slot := range slots {
		active[i] = slot.Issue
	}
	waiting := make([]string, len(line))
	for i, entry := range line {
		waiting[i] = entry.Issue.Key
	}
	trees := make([]string, 0, len(issues))
	for _, issue := range issues {
		if record.InLegionsWork(issue) && facts.Chains.IsRoot(issue.Key) {
			trees = append(trees, issue.Key)
		}
	}
	slices.Sort(trees)
	return AdmissionJudgement{
		Block: record.AdmissionBlock{
			Cap:       cap,
			P0Holders: record.HoldersOf(issues, slots).P0,
			Active:    active,
			Waiting:   waiting,
			Trees:     trees,
			Blocked:   Blocked(rows, issues, line, facts),
			Held:      record.HeldList(issues, slots, line, dropped, facts),
		},
		Line:  line,
		Rows:  rows,
		Facts: facts,
	}
}
