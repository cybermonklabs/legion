package daemon

import (
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The controller's set_status of todo on an issue the daemon has not recorded, which carries the
// legion label, is the controller's intake (R19's handover, R26 of dispatch://INFRA-3320): the
// call queues the todo (decision 5 of dispatch://INFRA-4730), the outbox writes it, and the
// outbox applies Dispatch's answer to it as the daemon's own write (api.ApplyOwnStatusWrite), so
// admission records the issue, stands it in the waiting line and starts it at a free slot before
// the issue's own event is delivered. On the scratch rig (the real Dispatch, Postgres and NATS)
// every event of the issue is dropped as the consumer delivers it (dropConsumedEvent), the todo's
// echo included, so nothing but the write's answer can have recorded it.
func TestControllerSetStatusTodoAdmitsAnUnrecordedLabelledIssue(t *testing.T) {
	t.Parallel()
	r := newPersonHandoverRig(t, 2)
	const title = "Harbour crane telemetry"
	var dropped, droppedTodo atomic.Int32
	r.tune = func(o *overrides) {
		o.dropConsumedEvent = func(fact intake.Fact) bool {
			issue, ok := fact.(intake.DispatchIssue)
			if !ok || issue.Title != title {
				return false
			}
			dropped.Add(1)
			if issue.Status == "todo" {
				droppedTodo.Add(1)
			}
			return true
		}
	}
	r.boot()
	issue := r.create(asPerson, title, nil)
	r.addLabel(issue.Key, asPerson, "legion")

	status, body := r.d.request(http.MethodPost, "/legion/v1/issues/status", api.IssueStatusRequest{GrantID: r.controllerGrant(), Issue: issue.Key, Status: "todo"}, false)
	if status != http.StatusOK {
		t.Fatalf("the controller's set_status todo of %s = %d %s, want 200", issue.Key, status, body)
	}

	settled(t, r.d, "start "+issue.Key+" in a free slot", func(s api.State) bool {
		_, recorded := s.Issues[issue.Key]
		return recorded && slices.Contains(s.Admission.Active, issue.Key)
	})
	architect := r.architectToken(issue.Key)
	launched := func() bool {
		return slices.ContainsFunc(r.rt.CallsOf("Spawn"), func(call fake.Call) bool { return call.Spec.Claim == architect })
	}
	if !testwait.Holds(testwait.Bounded(t, 60*time.Second, time.Second), 100*time.Millisecond, launched) {
		t.Fatalf("the runtime launched %+v in 60s, want %s's architect started; the daemon holds the claims %+v", r.rt.CallsOf("Spawn"), issue.Key, r.d.claims())
	}
	if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), 50*time.Millisecond, func() bool { return droppedTodo.Load() != 0 }) {
		t.Fatalf("no todo event of %s reached the consumer in 30s (%d of its events dropped); this proves nothing of a record made before its event", issue.Key, dropped.Load())
	}
}
