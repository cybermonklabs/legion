package docs

//ci:reads packages/contracts/fixtures/decision-blocks/* decisionBlockFixture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// decisionBlockFixture reads one of the decision-block fixtures the browser's line-based cut uses
// (packages/contracts/fixtures/decision-blocks), and checks it is written as pmdoc.Render writes
// it, so a stored version of it reads back byte for byte.
func decisionBlockFixture(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile("../../../../contracts/fixtures/decision-blocks/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	markdown := string(content)
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	rendered, err := pmdoc.Render(tree)
	if err != nil {
		t.Fatalf("render fixture %s: %v", name, err)
	}
	if rendered != markdown {
		t.Fatalf("fixture %s is not written as pmdoc.Render writes it; rendered:\n%s", name, rendered)
	}
	return markdown
}

// askNodeOf is the one ask node of the block markdown holds.
func askNodeOf(t *testing.T, markdown string) *pmdoc.Node {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse block %q: %v", markdown, err)
	}
	node := firstAsk(tree)
	if node == nil {
		t.Fatalf("block %q parsed to no ask node", markdown)
	}
	return node
}

// withAttr is node with the attribute name set to value, as a peer on the document socket or
// settlement's own flag (invalid, which no rendering writes) sets one on the live tree.
func withAttr(node *pmdoc.Node, name string, value any) *pmdoc.Node {
	node.Attrs[name] = value
	return node
}

// The blocks the pure tests of the carry's node test read: own, a decided block as the roll window
// writes it from its row (ownDecided), and open, the block of an open row; ownBare and openBare are
// the same blocks as a version written once the fleet is one build holds them, with none of the
// server-owned attributes.
const (
	ownAnswered   = `:::ask{#own urgency="low" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-02T10:00:00.123456Z"`
	ownDecided    = ownAnswered + ` selected="[&#x22;East&#x22;]" answer="The quiet one first."}` + "\n"
	ownWithheld   = ownAnswered + "}\n"
	ownBare       = `:::ask{#own urgency="low" multiple="false"}` + "\n"
	ownBody       = "Which region goes first?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"
	openDirective = `:::ask{#open urgency="med" multiple="false" state="open"}` + "\n"
	openBare      = `:::ask{#open urgency="med" multiple="false"}` + "\n"
	openBody      = "How many regions take the first wave?\n:::\n"
)

// ownAndOpenRows is the asks rows of the blocks above: own, answered by alice as ownDecided shows
// it, and open, still open.
func ownAndOpenRows() map[string]model.Ask {
	text := "The quiet one first."
	return map[string]model.Ask{
		"own": {
			ID: "own-ask", State: "answered", Urgency: "low", Question: "Which region goes first?",
			Options: []model.AskOption{{Label: "East", Description: "The quiet one."}, {Label: "West", Description: "The busy one."}},
			Answer:  &model.AskAnswer{User: "alice", Selected: []string{"East"}, Text: &text, At: time.Date(2026, 10, 2, 10, 0, 0, 123456000, time.UTC)},
		},
		"open": {ID: "open-ask", State: "open", Urgency: "med", Question: "How many regions take the first wave?", Options: []model.AskOption{}},
	}
}

// A block of the version just written agrees with its node, the row that projects it, when it
// shows no answer state other than the row's: none at all, as every block of a version written
// once the fleet is one build, or what the roll window keeps in step with the row - with the
// answer's words, or without them where the document has no room for them, as the answer route
// and settlement write such a block (rollWindowAskAttributesWithheld) and the release before
// wrote it. A copy's provenance on a block of the document's own, a block settlement could not
// read, and a state, answerer, time, selection or answer other than the row's each disagree, and
// so does the block of a decided row that asks other wording than the row was decided on or that
// settlement cannot read. An open row's body is free. A copy of another document's ask agrees with
// its source the same way, its provenance among what the roll window keeps.
func TestAnAskAgreesWithItsNodeShowingNoAnswerStateOrTheRowsOwn(t *testing.T) {
	rows := ownAndOpenRows()
	for name, test := range map[string]struct {
		id     string
		node   *pmdoc.Node
		agrees bool
	}{
		"a decided block with none of the attributes":         {"own", askNodeOf(t, ownBare+ownBody), true},
		"an open block with none of the attributes":           {"open", askNodeOf(t, openBare+openBody), true},
		"a decided block as the roll window keeps it in step": {"own", askNodeOf(t, ownDecided+ownBody), true},
		"an open block as the roll window keeps it in step":   {"open", askNodeOf(t, openDirective+openBody), true},
		// The document had no room for the answer's words: the block says who answered and when.
		"a decided block with its answer left out for want of room": {"own", askNodeOf(t, ownWithheld+ownBody), true},
		// An open row's question is still being written: its block may ask what the row does not yet.
		"an open row's block reworded": {"open", askNodeOf(t, openBare+"How many cloud regions take the first wave?\n\n- Two: The fewest.\n:::\n"), true},

		"a copied node":                     {"own", withAttr(askNodeOf(t, ownDecided+ownBody), "copied_from", "ask-1"), false},
		"a node settlement could not read":  {"open", withAttr(askNodeOf(t, openBare+openBody), "invalid", `ask block "open" has an empty question`), false},
		"a decided node flagged invalid":    {"own", withAttr(askNodeOf(t, ownBare+ownBody), "invalid", `ask block "own" has an empty question`), false},
		"a decided node asking other words": {"own", askNodeOf(t, ownBare+"Which region goes last?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"), false},
		"a decided node asking other options": {
			"own", askNodeOf(t, ownBare+"Which region goes first?\n\n- East: The quiet one.\n:::\n"), false,
		},
		"a decided node settlement cannot read": {"own", askNodeOf(t, ownBare+"Which region goes first?\n\n- : The quiet one.\n:::\n"), false},
		"a decided node showing open":           {"own", askNodeOf(t, `:::ask{#own urgency="low" multiple="false" state="open"}`+"\n"+ownBody), false},
		"a decided node showing another answerer": {
			"own", askNodeOf(t, `:::ask{#own urgency="low" multiple="false" state="answered" answered_by="bob" answered_at="2026-10-02T10:00:00.123456Z" selected="[&#x22;East&#x22;]" answer="The quiet one first."}`+"\n"+ownBody), false,
		},
		"a decided node showing another answered_at": {
			"own", askNodeOf(t, `:::ask{#own urgency="low" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-02T10:00:00Z" selected="[&#x22;East&#x22;]" answer="The quiet one first."}`+"\n"+ownBody), false,
		},
		"a decided node with its answer left out showing another answerer": {
			"own", askNodeOf(t, `:::ask{#own urgency="low" multiple="false" state="answered" answered_by="bob" answered_at="2026-10-02T10:00:00.123456Z"}`+"\n"+ownBody), false,
		},
		"a decided node showing its selection without its words": {"own", askNodeOf(t, ownAnswered+` selected="[&#x22;East&#x22;]"}`+"\n"+ownBody), false},
		"a decided node showing another selection":               {"own", askNodeOf(t, ownAnswered+` selected="[&#x22;West&#x22;]" answer="The quiet one first."}`+"\n"+ownBody), false},
		"a decided node showing other answer text":               {"own", askNodeOf(t, ownAnswered+` selected="[&#x22;East&#x22;]" answer="The busy one first."}`+"\n"+ownBody), false},
		"an open node showing answered":                          {"open", askNodeOf(t, `:::ask{#open urgency="med" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-02T10:00:00Z"}`+"\n"+openBody), false},
		"an open node showing a selection":                       {"open", askNodeOf(t, `:::ask{#open urgency="med" multiple="false" state="open" selected="[&#x22;Two&#x22;]"}`+"\n"+openBody), false},
		"an open node showing a forged answerer alone":           {"open", askNodeOf(t, `:::ask{#open urgency="med" multiple="false" answered_by="mallory"}`+"\n"+openBody), false},
	} {
		if agrees := askAgreesWithItsNode(test.node, rows[test.id], rows[test.id]); agrees != test.agrees {
			t.Errorf("%s: agrees with its node = %v, want %v (attributes %#v)", name, agrees, test.agrees, test.node.Attrs)
		}
	}

	// A copy of own on another document, as the roll window writes it from own's row, its
	// provenance after who answered and when; copyWords is the answer's words it holds beside them.
	const source = "dispatch://TEST-1/artifact/source-md"
	copyOf := ownAnswered + ` copied_from="own-ask" copied_from_document="` + source + `"`
	const copyWords = ` selected="[&#x22;East&#x22;]" answer="The quiet one first."`
	for name, test := range map[string]struct {
		node   *pmdoc.Node
		agrees bool
	}{
		"a copy with none of the attributes":                   {askNodeOf(t, ownBare+ownBody), true},
		"a copy as the roll window keeps it in step":           {askNodeOf(t, copyOf+copyWords+"}\n"+ownBody), true},
		"a copy with its answer left out for want of room":     {askNodeOf(t, copyOf+"}\n"+ownBody), true},
		"a copy showing another answerer, its answer left out": {askNodeOf(t, strings.Replace(copyOf, `answered_by="alice"`, `answered_by="bob"`, 1)+"}\n"+ownBody), false},
		"a copy naming no source, its answer left out":         {askNodeOf(t, ownWithheld+ownBody), false},
		"a copy showing its selection without its words":       {askNodeOf(t, copyOf+` selected="[&#x22;East&#x22;]"}`+"\n"+ownBody), false},
		"a copy settlement could not read":                     {withAttr(askNodeOf(t, copyOf+"}\n"+ownBody), "invalid", `ask block "own" has an empty question`), false},
	} {
		if agrees := copyAgreesWithItsSource(test.node, copiedSource{ask: rows["own"], document: source}, rows["own"]); agrees != test.agrees {
			t.Errorf("%s: agrees with its source = %v, want %v (attributes %#v)", name, agrees, test.agrees, test.node.Attrs)
		}
	}

	// A process of the release before answered own, and the decisions step's catch-up has not yet
	// stated that answer on own's node, which still holds it open: a write on a fleet of one build
	// then keeps the row's state (keptAskState, keptFromItsNode), and a version written from the
	// node (onItsNode), which keeps no state of an open ask, shows the ask as its node holds it, so
	// a block showing what the node holds agrees, as one showing the row's does. A block showing a
	// state neither holds disagrees, and so does `state="open"`, which no write of this build stores
	// and the release before wrote only beside its own row's open state.
	behind := rows["own"]
	behind.State, behind.Answer = asks.StateOpen, nil
	for name, test := range map[string]struct {
		node   *pmdoc.Node
		agrees bool
	}{
		"a decided block showing its row's answer":               {askNodeOf(t, ownWithheld+ownBody), true},
		"a decided block with none of the attributes":            {askNodeOf(t, ownBare+ownBody), true},
		"a decided block showing open":                           {askNodeOf(t, `:::ask{#own urgency="low" multiple="false" state="open"}`+"\n"+ownBody), false},
		"a decided block showing resolved":                       {askNodeOf(t, `:::ask{#own urgency="low" multiple="false" state="resolved"}`+"\n"+ownBody), false},
		"a decided block showing the node open with an answerer": {askNodeOf(t, `:::ask{#own urgency="low" multiple="false" answered_by="alice"}`+"\n"+ownBody), false},
	} {
		if agrees := askAgreesWithItsNode(test.node, rows["own"], behind); agrees != test.agrees {
			t.Errorf("%s, its node behind its row: agrees with its node = %v, want %v (attributes %#v)", name, agrees, test.agrees, test.node.Attrs)
		}
	}
	// A copy keeps its provenance whatever its source's state, so a copy of a source its node holds
	// open shows its provenance and no state, which only the node's reading accepts.
	if !copyAgreesWithItsSource(askNodeOf(t, `:::ask{#own urgency="low" multiple="false" copied_from="own-ask" copied_from_document="`+source+`"}`+"\n"+ownBody),
		copiedSource{ask: rows["own"], document: source}, behind) {
		t.Error("a copy showing its provenance and no state, its source's node open behind its row: disagrees with its source, want it to agree")
	}
	if copyAgreesWithItsSource(askNodeOf(t, `:::ask{#own urgency="low" multiple="false" copied_from="own-ask" copied_from_document="`+source+`"}`+"\n"+ownBody),
		copiedSource{ask: rows["own"], document: source}, rows["own"]) {
		t.Error("a copy showing its provenance and no state, its source answered on its node and its row: agrees with its source, want it to disagree")
	}
}

// withoutAskServerAttributes takes the eight off every ask block wherever it stands - at the top
// level, in a blockquote - and leaves every other node of the tree its own, so a tree without them
// is answered as it is, with nothing taken, and the tree it was given is not changed.
func TestWithoutAskServerAttributesTakesTheEightOffEveryAskBlock(t *testing.T) {
	quoted := "> " + strings.ReplaceAll(strings.TrimSuffix(openDirective+openBody, "\n"), "\n", "\n> ") + "\n"
	tree, err := pmdoc.Parse("Context.\n\n" + ownDecided + ownBody + "\n" + quoted)
	if err != nil {
		t.Fatalf("parse the document: %v", err)
	}
	own := firstAsk(tree)
	own.Attrs["invalid"] = "a reason no rendering writes"
	stripped, took := withoutAskServerAttributes(tree)
	if !took {
		t.Fatal("taking the attributes off a tree whose ask blocks hold them took nothing")
	}
	asks := 0
	pmdoc.Walk(stripped, func(node *pmdoc.Node) bool {
		if node.Type == "ask" {
			asks++
			for _, name := range askServerAttributeNames {
				if value, held := node.Attrs[name]; held {
					t.Errorf("ask block %v still holds %s=%v", node.Attrs[pmdoc.BlockIDAttr], name, value)
				}
			}
		}
		return true
	})
	if asks != 2 {
		t.Fatalf("the stripped tree holds %d ask blocks, want both", asks)
	}
	if _, held := own.Attrs["state"]; !held {
		t.Fatal("taking the attributes off changed the tree it was given")
	}
	if again, took := withoutAskServerAttributes(stripped); took || again != stripped {
		t.Fatalf("a tree holding none of the attributes was taken through as a copy, took %v; want itself, nothing taken", took)
	}
	if stripped.Children[0] != tree.Children[0] {
		t.Fatal("the paragraph beside the ask blocks was copied, want the tree's own node")
	}
}

// askNodes reads every ask node of a tree wherever it stands, each id once - the live tree of N
// and the stored tree of the version he approved are both read through it; a tree that repeats a
// block id, which settlement reads no block under, is not read at all.
func TestAskNodesOfTheCarriedVersionReadEveryBlockOnceAndRefuseARepeatedID(t *testing.T) {
	t.Parallel()
	tree, err := pmdoc.Parse(decisionBlockFixture(t, "answered.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]*pmdoc.Node{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "ask" {
			id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			want[id] = node
		}
		return true
	})
	for _, id := range []string{"wave-size", "rollback", "regions", "budget", "second-wave"} {
		if _, held := want[id]; !held {
			t.Fatalf("the fixture holds no block %q", id)
		}
	}
	if got, readable := askNodes(tree); !readable || !reflect.DeepEqual(got, want) {
		t.Fatalf("askNodes = %#v, %v; want %#v, readable", got, readable, want)
	}
	if got, readable := askNodes(nil); got != nil || !readable {
		t.Fatalf("askNodes(nil) = %#v, %v; want none, readable", got, readable)
	}

	const wave = `:::ask{#wave urgency="med" multiple="false"}` + "\nHow many regions take the first wave?\n:::\n"
	repeated := &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{
		askNodeOf(t, wave),
		{Type: "blockquote", Attrs: pmdoc.Attrs{}, Children: []*pmdoc.Node{askNodeOf(t, wave)}},
	}}
	if got, readable := askNodes(repeated); got != nil || readable {
		t.Fatalf("askNodes of a tree repeating a block id = %#v, %v; want none, not readable", got, readable)
	}
}

// The rule over the two versions: N carries the approval of A when, with the server-owned
// attributes taken off every ask block of each, the two render byte for byte the same, something
// was taken off one of them, A's stored text is what this build renders A to, and every block id
// of A still stands. The attributes dropped, or kept in step with an answer given since, carry; a
// word of prose, an ask's question, a block added or a block removed beside them do not; nor does
// a version that differs from A in nothing a case takes out, nor one A's stored bytes are not this
// build's rendering of. That every ask of N agrees with its node is its callers' to hold first
// (asksAgreeWithTheirNodes).
func TestCarryVerdictOverTheApprovedAndTheNewVersion(t *testing.T) {
	const prose = "# Rollout\n\nThe service ships behind a flag.\n\n"
	approvedText := prose + ownDecided + ownBody + "\n" + openDirective + openBody
	parse := func(markdown string) *pmdoc.Node {
		t.Helper()
		tree, err := pmdoc.ParseRendering(markdown)
		if err != nil {
			t.Fatalf("parse %q: %v", markdown, err)
		}
		return tree
	}
	if rendered, err := pmdoc.Render(parse(approvedText)); err != nil || rendered != approvedText {
		t.Fatalf("the approved fixture renders %q (%v), want itself: the verdict compares stored bytes", rendered, err)
	}
	answeredOpen := `:::ask{#open urgency="med" multiple="false" state="answered" answered_by="bob" answered_at="2026-10-03T10:00:00Z" selected="[]" answer="Two."}` + "\n"
	for _, test := range []struct {
		name     string
		approved string
		version  string
		carry    bool
	}{
		{"the attributes dropped, every other byte the same", approvedText, prose + ownBare + ownBody + "\n" + openBare + openBody, true},
		{"the attributes kept in step with an answer given since", approvedText, prose + ownDecided + ownBody + "\n" + answeredOpen + openBody, true},
		{"one word of prose changed beside the removal", approvedText, strings.Replace(prose, "a flag", "a switch", 1) + ownBare + ownBody + "\n" + openBare + openBody, false},
		{"an ask's question changed beside the removal", approvedText, prose + ownBare + ownBody + "\n" + openBare + "How many regions take the second wave?\n:::\n", false},
		{"a block added beside the removal", approvedText, prose + ownBare + ownBody + "\n" + openBare + openBody + "\nA new paragraph.\n", false},
		{"a block the approved version held removed", approvedText, prose + ownBare + ownBody, false},
		{"nothing a case takes out: neither version holds the attributes", prose + ownBare + ownBody + "\n" + openBare + openBody, prose + ownBare + ownBody + "\n" + openBare + openBody, false},
		{"approved bytes this build does not render so", strings.Replace(approvedText, "# Rollout\n\n", "Rollout\n=======\n\n", 1), prose + ownBare + ownBody + "\n" + openBare + openBody, false},
	} {
		change, carried, err := carryVerdict(carryInput{approved: parse(test.approved), approvedText: test.approved, version: parse(test.version)})
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if carried != test.carry {
			t.Errorf("%s: carry = %v, want %v", test.name, carried, test.carry)
		}
		if want := attributeRemovalCase.change + carriedReasonEnd; carried && change != want {
			t.Errorf("%s: the carried change reads %q, want %q", test.name, change, want)
		}
	}
}

// approveStoredVersion records login's approval of version number of artifactID as a row alone,
// with no event and so no event sequence: a review recorded before migration 0120, which the
// carry reads as it reads any other.
func approveStoredVersion(t *testing.T, database *store.Store, artifactID string, number int, login string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into artifact_reviews (artifact_id, version, state, actor)
		values ($1, $2, 'approved', $3)
	`, artifactID, number, `{"kind":"user","id":"`+login+`"}`); err != nil {
		t.Fatalf("approve version %d as %s: %v", number, login, err)
	}
}

// A changes request committed after an approval stops a carry of that approval, and one committed
// before it does not, in the order their events committed: by sequence, though each one's
// created_at, its transaction's start, says the opposite. A request that carries a sequence is
// after an approval that carries none, and the time and id decide only when neither does.
func TestChangesRequestedSinceFollowsTheOrderTheReviewsCommitted(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "Context\n")
	early := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	late := early.Add(time.Minute)
	insert := func(state string, at time.Time, seq *int64) model.ArtifactReview {
		t.Helper()
		review, err := ScanReview(database.Pool.QueryRow(context.Background(), `
			insert into artifact_reviews (artifact_id, version, state, actor, created_at, event_seq)
			values ($1, 1, $2, '{"kind":"user","id":"alice"}', $3, $4)
			returning `+ReviewColumns, artifactID, state, at, seq))
		if err != nil {
			t.Fatalf("insert %s review: %v", state, err)
		}
		return review
	}
	sequence := func(n int64) *int64 { return &n }
	for _, test := range []struct {
		name                    string
		approvalAt, requestAt   time.Time
		approvalSeq, requestSeq *int64
		since                   bool
	}{
		{"a request committed after the approval though its transaction began first", late, early, sequence(5), sequence(6), true},
		{"a request committed before the approval though its transaction began last", early, late, sequence(6), sequence(5), false},
		{"a sequenced request after an approval recorded before the sequence was kept", late, early, nil, sequence(6), true},
		{"an unsequenced request before a sequenced approval", early, late, sequence(6), nil, false},
		{"neither sequenced, the request later", early, late, nil, nil, true},
		{"neither sequenced, the request earlier", late, early, nil, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := database.Pool.Exec(context.Background(), `delete from artifact_reviews where artifact_id = $1`, artifactID); err != nil {
				t.Fatal(err)
			}
			approval := insert("approved", test.approvalAt, test.approvalSeq)
			insert("changes_requested", test.requestAt, test.requestSeq)
			got, err := changesRequestedSince(context.Background(), database.Pool, approval)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.since {
				t.Fatalf("changes requested since the approval = %v, want %v", got, test.since)
			}
		})
	}
}

// forgedAnsweredAsk appends to the document an ask block carrying the attributes only settlement
// writes - answered, by login, now - as a peer on the document socket can write them, holding
// body.
func forgedAnsweredAsk(id, login string, body ...*pmdoc.Node) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = append(tree.Children, &pmdoc.Node{
			Type: "ask",
			Attrs: pmdoc.Attrs{
				pmdoc.BlockIDAttr: id, "urgency": "high", "multiple": false,
				"state": "answered", "answered_by": login, "answered_at": time.Now().UTC().Format(time.RFC3339Nano),
			},
			Children: body,
		})
		return tree
	}
}

// carriedReviews is what a document's reviews and events say about a carry: how many reviews it
// has, and how many artifact.approved events name it.
func carriedReviews(t *testing.T, database *store.Store, artifactID string) (reviews, approvals int) {
	t.Helper()
	ctx := context.Background()
	if err := database.Pool.QueryRow(ctx, `select count(*) from artifact_reviews where artifact_id = $1`, artifactID).Scan(&reviews); err != nil {
		t.Fatalf("count reviews: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) from events where type = 'artifact.approved' and payload->>'artifact_id' = $1`, artifactID).Scan(&approvals); err != nil {
		t.Fatalf("count artifact.approved events: %v", err)
	}
	return reviews, approvals
}

// storedVersionMarkdown is the markdown of version number of artifactID.
func storedVersionMarkdown(t *testing.T, database *store.Store, artifactID string, number int) string {
	t.Helper()
	var markdown string
	if err := database.Pool.QueryRow(context.Background(), `select markdown from artifact_versions where artifact_id = $1 and number = $2`, artifactID, number).Scan(&markdown); err != nil {
		t.Fatalf("read version %d: %v", number, err)
	}
	return markdown
}

// An agent on the document socket writes an ask block in the attributes only the server writes:
// answered, by alice, who approved version 1. Its question is empty, so settlement cannot read it
// and writes no row for it; it flags the block invalid in the live tree (the renderer writes no
// `invalid` into markdown), and since the fleet is one build - no other build is registered - it
// takes the forged attributes off the block, so version 2 holds none of the seven a version can
// store and the live tree holds none of them either. The carry reads the rows and not the block:
// a node no row of this document backs refuses it, and alice's approval stays at version 1.
func TestAForgedAnsweredBlockOnTheSocketCarriesNoApproval(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "Context\n")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: 20 * time.Millisecond})
	testcleanup.Add(t, func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "Context\n")
	approveStoredVersion(t, database, artifactID, 1, "alice")

	editLiveTree(t, service, artifactID, forgedAnsweredAsk("forged", "alice", &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{}}))
	waitForDocumentVersion(t, database, artifactID, 2)
	markdown := storedVersionMarkdown(t, database, artifactID, 2)
	if !strings.Contains(markdown, `:::ask{#forged urgency="high" multiple="false"}`) {
		t.Fatalf("version 2 = %q, want the forged block with none of its forged attributes", markdown)
	}
	forged := firstAsk(liveTree(t, service, artifactID))
	if invalid, _ := forged.Attrs["invalid"].(string); !strings.Contains(invalid, "empty question") {
		t.Fatalf("forged block invalid = %q, want settlement to have flagged its empty question", invalid)
	}
	for _, name := range storedAskAttributes() {
		if value, held := forged.Attrs[name]; held {
			t.Errorf("the forged block keeps %s=%v in the live tree, want none of the seven on a fleet of one build", name, value)
		}
	}
	var rows int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from asks where block_artifact_id = $1 and block_id = 'forged'`, artifactID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("rows for the forged block = %d, want none", rows)
	}
	if reviews, approvals := carriedReviews(t, database, artifactID); reviews != 1 || approvals != 0 {
		t.Fatalf("reviews = %d, artifact.approved events = %d; want alice's approval of version 1 alone and no event", reviews, approvals)
	}
}

// joined runs op inside a transaction joined to the service (Join), committed through its ledger,
// as the answer route and the named-version route run their writes.
func joined(t *testing.T, service *Service, op func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background())
	ctx, ledger := service.Join(context.Background(), tx)
	defer ledger.Discard()
	if err := op(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// answerBlockAs writes login's answer to the block ask blockID of artifactID as the answer route
// writes it (settleAskAs): on the ask's node and its row, and into the block in step with them -
// while another build serves (serveTwoBuilds) the answer's words among them, and on a fleet of one
// build only the ones a version keeps, its state, answerer and time (R3).
func answerBlockAs(t *testing.T, service *Service, artifactID, blockID, login string, selected []string) {
	t.Helper()
	person := model.Actor{Kind: "user", ID: login}
	at := time.Now().UTC()
	change := asks.Answered(model.AskAnswer{User: login, Selected: selected, At: at})
	change.Who, change.At = person, at
	settleAskAs(t, service, artifactID, blockID, blockAskOn(t, service, artifactID, blockID).ID, person, change)
}

// regionSpec is the one-block document the repeated-block-id carry tests decide and then repeat:
// a single ask under the block id region.
const regionSpec = "Context\n\n:::ask{#region urgency=\"low\" multiple=\"false\"}\nWhich region goes first?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"

// seededRegionDocument creates regionSpec as a document, starts a service that settles a live
// write after settle, seeds the document's live text with the same markdown, and returns the
// store, the service and the document's id once settlement has indexed the block as an ask of the
// document - the row the carry reads.
func seededRegionDocument(t *testing.T, settle time.Duration) (*store.Store, *Service, string) {
	t.Helper()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, regionSpec)
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: settle})
	testcleanup.Add(t, func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, regionSpec)
	// Settlement indexes the block as an ask of this document, the row the carry reads.
	service.ScheduleSettlement(artifactID)
	waitFor(t, 3*time.Second, "settlement to index the block", func() bool {
		var rows int
		if err := database.Pool.QueryRow(context.Background(), `select count(*) from asks where block_artifact_id = $1 and block_id = 'region'`, artifactID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows == 1
	})
	return database, service, artifactID
}

// appendSecondRegionAsk is the live-tree edit of a document-socket peer that writes a second ask
// node under the block id region, leaving the tree holding that id twice.
func appendSecondRegionAsk(t *testing.T) func(*pmdoc.Node) *pmdoc.Node {
	t.Helper()
	return func(tree *pmdoc.Node) *pmdoc.Node {
		second, err := pmdoc.Parse(":::ask{#region urgency=\"low\" multiple=\"false\"}\nWhich region goes last?\n\n- North: The cold one.\n:::\n")
		if err != nil {
			t.Fatal(err)
		}
		tree.Children = append(tree.Children, second.Children...)
		return tree
	}
}

// A version named from the live tree while it holds two ask nodes under one decided block id - a
// second node a document-socket peer wrote with the id of the block the human answered - carries
// nothing: one node per id is the first thing the carry holds a version to, before any node is
// read. The same version without the repeat carries, so the repeat alone is what refuses. Another
// build serves (serveTwoBuilds), so the answer is written into the block and the version without
// the repeat differs from the approved one in the block's server-owned attributes alone, which
// R5's case carries.
func TestAVersionHoldingARepeatedBlockIDCarriesNothing(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, repeat bool) (reviews, approvals int) {
		database, service, artifactID := seededRegionDocument(t, 20*time.Millisecond)
		serveTwoBuilds(t, database)
		approveStoredVersion(t, database, artifactID, 1, "alice")

		answerBlockAs(t, service, artifactID, "region", "alice", []string{"East"})
		if repeat {
			editLiveTree(t, service, artifactID, appendSecondRegionAsk(t))
		}
		joined(t, service, func(ctx context.Context, tx pgx.Tx) error {
			_, err := service.NamedVersion(ctx, artifactID, "Named by the agent", model.Actor{Kind: "session", ID: "session-0123456789abcdef"})
			return err
		})
		return carriedReviews(t, database, artifactID)
	}
	if reviews, approvals := run(t, false); reviews != 2 || approvals != 1 {
		t.Fatalf("without the repeat: reviews = %d, artifact.approved events = %d; want the version holding her answer to carry her approval", reviews, approvals)
	}
	if reviews, approvals := run(t, true); reviews != 1 || approvals != 0 {
		t.Fatalf("with the repeat: reviews = %d, artifact.approved events = %d; want alice's approval of version 1 alone and no event", reviews, approvals)
	}
}

// On a fleet of one build, a version a write takes without settling the document first - a named
// version, and the version an agent's edit takes at once - stores of the seven attributes a
// version can store only what every version then keeps for the release before's readers
// (keptAskState): the state, answerer and time, in step with the ask, though the live tree still holds what the roll window
// wrote into an ask block: here bob's answer, words and all, given while the release before still
// served. The named version differs from the one alice approved, which the release before's
// settlement wrote with the open state, in those attributes alone, and every ask of it agrees with
// its node, so her approval is carried to it by the case that compares the two without them
// (attributeRemovalCase, R5).
func TestOnOneBuildAVersionTakenBeforeSettlementKeepsOnlyTheAsksStateAndCarriesTheApproval(t *testing.T) {
	const spec = "Context\n\n:::ask{#ask-1 urgency=\"med\" multiple=\"false\"}\nShip it?\n\n- Yes: Now.\n- No: Later.\n:::\n"
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	for _, write := range []struct {
		name    string
		version func(ctx context.Context, service *Service, artifactID string) error
		carries bool
	}{
		{"a named version", func(ctx context.Context, service *Service, artifactID string) error {
			_, err := service.NamedVersion(ctx, artifactID, "Named by the agent", agent)
			return err
		}, true},
		{"an edit's version", func(ctx context.Context, service *Service, artifactID string) error {
			if _, err := service.ApplyOps(ctx, artifactID, []model.EditOp{{Op: "replace", Find: "Context", With: "Context, edited"}}, agent, nil); err != nil {
				return err
			}
			_, err := service.SnapshotVersion(ctx, artifactID, agent)
			return err
		}, false},
	} {
		t.Run(write.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			// The release before serves beside this build while the ask opens, and its settlement
			// writes the ask's state into the block, as this build's does for it.
			serveTwoBuilds(t, service.store)
			seedServiceText(t, service, artifactID, spec)
			settleCurrentGeneration(t, service, artifactID)
			approved := latestVersionMarkdown(t, service, artifactID)
			if !strings.Contains(approved, `#ask-1 urgency="med" multiple="false" state="open"}`) {
				t.Fatalf("the version written while the release before serves = %q, want the ask's block holding its open state", approved)
			}
			var number int
			if err := service.store.Pool.QueryRow(context.Background(), `select max(number) from artifact_versions where artifact_id = $1`, artifactID).Scan(&number); err != nil {
				t.Fatal(err)
			}
			approveStoredVersion(t, service.store, artifactID, number, "alice")

			// Bob answers while the release before still serves: the roll window writes his answer,
			// its options among it, into the live block.
			bob := model.Actor{Kind: "user", ID: "bob"}
			at := time.Now().UTC()
			change := asks.Answered(model.AskAnswer{User: "bob", Selected: []string{"Yes"}, At: at})
			change.Who, change.At = bob, at
			settleAskAs(t, service, artifactID, "ask-1", blockAskOn(t, service, artifactID, "ask-1").ID, bob, change)
			if selected := firstAsk(liveTree(t, service, artifactID)).Attrs["selected"]; !sameAskAttribute(selected, []string{"Yes"}) {
				t.Fatalf("the live block after bob's answer holds selected %v, want the roll window's [Yes]", selected)
			}
			// The release before's last process leaves: the fleet is one build.
			if _, err := service.store.Pool.Exec(context.Background(), `delete from server_processes where schema_version < $1`, pmdoc.SchemaVersion()); err != nil {
				t.Fatalf("remove the release before's process: %v", err)
			}

			if err := joinedWrite(service, func(ctx context.Context) error { return write.version(ctx, service, artifactID) }); err != nil {
				t.Fatalf("write the version: %v", err)
			}
			var written int
			if err := service.store.Pool.QueryRow(context.Background(), `select max(number) from artifact_versions where artifact_id = $1`, artifactID).Scan(&written); err != nil {
				t.Fatal(err)
			}
			if written != number+1 {
				t.Fatalf("the latest version is %d after the write, want the one it wrote, %d", written, number+1)
			}
			version := latestVersionMarkdown(t, service, artifactID)
			kept := fmt.Sprintf(`#ask-1 urgency="med" multiple="false" state="answered" answered_by="bob" answered_at=%q}`, asks.AnsweredAtValue(at))
			if !strings.Contains(version, kept) {
				t.Fatalf("the version taken before settlement = %q, want the ask's opener %s: its state, answerer and time and none of the other four", version, kept)
			}
			if !write.carries {
				return
			}
			if reviews, approvals := carriedReviews(t, service.store, artifactID); reviews != 2 || approvals != 1 {
				t.Fatalf("reviews = %d, artifact.approved events = %d; want alice's approval carried to the named version", reviews, approvals)
			}
			var reason string
			if err := service.store.Pool.QueryRow(context.Background(), `
				select coalesce(reason, '') from artifact_reviews where artifact_id = $1 and version = $2 and state = 'approved'
			`, artifactID, number+1).Scan(&reason); err != nil {
				t.Fatalf("read the carried review: %v", err)
			}
			if !strings.Contains(reason, attributeRemovalCase.change) {
				t.Fatalf("the carried review's reason = %q, want the server-owned ask attributes' case %q", reason, attributeRemovalCase.change)
			}
		})
	}
}

// attributeRemovalReason is the reason R5's case records when it alone carried the approval of
// version from: the opening cmd/dispatch's TestAnApprovalCarriesAcrossTheRemovalOfTheServerOwnedAskAttributes
// reads (carriedReasonPrefix), then that case's change and carriedReasonEnd, spelled out byte for
// byte as the api package's attributeRemovalSentence spells it.
func attributeRemovalReason(from int) string {
	return fmt.Sprintf("Carried forward from the approval of version %d: the server-owned attributes of its ask blocks, which hold no answer, were the whole change since it.", from)
}

// approvedReasonAt is the reason of the approval of version number of artifactID.
func approvedReasonAt(t *testing.T, service *Service, artifactID string, number int) string {
	t.Helper()
	var reason string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select coalesce(reason, '') from artifact_reviews where artifact_id = $1 and version = $2 and state = 'approved'
	`, artifactID, number).Scan(&reason); err != nil {
		t.Fatalf("read the approval of version %d: %v", number, err)
	}
	return reason
}

// askOpeningLine is the opening line of the ask block blockID in markdown, "" where it holds none.
func askOpeningLine(markdown, blockID string) string {
	for line := range strings.SplitSeq(markdown, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, ":"), "ask{#"+blockID+" ") {
			return line
		}
	}
	return ""
}

// While another build serves, the answer route and settlement write an answer's words into its
// block only where the document has room for them; a block without room says who answered and
// when, as the release before wrote such a block (rollWindowAskAttributesWithheld), and agrees
// with its node. Alice approves the version the release before's settlement wrote of a document
// some hundred bytes short of what one upload may hold, the ask's block holding its open state;
// bob answers in 4,000 characters of words, which the document has no room for, so his answer is
// written into the block without them, and the version settlement writes of it differs from the
// one alice approved in the block's server-owned attributes alone. That version carries her
// approval by R5's case, which its reason names alone, and the ask keeps bob's words whole. Read
// against the write with the words alone, the block disagreed with its node, and bob's answer
// superseded her approval.
func TestAnAnswerLeftOutOfItsBlockForWantOfRoomCarriesTheApproval(t *testing.T) {
	const ask = ":::ask{#ask-1 urgency=\"med\" multiple=\"false\"}\nShip it?\n\n- Yes: Now.\n- No: Later.\n:::\n"
	// The prose leaves the document 1,000 bytes short of the cap: room for the block's state,
	// answerer and time, and none for bob's words.
	prose := strings.TrimSpace(strings.Repeat("word ", (pmdoc.MaxDocumentBytes-1000)/len("word ")))
	words := strings.TrimSpace(strings.Repeat("words ", 4000/len("words ")))
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	serveTwoBuilds(t, service.store)
	seedServiceText(t, service, artifactID, prose+"\n\n"+ask)
	settleCurrentGeneration(t, service, artifactID)
	approved := latestVersionNumber(t, service, artifactID)
	if line := askOpeningLine(latestVersionMarkdown(t, service, artifactID), "ask-1"); line != `:::ask{#ask-1 urgency="med" multiple="false" state="open"}` {
		t.Fatalf("the version written while the release before serves holds the block %q, want it holding its open state", line)
	}
	approveStoredVersion(t, service.store, artifactID, approved, "alice")

	bob := model.Actor{Kind: "user", ID: "bob"}
	at := time.Now().UTC()
	change := asks.Answered(model.AskAnswer{User: "bob", Selected: []string{"Yes"}, Text: &words, At: at})
	change.Who, change.At = bob, at
	settleAskAs(t, service, artifactID, "ask-1", blockAskOn(t, service, artifactID, "ask-1").ID, bob, change)
	settleCurrentGeneration(t, service, artifactID)

	written := latestVersionNumber(t, service, artifactID)
	if written != approved+1 {
		t.Fatalf("the latest version is %d after bob's answer settled, want the one version settlement wrote of it, %d", written, approved+1)
	}
	version := latestVersionMarkdown(t, service, artifactID)
	if len(version) > pmdoc.MaxDocumentBytes {
		t.Fatalf("the version settlement wrote of bob's answer is %d bytes, past the %d one upload may hold", len(version), pmdoc.MaxDocumentBytes)
	}
	if line := askOpeningLine(version, "ask-1"); !strings.HasPrefix(line, `:::ask{#ask-1 urgency="med" multiple="false" state="answered" answered_by="bob" answered_at="`) ||
		strings.Contains(line, "selected=") || strings.Contains(line, "answer=") {
		t.Fatalf("the version settlement wrote of bob's answer holds the block %q, want who answered and when, without the words the document has no room for", line)
	}
	if kept := blockAskOn(t, service, artifactID, "ask-1"); kept.Answer == nil || kept.Answer.Text == nil || *kept.Answer.Text != words {
		t.Fatalf("the ask after bob's answer = %#v, want it holding his words whole", kept.Answer)
	}
	if reviews, approvals := carriedReviews(t, service.store, artifactID); reviews != 2 || approvals != 1 {
		t.Fatalf("reviews = %d, artifact.approved events = %d; want alice's approval carried to version %d", reviews, approvals, written)
	}
	if reason := approvedReasonAt(t, service, artifactID, written); reason != attributeRemovalReason(approved) {
		t.Fatalf("the review carried to version %d reads %q, want %q", written, reason, attributeRemovalReason(approved))
	}
}

// carrySpec is a one-ask document whose ask a person answers while the release before serves,
// whose block the release before's settlement writes the answer into, words and selection.
const carrySpec = "Context\n\n:::ask{#ask-1 urgency=\"med\" multiple=\"false\"}\nShip it?\n\n- Yes: Now.\n- No: Later.\n:::\n"

// approvedBeforeOneBuild seeds carrySpec into a new document while the release before serves
// beside this build, settles it, has bob answer its ask and settles that, records alice's approval
// of the version settlement wrote - its block holding the answer whole - with reason ("" for
// none), and then has the release before's last process leave, so the fleet is one build and the
// next version takes the answer's selection off the block (keptAskState). It answers the service,
// the document and the version alice approved.
func approvedBeforeOneBuild(t *testing.T, reason string) (*Service, string, int) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	serveTwoBuilds(t, service.store)
	seedServiceText(t, service, artifactID, carrySpec)
	settleCurrentGeneration(t, service, artifactID)
	answerBlockAs(t, service, artifactID, "ask-1", "bob", []string{"Yes"})
	settleCurrentGeneration(t, service, artifactID)
	approved := latestVersionNumber(t, service, artifactID)
	if line := askOpeningLine(latestVersionMarkdown(t, service, artifactID), "ask-1"); !strings.Contains(line, ` state="answered" answered_by="bob" `) ||
		!strings.HasSuffix(line, ` selected="[&#x22;Yes&#x22;]"}`) {
		t.Fatalf("the version written while the release before serves holds the block %q, want it holding its answer whole", line)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into artifact_reviews (artifact_id, version, state, actor, reason)
		values ($1, $2, 'approved', '{"kind":"user","id":"alice"}', nullif($3, ''))
	`, artifactID, approved, reason); err != nil {
		t.Fatalf("approve version %d as alice: %v", approved, err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `delete from server_processes where schema_version < $1`, pmdoc.SchemaVersion()); err != nil {
		t.Fatalf("remove the release before's process: %v", err)
	}
	return service, artifactID, approved
}

// namedVersionByTheAgent names the live document as a version, as the agent's session, and
// answers the version it wrote.
func namedVersionByTheAgent(t *testing.T, service *Service, artifactID, summary string) int {
	t.Helper()
	if err := joinedWrite(service, func(ctx context.Context) error {
		_, err := service.NamedVersion(ctx, artifactID, summary, model.Actor{Kind: "session", ID: "session-0123456789abcdef"})
		return err
	}); err != nil {
		t.Fatalf("name %q: %v", summary, err)
	}
	return latestVersionNumber(t, service, artifactID)
}

// A carry names the version the approval it carries stands for only where that approval is itself
// a carry in the shape Dispatch writes one (approvalOrigin): a reason in the carried opening,
// naming a version before its own at which the same person approved the document. Alice approves
// a version with a reason of her own that opens as a carried reason does, naming a version past
// any the document holds, a version before hers that nobody approved, or one only bob approved;
// the next version, which drops the answer's selection, carries her approval naming the version she
// approved, not the one her words name.
func TestACarriedReasonAPersonWroteNamesNoVersionTheyDidNotApprove(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		// bob is the version bob approved before alice, 0 for none.
		bob int
	}{
		{"a version past any the document holds", "Carried forward from the approval of version 99: as I read it.", 0},
		{"a version before hers that nobody approved", "Carried forward from the approval of version 1: as I read it.", 0},
		{"a version before hers that only bob approved", "Carried forward from the approval of version 1: as I read it.", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID, approved := approvedBeforeOneBuild(t, test.reason)
			if test.bob > 0 {
				approveStoredVersion(t, service.store, artifactID, test.bob, "bob")
				// Bob's approval is older than alice's, whose approval is the newest review.
				if _, err := service.store.Pool.Exec(context.Background(), `
					update artifact_reviews set created_at = created_at - interval '1 hour' where artifact_id = $1 and version = $2
				`, artifactID, test.bob); err != nil {
					t.Fatalf("age bob's approval: %v", err)
				}
			}
			named := namedVersionByTheAgent(t, service, artifactID, "The state dropped")
			if named != approved+1 {
				t.Fatalf("the latest version is %d after the named version, want %d", named, approved+1)
			}
			if reason := approvedReasonAt(t, service, artifactID, named); reason != attributeRemovalReason(approved) {
				t.Fatalf("the review carried to version %d over alice's approval of version %d, whose reason reads %q, reads %q; want %q",
					named, approved, test.reason, reason, attributeRemovalReason(approved))
			}
		})
	}
}

// A version restoring the text of a version an approval was carried to says so: alice approves
// version A, written while the release before serves; once the fleet is one build a named version
// D drops the answer's selection and carries her approval; an edit of the prose supersedes it; and
// the edit undone restores D's text byte for byte, not A's. The approval carried to the restoring
// version names the version alice approved and the version whose text it holds, D, which her
// approval was carried to; said to hold A's text, the reason was false.
func TestARestoreOfACarriedVersionNamesTheVersionWhoseTextItHolds(t *testing.T) {
	service, artifactID, approved := approvedBeforeOneBuild(t, "")
	dropped := namedVersionByTheAgent(t, service, artifactID, "The state dropped")
	if reason := approvedReasonAt(t, service, artifactID, dropped); reason != attributeRemovalReason(approved) {
		t.Fatalf("the review carried to version %d reads %q, want %q", dropped, reason, attributeRemovalReason(approved))
	}
	droppedText := latestVersionMarkdown(t, service, artifactID)
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	edit := func(find, with string) int {
		t.Helper()
		if err := joinedWrite(service, func(ctx context.Context) error {
			if _, err := service.ApplyOps(ctx, artifactID, []model.EditOp{{Op: "replace", Find: find, With: with}}, agent, nil); err != nil {
				return err
			}
			_, err := service.SnapshotVersion(ctx, artifactID, agent)
			return err
		}); err != nil {
			t.Fatalf("replace %q with %q: %v", find, with, err)
		}
		return latestVersionNumber(t, service, artifactID)
	}
	if edited := edit("Context", "Context, edited"); edited != dropped+1 {
		t.Fatalf("the latest version is %d after the edit, want %d", edited, dropped+1)
	}
	restored := edit("Context, edited", "Context")
	if restored != dropped+2 {
		t.Fatalf("the latest version is %d after the edit was undone, want %d", restored, dropped+2)
	}
	if text := latestVersionMarkdown(t, service, artifactID); text != droppedText {
		t.Fatalf("version %d reads %q, want version %d's text %q byte for byte", restored, text, dropped, droppedText)
	}
	want := fmt.Sprintf("Carried forward from the approval of version %d: this version's text is version %d's, unchanged, which that approval was carried to.", approved, dropped)
	if reason := approvedReasonAt(t, service, artifactID, restored); reason != want {
		t.Fatalf("the review carried to version %d reads %q, want %q", restored, reason, want)
	}
}

// CarriedFromVersion reads the version out of the sentence each carry writes, over those very
// sentences, and reports nothing for a reason a person wrote. It is driven from
// carriedReasonPrefix, restoreReason and restoreCarriedReason themselves, so a change to any of
// them that the parser no longer reads fails here rather than leaving every carried review to
// read as a person's own.
func TestCarriedFromVersionReadsTheVersionEachCarrysReasonNames(t *testing.T) {
	carry := func(version int) string {
		return fmt.Sprintf(carriedReasonPrefix, version) + attributeRemovalCase.change + carriedReasonEnd
	}
	for _, test := range []struct {
		name    string
		reason  string
		version int
		carried bool
	}{
		{name: "a carry case's sentence", reason: carry(2), version: 2, carried: true},
		{name: "the restore's sentence", reason: fmt.Sprintf(restoreReason, 11), version: 11, carried: true},
		{name: "the restore of a carried version's sentence", reason: fmt.Sprintf(restoreCarriedReason, 3, 7), version: 3, carried: true},
		{name: "a reviewer's own reason", reason: "The rollback path is named."},
		{name: "no reason at all", reason: ""},
		{name: "the prefix with no version after it", reason: CarriedReviewReasonPrefix + ": unchanged."},
		{name: "the prefix later in the reason", reason: "Not " + carry(2)},
	} {
		t.Run(test.name, func(t *testing.T) {
			version, carried := CarriedFromVersion(test.reason)
			if carried != test.carried || version != test.version {
				t.Fatalf("CarriedFromVersion(%q) = %d, %t; want %d, %t",
					test.reason, version, carried, test.version, test.carried)
			}
		})
	}
}

// A transaction carries one document's approval at most once (errApprovalCarriedTwice, whose doc
// carries the reason). Here the agent names a version whose text is the approved text, which
// carries alice's approval, and both routes into the carry are then refused in that same
// transaction: a second named version of the document (carryApproval's guard) and the carry the
// upload route runs on its own (CarryUploadedApproval's). SnapshotVersion would not reach
// the carry for the first version, returning the latest version where the live text has not
// moved, so the versions are named.
func TestATransactionCannotCarryOneDocumentsApprovalTwice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "Context\n")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Second})
	testcleanup.Add(t, func() { _ = service.Shutdown(ctx) })
	seedServiceText(t, service, artifactID, "Context\n")
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	// The approved version is one this service wrote, so the version named below restores its
	// text byte for byte, which is what carries the approval (restoresApprovedText).
	joined(t, service, func(ctx context.Context, tx pgx.Tx) error {
		_, err := service.NamedVersion(ctx, artifactID, "Named before the approval", agent)
		return err
	})
	approveStoredVersion(t, database, artifactID, 2, "alice")

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	joinedCtx, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	restored, err := service.NamedVersion(joinedCtx, artifactID, "Restores the approved text", agent)
	if err != nil {
		t.Fatalf("the version restoring the approved text: %v", err)
	}
	if _, carried := ledger.PendingReview(artifactID); !carried {
		t.Fatal("the version restoring the approved text carried no approval, so the second version faces no first carry")
	}
	if _, err := service.NamedVersion(joinedCtx, artifactID, "A second version of the same document", agent); !errors.Is(err, errApprovalCarriedTwice) {
		t.Fatalf("the second version of the document in the same transaction = %v, want %v", err, errApprovalCarriedTwice)
	}
	// The upload route writes its version itself and carries its approval through its own entry
	// (the api package's storeArtifact calls CarryUploadedApproval), so that route's own guard
	// call is what refuses it here.
	if err := CarryUploadedApproval(joinedCtx, tx, service.events, artifactID, restored.Version.Number, "Context\n", nil, service.serverURL); !errors.Is(err, errApprovalCarriedTwice) {
		t.Fatalf("the upload route's carry over the same document in the same transaction = %v, want %v", err, errApprovalCarriedTwice)
	}
}
