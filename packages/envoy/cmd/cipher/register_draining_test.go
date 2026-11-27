package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/helper"
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
	t.Parallel()
	sock, calls := drainingHelper(t, 2)
	resp, err := registerWithPatience(sock, helper.Request{WaitSeconds: 5}, 0)
	if err != nil || !resp.OK || resp.State != "enrolled" || calls.Load() != 3 {
		t.Fatalf("register against a helper draining twice = %+v, %v after %d calls; want enrolled on the third", resp, err, calls.Load())
	}
}

// TestRegisterStopsRetryingDrainingAtItsWait: a helper still draining when the --wait budget runs
// out is reported, as before, with its DRAINING code, and not before that budget has passed. A
// retry that never stops, or one that stops late, is caught by timing the call against its 1 s
// wait, allowing for the time this process was held up (endsByItsDeadline).
func TestRegisterStopsRetryingDrainingAtItsWait(t *testing.T) {
	// Not parallel: it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past (main.endsByItsDeadline, main.requireOnTime, onTimeWithin at waits_test.go:239).
	sock, _ := drainingHelper(t, 1<<30)
	done := make(chan error, 1)
	meter := startStallMeter(t)
	start := time.Now()
	go func() {
		_, err := registerWithPatience(sock, helper.Request{WaitSeconds: 1}, 0)
		done <- err
	}()
	err := endsByItsDeadline(t, meter, "register against a helper that stays draining", start, time.Second, done)
	if elapsed := time.Since(start); err == nil || !strings.Contains(err.Error(), helper.CodeDraining) || elapsed < time.Second {
		t.Fatalf("register against a helper that stays draining = %v after %s; want DRAINING once its 1 s wait passed", err, elapsed)
	}
}
