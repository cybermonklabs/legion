package store

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// hashGateVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const hashGateVersion = 20261127204213

// releaseBeforePutGate is record.Postgres.PutGate's statement in the release before the hash gate
// (internal/record/store.go at 8d28f3cc), with that commit's column list: the write a daemon rolled
// back past this migration makes of a gate. It names none of the verdict's columns, so it leaves
// them as the row holds them. writeAsTheReleaseBefore gives it that commit's arguments.
const releaseBeforePutGate = `insert into design_gates (issue, step, artifact_id, latest_version, approved_version, approved_seq, approved_at,
		told_changes_version, told_changes_reason, told_changes_seq, told_changes_at, ever_opened,
		basis_issue, basis_artifact_id, basis_version, basis_approved_seq, basis_approved_at, basis_approver, basis_serves,
		basis_reason, basis_session, basis_registered_at, basis_pending_ask, kept_person_version, kept_person_seq, kept_person_at, rests_on,
		basis_moves)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
		$13, $14, $15, $16, $17, $18, $19::text[], $20, $21, $22, $23, $24, $25, $26, nullif($27::text[], '{}'),
		case when $13::text is null then 0 else 1 end)
		on conflict (issue, step) do update set artifact_id = excluded.artifact_id,
		latest_version = excluded.latest_version, approved_version = excluded.approved_version,
		approved_seq = excluded.approved_seq, approved_at = excluded.approved_at,
		told_changes_version = excluded.told_changes_version, told_changes_reason = excluded.told_changes_reason,
		told_changes_seq = excluded.told_changes_seq, told_changes_at = excluded.told_changes_at,
		ever_opened = design_gates.ever_opened or excluded.ever_opened,
		basis_issue = excluded.basis_issue, basis_artifact_id = excluded.basis_artifact_id, basis_version = excluded.basis_version,
		basis_approved_seq = excluded.basis_approved_seq, basis_approved_at = excluded.basis_approved_at,
		basis_approver = excluded.basis_approver, basis_serves = excluded.basis_serves, basis_reason = excluded.basis_reason,
		basis_session = excluded.basis_session, basis_registered_at = excluded.basis_registered_at,
		basis_pending_ask = excluded.basis_pending_ask, kept_person_version = excluded.kept_person_version,
		kept_person_seq = excluded.kept_person_seq, kept_person_at = excluded.kept_person_at,
		rests_on = (select array_agg(ancestor order by first) from (
			select ancestor, min(place) as first
			from unnest(coalesce(design_gates.rests_on, '{}') || coalesce(excluded.rests_on, '{}')) with ordinality as named (ancestor, place)
			group by ancestor) as kept),
		basis_moves = design_gates.basis_moves + case
			when (design_gates.basis_issue, design_gates.basis_artifact_id, design_gates.basis_version, design_gates.basis_pending_ask)
				is distinct from (excluded.basis_issue, excluded.basis_artifact_id, excluded.basis_version, excluded.basis_pending_ask) then 1
			else 0 end`

// releaseBeforeMark is a review's mark as the release before writes it: its sequence and its
// time, each null when unknown.
func releaseBeforeMark(mark record.EventMark) (seq, at any) {
	if mark.Seq > 0 {
		seq = mark.Seq
	}
	if !mark.At.IsZero() {
		at = mark.At
	}
	return seq, at
}

// writeAsTheReleaseBefore writes gate as the release before's PutGate writes it: its version rule
// for ever_opened, its basis, open or pending, in the basis columns, and the person's approval a
// step open on a basis keeps apart. It names none of the hash gate's columns, a basis's root
// version among them.
func writeAsTheReleaseBefore(t *testing.T, st *Store, gate record.DesignGate) {
	t.Helper()
	approvedSeq, approvedAt := releaseBeforeMark(gate.ApprovedMark)
	if gate.ApprovedVersion == nil {
		approvedSeq, approvedAt = nil, nil
	}
	toldSeq, toldAt := releaseBeforeMark(gate.ToldChangesMark)
	toldReason := gate.ToldChangesReason
	if gate.ToldChangesVersion == nil {
		toldSeq, toldAt, toldReason = nil, nil, ""
	}
	var basisIssue, basisArtifact, basisVersion, basisSeq, basisAt, basisApprover, basisReason, basisSession, registeredAt, pendingAsk any
	var basisServes []string
	if basis := gate.Basis; basis != nil {
		basisIssue, basisArtifact, basisVersion = basis.Issue, basis.ArtifactID, basis.Version
		basisServes, basisReason, basisSession, registeredAt = basis.Serves, basis.Reason, basis.Session, basis.RegisteredAt
		if basis.Pending != nil {
			pendingAsk = basis.Pending.Ask
		} else {
			basisSeq, basisAt = releaseBeforeMark(basis.Approval)
			basisApprover = basis.Approver
		}
	}
	var keptVersion, keptSeq, keptAt any
	if kept := gate.KeptPerson; kept != nil && gate.OnBasis() {
		keptVersion = kept.Version
		keptSeq, keptAt = releaseBeforeMark(kept.Mark)
	}
	opened := gate.EverOpened || gate.ApprovedVersion != nil && *gate.ApprovedVersion == gate.LatestVersion
	if _, err := st.Pool().Exec(t.Context(), releaseBeforePutGate,
		gate.Issue, string(gate.Step.OrSpec()), gate.ArtifactID, gate.LatestVersion, gate.ApprovedVersion, approvedSeq, approvedAt,
		gate.ToldChangesVersion, toldReason, toldSeq, toldAt, opened,
		basisIssue, basisArtifact, basisVersion, basisSeq, basisAt, basisApprover, basisServes,
		basisReason, basisSession, registeredAt, pendingAsk, keptVersion, keptSeq, keptAt, gate.RestsOn); err != nil {
		t.Fatalf("write the gate of %s as the release before writes it: %v", gate.Issue, err)
	}
}

// gatesOf is every gate of issue as this release's record store reads it.
func gatesOf(t *testing.T, st *Store, issue string) map[record.DesignStep]record.DesignGate {
	t.Helper()
	var gates map[record.DesignStep]record.DesignGate
	if err := Transact(t.Context(), st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		gates, err = record.NewStore().GatesOf(ctx, tx, issue)
		return err
	}); err != nil {
		t.Fatalf("read the gates of %s: %v", issue, err)
	}
	return gates
}

// putGate writes gate through this release's record store.
func putGate(t *testing.T, st *Store, gate record.DesignGate) {
	t.Helper()
	if err := Transact(t.Context(), st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutGate(ctx, tx, gate)
	}); err != nil {
		t.Fatalf("put the gate of %s: %v", gate.Issue, err)
	}
}

// addedColumnsSet is how many rows of the issues of project hold a value in a column the hash
// gate added, carried aside: the verdict, its digests and its mark, digest_owed, the told standing
// and a basis's root version.
func addedColumnsSet(t *testing.T, st *Store, project string) int {
	t.Helper()
	var set int
	if err := st.Pool().QueryRow(t.Context(), `select count(*) from design_gates where issue in (select key from issues where project = $1)
		and (latest_digest_version is not null or approval_current is not null or latest_digest is not null or latest_digest_seq is not null
		or latest_digest_at is not null or approved_digest is not null or digest_owed is not null or told_open is not null
		or basis_root_version is not null)`, project).Scan(&set); err != nil {
		t.Fatalf("read the hash gate's columns of %s: %v", project, err)
	}
	return set
}

// markOf is the mark a row's sequence and time columns hold, each zero where null.
func markOf(seq *int64, at *time.Time) record.EventMark {
	var mark record.EventMark
	if seq != nil {
		mark.Seq = *seq
	}
	if at != nil {
		mark.At = at.UTC()
	}
	return mark
}

// releaseBeforeGate is the spec step of issue as the release before reads it on the schema the way
// back leaves: in version mode, with its versions, its approval, and its basis, open or pending,
// with the person's approval a step open on a basis keeps apart. That release records no root
// version of a basis.
func releaseBeforeGate(t *testing.T, st *Store, issue string) record.DesignGate {
	t.Helper()
	gate := record.DesignGate{Issue: issue, Step: record.DesignStepSpec}
	var approvedSeq, basisSeq, keptSeq *int64
	var approvedAt, basisAt, registeredAt, keptAt *time.Time
	var keptVersion *int
	var basis record.GateBasis
	var ask string
	if err := st.Pool().QueryRow(t.Context(), `select artifact_id, latest_version, approved_version, approved_seq, approved_at,
		coalesce(basis_issue, ''), coalesce(basis_artifact_id, ''), coalesce(basis_version, 0), basis_approved_seq, basis_approved_at,
		coalesce(basis_approver, ''), basis_registered_at, coalesce(basis_pending_ask, ''), kept_person_version, kept_person_seq, kept_person_at,
		basis_moves from design_gates where issue = $1 and step = 'spec'`, issue).Scan(&gate.ArtifactID, &gate.LatestVersion, &gate.ApprovedVersion,
		&approvedSeq, &approvedAt, &basis.Issue, &basis.ArtifactID, &basis.Version, &basisSeq, &basisAt, &basis.Approver, &registeredAt, &ask,
		&keptVersion, &keptSeq, &keptAt, &gate.BasisMoves); err != nil {
		t.Fatalf("read the gate of %s as the release before reads it: %v", issue, err)
	}
	if gate.ApprovedVersion != nil {
		gate.ApprovedMark = markOf(approvedSeq, approvedAt)
	}
	if basis.ArtifactID == "" {
		return gate
	}
	if registeredAt != nil {
		basis.RegisteredAt = registeredAt.UTC()
	}
	if ask != "" {
		basis.Approver, basis.Pending = "", &record.PendingBasis{Ask: ask}
	} else {
		basis.Approval = markOf(basisSeq, basisAt)
	}
	gate.Basis = &basis
	if gate.OnBasis() && keptVersion != nil {
		gate.KeptPerson = &record.PersonApproval{Version: *keptVersion, Mark: markOf(keptSeq, keptAt)}
	}
	return gate
}

// bootOf is a boot of this release for project as the daemon's workflow records it
// (workflowRuntime.recordBoot): RecordBoot writes the boot's daemon_boot row and its marker, and
// before them, in the same transaction, the reset after an earlier release's boot
// (record.ResetGatesAfterEarlierRelease), which queues the gate seeds of a deployment that gates
// the spec step, due at at. It answers the boot's id, what the reset did, and the error of a boot
// whose transaction fails.
func bootOf(t *testing.T, st *Store, project string, at time.Time) (int64, record.GateReset, error) {
	t.Helper()
	var reset record.GateReset
	records := record.NewStore()
	id, err := st.RecordBoot(t.Context(), project, at, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reset, err = record.ResetGatesAfterEarlierRelease(ctx, tx, records, project, []record.DesignStep{record.DesignStepSpec}, at)
		return err
	})
	return id, reset, err
}

// bootAt is bootOf for a boot whose transaction commits.
func bootAt(t *testing.T, st *Store, project string, at time.Time) (int64, record.GateReset) {
	t.Helper()
	id, reset, err := bootOf(t, st, project, at)
	if err != nil {
		t.Fatalf("the boot of %s: %v", project, err)
	}
	return id, reset
}

// earlierReleaseBoot writes the daemon_boot row a boot of a release before the hash gate writes for
// project at at, which has no row of design_gate_verdict_boots, and answers its id.
func earlierReleaseBoot(t *testing.T, st *Store, project string, at time.Time) int64 {
	t.Helper()
	var id int64
	if err := st.Pool().QueryRow(t.Context(), `insert into daemon_boot (project, started_at) values ($1, $2) returning id`, project, at).Scan(&id); err != nil {
		t.Fatalf("record the earlier release's boot of %s: %v", project, err)
	}
	return id
}

// outboxKinds is how many rows of each kind the outbox holds.
func outboxKinds(t *testing.T, st *Store) map[string]int {
	t.Helper()
	rows, err := st.Pool().Query(t.Context(), `select kind, count(*) from outbox group by kind`)
	if err != nil {
		t.Fatalf("count the outbox's rows: %v", err)
	}
	defer rows.Close()
	kinds := map[string]int{}
	for rows.Next() {
		var kind string
		var count int
		if err := rows.Scan(&kind, &count); err != nil {
			t.Fatalf("read the outbox's count: %v", err)
		}
		kinds[kind] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("count the outbox's rows: %v", err)
	}
	return kinds
}

// assertToldNothing holds the outbox to before, but for its gate seeds: a boot's reset queues a gate
// seed of each registered step and tells nobody anything (no notice, role message, news reconcile
// or other row).
func assertToldNothing(t *testing.T, st *Store, before map[string]int, what string) {
	t.Helper()
	after := outboxKinds(t, st)
	for kind := range after {
		if _, ok := before[kind]; !ok {
			before[kind] = 0
		}
	}
	for kind, count := range before {
		if kind != "gate_seed" && after[kind] != count {
			t.Fatalf("%s left %d outbox rows of kind %s, want the %d before it: it tells nobody anything", what, after[kind], kind, count)
		}
	}
}

// gateColumns is each column of design_gates the hash gate adds that the table holds.
func gateColumns(t *testing.T, st *Store) []string {
	t.Helper()
	var columns []string
	if err := st.Pool().QueryRow(t.Context(), `select coalesce(array_agg(column_name::text order by column_name), '{}') from information_schema.columns
		where table_schema = 'public' and table_name = 'design_gates' and column_name = any($1)`,
		[]string{"latest_digest_version", "approval_current", "latest_digest", "latest_digest_seq", "latest_digest_at", "approved_digest",
			"digest_owed", "told_open", "basis_root_version", "carried"}).Scan(&columns); err != nil {
		t.Fatalf("read the hash gate's columns of design_gates: %v", err)
	}
	return columns
}

// rowsUnseeded is each design_gates row of the issues of project, as "issue/step", for which the
// outbox holds no gate seed of its step at its latest version for its issue's generation.
func rowsUnseeded(t *testing.T, st *Store, project string) []string {
	t.Helper()
	var rows []string
	if err := st.Pool().QueryRow(t.Context(), `select coalesce(array_agg(g.issue || '/' || g.step order by g.issue, g.step), '{}')
		from design_gates g join issues i on i.key = g.issue
		where i.project = $1 and not exists (select 1 from outbox o where o.kind = 'gate_seed' and o.issue = g.issue
			and o.payload = jsonb_build_object('artifactId', g.artifact_id, 'step', g.step, 'version', g.latest_version, 'generation', i.generation))`,
		project).Scan(&rows); err != nil {
		t.Fatalf("read the gate seeds of %s: %v", project, err)
	}
	return rows
}

// gateSeeds is how many gate_seed rows the outbox holds.
func gateSeeds(t *testing.T, st *Store) int {
	t.Helper()
	var count int
	if err := st.Pool().QueryRow(t.Context(), `select count(*) from outbox where kind = 'gate_seed'`).Scan(&count); err != nil {
		t.Fatalf("count the gate seeds: %v", err)
	}
	return count
}

// The migration of the hash gate (dispatch://INFRA-6065, R2, Departures D1 to D3) adds the verdict's
// columns to design_gates and the table design_gate_verdict_boots. Every row it finds reads in
// version mode, the told standing and the basis's root version null, so each gate open before it
// reads open after it; a copy a hand-over carried at its root's current generation, on a document
// another issue's row names, is marked carried. The check holds a verdict's version and the verdict
// together, and digest_owed follows the two versions with no write of its own. The outbox rows of
// the release before's gate kinds still decode: the hash gate adds no kind and no payload field.
//
// The way back, for the rows of one project's issues alone, ends each basis, open or pending, whose
// root version is below latest_version while a digest is owed, so the step is closed and the
// earlier release cannot open a pending one at a version nobody approved, and ends no basis whose
// digest is not owed, at the latest version or below it; then it puts those rows in version mode
// and clears every column the hash gate adds. It drops nothing: every column, the check, the table
// and the migration's version row stay, and another project's rows stay as they were. The release
// before's write still runs after it, and the roll forward applies nothing.
//
// Each boot of this release records its daemon_boot row and its row of design_gate_verdict_boots,
// both or neither (RecordBoot). A boot after an earlier release's boot of the same project (a
// daemon_boot row with no such row), in the transaction that records it, puts that project's rows
// in version mode and clears every column the hash gate adds, so a changes request the earlier
// release wrote over a verdict closes the gate; it ends no basis and tells nothing; it keeps a
// carried copy marked and marks one the earlier release carried; it touches no row of another
// project; and it queues a gate seed for each registered step. A boot after its own release's boot,
// a boot after another project's earlier release's boot, and a boot after one that failed after
// its record reset nothing; and a boot whose transaction fails leaves no daemon_boot row, no marker
// and no reset.
func TestHashGateMigrationAndWayBack(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := emptyStore(t)
	migrateThrough(t, st, hashGateVersion-1)
	exec := func(statement string, args ...any) error {
		_, err := st.Pool().Exec(ctx, statement, args...)
		return err
	}
	must := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if err := exec(statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	issueOf := func(t *testing.T, key, project string, generation int) {
		t.Helper()
		must(t, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ($1, $1, $2, 'a root', 'admitted', $3, 'in_progress', 'A', 1)`, key, project, generation)
	}
	issue := func(t *testing.T, key string, generation int) {
		t.Helper()
		issueOf(t, key, "LEGION", generation)
	}

	// The release before's rows: an open gate, a closed gate, a step open on a basis and a step
	// pending on an ancestor's request; an old root and the copy a hand-over carried of its gate
	// at the new root's current generation, with a document of the new root's own; a copy whose
	// hand-over was of an earlier generation; and a root whose hand-over carried no gate.
	for key, generation := range map[string]int{"LEGION-201": 1, "LEGION-202": 1, "LEGION-203": 1, "LEGION-204": 1,
		"LEGION-205": 1, "LEGION-206": 2, "LEGION-207": 3, "LEGION-209": 1} {
		issue(t, key, generation)
	}
	for _, seed := range []string{
		`insert into design_gates (issue, step, artifact_id, latest_version, approved_version, approved_seq, approved_at, ever_opened)
			values ('LEGION-201', 'spec', 'doc-201', 4, 4, 9, '2026-11-20T09:00:00Z', true)`,
		`insert into design_gates (issue, step, artifact_id, latest_version, approved_version, approved_seq, approved_at, ever_opened)
			values ('LEGION-202', 'spec', 'doc-202', 5, 4, 9, '2026-11-20T09:00:00Z', true)`,
		`insert into design_gates (issue, step, artifact_id, latest_version, approved_version, ever_opened, basis_issue, basis_artifact_id,
			basis_version, basis_approved_seq, basis_approved_at, basis_approver, basis_registered_at, rests_on, basis_moves)
			values ('LEGION-203', 'spec', 'doc-203', 4, 4, true, 'LEGION-100', 'artifact-100', 3, 31, '2026-11-20T08:00:00Z', 'user:alice',
			'2026-11-20T09:00:00Z', '{LEGION-100}', 1)`,
		`insert into design_gates (issue, step, artifact_id, latest_version, basis_issue, basis_artifact_id, basis_version,
			basis_registered_at, basis_pending_ask, basis_moves)
			values ('LEGION-204', 'spec', 'doc-204', 4, 'LEGION-100', 'artifact-100', 4, '2026-11-20T09:00:00Z', 'ask-100', 1)`,
		`insert into design_gates (issue, step, artifact_id, latest_version, approved_version, ever_opened)
			values ('LEGION-205', 'spec', 'doc-205', 3, 3, true)`,
		`insert into handed_over_roots (issue, generation, gate_carried) values ('LEGION-206', 2, true)`,
		`insert into design_gates (issue, step, artifact_id, latest_version, approved_version, ever_opened)
			values ('LEGION-206', 'spec', 'doc-205', 3, 3, true)`,
		`insert into design_gates (issue, step, artifact_id, latest_version) values ('LEGION-206', 'criteria', 'doc-206-criteria', 1)`,
		`insert into handed_over_roots (issue, generation, gate_carried) values ('LEGION-207', 2, true)`,
		`insert into design_gates (issue, step, artifact_id, latest_version, approved_version, ever_opened)
			values ('LEGION-207', 'spec', 'doc-205', 3, 3, true)`,
		`insert into handed_over_roots (issue, generation, gate_carried) values ('LEGION-209', 1, false)`,
		`insert into design_gates (issue, step, artifact_id, latest_version) values ('LEGION-209', 'spec', 'doc-205', 3)`,
	} {
		must(t, seed)
	}
	// The outbox rows of the gate's kinds, each as the release before encodes it (record.NewOutboxRow
	// at 8d28f3cc): a gate seed, a merger's start, and a notice of each kind a gate's move tells.
	releaseBeforeRows := []struct{ kind, payload string }{
		{"gate_seed", `{"artifactId":"artifact-208","step":"spec","version":4,"generation":1}`},
		{"supervise", `{"op":"start","tree":"LEGION-208","role":"merger","task":"Merge it.","generation":1,"phase":"merging","entry":3}`},
		{"notice", `{"kind":"design-approved","version":4,"actor":"user:alice","ref":"dispatch://LEGION-208/spec","review":{"Seq":12,"At":"2026-11-27T09:00:00Z"},"artifact":"artifact-208"}`},
		{"notice", `{"kind":"carried-gate-opened","version":4,"reason":"a human approved LEGION-207's spec document at version 4, which opens the gate LEGION-208 carried; any child waiting on it starts","actor":"user:alice","ref":"dispatch://LEGION-207/spec"}`},
		{"notice", `{"kind":"design-changes-requested","version":4,"reason":"name the rollback path","actor":"user:alice","ref":"dispatch://LEGION-208/spec","review":{"Seq":13,"At":"2026-11-27T09:00:00Z"},"artifact":"artifact-208"}`},
		{"notice", `{"kind":"basis-ended","role":"architect","version":3,"reason":"a person requested changes on version 3 of LEGION-100's spec","actor":"user:alice","ref":"dispatch://LEGION-100/spec"}`},
	}
	for _, row := range releaseBeforeRows {
		must(t, `insert into outbox (kind, issue, payload, attempts, next_at, last_error) values ($1, 'LEGION-208', $2::jsonb, 0, now(), '')`,
			row.kind, row.payload)
	}
	if err := exec(`update design_gates set latest_digest_version = 4 where issue = 'LEGION-201'`); err == nil ||
		!strings.Contains(err.Error(), "latest_digest_version") {
		t.Fatalf("a verdict written before the migration = %v, want it refused for naming a column the table does not have", err)
	}

	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the hash gate: %v", err)
	}

	t.Run("rows written before the migration read in version mode", func(t *testing.T) {
		for _, key := range []string{"LEGION-201", "LEGION-202", "LEGION-203", "LEGION-204"} {
			var versionMode bool
			if err := st.Pool().QueryRow(ctx, `select latest_digest_version is null and approval_current is null and latest_digest is null
				and latest_digest_seq is null and latest_digest_at is null and approved_digest is null and digest_owed is null
				and told_open is null and basis_root_version is null and not carried
				from design_gates where issue = $1`, key).Scan(&versionMode); err != nil || !versionMode {
				t.Fatalf("the row of %s after the migration is in version mode: %t (%v), want every verdict column null and carried false", key, versionMode, err)
			}
		}
		for key, open := range map[string]bool{"LEGION-201": true, "LEGION-202": false, "LEGION-203": true, "LEGION-204": false} {
			gate := gatesOf(t, st, key)[record.DesignStepSpec]
			if gate.Verdict != nil || gate.ToldOpen != nil || gate.Carried || gate.ApprovedDigest != "" {
				t.Fatalf("the gate of %s reads %#v, want it in version mode with no told standing, not carried", key, gate)
			}
			if gate.Basis != nil && gate.Basis.RootVersion != 0 {
				t.Fatalf("the basis of %s reads root version %d, want none: the release before recorded none", key, gate.Basis.RootVersion)
			}
			if record.GateOpen(gate) != open || record.ToldStanding(gate) != open || record.DigestOwed(gate) {
				t.Fatalf("the gate of %s reads open %t, told %t and owed %t, want open and told %t and not owed, as before the migration",
					key, record.GateOpen(gate), record.ToldStanding(gate), record.DigestOwed(gate), open)
			}
		}
		if gate := gatesOf(t, st, "LEGION-203")[record.DesignStepSpec]; !gate.OnBasis() {
			t.Fatalf("the step of LEGION-203 reads %#v, want it open on its basis", gate)
		}
		if gate := gatesOf(t, st, "LEGION-204")[record.DesignStepSpec]; !gate.PendingOnBasis() {
			t.Fatalf("the step of LEGION-204 reads %#v, want it pending on the ancestor's request", gate)
		}
	})

	t.Run("the carried backfill", func(t *testing.T) {
		for _, row := range []struct {
			issue, step string
			carried     bool
			why         string
		}{
			{"LEGION-206", "spec", true, "the copy its hand-over carried at its current generation, on its old root's document"},
			{"LEGION-206", "criteria", false, "a document of its own, which no other issue's row names"},
			{"LEGION-205", "spec", false, "the old root's own row: no hand-over created it"},
			{"LEGION-207", "spec", false, "a copy whose hand-over was of an earlier generation than the root's current one"},
			{"LEGION-209", "spec", false, "a root whose hand-over carried no gate"},
		} {
			var carried bool
			if err := st.Pool().QueryRow(ctx, `select carried from design_gates where issue = $1 and step = $2`, row.issue, row.step).Scan(&carried); err != nil {
				t.Fatalf("read whether %s's %s row is carried: %v", row.issue, row.step, err)
			}
			if carried != row.carried {
				t.Fatalf("%s's %s row migrated carried %t, want %t: %s", row.issue, row.step, carried, row.carried, row.why)
			}
		}
		if gates := gatesOf(t, st, "LEGION-206"); !gates[record.DesignStepSpec].Carried || gates[record.DesignStepCriteria].Carried {
			t.Fatalf("LEGION-206's gates read %#v, want its spec step carried and its criteria step its own", gates)
		}
	})

	t.Run("the check holds a verdict and its version together", func(t *testing.T) {
		for _, refused := range []struct{ why, statement, constraint string }{
			{"a verdict with no version", `update design_gates set approval_current = true where issue = 'LEGION-201'`, "design_gates_verdict_check"},
			{"a version with no verdict", `update design_gates set latest_digest_version = 4 where issue = 'LEGION-201'`, "design_gates_verdict_check"},
			{"a verdict's version of zero", `update design_gates set latest_digest_version = 0, approval_current = true where issue = 'LEGION-201'`, "check constraint"},
			{"a verdict's sequence of zero", `update design_gates set latest_digest_version = 4, approval_current = true, latest_digest_seq = 0 where issue = 'LEGION-201'`, "check constraint"},
			{"a root version of zero", `update design_gates set basis_root_version = 0 where issue = 'LEGION-203'`, "check constraint"},
		} {
			if err := exec(refused.statement); err == nil || !strings.Contains(err.Error(), refused.constraint) {
				t.Fatalf("%s = %v, want %s to refuse it", refused.why, err, refused.constraint)
			}
		}
		must(t, `update design_gates set latest_digest_version = 4, approval_current = true where issue = 'LEGION-201'`)
		must(t, `update design_gates set latest_digest_version = null, approval_current = null where issue = 'LEGION-201'`)
	})

	t.Run("digest_owed follows the two versions", func(t *testing.T) {
		issue(t, "LEGION-401", 1)
		owed := func() *bool {
			t.Helper()
			var owed *bool
			if err := st.Pool().QueryRow(ctx, `select digest_owed from design_gates where issue = 'LEGION-401'`).Scan(&owed); err != nil {
				t.Fatalf("read LEGION-401's digest_owed: %v", err)
			}
			return owed
		}
		must(t, `insert into design_gates (issue, step, artifact_id, latest_version, latest_digest_version, approval_current)
			values ('LEGION-401', 'spec', 'doc-401', 5, 4, true)`)
		if got := owed(); got == nil || !*got {
			t.Fatalf("digest_owed of a row inserted at latest 5 with a verdict of 4 = %v, want true", got)
		}
		must(t, `update design_gates set latest_digest_version = 5 where issue = 'LEGION-401'`)
		if got := owed(); got == nil || *got {
			t.Fatalf("digest_owed once the verdict is of 5 = %v, want false", got)
		}
		must(t, `update design_gates set latest_version = 6 where issue = 'LEGION-401'`)
		if got := owed(); got == nil || !*got {
			t.Fatalf("digest_owed after an update of latest_version alone to 6 = %v, want true", got)
		}
		must(t, `update design_gates set latest_digest_version = null, approval_current = null where issue = 'LEGION-401'`)
		if got := owed(); got != nil {
			t.Fatalf("digest_owed of a row in version mode = %v, want null", *got)
		}
		if err := exec(`update design_gates set digest_owed = false where issue = 'LEGION-401'`); err == nil || !strings.Contains(err.Error(), "digest_owed") {
			t.Fatalf("a write of digest_owed = %v, want it refused: the column is generated", err)
		}
	})

	// The rows this release writes in hash mode below. A person approved version 2 of each root's
	// own document (person), and the root's own verdict says that approval does not stand for the
	// version it is for. A basis on LEGION-100's approval of its version 3 opens a step, and a
	// pending one waits on that ancestor's request ask-100.
	at := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	person := record.EventMark{Seq: 9, At: at.Add(9 * time.Minute)}
	m30 := record.EventMark{Seq: 30, At: at.Add(30 * time.Minute)}
	m40 := record.EventMark{Seq: 40, At: at.Add(40 * time.Minute)}
	m50 := record.EventMark{Seq: 50, At: at.Add(50 * time.Minute)}
	ancestor := record.GateBasis{Issue: "LEGION-100", ArtifactID: "artifact-100", Version: 3, Approval: m30, Approver: "user:alice", RegisteredAt: at}
	ownApproval := func(key string, latest int) record.DesignGate {
		return record.DesignGate{Issue: key, Step: record.DesignStepSpec, ArtifactID: "doc-" + key, LatestVersion: latest,
			ApprovedVersion: new(2), ApprovedMark: person}
	}
	// owing is gate at latest version latest, with the root's own verdict false at version verdict,
	// a digest owed where verdict is below latest, and told open as told.
	owing := func(gate record.DesignGate, latest, verdict int, told bool) record.DesignGate {
		gate.LatestVersion = latest
		gate.Verdict, gate.ToldOpen = &record.GateVerdict{Version: verdict, Current: false, Mark: person}, new(told)
		return gate
	}
	// onBasis is key's step open on the ancestor's approval, registered at root version root with
	// the person's approval of 2 kept apart, told open as the basis opened it; pending is key's
	// step waiting on ask-100, registered at root version root, told closed. Each stands at latest
	// version latest with the verdict at verdict.
	onBasis := func(key string, root, latest, verdict int) record.DesignGate {
		return owing(record.OpenOnBasis(ownApproval(key, root), ancestor), latest, verdict, true)
	}
	pending := func(key string, root, latest, verdict int) record.DesignGate {
		return owing(record.PendOnBasis(ownApproval(key, root), ancestor, "ask-100"), latest, verdict, false)
	}
	// hashOpen is key's step open on the person's approval of 5, which Dispatch reports current
	// for 5, told open.
	hashOpen := func(key string) record.DesignGate {
		return record.DesignGate{Issue: key, Step: record.DesignStepSpec, ArtifactID: "doc-" + key, LatestVersion: 5, ApprovedVersion: new(5),
			ApprovedMark: m30, Verdict: &record.GateVerdict{Version: 5, Current: true, Mark: m30}, ToldOpen: new(true), EverOpened: true}
	}

	t.Run("the outbox rows of the release before decode", func(t *testing.T) {
		rows, err := st.Pool().Query(ctx, `select id, kind, issue, payload from outbox order by id`)
		if err != nil {
			t.Fatalf("read the outbox: %v", err)
		}
		var decoded int
		for rows.Next() {
			var row record.OutboxRow
			var kind string
			if err := rows.Scan(&row.ID, &kind, &row.Issue, &row.Payload); err != nil {
				t.Fatalf("read an outbox row: %v", err)
			}
			row.Kind = record.OutboxKind(kind)
			payload, err := record.DecodeOutboxPayload(row)
			if err != nil {
				t.Fatalf("the release before's %s row %s does not decode: %v", kind, row.Payload, err)
			}
			if encoded, err := json.Marshal(payload); err != nil || !jsonEqual(t, encoded, row.Payload) {
				t.Fatalf("the release before's %s row %s encodes again as %s (%v), want the same payload", kind, row.Payload, encoded, err)
			}
			decoded++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the outbox: %v", err)
		}
		if decoded != len(releaseBeforeRows) {
			t.Fatalf("decoded %d outbox rows, want the %d the release before queued", decoded, len(releaseBeforeRows))
		}
	})

	// A release before the hash gate boots on this schema after this release wrote rows of LEGION
	// in hash mode: its daemon_boot row has no row of design_gate_verdict_boots. It applies a
	// person's changes request over a verdict, which it leaves standing, and carries a gate at a
	// re-admission, a copy it leaves carried false. Then this release boots.
	t.Run("a boot after an earlier release's boot puts its own project's rows in version mode in the transaction that records it", func(t *testing.T) {
		for key, project := range map[string]string{"LEGION-701": "LEGION", "LEGION-702": "LEGION", "LEGION-703": "LEGION",
			"LEGION-704": "LEGION", "LEGION-705": "LEGION", "LEGION-706": "LEGION", "ACME-801": "ACME"} {
			issueOf(t, key, project, 1)
		}
		putGate(t, st, hashOpen("LEGION-701"))
		// LEGION-702 stands open on a basis and LEGION-703 waits on a request, each registered at root
		// version 3 while version 4 owes its digest; LEGION-704 stands open on a basis registered at 4
		// inside that fill window. LEGION-705 stands open on the person's approval of 6, which Dispatch
		// reports current for 7, with the approved digest it named.
		putGate(t, st, onBasis("LEGION-702", 3, 4, 3))
		putGate(t, st, pending("LEGION-703", 3, 4, 3))
		putGate(t, st, onBasis("LEGION-704", 4, 4, 3))
		putGate(t, st, record.DesignGate{Issue: "LEGION-705", Step: record.DesignStepSpec, ArtifactID: "doc-LEGION-705", LatestVersion: 7,
			ApprovedVersion: new(6), ApprovedMark: m30, ApprovedDigest: "sha256:approved", ToldOpen: new(true), EverOpened: true,
			Verdict: &record.GateVerdict{Version: 7, Current: true, Digest: "sha256:content-7", Mark: m40}})
		putGate(t, st, hashOpen("ACME-801"))

		earlier := earlierReleaseBoot(t, st, "LEGION", at.Add(time.Hour))
		changed := gatesOf(t, st, "LEGION-701")[record.DesignStepSpec]
		changed.ApprovedVersion, changed.ApprovedMark = nil, record.EventMark{}
		changed.ToldChangesVersion, changed.ToldChangesReason, changed.ToldChangesMark = new(5), "name the rollback path", m50
		writeAsTheReleaseBefore(t, st, changed)
		must(t, `insert into handed_over_roots (issue, generation, gate_carried) values ('LEGION-706', 1, true)`)
		writeAsTheReleaseBefore(t, st, record.DesignGate{Issue: "LEGION-706", ArtifactID: "doc-205", LatestVersion: 3, ApprovedVersion: new(3), EverOpened: true})
		moves := map[string]int{}
		for _, key := range []string{"LEGION-702", "LEGION-703", "LEGION-704"} {
			moves[key] = gatesOf(t, st, key)[record.DesignStepSpec].BasisMoves
		}

		seeds, outbox := gateSeeds(t, st), outboxKinds(t, st)
		boot, reset := bootAt(t, st, "LEGION", at.Add(2*time.Hour))

		var recorded bool
		if err := st.Pool().QueryRow(ctx, `select exists (select 1 from daemon_boot join design_gate_verdict_boots on boot = daemon_boot.id
			where daemon_boot.id = $1 and project = 'LEGION')`, boot).Scan(&recorded); err != nil || !recorded {
			t.Fatalf("the boot %d of LEGION and its marker are recorded: %t (%v), want both written with the reset", boot, recorded, err)
		}
		// The changes request the release before applied closes the gate: in version mode no
		// approval stands.
		if gate := gatesOf(t, st, "LEGION-701")[record.DesignStepSpec]; gate.Verdict != nil || record.GateOpen(gate) {
			t.Fatalf("the gate of LEGION-701, whose approval the release before cleared at a changes request, reads open %t (%#v, verdict %+v); "+
				"want it closed, in version mode", record.GateOpen(gate), gate, gate.Verdict)
		}
		for _, row := range []struct {
			key                    string
			open, onBasis, pending bool
			approved               int
			why                    string
		}{
			{"LEGION-702", false, false, false, 2, "its basis at root version 3 stands on the row, since the reset ends no basis, and this release reads a basis below the " +
				"latest version 4 in version mode as the version rule reads it, closed, back on the person's approval of 2 (basisAsWritten)"},
			{"LEGION-703", false, false, true, 2, "its wait at root version 3 stands, since the reset ends no basis, closed on the person's approval of 2"},
			{"LEGION-704", true, true, false, 4, "its basis, registered at version 4 inside the fill window, stands open at the latest version"},
			{"LEGION-705", false, false, false, 6, "the version rule reads no approval of the latest version 7"},
		} {
			gate := gatesOf(t, st, row.key)[record.DesignStepSpec]
			approved := 0
			if gate.ApprovedVersion != nil {
				approved = *gate.ApprovedVersion
			}
			if gate.Verdict != nil || gate.ToldOpen != nil || gate.ApprovedDigest != "" || record.GateOpen(gate) != row.open ||
				gate.OnBasis() != row.onBasis || gate.PendingOnBasis() != row.pending || approved != row.approved ||
				gate.Basis != nil && gate.Basis.RootVersion != 0 {
				t.Fatalf("the gate of %s after the reset reads open %t, approved %d (%#v, verdict %+v, basis %+v); want it in version mode, "+
					"open %t, approved %d, on a basis %t, pending %t, with no root version: %s",
					row.key, record.GateOpen(gate), approved, gate, gate.Verdict, gate.Basis, row.open, row.approved, row.onBasis, row.pending, row.why)
			}
		}
		// The reset ends no basis: each basis stands on its row as the reset found it, with its moves.
		for key, before := range moves {
			var standing bool
			var moved int
			if err := st.Pool().QueryRow(ctx, `select coalesce(basis_artifact_id, '') <> '', basis_moves from design_gates where issue = $1 and step = 'spec'`,
				key).Scan(&standing, &moved); err != nil || !standing || moved != before {
				t.Fatalf("the basis of %s after the reset stands %t with %d moves (%v), want it standing with the %d moves before: the reset ends no basis",
					key, standing, moved, err, before)
			}
		}
		if set := addedColumnsSet(t, st, "LEGION"); set != 0 {
			t.Fatalf("%d rows of LEGION hold a column the hash gate added after the reset, want none: every row is in version mode", set)
		}
		for _, row := range []struct {
			issue, step string
			carried     bool
			why         string
		}{
			{"LEGION-206", "spec", true, "the copy the migration marked stays marked"},
			{"LEGION-706", "spec", true, "the copy the release before carried is marked, as the migration marks one"},
			{"LEGION-206", "criteria", false, "a document of its own"},
			{"LEGION-205", "spec", false, "the old root's own row"},
			{"LEGION-207", "spec", false, "a copy whose hand-over was of an earlier generation than the root's current one"},
			{"LEGION-209", "spec", false, "a root whose hand-over carried no gate"},
		} {
			if got := gatesOf(t, st, row.issue)[record.DesignStep(row.step)]; got.Carried != row.carried {
				t.Fatalf("%s's %s row after the reset reads carried %t, want %t: %s", row.issue, row.step, got.Carried, row.carried, row.why)
			}
		}
		if gate := gatesOf(t, st, "ACME-801")[record.DesignStepSpec]; gate.Verdict == nil || gate.ToldOpen == nil || !record.GateOpen(gate) {
			t.Fatalf("ACME-801, of another project, reads %#v after LEGION's reset, want it in hash mode and open as this release wrote it", gate)
		}
		if unseeded := rowsUnseeded(t, st, "LEGION"); len(unseeded) != 0 {
			t.Fatalf("the rows of LEGION with no gate seed after the reset = %q, want a seed of each registered step", unseeded)
		}
		var rows int
		if err := st.Pool().QueryRow(ctx, `select count(*) from design_gates where issue in (select key from issues where project = 'LEGION')`).Scan(&rows); err != nil {
			t.Fatalf("count the rows of LEGION: %v", err)
		}
		if added := gateSeeds(t, st) - seeds; added != rows || reset.Seeds != rows {
			t.Fatalf("the reset queued %d gate seeds and answered %d, want one for each of the %d rows of LEGION, ACME's none", added, reset.Seeds, rows)
		}
		if want := (record.GateReset{EarlierBoot: earlier, Rows: 5, Seeds: rows}); reset != want {
			t.Fatalf("the reset answered %+v, want %+v: the five rows in hash mode or with a root version put in version mode", reset, want)
		}
		assertToldNothing(t, st, outbox, "the boot's reset")
	})

	// The boot above recorded itself with its reset and then failed, before it stamped its stop,
	// once its catch-up had put LEGION-707 in hash mode. The next boot finds that boot's marker.
	// Then an earlier release boots on ACME, which shares the database, and LEGION boots again.
	t.Run("a boot after one that failed after its record, after its own release's boot, or after another project's earlier release's boot resets nothing", func(t *testing.T) {
		issue(t, "LEGION-707", 1)
		putGate(t, st, hashOpen("LEGION-707"))
		seeds, outbox := gateSeeds(t, st), outboxKinds(t, st)
		if _, reset := bootAt(t, st, "LEGION", at.Add(3*time.Hour)); reset != (record.GateReset{}) {
			t.Fatalf("the boot after a boot that failed after its record reset %+v, want nothing done", reset)
		}
		earlierReleaseBoot(t, st, "ACME", at.Add(4*time.Hour))
		if _, reset := bootAt(t, st, "LEGION", at.Add(5*time.Hour)); reset != (record.GateReset{}) {
			t.Fatalf("the boot of LEGION after ACME's earlier release's boot reset %+v, want nothing done", reset)
		}
		for _, key := range []string{"LEGION-707", "ACME-801"} {
			if gate := gatesOf(t, st, key)[record.DesignStepSpec]; gate.Verdict == nil || gate.ToldOpen == nil {
				t.Fatalf("the gate of %s reads %#v, want it in hash mode as this release wrote it", key, gate)
			}
		}
		if got := gateSeeds(t, st); got != seeds {
			t.Fatalf("the outbox holds %d gate seeds after the boots, want the %d before them", got, seeds)
		}
		assertToldNothing(t, st, outbox, "the boots")
	})

	t.Run("RecordBoot writes a boot and its marker both or neither, and a boot whose transaction fails resets nothing", func(t *testing.T) {
		count := func(table string) int {
			t.Helper()
			var count int
			if err := st.Pool().QueryRow(ctx, `select count(*) from `+table).Scan(&count); err != nil {
				t.Fatalf("count the rows of %s: %v", table, err)
			}
			return count
		}
		id, err := st.RecordBoot(ctx, "LEGION", at.Add(6*time.Hour), nil)
		if err != nil {
			t.Fatalf("record a boot: %v", err)
		}
		var marked bool
		if err := st.Pool().QueryRow(ctx, `select exists (select 1 from design_gate_verdict_boots where boot = $1)`, id).Scan(&marked); err != nil || !marked {
			t.Fatalf("the boot %d has its row of design_gate_verdict_boots: %t (%v), want it written with the boot", id, marked, err)
		}
		// An earlier release boots on LEGION after it, so the next boot of this release resets
		// where its transaction commits.
		earlierReleaseBoot(t, st, "LEGION", at.Add(7*time.Hour))
		boots, markers, seeds := count("daemon_boot"), count("design_gate_verdict_boots"), gateSeeds(t, st)
		must(t, `create function refuse_verdict_boot() returns trigger language plpgsql as $$ begin raise exception 'the marker refused'; end $$`)
		must(t, `create trigger refuse_verdict_boot before insert on design_gate_verdict_boots for each row execute function refuse_verdict_boot()`)
		if _, _, err := bootOf(t, st, "LEGION", at.Add(8*time.Hour)); err == nil || !strings.Contains(err.Error(), "the marker refused") {
			t.Fatalf("a boot whose marker the store refuses = %v, want the refusal", err)
		}
		if after := count("daemon_boot"); after != boots {
			t.Fatalf("a boot whose marker the store refused left %d boots, want the %d before it: no daemon_boot row without its marker", after, boots)
		}
		if after := count("design_gate_verdict_boots"); after != markers {
			t.Fatalf("a boot whose marker the store refused left %d markers, want the %d before it", after, markers)
		}
		if gate := gatesOf(t, st, "LEGION-707")[record.DesignStepSpec]; gate.Verdict == nil || gate.ToldOpen == nil {
			t.Fatalf("the gate of LEGION-707 reads %#v after a boot whose transaction failed, want it in hash mode: no reset without its boot", gate)
		}
		if got := gateSeeds(t, st); got != seeds {
			t.Fatalf("the outbox holds %d gate seeds after a boot whose transaction failed, want the %d before it", got, seeds)
		}
		must(t, `drop trigger refuse_verdict_boot on design_gate_verdict_boots`)
		must(t, `drop function refuse_verdict_boot()`)
	})

	t.Run("the way back", func(t *testing.T) {
		wayBackRows := []string{"LEGION-601", "LEGION-602", "LEGION-603", "LEGION-604", "LEGION-605", "LEGION-606", "LEGION-607", "LEGION-608"}
		for _, key := range wayBackRows {
			issue(t, key, 1)
		}
		putGate(t, st, onBasis("LEGION-601", 3, 4, 3))
		putGate(t, st, pending("LEGION-602", 3, 4, 3))
		putGate(t, st, onBasis("LEGION-603", 4, 4, 3))
		putGate(t, st, pending("LEGION-604", 4, 4, 3))
		// LEGION-605 waits on a request registered before the migration, which recorded no root
		// version: it entered hash mode at its verdict's version 3, and version 4 owes its digest.
		beforeMigration := pending("LEGION-605", 3, 4, 3)
		beforeMigration.Basis = new(*beforeMigration.Basis)
		beforeMigration.Basis.RootVersion = 0
		putGate(t, st, beforeMigration)
		putGate(t, st, onBasis("LEGION-606", 3, 3, 3))
		// LEGION-607 stands open on a basis and LEGION-608 waits on a request, each registered at root
		// version 3 below the latest version 4, whose verdict Dispatch gave: neither owes its digest,
		// so the way back, which ends only what a version owing its digest held back, keeps both.
		putGate(t, st, onBasis("LEGION-607", 3, 4, 4))
		putGate(t, st, pending("LEGION-608", 3, 4, 4))
		// ACME-802, of another project whose daemon of this release goes on, stands open on a basis
		// registered at root version 3 while version 4 owes its digest: LEGION's way back leaves it,
		// and ACME-801 in hash mode, as they were.
		issueOf(t, "ACME-802", "ACME", 1)
		putGate(t, st, onBasis("ACME-802", 3, 4, 3))
		other := map[string]record.DesignGate{}
		for _, key := range []string{"ACME-801", "ACME-802"} {
			other[key] = gatesOf(t, st, key)[record.DesignStepSpec]
		}
		moves := map[string]int{}
		for _, key := range wayBackRows {
			moves[key] = gatesOf(t, st, key)[record.DesignStepSpec].BasisMoves
		}

		wayBack := wayBackOf(t, hashGateVersion, "the hash gate")
		if !strings.Contains(wayBack, "'<PROJECT>'") {
			t.Fatalf("the way back of the hash gate names no project as '<PROJECT>':\n%s", wayBack)
		}
		must(t, strings.ReplaceAll(wayBack, "<PROJECT>", "LEGION"))

		for _, row := range []struct {
			key      string
			ended    bool
			open     bool
			approved int
			why      string
		}{
			{"LEGION-602", true, false, 2, "its wait at root version 3 ends while version 4 owes its digest, on the person's approval of 2"},
			{"LEGION-605", true, false, 2, "its wait, registered before the migration at its verdict's version 3, ends while version 4 owes its digest"},
			{"LEGION-601", true, false, 2, "its basis at root version 3 ends while version 4 owes its digest, back on the person's approval of 2 it kept"},
			{"LEGION-603", false, true, 4, "its basis, registered at version 4 inside the fill window, stands"},
			{"LEGION-604", false, false, 2, "its wait, registered at version 4 inside the fill window, stands"},
			{"LEGION-606", false, true, 3, "its basis at version 3 owes no digest and stands"},
			{"LEGION-607", false, false, 3, "its basis at root version 3, below the latest version 4, owes no digest and stands: version mode reads it closed"},
			{"LEGION-608", false, false, 2, "its wait at root version 3, below the latest version 4, owes no digest and stands"},
		} {
			gate := releaseBeforeGate(t, st, row.key)
			// The release before answers the ancestor's request with a person's approval: it opens a
			// step still waiting on it at its own latest version, which nobody approved for a wait
			// registered at an older one.
			if answered := record.OpenPendingBasis(gate, m50, "user:bob"); row.ended && record.GateOpen(answered) {
				t.Fatalf("the step of %s reads open after the release before answers the ancestor's request (%#v, basis %+v): %s",
					row.key, answered, answered.Basis, row.why)
			}
			approved := 0
			if gate.ApprovedVersion != nil {
				approved = *gate.ApprovedVersion
			}
			moved := moves[row.key]
			if row.ended {
				moved++
			}
			if (gate.Basis == nil) != row.ended || record.GateOpen(gate) != row.open || approved != row.approved || gate.BasisMoves != moved {
				t.Fatalf("the step of %s after the way back = %#v (basis %+v), open %t, approved %d, basis moves %d; want its basis ended %t, "+
					"open %t, approved %d, basis moves %d: %s", row.key, gate, gate.Basis, record.GateOpen(gate), approved, gate.BasisMoves,
					row.ended, row.open, row.approved, moved, row.why)
			}
			if row.key == "LEGION-601" && (gate.ApprovedMark != person || gate.KeptPerson != nil) {
				t.Fatalf("the step of LEGION-601 after the way back = %#v, want the person's approval of 2 back at its own mark, none kept apart", gate)
			}
		}
		if set := addedColumnsSet(t, st, "LEGION"); set != 0 {
			t.Fatalf("%d rows of LEGION hold a column the hash gate added after the way back, want none: every row is in version mode", set)
		}
		if gate := gatesOf(t, st, "LEGION-206")[record.DesignStepSpec]; !gate.Carried {
			t.Fatalf("LEGION-206's carried copy reads %#v after the way back, want it marked carried again, as the migration marks it", gate)
		}
		for key, before := range other {
			if after := gatesOf(t, st, key)[record.DesignStepSpec]; !reflect.DeepEqual(after, before) {
				t.Fatalf("the gate of %s, of another project, after LEGION's way back = %#v (verdict %+v, basis %+v); want it as it was, %#v",
					key, after, after.Verdict, after.Basis, before)
			}
		}
		// It drops nothing, so the daemons of this release that serve other projects from the same
		// database go on: every column the migration adds, its check, design_gate_verdict_boots and
		// the migration's version row stand.
		if columns := gateColumns(t, st); len(columns) != 10 {
			t.Fatalf("the hash gate's columns of design_gates after the way back = %q, want all ten", columns)
		}
		var check, table, recorded bool
		if err := st.Pool().QueryRow(ctx, `select exists (select 1 from pg_constraint where conname = 'design_gates_verdict_check'),
			to_regclass('design_gate_verdict_boots') is not null, exists (select 1 from schema_version where version = $1)`, hashGateVersion).
			Scan(&check, &table, &recorded); err != nil || !check || !table || !recorded {
			t.Fatalf("after the way back the check stands %t, design_gate_verdict_boots stands %t and the migration's version row stands %t (%v); "+
				"want each to stand", check, table, recorded, err)
		}

		// The release before writes on the schema the way back leaves: a new row and a move of
		// one this release wrote.
		issue(t, "LEGION-501", 1)
		writeAsTheReleaseBefore(t, st, record.DesignGate{Issue: "LEGION-501", ArtifactID: "doc-501", LatestVersion: 2, ApprovedVersion: new(2)})
		writeAsTheReleaseBefore(t, st, record.DesignGate{Issue: "LEGION-705", ArtifactID: "doc-LEGION-705", LatestVersion: 8,
			ApprovedVersion: new(7), ApprovedMark: m50, EverOpened: true})
		if gate := gatesOf(t, st, "LEGION-501")[record.DesignStepSpec]; !record.GateOpen(gate) {
			t.Fatalf("the gate the release before wrote open reads %#v, want it open", gate)
		}
		earlier := earlierReleaseBoot(t, st, "LEGION", at.Add(9*time.Hour))

		if applied, err := st.Migrate(ctx); err != nil || applied != 0 {
			t.Fatalf("the roll forward applied %d migrations (%v), want none: the way back deletes no version row", applied, err)
		}
		if set := addedColumnsSet(t, st, "LEGION"); set != 0 {
			t.Fatalf("%d rows of LEGION hold a column the hash gate added after the roll forward, want none", set)
		}

		// The first boot of this release after the roll forward finds the earlier release's boot,
		// which has no row of design_gate_verdict_boots, and resets: every row of LEGION is in
		// version mode already, so it clears none, and it queues a seed of each registered step.
		_, reset := bootAt(t, st, "LEGION", at.Add(10*time.Hour))
		if reset.EarlierBoot != earlier || reset.Rows != 0 {
			t.Fatalf("the first boot's reset after the roll forward answered %+v, want the boot %d found and no row cleared", reset, earlier)
		}
		if unseeded := rowsUnseeded(t, st, "LEGION"); len(unseeded) != 0 {
			t.Fatalf("the rows of LEGION with no gate seed after the roll forward's reset = %q, want a seed of each registered step", unseeded)
		}
	})
}

// jsonEqual says whether a and b are the same JSON value.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var left, right any
	if err := json.Unmarshal(a, &left); err != nil {
		t.Fatalf("read %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &right); err != nil {
		t.Fatalf("read %s: %v", b, err)
	}
	return reflect.DeepEqual(left, right)
}
