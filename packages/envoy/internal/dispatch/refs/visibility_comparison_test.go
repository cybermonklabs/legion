package refs

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphseedtest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The two kinds the tests below register, one through each constructor of a kind that takes
// `author`, so each comparison and the flag are held to what they do before any kind
// cybermonklabs/legion#4193 adds is registered: a kind of that child registers through one of the two
// in the commit that adds it. Neither is a kind of the store, so each is registered only for the
// test that reads it (registerForTest), and the census of the registry
// (TestEveryNodeKindAnswersAVisibilityPredicate) never meets it.
const (
	authorComparedTestKind     = "refs_author_compared_test"
	credentialComparedTestKind = "refs_credential_compared_test"
)

// This test binary hands the store its two test kinds through the hand-off the registry's own
// init calls, once, as that init hands the kinds it flags: the store takes a node reserved for
// its author of an admitted kind alone.
func init() {
	graphstore.AdmitAuthorOnly(authorComparedTestKind, credentialComparedTestKind)
}

// thisChildsAuthorKinds are the kinds cybermonklabs/legion#4193 adds that take a node reserved for its
// author (R1): the kinds whose registry entry carries the flag, each once that child registers
// it. A lens_kind is a kind of that child too, and is always project-visible.
var thisChildsAuthorKinds = []string{
	graphstore.KindConstraint, graphstore.KindInteraction, graphstore.KindLens, graphstore.KindLensBlock, graphstore.KindRisk,
}

// registerForTest adds entries to the registry for one test, and takes them off when it ends, with
// the lists read off the registry (deriveRegistryLists) moved both times.
func registerForTest(t *testing.T, entries map[string]nodeKind) {
	t.Helper()
	for kind, entry := range entries {
		if _, held := nodeRegistry[kind]; held {
			t.Fatalf("the registry holds %s already, so this test would replace an entry of the build", kind)
		}
		nodeRegistry[kind] = entry
	}
	deriveRegistryLists()
	t.Cleanup(func() {
		for kind := range entries {
			delete(nodeRegistry, kind)
		}
		deriveRegistryLists()
	})
}

// TestTheKindsThatTakeAuthorAreThisChildsRegisteredKinds holds the registry's flag - every read of
// the kind binds its predicate, so the kind may hold a node reserved for its author - and the
// kinds cybermonklabs/legion#4193 registers to one list: the flagged kinds are exactly that child's
// author kinds the registry holds, so a flag on any other kind and a registered author kind with
// no flag each fail here, named, and neither list grows alone. A lens_kind is never flagged. The
// registry's init handed the store every flagged kind (graphstore.AdmitAuthorOnly), and the store
// admits no other kind but the two this test binary hands it itself. Only the two constructors of
// a kind that takes `author` set the flag, each with the comparison its name says.
func TestTheKindsThatTakeAuthorAreThisChildsRegisteredKinds(t *testing.T) {
	flagged := authorOnlyKinds()
	var registered []string
	for _, kind := range thisChildsAuthorKinds {
		if _, held := nodeRegistry[kind]; held {
			registered = append(registered, kind)
		}
	}
	slices.Sort(registered)
	t.Logf("the registry flags %v, and holds %v of the kinds %v that take author", flagged, registered, thisChildsAuthorKinds)
	if !slices.Equal(flagged, registered) {
		t.Errorf("the registry flags %v as kinds that take a node reserved for its author, and holds %v of %v: "+
			"the two are one list, each kind flagged in the commit that registers it", flagged, registered, thisChildsAuthorKinds)
	}
	if entry, held := nodeRegistry[graphstore.KindLensKind]; held && entry.authorOnly {
		t.Errorf("the registry flags %s, which is always project-visible", graphstore.KindLensKind)
	}
	// The kinds of that child registered so far, each as R1 and R19 have it: a constraint, a lens,
	// a finding of a lens and a risk are flagged and compare the author a credential proved, the
	// one rule of cybermonklabs/legion#3178, an interaction is flagged and compares the whole
	// credential, and a lens kind of the catalogue is registered unflagged. The first two are
	// written by migration 20261123174710, the next three by the reasoning write and an
	// interaction by the going POST, which is where they register. A finding is stored as a
	// block, so its entry reads the store under that kind, and every other entry under its own.
	for _, kind := range []string{graphstore.KindConstraint, graphstore.KindLens, graphstore.KindLensBlock, graphstore.KindRisk} {
		if entry, held := nodeRegistry[kind]; !held || !entry.authorOnly || entry.visibility.compares != comparesAuthor {
			t.Errorf("the registry holds %s as %+v (held %t), want it flagged and comparing the author a credential proved", kind, entry, held)
		}
	}
	if entry, held := nodeRegistry[graphstore.KindInteraction]; !held || !entry.authorOnly || entry.visibility.compares != comparesCredential ||
		entry.load == nil {
		t.Errorf("the registry holds %s as %+v (held %t), want it resolved, flagged and comparing the whole credential",
			graphstore.KindInteraction, entry, held)
	}
	if _, held := nodeRegistry[graphstore.KindLensKind]; !held {
		t.Errorf("the registry holds no %s, which the catalogue's migration writes", graphstore.KindLensKind)
	}
	for _, kind := range VisibilityKinds() {
		want := ""
		if kind == graphstore.KindLensBlock {
			want = BlockNodeKind
		}
		if stored := nodeRegistry[kind].visibility.storedKind; stored != want {
			t.Errorf("the registry reads %s's nodes under the stored kind %q, want %q", kind, stored, want)
		}
	}

	admitted := graphstore.AuthorOnlyKinds()
	want := slices.Sorted(slices.Values(append(slices.Clone(flagged), authorComparedTestKind, credentialComparedTestKind)))
	t.Logf("the store admits %v", admitted)
	if !slices.Equal(admitted, want) {
		t.Errorf("the store admits %v as kinds that take a node reserved for its author, want the registry's flagged kinds "+
			"and this binary's two test kinds, %v", admitted, want)
	}

	for _, constructed := range []struct {
		name     string
		entry    nodeKind
		flagged  bool
		compares comparison
	}{
		{"resolvedKind", resolvedKind(loadStoreNodes, nodeVisibility{}), false, comparesAuthor},
		{"unresolvedKind", unresolvedKind(), false, comparesAuthor},
		{"authoredKind", authoredKind(loadStoreNodes), true, comparesAuthor},
		{"credentialAuthoredKind", credentialAuthoredKind(loadStoreNodes), true, comparesCredential},
	} {
		if constructed.entry.authorOnly != constructed.flagged || constructed.entry.visibility.compares != constructed.compares ||
			!constructed.entry.stated {
			t.Errorf("%s makes an entry flagged %t comparing %d (stated %t), want flagged %t comparing %d",
				constructed.name, constructed.entry.authorOnly, constructed.entry.visibility.compares, constructed.entry.stated,
				constructed.flagged, constructed.compares)
		}
	}
}

// TestAKindComparesTheAuthorOrTheCredentialItsEntryStates holds each rendered predicate to the
// comparison the kind's registry entry states (spec requirement r-author, R19). A kind
// that compares the author a credential proved (authoredKind), the one rule every kind
// cybermonklabs/legion#3178 registers compares too (graphstore.AuthorIsSQL), answers a node reserved
// for its author to every credential of that author and to no other: to each credential that
// proves the person who wrote it - alice's cookie, her personal token and a session the broker
// verified as hers read what any of them wrote, under any spelling of her login, a capital outside
// ASCII among them, whose stored form and acting form go through one canonical form
// (user_threads_login) - and, where the author is no person, to each credential that proves that
// same session or that same workload: a session the broker proved (model.Actor's Proven) by its
// own id, and a session a pod's body named beside the workload its token proved as that workload.
// A session id only a body named, not proven and with no workload, proves nobody. A kind that
// compares the whole credential (credentialAuthoredKind), an interaction's, answers the node to
// the credential that wrote it alone, so alice's cookie reads nothing of what her token recorded,
// and a session's credential that names a workload too reads nothing that session wrote without
// it. A node whose author is nobody is answered to nobody under either comparison, and a project
// node to everybody.
//
// Every form the registry renders is read for each node and each reader - the loader's own read
// (Node, which binds VisibleRow), VisibleKind, VisibleKindRow, VisibleEnd and VisibleNode - and
// each answers the same.
func TestAKindComparesTheAuthorOrTheCredentialItsEntryStates(t *testing.T) {
	registerForTest(t, map[string]nodeKind{
		authorComparedTestKind:     authoredKind(loadStoreNodes),
		credentialComparedTestKind: credentialAuthoredKind(loadStoreNodes),
	})
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	// The two test kinds are types of no migration, and the store writes a node only of a type it
	// holds, so their types are seeded first.
	if err := graphseedtest.SeedSupertags(ctx, tx, authorComparedTestKind, credentialComparedTestKind); err != nil {
		t.Fatalf("seed the test kinds' types: %v", err)
	}
	name := func(login string) *string { return &login }

	aliceCookie := model.Actor{Kind: "user", ID: "alice"}
	aliceToken := model.Actor{Kind: "session", Owner: name("alice")}
	aliceSession := model.Actor{Kind: "session", ID: "session-the-broker-bound-to-alice", Owner: name("alice")}
	// A login whose capital is outside ASCII: model.CanonicalLogin folds ASCII letters alone, and
	// user_threads_login lowers every letter, so a comparison that put one side through each would
	// hide her own nodes from her.
	angstromCookie := model.Actor{Kind: "user", ID: "Ångström"}
	bobCookie := model.Actor{Kind: "user", ID: "bob"}
	bobToken := model.Actor{Kind: "session", Owner: name("bob")}
	workload := model.Actor{Service: name("system:serviceaccount:legion:worker")}
	// A session id a pod's body named beside the workload its token proved, not proven: that
	// workload is its author.
	podNamed := model.Actor{Kind: "session", ID: "session-a-pod-named", Service: name("system:serviceaccount:legion:worker")}
	// A session the broker proved that nobody owns proves its own id, and so does that session
	// where the broker proved it for a workload: one author, two credentials.
	unowned := model.Actor{Kind: "session", ID: "session-nobody-owns", Proven: true}
	unownedForWorkload := model.Actor{Kind: "session", ID: "session-nobody-owns", Service: name("system:serviceaccount:legion:worker"),
		Proven: true}
	// A session id only a body named, not proven and with no workload, proves nobody.
	bodyNamed := model.Actor{Kind: "session", ID: "session-a-body-named"}
	shared := model.Actor{}
	readers := []struct {
		name string
		who  model.Actor
	}{
		{"alice's cookie", aliceCookie}, {"alice's personal token", aliceToken}, {"alice's proved session", aliceSession},
		{"Ångström's cookie", angstromCookie}, {"bob's cookie", bobCookie}, {"bob's personal token", bobToken},
		{"a workload", workload}, {"a session a pod named", podNamed},
		{"the session nobody owns", unowned}, {"that session, proved for a workload", unownedForWorkload},
		{"a session only a body named", bodyNamed}, {"the shared bearer", shared},
	}
	alices := []string{"alice's cookie", "alice's personal token", "alice's proved session"}
	unowneds := []string{"the session nobody owns", "that session, proved for a workload"}

	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	node := func(kind, id, visibility string, madeBy model.Actor) graphstore.Node {
		return graphstore.Node{
			Kind: kind, ID: id, Supertag: kind, Visibility: visibility, State: graphstore.StateLive,
			MadeAt: at, MadeBy: madeBy, SourceEvent: graphstore.NoEvent,
		}
	}
	nodes := []struct {
		name string
		node graphstore.Node
		// readBy are the readers answered the node, by name.
		readBy []string
	}{
		{"a project node of the author kind", node(authorComparedTestKind, "project", graphstore.VisibilityProject, aliceCookie),
			[]string{"alice's cookie", "alice's personal token", "alice's proved session", "Ångström's cookie", "bob's cookie",
				"bob's personal token", "a workload", "a session a pod named", "the session nobody owns",
				"that session, proved for a workload", "a session only a body named", "the shared bearer"}},
		{"the author kind, written by alice's cookie", node(authorComparedTestKind, "by-cookie", graphstore.VisibilityAuthor, aliceCookie), alices},
		{"the author kind, written by alice's personal token", node(authorComparedTestKind, "by-token", graphstore.VisibilityAuthor, aliceToken), alices},
		{"the author kind, written by alice's proved session", node(authorComparedTestKind, "by-session", graphstore.VisibilityAuthor, aliceSession), alices},
		{"the author kind, written by alice's cookie under a login in mixed case", node(authorComparedTestKind, "by-spelling", graphstore.VisibilityAuthor,
			model.Actor{Kind: "user", ID: "  Alice "}), alices},
		{"the author kind, written by Ångström's cookie", node(authorComparedTestKind, "by-angstrom", graphstore.VisibilityAuthor, angstromCookie),
			[]string{"Ångström's cookie"}},
		{"the author kind, written by a workload", node(authorComparedTestKind, "by-workload", graphstore.VisibilityAuthor, workload),
			[]string{"a workload", "a session a pod named"}},
		{"the author kind, written by a session a pod named", node(authorComparedTestKind, "by-pod-named", graphstore.VisibilityAuthor, podNamed),
			[]string{"a workload", "a session a pod named"}},
		{"the author kind, written by the session nobody owns", node(authorComparedTestKind, "by-unowned", graphstore.VisibilityAuthor, unowned), unowneds},
		{"the author kind, written by a session only a body named", node(authorComparedTestKind, "by-body-named", graphstore.VisibilityAuthor, bodyNamed), nil},
		{"the author kind, written by nobody", node(authorComparedTestKind, "by-nobody", graphstore.VisibilityAuthor, shared), nil},
		{"the credential kind, written by alice's personal token", node(credentialComparedTestKind, "by-token", graphstore.VisibilityAuthor, aliceToken),
			[]string{"alice's personal token"}},
		{"the credential kind, written by alice's cookie", node(credentialComparedTestKind, "by-cookie", graphstore.VisibilityAuthor, aliceCookie),
			[]string{"alice's cookie"}},
		{"the credential kind, written by the session nobody owns", node(credentialComparedTestKind, "by-unowned", graphstore.VisibilityAuthor, unowned),
			[]string{"the session nobody owns"}},
		{"the credential kind, written by a session only a body named", node(credentialComparedTestKind, "by-body-named", graphstore.VisibilityAuthor, bodyNamed), nil},
		{"the credential kind, written by nobody", node(credentialComparedTestKind, "by-nobody", graphstore.VisibilityAuthor, shared), nil},
		{"a comment, written by alice's cookie", node(graphstore.KindComment, "11111111-1111-4111-8111-111111111111", graphstore.VisibilityAuthor, aliceCookie), alices},
		// The kinds of cybermonklabs/legion#4193 this build registers with the author comparison, a
		// finding among them, which the store keys as a block and every predicate reads under that
		// kind, so a finding reserved for its author is no more visible than a lens.
		{"a lens, written by alice's personal token", node(graphstore.KindLens, "22222222-2222-4222-8222-222222222222", graphstore.VisibilityAuthor, aliceToken), alices},
		{"a finding of a lens, written by alice's cookie", node(BlockNodeKind, graphstore.KindLens+"/22222222-2222-4222-8222-222222222222#f1", graphstore.VisibilityAuthor, aliceCookie),
			alices},
		{"a risk, written by bob's cookie", node(graphstore.KindRisk, "33333333-3333-4333-8333-333333333333", graphstore.VisibilityAuthor, bobCookie),
			[]string{"bob's cookie", "bob's personal token"}},
		{"a constraint, written by the session nobody owns", node(graphstore.KindConstraint, "44444444-4444-4444-8444-444444444444", graphstore.VisibilityAuthor, unowned),
			unowneds},
	}
	for _, written := range nodes {
		// The store takes the two test kinds at author, which this binary admits; a comment it
		// refuses (graphstore.ErrVisibilityNotAdmitted), so a comment's rows are seeded as such
		// a write would have stored them.
		if written.node.Kind == graphstore.KindComment {
			if err := graphseedtest.SeedAuthorOnly(ctx, tx, written.node); err != nil {
				t.Fatalf("seed %s: %v", written.name, err)
			}
			continue
		}
		if err := graphstore.Write(ctx, tx, []graphstore.Node{written.node}, nil); err != nil {
			t.Fatalf("write %s: %v", written.name, err)
		}
	}

	forms := []struct {
		name string
		// storeKinds is whether the form reads only the kinds the store holds rows of alone,
		// which the loader's own read does: a comment's loader reads its comments row, which
		// this fixture holds none of, so the four rendered forms are a comment's predicate here.
		storeKinds bool
		// read answers whether the form finds the node, given the kind the graph addresses it by
		// and the kind the store keys it by, which differ for a finding of a lens alone.
		read func(t *testing.T, who model.Actor, kind, stored, id string) bool
	}{
		{"the loader's read (Node)", true, func(t *testing.T, who model.Actor, kind, _, id string) bool {
			_, found, err := Node(ctx, tx, who, kind, id, true)
			if err != nil {
				t.Fatalf("read %s %s: %v", kind, id, err)
			}
			return found
		}},
		{"VisibleKind", false, func(t *testing.T, who model.Actor, kind, _, id string) bool {
			return predicateHolds(t, ctx, tx, `select `+VisibleKind(kind, "$1", "$2"), id, VisibilityArg(who))
		}},
		{"VisibleKindRow", false, func(t *testing.T, who model.Actor, kind, stored, id string) bool {
			return predicateHolds(t, ctx, tx, `select exists (select 1 from graph_nodes n where n.kind = $3 and n.id = $1 and `+
				VisibleKindRow(kind, "n", "$2")+`)`, id, VisibilityArg(who), stored)
		}},
		{"VisibleEnd", false, func(t *testing.T, who model.Actor, kind, _, id string) bool {
			return predicateHolds(t, ctx, tx, `select `+VisibleEnd(AnyKind, "$3::text", "$1", true, "$2"), id, VisibilityArg(who), kind)
		}},
		{"VisibleNode", false, func(t *testing.T, who model.Actor, _, stored, id string) bool {
			return predicateHolds(t, ctx, tx, `select `+VisibleNode("$3::text", "$1", "$2"), id, VisibilityArg(who), stored)
		}},
	}
	for _, written := range nodes {
		kind := graphstore.AddressKind(written.node.Kind, written.node.ID)
		for _, reader := range readers {
			want := slices.Contains(written.readBy, reader.name)
			for _, form := range forms {
				if form.storeKinds && written.node.Kind == graphstore.KindComment {
					continue
				}
				got := form.read(t, reader.who, kind, written.node.Kind, written.node.ID)
				t.Logf("%s, read by %s through %s: found %t", written.name, reader.name, form.name, got)
				if got != want {
					t.Errorf("%s, read by %s through %s, answers found %t, want %t", written.name, reader.name, form.name, got, want)
				}
			}
		}
	}
}

// predicateHolds runs one statement that answers whether a rendered predicate holds.
func predicateHolds(t *testing.T, ctx context.Context, tx pgx.Tx, statement string, args ...any) bool {
	t.Helper()
	var holds bool
	if err := tx.QueryRow(ctx, statement, args...).Scan(&holds); err != nil {
		t.Fatalf("run %s: %v", statement, err)
	}
	return holds
}
