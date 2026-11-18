// packages/envoy/cmd/agent-secrets-helper/helperversion_test.go
//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestTheHelperAnswersItsRunningVersion: `--version` reads the binary on disk, which after a pin
// move is the next release while the old process still serves the socket. The running helper
// answers the op `version` with the release it was built as, stamped as release_build stamps it
// (TestVersionNamesTheStampedRelease), so a client can say which helper it is talking to. The
// exchange is read as JSON off the socket, the wire every client speaks.
func TestTheHelperAnswersItsRunningVersion(t *testing.T) {
	const tag = "legion-envoy-v0.0.0-test"
	binary := cmdtest.BuildCommand(t, cmdtest.Command{Name: "stamped", Flags: []string{
		"-ldflags", "-X github.com/sjawhar/envoy/internal/buildversion.tag=" + tag,
	}})
	dir := shortTempDir(t)
	operator := filepath.Join(dir, "operator")
	if err := os.WriteFile(operator, []byte("alice@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "run", "helper.sock")
	cmd := exec.Command(binary, "serve")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "AGENT_SECRETS_URL=" + fakeBroker(t).URL,
		"AGENT_SECRETS_HELPER_SOCK=" + sock, "AGENT_SECRETS_OPERATOR_FILE=" + operator}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	testwait.Until(t, "the helper to answer on "+sock, func() (string, bool) {
		resp, err := helper.Call(sock, helper.Request{Op: "sessions"}, time.Second)
		return fmt.Sprintf("%+v %v", resp, err), err == nil && resp.OK
	})

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if deadline, bounded := testwait.Context(t).Deadline(); bounded {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write([]byte(`{"op":"version"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("reading the answer to op version: %v", err)
	}
	var answer struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		Code    string `json:"code"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(line, &answer); err != nil {
		t.Fatalf("the answer %q: %v", line, err)
	}
	if !answer.OK || answer.Version != tag {
		t.Fatalf("op version answered %s; want ok and version %q", line, tag)
	}
}
