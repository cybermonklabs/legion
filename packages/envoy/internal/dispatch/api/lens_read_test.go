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
)

// lensesResponse reads GET /api/v1/lenses?on= on as login.
func (r reasoningRig) lensesResponse(on, login string) *httptest.ResponseRecorder {
	r.t.Helper()
	return dispatchRequest(r.t, r.handler, http.MethodGet, "/api/v1/lenses?on="+url.QueryEscape(on), nil, login)
}

// lenses is the lens read of on as login, failing on anything but 200.
func (r reasoningRig) lenses(on, login string) nodeLenses {
	r.t.Helper()
	response := r.lensesResponse(on, login)
	if response.Code != http.StatusOK {
		r.t.Fatalf("read the lenses on %s as %s: status=%d body=%s", on, login, response.Code, response.Body.String())
	}
	return decodeBody[nodeLenses](r.t, response)
}

// invokeChain invokes n lenses as alice, each after the first triggered by the one before it:
// every one on on, or, stacked, the first on on and each after it on the lens before it.
func (r reasoningRig) invokeChain(on string, n int, stacked bool) []lensInvoked {
	r.t.Helper()
	chain := make([]lensInvoked, 0, n)
	for index := range n {
		at, extra := on, map[string]any(nil)
		if index > 0 {
			previous := chain[index-1].Address
			extra = map[string]any{"triggered_by": previous}
			if stacked {
				at = previous
			}
		}
		chain = append(chain, r.invoke(at, "botec", "alice", extra))
	}
	return chain
}

// readAddresses is the address of each lens read.
func readAddresses(lenses []lensRead) []string {
	addresses := make([]string, len(lenses))
	for index, lens := range lenses {
		addresses[index] = lens.Address
	}
	return addresses
}

// invokedAddresses is the address of each lens invoked.
func invokedAddresses(lenses []lensInvoked) []string {
	addresses := make([]string, len(lenses))
	for index, lens := range lenses {
		addresses[index] = lens.Address
	}
	return addresses
}

// lensNamed is the lens of lenses at address, failing where none is.
func lensNamed(t *testing.T, lenses []lensRead, address string) lensRead {
	t.Helper()
	for _, lens := range lenses {
		if lens.Address == address {
			return lens
		}
	}
	t.Fatalf("no lens of %v is %s", readAddresses(lenses), address)
	return lensRead{}
}

// lensKindWords is the words the catalogue's lens_kind node id carries.
func lensKindWords(t *testing.T, rig reasoningRig, id string) string {
	t.Helper()
	node, found, err := graphstore.ReadNode(context.Background(), rig.database.Pool, graphstore.KindLensKind, id)
	if err != nil || !found {
		t.Fatalf("read the lens_kind %s: found %t, %v", id, found, err)
	}
	words, _ := node.Attributes[graphstore.AttributeWords].(string)
	if words == "" {
		t.Fatalf("the lens_kind %s carries no words: %+v", id, node.Attributes)
	}
	return words
}

// TestTheLensReadAnswersEveryLensInInvocationOrder is c-lens-page
// (dispatch://INFRA-1950/verification#c-lens-page): two people, alice's trade-off analysis on the
// spec and bob's pre-mortem on it triggered by hers, and then alice's trade-off analysis written
// proposed and back to live through POST /api/v1/issues/{key}/nodes/{id}/state, so the lens
// invoked first is the lens whose row was written last. One read of the spec holds both, in the
// order of their part_of tuples, each with its part_of to the spec, its one has_kind with the
// kind's words, its author through authored_by, and the chain from both ends: placement and
// trigger kept apart.
func TestTheLensReadAnswersEveryLensInInvocationOrder(t *testing.T) {
	rig := newReasoningRig(t)
	ctx := context.Background()
	tradeOff := rig.invoke(rig.spec, "trade-off", "alice", nil)
	premortem := rig.invoke(rig.spec, graphstore.LensKindPreMortem, "bob", map[string]any{"triggered_by": tradeOff.Address})
	rig.writeState(graphstore.KindLens+"/"+tradeOff.ID, graphstore.StateProposed, "alice")
	rig.writeState(graphstore.KindLens+"/"+tradeOff.ID, graphstore.StateLive, "alice")

	var placedFirst, placedSecond, rewritten int64
	if err := rig.database.Pool.QueryRow(ctx, `
		select (select max(seq) from graph_tuples where from_kind = $1 and from_id = $2 and kind = $4),
		       (select max(seq) from graph_tuples where from_kind = $1 and from_id = $3 and kind = $4),
		       (select max(seq) from graph_tuples where from_kind = $1 and from_id = $2 and kind = $5)
	`, graphstore.KindLens, tradeOff.ID, premortem.ID, graphstore.TuplePartOf, graphstore.AttributeState).Scan(
		&placedFirst, &placedSecond, &rewritten); err != nil {
		t.Fatalf("read the seq of the two lenses' part_of tuples and of the trade-off's state: %v", err)
	}
	if placedFirst >= placedSecond || rewritten <= placedSecond {
		t.Fatalf("the trade-off's part_of is at %d, the pre-mortem's at %d and the trade-off's newest state at %d: "+
			"want the trade-off placed first and its row written last", placedFirst, placedSecond, rewritten)
	}

	read := rig.lenses(rig.spec, "alice")
	t.Logf("the read of the spec: %+v", read)
	if got, want := readAddresses(read.Lenses), []string{tradeOff.Address, premortem.Address}; !slices.Equal(got, want) {
		t.Fatalf("the read answers the lenses on the spec as %v; they were invoked %s (the trade-off analysis, part_of seq %d) "+
			"and then %s (the pre-mortem, part_of seq %d), and the read answers them in that order",
			got, tradeOff.Address, placedFirst, premortem.Address, placedSecond)
	}
	if read.On.Address != rig.spec || read.On.Kind != graphstore.KindArtifact || read.Lens != nil {
		t.Fatalf("the read answers the node %+v and the lens %+v, want the spec and no lens", read.On, read.Lens)
	}
	for index, want := range []struct {
		invoked      lensInvoked
		kind, author string
	}{
		{tradeOff, "trade-off", "alice"},
		{premortem, graphstore.LensKindPreMortem, "bob"},
	} {
		lens := read.Lenses[index]
		// Its placement is one part_of tuple, to the spec, and nothing else: the pre-mortem's
		// parent is the spec and not the lens that triggered it.
		if got := rig.current(graphstore.KindLens, want.invoked.ID, graphstore.TuplePartOf); !slices.Equal(got, []string{"artifact/" + rig.issue + "/spec"}) {
			t.Errorf("%s stands on %v, want one part_of tuple to the spec", lens.Address, got)
		}
		if lens.On == nil || *lens.On != rig.spec {
			t.Errorf("%s answers it stands on %v, want %s", lens.Address, lens.On, rig.spec)
		}
		// Its kind is one has_kind tuple to a lens_kind node of the catalogue, with that node's words.
		if got := rig.current(graphstore.KindLens, want.invoked.ID, graphstore.TupleHasKind); !slices.Equal(got, []string{"lens_kind/" + want.kind}) {
			t.Errorf("%s holds has_kind %v, want one to lens_kind/%s", lens.Address, got, want.kind)
		}
		if words := lensKindWords(t, rig, want.kind); lens.Kind == nil || lens.Kind.ID != want.kind || lens.Kind.Words != words {
			t.Errorf("%s answers the kind %+v, want %s with the words %q", lens.Address, lens.Kind, want.kind, words)
		}
		// Its author is the person its authored_by tuple names.
		if got := rig.current(graphstore.KindLens, want.invoked.ID, graphstore.TupleAuthoredBy); !slices.Equal(got, []string{"person/" + want.author}) {
			t.Errorf("%s holds authored_by %v, want person/%s", lens.Address, got, want.author)
		}
		if lens.Author == nil || lens.Author.Kind != graphstore.KindPerson || lens.Author.ID != want.author {
			t.Errorf("%s answers the author %+v, want the person %s", lens.Address, lens.Author, want.author)
		}
		if lens.State != graphstore.StateLive || lens.Visibility != graphstore.VisibilityProject {
			t.Errorf("%s answers state %q and visibility %q, want live and project", lens.Address, lens.State, lens.Visibility)
		}
	}

	// The chain, from both ends of the one triggered_by tuple.
	if got := rig.current(graphstore.KindLens, premortem.ID, graphstore.TupleTriggeredBy); !slices.Equal(got, []string{"lens/" + tradeOff.ID}) {
		t.Fatalf("the pre-mortem holds triggered_by %v, want the trade-off analysis", got)
	}
	first, second := read.Lenses[0], read.Lenses[1]
	if !slices.Equal(second.TriggeredBy, []string{tradeOff.Address}) || len(second.Triggered) != 0 {
		t.Errorf("the pre-mortem answers triggered by %v and triggering %v, want triggered by %s alone", second.TriggeredBy, second.Triggered, tradeOff.Address)
	}
	if !slices.Equal(first.Triggered, []string{premortem.Address}) || len(first.TriggeredBy) != 0 {
		t.Errorf("the trade-off analysis answers triggering %v and triggered by %v, want triggering %s alone", first.Triggered, first.TriggeredBy, premortem.Address)
	}
	if len(read.Chains) != 1 {
		t.Fatalf("the read answers %d chains, want the one the two lenses make", len(read.Chains))
	}
	chain := read.Chains[0]
	if got := readAddresses(chain.Lenses); !slices.Equal(got, []string{tradeOff.Address, premortem.Address}) || chain.StoppedAt != nil {
		t.Fatalf("the chain answers %v stopped at %v, want the trade-off analysis and then the pre-mortem, whole", got, chain.StoppedAt)
	}
	if !slices.Equal(chain.Lenses[1].TriggeredBy, []string{tradeOff.Address}) || !slices.Equal(chain.Lenses[0].Triggered, []string{premortem.Address}) {
		t.Errorf("the chain answers the pre-mortem triggered by %v and the trade-off triggering %v", chain.Lenses[1].TriggeredBy, chain.Lenses[0].Triggered)
	}
}

// TestTheLensReadAnswersEachLensWithItsFindingsAndRisks: a lens's findings come in rank order and
// a pre-mortem's risks in the order of their part_of tuples, each whole, a risk no check prevents
// with an empty list; a finding or a risk reserved for its author is in its author's read alone.
func TestTheLensReadAnswersEachLensWithItsFindingsAndRisks(t *testing.T) {
	rig := newReasoningRig(t)
	lens := rig.invoke(rig.spec, "trade-off", "alice", map[string]any{
		"verdict": "Keep one region.", "findings": []string{"A second region doubles the cost.", "Failover takes minutes."},
	})
	private := rig.post(nodePath("lens/"+lens.ID, "findings"), map[string]any{"text": "Mine alone.", "visibility": graphstore.VisibilityAuthor}, "alice")
	if private.Code != http.StatusCreated {
		t.Fatalf("add alice's finding: status=%d body=%s", private.Code, private.Body.String())
	}
	hiddenFinding := decodeBody[lensFinding](t, private)
	premortem := rig.invoke(rig.spec, graphstore.LensKindPreMortem, "alice", nil)
	risk := func(heading string, extra map[string]any) riskWritten {
		t.Helper()
		body := map[string]any{"lens": premortem.Address, "heading": heading, "early_warning": "Writes queue.", "prevention": "Build it concurrently."}
		for key, value := range extra {
			body[key] = value
		}
		response := rig.post("/risks", body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("write the risk %q: status=%d body=%s", heading, response.Code, response.Body.String())
		}
		return decodeBody[riskWritten](t, response)
	}
	prevented := risk("The migration locks the table.", map[string]any{"prevented_by": rig.check})
	unprevented := risk("The cache goes cold.", nil)
	hiddenRisk := risk("A risk she keeps.", map[string]any{"visibility": graphstore.VisibilityAuthor})

	response := rig.lensesResponse(rig.spec, "bob")
	if response.Code != http.StatusOK {
		t.Fatalf("bob's read: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, hidden := range []string{hiddenFinding.Address, hiddenRisk.Address} {
		if strings.Contains(response.Body.String(), hidden) {
			t.Errorf("bob's read names %s, which is alice's alone: %s", hidden, response.Body.String())
		}
	}
	bobs := decodeBody[nodeLenses](t, response)
	if got := readAddresses(bobs.Lenses); !slices.Equal(got, []string{lens.Address, premortem.Address}) {
		t.Fatalf("bob reads the lenses %v, want %s and then %s", got, lens.Address, premortem.Address)
	}
	read := bobs.Lenses[0]
	if read.Verdict == nil || *read.Verdict != "Keep one region." {
		t.Errorf("the lens answers the verdict %v", read.Verdict)
	}
	if len(read.Findings) != 2 {
		t.Fatalf("bob reads the findings %+v, want the two he may see", read.Findings)
	}
	for index, finding := range read.Findings {
		want := lens.Findings[index]
		if finding.Address != want.Address || finding.Text != want.Text || finding.State != graphstore.StateLive ||
			finding.Visibility != graphstore.VisibilityProject {
			t.Errorf("finding %d answers %+v, want %s %q", index, finding, want.Address, want.Text)
		}
	}
	if read.Findings[0].Rank >= read.Findings[1].Rank {
		t.Errorf("the findings rank %d and %d, want the first written first", read.Findings[0].Rank, read.Findings[1].Rank)
	}
	if len(read.Risks) != 0 {
		t.Errorf("the trade-off analysis answers the risks %+v, want none", read.Risks)
	}
	risks := bobs.Lenses[1].Risks
	if len(risks) != 2 {
		t.Fatalf("bob reads the risks %+v, want the two he may see", risks)
	}
	if risks[0].Address != prevented.Address || risks[0].Heading != "The migration locks the table." || risks[0].EarlyWarning != "Writes queue." ||
		risks[0].Prevention != "Build it concurrently." || !slices.Equal(risks[0].PreventedBy, []string{rig.check}) {
		t.Errorf("the first risk answers %+v, want %s prevented by %s", risks[0], prevented.Address, rig.check)
	}
	if risks[1].Address != unprevented.Address || risks[1].PreventedBy == nil || len(risks[1].PreventedBy) != 0 {
		t.Errorf("the second risk answers %+v, want %s prevented by an empty list", risks[1], unprevented.Address)
	}
	if !strings.Contains(response.Body.String(), `"prevented_by":[]`) {
		t.Errorf("bob's read answers no empty prevented_by list: %s", response.Body.String())
	}

	alices := rig.lenses(rig.spec, "alice")
	if findings := alices.Lenses[0].Findings; len(findings) != 3 || findings[2].Address != hiddenFinding.Address ||
		findings[2].Visibility != graphstore.VisibilityAuthor || findings[2].Text != "Mine alone." {
		t.Errorf("alice reads the findings %+v, want her own third at author", findings)
	}
	if risks := alices.Lenses[1].Risks; len(risks) != 3 || risks[2].Address != hiddenRisk.Address || risks[2].Visibility != graphstore.VisibilityAuthor {
		t.Errorf("alice reads the risks %+v, want her own third at author", risks)
	}
}

// TestALensReadSkipsALensTheReaderMayNotSee: the walk of a chain passes through no lens the
// reader may not see. alice's trade-off analysis A triggers her inversion H, reserved for her,
// which triggers C on the spec and D on the requirement; C triggers E. For bob, H is in no list,
// no chain and no count: A triggered nothing he may see, C was triggered by nothing, D - reached
// only through H - is nowhere, and his one chain is C and E. alice reads the whole chain, D on the
// requirement among it. A read on a lens's address answers its own trigger ends, each reader's.
func TestALensReadSkipsALensTheReaderMayNotSee(t *testing.T) {
	rig := newReasoningRig(t)
	a := rig.invoke(rig.spec, "trade-off", "alice", nil)
	hidden := rig.invoke(rig.spec, "inversion", "alice", map[string]any{"triggered_by": a.Address, "visibility": graphstore.VisibilityAuthor})
	c := rig.invoke(rig.spec, "botec", "alice", map[string]any{"triggered_by": hidden.Address})
	d := rig.invoke(rig.requirement, "second-order", "alice", map[string]any{"triggered_by": hidden.Address})
	e := rig.invoke(rig.spec, "conway", "alice", map[string]any{"triggered_by": c.Address})

	response := rig.lensesResponse(rig.spec, "bob")
	if response.Code != http.StatusOK {
		t.Fatalf("bob's read: status=%d body=%s", response.Code, response.Body.String())
	}
	t.Logf("bob's read: %s", response.Body.String())
	for _, absent := range []string{hidden.ID, d.ID} {
		if strings.Contains(response.Body.String(), absent) {
			t.Errorf("bob's read names %s, which he may reach only through alice's own lens", absent)
		}
	}
	bobs := decodeBody[nodeLenses](t, response)
	if got := readAddresses(bobs.Lenses); !slices.Equal(got, []string{a.Address, c.Address, e.Address}) {
		t.Fatalf("bob reads the lenses %v, want A, C and E", got)
	}
	if lens := bobs.Lenses[0]; len(lens.Triggered) != 0 || len(lens.TriggeredBy) != 0 {
		t.Errorf("bob reads A triggering %v and triggered by %v, want neither: H is alice's", lens.Triggered, lens.TriggeredBy)
	}
	if lens := bobs.Lenses[1]; len(lens.TriggeredBy) != 0 || !slices.Equal(lens.Triggered, []string{e.Address}) {
		t.Errorf("bob reads C triggered by %v and triggering %v, want triggering E alone", lens.TriggeredBy, lens.Triggered)
	}
	if len(bobs.Chains) != 1 || !slices.Equal(readAddresses(bobs.Chains[0].Lenses), []string{c.Address, e.Address}) {
		t.Fatalf("bob reads the chains %+v, want C and E alone", bobs.Chains)
	}

	alices := rig.lenses(rig.spec, "alice")
	if got := readAddresses(alices.Lenses); !slices.Equal(got, []string{a.Address, hidden.Address, c.Address, e.Address}) {
		t.Fatalf("alice reads the lenses %v, want A, H, C and E", got)
	}
	if lens := alices.Lenses[1]; lens.Visibility != graphstore.VisibilityAuthor || !slices.Equal(lens.TriggeredBy, []string{a.Address}) ||
		!slices.Equal(lens.Triggered, []string{c.Address, d.Address}) {
		t.Errorf("alice reads H as %+v, want hers alone, triggered by A and triggering C and D", lens)
	}
	if len(alices.Chains) != 1 {
		t.Fatalf("alice reads %d chains, want one", len(alices.Chains))
	}
	chain := alices.Chains[0]
	if got, want := readAddresses(chain.Lenses), []string{a.Address, hidden.Address, c.Address, d.Address, e.Address}; !slices.Equal(got, want) || chain.StoppedAt != nil {
		t.Fatalf("alice reads the chain %v stopped at %v, want %v whole", got, chain.StoppedAt, want)
	}
	if on := lensNamed(t, chain.Lenses, d.Address).On; on == nil || *on != rig.requirement {
		t.Errorf("D answers it stands on %v, want the requirement %s", on, rig.requirement)
	}

	// Either end of a trigger reads the other, each reader's as that reader may see it.
	for _, read := range []struct {
		login string
		want  []string
	}{{"bob", nil}, {"alice", []string{hidden.Address}}} {
		onC := rig.lenses(c.Address, read.login)
		if onC.Lens == nil || onC.Lens.Address != c.Address || onC.On.Kind != graphstore.KindLens {
			t.Fatalf("%s's read on C answers the node %+v and the lens %+v", read.login, onC.On, onC.Lens)
		}
		if !slices.Equal(onC.Lens.TriggeredBy, read.want) {
			t.Errorf("%s reads C triggered by %v, want %v", read.login, onC.Lens.TriggeredBy, read.want)
		}
		if !slices.Equal(onC.Lens.Triggered, []string{e.Address}) {
			t.Errorf("%s reads C triggering %v, want E", read.login, onC.Lens.Triggered)
		}
		if onC.Lens.On == nil || *onC.Lens.On != rig.spec || len(onC.Lenses) != 0 {
			t.Errorf("%s's read on C answers it stands on %v with the lenses %v on it, want the spec and none", read.login, onC.Lens.On, readAddresses(onC.Lenses))
		}
	}
}

// TestALensCycleIsReadOnceEach: lens B is triggered by A, and then A by B. The read returns, and
// the chain lists each of the two once: the visited set at work, apart from the depth bound.
func TestALensCycleIsReadOnceEach(t *testing.T) {
	rig := newReasoningRig(t)
	a := rig.invoke(rig.spec, "trade-off", "alice", nil)
	b := rig.invoke(rig.spec, graphstore.LensKindPreMortem, "alice", map[string]any{"triggered_by": a.Address})
	closed := rig.post(nodePath("lens/"+a.ID, "pointers"), map[string]any{
		"kind": graphstore.TupleTriggeredBy, "action": pointerAppend, "to": b.Address,
	}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("point A at B: status=%d body=%s", closed.Code, closed.Body.String())
	}
	for _, on := range []string{rig.spec, a.Address} {
		read := rig.lenses(on, "alice")
		if len(read.Chains) != 1 {
			t.Fatalf("the read on %s answers %d chains, want one", on, len(read.Chains))
		}
		chain := read.Chains[0]
		if got := readAddresses(chain.Lenses); !slices.Equal(got, []string{a.Address, b.Address}) || chain.StoppedAt != nil {
			t.Fatalf("the read on %s answers the chain %v stopped at %v, want A and B once each, whole", on, got, chain.StoppedAt)
		}
		for _, lens := range chain.Lenses {
			other := a.Address
			if lens.Address == a.Address {
				other = b.Address
			}
			if !slices.Equal(lens.TriggeredBy, []string{other}) || !slices.Equal(lens.Triggered, []string{other}) {
				t.Errorf("the read on %s answers %s triggered by %v and triggering %v, want %s both ways", on, lens.Address, lens.TriggeredBy, lens.Triggered, other)
			}
		}
	}
}

// TestALensChainOfSixtyIsCutAtFifty: a chain of sixty triggered lenses, ten past the bound,
// lists fifty and names the lens where the walk stopped, the fifty-first. Stacked, each lens on
// the one before it, the requirement's read holds one lens on it and one chain of fifty, each
// lens standing on the one before it. Flat, every lens on the paragraph, the paragraph's read
// holds all sixty on it, and fifty of them in all across every chain it answers: one chain, the
// first fifty, stopped at the fifty-first. A chain starts only at a lens triggered by no other
// lens on the paragraph, so no lens past the cut begins one, and no chain holds the fifty-first or
// any lens after it.
func TestALensChainOfSixtyIsCutAtFifty(t *testing.T) {
	rig := newReasoningRig(t)
	stacked := rig.invokeChain(rig.requirement, 60, true)
	read := rig.lenses(rig.requirement, "alice")
	if got := readAddresses(read.Lenses); !slices.Equal(got, []string{stacked[0].Address}) {
		t.Fatalf("the requirement holds the lenses %v, want the first of the chain alone", got)
	}
	if len(read.Chains) != 1 {
		t.Fatalf("the requirement's read answers %d chains, want one", len(read.Chains))
	}
	chain := read.Chains[0]
	t.Logf("the stacked chain lists %d lenses and stops at %v", len(chain.Lenses), chain.StoppedAt)
	if got, want := readAddresses(chain.Lenses), invokedAddresses(stacked[:lensChainDepth]); !slices.Equal(got, want) {
		t.Fatalf("the stacked chain lists %d lenses %v, want the first %d in the order invoked", len(got), got, lensChainDepth)
	}
	if chain.StoppedAt == nil || *chain.StoppedAt != stacked[lensChainDepth].Address {
		t.Fatalf("the stacked chain stops at %v, want the fifty-first lens %s", chain.StoppedAt, stacked[lensChainDepth].Address)
	}
	for index, lens := range chain.Lenses {
		want := rig.requirement
		if index > 0 {
			want = stacked[index-1].Address
		}
		if lens.On == nil || *lens.On != want {
			t.Fatalf("lens %d of the stacked chain answers it stands on %v, want %s", index+1, lens.On, want)
		}
	}

	flat := rig.invokeChain(rig.paragraph, 60, false)
	read = rig.lenses(rig.paragraph, "alice")
	if got := readAddresses(read.Lenses); !slices.Equal(got, invokedAddresses(flat)) {
		t.Fatalf("the paragraph holds %d lenses, want the sixty in the order invoked", len(got))
	}
	listed, past := 0, invokedAddresses(flat[lensChainDepth:])
	for index, chain := range read.Chains {
		listed += len(chain.Lenses)
		for _, lens := range chain.Lenses {
			if at := slices.Index(past, lens.Address); at >= 0 {
				t.Errorf("chain %d of the paragraph's read, of %d lenses, holds %s, lens %d of the chain of sixty, past the cut at %d",
					index+1, len(chain.Lenses), lens.Address, lensChainDepth+at+1, lensChainDepth)
			}
		}
	}
	if listed != lensChainDepth || len(read.Chains) != 1 {
		t.Fatalf("the paragraph's read lists %d lenses across %d chains, want %d in one chain: a lens past the cut begins no chain of its own",
			listed, len(read.Chains), lensChainDepth)
	}
	if got := readAddresses(read.Chains[0].Lenses); !slices.Equal(got, invokedAddresses(flat[:lensChainDepth])) ||
		read.Chains[0].StoppedAt == nil || *read.Chains[0].StoppedAt != flat[lensChainDepth].Address {
		t.Fatalf("the flat chain lists %d lenses stopped at %v, want the first fifty stopped at %s", len(got), read.Chains[0].StoppedAt, flat[lensChainDepth].Address)
	}
}

// TestABackwardLensChainOfAHundredAndTwentyAnswersFiftyInAll: a hundred and twenty lenses on the
// paragraph, invoked in order, each but the last then triggered by the one invoked after it through
// the pointer route, so the chain's triggers run back against the order invoked and its one root,
// the lens triggered by no other on the paragraph, is the last. Its walk lists the last fifty and
// stops at the seventieth. The first lens begins no chain of its own although its walk, fifty
// lenses long, meets no lens the root's walk visited before it stops: a walk from a lens that is no
// root that stops at the bound stands past a cut. So the read answers one chain, fifty lenses in
// all, and names where it stopped.
func TestABackwardLensChainOfAHundredAndTwentyAnswersFiftyInAll(t *testing.T) {
	rig := newReasoningRig(t)
	invoked := make([]lensInvoked, 0, 120)
	for range cap(invoked) {
		invoked = append(invoked, rig.invoke(rig.paragraph, "botec", "alice", nil))
	}
	for index, lens := range invoked[:len(invoked)-1] {
		pointed := rig.post(nodePath("lens/"+lens.ID, "pointers"), map[string]any{
			"kind": graphstore.TupleTriggeredBy, "action": pointerAppend, "to": invoked[index+1].Address,
		}, "alice")
		if pointed.Code != http.StatusOK {
			t.Fatalf("point lens %d at lens %d: status=%d body=%s", index+1, index+2, pointed.Code, pointed.Body.String())
		}
	}
	read := rig.lenses(rig.paragraph, "alice")
	addresses := invokedAddresses(invoked)
	if got := readAddresses(read.Lenses); !slices.Equal(got, addresses) {
		t.Fatalf("the paragraph holds %d lenses, want the %d in the order invoked", len(got), len(invoked))
	}
	// numbered is the place in the order invoked, from 1, of the lens at address; 0 for none.
	numbered := func(address *string) int {
		if address == nil {
			return 0
		}
		return slices.Index(addresses, *address) + 1
	}
	listed := 0
	for index, chain := range read.Chains {
		listed += len(chain.Lenses)
		t.Logf("chain %d lists %d lenses, lens %d to lens %d, and stops at lens %d (0: none)", index+1, len(chain.Lenses),
			numbered(&chain.Lenses[0].Address), numbered(&chain.Lenses[len(chain.Lenses)-1].Address), numbered(chain.StoppedAt))
	}
	if listed != lensChainDepth || len(read.Chains) != 1 {
		t.Fatalf("the paragraph's read lists %d lenses across %d chains, want %d in one chain: a lens past the cut begins no chain of its own",
			listed, len(read.Chains), lensChainDepth)
	}
	cut := len(invoked) - lensChainDepth
	chain := read.Chains[0]
	if got := readAddresses(chain.Lenses); !slices.Equal(got, addresses[cut:]) || numbered(chain.StoppedAt) != cut {
		t.Fatalf("the chain lists %d lenses and stops at lens %d, want lens %d to lens %d stopped at lens %d",
			len(got), numbered(chain.StoppedAt), cut+1, len(invoked), cut)
	}
}

// TestALensChainOfExactlyFiftyIsListedWhole: a chain of exactly fifty, flat on the spec or
// stacked on the requirement, is listed whole with no stop reported.
func TestALensChainOfExactlyFiftyIsListedWhole(t *testing.T) {
	rig := newReasoningRig(t)
	for _, shape := range []struct {
		name, on string
		stacked  bool
	}{{"flat", rig.spec, false}, {"stacked", rig.requirement, true}} {
		invoked := rig.invokeChain(shape.on, lensChainDepth, shape.stacked)
		read := rig.lenses(shape.on, "alice")
		if len(read.Chains) != 1 {
			t.Fatalf("the %s chain's read answers %d chains, want one", shape.name, len(read.Chains))
		}
		chain := read.Chains[0]
		t.Logf("the %s chain lists %d lenses and stops at %v", shape.name, len(chain.Lenses), chain.StoppedAt)
		if got := readAddresses(chain.Lenses); !slices.Equal(got, invokedAddresses(invoked)) || chain.StoppedAt != nil {
			t.Fatalf("the %s chain lists %d lenses stopped at %v, want all %d with no stop", shape.name, len(got), chain.StoppedAt, lensChainDepth)
		}
	}
}

// TestALensReadOfANodeNobodyMaySeeAnswersNotFound: on must name a node the caller may see. A
// lens or a constraint reserved for alice, a lens and a constraint nobody holds, an issue nobody
// holds and words that are no address each answer bob one 404, in the same words but the
// address; alice reads her own lens. An empty on is 400.
func TestALensReadOfANodeNobodyMaySeeAnswersNotFound(t *testing.T) {
	rig := newReasoningRig(t)
	hidden := rig.invoke(rig.spec, "trade-off", "alice", map[string]any{"visibility": graphstore.VisibilityAuthor})
	written := rig.post("/constraints", map[string]any{"standard": "A rule she keeps", "reference": "her notes", "visibility": graphstore.VisibilityAuthor}, "alice")
	if written.Code != http.StatusCreated {
		t.Fatalf("write alice's constraint: status=%d body=%s", written.Code, written.Body.String())
	}
	constraint := decodeBody[constraintRead](t, written)
	nobodys := "dispatch://lens/" + uuid.NewString()

	bodies := map[string]string{}
	for _, address := range []string{
		hidden.Address, nobodys, constraint.Address, "dispatch://constraint/" + uuid.NewString(),
		"dispatch://NOPE-404/spec", "no address at all",
	} {
		says := fmt.Sprintf("no node %q", address)
		response := rig.lensesResponse(address, "bob")
		if refused := requireRefused(t, "bob's read on "+address, response, http.StatusNotFound, "NOT_FOUND", says); refused.Error != says {
			t.Errorf("bob's read on %s is refused %q, want %q", address, refused.Error, says)
		}
		bodies[address] = strings.ReplaceAll(response.Body.String(), address, "<on>")
	}
	if bodies[hidden.Address] != bodies[nobodys] || bodies[constraint.Address] != bodies[nobodys] {
		t.Errorf("a node bob may not see answers %s and %s, and one nobody holds %s", bodies[hidden.Address], bodies[constraint.Address], bodies[nobodys])
	}

	own := rig.lenses(hidden.Address, "alice")
	if own.Lens == nil || own.Lens.Address != hidden.Address || own.Lens.Visibility != graphstore.VisibilityAuthor ||
		own.On.Visibility != graphstore.VisibilityAuthor {
		t.Fatalf("alice's read of her own lens answers the node %+v and the lens %+v", own.On, own.Lens)
	}
	if bobs := rig.lenses(rig.spec, "bob"); len(bobs.Lenses) != 0 || len(bobs.Chains) != 0 || bobs.Lenses == nil || bobs.Chains == nil {
		t.Errorf("bob reads the spec's lenses %v and chains %v, want two empty lists", readAddresses(bobs.Lenses), bobs.Chains)
	}
	requireRefused(t, "a read on nothing", rig.lensesResponse("", "bob"), http.StatusBadRequest, "INVALID_QUERY", "on is required")
}
