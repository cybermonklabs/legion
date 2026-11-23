//go:build linux

// packages/envoy/cmd/cipher/secret_gcp_supervisor_linux_test.go
//
// How a gcloud command is set up to start under its supervisor on Linux (superviseGcloud), and
// when a run of this binary is one. The rows that kill agent-secrets and watch gcloud end are
// secret_gcp_linux_test.go's.
package main

import (
	"bytes"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
)

// TestGcloudStartsUnderASupervisorOrWithSIGKILL: a gcloud command that can be found starts this
// binary, through /proc/self/exe, as its supervisor, with SIGTERM as the supervisor's parent-death
// signal, which it handles by killing the group; one that cannot be found is left to Start's own
// answer, and starts, should it ever, with SIGKILL as its own parent-death signal, as before the
// supervisor, since a gcloud may handle SIGTERM and live on.
func TestGcloudStartsUnderASupervisorOrWithSIGKILL(t *testing.T) {
	found := exec.Command("/bin/true", "auth", "list")
	if !superviseGcloud(found) {
		t.Fatal("superviseGcloud did not rewrite a gcloud that can be found")
	}
	if want := []string{"/bin/true", "/bin/true", "auth", "list"}; found.Path != selfExe || !slices.Equal(found.Args, want) || !slices.Contains(found.Env, gcloudSupervisorEnv+"=1") {
		t.Errorf("the supervised command runs %q %q with %q in its environment; want %q %q with %s=1", found.Path, found.Args, gcloudSupervisorEnv, selfExe, want, gcloudSupervisorEnv)
	}
	if attr := gcloudProcAttr(true); !attr.Setpgid || attr.Pdeathsig != syscall.SIGTERM {
		t.Errorf("the supervisor starts with %+v; want its own group and SIGTERM as its parent-death signal", attr)
	}
	missing := exec.Command("agent-secrets-test-no-such-gcloud")
	if missing.Err == nil {
		t.Fatal("control: exec.Command found agent-secrets-test-no-such-gcloud")
	}
	path := missing.Path
	if superviseGcloud(missing) || missing.Path != path || missing.Env != nil {
		t.Errorf("superviseGcloud rewrote a gcloud that cannot be found: path %q, environment %q", missing.Path, missing.Env)
	}
	if attr := gcloudProcAttr(false); !attr.Setpgid || attr.Pdeathsig != syscall.SIGKILL {
		t.Errorf("an unsupervised gcloud starts with %+v; want its own group and SIGKILL as its parent-death signal", attr)
	}
}

// TestTheGcloudSupervisorMarkerActsOnlyInAGroupLeader: a run of this binary with
// gcloudSupervisorEnv set is a gcloud supervisor only where it leads its own process group, as
// superviseGcloud starts one; elsewhere, the marker strayed into its environment, it runs as
// itself. A supervisor answers 128 plus the number of the signal that ended gcloud, here SIGUSR1,
// which a supervisor that re-raised the signal would have ignored.
func TestTheGcloudSupervisorMarkerActsOnlyInAGroupLeader(t *testing.T) {
	env := append(os.Environ(), gcloudSupervisorEnv+"=1")
	stray := exec.Command(os.Args[0], "-test.run=^$", "-test.count=1")
	stray.Env = env
	out, err := stray.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("PASS")) {
		t.Errorf("this test binary, with the marker set and not leading its group, ended %v, saying %q; want it ran as a test binary, PASS", err, out)
	}
	leader := exec.Command(os.Args[0], "/bin/sh", "-c", "kill -USR1 $$")
	leader.Env = env
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err = leader.CombinedOutput()
	if want := 128 + int(syscall.SIGUSR1); leader.ProcessState == nil || leader.ProcessState.ExitCode() != want {
		t.Errorf("this test binary, with the marker set and leading its group, ended %v, saying %q; want a supervisor's status %d for a gcloud ended by SIGUSR1", err, out, want)
	}
}
