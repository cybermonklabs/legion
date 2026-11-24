package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestABacklogCheckAsksNoStandInALocationNames: whatever answers at the stack's NATS monitor
// answers a backlog run's /varz read with a 302 whose Location names a stand-in on another host.
// The stand-in is asked nothing, and the run is refused by the redirect's own status: the check's
// client follows no redirect (redirects.Refused). The read carries no credential (backlogTarget
// builds the monitor URL from the stack's port alone, and checkNATSServerName gives guardGet no
// bearer), so there is none the stand-in could be sent and no text with one to capture.
func TestABacklogCheckAsksNoStandInALocationNames(t *testing.T) {
	t.Run("followed", func(t *testing.T) {
		target, asked := redirectstest.Target(t)
		monitor := httptest.NewServer(http.RedirectHandler("http://"+target+"/varz", http.StatusFound))
		t.Cleanup(monitor.Close)
		stack := testStack()
		stack.Ports.NATSMonitor = monitor.Listener.Addr().(*net.TCPAddr).Port
		data, err := json.Marshal(stack)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "stack.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runBacklog([]string{"-stack", path, "-sessions", "1"}); err == nil || !strings.Contains(err.Error(), "/varz answered 302") {
			t.Errorf("runBacklog = %v; want the NATS check refused by the redirect's own 302", err)
		}
		if n := asked(); n != 0 {
			t.Errorf("the stand-in the Location names was asked %d times; want none", n)
		}
	})
}
