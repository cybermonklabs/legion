// packages/envoy/cmd/cipher/identity_version_test.go
//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/helper"
)

// versionedHelper listens at a socket of the test's own and answers each connection by the op of
// the line it reads: `sign` with signAnswer, `version` with versionAnswer, both written as the
// JSON line the helper writes. The answers are raw JSON, the wire itself, so a test states what a
// helper of any release says without this client's own types.
func versionedHelper(t *testing.T, signAnswer, versionAnswer string) string {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "h.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			var req struct {
				Op string `json:"op"`
			}
			_ = json.Unmarshal(line, &req)
			answer := `{"code":"BAD_REQUEST","error":"unknown op ` + req.Op + `"}`
			switch req.Op {
			case "sign":
				answer = signAnswer
			case "version":
				answer = versionAnswer
			}
			_, _ = conn.Write([]byte(answer + "\n"))
			conn.Close()
		}
	}()
	return sock
}

const (
	helperSignsAnswer     = `{"ok":true,"proof":"eyJ.fake.proof"}`
	helperNotASession     = `{"code":"NOT_A_SESSION","error":"pid 5 is not inside a registered machine session"}`
	helperVersionAnswer   = `{"ok":true,"version":"legion-envoy-v0.0.0-test"}`
	olderHelperVersionAns = `{"code":"BAD_REQUEST","error":"unknown op version"}`
)

// TestIdentitySourceNamesTheHelpersVersion: the helper line of `identity --source` names the
// release of the helper process that answers on the socket, read from the helper itself, both
// where the helper is the source and where it only answered.
func TestIdentitySourceNamesTheHelpersVersion(t *testing.T) {
	binary := chainBinary(t)
	rows := []struct {
		name  string
		setup func(t *testing.T, r *chainRig) string // the stdout the row wants
	}{
		{name: "helper session", setup: func(t *testing.T, r *chainRig) string {
			sock := versionedHelper(t, helperSignsAnswer, helperVersionAnswer)
			r.env["AGENT_SECRETS_HELPER_SOCK"] = sock
			return "helper: " + sock + " (agent-secrets-helper legion-envoy-v0.0.0-test)\n"
		}},
		{name: "sign-in and a helper that says not a session", setup: func(t *testing.T, r *chainRig) string {
			writeSignInFixture(t, r, "ada@example.com")
			sock := versionedHelper(t, helperNotASession, helperVersionAnswer)
			r.env["AGENT_SECRETS_HELPER_SOCK"] = sock
			return "sign-in: ada@example.com\nhelper: " + sock + " (agent-secrets-helper legion-envoy-v0.0.0-test)\n"
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
			r := newChainRig(t, binary, broker)
			want := row.setup(t, r)
			code, stdout, stderr := r.run("identity", "--source")
			if code != 0 || stdout != want {
				t.Fatalf("identity --source: exit %d, stdout %q, stderr %q; want exit 0 and stdout %q", code, stdout, stderr, want)
			}
		})
	}
}

// TestIdentitySourceNamesAnOlderHelper: a helper from before the op `version` answers it
// BAD_REQUEST `unknown op version`. The helper line says so and names the binary of the process
// that answered, by its pid on the socket (here the test process, which serves the fake), so a
// reader can see which release is running without trusting the pin.
func TestIdentitySourceNamesAnOlderHelper(t *testing.T) {
	binary := chainBinary(t)
	broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
	r := newChainRig(t, binary, broker)
	sock := versionedHelper(t, helperSignsAnswer, olderHelperVersionAns)
	r.env["AGENT_SECRETS_HELPER_SOCK"] = sock
	want := fmt.Sprintf("helper: %s (a helper from before this release; its binary is /proc/%d/exe)\n", sock, os.Getpid())
	code, stdout, stderr := r.run("identity", "--source")
	if code != 0 || stdout != want {
		t.Fatalf("identity --source: exit %d, stdout %q, stderr %q; want exit 0 and stdout %q", code, stdout, stderr, want)
	}
}

// TestAnOlderHelperIsNamedOnAKernelWithoutPeerPidfd: a kernel before Linux 6.5, and gVisor, do
// not answer SO_PEERPIDFD, which is how helper.PeerOf pins a peer. The older-helper line still
// names the binary of the process on the socket there, by the pid SO_PEERCRED reads, since that
// pid is only shown to a reader. The pin is replaced by one that fails as such a kernel does, so
// this holds on any kernel; the fake helper runs in this test process, whose pid the line names.
func TestAnOlderHelperIsNamedOnAKernelWithoutPeerPidfd(t *testing.T) {
	sock := versionedHelper(t, helperSignsAnswer, olderHelperVersionAns)
	noPidfd := func(*net.UnixConn) (*helper.Peer, error) {
		return nil, fmt.Errorf("SO_PEERPIDFD: %w", syscall.ENOPROTOOPT)
	}
	want := fmt.Sprintf(" (a helper from before this release; its binary is /proc/%d/exe)", os.Getpid())
	if got := versionSuffix(sock, noPidfd); got != want {
		t.Fatalf("versionSuffix with no SO_PEERPIDFD: %q, want %q", got, want)
	}
}
