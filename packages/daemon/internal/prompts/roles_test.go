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
		testerRunsTheApp = "Let CI do its job: your job is to boot and run the app's real workflows like a user would."
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
	// application and its own checks, never a lane as a whole, and leaves CI to do its job
	// (dispatch://LEGION-631, and Sami's decision that the tester reads no CI,
	// dispatch://INFRA-1303/ask/dd2229bf-075c-4524-b045-41f39978dbb3), so the rule to rerun a lane
	// is the implementer's alone.
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
	if want := "It submits no merge and changes no merge setting"; !strings.Contains(goPart(t, "merger.md"), want) {
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

// A pull request in one of GitHub's stacks takes neither of the merger's calls (GitHub refuses
// `gh pr merge` and `enqueuePullRequest` for it, measured on cybermonklabs/legion#4961 on
// 2026-11-22), and the stack's own asynchronous merge refuses a GitHub App on a merge-queue base
// ("Enqueuer is not authorized to merge", github/gh-stack#497, measured on the same pull request
// with the merge App's token). That merge would also land every open pull request below, which
// the base holds to its reviews and checks alone and not to its issue's acceptance, retro or
// design gate. So the merger waits while every pull request below lands through its own issue,
// then takes the stack apart, after which its pull request merges as any other.
func TestTheMergerTakesAStackApartOnceNothingBelowItsPullRequestIsOpen(t *testing.T) {
	merger := rolePart(t, "merger.md")
	for _, want := range []string{
		"gh api repos/<o>/<r>/pulls/<n> --jq .stack",
		"gh api 'repos/<o>/<r>/stacks?pull_request=<n>'",
		"Where one below yours is open, merge nothing",
		"gh api -X POST repos/<o>/<r>/stacks/<stack number>/unstack",
		"yours is now a pull request like any other",
	} {
		if !strings.Contains(merger, want) {
			t.Errorf("roles/merger.md does not say %q", want)
		}
	}
	if strings.Contains(merger, "merge-async") {
		t.Errorf("roles/merger.md names merge-async, which refuses the App the merger acts as on a merge-queue base")
	}
}

// A `failed_checks` ejection of a head that has not changed is queued once more, and the work goes
// back only on the second such ejection (dispatch://INFRA-1303, R4, which says the rule is written
// in the merger's own text). The merger's part said the opposite until it did: "a queue that took
// the head out because its own `merge_group` checks failed is work to fix, so send the issue
// back", which sent cybermonklabs/legion#3972 round a lap on a flaky group build (INFRA-3204's
// retro). The record the requeue owes is pinned as a pointer to the page that holds it, so the
// merger keeps no second list of its facts. cybermonklabs/legion#4447 moved every step of the
// merger's out of skills/legion-worker/references/merge-gate.md into this part, so these rows also
// hold the two sentences the skills guard pinned there: the requeue at an unchanged head, and that
// no role re-runs a group build.
func TestTheMergerQueuesAFailedChecksEjectionOnceMore(t *testing.T) {
	merger := rolePart(t, "merger.md")
	for _, want := range []string{
		"At a head that has not changed, the removal gets that same head queued once more",
		"the issue goes back only on the second such removal of that head",
		"no role re-runs a group build",
		"carrying the facts `skill://legion-worker/references/red-at-the-head.md` names for the record",
		// The two enqueue budgets of one head, each told apart by its comment's first line
		// (INFRA-1303's review round 2, R2-15).
		"the first line `failed_checks requeue of <head sha>`",
		"begins with the line `merge_conflict requeue of <head sha>`",
		// The occurrence goes out with the record, before the requeue, since a green group build
		// merges the pull request and no later step of the merger's runs (review round 3, finding 4).
		"still before you queue it, run the three steps of that page's *What becomes of the record*",
		"saying that the requeued build's outcome is not yet known",
		// A group build that failed in no test owes no census step (the architect, 2026-11-20).
		"A group build that failed at a step and in no test, such as a download that answered 5xx or a runner that ended, owes the record and none of those three steps",
	} {
		if !strings.Contains(merger, want) {
			t.Errorf("roles/merger.md does not say %q", want)
		}
	}
	// The retired send-back is refused with any punctuation after "is work to fix", or none: the
	// comma of "is work to fix, so send the issue back", the full stop of "That is work to fix. Send
	// the issue back", and a colon, which two literal pins let through until the class search at
	// INFRA-1303's merge head. The rows below show the pattern takes each of the three.
	sendsBackAtOnce := regexp.MustCompile(`(?i)\bis work to fix\s*[^\w\s]*\s+(?:so\s+)?send the issue back\b`)
	for _, old := range []string{
		"is work to fix, so send the issue back",
		"That is work to fix. Send the issue back",
		"That is work to fix: send the issue back",
	} {
		if !sendsBackAtOnce.MatchString(old) {
			t.Errorf("the send-back pattern lets through %q", old)
		}
	}
	if phrase := sendsBackAtOnce.FindString(foldWhitespace(merger)); phrase != "" {
		t.Errorf("roles/merger.md still sends the issue back on the first `failed_checks` ejection: %q", phrase)
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
		// INFRA-1303 bounds the merger's re-run: only a failed `pull_request` check at a head its own
		// forward merge made, once, after the record (the architect's ruling of 2026-11-22T22:36Z).
		"wait for the base's fix, or re-run it within the bounds of `skill://legion-worker/references/red-at-the-head.md`",
		"You re-run only a failed `pull_request` check at a head your own forward merge made, once at that head, and only after you record the failure as that page says",
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
		body := goPart(t, strings.TrimPrefix(part, "go/"))
		for _, want := range []string{`op: "ready_for_update"`, "the pod that runs this issue's roles", "Step 2: let each subagent and each background job that you started finish."} {
			if !strings.Contains(body, want) {
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

// dispatch://INFRA-332's role-prompt rules R1, R2 and R4, dispatch://INFRA-924's fifth,
// dispatch://INFRA-1303's sixth and dispatch://INFRA-3204's check CI proves, each pinned by the
// sentence that carries it. A pin fails when a later edit deletes the rule; it does not show that
// an agent follows it, which each issue's session runs measure. Each INFRA-332 sentence sits in the
// core its role composes, so the rule reaches that role and no other: the simplify pass is the
// implementer's, the procedure and can-fail rules the tester's, the open-decision rule the
// planner's, and the planner's WAITING line is phase mechanics, so it lives in its residue rather
// than its core. R5 is the exception: Compose names core/oracle.md for no role, and it is pinned
// beside core/common.md to keep one wording of that rule in the prompt tree, which a later core
// would copy from. R6's holders are named in the worker part every phase worker composes, its
// limit for the reviewer in the reviewer's Go overlay and its wait for the implementer in the
// implementer's, and the tester's core carries the rule that keeps the tester out of it: the
// tester reads no CI.
func TestEveryRolePartCarriesItsGuidanceRule(t *testing.T) {
	// The phrase every part below carries for that rule. Outside these parts,
	// packages/pi-legion/src/skills-guard.test.ts counts it once, in go/worker-common.md.
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
		{"core/planner.md", "An open decision in a finished plan is a gap the steps after yours inherit."},
		{"planner.md", "end the turn with a line that begins WAITING, naming the decision"},
		// R5: a claim cites the file and the symbol or the text it was read at, never a line, in the
		// core every phase worker but the merger composes and in the oracle core the composer joins for
		// no role, which is where a later core would copy the sentence from (dispatch://INFRA-924).
		{"core/common.md", "A claim about how a system behaves cites the file and the symbol or the text you read it at; an uncited claim is an assumption and is written as one."},
		{"core/oracle.md", "A claim about how a system behaves cites the file and the symbol or the text you read it at; an uncited claim is an assumption and is written as one."},
		{"core/oracle.md", "Findings cite the file and the symbol, or text quoted from it."},
		// R6: the reviewer in `reviewing`, the implementer while it holds `implementing` and the
		// merger in `merging` at a head its own forward merge made re-run a red at the head once,
		// after they write the failure down, so a red the change did not cause costs the tree no
		// extra lap (dispatch://INFRA-1303). The tester reads no CI at all, Sami's decision on
		// dispatch://INFRA-1303/ask/dd2229bf-075c-4524-b045-41f39978dbb3: "let CI do its job, your
		// job is to boot and run the app's real workflows like a user would". Its core carries his
		// words, and that it reads no check run, re-runs no CI and waits on no CI. The six rows that
		// pinned the tester's re-run here until then (its holder sentence, the page's reading of
		// whose red it is, the record before the press, the disposal steps after a green re-run, its
		// wait for an unreported red, and its read of the head's checks before any verdict) went with
		// that re-run; the skills guard's `retiredTesterCiReads` refuses their sentences.
		{"core/tester.md", "Let CI do its job: your job is to boot and run the app's real workflows like a user would."},
		{"core/tester.md", "You read no check run, re-run no CI and wait on no CI"},
		{"core/tester.md", "A red check at the head of the pull request ends no round of yours, since your verdict is on the issue's checks and the surface you drove."},
		// R6's limit for the reviewer: a check the base does not require is neither waited for nor
		// re-run for the approval, merge-gate.md's rule since cybermonklabs/legion#4211, so the
		// reviewer's one re-run is of a red on a check the base requires, and a red of the other
		// kind whose log shows the change breaks something is a finding, as merge-gate.md says. The
		// sentence is in the reviewer's Go overlay alone, so its row names the part by its go/ path,
		// which the loop reads through goPart.
		{"go/reviewer.md", "A check the base does not require you neither wait for nor re-run for the approval: you approve over a red of it that the change did not make and record it as that page says, and a red of it whose log shows that the change breaks something is a finding like any other, as `skill://legion-worker/references/merge-gate.md` says."},
		// The reviewer's round order carries the record before the press and the disposal steps after
		// a green re-run (trial-i-head-2 and i-nos1-5 approved after green with no occurrence), and
		// its wait is the red-at-the-head page's. The skills guard pinned the same order in
		// merge-gate.md until cybermonklabs/legion#4447 left the round order to this part alone.
		{"go/reviewer.md", "record the failure in a `review` handoff record with `dispatch handoff-write` before the `gh run rerun` call"},
		{"go/reviewer.md", "and once that re-run is green, run the three steps of that page's *What becomes of the record*"},
		{"go/reviewer.md", "and a check you were told of that no `pull_request` run of the head holds yet, waited for once"},
		// A required red is judged on the counted set, and the line says both halves of the rule
		// (the architect's ruling of 2026-11-19 on the forward merge of cybermonklabs/legion#4447,
		// whose sentence "A required check red there is a `REQUEST_CHANGES` naming it" sent every
		// required red to a request for changes with no re-run): a red the change made is a request
		// for changes, and one it did not make takes the head's one re-run first. The read is the
		// counted set, never the commit's check runs, which the red-at-the-head page refuses.
		{"go/reviewer.md", "A required check red there that the change made is a `REQUEST_CHANGES` naming it, and one the change did not make takes the head's one re-run first"},
		{"go/reviewer.md", "Second the head's checks as GitHub counts them for the pull request, the counted set"},
		// The reviewer also reads heads past review, in `accepting`, `retro` or `merging`, where it
		// holds no phase, so its re-run of a red holds only while it holds `reviewing` (INFRA-1303's
		// review round 3).
		{"go/reviewer.md", "and one the change did not make takes the head's one re-run first while you hold `reviewing`"},
		// Once the run that holds the failed job has completed, and before any re-run of it, the
		// reviewer reads the spend over every `pull_request` run of the head, as the red-at-the-head
		// page says: round 18 of INFRA-1303's batch (o-head-1, 2, 4 and 5) read the failing run's
		// attempts alone, saw attempt 1, and pressed at a head whose one re-run another run's
		// attempt 2 had already spent, and the spend-free re-run of a cancelled job below points at
		// that run alone. The condition was the counted set's report until the architect's ruling of
		// 2026-11-22 on round 20, whose world-t reviewers pressed on the head's own completed run.
		{"go/reviewer.md", "Once the run that holds the failed job has completed, and before any re-run of it, read the spend over every `pull_request` run of the head, as that page says: the failing run on its first attempt says nothing, since another run's second attempt spends the head's one re-run too."},
		// A failed job of the head's own `pull_request` run is acted on once that run has completed,
		// whether or not the counted set lists its check yet, and no other run is acted on at all;
		// a read made seconds after a push decides no head has no red, so a check no `pull_request`
		// run of the head holds is waited for once (the architect's ruling of 2026-11-22 on round
		// 20, which retired "unreported, not absent, and gets no re-run").
		{"go/reviewer.md", "A failed job of the head's own `pull_request` run is acted on once that run has completed, whether or not the counted set lists its check yet, and no run that is not a `pull_request` run of the head is acted on at all; a read made seconds after a push never shows that the head has no red, so a check you were told of that no `pull_request` run of the head holds is waited for once"},
		// Main's spend-free re-run of a cancelled or absent required check stays, and its read stays
		// the counted set. Its trigger read "One `completed` as `cancelled`, or absent from that
		// set", and the counted set's lag is exactly a failed job of a completed `pull_request` run
		// that the set does not list yet, so that red fitted both the red rule (record, spend read,
		// press) and this re-run (no record, nothing spent): the architect's gap review of
		// 2026-11-22. The case is now a check whose job a `pull_request` run of the head holds
		// cancelled or never ran, and a failed job of a completed `pull_request` run of the head is
		// never that case. Where no `pull_request` run of the head holds the job, the reviewer tells
		// the architect and does not approve. The skills guard's `retiredRerunProhibitions` refuses
		// the old trigger in every prompt part.
		{"go/reviewer.md", "A required check absent from the counted set, or `completed` as `cancelled` there, whose job in a `pull_request` run of the head was cancelled or never ran: re-run that run's failed and cancelled jobs, once, then wait"},
		{"go/reviewer.md", "A failed job of a completed `pull_request` run of the head is never that case: it is a red, which that page decides."},
		{"go/reviewer.md", "When no `pull_request` run of the head holds the job, or nothing will run it (a check only a commit status reports, a workflow another repository defines), tell the architect and do not approve."},
		// The worker part names the same record, the merger's `dispatch comment` beside it, since the
		// merger writes no handoff, and the implementer's overlay the same reading of whose red it is.
		{"go/worker-common.md", "The reviewer or the implementer records the failure in a handoff record of its own phase with `dispatch handoff-write`, and the merger, which writes no handoff, in a `dispatch comment` on the issue."},
		{"go/implementer.md", "(*Which red is the change's* in `skill://legion-worker/references/red-at-the-head.md` decides which of the two it is, and a pass in your own pod does not move that)"},
		// The implementer's overlay forbids every wait on CI but the ones the red-at-the-head page
		// gives it for a red it acts on: the wait on the run that holds the red until it ends, the
		// one wait for a check no `pull_request` run of the head holds yet, and the wait on its own
		// re-run of a red the change did not make. Its "Push, then complete: never wait for CI" said
		// no wait at all while the page had it wait for that re-run (the architect, 2026-11-20), and
		// its exception then named the one re-run alone while the page had it wait before it pressed
		// as well (the architect's gap review of 2026-11-20, item 2). It acts on the head's own
		// completed `pull_request` run whether or not the counted set lists the check yet, as the
		// reviewer does (the architect's ruling of 2026-11-22 on round 20).
		{"go/implementer.md", "Push, then complete: never wait for CI, except for the waits `skill://legion-worker/references/red-at-the-head.md` gives for a red you act on."},
		{"go/implementer.md", "Outside those waits, each of which a wake ends as that page says, never wait for, watch or poll the pull request's remote CI"},
		// While the tester or the reviewer holds the work, the implementer reads a red at once and starts
		// the fix outside the workspace, pushing nothing but a forward merge until the work comes back
		// (the person's answer of 2026-11-22, dispatch://INFRA-1303/ask/1e24dfc4-54b7-449c-bf11-f917823ac6a7).
		{"go/implementer.md", "While the tester or the reviewer holds the work, the daemon tells you of each CI result as well: read the failed job's log at once, and where the change made the red, start the fix at once, in a copy outside the issue workspace, and push it when the work comes back to you."},
		{"go/implementer.md", "While another role holds the work you re-run nothing and push nothing but a forward merge the daemon asks of you."},
		{"go/implementer.md", "A failed job of the head's own `pull_request` run is acted on once that run has completed, whether or not the counted set lists its check yet, and no run that is not a `pull_request` run of the head is acted on at all; a read made seconds after a push never shows that the head has no red, so a check you were told of that no `pull_request` run of the head holds is waited for once"},
		// The worker part names the three roles that hold the re-run, the merger only at a head its
		// own forward merge made, and the two that press none. It is composed into the merger's
		// prompt too, and the forward merge of main's guide cut (5748720cd9) took main's line, which
		// named no role and so gave the merger the re-run at every head (INFRA-1303's review round
		// 3); the tester left the list when it came to read no CI. The merger joined it, with its
		// bound, once cybermonklabs/legion#4979 made the forward merge in `merging` its own (the
		// architect's ruling of 2026-11-22T22:36Z), and the list read the reviewer and the
		// implementer alone until then. The architect's part names the same holders and bound.
		{"go/worker-common.md", "only the reviewer in `reviewing`, the implementer while it holds `implementing`, or the merger in `merging` at a head its own forward merge made re-runs one the change did not make"},
		{"go/worker-common.md", "It re-runs it once at that head, only after it records the failure, and follows the order, the waits and the read of the spend in `skill://legion-worker/references/red-at-the-head.md`."},
		{"go/worker-common.md", "The merger re-runs only a failed `pull_request` check: a `merge_group` check goes only through the one requeue of a `failed_checks` removal, and at any other head the merger re-runs nothing."},
		{"go/worker-common.md", "The tester presses nothing, and an implementer that runs retro re-runs nothing."},
		{"go/architect-common.md", "The merger in `merging` re-runs only a failed `pull_request` check at a head its own forward merge made, once at that head and only after it records the failure in a `dispatch comment` on the issue, and it follows the order, the waits and the read of the spend in `skill://legion-worker/references/red-at-the-head.md`."},
		{"go/architect-common.md", "At any other head the merger re-runs nothing, and the tester presses nothing."},
		// A check CI proves: a check carrying `ci_check` is proved by the CI run it names, so no
		// role runs it or records its result, and its results come from CI: the one `dispatch
		// results` derives from that run, and each one a run of its CI job records through the
		// secrets broker (R5 of the spec of that rule). Each part that orders a check
		// run, recorded or judged, or lists a check's attributes, carries the exception. The
		// tester's and the implementer's role parts point at the worker skill's page, which a core
		// part may not name.
		{"core/tester.md", ciCheckRule},
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
		// The tester reads no CI (Sami's decision on
		// dispatch://INFRA-1303/ask/dd2229bf-075c-4524-b045-41f39978dbb3), so its role part reads
		// no `ci_check` check's derived result, and no such check takes a fail result of the
		// tester's. Dispatch refuses any role's result for one (`409 CHECK_PROVED_BY_CI`), so a
		// `ci_check` check never has the implementer's pass, and the part's "A `before_merge` check
		// with no pass standing from the implementer … is a test failure" failed every one; the
		// tester's fail results then replaced that rule whole, so no check is a fail for want of the
		// implementer's pass. The part said "that read shows its one derived result" until the
		// architect's gap review of 2026-11-22, and the skills guard's `retiredTesterCiReads`
		// refuses that sentence in every prompt part.
		{"tester.md", "A check carrying `ci_check` is CI's (`skill://legion-worker/references/ci-checks.md`): you do not read its derived result, run it or record it, and it needs no pass from the implementer and takes no fail result of yours."},
		{"tester.md", "A check with no pass from the implementer is not a fail of its own: record nothing for it."},
		// The reviewer's gate takes R5's three cases for a check CI proves: a run still going or
		// unread is waited for, and a name no run answers goes to the architect, rather than sending
		// the work back to an implementer who cannot cure it. The final gate points at that rule.
		{"reviewer.md", "A `ci_check` check with no pass takes the end-of-round rule above, with its three cases."},
	} {
		read := rolePart
		if strings.HasPrefix(pin.file, "go/") {
			read = func(t *testing.T, file string) string { return goPart(t, strings.TrimPrefix(file, "go/")) }
		}
		if !strings.Contains(read(t, pin.file), pin.rule) {
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

// R2 of dispatch://INFRA-1303: no text of the tester's points at the red-at-the-head page, since
// the tester reads no CI and presses no re-run (Sami's decision on
// dispatch://INFRA-1303/ask/dd2229bf-075c-4524-b045-41f39978dbb3). The architect's gap review of
// 2026-11-22 found no row that held it. The row reads each part Compose gives the tester and
// gives no holder of the re-run, the implementer, the reviewer or the merger: roles/core/tester.md,
// roles/tester.md and go/tester.md today. A part the tester shares with a holder (core/common.md,
// mechanics/headless.md, go/worker-common.md) is that holder's text too, and go/worker-common.md
// links the page in the holders' sentences, before "The tester presses nothing", so the row leaves
// the shared parts alone. In the tester's own parts it refuses two things. The page's file name,
// which every link and every path to the page holds:
// `skill://legion-worker/references/red-at-the-head.md`, or "the worker skill's red-at-the-head
// page", which roles/core/tester.md carried until 0ac2891a. And a sentence that sends its reader
// to the page by its title: a verb of reading before "A red check at the head of the pull request"
// in one sentence. The core's "A red check at the head of the pull request ends no round of yours"
// opens its sentence with the title, names the page's subject, sends no one to read the page, and
// passes.
func TestNoPartComposedForTheTesterAlonePointsAtTheRedAtTheHeadPage(t *testing.T) {
	stateDir := t.TempDir()
	composer, err := New(stateDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	holders := map[string]bool{}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleReviewer, claim.RoleMerger} {
		parts, err := composer.Compose(role, false)
		if err != nil {
			t.Fatalf("Compose %s: %v", role, err)
		}
		for _, path := range parts.RolePromptPaths {
			holders[path] = true
		}
	}
	tester, err := composer.Compose(claim.RoleTester, false)
	if err != nil {
		t.Fatalf("Compose %s: %v", claim.RoleTester, err)
	}
	sendsToThePage := regexp.MustCompile(`(?i)\b(?:read|reads|open|opens|follow|follows|load|loads|see|sees|consult|consults)\b[^.]*?\ba red check at the head of the pull request\b`)
	var own []string
	for _, path := range tester.RolePromptPaths {
		if holders[path] {
			continue
		}
		name, err := filepath.Rel(stateDir, path)
		if err != nil {
			t.Fatalf("name %s under %s: %v", path, stateDir, err)
		}
		own = append(own, name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := foldWhitespace(string(body))
		if strings.Contains(text, "red-at-the-head") {
			t.Errorf("%s, composed for the tester alone, names the red-at-the-head page", name)
		}
		if found := sendsToThePage.FindString(text); found != "" {
			t.Errorf("%s, composed for the tester alone, sends the tester to the red-at-the-head page: %q", name, found)
		}
	}
	if len(own) == 0 {
		t.Fatal("Compose gives the tester no part of its own, so this row read nothing")
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
