package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// referencesAt reads one address through GET /api/v1/references in one direction: `to`, the
// edges into it, or `from`, the edges out of it.
func referencesAt(t *testing.T, handler http.Handler, direction, address, login string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodGet, "/api/v1/references?"+direction+"="+url.QueryEscape(address), nil, login)
}

// edgeEnds names each edge of a references answer as `<direction> <kind> <far end>`, the far end
// by its address where the node answers one and by `<kind>/<id>` where it answers none, sorted.
func edgeEnds(edges []model.GraphEdge) []string {
	ends := make([]string, 0, len(edges))
	for _, edge := range edges {
		end := edge.Node.Ref
		if end == "" {
			end = edge.Node.Kind + "/" + edge.Node.ID
		}
		ends = append(ends, edge.Direction+" "+edge.Kind+" "+end)
	}
	slices.Sort(ends)
	return ends
}

// requireNobodysAnswer fails unless hidden, the answer to a node the reader may not see, is
// nobodys, the answer to an address of the same form nobody wrote, byte for byte once the id
// nobody holds is read as the hidden node's: the same status, the same Content-Type and the same
// body. It reports whether hidden answered so.
func requireNobodysAnswer(t *testing.T, name string, hidden, nobodys *httptest.ResponseRecorder, hiddenID, nobodysID string) bool {
	t.Helper()
	t.Logf("%s: hidden status=%d body=%s; nobody's status=%d body=%s", name, hidden.Code, hidden.Body.String(), nobodys.Code, nobodys.Body.String())
	held := true
	if hidden.Code != http.StatusNotFound {
		t.Errorf("%s: the hidden node answers %d %s, want 404", name, hidden.Code, hidden.Body.String())
		held = false
	}
	if hidden.Code != nobodys.Code {
		t.Errorf("%s: the hidden node answers %d and an address nobody holds %d", name, hidden.Code, nobodys.Code)
		held = false
	}
	if got, want := hidden.Header().Get("Content-Type"), nobodys.Header().Get("Content-Type"); got != want {
		t.Errorf("%s: the hidden node answers Content-Type %q and an address nobody holds %q", name, got, want)
		held = false
	}
	if !strings.Contains(nobodys.Body.String(), nobodysID) {
		t.Fatalf("%s: the answer to an address nobody holds %s does not name %s", name, nobodys.Body.String(), nobodysID)
	}
	if got, want := hidden.Body.String(), strings.ReplaceAll(nobodys.Body.String(), nobodysID, hiddenID); got != want {
		t.Errorf("%s: the hidden node answers\n%s\nand an address nobody holds, its id read as the hidden one's,\n%s", name, got, want)
		held = false
	}
	return held
}

// lensReader is one reader of a lens for requireReadsAsALensNobodyWrote: the reader's login, which
// a failure names, the requests it sends, and the lens it reads.
type lensReader struct {
	login string
	as    sender
	lens  lensInvoked
}

// requireReadsAsALensNobodyWrote holds each reader's GET /api/v1/lenses?on= and GET
// /api/v1/references?to= of its lens, by the lens's dispatch:// address, to what the same read of a
// lens nobody wrote answers (requireNobodysAnswer): 404 NOT_FOUND, byte for byte once the id is
// substituted, since the reference read names such a lens by its id alone. failure is the words of
// a read that answers otherwise, given the read, its status and body, and the reader's login.
func requireReadsAsALensNobodyWrote(t *testing.T, failure func(read, status, login string) string, readers ...lensReader) {
	t.Helper()
	nobodysID := uuid.NewString()
	nobodys := refs.StoreRef(graphstore.KindLens, nobodysID)
	for _, reader := range readers {
		for _, route := range []string{"/api/v1/lenses?on=", "/api/v1/references?to="} {
			read := "GET " + route + reader.lens.Address
			own := reader.as(http.MethodGet, route+url.QueryEscape(reader.lens.Address), nil)
			if !requireNobodysAnswer(t, reader.login+"'s "+read, own, reader.as(http.MethodGet, route+url.QueryEscape(nobodys), nil), reader.lens.ID, nobodysID) {
				t.Error(failure(read, fmt.Sprintf("%d %s", own.Code, own.Body.String()), reader.login))
			}
		}
	}
}

// TestTheReferencesReadAnswersEachNewKindAtItsAddress: GET /api/v1/references reads a lens, a
// finding of it, a risk, a constraint and a lens kind at their own addresses, ?to= and ?from=.
// Each answers its author 200 with the node's kind, address and visibility and the tuples into
// and out of it: a finding's part_of to its lens and authored_by to its author, and the part_of
// of a lens invoked on it, which names the finding as a lens_block, so the finding's backlink
// count, its ?to= list and the lens read of it agree for each reader. A finding is no check and
// no lens, so a prevented_by at it and a risk under it are refused. A node reserved for its
// author answers a second person, in both directions, byte for byte what an address of the same
// form nobody wrote answers once that id is read as the hidden one's, and it is absent from the
// edges of the nodes it stands on. An address of each kind nobody wrote is a 404, never a 500.
func TestTheReferencesReadAnswersEachNewKindAtItsAddress(t *testing.T) {
	t.Parallel()
	rig := newReasoningRig(t)
	created := func(name string, response *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		t.Helper()
		if response.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s", name, response.Code, response.Body.String())
		}
		return response
	}

	lens := rig.invoke(rig.spec, "trade-off", "alice", map[string]any{"findings": []string{"A second region doubles the cost."}})
	finding := lens.Findings[0]
	hiddenFinding := decodeBody[lensFinding](t, created("add alice's own finding", rig.post(nodePath("lens/"+lens.ID, "findings"),
		map[string]any{"text": "A note she keeps.", "visibility": graphstore.VisibilityAuthor}, "alice")))
	if hiddenFinding.Visibility != graphstore.VisibilityAuthor {
		t.Fatalf("alice's own finding answers visibility %q, want author", hiddenFinding.Visibility)
	}
	premortem := rig.invoke(rig.requirement, graphstore.LensKindPreMortem, "alice", nil)
	risk := decodeBody[riskWritten](t, created("write a risk", rig.post("/risks", map[string]any{
		"lens": premortem.Address, "heading": "The migration locks the table.", "early_warning": "Writes queue.", "prevention": "Build the index concurrently.",
	}, "alice")))
	hiddenRisk := decodeBody[riskWritten](t, created("write alice's own risk", rig.post("/risks", map[string]any{
		"lens": premortem.Address, "heading": "She doubts the rollback.", "early_warning": "None yet.", "prevention": "Rehearse it.",
		"visibility": graphstore.VisibilityAuthor,
	}, "alice")))
	constraint := decodeBody[constraintRead](t, created("write a constraint", rig.post("/constraints",
		map[string]any{"standard": "RFC 9110", "reference": "section 9.2.2"}, "alice")))
	hiddenConstraint := decodeBody[constraintRead](t, created("write alice's own constraint", rig.post("/constraints",
		map[string]any{"standard": "A rule she keeps", "reference": "her notes", "visibility": graphstore.VisibilityAuthor}, "alice")))
	if pointed := rig.post(nodePath(rig.requirementNode, "pointers"), map[string]any{
		"kind": graphstore.TupleConstrainedBy, "action": pointerAppend, "to": constraint.Address,
	}, "alice"); pointed.Code != http.StatusOK {
		t.Fatalf("point the requirement at the constraint: status=%d body=%s", pointed.Code, pointed.Body.String())
	}
	// A lens on the finding and one on the risk, so a tuple stands into each, and a lens alice
	// keeps to herself on the finding beside the first.
	onFinding := rig.invoke(finding.Address, "botec", "alice", nil)
	onRisk := rig.invoke(risk.Address, "botec", "alice", nil)
	hiddenLens := rig.invoke(finding.Address, "botec", "alice", map[string]any{"visibility": graphstore.VisibilityAuthor})
	const tradeOff = "dispatch://lens_kind/trade-off"
	alice := "person/alice"

	for _, read := range []struct {
		name, address, kind, visibility string
		to, from                        []string
	}{
		{
			name: "the lens", address: lens.Address, kind: graphstore.KindLens, visibility: graphstore.VisibilityProject,
			to: []string{"in part_of " + finding.Address, "in part_of " + hiddenFinding.Address},
			from: []string{
				"out authored_by " + alice, "out has_kind " + tradeOff, "out part_of " + rig.spec,
			},
		},
		{
			name: "the finding", address: finding.Address, kind: graphstore.KindLensBlock, visibility: graphstore.VisibilityProject,
			to:   []string{"in part_of " + onFinding.Address, "in part_of " + hiddenLens.Address},
			from: []string{"out authored_by " + alice, "out part_of " + lens.Address},
		},
		{
			name: "alice's own finding", address: hiddenFinding.Address, kind: graphstore.KindLensBlock, visibility: graphstore.VisibilityAuthor,
			to:   []string{},
			from: []string{"out authored_by " + alice, "out part_of " + lens.Address},
		},
		{
			name: "the lens on the finding", address: onFinding.Address, kind: graphstore.KindLens, visibility: graphstore.VisibilityProject,
			to:   []string{},
			from: []string{"out authored_by " + alice, "out has_kind dispatch://lens_kind/botec", "out part_of " + finding.Address},
		},
		{
			name: "the risk", address: risk.Address, kind: graphstore.KindRisk, visibility: graphstore.VisibilityProject,
			to:   []string{"in part_of " + onRisk.Address},
			from: []string{"out authored_by " + alice, "out part_of " + premortem.Address},
		},
		{
			name: "the constraint", address: constraint.Address, kind: graphstore.KindConstraint, visibility: graphstore.VisibilityProject,
			to:   []string{"in constrained_by " + rig.requirement},
			from: []string{"out authored_by " + alice, "out part_of dispatch://" + rig.issue},
		},
	} {
		for _, direction := range []struct {
			name string
			want []string
		}{{"to", read.to}, {"from", read.from}} {
			response := referencesAt(t, rig.handler, direction.name, read.address, "alice")
			t.Logf("alice's ?%s=%s: status=%d body=%s", direction.name, read.address, response.Code, response.Body.String())
			if response.Code != http.StatusOK {
				t.Errorf("alice's ?%s= of %s answers %d %s, want 200", direction.name, read.name, response.Code, response.Body.String())
				continue
			}
			answer := decodeBody[model.GraphReferences](t, response)
			if answer.Node.Kind != read.kind || answer.Node.Ref != read.address || answer.Node.Visibility != read.visibility ||
				answer.Node.State != graphstore.StateLive {
				t.Errorf("alice's ?%s= of %s answers the node %+v, want kind %s at %s, live at %s",
					direction.name, read.name, answer.Node, read.kind, read.address, read.visibility)
			}
			if got, want := edgeEnds(answer.Edges), slices.Sorted(slices.Values(direction.want)); !slices.Equal(got, want) {
				t.Errorf("alice's ?%s= of %s answers the edges %v, want %v", direction.name, read.name, got, want)
			}
		}
	}

	// A lens kind is read at its address too, with the lenses of its kind.
	kindRead := referencesAt(t, rig.handler, "to", tradeOff, "alice")
	t.Logf("alice's ?to=%s: status=%d body=%s", tradeOff, kindRead.Code, kindRead.Body.String())
	if kindRead.Code != http.StatusOK {
		t.Fatalf("alice's ?to= of the lens kind answers %d %s, want 200", kindRead.Code, kindRead.Body.String())
	}
	kindAnswer := decodeBody[model.GraphReferences](t, kindRead)
	if kindAnswer.Node.Kind != graphstore.KindLensKind || kindAnswer.Node.Ref != tradeOff || kindAnswer.Node.Visibility != graphstore.VisibilityProject {
		t.Errorf("the lens kind answers the node %+v, want a project lens_kind at %s", kindAnswer.Node, tradeOff)
	}
	if got := edgeEnds(kindAnswer.Edges); !slices.Contains(got, "in has_kind "+lens.Address) {
		t.Errorf("the lens kind answers the edges %v, want the has_kind of %s among them", got, lens.Address)
	}
	kindFrom := referencesAt(t, rig.handler, "from", tradeOff, "alice")
	t.Logf("alice's ?from=%s: status=%d body=%s", tradeOff, kindFrom.Code, kindFrom.Body.String())
	if kindFrom.Code != http.StatusOK {
		t.Errorf("alice's ?from= of the lens kind answers %d %s, want 200", kindFrom.Code, kindFrom.Body.String())
	} else if node := decodeBody[model.GraphReferences](t, kindFrom).Node; node.Kind != graphstore.KindLensKind || node.Ref != tradeOff {
		t.Errorf("alice's ?from= of the lens kind answers the node %+v", node)
	}

	// Bob reads the project nodes, and none of alice's own among their edges.
	bobsLens := referencesAt(t, rig.handler, "to", lens.Address, "bob")
	if bobsLens.Code != http.StatusOK {
		t.Fatalf("bob's ?to= of the lens answers %d %s", bobsLens.Code, bobsLens.Body.String())
	}
	if got, want := edgeEnds(decodeBody[model.GraphReferences](t, bobsLens).Edges), []string{"in part_of " + finding.Address}; !slices.Equal(got, want) {
		t.Errorf("bob's ?to= of the lens answers the edges %v, want %v", got, want)
	}
	bobsPremortem := referencesAt(t, rig.handler, "to", premortem.Address, "bob")
	if got := edgeEnds(decodeBody[model.GraphReferences](t, bobsPremortem).Edges); !slices.Equal(got, []string{"in part_of " + risk.Address}) {
		t.Errorf("bob's ?to= of the pre-mortem answers the edges %v, want the project risk alone", got)
	}
	bobsFinding := referencesAt(t, rig.handler, "to", finding.Address, "bob")
	if bobsFinding.Code != http.StatusOK {
		t.Fatalf("bob's ?to= of the project finding answers %d %s, want 200", bobsFinding.Code, bobsFinding.Body.String())
	}
	if got, want := edgeEnds(decodeBody[model.GraphReferences](t, bobsFinding).Edges), []string{"in part_of " + onFinding.Address}; !slices.Equal(got, want) {
		t.Errorf("bob's ?to= of the project finding answers the edges %v, want %v", got, want)
	}
	if bobsFrom := referencesAt(t, rig.handler, "from", finding.Address, "bob"); bobsFrom.Code != http.StatusOK {
		t.Errorf("bob's ?from= of the project finding answers %d %s, want 200", bobsFrom.Code, bobsFrom.Body.String())
	}

	// A lens on the finding names it as the graph addresses it, so the finding's backlink count,
	// its ?to= list and the lens read of it each hold the lens, alice's own for alice alone.
	if got := rig.current(graphstore.KindLens, onFinding.ID, graphstore.TuplePartOf); !slices.Equal(got, []string{graphstore.KindLensBlock + "/" + finding.ID}) {
		t.Errorf("the lens on the finding stands on %v, want the finding as a lens_block", got)
	}
	for login, want := range map[string][]string{"alice": {onFinding.Address, hiddenLens.Address}, "bob": {onFinding.Address}} {
		counts, err := refs.BacklinkCounts(t.Context(), rig.database.Pool, model.Actor{Kind: "user", ID: login},
			graphstore.KindLensBlock, []string{finding.ID}, nil)
		if err != nil {
			t.Fatalf("count the finding's backlinks as %s: %v", login, err)
		}
		listed := edgeEnds(decodeBody[model.GraphReferences](t, referencesAt(t, rig.handler, "to", finding.Address, login)).Edges)
		if counts[finding.ID] != len(want) || len(listed) != len(want) {
			t.Errorf("%s counts %d backlinks of the finding and lists %v, want the lenses %v", login, counts[finding.ID], listed, want)
		}
		if got, wanted := slices.Sorted(slices.Values(readAddresses(rig.lenses(finding.Address, login).Lenses))), slices.Sorted(slices.Values(want)); !slices.Equal(got, wanted) {
			t.Errorf("%s's lens read of the finding answers %v, want %v", login, got, wanted)
		}
	}
	// A finding is no check and no lens, so neither a prevented_by at it nor a risk under it is
	// taken, and neither stores anything.
	risks, preventions := rig.counts(graphstore.KindRisk, graphstore.TuplePreventedBy)
	requireRefused(t, "a prevented_by at the finding", rig.post(nodePath("risk/"+risk.ID, "pointers"), map[string]any{
		"kind": graphstore.TuplePreventedBy, "action": pointerAppend, "to": finding.Address,
	}, "alice"), http.StatusBadRequest, "INVALID_POINTER", graphstore.TuplePreventedBy, graphstore.KindRisk, graphstore.KindLensBlock)
	requireRefused(t, "a risk under the finding", rig.post("/risks", map[string]any{
		"lens": finding.Address, "heading": "No lens.", "early_warning": "None.", "prevention": "None.",
	}, "alice"), http.StatusBadRequest, "INVALID_POINTER", graphstore.TuplePartOf, graphstore.KindRisk, graphstore.KindLensBlock)
	if afterRisks, afterPreventions := rig.counts(graphstore.KindRisk, graphstore.TuplePreventedBy); afterRisks != risks || afterPreventions != preventions {
		t.Errorf("the store holds %d risks and %d prevented_by tuples after the refusals, and held %d and %d before",
			afterRisks, afterPreventions, risks, preventions)
	}

	// Each node alice keeps to herself answers bob what an address of its form nobody wrote
	// answers, in both directions.
	nobodysBlock := uuid.NewString()
	for _, hidden := range []struct{ name, address, id, nobodys, nobodysID string }{
		{
			name: "alice's own finding", address: hiddenFinding.Address, id: hiddenFinding.BlockID,
			nobodys: lens.Address + "#b-" + nobodysBlock, nobodysID: nobodysBlock,
		},
		{name: "alice's own lens", address: hiddenLens.Address, id: hiddenLens.ID},
		{name: "alice's own risk", address: hiddenRisk.Address, id: hiddenRisk.ID},
		{name: "alice's own constraint", address: hiddenConstraint.Address, id: hiddenConstraint.ID},
	} {
		if hidden.nobodys == "" {
			hidden.nobodysID = uuid.NewString()
			hidden.nobodys = strings.ReplaceAll(hidden.address, hidden.id, hidden.nobodysID)
		}
		for _, direction := range []string{"to", "from"} {
			requireNobodysAnswer(t, "bob's ?"+direction+"= of "+hidden.name,
				referencesAt(t, rig.handler, direction, hidden.address, "bob"),
				referencesAt(t, rig.handler, direction, hidden.nobodys, "bob"), hidden.id, hidden.nobodysID)
		}
	}

	// An address of each kind nobody wrote is a 404 to its author too.
	for _, address := range []string{
		"dispatch://lens/" + uuid.NewString(),
		"dispatch://lens/" + uuid.NewString() + "#b-" + uuid.NewString(),
		lens.Address + "#b-" + uuid.NewString(),
		"dispatch://risk/" + uuid.NewString(),
		"dispatch://constraint/" + uuid.NewString(),
		"dispatch://lens_kind/no-such-kind",
	} {
		for _, direction := range []string{"to", "from"} {
			if response := referencesAt(t, rig.handler, direction, address, "alice"); response.Code != http.StatusNotFound {
				t.Errorf("alice's ?%s=%s answers %d %s, want 404", direction, address, response.Code, response.Body.String())
			}
		}
	}
}
