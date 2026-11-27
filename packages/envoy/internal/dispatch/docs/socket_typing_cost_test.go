package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// typingCostPeers, typingCostKeystroke and typingCostWindow are the room the measurement types in:
// ten people, each typing a keystroke every 150 ms with a cursor (awareness) update after each,
// for ten seconds, after one second of warm-up that is not counted.
const (
	typingCostPeers     = 10
	typingCostKeystroke = 150 * time.Millisecond
	typingCostWindow    = 10 * time.Second
	typingCostWarmUp    = time.Second
)

// typingCostStatements counts every statement the service's pool sends, and apart the reads a
// document socket's ask makes: the rule's (readableRoomsStatement), whether a project is private,
// and the stand-in for the API's refresh of the stored listener answer.
type typingCostStatements struct {
	all, rule, private, roster atomic.Int64
}

func (c *typingCostStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.all.Add(1)
	switch {
	case data.SQL == readableRoomsStatement:
		c.rule.Add(1)
	case strings.Contains(data.SQL, "select exists (select 1 from projects where visibility = 'private')"):
		c.private.Add(1)
	case data.SQL == typingCostRosterRead:
		c.roster.Add(1)
	}
	return ctx
}

func (*typingCostStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// typingCostRosterRead stands in for the API's refresh of the stored listener answer before each
// ask of the rule (api: refreshRosterForContent), which reads the route_roster row while it is
// fresh: one statement, as that read is.
const typingCostRosterRead = `select coalesce((select expires_at > now() from route_roster), false)`

// TestDocumentRoomTypingCost measures what a room of ten typing people costs the database and the
// frames: statements per second and the delivery latency of the cursor (awareness) frames, sent by
// one peer and read by each other, which wait in each socket's one writer behind the room's
// document frames and so carry each data frame's ask. It runs a deployment with no private project
// (R9) and one with a private project the typists are not on, each with the room sync running as
// every process runs it. It is a measurement, not a check: it runs only with
// DISPATCH_BENCH_DOCSOCKET=1, against the database storetest opens.
func TestDocumentRoomTypingCost(t *testing.T) {
	// Not parallel: it measures the database's statements a second and the cursor frames' delivery
	// latency by the wall clock, which the package's parallel tests would share the machine with.
	if os.Getenv("DISPATCH_BENCH_DOCSOCKET") != "1" {
		t.Skip("set DISPATCH_BENCH_DOCSOCKET=1 to measure a typing room's cost")
	}
	for _, private := range []bool{false, true} {
		name := "no private project (R9)"
		if private {
			name = "a private project"
		}
		t.Run(name, func(t *testing.T) { measureTypingRoom(t, private) })
	}
}

func measureTypingRoom(t *testing.T, private bool) {
	statements := &typingCostStatements{}
	database := storetest.TracedStore(t, storetest.Open(t), statements)
	artifactID := createDocument(t, database, "# First")
	service := newTestServiceOn(t, database)
	service.SetRosterRefresh(func(ctx context.Context) {
		var fresh bool
		_ = database.Pool.QueryRow(ctx, typingCostRosterRead).Scan(&fresh)
	})
	if private {
		if _, err := database.Pool.Exec(context.Background(), `insert into projects (key, name, default_assignee) values ('PRIV', 'Private', 'alice')`); err != nil {
			t.Fatalf("create PRIV: %v", err)
		}
		owner := model.Actor{Kind: "user", ID: "alice"}
		writeAccess(t, service, func(ctx context.Context, tx pgx.Tx) error {
			if _, _, err := access.SetVisibility(ctx, tx, service.events, "PRIV", model.ProjectPrivate, owner); err != nil {
				return err
			}
			_, _, err := access.AddPrincipal(ctx, tx, service.events, "PRIV", access.PrincipalPerson, "bob@outside.example", owner)
			return err
		})
	}
	paragraphs := make([]string, typingCostPeers)
	for i := range paragraphs {
		paragraphs[i] = fmt.Sprintf("p%d", i)
	}
	seed := strings.Join(paragraphs, "\n\n")
	seedServiceText(t, service, artifactID, seed)
	runRoomSync(t, newRoomSync(service))
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	testcleanup.Add(t, httpServer.Close)

	url := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID + "?schema_version=" + fmt.Sprint(pmdoc.SchemaVersion())
	peers := make([]*docstest.Peer, typingCostPeers)
	for i := range peers {
		login := "alice"
		if i > 0 {
			login = fmt.Sprintf("typist-%d@example.com", i)
		}
		peers[i] = docstest.Dial(t, url, http.Header{"X-Dispatch-User": []string{login}}, artifactID, crdt.New())
		if err := peers[i].WaitForText(peerMarkdown, seed+"\n", roomWait(t)); err != nil {
			t.Fatalf("typist %d reads the document: %v", i, err)
		}
	}

	var counting atomic.Bool
	var mu sync.Mutex
	var latencies []time.Duration
	var dropped atomic.Int64
	ctx, stop := context.WithCancel(context.Background())
	var readers sync.WaitGroup
	for _, peer := range peers {
		readers.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-peer.Ended:
					dropped.Add(1)
					return
				case entries := <-peer.Awareness:
					now := time.Now()
					for _, entry := range entries {
						var state struct {
							Sent int64 `json:"sent"`
						}
						if json.Unmarshal(entry.State, &state) != nil || state.Sent == 0 || !counting.Load() {
							continue
						}
						mu.Lock()
						latencies = append(latencies, now.Sub(time.Unix(0, state.Sent)))
						mu.Unlock()
					}
				}
			}
		})
	}
	var typists sync.WaitGroup
	var sendErrors atomic.Int64
	for i, peer := range peers {
		typists.Go(func() {
			fragment := peer.Doc.GetXmlFragment(fragmentName)
			ticker := time.NewTicker(typingCostKeystroke)
			defer ticker.Stop()
			for n := 0; ; n++ {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				if _, err := peer.Send(func(txn *crdt.Transaction) {
					tree, err := pmdoc.ReadInTransaction(txn, fragment)
					if err != nil || len(tree.Children) <= i || len(tree.Children[i].Children) == 0 {
						return
					}
					tree.Children[i].Children[0].Text += "x"
					_ = pmdoc.Update(txn, fragment, tree)
				}); err != nil {
					sendErrors.Add(1)
				}
				if err := peer.SetAwareness(uint64(100+i), map[string]any{"sent": time.Now().UnixNano(), "n": n}); err != nil {
					sendErrors.Add(1)
				}
			}
		})
	}
	time.Sleep(typingCostWarmUp)
	startAll, startRule, startPrivate, startRoster := statements.all.Load(), statements.rule.Load(), statements.private.Load(), statements.roster.Load()
	counting.Store(true)
	time.Sleep(typingCostWindow)
	counting.Store(false)
	all, rule, privateReads, roster := statements.all.Load()-startAll, statements.rule.Load()-startRule, statements.private.Load()-startPrivate, statements.roster.Load()-startRoster
	stop()
	typists.Wait()
	readers.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(latencies) == 0 {
		t.Fatalf("UNKNOWN: no cursor frame was read in the window; nothing was measured")
	}
	slices.Sort(latencies)
	at := func(q float64) time.Duration { return latencies[min(len(latencies)-1, int(q*float64(len(latencies))))] }
	seconds := typingCostWindow.Seconds()
	t.Logf("RESULT deployment=%s peers=%d cursor_frames=%d p50=%s p99=%s max=%s statements_per_s=%.1f ask_rule_per_s=%.1f ask_private_per_s=%.1f ask_roster_per_s=%.1f closed_sockets=%d send_errors=%d",
		map[bool]string{false: "R9", true: "private"}[private], typingCostPeers, len(latencies),
		at(0.50).Round(10*time.Microsecond), at(0.99).Round(10*time.Microsecond), latencies[len(latencies)-1].Round(10*time.Microsecond),
		float64(all)/seconds, float64(rule)/seconds, float64(privateReads)/seconds, float64(roster)/seconds, dropped.Load(), sendErrors.Load())
}
