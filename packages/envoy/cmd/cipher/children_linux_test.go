// packages/envoy/cmd/cipher/children_linux_test.go
//
// The child tracker holds for any child a store starts, not gcloud alone: an `op` stand-in
// started through runChild, whose child holds its output, is killed with its group when a signal
// ends agent-secrets.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/testexec"
)

// childSignalHelper names the pid file of TestChildSignalHelper's stand-in's child; set, the test
// runs as the helper.
const childSignalHelper = "AGENT_SECRETS_CHILD_SIGNAL_HELPER"

// TestChildSignalHelper is the process TestASignalKillsAnyRunningChildsGroup signals: it runs an
// `op` stand-in through runChild, a script whose child names itself in the pid file and holds its
// output, and never answers. It prints RETURNED should runChild return.
func TestChildSignalHelper(t *testing.T) {
	pidFile := os.Getenv(childSignalHelper)
	if pidFile == "" {
		t.Skip("subprocess helper")
	}
	op := filepath.Join(filepath.Dir(pidFile), "op")
	script := "#!/bin/sh\n" +
		`/bin/sh -c 'echo $$ >"` + pidFile + `"; exec sleep 3600' &` + "\n" +
		"wait\n"
	if err := testexec.WriteFile(op, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := runChild(exec.CommandContext(context.Background(), op))
	fmt.Println("RETURNED", err)
}

// TestASignalKillsAnyRunningChildsGroup: an interrupt that ends agent-secrets while a store's
// child runs - here an `op` stand-in, not gcloud - ends it by SIGINT and kills the child's whole
// group, the grandchild that held its output among them.
func TestASignalKillsAnyRunningChildsGroup(t *testing.T) {
	child := gcloudChild{pidFile: filepath.Join(t.TempDir(), "child.pid")}
	t.Cleanup(func() { child.cleanUp(t) })
	cmd := exec.Command("env", "--default-signal=INT", os.Args[0], "-test.run=^TestChildSignalHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), childSignalHelper+"="+child.pidFile)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-waited })
	seen := testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
		pid := child.pid()
		if pid == 0 {
			return false
		}
		running, err := child.runs(pid)
		return err == nil && running
	})
	if !seen {
		t.Fatalf("the stand-in's child was never seen running; the helper said %q", out.String())
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	testwait.Receive(t, "agent-secrets' end by the interrupt while the stand-in ran", waited)
	h := &signInHelper{cmd: cmd, out: &out, waited: waited}
	h.requireEndedByInterrupt(t)
	child.requireGone(t)
}
