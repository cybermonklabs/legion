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
		{callerCarolPrivToken, model.Viewer{Person: seedCarol, Open: true, Lists: []string{seedPrivate}, Sees: []string{seedPrivate, seedPublic}, AnyPrivate: true}},
		{callerAlicePrivToken, model.Viewer{Person: seedAlice, Open: true, Lists: []string{seedPrivate}, Sees: []string{seedPublic}, AnyPrivate: true}},
		{callerBobPrivToken, model.Viewer{Person: seedBob, Restricted: true, Lists: []string{seedPrivate}, Sees: []string{seedPrivate}, AnyPrivate: true}},
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

// serverReads names every use of everyReader in the api package's source, by the file and the
// function that holds it, with why that read answers no single caller. Each one reads content that
// belongs to one owner - one project or one conversation - and is gated per viewer where it leaves
// the server, or names the slice whose change gates it.
var serverReads = map[string]string{
	"request_keys.go:heldAnswerRefusal": "a repeat's object is read as the server sees it only to tell an object nobody holds " +
		"from one the caller does not see: the same address is then read as the caller, and nothing the server read is " +
		"answered; only whether the caller's own read found it decides the repeat",
	"project_scope.go:withinProjectScope": "a full member's broker token that names one project (namedProjectOf) is held to " +
		"the project of each object its write resolves, read as the server sees it, so the check decides by the object's own " +
		"project and not by what the token sees; nothing the server read is answered: a write outside that project, or of a " +
		"private project the token does not name, is refused with the one answer that names only the credential's own " +
		"project, alike for a hidden object and a missing one",
	"comment_projection.go:commentEventPayload": "an event's payload is appended once and read by every subscriber of its topics " +
		"and every reader of its owner's event log",
	"comment_projection.go:commentAnchorRefreshEvent": "the payload of comment.anchor_refreshed, appended once and read by every " +
		"subscriber and every reader of its owner's event log, as commentEventPayload's is",
	"message_corrections.go:messageCorrectionEvent": "the payload of a message's edit or delete, appended once and read by every " +
		"subscriber and every reader of the conversation, as the create's frame is (finishPendingMessageDelivery)",
	"message_delivery.go:finishPendingMessageDelivery": "a delivery's frame is sent to the session it targets and read again by " +
		"every reader of the conversation, not by the person who wrote the message",
	"comment_projection.go:projectThreadMark": "the thread a document projects (ProjectMark) is written into the document every " +
		"reader of that document reads, which the document's own read gates by its project",
	"comment_actions.go:commentAction": "loadReplyChain: the thread a document projects (ProjectMark) is written into the document " +
		"every reader of that document reads, which the document's own read gates by its project",
	"comment_create.go:createCommentFor": "loadReplyChain: the thread a document projects (ProjectMark) is written into the document " +
		"every reader of that document reads, which the document's own read gates by its project",
	"comment_queries.go:answerCommentList": "the comment list is kept in listAnswers under its owner and its ETag and handed to the " +
		"next reader whose request names the same pair, so its comments are read for no one reader; the owner's check runs " +
		"before the kept answer is looked up. At S3ab answerCommentList leaves no session out; S3d leaves out the sessions a comment names for the caller",
	"delivery_sweep.go:resumeOwedDelivery": "the delivery sweep resends an owed attempt of a comment or a message a writer already " +
		"wrote, as that write's own send would, to the one session the attempt names; no caller reads what it loads, and it " +
		"puts this reader in ctx so the session route it resolves is read as the server's (resolveDeliveryTarget)",
	"delivery_sweep.go:settleSupersededMention": "the delivery sweep settles an owed mention attempt of a comment a writer already " +
		"wrote; it answers no caller, and the receipt it appends is read through the event log's rule",
	"delivery_sweep.go:settleUndeliverableMention": "the delivery sweep settles an owed mention attempt nothing can send; it answers " +
		"no caller, and the receipt it appends is read through the event log's rule",
	"comment_delivery.go:claimCommentDeliveryAttempt": "a mention's sender locks the comment it is already sending, which its writer " +
		"or the delivery sweep loaded, to claim the one attempt it sends; no caller reads what it locks",
	"results_ci.go:deriveCIResults": "the line of a result CI proves is the server's own, from CI's run, written with no request: it " +
		"drops the words of every project's keys (readLineWords), as every line did before projects could be private",
	"parent_status_fix.go:call": "R25's fix of a parent's status runs at boot for no caller: it writes the status the rule owes and " +
		"answers no one",
	"event_stream_relay.go:page": "the relay reads one page of the event log for every subscriber at once, which each live socket " +
		"gates by its own viewer (eventReadableSQL) where it sends an event on, and reads again as its viewer where that viewer " +
		"does not see every object (readerEvents)",
}

// TestEveryServerReadIsNamed holds every use of everyReader to serverReads. everyReader is the
// server's own read, ServerViewer, which sees every project and every party and proves no author,
// so each read it makes must say why no single caller is its reader. The uses are read out of the
// package's own source, so a use added later and left out of the table fails here, named, and so
// does an entry of the table that names no use.
func TestEveryServerReadIsNamed(t *testing.T) {
	t.Parallel()
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

// everyReaderUses is each function of the api package's source that names everyReader, or spells
// the server's own reader or viewer itself (model.ServerReader, model.ServerViewer), as
// "file:function", sorted and distinct. The declaration of everyReader is no use; a read that
// builds the server's reader inline, or binds model.ServerReader, is one, so it is named as every
// other is.
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
				switch node := node.(type) {
				case *ast.Ident:
					if node.Name == "everyReader" {
						found[file+":"+function.Name.Name] = true
					}
				case *ast.SelectorExpr:
					if node.Sel.Name == "ServerViewer" || node.Sel.Name == "ServerReader" {
						found[file+":"+function.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	return slices.Sorted(maps.Keys(found))
}
