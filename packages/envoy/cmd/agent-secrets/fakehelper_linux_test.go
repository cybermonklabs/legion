// packages/envoy/cmd/agent-secrets/fakehelper_linux_test.go
//go:build linux

// The contract every fake helper of this package keeps (serveFakeHelper): a connection that
// sends no request holds only itself. Linux-only because three of the fakes are.
package main

import (
	"errors"
	"net"
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// TestEachFakeHelperAnswersPastAConnectionThatSendsNothing: behind a connection that sends no
// request, the call a fake's client makes, with that client's own wait, still gets the fake's
// answer, or from hangUpHelper its hang-up. fakeHelperAnswering's own case is
// TestFakeHelperAnswersPastAConnectionThatSendsNothing. silentHelperAt is not here: it answers no
// connection by design, and reads none, so no connection of it waits behind another.
func TestEachFakeHelperAnswersPastAConnectionThatSendsNothing(t *testing.T) {
	for _, row := range []struct {
		name  string
		start func(t *testing.T) string
		// call is the call the fake's client makes, with that client's wait; it fails t unless the
		// fake answered it.
		call func(t *testing.T, sock string)
	}{
		{"hangUpHelper", hangUpHelper, func(t *testing.T, sock string) {
			_, err := helper.Call(sock, helper.Request{Op: "enroll-session-key"}, helper.BrokerCallWait)
			if !errors.Is(err, helper.ErrNoReply) || errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("enroll-session-key = %v; want the fake's hang-up (no reply), not the call's timeout", err)
			}
		}},
		{"fakeHelperWithPids", func(t *testing.T) string {
			sock, _ := fakeHelperWithPids(t, helper.Response{OK: true, State: "enrolling", RuntimeID: "session-behind"})
			return sock
		}, func(t *testing.T, sock string) {
			if resp, err := helper.Call(sock, helper.Request{Op: "register"}, registerCallWait(0)); err != nil || resp.RuntimeID != "session-behind" {
				t.Fatalf("register = %+v, %v; want the fake's answer", resp, err)
			}
		}},
		{"versionedHelper", func(t *testing.T) string {
			return versionedHelper(t, helperSignsAnswer, helperVersionAnswer)
		}, func(t *testing.T, sock string) {
			if got, want := versionSuffix(sock, helper.PeerOf), " (agent-secrets-helper legion-envoy-v0.0.0-test)"; got != want {
				t.Fatalf("identity's version question = %q; want %q, the fake's answer", got, want)
			}
		}},
		{"fakeLoginHelper", func(t *testing.T) string {
			return fakeLoginHelper(t, "KQ7M-X4PZ", []string{"issued"})
		}, func(t *testing.T, sock string) {
			if resp, err := helper.Call(sock, helper.Request{Op: "login-status"}, loginStatusWait); err != nil || resp.LoginState != "issued" {
				t.Fatalf("login-status = %+v, %v; want the fake's answer", resp, err)
			}
		}},
		{"drainingHelper", func(t *testing.T) string {
			sock, _ := drainingHelper(t, 0)
			return sock
		}, func(t *testing.T, sock string) {
			if resp, err := registerWithPatience(sock, helper.Request{}, 0); err != nil || resp.State != "enrolled" {
				t.Fatalf("register = %+v, %v; want the fake's answer", resp, err)
			}
		}},
		{"fakeHelperByOp", func(t *testing.T) string {
			sock, _ := fakeHelperByOp(t, map[string]helper.Response{"enroll-session-key": {OK: true, EnrollmentID: "enr-behind"}})
			return sock
		}, func(t *testing.T, sock string) {
			if resp, err := helper.Call(sock, helper.Request{Op: "enroll-session-key"}, helper.BrokerCallWait); err != nil || resp.EnrollmentID != "enr-behind" {
				t.Fatalf("enroll-session-key = %+v, %v; want the fake's answer", resp, err)
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			sock := row.start(t)
			silent, err := net.Dial("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			defer silent.Close()
			row.call(t, sock)
		})
	}
}
