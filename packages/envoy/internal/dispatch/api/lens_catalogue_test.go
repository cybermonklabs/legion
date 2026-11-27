package api

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seededLensSources is the source and the reference each kind c-lens-catalogue names the source of
// holds on its constraint, as this change's research task read each in the source itself. For the
// ten kinds of the lens research the catalogue is seeded from (a record outside this repository),
// six are the sources that research cites, with second-order taking Bastiat (1850) where it cited
// Chesterton's fence, and best-practice, first-principles, conway and botec take the four the
// research task found and checked. The eleventh is chestertons-fence, the further kind R7 gives
// Chesterton (1929). Written here, character for character, so a seed that moves one fails this
// test rather than passing on whatever the migration says.
var seededLensSources = map[string]struct{ source, reference string }{
	"pre-mortem": {
		`Gary Klein, "Performing a Project Premortem", Harvard Business Review, September 2007 (Forethought section)`,
		`Harvard Business Review vol. 85, September 2007; HBR reprint F0709A; https://hbr.org/2007/09/performing-a-project-premortem`,
	},
	"boring-solution": {
		`Dan McKinley, "Choose Boring Technology", mcfunley.com, 30 March 2015`,
		`https://mcfunley.com/choose-boring-technology`,
	},
	"best-practice": {
		`Robert C. Camp, Benchmarking: The Search for Industry Best Practices that Lead to Superior Performance, ASQC Quality Press, 1989`,
		`Milwaukee, WI: ASQC Quality Press; New York: Quality Resources, 1989. ISBN 0-87389-058-2. Ch. 1 "Introduction", section "Benchmarking Defined", pp. 10–12.`,
	},
	"first-principles": {
		`Aristotle, Physics, book I, chapter 1`,
		`Physics I.1, 184a10–16 (Bekker); trans. R. P. Hardie and R. K. Gaye`,
	},
	"inversion": {
		`Charles T. Munger, Harvard School commencement speech, Harvard School, Los Angeles, 13 June 1986`,
		`Poor Charlie's Almanack, ch. 4, "Talk One: Harvard School Commencement Speech", Stripe Press online edition: https://www.stripe.press/poor-charlies-almanack/talk-one`,
	},
	"trade-off": {
		`Rick Kazman, Mark Klein, Paul Clements, ATAM: Method for Architecture Evaluation, Software Engineering Institute, Carnegie Mellon University, August 2000`,
		`Technical Report CMU/SEI-2000-TR-004 (ESC-TR-2000-004); §7.2 "Sensitivity and Tradeoff Points", pp. 22–23; https://www.sei.cmu.edu/documents/629/2000_005_001_13706.pdf`,
	},
	"reversibility": {
		`Jeffrey P. Bezos, 2015 Letter to Shareholders, Amazon.com, Inc.`,
		`"To our shareowners", section "Invention Machine"; Exhibit 99.1 to Amazon.com, Inc.'s Form 8-K filed 5 April 2016 (SEC accession 0001193125-16-530910); https://www.sec.gov/Archives/edgar/data/1018724/000119312516530910/d168744dex991.htm`,
	},
	"second-order": {
		`Frédéric Bastiat, "What Is Seen and What Is Not Seen" ("Ce qu'on voit et ce qu'on ne voit pas"), 1850`,
		`Selected Essays on Political Economy, trans. Seymour Cain, ed. George B. de Huszar (Irvington-on-Hudson, NY: Foundation for Economic Education, 1995), ch. 1, opening paragraph; https://www.econlib.org/book-chapters/chapter-chapter-1-what-is-seen-and-what-is-not-seen/`,
	},
	"conway": {
		`Melvin E. Conway, "How Do Committees Invent?", Datamation, April 1968`,
		`Datamation vol. 14, no. 4 (April 1968), pp. 28–31, F. D. Thompson Publications. Author's reprint: https://www.melconway.com/research/committees.html`,
	},
	"botec": {
		`Jon Bentley, "Programming Pearls: The Back of the Envelope", Communications of the ACM, March 1984`,
		`Communications of the ACM 27(3), March 1984, pp. 180–184; doi:10.1145/357994.381168`,
	},
	"chestertons-fence": {
		`G. K. Chesterton, The Thing, 1929, chapter "The Drift from Domesticity"`,
		`The Thing, London: Sheed & Ward, 1929, ch. 4 "The Drift from Domesticity"; p. 29 in the 1946 Sheed & Ward reprint. The US edition is The Thing: Why I Am a Catholic, New York: Dodd, Mead, 1930.`,
	},
}

// seededLensKinds is the ten ids of that lens research, in its order.
var seededLensKinds = []string{
	"pre-mortem", "boring-solution", "best-practice", "first-principles", "inversion",
	"trade-off", "reversibility", "second-order", "conway", "botec",
}

// TestLensKindCatalogueIsSeededWithItsSources is c-lens-catalogue
// (spec requirement r-lens-catalogue). The store a test opens is migrated, so it holds
// the seed; the catalogue is read back through GET /api/v1/schema/blocks. At least ten lens kinds
// exist, the ten of that lens research among them; every kind carries one constrained_by tuple to a
// constraint node, and a kind with none fails naming the kind and its missing source; every
// constraint the seed holds is the far end of exactly one constrained_by tuple from a lens_kind
// node, and one that is not fails naming it; and each of the ten, and chestertons-fence, holds the
// source and reference seededLensSources writes, character for character.
//
// Then the catalogue is shown to be data and not code: a further kind is added at runtime through
// graphstore.AddLensKind, the write `envoy-dispatch lens-kind-add` runs, and in this same process,
// with no restart and no reload, the same catalogue read answers it with its source and a lens
// written with a has_kind tuple to it is taken, the edge standing in graph_edges. A has_kind tuple
// to a kind id no lens_kind node holds, written through the route a lens's kind is written by
// (POST /api/v1/issues/{key}/lenses), is refused 400 INVALID_POINTER naming the kind, and the store
// holds no lens and no tuple of it after. The schema version the store's type nodes build is one
// value before and after.
func TestLensKindCatalogueIsSeededWithItsSources(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	versionBefore := storeSchemaVersion(t, ctx, database)

	read, _ := readBlockSchemaRoute(t, handler)
	if len(read.LensKinds) < len(seededLensKinds) {
		t.Errorf("the catalogue holds %d lens kinds, want at least the %d kinds the catalogue's migration seeds", len(read.LensKinds), len(seededLensKinds))
	}
	kinds := map[string]lensKindRead{}
	for _, kind := range read.LensKinds {
		kinds[kind.ID] = kind
		if len(kind.Sources) != 1 {
			t.Errorf("the lens kind %s carries %d constrained_by tuples to a constraint node, want one: its source is missing", kind.ID, len(kind.Sources))
		}
	}
	for _, id := range slices.Sorted(maps.Keys(seededLensSources)) {
		kind, held := kinds[id]
		if !held {
			t.Errorf("the catalogue lacks the lens kind %s", id)
			continue
		}
		want := seededLensSources[id]
		if len(kind.Sources) != 1 {
			continue
		}
		if got := kind.Sources[0]; got.Standard != want.source || got.Reference != want.reference {
			t.Errorf("the lens kind %s holds the source %q with the reference %q, want %q with %q", id, got.Standard, got.Reference, want.source, want.reference)
		}
	}
	requireEverySeededConstraintIsOneKindsSource(t, ctx, database)

	// One further kind, added at runtime as lens-kind-add adds one.
	added := graphstore.LensKind{
		ID: "catalogue-test-kind", Words: "a kind added after the seed",
		Source: "A source the test names", Reference: "the reference the test names",
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the add: %v", err)
	}
	source, err := graphstore.AddLensKind(ctx, tx, added, time.Now().UTC())
	if err != nil {
		t.Fatalf("add the lens kind %s: %v", added.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the add: %v", err)
	}

	again, _ := readBlockSchemaRoute(t, handler)
	found := false
	for _, kind := range again.LensKinds {
		if kind.ID != added.ID {
			continue
		}
		found = true
		want := lensKindSourceRead{ID: source, Ref: "dispatch://constraint/" + source, Standard: added.Source, Reference: added.Reference}
		if kind.Words != added.Words || len(kind.Sources) != 1 || kind.Sources[0] != want {
			t.Errorf("the added kind reads %+v, want its words %q and its one source %+v", kind, added.Words, want)
		}
	}
	if !found {
		t.Fatalf("the catalogue read after the add answers no %s: the read holds a copy of the catalogue", added.ID)
	}
	if len(again.LensKinds) != len(read.LensKinds)+1 {
		t.Errorf("the catalogue holds %d kinds after one add, want %d", len(again.LensKinds), len(read.LensKinds)+1)
	}

	// A lens of the new kind, written through the store's writer with its has_kind tuple.
	lens := uuid.NewString()
	at := time.Now().UTC()
	who := model.Actor{Kind: "user", ID: "alice"}
	tx, err = database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lens: %v", err)
	}
	if err := graphstore.Write(ctx, tx, []graphstore.Node{{
		Kind: graphstore.KindLens, ID: lens, Supertag: graphstore.KindLens,
		Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
		MadeAt: at, MadeBy: who, SourceEvent: graphstore.NoEvent,
	}}, []graphstore.Tuple{{
		FromKind: graphstore.KindLens, FromID: lens, Kind: graphstore.TupleHasKind,
		ToKind: graphstore.KindLensKind, ToID: added.ID,
		At: at, Who: who, SourceEvent: graphstore.NoEvent, Asserted: true,
	}}); err != nil {
		t.Fatalf("write a lens of the added kind: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the lens: %v", err)
	}
	var edges int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_edges
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5`,
		graphstore.KindLens, lens, graphstore.TupleHasKind, graphstore.KindLensKind, added.ID).Scan(&edges); err != nil {
		t.Fatalf("read the lens's has_kind edge: %v", err)
	}
	if edges != 1 {
		t.Errorf("graph_edges holds %d has_kind edges from the lens to %s, want one", edges, added.ID)
	}
	if node, found, err := refs.Node(ctx, database.Pool, model.Actor{}, graphstore.KindLensKind, added.ID, true); err != nil || !found ||
		node.Ref != "dispatch://lens_kind/"+added.ID {
		t.Errorf("the added kind resolves as %+v (found %t, %v), want its node at dispatch://lens_kind/%s", node, found, err, added.ID)
	}

	// A has_kind tuple to a kind id no lens_kind node holds, through the lens route, which writes
	// a lens's kind: refused, naming the kind, with nothing stored.
	const unknown = "catalogue-test-unknown-kind"
	if _, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindLensKind, unknown); err != nil || held {
		t.Fatalf("read the lens_kind %s before the write: held %t, %v; want no node to hold it", unknown, held, err)
	}
	stored := func() (lenses, tuples int) {
		t.Helper()
		if err := database.Pool.QueryRow(ctx, `
			select (select count(*) from graph_nodes where kind = $1),
			       (select count(*) from graph_tuples where kind = $2 and to_kind = $3 and to_id = $4)`,
			graphstore.KindLens, graphstore.TupleHasKind, graphstore.KindLensKind, unknown).Scan(&lenses, &tuples); err != nil {
			t.Fatalf("count the lenses and the has_kind tuples to %s: %v", unknown, err)
		}
		return lenses, tuples
	}
	issue := createInteractionIssue(t, handler, "LKC", "Lens kind catalogue", "A paragraph a lens stands on.\n")
	lensesBefore, _ := stored()
	refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/lenses", map[string]any{
		"on": "dispatch://" + issue.Key + "/spec", "kind": unknown,
	}, "alice")
	t.Logf("a lens of the kind %s: status=%d body=%s", unknown, refused.Code, refused.Body.String())
	if lensesAfter, tuples := stored(); tuples != 0 || lensesAfter != lensesBefore {
		t.Errorf("the unknown kind %s is stored: %d has_kind tuples point at lens_kind/%s, and the store holds %d lenses after the write, %d before it",
			unknown, tuples, unknown, lensesAfter, lensesBefore)
	}
	if refused.Code != http.StatusBadRequest {
		t.Errorf("a lens of the kind %s answers status=%d body=%s, want 400 INVALID_POINTER", unknown, refused.Code, refused.Body.String())
	} else if refusal := decodeBody[refusalAnswer](t, refused); refusal.Code != codeInvalidPointer || !strings.Contains(refusal.Error, unknown) {
		t.Errorf("a lens of the kind %s is refused %+v, want %s naming the kind", unknown, refusal, codeInvalidPointer)
	}

	if versionAfter := storeSchemaVersion(t, ctx, database); versionAfter != versionBefore {
		t.Errorf("the store's block schema version is %d after the add, want %d as before it", versionAfter, versionBefore)
	}
	if again.Version != read.Version {
		t.Errorf("the route answers block schema version %d after the add, want %d as before it", again.Version, read.Version)
	}
}

// requireEverySeededConstraintIsOneKindsSource holds every constraint node the migrated store
// holds - each one the seed's, since nothing else has written one - to being the far end of exactly
// one current constrained_by tuple from a lens_kind node, so the seed holds no constraint outside
// the catalogue's shape (cybermonklabs/legion#4177's project clause reads it as the catalogue).
func requireEverySeededConstraintIsOneKindsSource(t *testing.T, ctx context.Context, database *store.Store) {
	t.Helper()
	constraints, err := graphstore.NodesOfKind(ctx, database.Pool, graphstore.KindConstraint)
	if err != nil {
		t.Fatalf("read the constraint nodes: %v", err)
	}
	if len(constraints) == 0 {
		t.Fatal("the store holds no constraint node, so no source of the catalogue was read")
	}
	for _, constraint := range constraints {
		into, err := graphstore.CurrentIn(ctx, database.Pool, graphstore.KindConstraint, constraint.ID, []string{graphstore.TupleConstrainedBy})
		if err != nil {
			t.Fatalf("read the constrained_by tuples into %s: %v", constraint.ID, err)
		}
		fromKinds := 0
		for _, tuple := range into {
			if tuple.FromKind == graphstore.KindLensKind {
				fromKinds++
			}
		}
		if fromKinds != 1 || len(into) != 1 {
			t.Errorf("the seeded constraint %s is the far end of %d constrained_by tuples from a lens_kind node and %d in all, want exactly one, from a lens_kind",
				constraint.ID, fromKinds, len(into))
		}
	}
}

// storeSchemaVersion is the block schema version the store's type nodes build (schemanodes.Read),
// the version a server installs.
func storeSchemaVersion(t *testing.T, ctx context.Context, database *store.Store) int {
	t.Helper()
	schema, err := schemanodes.Read(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the store's block schema: %v", err)
	}
	return schema.Version
}
