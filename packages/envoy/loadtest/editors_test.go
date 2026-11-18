package main

import (
	"context"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygows "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/testwait"
)

// seededDocument is a document holding a seeded spec, written as Dispatch writes a document it
// creates from markdown (internal/dispatch/docs encodeDocumentTree): the parsed tree through
// pmdoc.Update into the prosemirror fragment, with the marks map, in one transaction.
func seededDocument(t *testing.T) *crdt.Doc {
	t.Helper()
	markdown, _ := specMarkdown(rand.New(rand.NewPCG(1, 1)), "LGA-0001", 4000, false)
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse the seeded spec: %v", err)
	}
	doc := crdt.New()
	fragment := doc.GetXmlFragment(proofFragment)
	doc.GetMap("marks")
	if err := doc.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
		t.Fatalf("write the seeded spec: %v", err)
	}
	return doc
}

// render reads doc's tree and renders it as Dispatch does, failing the test when either refuses
// it: a tree outside the Proof schema.
func render(t *testing.T, doc *crdt.Doc) string {
	t.Helper()
	tree, err := pmdoc.Read(doc.GetXmlFragment(proofFragment))
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	markdown, err := pmdoc.Render(tree)
	if err != nil {
		t.Fatalf("render the document: %v", err)
	}
	return markdown
}

func TestEditParagraphStaysInsideTheProofSchema(t *testing.T) {
	doc := seededDocument(t)
	r := rand.New(rand.NewPCG(7, 7))
	for i := range 300 {
		text := " " + words[r.IntN(len(words))]
		if i%editorMarkerEvery == 0 {
			text = " " + editorMarker(int64(i))
		}
		if editParagraph(doc, r, text) == nil {
			t.Fatalf("edit %d made no update", i)
		}
	}
	markdown := render(t, doc)
	if !strings.Contains(markdown, "\n\nRevision token r0\n\n") {
		t.Fatalf("the edits changed the revision token line:\n%s", markdown)
	}
	if !strings.Contains(markdown, editorMarker(250)) {
		t.Fatalf("the document lacks the marker %s whole:\n%s", editorMarker(250), markdown)
	}
	for _, block := range strings.Split(markdown, "\n\n") {
		if block != strings.TrimSpace(block) && !strings.HasPrefix(block, "|") {
			t.Fatalf("an edit left a paragraph that starts or ends with a space: %q", block)
		}
	}
	// The same edits applied to a copy that only received the updates render the same.
	replica := crdt.New()
	if err := crdt.ApplyUpdateV1(replica, crdt.EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
		t.Fatalf("apply the edited document to a replica: %v", err)
	}
	if got := render(t, replica); got != markdown {
		t.Fatal("a replica of the edited document renders differently")
	}
}

// editorRoom starts room as Dispatch's document server runs it: ygo's websocket server on
// Hocuspocus framing, which loads the document from seeded and admits a token equal to the
// server's schema version read-write. Where ids is true it is served behind
// docstest.GivingClientIDs, so each socket's first frame names the client id the room gives it,
// as Dispatch's admission does; the returned record of admissions is nil otherwise. url is the
// room's document websocket with its schema_version.
func editorRoom(t *testing.T, room string, seeded []byte, ids bool) (srv *ygows.Server, given *docstest.ClientIDRoom, url string) {
	t.Helper()
	schema := strconv.Itoa(pmdoc.SchemaVersion())
	srv = ygows.NewServer()
	srv.HocuspocusFraming = true
	srv.OnTokenAuth = func(_, token string) (ygows.ConnectionConfig, error) {
		return ygows.ConnectionConfig{ReadOnly: token != schema}, nil
	}
	srv.OnLoadDocument = func(_ context.Context, _ string, doc *crdt.Doc) error {
		return crdt.ApplyUpdateV1(doc, seeded, nil)
	}
	var handler http.Handler = srv
	if ids {
		given = docstest.GivingClientIDs(srv)
		handler = given
	}
	mux := http.NewServeMux()
	mux.Handle("/ws/doc/{room}", handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv, given, "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + room + "?schema_version=" + schema
}

// requireClients requires the room's copy of room to name no client but the seeded document's
// and want: every edit an editor sent landed under the id the room gave it.
func requireClients(t *testing.T, srv *ygows.Server, room string, seeded *crdt.Doc, want ...crdt.ClientID) {
	t.Helper()
	roomDoc := srv.GetDoc(room)
	if roomDoc == nil {
		t.Fatal("the room is not resident while its editors are connected")
	}
	allowed := append(slices.Collect(maps.Keys(seeded.StateVector())), want...)
	for client := range roomDoc.StateVector() {
		if !slices.Contains(allowed, client) {
			t.Fatalf("the room holds edits under client id %d, which it gave no editor (it gave %v)", client, want)
		}
	}
}

func TestEditorUpdateAppliesOnARoomAndRelays(t *testing.T) {
	room := "0b6a2c8e-7f4d-4c1a-9e7b-3d2f1a0c9b8e"
	schema := strconv.Itoa(pmdoc.SchemaVersion())
	seededDoc := seededDocument(t)
	srv, given, url := editorRoom(t, room, crdt.EncodeStateAsUpdateV1(seededDoc, nil), true)
	d := &editorDoc{key: "LGA-0001", artifactID: room, editors: 2}
	stats := newEditorStats()
	ctx := context.Background()
	a, err := dialEditor(ctx, url, "person-0", schema, nil, 0, d, stats)
	if err != nil {
		t.Fatalf("connect editor a: %v", err)
	}
	t.Cleanup(a.close)
	b, err := dialEditor(ctx, url, "person-1", schema, nil, 0, d, stats)
	if err != nil {
		t.Fatalf("connect editor b: %v", err)
	}
	t.Cleanup(b.close)
	admissions := given.Admissions()
	if want := []docstest.Admission{{Person: "person-0", Given: a.client}, {Person: "person-1", Given: b.client}}; !slices.Equal(admissions, want) {
		t.Fatalf("the room admitted %+v, want each editor presenting an empty client id and given its own: %+v", admissions, want)
	}
	for _, e := range []*editorConn{a, b} {
		if !e.given || e.ydoc.ClientID() != e.client {
			t.Fatalf("an editor given client id %d (given: %t) has a copy under %d", e.client, e.given, e.ydoc.ClientID())
		}
	}
	waitUntil(t, "both editors to sync", func() bool { return a.synced.Load() && b.synced.Load() })

	marker := editorMarker(1)
	var sentAt time.Time
	if err := a.send(rand.New(rand.NewPCG(3, 3)), " "+marker, true, func(at time.Time) { sentAt = at }); err != nil {
		t.Fatalf("send the edit: %v", err)
	}
	waitUntil(t, "the room to acknowledge the edit and b to receive it", func() bool {
		stats.mu.Lock()
		defer stats.mu.Unlock()
		return stats.acked == 1 && len(stats.relayMs) == 1
	})
	stats.mu.Lock()
	refused, readOnly, closes, relay := stats.refused, stats.readOnly, len(stats.closes), stats.relayMs[0]
	stats.mu.Unlock()
	if refused != 0 || readOnly != 0 || closes != 0 {
		t.Fatalf("refused %d, read-only %d, closes %d; want none", refused, readOnly, closes)
	}
	if relay < 0 || relay > float64(time.Since(sentAt))/1e6 {
		t.Fatalf("relay latency %.3f ms is outside the send-to-now interval", relay)
	}
	requireClients(t, srv, room, seededDoc, a.client, b.client)
	markdown := render(t, srv.GetDoc(room))
	if !strings.Contains(markdown, marker) {
		t.Fatalf("the room's document lacks the marker:\n%s", markdown)
	}
	if got := render(t, b.ydoc); got != markdown {
		t.Fatal("editor b's copy renders differently from the room's")
	}
}

// An editor that connects again presents the client id it was given, is given it back, and goes
// on in the copy it held: a new copy of that id would take the room's state, which holds the
// editor's own edits under it, and ygo would then draw the copy another id that no admission
// gave out. An editor whose id the room does not give back starts a new copy under the one it
// is given.
func TestAnEditorThatConnectsAgainPresentsItsClientIdAndKeepsItsCopy(t *testing.T) {
	room := "5d1c9a7e-2b3f-4e6a-8c0d-9f7b1e3a5c2d"
	schema := strconv.Itoa(pmdoc.SchemaVersion())
	seededDoc := seededDocument(t)
	srv, given, url := editorRoom(t, room, crdt.EncodeStateAsUpdateV1(seededDoc, nil), true)
	d := &editorDoc{key: "LGA-0001", artifactID: room, editors: 1}
	stats := newEditorStats()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(5, 5))
	acked := func(n int) func() bool {
		return func() bool {
			stats.mu.Lock()
			defer stats.mu.Unlock()
			return stats.acked == n
		}
	}

	first, err := dialEditor(ctx, url, "person-0", schema, nil, 0, d, stats)
	if err != nil {
		t.Fatalf("connect the editor: %v", err)
	}
	waitUntil(t, "the editor to sync", first.synced.Load)
	if err := first.send(r, " before", true, nil); err != nil {
		t.Fatalf("send the first edit: %v", err)
	}
	waitUntil(t, "the room to acknowledge the first edit", acked(1))
	first.close()

	again, err := dialEditor(ctx, url, "person-0", schema, first.ydoc, first.client, d, stats)
	if err != nil {
		t.Fatalf("connect the editor again: %v", err)
	}
	t.Cleanup(again.close)
	held := strconv.FormatUint(uint64(first.client), 10)
	if got, want := given.Admissions()[1], (docstest.Admission{Person: "person-0", Presented: held, Given: first.client}); got != want {
		t.Fatalf("the room admitted the editor's second socket as %+v, want %+v", got, want)
	}
	if !again.given || again.client != first.client || again.ydoc != first.ydoc {
		t.Fatalf("the editor went on under client id %d (given: %t) in a new copy (%t), want the %d it was given back in the copy it held", again.client, again.given, again.ydoc != first.ydoc, first.client)
	}
	waitUntil(t, "the editor to sync again", again.synced.Load)
	if err := again.send(r, " after", true, nil); err != nil {
		t.Fatalf("send the second edit: %v", err)
	}
	waitUntil(t, "the room to acknowledge the second edit", acked(2))
	if again.ydoc.ClientID() != first.client {
		t.Fatalf("the editor's copy writes under client id %d after it synced again, want %d", again.ydoc.ClientID(), first.client)
	}
	if clock := srv.GetDoc(room).StateVector().Clock(first.client); clock != again.ydoc.StateVector().Clock(first.client) || clock == 0 {
		t.Fatalf("the room holds client id %d to clock %d, want both edits' %d", first.client, clock, again.ydoc.StateVector().Clock(first.client))
	}
	requireClients(t, srv, room, seededDoc, first.client)
	stats.mu.Lock()
	refused, closes := stats.refused, len(stats.closes)
	stats.mu.Unlock()
	if refused != 0 || closes != 0 {
		t.Fatalf("refused %d, closes %d; want none", refused, closes)
	}

	// Another person presenting the same id is not given it, so that editor starts a new copy
	// under the id it is given.
	other, err := dialEditor(ctx, url, "person-1", schema, again.ydoc, again.client, d, stats)
	if err != nil {
		t.Fatalf("connect another person's editor: %v", err)
	}
	t.Cleanup(other.close)
	if other.client == again.client || other.ydoc == again.ydoc || other.ydoc.ClientID() != other.client {
		t.Fatalf("an editor not given back the id %d it presented goes on under %d in its copy under %d (the held copy: %t)", again.client, other.client, other.ydoc.ClientID(), other.ydoc == again.ydoc)
	}
}

// A room that gives no client id - a Dispatch build from before client ids, here ygo's server
// alone - sends its sync step 1 first. That is no failure: the editor writes under the id its
// copy drew, keeps it across a reconnect, and hands that first frame to its reader. The reader's
// answer to it is how the room gets what the copy holds and the room lacks, such as an edit typed
// while the socket was down, which reaches the room here by nothing else.
func TestAnEditorAgainstARoomThatGivesNoIdWritesUnderItsCopysOwn(t *testing.T) {
	room := "8e4f2a6c-1d3b-4a5e-9c7f-0b2d4e6f8a1c"
	schema := strconv.Itoa(pmdoc.SchemaVersion())
	seededDoc := seededDocument(t)
	srv, _, url := editorRoom(t, room, crdt.EncodeStateAsUpdateV1(seededDoc, nil), false)
	d := &editorDoc{key: "LGA-0001", artifactID: room, editors: 1}
	stats := newEditorStats()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(9, 9))
	acked := func(n int) func() bool {
		return func() bool {
			stats.mu.Lock()
			defer stats.mu.Unlock()
			return stats.acked == n
		}
	}
	roomHolds := func(e *editorConn) func() bool {
		return func() bool {
			want := e.ydoc.StateVector().Clock(e.client)
			return want > 0 && srv.GetDoc(room).StateVector().Clock(e.client) == want
		}
	}

	first, err := dialEditor(ctx, url, "person-0", schema, nil, 0, d, stats)
	if err != nil {
		t.Fatalf("connect the editor: %v", err)
	}
	if first.given || first.client != first.ydoc.ClientID() {
		t.Fatalf("an editor the room gave no id writes under %d (given: %t), want its copy's own %d", first.client, first.given, first.ydoc.ClientID())
	}
	waitUntil(t, "the editor to sync", first.synced.Load)
	if err := first.send(r, " online", true, nil); err != nil {
		t.Fatalf("send the first edit: %v", err)
	}
	waitUntil(t, "the room to acknowledge the first edit", acked(1))
	first.close()
	if editParagraph(first.ydoc, r, " offline") == nil {
		t.Fatal("the copy took no edit while the socket was down")
	}

	again, err := dialEditor(ctx, url, "person-0", schema, first.ydoc, first.client, d, stats)
	if err != nil {
		t.Fatalf("connect the editor again: %v", err)
	}
	t.Cleanup(again.close)
	if again.given || again.client != first.client || again.ydoc != first.ydoc {
		t.Fatalf("the editor went on under client id %d (given: %t) in a new copy (%t), want its own %d in the copy it held", again.client, again.given, again.ydoc != first.ydoc, first.client)
	}
	waitUntil(t, "the editor to sync again", again.synced.Load)
	waitUntil(t, "the room to take the edit typed while the socket was down", roomHolds(again))
	if err := again.send(r, " after", true, nil); err != nil {
		t.Fatalf("send the third edit: %v", err)
	}
	waitUntil(t, "the room to acknowledge the third edit", acked(2))
	waitUntil(t, "the room to hold every edit", roomHolds(again))
	requireClients(t, srv, room, seededDoc, first.client)
	stats.mu.Lock()
	refused, closes := stats.refused, len(stats.closes)
	stats.mu.Unlock()
	if refused != 0 || closes != 0 {
		t.Fatalf("refused %d, closes %d; want none", refused, closes)
	}
}

func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	testwait.WithinPolling(t, 10*time.Second, 5*time.Millisecond, what, done)
}
