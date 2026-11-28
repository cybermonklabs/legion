package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// A person unlinks a running child's issue from its parent, so the child is a root that holds
// its own design gate (R15 of dispatch://INFRA-3320). The sweep's pass over the running architects
// (supervision.relaunchUnderGateRoots) relaunches the child's architect in its session under its
// own key and with the gate: its launch is told the design gate steps (specs, DesignGateFragment),
// which its role prompt reads to know what to register, its claim holds the gate, which is what
// the machine reads it as its tree's root by (supervise's treeRoot), and its prompt is a root
// architect's, the owner walk ending at its own issue now. The next sweep finds nothing to change.
//
// The negative control is the pass posting the gate root alone, as it did: the relaunch is then
// told no design gate steps and the claim holds no gate.
func TestAnUnlinkedArchitectIsRelaunchedHoldingItsGate(t *testing.T) {
	t.Parallel()
	gated := config.DesignGateRootIssues.Steps()
	r := newMergeGateRig(t, gated)
	r.launchWithRealSpecs(gated)
	const parent, child = "LEGION-800", "LEGION-801"
	r.putRoot(record.Issue{Key: parent, Phase: phase.Implementing, Status: "in_progress", TakenBack: true})
	issue := mergeGateIssue(child)
	issue.Phase, issue.Status, issue.TakenBack, issue.Parent, issue.Tree = phase.Implementing, "in_progress", true, new(parent), parent
	putChainIssue(t, r.pool, r.records, issue)
	r.openTree(child)

	architect := mustClaimToken(t, child, claim.RoleArchitect)
	machine := r.startArchitect(child, architect)
	line := DesignGateFragment(gated)
	if c := machine.Claim(); c.LaunchedTree() != parent || c.HoldsGate || c.LaunchedRoot() {
		t.Fatalf("the architect of %s is launched under %q holding the gate %t, a root %t; want %s's gate root, no gate of its own and no root",
			child, c.LaunchedTree(), c.HoldsGate, c.LaunchedRoot(), parent)
	}
	for _, call := range r.rt.CallsOf("Spawn") {
		if call.Spec.Claim == architect && (strings.Contains(call.Spec.Prompt.Addressing, line) || rootPrompt(call.Spec)) {
			t.Fatalf("the architect of %s under %s was told %q with the prompt %v, want no design gate steps and a member's prompt",
				child, parent, call.Spec.Prompt.Addressing, call.Spec.Prompt.RolePromptPaths)
		}
	}

	putCopyRow(t, r.pool, child, "", "in_progress")
	r.sweepGateRoots(machine)

	relaunched := machine.Claim()
	if relaunched.LaunchedTree() != child || !relaunched.HoldsGate || !relaunched.LaunchedRoot() {
		t.Fatalf("the relaunched architect of %s is launched under %q holding the gate %t, a root %t; want its own key, holding its own gate, a root",
			child, relaunched.LaunchedTree(), relaunched.HoldsGate, relaunched.LaunchedRoot())
	}
	resumes := resumesOf(r.rt, architect)
	if len(resumes) != 1 || resumes[0].Spec.Tree != child || !strings.Contains(resumes[0].Spec.Prompt.Addressing, line) || !rootPrompt(resumes[0].Spec) {
		t.Fatalf("the architect of %s was resumed %d times (%+v), want once under %s with a root's prompt, told %q", child, len(resumes), resumes, child, line)
	}

	bootClaim(t, machine, architect)
	r.sweepGateRoots(machine)
	if resumes := resumesOf(r.rt, architect); len(resumes) != 1 {
		t.Fatalf("the architect of %s was resumed %d times after a second sweep, want the one relaunch", child, len(resumes))
	}
}

// A gate holder at work under a parent at work is no root: its architect is launched with a
// member's prompt, addressed to the architect above it, and told its own design gate's steps. Its
// parent leaves Legion's work, so the owner walk from the gate holder's parent now ends at the gate
// holder itself, under the same gate root and with the same gate (R18 of dispatch://INFRA-3320: who
// acts on an issue is read from the Dispatch chain). No notice tells its architect (workflow's
// leave tells the architect above the parent alone), so the sweep's pass relaunches it in its
// session with a root architect's prompt (supervise.GateRootMoved's Root), once.
//
// The negative control is the pass weighing the gate root and the gate alone: the architect is not
// relaunched and keeps a member's prompt.
func TestAGateHolderWhoseParentLeavesWorkIsRelaunchedAsItsTreesRoot(t *testing.T) {
	t.Parallel()
	gated := config.DesignGateRootIssues.Steps()
	r := newMergeGateRig(t, gated)
	r.launchWithRealSpecs(gated)
	const parent, holder = "LEGION-850", "LEGION-851"
	above := r.putRoot(record.Issue{Key: parent, Phase: phase.Implementing, Status: "in_progress", TakenBack: true})
	issue := mergeGateIssue(holder)
	issue.Phase, issue.Status, issue.TakenBack, issue.HoldsGate, issue.Parent = phase.Implementing, "in_progress", true, true, new(parent)
	putChainIssue(t, r.pool, r.records, issue)
	r.openTree(holder)

	architect := mustClaimToken(t, holder, claim.RoleArchitect)
	machine := r.startArchitect(holder, architect)
	line := DesignGateFragment(gated)
	spawns := r.rt.CallsOf("Spawn")
	if c := machine.Claim(); c.LaunchedTree() != holder || !c.HoldsGate || c.LaunchedRoot() || len(spawns) != 1 ||
		rootPrompt(spawns[0].Spec) || !strings.Contains(spawns[0].Spec.Prompt.Addressing, line) ||
		!strings.Contains(spawns[0].Spec.Prompt.Addressing, "the architect above you is `"+architectTopic(t, parent)+"`") {
		t.Fatalf("the gate holder's architect is launched under %q holding the gate %t, a root %t, spawned %d times; want its own key, its gate, a member's prompt addressed to %s's architect and told %q",
			c.LaunchedTree(), c.HoldsGate, c.LaunchedRoot(), len(spawns), parent, line)
	}

	above.Status = "backlog"
	putRecordOnly(t, r.pool, r.records, above)
	r.sweepGateRoots(machine)

	resumes := resumesOf(r.rt, architect)
	if c := machine.Claim(); len(resumes) != 1 || !c.LaunchedRoot() || c.LaunchedTree() != holder || !c.HoldsGate ||
		!rootPrompt(resumes[0].Spec) || !strings.Contains(resumes[0].Spec.Prompt.Addressing, line) {
		t.Fatalf("once %s left Legion's work the gate holder's architect was resumed %d times (%+v), a root %t; want once, with a root's prompt and its design gate steps",
			parent, len(resumes), resumes, c.LaunchedRoot())
	}
	bootClaim(t, machine, architect)
	r.sweepGateRoots(machine)
	if resumes := resumesOf(r.rt, architect); len(resumes) != 1 {
		t.Fatalf("the gate holder's architect was resumed %d times after a second sweep, want the one relaunch", len(resumes))
	}
}

// A child whose new parent's row the copy does not hold yet stands on an unknown chain (R16 of
// dispatch://INFRA-3320), which makes no root, and work under way on it goes on. The sweep's pass
// over the running architects (supervision.relaunchUnderGateRoots) leaves the child's architect
// running under the gate root it was launched under, where StateTree would name the child itself.
// Once the copy holds the parent's row, the pass relaunches the architect once, under the parent,
// the gate root the known chain names.
//
// The negative control is workflowRuntime.gateRoots reading the gate root of an unknown chain: the
// first sweep then relaunches the architect under its own key.
func TestAnArchitectOnAnUnknownChainKeepsItsGateRootUntilTheChainIsKnown(t *testing.T) {
	t.Parallel()
	r := newMergeGateRig(t, config.DesignGateRootIssues.Steps())
	const parent, moved, child = "LEGION-810", "LEGION-811", "LEGION-812"
	r.putRoot(record.Issue{Key: parent, Phase: phase.Implementing, Status: "in_progress", TakenBack: true})
	issue := mergeGateIssue(child)
	issue.Phase, issue.Status, issue.TakenBack, issue.Parent, issue.Tree = phase.Implementing, "in_progress", true, new(parent), parent
	putChainIssue(t, r.pool, r.records, issue)
	r.openTree(child)
	architect := mustClaimToken(t, child, claim.RoleArchitect)
	machine := r.startArchitect(child, architect)
	if launched := machine.Claim().LaunchedTree(); launched != parent {
		t.Fatalf("the architect of %s is launched under %q, want %s", child, launched, parent)
	}

	// The child moves under a root the daemon records at work, holding its own gate, whose row
	// the copy does not hold yet.
	root := mergeGateIssue(moved)
	root.Phase, root.Status, root.TakenBack, root.HoldsGate = phase.Implementing, "in_progress", true, true
	putRecordOnly(t, r.pool, r.records, root)
	putCopyRow(t, r.pool, child, moved, "in_progress")
	r.sweepGateRoots(machine)
	if c, resumes := machine.Claim(), resumesOf(r.rt, architect); c.LaunchedTree() != parent || c.HoldsGate || len(resumes) != 0 {
		t.Fatalf("on the unknown chain the architect of %s is launched under %q holding the gate %t, resumed %d times; want it left under %s",
			child, c.LaunchedTree(), c.HoldsGate, len(resumes), parent)
	}

	putCopyRow(t, r.pool, moved, "", "in_progress")
	r.sweepGateRoots(machine)
	resumes := resumesOf(r.rt, architect)
	if c := machine.Claim(); c.LaunchedTree() != moved || c.HoldsGate || len(resumes) != 1 || resumes[0].Spec.Tree != moved {
		t.Fatalf("once the chain is known the architect of %s is launched under %q holding the gate %t, resumed %+v; want one resume under %s",
			child, c.LaunchedTree(), c.HoldsGate, resumes, moved)
	}
	bootClaim(t, machine, architect)
	r.sweepGateRoots(machine)
	if resumes := resumesOf(r.rt, architect); len(resumes) != 1 {
		t.Fatalf("the architect of %s was resumed %d times after a second sweep, want the one relaunch", child, len(resumes))
	}
}

// A start on a chain the copy cannot show to its top reads no gate root (launchOn): StateTree
// there would name the issue itself and launch its role as a root's, and on an unknown chain the
// daemon starts nothing and makes no root, while work under way goes on (R16 of
// dispatch://INFRA-3320). A start that would make a claim, or launch one no start of this release
// has marked, waits, and notes its issue for R16's re-read at each attempt; once the copy holds
// the missing row, the same row starts the claim under the gate root the known chain names. A
// start of a claim that a start of this release marked goes on, and its launch keeps the gate root
// the claim records.
//
// The negative control is launchOf reading StateTree of an unknown chain: each start then spawns
// its claim under the child's own key at once.
func TestAStartOnAnUnknownChainReadsNoGateRoot(t *testing.T) {
	t.Parallel()
	steps := config.DesignGateRootIssues.Steps()

	t.Run("a start that would make a claim waits until the chain is known", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := newMergeGateRig(t, steps)
		var noted []string
		r.runner.noteUnknownChain = func(key string) { noted = append(noted, key) }
		const moved, child = "LEGION-820", "LEGION-821"
		// The child stands under a root the daemon records at work, holding its own gate, whose
		// row the copy does not hold yet.
		parent := mergeGateIssue(moved)
		parent.Phase, parent.Status, parent.TakenBack, parent.HoldsGate = phase.Implementing, "in_progress", true, true
		putRecordOnly(t, r.pool, r.records, parent)
		issue := mergeGateIssue(child)
		issue.Phase, issue.Status, issue.TakenBack, issue.Parent, issue.Tree = phase.Planning, "in_progress", true, new(moved), moved
		putChainIssue(t, r.pool, r.records, issue)
		r.openTree(child)

		planner := mustClaimToken(t, child, claim.RolePlanner)
		row := mustOutboxRow(t, child, record.SuperviseRequest{Op: "start", Tree: child, Role: claim.RolePlanner,
			Generation: issue.Generation, Phase: phase.Planning, Task: "Plan it."}, time.Now())
		for attempt := range 2 {
			if err := r.runner.execute(ctx, row); !errors.Is(err, wait.ErrWaiting) || !errors.Is(err, record.ErrChainUnknown) {
				t.Fatalf("attempt %d at the planner's start of %s, whose parent's row the copy lacks, = %v; want a wait on the unknown chain", attempt, child, err)
			}
		}
		if _, found := r.sup.Machine(planner); found || r.spawnsOf(planner) != 0 || !slices.Equal(noted, []string{child, child}) {
			t.Fatalf("while the chain is unknown the start made its claim (%t), spawned it %d times and noted %v; want no claim, no spawn and %s noted at each attempt",
				found, r.spawnsOf(planner), noted, child)
		}

		putCopyRow(t, r.pool, moved, "", "in_progress")
		if err := r.runner.execute(ctx, row); err != nil {
			t.Fatalf("the planner's start of %s once the chain is known = %v, want it run", child, err)
		}
		if trees := r.spawnTrees(planner); len(trees) != 1 || trees[0] != moved {
			t.Fatalf("the planner of %s was spawned under %v, want once under its gate root %s", child, trees, moved)
		}
	})

	t.Run("a start that would launch a claim no start of this release marked waits", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := newMergeGateRig(t, steps)
		const root, moved, child = "LEGION-825", "LEGION-826", "LEGION-827"
		r.putRoot(record.Issue{Key: root, Phase: phase.Implementing, Status: "in_progress", TakenBack: true})
		issue := mergeGateIssue(child)
		issue.Phase, issue.Status, issue.TakenBack, issue.Parent, issue.Tree = phase.Implementing, "in_progress", true, new(root), root
		putChainIssue(t, r.pool, r.records, issue)
		r.openTree(child)
		// The claim the release before left: it has no process and no launch tree, and stores
		// its issue's stored tree.
		implementer := mustClaimToken(t, child, claim.RoleImplementer)
		if _, _, err := r.sup.Create(ctx, supervise.Claim{Token: implementer, Project: "legion", Tree: root, Issue: child,
			Role: claim.RoleImplementer, State: supervise.StateQueued}, ""); err != nil {
			t.Fatalf("create the implementer claim of %s: %v", child, err)
		}
		// The child moves under a root the daemon records at work, holding its own gate, whose
		// row the copy does not hold yet.
		parent := mergeGateIssue(moved)
		parent.Phase, parent.Status, parent.TakenBack, parent.HoldsGate = phase.Implementing, "in_progress", true, true
		putRecordOnly(t, r.pool, r.records, parent)
		putCopyRow(t, r.pool, child, moved, "in_progress")

		row := mustOutboxRow(t, child, record.SuperviseRequest{Op: "start", Tree: child, Role: claim.RoleImplementer,
			Generation: issue.Generation}, time.Now())
		if err := r.runner.execute(ctx, row); !errors.Is(err, wait.ErrWaiting) || !errors.Is(err, record.ErrChainUnknown) {
			t.Fatalf("the implementer's start of %s on the unknown chain = %v; want a wait on the unknown chain", child, err)
		}
		if trees := r.spawnTrees(implementer); len(trees) != 0 {
			t.Fatalf("the implementer of %s was spawned under %v on the unknown chain, want no launch", child, trees)
		}

		putCopyRow(t, r.pool, moved, "", "in_progress")
		if err := r.runner.execute(ctx, row); err != nil {
			t.Fatalf("the implementer's start of %s once the chain is known = %v, want it run", child, err)
		}
		if trees := r.spawnTrees(implementer); len(trees) != 1 || trees[0] != moved {
			t.Fatalf("the implementer of %s was spawned under %v, want once under its gate root %s, not the tree its claim stores", child, trees, moved)
		}
	})

	t.Run("a start of a claim this release marked keeps the gate root it records", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := newMergeGateRig(t, steps)
		const root, missing, child = "LEGION-830", "LEGION-831", "LEGION-832"
		r.putRoot(record.Issue{Key: root, Phase: phase.Implementing, Status: "in_progress", TakenBack: true})
		// The child was first recorded as a root of its own, so its stored tree is its own key,
		// and then put under root.
		issue := mergeGateIssue(child)
		issue.Phase, issue.Status, issue.TakenBack, issue.Parent = phase.Implementing, "in_progress", true, new(root)
		putChainIssue(t, r.pool, r.records, issue)
		r.openTree(child)
		// The claim has no process of its own, and its last start read root as its gate root.
		implementer := mustClaimToken(t, child, claim.RoleImplementer)
		if _, _, err := r.sup.Create(ctx, supervise.Claim{Token: implementer, Project: "legion", Tree: child, LaunchTree: root, Issue: child,
			Role: claim.RoleImplementer, State: supervise.StateQueued}, ""); err != nil {
			t.Fatalf("create the implementer claim of %s: %v", child, err)
		}

		putCopyRow(t, r.pool, child, missing, "in_progress")
		if err := r.runner.execute(ctx, mustOutboxRow(t, child, record.SuperviseRequest{Op: "start", Tree: child, Role: claim.RoleImplementer,
			Generation: issue.Generation}, time.Now())); err != nil {
			t.Fatalf("the implementer's start of %s on the unknown chain = %v, want it run", child, err)
		}
		if trees := r.spawnTrees(implementer); len(trees) != 1 || trees[0] != root {
			t.Fatalf("the implementer of %s was spawned under %v, want once under the gate root its claim records, %s", child, trees, root)
		}
	})
}

// An architect's launch on a chain the copy cannot show to its top reads no owner from the walk,
// whose answer there, the architect's own claim, would make a root's prompt of it (R16 of
// dispatch://INFRA-3320: an unknown chain makes no root). It launches as its last launch did
// (workflowRuntime.owningArchitect), addressed to the architect that launch was addressed to
// (supervise.Claim.LaunchOwner): a gate holder under a parent at work, which holds its own gate and
// so last launched under its own key, is given a member's prompt with its gate's design gate steps,
// addressed to the architect above it; a member's architect is given a member's prompt; and a
// root's architect keeps a root's prompt. A claim no launch of this release built records no
// owner, and is addressed by the gate root of its launch (decision 13 of dispatch://INFRA-4730):
// for a claim no start of this release marked either, the tree it stores, so a root's prompt
// exactly where that tree is its own issue. The same gate holder's claim with no owner recorded is
// given a root's prompt: the recorded owner is what keeps it a member's.
//
// The negative control is owningArchitect reading no recorded owner: the gate holder's architect
// is then given a root's prompt, addressed to itself.
func TestAnArchitectLaunchedOnAnUnknownChainLaunchesAsItsLastLaunchDid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	const parent, holder, member, root, unmarked, missing = "LEGION-840", "LEGION-841", "LEGION-842", "LEGION-843", "LEGION-844", "LEGION-849"
	for _, key := range []string{parent, holder, member, root, unmarked} {
		issue := mergeGateIssue(key)
		issue.Phase, issue.Status, issue.HoldsGate = phase.Implementing, "in_progress", key != member
		putChainIssue(t, pool, records, issue)
	}
	// Each stood on a known chain at its last launch, the gate holder and the member under parent;
	// each now names a parent whose row the copy does not hold yet.
	for _, key := range []string{holder, member, root, unmarked} {
		putCopyRow(t, pool, key, missing, "in_progress")
	}
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	gated := config.DesignGateRootIssues.Steps()
	line := DesignGateFragment(gated)
	w := &workflowRuntime{pool: pool, records: records}
	s := specs{stateDir: t.TempDir(), project: "legion", prompts: composer, designSteps: gated, appOwners: noAppOwners, owner: w.owningArchitect}
	for _, tc := range []struct {
		name  string
		claim supervise.Claim
		owner string
	}{
		{"a gate holder under a parent at work", supervise.Claim{Issue: holder, Tree: holder, LaunchTree: holder, HoldsGate: true,
			LaunchOwner: mustClaimToken(t, parent, claim.RoleArchitect)}, parent},
		{"a member", supervise.Claim{Issue: member, Tree: member, LaunchTree: parent,
			LaunchOwner: mustClaimToken(t, parent, claim.RoleArchitect)}, parent},
		{"a root", supervise.Claim{Issue: root, Tree: root, LaunchTree: root, HoldsGate: true,
			LaunchOwner: mustClaimToken(t, root, claim.RoleArchitect)}, root},
		{"a claim of the release before, on its stored tree's root", supervise.Claim{Issue: unmarked, Tree: unmarked, HoldsGate: true}, unmarked},
		{"the gate holder with no owner recorded", supervise.Claim{Issue: holder, Tree: holder, LaunchTree: holder, HoldsGate: true}, holder},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.claim
			c.Token, c.Project, c.Role = mustClaimToken(t, c.Issue, claim.RoleArchitect), "legion", claim.RoleArchitect
			spec, err := s.SpawnSpec(ctx, c)
			if err != nil {
				t.Fatalf("SpawnSpec of the architect of %s: %v", c.Issue, err)
			}
			ownRoot := tc.owner == c.Issue
			addressing := "the architect above you is `" + architectTopic(t, tc.owner) + "`"
			if ownRoot {
				addressing = "the architect that owns your issue is `" + architectTopic(t, tc.owner) + "`"
			}
			told := spec.Prompt.Addressing
			if rootPrompt(spec) != ownRoot || !strings.Contains(told, addressing) || strings.Contains(told, line) != c.HoldsGate {
				t.Errorf("the architect of %s on the unknown chain is given the prompt %v and told %q; want a root's prompt %t, told %q and its design gate steps %t",
					c.Issue, spec.Prompt.RolePromptPaths, told, ownRoot, addressing, c.HoldsGate)
			}
			if want := mustClaimToken(t, tc.owner, claim.RoleArchitect); spec.Owner != want {
				t.Errorf("the launch of the architect of %s carries the owner %q, want %q, which its claim records", c.Issue, spec.Owner, want)
			}
		})
	}
}

// launchWithRealSpecs has the rig's launches built by the daemon's own specs over the shipped
// prompts and the deployment's gated steps, each addressed to the end of the owner walk the
// workflow reads at that launch (workflowRuntime.owningArchitect), as a daemon builds them.
func (r *mergeGateRig) launchWithRealSpecs(steps []record.DesignStep) {
	r.t.Helper()
	composer, err := prompts.New(r.t.TempDir())
	if err != nil {
		r.t.Fatalf("compose the shipped prompts: %v", err)
	}
	w := &workflowRuntime{pool: r.pool, records: r.records, dispatchProject: "LEGION", log: quietLogger()}
	r.sup.deps.Specs = specs{stateDir: r.t.TempDir(), project: "legion", prompts: composer, designSteps: steps, appOwners: noAppOwners,
		owner: w.owningArchitect}
}

// rootPrompt says whether spec is a root architect's launch: its role prompt is the root's.
func rootPrompt(spec runtime.SpawnSpec) bool {
	return slices.ContainsFunc(spec.Prompt.RolePromptPaths, func(path string) bool { return filepath.Base(path) == "architect-root.md" })
}

// sweepGateRoots runs the sweep's pass over the running architects for machine's claim
// (supervision.relaunchUnderGateRoots over workflowRuntime.gateRoots, as the workflow attaches
// it), and waits until the machine has handled what the pass posted and what that set going.
func (r *mergeGateRig) sweepGateRoots(machine *supervise.Machine) {
	r.t.Helper()
	w := &workflowRuntime{pool: r.pool, records: r.records, dispatchProject: "LEGION", log: quietLogger()}
	s := &supervision{log: quietLogger(), supervisor: r.sup, gateRoots: w.gateRoots}
	held := machine.Claim()
	s.relaunchUnderGateRoots(context.Background(), []supervise.Claim{held})
	handled := make(chan struct{})
	r.sup.drained(held.Token, handled)
	testwait.Receive(r.t, "the machine of "+string(held.Token)+" to handle the sweep's post", handled)
	machine.Wait()
}

// spawnTrees is the tree each spawn of token's claim was launched under, in order.
func (r *mergeGateRig) spawnTrees(token claim.Token) []string {
	var trees []string
	for _, call := range r.rt.CallsOf("Spawn") {
		if call.Spec.Claim == token {
			trees = append(trees, call.Spec.Tree)
		}
	}
	return trees
}
