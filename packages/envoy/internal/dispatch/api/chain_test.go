package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// readChain reads GET /api/v1/issues/{key}/chain with query as login, and fails on anything but
// 200. It answers the decoded chain and the body as the server wrote it, for a search of what the
// answer holds anywhere.
func readChain(t *testing.T, handler http.Handler, key, query, login string) (model.IssueChain, string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/chain"+query, nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("%s reads the chain of %s%s: status=%d body=%s", login, key, query, response.Code, response.Body.String())
	}
	t.Logf("%s reads the chain of %s%s: %s", login, key, query, response.Body.String())
	return decodeBody[model.IssueChain](t, response), response.Body.String()
}

// chainRequirementOf is the requirement of chain whose block id is block.
func chainRequirementOf(t *testing.T, chain model.IssueChain, block string) model.IssueChainRequirement {
	t.Helper()
	for _, requirement := range chain.Requirements {
		if requirement.BlockID == block {
			return requirement
		}
	}
	t.Fatalf("the chain of %s lists no requirement %s: %+v", chain.Issue, block, chain.Requirements)
	return model.IssueChainRequirement{}
}

// chainChecksOf is every check of chain whose block id is block.
func chainChecksOf(chain model.IssueChain, block string) []model.IssueChainCheck {
	var checks []model.IssueChainCheck
	for _, check := range chain.Checks {
		if check.BlockID == block {
			checks = append(checks, check)
		}
	}
	return checks
}

// chainCheckOf is the one check of chain whose block id is block.
func chainCheckOf(t *testing.T, chain model.IssueChain, block string) model.IssueChainCheck {
	t.Helper()
	checks := chainChecksOf(chain, block)
	if len(checks) != 1 {
		t.Fatalf("the chain of %s lists %d checks %s, want one: %+v", chain.Issue, len(checks), block, chain.Checks)
	}
	return checks[0]
}

// chainResultsOf is every result of chain whose id is id.
func chainResultsOf(chain model.IssueChain, id string) []model.IssueChainResult {
	var results []model.IssueChainResult
	for _, result := range chain.Results {
		if result.Result.ID == id {
			results = append(results, result)
		}
	}
	return results
}

// recordChainResult records one result of check on key, as alice, at headCommit, and answers it.
func recordChainResult(t *testing.T, handler http.Handler, key, check, verdict string, fields map[string]any) model.CheckResult {
	t.Helper()
	recorded := recordResult(t, handler, key, resultBody(check, verdict, headCommit, fields))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a %s of %s on %s: status=%d body=%s", verdict, check, key, recorded.Code, recorded.Body.String())
	}
	return decodeBody[recordedResult](t, recorded).Result
}

// sameIDs reports whether got and want hold the same ids, whatever their order.
func sameIDs(got, want []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want)))
}

// TestTheChainWalksFromADecisionToItsResultAndBack: one read of an issue answers the chain from the
// decisions to the requirement they shaped - an agent's decision block and a person's answered ask -
// to the check that serves it, to that check's one result, with the words at every hop, and the
// same walk read backwards from the result to its check, its requirement and its decisions. Read
// at the commit the result ran at, the pass stands and proves the check; read naming no commit,
// the check is compared with what the results read compares it with, which for an issue with no
// pull request is nothing, and the chain says the check stands unproven because of it.
func TestTheChainWalksFromADecisionToItsResultAndBack(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	const (
		requirementWords = "An urgent ask comes first."
		decisionWords    = "Urgency is the ask's own field."
		question         = "Which list comes first?"
		answer           = "The urgent list, every time."
		observed         = "bun test: 12 pass, 0 fail."
	)
	issue := createSpecIssue(t, handler, "CORE", ":::requirement{#r1}\n"+requirementWords+"\n:::\n\n"+
		":::decision{#d1 shaped=\"dispatch://CORE-1/spec#b-r1\"}\n"+decisionWords+"\n:::\n")
	if issue.Key != "CORE-1" {
		t.Fatalf("the seeded issue is %s, want CORE-1, which its spec's references name", issue.Key)
	}
	// The person's decision: an ask shaping r1, answered by bob.
	uploadAs(t, handler, issue.Key, "asks.md", ":::ask{#a1 urgency=\"med\" multiple=\"false\" shaped=\"dispatch://CORE-1/spec#b-r1\"}\n"+
		question+"\n\n- Urgent: The urgent list\n- Recent: The newest first\n:::\n", "alice")
	settleIssue(t, deps.Docs.(*docs.Service), issue.Key)
	askID := blockAskID(t, handler, issue.Key, "a1")
	answerDecisionBlock(t, handler, askID, "bob", []string{"Urgent"}, answer, nil)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", checkServing("c1", "dispatch://CORE-1/spec#b-r1"))
	result := recordChainResult(t, handler, issue.Key, "c1", "pass", map[string]any{"observed": observed})
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	checks := readDecisionDocument(t, database, issue.Key, "verification-md")
	decisions := []string{spec.address("d1"), askID}

	chain, _ := readChain(t, handler, issue.Key, "?at="+headCommit, "alice")
	if chain.At == nil || *chain.At != headCommit {
		t.Errorf("the chain read at %s names the commit %v, want it", headCommit, chain.At)
	}

	// Forward: the decisions, the requirement they shaped, the check that serves it, its result.
	requirement := chainRequirementOf(t, chain, "r1")
	if requirement.ID != spec.address("r1") || requirement.Text != requirementWords {
		t.Errorf("r1 reads %q with the words %q, want %s with %q", requirement.ID, requirement.Text, spec.address("r1"), requirementWords)
	}
	want := []model.IssueChainDecision{
		{Kind: refs.BlockNodeKind, ID: spec.address("d1"), BlockID: "d1", Text: decisionWords, Shaped: []string{requirement.ID}},
		{Kind: graphstore.KindAsk, ID: askID, Text: question, Answer: answer, Shaped: []string{requirement.ID}},
	}
	if len(chain.Decisions) != len(want) {
		t.Fatalf("the chain's decisions are %+v, want d1 and the answered ask %s", chain.Decisions, askID)
	}
	for _, decision := range want {
		if !slices.ContainsFunc(chain.Decisions, func(read model.IssueChainDecision) bool {
			return read.Kind == decision.Kind && read.ID == decision.ID && read.BlockID == decision.BlockID &&
				read.Text == decision.Text && read.Answer == decision.Answer && slices.Equal(read.Shaped, decision.Shaped)
		}) {
			t.Errorf("the chain's decisions are %+v, want %+v among them", chain.Decisions, decision)
		}
	}
	if !sameIDs(requirement.Decisions, decisions) || !slices.Equal(requirement.Checks, []string{checks.address("c1")}) || !requirement.Proved {
		t.Errorf("r1 names the decisions %v and the checks %v, proved %t; want d1 and the ask, c1 and proved", requirement.Decisions, requirement.Checks, requirement.Proved)
	}
	check := chainCheckOf(t, chain, "c1")
	if check.ID != checks.address("c1") || check.Text != "The check that proves it." || !slices.Equal(check.Serves, []string{requirement.ID}) ||
		!slices.Equal(check.Results, []string{result.ID}) || !check.Proved || len(check.Unproven) != 0 || check.PassStands.Answer != model.PassStandsYes {
		t.Errorf("c1 reads %+v, want %s with its words, serving r1, holding its one result, proved", check, checks.address("c1"))
	}
	if len(check.ComparedWith) != 1 || check.ComparedWith[0].Kind != model.CheckBasisNamed || check.ComparedWith[0].Commit != headCommit {
		t.Errorf("c1 is compared with %+v, want the one commit the read named", check.ComparedWith)
	}

	// Backward: from the result to its check, its requirement and its decision, by their ids.
	if len(chain.Results) != 1 {
		t.Fatalf("the chain holds the results %+v, want the one recorded", chain.Results)
	}
	back := chain.Results[0]
	if back.Result.ID != result.ID || back.Result.Observed != observed || back.Result.Verdict != "pass" || !back.Proves {
		t.Errorf("the result reads %+v, want %s, a pass that proves its check, with its reading %q", back, result.ID, observed)
	}
	if back.Check != check.ID || !slices.Equal(back.Requirements, []string{requirement.ID}) || !sameIDs(back.Decisions, decisions) {
		t.Errorf("the walk back from the result reaches the check %s, the requirements %v and the decisions %v; want c1, r1 and d1",
			back.Check, back.Requirements, back.Decisions)
	}
	if stands := back.Result.CommitStands; stands == nil || stands.Answer != model.CommitAtBasis || back.Result.CheckWordsMoved == nil || *back.Result.CheckWordsMoved {
		t.Errorf("the result stands at %+v with the words moved %v, want at the named commit with its words unmoved", back.Result.CommitStands, back.Result.CheckWordsMoved)
	}

	// Naming no commit, the check is compared with what the results read compares it with: an
	// issue with no pull request compares with nothing, so the pass is evidence and no proof.
	unnamed, _ := readChain(t, handler, issue.Key, "", "alice")
	check = chainCheckOf(t, unnamed, "c1")
	if unnamed.At != nil || check.Proved || check.ComparedNothing != model.ReasonNoPullRequestOrBranch ||
		!slices.Contains(check.Unproven, model.IssueChainUnproven{Reason: unprovenComparedNothing}) {
		t.Errorf("read naming no commit, the chain is at %v and c1 reads %+v; want no commit named, c1 compared with nothing and unproven for it", unnamed.At, check)
	}
	// The results read answers its compatibility currency on a read naming no commit; the chain
	// answers the standing alone.
	if chainRequirementOf(t, unnamed, "r1").Proved || unnamed.Results[0].Proves || unnamed.Results[0].Result.Currency != "" {
		t.Errorf("read naming no commit, r1 or its result reads as proved, or the result carries a currency: %+v", unnamed)
	}
	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/chain?at=HEAD", nil, "alice"); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "INVALID_QUERY") {
		t.Errorf("the chain read at HEAD answers %d %s, want 400 INVALID_QUERY", response.Code, response.Body.String())
	}
}

// TestTheChainListsACheckServingThreeRequirementsOnceUnderEach: placement and coverage are two
// relations. One check serves three requirements that stand under two criteria. The chain lists the
// check once, as one node with one id, and each of the three requirements names that id; its one
// result is listed once; and its own parent is one node, the requirement its first write placed it
// under, whatever else it serves. Each requirement's parent is its own criterion.
func TestTheChainListsACheckServingThreeRequirementsOnceUnderEach(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	if issue := createSpecIssue(t, handler, "CORE", placementSpec); issue.Key != "CORE-1" {
		t.Fatalf("the seeded issue is %s, want CORE-1, which its spec's references name", issue.Key)
	}
	ref := func(id string) string { return "dispatch://CORE-1/spec#b-" + id }
	uploadDocument(t, handler, "/api/v1/issues/CORE-1", "verification.md",
		":::check{#c1 serves=\""+ref("r1")+" "+ref("r2")+" "+ref("r3")+"\" surface=\"cli\" run=\"go test ./...\"}\nThe check of three.\n:::\n")
	result := recordChainResult(t, handler, "CORE-1", "c1", "pass", nil)
	spec := readDecisionDocument(t, database, "CORE-1", "spec")
	checks := readDecisionDocument(t, database, "CORE-1", "verification-md")

	chain, body := readChain(t, handler, "CORE-1", "?at="+headCommit, "alice")
	check := chainCheckOf(t, chain, "c1")
	if check.ID != checks.address("c1") {
		t.Errorf("the check of three is %s, want %s", check.ID, checks.address("c1"))
	}
	served := []string{spec.address("r1"), spec.address("r2"), spec.address("r3")}
	if !sameIDs(check.Serves, served) {
		t.Errorf("c1 serves %v, want the three requirements %v", check.Serves, served)
	}
	for _, block := range []string{"r1", "r2", "r3"} {
		if requirement := chainRequirementOf(t, chain, block); !slices.Equal(requirement.Checks, []string{check.ID}) || !requirement.Proved {
			t.Errorf("%s names the checks %v, proved %t; want the one check %s, proved", block, requirement.Checks, requirement.Proved, check.ID)
		}
	}
	if requirement := chainRequirementOf(t, chain, "r4"); len(requirement.Checks) != 0 {
		t.Errorf("r4, which c1 does not serve, names the checks %v", requirement.Checks)
	}
	if held := chainResultsOf(chain, result.ID); len(held) != 1 || !slices.Equal(check.Results, []string{result.ID}) || len(chain.Results) != 1 {
		t.Errorf("the chain lists the result %s %d times and c1 names %v, want it once", result.ID, len(held), check.Results)
	} else if !sameIDs(held[0].Requirements, served) {
		t.Errorf("the walk back from the result reaches %v, want the three requirements", held[0].Requirements)
	}
	if count := strings.Count(body, `"id":"`+result.ID+`"`); count != 1 {
		t.Errorf("the chain's body carries the result %s %d times, want once", result.ID, count)
	}
	if check.Parent == nil || check.Parent.Kind != refs.BlockNodeKind || check.Parent.ID != spec.address("r1") {
		t.Errorf("c1 stands under %+v, want one parent, r1 (%s), the first requirement it was written to serve", check.Parent, spec.address("r1"))
	}
	for block, criterion := range map[string]string{"r1": "k1", "r2": "k1", "r3": "k2"} {
		if parent := chainRequirementOf(t, chain, block).Parent; parent == nil || parent.ID != spec.address(criterion) {
			t.Errorf("%s stands under %+v, want its criterion %s", block, parent, criterion)
		}
	}
	if blockNodesNamed(t, database, "c1") != 1 {
		t.Errorf("the store holds %d nodes of c1, want one", blockNodesNamed(t, database, "c1"))
	}
}

// TestTheChainNamesEachGap: the chain names each gap of its seed and no other. A requirement no
// decision shaped, a requirement no check serves, a check holding no result, and a check and a
// plan task whose serves names nothing, each hang where the tree placed them: the check and the
// plan task that serve nothing hang from the issue. The gaps are read from the tuples and from no
// second copy: a serves tuple appended to the store alone, which no document holds and no index
// is rebuilt after, takes a check's gap and a requirement's away, and one retracted the same way
// leaves the requirement it served a gap and its check serving nothing.
func TestTheChainNamesEachGap(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	ref := func(id string) string { return "dispatch://CORE-1/spec#b-" + id }
	if issue := createSpecIssue(t, handler, "CORE", ":::requirement{#r1}\nShaped and proved.\n:::\n\n"+
		":::requirement{#r2}\nNo decision shaped this.\n:::\n\n:::requirement{#r3}\nNo check serves this.\n:::\n\n"+
		":::decision{#d1 shaped=\""+ref("r1")+" "+ref("r3")+"\"}\nThe one decision.\n:::\n"); issue.Key != "CORE-1" {
		t.Fatalf("the seeded issue is %s, want CORE-1, which its spec's references name", issue.Key)
	}
	uploadDocument(t, handler, "/api/v1/issues/CORE-1", "verification.md", checkServing("c1", ref("r1"))+"\n"+
		checkServing("c2", ref("r2"))+"\n:::check{#c3 surface=\"cli\" run=\"go test ./...\"}\nA check of nothing.\n:::\n")
	uploadDocument(t, handler, "/api/v1/issues/CORE-1", "plan.md", planTaskServing("t1", ref("r1")+" "+ref("r2")+" "+ref("r3"))+"\n"+
		":::plan_task{#t2}\nWork that serves nothing.\n:::\n")
	recordChainResult(t, handler, "CORE-1", "c1", "pass", nil)
	spec := readDecisionDocument(t, database, "CORE-1", "spec")
	checks := readDecisionDocument(t, database, "CORE-1", "verification-md")

	chain, _ := readChain(t, handler, "CORE-1", "?at="+headCommit, "alice")
	var got []string
	for _, gap := range chain.Gaps {
		got = append(got, gap.Kind+" "+gap.Node+": "+gap.Line)
	}
	want := []string{
		chainGapNoDecision + " " + spec.address("r2") + ": r2 " + noDecisionReason,
		chainGapNoCheck + " " + spec.address("r3") + ": r3 " + noCheckReason,
		chainGapNoResult + " " + checks.address("c2") + ": c2 " + noResultReason,
		chainGapNoResult + " " + checks.address("c3") + ": c3 " + noResultReason,
		chainGapServesNothing + " " + checks.address("c3") + ": c3 " + servesNothingReason,
		chainGapServesNothing + " " + spec.address("t2") + ": t2 " + servesNothingReason,
	}
	if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Errorf("the chain names the gaps\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, block := range []string{"c2", "c3"} {
		if check := chainCheckOf(t, chain, block); check.Proved || !slices.Contains(check.Unproven, model.IssueChainUnproven{Reason: unprovenNoResult}) {
			t.Errorf("%s, which holds no result, reads %+v; want it unproven for holding none", block, check)
		}
	}
	issueNode := func(parent *model.IssueTreeParent) bool {
		return parent != nil && parent.Kind == graphstore.KindIssue && parent.ID == "CORE-1"
	}
	if check := chainCheckOf(t, chain, "c3"); !issueNode(check.Parent) || len(check.Serves) != 0 {
		t.Errorf("c3, which serves nothing, reads %+v; want it under the issue, serving nothing", check)
	}
	var task *model.IssueChainPlanTask
	for index := range chain.PlanTasks {
		if chain.PlanTasks[index].BlockID == "t2" {
			task = &chain.PlanTasks[index]
		}
	}
	if task == nil || !issueNode(task.Parent) || len(task.Serves) != 0 {
		t.Errorf("the plan task t2, which serves nothing, reads %+v in %+v; want it under the issue, serving nothing", task, chain.PlanTasks)
	}
	if requirement := chainRequirementOf(t, chain, "r1"); !requirement.Proved || len(requirement.PlanTasks) != 1 {
		t.Errorf("r1 reads %+v, want it proved and served by the plan task t1", requirement)
	}

	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{
		{FromKind: refs.BlockNodeKind, FromID: checks.address("c3"), Kind: servesTupleKind, ToKind: refs.BlockNodeKind,
			ToID: spec.address("r3"), At: time.Now().UTC(), Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true},
		{FromKind: refs.BlockNodeKind, FromID: checks.address("c2"), Kind: servesTupleKind, ToKind: refs.BlockNodeKind,
			ToID: spec.address("r2"), At: time.Now().UTC(), Who: alice, SourceEvent: graphstore.NoEvent, Asserted: false},
	}); err != nil {
		t.Fatalf("write the serves of c3 and retract the serves of c2: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the serves tuples: %v", err)
	}
	moved, _ := readChain(t, handler, "CORE-1", "?at="+headCommit, "alice")
	got = nil
	for _, gap := range moved.Gaps {
		got = append(got, gap.Kind+" "+gap.Node+": "+gap.Line)
	}
	want = []string{
		chainGapNoDecision + " " + spec.address("r2") + ": r2 " + noDecisionReason,
		chainGapNoCheck + " " + spec.address("r2") + ": r2 " + noCheckReason,
		chainGapNoResult + " " + checks.address("c2") + ": c2 " + noResultReason,
		chainGapServesNothing + " " + checks.address("c2") + ": c2 " + servesNothingReason,
		chainGapNoResult + " " + checks.address("c3") + ": c3 " + noResultReason,
		chainGapServesNothing + " " + spec.address("t2") + ": t2 " + servesNothingReason,
	}
	if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Errorf("after the serves tuples moved the chain names the gaps\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if requirement := chainRequirementOf(t, moved, "r3"); !slices.Equal(requirement.Checks, []string{checks.address("c3")}) {
		t.Errorf("after c3's serves tuple named r3, r3 names the checks %v, want c3 alone", requirement.Checks)
	}
	if check := chainCheckOf(t, moved, "c3"); !issueNode(check.Parent) {
		t.Errorf("c3 moved to %+v when its serves did, want it still under the issue", check.Parent)
	}
}

// TestTheChainReadsAFailedAMovedAndARetractedResultAsUnproven: evidence is not proof. Of four
// checks each holding one result, the one whose result failed, the one whose result ran against
// words the check no longer holds, and the one whose pass was retracted each stand unproven, and
// the chain says which of the three kept each; the fourth, whose pass stands, proves its
// requirement. Each answer is the standing the results read gives.
func TestTheChainReadsAFailedAMovedAndARetractedResultAsUnproven(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	ref := func(id string) string { return "dispatch://CORE-1/spec#b-" + id }
	if issue := createSpecIssue(t, handler, "CORE", ":::requirement{#r1}\nA check that failed.\n:::\n\n"+
		":::requirement{#r2}\nA check reworded after its pass.\n:::\n\n:::requirement{#r3}\nA check whose pass was retracted.\n:::\n\n"+
		":::requirement{#r4}\nA check that passed.\n:::\n"); issue.Key != "CORE-1" {
		t.Fatalf("the seeded issue is %s, want CORE-1, which its spec's references name", issue.Key)
	}
	verification := func(words string) string {
		return checkServing("cf", ref("r1")) + "\n:::check{#cm serves=\"" + ref("r2") + "\" surface=\"cli\" run=\"go test ./...\"}\n" + words + "\n:::\n\n" +
			checkServing("cr", ref("r3")) + "\n" + checkServing("cp", ref("r4"))
	}
	uploadDocument(t, handler, "/api/v1/issues/CORE-1", "verification.md", verification("The check that proves it."))
	failed := recordChainResult(t, handler, "CORE-1", "cf", "fail", map[string]any{"failed": 1})
	moved := recordChainResult(t, handler, "CORE-1", "cm", "pass", nil)
	retracted := recordChainResult(t, handler, "CORE-1", "cr", "pass", nil)
	passed := recordChainResult(t, handler, "CORE-1", "cp", "pass", nil)
	uploadDocument(t, handler, "/api/v1/issues/CORE-1", "verification.md", verification("The check that proves it, reworded."))
	if response := retractAsPerson(t, handler, retracted.ID, "The run named the wrong commit."); response.Code != http.StatusCreated {
		t.Fatalf("retract the pass of cr: status=%d body=%s", response.Code, response.Body.String())
	}

	chain, _ := readChain(t, handler, "CORE-1", "?at="+headCommit, "alice")
	for _, want := range []struct {
		check, requirement, reason string
		result                     model.CheckResult
	}{
		{"cf", "r1", unprovenFailed, failed},
		{"cm", "r2", unprovenWordsMoved, moved},
		{"cr", "r3", unprovenRetracted, retracted},
	} {
		check := chainCheckOf(t, chain, want.check)
		if check.Proved || check.PassStands.Answer != model.PassStandsNo ||
			!slices.Contains(check.Unproven, model.IssueChainUnproven{Reason: want.reason, Result: want.result.ID}) {
			t.Errorf("%s reads %+v; want it unproven, its one result %s keeping it for %s", want.check, check, want.result.ID, want.reason)
		}
		if requirement := chainRequirementOf(t, chain, want.requirement); requirement.Proved {
			t.Errorf("%s, served by %s alone, reads as proved", want.requirement, want.check)
		}
		if held := chainResultsOf(chain, want.result.ID); len(held) != 1 || held[0].Proves {
			t.Errorf("the result %s of %s reads %+v, want it listed once, proving nothing", want.result.ID, want.check, held)
		}
	}
	if check := chainCheckOf(t, chain, "cm"); slices.Contains(check.Unproven, model.IssueChainUnproven{Reason: unprovenFailed, Result: moved.ID}) {
		t.Errorf("cm's pass reads as failed: %+v", check.Unproven)
	}
	if held := chainResultsOf(chain, retracted.ID); len(held) != 1 || held[0].Result.Retracted == nil || held[0].Result.Retracted.Reason != "The run named the wrong commit." {
		t.Errorf("the retracted result reads %+v, want it with its retraction", held)
	}
	if check := chainCheckOf(t, chain, "cp"); !check.Proved || len(check.Unproven) != 0 || !chainRequirementOf(t, chain, "r4").Proved {
		t.Errorf("cp, whose pass stands, reads %+v; want it and r4 proved", check)
	}
	if held := chainResultsOf(chain, passed.ID); len(held) != 1 || !held[0].Proves {
		t.Errorf("the standing pass of cp reads %+v, want it proving its check", held)
	}
}

// TestTheChainHidesAnAuthorHeldResultFromASecondCredential: the chain reads results as every read
// of the store reads a node. A result whose node is reserved for alice, who recorded it, is in her
// chain and proves her check; bob's chain holds nothing of it, and to him the check holds no
// result, stands unproven and is a gap.
func TestTheChainHidesAnAuthorHeldResultFromASecondCredential(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	if issue := createSpecIssue(t, handler, "CORE", ":::requirement{#r1}\nAn urgent ask comes first.\n:::\n"); issue.Key != "CORE-1" {
		t.Fatalf("the seeded issue is %s, want CORE-1, which its spec's references name", issue.Key)
	}
	uploadDocument(t, handler, "/api/v1/issues/CORE-1", "verification.md", checkServing("c1", "dispatch://CORE-1/spec#b-r1"))
	result := recordChainResult(t, handler, "CORE-1", "c1", "pass", nil)
	// The store takes no result reserved for its author from a write, so the test reserves the
	// node by SQL, as the tree read's tests reserve a lens.
	tag, err := database.Pool.Exec(context.Background(), `update graph_nodes set visibility = $3 where kind = $1 and id = $2`,
		graphstore.KindResult, result.ID, graphstore.VisibilityAuthor)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("reserve the result %s for alice: %d rows, %v", result.ID, tag.RowsAffected(), err)
	}

	alices, _ := readChain(t, handler, "CORE-1", "?at="+headCommit, "alice")
	if check := chainCheckOf(t, alices, "c1"); !slices.Equal(check.Results, []string{result.ID}) || !check.Proved {
		t.Errorf("alice reads c1 as %+v, want her own result %s proving it", check, result.ID)
	}
	bobs, body := readChain(t, handler, "CORE-1", "?at="+headCommit, "bob")
	check := chainCheckOf(t, bobs, "c1")
	if len(check.Results) != 0 || check.Proved || !slices.Contains(check.Unproven, model.IssueChainUnproven{Reason: unprovenNoResult}) {
		t.Errorf("bob reads c1 as %+v, want it holding no result, unproven", check)
	}
	if len(bobs.Results) != 0 || strings.Contains(body, result.ID) {
		t.Errorf("bob's chain holds the results %+v, and carries alice's result %s: %t", bobs.Results, result.ID, strings.Contains(body, result.ID))
	}
	if chainRequirementOf(t, bobs, "r1").Proved || !slices.ContainsFunc(bobs.Gaps, func(gap model.IssueChainGap) bool {
		return gap.Kind == chainGapNoResult && gap.Node == check.ID
	}) {
		t.Errorf("bob reads r1 as proved, or c1 as no gap: %+v", bobs.Gaps)
	}
}
