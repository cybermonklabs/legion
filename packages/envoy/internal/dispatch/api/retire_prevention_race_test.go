package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAPreventedByAppendedWhileItsChecksDocumentIsRetiredIsNotLeftStanding retires the document of
// a check of another issue in a transaction held open, appends a prevented_by from a risk to that
// check through the pointer route meanwhile, and then commits the retirement. A retired document's
// checks are run by nobody, so no prevented_by may point at one (spec requirement r-constraint-risk):
// whichever write commits second must see the other, and the risk must hold no prevented_by to the
// check once both are done. The pointer route locks only the risk's issue and reads the document's
// retired_at with no lock, and the retirement reads the check's prevented_by tuples before the
// append commits, so both commit and the tuple is left standing at a check nobody runs.
func TestAPreventedByAppendedWhileItsChecksDocumentIsRetiredIsNotLeftStanding(t *testing.T) {
	t.Parallel()
	rig := newPremortemRig(t)
	ctx := context.Background()
	premortem := rig.invoke(rig.spec, graphstore.LensKindPreMortem, "alice", nil)
	written := rig.post("/risks", map[string]any{
		"lens": premortem.Address, "heading": "A job runs twice.", "early_warning": "Two drainers.", "prevention": "One drainer.",
	}, "alice")
	if written.Code != http.StatusCreated {
		t.Fatalf("write the risk: status=%d body=%s", written.Code, written.Body.String())
	}
	risk := decodeBody[riskWritten](t, written)
	document := rig.otherIssueDocumentID

	tx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the retirement: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := docs.RetireDocument(ctx, tx, document, model.Actor{Kind: "user", ID: "alice"},
		"The test retires the document that holds the check."); err != nil {
		t.Fatalf("retire %s: %v", document, err)
	}

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- rig.post(nodePath(risk.Address, "pointers"), map[string]any{
			"kind": graphstore.TuplePreventedBy, "action": "append", "to": rig.otherIssueCheck,
		}, "alice")
	}()
	var appended *httptest.ResponseRecorder
	select {
	case appended = <-answered:
	case <-time.After(3 * time.Second):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the retirement: %v", err)
	}
	if appended == nil {
		select {
		case appended = <-answered:
		case <-time.After(30 * time.Second):
			t.Fatalf("the append to %s did not answer within 30 s of the retirement's commit", rig.otherIssueCheck)
		}
	}
	t.Logf("the append to %s while its document was being retired: status=%d body=%s",
		rig.otherIssueCheck, appended.Code, appended.Body.String())

	if held := rig.current(graphstore.KindRisk, risk.ID, graphstore.TuplePreventedBy); len(held) != 0 {
		t.Errorf("after the retirement of %s committed, the risk %s is prevented by %v, a check of a retired document "+
			"(the append answered %d)", document, risk.Address, held, appended.Code)
	}
}
