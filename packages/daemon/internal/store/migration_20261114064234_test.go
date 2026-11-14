package store

import (
	"context"
	"strings"
	"testing"
)

// gateBasisVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const gateBasisVersion = 20261114064234

// 20261114064234 gives a design-gate step its basis, the person's approval it keeps apart while a
// basis stands and the ancestor it rests on (dispatch://INFRA-3320), and an issue the ancestor its
// spec is a view on. The negative control is the write itself, which names columns the tables do
// not have before the migration. A gate and an issue the earlier release wrote migrate with none
// of it, and a write of an earlier release, which names none of the new columns, is still taken
// on a row that carries them. Each version and sequence refuses zero, as 0055's do, and each read
// by a basis, a pending request or a rest has its index.
func TestTheGateBasisArrivesWith20261114064234(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, gateBasisVersion-1)
	exec := func(statement string) error {
		_, err := store.Pool().Exec(ctx, statement)
		return err
	}
	for _, statement := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-208', 'LEGION-208', 'LEGION', 'admitted root', 'admitted', 1, 'in_progress', 'A', 1)`,
		`insert into design_gates (issue, artifact_id, latest_version, approved_version, approved_seq)
			values ('LEGION-208', '7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26', 4, 2, 9)`,
	} {
		if err := exec(statement); err != nil {
			t.Fatalf("seed the earlier release's rows: %v", err)
		}
	}
	basisWrite := `update design_gates set basis_issue = 'LEGION-100', basis_artifact_id = 'artifact-100', basis_version = 3,
		basis_approved_seq = 31, basis_approved_at = now(), basis_approver = 'user:alice',
		basis_serves = '{dispatch://LEGION-100/spec#b-r1-ancestor@v3}', basis_reason = 'the checks serve r1',
		basis_session = 'ses_architect', basis_registered_at = now(), approved_version = 4, approved_seq = null,
		kept_person_version = 2, kept_person_seq = 9, kept_person_at = now(), rests_on = 'LEGION-100'
		where issue = 'LEGION-208'`
	if err := exec(basisWrite); err == nil || !strings.Contains(err.Error(), "basis_issue") {
		t.Fatalf("a basis written before the migration = %v, want it refused for naming a column the table does not have", err)
	}
	if err := exec(`update issues set view_of = 'LEGION-100' where key = 'LEGION-208'`); err == nil || !strings.Contains(err.Error(), "view_of") {
		t.Fatalf("a view written before the migration = %v, want it refused for naming a column the table does not have", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the gate basis: %v", err)
	}

	var carried int
	if err := store.Pool().QueryRow(ctx, `select count(*) from design_gates where issue = 'LEGION-208'
		and basis_issue is null and basis_artifact_id is null and basis_version is null and basis_approved_seq is null
		and basis_approved_at is null and basis_approver is null and basis_serves is null and basis_reason is null
		and basis_session is null and basis_registered_at is null and basis_pending_ask is null
		and kept_person_version is null and kept_person_seq is null and kept_person_at is null and rests_on is null
		and approved_version = 2 and approved_seq = 9`).Scan(&carried); err != nil || carried != 1 {
		t.Fatalf("the earlier release's gate after the migration = %d rows with no basis and its approval as it was (%v), want 1", carried, err)
	}
	var viewOf *string
	if err := store.Pool().QueryRow(ctx, `select view_of from issues where key = 'LEGION-208'`).Scan(&viewOf); err != nil || viewOf != nil {
		t.Fatalf("the earlier release's issue after the migration is a view on %v (%v), want none", viewOf, err)
	}

	for _, accepted := range []string{
		basisWrite,
		`update design_gates set basis_pending_ask = 'ask-1', basis_approved_seq = null, basis_approver = null where issue = 'LEGION-208'`,
		`update issues set view_of = 'LEGION-100' where key = 'LEGION-208'`,
		// The earlier release's own writes, which name none of the new columns, on a row that
		// carries them: a new version, a changes request, a person's approval, and its upsert.
		`update design_gates set latest_version = 5 where issue = 'LEGION-208'`,
		`update design_gates set approved_version = null where issue = 'LEGION-208'`,
		`update design_gates set approved_version = 5, approved_seq = 50 where issue = 'LEGION-208'`,
		`insert into design_gates (issue, step, artifact_id, latest_version, approved_version) values ('LEGION-208', 'spec', '7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26', 6, null)
			on conflict (issue, step) do update set latest_version = excluded.latest_version, approved_version = excluded.approved_version`,
	} {
		if err := exec(accepted); err != nil {
			t.Fatalf("%s: %v, want it accepted", accepted, err)
		}
	}
	var kept string
	if err := store.Pool().QueryRow(ctx, `select rests_on from design_gates where issue = 'LEGION-208'`).Scan(&kept); err != nil || kept != "LEGION-100" {
		t.Fatalf("the rest after the earlier release's writes = %q (%v), want LEGION-100 left as it was", kept, err)
	}

	for _, refused := range []struct{ why, statement string }{
		{"a basis version of zero", `update design_gates set basis_version = 0 where issue = 'LEGION-208'`},
		{"a basis approval sequence of zero", `update design_gates set basis_approved_seq = 0 where issue = 'LEGION-208'`},
		{"a kept version of zero", `update design_gates set kept_person_version = 0 where issue = 'LEGION-208'`},
		{"a kept sequence of zero", `update design_gates set kept_person_seq = 0 where issue = 'LEGION-208'`},
	} {
		if err := exec(refused.statement); err == nil || !strings.Contains(err.Error(), "check constraint") {
			t.Fatalf("%s = %v, want a check constraint to refuse it", refused.why, err)
		}
	}

	for _, index := range []string{"design_gates_basis_artifact_id", "design_gates_basis_pending_ask", "design_gates_basis_issue", "design_gates_rests_on"} {
		var found int
		if err := store.Pool().QueryRow(ctx, `select count(*) from pg_indexes where tablename = 'design_gates' and indexname = $1`, index).Scan(&found); err != nil || found != 1 {
			t.Fatalf("index %s = %d (%v), want it created", index, found, err)
		}
	}
}
