package prompts

import (
	"embed"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The role prompt parts Compose joins. A core and the interactive fragment are also composed for an
// interactive session, whose subagent runs no Legion mechanics, so they stay mode-neutral; the
// headless fragment carries every phase worker's shared mechanics once.
var (
	phaseRoles         = []string{"planner", "implementer", "tester", "reviewer", "merger"}
	cores              = []string{"planner", "implementer", "tester", "reviewer", "oracle"}
	composedWithCommon = []string{"planner", "implementer", "tester", "reviewer"}
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
	repoSpecific = []string{
		"Inspect", "inspect_ai", "inspect_", "Hawk", "middleman", "Taiga",
		// The runbook a deployment sends a heavy run to, and the bucket it keeps one's output in.
		// A deployment's own names are not listed here: this repository is public, so a test of it
		// cannot hold them. check-private-names refuses those the deployment's own term list holds,
		// in every file it reads, role parts included.
		"running-heavy-jobs", "gs://",
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

// A reusable part is composed for every deployment, so it names no repository, library, service or
// bucket of one deployment's own. Both trees are read: the roles/ parts Compose joins for any role
// and every go/ part, because the pod memory rule tells a role where its deployment's instructions
// send a heavy run, and the next hand to write that rule down can spell a deployment's own route
// out in the architect parts, which this test read nothing of before.
//
// Each part is held to two checks: the generic names of repoSpecific, and the rules leakguard
// ports from .github/scripts/check-private-names.sh's built-in rules. Of a deployment's own names,
// check-private-names refuses those the deployment's own term list holds, in every file it reads.
func TestNoReusablePartNamesARepositoryOrLibrary(t *testing.T) {
	// leakguard's rules are embedded in it (private-names.toml) and read no input, so this check
	// runs the same wherever the test runs.
	guard := leakguard.New(leakguard.Options{})
	check := func(name, text string) {
		for _, needle := range repoSpecific {
			if strings.Contains(text, needle) {
				t.Errorf("%s contains %q", name, needle)
			}
		}
		if err := guard.Rules(t.Context(), leakguard.Record{Label: name, Text: text}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, file := range reusableParts {
		check(file, rolePart(t, file))
	}
	err := eachPart(goParts, "go", func(name string, body []byte) error {
		check("go/"+name, string(body))
		return nil
	})
	if err != nil {
		t.Fatalf("read the go parts: %v", err)
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

func TestSharedRulesSitInCommonOnceAndRoleRulesInTheirCores(t *testing.T) {
	const (
		readSource       = "read the code that already does the nearest thing"
		noDefer          = "Nothing needed for correctness is deferred"
		fastChecks       = "run the repository's fast local checks"
		testerRunsTheApp = "Never run a lane again as a whole"
		redTest          = "the test itself is not theirs to change"
		dontModify       = "make the tester's red test pass; do not modify it"
	)
	contains := func(file, needle string, want bool) {
		t.Helper()
		if strings.Contains(rolePart(t, file), needle) != want {
			t.Errorf("%s contains %q = %t, want %t", file, needle, !want, want)
		}
	}
	contains("core/common.md", readSource, true)
	contains("core/common.md", noDefer, true)
	// The four roles that compose common.md do not repeat it; core/oracle.md is joined to no
	// common part and composed for no role, so it keeps the read-first rule itself, never the
	// hardening ledger (it changes nothing).
	for _, role := range composedWithCommon {
		contains("core/"+role+".md", readSource, false)
		contains("core/"+role+".md", noDefer, false)
	}
	contains("core/oracle.md", readSource, true)
	contains("core/oracle.md", noDefer, false)
	// The implementer runs the repository's fast local checks before a push; the tester runs the
	// application and its own `before_merge` checks, and never a lane as a whole, so the rule to
	// rerun a lane is the implementer's alone.
	contains("core/implementer.md", fastChecks, true)
	contains("core/tester.md", fastChecks, false)
	contains("core/tester.md", testerRunsTheApp, true)
	contains("core/tester.md", redTest, true)
	contains("core/implementer.md", dontModify, true)
	for _, role := range []string{"planner", "reviewer", "oracle"} {
		contains("core/"+role+".md", dontModify, false)
	}
	for _, role := range []string{"planner", "implementer", "reviewer", "oracle"} {
		contains("core/"+role+".md", redTest, false)
		contains("core/"+role+".md", testerRunsTheApp, false)
	}
}

// A finding outside the tree takes one of two paths (skill://legion-worker/references/findings.md):
// a clear defect goes to the controller, and a finding that needs a decision is an ask. The headless
// fragment composed into the same planner prompt says the same, so the core may not say "always an ask".
func TestPlannerKeepsFindingsAndHumanToDoAsksOutsideDesignQuestions(t *testing.T) {
	planner := rolePart(t, "core/planner.md")
	for _, rule := range []string{
		"Read the repository's `STRATEGY.md` when it has one",
		"A finding outside the tree is never an issue you file.",
		"A clear defect in code this tree touches or depends on is fixed in this tree's own pull request.",
		"Any other clear defect goes to whoever coordinates the project's work, with its evidence, and a finding that needs a decision is an ask.",
		"A to-do only a person can do is always an ask.",
	} {
		if !strings.Contains(planner, rule) {
			t.Errorf("core/planner.md does not contain %q", rule)
		}
	}
	if strings.Contains(planner, "A finding outside the tree, and a to-do only a person can do, is always an ask.") {
		t.Error("core/planner.md still makes every finding outside the tree an ask, which contradicts the headless fragment's clear-defect path")
	}
}

func TestTheHeadlessFragmentHoldsTheWorkersSharedRules(t *testing.T) {
	headless := rolePart(t, "mechanics/headless.md")
	interactive := rolePart(t, "mechanics/interactive.md")
	// The interactive fragment states its own one-line version of the skills-first preamble.
	const preamble = "find this repository's skills"
	if !strings.Contains(headless, preamble) || strings.Contains(interactive, preamble) {
		t.Errorf("the skills-first preamble %q is in the headless fragment %t and the interactive one %t, want only the headless",
			preamble, strings.Contains(headless, preamble), strings.Contains(interactive, preamble))
	}
	for _, role := range cores {
		if strings.Contains(rolePart(t, "core/"+role+".md"), preamble) {
			t.Errorf("core/%s.md repeats the skills-first preamble", role)
		}
	}
	shared := []string{
		"## Step one: find this repository's skills",
		"create another workspace",
		"Read and follow `skill://legion-worker` before acting.",
		`op: "handoff_complete"`,
		"When your phase is done, stay in this session afterwards:",
		"roleToken",
	}
	for _, rule := range shared {
		if !strings.Contains(headless, rule) {
			t.Errorf("mechanics/headless.md omits %q", rule)
		}
	}
	for _, role := range phaseRoles {
		residue := rolePart(t, role+".md")
		for _, rule := range append(slices.Clone(shared), preamble) {
			if strings.Contains(residue, rule) {
				t.Errorf("%s.md repeats the headless fragment's %q", role, rule)
			}
		}
	}
}

// Each phase worker's residue is headed by its role, and only the roles whose plan names skills for
// them are told to read those.
func TestEachResidueNamesItsRoleAndOnlyTheSkilledRolesReadRequiredSkills(t *testing.T) {
	const requiredSkills = "Then read the plan handoff's `requiredSkills` for your role and follow those too."
	skilled := []string{"implementer", "reviewer", "tester"}
	for _, role := range phaseRoles {
		residue := rolePart(t, role+".md")
		if heading := "# Legion " + strings.ToUpper(role[:1]) + role[1:]; !strings.Contains(residue, heading) {
			t.Errorf("%s.md omits its heading %q", role, heading)
		}
		if got, want := strings.Contains(residue, requiredSkills), slices.Contains(skilled, role); got != want {
			t.Errorf("%s.md tells it to read requiredSkills = %t, want %t", role, got, want)
		}
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
	for _, agent := range []string{"plan-gap-analyst", "plan-reviewer"} {
		if !slices.Equal(agents[agent], []string{"roles/planner.md"}) {
			t.Errorf("task agent %s is dispatched by %q, want roles/planner.md alone", agent, agents[agent])
		}
	}
	for agent, files := range agents {
		if _, err := os.Stat(filepath.Join("..", "..", "..", "pi-legion", "agents", agent+".md")); err != nil {
			t.Errorf("task agent %s, dispatched by %q, is not shipped in the plugin's agents/: %v", agent, files, err)
		}
	}
}

// The merger merges the pull request itself, at the head it read, and the daemon submits no merge
// and changes no merge setting (dispatch://INFRA-714, R2). So a part that has the daemon submit a
// merge, arm auto-merge or enqueue a pull request contradicts what the merger is told to do, while
// the parts may — and do — say the daemon merges nothing. The claim is a pattern rather than
// spelled out, so a grep for it over this directory finds only prompt text; it matches the claim
// made of the daemon and not its denial, which is why the verb must follow `daemon` directly and
// name what is merged.
func TestNoPartSaysTheDaemonMerges(t *testing.T) {
	daemonMerges := regexp.MustCompile(`(?i)daemon\s+(submits|arms|enqueues|enables|merges|turns)\s+(the|a|an|that|its|it|ready|auto)\b`)
	for dir, parts := range map[string]embed.FS{"roles": roleParts, "go": goParts} {
		err := eachPart(parts, dir, func(name string, body []byte) error {
			if phrase := daemonMerges.Find(body); phrase != nil {
				t.Errorf("%s/%s says %q", dir, name, phrase)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the %s parts: %v", dir, err)
		}
	}
	// The merger's own parts carry the other half: the merge it runs, the head GitHub is held to,
	// the one setting it never touches, and the daemon's own silence on all three.
	for _, want := range []string{"gh pr merge <n> --squash --match-head-commit <head>", "Never turn auto-merge on"} {
		if !strings.Contains(rolePart(t, "merger.md"), want) {
			t.Errorf("roles/merger.md does not say %q", want)
		}
	}
	goMerger, err := goParts.ReadFile("go/merger.md")
	if err != nil {
		t.Fatalf("read go/merger.md: %v", err)
	}
	if want := "It submits no merge and changes no merge setting"; !strings.Contains(string(goMerger), want) {
		t.Errorf("go/merger.md does not say %q", want)
	}
}

// GitHub's default squash message, `squash_merge_commit_message` of `COMMIT_MESSAGES`, joins every
// commit message of a pull request, which put this tree's Dispatch keys, `Omp-Session:` trailers
// and `skip-checks: true` lines on the default branch (dispatch://INFRA-714, R2), and would put its
// `Legion-Role:` trailers there too. So the merger writes the message itself, and on the one path
// that cannot — `enqueuePullRequest` takes no message — it reads the repository's setting first
// and stops rather than enqueueing under a setting that joins them. The command's shape is the
// instruction: a merge that drops `--subject` or `--body ”` lets GitHub write the message again.
func TestTheMergersSquashMessageIsItsOwn(t *testing.T) {
	merger := rolePart(t, "merger.md")
	for _, want := range []string{
		"gh pr merge <n> --squash --match-head-commit <head> --subject '<pull request title> (#<n>)' --body ''",
		"--subject",
		"--body ''",
		"squash_merge_commit_message",
		"COMMIT_MESSAGES",
		"`enqueuePullRequest` takes no message",
		"Do not enqueue it and do not complete.",
	} {
		if !strings.Contains(merger, want) {
			t.Errorf("roles/merger.md does not say %q", want)
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
		"ask both, on their role topics, to record their results at the new head",
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

// Every update round a part describes is one a daemon of this release can open. The stale-plugin
// lane replaces a stale process at a paced wake and asks its worker nothing, so nothing builds a
// plugin round id any more (supervise's PluginRoundPrefix has no constructor beside PodRound's),
// and the parts that still told a role its process alone would be replaced "on a different
// plugin" promised a task no daemon could send. Nothing saw that: the sentences were true when
// they were written and no row read them against the lane they described.
//
// So the rule is read off the daemon's own vocabulary rather than off the prose. The openers are
// counted in supervise's own source: a kind a prompt may describe is one some path builds an id
// for. A pod round has PodRound, which is why the parts may — and do — describe a pod the daemon
// replaces; a plugin round has no such path, so no part may promise one. The day someone builds
// a plugin round id again this row fails and names what the parts must say once more, which a
// row quoting the deleted sentences could never do: it would have passed on the day the lane
// went, exactly as the empty census did.
func TestNoPartPromisesAnUpdateRoundNoDaemonCanOpen(t *testing.T) {
	buildsAPluginRound := regexp.MustCompile(`\bPluginRoundPrefix\s*\+`)
	buildsAPodRound := regexp.MustCompile(`\bPodRoundPrefix\s*\+`)
	files, err := os.ReadDir(filepath.Join("..", "supervise"))
	if err != nil {
		t.Fatalf("read the supervise package: %v", err)
	}
	pod, plugin := false, false
	for _, entry := range files {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join("..", "supervise", name))
		if err != nil {
			t.Fatalf("read supervise/%s: %v", name, err)
		}
		pod = pod || buildsAPodRound.Match(body)
		plugin = plugin || buildsAPluginRound.Match(body)
	}
	// A reader that found neither opener would pass below while checking nothing, so the pod
	// round's own constructor is the control: it is what the parts are allowed to describe.
	if !pod {
		t.Fatalf("nothing in supervise builds a %q round id; this row reads the openers and would check nothing", supervise.PodRoundPrefix)
	}
	promisesAPluginRound := regexp.MustCompile(`(?i)your process alone|on a different plugin|plugin (update )?(round|task)`)
	whitespace := regexp.MustCompile(`\s+`)
	for dir, parts := range map[string]embed.FS{"roles": roleParts, "go": goParts} {
		err := eachPart(parts, dir, func(name string, body []byte) error {
			phrase := promisesAPluginRound.Find(whitespace.ReplaceAll(body, []byte(" ")))
			if phrase != nil && !plugin {
				t.Errorf("%s/%s says %q, and no path builds a plugin round id: a daemon of this release replaces the pod and never a role's process alone", dir, name, phrase)
			}
			if phrase == nil && plugin && strings.Contains(string(body), "ready_for_update") {
				t.Errorf("%s/%s describes an update round and never the plugin round, which supervise builds again: say what a role answering one must do", dir, name)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the %s parts: %v", dir, err)
		}
	}
	// The pod half is what survives, and the two parts that carry it are listed rather than
	// counted: a part that stopped telling a role how to answer an update round fails here, and so
	// does one that lost the step that lets a role's own background work finish before it answers
	// (dispatch://INFRA-4203 R2).
	for _, part := range []string{"go/worker-common.md", "go/architect-common.md"} {
		body, err := goParts.ReadFile(part)
		if err != nil {
			t.Fatalf("read %s: %v", part, err)
		}
		for _, want := range []string{`op: "ready_for_update"`, "the pod that runs this issue's roles", "Step 2: let each subagent and each background job that you started finish."} {
			if !strings.Contains(string(body), want) {
				t.Errorf("%s does not say %q", part, want)
			}
		}
	}
}

// A release of an idle issue pod ends every role process in it and keeps each role's session, its
// phase and its pending task (supervise.SuspendedByReleased), and the next task, message or CI
// settlement for one of its roles brings the pod back. So a part that says a role, a process or a
// session stays live from its first assignment until its issue closes or leaves the workflow, or
// that a waiting role is not suspended, says what the daemon no longer does, and an agent that
// reads it treats a pod that went as news about its issue. Four of these sentences cross a line
// break in their part, so the match runs over a body whose whitespace is collapsed to one space,
// and the needle is a pattern rather than the sentences spelled out, so a grep for one of them
// over this directory finds only prompt text.
func TestNoPartSaysARoleStaysLiveUntilItsIssueEnds(t *testing.T) {
	saysARoleStaysLive := regexp.MustCompile(`(?i)stays? live,? (from|until|only|as)|idle and not suspended`)
	whitespace := regexp.MustCompile(`\s+`)
	for dir, parts := range map[string]embed.FS{"roles": roleParts, "go": goParts} {
		err := eachPart(parts, dir, func(name string, body []byte) error {
			if phrase := saysARoleStaysLive.Find(whitespace.ReplaceAll(body, []byte(" "))); phrase != nil {
				t.Errorf("%s/%s says %q: a released pod's roles lose their processes and keep their sessions", dir, name, phrase)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the %s parts: %v", dir, err)
		}
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

// Every part of both trees carries none of them, not only the parts Compose joins for a role: a
// sentence left in a part the tester never composes still reaches whichever role does compose it,
// and a part Compose joins for nobody today is joined by the next role that needs it. The match
// runs over a body whose whitespace is collapsed to one space, since a part hard-wraps and a
// sentence can cross the break.
func TestNoPartMakesTheProductionCheckTheImplementersOrWakesItFromADeploy(t *testing.T) {
	whitespace := regexp.MustCompile(`\s+`)
	for dir, parts := range map[string]embed.FS{"roles": roleParts, "go": goParts} {
		err := eachPart(parts, dir, func(name string, body []byte) error {
			folded := string(whitespace.ReplaceAll(body, []byte(" ")))
			for _, phrase := range retiredCheckSentences {
				if strings.Contains(folded, phrase) {
					t.Errorf("%s/%s says %q: the production check is the project's `production_check` role's, and nothing the daemon reads from a deployment wakes a look", dir, name, phrase)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the %s parts: %v", dir, err)
		}
	}
}

// Background work is on for every role: Legion writes no Oh My Pi settings overlay, so
// a long bash call becomes a background job and a task subagent runs as one, and a role reads a
// result with the `wait` tool. Nothing takes a turn away from a role either: a CI red
// that ends a phase starts the implementer at once and tells the role by one message, which stops
// nothing. So no part may say a takeover cancels a turn or tell a role to keep its work in
// the foreground, and a part that dispatches a task subagent names the tool that reads its result.
//
// Both halves are read over the markdown parts alone, and both are exact. The call shape appears
// in prompts_test.go and in this file, which are not prompt text, and the English word "wait" is
// in many parts — go/implementer.md forbids waiting for CI — so the needle is the backticked
// token. Each retired sentence is joined from its two halves, as TestNoPartSaysLegionNeverMerges
// keeps its phrases a pattern, so that a grep for one over this directory finds prompt text and
// never this test. The parts that dispatch are listed here rather than counted, so a part that
// stopped dispatching fails as loudly as one that started: a reader that found nothing would
// otherwise pass while checking nothing. The skills half of this rule, over skills/ and the
// plugin the Claude Code roles load, is packages/pi-legion/src/skills-guard.test.ts.
func TestNoPartKeepsItsWorkInTheForegroundAndEveryDispatchNamesWait(t *testing.T) {
	retired := []string{
		"interrupts" + " your turn",
		"tool calls" + " are cancelled",
		"interrupted" + " its turn",
		"run none" + " in the background",
		"one foreground" + " command",
		"blocking" + ": true",
	}
	var dispatching []string
	for _, dir := range []string{"go", "roles"} {
		parts := goParts
		if dir == "roles" {
			parts = roleParts
		}
		err := eachPart(parts, dir, func(name string, body []byte) error {
			if !strings.HasSuffix(name, ".md") {
				return nil
			}
			part, text := dir+"/"+name, string(body)
			for _, phrase := range retired {
				if strings.Contains(text, phrase) {
					t.Errorf("%s says %q, which does not hold: nothing interrupts a turn and background work is on", part, phrase)
				}
			}
			if strings.Contains(text, `task(agent="`) {
				dispatching = append(dispatching, part)
				if !strings.Contains(text, "`wait`") {
					t.Errorf("%s dispatches a task subagent and never names `wait`, the tool that reads a job's result", part)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the %s parts: %v", dir, err)
		}
	}
	want := []string{"go/claude-mechanics.md", "roles/implementer.md", "roles/planner.md"}
	if !slices.Equal(dispatching, want) {
		t.Errorf("the parts that dispatch a task subagent are %q, want %q", dispatching, want)
	}
}

// dispatch://INFRA-332's role-prompt rules R1, R2 and R4, dispatch://INFRA-924's fifth and
// dispatch://INFRA-3204's check CI proves, each pinned by the sentence that carries it. A pin fails
// when a later edit deletes the rule; it does not show that an agent follows it, which each
// issue's session runs measure. Each INFRA-332 sentence sits in the core its role composes, so the
// rule reaches that role and no other: the simplify pass is the implementer's, the procedure and
// can-fail rules the tester's, the open-decision rule the planner's, and the planner's WAITING line
// is phase mechanics, so it lives in its residue rather than its core. R5 is the exception: Compose
// names core/oracle.md for no role, and it is pinned beside core/common.md to keep one wording of
// that rule in the prompt tree, which a later core would copy from.
func TestEveryRolePartCarriesItsGuidanceRule(t *testing.T) {
	// The phrase that the parts below carry for that rule. core/tester.md says what CI proves in a
	// sentence of its own, pinned below. Outside these parts,
	// packages/pi-legion/src/skills-guard.test.ts counts the phrase once, in go/worker-common.md.
	const ciCheckRule = "check carrying `ci_check` is CI's"
	for _, pin := range []struct{ file, rule string }{
		// R1: the simplify pass covers every file of the diff and reports what it sees.
		{"core/implementer.md", "Your behaviour-preserving simplify pass covers every file of the change"},
		{"core/implementer.md", `"out of scope for this pass" is not a disposal`},
		// R2: the tester runs the procedure, and records only checks that can fail.
		{"core/tester.md", "A documented procedure is passed only by running it."},
		{"core/tester.md", "Every check you record must be able to fail"},
		// R4: the planner completes no phase with an open decision.
		{"core/planner.md", "A decision only whoever owns the plan can make is never left open in the plan"},
		{"core/planner.md", "do not complete the phase, and do not choose for them so the plan can be finished"},
		{"core/planner.md", "An open decision in a finished plan is a gap the implementation inherits."},
		{"planner.md", "end the turn with a line that begins WAITING, naming the decision"},
		// R5: a claim cites the file and the symbol or the text it was read at, never a line, in the
		// core every phase worker but the merger composes and in the oracle core the composer joins for
		// no role, which is where a later core would copy the sentence from (dispatch://INFRA-924).
		{"core/common.md", "A claim about how a system behaves cites the file and the symbol or the text you read it at; an uncited claim is an assumption and is written as one."},
		{"core/oracle.md", "A claim about how a system behaves cites the file and the symbol or the text you read it at; an uncited claim is an assumption and is written as one."},
		{"core/oracle.md", "Findings cite the file and the symbol, or text quoted from it."},
		// A check CI proves: a check carrying `ci_check` is proved by the CI run it names, so no
		// role runs it or records its result, and `dispatch results` derives that result
		// (dispatch://INFRA-3204, R5 of its spec). Each part that orders a check run, recorded or
		// judged, or lists a check's attributes, carries the exception. The tester's and the
		// implementer's role parts point at the worker skill's page, which a core part may not name.
		{"core/tester.md", "CI proves a check that carries `ci_check`"},
		{"core/implementer.md", ciCheckRule},
		{"core/reviewer.md", ciCheckRule},
		{"tester.md", ciCheckRule},
		{"implementer.md", ciCheckRule},
		{"architect.md", ciCheckRule},
		{"architect-root.md", ciCheckRule},
		{"tester.md", "`skill://legion-worker/references/ci-checks.md`"},
		{"implementer.md", "`skill://legion-worker/references/ci-checks.md`"},
		{"architect.md", "takes no `run`"},
		{"architect-root.md", "takes no `run`"},
		// The reviewer's gate takes R5's three cases for a check CI proves: a run still going or
		// unread is waited for, and a name no run answers goes to the architect, rather than sending
		// the work back to an implementer who cannot cure it. The final gate points at that rule.
		{"reviewer.md", "Wait for a `ci_check` run still going or unread, and send a name no run answers to the architect."},
		{"reviewer.md", "A `ci_check` check with no pass takes the end-of-round rule above, with its three cases."},
	} {
		if !strings.Contains(rolePart(t, pin.file), pin.rule) {
			t.Errorf("%s does not contain %q", pin.file, pin.rule)
		}
	}
	// The replaced sentence said the planner answers such a decision by naming it, which is what
	// let a plan complete with four decisions open (dispatch://INFRA-206).
	if strings.Contains(rolePart(t, "core/planner.md"), "and saying so is its answer") {
		t.Error("core/planner.md still answers a decision its owner must make by naming it")
	}
	// The forms R5 retires. A part carrying one again would give a retro two orders that disagree
	// (dispatch://INFRA-924).
	for _, file := range []string{"core/common.md", "core/oracle.md"} {
		text := rolePart(t, file)
		for _, retired := range []string{"file and line", "file:line"} {
			if strings.Contains(text, retired) {
				t.Errorf("%s still contains %q", file, retired)
			}
		}
	}
}

// Every file-backed role's handoff body names the head its work was done against
// (dispatch://INFRA-3146). The daemon takes a completion of an earlier entry of the same phase
// only at the head that entry's record holds, so a role whose handoff paragraph lost the field
// would write a record the daemon cannot place, and its own completion would be refused for a
// phase it is holding. Each of the four parts is pinned twice: the field as the body names it,
// with the one thing its value is, and the clause that carries it to the daemon, which names
// that role's own phase word, since a record of one phase is never read for another. Both
// needles are short and sit inside the paragraph, so a rewording of the prose around them
// stands and the field's removal is what fails them. The merger writes no handoff, so it is not
// among them. The match runs over a body whose whitespace is collapsed to one space, since a
// part hard-wraps and a sentence can cross the break.
func TestEveryFileBackedRoleHandoffNamesTheHeadItWasDoneAgainst(t *testing.T) {
	for _, role := range []struct{ part, phase string }{
		{"planner.md", "plan"},
		{"implementer.md", "implement"},
		{"tester.md", "test"},
		{"reviewer.md", "review"},
	} {
		body := foldWhitespace(rolePart(t, role.part))
		for _, needle := range []string{
			"`head` (the 40-character lower-case commit",
			"to the daemon, which takes a completion of an earlier `" + role.phase + "` entry only at the head the record holds",
		} {
			if !strings.Contains(body, needle) {
				t.Errorf("%s does not contain %q", role.part, needle)
			}
		}
	}
	// One field, not two: the reviewer's handoff named the head it reviewed under a longer name
	// before this rule, and a part carrying both names has the role write one while the `legion`
	// tool reads the other: the tool reads `head` out of the record's body (pi-shared's
	// findHandoffReport) and sends it as the completion's commit, and the daemon reads no body at
	// all. The retired name is joined from its halves, as TestNoPartSaysTheDaemonMerges keeps its
	// claim a pattern, so that a grep for it over the repository finds the daemon's changelog
	// fragment that records the rename (packages/daemon/.changes/unreleased), the frozen history
	// that named the field before it, and never this test.
	retired := "head" + "Reviewed"
	err := eachPart(roleParts, "roles", func(name string, body []byte) error {
		if strings.Contains(string(body), retired) {
			t.Errorf("roles/%s still names %s, which `head` replaced", name, retired)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the roles parts: %v", err)
	}
}

// The pod memory rule is written twice, once for each reader: a phase worker meets it in the
// headless fragment every worker composes, and an architect in the common part every architect
// tree joins. One part each is the whole of it. Copied into a second part of the same tree it
// drifts, because the next edit reaches one copy and leaves the other saying what the pod no
// longer does, and a reader that counted the copies would pass while the text spread through
// every residue. So each paragraph's first sentence is pinned to the one part that carries it
// and refused in every other part of that tree. The match runs over a body whose whitespace is
// collapsed to one space, since a part hard-wraps and a sentence can cross the break.
func TestThePodMemoryRuleIsWrittenInOneWorkerPartAndOneArchitectPart(t *testing.T) {
	fold := func(body []byte) string { return foldWhitespace(string(body)) }
	for _, pin := range []struct {
		dir      string
		parts    embed.FS
		want     string
		sentence string
	}{
		{"roles", roleParts, "mechanics/headless.md", "In an issue pod every role of this issue runs in one container and shares its memory limit"},
		{"go", goParts, "architect-common.md", "Your subagents run their commands in your own issue pod"},
	} {
		var carrying []string
		err := eachPart(pin.parts, pin.dir, func(name string, body []byte) error {
			if strings.Contains(fold(body), pin.sentence) {
				carrying = append(carrying, pin.dir+"/"+name)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the %s parts: %v", pin.dir, err)
		}
		if want := []string{pin.dir + "/" + pin.want}; !slices.Equal(carrying, want) {
			t.Errorf("%q is in %q, want %q alone", pin.sentence, carrying, want)
		}
	}
}

// Each of the two paragraphs ends by passing the rule on, because a subagent runs its commands in
// the same pod and reads none of these parts: the worker's clause tells a role to put these facts
// in the context of each subagent it starts, and the architect's says the same of its own. The
// clause belongs where the rule is written and nowhere else, so a part that dispatches a task
// subagent carries no copy of it. The three parts that dispatch one are named here rather than
// counted, as TestNoPartKeepsItsWorkInTheForegroundAndEveryDispatchNamesWait names its want list,
// so a part renamed out from under this test fails it rather than passing with nothing read. The
// match runs over a body whose whitespace is collapsed to one space, since a part hard-wraps and
// the clause can cross the break.
func TestThePassOnClauseIsWrittenOnlyWhereThePodMemoryRuleIs(t *testing.T) {
	fold := func(body []byte) string { return foldWhitespace(string(body)) }
	for _, pin := range []struct {
		dir       string
		parts     embed.FS
		want      string
		unwritten []string
		clause    string
	}{
		{
			dir: "roles", parts: roleParts, want: "mechanics/headless.md",
			unwritten: []string{"implementer.md", "planner.md"},
			clause:    "Each subagent you start runs its commands in this pod, so put these facts in its context",
		},
		{
			dir: "go", parts: goParts, want: "architect-common.md",
			unwritten: []string{"claude-mechanics.md"},
			clause:    "you put these facts in the context of each subagent you start",
		},
	} {
		var carrying, read []string
		err := eachPart(pin.parts, pin.dir, func(name string, body []byte) error {
			read = append(read, name)
			if strings.Contains(fold(body), pin.clause) {
				carrying = append(carrying, pin.dir+"/"+name)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the %s parts: %v", pin.dir, err)
		}
		if want := []string{pin.dir + "/" + pin.want}; !slices.Equal(carrying, want) {
			t.Errorf("%q is in %q, want %q alone", pin.clause, carrying, want)
		}
		// The walk above refuses the clause in every part but one, the dispatching parts of this
		// tree among them: two under roles/ and one under go/, three across the two. What it
		// cannot say is that those parts are still there to be read, so each is named and held to
		// having been walked.
		for _, name := range pin.unwritten {
			if !slices.Contains(read, name) {
				t.Errorf("%s/%s, which dispatches a task subagent, is not a part of this tree any more; the walk read %q", pin.dir, name, read)
			}
		}
	}
}

// The headless fragment sends four runs to CI on the pull request, and
// skill://legion-worker/references/tools.md, which that same paragraph sends a role to for what it
// may run in the pod, sends the same four. A role reads both, so the two must agree: a run the
// fragment calls CI's work and the skill does not leaves the role with two answers. Three rows
// pin the four runs, each a substring both files hold: the bare `go test ./...` for the module
// run, the name with the comma before it for the Postgres-backed package, and for the browser
// suite and `lake build` the one clause both files carry whole, so one row pins those two. The
// skill is read from the repository by relative path, as
// TestEveryTaskAgentARolePromptDispatchesIsShipped reads the plugin's agents/, and a skill file
// this package cannot read is a fatal rather than a quiet pass. The match runs over a body whose
// whitespace is collapsed to one space, since the skill hard-wraps: the browser-suite clause
// crosses a line break in it today, and a reflow of the skill could put a break inside either
// of the other two.
func TestThePodMemoryRuleAndTheToolsSkillSendTheSameRunsToCI(t *testing.T) {
	fold := foldWhitespace
	fragment := fold(rolePart(t, "mechanics/headless.md"))
	skillPath := filepath.Join("..", "..", "..", "..", "skills", "legion-worker", "references", "tools.md")
	body, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read %s, the skill the headless fragment sends a role to: %v", skillPath, err)
	}
	skill := fold(string(body))
	for _, run := range []struct{ name, agreed string }{
		{"a whole module's test run", "`go test ./...`"},
		{"a whole Postgres-backed package", ", a whole Postgres-backed package"},
		{"a browser suite run whole and `make -j` or `lake build`", ", a browser suite run whole, and `make -j` or `lake build`"},
	} {
		if !strings.Contains(fragment, run.agreed) {
			t.Errorf("roles/mechanics/headless.md does not hold %q, what it and %s agree on about %s", run.agreed, skillPath, run.name)
		}
		if !strings.Contains(skill, run.agreed) {
			t.Errorf("%s does not hold %q, what it and roles/mechanics/headless.md agree on about %s", skillPath, run.agreed, run.name)
		}
	}
}

// testerParts are the parts Compose joins for the tester, each by its path in this package, with
// its text. Compose names them, so a part it adds to the tester's prompt is read too. The three
// parts that state what the tester runs must be among them, so a Compose that names none of them
// ends the test rather than letting it pass on nothing.
func testerParts(t *testing.T) map[string]string {
	t.Helper()
	stateDir := t.TempDir()
	composer, err := New(stateDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	composed, err := composer.Compose(claim.RoleTester, false)
	if err != nil {
		t.Fatalf("Compose the tester: %v", err)
	}
	parts := map[string]string{}
	for _, path := range composed.RolePromptPaths {
		rel, err := filepath.Rel(filepath.Join(stateDir, "prompts"), path)
		if err != nil {
			t.Fatalf("place %s in the snapshot: %v", path, err)
		}
		rel = filepath.ToSlash(rel)
		if shared, ok := strings.CutPrefix(rel, "shared/"); ok {
			rel = "roles/" + shared
		}
		parts[rel] = partText(t, rel)
	}
	for _, want := range []string{"roles/core/tester.md", "roles/tester.md", "go/worker-common.md"} {
		if _, ok := parts[want]; !ok {
			t.Fatalf("Compose joins %q for the tester, and not %s", slices.Sorted(maps.Keys(parts)), want)
		}
	}
	return parts
}

// partPin is a sentence that a part must hold, matched over the part with its whitespace folded.
type partPin struct{ part, text string }

// missingPins are the pins whose part, with its whitespace folded, does not hold the pin's text,
// one message each.
func missingPins(parts map[string]string, pins []partPin) []string {
	var missing []string
	for _, pin := range pins {
		if !strings.Contains(foldWhitespace(parts[pin.part]), pin.text) {
			missing = append(missing, fmt.Sprintf("%s does not hold %q", pin.part, pin.text))
		}
	}
	return missing
}

// requirePins fails t for each pin that its part does not hold. A control runs first: each pin
// is taken out of a copy of its part, and the check must report that copy. A pin that its part
// holds twice stays in the copy, so the control also refuses a pin whose removal from one place
// another copy would hide.
func requirePins(t *testing.T, parts map[string]string, pins []partPin) {
	t.Helper()
	for _, pin := range pins {
		planted := maps.Clone(parts)
		planted[pin.part] = strings.Replace(foldWhitespace(parts[pin.part]), pin.text, "", 1)
		if got := missingPins(planted, []partPin{pin}); len(got) != 1 {
			t.Errorf("control: a copy of %s without %q is not reported, so the part holds it more than once", pin.part, pin.text)
		}
	}
	for _, problem := range missingPins(parts, pins) {
		t.Error(problem)
	}
}

// heldPhrase is a phrase that a part holds and that no part may hold.
type heldPhrase struct{ part, phrase string }

// partsHolding are the phrases that each part holds, with its whitespace folded, in the order of
// the parts' paths and then of the phrases. With caseless, the match ignores case, so a sentence
// that starts with a phrase is found too.
func partsHolding(parts map[string]string, phrases []string, caseless bool) []heldPhrase {
	var held []heldPhrase
	for _, part := range slices.Sorted(maps.Keys(parts)) {
		text := foldWhitespace(parts[part])
		for _, phrase := range phrases {
			haystack, needle := text, phrase
			if caseless {
				haystack, needle = strings.ToLower(text), strings.ToLower(phrase)
			}
			if strings.Contains(haystack, needle) {
				held = append(held, heldPhrase{part, phrase})
			}
		}
	}
	return held
}

// requirePlantsCaught is the control of a check that no part holds any of phrases. It plants
// each sentence alone at the end of a copy of the part the plant names (a new part where there is
// none of that name), and fails t unless the check reports that part with each phrase the
// sentence holds. A plant that holds no phrase fails t, and so does a phrase that no plant holds:
// the test has then never seen the check catch it.
func requirePlantsCaught(t *testing.T, parts map[string]string, phrases []string, caseless bool, plants []partPin) {
	t.Helper()
	caught := map[string]bool{}
	for _, plant := range plants {
		own := partsHolding(map[string]string{plant.part: plant.text}, phrases, caseless)
		if len(own) == 0 {
			t.Errorf("control: the plant %q holds none of the phrases, so it shows nothing", plant.text)
		}
		planted := maps.Clone(parts)
		planted[plant.part] += "\n\n" + plant.text + "\n"
		reported := partsHolding(planted, phrases, caseless)
		for _, want := range own {
			if !slices.Contains(reported, want) {
				t.Errorf("control: %q planted in %s is not reported", want.phrase, plant.part)
			}
			caught[want.phrase] = true
		}
	}
	for _, phrase := range phrases {
		if !caught[phrase] {
			t.Errorf("control: no plant holds %q, so the check was never seen to catch it", phrase)
		}
	}
}

// The words of the tester's run rule from before each of its `before_merge` checks became its own
// to run again at each head that it tests, the census rule that made each invoked entry a check of
// its own, and the failure fields its `test` handoff held. Each is joined from its halves, as
// retiredCheckSentences are, so that a search of the repository for one finds the prompt text that
// holds it and never this test.
const (
	everyCheckOfTheIssue  = "Run every check" + " of the issue"
	everyBeforeMergeCheck = "run every `before_merge`" + " check"
	aFullTestRound        = "a full test" + " round"
	sizeYourRound         = "size your round" + " to that change"
	notRunAgain           = "cannot affect is" + " not run again"
	implementerProofField = "implementer" + "Proof"
	failuresField         = "`fail" + "ures`"
	failedField           = "`fail" + "ed`"
	readCIForStatic       = "read CI for" + " everything static"
	aLaneStillRunning     = "A lane still running" + " is read as running"
	oneDerivedResult      = "that read shows its" + " one derived result"
	censusEntryIsACheck   = "every entry a person" + " invokes is a check"
)

// oldTesterRunRule are the phrases that no part of the tester's prompt may hold.
var oldTesterRunRule = []string{
	everyCheckOfTheIssue, everyBeforeMergeCheck, aFullTestRound, sizeYourRound, notRunAgain,
	implementerProofField, failuresField, failedField, readCIForStatic, aLaneStillRunning,
	oneDerivedResult, censusEntryIsACheck,
}

// mainTesterSentences are sentences of the tester's parts as main held them before this rule,
// each in the part that held it. The last one is older: it was in roles/tester.md until the part
// on a new head replaced it. Each holds at least one phrase of oldTesterRunRule.
var mainTesterSentences = []partPin{
	{"roles/core/tester.md", "Record what you ran and what you observed, and " + readCIForStatic + " — lint, types, the unit and integration suites and every other lane are CI's work, read on GitHub at the head you tested and written down nowhere for a later step to trust, except a `ci_check` job; rerun none of them yourself, and never re-run a CI lane as evidence."},
	{"roles/core/tester.md", aLaneStillRunning + "; never wait for it."},
	{"roles/core/tester.md", everyCheckOfTheIssue + " on the surface a person reaches it through — the CLI you type, the endpoint you curl, the TUI you drive in tmux, the workflow you dispatch, the job you submit — as the repository's testing skills describe."},
	{"roles/core/tester.md", "On a new head, read what changed since the head you last tested (`git diff <that head> <new head>`), and " + sizeYourRound + ": test again only what it can affect, and confirm the rest by naming the earlier head."},
	{"roles/core/tester.md", "When the PR carries a `## Contract change census`, " + censusEntryIsACheck + ": drive it through its real surface, meaning the client that builds the input, run through the new check, and record it like any other."},
	{"roles/tester.md", "A check carrying `ci_check` is CI's: " + oneDerivedResult + " (`skill://legion-worker/references/ci-checks.md`)."},
	{"roles/tester.md", "Judge those results and record the verdict in your `test` handoff as `" + implementerProofField + "` (`{verdict: \"verified\" | \"rejected\", how}`)."},
	{"roles/tester.md", "Otherwise " + everyBeforeMergeCheck + " yourself before completing, a whole test suite excepted as above."},
	{"roles/tester.md", "A check that the change since its pass " + notRunAgain + ": record it at the new head with `--observed 'carried from <old sha>: <why the change cannot affect it>'`."},
	{"roles/tester.md", "`head` (the 40-character lower-case commit your recorded results name), `" + implementerProofField + "`, and " + failuresField + " (each `{check, evidence}`) with " + failedField + " counting them."},
	{"roles/tester.md", "A class search that finds an instance is a failure: record it under " + failuresField + " with the command and what it found, and run " + aFullTestRound + "."},
}

// The tester's checks are the `before_merge` checks that CI does not prove, and the tester runs
// each of them again at each head that it tests, whoever ran it before. The core holds the one
// definition, the sentences on a check whose words give its run to another runner and on a check
// that runs after the merge, and the duty. A contract change census adds no check of its own: the
// tester drives each entry, records a result only on one of its own checks, and reports an entry
// that no check covers to the architect. The role part and the worker part say the same for a new
// head and for a merge of the base, and the worker part keeps the implementer's carry in a clause
// of its own. The tester reads no CI: the core says so, and the worker part takes the tester out
// of the roles that decide on a red check. The control plants main's sentences of the core in its
// place, and the core's sentences below must all be reported missing there.
func TestTheTesterRunsEachOfItsBeforeMergeChecksAtEachHeadItTests(t *testing.T) {
	parts := testerParts(t)
	const core = "roles/core/tester.md"
	pins := []partPin{
		{core, "Your checks are the `before_merge` checks that CI does not prove."},
		{core, "A check is not one of your checks where its own words give its run only to another role, a person or another step."},
		{core, "Nor is it where the issue's document or plan does so, or says that you do not run it."},
		{core, "A check whose `when` is `after_merge` or `default_branch` runs at its own time, never in a testing round."},
		{core, "Its words and the part on the production check say when."},
		{core, "Run each of your `before_merge` checks on the surface a person reaches it through, at the head, with one negative control, whoever ran it before."},
		{core, "Treat each of them as broken until your own run at the head passes."},
		{core, "A pass that the implementer recorded does not take the place of that run."},
		{core, "Nor does a result carried from an earlier head, your own included."},
		{core, "Run each of your `before_merge` checks by its own tests, never a whole suite, whatever lane holds them."},
		{core, "You read no CI, and wait for none."},
		{core, "drive again what that change can affect, and run each of your `before_merge` checks again at the new head"},
		{core, "Each of your `before_merge` checks runs again, whatever those files or the resolution can reach"},
		{core, "drive each entry a person invokes through its real surface: the client that builds the input, run through the new check."},
		{core, "Record a result only where one of your `before_merge` checks covers the entry, and record it on that check."},
		{core, "An entry that no check covers is no fail result: report it to the architect, and the work adds the check that missed it."},
		{"roles/tester.md", "Otherwise run each of your `before_merge` checks yourself before completing, a whole test suite excepted as above, and record each with `dispatch result --issue <issue> --check <block id> --role tester --verdict pass --commit <head sha>`"},
		{"roles/tester.md", "Each of your `before_merge` checks runs again at each head that you test, whatever the change since its pass can affect."},
		{"roles/tester.md", "A result carried from an earlier head does not take the place of that run, your own included"},
		{"go/worker-common.md", "whether it is re-run, waited for or fixed is a decision for the roles it reached, except the tester, which reads no CI"},
		{"go/worker-common.md", "A `before_merge` check those files or the resolution can reach runs again, by the implementer where it carries no `ci_check`."},
		{"go/worker-common.md", "At a head that it tests, the tester runs again each `before_merge` check that its role prompt gives it, whatever the merge can reach."},
		{"go/worker-common.md", "The empty commit changes no file. So the implementer records each `before_merge` check with no `ci_check` again at the new head, as carried from the commit before it"},
		{"go/worker-common.md", "It runs none of them again. At a head that it tests, the tester runs each `before_merge` check that its role prompt gives it."},
	}
	var mainCore []string
	for _, sentence := range mainTesterSentences {
		if sentence.part == core {
			mainCore = append(mainCore, sentence.text)
		}
	}
	planted := maps.Clone(parts)
	planted[core] = strings.Join(mainCore, " ")
	var corePins []partPin
	for _, pin := range pins {
		if pin.part == core {
			corePins = append(corePins, pin)
		}
	}
	if got := missingPins(planted, corePins); len(got) != len(corePins) {
		t.Errorf("control: main's sentences of %s, planted in its place, miss %d of its %d sentences, want all", core, len(got), len(corePins))
	}
	requirePins(t, parts, pins)
}

// A failure the tester finds is a fail result on its check, recorded at the head its `test`
// record names before any later push, whatever the implementer recorded. A check with no pass
// from the implementer is no fail of its own, and a defect that no check covers goes to the
// architect. The `test` handoff holds no failure.
func TestTheTesterRecordsEachFailAsAFailResult(t *testing.T) {
	parts := testerParts(t)
	requirePins(t, parts, []partPin{
		{"roles/tester.md", "When a run of one of your checks fails, record a fail result on that check with `dispatch result --issue <issue> --check <block id> --role tester --verdict fail --commit <head sha>`, its evidence in `--observed`, whatever the implementer recorded."},
		{"roles/tester.md", "Record it at the head your `test` record names, before you push any commit after that head."},
		{"roles/tester.md", "A check with no pass from the implementer is not a fail of its own: record nothing for it."},
		{"roles/tester.md", "A defect that no check covers is no fail result: report it to the architect, and the work adds the check that missed it."},
		{"roles/tester.md", "the fail result you record on its check names the test and the command that shows it failing"},
		{"roles/tester.md", "Each failure is a fail result on its check, never a field of this handoff."},
	})
}

// Every phase worker retracts a result of its own only when it observed wrongly, with that
// reason, and never to end a state or because a later run passed. A run whose setup did not meet
// the check's words observed wrongly. A refused retraction goes to the architect. The command is
// one `dispatch result` takes: a retraction carries its reason alone, and `dispatch result`
// refuses one that also carries `--issue` ("retract takes reason alone, and this call also
// carries issue"), so the control plants that command and the check must report it.
func TestTheWorkerPartGivesTheRetractRule(t *testing.T) {
	parts := testerParts(t)
	const worker = "go/worker-common.md"
	requirePins(t, parts, []partPin{
		{worker, "Retract a result of your own only when it observed wrongly, with that reason: `dispatch result --retract <result id> --reason '<why it observed wrongly>'`."},
		{worker, "A wrong command, a wrong head or a misread output observed wrongly."},
		{worker, "So did a run whose setup did not meet the check's words, such as a service the check needs that was missing from your pod."},
		{worker, "Never retract a true result to end a state, and never retract a fail because a later run passed."},
		{worker, "When Dispatch refuses the retraction, name the result and its reason to the architect."},
	})
	retractWithIssue := regexp.MustCompile("dispatch result --retract[^`]*--issue")
	if !retractWithIssue.MatchString(foldWhitespace(parts[worker] + "\n\n`dispatch result --retract <result id> --issue <issue> --reason '<why>'`\n")) {
		t.Error("control: a retraction planted with `--issue` is not reported")
	}
	if phrase := retractWithIssue.FindString(foldWhitespace(parts[worker])); phrase != "" {
		t.Errorf("%s gives a retraction that `dispatch result` refuses, since it also carries `--issue`: %q", worker, phrase)
	}
}

// ciWaits are the sentences of parts that name a wait on CI and do not take the tester out of it,
// each with its part, in the order of the parts' paths. A sentence names such a wait when it holds
// both the word CI and a form of "wait". It takes the tester out when it names the tester's
// exception, or when it denies the wait or the read.
func ciWaits(parts map[string]string) []heldPhrase {
	namesCI := regexp.MustCompile(`\bCI\b`)
	namesAWait := regexp.MustCompile(`(?i)\bwait`)
	takesTheTesterOut := regexp.MustCompile(`except the tester|\bno CI\b|not a wait on CI`)
	var waits []heldPhrase
	for _, part := range slices.Sorted(maps.Keys(parts)) {
		for _, sentence := range clauses(parts[part]) {
			if namesCI.MatchString(sentence) && namesAWait.MatchString(sentence) && !takesTheTesterOut.MatchString(sentence) {
				waits = append(waits, heldPhrase{part, sentence})
			}
		}
	}
	return waits
}

// The worker part names the waits that only the backstop wakes. CI of another repository is one of
// them for every role except the tester: the tester reads no CI and waits for none. So no sentence
// of the tester's parts names a wait on CI that the tester may take. The controls: main's sentence,
// which names CI of another repository as a wait for every role, and a reworded one, each planted
// in a copy of the worker part, are reported, and the head's two sentences, planted alone, are
// not.
func TestTheWorkerPartGivesTheTesterNoCIWait(t *testing.T) {
	parts := testerParts(t)
	const worker = "go/worker-common.md"
	head := []partPin{
		{worker, "Such a wait is a Dispatch topic you subscribed to with `envoy_subscribe`, a deploy run, or a Kubernetes Job you started."},
		{worker, "For each role except the tester, CI of a repository that is not your issue's is such a wait too: the tester reads no CI and waits for none."},
	}
	requirePins(t, parts, head)
	for _, plant := range []string{
		"Such a wait is a Dispatch topic you subscribed to with `envoy_subscribe`, CI of a repository that is not your issue's, a deploy run, or a Kubernetes Job you started.",
		"Wait for another repository's CI as you wait for a deploy run.",
	} {
		planted := maps.Clone(parts)
		planted[worker] += "\n\n" + plant + "\n"
		if !slices.Contains(ciWaits(planted), heldPhrase{worker, plant}) {
			t.Errorf("control: %q planted in %s is not reported", plant, worker)
		}
	}
	for _, pin := range head {
		if got := ciWaits(map[string]string{pin.part: pin.text}); len(got) != 0 {
			t.Errorf("control: the head's sentence %q, planted alone, is reported: %q", pin.text, got)
		}
	}
	for _, wait := range ciWaits(parts) {
		t.Errorf("%s names a wait on CI that the tester may take: %q", wait.part, wait.phrase)
	}
}

// No part of the tester's prompt keeps the run rule that its checks and its duty replaced, the
// failure fields of its `test` handoff, or an order to read CI. Each phrase is matched with the
// part's whitespace folded and its case ignored. The control plants main's sentences one at a
// time, each in the part that held it, and each phrase must be reported there.
func TestNoTesterPartHoldsTheOldRunRule(t *testing.T) {
	parts := testerParts(t)
	requirePlantsCaught(t, parts, oldTesterRunRule, true, mainTesterSentences)
	for _, held := range partsHolding(parts, oldTesterRunRule, true) {
		t.Errorf("%s still says %q", held.part, held.phrase)
	}
}

// allParts are every part of both trees, each by its path in this package, with its text.
func allParts(t *testing.T) map[string]string {
	t.Helper()
	parts := map[string]string{}
	for dir, tree := range map[string]embed.FS{"roles": roleParts, "go": goParts} {
		err := eachPart(tree, dir, func(name string, body []byte) error {
			parts[dir+"/"+name] = string(body)
			return nil
		})
		if err != nil {
			t.Fatalf("read the %s parts: %v", dir, err)
		}
	}
	return parts
}

// testHandoffFailureReads are the failure fields of a `test` handoff and a read of them. `failed`
// is matched only as the count of failures, since a review's outcome is `failed` too. Each is
// joined from its halves, as oldTesterRunRule's phrases are.
var testHandoffFailureReads = []string{
	implementerProofField, failuresField, "with " + failedField + " counting", "tester's fail" + "ures",
}

// A failure is a fail result on its check, so each role that reads a failure reads it with
// `dispatch results`, never from a `test` handoff, and no part of either tree names the handoff's
// failure fields. Each of the eight parts that read a failure holds its sentence that sends its
// role to the fail results. The architect's three parts hold the check states in the same
// sentence, and the reviewer's core holds the failure sentence of the reviewer's own part. A read
// of the handoff's `head` or `note` stays. The quiet control: in the planner's "When a review ended
// `failed`", `failed` is a review's outcome, and the check finds nothing in that line. The planted
// control: a temporary part that holds main's field list of the tester's handoff, and a line that
// copies the tester's failures, is reported for each field and for the copy. A third control: a
// copy of the reviewer's core that still names `dispatch results` but lacks the failure sentence
// is reported, so a part that names the command for another read does not pass for the failure
// read.
func TestNoRolePartReadsAFailureFromATestHandoff(t *testing.T) {
	parts := allParts(t)
	const reviewEnded = "When a review ended " + failedField
	var line string
	for _, candidate := range strings.Split(parts["roles/planner.md"], "\n") {
		if strings.Contains(candidate, reviewEnded) {
			line = candidate
		}
	}
	if line == "" {
		t.Fatalf("roles/planner.md does not hold %q, the quiet control", reviewEnded)
	}
	if held := partsHolding(map[string]string{"roles/planner.md": line}, testHandoffFailureReads, false); len(held) != 0 {
		t.Errorf("control: the check finds %q in the planner's line %q, which names a review's outcome and no field", held, line)
	}
	requirePlantsCaught(t, parts, testHandoffFailureReads, false, []partPin{
		{"roles/planted.md", "`head` (the 40-character lower-case commit your recorded results name), `" + implementerProofField + "`, and " + failuresField + " (each `{check, evidence}`) with " + failedField + " counting them."},
		{"roles/planted.md", "Copy the tester's fail" + "ures into your acceptance."},
	})
	for _, held := range partsHolding(parts, testHandoffFailureReads, false) {
		t.Errorf("%s still says %q", held.part, held.phrase)
	}
	const results = "`dispatch results --issue <issue>`"
	const evidence = results + " at each open head, whose states decide each check and whose fail results give each failure, the tester's handoff for its `head` and `note`, and the reviewer's approval of the head by SHA"
	const reviewerFailures = "Read each failure from the fail results in " + results + ", never from a `test` handoff."
	const reviewerCore = "roles/core/reviewer.md"
	withoutFailures := maps.Clone(parts)
	withoutFailures[reviewerCore] = strings.Replace(foldWhitespace(parts[reviewerCore]), reviewerFailures, "", 1)
	if !strings.Contains(withoutFailures[reviewerCore], results) {
		t.Errorf("control: a copy of %s without %q no longer names %s, so it does not show a part that names the command and lacks the failure read", reviewerCore, reviewerFailures, results)
	}
	if got := missingPins(withoutFailures, []partPin{{reviewerCore, reviewerFailures}}); len(got) != 1 {
		t.Errorf("control: a copy of %s that names %s and lacks %q is not reported", reviewerCore, results, reviewerFailures)
	}
	requirePins(t, parts, []partPin{
		{"go/architect-common.md", "read the evidence (" + evidence + ")"},
		{"roles/architect.md", "read " + evidence},
		{"roles/architect-root.md", "read " + evidence},
		{"roles/implementer.md", "Read each check that failed from its fail results in " + results + " at the tested head; the `test` handoff holds no failure."},
		{"roles/core/implementer.md", "Read each failure of that round from the fail results that " + results + " prints at the tested head."},
		{"roles/reviewer.md", reviewerFailures},
		{reviewerCore, reviewerFailures},
		{"roles/tester.md", "read each earlier failure from the fail results that " + results + " prints, not from a handoff"},
	})
}
