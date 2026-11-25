package prompts

import (
	"embed"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// fileBackedRoles are the roles whose phase ends with a handoff record. Each composes
// go/worker-common.md and the headless fragment, so the composed prompt of each carries the rules
// that those two parts state.
var fileBackedRoles = []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer}

// A file-backed phase's handoff body is `head` and, where the role has one, `note`, and nothing
// else (dispatch://INFRA-4942 R1). The record already holds the issue, the phase and the version,
// so the body repeats none of them, and no role asks for a field that copies another record (R4).
// The planner's, the implementer's and the reviewer's handoff paragraphs each name the two keys
// and close the list with "and nothing else". The tester's paragraph also keeps
// `implementerProof`, `failures` and `failed`, which dispatch://INFRA-4930 decides. The shared
// body rule in go/worker-common.md names the two keys and says that the body repeats no stamp.
// No paragraph asks for a stamp or for a field that its role wrote before. In a role part, the
// paragraph read is the one that holds the `head` needle of
// TestEveryFileBackedRoleHandoffNamesTheHeadItWasDoneAgainst.
// Putting `filesChanged` back in the implementer's paragraph, `decision` back in the reviewer's,
// or `schemaVersion` back in the body rule fails here (d-pins' negative control). This shows the
// text is loaded, never that a role follows it.
func TestEachRolesHandoffBodyIsItsHeadAndANote(t *testing.T) {
	const head = "`head` (the 40-character lower-case commit"
	stamps := []string{"`schemaVersion", "`phase: \"", "`issue: \""}
	headAndNote := []string{"`note`", "and nothing else"}
	for _, tc := range []struct {
		part, marker string
		want, gone   []string
	}{
		{"roles/planner.md", head, headAndNote, append([]string{"requiredSkills", "gapAnalysis", "planReview", "specDepartures"}, stamps...)},
		{"roles/implementer.md", head, headAndNote, append([]string{"filesChanged", "trickyParts", "deviations", "openQuestions", "discoveredComplexity"}, stamps...)},
		{"roles/reviewer.md", head, headAndNote, append([]string{"`decision`", "`critical`", "`important`", "`minor`", "keyFindings"}, stamps...)},
		{"roles/tester.md", head, []string{"`note`", "`implementerProof`", "`failures`", "`failed`", "and nothing else"}, stamps},
		{"go/worker-common.md", "A file-backed phase", []string{"`head`", "`note`", "repeats none of them"}, []string{"`schemaVersion"}},
	} {
		t.Run(tc.part, func(t *testing.T) {
			ruleIn(t, tc.part, tc.marker, tc.want...)
			for _, paragraph := range ruleParagraphs(partText(t, tc.part), tc.marker) {
				holdsAndRetires(t, tc.part+"'s handoff paragraph", paragraph, nil, tc.gone)
			}
		})
	}
}

// No prompt part asks for a field that a handoff no longer holds (dispatch://INFRA-4942 R4): the
// implement handoff's filesChanged, trickyParts, openQuestions and discoveredComplexity, the
// review handoff's keyFindings, the plan handoff's requiredSkills, gapAnalysis, planReview and
// specDepartures, and the three stamps of every body. Because no part names requiredSkills, no
// prompt reads a plan handoff's skill list either (R5). The walk reads every part of both trees,
// a part that Compose joins for no role too, so it is the inventory of every prompt. Each hit
// names its part, its line and the field. `deviations`, `decision` and the three review counts
// are read in the handoff paragraphs alone (TestEachRolesHandoffBodyIsItsHeadAndANote), because
// another rule can use each of those words in its own sense. This shows the text is loaded, never
// that a role follows it.
func TestNoPartAsksForAFieldAHandoffNoLongerHolds(t *testing.T) {
	fields := []string{"filesChanged", "trickyParts", "openQuestions", "discoveredComplexity", "keyFindings",
		"requiredSkills", "gapAnalysis", "planReview", "specDepartures", "schemaVersion", "`phase: \"", "`issue: \""}
	for dir, parts := range map[string]embed.FS{"roles": roleParts, "go": goParts} {
		err := eachPart(parts, dir, func(name string, body []byte) error {
			for n, line := range strings.Split(string(body), "\n") {
				for _, field := range fields {
					if strings.Contains(line, field) {
						t.Errorf("%s/%s:%d: %s", dir, name, n+1, field)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the %s parts: %v", dir, err)
		}
	}
}

// Each field that a handoff no longer holds goes to the record that owns it (dispatch://INFRA-4942
// R4). For the implementer, what it changed is the diff, why is the pull request body, each
// check's outcome and evidence are that check's result, and a question about a task of the plan is
// a comment on that task's block (R6). For the reviewer, its decision and each finding are the
// review it submits on GitHub. For the planner, the gap analysis, the plan reviews and the
// departures from the spec are two sections of `plan.md`, and the departure rule names the section
// that holds each departure. For the tester, environment evidence is the result of the check that
// calls for it. The implementer's core no longer asks for the files changed or the unanswered
// questions in its report, and the tester's core names a missing Verification document in its
// report. A clause is held by its terms in their order, so a rewording that keeps them passes. This
// shows the text is loaded, never that a role follows it.
func TestEachRoleSendsWhatItsHandoffHeldToTheRecordThatOwnsIt(t *testing.T) {
	for _, tc := range []struct {
		part    string
		clauses [][]string
	}{
		{"roles/implementer.md", [][]string{
			{"What you changed is the diff"},
			{"pull request body"},
			{"each check's outcome and its evidence are that check's result"},
			{"a question about a task of the plan", "comment on that task's block"},
		}},
		{"roles/reviewer.md", [][]string{{"your decision and each finding are the review you submit on GitHub"}}},
		{"roles/tester.md", [][]string{{"Environment or secret-scrub evidence", "in the result of the check that calls for it", "never in your `test` handoff"}}},
	} {
		text := partText(t, tc.part)
		for _, terms := range tc.clauses {
			if !clauseHolds(text, terms...) {
				t.Errorf("no clause of %s holds %q, in that order", tc.part, terms)
			}
		}
	}
	ruleIn(t, "roles/planner.md", "The plan's own records are in `plan.md`",
		"`## Plan checks`", "gap analysis", "plan reviews", "`## Departures from the spec`")
	ruleIn(t, "roles/planner.md", "makes the plan build something differently from the spec's design", "`## Departures from the spec`")
	holdsAndRetires(t, "roles/core/implementer.md", partText(t, "roles/core/implementer.md"),
		[]string{"Report every check you recorded."}, []string{"files changed", "unanswered questions"})
	holdsAndRetires(t, "roles/core/tester.md", partText(t, "roles/core/tester.md"),
		[]string{"say which of the two it is in your report"}, nil)
}

// A handoff's `note` is one paragraph of at most 1,000 characters for the next role
// (dispatch://INFRA-4942 R2). It holds only facts of two kinds: how to run the issue's checks in
// the worker pod, or an order of work that the next role must keep. It holds no reason for the
// work, no deviation and no concern that the role examined and dismissed, because a later role
// can excuse a defect by one. A role with none of these facts writes no `note`. The rule is
// written once, in go/worker-common.md, which every phase worker composes, and no other part of
// either tree states it. The composed prompt of each file-backed role carries it. Deleting the
// paragraph fails here. This shows the text is loaded, never that a role follows it.
func TestTheNoteIsOneShortParagraphOfTwoKinds(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const (
		home   = "go/worker-common.md"
		marker = "`note` is one paragraph"
	)
	ruleIn(t, home, marker, "at most 1,000 characters", "for the next role", "how to run this issue's checks in the worker pod",
		"an order of work it must keep", "no reason for the work", "no deviation", "no concern you examined and dismissed", "write no `note`")
	for dir, parts := range map[string]embed.FS{"roles": roleParts, "go": goParts} {
		err := eachPart(parts, dir, func(name string, body []byte) error {
			if part := dir + "/" + name; part != home && strings.Contains(foldWhitespace(string(body)), marker) {
				t.Errorf("%s says %q, which %s alone states", part, marker, home)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the %s parts: %v", dir, err)
		}
	}
	for _, role := range fileBackedRoles {
		composedSays(t, composer, role, false, "`note` is one paragraph of at most 1,000 characters")
	}
}

// A role writes its handoff once, when it completes its phase, and writes none before it
// (dispatch://INFRA-4942 R3). Work between assignments writes no handoff. A role writes the
// handoff again only where `handoff_complete` refuses the one it wrote and names why, and the new
// one has the shape that its role part gives. So the answer to `HANDOFF_SESSION_MISMATCH` no
// longer tells a role to read a record that another session wrote and write it again. The rule is
// written once, in go/worker-common.md, and the composed prompt of each file-backed role carries
// it. Deleting the paragraph fails here. This shows the text is loaded, never that a role follows
// it.
func TestARoleWritesItsHandoffOnceWhenItCompletes(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const marker = "Write your handoff once"
	ruleIn(t, "go/worker-common.md", marker, "when you complete your phase", "none before it", "between assignments writes no handoff",
		"again only where `handoff_complete` refuses the one you wrote and names why", "in the shape your role part gives")
	holdsAndRetires(t, "go/worker-common.md", partText(t, "go/worker-common.md"), nil, []string{"read such a record, write it again"})
	for _, role := range fileBackedRoles {
		composedSays(t, composer, role, false, marker)
	}
}

// The planner names, for each task of its plan, the skills and the repository docs that the
// task's work needs (dispatch://INFRA-4942 R5). The core states the duty and says that an empty
// list means that the task needs nothing beyond the skills its role loads, and it no longer names
// requiredSkills. The planner's role part reads the release's block schema first. Where
// `plan_task` declares `skills` and `docs` attributes, the planner writes them there. Where it
// does not, the planner writes a `Skills:` line and a `Docs:` line in the task's text, because
// Dispatch refuses an attribute that the schema does not declare. Under an owner's task block,
// the planner writes the two lines in each plain section. Each such section names the id of the
// owner's `plan_task` block it belongs to. The command needles have no backtick at either end,
// because each is a part of a longer code span. Taking the naming sentence out of the core, or
// the schema read out of the role part, fails here (d-pins' negative control). This shows the
// text is loaded, never that a planner follows it.
func TestThePlannerNamesEachTasksSkillsAndDocs(t *testing.T) {
	core := partText(t, "roles/core/planner.md")
	for _, terms := range [][]string{
		{"For each task of the plan", "skills", "repository docs"},
		{"empty list", "nothing beyond the skills its role loads"},
	} {
		if !clauseHolds(core, terms...) {
			t.Errorf("no clause of roles/core/planner.md holds %q, in that order", terms)
		}
	}
	holdsAndRetires(t, "roles/core/planner.md", core, nil, []string{"requiredSkills"})
	ruleIn(t, "roles/planner.md", "/api/v1/schema/blocks",
		`curl -fsS "$DISPATCH_URL/api/v1/schema/blocks"`,
		`select(.name == "plan_task")`,
		"`skills` and `docs` attributes",
		"a `Skills:` line and a `Docs:` line",
		"refuses an attribute the schema does not declare",
		"plain sections",
		"the id of the owner's `plan_task` block it belongs to")
}

// Each role after the planner reads the skills and the repository docs that its tasks name
// (dispatch://INFRA-4942 R5). The implementer reads those of each task before it starts that
// task, and gives them to the task's deep-worker in step 2. The tester reads those of each task
// whose `serves` names a requirement that a check it runs serves. The reviewer reads those of
// every task. Each reads the `skills` and `docs` attributes of the task's block where the block has
// them, else the `Skills:` and `Docs:` lines, and a task that names none gives the role nothing
// beyond its own skills. The planner and the merger read no task's lines. Taking "the attributes"
// out of the implementer's reading clause fails here (d-pins' negative control). This shows the
// text is loaded, never that a role follows it.
func TestEachReaderReadsTheSkillsAndDocsOfItsTasks(t *testing.T) {
	const lines = "else the `Skills:` and `Docs:` lines of its text"
	for _, tc := range []struct {
		part  string
		terms []string
	}{
		{"roles/implementer.md", []string{"Before you start each task of the plan", "skills", "repository docs", "`skills` and `docs` attributes", "`Skills:` and `Docs:` lines"}},
		{"roles/tester.md", []string{"`serves`", "`skills` and `docs` attributes", "`Skills:` and `Docs:` lines"}},
		{"roles/reviewer.md", []string{"every task of the plan", "`skills` and `docs` attributes", "`Skills:` and `Docs:` lines"}},
	} {
		if !clauseHolds(partText(t, tc.part), tc.terms...) {
			t.Errorf("no clause of %s holds %q, in that order", tc.part, tc.terms)
		}
		ruleIn(t, tc.part, lines, "beyond your role's own")
	}
	const step = "2. Hand each coding task"
	var steps []string
	for _, block := range blocks(partText(t, "roles/implementer.md")) {
		if strings.HasPrefix(block, step) {
			steps = append(steps, block)
		}
	}
	if len(steps) != 1 {
		t.Errorf("roles/implementer.md holds %d items that start %q, want one", len(steps), step)
	} else if want := "the skills and the repository docs that task names"; !strings.Contains(steps[0], want) {
		t.Errorf("roles/implementer.md's step 2 omits %q: %s", want, steps[0])
	}
	for _, part := range []string{"roles/planner.md", "roles/merger.md"} {
		holdsAndRetires(t, part, partText(t, part), nil, []string{lines})
	}
}

// A role's question about a task of the plan, or its departure from one, is a comment on that
// task's block in `plan.md`, addressed to the architect (dispatch://INFRA-4942 R6). Where no
// task's block fits, the comment goes on the issue itself. The daemon wakes no role for a comment,
// so the role sends the architect a message that holds only the comment's link, and the
// architect's message back holds only the link of its reply in that thread. A role acts on no
// decision that has no Dispatch record: it asks the architect to write a decision that came by
// message alone in the task's thread. No role writes a status update as a comment. The rule is
// written once, in the headless fragment that every phase worker composes, and the composed
// prompt of each file-backed role carries it. The command needle has no backtick at either end,
// because it is a part of a longer code span. Deleting the paragraph fails here. This shows the
// text is loaded, never that a role follows it.
func TestAQuestionAboutAPlanTaskIsACommentOnItsBlock(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ruleIn(t, "roles/mechanics/headless.md", "is a comment on that task's block",
		"`plan.md`",
		"addressed to the architect",
		"dispatch comment --issue <issue> --artifact plan.md --quote",
		"comment on the issue itself",
		"wakes no role",
		"`envoy_publish`",
		"holds only the comment's link",
		"reply in that thread",
		"holds only the reply's link",
		"no decision that has no Dispatch record",
		"ask the architect to write in that task's thread",
		"No role writes a status update as a comment")
	for _, role := range fileBackedRoles {
		composedSays(t, composer, role, false, "is a comment on that task's block in `plan.md`")
	}
}

// The architect answers a worker's question or departure about a task of the plan with a reply in
// that task's thread, then sends the worker a message that holds only the reply's link
// (dispatch://INFRA-4942 R6). A decision that the architect sends a role about a task is such a
// reply too, because a role acts on no decision that has no Dispatch record. The rule is written
// once in each architect part that a composed architect prompt joins: the sub-architect's part,
// the root's part and the part that both share. The composed prompt of each kind carries it.
// Taking the rule out of go/architect-common.md fails here, which the join alone does not show
// (d-pins' negative control). The command needle has no backtick at either end, because it is a
// part of a longer code span. This shows the text is loaded, never that an architect follows it.
func TestTheArchitectAnswersAPlanTaskInItsThread(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const decision = "A decision you send a role about a task is such a reply too"
	for _, part := range []string{"roles/architect.md", "roles/architect-root.md", "go/architect-common.md"} {
		ruleIn(t, part, "reply in that thread",
			"holds only the link of its comment",
			"dispatch comment --issue <issue> --reply-to <comment id>",
			"`envoy_publish`",
			"holds only your reply's link",
			decision,
			"no decision that has no Dispatch record")
	}
	for _, isRoot := range []bool{true, false} {
		composedSays(t, composer, claim.RoleArchitect, isRoot, decision)
	}
}

// A completion refused HANDOFF_HEAD_MERGED_FORWARD moves nothing, but it is no unspent fact, so
// intake.ApplyFact keeps its key (only a refused check report or an unspent fact gives its key
// back), and the same call again is answered HANDOFF_ALREADY_RECORDED. The part every phase
// worker shares says so, and no longer says that nothing was recorded. This shows the text is
// loaded, never that a role follows it.
func TestAMergedForwardRefusalIsKeptSoTheSameCallIsAnsweredAsReceived(t *testing.T) {
	holdsAndRetires(t, "go/worker-common.md", partText(t, "go/worker-common.md"),
		[]string{"is refused with `HANDOFF_HEAD_MERGED_FORWARD`; it moves nothing, and the daemon keeps the refusal, so the same call again is answered `HANDOFF_ALREADY_RECORDED`."},
		[]string{"`HANDOFF_HEAD_MERGED_FORWARD`, and nothing was recorded"})
}
