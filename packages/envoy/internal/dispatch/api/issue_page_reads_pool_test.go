package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// issuePageSeed is an issue holding every read the new issue page makes of it (#4178): a spec
// whose requirements a child's plan task and the issue's own check serve, a result recorded
// against that check, and the references reference-targets resolves.
type issuePageSeed struct {
	key        string
	references []string
}

// seedIssuePage is the issue every row below reads. Its references name each kind of node the
// batched read resolves - an issue, a document, a block of a document, a comment, a message, an
// ask, and a block of each of those three - and the comment, the message, the ask, the document
// and the document's block a second time by an id written short, the block pinned to a version
// once more, so the read runs every statement it can: ResolvePrefixes' six, a TargetNames
// statement per node kind, and a BlockTexts statement per kind of block.
func seedIssuePage(t *testing.T, handler http.Handler) issuePageSeed {
	t.Helper()
	createReferenceAPIProject(t, handler, "PAGE")
	issue := createSpecIssue(t, handler, "PAGE", "A paragraph of the spec.\n\n"+twoRequirements)
	spec := "dispatch://" + issue.Key + "/spec"
	child := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "PAGE", "title": "Child", "parent": issue.Key, "force": true}, "alice"))
	uploadDocument(t, handler, "/api/v1/issues/"+child.Key, "plan.md", planTaskServing("t1", spec+"#b-r2"))
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", checkServing("k1", spec+"#b-r1"))
	if recorded := recordResult(t, handler, issue.Key, resultBody("k1", "pass", strings.Repeat("1", 40), nil)); recorded.Code != http.StatusCreated {
		t.Fatalf("record a result: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	paragraph := blockIDsByText(t, handler, issue.PrimaryArtifactID)["A paragraph of the spec."]
	if len(paragraph) < 8 {
		t.Fatalf("the spec's paragraph has the block id %q, want one an id written short can name", paragraph)
	}
	comment := createReferenceAPIComment(t, handler, issue.Key, "A comment's first block.\n\nIts second block.")
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "A message's first block.\n\nIts second block."}, "alice")
	ask := readPartAsk(t, handler, createPartAsk(t, handler, issue.Key, "Which requirement first?",
		map[string]string{"label": "r1"}, map[string]string{"label": "r2"}).ID)
	if len(ask.Options) == 0 || ask.Options[0].ID == "" {
		t.Fatalf("the ask answers options %#v, want a part id on its first", ask.Options)
	}
	notes := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "notes.md", "A note.\n")
	item := "dispatch://" + issue.Key
	short := spec + "#b-" + text.EncodeBlockID(paragraph[:8])
	return issuePageSeed{key: issue.Key, references: []string{
		item,
		spec,
		spec + "#b-r1",
		spec + "#b-" + text.EncodeBlockID(paragraph),
		short,
		short + "@v1",
		item + "/comment/" + comment.ID,
		item + "/comment/" + comment.ID[:8],
		item + "/comment/" + comment.ID + "#b-b2",
		item + "/message/" + message.ID,
		item + "/message/" + message.ID[:8],
		item + "/message/" + message.ID + "#b-b2",
		item + "/ask/" + ask.ID,
		item + "/ask/" + ask.ID[:8],
		askPartRef(issue.Key, ask.ID, ask.Options[0].ID),
		item + "/artifact/" + notes.Artifact.ID[:8],
	}}
}

// issuePageReadAcquires is how many request-pool connections one GET of target takes, read as
// alice with `Dispatch-Trees: 1`, the read that runs the most statements: the trees of what it
// answers, from the table and from the block index, besides everything a read without the header
// runs. The same read without the header is made first, so every document it reads is in the
// read cache when the counted read comes: the first read of a document no room holds folds its
// stored history on a connection of its own, apart from the request's and never one admission
// hands out (docs.Service.foldContext), which is the document's work and not the route's. That
// first read asks for no tree, so the counted read still asks the table for every tree it
// answers. It fails the row where the read leaves a request-pool connection taken once it
// answered.
func issuePageReadAcquires(t *testing.T, handler http.Handler, database *store.Store, target string) uint64 {
	t.Helper()
	treesRequest(t, handler, target, false)
	awaitQuietRequestPool(t, database.Pool)
	before := requestPoolStats(t, database.Pool).Acquire.Count
	treesRequest(t, handler, target, true)
	acquires := requestPoolStats(t, database.Pool).Acquire.Count - before
	if held := database.Pool.Stat().AcquiredConns(); held != 0 {
		t.Errorf("GET %s left %d request-pool connections taken once it answered, want 0", target, held)
	}
	return acquires
}

// A batched reference read is one pinned read (served, apiRoute.pinsConnection): the caller's
// identity, the short ids resolved (refs.ResolvePrefixes), the names of every kind of node named
// (refs.TargetNames), the blocks' texts (refs.BlockTexts) and the stored trees all run one after
// another on the one request-pool connection its first statement took, each refs call handed
// Store.Pool, which under the pin runs on that connection. Served without the pin, every one of
// those statements takes a connection of its own and the row fails.
func TestAReferenceTargetsRequestTakesOneRequestPoolConnection(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	seed := seedIssuePage(t, handler)
	if acquires := issuePageReadAcquires(t, handler, database, referenceTargetsOf(seed.references...)); acquires != 1 {
		t.Errorf("one reference-targets answer of %d references took %d request-pool connections, want 1: its pinned connection",
			len(seed.references), acquires)
	}
}

// A results read is one pinned read: the issue, its Verification document, the document's checks
// with their trees (checkBlocksOf), the recorded results and retractions, the comparisons and the
// stored trees all run on the request's one pinned connection. Served without the pin, each takes
// a connection of its own and the row fails.
func TestAResultsRequestTakesOneRequestPoolConnection(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	seed := seedIssuePage(t, handler)
	if acquires := issuePageReadAcquires(t, handler, database, "/api/v1/issues/"+seed.key+"/results"); acquires != 1 {
		t.Errorf("one results answer took %d request-pool connections, want 1: its pinned connection", acquires)
	}
}

// A coverage read is one pinned read: the issue, its spec and plan (issueCoverage), the spec's
// requirements with their trees, the issue's descendants, the blocks that serve each requirement
// and the stored trees all run on the request's one pinned connection. Served without the pin,
// each takes a connection of its own and the row fails.
func TestACoverageRequestTakesOneRequestPoolConnection(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	seed := seedIssuePage(t, handler)
	if acquires := issuePageReadAcquires(t, handler, database, "/api/v1/issues/"+seed.key+"/coverage"); acquires != 1 {
		t.Errorf("one coverage answer took %d request-pool connections, want 1: its pinned connection", acquires)
	}
}
