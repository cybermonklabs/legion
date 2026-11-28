package store

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
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

// writeAsTheReleaseBefore writes gate, a step on no basis, as the release before's PutGate writes
// it: its version rule for ever_opened, and the basis columns null.
func writeAsTheReleaseBefore(t *testing.T, st *Store, gate record.DesignGate) {
	t.Helper()
	if gate.Basis != nil {
		t.Fatalf("writeAsTheReleaseBefore writes a step on no basis, and %s's names one", gate.Issue)
	}
	approvedSeq, approvedAt := releaseBeforeMark(gate.ApprovedMark)
	if gate.ApprovedVersion == nil {
		approvedSeq, approvedAt = nil, nil
	}
	toldSeq, toldAt := releaseBeforeMark(gate.ToldChangesMark)
	toldReason := gate.ToldChangesReason
	if gate.ToldChangesVersion == nil {
		toldSeq, toldAt, toldReason = nil, nil, ""
	}
	opened := gate.EverOpened || gate.ApprovedVersion != nil && *gate.ApprovedVersion == gate.LatestVersion
	if _, err := st.Pool().Exec(t.Context(), releaseBeforePutGate,
		gate.Issue, string(gate.Step.OrSpec()), gate.ArtifactID, gate.LatestVersion, gate.ApprovedVersion, approvedSeq, approvedAt,
		gate.ToldChangesVersion, toldReason, toldSeq, toldAt, opened,
		nil, nil, nil, nil, nil, nil, []string(nil), nil, nil, nil, nil, nil, nil, nil, gate.RestsOn); err != nil {
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

// verdictColumns is what the row of issue's spec step holds in the verdict's columns, each null as
// the empty text: the version, the verdict, the digest, the mark's sequence, the told standing,
// the approved digest, the root version of its basis and whether the row is a carried copy.
func verdictColumns(t *testing.T, st *Store, issue string) []string {
	t.Helper()
	var columns []string
	if err := st.Pool().QueryRow(t.Context(), `select array[coalesce(latest_digest_version::text, ''), coalesce(approval_current::text, ''),
		coalesce(latest_digest, ''), coalesce(latest_digest_seq::text, ''), coalesce(told_open::text, ''), coalesce(approved_digest, ''),
		coalesce(basis_root_version::text, ''), carried::text]
		from design_gates where issue = $1 and step = 'spec'`, issue).Scan(&columns); err != nil {
		t.Fatalf("read the verdict's columns of %s: %v", issue, err)
	}
	return columns
}

// The migration of the hash gate (dispatch://INFRA-6065, R2, Departures D1 to D3) adds the verdict's
// columns to design_gates. Every row it finds reads in version mode, the told standing and the
// basis's root version null, so each gate open before it reads open after it; a copy a hand-over
// carried at its root's current generation, on a document another issue's row names, is marked
// carried. The check holds a verdict's version and the verdict together, and digest_owed follows
// the two versions with no write of its own. A daemon of the release before writes none of these
// columns, and the reader takes the row it moved after a verdict as that move would have left it
// under this release (R-old-1 to R-old-4). The way back drops the columns and the migration's
// version row, the release before's write still runs after it, and the roll forward puts every row
// in version mode. The outbox rows of the release before's gate kinds still decode: the hash gate
// adds no kind and no payload field.
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
	issue := func(t *testing.T, key string, generation int) {
		t.Helper()
		must(t, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ($1, $1, 'LEGION', 'a root', 'admitted', $2, 'in_progress', 'A', 1)`, key, generation)
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

	// Each row stands in hash mode as this release wrote it, with a verdict at m40, before the
	// release before moves it.
	at := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	m30 := record.EventMark{Seq: 30, At: at.Add(30 * time.Minute)}
	m40 := record.EventMark{Seq: 40, At: at.Add(40 * time.Minute)}
	m50 := record.EventMark{Seq: 50, At: at.Add(50 * time.Minute)}
	hashRow := func(key string, approved int, current bool) record.DesignGate {
		return record.DesignGate{Issue: key, Step: record.DesignStepSpec, ArtifactID: "doc-" + key, LatestVersion: 7,
			ApprovedVersion: new(approved), ApprovedMark: m30, ApprovedDigest: "sha256:approved",
			Verdict:  &record.GateVerdict{Version: 7, Current: current, Digest: "sha256:content-7", Mark: m40},
			ToldOpen: new(current)}
	}
	t.Run("an older daemon's write after a verdict", func(t *testing.T) {
		for _, tc := range []struct {
			name, key string
			before    record.DesignGate
			move      func(*record.DesignGate)
			verdict   *record.GateVerdict
			open      bool
			owed      bool
		}{
			{"R-old-1 a changes request", "LEGION-301", hashRow("LEGION-301", 7, true), func(g *record.DesignGate) {
				g.ApprovedVersion, g.ApprovedMark = nil, record.EventMark{}
				g.ToldChangesVersion, g.ToldChangesReason, g.ToldChangesMark = new(7), "name the rollback path", m50
			}, &record.GateVerdict{Version: 7, Current: false, Mark: m50}, false, false},
			{"R-old-2 an approval of the latest version", "LEGION-302", hashRow("LEGION-302", 6, false), func(g *record.DesignGate) {
				g.ApprovedVersion, g.ApprovedMark = new(7), m50
			}, &record.GateVerdict{Version: 7, Current: true, Mark: m50}, true, false},
			{"R-old-3 a new version", "LEGION-303", hashRow("LEGION-303", 7, true), func(g *record.DesignGate) {
				g.LatestVersion = 8
			}, &record.GateVerdict{Version: 7, Current: true, Digest: "sha256:content-7", Mark: m40}, false, true},
			{"R-old-4 an approval below the latest version", "LEGION-304", hashRow("LEGION-304", 6, true), func(g *record.DesignGate) {
				g.ApprovedVersion, g.ApprovedMark = new(7), m50
				g.LatestVersion = 8
			}, nil, false, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				issue(t, tc.key, 1)
				putGate(t, st, tc.before)
				moved := gatesOf(t, st, tc.key)[record.DesignStepSpec]
				tc.move(&moved)
				writeAsTheReleaseBefore(t, st, moved)

				got := gatesOf(t, st, tc.key)[record.DesignStepSpec]
				if !reflect.DeepEqual(got.Verdict, tc.verdict) {
					t.Fatalf("the moved row reads the verdict %#v, want %#v", got.Verdict, tc.verdict)
				}
				if record.GateOpen(got) != tc.open || record.DigestOwed(got) != tc.owed {
					t.Fatalf("the moved row reads open %t and owed %t, want open %t and owed %t", record.GateOpen(got), record.DigestOwed(got), tc.open, tc.owed)
				}
				switch {
				case tc.verdict == nil && got.ToldOpen != nil:
					t.Fatalf("the moved row in version mode reads the told standing %t, want none", *got.ToldOpen)
				case tc.owed && (got.ToldOpen == nil || !*got.ToldOpen):
					t.Fatalf("the row whose new version owes its digest reads the told standing %v, want the one told before, open", got.ToldOpen)
				case tc.verdict != nil && !tc.owed && (got.ToldOpen == nil || *got.ToldOpen != tc.open):
					t.Fatalf("the moved row reads the told standing %v, want %t, as it opens", got.ToldOpen, tc.open)
				}
				if tc.verdict != nil && !tc.owed && got.ApprovedDigest != "" {
					t.Fatalf("the moved row keeps the approved digest %q, which names the approval before the move", got.ApprovedDigest)
				}

				// This release's next write of the row writes what it read.
				putGate(t, st, got)
				if again := gatesOf(t, st, tc.key)[record.DesignStepSpec]; !reflect.DeepEqual(again, got) {
					t.Fatalf("the row after this release's next write reads %#v, want %#v as read before it", again, got)
				}
				want := []string{"", "", "", "", "", "", "", "false"}
				if v := got.Verdict; v != nil {
					want[0], want[1], want[2], want[3] = strconv.Itoa(v.Version), strconv.FormatBool(v.Current), v.Digest, strconv.FormatInt(v.Mark.Seq, 10)
					want[4] = strconv.FormatBool(*got.ToldOpen)
				}
				if got.ApprovedVersion != nil {
					want[5] = got.ApprovedDigest
				}
				if columns := verdictColumns(t, st, tc.key); !reflect.DeepEqual(columns, want) {
					t.Fatalf("the verdict's columns after this release's next write = %q, want %q", columns, want)
				}
			})
		}
	})

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

	t.Run("the way back", func(t *testing.T) {
		must(t, wayBackOf(t, hashGateVersion, "the hash gate"))
		var left []string
		if err := st.Pool().QueryRow(ctx, `select coalesce(array_agg(column_name::text order by column_name), '{}') from information_schema.columns
			where table_schema = 'public' and table_name = 'design_gates' and column_name = any($1)`,
			[]string{"latest_digest_version", "approval_current", "latest_digest", "latest_digest_seq", "latest_digest_at", "approved_digest",
				"digest_owed", "told_open", "basis_root_version", "carried"}).Scan(&left); err != nil || len(left) != 0 {
			t.Fatalf("the verdict's columns after the way back = %q (%v), want none", left, err)
		}
		var recorded bool
		if err := st.Pool().QueryRow(ctx, `select exists (select 1 from schema_version where version = $1)`, hashGateVersion).Scan(&recorded); err != nil || recorded {
			t.Fatalf("the migration's version row after the way back stands: %t (%v), want it deleted", recorded, err)
		}

		// The release before writes on the schema the way back leaves: a new row and a move of
		// one this release wrote in hash mode.
		issue(t, "LEGION-501", 1)
		writeAsTheReleaseBefore(t, st, record.DesignGate{Issue: "LEGION-501", ArtifactID: "doc-501", LatestVersion: 2, ApprovedVersion: new(2)})
		writeAsTheReleaseBefore(t, st, record.DesignGate{Issue: "LEGION-302", ArtifactID: "doc-LEGION-302", LatestVersion: 8,
			ApprovedVersion: new(7), ApprovedMark: m50, EverOpened: true})

		if _, err := st.Migrate(ctx); err != nil {
			t.Fatalf("roll forward through the hash gate: %v", err)
		}
		var hashMode int
		if err := st.Pool().QueryRow(ctx, `select count(*) from design_gates where latest_digest_version is not null or approval_current is not null
			or latest_digest is not null or latest_digest_seq is not null or latest_digest_at is not null or approved_digest is not null
			or digest_owed is not null or told_open is not null or basis_root_version is not null`).Scan(&hashMode); err != nil || hashMode != 0 {
			t.Fatalf("%d rows after the roll forward carry a verdict's column (%v), want every row in version mode", hashMode, err)
		}
		for _, key := range []string{"LEGION-301", "LEGION-302", "LEGION-303", "LEGION-304", "LEGION-501"} {
			if gate := gatesOf(t, st, key)[record.DesignStepSpec]; gate.Verdict != nil || gate.ToldOpen != nil {
				t.Fatalf("the gate of %s after the roll forward reads %#v, want it in version mode", key, gate)
			}
		}
		if gate := gatesOf(t, st, "LEGION-501")[record.DesignStepSpec]; !record.GateOpen(gate) {
			t.Fatalf("the gate the release before wrote open reads %#v after the roll forward, want it open", gate)
		}
		if gate := gatesOf(t, st, "LEGION-206")[record.DesignStepSpec]; !gate.Carried {
			t.Fatalf("LEGION-206's carried copy reads %#v after the roll forward, want it marked carried again", gate)
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
