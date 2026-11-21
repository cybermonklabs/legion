package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The exact text of each of alice's five author-only nodes, each holding one word no other node
// of the test holds, and the comment her run writes, whose own word the search control finds.
const (
	hiddenLensVerdict    = "The zephyrine rollout waits a week."
	hiddenFindingText    = "The thistlemoor queue drops writes."
	hiddenRiskHeading    = "The marblewort cache fills the disk."
	hiddenConstraintRule = "The quoddity standard for retries"
	hiddenRunWords       = "Where does the vellichor job write its log?"
	runCommentBody       = "The brindlecove note the run left on the issue."
	runCommentWord       = "brindlecove"
)

// trailingFindingText is the project finding alice writes under the project lens after her hidden
// one, so that her block stands between two project blocks of the same lens.
const trailingFindingText = "The rollback script was never rehearsed."

// findingAddressForm is the address of a finding of a lens, `dispatch://lens/<uuid>#b-<id>`, with
// the lens's uuid and the block id as its two groups.
var findingAddressForm = regexp.MustCompile(`^dispatch://lens/([0-9a-f-]{36})#b-([0-9a-f-]{36})$`)

// credentialReads is one credential of this test - a person's cookie or a person's personal token
// - and every answer it was given. Each answer is logged as it is read, so the two people's reads
// stand in one output, and kept, so the text of each hidden node can be looked for in every answer
// the second person read.
type credentialReads struct {
	t       *testing.T
	handler http.Handler
	name    string
	// login is the cookie's person, and token the personal token's plaintext; one of them is set.
	login, token string
	// actor is what the server resolves the credential to (resolvedBy), which every backlink
	// count of this test is read as.
	actor  model.Actor
	bodies []string
}

// resolvedBy is the actor resolver resolves the credential to, as every route resolves the caller
// of a request (server.optionalActor): a request carrying the credential as get sends it.
func (c *credentialReads) resolvedBy(resolver *server) model.Actor {
	c.t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/references", nil)
	c.credential(request)
	actor, _, err := resolver.optionalActor(request)
	if err != nil {
		c.t.Fatalf("%s: resolve the credential: %v", c.name, err)
	}
	return actor
}

// credential sets on request the header that carries the credential: the personal token as a
// bearer, or the cookie's person.
func (c *credentialReads) credential(request *http.Request) {
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	} else {
		request.Header.Set("X-Dispatch-User", c.login)
	}
}

// serve answers request with the credential, logging the answer and keeping its body.
func (c *credentialReads) serve(request *http.Request) *httptest.ResponseRecorder {
	c.t.Helper()
	c.credential(request)
	response := httptest.NewRecorder()
	c.handler.ServeHTTP(response, request)
	read := request.Method + " " + request.URL.String()
	if wantsDisplayTrees(request) {
		read += " with " + displayTreesHeader + ": 1"
	}
	c.t.Logf("%s: %s: status=%d body=%s", c.name, read, response.Code, response.Body.String())
	c.bodies = append(c.bodies, response.Body.String())
	return response
}

// get reads target with the credential.
func (c *credentialReads) get(target string) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.serve(httptest.NewRequest(http.MethodGet, target, nil))
}

// ok reads target with the credential, failing on anything but 200.
func (c *credentialReads) ok(target string) *httptest.ResponseRecorder {
	c.t.Helper()
	response := c.get(target)
	if response.Code != http.StatusOK {
		c.t.Fatalf("%s: GET %s answers %d %s, want 200", c.name, target, response.Code, response.Body.String())
	}
	return response
}

// references is GET /api/v1/references of address in one direction, `to` or `from`.
func (c *credentialReads) references(direction, address string) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.get("/api/v1/references?" + direction + "=" + url.QueryEscape(address))
}

// edges is the edges of address in one direction (edgeEnds), failing on anything but 200.
func (c *credentialReads) edges(direction, address string) []string {
	c.t.Helper()
	response := c.references(direction, address)
	if response.Code != http.StatusOK {
		c.t.Fatalf("%s: ?%s=%s answers %d %s, want 200", c.name, direction, address, response.Code, response.Body.String())
	}
	return edgeEnds(decodeBody[model.GraphReferences](c.t, response).Edges)
}

// lenses is the lens read of on (GET /api/v1/lenses?on=), failing on anything but 200.
func (c *credentialReads) lenses(on string) nodeLenses {
	c.t.Helper()
	return decodeBody[nodeLenses](c.t, c.ok("/api/v1/lenses?on="+url.QueryEscape(on)))
}

// node is the node GET /api/v1/references?to= answers at address, failing on anything but 200.
func (c *credentialReads) node(address string) model.GraphNode {
	c.t.Helper()
	response := c.references("to", address)
	if response.Code != http.StatusOK {
		c.t.Fatalf("%s: the read of %s at its address answers %d %s, want 200", c.name, address, response.Code, response.Body.String())
	}
	return decodeBody[model.GraphReferences](c.t, response).Node
}

// targets is what GET /api/v1/reference-targets answers for addresses, one member each in their
// order, failing on anything but 200 or on a member count that is not theirs. With trees it is
// read as the dashboard reads it, with `Dispatch-Trees: 1`, so that each member of kind block the
// read answers the credential carries its text_tree.
func (c *credentialReads) targets(trees bool, addresses ...string) []model.ReferenceTarget {
	c.t.Helper()
	target := referenceTargetsOf(addresses...)
	request := httptest.NewRequest(http.MethodGet, target, nil)
	if trees {
		request.Header.Set(displayTreesHeader, "1")
	}
	response := c.serve(request)
	if response.Code != http.StatusOK {
		c.t.Fatalf("%s: GET %s (trees %t) answers %d %s, want 200", c.name, target, trees, response.Code, response.Body.String())
	}
	answered := decodeBody[model.ReferenceTargets](c.t, response).Targets
	if len(answered) != len(addresses) {
		c.t.Fatalf("%s: the reference targets of %v (trees %t) answer %d members, want one each", c.name, addresses, trees, len(answered))
	}
	return answered
}

// countedNode is a node whose backlink count the test keeps: what it is called, and its kind and
// id as the count keys it.
type countedNode struct{ name, kind, id string }

// keptPage is what one person reads of the spec's nodes, kept to compare before and after alice's
// writes: the constraint list by address, the lens read of the spec and the project lens's own
// lens read (lensReadLines), the references of the spec, the requirement and the project lens in
// each direction (edgeEnds), and the backlink count of each counted node.
type keptPage struct {
	constraints                 []string
	specLines, projectLensLines []string
	references                  map[string][]string
	counts                      map[string]int
}

// requireSamePage fails on each read of got that answers otherwise than the same read of want,
// what naming the two pages.
func requireSamePage(t *testing.T, what string, got, want keptPage) {
	t.Helper()
	if !slices.Equal(got.constraints, want.constraints) {
		t.Errorf("%s: the constraint list is %v, against %v", what, got.constraints, want.constraints)
	}
	for name, lines := range map[string][2][]string{
		"the spec":         {got.specLines, want.specLines},
		"the project lens": {got.projectLensLines, want.projectLensLines},
	} {
		if !slices.Equal(lines[0], lines[1]) {
			t.Errorf("%s: the lens read of %s answers\n%s\nagainst\n%s", what, name, strings.Join(lines[0], "\n"), strings.Join(lines[1], "\n"))
		}
	}
	for target, ends := range want.references {
		if !slices.Equal(got.references[target], ends) {
			t.Errorf("%s: %s answers the edges %v, against %v", what, target, got.references[target], ends)
		}
	}
	for name, count := range want.counts {
		if got.counts[name] != count {
			t.Errorf("%s: %d backlinks of %s, against %d", what, got.counts[name], name, count)
		}
	}
}

// lensReadLines names everything a lens read answers that a reader could count or tell one node
// from another by: each lens by address with its kind, its place, its state and visibility and the
// lenses at either end of its triggers; each finding by address with its rank and text; each risk
// by address with its heading and checks; and each chain by its lenses and where it stopped.
func lensReadLines(read nodeLenses) []string {
	lines := []string{"on " + read.On.Kind + " " + read.On.Address + " " + read.On.State + "/" + read.On.Visibility}
	lens := func(prefix string, held lensRead) {
		kind, on := "none", "none"
		if held.Kind != nil {
			kind = held.Kind.ID
		}
		if held.On != nil {
			on = *held.On
		}
		lines = append(lines, fmt.Sprintf("%slens %s kind=%s on=%s %s/%s triggered_by=%v triggered=%v",
			prefix, held.Address, kind, on, held.State, held.Visibility, held.TriggeredBy, held.Triggered))
		for _, finding := range held.Findings {
			lines = append(lines, fmt.Sprintf("%s  finding %s rank=%d %s/%s %q", prefix, finding.Address, finding.Rank,
				finding.State, finding.Visibility, finding.Text))
		}
		for _, risk := range held.Risks {
			lines = append(lines, fmt.Sprintf("%s  risk %s %s/%s %q prevented_by=%v", prefix, risk.Address, risk.State,
				risk.Visibility, risk.Heading, risk.PreventedBy))
		}
	}
	if read.Lens != nil {
		lens("itself: ", *read.Lens)
	}
	for _, held := range read.Lenses {
		lens("", held)
	}
	for _, chain := range read.Chains {
		stopped := "none"
		if chain.StoppedAt != nil {
			stopped = *chain.StoppedAt
		}
		lines = append(lines, fmt.Sprintf("chain %v stopped_at=%s", readAddresses(chain.Lenses), stopped))
	}
	return lines
}

// findingAddresses is the address of each finding read.
func findingAddresses(findings []lensFindingRead) []string {
	addresses := make([]string, len(findings))
	for index, finding := range findings {
		addresses[index] = finding.Address
	}
	return addresses
}

// riskAddresses is the address of each risk of each lens read, in the order the read answers them.
func riskAddresses(lenses []lensRead) []string {
	var addresses []string
	for _, lens := range lenses {
		for _, risk := range lens.Risks {
			addresses = append(addresses, risk.Address)
		}
	}
	return addresses
}

// commentIDs is the id of each comment the issue's comment list answers the credential.
func commentIDs(t *testing.T, reader *credentialReads, issue string) []string {
	t.Helper()
	var ids []string
	for _, comment := range decodeBody[[]model.Comment](t, reader.ok("/api/v1/issues/"+issue+"/comments")) {
		ids = append(ids, comment.ID)
	}
	return ids
}

// requireOnlyTheFiveKindsTakeAuthor holds that the store takes a node reserved for its author of
// the five registry kinds this test reads one node of, and of no other: every other kind the read
// registry holds is refused author by the one rule every node write asks
// (graphstore.CheckVisibilityAdmitted), naming that kind, and each of the five is admitted by it.
func requireOnlyTheFiveKindsTakeAuthor(t *testing.T) {
	t.Helper()
	five := []string{graphstore.KindConstraint, graphstore.KindInteraction, graphstore.KindLens, graphstore.KindLensBlock, graphstore.KindRisk}
	slices.Sort(five)
	t.Logf("the store takes a node reserved for its author of %v; the registry holds %v", graphstore.AuthorOnlyKinds(), refs.VisibilityKinds())
	if got := graphstore.AuthorOnlyKinds(); !slices.Equal(got, five) {
		t.Errorf("the store takes a node reserved for its author of %v, want exactly %v", got, five)
	}
	for _, kind := range refs.VisibilityKinds() {
		err := graphstore.CheckVisibilityAdmitted(kind, "a-node-of-"+kind, graphstore.VisibilityAuthor)
		var refused *graphstore.VisibilityNotAdmittedError
		switch takes := slices.Contains(five, kind); {
		case takes && err != nil:
			t.Errorf("a %s asked for author is refused (%v), want it taken", kind, err)
		case !takes && (!errors.As(err, &refused) || refused.Kind != kind):
			t.Errorf("a %s asked for author answers %v, want the refusal naming %s", kind, err, kind)
		}
	}
}

// TestAnAuthorOnlyNodeIsAnsweredToItsAuthorAndToNobodyElse is c-visibility-page
// of the verification document: one node of each of the five registry
// kinds that take author - a lens, a block of a lens's finding, a risk, a constraint and an
// interaction - is answered to its author and to nobody else, and no other kind takes author.
//
// Two people each read and write with a cookie of their own, and each holds a personal token with
// the person scope; every backlink count is read as the actor the server resolves the credential
// to (server.optionalActor). Alice's cookie writes the project nodes: a pre-mortem on the spec
// holding one finding, a risk under it, a second lens on the spec triggered by the pre-mortem, so
// the two make a chain, a constraint, and the requirement's constrained_by to that constraint.
// What each of the four credentials reads of them is kept, and is the same: the constraint list,
// the lens read of the spec and of the project lens, the references of the spec, the requirement
// and the project lens in both directions, and the backlink count of each of those nodes and of
// the project finding, risk and constraint. Alice's cookie then writes, each reserved for her and
// each carrying one word no other node carries, a pre-mortem on the spec triggered by the project
// lens, a risk under it, a constraint, and a finding under the project lens; points the
// requirement at her constraint; records a run of the command bar; and writes a comment on the
// issue under the run's header.
//
// Alice reads each of her nodes in its list beside the project node, her lens in the chain of the
// two project lenses, and each at its own address with visibility author. Her backlink count of
// the spec is one higher, her lens's part_of; her ?to= of the project lens holds her finding's
// part_of and her lens's triggered_by, and its count is two higher; her personal token reads all
// of it as her cookie does. Each of bob's credentials repeats every kept read and is answered what
// it was, the project lens and its chain of the two project lenses among it, so the same reads
// that move for alice stand still for him. Each of her five nodes at its address answers him what
// an address of its kind nobody holds answers, byte for byte, and no answer he read holds the text
// of any of them, beside the same text found in hers. Asked for trees, the batched reference read
// gives alice's two credentials her finding's text tree, the tree of every word of it, and answers
// bob's two the finding as the address nobody holds beside it, gone and with no tree. A second
// project finding written after her hidden one reads, to him, at the rank alice reads of it, the
// store's key of its part_of, so no gap is left where hers stands. Search answers no hit for any of
// the five words, to both, beside the comment's word found. Alice's personal token reads her four
// nodes of the kinds that compare the person, bob's reads none. Last, a comment asked for author is
// refused naming its kind, and the comment list holds no new comment.
func TestAnAuthorOnlyNodeIsAnsweredToItsAuthorAndToNobodyElse(t *testing.T) {
	requireOnlyTheFiveKindsTakeAuthor(t)

	rig := newReasoningRig(t)
	ctx := context.Background()
	const alice, bob = "alice", "bob"
	// What the rig's server resolves the credential of a request to (server.optionalActor): its
	// store, its people and its shared token, as newRegisteredTestServer boots it.
	resolver := directServer(Deps{Store: rig.database, Identity: headerIdentity(rig.database), AgentTokens: sharedAgentTokens(t, "agent-token")})
	reader := func(name, login, token string) *credentialReads {
		t.Helper()
		reads := &credentialReads{t: t, handler: rig.handler, name: name, login: login, token: token}
		reads.actor = reads.resolvedBy(resolver)
		return reads
	}
	aliceCookie, bobCookie := reader("alice's cookie", alice, ""), reader("bob's cookie", bob, "")
	aliceToken := reader("alice's personal token", "", personalTokenOwnedBy(t, rig.database, alice, model.ScopePerson))
	bobToken := reader("bob's personal token", "", personalTokenOwnedBy(t, rig.database, bob, model.ScopePerson))
	for _, resolved := range []struct {
		reads  *credentialReads
		person string
	}{{aliceCookie, alice}, {aliceToken, alice}, {bobCookie, bob}, {bobToken, bob}} {
		actor, cookie := resolved.reads.actor, resolved.reads.token == ""
		if cookie && (actor.Kind != "user" || actor.ID != resolved.person) ||
			!cookie && (actor.Owner == nil || *actor.Owner != resolved.person || actor.Credential == nil) {
			t.Fatalf("%s resolves as %+v, want a credential that proves %s", resolved.reads.name, actor, resolved.person)
		}
	}
	readers := []*credentialReads{aliceCookie, aliceToken, bobCookie, bobToken}
	created := func(name string, response *httptest.ResponseRecorder, status int) *httptest.ResponseRecorder {
		t.Helper()
		t.Logf("alice's cookie writes %s: status=%d body=%s", name, response.Code, response.Body.String())
		if response.Code != status {
			t.Fatalf("alice's cookie writes %s: status=%d body=%s, want %d", name, response.Code, response.Body.String(), status)
		}
		return response
	}

	// The project nodes, every one of them written by alice's cookie and none reserved for her.
	project := rig.invoke(rig.spec, graphstore.LensKindPreMortem, alice, map[string]any{
		"verdict": "The cutover holds if the index builds first.", "findings": []string{"The index build takes an hour."},
	})
	if len(project.Findings) != 1 || project.Visibility != graphstore.VisibilityProject {
		t.Fatalf("the project lens answers %+v, want one project finding", project)
	}
	projectFinding := project.Findings[0]
	projectRisk := decodeBody[riskWritten](t, created("the project risk", rig.post("/risks", map[string]any{
		"lens": project.Address, "heading": "The migration locks the table.", "early_warning": "Writes queue.",
		"prevention": "Build the index concurrently.",
	}, alice), http.StatusCreated))
	// The second project lens, triggered by the first, so the two make a chain every reader reads.
	chained := rig.invoke(rig.spec, "trade-off", alice, map[string]any{
		"verdict": "The index build fits in the maintenance window.", "triggered_by": project.Address,
	})
	if chained.Visibility != graphstore.VisibilityProject || chained.TriggeredBy == nil || *chained.TriggeredBy != project.Address {
		t.Fatalf("the second project lens answers %+v, want a project lens triggered by %s", chained, project.Address)
	}
	projectConstraint := decodeBody[constraintRead](t, created("the project constraint", rig.post("/constraints",
		map[string]any{"standard": "RFC 9110", "reference": "section 9.2.2"}, alice), http.StatusCreated))
	created("the requirement's constrained_by to the project constraint", rig.post(nodePath(rig.requirementNode, "pointers"), map[string]any{
		"kind": graphstore.TupleConstrainedBy, "action": pointerAppend, "to": projectConstraint.Address,
	}, alice), http.StatusOK)

	counted := []countedNode{
		{"the spec", graphstore.KindArtifact, rig.issue + "/spec"},
		{"the requirement", refs.BlockNodeKind, rig.requirementNode},
		{"the project lens", graphstore.KindLens, project.ID},
		{"the project finding", graphstore.KindLensBlock, projectFinding.ID},
		{"the project risk", graphstore.KindRisk, projectRisk.ID},
		{"the project constraint", graphstore.KindConstraint, projectConstraint.ID},
	}
	read := func(reads *credentialReads) keptPage {
		t.Helper()
		page := keptPage{references: map[string][]string{}, counts: map[string]int{}}
		for _, row := range decodeBody[issueConstraints](t, reads.ok("/api/v1/issues/"+rig.issue+"/constraints")).Constraints {
			page.constraints = append(page.constraints, row.Address)
		}
		page.specLines = lensReadLines(reads.lenses(rig.spec))
		page.projectLensLines = lensReadLines(reads.lenses(project.Address))
		for _, address := range []string{rig.spec, rig.requirement, project.Address} {
			for _, direction := range []string{"to", "from"} {
				page.references["?"+direction+"="+address] = reads.edges(direction, address)
			}
		}
		for _, node := range counted {
			counts, err := refs.BacklinkCounts(ctx, rig.database.Pool, reads.actor, node.kind, []string{node.id}, nil)
			if err != nil {
				t.Fatalf("%s: count the backlinks of %s: %v", reads.name, node.name, err)
			}
			page.counts[node.name] = counts[node.id]
		}
		t.Logf("%s: the constraint list %v; the backlink counts %v", reads.name, page.constraints, page.counts)
		return page
	}
	kept := map[*credentialReads]keptPage{}
	for _, reads := range readers {
		kept[reads] = read(reads)
	}
	// Before alice's writes every node is the project's, so each credential reads what her cookie
	// reads, the counts as each credential resolves among it.
	for _, reads := range readers[1:] {
		requireSamePage(t, reads.name+" before alice's writes, against alice's cookie", kept[reads], kept[aliceCookie])
	}

	// Alice's five author-only nodes, each written by her cookie, which proves her, and each
	// accepted.
	hiddenLens := rig.invoke(rig.spec, graphstore.LensKindPreMortem, alice, map[string]any{
		"verdict": hiddenLensVerdict, "triggered_by": project.Address, "visibility": graphstore.VisibilityAuthor,
	})
	t.Logf("alice's cookie writes her lens: %+v", hiddenLens)
	hiddenFinding := decodeBody[lensFinding](t, created("her finding", rig.post(nodePath(graphstore.KindLens+"/"+project.ID, "findings"),
		map[string]any{"text": hiddenFindingText, "visibility": graphstore.VisibilityAuthor}, alice), http.StatusCreated))
	hiddenRisk := decodeBody[riskWritten](t, created("her risk", rig.post("/risks", map[string]any{
		"lens": hiddenLens.Address, "heading": hiddenRiskHeading, "early_warning": "The disk alarm fires.",
		"prevention": "Cap the cache.", "visibility": graphstore.VisibilityAuthor,
	}, alice), http.StatusCreated))
	hiddenConstraint := decodeBody[constraintRead](t, created("her constraint", rig.post("/constraints", map[string]any{
		"standard": hiddenConstraintRule, "reference": "Her own notes, page two.", "visibility": graphstore.VisibilityAuthor,
	}, alice), http.StatusCreated))
	for name, visibility := range map[string]string{
		"her lens": hiddenLens.Visibility, "her finding": hiddenFinding.Visibility,
		"her risk": hiddenRisk.Visibility, "her constraint": hiddenConstraint.Visibility,
	} {
		if visibility != graphstore.VisibilityAuthor {
			t.Fatalf("%s answers visibility %q, want author", name, visibility)
		}
	}
	pointed := decodeBody[pointerWritten](t, created("the requirement's constrained_by to her constraint",
		rig.post(nodePath(rig.requirementNode, "pointers"), map[string]any{
			"kind": graphstore.TupleConstrainedBy, "action": pointerAppend, "to": hiddenConstraint.Address,
		}, alice), http.StatusOK))
	if pointed.To.Address != hiddenConstraint.Address || pointed.To.Visibility != graphstore.VisibilityAuthor {
		t.Fatalf("the requirement's second constrained_by answers the far end %+v, want her constraint at author", pointed.To)
	}
	run := decodeBody[askedRun](t, created("her run of the command bar", dispatchRequest(t, rig.handler, http.MethodPost,
		"/api/v1/going/requests", map[string]any{"words": hiddenRunWords}, alice), http.StatusCreated))
	runWindowAhead(t, rig.database, run.ID)
	comment := decodeBody[writtenRecord](t, created("a comment under her run's header", requestForRun(t, rig.handler, http.MethodPost,
		"/api/v1/issues/"+rig.issue+"/comments", map[string]any{"body": runCommentBody}, alice, run.ID), http.StatusCreated)).ID
	commentAddress := "dispatch://" + rig.issue + "/comment/" + comment
	interaction := "dispatch://" + graphstore.KindInteraction + "/" + run.ID

	nobodysBlock := uuid.NewString()
	hidden := []struct {
		name, address, kind, id, text, word string
		nobodys, nobodysID                  string
		// authorCompared is whether the kind's predicate compares the author a credential proved
		// (graphstore.AuthorIsSQL), which a person's personal token reads as that person's cookie
		// does; an interaction's compares the whole credential instead.
		authorCompared bool
	}{
		{name: "alice's lens", address: hiddenLens.Address, kind: graphstore.KindLens, id: hiddenLens.ID,
			text: hiddenLensVerdict, word: "zephyrine", authorCompared: true},
		{name: "alice's finding", address: hiddenFinding.Address, kind: graphstore.KindLensBlock, id: hiddenFinding.BlockID,
			text: hiddenFindingText, word: "thistlemoor", authorCompared: true,
			nobodys: project.Address + "#b-" + nobodysBlock, nobodysID: nobodysBlock},
		{name: "alice's risk", address: hiddenRisk.Address, kind: graphstore.KindRisk, id: hiddenRisk.ID,
			text: hiddenRiskHeading, word: "marblewort", authorCompared: true},
		{name: "alice's constraint", address: hiddenConstraint.Address, kind: graphstore.KindConstraint, id: hiddenConstraint.ID,
			text: hiddenConstraintRule, word: "quoddity", authorCompared: true},
		{name: "alice's interaction", address: interaction, kind: graphstore.KindInteraction, id: run.ID,
			text: hiddenRunWords, word: "vellichor"},
	}
	for index := range hidden {
		if hidden[index].nobodys == "" {
			hidden[index].nobodysID = uuid.NewString()
			hidden[index].nobodys = strings.ReplaceAll(hidden[index].address, hidden[index].id, hidden[index].nobodysID)
		}
		// One node of the store holds each word, in an attribute of its row or in a value of it.
		var holders int
		if err := rig.database.Pool.QueryRow(ctx, `
			select count(distinct (kind, id)) from (
				select kind, id from graph_nodes where attributes::text like '%' || $1 || '%'
				union all
				select from_kind, from_id from graph_tuples where to_kind = $2 and to_id like '%' || $1 || '%'
			) holding`, hidden[index].word, graphstore.KindValue).Scan(&holders); err != nil {
			t.Fatalf("count the nodes holding %q: %v", hidden[index].word, err)
		}
		if holders != 1 {
			t.Fatalf("%d nodes of the store hold the word %q of %s, want that one alone", holders, hidden[index].word, hidden[index].name)
		}
	}

	// Alice: each of her nodes is in the list of its kind beside the project node, and her personal
	// token reads every kept read as her cookie does, the counts as it resolves among them.
	alicePage := read(aliceCookie)
	requireSamePage(t, aliceToken.name+" after alice's writes, against her cookie", read(aliceToken), alicePage)
	if want := []string{projectConstraint.Address, hiddenConstraint.Address}; !slices.Equal(alicePage.constraints, want) {
		t.Errorf("alice's constraint list is %v, want the project constraint and then hers, %v", alicePage.constraints, want)
	}
	aliceSpec := aliceCookie.lenses(rig.spec)
	if got, want := readAddresses(aliceSpec.Lenses), []string{project.Address, chained.Address, hiddenLens.Address}; !slices.Equal(got, want) {
		t.Errorf("alice's lens read of the spec holds the lenses %v, want hers after the two project lenses, %v", got, want)
	}
	if her := lensNamed(t, aliceSpec.Lenses, hiddenLens.Address); her.Visibility != graphstore.VisibilityAuthor ||
		her.Verdict == nil || *her.Verdict != hiddenLensVerdict || !slices.Equal(her.TriggeredBy, []string{project.Address}) {
		t.Errorf("alice's lens read answers her lens as %+v, want it at author, with her verdict, triggered by %s", her, project.Address)
	}
	if got, want := riskAddresses(aliceSpec.Lenses), []string{projectRisk.Address, hiddenRisk.Address}; !slices.Equal(got, want) {
		t.Errorf("alice's lens read of the spec holds the risks %v, want hers beside the project risk, %v", got, want)
	}
	aliceProject := lensNamed(t, aliceSpec.Lenses, project.Address)
	if len(aliceSpec.Chains) != 1 || aliceSpec.Chains[0].StoppedAt != nil ||
		!slices.Equal(readAddresses(aliceSpec.Chains[0].Lenses), []string{project.Address, chained.Address, hiddenLens.Address}) ||
		!slices.Equal(aliceProject.Triggered, []string{chained.Address, hiddenLens.Address}) {
		t.Errorf("alice's lens read answers the chains %+v and the project lens triggering %v, want the one chain of the two project lenses and hers, whole",
			aliceSpec.Chains, aliceProject.Triggered)
	}
	aliceFindings := aliceCookie.lenses(project.Address).Lens
	if aliceFindings == nil {
		t.Fatalf("alice's lens read of the project lens answers no lens")
	}
	for name, findings := range map[string][]lensFindingRead{
		"of the spec": aliceProject.Findings, "of the project lens": aliceFindings.Findings,
	} {
		if got, want := findingAddresses(findings), []string{projectFinding.Address, hiddenFinding.Address}; !slices.Equal(got, want) {
			t.Errorf("alice's lens read %s holds the project lens's findings %v, want the hidden block beside the project block, %v", name, got, want)
			continue
		}
		held := findings[1]
		match := findingAddressForm.FindStringSubmatch(held.Address)
		if match == nil || match[1] != project.ID || held.Visibility != graphstore.VisibilityAuthor || held.Text != hiddenFindingText {
			t.Errorf("alice's lens read %s answers the hidden block %+v, want it at dispatch://lens/%s#b-<id>, at author", name, held, project.ID)
			continue
		}
		if id, err := uuid.Parse(match[2]); err != nil || id.Version() != 4 || match[2] != hiddenFinding.BlockID {
			t.Errorf("the hidden block's id %q is no random block id (%v), or not the %q its write answered", match[2], err, hiddenFinding.BlockID)
		}
		if held.Rank <= findings[0].Rank {
			t.Errorf("alice's lens read %s ranks the hidden block %d and the project block %d, want it after", name, held.Rank, findings[0].Rank)
		}
	}

	// Alice reads each of her five at its own address, at visibility author.
	for _, node := range hidden {
		answered := aliceCookie.node(node.address)
		if answered.Kind != node.kind || answered.Ref != node.address || answered.Visibility != graphstore.VisibilityAuthor {
			t.Errorf("alice's cookie reads %s at its address as %+v, want a %s at %s with visibility author", node.name, answered, node.kind, node.address)
		}
	}
	if from := alicePage.references["?from="+rig.requirement]; !slices.Contains(from, "out constrained_by "+hiddenConstraint.Address) ||
		!slices.Contains(from, "out constrained_by "+projectConstraint.Address) {
		t.Errorf("alice's references of the requirement are %v, want its constrained_by to her constraint beside the project one", from)
	}
	if from := aliceCookie.edges("from", commentAddress); !slices.Contains(from, "out "+graphstore.TupleChangedByRun+" "+interaction) {
		t.Errorf("alice's references of the comment are %v, want its changed_by_run to %s", from, interaction)
	}
	if alicePage.counts["the spec"] != kept[aliceCookie].counts["the spec"]+1 {
		t.Errorf("alice counts %d backlinks of the spec, and %d before her writes: want one more, her lens's part_of",
			alicePage.counts["the spec"], kept[aliceCookie].counts["the spec"])
	}
	// A document's ?to= lists its part_of edges only when asked for by kind (parseReferencesQuery),
	// so the list behind that count is read so: hers holds her lens's part_of beside the project
	// lens's, and his the project lens's alone.
	for _, reads := range []*credentialReads{aliceCookie, bobCookie} {
		response := reads.get("/api/v1/references?to=" + url.QueryEscape(rig.spec) + "&kind=" + graphstore.TuplePartOf)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: the part_of edges into the spec answer %d %s, want 200", reads.name, response.Code, response.Body.String())
		}
		ends := edgeEnds(decodeBody[model.GraphReferences](t, response).Edges)
		holdsHers := slices.Contains(ends, "in part_of "+hiddenLens.Address)
		if !slices.Contains(ends, "in part_of "+project.Address) || holdsHers != (reads == aliceCookie) {
			t.Errorf("%s reads the part_of edges into the spec %v, want the project lens's, and alice's lens's for alice alone", reads.name, ends)
		}
	}
	// The same reads move for their author: alice's ?to= of the project lens holds her finding's
	// part_of and her lens's triggered_by, which no read held before her writes, and her count of
	// the project lens is two higher. Bob's of both stand still below.
	hiddenInto := []string{"in " + graphstore.TuplePartOf + " " + hiddenFinding.Address, "in " + graphstore.TupleTriggeredBy + " " + hiddenLens.Address}
	for _, end := range hiddenInto {
		if into := alicePage.references["?to="+project.Address]; !slices.Contains(into, end) ||
			slices.Contains(kept[aliceCookie].references["?to="+project.Address], end) {
			t.Errorf("alice's ?to=%s answers the edges %v, and answered %v before her writes: want %q in it now and not then",
				project.Address, into, kept[aliceCookie].references["?to="+project.Address], end)
		}
	}
	if alicePage.counts["the project lens"] != kept[aliceCookie].counts["the project lens"]+len(hiddenInto) {
		t.Errorf("alice counts %d backlinks of the project lens, and %d before her writes: want %d more, her finding's part_of and her lens's triggered_by",
			alicePage.counts["the project lens"], kept[aliceCookie].counts["the project lens"], len(hiddenInto))
	}

	// Bob: every read either of his credentials kept answers what it answered, so no list holds an
	// author-only node and no count moved, the reads alice's two tuples into the project lens
	// moved for her among them. His lenses read of the spec holds the project lens and its chain,
	// the two project lenses, whole, and not her lens.
	var bobPage keptPage
	for _, reads := range []*credentialReads{bobCookie, bobToken} {
		page := read(reads)
		if reads == bobCookie {
			bobPage = page
		}
		requireSamePage(t, reads.name+" after alice's writes, against what it kept", page, kept[reads])
		for _, end := range hiddenInto {
			if into := page.references["?to="+project.Address]; slices.Contains(into, end) {
				t.Errorf("%s's ?to=%s answers the edges %v, holding %q of alice's", reads.name, project.Address, into, end)
			}
		}
		spec := reads.lenses(rig.spec)
		if got, want := readAddresses(spec.Lenses), []string{project.Address, chained.Address}; !slices.Equal(got, want) {
			t.Errorf("%s's lens read of the spec holds the lenses %v, want the two project lenses, %v", reads.name, got, want)
		}
		if len(spec.Chains) != 1 || spec.Chains[0].StoppedAt != nil ||
			!slices.Equal(readAddresses(spec.Chains[0].Lenses), []string{project.Address, chained.Address}) {
			t.Errorf("%s's lens read of the spec answers the chains %+v, want the one chain of the two project lenses, whole, without hers",
				reads.name, spec.Chains)
		}
		held := lensNamed(t, spec.Lenses, project.Address)
		if !slices.Equal(held.Triggered, []string{chained.Address}) || !slices.Equal(riskAddresses(spec.Lenses), []string{projectRisk.Address}) {
			t.Errorf("%s's lens read answers the project lens triggering %v and the risks %v, want the second project lens alone and the project risk alone",
				reads.name, held.Triggered, riskAddresses(spec.Lenses))
		}
	}

	// No gap a count could read. Alice's cookie writes a second project finding under the project
	// lens after her hidden one, so her block stands between two project blocks. Each of bob's
	// reads of the project lens's findings holds the two project blocks alone, each at the rank
	// alice reads of it, which is the store's own key of that block's part_of: a rank counted over
	// every finding would leave him a gap where hers stands, and one counted over his alone would
	// read otherwise than hers. Each block's id is a random one.
	trailing := decodeBody[lensFinding](t, created("a second project finding, after hers", rig.post(nodePath(graphstore.KindLens+"/"+project.ID, "findings"),
		map[string]any{"text": trailingFindingText}, alice), http.StatusCreated))
	if trailing.Visibility != graphstore.VisibilityProject {
		t.Fatalf("the second project finding answers %+v, want it project-visible", trailing)
	}
	keyed := map[string]int64{}
	for _, finding := range []lensFinding{projectFinding, hiddenFinding, trailing} {
		var key int64
		if err := rig.database.Pool.QueryRow(ctx, `
			select max(seq) from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
		`, refs.BlockNodeKind, finding.ID, graphstore.TuplePartOf, graphstore.KindLens, project.ID).Scan(&key); err != nil {
			t.Fatalf("read the seq of the part_of of %s: %v", finding.Address, err)
		}
		keyed[finding.Address] = key
	}
	findingsOf := func(reads *credentialReads) map[string][]lensFindingRead {
		t.Helper()
		onLens := reads.lenses(project.Address).Lens
		if onLens == nil {
			t.Fatalf("%s's lens read of the project lens answers no lens", reads.name)
		}
		return map[string][]lensFindingRead{
			"of the spec":         lensNamed(t, reads.lenses(rig.spec).Lenses, project.Address).Findings,
			"of the project lens": onLens.Findings,
		}
	}
	aliceRanks := map[string]int64{}
	for name, findings := range findingsOf(aliceCookie) {
		if got, want := findingAddresses(findings), []string{projectFinding.Address, hiddenFinding.Address, trailing.Address}; !slices.Equal(got, want) {
			t.Errorf("alice's lens read %s holds the project lens's findings %v, want her hidden block between the two project blocks, %v", name, got, want)
			continue
		}
		for _, finding := range findings {
			if finding.Rank != keyed[finding.Address] {
				t.Errorf("alice's lens read %s ranks %s %d, and the store keys its part_of %d", name, finding.Address, finding.Rank, keyed[finding.Address])
			}
			aliceRanks[finding.Address] = finding.Rank
		}
	}
	for _, reads := range []*credentialReads{bobCookie, bobToken} {
		for name, findings := range findingsOf(reads) {
			if got, want := findingAddresses(findings), []string{projectFinding.Address, trailing.Address}; !slices.Equal(got, want) {
				t.Errorf("%s's lens read %s holds the project lens's findings %v, want the two project blocks and not the hidden one, %v", reads.name, name, got, want)
				continue
			}
			for _, finding := range findings {
				if finding.Rank != keyed[finding.Address] || finding.Rank != aliceRanks[finding.Address] {
					t.Errorf("%s's lens read %s ranks %s %d, alice's ranks it %d and the store keys its part_of %d: want the one key, which leaves no gap where her block stands",
						reads.name, name, finding.Address, finding.Rank, aliceRanks[finding.Address], keyed[finding.Address])
				}
				match := findingAddressForm.FindStringSubmatch(finding.Address)
				if match == nil {
					t.Errorf("%s's lens read %s answers a project block at %q, no finding's address", reads.name, name, finding.Address)
				} else if id, err := uuid.Parse(match[2]); err != nil || id.Version() != 4 {
					t.Errorf("%s's lens read %s answers the project block's id %q, no random block id (%v)", reads.name, name, match[2], err)
				}
			}
		}
	}
	if from := bobPage.references["?from="+rig.requirement]; slices.Contains(from, "out constrained_by "+hiddenConstraint.Address) {
		t.Errorf("bob's references of the requirement hold its constrained_by to alice's constraint: %v", from)
	}
	for _, direction := range []string{"to", "from"} {
		response := bobCookie.references(direction, commentAddress)
		if response.Code != http.StatusOK {
			t.Fatalf("bob's ?%s= of the comment answers %d %s, want 200", direction, response.Code, response.Body.String())
		}
		for _, edge := range decodeBody[model.GraphReferences](t, response).Edges {
			if edge.Kind == graphstore.TupleChangedByRun || edge.Node.Kind == graphstore.KindInteraction {
				t.Errorf("bob's ?%s= of the comment answers %+v, want no changed_by_run and no interaction", direction, edge)
			}
		}
	}

	// Every project node is in each credential's answers, at its own address and project-visible.
	for _, reads := range readers {
		for _, node := range []struct{ name, address string }{
			{"the project lens", project.Address}, {"the second project lens", chained.Address},
			{"the project finding", projectFinding.Address}, {"the second project finding", trailing.Address},
			{"the project risk", projectRisk.Address}, {"the project constraint", projectConstraint.Address},
		} {
			if answered := reads.node(node.address); answered.Ref != node.address || answered.Visibility != graphstore.VisibilityProject {
				t.Errorf("%s reads %s at its address as %+v, want it project-visible at %s", reads.name, node.name, answered, node.address)
			}
		}
	}
	for login, page := range map[string]keptPage{alice: alicePage, bob: bobPage} {
		if !slices.Contains(page.constraints, projectConstraint.Address) {
			t.Errorf("%s's constraint list %v does not hold the project constraint", login, page.constraints)
		}
	}

	// Each of alice's five answers bob, at its address, what an address of its kind nobody holds
	// answers him; and her personal token reads her four nodes of the kinds that compare the
	// author a credential proved, at visibility author, where bob's reads none of them.
	for _, node := range hidden {
		for _, direction := range []string{"to", "from"} {
			requireNobodysAnswer(t, fmt.Sprintf("%s: GET /api/v1/references?%s=%s, %s holding %q", bobCookie.name, direction, node.address, node.name, node.text),
				bobCookie.references(direction, node.address), bobCookie.references(direction, node.nobodys), node.id, node.nobodysID)
		}
		if !node.authorCompared {
			continue
		}
		if answered := aliceToken.node(node.address); answered.Kind != node.kind || answered.Ref != node.address ||
			answered.Visibility != graphstore.VisibilityAuthor {
			t.Errorf("%s reads %s at its address as %+v, want a %s at %s with visibility author", aliceToken.name, node.name, answered, node.kind, node.address)
		}
		for _, direction := range []string{"to", "from"} {
			requireNobodysAnswer(t, fmt.Sprintf("%s: GET /api/v1/references?%s=%s, %s holding %q", bobToken.name, direction, node.address, node.name, node.text),
				bobToken.references(direction, node.address), bobToken.references(direction, node.nobodys), node.id, node.nobodysID)
		}
	}

	// The batched reference read answers bob each of alice's five as it answers him an address of
	// its kind nobody holds: gone, named nothing. Alice's cookie, which wrote them, is answered each
	// of them, and her personal token each of the four whose kind compares the author it proved.
	for _, node := range hidden {
		for _, reads := range []*credentialReads{aliceCookie, aliceToken, bobCookie, bobToken} {
			answered := reads.targets(false, node.address, node.nobodys)
			held, nobodys := answered[0], answered[1]
			switch {
			case reads == aliceCookie || reads == aliceToken && node.authorCompared:
				if held.Gone || held.Kind != nobodys.Kind {
					t.Errorf("%s: GET /api/v1/reference-targets answers %s, %s, as %+v, want it answered under the kind %q", reads.name, node.address, node.name, held, nobodys.Kind)
				}
			case reads == bobCookie || reads == bobToken:
				if !held.Gone || held.Name != "" || held.Kind != nobodys.Kind || held.Ambiguous ||
					!nobodys.Gone || nobodys.Name != "" || nobodys.Ambiguous {
					t.Errorf("%s: GET /api/v1/reference-targets answers %s, %s holding %q, as %+v, and %s, which nobody holds, as %+v: want the two alike, gone and naming nothing",
						reads.name, node.address, node.name, node.text, held, node.nobodys, nobodys)
				}
			}
		}
	}
	// Asked for trees, the same read draws alice's finding whole for its author: her cookie and her
	// personal token, which proves her, are given its text_tree, the tree of every word of it. Each
	// of bob's credentials is answered it as the finding nobody holds beside it, gone, named
	// nothing and with no tree, and no answer he read holds its words (below).
	for _, node := range hidden {
		if node.kind != graphstore.KindLensBlock {
			continue
		}
		for _, reads := range []*credentialReads{aliceCookie, aliceToken, bobCookie, bobToken} {
			answered := reads.targets(true, node.address, node.nobodys)
			held, nobodys := answered[0], answered[1]
			if !nobodys.Gone || nobodys.Name != "" || len(nobodys.TextTree) != 0 {
				t.Errorf("%s: GET /api/v1/reference-targets with trees answers %s, which nobody holds, as %+v, want it gone with no tree",
					reads.name, node.nobodys, nobodys)
			}
			if reads == aliceCookie || reads == aliceToken {
				if held.Gone || held.Kind != "block" || len(held.TextTree) == 0 {
					t.Errorf("%s: GET /api/v1/reference-targets with trees answers %s, %s, as %+v, want a block that is not gone with its text_tree",
						reads.name, node.address, node.name, held)
				} else if got := treeText(t, json.RawMessage(held.TextTree)); got != node.text {
					t.Errorf("%s: the text_tree of %s, %s, holds the text %q, want %q", reads.name, node.address, node.name, got, node.text)
				}
				continue
			}
			if !held.Gone || held.Name != "" || len(held.TextTree) != 0 || held.Kind != nobodys.Kind || held.Ambiguous {
				t.Errorf("%s: GET /api/v1/reference-targets with trees answers %s, %s holding %q, as %+v, and %s, which nobody holds, as %+v: want the two alike, gone, naming nothing and with no tree",
					reads.name, node.address, node.name, node.text, held, node.nobodys, nobodys)
			}
		}
	}

	// Search holds no leg for these kinds: each word answers no hit and a total of zero to both,
	// beside the comment's own word, which it finds for both.
	for _, reads := range []*credentialReads{aliceCookie, bobCookie} {
		control := decodeBody[model.SearchResponse](t, reads.ok("/api/v1/search?q="+url.QueryEscape(runCommentWord)))
		if control.Total == 0 || !slices.ContainsFunc(control.Results, func(result model.SearchResult) bool {
			return result.Kind == graphstore.KindComment && result.ID == comment
		}) {
			t.Errorf("%s's search for %q answers %+v, want the comment %s: a search that finds nothing proves nothing", reads.name, runCommentWord, control, comment)
		}
		for _, node := range hidden {
			found := decodeBody[model.SearchResponse](t, reads.ok("/api/v1/search?q="+url.QueryEscape(node.word)))
			if found.Total != 0 || len(found.Results) != 0 {
				t.Errorf("%s's search for %q, the word of %s, answers total %d and %+v, want no hit and zero", reads.name, node.word, node.name, found.Total, found.Results)
			}
		}
	}

	// No answer bob read holds the text or the word of any of the five, beside the same text and
	// word found in alice's answers.
	for _, node := range hidden {
		for _, text := range []string{node.text, node.word} {
			if !slices.ContainsFunc(aliceCookie.bodies, func(body string) bool { return strings.Contains(body, text) }) {
				t.Errorf("no answer alice's cookie read holds %q of %s, so finding none in bob's proves nothing", text, node.name)
			}
			for _, reads := range []*credentialReads{bobCookie, bobToken} {
				for _, body := range reads.bodies {
					if strings.Contains(body, text) {
						t.Errorf("%s read %q of %s in %s", reads.name, text, node.name, body)
					}
				}
			}
		}
	}
	t.Logf("searched %d answers of bob's cookie and %d of his personal token for the text of alice's five, beside %d answers of hers",
		len(bobCookie.bodies), len(bobToken.bodies), len(aliceCookie.bodies))

	// Last, a comment asked for author is refused naming its kind, and stores nothing.
	listed := map[string][]string{alice: commentIDs(t, aliceCookie, rig.issue), bob: commentIDs(t, bobCookie, rig.issue)}
	refused := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments",
		map[string]any{"body": "A remark she keeps.", "visibility": graphstore.VisibilityAuthor}, alice)
	requireRefused(t, "alice's comment asked for author", refused, http.StatusBadRequest, "VISIBILITY_NOT_ADMITTED",
		"the node kind "+graphstore.KindComment+" takes no node reserved for its author")
	for login, reads := range map[string]*credentialReads{alice: aliceCookie, bob: bobCookie} {
		if after := commentIDs(t, reads, rig.issue); !slices.Equal(after, listed[login]) {
			t.Errorf("%s's comment list holds %v after the refusal, and held %v", login, after, listed[login])
		}
	}
}
