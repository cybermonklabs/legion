package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/testwait"
)

// drainingHelper listens at a fresh socket and answers register DRAINING for its first drains
// calls, then enrolled; it counts the calls.
func drainingHelper(t *testing.T, drains int32) (string, *atomic.Int32) {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "h.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var calls atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(conn).ReadBytes('\n')
			resp := helper.Response{Code: helper.CodeDraining, Error: "the helper is restarting; retry"}
			if calls.Add(1) > drains {
				resp = helper.Response{OK: true, State: "enrolled", RuntimeID: "rt", EnrollmentID: "e"}
			}
			data, _ := json.Marshal(resp)
			_, _ = conn.Write(append(data, '\n'))
			_ = conn.Close()
		}
	}()
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
// out is reported, as before, with its DRAINING code, after more than the one call a register
// that gave up at once would have made. A retry that never stopped would hold the shared wait
// below until the test binary's deadline, which fails naming it.
func TestRegisterStopsRetryingDrainingAtItsWait(t *testing.T) {
	sock, calls := drainingHelper(t, 1<<30)
	answered := make(chan error, 1)
	go func() {
		_, err := registerWithPatience(sock, helper.Request{WaitSeconds: 1}, 0)
		answered <- err
	}()
	err := testwait.Receive(t, "register to stop retrying a helper that stays draining", answered)
	if err == nil || !strings.Contains(err.Error(), helper.CodeDraining) {
		t.Fatalf("register against a helper that stays draining = %v; want DRAINING once its 1 s wait passed", err)
	}
	if n := calls.Load(); n < 2 {
		t.Fatalf("the helper saw %d register call(s); want the 1 s wait spent on more than the first", n)
	}
}
