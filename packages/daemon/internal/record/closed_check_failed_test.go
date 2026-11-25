package record

import (
	"testing"
	"time"
)

// A fail recorded after an issue closed by its checks is the controller's to route, as any defect
// is, and no architect's: the issue's tree has closed. So closed-check-failed is a controller-only
// kind. It is queued as a controller notice and refused as an architect's.
func TestTheClosedCheckFailedNoticeIsTheControllersAlone(t *testing.T) {
	if !controllerOnlyNoticeKind(ClosedCheckFailedNotice) {
		t.Fatalf("the %s kind is not controller-only, so a notice of it would be queued to an architect's topic", ClosedCheckFailedNotice)
	}
	if validNoticeKind(ClosedCheckFailedNotice) {
		t.Fatalf("the %s kind is on the architect's own list, so an architect would be told a fail after its tree closed", ClosedCheckFailedNotice)
	}
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	reason := "ACME-1 closed by its checks, and check c-done then recorded a fail: result result-9."
	if _, err := NewOutboxRow("ACME-1", ControllerNotice{Kind: ClosedCheckFailedNotice, Reason: reason}, at); err != nil {
		t.Fatalf("the controller notice the daemon writes is refused: %v", err)
	}
	if _, err := NewOutboxRow("ACME-1", Notice{Kind: ClosedCheckFailedNotice, Reason: reason}, at); err == nil {
		t.Fatal("the notice is taken as an architect's notice, and no architect of a closed tree reads it")
	}
}
