package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/collabtest"
	"github.com/sjawhar/envoy/internal/oidc"
)

// The xid window rigs' decorators of the collaborators the api package hands the product: docs.API,
// the ones the api package declares, and the embedder, the file store and the agent stream. Each
// checks every method that takes a context (collabtest.Check) and calls through; newTestServer
// hands them to the product whenever a row installs an xid tracer, with identity's decorator
// (collabtest.CheckedIdentity), which the docs rig hands its document service too. They live here
// rather than in collabtest, which imports no collaborator's package but identity, so no other
// rig's test binary reads these packages through it.

// checkedDocs wraps every docs.API method. ScheduleSettlement takes no context, so it cannot be
// checked. A service that serves the document socket keeps serving it through the decorator
// (register mounts /ws/doc/{room} on a Docs that is an http.Handler).
func checkedDocs(t testing.TB, next docs.API) docs.API {
	if next == nil {
		return nil
	}
	checked := checkedDocsAPI{t: t, next: next}
	if socket, ok := next.(http.Handler); ok {
		return checkedDocsSocket{checkedDocsAPI: checked, socket: socket}
	}
	return checked
}

type checkedDocsSocket struct {
	checkedDocsAPI
	socket http.Handler
}

func (c checkedDocsSocket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.socket.ServeHTTP(w, r)
}

type checkedDocsAPI struct {
	t    testing.TB
	next docs.API
}

func (c checkedDocsAPI) check(ctx context.Context, method string) {
	collabtest.Check(c.t, ctx, "docs.API", method)
}

func (c checkedDocsAPI) SeedText(ctx context.Context, artifactID, markdown string, actor model.Actor) (string, error) {
	c.check(ctx, "SeedText")
	return c.next.SeedText(ctx, artifactID, markdown, actor)
}

func (c checkedDocsAPI) ReplaceText(ctx context.Context, artifactID, markdown string, actor model.Actor, precondition *model.EditPrecondition) (string, error) {
	c.check(ctx, "ReplaceText")
	return c.next.ReplaceText(ctx, artifactID, markdown, actor, precondition)
}

func (c checkedDocsAPI) IndexDocumentReferences(ctx context.Context, artifactID string, version int, markdown string) (model.ReferenceChanges, graphstore.Uncaused, error) {
	c.check(ctx, "IndexDocumentReferences")
	return c.next.IndexDocumentReferences(ctx, artifactID, version, markdown)
}

func (c checkedDocsAPI) RebuildDocument(ctx context.Context, artifactID string, markdown *string, actor model.Actor) (docs.RebuildReport, docs.VersionResult, error) {
	c.check(ctx, "RebuildDocument")
	return c.next.RebuildDocument(ctx, artifactID, markdown, actor)
}

func (c checkedDocsAPI) InjectSchemaInvalidForTest(ctx context.Context, artifactID string) error {
	c.check(ctx, "InjectSchemaInvalidForTest")
	return c.next.InjectSchemaInvalidForTest(ctx, artifactID)
}

func (c checkedDocsAPI) InjectUnknownElementForTest(ctx context.Context, artifactID string) error {
	c.check(ctx, "InjectUnknownElementForTest")
	return c.next.InjectUnknownElementForTest(ctx, artifactID)
}

func (c checkedDocsAPI) Text(ctx context.Context, artifactID string) (string, error) {
	c.check(ctx, "Text")
	return c.next.Text(ctx, artifactID)
}

func (c checkedDocsAPI) TextWithToken(ctx context.Context, artifactID string) (string, string, error) {
	c.check(ctx, "TextWithToken")
	return c.next.TextWithToken(ctx, artifactID)
}

func (c checkedDocsAPI) Blocks(ctx context.Context, artifactID string) ([]model.ArtifactBlock, error) {
	c.check(ctx, "Blocks")
	return c.next.Blocks(ctx, artifactID)
}

func (c checkedDocsAPI) TextWithBlocks(ctx context.Context, artifactID string) (string, []model.ArtifactBlock, error) {
	c.check(ctx, "TextWithBlocks")
	return c.next.TextWithBlocks(ctx, artifactID)
}

func (c checkedDocsAPI) TextWithTokenAndBlocks(ctx context.Context, artifactID string) (string, string, []model.ArtifactBlock, error) {
	c.check(ctx, "TextWithTokenAndBlocks")
	return c.next.TextWithTokenAndBlocks(ctx, artifactID)
}

func (c checkedDocsAPI) BlockPath(ctx context.Context, artifactID, blockID string) (model.BlockPath, error) {
	c.check(ctx, "BlockPath")
	return c.next.BlockPath(ctx, artifactID, blockID)
}

func (c checkedDocsAPI) SnapshotVersion(ctx context.Context, artifactID string, actor model.Actor) (docs.VersionResult, error) {
	c.check(ctx, "SnapshotVersion")
	return c.next.SnapshotVersion(ctx, artifactID, actor)
}

func (c checkedDocsAPI) SettleVersion(ctx context.Context, artifactID string, actor model.Actor) (docs.VersionResult, error) {
	c.check(ctx, "SettleVersion")
	return c.next.SettleVersion(ctx, artifactID, actor)
}

func (c checkedDocsAPI) SettleIssueDocuments(ctx context.Context, issueKey string) error {
	c.check(ctx, "SettleIssueDocuments")
	return c.next.SettleIssueDocuments(ctx, issueKey)
}

func (c checkedDocsAPI) WriteIssueAskStates(ctx context.Context, issueKey string, asks []model.Ask, actor model.Actor, markUnindexed bool) ([]docs.BlockRef, error) {
	c.check(ctx, "WriteIssueAskStates")
	return c.next.WriteIssueAskStates(ctx, issueKey, asks, actor, markUnindexed)
}

func (c checkedDocsAPI) MarkReopenOwedAskDocuments(ctx context.Context, issueKey string) error {
	c.check(ctx, "MarkReopenOwedAskDocuments")
	return c.next.MarkReopenOwedAskDocuments(ctx, issueKey)
}

func (c checkedDocsAPI) FollowClosedIssueVersions(ctx context.Context, issueKey string) error {
	c.check(ctx, "FollowClosedIssueVersions")
	return c.next.FollowClosedIssueVersions(ctx, issueKey)
}

func (c checkedDocsAPI) AcquireConditionalEdit(ctx context.Context, artifactID string) (func(), error) {
	c.check(ctx, "AcquireConditionalEdit")
	return c.next.AcquireConditionalEdit(ctx, artifactID)
}

func (c checkedDocsAPI) ApplyOps(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor, precondition *model.EditPrecondition) (docs.EditOutcome, error) {
	c.check(ctx, "ApplyOps")
	return c.next.ApplyOps(ctx, artifactID, ops, actor, precondition)
}

func (c checkedDocsAPI) SetBlockAttributes(ctx context.Context, artifactID, blockID string, attributes map[string]any, actor model.Actor) error {
	c.check(ctx, "SetBlockAttributes")
	return c.next.SetBlockAttributes(ctx, artifactID, blockID, attributes, actor)
}

func (c checkedDocsAPI) SetAskBlockText(ctx context.Context, artifactID, blockID string, edit docs.AskBlockEdit, actor model.Actor) (docs.AskBlockText, error) {
	c.check(ctx, "SetAskBlockText")
	return c.next.SetAskBlockText(ctx, artifactID, blockID, edit, actor)
}

func (c checkedDocsAPI) ScheduleSettlement(artifactID string) { c.next.ScheduleSettlement(artifactID) }

func (c checkedDocsAPI) SettleCopiesOf(ctx context.Context, ask model.Ask) error {
	c.check(ctx, "SettleCopiesOf")
	return c.next.SettleCopiesOf(ctx, ask)
}

func (c checkedDocsAPI) CopiesOf(ctx context.Context, ask model.Ask) ([]string, error) {
	c.check(ctx, "CopiesOf")
	return c.next.CopiesOf(ctx, ask)
}

func (c checkedDocsAPI) MarkQuote(ctx context.Context, artifactID string, mark docs.MarkSpec, quote string, occurrence *int) (docs.Anchored, error) {
	c.check(ctx, "MarkQuote")
	return c.next.MarkQuote(ctx, artifactID, mark, quote, occurrence)
}

func (c checkedDocsAPI) VerifyMark(ctx context.Context, artifactID string, kind docs.MarkKind, id string) (docs.Anchored, error) {
	c.check(ctx, "VerifyMark")
	return c.next.VerifyMark(ctx, artifactID, kind, id)
}

func (c checkedDocsAPI) BlockForQuote(ctx context.Context, artifactID, quote string) (string, error) {
	c.check(ctx, "BlockForQuote")
	return c.next.BlockForQuote(ctx, artifactID, quote)
}

func (c checkedDocsAPI) SuggestionKind(ctx context.Context, artifactID, id string) (string, error) {
	c.check(ctx, "SuggestionKind")
	return c.next.SuggestionKind(ctx, artifactID, id)
}

func (c checkedDocsAPI) AcceptSuggestion(ctx context.Context, artifactID, id, replaceWith string, actor model.Actor) error {
	c.check(ctx, "AcceptSuggestion")
	return c.next.AcceptSuggestion(ctx, artifactID, id, replaceWith, actor)
}

func (c checkedDocsAPI) RejectSuggestion(ctx context.Context, artifactID, id string, actor model.Actor) error {
	c.check(ctx, "RejectSuggestion")
	return c.next.RejectSuggestion(ctx, artifactID, id, actor)
}

func (c checkedDocsAPI) RejectDeletedCommentSuggestion(ctx context.Context, artifactID, id string, actor model.Actor) (string, error) {
	c.check(ctx, "RejectDeletedCommentSuggestion")
	return c.next.RejectDeletedCommentSuggestion(ctx, artifactID, id, actor)
}

func (c checkedDocsAPI) ProjectMark(ctx context.Context, artifactID, markID string, record docs.MarkRecord, actor model.Actor) error {
	c.check(ctx, "ProjectMark")
	return c.next.ProjectMark(ctx, artifactID, markID, record, actor)
}

func (c checkedDocsAPI) Join(ctx context.Context, tx pgx.Tx) (context.Context, *docs.Ledger) {
	c.check(ctx, "Join")
	return c.next.Join(ctx, tx)
}

func (c checkedDocsAPI) JoinWrite(ctx context.Context, artifactID string) error {
	c.check(ctx, "JoinWrite")
	return c.next.JoinWrite(ctx, artifactID)
}

func (c checkedDocsAPI) NamedVersion(ctx context.Context, artifactID, summary string, actor model.Actor) (docs.VersionResult, error) {
	c.check(ctx, "NamedVersion")
	return c.next.NamedVersion(ctx, artifactID, summary, actor)
}

func (c checkedDocsAPI) Evict(ctx context.Context, artifactID string) error {
	c.check(ctx, "Evict")
	return c.next.Evict(ctx, artifactID)
}

func (c checkedDocsAPI) Quiesce(ctx context.Context) error {
	c.check(ctx, "Quiesce")
	return c.next.Quiesce(ctx)
}

// checkedAgentPublisher wraps the one AgentPublisher method.
func checkedAgentPublisher(t testing.TB, next AgentPublisher) AgentPublisher {
	if next == nil {
		return nil
	}
	return checkedPublisher{t: t, next: next}
}

type checkedPublisher struct {
	t    testing.TB
	next AgentPublisher
}

func (c checkedPublisher) PublishReportingDuplicateWithin(ctx context.Context, envelope contracts.Envelope) (bool, error) {
	collabtest.Check(c.t, ctx, "api.AgentPublisher", "PublishReportingDuplicateWithin")
	return c.next.PublishReportingDuplicateWithin(ctx, envelope)
}

// checkedAnswerer wraps the one Answerer method.
func checkedAnswerer(t testing.TB, next Answerer) Answerer {
	if next == nil {
		return nil
	}
	return checkedAnswering{t: t, next: next}
}

type checkedAnswering struct {
	t    testing.TB
	next Answerer
}

func (c checkedAnswering) Run(ctx context.Context, request going.Request, emit func(going.Event) error) error {
	collabtest.Check(c.t, ctx, "api.Answerer", "Run")
	return c.next.Run(ctx, request, emit)
}

// checkedTokenVerifier wraps the one ServiceTokenVerifier method.
func checkedTokenVerifier(t testing.TB, next ServiceTokenVerifier) ServiceTokenVerifier {
	if next == nil {
		return nil
	}
	return checkedVerifier{t: t, next: next}
}

type checkedVerifier struct {
	t    testing.TB
	next ServiceTokenVerifier
}

func (c checkedVerifier) Verify(ctx context.Context, raw string) (oidc.Claims, error) {
	collabtest.Check(c.t, ctx, "api.ServiceTokenVerifier", "Verify")
	return c.next.Verify(ctx, raw)
}

// checkedEmbedder wraps Embed. Identity and Limits take no context, so they cannot be checked:
// both answer from the embedder's own configuration.
func checkedEmbedder(t testing.TB, next embed.Embedder) embed.Embedder {
	if next == nil {
		return nil
	}
	return checkedEmbedding{t: t, next: next}
}

type checkedEmbedding struct {
	t    testing.TB
	next embed.Embedder
}

func (c checkedEmbedding) Embed(ctx context.Context, texts []string, inputType embed.InputType) ([][]float32, error) {
	collabtest.Check(c.t, ctx, "embed.Embedder", "Embed")
	return c.next.Embed(ctx, texts, inputType)
}

func (c checkedEmbedding) Identity() string { return c.next.Identity() }

func (c checkedEmbedding) Limits() embed.Limits { return c.next.Limits() }

// checkedFiles wraps Put, Get and Healthy.
func checkedFiles(t testing.TB, next files.Store) files.Store {
	if next == nil {
		return nil
	}
	return checkedFileStore{t: t, next: next}
}

type checkedFileStore struct {
	t    testing.TB
	next files.Store
}

func (c checkedFileStore) Put(ctx context.Context, sha, mime string, body []byte) error {
	collabtest.Check(c.t, ctx, "files.Store", "Put")
	return c.next.Put(ctx, sha, mime, body)
}

func (c checkedFileStore) Get(ctx context.Context, sha string) (*files.Object, error) {
	collabtest.Check(c.t, ctx, "files.Store", "Get")
	return c.next.Get(ctx, sha)
}

func (c checkedFileStore) Healthy(ctx context.Context) error {
	collabtest.Check(c.t, ctx, "files.Store", "Healthy")
	return c.next.Healthy(ctx)
}

// checkedAgentStream wraps Replay. Subscribe and Watch take no context, so they cannot be checked.
func checkedAgentStream(t testing.TB, next agentstream.Source) agentstream.Source {
	if next == nil {
		return nil
	}
	return checkedStream{t: t, next: next}
}

type checkedStream struct {
	t    testing.TB
	next agentstream.Source
}

func (c checkedStream) Subscribe(sessionID string, deliver func(agentstream.Frame)) (func(), error) {
	return c.next.Subscribe(sessionID, deliver)
}

func (c checkedStream) Replay(ctx context.Context, sessionID string) (agentstream.Frame, error) {
	collabtest.Check(c.t, ctx, "agentstream.Source", "Replay")
	return c.next.Replay(ctx, sessionID)
}

func (c checkedStream) Watch(sessionID string) error { return c.next.Watch(sessionID) }

// The control of the decorators the api rig hands the product: a collaborator call one of them
// wraps is reported once its transaction has written, and not before. The call is identity's Login,
// through collabtest.CheckedIdentity, which the docs rig's own control calls too
// (docs.TestXidWindowCatchesACollaboratorCallInsideTheWindow), on the traced pool's marked context:
// before the transaction's first write the check stays silent, and after it the one call is
// reported, naming identity.Identity.Login. A decorator that checked nothing would pass every
// route row of the xid window table unseen.
func TestXidWindowCatchesACollaboratorCallInsideTheWindow(t *testing.T) {
	tracer := storetest.NewXidTracer()
	database := &store.Store{Pool: storetest.Traced(t, storetest.Open(t).Pool, tracer, nil)}
	caught := &collabtest.RecordedFailures{TB: t}
	people := collabtest.CheckedIdentity(caught, loginAs("alice@example.com"))
	ctx := storetest.MarkXid(context.Background())
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	login := func(when string) {
		t.Helper()
		if _, err := people.Login(httptest.NewRequest(http.MethodGet, "/api/v1/issues", nil).WithContext(ctx)); err != nil {
			t.Fatalf("%s: resolve the request's person: %v", when, err)
		}
	}
	login("before the write")
	if failures := caught.Reported(); len(failures) != 0 {
		t.Fatalf("a collaborator call before the transaction's first write was reported: %q", failures)
	}
	if _, err := tx.Exec(ctx, `insert into projects (key, name, default_assignee) values ('RIG', 'Rig', 'alice')`); err != nil {
		t.Fatalf("write: %v", err)
	}
	login("after the write")
	failures := caught.Reported()
	if len(failures) != 1 || !strings.HasPrefix(failures[0], "identity.Identity.Login was called while its caller holds a connection of the pool") {
		t.Fatalf("a collaborator call after the transaction's first write was reported as %q, want one failure naming identity.Identity.Login", failures)
	}
}

// loginAs is an identity that resolves every request to one person, reading nothing.
type loginAs string

func (l loginAs) Login(*http.Request) (string, error) { return string(l), nil }
