package main

import (
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// drainingHelper listens at a fresh socket and answers register DRAINING for its first drains
// calls, then enrolled; it counts the calls. Each connection is served on a goroutine of its own,
// its request read within the longer of the waits its clients give one call: helper.BrokerCallWait
// for enroll --helper, and registerCallWait for register --wait 5, the longest --wait a test gives
// register against it (serveFakeHelper).
func drainingHelper(t *testing.T, drains int32) (string, *atomic.Int32) {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "h.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	serveFakeHelper(t, ln, max(helper.BrokerCallWait, registerCallWait(5)), func(int, *net.UnixConn, []byte) []byte {
		resp := helper.Response{Code: helper.CodeDraining, Error: "the helper is restarting; retry"}
		if calls.Add(1) > drains {
			resp = helper.Response{OK: true, State: "enrolled", RuntimeID: "rt", EnrollmentID: "e"}
		}
		data, _ := json.Marshal(resp)
		return data
	})
	return sock, &calls
}

// TestRegisterRetriesADrainingHelper: a helper that answers DRAINING is about to be restarted, so
// register retries it and registers with the restarted helper rather than giving up at once.
func TestRegisterRetriesADrainingHelper(t *testing.T) {
	sock, calls := drainingHelper(t, 2)
	resp, err := registerWithPatience(sock, helper.Request{WaitSeconds: 5}, 0)
	if err != nil || !resp.OK || resp.State != "enrolled" || calls.Load() != 3 {
		t.Fatalf("register against a helper draining twice = %+v, %v after %d calls; want enrolled on the third", resp, err, calls.Load())
	}
}

// TestRegisterStopsRetryingDrainingAtItsWait: a helper still draining when the --wait budget runs
// out is reported, as before, with its DRAINING code, and not before that budget has passed. The
// upper bound only catches a retry that never stops.
func TestRegisterStopsRetryingDrainingAtItsWait(t *testing.T) {
	sock, _ := drainingHelper(t, 1<<30)
	start := time.Now()
	_, err := registerWithPatience(sock, helper.Request{WaitSeconds: 1}, 0)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), helper.CodeDraining) || elapsed < time.Second || elapsed > 30*time.Second {
		t.Fatalf("register against a helper that stays draining = %v after %s; want DRAINING once its 1 s wait passed", err, elapsed)
	}
}
