package api

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
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

// meInboxRig is a test server whose store is traced for the one read's own statements: a request
// marked countedRead is the one whose statements meInboxStatements records, and the caller's
// identity is resolved on the untraced pool, so a sign-in lookup is no statement of the read.
type meInboxRig struct {
	database   *store.Store
	traced     *store.Store
	handler    http.Handler
	statements *meInboxStatements
}

func newMeInboxRig(t *testing.T) *meInboxRig {
	t.Helper()
	database := storetest.Open(t)
	statements := &meInboxStatements{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, statements, nil)}
	handler, _, _ := newTestServer(t, testServerOptions{
		store:    traced,
		identity: headerIdentity(database),
		// A document's settlement is database work of its own on the traced pool; a delay no
		// test reaches keeps it out of the read's statements.
		settle: time.Hour,
	})
	return &meInboxRig{database: database, traced: traced, handler: handler, statements: statements}
}

// meInboxStatements records every statement and batch a traced pool runs for a request marked
// countedRead, through a storetest.StatementRecorder, each beside the connection that ran it: a
// batch is one round trip whatever it holds, so a read that sent four statements as one batch is
// recorded as the batch it is and is not one statement. The horizon's observation runs on its own
// pool (events.Horizon, store.Pool.Horizon), which is traced too since it is opened from this
// pool's configuration; ownStatements leaves that pool's connections out.
type meInboxStatements struct {
	recorder storetest.StatementRecorder
	mu       sync.Mutex
	conns    []*pgx.Conn
	// taken is how many of the recorded statements ownStatements has answered already.
	taken int
}

func (s *meInboxStatements) record(ctx context.Context, conn *pgx.Conn, sql string) {
	if ctx.Value(countedRead{}) == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recorder.TraceQueryStart(ctx, conn, pgx.TraceQueryStartData{SQL: sql})
	s.conns = append(s.conns, conn)
}

func (s *meInboxStatements) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.record(ctx, conn, data.SQL)
	return ctx
}

func (s *meInboxStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (s *meInboxStatements) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	s.record(ctx, conn, fmt.Sprintf("a batch of %d statements", data.Batch.Len()))
	return ctx
}

func (s *meInboxStatements) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (s *meInboxStatements) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// ownStatements is every statement recorded since the last call that ran on a connection of the
// traced request pool, which is every connection but the horizon pool's.
func (s *meInboxStatements) ownStatements(t *testing.T, traced *store.Store) []string {
	t.Helper()
	horizon, err := traced.Pool.Horizon()
	if err != nil {
		t.Fatalf("open the traced pool's horizon pool: %v", err)
	}
	observers := horizonConnections(t, horizon)
	s.mu.Lock()
	defer s.mu.Unlock()
	recorded := s.recorder.Statements()
	var own []string
	for index, statement := range recorded {
		if index < s.taken {
			continue
		}
		if !slices.Contains(observers, s.conns[index]) {
			own = append(own, statement)
		}
	}
	s.taken = len(recorded)
	return own
}

// horizonConnections is every connection the horizon pool holds, each acquired idle and released,
// which is every one it has opened once the read has returned.
func horizonConnections(t *testing.T, pool *pgxpool.Pool) []*pgx.Conn {
	t.Helper()
	held := pool.AcquireAllIdle(context.Background())
	conns := make([]*pgx.Conn, 0, len(held))
	for _, conn := range held {
		conns = append(conns, conn.Conn())
		conn.Release()
	}
	return conns
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
// of any kind, in one statement a reader can observe. Four nodes each tie the person by one of
// the four arms - a delivery, their own writing, their taking part, a mention of them - and a
// fifth ties them by nothing; a second person is reached by all five, a sixth node by a kind no
// writer of this build appends, and a placement moves under one of them.
func TestOneReadAnswersEveryRelationToThePersonsNode(t *testing.T) {
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
		return graphstore.Authored(ctx, tx, written.kind, written.id, model.Actor{Kind: "user", ID: alice}, at(11), graphstore.NoEvent)
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
		rig.statements.ownStatements(t, rig.traced)
		marked := context.WithValue(context.Background(), countedRead{}, true)
		answer := decodeMeInbox(t, meInboxRequestAs(t, rig.handler, marked, alice, nil))
		if own := rig.statements.ownStatements(t, rig.traced); len(own) != 1 {
			t.Fatalf("the read ran %d statements, want 1:\n%s", len(own), strings.Join(own, "\n---\n"))
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
				unheard, sixth.kind, sixth.id, slices.Sorted(func(yield func(string) bool) {
					for kind := range known {
						if !yield(kind) {
							return
						}
					}
				}))
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
		for _, kind := range []string{meInboxDelivered, meInboxAuthored, meInboxParticipates, meInboxNamed} {
			// The control: the same search over a planted copy finds the literal, so a clean
			// answer below is a reading of the file and not of a search that finds nothing.
			if planted := string(source) + "\n// '" + kind + "'\n"; !strings.Contains(planted, kind) {
				t.Fatalf("the search for %q does not find it in a copy that plants it", kind)
			}
			if strings.Contains(string(source), kind) {
				t.Errorf("me_inbox.go names the relation kind %q: the read takes its kinds from the store", kind)
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
}

// The identity rule of the read (person_nodes in meInboxBuild): a session the server proved the
// person's - the alias_of tuple from it to the person's node, which graphstore.Authored writes
// beside a write the person's credential made from that session - is one of the person's own
// nodes. So a relation into the session is the person's, while none of the person's own nodes is
// an entry, and an identity tuple is no relation of any entry, whichever person it ends at.
func TestTheOneReadTakesAProvedSessionAsThePersonsOwnNode(t *testing.T) {
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
			return graphstore.Authored(ctx, tx, graphstore.KindComment, comment, author, at(minute), graphstore.NoEvent)
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
