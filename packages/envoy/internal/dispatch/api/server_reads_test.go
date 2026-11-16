package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestReaderOfAnswersEachCallersViewer: readerOf answers each caller of verification v16 with the
// viewer access.Resolve gives it, the credential dropped; a reader a layer already put in the
// request's context is answered as it is, with no second resolution; and a credential that does
// not resolve is refused as every gate refuses one.
//
// The personal tokens that list a project read their list only once the server reads
// agent_tokens.projects, so the callers whose answer turns on that are not asked here.
func TestReaderOfAnswersEachCallersViewer(t *testing.T) {
	seed := seedPrivateProject(t)
	carol := seedCarol
	for _, want := range []struct {
		caller privateCaller
		viewer model.Viewer
	}{
		{callerAlice, model.Viewer{Person: seedAlice, Open: true, Sees: []string{seedPublic}, AnyPrivate: true}},
		{callerBob, model.Viewer{Person: seedBob, Restricted: true, Sees: []string{seedPrivate}, AnyPrivate: true}},
		{callerCarol, model.Viewer{Person: seedCarol, Open: true, Sees: []string{seedPrivate, seedPublic}, AnyPrivate: true}},
		{callerCarolToken, model.Viewer{Person: seedCarol, Open: true, Sees: []string{seedPublic}, AnyPrivate: true}},
		{callerSharedToken, model.Viewer{Open: true, Sees: []string{seedPublic}, AnyPrivate: true}},
	} {
		request := seed.readerRequest(t, want.caller)
		recorder := httptest.NewRecorder()
		reader, ok := seed.server.readerOf(recorder, request)
		if !ok {
			t.Fatalf("readerOf refused %s: %d %s", want.caller, recorder.Code, recorder.Body.String())
		}
		if reader.Credential != nil {
			t.Errorf("readerOf answered %s with its credential %+v: a reader carries none", want.caller, reader.Credential)
		}
		if reader.Viewer == nil {
			t.Fatalf("readerOf answered %s with no viewer", want.caller)
		}
		if !reflect.DeepEqual(*reader.Viewer, want.viewer) {
			t.Errorf("readerOf answered %s the viewer %+v, want %+v", want.caller, *reader.Viewer, want.viewer)
		}
		if want.caller == callerCarolToken && (reader.Owner == nil || *reader.Owner != carol) {
			t.Errorf("readerOf answered %s as %+v, want the token's person", want.caller, reader)
		}
	}

	kept := model.Actor{Kind: "user", ID: "kept", Viewer: &model.Viewer{Person: "kept", Sees: []string{}}}
	request := seed.readerRequest(t, callerAlice)
	request = request.WithContext(withReader(request.Context(), kept))
	reader, ok := seed.server.readerOf(httptest.NewRecorder(), request)
	if !ok || reader.ID != "kept" || reader.Viewer != kept.Viewer {
		t.Errorf("readerOf over a request whose context holds a reader answered %+v, %t; want that reader as it is", reader, ok)
	}

	// A token nobody minted: a minted one with its last character changed, so it has a minted
	// token's shape and resolves to no row.
	minted := seed.tokens[callerCarolToken]
	unminted := minted[:len(minted)-1] + map[bool]string{true: "B", false: "A"}[strings.HasSuffix(minted, "A")]
	refused := httptest.NewRequest(http.MethodGet, "/api/v1/issues", nil)
	refused.Header.Set("Authorization", "Bearer "+unminted)
	recorder := httptest.NewRecorder()
	if _, ok := seed.server.readerOf(recorder, refused); ok || recorder.Code != http.StatusUnauthorized {
		t.Errorf("readerOf over a token nobody minted answered %t with %d %s; want 401", ok, recorder.Code, recorder.Body.String())
	}
}

// readerRequest is a request as caller, as request sends one, for a test that calls a server
// method on it directly.
func (s privateSeed) readerRequest(t *testing.T, caller privateCaller) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/issues", nil)
	if login := caller.login(); login != "" {
		request.Header.Set("X-Dispatch-User", login)
	} else {
		request.Header.Set("Authorization", "Bearer "+s.bearer(t, caller))
	}
	return request
}

// serverReads names every use of everyReader in the api package's source, by the file and the
// function that holds it, with why that read answers no single caller. Each one reads content that
// belongs to one owner - one project or one conversation - and is gated per viewer where it leaves
// the server.
var serverReads = map[string]string{
	"comment_projection.go:commentEventPayload": "an event's payload is appended once and read by every subscriber of its topics " +
		"and every reader of its owner's event log, which the event log and the live socket gate by project",
	"message_delivery.go:finishPendingMessageDelivery": "a delivery's frame is sent to the session it targets and read again by " +
		"every reader of the conversation, which the conversation's own read gates by its parties",
	"comment_actions.go:reopenComment": "loadReplyChain: the thread a document projects (ProjectMark) is written into the document " +
		"every reader of that document reads, which the document's own read gates by its project",
	"comment_actions.go:editComment": "loadReplyChain: the thread a document projects (ProjectMark) is written into the document " +
		"every reader of that document reads, which the document's own read gates by its project",
	"comment_actions.go:commentAction": "loadReplyChain: the thread a document projects (ProjectMark) is written into the document " +
		"every reader of that document reads, which the document's own read gates by its project",
	"comment_create.go:createCommentFor": "loadReplyChain: the thread a document projects (ProjectMark) is written into the document " +
		"every reader of that document reads, which the document's own read gates by its project",
	"comment_queries.go:answerCommentList": "the comment list is kept under its owner and its ETag and handed to the next reader " +
		"who names the same pair, so it is read for no one reader; the owner's check runs before the kept answer is looked up",
	"event_stream_relay.go:page": "the relay reads one page of the event log for every subscriber at once, which each live socket " +
		"gates by its own viewer (eventReadableSQL) where it sends an event on",
	"live_events.go:replayThrough": "an events subscription's catch-up reads the log it replays before its socket sends any of it, " +
		"which the socket gates by its own viewer (eventReadableSQL) where it sends an event on",
	"delivery_sweep.go:resumeOwedDelivery": "the delivery sweep resends an owed attempt of a comment or a message a writer already " +
		"wrote, as that write's own send would, to the one session the attempt names; no caller reads what it loads",
	"delivery_sweep.go:settleSupersededMention": "the delivery sweep settles an owed mention attempt of a comment a writer already " +
		"wrote; it answers no caller, and the receipt it appends is read through the event log's rule",
	"delivery_sweep.go:settleUndeliverableMention": "the delivery sweep settles an owed mention attempt nothing can send; it answers " +
		"no caller, and the receipt it appends is read through the event log's rule",
	"comment_delivery.go:claimCommentDeliveryAttempt": "a mention's sender locks the comment it is already sending, which its writer " +
		"or the delivery sweep loaded, to claim the one attempt it sends; no caller reads what it locks",
	"suggestions.go:relatedSearch": "the related-item check runs off the write that triggered it, detached, with no credential of " +
		"the writer's to hand, and has read every project since before a project could be private; plan task t-a-14 binds the " +
		"writer's own viewer here in place of this read",
}

// TestEveryServerReadIsNamed holds every use of everyReader to serverReads. everyReader is the
// server's own read, ServerViewer, which sees every project and every party and proves no author,
// so each read it makes must say why no single caller is its reader. The uses are read out of the
// package's own source, so a use added later and left out of the table fails here, named, and so
// does an entry of the table that names no use.
func TestEveryServerReadIsNamed(t *testing.T) {
	if everyReader.Viewer != model.ServerViewer {
		t.Fatalf("everyReader reads with the viewer %+v, want model.ServerViewer", everyReader.Viewer)
	}
	if graphstore.ProvesAuthor(everyReader) {
		t.Fatalf("everyReader %+v proves an author, so a node reserved for its author would be in what everyone reads", everyReader)
	}
	uses := everyReaderUses(t)
	if len(uses) == 0 {
		t.Fatal("the api package's source holds no use of everyReader, so the walk read nothing")
	}
	for _, use := range uses {
		if _, named := serverReads[use]; !named {
			t.Errorf("%s reads as everyReader, the server's own viewer, and serverReads names no reason: "+
				"read as the caller (readerOf), or name the site with why no single caller reads what it reads", use)
		}
	}
	for _, site := range slices.Sorted(maps.Keys(serverReads)) {
		if !slices.Contains(uses, site) {
			t.Errorf("serverReads names %s, which holds no use of everyReader", site)
		}
	}
	t.Logf("everyReader is read at %v", uses)
}

// everyReaderUses is each function of the api package's source that names everyReader, as
// "file:function", sorted and distinct. The declaration itself is no use.
func everyReaderUses(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the package's source: %v", err)
	}
	fileSet := token.NewFileSet()
	found := map[string]bool{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if identifier, ok := node.(*ast.Ident); ok && identifier.Name == "everyReader" {
					found[file+":"+function.Name.Name] = true
				}
				return true
			})
		}
	}
	return slices.Sorted(maps.Keys(found))
}
