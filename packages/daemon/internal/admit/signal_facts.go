package admit

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// keepSignalFacts keeps what fact carries of an input of a signal of admission by signals, in the
// fact's own transaction and before the fact's admission pass (Apply), so the pass the fact runs
// reads what it kept, and the fact, the rows it kept and the pass commit together or not at all.
// The fact's processed mark (intake.ApplyFactWith) makes a fact delivered twice a duplicate that
// reaches no handler, and each row is keyed by its source's own identity as well, so a fact is
// kept once however it arrives.
//
//   - intake.MemoryGuardKill: the memory kill of an issue pod of the project, of a role's process or
//     of one of no role, by the guard or by the pod's kernel, kept at the report's own moment
//     (record.Store.KeepMemoryKill), whatever the workflow does with it: the engine runs first and
//     tells the role, or tells no role where the process was of none or the daemon records no such
//     issue, but the kill counts either way, as the issue pod's launcher line of it does.
//     intake.ControllerMemoryGuardKill keeps nothing: a kill in the controller's pod does not
//     count.
//   - intake.DaemonBooted: each boot records that this daemon keeps the kills from now on, where
//     no earlier boot recorded it (record.Store.BeginSignalInput), so the kills signal is recorded
//     from the first boot of the release that keeps them plus the hour it counts over
//     (signals.KillSpan), and not before, where no kill was kept.
func (a *Admission) keepSignalFacts(ctx context.Context, tx pgx.Tx, fact intake.Fact) error {
	switch fact := fact.(type) {
	case intake.MemoryGuardKill:
		return a.store.KeepMemoryKill(ctx, tx, a.project, fact.Report, a.now())
	case intake.DaemonBooted:
		return a.store.BeginSignalInput(ctx, tx, a.project, signals.Kills, a.now())
	}
	return nil
}
