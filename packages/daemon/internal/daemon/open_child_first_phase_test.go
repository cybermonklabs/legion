package daemon

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A root's first phase waits while Dispatch shows a child of it open (the children-first rule):
// a person files a child under the root and moves it to backlog before the root's gate opens,
// and the daemon records nothing of that child before a release. The gate's opening must leave
// the root at admitted, its first phase held on the open child, rather than start that phase and
// take it back at the next tick.
func TestARootsFirstPhaseWaitsForAChildFiledInBacklog(t *testing.T) {
	t.Parallel()
	r := newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 1, config.Project{})
	child := r.childInBacklog()
	r.givePlan("implementer", "reviewer")
	spec := r.specs[r.key]
	r.person(http.MethodPost, "/api/v1/artifacts/"+spec+"/reviews", map[string]any{"state": "approved"}, http.StatusCreated, nil)
	if status, body := r.d.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
		"grantId": r.worker(claim.RoleArchitect).grant(), "issue": r.key, "artifactId": spec, "version": 1}, false); status != http.StatusOK {
		t.Fatalf("register_gate of %s = %d %s, want 200", r.key, status, body)
	}
	var stands phase.Phase
	if left := testwait.Holds(20*time.Second, 100*time.Millisecond, func() bool {
		stands = r.d.state().Issues[r.key].Phase
		return stands != "" && stands != phase.Admitted
	}); left {
		t.Fatalf("%s left admitted for %s once its gate opened while its child %s stands open in backlog, want its first phase held at admitted",
			r.key, stands, child)
	}
}
