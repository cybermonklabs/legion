package startup

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
)

var _ = registerRetirement(decisionsDocumentRetirement)

// decisionsDocumentRetirement deletes what the release that retired each issue's decisions
// document kept for one release: the design_gates rows of the step `decisions`, the function
// decisions_documents() (migration 20261117093337), and the three checks' admission of
// `decisions` - artifacts_role_check (20261105232931), the check on projects.gated_steps
// (20261105204141) and the check on design_gates.step (20261110214252). That release stopped
// writing the value, converted every decisions document into nodes and retired it, and kept these
// for a build of the release before it, which writes the role, the step and the document.
//
// Its guard is StoresMovedKinds, the list of this release (dispatch://INFRA-1098), and not the
// relation kinds that release moved into the store (AsksKinds): a build of that release, and of the
// issue-room release after it (dispatch://INFRA-1253), the release before this one, lists those
// kinds and still calls decisions_documents() - its issue read for decisions_artifact_id and its
// decisions step's tick - so dropping the function under them leaves the build a rollback reaches
// answering 500 on every issue read. A build of this release reads and writes none of these
// objects, and only a build of this release or a later one announces stores_moved, so the first
// boot of a build after this one deletes them.
//
// It deletes only where decisions_documents() answers no document, a document with no role whose
// slug is a decisions document's counting as one, since the function itself counts it: a guard on
// the role alone would drop the function while it still answers a document. Each document it
// answers is a holder. A build of that release writes no such document (409 DECISIONS_ARE_NODES),
// and the first guard keeps out a build of the release before, so no document comes to stand
// between this read and the drop.
//
// It deletes in this order, because each narrowed check refuses what the statements before it
// remove: the design_gates rows of the step first, which the narrowed check on design_gates.step
// would refuse, then the function, then the three checks. A projects row whose gated_steps still
// holds `decisions`, or an artifact whose role is `decisions`, makes its check's narrowing fail,
// and the whole transaction with it.
var decisionsDocumentRetirement = Retirement{
	Name:  "the decisions document's kept objects",
	Kinds: StoresMovedKinds,
	Stands: func(ctx context.Context, q queryer) (bool, error) {
		var stands bool
		err := q.QueryRow(ctx, `select to_regprocedure('decisions_documents()') is not null`).Scan(&stands)
		return stands, err
	},
	Holds: func(ctx context.Context, q queryer) ([]Holder, error) {
		rows, err := q.Query(ctx, `select id::text, issue_key, ref_key from decisions_documents() order by issue_key, ref_key`)
		if err != nil {
			return nil, fmt.Errorf("read the decisions documents that stand: %w", err)
		}
		return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Holder, error) {
			var id, issue, ref string
			if err := row.Scan(&id, &issue, &ref); err != nil {
				return nil, err
			}
			return Holder{slog.String("decisions_document", id), slog.String("issue", issue), slog.String("ref_key", ref)}, nil
		})
	},
	Delete: func(ctx context.Context, tx pgx.Tx) error {
		for _, statement := range []string{
			`delete from design_gates where step = 'decisions'`,
			`drop function decisions_documents()`,
			`alter table artifacts drop constraint artifacts_role_check,
				add constraint artifacts_role_check check (role is null or role in ('verification', 'plan'))`,
			`alter table projects drop constraint projects_gated_steps_check,
				add constraint projects_gated_steps_check check (gated_steps <@ array['criteria', 'spec'])`,
			`alter table design_gates drop constraint design_gates_step_check,
				add constraint design_gates_step_check check (step in ('criteria', 'spec'))`,
		} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return fmt.Errorf("%s: %w", statement, err)
			}
		}
		return nil
	},
}
