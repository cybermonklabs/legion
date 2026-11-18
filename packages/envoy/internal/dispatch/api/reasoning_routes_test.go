package api

import (
	"context"
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
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// reasoningRig is one issue whose spec opens with a paragraph and holds a requirement, r1, with a
// Verification document holding one check, k1, and a second, k2: the nodes the routes of the
// reasoning kinds point at. Its people write through their cookies, which prove them.
type reasoningRig struct {
	t        *testing.T
	handler  http.Handler
	database *store.Store
	issue    string
	// spec, requirement, paragraph and check are dispatch:// addresses; requirementNode is the
	// requirement's store id, which a tuple names it by.
	spec, requirement, paragraph, check, secondCheck string
	requirementNode                                  string
}

func newReasoningRig(t *testing.T) reasoningRig {
	t.Helper()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "RSN", "Reasoned work",
		"An opening paragraph.\n\n:::requirement{#r1}\nEvery write is idempotent.\n:::\n")
	rig := reasoningRig{t: t, handler: handler, database: database, issue: issue.Key}
	rig.spec = "dispatch://" + issue.Key + "/spec"
	rig.requirement = refs.BlockRef(rig.spec, "r1")
	rig.paragraph = refs.BlockRef(rig.spec, firstBlockOf(t, handler, issue.PrimaryArtifactID))
	if err := database.Pool.QueryRow(context.Background(),
		`select ref_key || '#r1' from artifacts where id = $1::uuid`, issue.PrimaryArtifactID).Scan(&rig.requirementNode); err != nil {
		t.Fatalf("read the requirement's node id: %v", err)
	}
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		checkServing("k1", rig.requirement)+"\n"+checkServing("k2", rig.requirement))
	document := "dispatch://" + issue.Key + "/artifact/" + verification.Artifact.Slug
	rig.check, rig.secondCheck = refs.BlockRef(document, "k1"), refs.BlockRef(document, "k2")
	return rig
}

// post sends body to the issue's route path as login, a person's cookie.
func (r reasoningRig) post(path string, body map[string]any, login string) *httptest.ResponseRecorder {
	r.t.Helper()
	return dispatchRequest(r.t, r.handler, http.MethodPost, "/api/v1/issues/"+r.issue+path, body, login)
}

// nodePath is the route path of a node's own routes, its store address one escaped segment.
func nodePath(address, route string) string {
	return "/nodes/" + url.PathEscape(address) + "/" + route
}

// invoke writes a lens of kind on on as login and answers it, failing on anything but 201.
func (r reasoningRig) invoke(on, kind, login string, extra map[string]any) lensInvoked {
	r.t.Helper()
	body := map[string]any{"on": on, "kind": kind}
	for key, value := range extra {
		body[key] = value
	}
	response := r.post("/lenses", body, login)
	if response.Code != http.StatusCreated {
		r.t.Fatalf("invoke a %s lens on %s as %s: status=%d body=%s", kind, on, login, response.Code, response.Body.String())
	}
	return decodeBody[lensInvoked](r.t, response)
}

// counts is how many nodes of kind and tuples of tupleKind the store holds.
func (r reasoningRig) counts(kind, tupleKind string) (nodes, tuples int) {
	r.t.Helper()
	if err := r.database.Pool.QueryRow(context.Background(), `
		select (select count(*) from graph_nodes where kind = $1), (select count(*) from graph_tuples where kind = $2)
	`, kind, tupleKind).Scan(&nodes, &tuples); err != nil {
		r.t.Fatalf("count the %s nodes and the %s tuples: %v", kind, tupleKind, err)
	}
	return nodes, tuples
}

// current is the far ends of the asserted tuples of kind out of the node the store keys by
// fromKind and fromID, each as `<kind>/<id>`, sorted.
func (r reasoningRig) current(fromKind, fromID, kind string) []string {
	r.t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), r.database.Pool, fromKind, fromID, []string{kind})
	if err != nil {
		r.t.Fatalf("read the %s tuples out of %s %s: %v", kind, fromKind, fromID, err)
	}
	ends := make([]string, 0, len(held))
	for _, tuple := range held {
		ends = append(ends, tuple.ToKind+"/"+tuple.ToID)
	}
	slices.Sort(ends)
	return ends
}

// requireRefused fails unless response is status with code, its words holding says.
func requireRefused(t *testing.T, name string, response *httptest.ResponseRecorder, status int, code, says string) refusalAnswer {
	t.Helper()
	t.Logf("%s: status=%d body=%s", name, response.Code, response.Body.String())
	if response.Code != status {
		t.Fatalf("%s: status=%d body=%s, want %d %s", name, response.Code, response.Body.String(), status, code)
	}
	refusal := decodeBody[refusalAnswer](t, response)
	if refusal.Code != code || !strings.Contains(refusal.Error, says) {
		t.Fatalf("%s is refused %+v, want %s saying %q", name, refusal, code, says)
	}
	return refusal
}

// TestTheReasoningRoutesWriteEachNodeAndAnswerItsShape drives each route of the reasoning kinds
// once as a person whose cookie proves her: a lens invoked on the spec with two findings, a
// finding added later, a risk under a pre-mortem prevented by a check, a constraint, a pointer
// from the requirement to it, and the issue's constraint list. Each answer names the node at its
// address with its author, state and visibility, and the store holds what the answer says.
func TestTheReasoningRoutesWriteEachNodeAndAnswerItsShape(t *testing.T) {
	rig := newReasoningRig(t)

	lens := rig.invoke(rig.spec, "trade-off", "alice", map[string]any{
		"verdict": "Keep one region.", "findings": []string{"A second region doubles the cost.", "Failover takes minutes."},
	})
	t.Logf("the lens: %+v", lens)
	if lens.Address != "dispatch://lens/"+lens.ID || lens.Kind != "trade-off" || lens.On != rig.spec ||
		lens.Verdict == nil || *lens.Verdict != "Keep one region." || lens.TriggeredBy != nil {
		t.Fatalf("the lens answers %+v", lens)
	}
	if lens.Author == nil || lens.Author.Kind != graphstore.KindPerson || lens.Author.ID != "alice" ||
		lens.State != graphstore.StateLive || lens.Visibility != graphstore.VisibilityProject {
		t.Fatalf("the lens answers author %+v, state %q and visibility %q", lens.Author, lens.State, lens.Visibility)
	}
	if len(lens.Findings) != 2 {
		t.Fatalf("the lens answers %d findings, want 2", len(lens.Findings))
	}
	for index, finding := range lens.Findings {
		if _, err := uuid.Parse(finding.BlockID); err != nil {
			t.Errorf("finding %d's block id %q is no random block id: %v", index, finding.BlockID, err)
		}
		if finding.Address != lens.Address+"#b-"+finding.BlockID || finding.ID != "lens/"+lens.ID+"#"+finding.BlockID ||
			finding.Lens != lens.Address || finding.Visibility != graphstore.VisibilityProject {
			t.Errorf("finding %d answers %+v", index, finding)
		}
	}
	if got := rig.current(graphstore.KindLens, lens.ID, graphstore.TupleHasKind); !slices.Equal(got, []string{"lens_kind/trade-off"}) {
		t.Errorf("the lens holds has_kind %v, want lens_kind/trade-off", got)
	}
	if got := rig.current(graphstore.KindLens, lens.ID, graphstore.TuplePartOf); !slices.Equal(got, []string{"artifact/" + rig.issue + "/spec"}) {
		t.Errorf("the lens stands on %v, want the spec by its ref_key", got)
	}
	// The findings rank in the order they were written: the seq of their part_of tuples.
	ranked, err := readStrings(context.Background(), rig.database.Pool, `
		select from_id from graph_tuples where kind = $1 and to_kind = $2 and to_id = $3 and from_kind = 'block' order by seq
	`, graphstore.TuplePartOf, graphstore.KindLens, lens.ID)
	if err != nil {
		t.Fatalf("read the findings' part_of tuples: %v", err)
	}
	if want := []string{lens.Findings[0].ID, lens.Findings[1].ID}; !slices.Equal(ranked, want) {
		t.Errorf("the findings rank %v, want %v", ranked, want)
	}

	// A finding added later stands after them, at the lens's visibility.
	added := rig.post(nodePath("lens/"+lens.ID, "findings"), map[string]any{"text": "The bill doubles too."}, "alice")
	if added.Code != http.StatusCreated {
		t.Fatalf("add a finding: status=%d body=%s", added.Code, added.Body.String())
	}
	finding := decodeBody[lensFinding](t, added)
	if finding.Lens != lens.Address || finding.Text != "The bill doubles too." || finding.Visibility != graphstore.VisibilityProject ||
		finding.Author == nil || finding.Author.ID != "alice" || !strings.HasPrefix(finding.Address, lens.Address+"#b-") {
		t.Fatalf("the added finding answers %+v", finding)
	}
	// A finding added through the lens's dispatch:// address lands under the same lens.
	if again := rig.post(nodePath(lens.Address, "findings"), map[string]any{"text": "And the on-call load."}, "alice"); again.Code != http.StatusCreated {
		t.Fatalf("add a finding by the lens's address: status=%d body=%s", again.Code, again.Body.String())
	}

	// A risk under a pre-mortem, prevented by a check of the Verification document.
	premortem := rig.invoke(rig.requirement, graphstore.LensKindPreMortem, "alice", nil)
	risked := rig.post("/risks", map[string]any{
		"lens": premortem.Address, "heading": "The migration locks the table.",
		"early_warning": "Writes queue.", "prevention": "Build the index concurrently.", "prevented_by": rig.check,
	}, "alice")
	if risked.Code != http.StatusCreated {
		t.Fatalf("write a risk: status=%d body=%s", risked.Code, risked.Body.String())
	}
	risk := decodeBody[riskWritten](t, risked)
	if risk.Address != "dispatch://risk/"+risk.ID || risk.Lens != premortem.Address || risk.Heading != "The migration locks the table." ||
		risk.EarlyWarning != "Writes queue." || risk.Prevention != "Build the index concurrently." ||
		!slices.Equal(risk.PreventedBy, []string{rig.check}) || risk.Visibility != graphstore.VisibilityProject {
		t.Fatalf("the risk answers %+v", risk)
	}
	node, found, err := graphstore.ReadNode(context.Background(), rig.database.Pool, graphstore.KindRisk, risk.ID)
	if err != nil || !found || node.Attributes[graphstore.AttributeEarlyWarning] != "Writes queue." ||
		node.Attributes[graphstore.AttributePrevention] != "Build the index concurrently." {
		t.Fatalf("the risk's row is %+v (found %t, %v), want its early warning and prevention", node, found, err)
	}

	// A constraint, and the requirement's pointer to it.
	written := rig.post("/constraints", map[string]any{"standard": "RFC 9110", "reference": "section 9.2.2"}, "alice")
	if written.Code != http.StatusCreated {
		t.Fatalf("write a constraint: status=%d body=%s", written.Code, written.Body.String())
	}
	constraint := decodeBody[constraintRead](t, written)
	if constraint.Address != "dispatch://constraint/"+constraint.ID || constraint.Standard == nil || *constraint.Standard != "RFC 9110" ||
		constraint.Reference == nil || *constraint.Reference != "section 9.2.2" || constraint.Measurement != nil ||
		constraint.Author == nil || constraint.Author.ID != "alice" {
		t.Fatalf("the constraint answers %+v", constraint)
	}
	pointed := rig.post(nodePath(rig.requirementNode, "pointers"), map[string]any{
		"kind": graphstore.TupleConstrainedBy, "action": pointerAppend, "to": constraint.Address,
	}, "alice")
	if pointed.Code != http.StatusOK {
		t.Fatalf("point the requirement at the constraint: status=%d body=%s", pointed.Code, pointed.Body.String())
	}
	pointer := decodeBody[pointerWritten](t, pointed)
	if pointer.Node.Address != rig.requirement || pointer.To.Address != constraint.Address || pointer.From != nil ||
		pointer.Kind != graphstore.TupleConstrainedBy || pointer.Action != pointerAppend {
		t.Fatalf("the pointer answers %+v", pointer)
	}
	if got := rig.current(refs.BlockNodeKind, rig.requirementNode, graphstore.TupleConstrainedBy); !slices.Equal(got, []string{"constraint/" + constraint.ID}) {
		t.Errorf("the requirement holds constrained_by %v, want the constraint", got)
	}
	measured := rig.post("/constraints", map[string]any{"measurement": "p99 write latency", "value": "41 ms", "taken_where": "staging, 2026-11-15"}, "alice")
	if measured.Code != http.StatusCreated {
		t.Fatalf("write a measurement: status=%d body=%s", measured.Code, measured.Body.String())
	}

	listed := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+rig.issue+"/constraints", nil, "bob")
	if listed.Code != http.StatusOK {
		t.Fatalf("list the constraints: status=%d body=%s", listed.Code, listed.Body.String())
	}
	list := decodeBody[issueConstraints](t, listed)
	t.Logf("bob's list: %s", listed.Body.String())
	if list.Issue != rig.issue || len(list.Constraints) != 2 || list.Constraints[0].ID != constraint.ID {
		t.Fatalf("the list answers %+v, want the standard and then the measurement", list)
	}
	second := list.Constraints[1]
	if second.Measurement == nil || *second.Measurement != "p99 write latency" || second.Value == nil || *second.Value != "41 ms" ||
		second.TakenWhere == nil || second.Standard != nil || second.Author == nil || second.Author.ID != "alice" ||
		second.State != graphstore.StateLive || second.Visibility != graphstore.VisibilityProject {
		t.Fatalf("the measurement lists as %+v", second)
	}
}

// TestTheReasoningRoutesRefuseAndStoreNothing holds each refusal of the reasoning routes to its
// status and code, and to a store that holds after it what it held before.
func TestTheReasoningRoutesRefuseAndStoreNothing(t *testing.T) {
	rig := newReasoningRig(t)

	before, _ := rig.counts(graphstore.KindLens, graphstore.TupleHasKind)
	requireRefused(t, "a lens with no kind", rig.post("/lenses", map[string]any{"on": rig.spec}, "alice"),
		http.StatusBadRequest, "LENS_KIND_REQUIRED", "exactly one kind")
	requireRefused(t, "a lens of a kind nobody holds", rig.post("/lenses", map[string]any{"on": rig.spec, "kind": "astrology"}, "alice"),
		http.StatusBadRequest, "INVALID_POINTER", `"astrology"`)
	requireRefused(t, "author on the shared bearer", sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issue+"/lenses",
		map[string]any{"on": rig.spec, "kind": "trade-off", "visibility": graphstore.VisibilityAuthor, "actor": sessionActor()}),
		http.StatusForbidden, "AUTHOR_NAMES_NOBODY", "names nobody")
	if after, _ := rig.counts(graphstore.KindLens, graphstore.TupleHasKind); after != before {
		t.Fatalf("the store holds %d lenses after the refusals, and held %d before", after, before)
	}

	// On the shared bearer a lens is project-visible and names the session its body names as
	// its writer (spec v46 R12), though that bearer proves no author.
	shared := sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issue+"/lenses",
		map[string]any{"on": rig.spec, "kind": "inversion", "actor": sessionActor()})
	if shared.Code != http.StatusCreated {
		t.Fatalf("a lens on the shared bearer: status=%d body=%s", shared.Code, shared.Body.String())
	}
	inversion := decodeBody[lensInvoked](t, shared)
	if inversion.Author == nil || inversion.Author.Kind != graphstore.KindSession || inversion.Author.ID != sessionActor()["id"] ||
		inversion.Visibility != graphstore.VisibilityProject {
		t.Fatalf("the shared bearer's lens answers author %+v and visibility %q, want the session its body names and project", inversion.Author, inversion.Visibility)
	}

	// A second has_kind is the store's 409, and the lens holds the kind it held.
	_, hasKinds := rig.counts(graphstore.KindLens, graphstore.TupleHasKind)
	requireRefused(t, "a second has_kind", rig.post(nodePath("lens/"+inversion.ID, "pointers"),
		map[string]any{"kind": graphstore.TupleHasKind, "action": pointerAppend, "to": "botec"}, "alice"),
		http.StatusConflict, "POINTER_ALREADY_ASSERTED", "exactly one kind")
	if _, after := rig.counts(graphstore.KindLens, graphstore.TupleHasKind); after != hasKinds {
		t.Errorf("the store holds %d has_kind tuples after the refusal, and held %d before", after, hasKinds)
	}
	if got := rig.current(graphstore.KindLens, inversion.ID, graphstore.TupleHasKind); !slices.Equal(got, []string{"lens_kind/inversion"}) {
		t.Errorf("the lens holds has_kind %v after the refusal", got)
	}

	// A risk under an inversion is the store's 400, and no risk is stored.
	risks, _ := rig.counts(graphstore.KindRisk, graphstore.TuplePreventedBy)
	requireRefused(t, "a risk under an inversion", rig.post("/risks", map[string]any{
		"lens": inversion.Address, "heading": "A failure.", "early_warning": "A sign.", "prevention": "A guard.",
	}, "alice"), http.StatusBadRequest, "INVALID_POINTER", "a risk belongs under a lens of kind pre-mortem")
	if after, _ := rig.counts(graphstore.KindRisk, graphstore.TuplePreventedBy); after != risks {
		t.Errorf("the store holds %d risks after the refusal, and held %d before", after, risks)
	}

	// A constrained_by at a paragraph is the store's 400, and no tuple is stored.
	_, constrained := rig.counts(graphstore.KindConstraint, graphstore.TupleConstrainedBy)
	requireRefused(t, "a constrained_by at a paragraph", rig.post(nodePath(rig.requirementNode, "pointers"),
		map[string]any{"kind": graphstore.TupleConstrainedBy, "action": pointerAppend, "to": rig.paragraph}, "alice"),
		http.StatusBadRequest, "INVALID_POINTER", "constrained_by")
	if _, after := rig.counts(graphstore.KindConstraint, graphstore.TupleConstrainedBy); after != constrained {
		t.Errorf("the store holds %d constrained_by tuples after the refusal, and held %d before", after, constrained)
	}

	// A body of neither constraint shape, a part_of, a finding under a node that is no lens.
	requireRefused(t, "a constraint of neither shape", rig.post("/constraints", map[string]any{"standard": "RFC 9110", "value": "41 ms"}, "alice"),
		http.StatusBadRequest, "CONSTRAINT_INPUT", "both")
	requireRefused(t, "a part_of through the pointer route", rig.post(nodePath("lens/"+inversion.ID, "pointers"),
		map[string]any{"kind": graphstore.TuplePartOf, "action": pointerRetarget, "from": rig.spec, "to": rig.requirement}, "alice"),
		http.StatusBadRequest, "POINTER_INPUT", "part_of")
	requireRefused(t, "a finding under a block", rig.post(nodePath(rig.requirementNode, "findings"), map[string]any{"text": "Misplaced."}, "alice"),
		http.StatusBadRequest, "INVALID_POINTER", "a finding stands under a lens")
}

// TestAPointerRetargetMovesTheKindAndKeepsThePlace: a has_kind retarget of a lens that is no
// pre-mortem moves its kind and leaves its part_of as it was, and a retarget of a pre-mortem's
// kind away from pre-mortem is the same 409, body for body, whether a risk hangs under the lens
// or none does.
func TestAPointerRetargetMovesTheKindAndKeepsThePlace(t *testing.T) {
	rig := newReasoningRig(t)
	lens := rig.invoke(rig.spec, "trade-off", "alice", nil)
	place := rig.current(graphstore.KindLens, lens.ID, graphstore.TuplePartOf)

	moved := rig.post(nodePath("lens/"+lens.ID, "pointers"), map[string]any{
		"kind": graphstore.TupleHasKind, "action": pointerRetarget, "from": "trade-off", "to": "botec",
	}, "alice")
	if moved.Code != http.StatusOK {
		t.Fatalf("retarget the lens's kind: status=%d body=%s", moved.Code, moved.Body.String())
	}
	answer := decodeBody[pointerWritten](t, moved)
	if answer.From == nil || answer.From.Address != "dispatch://lens_kind/trade-off" || answer.To.Address != "dispatch://lens_kind/botec" {
		t.Fatalf("the retarget answers %+v", answer)
	}
	if got := rig.current(graphstore.KindLens, lens.ID, graphstore.TupleHasKind); !slices.Equal(got, []string{"lens_kind/botec"}) {
		t.Errorf("the lens holds has_kind %v after the retarget, want botec alone", got)
	}
	if got := rig.current(graphstore.KindLens, lens.ID, graphstore.TuplePartOf); !slices.Equal(got, place) {
		t.Errorf("the lens stands on %v after the retarget, and stood on %v before", got, place)
	}

	held := rig.invoke(rig.spec, graphstore.LensKindPreMortem, "alice", nil)
	if risked := rig.post("/risks", map[string]any{
		"lens": held.Address, "heading": "A failure.", "early_warning": "A sign.", "prevention": "A guard.",
	}, "alice"); risked.Code != http.StatusCreated {
		t.Fatalf("write a risk under the pre-mortem: status=%d body=%s", risked.Code, risked.Body.String())
	}
	empty := rig.invoke(rig.spec, graphstore.LensKindPreMortem, "alice", nil)
	var bodies []string
	for _, premortem := range []lensInvoked{held, empty} {
		refused := rig.post(nodePath("lens/"+premortem.ID, "pointers"), map[string]any{
			"kind": graphstore.TupleHasKind, "action": pointerRetarget, "from": graphstore.LensKindPreMortem, "to": "trade-off",
		}, "alice")
		requireRefused(t, "a pre-mortem's kind retargeted away", refused, http.StatusConflict, "POINTER_ALREADY_ASSERTED", "pre-mortem")
		bodies = append(bodies, refused.Body.String())
		if got := rig.current(graphstore.KindLens, premortem.ID, graphstore.TupleHasKind); !slices.Equal(got, []string{"lens_kind/" + graphstore.LensKindPreMortem}) {
			t.Errorf("the pre-mortem holds has_kind %v after the refusal", got)
		}
	}
	if bodies[0] != bodies[1] {
		t.Errorf("the refusal reads %s for the pre-mortem holding a risk and %s for the empty one", bodies[0], bodies[1])
	}
}

// TestAFarEndTheCallerMayNotSeeAnswersAsNoNode: a node reserved for its author is no end the
// second person may name, through any reasoning route: a lens on it, a finding under it, a
// pointer at it. Each is the 404 a node nobody holds answers, in the same words but the address,
// and nothing is stored. The constraint list holds the hidden constraint for its author alone.
func TestAFarEndTheCallerMayNotSeeAnswersAsNoNode(t *testing.T) {
	rig := newReasoningRig(t)
	hidden := rig.invoke(rig.spec, "trade-off", "alice", map[string]any{"visibility": graphstore.VisibilityAuthor})
	if hidden.Visibility != graphstore.VisibilityAuthor {
		t.Fatalf("alice's lens answers visibility %q, want author", hidden.Visibility)
	}
	private := rig.post("/constraints", map[string]any{"standard": "A rule she keeps", "reference": "her notes", "visibility": graphstore.VisibilityAuthor}, "alice")
	if private.Code != http.StatusCreated {
		t.Fatalf("write alice's constraint: status=%d body=%s", private.Code, private.Body.String())
	}
	constraint := decodeBody[constraintRead](t, private)
	if constraint.Visibility != graphstore.VisibilityAuthor {
		t.Fatalf("alice's constraint answers visibility %q, want author", constraint.Visibility)
	}
	public := rig.post("/constraints", map[string]any{"standard": "RFC 9110", "reference": "section 9.2.2"}, "bob")
	if public.Code != http.StatusCreated {
		t.Fatalf("write bob's constraint: status=%d body=%s", public.Code, public.Body.String())
	}
	nobodys := "dispatch://lens/" + uuid.NewString()
	notFound := func(address string) string { return fmt.Sprintf("no node %q under %s", address, rig.issue) }

	lenses, _ := rig.counts(graphstore.KindLens, graphstore.TupleHasKind)
	for _, address := range []string{hidden.Address, nobodys} {
		refused := requireRefused(t, "bob's lens on "+address, rig.post("/lenses", map[string]any{"on": address, "kind": "botec"}, "bob"),
			http.StatusNotFound, "NOT_FOUND", notFound(address))
		if refused.Error != notFound(address) {
			t.Errorf("bob's lens on %s is refused %q, want %q", address, refused.Error, notFound(address))
		}
		segment := strings.TrimPrefix(address, "dispatch://")
		requireRefused(t, "bob's finding under "+address, rig.post(nodePath(segment, "findings"), map[string]any{"text": "Mine."}, "bob"),
			http.StatusNotFound, "NOT_FOUND", notFound(segment))
		requireRefused(t, "bob's trigger by "+address, rig.post("/lenses", map[string]any{"on": rig.spec, "kind": "botec", "triggered_by": address}, "bob"),
			http.StatusNotFound, "NOT_FOUND", notFound(address))
	}
	if after, _ := rig.counts(graphstore.KindLens, graphstore.TupleHasKind); after != lenses {
		t.Fatalf("the store holds %d lenses after bob's refusals, and held %d before", after, lenses)
	}
	nobodysConstraint := "dispatch://constraint/" + uuid.NewString()
	for _, address := range []string{constraint.Address, nobodysConstraint} {
		requireRefused(t, "bob's pointer at "+address, rig.post(nodePath(rig.requirementNode, "pointers"),
			map[string]any{"kind": graphstore.TupleConstrainedBy, "action": pointerAppend, "to": address}, "bob"),
			http.StatusNotFound, "NOT_FOUND", notFound(address))
	}
	if got := rig.current(refs.BlockNodeKind, rig.requirementNode, graphstore.TupleConstrainedBy); len(got) != 0 {
		t.Errorf("the requirement holds constrained_by %v after bob's refusals", got)
	}

	ids := func(login string) map[string]string {
		t.Helper()
		listed := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+rig.issue+"/constraints", nil, login)
		if listed.Code != http.StatusOK {
			t.Fatalf("list the constraints as %s: status=%d body=%s", login, listed.Code, listed.Body.String())
		}
		visibility := map[string]string{}
		for _, row := range decodeBody[issueConstraints](t, listed).Constraints {
			visibility[row.ID] = row.Visibility
		}
		return visibility
	}
	bobsList, alicesList := ids("bob"), ids("alice")
	if _, listed := bobsList[constraint.ID]; listed || len(bobsList) != 1 {
		t.Errorf("bob's list is %v, want bob's constraint alone and not alice's %s", bobsList, constraint.ID)
	}
	if alicesList[constraint.ID] != graphstore.VisibilityAuthor || len(alicesList) != 2 {
		t.Errorf("alice's list is %v, want her constraint at author beside bob's", alicesList)
	}
}

// TestARepeatedLensWriteUnderItsKeyWritesOnce: the route's Idempotency-Key answers a repeat of a
// lens's invocation from the key, and the store holds one lens.
func TestARepeatedLensWriteUnderItsKeyWritesOnce(t *testing.T) {
	rig := newReasoningRig(t)
	before, _ := rig.counts(graphstore.KindLens, graphstore.TupleHasKind)
	key := uuid.NewString()
	body := map[string]any{"on": rig.spec, "kind": "botec", "findings": []string{"A thousand writes a second."}}
	first := keyedRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issue+"/lenses", body, "alice", key)
	second := keyedRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issue+"/lenses", body, "alice", key)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated || first.Body.String() != second.Body.String() {
		t.Fatalf("the two writes under one key answer %d %s and %d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	if after, _ := rig.counts(graphstore.KindLens, graphstore.TupleHasKind); after != before+1 {
		t.Fatalf("the store holds %d lenses after two writes under one key, and held %d before", after, before)
	}
}

// TestALensOnABlockOfACommentStandsOnItWhereEveryReaderKeysIt: a lens invoked on a comment's
// block names the block by its item's block kind, comment_block, the kind graph_nodes keys such a
// block by and every reader of an edge into one keys it by (farEndOf). Its part_of tuple stands on
// the comment_block, its answer names the block as the node it stands on, and the block's ?to=
// list, its backlink count and the lens read of it each hold the lens, and alice's own lens for
// alice alone.
func TestALensOnABlockOfACommentStandsOnItWhereEveryReaderKeysIt(t *testing.T) {
	rig := newReasoningRig(t)
	comment := createThreadComment(t, rig.handler, rig.issue, map[string]any{"body": "The first remark.\n\nThe second remark."}, "alice")
	node := "comment/" + comment.ID + "#b1"
	block := "dispatch://" + rig.issue + "/comment/" + comment.ID + "#b-b1"
	lens := rig.invoke(block, "botec", "alice", nil)
	hidden := rig.invoke(block, "botec", "alice", map[string]any{"visibility": graphstore.VisibilityAuthor})
	for _, invoked := range []lensInvoked{lens, hidden} {
		if invoked.On != block {
			t.Errorf("the lens %s answers it stands on %q, want the comment's block %s", invoked.Address, invoked.On, block)
		}
		if got, want := rig.current(graphstore.KindLens, invoked.ID, graphstore.TuplePartOf), []string{refs.ItemComment.BlockKind() + "/" + node}; !slices.Equal(got, want) {
			t.Errorf("the lens %s stands on %v, want %v", invoked.Address, got, want)
		}
	}

	for login, want := range map[string][]string{"alice": {lens.Address, hidden.Address}, "bob": {lens.Address}} {
		counts, err := refs.BacklinkCounts(t.Context(), rig.database.Pool, model.Actor{Kind: "user", ID: login},
			refs.ItemComment.BlockKind(), []string{node}, nil)
		if err != nil {
			t.Fatalf("count the comment block's backlinks as %s: %v", login, err)
		}
		response := referencesAt(t, rig.handler, "to", block, login)
		if response.Code != http.StatusOK {
			t.Fatalf("%s's ?to= of the comment's block answers %d %s, want 200", login, response.Code, response.Body.String())
		}
		listed := edgeEnds(decodeBody[model.GraphReferences](t, response).Edges)
		wantListed := make([]string, len(want))
		for index, address := range want {
			wantListed[index] = "in part_of " + address
		}
		slices.Sort(wantListed)
		if counts[node] != len(want) || !slices.Equal(listed, wantListed) {
			t.Errorf("%s counts %d backlinks of the comment's block and lists %v, want %d and %v", login, counts[node], listed, len(want), wantListed)
		}
		read := rig.lenses(block, login)
		if got, wanted := slices.Sorted(slices.Values(readAddresses(read.Lenses))), slices.Sorted(slices.Values(want)); !slices.Equal(got, wanted) {
			t.Errorf("%s's lens read of the comment's block answers %v, want %v", login, got, wanted)
		}
		for _, onBlock := range read.Lenses {
			if onBlock.On == nil || *onBlock.On != block {
				t.Errorf("%s's lens read answers the lens %s on %v, want the comment's block %s", login, onBlock.Address, onBlock.On, block)
			}
		}
	}
}
