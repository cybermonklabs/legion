package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// serverReads names every use of everyReader in the api package's source, by the file and the
// function that holds it, with why that read answers no single caller. Each one reads content that
// belongs to one owner - one project or one conversation - and is gated per viewer where it leaves
// the server, or names the slice whose change gates it.
var serverReads = map[string]string{
	"request_keys.go:heldAnswerHidden": "a repeat's object is read as the server sees it only to tell an object nobody holds " +
		"from one the caller does not see: the same address is then read as the caller, and nothing the server read is " +
		"answered; only whether the caller's own read found it decides the repeat",
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
	"event_stream_relay.go:page": "the relay reads one page of the event log for every subscriber at once; at this slice every " +
		"events subscription is sent every event, as main sends it, and S4 gates each live socket by its own viewer where it " +
		"sends an event on",
	"live_events.go:resolveEventReplay": "the /ws/live events view replays the log as the relay publishes it, for every " +
		"subscriber alike; S4 replays it as each socket's own reader (replayThrough with that reader)",
	"delivery_live_hub_view.go:parseDeliveryLiveView": "the five Delivery views on the live socket read as the server's reader " +
		"while the socket keeps none of its caller's beyond its admission; S4 resolves each subscriber's reader into its " +
		"view (deliveryHub.Subscribe) and re-checks it on the socket's heartbeat",
	"going_routes.go:startGoingRun": "a going run's search reads as the server's reader, as a run matched every row its query " +
		"matched before projects could be private: a bridge, which S3c-issues replaces with the run's author as a reader when " +
		"it decides what a run answers",
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
