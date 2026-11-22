package daemon

import (
	"bytes"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/stalledconnect"
)

// A clean stop whose connect to the listener never completes ends on its own context deadline,
// CleanSessionStopBudget, and not on a connect limit of the transport. The stop's client sends
// through a transport of its own whose dialer has no limit (notify.stopTransport), where Go's
// shared transport gives a connect 30 s. A dialer's limit sits inside the transport's DialContext
// function, where no test can read it as a field, so this test stalls a connect instead: the stop
// is sent to a listener whose accept queue is full, so its connect is neither answered nor refused
// (stalledconnect.Listen). A connect limit under the budget would end the stop first, with the
// dialer's "i/o timeout", and one equal to it would race the stop's context, which starts first;
// with no deadline on the stop's context the connect would hang until the test's bound. The stop
// must run its whole budget and log its context's deadline instead (awaitStopOnItsDeadline).
// notify's TestTheCleanStopsTransportKeepsNoLimitOfTheSharedOne shows that a limit set on the
// shared transport, of 2 s, 4.9 s or the budget itself, does not reach the stop's own.
func TestCleanSessionStopContextEndsBeforeAStalledConnect(t *testing.T) {
	address := stalledconnect.Listen(t)
	var inFlight sessionStopsInFlight
	var logged bytes.Buffer
	stop := cleanSessionStop(&inFlight, "http://"+address, "envoy-token", sessionStopOverrides{}, bufferedLogger(&logged))
	began := time.Now()
	awaitStopOnItsDeadline(t, stop("ses_dead"), began, &logged, "connect")
}
