package daemon

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// Every status write the daemon queues keeps Dispatch's parent status rule (R24 of
// dispatch://INFRA-3320): against a scratch Dispatch with DISPATCH_PARENT_STATUS_RULE on, and
// again with it off, no write of the daemon's is refused. One case for each kind of status row the
// outbox judges (its ruleFor) and for each writer this branch queues one through: admission's move
// of a root into work and the move of a person's handover (ordinary rows), the person's handover
// itself (a person-handover row), the re-assert of the daemon's own status over a session's park
// (a re-assert row), and close_root's close (a close row). The boot's correction of a status
// Dispatch shows out of step with the phase (correctStatuses, an ordinary row) is
// status_scratch_dispatch_test.go's, against the same rig's Dispatch with the rule on. The merge's
// close is a close row as close_root's is. The writes of the architect's and the controller's
// calls (park_child, rerun_child, set_status, release_children) are queued by the API's own
// change of those routes, whose cases are written with it.
func TestADaemonStatusWriteNeverBreaksTheRule(t *testing.T) {
	for _, rule := range []string{"on", "off"} {
		t.Run("rule "+rule, func(t *testing.T) {
			r := newStopReturnRigOn(t, scratchDispatchOptions{natsURL: testnats.URL(t), env: []string{parentStatusRule + "=" + rule}}, 3)

			// Admission's move into work, an ordinary row.
			admitted, grant := r.runningRoot("Pipit ordinary move " + rule)

			// A person's handover of a triage root (a person-handover row), and then admission's
			// move of it into work.
			handed := r.create(asPerson, "Rook handed over "+rule, nil)
			r.addLabel(handed.Key, asPerson, "legion")
			awaitDispatchStatus(t, r.baseURL, handed.Key, "in_progress")

			// The architect's own session parks its root, which the daemon undoes with a re-assert
			// row of the status its record kept.
			session := r.architects[admitted.Key].session
			if status, answer := dispatchAnswerWith(t, r.baseURL, http.MethodPatch, "/api/v1/issues/"+admitted.Key,
				map[string]any{"status": "backlog", "actor": map[string]string{"kind": "session", "id": session}}, bearing(scratchAgentToken)); status != http.StatusOK {
				t.Fatalf("the architect session's park of %s = %d: %s", admitted.Key, status, answer)
			}
			awaitDispatchStatus(t, r.baseURL, admitted.Key, "in_progress")

			// close_root's close, a close row.
			r.closeRoot(admitted.Key, grant, "the human decided no change is needed")
			awaitDispatchStatus(t, r.baseURL, admitted.Key, "done")

			r.statusRowGone(admitted.Key)
			r.statusRowGone(handed.Key)
			logs := r.logs.String()
			for _, refused := range []string{
				"outbox status write refused by Dispatch's parent status rule",
				"outbox status write refused by Dispatch and dropped",
				"outbox status write refused by Dispatch; its architect is told",
			} {
				if strings.Contains(logs, refused) {
					t.Fatalf("the daemon logged %q with the rule %s: a write of its own was refused\n%s", refused, rule, logs)
				}
			}
			for _, key := range []string{admitted.Key, handed.Key} {
				if told := r.notices(key, "message-refused"); len(told) != 0 {
					t.Fatalf("message-refused notices of %s = %+v, want none: no write of the daemon's is refused", key, told)
				}
			}
		})
	}
}
