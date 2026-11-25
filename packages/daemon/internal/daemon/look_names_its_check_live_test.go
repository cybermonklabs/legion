//go:build e2e

// The production look's report goes through executePhaseOp and its real daemon client under
// bun, against a whole booted daemon and a real Dispatch of this checkout. The check identity
// comes from the task the fake runtime took, never from the store. Both outcomes are read
// through the operator's own `legion state --json` command.
package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

func TestLookNamesItsCheckLive(t *testing.T) {
	fixtureenv.Live(t)
	proof := newLiveProof(t, "scripts/e2e/look-names-its-check.sh")
	proof.cfg.ReviewRoundCap = 3
	root := liveCLICheckout(t)
	// Dispatch's events reach the daemon: a status write waits for them, and the merge's close
	// lands only on Dispatch's done (R20 and R24 of dispatch://INFRA-3320), which the look waits for.
	rig := newOwnerStepsRig(t, proof, startHandoffDispatch(t, root, proof.cfg.NatsURLs[0]).scratchDispatch, 1,
		config.Project{ProductionCheck: claim.RoleTester})
	l := &ownerStepsClientLive{ownerStepsRig: rig, root: root}
	binary := filepath.Join(t.TempDir(), "legion")
	build := exec.Command("go", "build", "-o", binary, "./cmd/legion")
	build.Dir = filepath.Join(root, "packages/daemon")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build legion: %v\n%s", err, out)
	}
	home := t.TempDir()
	state := func() api.Issue {
		t.Helper()
		command := exec.Command(binary, "state", "--json")
		command.Dir = home
		command.Env = append(os.Environ(), "HOME="+home, "LEGION_DAEMON_URL="+rig.d.base)
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("legion state --json: %v\n%s", err, out)
		}
		var got api.State
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("decode legion state --json: %v\n%s", err, out)
		}
		issue, ok := got.Issues[rig.key]
		if !ok {
			t.Fatalf("legion state --json enumerates no issue %s", rig.key)
		}
		return issue
	}
	l.givePlan("implementer", "tester")
	l.openGate(phase.Implementing)
	l.walkSteps([]string{"implementer", "tester"})
	tester := l.worker(claim.RoleTester)
	opened := l.lookOpens(tester)
	l.check("control-client-look-unnamed", func() {
		outcome := l.client(root, tester, "handoff_complete", map[string]any{"summary": "looked", "verdict": "pass"}, nil)
		if err := refused(outcome, http.StatusConflict, "CHECK_LOOK_NOT_SERVED"); err != nil {
			t.Fatal(err)
		}
		got := state()
		if got.Phase != phase.Done || got.Check == nil || got.Check.Looks != 1 || !got.Check.LookOpen || !got.Check.OpenedAt.Equal(opened) {
			t.Fatalf("after unnamed report: phase %s, check %+v; want the named issue's first look still open", got.Phase, got.Check)
		}
		l.note("unnamed report: %s; legion state --json: look 1 still open at %s", outcome, got.Check.OpenedAt.Format(time.RFC3339Nano))
	})
	l.check("client-look-named", func() {
		wake := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Microsecond)
		outcome := l.client(root, tester, "handoff_complete", map[string]any{"summary": "the deploy has not run", "verdict": "pending",
			"wake_at": wake.Format(time.RFC3339Nano), "check_opened_at": opened.Format(time.RFC3339Nano)}, nil)
		if err := stopTaken(outcome); err != nil {
			t.Fatalf("named look's report: %v", err)
		}
		got := state()
		if got.Phase != phase.Done || got.Check == nil || got.Check.Looks != 1 || got.Check.LookOpen || !got.Check.OpenedAt.Equal(opened) || !got.Check.NextWakeAt.Equal(wake) {
			t.Fatalf("after named report: phase %s, check %+v; want look 1 retired on its open check with next wake %s", got.Phase, got.Check, wake)
		}
		l.note("named report: %s; legion state --json: check open, lookOpen false, nextWakeAt %s", outcome, got.Check.NextWakeAt.Format(time.RFC3339Nano))
	})
}
