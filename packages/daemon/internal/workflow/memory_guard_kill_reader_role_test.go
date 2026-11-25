package workflow

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// dispatch://INFRA-3686's R4: "A card for a message from another session, and a card for a daemon
// notice or role message, carries no reply command, no session id or role of the reader". The
// memory-guard-kill notice, which main's #4273 added and this branch's merge of main 054cd5ca made
// a record, reaches the controller alone, on its project's controller topic, and the card prints
// each field of the payload the outbox publishes there (record.Notice, the daemon's
// controllerNotice). Main's notice carried `role: controller` beside its reason; this branch took
// main's `role: architect` out of the checks-red notice for R4 (verification c-card-compact: "no
// field that names the reader's role"), and keeps `role: controller` here, on every cause. The
// controller's prompt names every other field of the notice and not this one: the process is
// "one of your own processes" in every text of the kind.
func TestAMemoryGuardKillNoticeCarriesNoRoleOfItsReader(t *testing.T) {
	for _, cause := range []shimwire.MemoryKillCause{shimwire.MemoryKillCauseGuard, shimwire.MemoryKillCauseKernel,
		shimwire.MemoryKillCauseKernelProtected} {
		report := shimwire.LauncherMemoryKill{Guard: "guard-7f3a", Sequence: 3, PID: 4242, Executable: "python3", SizeBytes: 851862323,
			ReadingBytes: 3360836485, CounterBytes: 3560836485, LimitBytes: 4294967296, Cause: cause}
		notice, err := record.ControllerNoticeOf(ControllerMemoryGuardKillNotice("LEGION", report))
		if err != nil {
			t.Fatalf("cause %q: the memory-guard-kill record is no controller notice: %v", cause, err)
		}
		payload, err := json.Marshal(record.Notice(notice))
		if err != nil {
			t.Fatalf("cause %q: encode the published notice: %v", cause, err)
		}
		var fields map[string]any
		if err := json.Unmarshal(payload, &fields); err != nil {
			t.Fatalf("cause %q: decode the published notice %s: %v", cause, payload, err)
		}
		for key, value := range fields {
			if value == string(claim.RoleController) {
				t.Errorf("cause %q: the notice the controller's card prints carries %s: %v, the role of its reader: %s", cause, key, value, payload)
			}
		}
	}
}
