//go:build e2e

// The live proof's driver of the agents of an owner's plan: the owner of an issue gives the steps
// `implementer:a tester:a reviewer`, and a person reads with the real `dispatch` CLI that steps 1 and
// 2 ran in one agent and step 3 in another; the owner of another gives `implementer:a reviewer
// implementer:a`, and the person reads that steps 1 and 3 ran in one agent and step 2 in another;
// the owner of a third gives `implementer reviewer implementer`, and the person reads that step 3
// ran in a second agent of the role implementer, `…-implementer-2`, beside the agent of step 1.
// scripts/e2e/agents.sh builds this driver with `-tags e2e` and owns the nats-server and the
// PostgreSQL it runs against.
//
// Nothing of the key's path is stood in for. Dispatch is the real server this checkout builds
// (startHandoffDispatch), on a database of its own on LEGION_TEST_PG_DSN's server. The daemon is the
// whole daemon (run, through startDaemonWithin) with dispatch_url at that server, so register_plan
// reads the plan from Dispatch with the daemon's own client, and each step record goes through the
// daemon's outbox to the real route. The issues walk as the package's whole-daemon tests walk them
// (walkOneKey, walkRoleTwice and walkSecondAgent, agents_test.go). The reader of the steps is the
// same CLI as a person of the project, with the personal token Dispatch minted for that person
// (stepRecordsLive).
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

// TestAgentsLive drives the proof's checks against one booted daemon and one Dispatch, over five
// issues of one project: the first walks `implementer:a tester:a reviewer`, the second, its
// control, `implementer:a tester reviewer`; the third walks `implementer:a reviewer implementer:a`
// through a failed review, and the fourth, its control, `implementer:a reviewer implementer`,
// whose step 3 is a second agent of the role implementer; the fifth walks `implementer reviewer
// implementer`, a second agent of one role with no key anywhere.
func TestAgentsLive(t *testing.T) {
	l := newOwnerStepsLive(t, "scripts/e2e/agents.sh", 5)
	keyed, control, twice, unkeyed, second := l.issues[0].key, l.issues[1].key, l.issues[2].key, l.issues[3].key, l.issues[4].key
	l.note("Dispatch %s (database of its own), project %s mapped to %s, issues %s, %s, %s, %s and %s; daemon at dispatch_url %s",
		l.dispatch.base, l.cfg.Project, liveRepo, keyed, control, twice, unkeyed, second, l.cfg.DispatchURL)
	l.check("one-key-two-roles", func() {
		l.on(keyed)
		l.oneKeyRead(oneKeyPlan, true)
	})
	l.check("control-no-key-on-step-2", func() {
		l.on(control)
		l.oneKeyRead(oneKeyControlPlan, false)
	})
	l.check("role-two-times", func() {
		l.on(twice)
		l.roleTwiceRead()
	})
	l.check("control-no-key-on-step-3", func() {
		l.on(unkeyed)
		l.secondAgentRead(roleTwiceUnkeyedPlan)
	})
	l.check("second-agent", func() {
		l.on(second)
		l.secondAgentRead(secondAgentPlan)
	})
}

// oneKeyRead walks the walk's issue through steps (walkOneKey), holds its step records to the key's
// (oneKeyHolds), and holds that `dispatch steps --issue <KEY>`, run as the person, prints the agent
// of each step as the records name it (printedFirstRuns): with oneKeyHolds, one agent for steps 1
// and 2 when keyed, two when not, and another for step 3.
func (l *ownerStepsLive) oneKeyRead(steps []string, keyed bool) {
	t := l.t
	t.Helper()
	records := l.walkOneKey(steps, keyed)
	if err := oneKeyHolds(l.ownerStepsRig, records, keyed); err != nil {
		t.Fatal(err)
	}
	l.note("%s walked %s, retro and merging to done; the records name step 1 %s in %s, step 2 %s in %s, step 3 %s in %s",
		l.key, strings.Join(steps, " "), records[0].Agent, records[0].Session, records[1].Agent, records[1].Session, records[2].Agent, records[2].Session)
	agents := l.printedFirstRuns(records[:])
	if !keyed {
		l.note("control: step 2 ran in %s, an agent of its own beside %s", agents[1], l.token(claim.RoleImplementer))
	}
}

// roleTwiceRead walks the walk's issue through `implementer:a reviewer implementer:a` and a failed
// review (walkRoleTwice), holds its step records to one key's at steps 1 and 3 (roleTwiceHolds),
// and holds that `dispatch steps --issue <KEY>`, run as the person, prints the agent of each step's
// first run as the records name it (printedFirstRuns): with roleTwiceHolds, one agent for steps 1
// and 3, and another for step 2.
func (l *ownerStepsLive) roleTwiceRead() {
	t := l.t
	t.Helper()
	records := l.walkRoleTwice()
	if err := roleTwiceHolds(l.ownerStepsRig, records); err != nil {
		t.Fatal(err)
	}
	for i, run := range roleTwiceRuns {
		l.note("%s: run %d of step %d names %s in %s", l.key, run.attempt, run.step, records[i].Agent, records[i].Session)
	}
	first := map[int]stepRecordRead{}
	for i, run := range roleTwiceRuns {
		if run.attempt == 1 {
			first[run.step] = records[i]
		}
	}
	l.printedFirstRuns([]stepRecordRead{first[1], first[2], first[3]})
}

// secondAgentRead walks the walk's issue through steps, `implementer reviewer implementer` with or
// without a key on step 1 (walkSecondAgent), holds its step records to three agents, step 3's the
// second agent of the role implementer, `…-implementer-2`, each in a session of its own
// (secondAgentHolds), and holds that `dispatch steps --issue <KEY>`, run as the person, prints the
// agent of each step as the records name it: three agents.
func (l *ownerStepsLive) secondAgentRead(steps []string) {
	t := l.t
	t.Helper()
	records := l.walkSecondAgent(steps)
	if err := secondAgentHolds(l.ownerStepsRig, records); err != nil {
		t.Fatal(err)
	}
	l.note("%s walked %s, retro and merging to done; the records name step 1 %s in %s, step 2 %s in %s, step 3 %s in %s",
		l.key, strings.Join(steps, " "), records[0].Agent, records[0].Session, records[1].Agent, records[1].Session, records[2].Agent, records[2].Session)
	agents := l.printedFirstRuns(records)
	if agents[0] == agents[1] || agents[2] == agents[0] || agents[2] == agents[1] {
		t.Fatalf("`dispatch steps --issue %s` prints the agents %q of steps 1 to 3; want three agents", l.key, agents)
	}
}

// printedFirstRuns runs `dispatch steps --issue <KEY>` as the person until it prints an agent for
// the first run of each of steps 1 to 3, holds each to the agent records names for that step, which
// is the record of its first run, notes what the command printed and answers the three agents.
func (l *ownerStepsLive) printedFirstRuns(records []stepRecordRead) [3]string {
	t := l.t
	t.Helper()
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
	l.note("`dispatch steps --issue %s` as the person, exit 0:", l.key)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		l.note("  %s", line)
	}
	return agents
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
