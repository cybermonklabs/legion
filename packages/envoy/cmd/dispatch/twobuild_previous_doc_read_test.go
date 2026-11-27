//go:build twobuild

// c-previous-release-doc-read: the `dispatch doc-read` of the release before still reads which
// asks of a spec are answered or resolved, and by whom, from this build's text route. That command
// reads the markdown alone and sends no `Dispatch-Asks` header, so the route serves it the kept
// form (docs' KeptText). This build's `dispatch doc-read` sends `Dispatch-Asks: 1`, reads none of
// the eight server-owned ask attributes in the markdown, and reads each answer from the asks member.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// docReadToken is the line a `dispatch doc-read` of either build prints its document's token on.
var docReadToken = regexp.MustCompile(`(?m)^Document token: (\S+)$`)

// docReadAskOpeners is the attributes of each ask block's opening line in what a `dispatch
// doc-read` printed, by the block's id.
func docReadAskOpeners(printed string) map[string]pmdoc.Attrs {
	openers := map[string]pmdoc.Attrs{}
	for _, line := range previousCLIAskOpener.FindAllString(printed, -1) {
		_, attrs, opens := pmdoc.ParseTypedDirectiveOpen(strings.TrimLeft(line, "> \t-"))
		if id, _ := attrs[pmdoc.BlockIDAttr].(string); opens && id != "" {
			openers[id] = attrs
		}
	}
	return openers
}

// Only this build serves. Through it an agent writes a spec that holds four asks, and a person
// answers two of them, resolves the third and leaves the fourth open. The previous build's
// `dispatch doc-read` of the spec then prints the state, the answerer and the time of each answered
// ask on its block's opening line, equal to the ask's node, the state of the resolved ask, no state
// on the open ask, and no answer's words on any block. This build's `dispatch doc-read` of the same
// spec prints none of the eight server-owned attributes on any block, and each answer in its trailer
// from the asks member. Both reads print the same token.
func TestThePreviousReleasesDocReadReadsWhichAsksAreDecided(t *testing.T) {
	ctx := context.Background()
	builds := newTwoBuilds(t)
	current := builds.startCurrent(t, nil)
	project := uniqueProjectKey(t, "PDR")
	current.createProject(t, project, "The previous doc-read")
	home := t.TempDir()
	created := currentCLI(t).runOK(t, current, builds.currentName(), home,
		"issue", "--project", project, "--title", "Decided asks read", "--spec", previousCLISpec, "--force")
	issue := key(t, project, created.stdout)
	var read servedIssue
	getJSON(t, current, "/api/v1/issues/"+issue, &read)
	spec := read.PrimaryArtifactID
	first := settledAskOf(t, current, issue, "first")
	second := settledAskOf(t, current, issue, "second")
	third := settledAskOf(t, current, issue, "third")
	settledAskOf(t, current, issue, "fourth")

	answerThrough(t, current, first, "Ship", "Ship the first now.")
	answerThrough(t, current, second, "Hold", "Hold the second.")
	current.call(t, http.MethodPost, "/api/v1/asks/"+third+"/resolve", `{"kind":"resolved","reason":"Settled in the thread."}`, http.StatusOK)
	waitForNoSettlementOwed(t, ctx, builds, spec)

	type decided struct {
		block, ask, state string
	}
	want := []decided{{"first", first, asks.StateAnswered}, {"second", second, asks.StateAnswered}, {"third", third, asks.StateResolved}, {"fourth", "", ""}}
	answered := map[string][2]string{}
	for _, ask := range want {
		if ask.state != asks.StateAnswered {
			continue
		}
		state, answeredBy, answeredAt := nodeAnswerOf(t, ctx, builds, ask.ask)
		if state != asks.StateAnswered || answeredBy == "" || answeredAt == "" {
			t.Fatalf("the node of ask %s (block %s) holds state %q, answered_by %q and answered_at %q, want the person's answer", ask.ask, ask.block, state, answeredBy, answeredAt)
		}
		answered[ask.block] = [2]string{answeredBy, answeredAt}
	}

	previousRead := builds.previousCLI().runOK(t, current, builds.currentName(), home, "doc-read", "--issue", issue)
	previousOpeners := docReadAskOpeners(previousRead.stdout)
	for _, ask := range want {
		attrs, held := previousOpeners[ask.block]
		if !held {
			t.Errorf("the previous build's dispatch doc-read prints no ask block %s:\n%s", ask.block, previousRead.stdout)
			continue
		}
		switch state, stated := attrs["state"]; {
		case ask.state == "" && stated:
			t.Errorf("the previous build's dispatch doc-read prints the open ask block %s with state=%v, want no state, as the release before reads an open ask", ask.block, state)
		case ask.state != "" && !stated:
			t.Errorf("the previous build's dispatch doc-read prints the ask block %s with no state, want state=%q, as its node holds it", ask.block, ask.state)
		case ask.state != "" && state != ask.state:
			t.Errorf("the previous build's dispatch doc-read prints the ask block %s with state=%v, want state=%q, as its node holds it", ask.block, state, ask.state)
		}
		if node, wanted := answered[ask.block]; wanted && (attrs["answered_by"] != node[0] || attrs["answered_at"] != node[1]) {
			t.Errorf("the previous build's dispatch doc-read prints the ask block %s with answered_by=%v and answered_at=%v, want its node's %q and %q", ask.block, attrs["answered_by"], attrs["answered_at"], node[0], node[1])
		}
		for _, name := range []string{"selected", "answer", "invalid"} {
			if value, held := attrs[name]; held {
				t.Errorf("the previous build's dispatch doc-read prints the ask block %s with %s=%v, want no answer's words on any block", ask.block, name, value)
			}
		}
	}

	currentRead := currentCLI(t).runOK(t, current, builds.currentName(), home, "doc-read", "--issue", issue)
	currentOpeners := docReadAskOpeners(currentRead.stdout)
	for _, ask := range want {
		attrs, held := currentOpeners[ask.block]
		if !held {
			t.Errorf("this build's dispatch doc-read prints no ask block %s:\n%s", ask.block, currentRead.stdout)
			continue
		}
		for _, name := range []string{"state", "answered_by", "answered_at", "selected", "answer", "invalid", "copied_from", "copied_from_document"} {
			if value, held := attrs[name]; held {
				t.Errorf("this build's dispatch doc-read prints the ask block %s with %s=%v, want none of the eight server-owned ask attributes on any block", ask.block, name, value)
			}
		}
	}
	// This build's trailer quotes each block id as JSON, so a block id holding a line feed forges no
	// trailer line (text-asks.ts, settledAskLines).
	words := map[string]string{"first": "Ship the first now.", "second": "Hold the second."}
	for block, node := range answered {
		id, _ := json.Marshal(block)
		quoted, _ := json.Marshal(words[block])
		if line := fmt.Sprintf("Answered %s by %s at %s: %s", id, node[0], node[1], quoted); !strings.Contains(currentRead.stdout, line) {
			t.Errorf("this build's dispatch doc-read prints no line opening %q, the answer the asks member holds:\n%s", line, currentRead.stdout)
		}
	}

	previousToken, currentToken := docReadToken.FindStringSubmatch(previousRead.stdout), docReadToken.FindStringSubmatch(currentRead.stdout)
	if previousToken == nil || currentToken == nil || previousToken[1] != currentToken[1] {
		t.Errorf("the previous build's dispatch doc-read prints the token %q and this build's %q, want one token whichever form the reader read", previousToken, currentToken)
	}
	t.Logf("the previous build's dispatch doc-read:\n%s\nthis build's dispatch doc-read:\n%s", previousRead.stdout, currentRead.stdout)
}
