package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/stalledconnect"
)

// A request through Go's shared http.DefaultTransport, which the clean stop's own transport is
// cloned from (TestGosSharedTransportLeavesTheCleanStopToItsBudget), ends on its context, given
// daemon.CleanSessionStopBudget, when its connect never completes. The connect limit sits inside
// the transport's DialContext function, where no test can read it, so the request goes to a
// listener whose accept queue is full (stalledconnect.Listen). A connect limit under the budget,
// set by an init in any package of the daemon's binary, which this package's test binary links,
// ends the request first, after less than the budget and with the dialer's "i/o timeout"; one
// equal to the budget races the context, which starts first. The clock is read before the context
// starts, so time the machine spends between the two counts toward the request and a request that
// ran its whole context never reads as early. The clean stop itself dials with a dialer of its
// own and no limit (notify.stopTransport), so neither reaches it.
func TestAStalledConnectThroughGosSharedTransportLastsTheCleanStopsBudget(t *testing.T) {
	address := stalledconnect.Listen(t)
	began := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), daemon.CleanSessionStopBudget)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/", nil)
	if err != nil {
		t.Fatalf("the request: %v", err)
	}
	response, err := http.DefaultTransport.RoundTrip(request)
	took := time.Since(began)
	if err == nil {
		_ = response.Body.Close()
		t.Fatalf("a request to a listener whose accept queue is full was answered %q, want its connect stalled", response.Status)
	}
	if took < daemon.CleanSessionStopBudget {
		t.Fatalf("a request through http.DefaultTransport whose connect stalled ended after %s with %q, before its context's %s: a connect limit of the transport under the clean stop's budget ended it",
			took, err, daemon.CleanSessionStopBudget)
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("a request through http.DefaultTransport whose connect stalled ended after %s with %q, want its context's deadline, %s, before any limit of the transport",
			took, err, daemon.CleanSessionStopBudget)
	}
}
