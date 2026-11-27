package admit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A re-admission keeps the approval its gate held, and the gate's clear comes after it, so the
// kept approval is the only record left of which document the person approved. The start asks the
// one predicate about that approval of that document (workflow.KeptDecisionsTask, record.KeptApproval
// .Artifact, dispatch://INFRA-3136/spec R2): an approval of the same version of another document
// is not the person's word on this one. So the start that names the kept approval must carry the
// document of the gate it was kept from. A keep that drops the document leaves the start asking of
// whichever document the gate holds next, and no test of the start's text would see it.
func TestAReadmissionKeepsTheDocumentOfTheApprovalItKeeps(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Clock: func() time.Time { return fixedNow }, Steps: config.DesignGateRootIssues.Steps()}, nil)
	admission := New(record.NewStore(), engine, 1, testProject, nil)
	admission.now = func() time.Time { return fixedNow }
	parkedRoot(t, pool)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutGate(ctx, tx, record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root",
			LatestVersion: 4, ApprovedVersion: new(3)}); err != nil {
			t.Fatalf("put the gate: %v", err)
		}
	})
	readmitRoot(t, pool, admission)
	starts := architectStartRows(t, pool)
	if len(starts) != 1 || starts[0].Kept == nil {
		t.Fatalf("architect starts %#v, want one that names the kept approval", starts)
	}
	if kept := starts[0].Kept.KeptApproval; kept.Approved != 3 || kept.Latest != 4 || kept.Artifact != "artifact-root" {
		t.Fatalf("the start's kept approval = %#v, want version 3 of 4 of artifact-root, the document the gate held approved", kept)
	}
}
