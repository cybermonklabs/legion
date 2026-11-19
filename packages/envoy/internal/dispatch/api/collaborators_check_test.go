package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/collabtest"
	"github.com/sjawhar/envoy/internal/oidc"
)

// The xid window rigs' decorators of the collaborators the api package hands the product: docs.API,
// the ones the api package declares, and identity, the embedder, the file store and the agent
// stream. Each checks every method that takes a context (collabtest.Check) and calls through;
// newTestServer hands them to the product whenever a row installs an xid tracer. They live here
// rather than in collabtest, which imports no collaborator's package, so no other rig's test
// binary reads these packages through it.

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

func (c checkedDocsAPI) ReplaceText(ctx context.Context, artifactID, markdown string, actor model.Actor) (string, error) {
	c.check(ctx, "ReplaceText")
	return c.next.ReplaceText(ctx, artifactID, markdown, actor)
}

func (c checkedDocsAPI) IndexDocumentReferences(ctx context.Context, artifactID string, version int, markdown string) (model.ReferenceChanges, error) {
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

// checkedIdentity wraps Login, which takes its context from the request it resolves.
func checkedIdentity(t testing.TB, next identity.Identity) identity.Identity {
	if next == nil {
		return nil
	}
	return checkedLogin{t: t, next: next}
}

type checkedLogin struct {
	t    testing.TB
	next identity.Identity
}

func (c checkedLogin) Login(r *http.Request) (string, error) {
	collabtest.Check(c.t, r.Context(), "identity.Identity", "Login")
	return c.next.Login(r)
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
