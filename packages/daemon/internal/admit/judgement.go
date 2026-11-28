package admit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// passRead is one read of what an admission pass judges (readPass): the project's live records,
// every project's slots and the line facts over those records, beside the transaction it was taken
// in and how many rows that transaction had written to the tables a judgement reads once the read
// finished (record.Store.JudgedWrites), with counted false where Postgres kept no such count. The
// count is taken right after readPass marks the roots, whose one write — marking roots that hold
// their own gate (MarkHoldsGate) — changes no judgement (Chains.HoldsOwnGate answers the same for a
// root marked or not), so the pass's own mark sits in the count's baseline while any later write in
// the transaction, by any code, makes a later count differ; and a count is the transaction's own, so
// a read of another transaction never stands in this one.
type passRead struct {
	tx      pgx.Tx
	writes  int64
	counted bool
	issues  []record.Issue
	slots   []record.Slot
	facts   record.LineFacts
}

// lineJudgement is a pass's read (passRead) and, when judged is set, the judgement of the line the
// pass made over it (blockers.JudgeLine): its mirror rows, its facts as the judgement filled them,
// its line and the rows the line left behind. The read's slots are every project's; the line was
// judged over the project's own among them (record.OwnSlots).
type lineJudgement struct {
	passRead
	judged      bool
	rows        map[string]blockers.Row
	judgedFacts record.LineFacts
	line        []record.LineEntry
	dropped     []record.HeldIssue
}

// readPass is what every pass reads its records through: the pass's shared reads (readShared),
// then the mark of every issue in Legion's work they find a root as holding its own gate, then the
// transaction's count of the rows a judgement reads (record.Store.JudgedWrites), taken after the
// mark, so the pass's end can tell whether that read still stands (finalJudgement) without the
// pass's own root mark (MarkHoldsGate) making the judgement look stale.
func (a *Admission) readPass(ctx context.Context, tx pgx.Tx) (passRead, error) {
	issues, slots, facts, err := a.readShared(ctx, tx)
	if err != nil {
		return passRead{}, err
	}
	// Every recorded issue in Legion's work that the pass finds a root holds its own gate from now
	// on, and keeps it once it gets a parent in Legion's work (R15 of dispatch://INFRA-3320,
	// record.Chains.HoldsOwnGate). An issue out of Legion's work is marked nothing, though nothing
	// above it may be in work either: the mark outlives the stop, so a member stopped with its root
	// would come back beneath that root holding a gate it never held as a root. One statement marks
	// every root the read finds unmarked, and none is sent when there is none.
	var roots []string
	for _, issue := range issues {
		if !issue.HoldsGate && record.InLegionsWork(issue) && facts.Chains.IsRoot(issue.Key) {
			roots = append(roots, issue.Key)
		}
	}
	if len(roots) > 0 {
		if err := a.store.MarkHoldsGate(ctx, tx, roots); err != nil {
			return passRead{}, fmt.Errorf("mark the roots that hold their own gate: %w", err)
		}
	}
	writes, counted, err := a.store.JudgedWrites(ctx, tx)
	if err != nil {
		return passRead{}, err
	}
	return passRead{tx: tx, writes: writes, counted: counted, issues: issues, slots: slots, facts: facts}, nil
}

// judged records, as this pass's last judgement, the judgement of the line JudgeLine answered over
// read (rows, facts, line and dropped), and answers it.
func (a *Admission) judged(read passRead, rows map[string]blockers.Row, facts record.LineFacts, line []record.LineEntry, dropped []record.HeldIssue) *lineJudgement {
	a.last = &lineJudgement{passRead: read, judged: true, rows: rows, judgedFacts: facts, line: line, dropped: dropped}
	return a.last
}

// judge judges the line over read (blockers.JudgeLine) and records the judgement as this pass's
// last (judged).
func (a *Admission) judge(ctx context.Context, tx pgx.Tx, read passRead) (*lineJudgement, error) {
	rows, facts, line, dropped, err := blockers.JudgeLine(ctx, tx, a.store, a.project, read.issues, record.OwnSlots(read.issues, read.slots), read.facts, nil)
	if err != nil {
		return nil, err
	}
	return a.judged(read, rows, facts, line, dropped), nil
}

// finalJudgement is the judgement of the project's admission over the transaction's records as
// they stand now, after every write the transaction has made so far, the workflow handler's ahead
// of this pass and the pass's own promotions and releases among them. It is the pass's last
// judgement (last) where the transaction has written no row a judgement reads since the read that
// judgement was made over: both counts are ok and equal (record.Store.JudgedWrites answers the
// count that read was taken at). Then a fresh read would read the same rows and a fresh judgement
// judge them alike, so the pass judges nothing again. Where the pass's last read stands and it
// made no judgement over it (the entry sweep's), it judges that read and reads the project's
// records no second time, though the judgement still reads the Dispatch mirror's facts
// (blockers.JudgeLine). Otherwise, and wherever Postgres keeps no counts, it reads the project
// again and judges that read. Either way the answer is recorded as the pass's last judgement, so a
// second caller at the pass's end gets the same judgement.
//
// The rows a pass writes after this — the block's own row and its state event, a controller
// notice — are no input of any judgement: the block's table is not among the tables a judgement
// reads, and the only outbox rows a judgement reads are the starts the project owes
// (record.Store.OwedStarts), which no such write is. That is why the pass's end judges once and
// hands the one judgement to each of its readers (endPass) rather than asking this again after
// each write.
func (a *Admission) finalJudgement(ctx context.Context, tx pgx.Tx) (*lineJudgement, error) {
	if last := a.last; last != nil && last.tx == tx && last.counted {
		writes, counted, err := a.store.JudgedWrites(ctx, tx)
		if err != nil {
			return nil, err
		}
		if counted && writes == last.writes {
			if last.judged {
				return last, nil
			}
			return a.judge(ctx, tx, last.passRead)
		}
	}
	read, err := a.readPass(ctx, tx)
	if err != nil {
		return nil, err
	}
	return a.judge(ctx, tx, read)
}

// admission is the admission judgement of j under cap (blockers.AdmissionOf): the block and its
// parts, read from j alone.
func (j *lineJudgement) admission(cap int) blockers.AdmissionJudgement {
	return blockers.AdmissionOf(j.issues, record.OwnSlots(j.issues, j.slots), j.rows, j.judgedFacts, j.line, j.dropped, cap)
}
