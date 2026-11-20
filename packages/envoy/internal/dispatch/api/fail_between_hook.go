package api

import (
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/failpoint"
)

// armFailBetween is PUT /api/v1/writes/_test/fail-between/{writer}: it arms one failure for the
// next write of that writer, injected between the writer's store write and its projection write
// (failpoint.Between). The write then fails as any failed statement fails it - its transaction
// rolls back, the store write and the projection write with it, and the event it appended in that
// transaction reaches nobody - which is what c-census reads back of each writer the census names
// (dispatch://INFRA-1152 R9).
//
// It arms the process the call lands on, as the slow-write hook does, and no other: a driver that
// boots one server reaches the one that serves its writes. Test-only: mounted only under
// Deps.TestHooksEnabled.
func (s *server) armFailBetween(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	writer := r.PathValue("writer")
	if !failpoint.Known(writer) {
		writeError(w, "WRITER_INPUT", http.StatusBadRequest,
			"writer must be one of "+strings.Join(failpoint.Writers, ", "))
		return
	}
	if s.failBetween == nil {
		writeError(w, "TEST_HOOKS_OFF", http.StatusServiceUnavailable,
			"this process carries no fail-between set, which DISPATCH_TEST_HOOKS=1 makes")
		return
	}
	s.failBetween.Arm(writer)
	WriteJSON(w, http.StatusOK, testHookOK{OK: true})
}
