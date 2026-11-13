package prompts

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
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
		// A deployment's own name for itself, for the repository it runs the daemon from, for the
		// runbook it sends a heavy run to, and for the bucket it keeps one's output in. The match
		// is case-sensitive, so the deployment's name stands here in both the forms a part could
		// write it in, the slug and the prose one, as `Inspect` and `inspect_ai` already do.
		"theorem", "Theorem", "agent-infra", "running-heavy-jobs", "gs://",
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
func TestNoReusablePartNamesARepositoryOrLibrary(t *testing.T) {
	for _, file := range reusableParts {
		text := rolePart(t, file)
		for _, needle := range repoSpecific {
			if strings.Contains(text, needle) {
				t.Errorf("%s contains %q", file, needle)
			}
		}
	}
	err := eachPart(goParts, "go", func(name string, body []byte) error {
		for _, needle := range repoSpecific {
			if strings.Contains(string(body), needle) {
				t.Errorf("go/%s contains %q", name, needle)
			}
		}
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
		testerRunsTheApp = "read CI for everything static"
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
	// application and reads CI for the static lanes (dispatch://LEGION-631), so the rule to rerun
	// them is the implementer's alone.
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
// and `skip-checks: true` lines on the default branch (dispatch://INFRA-714, R2). So the merger
// writes the message itself, and on the one path that cannot — `enqueuePullRequest` takes no
// message — it reads the repository's setting first and stops rather than enqueueing under a
// setting that joins them. The command's shape is the instruction: a merge that drops `--subject`
// or `--body ”` lets GitHub write the message again.
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
	// counted: a part that stopped telling a role how to answer an update round fails here.
	for _, part := range []string{"go/worker-common.md", "go/architect-common.md"} {
		body, err := goParts.ReadFile(part)
		if err != nil {
			t.Fatalf("read %s: %v", part, err)
		}
		for _, want := range []string{`op: "ready_for_update"`, "the pod that runs this issue's roles"} {
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

// dispatch://INFRA-332's four role-prompt rules and dispatch://INFRA-924's fifth, each pinned by
// the sentence that carries it. A pin fails when a later edit deletes the rule; it does not show
// that an agent follows it, which each issue's session runs measure. Each of the first four
// sentences sits in the core its role composes, so the rule reaches that role and no other: the
// simplify pass and the class search are the implementer's, the procedure and can-fail rules the
// tester's, the open-decision rule the planner's, and the planner's WAITING line is phase
// mechanics, so it lives in its residue rather than its core. R5 is the exception: Compose names
// core/oracle.md for no role, and it is pinned beside core/common.md to keep one wording of that
// rule in the prompt tree, which a later core would copy from.
func TestEveryRolePartCarriesItsGuidanceRule(t *testing.T) {
	for _, pin := range []struct{ file, rule string }{
		// R1: the simplify pass covers every file of the diff and reports what it sees.
		{"core/implementer.md", "Your behaviour-preserving simplify pass covers every file of the change"},
		{"core/implementer.md", `"out of scope for this pass" is not a disposal`},
		// R2: the tester runs the procedure, and records only checks that can fail.
		{"core/tester.md", "A documented procedure is passed only by running it."},
		{"core/tester.md", "Every check you record must be able to fail"},
		// R3: after a fix or a forward merge, search the new head for each earlier finding's class.
		{"core/implementer.md", "search the head you are about to push for the class of every finding of every earlier review round"},
		{"core/tester.md", "An equal fingerprint still gets the class search"},
		{"implementer.md", "`classSearch`, one entry per finding of every earlier review round"},
		{"tester.md", "`classSearch`, one entry per finding of every earlier review round"},
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
