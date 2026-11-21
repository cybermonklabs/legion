package replay

import "github.com/sjawhar/legion/daemon/internal/record"

// RowKind is one kind of outbox row as the replay's worker stub tells them apart: its
// record.OutboxKind and, for a supervise row, its op, since a start, a suspend and a close of a
// role are three different acts on the issue's work.
type RowKind struct {
	Kind record.OutboxKind
	Op   record.SuperviseOp
}

// OutboxWay is what the replay's worker stub does with one kind of outbox row, which in a daemon
// an executor runs against something outside it: a worker, Dispatch, GitHub.
type OutboxWay string

const (
	// OutboxExecuted is a row the worker stub executes as the daemon's executor would, against
	// the replay's own state: a start opens the issue's next recorded segment, a stop pauses or
	// cancels it, a status write becomes the Dispatch event its executor's write makes, and a row
	// whose executor applies a fact back into the daemon applies that fact through the chain at
	// the row's own time.
	OutboxExecuted OutboxWay = "executed"
	// OutboxFinished is a row the worker stub finishes without acting, under the assumption
	// RowHandling.Assumption names: its executor changes nothing the replay decides or measures.
	OutboxFinished OutboxWay = "finished"
)

// RowHandling is what the worker stub does with one kind of outbox row: whether it executes the
// row or finishes it, what executing it does and the facts the daemon's executor of it applies
// back into the daemon, or the assumption a finished row rests on.
type RowHandling struct {
	// Way is whether the stub executes the row or finishes it.
	Way OutboxWay
	// Does is what the stub does to execute the row; empty for a row it finishes.
	Does string
	// Facts is every fact, by its type's name in package intake, the daemon's executor of the row
	// applies back into the daemon and the stub applies in its place; none for a row whose
	// executor applies none.
	Facts []string
	// Assumption is the number of the assumption (Assumptions) a finished row rests on; zero for
	// a row the stub executes.
	Assumption int
}

// OutboxHandling is what the replay's worker stub does with each kind of outbox row: every
// record.OutboxKind, and a supervise row by each of its ops. A row of a kind with no entry here
// is neither executed nor finished under an assumption, and the inventory test
// (TestEveryOutboxKindIsExecutedOrFinishedUnderAnAssumption) names it.
var OutboxHandling = map[RowKind]RowHandling{}
