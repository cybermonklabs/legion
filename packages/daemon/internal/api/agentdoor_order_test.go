package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// agentDoor refuses every grant but the controller's 403 CONTROLLER_REQUIRED before the claim is
// looked up, so nothing of any claim is read for it (agentDoor's comment). The test serves the
// door from a daemon that cannot read the record, where the lookup (workerToken) fails: an
// architect's grant is still refused 403 at each of the six actions. The controller's grant is the
// control: it passes the grant check, reaches the lookup on the same daemon and is refused 500
// CLAIM_TOKEN_FAILED, so the architect's 403 comes from the order of the checks, not from a lookup
// that happened to succeed. With the lookup above the grant check, as at a83093dc86ef, the
// architect's rows read 500 as well.
func TestAnArchitectsGrantIsRefusedBeforeTheClaimIsLookedUp(t *testing.T) {
	h := newHarness(t)
	grants := credential.New(nil)
	server := func(withRecord bool) http.Handler {
		opts := Options{Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
			Controller: h.store, Grants: grants, Releaser: h.runtime, Trees: h.store}
		if withRecord {
			opts.Pool, opts.Record = h.store.Pool(), record.NewStore()
		}
		return NewServer("127.0.0.1", 8437, opts).Handler
	}
	withRecord, withoutRecord := server(true), server(false)
	h.handler = withRecord
	architect := treeArchitect(t, h)
	for _, op := range agentDoorActions {
		t.Run(op.name, func(t *testing.T) {
			h.handler = withRecord
			architectGrant, controllerGrantID := architect.grant(t), controllerGrant(t, h).GrantID
			h.handler = withoutRecord
			ask := func(grant string) ClaimRequest {
				return ClaimRequest{GrantID: grant, Issue: "LEGION-209", Role: string(claim.RoleImplementer)}
			}
			refusedSaying(t, h.request(http.MethodPost, op.route, ask(architectGrant), nil),
				http.StatusForbidden, "CONTROLLER_REQUIRED", op.as.name+" requires a controller grant")
			recorder := h.request(http.MethodPost, op.route, ask(controllerGrantID), nil)
			if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "CLAIM_TOKEN_FAILED") {
				t.Fatalf("the controller's %s on a daemon that cannot read the record = %d %s, want 500 CLAIM_TOKEN_FAILED: the lookup runs once the grant passes",
					op.name, recorder.Code, recorder.Body)
			}
		})
	}
}
