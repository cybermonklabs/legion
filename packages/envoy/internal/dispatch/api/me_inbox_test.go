package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The relation kinds these tests seed, spelled here because the read under test spells none of
// them (c-one-relation's grep of me_inbox.go holds it to that).
const (
	meInboxDelivered    = graphstore.TupleDeliveredTo
	meInboxAuthored     = graphstore.TupleAuthoredBy
	meInboxParticipates = graphstore.TupleParticipatesIn
	meInboxNamed        = graphstore.TupleMentions
)

// meInboxRig is a test server whose store is traced for the one read's own statements: once a
// request's credential has resolved to a person (meInboxWindowIdentity) meInboxStatements records
// every statement the traced pool runs, on whatever context, each statement a batch sends on its
// own, until the test's call of the handler returns (windowedRead). The caller's identity is resolved
// on the untraced pool and before that window opens, so a sign-in lookup is no statement of the read.
// The server's event horizon observes through the traced pool's horizon pool with each statement
// it sends there marked (markedHorizonPool), so the horizon's own are told from the read's
// (ownStatements).
type meInboxRig struct {
	database   *store.Store
	handler    http.Handler
	statements *meInboxStatements
}

func newMeInboxRig(t *testing.T) *meInboxRig {
	t.Helper()
	database := storetest.Open(t)
	statements := &meInboxStatements{eachOfABatch: true, windowed: true}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, statements, nil)}
	handler, _, _ := newTestServer(t, testServerOptions{
		store:     traced,
		identity:  meInboxWindowIdentity{HeaderIdentity: headerIdentity(database), statements: statements},
		horizonDB: markedHorizonPool(t, traced),
		// A document's settlement is database work of its own on the traced pool; a delay no
		// test reaches keeps it out of the read's window.
		settle: time.Hour,
	})
	return &meInboxRig{database: database, handler: handler, statements: statements}
}

// windowedRead sends the read as login with the window armed, and answers its response and every
// statement the traced pool ran from the moment login's credential resolved until the handler
// returned, on whatever context and whichever connection it ran, less the horizon's own
// (ownStatements).
func (rig *meInboxRig) windowedRead(t *testing.T, login string, query url.Values) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	rig.statements.arm()
	response := meInboxRequestAs(t, rig.handler, context.Background(), login, query)
	rig.statements.closeWindow()
	return response, rig.statements.ownStatements()
}

// meInboxWindowIdentity is the rig's identity: the header identity, over the untraced store, which
// opens an armed window of its statements once it has resolved the caller to a person.
type meInboxWindowIdentity struct {
	identity.HeaderIdentity
	statements *meInboxStatements
}

func (i meInboxWindowIdentity) Login(r *http.Request) (string, error) {
	login, err := i.HeaderIdentity.Login(r)
	if err == nil {
		i.statements.openWindow()
	}
	return login, err
}

// meInboxStatements records statements a traced pool runs, through a storetest.StatementRecorder,
// each beside whether the server's event horizon ran it (horizonObservation). windowed records
// every statement the pool runs while its window is open, whatever context it runs on, so a
// statement a handler sends on a context of its own is recorded as surely as one on its request's:
// the test arms the window (arm), the rig's identity opens it once the credential has resolved to
// a person (openWindow), and the test closes it when the handler has returned (closeWindow).
// Nothing else of the server runs on the pool in that window: the rig's sign-in reads the untraced
// pool, a document's settlement waits an hour, and the server's workers - the mention sender's
// claim refresh, the delivery hub and the event relay - read it only for a mention, a socket or a
// subscription, none of which the case opens. Otherwise it records the statements of a request
// marked countedRead, which is what c-p99's statement rule counts (oneInboxHere). A batch is one
// round trip whatever it holds, so by default it is recorded as the one batch it is; with
// eachOfABatch it is recorded as each statement it sends, in the order it queued them
// (pgx.TraceBatchStartData's batch), so a read that sent four statements as one batch is four
// statements and not one. The horizon observes through a pool of its own (events.Horizon,
// store.Pool.Horizon), which is traced too since it is opened from this pool's configuration; a
// rig's horizon marks the context of each statement it sends there (markedHorizonPool), and
// ownStatements leaves out the statements so marked and no other, so a statement anything else
// sends on that pool's connections counts.
type meInboxStatements struct {
	recorder     storetest.StatementRecorder
	eachOfABatch bool
	windowed     bool
	mu           sync.Mutex
	// armed is set for the next credential that resolves (arm), and open from then until
	// closeWindow.
	armed, open bool
	// observed is, beside each recorded statement, whether its context carries the horizon's
	// mark (horizonObservation).
	observed []bool
	// taken is how many of the recorded statements ownStatements has answered already.
	taken int
}

// arm has the next credential that resolves open the window.
func (s *meInboxStatements) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = true
}

// openWindow opens the window the test armed, from the moment a credential resolved.
func (s *meInboxStatements) openWindow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.armed {
		s.open = true
	}
}

// closeWindow ends the window, once the handler has returned.
func (s *meInboxStatements) closeWindow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed, s.open = false, false
}

func (s *meInboxStatements) record(ctx context.Context, conn *pgx.Conn, sql string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.windowed && !s.open || !s.windowed && ctx.Value(countedRead{}) == nil {
		return
	}
	s.recorder.TraceQueryStart(ctx, conn, pgx.TraceQueryStartData{SQL: sql})
	s.observed = append(s.observed, ctx.Value(horizonObservation{}) != nil)
}

func (s *meInboxStatements) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.record(ctx, conn, data.SQL)
	return ctx
}

func (s *meInboxStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (s *meInboxStatements) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if !s.eachOfABatch {
		s.record(ctx, conn, fmt.Sprintf("a batch of %d statements", data.Batch.Len()))
		return ctx
	}
	for _, queued := range data.Batch.QueuedQueries {
		s.record(ctx, conn, queued.SQL)
	}
	return ctx
}

func (s *meInboxStatements) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (s *meInboxStatements) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// ownStatements is every statement recorded since the last call but the horizon's own, those whose
// context the rig's horizon marked (horizonObservation): every other statement counts, whichever
// pool's connection ran it, the horizon pool's included.
func (s *meInboxStatements) ownStatements() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	recorded := s.recorder.Statements()
	var own []string
	for index := s.taken; index < len(recorded); index++ {
		if !s.observed[index] {
			own = append(own, recorded[index])
		}
	}
	s.taken = len(recorded)
	return own
}

// meInboxReadStatements is statements less the four that frame the read's statement in its
// batch and read nothing (readMeInbox): the read-only transaction's begin, each of
// meInboxSettings and its commit.
func meInboxReadStatements(statements []string) []string {
	framing := append([]string{`begin transaction read only`, `commit`}, meInboxSettings...)
	reads := make([]string, 0, len(statements))
	for _, statement := range statements {
		if !slices.Contains(framing, statement) {
			reads = append(reads, statement)
		}
	}
	return reads
}

// horizonObservation marks the context of each statement the event horizon of a rig's server runs
// (markedHorizon), and of no other statement.
type horizonObservation struct{}

// markedHorizon is the pool a rig's event horizon observes through, which sends each statement
// the horizon runs - its observations, the reads beside them, its stall diagnosis and its write of
// the horizon it proved - on a context marked horizonObservation. A statement anything else sends
// on the same pool goes through the pool itself, unmarked.
type markedHorizon struct{ events.HorizonDB }

func (m markedHorizon) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	return m.HorizonDB.SendBatch(context.WithValue(ctx, horizonObservation{}, true), b)
}

func (m markedHorizon) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return m.HorizonDB.QueryRow(context.WithValue(ctx, horizonObservation{}, true), sql, args...)
}

// markedHorizonPool is traced's horizon pool, wrapped so each statement the event horizon sends
// through it is marked (markedHorizon), for a server to observe through (Deps.HorizonDB) so a
// count of the traced store's statements tells the horizon's own from every other
// (ownStatements). Each rig whose meInboxStatements counts a server's statements (ownStatements)
// gives its server its horizon through it: the one read's (newMeInboxRig), the gate's
// (newGateRig) and c-p99's (bootOneInboxHere).
func markedHorizonPool(t *testing.T, traced *store.Store) events.HorizonDB {
	t.Helper()
	pool, err := traced.Pool.Horizon()
	if err != nil {
		t.Fatalf("open the traced store's horizon pool: %v", err)
	}
	return markedHorizon{pool}
}

// readMeInboxAs is one read of GET /api/v1/me/inbox as login, query its query string.
func readMeInboxAs(t *testing.T, handler http.Handler, login string, query url.Values) meInboxResponse {
	t.Helper()
	return decodeMeInbox(t, meInboxRequestAs(t, handler, context.Background(), login, query))
}

// meInboxRequestAs sends the read on ctx and answers the recorder.
func meInboxRequestAs(t *testing.T, handler http.Handler, ctx context.Context, login string, query url.Values) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/v1/me/inbox"
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", login)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeMeInbox(t *testing.T, response *httptest.ResponseRecorder) meInboxResponse {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("read the inbox: status %d, body %s", response.Code, response.Body.String())
	}
	var answer meInboxResponse
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the inbox %s: %v", response.Body.String(), err)
	}
	return answer
}

// meInboxSeed writes nodes and tuples straight into the store, in one transaction of its own.
type meInboxSeed struct {
	t        *testing.T
	database *store.Store
}

// node writes one live node of kind, made by author at at.
func (seed meInboxSeed) node(kind, id, visibility string, author model.Actor, at time.Time) {
	seed.t.Helper()
	seed.write(func(ctx context.Context, tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, []graphstore.Node{{
			Kind: kind, ID: id, Supertag: kind, Visibility: visibility, State: graphstore.StateLive,
			MadeAt: at, MadeBy: author, SourceEvent: graphstore.NoEvent,
		}}, nil)
	})
}

// toPerson appends tuples through the one writer of the three kinds that name a person.
func (seed meInboxSeed) toPerson(tuples ...graphstore.Tuple) {
	seed.t.Helper()
	seed.write(func(ctx context.Context, tx pgx.Tx) error { return graphstore.ToPerson(ctx, tx, tuples) })
}

// tuples appends tuples through graphstore.Write, the writer of every kind ToPerson does not own.
func (seed meInboxSeed) tuples(tuples ...graphstore.Tuple) {
	seed.t.Helper()
	seed.write(func(ctx context.Context, tx pgx.Tx) error { return graphstore.Write(ctx, tx, nil, tuples) })
}

func (seed meInboxSeed) write(write func(context.Context, pgx.Tx) error) {
	seed.t.Helper()
	if err := pgx.BeginFunc(context.Background(), seed.database.Pool, func(tx pgx.Tx) error {
		return write(context.Background(), tx)
	}); err != nil {
		seed.t.Fatalf("seed the store: %v", err)
	}
}

// meInboxTuple is one asserted tuple at at, written under no event.
func meInboxTuple(fromKind, fromID, kind, toKind, toID string, at time.Time) graphstore.Tuple {
	return graphstore.Tuple{
		FromKind: fromKind, FromID: fromID, Kind: kind, ToKind: toKind, ToID: toID,
		At: at, Who: model.Actor{Kind: "system", ID: "me-inbox-test"}, SourceEvent: graphstore.NoEvent, Asserted: true,
	}
}

// meInboxEntryOfNode is the entry of answer whose node is kind/id, and whether there is one.
func meInboxEntryOfNode(answer meInboxResponse, kind, id string) (meInboxEntry, bool) {
	for _, entry := range answer.Entries {
		if entry.Node.Kind == kind && entry.Node.ID == id {
			return entry, true
		}
	}
	return meInboxEntry{}, false
}

// meInboxNodes is every node of answer, as kind:id, in its order.
func meInboxNodes(answer meInboxResponse) []string {
	nodes := make([]string, 0, len(answer.Entries))
	for _, entry := range answer.Entries {
		nodes = append(nodes, entry.Node.Kind+":"+entry.Node.ID)
	}
	return nodes
}

// meInboxKinds is every relation of entry as kind/direction, in its order.
func meInboxKinds(entry meInboxEntry) []string {
	kinds := make([]string, 0, len(entry.Relations))
	for _, relation := range entry.Relations {
		kinds = append(kinds, relation.Kind+"/"+relation.Direction)
	}
	return kinds
}

// c-one-relation: the read takes the person's node and answers the nodes with a relation to it,
// of any kind, in one statement a reader can observe, each statement of its batch counted. Four
// nodes each tie the person by one of the four arms - a delivery, their own writing, their taking
// part, a mention of them - and a fifth ties them by nothing; a second person is reached by all
// five, a sixth node by a kind no writer of this build appends, and a placement moves under one of
// them. Last, the read answers at the horizon notifications read at (meInboxAtTheHorizon).
func TestOneReadAnswersEveryRelationToThePersonsNode(t *testing.T) {
	// Not parallel: its case at the horizon (meInboxAtTheHorizon) holds a transaction id on the
	// cluster, which holds back the event horizon of every test beside it, and needs the read after
	// a comment written while no other transaction holds an id to answer that comment, which a test
	// beside it creating or dropping its database (storetest.Open) can keep from it past
	// referencesSinceWait.
	rig := newMeInboxRig(t)
	seed := meInboxSeed{t: t, database: rig.database}
	alice, bob := "alice", "bob"
	writer := model.Actor{Kind: "user", ID: "carol"}
	base := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	at := func(minute int) time.Time { return base.Add(time.Duration(minute) * time.Minute) }
	type seeded struct {
		kind, id string
	}
	delivered := seeded{graphstore.KindComment, uuid.NewString()}
	written := seeded{graphstore.KindComment, uuid.NewString()}
	answered := seeded{graphstore.KindAsk, uuid.NewString()}
	named := seeded{graphstore.KindComment, uuid.NewString()}
	untied := seeded{graphstore.KindComment, uuid.NewString()}
	for index, node := range []seeded{delivered, written, answered, named, untied} {
		seed.node(node.kind, node.id, graphstore.VisibilityProject, writer, at(index))
	}
	// The placement of the delivered comment, under its issue: a part_of out of it, which is no
	// relation to anybody.
	seed.tuples(meInboxTuple(delivered.kind, delivered.id, graphstore.TuplePartOf, graphstore.KindIssue, "CORE-1", at(5)))
	seed.write(func(ctx context.Context, tx pgx.Tx) error {
		return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			return graphstore.QueueAuthored(b, written.kind, written.id, model.Actor{Kind: "user", ID: alice}, at(11), graphstore.NoEvent)
		})
	})
	seed.toPerson(
		meInboxTuple(delivered.kind, delivered.id, meInboxDelivered, graphstore.KindPerson, alice, at(10)),
		meInboxTuple(graphstore.KindPerson, alice, meInboxParticipates, answered.kind, answered.id, at(12)),
		meInboxTuple(named.kind, named.id, meInboxNamed, graphstore.KindPerson, alice, at(13)),
	)
	// The second person is reached by every one of the five nodes.
	reachedBob := make([]graphstore.Tuple, 0, 5)
	for index, node := range []seeded{delivered, written, answered, named, untied} {
		reachedBob = append(reachedBob, meInboxTuple(node.kind, node.id, meInboxDelivered, graphstore.KindPerson, bob, at(20+index)))
	}
	seed.toPerson(reachedBob...)

	t.Run("one statement answers the four arms and not the node with no relation", func(t *testing.T) {
		response, own := rig.windowedRead(t, alice, nil)
		answer := decodeMeInbox(t, response)
		// Every statement the traced pool ran from the moment alice's credential resolved until
		// the handler returned counts, on whatever context and whichever connection it ran, each of
		// a batch on its own, but for the horizon's own (ownStatements) and the four that frame the
		// read's statement in its read-only transaction and read nothing.
		if reads := meInboxReadStatements(own); len(reads) != 1 {
			t.Fatalf("the read ran %d statements, want 1; every statement the pool ran while it was served:\n%s", len(reads), strings.Join(own, "\n---\n"))
		}
		want := []struct {
			node      seeded
			kind      string
			direction string
			at        time.Time
		}{
			{named, meInboxNamed, meInboxInbound, at(13)},
			{answered, meInboxParticipates, meInboxOutbound, at(12)},
			{written, meInboxAuthored, meInboxInbound, at(11)},
			{delivered, meInboxDelivered, meInboxInbound, at(10)},
		}
		if answer.Person != alice || answer.Total != len(want) || len(answer.Entries) != len(want) {
			t.Fatalf("alice's read = person %q, total %d, entries %v; want alice, %d entries", answer.Person, answer.Total, meInboxNodes(answer), len(want))
		}
		for index, expected := range want {
			entry := answer.Entries[index]
			if entry.Node != (meInboxNode{Kind: expected.node.kind, ID: expected.node.id}) {
				t.Errorf("entry %d is %s:%s, want %s:%s (newest moment first)", index, entry.Node.Kind, entry.Node.ID, expected.node.kind, expected.node.id)
				continue
			}
			wantRelations := []meInboxRelation{{Kind: expected.kind, Direction: expected.direction, At: expected.at}}
			if !reflect.DeepEqual(entry.Relations, wantRelations) || !entry.Moment.Equal(expected.at) {
				t.Errorf("%s:%s carries %+v at %s, want %+v at %s", entry.Node.Kind, entry.Node.ID, entry.Relations, entry.Moment, wantRelations, expected.at)
			}
		}
		if _, held := meInboxEntryOfNode(answer, untied.kind, untied.id); held {
			t.Errorf("the node with no relation to alice is an entry of her read: %v", meInboxNodes(answer))
		}
	})

	t.Run("each person's read holds their own relations and none of the other's", func(t *testing.T) {
		answer := readMeInboxAs(t, rig.handler, bob, nil)
		if answer.Total != 5 {
			t.Fatalf("bob's read holds %v, want the five nodes that reached him", meInboxNodes(answer))
		}
		for _, entry := range answer.Entries {
			if kinds := meInboxKinds(entry); !slices.Equal(kinds, []string{meInboxDelivered + "/" + meInboxInbound}) {
				t.Errorf("bob's entry %s:%s carries %v, want his own delivery alone", entry.Node.Kind, entry.Node.ID, kinds)
			}
		}
		for _, entry := range readMeInboxAs(t, rig.handler, alice, nil).Entries {
			for _, relation := range entry.Relations {
				if relation.Kind == meInboxDelivered && !relation.At.Equal(at(10)) {
					t.Errorf("alice's entry %s:%s carries bob's delivery at %s", entry.Node.Kind, entry.Node.ID, relation.At)
				}
			}
		}
	})

	t.Run("a kind no writer of this build appends is an entry of the same read", func(t *testing.T) {
		const unheard = "t10_never_heard_of"
		sixth := seeded{graphstore.KindComment, uuid.NewString()}
		seed.node(sixth.kind, sixth.id, graphstore.VisibilityProject, writer, at(30))
		seed.tuples(meInboxTuple(sixth.kind, sixth.id, unheard, graphstore.KindPerson, alice, at(31)))
		answer := readMeInboxAs(t, rig.handler, alice, nil)
		entry, held := meInboxEntryOfNode(answer, sixth.kind, sixth.id)
		if !held {
			known := map[string]bool{}
			for _, entry := range answer.Entries {
				for _, relation := range entry.Relations {
					known[relation.Kind] = true
				}
			}
			t.Fatalf("after a %s tuple into alice, the read answers no entry for %s:%s; the kinds it knows are %v",
				unheard, sixth.kind, sixth.id, slices.Sorted(maps.Keys(known)))
		}
		if kinds := meInboxKinds(entry); !slices.Equal(kinds, []string{unheard + "/" + meInboxInbound}) {
			t.Errorf("the sixth node carries %v, want %s", kinds, unheard)
		}
	})

	t.Run("the file that holds the read names none of the four kinds", func(t *testing.T) {
		source, err := os.ReadFile("me_inbox.go")
		if err != nil {
			t.Fatalf("read me_inbox.go: %v", err)
		}
		text := string(source)
		// A kind is named by its literal or by the graphstore constant that spells it. The file
		// names one of the four, the delivery kind, once and for an index's sake, in the predicate
		// the reads out of the person's nodes state beside the kind they bind (meInboxOutOfPerson),
		// which puts no kind in the answer; that declaration is taken out before the search.
		text = strings.Replace(text, "const meInboxOutOfPerson = `t.kind <> '` + graphstore.TupleDeliveredTo + `'`", "", 1)
		for _, kind := range []struct{ literal, constant string }{
			{meInboxDelivered, "TupleDeliveredTo"}, {meInboxAuthored, "TupleAuthoredBy"},
			{meInboxParticipates, "TupleParticipatesIn"}, {meInboxNamed, "TupleMentions"},
		} {
			if strings.Contains(text, kind.literal) {
				t.Errorf("me_inbox.go names the relation kind %q: the read takes its kinds from the store", kind.literal)
			}
			if strings.Contains(text, "graphstore."+kind.constant) {
				t.Errorf("me_inbox.go names the relation kind %q as graphstore.%s: the read takes its kinds from the store", kind.literal, kind.constant)
			}
		}
	})

	t.Run("two relations to one node are one entry, and a placement moves no relation", func(t *testing.T) {
		parent := func() []string {
			t.Helper()
			tuples, err := graphstore.CurrentOut(context.Background(), rig.database.Pool, delivered.kind, delivered.id, []string{graphstore.TuplePartOf})
			if err != nil {
				t.Fatalf("read the delivered comment's parent: %v", err)
			}
			parents := make([]string, 0, len(tuples))
			for _, tuple := range tuples {
				parents = append(parents, tuple.ToKind+":"+tuple.ToID)
			}
			return parents
		}
		before := parent()
		seed.toPerson(meInboxTuple(delivered.kind, delivered.id, meInboxNamed, graphstore.KindPerson, alice, at(40)))
		if after := parent(); !slices.Equal(before, after) || !slices.Equal(after, []string{"issue:CORE-1"}) {
			t.Fatalf("the delivered comment's parent is %v after a mention of alice, %v before; want issue:CORE-1 both times", after, before)
		}
		bothKinds := []string{meInboxDelivered + "/" + meInboxInbound, meInboxNamed + "/" + meInboxInbound}
		entryKinds := func() []string {
			t.Helper()
			answer := readMeInboxAs(t, rig.handler, alice, nil)
			count := 0
			for _, entry := range answer.Entries {
				if entry.Node.ID == delivered.id {
					count++
				}
			}
			entry, held := meInboxEntryOfNode(answer, delivered.kind, delivered.id)
			if !held || count != 1 {
				t.Fatalf("the delivered comment is %d entries of alice's read, want one: %v", count, meInboxNodes(answer))
			}
			return meInboxKinds(entry)
		}
		if kinds := entryKinds(); !slices.Equal(kinds, bothKinds) {
			t.Fatalf("the delivered comment carries %v, want %v", kinds, bothKinds)
		}
		seed.tuples(
			graphstore.Tuple{
				FromKind: delivered.kind, FromID: delivered.id, Kind: graphstore.TuplePartOf, ToKind: graphstore.KindIssue, ToID: "CORE-1",
				At: at(41), Who: writer, SourceEvent: graphstore.NoEvent, Asserted: false,
			},
			meInboxTuple(delivered.kind, delivered.id, graphstore.TuplePartOf, graphstore.KindIssue, "CORE-2", at(41)),
		)
		if moved := parent(); !slices.Equal(moved, []string{"issue:CORE-2"}) {
			t.Fatalf("the delivered comment's parent is %v after the move, want issue:CORE-2", moved)
		}
		if kinds := entryKinds(); !slices.Equal(kinds, bothKinds) {
			t.Errorf("after the move the delivered comment carries %v, want %v: a move writes no relation to a person", kinds, bothKinds)
		}
	})

	t.Run("a retracted tuple takes its node out", func(t *testing.T) {
		retraction := meInboxTuple(graphstore.KindPerson, alice, meInboxParticipates, answered.kind, answered.id, at(50))
		retraction.Asserted = false
		seed.toPerson(retraction)
		if _, held := meInboxEntryOfNode(readMeInboxAs(t, rig.handler, alice, nil), answered.kind, answered.id); held {
			t.Errorf("the ask alice no longer takes part in is still an entry of her read")
		}
	})

	t.Run("the read answers at the safe horizon notifications read at", meInboxAtTheHorizon)
}

// meInboxAtTheHorizon is the horizon of the one read: it reads at the event horizon notifications
// read at (sinceHorizon), as the event read of GET /api/v1/events does, and answers that horizon.
// Six writes through the routes each reach the person, one of each kind that does, the person
// named by a login a person address takes (mentionedPerson) so a comment can mention them; every
// tuple into or out of their node that an event at or below the answered horizon appended puts its
// node among the read's entries, whatever that event's type and issue. A comment of theirs written
// while no other transaction is open is an entry of the next read. A comment written to them while
// an event write of Dispatch's own database stays open (holdEventWrite) is above the horizon the
// read answers then and no entry of it, and is an entry once that write ends.
func meInboxAtTheHorizon(t *testing.T) {
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	ctx := context.Background()
	const person = mentionedPerson
	// reaches runs write, which the case names what, and requires it to append a tuple under an
	// event into or out of the person's node.
	reaches := func(what string, write func()) {
		t.Helper()
		var before int64
		if err := database.Pool.QueryRow(ctx, `select coalesce(max(seq), 0) from graph_tuples`).Scan(&before); err != nil {
			t.Fatalf("read the store's last tuple before %s: %v", what, err)
		}
		write()
		var appended int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from graph_tuples
			where seq > $1 and source_event <> 0
			  and ((to_kind = $2 and to_id = $3) or (from_kind = $2 and from_id = $3))
		`, before, graphstore.KindPerson, person).Scan(&appended); err != nil {
			t.Fatalf("count the tuples %s appended to %s: %v", what, person, err)
		}
		if appended == 0 {
			t.Fatalf("%s appended no tuple into or out of %s's node, so it does not reach her", what, person)
		}
	}
	issue := createInteractionIssue(t, handler, "HORZ", "The one read's horizon", "A spec")
	thread := createThreadComment(t, handler, issue.Key, map[string]any{"body": "The person starts a thread."}, person)
	reaches("a comment in a thread the person started", func() {
		createThreadComment(t, handler, issue.Key, map[string]any{"body": "Bob replies in it.", "reply_to": thread.ID}, "bob")
	})
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "The person writes a message."}, person)
	reaches("a reply to the person's issue message", func() {
		createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Bob answers it.", "in_reply_to": message.ID}, "bob")
	})
	assigned := createInteractionIssue(t, handler, "HORA", "An issue assigned to the person", "A spec")
	assignIssue(t, handler, assigned.Key, person)
	reaches("an ask on an issue assigned to the person", func() { createIssueAsk(t, handler, assigned.Key, "Is it ready?") })
	asked := createPersonAsk(t, handler, issue.Key, person, "The person asks: ship it?")
	reaches("an answer to an ask the person asked", func() {
		if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+asked.ID+"/answer", map[string]any{
			"selected": []string{"Ship"},
		}, "bob"); answered.Code != http.StatusOK {
			t.Fatalf("bob's answer to the person's ask: status=%d body=%s", answered.Code, answered.Body.String())
		}
	})
	direct := postAgentMessage(t, handler, "s1", map[string]any{"body": "Can you look at the deploy?", "delivery": "aside"}, person)
	if direct.Code != http.StatusCreated {
		t.Fatalf("the person's direct message: status=%d body=%s", direct.Code, direct.Body.String())
	}
	root := decodeBody[model.Message](t, direct)
	reaches("a session's reply to the person's direct message", func() {
		if replied := replyTo(t, handler, root.ID, "Looking."); replied.Code != http.StatusCreated {
			t.Fatalf("s1's reply to the person's direct message: status=%d body=%s", replied.Code, replied.Body.String())
		}
	})
	reaches("a comment that mentions the person", func() {
		createThreadComment(t, handler, issue.Key, map[string]any{"body": "Over to dispatch://person/" + person + " for the copy."}, "bob")
	})

	// Each node an event at or below the answered horizon tied to the person, by the newest tuple
	// of that relation the horizon reads, where that tuple asserts it: the identity tuple and a
	// value are no entry of the read, which leaves both out.
	answer := readMeInboxAs(t, handler, person, nil)
	tied, err := readStrings(ctx, database.Pool, `
		select distinct newest.node from (
			select distinct on (t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id)
			       case when t.to_kind = $1 and t.to_id = $2 then t.from_kind || ':' || t.from_id
			            else t.to_kind || ':' || t.to_id end as node,
			       t.asserted, t.source_event
			from graph_tuples t
			where ((t.to_kind = $1 and t.to_id = $2) or (t.from_kind = $1 and t.from_id = $2))
			  and t.source_event <= $3 and t.kind <> $4 and t.to_kind <> $5
			order by t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, t.seq desc
		) newest
		where newest.asserted and newest.source_event <> 0
	`, graphstore.KindPerson, person, answer.Horizon, graphstore.TupleAliasOf, graphstore.KindValue)
	if err != nil {
		t.Fatalf("read the nodes the events at or below horizon %d tied to %s: %v", answer.Horizon, person, err)
	}
	if len(tied) == 0 {
		t.Fatalf("no event at or below horizon %d tied a node to %s, so the case reads nothing", answer.Horizon, person)
	}
	for _, node := range tied {
		kind, id, _ := strings.Cut(node, ":")
		if _, held := meInboxEntryOfNode(answer, kind, id); !held {
			t.Errorf("an event at or below horizon %d tied %s to %s, and their read answers no entry for it: %v", answer.Horizon, node, person, meInboxNodes(answer))
		}
	}

	// Their own comment, written while no other transaction is open, is an entry of the next read.
	own := createThreadComment(t, handler, issue.Key, map[string]any{"body": "The person writes while nothing else is open."}, person)
	if next := readMeInboxAs(t, handler, person, nil); !meInboxHolds(next, graphstore.KindComment, own.ID) {
		t.Errorf("the read after the person's comment %s (event %d) answers horizon %d and no entry for it",
			own.ID, writeEventOf(t, database, "comment.created", own.ID), next.Horizon)
	}

	// A comment written to them while an event write of the server's database is open is above the
	// horizon the read answers, until that write ends: the horizon waits on no other transaction
	// (events.Horizon).
	release := holdEventWrite(t, database)
	held := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Bob writes while an event write is open.", "reply_to": thread.ID}, "bob")
	event := writeEventOf(t, database, "comment.created", held.ID)
	during := readMeInboxAs(t, handler, person, nil)
	if during.Horizon >= event {
		t.Errorf("while an event write is open the read answers horizon %d, at or above the event %d of the comment %s written to the person meanwhile",
			during.Horizon, event, held.ID)
	}
	if meInboxHolds(during, graphstore.KindComment, held.ID) {
		t.Errorf("the read whose horizon is %d answers the comment %s written to the person while an event write held the horizon, whose event %d is above it",
			during.Horizon, held.ID, event)
	}
	release()
	if after := readMeInboxAs(t, handler, person, nil); !meInboxHolds(after, graphstore.KindComment, held.ID) {
		t.Errorf("once the open event write ended the read answers horizon %d and no entry for the comment %s, whose event is %d",
			after.Horizon, held.ID, event)
	}
}

// meInboxHolds is whether answer holds an entry whose node is kind/id.
func meInboxHolds(answer meInboxResponse, kind, id string) bool {
	_, held := meInboxEntryOfNode(answer, kind, id)
	return held
}

// The identity rule of the read (person_nodes in meInboxBuild): a session the server proved the
// person's - the alias_of tuple from it to the person's node, which graphstore.QueueAuthored
// writes beside a write the person's credential made from that session - is one of the person's
// own nodes. So a relation into the session is the person's, while none of the person's own nodes
// is an entry, and an identity tuple is no relation of any entry, whichever person it ends at.
func TestTheOneReadTakesAProvedSessionAsThePersonsOwnNode(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	alice, bob := "alice", "bob"
	writer := model.Actor{Kind: "user", ID: "carol"}
	base := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	at := func(minute int) time.Time { return base.Add(time.Duration(minute) * time.Minute) }
	session, message := "s-"+uuid.NewString(), uuid.NewString()
	// A message delivered to the session, as a delivery reaches an agent: the session's node and
	// the delivered_to from the message to it, which names no person.
	seed := meInboxSeed{t: t, database: database}
	seed.node(graphstore.KindSession, session, graphstore.VisibilityProject, writer, at(0))
	seed.node(graphstore.KindMessage, message, graphstore.VisibilityProject, writer, at(0))
	seed.tuples(meInboxTuple(graphstore.KindMessage, message, meInboxDelivered, graphstore.KindSession, session, at(1)))
	if answer := readMeInboxAs(t, handler, alice, nil); answer.Total != 0 {
		t.Fatalf("before any credential proved the session alice's, her read holds %v, want nothing", meInboxNodes(answer))
	}
	// prove writes a comment that login's credential made from the session, through the store's
	// writer of who wrote a thing, which states the session's alias_of to login beside it.
	prove := func(t *testing.T, login string, minute int) string {
		t.Helper()
		comment := uuid.NewString()
		author := model.Actor{Kind: "user", ID: login, Act: &model.ActorAct{Session: session}}
		seed := meInboxSeed{t: t, database: database}
		seed.node(graphstore.KindComment, comment, graphstore.VisibilityProject, author, at(minute))
		seed.write(func(ctx context.Context, tx pgx.Tx) error {
			return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
				return graphstore.QueueAuthored(b, graphstore.KindComment, comment, author, at(minute), graphstore.NoEvent)
			})
		})
		aliases, err := graphstore.CurrentOut(context.Background(), database.Pool, graphstore.KindSession, session, []string{graphstore.TupleAliasOf})
		if err != nil || !slices.ContainsFunc(aliases, func(alias graphstore.Tuple) bool {
			return alias.ToKind == graphstore.KindPerson && alias.ToID == login
		}) {
			t.Fatalf("after %s's write from the session, it is an alias of %+v (%v), want of %s among them", login, aliases, err, login)
		}
		return comment
	}
	proof := prove(t, alice, 2)
	// Newest moment first: her comment's authorship, then the message's delivery to the session.
	want := []string{graphstore.KindComment + ":" + proof, graphstore.KindMessage + ":" + message}
	// identityFree reads login's answer and fails where it holds one of login's own nodes - the
	// person, or the session proved theirs - or a relation of the identity kind, and where a read
	// narrowed to that kind holds anything.
	identityFree := func(t *testing.T, login string) meInboxResponse {
		t.Helper()
		answer := readMeInboxAs(t, handler, login, nil)
		own := []string{graphstore.KindPerson + ":" + login, graphstore.KindSession + ":" + session}
		for _, entry := range answer.Entries {
			if node := entry.Node.Kind + ":" + entry.Node.ID; slices.Contains(own, node) {
				t.Errorf("%s's read holds %s, one of their own nodes, carrying %v", login, node, meInboxKinds(entry))
			}
			for _, relation := range entry.Relations {
				if relation.Kind == graphstore.TupleAliasOf {
					t.Errorf("%s's read holds %s:%s by the identity tuple: %v", login, entry.Node.Kind, entry.Node.ID, meInboxKinds(entry))
				}
			}
		}
		if narrowed := readMeInboxAs(t, handler, login, url.Values{"kind": {graphstore.TupleAliasOf}}); narrowed.Total != 0 || len(narrowed.Entries) != 0 {
			t.Errorf("%s's read narrowed to %s holds %v, want nothing: the identity tuple is no relation", login, graphstore.TupleAliasOf, meInboxNodes(narrowed))
		}
		return answer
	}

	t.Run("a delivery to the proved session is the person's, and no end of the alias is an entry", func(t *testing.T) {
		answer := identityFree(t, alice)
		if nodes := meInboxNodes(answer); !slices.Equal(nodes, want) {
			t.Fatalf("alice's read holds %v, want %v: her comment, and the message delivered to the session proved hers", nodes, want)
		}
		entry, _ := meInboxEntryOfNode(answer, graphstore.KindMessage, message)
		if kinds := meInboxKinds(entry); !slices.Equal(kinds, []string{meInboxDelivered + "/" + meInboxInbound}) || !entry.Moment.Equal(at(1)) {
			t.Errorf("the message carries %v at %s, want its delivery to the session at %s", kinds, entry.Moment, at(1))
		}
	})

	t.Run("the session's alias to another person is no entry of hers, nor hers of his", func(t *testing.T) {
		prove(t, bob, 3)
		if nodes := meInboxNodes(identityFree(t, alice)); !slices.Equal(nodes, want) {
			t.Errorf("after bob's credential proved the same session, alice's read holds %v, want %v", nodes, want)
		}
		identityFree(t, bob)
	})

	// No writer of this build appends a relation between a person and their session but the alias,
	// and the read takes its kinds from the store, so the case appends one of a kind no writer knows.
	t.Run("a relation of any other kind between the person and her session ties no other node", func(t *testing.T) {
		const later = "linked_by_a_later_write"
		meInboxSeed{t: t, database: database}.tuples(meInboxTuple(graphstore.KindSession, session, later, graphstore.KindPerson, alice, at(4)))
		if nodes := meInboxNodes(identityFree(t, alice)); !slices.Equal(nodes, want) {
			t.Errorf("after a %s tuple from her session to her, alice's read holds %v, want %v", later, nodes, want)
		}
	})
}

// The settings the one read makes (meInboxSettings) end with its own transaction: a pooled
// connection carries none of them to the next statement it runs, which may be another client's.
// The read runs through its own path (readMeInbox) on a pool of one connection, so the statement
// after it lands on the connection it ran on - the backend's pid says so - and that connection's
// value of each setting the read makes must be the one it held before the read: the server's own.
// Where the server's own value is the one the read sets, a session-level leak would read the same,
// so the connection is first given another value of that setting at session level, the one the
// read must then leave it holding. Each setting is named from the read's own text, `set local
// <name> = <value>` (or `set <name> = <value>`, which is the leak this test is for).
func TestTheOneReadsSettingsEndWithItsTransaction(t *testing.T) {
	t.Parallel()
	_, database, deps := newTestServer(t, testServerOptions{})
	deps.Store = storetest.SingleConnection(t, database)
	served := directServer(deps)
	pool := deps.Store.Pool
	ctx := context.Background()

	names := make([]string, 0, len(meInboxSettings))
	for _, setting := range meInboxSettings {
		assignment, local := strings.CutPrefix(setting, "set local ")
		if !local {
			assignment, _ = strings.CutPrefix(setting, "set ")
		}
		name, _, assigns := strings.Cut(assignment, " = ")
		if !assigns || strings.ContainsAny(name, " ;") {
			t.Fatalf("the read's setting %q names no one setting this test can read back", setting)
		}
		names = append(names, name)
	}
	type held struct {
		pid    int32
		values []string
	}
	read := func(q interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}) held {
		t.Helper()
		var got held
		if err := q.QueryRow(ctx, `
			select pg_backend_pid(),
			       array(select current_setting(name) from unnest($1::text[]) with ordinality as named(name, at) order by at)
		`, names).Scan(&got.pid, &got.values); err != nil {
			t.Fatalf("read %v: %v", names, err)
		}
		return got
	}

	// What the read sets, read inside a transaction that runs its settings and is rolled back.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, setting := range meInboxSettings {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	sets := read(tx)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back: %v", err)
	}
	own := read(pool)
	for index, name := range names {
		if own.values[index] != sets.values[index] {
			continue
		}
		var other string
		if err := pool.QueryRow(ctx, `
			select set_config(s.name, coalesce(
			         (select value from unnest(s.enumvals) value where value <> $2 limit 1),
			         case when $2 = 'on' then 'off' else 'on' end), false)
			from pg_settings s where s.name = $1
		`, name, sets.values[index]).Scan(&other); err != nil {
			t.Fatalf("give the connection another %s than the read's %s: %v", name, sets.values[index], err)
		}
	}
	before := read(pool)
	for index, name := range names {
		if before.values[index] == sets.values[index] {
			t.Fatalf("the connection holds %s = %s, the read's own value, so a leak of it would not show", name, before.values[index])
		}
	}

	limit := 200
	for _, request := range []meInboxRequest{{}, {state: myRepliesStateDismissed, limit: &limit}} {
		statement, args, err := meInboxStatement(model.Actor{Kind: "user", ID: "alice"}, 0, request, false, false, nil)
		if err != nil {
			t.Fatalf("build the read of state %q: %v", request.state, err)
		}
		if _, err := served.readMeInbox(ctx, statement, args); err != nil {
			t.Fatalf("the read of state %q: %v", request.state, err)
		}
		after := read(pool)
		if after.pid != before.pid {
			t.Fatalf("the statement after the read ran on backend %d, not %d, the read's own", after.pid, before.pid)
		}
		if !slices.Equal(after.values, before.values) {
			t.Errorf("after the read of state %q the connection holds %v = %v, want %v as before it: a setting of the read outlived its transaction",
				request.state, names, after.values, before.values)
		}
	}
}
