package supervise

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// endGateRootMoved is why a process the gate-root lane relaunches is let go, in the daemon's
// words, which its next process is told (RelaunchSentence).
const endGateRootMoved = "its issue now stands under another gate root, so it was started again in its session to register that one"

// GateRootMoved is the daemon's pass finding the claim's process launched under another gate root
// than the one its issue stands under on the Dispatch chain now (Claim.LaunchTree, decision 13 of
// dispatch://INFRA-4730): its issue's parent changed, or its children were unlinked from it, and
// the root of the gate above it is another issue. Tree is that gate root. The machine relaunches
// the process in its own session under it (relaunchUnderGateRoot), so the role's LEGION_TREE is
// the new gate root and its plugin registers again (R22 of dispatch://INFRA-3320).
type GateRootMoved struct {
	Claim claim.Token
	Tree  string
}

func (GateRootMoved) isEvent() {}

// relaunchUnderGateRoot is the GateRootMoved event's row, for the four states a claim holds its
// process's capability in.
func relaunchUnderGateRoot(m *Machine, ctx context.Context, ev Event) error {
	return m.relaunchUnder(ctx, ev.(GateRootMoved).Tree)
}

// relaunchUnder replaces the claim's process in its own session under tree, the gate root its
// issue stands under now, once nothing the process does is cut: the process is resumed from its
// recorded session on a new process whose launch names tree (Claim.LaunchTree, spawnSpec), the
// same replacement the stale-plugin lane makes (replaceProcess), charged nothing, since the
// process did nothing wrong.
//
// A claim whose launch already names tree is decided nothing, so the pass's re-posts at every
// sweep are free. A claim with a launch already waiting its turn in the wake pacing has the tree
// written for that launch, which reads it when it runs. So does a claim whose record says its
// process died (Claim.DeathCharged, over the process it records), whose death owes a relaunch a
// restart forgets: the boot's replay of that death (RecordedDeath) or its next report raises it,
// and nothing is replaced here, since a replacement would launch outside the boot's owed order
// and charge the death nothing. A claim in a turn keeps its process until the turn ends
// (afterTurn), because what that turn has not pushed would be lost with it, and the relaunch runs
// then. A claim with no process of its own has nothing to replace: its next start writes the gate
// root for its next launch (Machine.MarkLaunch). A relaunch the lifecycle recheck refuses, or one
// that ends with no launch at all, leaves the launch tree as it found it, so the next sweep finds
// the claim again.
func (m *Machine) relaunchUnder(ctx context.Context, tree string) error {
	if tree == "" || m.claim.LaunchedTree() == tree {
		m.retree = ""
		return nil
	}
	if m.wake != nil || m.claim.DeathCharged && m.claim.Locator != nil {
		m.claim.LaunchTree = tree
		return m.persist(ctx)
	}
	if !HoldsCapability(m.claim.State) {
		return nil
	}
	if m.claim.State == StateWorking && !claim.IsController(m.claim.Role, m.claim.Issue, m.claim.Tree) {
		if m.retree == "" {
			m.log.Info("supervise: the process was launched under another gate root; its relaunch in its session waits for the agent's turn to end",
				"launch_tree", m.claim.LaunchedTree(), "gate_root", tree)
		}
		m.retree = tree
		return nil
	}
	if m.held == nil {
		if err := m.checkLaunch(ctx); err != nil {
			return err
		}
	}
	m.log.Info("supervise: the process was launched under another gate root; relaunching it in its session under its issue's gate root",
		"incarnation", m.claim.Locator.Incarnation, "launch_tree", m.claim.LaunchedTree(), "gate_root", tree)
	previous := m.claim.LaunchTree
	m.claim.LaunchTree = tree
	end, err := m.replaceProcess(ctx, "the process launched under another gate root is replaced mid-turn", ordinaryEnd, endGateRootMoved)
	if end == EndNotMade {
		m.claim.LaunchTree = previous
	}
	return err
}
