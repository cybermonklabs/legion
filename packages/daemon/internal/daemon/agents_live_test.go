//go:build e2e

// The live proof's driver of one key across roles: the owner of an issue gives the steps
// `implementer:a tester:a reviewer`, and a person reads with the real `dispatch` CLI that steps 1 and
// 2 ran in one agent and step 3 in another. scripts/e2e/agents.sh builds this driver with `-tags
// e2e` and owns the nats-server and the PostgreSQL it runs against.
//
// Nothing of the key's path is stood in for. Dispatch is the real server this checkout builds
// (startHandoffDispatch), on a database of its own on LEGION_TEST_PG_DSN's server. The daemon is the
// whole daemon (run, through startDaemonWithin) with dispatch_url at that server, so register_plan
// reads the plan from Dispatch with the daemon's own client, and each step record goes through the
// daemon's outbox to the real route. The issues walk as the package's whole-daemon test of a key
// walks them (walkOneKey, agents_test.go). The reader of the steps is the same CLI as a person of the
// project, with the personal token Dispatch minted for that person (stepRecordsLive).
//
// What is stood in for lies outside the key: GitHub's REST API (stepRecordsGitHub), the Apps'
// tokens, Envoy's listener, and the panes, which the package's fake runtime launches and the driver
// plays, a relaunched pane registering again in the session its claim recorded (relaunchedAs).
package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// TestAgentsLive drives the proof's checks against one booted daemon and one Dispatch, over two
// issues of one project: the first walks `implementer:a tester:a reviewer`, the second, the control,
// `implementer:a tester reviewer`.
func TestAgentsLive(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (newOwnerStepsLive).
	l := newOwnerStepsLive(t, "scripts/e2e/agents.sh", 2)
	keyed, control := l.issues[0].key, l.issues[1].key
	l.note("Dispatch %s (database of its own), project %s mapped to %s, issues %s and %s; daemon at dispatch_url %s",
		l.dispatch.base, l.cfg.Project, liveRepo, keyed, control, l.cfg.DispatchURL)
	l.check("one-key-two-roles", func() {
		l.on(keyed)
		l.oneKeyRead(oneKeyPlan, true)
	})
	l.check("control-no-key-on-step-2", func() {
		l.on(control)
		l.oneKeyRead(oneKeyControlPlan, false)
	})
}

// oneKeyRead walks the walk's issue through steps (walkOneKey), holds its step records to the key's
// (oneKeyHolds), and holds that `dispatch steps --issue <KEY>`, run as the person, prints the agent
// of each step as the records name it: one agent for steps 1 and 2 when keyed, two when not, and
// another for step 3.
func (l *ownerStepsLive) oneKeyRead(steps []string, keyed bool) {
	t := l.t
	t.Helper()
	records := l.walkOneKey(steps, keyed)
	if err := oneKeyHolds(l.ownerStepsRig, records, keyed); err != nil {
		t.Fatal(err)
	}
	l.note("%s walked %s, retro and merging to done; the records name step 1 %s in %s, step 2 %s in %s, step 3 %s in %s",
		l.key, strings.Join(steps, " "), records[0].Agent, records[0].Session, records[1].Agent, records[1].Session, records[2].Agent, records[2].Session)
	var out string
	var agents [3]string
	for deadline := time.Now().Add(stepRecordsWait); ; time.Sleep(time.Second) {
		var code int
		out, code = l.reads.steps()
		if code != 0 {
			t.Fatalf("`dispatch steps --issue %s` exited %d: %s", l.key, code, out)
		}
		printed, err := parseSteps(l.key, out)
		if err == nil {
			err = printedAgents(printed, &agents)
		}
		if err == nil || time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("`dispatch steps --issue %s` printed\n%s\nand %v", l.key, out, err)
			}
			break
		}
	}
	for place, record := range records {
		if agents[place] != record.Agent {
			t.Fatalf("`dispatch steps --issue %s` prints step %d's agent %q, want %q, the record's", l.key, place+1, agents[place], record.Agent)
		}
	}
	if (agents[0] == agents[1]) != keyed || agents[2] == agents[0] {
		t.Fatalf("`dispatch steps --issue %s` prints the agents %q of steps 1 to 3; want steps 1 and 2 one agent = %t, and step 3 another", l.key, agents, keyed)
	}
	l.note("`dispatch steps --issue %s` as the person, exit 0:", l.key)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		l.note("  %s", line)
	}
	if !keyed {
		l.note("control: step 2 ran in %s, an agent of its own beside %s", agents[1], l.token(claim.RoleImplementer))
	}
}

// printedAgents fills agents with the agent `dispatch steps` prints for steps 1 to 3, each of kind
// step and attempt 1; the error names a step it prints no line of, or no agent for.
func printedAgents(printed stepsPrinted, agents *[3]string) error {
	for place := range agents {
		agents[place] = ""
		for _, line := range printed.lines {
			if line.kind == "step" && line.step == place+1 && line.attempt == 1 {
				agents[place] = line.agent
			}
		}
		if agents[place] == "" || agents[place] == "-" {
			return fmt.Errorf("no line of step %d names an agent", place+1)
		}
	}
	return nil
}
