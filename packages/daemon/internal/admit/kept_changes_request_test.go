package admit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// INFRA-3136 R2: whether a person's recorded decision is still that person's word is decided at
// the moment a task about it is sent, and a changes request a later approval followed is not.
// Here a person requested changes to a parked root's spec while nobody in its tree could hear it,
// so the daemon kept that decision for the root's next architect (the outbox's parkDecision, or
// the relay: record.Store.AddParkedDecision). The same person then approved the latest version,
// which is their hand-over (sjawhar/legion#2619): the daemon queues the root's todo, keeps
// nothing, and so replaces nothing on the spec's reference. The re-admission's start must not
// tell the new architect that the person requested changes: the person's word is the approval
// that came after, and a task naming the changes request sends the architect to revise a spec
// the person has just approved, which is dispatch://INFRA-3111's defect on the kept-decision path.
func TestAChangesRequestKeptOnAParkedRootIsNotNamedOnceThePersonsApprovalHandsItOver(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	parkedRoot(t, pool)
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if err := records.PutGate(ctx, tx, record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 3}); err != nil {
			t.Fatalf("put the gate: %v", err)
		}
		if err := records.AddParkedDecision(ctx, tx, record.ParkedDecision{Issue: "LEGION-ROOT", Ref: "dispatch://LEGION-ROOT/spec",
			Decision: record.DecisionDesignChangesRequested, Actor: "user:alice", At: fixedNow.Add(-time.Hour)}); err != nil {
			t.Fatalf("keep the person's changes request: %v", err)
		}
	})
	view := &dispatch.Issue{Key: "LEGION-ROOT", Project: testProject, Title: "root", Status: "backlog", Labels: []string{dispatch.LegionLabel},
		HandedOver: true, PrimaryArtifactID: "artifact-root"}
	approval := intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved,
		Version: 3, Mark: record.EventMark{Seq: 4, At: fixedNow}, Actor: alice, Issue: view}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "approved", approval, admission.engine, admission); err != nil {
		t.Fatalf("apply the person's approval: %v", err)
	}
	readmitRoot(t, pool, admission)
	starts := architectStartRows(t, pool)
	if len(starts) != 1 {
		t.Fatalf("architect starts %#v, want one", starts)
	}
	if strings.Contains(starts[0].Task, string(record.DecisionDesignChangesRequested)) {
		t.Fatalf("the start's task %q names the person's changes request, which their approval of version 3 at %s followed: "+
			"a task names a person's decision only while it is still their word (classify.ReviewSuperseded)", starts[0].Task, fixedNow)
	}
}
