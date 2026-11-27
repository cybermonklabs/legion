//go:build twobuild

// c-previous-release-cli: a `dispatch` command of the release before still requests approval of a
// spec whose asks are all decided once processes.Live shows one build, since every version this
// build writes then keeps each ask block's state on its opening line (docs' keptAskState), which
// that command's refuseOpenDecisionBlocks reads from the spec's stored latest version.

package main

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// previousCLISpec is the spec the previous build writes: a paragraph of prose and four asks.
const previousCLISpec = "# Decided asks\n\nThe service ships behind a flag.\n\n" +
	":::ask{#first urgency=\"med\" multiple=\"false\"}\nShip the first part?\n\n- Ship: Now.\n- Hold: Later.\n:::\n\n" +
	":::ask{#second urgency=\"med\" multiple=\"false\"}\nShip the second part?\n\n- Ship: Now.\n- Hold: Later.\n:::\n\n" +
	":::ask{#third urgency=\"med\" multiple=\"false\"}\nShip the third part?\n\n- Ship: Now.\n- Hold: Later.\n:::\n\n" +
	":::ask{#fourth urgency=\"med\" multiple=\"false\"}\nShip the fourth part?\n\n- Ship: Now.\n- Hold: Later.\n:::\n"

// answeredBeforeTheCatchUpSpec is the spec the previous build writes for the row of the window
// before this build's catch-up: a paragraph of prose and one ask, which the previous build answers.
const answeredBeforeTheCatchUpSpec = "# An answered ask\n\nThe service ships behind a flag.\n\n" +
	":::ask{#ship urgency=\"med\" multiple=\"false\"}\nShip the service?\n\n- Ship: Now.\n- Hold: Later.\n:::\n"

// eightAskAttributes is any of the eight server-owned attributes of an ask block as an opening
// line writes it.
var eightAskAttributes = regexp.MustCompile(` (state|answered_by|answered_at|selected|answer|invalid|copied_from|copied_from_document)="`)

// previousCLIAskOpener is the opening line of an ask block, at any depth and under any quote or
// list prefix, and previousCLIAskAttribute one of the eight server-owned attributes on it with its
// value, which holds no raw quote (the renderer writes one as a reference).
var (
	previousCLIAskOpener    = regexp.MustCompile(`(?m)^[> \t-]*:{3,}ask\{[^\n]*$`)
	previousCLIAskAttribute = regexp.MustCompile(` (state|answered_by|answered_at|selected|answer|invalid|copied_from|copied_from_document)="[^"]*"`)
)

// withoutTheEightAskAttributes is markdown with the eight server-owned attributes taken off every
// ask block's opening line, and every other byte as it is.
func withoutTheEightAskAttributes(markdown string) string {
	return previousCLIAskOpener.ReplaceAllStringFunc(markdown, func(line string) string {
		return previousCLIAskAttribute.ReplaceAllString(line, "")
	})
}

// The previous build writes the spec and a person answers its first ask there; the previous build
// stops and leaves the fleet; through this build an agent changes one word of the spec's prose,
// the spec's first accepted write on a fleet of one build, and once it has settled a person answers
// the second ask, declines the third and resolves the fourth through this build's routes. The
// previous build's CLI then requests approval of the spec against this build's server and is
// refused nothing, and the spec's latest version, which the answer, the decline and the resolve
// wrote, keeps on each ask's opening line the state its node holds, and the answerer and time
// where answered, and nothing more; the live text serves that version, character for character,
// with none of the eight.
func TestThePreviousReleasesCLIRequestsApprovalOverDecidedAsksOnceTheFleetIsOneBuild(t *testing.T) {
	ctx := context.Background()
	builds := newTwoBuilds(t)
	previous := builds.startPrevious(t, nil)
	current := builds.startCurrent(t, nil)
	project := uniqueProjectKey(t, "PRC")
	previous.createProject(t, project, "The previous CLI")
	issue, spec := previous.createIssueIn(t, project, "Decided asks", previousCLISpec)
	first := settledAskOf(t, previous, issue, "first")
	second := settledAskOf(t, previous, issue, "second")
	third := settledAskOf(t, previous, issue, "third")
	fourth := settledAskOf(t, previous, issue, "fourth")
	answerThrough(t, previous, first, "Ship", "Ship the first now.")
	waitForNoSettlementOwed(t, ctx, builds, spec)
	// This build's decisions step catches the previous build's answer up onto the ask's node while
	// both serve (ruling E1), which every version this build writes then reads it from.
	for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(time.Second) {
		if state, _, _ := nodeAnswerOf(t, ctx, builds, first); state == asks.StateAnswered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the previous build's answer to %s never reached the ask's node", first)
		}
	}

	stop(t, previous)
	waitForThePreviousBuildToLeaveTheFleet(t, builds)

	// The agent's one-word change of prose, the spec's first accepted write on one build.
	home := t.TempDir()
	currentCLI(t).runOK(t, current, builds.currentName(), home, "doc-edit", "--issue", issue,
		"--ops-json", `[{"op":"replace","find":"behind a flag","with":"behind a switch"}]`)
	waitForNoSettlementOwed(t, ctx, builds, spec)
	proseVersion, _ := latestVersionOf(t, current, spec)

	answerThrough(t, current, second, "Hold", "Hold the second.")
	current.call(t, http.MethodPost, "/api/v1/asks/"+third+"/decline", `{"note":"Not this release."}`, http.StatusOK)
	current.call(t, http.MethodPost, "/api/v1/asks/"+fourth+"/resolve", `{"kind":"resolved","reason":"Settled in the thread."}`, http.StatusOK)
	waitForNoSettlementOwed(t, ctx, builds, spec)

	cli := builds.previousCLI()
	cli.runOK(t, current, builds.currentName(), home, "claim", "--issue", issue)
	requested := cli.run(t, current, home, "request-approval", "--issue", issue, "--summary", "Every ask of the spec is decided.")
	if requested.code != 0 {
		t.Fatalf("the previous build's CLI refused dispatch request-approval on a spec whose asks are all decided:\n%s", requested)
	}

	number, markdown := latestVersionOf(t, current, spec)
	var open []model.Ask
	getJSON(t, current, "/api/v1/issues/"+issue+"/asks?state=open", &open)
	requestedAt := -1
	for _, ask := range open {
		if ask.Kind == "approval" && ask.Approval != nil {
			if document := ask.Approval.Document(spec); document != nil {
				requestedAt = document.Version
			}
		}
	}
	if requestedAt != number {
		t.Errorf("the spec holds an open approval request at version %d, want one at its latest version %d: %+v", requestedAt, number, open)
	}
	if number <= proseVersion {
		t.Errorf("the spec's latest version is %d, want one newer than %d, the prose write's: the answer, the decline and the resolve wrote none", number, proseVersion)
	}

	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("read the spec's latest version: %v\n%s", err, markdown)
	}
	for _, want := range []struct {
		block, ask, state string
	}{{"first", first, asks.StateAnswered}, {"second", second, asks.StateAnswered}, {"third", third, asks.StateResolved}, {"fourth", fourth, asks.StateResolved}} {
		node := askBlockOf(tree, want.block)
		if node == nil {
			t.Errorf("the spec's latest version holds no ask block %s:\n%s", want.block, markdown)
			continue
		}
		state, answeredBy, answeredAt := nodeAnswerOf(t, ctx, builds, want.ask)
		if state != want.state || node.Attrs["state"] != state {
			t.Errorf("block %s's opener holds state %v, want its node's %q (the node holds %q)", want.block, node.Attrs["state"], want.state, state)
		}
		if want.state == asks.StateAnswered && (node.Attrs["answered_by"] != answeredBy || node.Attrs["answered_at"] != answeredAt || answeredBy == "" || answeredAt == "") {
			t.Errorf("block %s's opener holds answered_by %v and answered_at %v, want its node's %q and %q", want.block, node.Attrs["answered_by"], node.Attrs["answered_at"], answeredBy, answeredAt)
		}
		if want.state != asks.StateAnswered {
			for _, name := range []string{"answered_by", "answered_at"} {
				if value, held := node.Attrs[name]; held {
					t.Errorf("block %s, whose ask is %s, holds %s=%v", want.block, want.state, name, value)
				}
			}
		}
		for _, name := range []string{"selected", "answer", "invalid"} {
			if value, held := node.Attrs[name]; held {
				t.Errorf("block %s's opener in version %d holds %s=%v", want.block, number, name, value)
			}
		}
	}
	// The live text as this build's CLI reads it, with `Dispatch-Asks: 1` (documentText).
	text := current.documentText(t, spec)
	if eightAskAttributes.MatchString(text) {
		t.Errorf("GET /api/v1/artifacts/%s/text, read with Dispatch-Asks: 1, serves a server-owned ask attribute:\n%s", spec, text)
	}
	// It serves the same spec: the version's stored text with the eight taken off, character for
	// character.
	if want := withoutTheEightAskAttributes(markdown); text != want {
		t.Errorf("GET /api/v1/artifacts/%s/text, read with Dispatch-Asks: 1, serves\n%s\nwant the spec's version %d with none of the eight server-owned ask attributes, character for character:\n%s",
			spec, text, number, want)
	}
	t.Logf("the previous build's CLI requested approval:\n%s\nthe spec's version %d:\n%s", requested, number, markdown)
}

// The window before this build's catch-up. This build's decisions step is held
// (DISPATCH_TEST_HOLD_DECISIONS_STEP), so its catch-up never states on the ask's node an answer the
// previous build records, as a step whose last tick ran before that answer has not. Whether the
// row reaches that window is the previous build's to say, and the row reads which build it runs
// beside from what that build does, before this build starts (previousStatesAnswersOnAskNodes). A
// build from before answers on ask nodes, as a release from before this change is, records a
// person's answer and states none on the ask's node, so the node holds no answer until a catch-up
// states it. A build that states them, such as this tree built as the previous build in the merge
// group's dispatch-two-builds-same, states the answer on the ask's node as it records it, so there
// is no window and the node holds the answer throughout.
//
// The previous build writes the spec and a person answers its one ask there; the previous build
// stops and leaves the fleet; through this build an agent changes one word of the spec's prose,
// the spec's first accepted write on a fleet of one build.
// In both cases the version that write settles into keeps on the ask's opening line the state, the
// answerer and the time the previous build's own version stored (R5), and none of selected,
// answer and invalid; and the previous build's CLI then requests approval of the spec against this
// build's server and is refused nothing.
func TestThePreviousReleasesCLIRequestsApprovalOverAnAskItAnsweredBeforeTheCatchUp(t *testing.T) {
	ctx := context.Background()
	builds := newTwoBuilds(t)
	previous := builds.startPrevious(t, nil)
	statesAnswers := previousStatesAnswersOnAskNodes(t, ctx, builds)
	t.Logf("the %s states a person's answer on the ask's node as it records it: %t", builds.previousName(), statesAnswers)
	current := builds.startCurrent(t, map[string]string{"DISPATCH_TEST_HOOKS": "1", "DISPATCH_TEST_HOLD_DECISIONS_STEP": "1"})
	issue, spec, ship := answeredThroughThePrevious(t, ctx, builds, "PRW", "The window before the catch-up", "An answered ask")

	// The previous build's version of its answer: the opener the previous build's CLI reads.
	answeredVersion, answeredMarkdown := latestVersionOf(t, previous, spec)
	answeredTree, err := pmdoc.ParseRendering(answeredMarkdown)
	if err != nil {
		t.Fatalf("read the spec's version %d: %v\n%s", answeredVersion, err, answeredMarkdown)
	}
	answered := askBlockOf(answeredTree, "ship")
	if answered == nil {
		t.Fatalf("the spec's version %d holds no ask block ship:\n%s", answeredVersion, answeredMarkdown)
	}
	answeredBy := stringAttr(answered, "answered_by")
	answeredAt, answeredAtErr := time.Parse(time.RFC3339Nano, stringAttr(answered, "answered_at"))
	if answered.Attrs["state"] != asks.StateAnswered || answeredBy == "" || answeredAtErr != nil {
		t.Fatalf("the previous build's answer to %s settled into version %d, whose opener holds state %v, answered_by %v and answered_at %v, want the answer's:\n%s",
			ship, answeredVersion, answered.Attrs["state"], answered.Attrs["answered_by"], answered.Attrs["answered_at"], answeredMarkdown)
	}

	stop(t, previous)
	waitForThePreviousBuildToLeaveTheFleet(t, builds)
	// Beside a build that states no answer on an ask's node, the window: the held step has stated
	// no answer on it. Beside one that states answers, the node holds the answer it recorded: state
	// answered, and the answerer and the time the previous build's own version stored.
	nodeState, nodeAnsweredBy, nodeAnsweredAt := nodeAnswerOf(t, ctx, builds, ship)
	if statesAnswers {
		if nodeState != asks.StateAnswered {
			t.Fatalf("the %s stated the answer to the probe's ask on that ask's node, and not the answer to ask %s on its own: the node holds state %q, answered_by %q and answered_at %q, want the answer it recorded",
				builds.previousName(), ship, nodeState, nodeAnsweredBy, nodeAnsweredAt)
		}
		if at, err := time.Parse(time.RFC3339Nano, nodeAnsweredAt); nodeAnsweredBy != answeredBy || err != nil || !at.Equal(answeredAt) {
			t.Fatalf("the node of ask %s holds answered_by %q and answered_at %q, want %q and %v, which the previous build's version %d stored",
				ship, nodeAnsweredBy, nodeAnsweredAt, answeredBy, answered.Attrs["answered_at"], answeredVersion)
		}
	} else if nodeState == asks.StateAnswered {
		t.Fatalf("the node of ask %s holds the previous build's answer while this build's decisions step is held: the row does not reach the window before the catch-up", ship)
	}

	// The agent's one-word change of prose, the spec's first accepted write on one build.
	home := t.TempDir()
	currentCLI(t).runOK(t, current, builds.currentName(), home, "doc-edit", "--issue", issue,
		"--ops-json", `[{"op":"replace","find":"behind a flag","with":"behind a switch"}]`)
	waitForNoSettlementOwed(t, ctx, builds, spec)
	number, markdown := latestVersionOf(t, current, spec)
	if number <= answeredVersion || !strings.Contains(markdown, "behind a switch") {
		t.Fatalf("the spec's latest version is %d, want one newer than %d that holds the agent's change:\n%s", number, answeredVersion, markdown)
	}
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("read the spec's version %d: %v\n%s", number, err, markdown)
	}
	block := askBlockOf(tree, "ship")
	if block == nil {
		t.Fatalf("the spec's version %d holds no ask block ship:\n%s", number, markdown)
	}
	if block.Attrs["state"] != asks.StateAnswered {
		t.Errorf("block ship's opener in version %d holds state %v, want %q, which the previous build's version %d stored (the ask's node holds state %q)",
			number, block.Attrs["state"], asks.StateAnswered, answeredVersion, nodeState)
	}
	if block.Attrs["answered_by"] != answeredBy {
		t.Errorf("block ship's opener in version %d holds answered_by %v, want %q, which the previous build's version %d stored", number, block.Attrs["answered_by"], answeredBy, answeredVersion)
	}
	if at, err := time.Parse(time.RFC3339Nano, stringAttr(block, "answered_at")); err != nil || !at.Equal(answeredAt) {
		t.Errorf("block ship's opener in version %d holds answered_at %v, want %v, which the previous build's version %d stored", number, block.Attrs["answered_at"], answered.Attrs["answered_at"], answeredVersion)
	}
	for _, name := range []string{"selected", "answer", "invalid"} {
		if value, held := block.Attrs[name]; held {
			t.Errorf("block ship's opener in version %d holds %s=%v", number, name, value)
		}
	}

	cli := builds.previousCLI()
	cli.runOK(t, current, builds.currentName(), home, "claim", "--issue", issue)
	requested := cli.run(t, current, home, "request-approval", "--issue", issue, "--summary", "The spec's one ask is answered.")
	if requested.code != 0 {
		t.Errorf("the previous build's CLI refused dispatch request-approval on a spec whose one ask it answered:\n%s", requested)
	}
	t.Logf("the previous build's CLI ran request-approval:\n%s\nthe spec's version %d, the previous build's answer:\n%s\nthe spec's version %d, after the agent's change:\n%s",
		requested, answeredVersion, answeredMarkdown, number, markdown)
}

// previousStatesAnswersOnAskNodes answers whether the previous build states a person's answer on
// the ask's node as it records it. Through that build it answers the one ask of a spec of a
// project of its own (answeredThroughThePrevious), and reads the ask's node: state answered is a
// build that states the answer there itself, and any other state, or none, a build from before
// that. It runs while the previous build's process is the only one live, so that build is the only
// process that can have written the node: this build has not started, so neither its boot nor its
// decisions step, held or not, can have written it. The probe's ask stays as it is for the rest of
// the row: no process answers it again, and this build's held step states nothing on its node.
func previousStatesAnswersOnAskNodes(t *testing.T, ctx context.Context, builds *twoBuilds) bool {
	t.Helper()
	_, _, ask := answeredThroughThePrevious(t, ctx, builds, "PRP", "Whether the previous build states answers on ask nodes", "An ask the previous build answers alone")
	state, _, _ := nodeAnswerOf(t, ctx, builds, ask)
	return state == asks.StateAnswered
}

// answeredThroughThePrevious makes, through the previous build, a project whose key starts with
// prefix and whose name is projectName, and in it an issue titled issueTitle whose spec is
// answeredBeforeTheCatchUpSpec; it waits for the ask of the spec's block ship, answers it there as
// the test's person, and waits until the spec owes no settlement. It answers the issue, the spec
// and the ask.
func answeredThroughThePrevious(t *testing.T, ctx context.Context, builds *twoBuilds, prefix, projectName, issueTitle string) (issue, spec, ask string) {
	t.Helper()
	previous := builds.previous()
	project := uniqueProjectKey(t, prefix)
	previous.createProject(t, project, projectName)
	issue, spec = previous.createIssueIn(t, project, issueTitle, answeredBeforeTheCatchUpSpec)
	ask = settledAskOf(t, previous, issue, "ship")
	answerThrough(t, previous, ask, "Ship", "Ship it now.")
	waitForNoSettlementOwed(t, ctx, builds, spec)
	return issue, spec, ask
}

// stringAttr is node's attribute name as a string, or "" where it holds none or another kind.
func stringAttr(node *pmdoc.Node, name string) string {
	value, _ := node.Attrs[name].(string)
	return value
}

// settledAskOf waits for the ask settlement indexed from the ask block blockID of the issue's
// documents through p, and answers its id.
func settledAskOf(t *testing.T, p *dispatchProcess, issue, blockID string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var listed []struct {
			ID      string  `json:"id"`
			BlockID *string `json:"block_id"`
		}
		getJSON(t, p, "/api/v1/issues/"+issue+"/asks?state=all", &listed)
		for _, ask := range listed {
			if ask.BlockID != nil && *ask.BlockID == blockID {
				return ask.ID
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ask was indexed from the block %s of %s within 30s: %+v", blockID, issue, listed)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// answerThrough answers the ask as the test's person through p's answer route.
func answerThrough(t *testing.T, p *dispatchProcess, askID, selected, text string) {
	t.Helper()
	p.call(t, http.MethodPost, "/api/v1/asks/"+askID+"/answer",
		jsonBody(t, map[string]any{"selected": []string{selected}, "text": text, "expected_edited_at": nil}), http.StatusOK)
}

// waitForNoSettlementOwed waits until doc_settlements_pending holds no row for the document.
func waitForNoSettlementOwed(t *testing.T, ctx context.Context, builds *twoBuilds, artifactID string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		// A write's settlement is armed a moment after the write answers.
		time.Sleep(3 * time.Second)
		var owed bool
		if err := builds.database.Pool.QueryRow(ctx, `select exists (select 1 from doc_settlements_pending where artifact_id = $1::uuid)`, artifactID).Scan(&owed); err != nil {
			t.Fatalf("read whether %s owes a settlement: %v", artifactID, err)
		}
		if !owed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still owes a settlement after 60s", artifactID)
		}
	}
}

// latestVersionOf is the document's newest version, its number and its stored text, as p reads
// them (GET /api/v1/artifacts/{id} and GET /api/v1/artifacts/{id}/versions/{number}).
func latestVersionOf(t *testing.T, p *dispatchProcess, artifactID string) (int, string) {
	t.Helper()
	var artifact struct {
		Versions []struct {
			Number int `json:"number"`
		} `json:"versions"`
	}
	getJSON(t, p, "/api/v1/artifacts/"+artifactID, &artifact)
	number := 0
	for _, version := range artifact.Versions {
		number = max(number, version.Number)
	}
	var version struct {
		Markdown string `json:"markdown"`
	}
	getJSON(t, p, "/api/v1/artifacts/"+artifactID+"/versions/"+strconv.Itoa(number), &version)
	return number, version.Markdown
}

// askBlockOf is tree's ask block blockID, or nil.
func askBlockOf(tree *pmdoc.Node, blockID string) *pmdoc.Node {
	var found *pmdoc.Node
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); node.Type == "ask" && id == blockID {
			found = node
		}
		return found == nil
	})
	return found
}

// nodeAnswerOf is what the ask's node holds now: its answer_state, the person its answered_by
// names and its answered_at, each "" where it holds none.
func nodeAnswerOf(t *testing.T, ctx context.Context, builds *twoBuilds, askID string) (state, answeredBy, answeredAt string) {
	t.Helper()
	rows, err := builds.database.Pool.Query(ctx, `
		select kind, to_id from (
			select distinct on (kind, case when to_kind = 'value' then '' else to_id end) kind, to_id, asserted
			from graph_tuples where from_kind = $1 and from_id = $2 and kind = any($3)
			order by kind, case when to_kind = 'value' then '' else to_id end, seq desc) newest
		where asserted
	`, graphstore.KindAsk, askID, []string{graphstore.TupleAnswerState, graphstore.TupleAnsweredBy, graphstore.TupleAnsweredAt})
	if err != nil {
		t.Fatalf("read the node of ask %s: %v", askID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, value string
		if err := rows.Scan(&kind, &value); err != nil {
			t.Fatalf("read a tuple of the node of ask %s: %v", askID, err)
		}
		switch kind {
		case graphstore.TupleAnswerState:
			state = value
		case graphstore.TupleAnsweredBy:
			answeredBy = strings.TrimSpace(value)
		case graphstore.TupleAnsweredAt:
			answeredAt = value
		}
	}
	return state, answeredBy, answeredAt
}
