package launcher

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/memoryguard"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The pids the reaping cases add to controllerTree: direct children of the shim that the shim
// adopted, each younger than Oh My Pi (the fake /proc's start time is the pid's).
const (
	ctlAdopted = 1600 + iota
	ctlAdoptedHelper
	ctlAdoptedSecretsCopy
	ctlAdoptedSecretsRun
	ctlAdoptedSecretsRenew
)

// The controller pod's single-role shim reaps orphans (reapOrphansFlag, which reapingShim gives
// it, as it gives controllerTree's shim), so an orphan of a command Oh My Pi ran becomes a direct
// child of the shim once its parent exits. The classifier spares only the shim's own children
// (shimOwnChildren): Oh My Pi, the oldest direct child that does not run the shim's
// --agent-secrets-bin, and the secrets renewer and keygen, that executable run as
// shimwire.AgentSecretsRenew and shimwire.AgentSecretsKeygen. An adopted orphan is the
// controller's work, as it was when PID 1 adopted it: a dev server; an Oh My Pi helper whose
// parent died; a copy of the secrets binary's name and arguments at another executable; and the
// real secrets binary run as something else. The real secrets binary run as renew is spared:
// that is the rule's stated cost. The plant that spares every direct child of the shim again
// fails the four adopted rows, each read as protected and a session process.
func TestAReapingControllerShimsAdoptedOrphansAreNotSpared(t *testing.T) {
	t.Parallel()
	pod := controllerTree(t, nil)
	pod.add(
		guardProcess{pid: ctlAdopted, ppid: ctlShim, exe: "/usr/bin/node", args: []string{"node", "dev-server.js"}},
		guardProcess{pid: ctlAdoptedHelper, ppid: ctlShim, exe: controllerOmp, args: []string{controllerOmp, "__omp_worker_lsp_mux"}},
		guardProcess{pid: ctlAdoptedSecretsCopy, ppid: ctlShim, exe: "/tmp/agent-secrets", args: []string{"agent-secrets", shimwire.AgentSecretsRenew}},
		guardProcess{pid: ctlAdoptedSecretsRun, ppid: ctlShim, exe: controllerSecrets, args: []string{"agent-secrets", "run", "--", "make"}},
		guardProcess{pid: ctlAdoptedSecretsRenew, ppid: ctlShim, exe: controllerSecrets, args: []string{"agent-secrets", shimwire.AgentSecretsRenew}},
	)
	m, _ := controllerManager()
	classes := newControllerClassifier(m, &pidGuard{pids: map[int]bool{ctlHeld: true}}).classify(scanTable(t, pod))
	for _, tc := range []struct {
		pid             int
		spared, session bool
		what            string
	}{
		{ctlShim, true, true, "the shim"},
		{ctlOmp, true, true, "Oh My Pi, the shim's oldest child"},
		{ctlRenewer, true, true, "the secrets renewer"},
		{ctlKeygen, true, true, "the secrets keygen"},
		{ctlBroker, true, false, "the daemon broker, a helper under Oh My Pi"},
		{ctlAdopted, false, false, "an adopted orphan, a dev server"},
		{ctlAdoptedHelper, false, false, "an adopted Oh My Pi helper whose parent died"},
		{ctlAdoptedSecretsCopy, false, false, "an adopted copy of the secrets binary's name and arguments"},
		{ctlAdoptedSecretsRun, false, false, "the real secrets binary, adopted, run as something else"},
		{ctlAdoptedSecretsRenew, true, true, "the real secrets binary, adopted, run as renew: the rule's cost"},
	} {
		want := memoryguard.Class{Role: "controller", Protected: tc.spared, Session: tc.session}
		if got := classes[tc.pid]; got != want {
			t.Errorf("%s (pid %d): %+v, want %+v", tc.what, tc.pid, got, want)
		}
	}
}
