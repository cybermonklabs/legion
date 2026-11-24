package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestADashboardProbeNamesEachStatusWithNoPhraseItWrote: a Dispatch, or whatever answers in its
// place, refuses both dashboard streams under a reason phrase that quotes a credential's URL: the
// live socket's handshake and the SSE feed. The probe's error names each status by its code and
// the standard phrase (redact.StatusLine), and none of the phrase the server wrote. Captures
// loadtest.status-line/dashboard.
func TestADashboardProbeNamesEachStatusWithNoPhraseItWrote(t *testing.T) {
	_, _, err := probeDashboard(context.Background(), redacttest.StatusLineServer(t, http.StatusNotFound, "", ""))
	if err == nil {
		t.Fatal("probeDashboard answered no error")
	}
	redacttest.RequireNone(t, "loadtest.status-line/dashboard", err.Error())
	if want := "/ws/live answered 404 Not Found, and GET " + ssePath + " answered 404 Not Found"; !strings.Contains(err.Error(), want) {
		t.Errorf("probeDashboard's error is %q, want %q in it", err, want)
	}
}
