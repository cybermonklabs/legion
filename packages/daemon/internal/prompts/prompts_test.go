package prompts

//ci:reads packages/daemon/docker/omp-plugins/package.json TestEveryCodeReadingRoleIsToldWhenToAskCodeGraphOnEachHost reads the plugin version codeGraphActions copies
//ci:reads packages/daemon/docker/codegraph/package.json TestEveryCodeReadingRoleIsToldWhenToAskCodeGraphOnEachHost reads the CLI version codeGraphCLIForms copies

import (
	"embed"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A broken role sequence drops the shared core, uses a root prompt for a child, or makes the Go
// daemon's instructions win before the shared role instructions. The prompt paths are what every
// launch concatenates into the one prompt file its agent reads, so their order is the contract. The
// Go daemon's text is the role's own part, then the part every architect or every phase worker
// shares.
func TestComposeOrdersSharedRolePartsBeforeTheGoDaemonParts(t *testing.T) {
	stateDir := t.TempDir()
	composer, err := New(stateDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Every operation the legion tool gives an architect besides register_gate
	// (packages/pi-legion/src/tools.ts), which a prompt that never names it leaves unused.
	architectOperations := []string{"`release_children`", "`rerun_child`", "`request_backward_move`", "`retry_or_escalate`", "`sign_off`", "`read_record`"}

	for _, tc := range []struct {
		name     string
		role     claim.Role
		isRoot   bool
		shared   []string
		goParts  []string
		contains []string
	}{
		// Every architect is told what a pull-request-missing notice asks of it, whatever its tree,
		// and which code each of the two pre-checked texts is refused with: its length, and the
		// decision block it holds (INFRA-1201).
		{"root architect", claim.RoleArchitect, true, []string{"architect-root.md"}, []string{"architect-root.md", "architect-common.md"}, append([]string{"it chooses and starts the next role", "`register_gate`", "end the tree with `close_root`", "Every notice about an issue you own arrives on your own role topic", "The merge carries no handoff onto the pull request's base", "A `pull-request-missing` notice", "`close_root` answers `CLOSE_REASON_TOO_LONG`", "`CLOSE_REASON_DECISION_BLOCK`"}, architectOperations...)},
		{"sub-architect", claim.RoleArchitect, false, []string{"architect.md"}, []string{"architect.md", "architect-common.md"}, append([]string{"it chooses and starts the next role", "`register_gate` refuses a child issue", "Every notice about an issue you own arrives on your own role topic", "The merge carries no handoff onto the pull request's base", "A `pull-request-missing` notice", "`close_root` answers `CLOSE_REASON_TOO_LONG`", "`CLOSE_REASON_DECISION_BLOCK`"}, architectOperations...)},
		{"planner", claim.RolePlanner, false, []string{"core/common.md", "core/planner.md", "mechanics/headless.md", "planner.md"}, []string{"planner.md", "worker-common.md"}, []string{`op: "handoff_complete"`, "jj -R \"$LEGION_WORKSPACE\" git push --bookmark legion/<issue>", "dispatch handoff-write --issue <issue> --phase <word> --body-file", "dispatch handoff-read --issue <issue> --phase <word> --version <n>", "A handoff is never committed or pushed", "`HANDOFF_SESSION_MISMATCH`", "A refusal records nothing"}},
		// A root with no code of its own pushes one empty commit to open its pull request. The
		// implementer is also told the completion of implementing the handoff route refuses with no
		// open pull request, and what to do about it.
		{"implementer", claim.RoleImplementer, false, []string{"core/common.md", "core/implementer.md", "mechanics/headless.md", "implementer.md"}, []string{"implementer.md", "worker-common.md"}, []string{`op: "handoff_complete"`,
			"Retro removes nothing", "the root of its children; no code of its own",
			"(`HANDOFF_NO_PULL_REQUEST`), recording nothing: open the pull request and complete again at the same commit"}},
		// The tester runs the application, not CI again (dispatch://LEGION-631).
		{"tester", claim.RoleTester, false, []string{"core/common.md", "core/tester.md", "mechanics/headless.md", "tester.md"}, []string{"tester.md", "worker-common.md"}, []string{`op: "handoff_complete"`, `verdict: "pass"`, `verdict: "fail"`,
			"read CI for everything static", "try to break it", "dispatch handoff-write --issue <issue> --phase test"}},
		{"reviewer", claim.RoleReviewer, false, []string{"core/common.md", "core/reviewer.md", "mechanics/headless.md", "reviewer.md"}, []string{"reviewer.md", "worker-common.md"}, []string{`op: "handoff_complete"`,
			"Retro commits nothing, so nothing of retro's sits above your approved head", "`pullRequest.head` is the commit the pull request stands on"}},
		// The merge the merger is told to run names the head GitHub must match, on a queue base and
		// off it, and carries the message it writes itself; its completion is an ordinary phase
		// completion, refused only on the grounds every phase worker's is (dispatch://INFRA-714, R2).
		// It also disarms an auto-merge an older release armed, since GitHub holds an armed pull
		// request to no head, and it merges nothing while the design gate is closed (R5).
		{"merger", claim.RoleMerger, false, []string{"mechanics/headless.md", "merger.md"}, []string{"merger.md", "worker-common.md"}, []string{`op: "handoff_complete"`,
			"gh pr merge <n> --squash --match-head-commit <head> --subject '<pull request title> (#<n>)' --body ''",
			"enqueuePullRequest", "squash_merge_commit_message",
			"gh pr merge <n> --disable-auto", "autoMergeRequest", "dequeuePullRequest(input:{id:$pr})",
			"The design gate of <tree> closed: version <n> of its <step> document (<id>)",
			"`HANDOFF_NOT_CURRENT_PHASE`", "`HANDOFF_STALE_GENERATION`", "`HANDOFF_NO_RUN`",
			"`HANDOFF_ALREADY_RECORDED`", "`TREE_LINGERING`"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts, err := composer.Compose(tc.role, tc.isRoot)
			if err != nil {
				t.Fatalf("Compose: %v", err)
			}
			want := make([]string, 0, len(tc.shared)+len(tc.goParts))
			for _, part := range tc.shared {
				want = append(want, filepath.Join(stateDir, "prompts", "shared", part))
			}
			for _, part := range tc.goParts {
				want = append(want, filepath.Join(stateDir, "prompts", "go", part))
			}
			if !reflect.DeepEqual(parts.RolePromptPaths, want) {
				t.Fatalf("RolePromptPaths = %q, want %q", parts.RolePromptPaths, want)
			}
			// A pane reads each shared part from the snapshot by path, so the snapshot holds the
			// embedded part byte for byte.
			for _, part := range tc.shared {
				got, err := os.ReadFile(filepath.Join(stateDir, "prompts", "shared", part))
				if err != nil {
					t.Fatalf("read the snapshot's %s: %v", part, err)
				}
				if string(got) != rolePart(t, part) {
					t.Errorf("the snapshot's %s is not the embedded part", part)
				}
			}
			// Every launch joins the parts byte for byte (runtime.ComposePrompt), so the role's Go
			// daemon part and the shared one must meet at one blank line, as the paragraphs within a
			// part do. The phrases each role is held to are read from the whole composed prompt,
			// shared parts and the Go daemon's alike.
			var text strings.Builder
			for i, path := range parts.RolePromptPaths {
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read part: %v", err)
				}
				if joined := text.String(); i > len(tc.shared) {
					gap := len(joined) - len(strings.TrimRight(joined, "\n")) + len(body) - len(strings.TrimLeft(string(body), "\n"))
					if gap != 2 {
						t.Errorf("Go daemon parts %q join %s with %d newlines, want one blank line", tc.goParts, path, gap)
					}
				}
				text.Write(body)
			}
			for _, requirement := range append([]string{
				"This Go daemon advances phases",
				"re-read your issue record with the `legion` tool's `read_record`",
				// Every role is told not to retry a `legion` call its plugin refused because the
				// daemon answered a shape it does not read (INFRA-1048): the phrase pinned is the
				// plugin's own refusal text, which survives a rewording of the clauses around it.
				"the daemon API contract does not describe",
				// Every role is told the update round's terms (dispatch://INFRA-2981): its ask comes
				// as a message on the role's own topic, the end of a turn answers nothing, and the
				// round ends at the moment the ask names whatever the role answers.
				"The daemon can send an update message to your role topic.",
				"The end of a turn is no answer",
				"The round ends at the moment the message names, whether or not you answer.",
			}, tc.contains...) {
				if !strings.Contains(text.String(), requirement) {
					t.Errorf("the composed %s prompt omits %q", tc.name, requirement)
				}
			}
		})
	}
}

// The planner this daemon composes runs the gap analyst before it drafts and the plan reviewer
// after (LEGION-421). Only the shared headless residue names them: the core is also composed with
// the interactive fragment, and no load probe checks the agents an interactive session names, and
// the Go daemon's own parts leave the checks to the shared text.
func TestTheComposedPlannerDispatchesItsPlanChecksFromTheHeadlessResidue(t *testing.T) {
	stateDir := t.TempDir()
	composer, err := New(stateDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	parts, err := composer.Compose(claim.RolePlanner, false)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	var text strings.Builder
	dispatched := promptrefs.New()
	for _, path := range parts.RolePromptPaths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text.Write(body)
		rel, err := filepath.Rel(stateDir, path)
		if err != nil {
			t.Fatal(err)
		}
		dispatched.Text(filepath.Join("state", rel), body)
	}
	gap := strings.Index(text.String(), "`task(agent=\"plan-gap-analyst\")`")
	review := strings.Index(text.String(), "`task(agent=\"plan-reviewer\")`")
	if gap < 0 || review < gap {
		t.Errorf("the composed planner dispatches the gap analyst at %d and the plan reviewer at %d, want the analyst first", gap, review)
	}
	want := []string{"plan-gap-analyst", "plan-reviewer"}
	residue := []string{filepath.Join("state", "prompts", "shared", "planner.md")}
	for _, agent := range want {
		files, named := dispatched[promptrefs.TaskAgents][agent]
		if !named {
			t.Errorf("the composed planner does not dispatch task agent %s", agent)
			continue
		}
		if !slices.Equal(files, residue) {
			t.Errorf("task agent %s is dispatched by %q, want the headless residue %q alone", agent, files, residue)
		}
	}
}

// composedText is the whole text a role's composed prompt gives its pane: the parts Compose names,
// in the order the tmux runtime appends them (Parts.RolePromptPaths), read back from the snapshot
// and joined. A test that asks whether a prompt still says something retired asks it of this join,
// since which part holds the words is no part of the claim, and a compose or a read that fails
// leaves the question unanswered rather than unsaid, so both end the test (t.Fatalf).
func composedText(t *testing.T, composer *Composer, role claim.Role, isRoot bool) string {
	t.Helper()
	parts, err := composer.Compose(role, isRoot)
	if err != nil {
		t.Fatalf("Compose %s (root %t): %v", role, isRoot, err)
	}
	return readJoined(t, parts.RolePromptPaths...)
}

// No role's handoff is a commit: each is a record on the Dispatch issue, so no phase worker's
// composed prompt still teaches the committed handoff file, its commit and push, the trailer that
// kept a handoff-only push from starting CI, or retro's removal of the handoffs from the head that
// merges. Nor does any carry the rules that removal replaced: that the approved and the merged
// head keep .legion/, and that the daemon strips the leftovers from the next issue's branch. No
// prompt names READY either: the merger submits the merge itself (dispatch://INFRA-714, R2), so
// the refusals READY answered with went with it.
func TestNoComposedPromptCommitsAHandoff(t *testing.T) {
	stateDir := t.TempDir()
	composer, err := New(stateDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	retired := []string{"No role pushes a `.legion/` deletion", "No role removes `.legion/`", "nobody removes it before the merge", "strips whatever `.legion/`", "still carries `.legion/`",
		"Retro then commits only `docs/solutions/`", "no file changes above the approved head", "the same sha when retro added nothing", "Retro's `docs/solutions/` commit is never",
		"'~(docs/solutions | .legion/<issue>)'", ".legion/<issue>/<phase>.json", "record handoff", "handoff-only", "Retro's last commit removes",
		"committed handoff", "`READY_UNREVIEWED_CHANGE`", "`READY_NOT_APPROVED`", "Approved head:"}
	for _, role := range []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger} {
		text := composedText(t, composer, role, false)
		for _, phrase := range retired {
			if strings.Contains(text, phrase) {
				t.Errorf("the composed %s prompt still says %q", role, phrase)
			}
		}
		// Every phrase above is one sentence that said READY, and a new one would pass them all,
		// so the word itself is censused: the only READY a composed prompt may hold is the one
		// inside HANDOFF_ALREADY_RECORDED, the refusal that outlived the packet.
		for at := 0; ; {
			found := strings.Index(text[at:], "READY")
			if found < 0 {
				break
			}
			at += found + len("READY")
			if !strings.HasSuffix(text[:at], "ALREADY") {
				t.Errorf("the composed %s prompt names READY: %q", role, text[max(0, at-120):min(len(text), at+120)])
			}
		}
	}
}

// GitHub, under each repository's base-branch rules, decides whether a merge happens, and no
// prompt may tell an agent that Legion refuses one over a check (dispatch://INFRA-83). The daemon
// reads none of the head's checks and judges none, so no composed prompt — any phase worker's,
// and the architect's in both its shapes — still names the refusal that read them, the packet
// note that said which checks went unread, or a trailer the daemon turns away for the CI it
// leaves the head without.
func TestNoComposedPromptSaysLegionRefusesAMergeOverACheck(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	retired := []string{"READY_CHECKS_NOT_GREEN", "no check is required on", "refused at READY"}
	for _, tc := range []struct {
		role   claim.Role
		isRoot bool
	}{
		{claim.RolePlanner, false}, {claim.RoleImplementer, false}, {claim.RoleTester, false},
		{claim.RoleReviewer, false}, {claim.RoleMerger, false},
		{claim.RoleArchitect, true}, {claim.RoleArchitect, false},
	} {
		text := composedText(t, composer, tc.role, tc.isRoot)
		for _, phrase := range retired {
			if strings.Contains(text, phrase) {
				t.Errorf("the composed %s prompt (root %t) still says %q", tc.role, tc.isRoot, phrase)
			}
		}
	}
}

// An issue with no Verification document, or with one holding no `before_merge` check, is a
// coverage gap every role names and none refuses (dispatch://INFRA-174, R1 and R2): every gate
// counts the `check` blocks that exist, and a rule that made the empty case a failure would stop
// every issue in flight on the day these prompts deploy. So the tester records the gap in its
// handoff without failing the phase for it, the reviewer names it in its review without asking
// for changes over it, and the architect's acceptance comment names it and decides, which is the
// only gate on an issue that has no `before_merge` check. Each composed prompt that carries the
// rule says the empty case and its role's answer to it, and no paragraph of that rule calls the
// case a failure or a request for changes. Each is also told what a child proves: a child's own
// check whose `serves` names the root's requirement, never a result against the root's own check.
func TestTheComposedPromptsNameAnIssueWithNoBeforeMergeCheckAsAGap(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const empty = "no Verification document, or with one that holds no `before_merge` check"
	// A rule paragraph that says any of these has made the gap a refusal again. They are matched
	// against the lower-cased paragraph, so a sentence that opens with one is caught too.
	refusals := []string{"a failure", "a test failure", "fail the phase", "fails the phase",
		"request_changes", "request changes", "requests changes", "`rejected`",
		"is proved by nothing", "has no evidence at all", "accepted by nobody"}
	for _, tc := range []struct {
		role    claim.Role
		isRoot  bool
		answers []string
	}{
		{claim.RoleTester, false, []string{"is a gap you name, never a reason of its own to send the issue back"}},
		{claim.RoleReviewer, false, []string{"is a gap you name in your review, never a review that asks for changes"}},
		{claim.RoleArchitect, false, []string{"your acceptance is the only gate on it", "name that gap in your acceptance comment and decide there"}},
		{claim.RoleArchitect, true, []string{"your acceptance is the only gate on it", "name that gap in your acceptance comment and decide there"}},
	} {
		text := composedText(t, composer, tc.role, tc.isRoot)
		paragraphs := ruleParagraphs(text, empty)
		if len(paragraphs) == 0 {
			t.Errorf("the composed %s prompt (root %t) omits %q", tc.role, tc.isRoot, empty)
		}
		for _, paragraph := range paragraphs {
			for _, refusal := range refusals {
				if strings.Contains(strings.ToLower(paragraph), refusal) {
					t.Errorf("the composed %s prompt (root %t) still calls the gap %q: %s",
						tc.role, tc.isRoot, refusal, paragraph)
				}
			}
		}
		for _, want := range append(tc.answers, "`serves` names") {
			if !strings.Contains(foldWhitespace(text), want) {
				t.Errorf("the composed %s prompt (root %t) omits %q", tc.role, tc.isRoot, want)
			}
		}
	}
}

// foldWhitespace is one line of text, so a phrase a hard-wrapped prompt breaks over two lines is
// still one phrase to match.
func foldWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// readJoined is the bytes of each of paths, read in order and joined; a read that fails ends the
// test (t.Fatalf), naming the path.
func readJoined(t *testing.T, paths ...string) string {
	t.Helper()
	var text strings.Builder
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text.Write(body)
	}
	return text.String()
}

// ruleParagraphs are the blank-line-separated paragraphs of a composed prompt that hold marker,
// each folded to one line: a paragraph is the span a reader takes the rule and its framing from,
// so it is the span a test asks what the rule calls its case. A composed prompt holds one per
// part that carries the rule (the role's own part and the core its phase shares).
func ruleParagraphs(text, marker string) []string {
	var found []string
	for _, paragraph := range strings.Split(text, "\n\n") {
		if folded := foldWhitespace(paragraph); strings.Contains(folded, marker) {
			found = append(found, folded)
		}
	}
	return found
}

// clauses are the clauses of text, each folded to one line: each blank-line paragraph cut after
// every full stop and every semicolon a space follows. A code span's own full stop (`<title>.`) is
// followed by its closing backtick, so it ends no clause, and a hard wrap inside one leaves it
// whole. A clause is the span a reader takes one verdict's rule from: a sentence that lists the
// three verdicts gives each its own clause, and the words of one clause limit no other.
func clauses(text string) []string {
	var found []string
	for _, paragraph := range strings.Split(text, "\n\n") {
		for _, sentence := range strings.SplitAfter(foldWhitespace(paragraph), ". ") {
			found = append(found, strings.SplitAfter(sentence, "; ")...)
		}
	}
	return found
}

// What a phase proved is a result recorded against a `check` block of the issue's Verification
// document (`dispatch result`), and the pull request body carries one line per check. No composed
// prompt — any phase worker's, and the architect's in both its shapes — still teaches the two
// lines the body carried before, the six-field `proof` array a handoff carried, or the
// `Production:` line the production check wrote, which is now an `after_merge` result a closed
// issue takes. The two line names are built from their role rather than written out, so a search
// of the repository for either finds the texts that teach it and not this test.
//
// The production check's own retired sentences are read here too (retiredCheckSentences): the
// check is no longer the implementer's, and no deployment's event wakes a look. This test reads
// what Compose joins, so it answers for the prompt a role is actually given;
// TestNoPartMakesTheProductionCheckTheImplementersOrWakesItFromADeploy reads the same sentences
// over every part of both trees, where one Compose never joins would otherwise sit unread.
func TestNoComposedPromptTeachesAnE2ELineOrAProofArray(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	retired := []string{"`proof` array", "`Production:` line",
		"pending deploy of", "the production-like proof", "the implementer's proof"}
	retired = append(retired, retiredCheckSentences...)
	for _, role := range []string{"implementer", "tester"} {
		retired = append(retired, fmt.Sprintf("E2E (%s)", role))
	}
	for _, tc := range []struct {
		role   claim.Role
		isRoot bool
	}{
		{claim.RolePlanner, false}, {claim.RoleImplementer, false}, {claim.RoleTester, false},
		{claim.RoleReviewer, false}, {claim.RoleMerger, false},
		{claim.RoleArchitect, true}, {claim.RoleArchitect, false},
	} {
		text := composedText(t, composer, tc.role, tc.isRoot)
		for _, phrase := range retired {
			if strings.Contains(text, phrase) {
				t.Errorf("the composed %s prompt (root %t) still says %q", tc.role, tc.isRoot, phrase)
			}
		}
	}
}

// A state directory outlives the daemon binary that wrote its prompt snapshot, so a boot of a newer
// daemon finds the older daemon's words there. A part whose content is not the running binary's is
// rewritten, shared or the daemon's own: a prompt that contradicts the daemon's own behaviour (who
// posts the merge) is the defect, and nothing treats these files as the operator's to edit (the
// operator's text is `instructions`). A part already holding the running binary's content is left
// as it is, so an ordinary restart writes nothing.
func TestNewRewritesAPartTheRunningDaemonDidNotWrite(t *testing.T) {
	stateDir := t.TempDir()
	if _, err := New(stateDir); err != nil {
		t.Fatalf("first New: %v", err)
	}
	snapshot := func(name string) string { return filepath.Join(stateDir, "prompts", name) }
	embedded := map[string]string{
		"go/merger.md":          "go/merger.md",
		"go/tester.md":          "go/tester.md",
		"shared/merger.md":      "roles/merger.md",
		"shared/core/tester.md": "roles/core/tester.md",
	}
	stale := []string{"go/merger.md", "shared/merger.md"}
	current := []string{"go/tester.md", "shared/core/tester.md"}
	for _, name := range stale {
		if err := os.WriteFile(snapshot(name), []byte("an older daemon's "+name+"\n"), 0o600); err != nil {
			t.Fatalf("write the older part: %v", err)
		}
	}
	written := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, name := range current {
		if err := os.Chtimes(snapshot(name), written, written); err != nil {
			t.Fatalf("date the current part: %v", err)
		}
	}
	if _, err := New(stateDir); err != nil {
		t.Fatalf("second New: %v", err)
	}
	for name, source := range embedded {
		parts := goParts
		if strings.HasPrefix(source, "roles/") {
			parts = roleParts
		}
		want, err := parts.ReadFile(source)
		if err != nil {
			t.Fatalf("read embedded %s: %v", source, err)
		}
		got, err := os.ReadFile(snapshot(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s after the second New = %q, want the running daemon's part", name, got)
		}
	}
	for _, name := range current {
		if info, err := os.Stat(snapshot(name)); err != nil || !info.ModTime().Equal(written) {
			t.Errorf("%s, already holding the running daemon's content, was written again: %v", name, err)
		}
	}
}

// stepClauses are the clauses of a composed prompt that name marker: its sentences, split again
// at each comma, since a rule that binds each design-gate step to its document names one pair
// per clause. A clause is the span a reader takes one binding from, so a test asks which
// document a step is bound to of the clause that names that step, never of the prompt as a
// whole: a prompt naming the three steps in one place and the three documents in another would
// satisfy the second question while teaching nothing.
func stepClauses(text, marker string) []string {
	var found []string
	for _, sentence := range strings.Split(foldWhitespace(text), ". ") {
		for _, clause := range strings.Split(sentence, ", ") {
			if strings.Contains(clause, marker) {
				found = append(found, clause)
			}
		}
	}
	return found
}

// The root architect's composed prompt binds each design-gate step to the document that step
// records, so an architect whose addressing line names a step knows what to register for it; a
// step bound in a clause that names another step binds nothing, since the architect cannot tell
// which of the two the document belongs to. The prompt also tells a root whose gate was carried
// what to do about a step nothing has registered — register it, on this issue's own document —
// rather than only what not to do, and tells every root how to find which step a review notice
// was of, since `design-approved`, `design-changes-requested` and `carried-gate-opened` each
// name a version and no step. The policy words the list replaced are gone from both parts: an
// architect told to read `gates.design: root-issues` off its addressing would wait for a line
// the daemon no longer writes.
func TestTheRootArchitectPartNamesTheGatedSteps(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	text := composedText(t, composer, claim.RoleArchitect, true)
	documents := []struct {
		step     record.DesignStep
		document string
	}{
		{record.DesignStepDecisions, "decisions document"},
		{record.DesignStepCriteria, "Verification document"},
		{record.DesignStepSpec, "primary document"},
	}
	for _, bind := range documents {
		bound := false
		for _, clause := range stepClauses(text, "`"+string(bind.step)+"` step") {
			if !strings.Contains(clause, bind.document) {
				continue
			}
			others := false
			for _, other := range documents {
				if other.step != bind.step && strings.Contains(clause, "`"+string(other.step)+"`") {
					others = true
				}
			}
			if !others {
				bound = true
			}
		}
		if !bound {
			t.Errorf("the composed root architect prompt binds the `%s` step to its %s in no clause of its own",
				bind.step, bind.document)
		}
	}
	// The architect reads the steps off its addressing line, so both parts name that line by the
	// words the daemon writes on it (daemon.DesignGateFragment).
	if !strings.Contains(text, "`Design gate steps`") {
		t.Error("the composed root architect prompt never names the `Design gate steps` line its steps come from")
	}
	// Folded, because a retired phrase broken across two lines is still a retired phrase a reader
	// follows, and the line a prompt wraps at is nobody's decision.
	for _, retired := range retiredGatePhrases {
		if strings.Contains(foldWhitespace(text), retired) {
			t.Errorf("the composed root architect prompt still says %q", retired)
		}
	}
	carried := ruleParagraphs(text, "says `carried`")
	if len(carried) != 2 {
		t.Fatalf("the composed root architect prompt holds %d paragraphs on a carried root, want one in each part", len(carried))
	}
	for _, paragraph := range carried {
		folded := strings.ToLower(paragraph)
		if !strings.Contains(folded, "register each step the catch-up reports `unregistered`") {
			t.Errorf("the carried-root rule does not tell the architect to register each unregistered step: %s", paragraph)
		}
		if strings.Contains(folded, "never `register_gate`") {
			t.Errorf("the carried-root rule still forbids every registration: %s", paragraph)
		}
		if !strings.Contains(paragraph, "this issue's own document for that step") {
			t.Errorf("the carried-root rule does not say which document that registration names: %s", paragraph)
		}
	}
	notices := ruleParagraphs(text, "`design-changes-requested`")
	if len(notices) != 2 {
		t.Fatalf("the composed root architect prompt holds %d paragraphs on the review notices, want one in each part", len(notices))
	}
	for _, paragraph := range notices {
		for _, want := range []string{"`gatedSteps`", "`read_record`"} {
			if !strings.Contains(paragraph, want) {
				t.Errorf("the review-notice rule does not send the architect to %s for the step the notice leaves out: %s", want, paragraph)
			}
		}
	}
}

// retiredGatePhrases are the words the step list replaced. A prompt that still carries one tells
// its reader to look on the addressing line for a sentence the daemon no longer writes. The last
// three are the registration words option b' replaced (dispatch://INFRA-175, Design 7): a root
// with a Verification document registers it as `criteria` under a line that names `spec` alone,
// not only where the line names both, and each document is registered at the version the
// approval question names for it, which a request for two documents prints once for each.
var retiredGatePhrases = []string{"gates.design: root-issues", "gates.design: off", "Design gate policy",
	"either design gate policy", "The gate records one document",
	"Where the line names both `criteria` and `spec`", "at the version the request returned", "at the version it returned"}

// The controller reads the same addressing line as a root architect, so its own prompt names the
// steps sentence and carries none of the words that sentence replaced. Its text is read folded:
// the retired phrase that outlived the first sweep was one broken across two lines.
func TestTheControllerPartNamesTheGatedSteps(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, headless := range []bool{true, false} {
		paths, err := composer.ControllerPromptPaths(headless)
		if err != nil {
			t.Fatalf("ControllerPromptPaths(headless %t): %v", headless, err)
		}
		folded := foldWhitespace(readJoined(t, paths...))
		if !strings.Contains(folded, "`Design gate steps:`") {
			t.Errorf("the composed controller prompt (headless %t) never names the `Design gate steps:` line it reads its steps off", headless)
		}
		for _, retired := range retiredGatePhrases {
			if strings.Contains(folded, retired) {
				t.Errorf("the composed controller prompt (headless %t) still says %q", headless, retired)
			}
		}
	}
}

// A push moves the branch, but the pull request can keep naming the old commit, so the new
// commit gets no checks and nothing reports it (dispatch://INFRA-1318). Every pushing role's
// composed prompt therefore carries the read of the pull request's own head after its own push,
// escalation sentence included, and neither architect shape does: a later edit that moves the
// step into a part both shapes compose fails here rather than telling an architect to push. The
// implementer's part carries the carve-out that names the step's spaced reads as the one sleep
// its "never sleep in a loop for anything" rule allows. This shows the text is loaded, never
// that a role follows it.
func TestTheComposedPushingPromptsPinTheReadOfThePullRequestHead(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const headRead = "gh pr view <n> --repo <the issue's repository> --json headRefOid --jq .headRefOid"
	step := []string{
		headRead,
		"about ten seconds after your push, again about ten seconds later, and a last time about thirty seconds after that, with a read that gives the pushed commit ending the step there and only the last read deciding a mismatch",
		"once the branch has an open pull request",
		"an empty one when your work is complete",
		"tell your architect the commit you pushed, the head each read gave and the time of each, and end the turn with a WAITING line naming that answer rather than completing: a phase completed here hands the next role a commit the pull request does not name.",
		"The merger pushes only a merge of the base forward, and makes this read after that push as any role does",
	}
	for _, role := range []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester,
		claim.RoleReviewer, claim.RoleMerger} {
		text := foldWhitespace(composedText(t, composer, role, false))
		for _, phrase := range step {
			if !strings.Contains(text, phrase) {
				t.Errorf("the composed %s prompt omits %q", role, phrase)
			}
		}
	}
	const carveOut = "the step's spaced reads of the pull request's head after a push are the one sleep this rule allows, and they read no check"
	if !strings.Contains(foldWhitespace(composedText(t, composer, claim.RoleImplementer, false)), carveOut) {
		t.Errorf("the composed %s prompt omits %q, so it holds \"never sleep in a loop for anything\" beside a step whose reads are spaced by sleeps, with nothing saying that rule allows them",
			claim.RoleImplementer, carveOut)
	}
	for _, isRoot := range []bool{true, false} {
		text := foldWhitespace(composedText(t, composer, claim.RoleArchitect, isRoot))
		if strings.Contains(text, headRead) {
			t.Errorf("the composed %s prompt (root %t) says %q", claim.RoleArchitect, isRoot, headRead)
		}
	}
}

// A review the review App submits decides a round only when the head its Legion footer names is
// the pull request's current head (`intake.PullRequestReview.LegionHead`, `workflow.refusesRound`),
// so a reviewer whose prompt teaches the footer without the head submits a review every round
// sets aside. The composed reviewer prompt therefore carries the footer's own shape with the
// head in it, which head that is — the one whose diff and checks the review read, never the head
// GitHub happens to show at the moment of submission, since a review of an unread head is the
// fault the rule exists to stop — the read that tells the reviewer a push landed under it
// (`--json headRefOid`), the reason GitHub's own `commit_id` cannot stand in for the footer, and
// what a wrong head costs. The same footer names the entry of the reviewer's round, the number of
// the newest ` Review entry: <n>.` sentence that is the last sentence of a task or a wake of its
// session (a task's own text can quote the same words above it), since only
// a review whose footer names the entry on the reviewer's row moves the commit a later round reads
// from: the shape with the entry in both the template and the rule, the rule whole in the daemon
// part (a message that names no entry changes none, and the field is left out only where nothing in
// the session named one), and that a review with no entry, or another one, still decides the round.
// The entry is a JSON string, `"entry":"<entry>"`, and the prompt says why: a placeholder left
// unquoted makes the footer invalid JSON, which loses its session and its head too, so the review
// is set aside, while a quoted value that is no number costs the entry alone. The prompt says which
// sentence the entry is read from in exactly the two places pinned, the role part's and the daemon
// part's, so no third wording of that rule stands beside them. The footer with no entry is gone.
// This shows the text is loaded, never that a reviewer follows it.
func TestTheComposedReviewerPromptWritesTheHeadItReviewedInItsFooter(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	text := foldWhitespace(composedText(t, composer, claim.RoleReviewer, false))
	for _, phrase := range []string{
		`"phase":"review","head":"<the head you reviewed>","entry":"<entry>"} -->`,
		`\"phase\":\"review\",\"head\":\"<head-sha>\",\"entry\":\"<entry>\"} -->`,
		"beside its session, its phase and its `\"entry\":\"<entry>\"`",
		"gh pr view <n> --repo <owner>/<repo> --json headRefOid --jq .headRefOid",
		"the head whose diff and checks you read",
		"rather than renaming the footer to a head you have not read",
		"GitHub rewrites `commit_id` to the pull request's current head",
		"A review whose footer names another head, or no head, decides no round",
		"The footer's `entry` is the entry of your round, the number of the newest `Review entry: <n>.` sentence that is the last sentence of a task or a wake of this session (the daemon part below gives the rule whole), written in quotes as a string of digits, `\"entry\":\"7\"`:",
		"It is in quotes because a footer that is not valid JSON, such as one with an unquoted `<entry>` left in it, loses its session and its head too, and Legion then sets the review aside, while a quoted value that is no number costs the entry alone.",
		"by it Legion tells a review of this round from a review of an earlier round, and only a review of this round moves the commit a later round reads from",
		"A review with no entry, or with another one, still decides the round, and the next round then reads more: from an older commit, or the whole change.",
		"The `<entry>` is the entry of your round, in digits, as the next paragraph says; a review with no entry, or with another one, decides the round as any other, but moves no commit.",
		"The entry of your round is the number of the newest `Review entry: <n>.` sentence that is the last sentence of a task or a wake of this session. A message that names none, such as a notice, a reminder or an Envoy card, does not change it. Leave the `entry` field out of your footer only where nothing in this session named one.",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("the composed reviewer prompt omits %q", phrase)
		}
	}
	if rule := "the number of the newest `Review entry: <n>.` sentence"; strings.Count(text, rule) != 2 {
		t.Errorf("the composed reviewer prompt says %q %d times, want twice: the role part's sentence and the daemon part's, each pinned above, "+
			"which read the entry from the last sentence of a task", rule, strings.Count(text, rule))
	}
	for _, retired := range []string{
		`"phase":"review","head":"<the head you reviewed>"} -->`,
		`\"phase\":\"review\",\"head\":\"<head-sha>\"} -->`,
	} {
		if strings.Contains(text, retired) {
			t.Errorf("the composed reviewer prompt still says %q, a footer with no entry", retired)
		}
	}
}

// The task of a later review round names the commit the reviewer's own newest deciding review of
// its round read (` Last reviewed commit: <sha>.`, which workflow's namingTheReviewRound writes from
// record.PhaseRow.ReviewedHead), so the reviewer can read the difference from that commit rather
// than the whole change again. The composed reviewer prompt therefore names the field, the
// function that writes it into the task, and whose review sets it: the session the daemon recorded
// on the claim it started the reviewer under, in the first round as in every later one, in a review
// whose Legion footer names the entry on the reviewer's row (` Review entry: <n>.`, the task's last
// sentence, record.PhaseRow.Entry, which the task of a wake ends with too) while that row holds the
// entry's task. The core part names the commit in the daemon part's words, the head the Legion
// footer of the reviewer's own newest deciding review named, since a deciding review whose footer
// names no entry or another one moves no commit. It says to read the difference from that commit
// and each thread, takes that commit as the earlier head the review sizes itself to, names in each
// task of the thermonuclear pair the diff it reviews, keeps every defect seen outside it a
// finding, and reads the whole change where the commit is no ancestor of the head. The pair's old
// sentence, which ran it over the whole change in every round, is gone, and so is the
// unchanged-diff fingerprint, which dispatch://INFRA-4406 R1 deleted from the role prompts. This
// shows the text is loaded, never that a reviewer follows it.
func TestTheComposedReviewerPromptReadsALaterRoundFromTheCommitItsTaskNames(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	text := foldWhitespace(composedText(t, composer, claim.RoleReviewer, false))
	for _, phrase := range []string{
		"Last reviewed commit: <sha>",
		"no entry into a phase clears it",
		"`namingTheReviewRound` in `packages/daemon/internal/workflow/review_round.go` writes it into the task",
		"Your session is the one the daemon recorded on the claim it started you under, in your first round as in every later one;",
		"it is recorded when your agent registers, and while that claim records no session, no review of the review App decides the round or sets the commit.",
		"Your task names the entry of your round as its last sentence, `Review entry: <n>.`: the number of your entry into the round, which the daemon keeps on your phase row (`PhaseRow.Entry` in `packages/daemon/internal/record/record.go`) and raises at each new entry.",
		"The task a wake gives you once your pod was released and brought back ends with it too, as each task of the round does.",
		"When the task of a round names `Last reviewed commit: <commit>`, the head the Legion footer of your own newest deciding review named (the daemon part below says which review that is), read the difference from that commit to the head (`git diff <commit> <head>`), and each thread",
		"an `APPROVE` or a `REQUEST_CHANGES` your session submitted as the review App whose footer names the entry your row holds, and which the daemon took as its round's decision while your row held that entry's task, which a new generation's clear empties until the daemon enters you again.",
		"A `COMMENT`, a review the daemon set aside, a person's review and a review whose footer names no entry or another than your row's, such as one of a round before a new generation or a carried hand-over's promotion entered you again, leave it where it stands.",
		"the head read by the newest of your own deciding reviews whose footer named the entry of its round; the daemon part below gives the whole rule and where the commit comes from.",
		"read the difference from that commit to the head (`git diff <commit> <head>`), and each thread",
		"over the diff that round reads, when that diff touches runtime code, and name that diff in each agent's task",
		"The commit limits what you must read, never what you report: a defect you see outside the difference is a finding like any other",
		"On a new head, that commit is your earlier head, unless it is not an ancestor of the head (`git merge-base --is-ancestor <commit> <head>` does not exit 0): then you read the whole change, as for `none`.",
		"Size your review to the change you read: review again only what it can affect, and confirm the rest by naming the earlier head.",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("the composed reviewer prompt omits %q", phrase)
		}
	}
	for _, retired := range []string{
		"Run the thermonuclear pair once at the head the round reviews when the diff touches runtime code",
		"fingerprint",
	} {
		if strings.Contains(text, retired) {
			t.Errorf("the composed reviewer prompt still says %q", retired)
		}
	}
}

// The agent that does the retro rates the plan in the completion that ends the retro, and the rule
// of the rating is stated in prose once, in the step of skill://legion-retro that gives the two
// fields (pi-legion's skills-guard test pins it). The composed prompt of the role the daemon starts
// on the retro (phase.RoleFor) sends it there: it names the two fields the completion carries, the
// skill, and the three refusals the daemon answers a completion that breaks the rule with (api's
// planRatingOf and workflow's planRatingRefusal), each code as the daemon writes it. This shows the
// text is loaded, never that a role follows it.
func TestTheComposedRetroNamesTheRatingsFieldsAndRefusals(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	retro := phase.RoleFor(phase.Retro)
	text := foldWhitespace(composedText(t, composer, retro, false))
	for _, token := range []string{
		"`plan_rating`", "`plan_reason`", "`skill://legion-retro`",
		"`PLAN_RATING_REQUIRED`", "`PLAN_RATING_UNEXPECTED`", "`PLAN_RATING_INPUT`",
	} {
		if !strings.Contains(text, token) {
			t.Errorf("the composed %s prompt, the retro's, omits %s", retro, token)
		}
	}
}

// codeGraphActions are the actions the `codegraph` tool takes: the action table of
// @bopstack/pi-codegraph (its src/descriptors.ts), at the version packages/daemon/docker/omp-plugins
// pins for the worker image. A prompt that names another action sends its role to a call the tool
// refuses.
var codeGraphActions = []string{
	"help", "search", "explore", "node", "callers", "callees", "files", "status",
	"impact", "affected", "init", "index", "sync", "unlock",
}

// codeGraphPluginVersion is the version of @bopstack/pi-codegraph that codeGraphActions copies, and
// codeGraphCLIVersion the version of the CodeGraph CLI (@colbymchenry/codegraph) whose commands
// codeGraphCLIForms and go/claude-mechanics.md copy and whose answers the prompts' CodeGraph rule
// describes. Each is the pin of the worker image.
const (
	codeGraphPluginVersion = "0.1.1"
	codeGraphCLIVersion    = "1.6.4"
)

// codeGraphRoles classifies every role a claim can be on by what its prompt must say about the
// `codegraph` tool: the actions of the step it carries at the point where it decides, besides
// `status` and `init`, which the rule for a missing, an empty or a stale answer names for every role
// that reads code. A role that is not here fails TestEveryCodeReadingRoleIsToldWhenToAskCodeGraphOnEachHost
// until its actions are listed. Every claim role reads code today. A claim role added later that
// reads no code needs this test to hold its prompt to no `xd://codegraph` instead. The controller
// has no claim role: its spawn spec carries no repository or workspace, so its prompt files are
// checked apart.
var codeGraphRoles = map[claim.Role][]string{
	claim.RoleArchitect:   {"impact", "callers", "affected", "explore"},
	claim.RolePlanner:     {"impact", "callers", "affected", "explore"},
	claim.RoleImplementer: {"impact", "callers", "affected"},
	claim.RoleTester:      {"impact", "callers", "affected"},
	claim.RoleReviewer:    {"impact", "callers"},
	claim.RoleMerger:      {"callers", "impact"},
}

// codeGraphByCommand is whether each backend's roles reach CodeGraph through the `codegraph`
// command, which the backend's prompt part maps action by action, rather than through the tool that
// every role's own text names. Claude Code has no such tool. A backend that is not here fails the
// test until someone says how its roles reach CodeGraph.
var codeGraphByCommand = map[runtime.Backend]bool{
	runtime.BackendOmp:    false,
	runtime.BackendClaude: true,
}

// codeGraphCLIForms is the command a role that reaches CodeGraph by its command runs for each
// action a prompt names.
var codeGraphCLIForms = map[string]string{
	"impact":   "codegraph impact <symbol>",
	"callers":  "codegraph callers <symbol>",
	"affected": "codegraph affected <path> --depth 1",
	"explore":  "codegraph explore <words>",
	"status":   "codegraph status",
	"init":     "codegraph init <path>",
}

// dockerPin is the version the worker image pins for package, read from the package.json under
// packages/daemon/docker/dir.
func dockerPin(t *testing.T, dir, pkg string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "docker", dir, "package.json"))
	if err != nil {
		t.Fatalf("read the pin of %s: %v", pkg, err)
	}
	var pinned struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(body, &pinned); err != nil {
		t.Fatalf("decode the pin of %s: %v", pkg, err)
	}
	return pinned.Dependencies[pkg]
}

// Every role that reads or changes code is told when to ask the `codegraph` tool, for the host it
// runs on: the architects before the spec names what a change touches, the planner before a task
// names its files and tests, the implementer before it changes a symbol, the tester before it
// judges the implementer's tests, the reviewer for each changed exported symbol and the merger
// where the base changed code the change uses. The inventory is every claim role on every backend
// that runs it, each row the prompt that backend launches the role with, so a role or a backend
// added later fails here until it is classified, and the controller, which works no checkout, is
// told nothing.
//
// Each prompt gives the call's shape once and the rule for a missing tool and for an empty, a stale
// or a partial answer once. A role that took `status` or the tool's own advice would start a second
// build beside the warm-up's, which corrupts the index. Each prompt has the role check the
// warm-up's lease file with no CodeGraph call before its first query and before each `status`,
// because a `status` during the warm-up's build made that build fail. Every action a prompt names
// is one the tool takes and one the role's row lists, `affected` always with `"depth":1`, since its
// default depth fans out across unrelated tests (3,101 test files for one Go file at depth 5 and 17
// at depth 1, measured on CodeGraph 1.6.4). This shows the text is loaded, never that a role
// follows it.
func TestEveryCodeReadingRoleIsToldWhenToAskCodeGraphOnEachHost(t *testing.T) {
	if got := dockerPin(t, "omp-plugins", "@bopstack/pi-codegraph"); got != codeGraphPluginVersion {
		t.Fatalf("@bopstack/pi-codegraph is pinned at %q and codeGraphActions copies %s: re-read src/descriptors.ts of the pinned version and update codeGraphActions", got, codeGraphPluginVersion)
	}
	if got := dockerPin(t, "codegraph", "@colbymchenry/codegraph"); got != codeGraphCLIVersion {
		t.Fatalf("@colbymchenry/codegraph is pinned at %q and codeGraphCLIForms copies %s: run `codegraph <subcommand> --help` of each form in the pinned version and update codeGraphCLIForms and go/claude-mechanics.md", got, codeGraphCLIVersion)
	}

	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const (
		callShape          = "`write` to `xd://codegraph` the content `{\"action\":"
		staleAnswer        = "It does not see an edit made since the build, by you, by another role or by the base"
		pendingChange      = "`status` counts each changed file as a pending change"
		buildRunning       = "While a build runs, `status` says the index is truncated or that an index rebuild is running in another process"
		leaseCheck         = "run `find <the workspace's absolute path>/.codegraph/legion-warm.lock -mmin -1`"
		leaseFresh         = "When it prints the path, read the source, make no `codegraph` call"
		statusOnFailure    = "Ask `{\"action\":\"status\"}` when an answer is empty or failed"
		readsSource        = "Read the source with grep and the read tool in three cases"
		missingTool        = "The tool is missing: it says it is unavailable on `PATH`, or the workspace is not initialized"
		emptyAnswer        = "An empty answer never means \"none\""
		partialAnswer      = "The answer may be stale or partial"
		anyEditor          = "whoever changed it"
		nameMatching       = "`impact` and `callers` match by name"
		blockPerDefinition = "give one block for each definition of a name"
		laterInit          = "only when that later answer says the workspace is not initialized too"
		noIndexAction      = "Run no other index action, whatever `status` or the tool's own guidance advises"
		// Held only by a prompt of a role that reaches CodeGraph by its command, and the retired
		// call and the retired sentence, which no prompt holds.
		claudeMapping      = "is the `codegraph` command"
		retiredCall        = "codegraph({"
		retiredStatusFirst = "Ask `{\"action\":\"status\"}` before your first query"
	)
	onceEach := []string{callShape, staleAnswer, pendingChange, leaseCheck, leaseFresh, buildRunning, statusOnFailure, readsSource, missingTool, emptyAnswer, partialAnswer, anyEditor, nameMatching, blockPerDefinition, laterInit, noIndexAction}
	action := regexp.MustCompile(`"action":"([^"]*)"`)
	affectedCall := regexp.MustCompile(`\{"action":"affected"[^}]*\}`)

	for role := range codeGraphRoles {
		if !claim.IsRole(role) {
			t.Errorf("codeGraphRoles classifies %q, which is not a claim role", role)
		}
	}
	backends := slices.Sorted(maps.Keys(runtime.Backends))
	for _, backend := range backends {
		byCommand, ok := codeGraphByCommand[backend]
		if !ok {
			t.Errorf("the backend %q has no row in codeGraphByCommand: say how its roles reach the `codegraph` tool", backend)
		} else if byCommand && runtime.Backends[backend].PromptPart == "" {
			t.Errorf("the backend %q reaches CodeGraph by its command, and names no prompt part that maps the actions to it", backend)
		}
	}

	for _, role := range claim.Roles {
		listed, ok := codeGraphRoles[role]
		if !ok {
			t.Errorf("the role %q has no row in codeGraphRoles: say whether its prompt must tell it when to ask the `codegraph` tool", role)
			continue
		}
		named := append([]string{"status", "init"}, listed...)
		for _, isRoot := range []bool{true, false} {
			if isRoot && role != claim.RoleArchitect {
				continue
			}
			for _, backend := range backends {
				info := runtime.Backends[backend]
				if !info.Runs(role) {
					continue
				}
				row := fmt.Sprintf("%s (root %t) on %s", role, isRoot, backend)
				t.Run(row, func(t *testing.T) {
					launched := composedText(t, composer, role, isRoot)
					if info.PromptPart != "" {
						launched += readJoined(t, composer.Part(info.PromptPart))
					}
					text := foldWhitespace(launched)
					for _, needle := range onceEach {
						if n := strings.Count(text, needle); n != 1 {
							t.Errorf("the prompt of %s holds %q %d times, want once", row, needle, n)
						}
					}
					if strings.Contains(text, retiredCall) {
						t.Errorf("the prompt of %s holds %q, a call no host can make", row, retiredCall)
					}
					if n := strings.Count(text, retiredStatusFirst); n != 0 {
						t.Errorf("the prompt of %s holds %q %d times, want 0: a `status` while the warm-up builds the index makes that build fail", row, retiredStatusFirst, n)
					}
					found := map[string]bool{}
					for _, match := range action.FindAllStringSubmatch(text, -1) {
						found[match[1]] = true
						switch {
						case !slices.Contains(codeGraphActions, match[1]):
							t.Errorf("the prompt of %s names the action %q, which the `codegraph` tool does not take (%q)", row, match[1], codeGraphActions)
						case !slices.Contains(named, match[1]):
							t.Errorf("the prompt of %s names the action %q, which is not one of the actions codeGraphRoles lists for it (%q, and status and init)", row, match[1], listed)
						}
					}
					for _, want := range named {
						if !found[want] {
							t.Errorf("the prompt of %s never names the action %q", row, want)
						}
					}
					for _, call := range affectedCall.FindAllString(text, -1) {
						if !strings.Contains(call, `"depth":1`) {
							t.Errorf("the prompt of %s names the call %s without \"depth\":1: the default depth fans out across unrelated tests", row, call)
						}
					}
					wantMapping := 0
					if codeGraphByCommand[backend] {
						wantMapping = 1
						forms := map[string]string{}
						for _, want := range named {
							form, ok := codeGraphCLIForms[want]
							if !ok {
								t.Errorf("codeGraphCLIForms has no command for the action %q, which the prompt of %s names", want, row)
								continue
							}
							forms[want] = form
						}
						for what, form := range forms {
							if !strings.Contains(text, form) {
								t.Errorf("the prompt of %s does not say that %s is %q", row, what, form)
							}
						}
					}
					if n := strings.Count(text, claudeMapping); n != wantMapping {
						t.Errorf("the prompt of %s holds %q %d times, want %d: only a backend that reaches CodeGraph by its command is told the command for each action", row, claudeMapping, n, wantMapping)
					}
				})
			}
		}
	}

	for _, headless := range []bool{true, false} {
		paths, err := composer.ControllerPromptPaths(headless)
		if err != nil {
			t.Fatalf("ControllerPromptPaths(%t): %v", headless, err)
		}
		for _, path := range paths {
			body := readJoined(t, path)
			if strings.Contains(body, "xd://codegraph") {
				t.Errorf("the controller's prompt part %s names `xd://codegraph`: the controller works no checkout", path)
			}
			if strings.Contains(body, retiredCall) {
				t.Errorf("the controller's prompt part %s holds %q, a call no host can make", path, retiredCall)
			}
		}
	}
}

// bannedCheckLimits are limits of the last look that the production check's rule no longer has:
// each keeps the last look's pending, or its close as not run, for a merge that production does
// not carry. Each is joined from halves, as retiredCheckSentences are, so a search of the
// repository for one of them finds the texts that state it and never this test.
// internal/workflow and internal/textcheck keep their own copies, since a test package cannot
// import another package's test helpers.
var bannedCheckLimits = []string{
	"not carrying" + " the merge alone",
	"did not carry the merge" + " within the bound",
	"never carried" + " the merge",
}

// whetherOrNotCarried are the words that keep a fail clause's mention of production from limiting
// the fail.
const whetherOrNotCarried = "whether or not production carries the merge"

// isCheckFailClause reports whether clause, one of clauses, makes a requirement still unobserved
// at the last look a fail.
func isCheckFailClause(clause string) bool {
	return strings.Contains(clause, "unobserved") && strings.Contains(clause, "last") && strings.Contains(clause, "fail")
}

// checkRuleFaults are the ways clause breaks the production check's rule, one message each: it
// states a limit the rule no longer has (bannedCheckLimits), or it is a fail clause
// (isCheckFailClause) that still holds "production carries the merge" when every
// whetherOrNotCarried is taken out of it.
func checkRuleFaults(clause string) []string {
	var faults []string
	for _, phrase := range bannedCheckLimits {
		if strings.Contains(clause, phrase) {
			faults = append(faults, fmt.Sprintf("says %q, a limit the production check's rule no longer has: a requirement still unobserved at the last look is a fail %s, and that look's pending is only for an issue with no requirement that only the deploy can show: %q", phrase, whetherOrNotCarried, clause))
		}
	}
	if isCheckFailClause(clause) && strings.Contains(strings.ReplaceAll(clause, whetherOrNotCarried, ""), "production carries the merge") {
		faults = append(faults, fmt.Sprintf("limits the fail of a requirement still unobserved at the last look by whether production carries the merge, but that requirement is a fail %s: %q", whetherOrNotCarried, clause))
	}
	return faults
}

// A production check opens at the issue's close, which comes at the merge or, for a merge that
// waits on the issues beneath it, later (workflow.closeMerged), and its bound is counted from that
// close (workflow.checkBound). A requirement that only the deploy can show is a fail when a look
// observes it as not met. It is also a fail when it is still unobserved at the last look, whether
// or not production carries the merge, and that look's pending is only for an issue with no such
// requirement (workflow.checkTask). So the composed prompt of each role a look reaches, the
// tester's and the implementer's, holds that rule clause by clause. No clause states a limit the
// rule no longer has, and no fail clause, one that holds "unobserved", "last" and "fail", holds
// "production carries the merge" outside the words "whether or not production carries the merge"
// (checkRuleFaults). Each prompt holds one fail clause with those words at least, and one fail
// clause for a requirement observed as not met, so a matcher that stopped finding the rule fails
// here rather than passing on nothing. The unit is the clause, cut at `; ` as well as at full stops
// as workflow's check task test cuts its task, never the sentence: matched sentence by sentence, a
// test of this rule passed with the concepts page's limit taken out, since that page's one
// sentence of the three verdicts reads whether production carries the merge in its opening
// clause. A test that passes on the revert it exists to catch measures the wrong unit, so a check
// widened from here keeps the clause. TestTheProductionCheckRuleHoldsInEveryPromptPart holds
// every part of both trees to the same limits and names the part that breaks them. The pages and
// the proof scripts that state the rule outside a prompt are read by internal/textcheck's
// TestThePagesFailAnUnobservedRequirementWhetherOrNotProductionCarriesTheMerge, with the same
// clause rule, so an edit to one of them alone starts the text checks rather than this package's
// whole lane. Besides: a clause naming `production_check_hours` counts it from the close, never
// from the merge; a clause saying the session is handed the check task names the close before it;
// and no clause says the merge closes the issue, since in these two prompts the close is the
// moment the check task is handed and the role's work ends, and a merge that waits on the issues
// beneath it comes before that moment. The architect's, the merger's and the reviewer's "the merge
// closes" name only what causes the close, which is true, and no prompt this test reads holds
// them. This shows the text is loaded, never that a role follows it.
func TestTheProductionCheckFailsAnUnobservedRequirementWhetherOrNotProductionCarriesTheMerge(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	theClose := regexp.MustCompile(`\bclose\b`)
	boundFromTheMerge := regexp.MustCompile("production_check_hours`? (after|from|since) the merge")
	type text struct{ name, body string }
	var texts []text
	for _, role := range []claim.Role{claim.RoleTester, claim.RoleImplementer} {
		texts = append(texts, text{"the composed " + string(role) + " prompt", composedText(t, composer, role, false)})
	}
	for _, tc := range texts {
		t.Run(tc.name, func(t *testing.T) {
			statements, observedNotMet := 0, 0
			for _, clause := range clauses(tc.body) {
				for _, fault := range checkRuleFaults(clause) {
					t.Errorf("%s %s", tc.name, fault)
				}
				if isCheckFailClause(clause) && strings.Contains(clause, whetherOrNotCarried) {
					statements++
				}
				if isCheckFailClause(clause) && strings.Contains(clause, "observed as not met") {
					observedNotMet++
				}
				if strings.Contains(clause, "`production_check_hours`") && (!theClose.MatchString(clause) || boundFromTheMerge.MatchString(clause)) {
					t.Errorf("%s counts the check's bound from something other than the issue's close: %q", tc.name, clause)
				}
				if before, _, found := strings.Cut(clause, "hands this session"); found && !theClose.MatchString(before) {
					t.Errorf("%s hands this session the check task at something other than the issue's close: %q", tc.name, clause)
				}
				if strings.Contains(strings.ToLower(clause), "merge closes") {
					t.Errorf("%s says the merge closes the issue, which puts the check's hand-off and the end of this role's work at the merge, before the close for a merge that waits on the issues beneath it: %q", tc.name, clause)
				}
			}
			if statements == 0 {
				t.Errorf("%s holds no clause that makes a requirement still unobserved at the last look a fail %s", tc.name, whetherOrNotCarried)
			}
			if observedNotMet == 0 {
				t.Errorf("%s holds no fail clause for a requirement observed as not met", tc.name)
			}
		})
	}
}

// Every part of both trees holds the production check's rule clause by clause, not only the parts
// Compose joins for the tester and the implementer. A sentence left in a part that neither role
// composes still reaches whichever role does compose it, and a part Compose joins for nobody today
// is joined by the next role that needs it. So no clause of a part states a limit the rule no
// longer has, and no fail clause of a part limits the fail by whether production carries the merge
// (checkRuleFaults). Each failure names the part's file, so a revert of one sentence names the
// file it is in. A part need not state the rule, since most parts are about other work: the
// composed prompts hold the statement
// (TestTheProductionCheckFailsAnUnobservedRequirementWhetherOrNotProductionCarriesTheMerge).
func TestTheProductionCheckRuleHoldsInEveryPromptPart(t *testing.T) {
	for dir, parts := range map[string]embed.FS{"roles": roleParts, "go": goParts} {
		err := eachPart(parts, dir, func(name string, body []byte) error {
			for _, clause := range clauses(string(body)) {
				for _, fault := range checkRuleFaults(clause) {
					t.Errorf("%s/%s %s", dir, name, fault)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the %s parts: %v", dir, err)
		}
	}
}
