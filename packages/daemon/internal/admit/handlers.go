package admit

import (
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// Handlers is the chain of handlers every fact of project is applied through (intake.ApplyFact),
// in its order: the Dispatch mirror (blockers.NewMirror over records), then the workflow engine,
// then admission. The order is load-bearing. The engine decides whether to start a child and
// admission whether to admit a root inside the fact's one transaction, from the mirror's rows, so
// the row an event carries must already be written when they look; and admission promotes from
// the records the engine has just written.
//
// It is the one statement of the chain: the daemon builds its handlers with it, and so does a
// replay of a recorded window, so the replay runs the daemon's own pass rather than a copy of its
// order.
func Handlers(records record.Store, project string, engine *workflow.Engine, admission *Admission) []intake.Handler {
	return []intake.Handler{blockers.NewMirror(records, project), engine, admission}
}
