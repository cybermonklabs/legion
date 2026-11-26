package launcher

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/memoryguard"
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

// controllerSecrets is the agent-secrets executable of controllerTree's renewer and keygen.
const controllerSecrets = "/usr/local/bin/agent-secrets"

// The controller pod's single-role shim reaps orphans (reapOrphansFlag, which reapingShim gives
// it), so an orphan of a command Oh My Pi ran becomes a direct child of the shim once its parent
// exits. The classifier spares only the shim's own children there (shimOwnChildren): Oh My Pi, the
// oldest direct child that does not run the shim's --agent-secrets-bin, and the secrets renewer and
// keygen, that executable run as renew and keygen. An adopted orphan is the controller's work, as
// it was when PID 1 adopted it: a dev server; an Oh My Pi helper whose parent died; a copy of the
// secrets binary's name and arguments at another executable; and the real secrets binary run as
// something else. The real secrets binary run as renew is spared: that is the rule's stated cost.
//
// The control is the same table with a shim that reaps none, the controller's shim before this
// rule: there every direct child of the shim is spared and a session process, the adopted orphans
// with them, which shows the reaping rows read the rule and not an orphan the table lacks.
func TestAReapingControllerShimsAdoptedOrphansAreNotSpared(t *testing.T) {
	t.Parallel()
	for _, reaping := range []bool{true, false} {
		pod := controllerTree(t, nil)
		args := []string{"legion", shimArgument}
		if reaping {
			args = append(args, reapOrphansFlag)
		}
		args = append(args, "--connect", "tcp://daemon", "--agent-secrets-bin", controllerSecrets, "--", controllerOmp)
		pod.add(
			guardProcess{pid: ctlShim, ppid: os.Getpid(), exe: launcherExe(t), args: args},
			guardProcess{pid: ctlAdopted, ppid: ctlShim, exe: "/usr/bin/node", args: []string{"node", "dev-server.js"}},
			guardProcess{pid: ctlAdoptedHelper, ppid: ctlShim, exe: controllerOmp, args: []string{controllerOmp, "__omp_worker_lsp_mux"}},
			guardProcess{pid: ctlAdoptedSecretsCopy, ppid: ctlShim, exe: "/tmp/agent-secrets", args: []string{"agent-secrets", "renew"}},
			guardProcess{pid: ctlAdoptedSecretsRun, ppid: ctlShim, exe: controllerSecrets, args: []string{"agent-secrets", "run", "--", "make"}},
			guardProcess{pid: ctlAdoptedSecretsRenew, ppid: ctlShim, exe: controllerSecrets, args: []string{"agent-secrets", "renew"}},
		)
		m, _ := controllerManager()
		classes := newControllerClassifier(m, &pidGuard{pids: map[int]bool{ctlHeld: true}}).classify(scanTable(t, pod))
		for _, tc := range []struct {
			pid int
			// spared is the class the row has when the shim reaps orphans; with a shim that reaps
			// none every row here is a direct child of the shim, or a helper under Oh My Pi, and
			// spared, and each direct child is a session process.
			spared, session, child bool
			what                   string
		}{
			{ctlShim, true, true, false, "the shim"},
			{ctlOmp, true, true, true, "Oh My Pi, the shim's oldest child"},
			{ctlRenewer, true, true, true, "the secrets renewer"},
			{ctlKeygen, true, true, true, "the secrets keygen"},
			{ctlBroker, true, false, false, "the daemon broker, a helper under Oh My Pi"},
			{ctlAdopted, false, false, true, "an adopted orphan, a dev server"},
			{ctlAdoptedHelper, false, false, true, "an adopted Oh My Pi helper whose parent died"},
			{ctlAdoptedSecretsCopy, false, false, true, "an adopted copy of the secrets binary's name and arguments"},
			{ctlAdoptedSecretsRun, false, false, true, "the real secrets binary, adopted, run as something else"},
			{ctlAdoptedSecretsRenew, true, true, true, "the real secrets binary, adopted, run as renew: the rule's cost"},
		} {
			want := memoryguard.Class{Role: "controller", Protected: tc.spared, Session: tc.session}
			if !reaping {
				want = memoryguard.Class{Role: "controller", Protected: true, Session: tc.session || tc.child}
			}
			if got := classes[tc.pid]; got != want {
				t.Errorf("reaping %t: %s (pid %d): %+v, want %+v", reaping, tc.what, tc.pid, got, want)
			}
		}
	}
}
