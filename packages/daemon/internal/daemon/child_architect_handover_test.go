package daemon

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// promptParts is the file name of every role prompt part a launch composes, in order.
func promptParts(spec runtime.SpawnSpec) []string {
	parts := make([]string, 0, len(spec.Prompt.RolePromptPaths))
	for _, path := range spec.Prompt.RolePromptPaths {
		parts = append(parts, filepath.Base(path))
	}
	return parts
}

// A child its finished tree hands over as a root of its own keeps the architect that worked it.
// The child's architect claim is the one its own first phase start created under its tree, and
// that launch composes a child's architect prompt (architect.md). A person then closes the root as
// done, its tree lingers, and the linger's end closes the tree's claims and hands the child, still
// in the workflow, to Legion with one Dispatch write of todo and the legion label, which the
// outbox applies to the record once Dispatch shows it (the outbox's consumeHandover): the child
// is admitted as a root of its own. Admission's start of its root architect finds the child's
// architect claim and resumes it: the same claim, re-pointed at the child's new tree, relaunched
// from the session file it recorded with the session it registered, and that launch composes a
// tree root's architect prompt (architect-root.md), since each launch composes its prompt from
// the claim's tree as it stands. The write's own event, arriving after, changes nothing more.
func TestAHandedOverChildKeepsItsArchitectsSession(t *testing.T) {
	rig := newChildArchitectRig(t)
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	rig.sup.deps.Specs = specs{stateDir: t.TempDir(), project: "legion", prompts: composer, appOwners: noAppOwners}
	token := mustClaimToken(t, doorChild, claim.RoleArchitect)

	rig.written("release", childAt("todo", 2))
	rig.drain()
	first := launchesSince(rig.rt, token, 0)
	if len(first) != 1 || first[0].Method != "Spawn" || !slices.Contains(promptParts(first[0].Spec), "architect.md") ||
		slices.Contains(promptParts(first[0].Spec), "architect-root.md") {
		t.Fatalf("the child's first phase start launched its architect %+v, want one spawn composing a child's architect prompt", first)
	}
	machine := rig.boot(doorChild, claim.RoleArchitect, "ses-child-architect")
	// The child's planner is at work too, so the tree's close stops it as it stops the architect,
	// and no role of the old tree still runs when the new tree's start comes (the outbox's
	// stopElsewhere).
	rig.boot(doorChild, claim.RolePlanner, "ses-child-planner")
	rig.drain()
	before, started := machine.Claim(), readIssue(t, rig.pool, doorChild)
	if before.Tree != doorRoot || before.Session != "ses-child-architect" {
		t.Fatalf("the child's architect before the hand-over = %+v, want it in the tree of %s on its session", before, doorRoot)
	}

	rig.apply("root-done", intake.DispatchIssue{Key: doorRoot, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.closed", Status: "done",
		Title: "root", Rank: "U", HandedOver: true})
	rig.drain()
	if got := machine.Claim(); got.State != supervise.StateSuspended || got.Session != before.Session {
		t.Fatalf("the child's architect while its tree lingers = %+v, want it suspended with its session", got)
	}

	called := len(rig.rt.Calls())
	rig.apply("linger-end", intake.LingerExpired{Issue: doorRoot, Generation: 1})
	rig.drain()
	if !slices.ContainsFunc(rig.dispatch.handOvers, func(written string) bool { return strings.HasPrefix(written, doorChild+" todo") }) {
		t.Fatalf("the linger's end wrote the hand-overs %v, want %s handed to Legion as todo", rig.dispatch.handOvers, doorChild)
	}
	handed := readIssue(t, rig.pool, doorChild)
	if handed.Tree != doorChild || handed.Generation <= started.Generation || handed.HandedOverFrom != "" {
		t.Fatalf("the handed-over child = %#v, want it admitted as a root of its own at a generation past %d", handed, started.Generation)
	}
	rig.apply("handover-event", intake.DispatchIssue{Key: doorChild, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "todo",
		Title: doorChild, Parent: doorRoot, Rank: "V", HandedOver: true, DaemonActor: true})
	rig.drain()
	if again := rig.machine(doorChild, claim.RoleArchitect); again != machine {
		t.Fatal("the handed-over child's root architect is a new claim, want the claim its first phase start created")
	}
	after := machine.Claim()
	if after.Tree != doorChild || after.Issue != doorChild || after.Session != before.Session || after.SessionFile != before.SessionFile {
		t.Fatalf("the child's architect after the hand-over = %+v, want it re-pointed at the tree of %s with its session %s kept", after, doorChild, before.Session)
	}
	launches := launchesSince(rig.rt, token, called)
	if len(launches) != 1 || launches[0].Method != "Resume" || launches[0].Spec.ResumeSessionFile != before.SessionFile {
		t.Fatalf("the hand-over launched the child's architect %+v, want one resume of the session file %s", launches, before.SessionFile)
	}
	if parts := promptParts(launches[0].Spec); !slices.Contains(parts, "architect-root.md") || slices.Contains(parts, "architect.md") {
		t.Fatalf("the resumed launch composes %v, want a tree root's architect prompt (architect-root.md) and no child's", parts)
	}
}
