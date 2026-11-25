package store

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// designGateDocumentIssueVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const designGateDocumentIssueVersion = 20261122040952

// 20261122040952 adds design_gates.document_issue, the issue whose document a gate row is bound to
// (record.DesignGate.DocumentIssue). Before it, a write of the column is refused: the column is not
// there. The backfill names, on every row of a document, the issue of that document's own
// registration where the store holds it: the row of an issue that is no root a hand-over created
// with its gate at its generation (LEGION-208's rows; LEGION-209, LEGION-210 and LEGION-211 are
// such roots, handed_over_roots at their generation with gate_carried). So a carried root's copy
// names the old root whether or not its handed_over_from still names that root (LEGION-209 does,
// LEGION-210's promotion cleared its), and so does a copy of a copy (LEGION-211, carried from
// LEGION-209), which names the first old root, whose document it is, not the one it was carried
// from. A document whose rows the store cannot tell a registration from a copy of keeps null on
// every row: one a carried root registered itself (criteria-209, which LEGION-209 and its copy on
// LEGION-211 hold), and one rows of two issues that are no carried root hold (shared, which the
// one-issue rule leaves to Dispatch rather than to a guess). After it, a write that names no
// document issue keeps the one the row holds on the same document, and a write on another
// document takes the one it names.
func TestADesignGateRowNamesTheIssueOfItsDocumentAfter20261122040952(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, designGateDocumentIssueVersion-1)
	for _, seed := range []struct{ key, from string }{
		{"LEGION-208", ""}, {"LEGION-209", "LEGION-208"}, {"LEGION-210", ""}, {"LEGION-211", "LEGION-209"}, {"LEGION-212", ""}, {"LEGION-213", ""},
	} {
		if _, err := store.Pool().Exec(ctx, `insert into issues
			(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, handed_over_from)
			values ($1, $1, 'LEGION', 'a root', 'admitted', 2, 'in_progress', 'A', 1, $2)`, seed.key, seed.from); err != nil {
			t.Fatalf("record %s: %v", seed.key, err)
		}
	}
	for _, key := range []string{"LEGION-209", "LEGION-210", "LEGION-211"} {
		if _, err := store.Pool().Exec(ctx, `insert into handed_over_roots (issue, generation, gate_carried) values ($1, 2, true)`, key); err != nil {
			t.Fatalf("record %s handed over with its gate: %v", key, err)
		}
	}
	for _, row := range []struct{ issue, step, artifact string }{
		{"LEGION-208", "spec", "spec-208"}, {"LEGION-208", "criteria", "criteria-208"},
		{"LEGION-209", "spec", "spec-208"}, {"LEGION-209", "criteria", "criteria-209"},
		{"LEGION-210", "spec", "spec-208"},
		{"LEGION-211", "spec", "spec-208"}, {"LEGION-211", "criteria", "criteria-209"},
		{"LEGION-212", "spec", "shared"}, {"LEGION-213", "spec", "shared"},
	} {
		if _, err := store.Pool().Exec(ctx, `insert into design_gates (issue, step, artifact_id, latest_version) values ($1, $2, $3, 2)`,
			row.issue, row.step, row.artifact); err != nil {
			t.Fatalf("record the gate row of %s %s: %v", row.issue, row.step, err)
		}
	}
	_, err := store.Pool().Exec(ctx, `update design_gates set document_issue = issue`)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a write of document_issue before the migration = %v, want it refused: the column is the migration's", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the issue of a gate row's document: %v", err)
	}
	gates := func(issue string) map[record.DesignStep]record.DesignGate {
		t.Helper()
		var held map[record.DesignStep]record.DesignGate
		if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			held, err = record.NewStore().GatesOf(ctx, tx, issue)
			return err
		}); err != nil {
			t.Fatalf("read the gates of %s: %v", issue, err)
		}
		return held
	}
	for _, want := range []struct {
		issue    string
		step     record.DesignStep
		recorded string
	}{
		{"LEGION-208", record.DesignStepSpec, "LEGION-208"},
		{"LEGION-208", record.DesignStepCriteria, "LEGION-208"},
		{"LEGION-209", record.DesignStepSpec, "LEGION-208"},
		{"LEGION-209", record.DesignStepCriteria, ""},
		{"LEGION-210", record.DesignStepSpec, "LEGION-208"},
		{"LEGION-211", record.DesignStepSpec, "LEGION-208"},
		{"LEGION-211", record.DesignStepCriteria, ""},
		{"LEGION-212", record.DesignStepSpec, ""},
		{"LEGION-213", record.DesignStepSpec, ""},
	} {
		if row := gates(want.issue)[want.step]; row.DocumentIssue != want.recorded {
			t.Errorf("the %s row of %s after the migration records document issue %q; want %q", want.step, want.issue, row.DocumentIssue, want.recorded)
		}
	}

	put := func(gate record.DesignGate) {
		t.Helper()
		if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			return record.NewStore().PutGate(ctx, tx, gate)
		}); err != nil {
			t.Fatalf("write the gate row: %v", err)
		}
	}
	reset := record.DesignGate{Issue: "LEGION-209", Step: record.DesignStepSpec, ArtifactID: "spec-208", LatestVersion: 3}
	put(reset)
	if got := gates("LEGION-209")[record.DesignStepSpec]; got.LatestVersion != 3 || got.DocumentIssue != "LEGION-208" {
		t.Fatalf("a write of the carried row on its own document that names no document issue left %+v, want version 3 and LEGION-208 kept", got)
	}
	own := record.DesignGate{Issue: "LEGION-209", Step: record.DesignStepSpec, ArtifactID: "spec-209", LatestVersion: 1, DocumentIssue: "LEGION-209"}
	put(own)
	if got := gates("LEGION-209")[record.DesignStepSpec]; got.ArtifactID != "spec-209" || got.DocumentIssue != "LEGION-209" {
		t.Fatalf("a registration of the root's own document over the carried row left %+v, want spec-209 of LEGION-209", got)
	}
	put(record.DesignGate{Issue: "LEGION-210", Step: record.DesignStepSpec, ArtifactID: "spec-210", LatestVersion: 1})
	if got := gates("LEGION-210")[record.DesignStepSpec]; got.DocumentIssue != "" || got.DocumentOf() != "LEGION-210" {
		t.Fatalf("a write on another document that names no document issue left %+v, want none recorded, read as LEGION-210's own", got)
	}
}
