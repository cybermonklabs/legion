package supervise

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// endGateRootMoved is why a process the gate-root lane relaunches is let go, in the daemon's
// words, which its next process is told (RelaunchSentence).
const endGateRootMoved = "its issue now stands elsewhere on its Dispatch chain (under another gate root, holding or not holding a design gate of its own, " +
	"or as its tree's root or not), so it was started again in its session to take that up"

// GateRootMoved is the daemon's pass finding the claim's process launched under another gate root
// than the one its issue stands under on the Dispatch chain now (Claim.LaunchTree, decision 13 of
// dispatch://INFRA-4730), with another answer to whether its issue holds its own design gate
// (Claim.HoldsGate), or, for an architect, with another answer to whether it is its tree's root
// architect (Claim.LaunchedRoot): its issue's parent changed, its children were unlinked from it,
// or the issue above it left Legion's work, so the root of the gate above it is another issue,
// or the issue itself, and the architect that owns it may be its own. Tree is that gate root,
// HoldsGate whether the issue holds its own gate (record.Chains.HoldsOwnGate), and Root whether
// the owner walk from the issue's parent ends at the issue itself, which gives its architect a
// root's prompt (R18 of dispatch://INFRA-3320). The machine relaunches the process in its own
// session (relaunchUnderGateRoot), so the role's LEGION_TREE is the new gate root and its plugin
// registers again (R22 of dispatch://INFRA-3320), an architect whose issue holds its own gate is
// told the design gate steps and is read as a gate holder's (Claim.treeRoot), and the launch reads
// the owner walk again for its prompt.
type GateRootMoved struct {
	Claim     claim.Token
	Tree      string
	HoldsGate bool
	Root      bool
}

func (GateRootMoved) isEvent() {}

// LaunchedRoot says whether the claim's last launch was a root architect's: addressed to the claim
// itself as the architect that owns its issue (LaunchOwner), which is what gives a launch a root's
// prompt (internal/daemon's workerSpawnSpec). A claim no launch of this release has built records
// no owner, and reads by the gate root decision 13 of dispatch://INFRA-4730 keeps for its launch
// (LaunchedTree): a root's where that is its own issue, which, for a claim no start of this
// release has marked either, is the tree it stores, the release before's rule for a tree's root
// (claim.IsTreeRoot). A phase worker's launch is never a root's.
func (c Claim) LaunchedRoot() bool {
	if c.Role != claim.RoleArchitect {
		return false
	}
	if c.LaunchOwner != "" {
		return c.LaunchOwner == c.Token
	}
	return c.LaunchedTree() == c.Issue
}

// relaunchUnderGateRoot is the GateRootMoved event's row, for the four states a claim holds its
// process's capability in.
func relaunchUnderGateRoot(m *Machine, ctx context.Context, ev Event) error {
	return m.relaunchUnder(ctx, ev.(GateRootMoved))
}

// relaunchUnder replaces the claim's process in its own session under moved's gate root and its
// issue's own gate, as the Dispatch chain reads them now, once nothing the process does is cut: the
// process is resumed from its recorded session on a new process whose launch names that gate root
// (Claim.LaunchTree, spawnSpec), carries that gate (Claim.HoldsGate) and reads the owner walk again
// for its prompt (Claim.LaunchOwner), the same replacement the stale-plugin lane makes
// (replaceProcess), charged nothing, since the process did nothing wrong.
//
// A claim whose launch already names all three is decided nothing, so the pass's re-posts at every
// sweep are free. A claim with a launch already waiting its turn in the wake pacing has the gate
// root and the gate written for that launch, which reads them, and the owner walk, when it runs. So
// does a claim whose record says its process died (Claim.DeathCharged, over the process it
// records), whose death owes a relaunch a restart forgets: the boot's replay of that death
// (RecordedDeath) or its next report raises it, and nothing is replaced here, since a replacement
// would launch outside the boot's owed order and charge the death nothing. A claim in a turn keeps
// its process until the turn ends (afterTurn), because what that turn has not pushed would be lost
// with it, and the relaunch runs then. A claim with no process of its own has nothing to replace:
// its next start writes the gate root and the gate for its next launch (Machine.MarkLaunch). A
// relaunch the lifecycle recheck refuses, or one that ends with no launch at all, leaves the claim
// as it found it, so the next sweep finds the claim again.
func (m *Machine) relaunchUnder(ctx context.Context, moved GateRootMoved) error {
	if moved.Tree == "" || m.claim.LaunchedTree() == moved.Tree && m.claim.HoldsGate == moved.HoldsGate && m.claim.LaunchedRoot() == moved.Root {
		m.retree = GateRootMoved{}
		return nil
	}
	if m.wake != nil || m.claim.DeathCharged && m.claim.Locator != nil {
		m.claim.LaunchTree, m.claim.HoldsGate = moved.Tree, moved.HoldsGate
		return m.persist(ctx)
	}
	if !HoldsCapability(m.claim.State) {
		return nil
	}
	if m.claim.State == StateWorking && !claim.IsController(m.claim.Role, m.claim.Issue, m.claim.Tree) {
		if m.retree.Tree == "" {
			m.log.Info("supervise: the process was launched at another place on its issue's Dispatch chain; its relaunch in its session waits for the agent's turn to end",
				"launch_tree", m.claim.LaunchedTree(), "gate_root", moved.Tree, "holds_gate", moved.HoldsGate, "root", moved.Root)
		}
		m.retree = moved
		return nil
	}
	if m.held == nil {
		if err := m.checkLaunch(ctx); err != nil {
			return err
		}
	}
	m.log.Info("supervise: the process was launched at another place on its issue's Dispatch chain; relaunching it in its session",
		"incarnation", m.claim.Locator.Incarnation, "launch_tree", m.claim.LaunchedTree(), "gate_root", moved.Tree, "holds_gate", moved.HoldsGate, "root", moved.Root)
	tree, holds := m.claim.LaunchTree, m.claim.HoldsGate
	m.claim.LaunchTree, m.claim.HoldsGate = moved.Tree, moved.HoldsGate
	end, err := m.replaceProcess(ctx, "the process launched at another place on its issue's Dispatch chain is replaced mid-turn", ordinaryEnd, endGateRootMoved)
	if end == EndNotMade {
		m.claim.LaunchTree, m.claim.HoldsGate = tree, holds
	}
	return err
}
