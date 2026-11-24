package prompts

//ci:reads packages/pi-legion/agents/*.md TestEveryTaskAgentARolePromptDispatchesIsShipped stats agents/<name>.md for each task agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
)

// The role prompt parts Compose joins. A core and the interactive fragment are also composed for an
// interactive session, whose subagent runs no Legion mechanics, so they stay mode-neutral; the
// headless fragment carries every phase worker's shared mechanics once.
var (
	phaseRoles         = []string{"planner", "implementer", "tester", "reviewer", "merger"}
	cores              = []string{"planner", "implementer", "tester", "reviewer", "oracle"}
	modeNeutral        = append(append([]string{"core/common.md"}, coreFiles()...), "mechanics/interactive.md")
	reusableParts      = append(slices.Clone(modeNeutral), "mechanics/headless.md")
	headlessOnly       = []string{
		"LEGION_", "GH_CONFIG_DIR", "handoff_", "envoy_publish",
		"roleToken", "spawn_worker", "legion-worker",
		// Naming a task agent is headless mechanics: the daemon's load probe checks that a pane's Oh
		// My Pi finds every agent a role prompt names and runs it on its own model, and nothing
		// checks an interactive session. The needle is the boot gate's own form (promptrefs), so a
		// dispatch that carries other arguments is caught too.
		`agent="`,
	}
)

func coreFiles() []string {
	files := make([]string, 0, len(cores))
	for _, role := range cores {
		files = append(files, "core/"+role+".md")
	}
	return files
}

// rolePart is one embedded role prompt part, by its path under roles/.
func rolePart(t *testing.T, name string) string {
	t.Helper()
	body, err := roleParts.ReadFile("roles/" + name)
	if err != nil {
		t.Fatalf("read role prompt part %s: %v", name, err)
	}
	return string(body)
}

// goPart is one embedded daemon prompt part, by its path under go/.
func goPart(t *testing.T, name string) string {
	t.Helper()
	body, err := goParts.ReadFile("go/" + name)
	if err != nil {
		t.Fatalf("read the daemon part go/%s: %v", name, err)
	}
	return string(body)
}

func TestCoresAndTheInteractiveFragmentCarryNoHeadlessMechanics(t *testing.T) {
	for _, file := range modeNeutral {
		text := rolePart(t, file)
		for _, needle := range headlessOnly {
			if strings.Contains(text, needle) {
				t.Errorf("%s contains %q", file, needle)
			}
		}
	}
}

// Every role a claim can be on has its own part, and none is blank: a blank part boots a worker with
// no instructions at all. The merger composes no core.
func TestEveryRoleHasItsPartAndTheMergerHasNoCore(t *testing.T) {
	for _, role := range claim.Roles {
		if strings.TrimSpace(rolePart(t, string(role)+".md")) == "" {
			t.Errorf("roles/%s.md is blank", role)
		}
	}
	if _, err := fs.Stat(roleParts, "roles/core/merger.md"); err == nil {
		t.Error("roles/core/merger.md exists; the merger composes no core")
	}
}

// The runtimes join the parts byte for byte, so each starts with a heading and ends with one
// newline.
func TestEveryPartStartsWithAHeadingAndEndsWithOneNewline(t *testing.T) {
	parts := slices.Clone(reusableParts)
	for _, role := range phaseRoles {
		parts = append(parts, role+".md")
	}
	for _, file := range parts {
		text := rolePart(t, file)
		if !strings.HasPrefix(text, "#") {
			t.Errorf("%s does not start with a heading", file)
		}
		if !strings.HasSuffix(text, "\n") || strings.HasSuffix(text, "\n\n") {
			t.Errorf("%s does not end with exactly one newline", file)
		}
	}
}

// Every task agent a role prompt dispatches, in the form the boot gate resolves, is shipped in the
// plugin's agents/, where Oh My Pi finds it in a pane and in a pod.
func TestEveryTaskAgentARolePromptDispatchesIsShipped(t *testing.T) {
	agents := RoleReferences()[promptrefs.TaskAgents]
	// A reader that finds nothing would pass below while checking nothing.
	if len(agents) == 0 {
		t.Fatal("RoleReferences finds no task agent that a role prompt dispatches, so nothing below would be checked")
	}
	for agent, files := range agents {
		if _, err := os.Stat(filepath.Join("..", "..", "..", "pi-legion", "agents", agent+".md")); err != nil {
			t.Errorf("task agent %s, dispatched by %q, is not shipped in the plugin's agents/: %v", agent, files, err)
		}
	}
}

// Past the review a conflict with the base is the merger's (cybermonklabs/legion#4979): the
// merger merges the base forward itself, keeps both sides' intent, adapts in that merge what the
// base's newer code breaks, has the reviewer read the merge's resolution and approve the new head
// before it merges it, may ask the other workers, and sends the issue back only for a change to the
// change's own logic. Its part says so, and no longer tells it to hand the conflict to the
// implementer, to send a failed queue check back as a matter of course, or that it pushes nothing.
func TestTheMergerResolvesAConflictItself(t *testing.T) {
	merger := rolePart(t, "merger.md")
	for _, want := range []string{
		"## A head that conflicts with its base",
		"A conflict with the base, and a check the base's newer code breaks, are yours",
		"Merge; do not rebase.",
		"git show --remerge-diff <merge>",
		"so that both sides keep their intent",
		"adapt the change in the same merge commit",
		"Merge or enqueue the new head only once the reviewer's approval of it by SHA is on the pull request",
		"You may ask the implementer, the tester or the reviewer what they know",
		"Run again what the merge can reach",
		"so ask each of them, on their role topics, to record their results at the new head",
		"Where it has none, no approval holds the new head",
		"### A check that failed",
		"Read its log, and the same check on the base branch, and decide its cause.",
		"re-run it, or wait for the base's fix",
		"Send the issue back with `request_backward_move` only when the fix is a change to the change's own logic that no merge of the base resolves",
	} {
		if !strings.Contains(merger, want) {
			t.Errorf("roles/merger.md does not say %q", want)
		}
	}
	for _, gone := range []string{
		"the daemon asks the implementer to merge the base forward while the issue stays in `merging`",
		"That is work to fix. Send the issue back with `request_backward_move`",
		"You push nothing.",
	} {
		if strings.Contains(merger, gone) {
			t.Errorf("roles/merger.md still says %q", gone)
		}
	}
	goMerger, err := goParts.ReadFile("go/merger.md")
	if err != nil {
		t.Fatalf("read go/merger.md: %v", err)
	}
	if want := "the daemon sends you a merge-forward message on your role topic"; !strings.Contains(string(goMerger), want) {
		t.Errorf("go/merger.md does not say %q", want)
	}
}

// retiredCheckSentences are what no role prompt part may say about the production check any more
// (dispatch://INFRA-809). A merge that closes an issue opens a check only where the project's
// `production_check` names a role, the one role it can name is the tester, and the check's own
// record holds the role that works it; so no part makes the check the implementer's. And nothing
// the daemon reads from a deployment wakes a look: Dispatch's `issue.deployed`, a Dispatch build,
// a boot of the daemon's own build and a deploy workflow's run are all deleted, so no part
// promises a wake from one. The checking agent's own `wake_at` on a pending report, the retry of
// a look nobody reported and the check's bound are the three wakes left. Each sentence is joined
// from its halves, as TestNoPartSaysLegionNeverMerges keeps its phrases a pattern, so that a
// search of the repository for one of them finds the prompt text and never this test.
var retiredCheckSentences = []string{
	"implementer's" + " production check",
	"close is" + " the implementer's",
	"in the implementer's" + " own session",
	"implementer is handed" + " its closed issue's",
	"implementer's report on" + " a look",
	"issue." + "deployed",
	"deploy workflow" + "'s run",
	"daemon's boot on" + " build",
	"Dispatch serving" + " build",
	"the deploy" + "'s event",
}
