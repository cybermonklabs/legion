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

// The daemon submits the merge itself once it accepts READY, and Legion enforces no GitHub
// restriction the repository itself does not: a part that says Legion never merges, shared or the
// Go daemon's, contradicts what the merger is told the daemon does. The phrases, in any case,
// are a pattern rather than spelled out, so a grep for them over this directory finds only prompt
// text.
func TestNoPartSaysLegionNeverMerges(t *testing.T) {
	saysLegionNeverMerges := regexp.MustCompile(`(?i)never merg(es)|merg(e|es) nothing`)
	for dir, parts := range map[string]embed.FS{"roles": roleParts, "go": goParts} {
		err := eachPart(parts, dir, func(name string, body []byte) error {
			if phrase := saysLegionNeverMerges.Find(body); phrase != nil {
				t.Errorf("%s/%s says %q", dir, name, phrase)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the %s parts: %v", dir, err)
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
		// The walk above refuses the clause in every part but one, this tree's three dispatching
		// parts among them. What it cannot say is that those three are still there to be read, so
		// each is named and held to having been walked.
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
// fragment calls CI's work and the skill does not leaves the role with two answers. Each run is
// pinned by the longest substring both files hold rather than by its bare name, which a file could
// carry while saying the opposite of the other and pass; the browser suite and `lake build` sit in
// one clause both files carry whole, so one row pins them both. The skill is read from the
// repository by relative path, as TestEveryTaskAgentARolePromptDispatchesIsShipped reads the
// plugin's agents/, and a skill file this package cannot read is a fatal rather than a quiet pass.
// The match runs over a body whose whitespace is collapsed to one space, since the skill hard-wraps
// and every one of these clauses crosses a break in it.
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
