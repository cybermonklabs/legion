package main

import (
	"context"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// typableDocument is a document of two paragraphs a peer may type into.
func typableDocument() *crdt.Doc {
	document := crdt.New()
	fragment := document.GetXmlFragment(fragmentName)
	document.Transact(func(txn *crdt.Transaction) {
		for index, text := range []string{"A first paragraph the peers may type into.", "And a second one."} {
			paragraph := crdt.NewYXmlElement("paragraph")
			fragment.InsertElement(txn, index, paragraph)
			run := crdt.NewYXmlText()
			paragraph.InsertText(txn, 0, run)
			run.Insert(txn, 0, text, nil)
		}
	})
	return document
}

// testRoom serves artifact's room as ygo's websocket server on Hocuspocus framing, loading the
// document from seeded, behind docstest.GivingClientIDs where ids is true, so each socket that
// presents the client id parameter is given an id in its first frame, as Dispatch's admission
// does; the returned record of admissions is nil otherwise. base is the Dispatch URL whose
// /ws/doc/<artifact> is the room.
func testRoom(t *testing.T, artifact string, seeded []byte, ids bool) (srv *ygows.Server, given *docstest.ClientIDRoom, base *url.URL) {
	t.Helper()
	srv = ygows.NewServer()
	srv.HocuspocusFraming = true
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
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return srv, given, base
}

// A peer presents an empty client id, takes the one the room's first frame names, and every
// keystroke it sends lands under that id.
func TestAPeerWritesUnderTheClientIdTheRoomGivesIt(t *testing.T) {
	const artifact = "3c7a1e5f-9b2d-4f6a-8e0c-2d4b6f8a0c1e"
	seeded := typableDocument()
	srv, given, base := testRoom(t, artifact, crdt.EncodeStateAsUpdateV1(seeded, nil), true)
	// An observer that takes no id keeps the room resident after the peer has gone.
	observer := docstest.Dial(t, "ws"+strings.TrimPrefix(base.String(), "http")+"/ws/doc/"+artifact+"?schema_version="+strconv.Itoa(pmdoc.SchemaVersion()),
		http.Header{"X-Dispatch-User": {"observer"}}, artifact, crdt.New())
	select {
	case <-observer.Answers:
	case <-time.After(10 * time.Second):
		t.Fatal("the room sent the observer no document")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	r := result{artifact: artifact}
	typePeer(ctx, config{rate: 50, connectTimeout: 10 * time.Second}, roomURL(base, artifact),
		http.Header{"X-Dispatch-User": {"browsertype-0"}}, rand.New(rand.NewPCG(1, 1)), &r)
	if r.err != nil || r.sent == 0 {
		t.Fatalf("the peer sent %d updates and ended with %v, want some and no error", r.sent, r.err)
	}
	admissions := given.Admissions()
	if len(admissions) != 1 || admissions[0].Person != "browsertype-0" || admissions[0].Presented != "" {
		t.Fatalf("the room admitted %+v, want the peer alone, presenting an empty client id", admissions)
	}
	client := admissions[0].Given
	testwait.WithinPolling(t, 10*time.Second, 5*time.Millisecond, "the room to apply the peer's keystrokes", func() bool {
		return srv.GetDoc(artifact).StateVector().Clock(client) > 0
	})
	allowed := append(slices.Collect(maps.Keys(seeded.StateVector())), client)
	for holds := range srv.GetDoc(artifact).StateVector() {
		if !slices.Contains(allowed, holds) {
			t.Fatalf("the room holds edits under client id %d; the peer was given %d", holds, client)
		}
	}
}

// A room that gives no client id - a Dispatch build from before client ids, here ygo's server
// alone - sends its sync step 1 first. That is no failure: the peer syncs and types under the id
// its own document drew, and the dial hands that first frame on to the peer's reader, whose answer
// to it is how the room gets what the peer's document holds and the room lacks.
func TestAPeerAgainstARoomThatGivesNoIdWritesUnderItsOwn(t *testing.T) {
	const artifact = "6f0b2d4e-8a1c-4e3f-9d5b-7c9e1a3f5b2d"
	seeded := typableDocument()
	srv, _, base := testRoom(t, artifact, crdt.EncodeStateAsUpdateV1(seeded, nil), false)
	observer := docstest.Dial(t, "ws"+strings.TrimPrefix(base.String(), "http")+"/ws/doc/"+artifact+"?schema_version="+strconv.Itoa(pmdoc.SchemaVersion()),
		http.Header{"X-Dispatch-User": {"observer"}}, artifact, crdt.New())
	select {
	case <-observer.Answers:
	case <-time.After(10 * time.Second):
		t.Fatal("the room sent the observer no document")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	r := result{artifact: artifact}
	typePeer(ctx, config{rate: 50, connectTimeout: 10 * time.Second}, roomURL(base, artifact),
		http.Header{"X-Dispatch-User": {"browsertype-0"}}, rand.New(rand.NewPCG(1, 1)), &r)
	if r.err != nil || r.sent == 0 {
		t.Fatalf("the peer sent %d updates and ended with %v, want some and no error", r.sent, r.err)
	}
	var own []crdt.ClientID
	testwait.WithinPolling(t, 10*time.Second, 5*time.Millisecond, "the room to apply the peer's keystrokes", func() bool {
		own = own[:0]
		for client := range srv.GetDoc(artifact).StateVector() {
			if seeded.StateVector().Clock(client) == 0 {
				own = append(own, client)
			}
		}
		return len(own) > 0
	})
	if len(own) != 1 {
		t.Fatalf("the room holds edits under client ids %v beside the seed's, want the one the peer's document drew", own)
	}

	// A document holding an edit the room lacks, dialled as the peer dials: the room gets the edit
	// only from the answer to its first frame, the sync step 1 the dial read.
	local := crdt.New()
	if err := crdt.ApplyUpdateV1(local, crdt.EncodeStateAsUpdateV1(seeded, nil), nil); err != nil {
		t.Fatal(err)
	}
	fragment := local.GetXmlFragment(fragmentName)
	local.Transact(func(txn *crdt.Transaction) { paragraphTexts(fragment)[0].Insert(txn, 1, "x", nil) })
	held := local.StateVector().Clock(local.ClientID())
	if held == 0 {
		t.Fatal("the local document took no edit")
	}
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDial()
	peer, err := docstest.DialGivenContext(dialCtx, roomURL(base, artifact), http.Header{"X-Dispatch-User": {"browsertype-1"}}, artifact,
		func(given crdt.ClientID) *crdt.Doc {
			if given != 0 {
				t.Errorf("a room that gives no id gave %d", given)
			}
			return local
		})
	if err != nil {
		t.Fatalf("dial the room: %v", err)
	}
	defer peer.Close()
	testwait.WithinPolling(t, 10*time.Second, 5*time.Millisecond, "the room to take the edit the dialled document held", func() bool {
		return srv.GetDoc(artifact).StateVector().Clock(local.ClientID()) == held
	})
}
