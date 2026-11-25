package api

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The check-then-read windows of the per-statement census (m19): a route reads an object as its
// reader may read it, then reads the rows that object owns by its id in statements of their own.
// A read that binds no clause of its own answers those rows for an object made hidden between the
// two. Each row here makes the object hidden in that window - the moment the route's own read of
// it has run - and reads what the keyed read answered.
//
// The object is one a session's rule decides (access.SessionClause, the live session_projects
// pairs): a direct message to the session, and a file of its conversation. It is the one kind of
// owner a reader's viewer, resolved once per request (readerFor), does not decide alone: a
// project's own rule reads only that viewer, and no object changes project, so an issue, a
// document, an ask or a comment has no window (a conversation's file holds no ask and no comment,
// 400 ARTIFACT_AGENT_OWNED). A session placed in a project the reader does not see is hidden from
// that reader from the moment its session_projects pair commits, so the switch is that pair.

// gapWindow is a tracer that, armed, places the seed's session in PRIV right after the first
// statement check matches has run, and records what each statement read matches answers after it.
type gapWindow struct {
	seed *gapSeed

	mu      sync.Mutex
	check   func(sql string) bool
	read    func(sql string) bool
	armed   bool
	flipped bool
	flipErr error
	// answered is the rows each read answered after the switch, one entry per statement.
	answered []int64
}

type gapSQLKey struct{}

func (w *gapWindow) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, gapSQLKey{}, data.SQL)
}

func (w *gapWindow) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(gapSQLKey{}).(string)
	w.observe(sql, data.CommandTag, data.Err)
}

func (w *gapWindow) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (w *gapWindow) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	w.observe(data.SQL, data.CommandTag, data.Err)
}

func (w *gapWindow) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// observe runs the switch after the check, once, and records each read after it. The switch runs
// outside the lock: its own statement passes through here too.
func (w *gapWindow) observe(sql string, tag pgconn.CommandTag, err error) {
	w.mu.Lock()
	switchNow := w.armed && err == nil && w.check(sql)
	if switchNow {
		w.armed = false
	}
	if w.flipped && w.read != nil && w.read(sql) {
		w.answered = append(w.answered, tag.RowsAffected())
	}
	w.mu.Unlock()
	if !switchNow {
		return
	}
	flipErr := w.seed.hideSession()
	w.mu.Lock()
	w.flipped, w.flipErr = true, flipErr
	w.mu.Unlock()
}

// arm sets the window's check and read and shows the session again, so the next request starts
// with its object visible to alice.
func (w *gapWindow) arm(t *testing.T, check, read func(string) bool) {
	t.Helper()
	w.seed.showSession(t)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.check, w.read, w.armed, w.flipped, w.flipErr, w.answered = check, read, true, false, nil, nil
}

// result is whether the switch ran and the rows each read answered after it.
func (w *gapWindow) result(t *testing.T) []int64 {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.flipErr != nil {
		t.Fatalf("place the session in PRIV inside the window: %v", w.flipErr)
	}
	if !w.flipped {
		t.Fatal("the route ran no statement the check names, so no window opened")
	}
	w.armed = false
	return append([]int64(nil), w.answered...)
}

// sqlHolding answers a matcher over a statement's text: true when it holds every fragment.
func sqlHolding(fragments ...string) func(string) bool {
	return func(sql string) bool {
		for _, fragment := range fragments {
			if !strings.Contains(sql, fragment) {
				return false
			}
		}
		return true
	}
}

// gapSeed is verification v16's seed with a conversation of an agent of alice's: the session
// gapSession, placed in PUB alone and tied to alice (session_owners, as a proved write of her
// credential ties it), who sees no private project, so alice reads it and its content
// (access.SessionContentClause); alice's direct message to it; a file of its conversation; and,
// citing that file, a comment on PUB's issue and a document of PUB, whose mention stores an
// excerpt.
type gapSeed struct {
	privateSeed
	window          *gapWindow
	direct, address string
	citingComment   string
}

const (
	gapSession = "session-gap-owner"
	gapPeer    = "session-gap-peer"
)

func seedGapWindow(t *testing.T) *gapSeed {
	t.Helper()
	window := &gapWindow{}
	seed := &gapSeed{window: window}
	window.seed = seed
	seed.privateSeed = seedPrivateProjectWith(t, testServerOptions{tracer: window})
	ctx := context.Background()
	if _, err := seed.database.Pool.Exec(ctx, `insert into session_projects (session_id, project_key) values ($1, $3), ($2, $3)`,
		gapSession, gapPeer, seedPublic); err != nil {
		t.Fatalf("place the sessions in PUB: %v", err)
	}
	if _, err := seed.database.Pool.Exec(ctx, `insert into session_owners (session_id, login) values ($1, $3), ($2, $3)`,
		gapSession, gapPeer, seedAlice); err != nil {
		t.Fatalf("tie the sessions to alice: %v", err)
	}
	seed.direct = createdID(t, seed.mustRequest(t, callerAlice, http.MethodPost, "/api/v1/agents/"+gapSession+"/messages", map[string]any{
		"body": "Are you still on the build?", "delivery": "btw",
	}, http.StatusCreated))
	uploaded := multipartRequest(t, seed.handler, "/api/v1/agents/"+gapSession+"/artifacts", map[string]string{"name": "gap-shot.png"},
		"gap-shot.png", "image/png", pngBytes("the gap shot"), seedCarol)
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("carol uploads to %s's conversation: %d %s", gapSession, uploaded.Code, uploaded.Body.String())
	}
	file := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	seed.address = "dispatch://agent/" + gapSession + "/artifact/" + file.Slug
	seed.citingComment = createdID(t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/comments",
		map[string]any{"body": "The shot is at " + seed.address + " for the record."}, http.StatusCreated))
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/projects/"+seedPublic+"/artifacts", map[string]any{
		"name": "gap-citations.md", "content": "# Citations\n\nThe shot of the build is at " + seed.address + " today.\n",
	}, http.StatusCreated)
	return seed
}

// hideSession places gapSession in PRIV, which alice does not see: from its commit she reads
// neither the session nor anything of its conversation.
func (s *gapSeed) hideSession() error {
	_, err := s.database.Pool.Exec(context.Background(), `insert into session_projects (session_id, project_key) values ($1, $2)
		on conflict do nothing`, gapSession, seedPrivate)
	return err
}

// showSession takes gapSession out of PRIV again.
func (s *gapSeed) showSession(t *testing.T) {
	t.Helper()
	if _, err := s.database.Pool.Exec(context.Background(), `delete from session_projects where session_id = $1 and project_key = $2`,
		gapSession, seedPrivate); err != nil {
		t.Fatalf("take the session out of PRIV: %v", err)
	}
}

// gapWindowRead is one route the window opens in: the read that checks the object, the keyed read
// that must hold on its own, and what alice's answer must not hold once the object is hidden.
type gapWindowRead struct {
	name, target string
	check, read  func(string) bool
	// leaked names, in alice's answer, the rows the keyed read answered for the hidden object.
	leaked []string
}

func (s *gapSeed) windowReads() []gapWindowRead {
	messageDeliveries := func(sql string) bool {
		return strings.Contains(sql, "from message_deliveries") && strings.Contains(sql, "message_id = any($1::uuid[])")
	}
	edges := func(sql string) bool {
		return strings.Contains(sql, "from graph_edges g where g.to_kind = 'artifact' and g.to_id = $1")
	}
	excerpts := func(sql string) bool { return strings.Contains(sql, "r.excerpt_text from refs r") }
	return []gapWindowRead{
		{
			name: "a direct message's deliveries", target: "/api/v1/messages/" + s.direct,
			check: sqlHolding("from messages m where m.id = $1 and"), read: messageDeliveries,
			leaked: []string{`"attempt":`},
		},
		{
			name: "a conversation file's references", target: "/api/v1/references?" + url.Values{"to": {s.address}}.Encode(),
			check: sqlHolding("/* visibility predicate: artifact */"), read: edges,
			leaked: []string{s.citingComment, "gap-citations"},
		},
		{
			// The excerpts ride on the edges they are of, so with the edges held alice's answer
			// carries none either way: the row is the rows the excerpt read answered.
			name: "the stored excerpts of a conversation file's mentions", target: "/api/v1/references?" + url.Values{"to": {s.address}}.Encode(),
			check: sqlHolding("/* visibility predicate: artifact */"), read: excerpts,
		},
	}
}

// TestCensusGapsAKeyedReadHoldsItsObjectOnItsOwn: each read keyed by an object an earlier
// statement of the route checked binds the reader's clause itself, so an object made hidden
// between the two answers no row: a direct message's delivery attempts (loadMessageDeliveries,
// after loadMessage), and a conversation file's edges (refs.Edges) and the stored excerpts of its
// mentions (refs.StoredExcerpts), after the file's own read (refs.Node). alice reads each route
// while gapSession's conversation is hers to read; the moment the route's own read of the object
// has run, the session is placed in PRIV, and the keyed read must answer nothing of it.
//
// Control: one keyed read's clause dropped; it then answers the hidden object's rows and its row
// fails, naming them, and in alice's answer what they carry.
func TestCensusGapsAKeyedReadHoldsItsObjectOnItsOwn(t *testing.T) {
	t.Parallel()
	seed := seedGapWindow(t)
	for _, read := range seed.windowReads() {
		t.Run(read.name, func(t *testing.T) {
			seed.window.arm(t, read.check, read.read)
			response := seed.request(t, callerAlice, http.MethodGet, read.target, nil)
			answered := seed.window.result(t)
			t.Logf("GET %s = %d; keyed reads after the switch answered %v rows", read.target, response.Code, answered)
			if len(answered) == 0 {
				t.Fatalf("GET %s ran no keyed read after the switch, so this row measures nothing: %s", read.target, response.Body.String())
			}
			for _, rows := range answered {
				if rows != 0 {
					t.Errorf("GET %s: a keyed read answered %d rows of an object alice no longer reads", read.target, rows)
				}
			}
			for _, leaked := range read.leaked {
				if strings.Contains(response.Body.String(), leaked) {
					t.Errorf("GET %s answers %s of an object alice no longer reads: %s", read.target, leaked, response.Body.String())
				}
			}
		})
	}
}

// TestCensusGapsWithTheObjectInViewTheKeyedReadsAnswerAsBefore is the window rows' control and R9:
// with the session left in PUB, and again with no private project and the session in PRIV too,
// alice reads each route's keyed rows - the delivery attempt, the citing comment and document -
// as the base answered them. Each answer is logged with its times and ids blanked, beside its
// sha256, for the report's comparison of the two commits.
func TestCensusGapsWithTheObjectInViewTheKeyedReadsAnswerAsBefore(t *testing.T) {
	t.Parallel()
	seed := seedGapWindow(t)
	check := func(t *testing.T, label string) {
		for _, read := range seed.windowReads() {
			response := seed.mustRequest(t, callerAlice, http.MethodGet, read.target, nil, http.StatusOK)
			body := response.Body.String()
			blanked := timestampsAndIDs.ReplaceAllString(body, "<varies>")
			t.Logf("%s %s %s sha256 %x", label, read.name, timestampsAndIDs.ReplaceAllString(read.target, "<id>"), sha256.Sum256([]byte(blanked)))
			for _, kept := range read.leaked {
				if !strings.Contains(body, kept) {
					t.Errorf("%s: GET %s holds no %s: %s", label, read.target, kept, body)
				}
			}
		}
	}
	seed.showSession(t)
	check(t, "in view")
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	if err := seed.hideSession(); err != nil {
		t.Fatalf("place the session in PRIV: %v", err)
	}
	check(t, "R9")
}
