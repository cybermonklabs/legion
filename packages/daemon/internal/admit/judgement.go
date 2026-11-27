package admit

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// passRead is one read of what an admission pass judges (reread): the project's live records,
// every project's slots and the line facts over those records, beside the transaction it was taken
// in and how many rows that transaction had written to the tables a judgement reads when the read
// was taken (record.Store.JudgedWrites), with counted false where Postgres kept no such count. The
// count is taken before the read, so a write anywhere in the transaction after it, by any code,
// makes a later count differ; and a count is the transaction's own, so a read of another
// transaction never stands in this one.
type passRead struct {
	tx      pgx.Tx
	writes  int64
	counted bool
	issues  []record.Issue
	slots   []record.Slot
	facts   record.LineFacts
}

// passJudgement is a pass's read (passRead) and, when judged is set, the judgement of the line the
// pass made over it (blockers.JudgeLine): its mirror rows, its facts as the judgement filled them,
// its line and the rows the line left behind. The read's slots are every project's; the line was
// judged over the project's own among them (record.OwnSlots).
type passJudgement struct {
	passRead
	judged      bool
	rows        map[string]blockers.Row
	judgedFacts record.LineFacts
	line        []record.LineEntry
	dropped     []record.HeldIssue
}

// readPass is reread with the transaction's count of the rows a judgement reads taken first
// (record.Store.JudgedWrites), and it is what every pass reads its records through, so the pass's
// end can tell whether that read still stands (finalJudgement).
func (a *Admission) readPass(ctx context.Context, tx pgx.Tx) (passRead, error) {
	writes, counted, err := a.store.JudgedWrites(ctx, tx)
	if err != nil {
		return passRead{}, err
	}
	issues, slots, facts, err := a.reread(ctx, tx)
	if err != nil {
		return passRead{}, err
	}
	return passRead{tx: tx, writes: writes, counted: counted, issues: issues, slots: slots, facts: facts}, nil
}

// judged records, as this pass's last judgement, the judgement of the line JudgeLine answered over
// read (rows, facts, line and dropped), and answers it.
func (a *Admission) judged(read passRead, rows map[string]blockers.Row, facts record.LineFacts, line []record.LineEntry, dropped []record.HeldIssue) *passJudgement {
	a.last = &passJudgement{passRead: read, judged: true, rows: rows, judgedFacts: facts, line: line, dropped: dropped}
	return a.last
}

// judge judges the line over read (blockers.JudgeLine, under this pass's clock) and records the
// judgement as this pass's last (judged).
func (a *Admission) judge(ctx context.Context, tx pgx.Tx, read passRead) (*passJudgement, error) {
	rows, facts, line, dropped, err := blockers.JudgeLine(ctx, tx, a.store, a.project, read.issues, record.OwnSlots(read.issues, read.slots), read.facts, nil, a.now())
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
func (a *Admission) finalJudgement(ctx context.Context, tx pgx.Tx) (*passJudgement, error) {
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
func (j *passJudgement) admission(cap int) blockers.AdmissionJudgement {
	return blockers.AdmissionOf(j.issues, record.OwnSlots(j.issues, j.slots), j.rows, j.judgedFacts, j.line, j.dropped, cap)
}
