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

// eightAskAttributes is any of the eight server-owned attributes of an ask block as an opening
// line writes it.
var eightAskAttributes = regexp.MustCompile(` (state|answered_by|answered_at|selected|answer|invalid|copied_from|copied_from_document)="`)

// The previous build writes the spec and a person answers its first ask there; the previous build
// stops and leaves the fleet; through this build an agent changes one word of the spec's prose,
// the spec's first accepted write on a fleet of one build, and once it has settled a person answers
// the second ask, declines the third and resolves the fourth through this build's routes. The
// previous build's CLI then requests approval of the spec against this build's server and is
// refused nothing, and the spec's latest version, which the answer, the decline and the resolve
// wrote, keeps on each ask's opening line the state its node holds, and the answerer and time
// where answered, and nothing more; the live text serves none of the eight.
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
		if state, _, _ := askNodeAnswerOf(t, ctx, builds, first); state == asks.StateAnswered {
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
		state, answeredBy, answeredAt := askNodeAnswerOf(t, ctx, builds, want.ask)
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
	if text := current.documentText(t, spec); eightAskAttributes.MatchString(text) {
		t.Errorf("GET /api/v1/artifacts/%s/text serves a server-owned ask attribute:\n%s", spec, text)
	}
	t.Logf("the previous build's CLI requested approval:\n%s\nthe spec's version %d:\n%s", requested, number, markdown)
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

// askNodeAnswerOf is what the ask's node holds now: its answer_state, the person its answered_by
// names and its answered_at, each "" where it holds none.
func askNodeAnswerOf(t *testing.T, ctx context.Context, builds *twoBuilds, askID string) (state, answeredBy, answeredAt string) {
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
