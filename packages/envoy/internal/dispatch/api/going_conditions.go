package api

// going_conditions.go is how a request the command bar sends in-process is made conditional. An
// undo's write back (going_undo.go) - an issue's fields, an ask's parts, a document's content, a
// comment's or an issue message's body, a claim taken back or released, a thread reopened or
// resolved again, a created issue closed - puts its value back only where the item still holds
// what the undone write left, and every upload the bar sends is conditional too: a create alone,
// which makes a document only where its owner holds none of the name, or, for a row of a plan
// whose approval showed the document or file of its name and whose read before finds it as the
// approval showed it, a write over that very artifact at the version the read found, refused
// where another writer wrote it since (a row whose read finds anything else under the name waits
// for its person again: AgentListener.rowUpload). Each is a compare-and-set its route decides
// under the lock it takes for the write, never a read and then a write.
//
// The condition rides on the request's context (goingSend.condition, withGoingCondition), never
// in its body. No client off the network can set a context, so none can make a route conditional
// or send one a route of another build would drop: two builds of Dispatch serve one database
// while a release rolls out, and a route ignores a member it does not declare (decodeJSON), so a
// condition carried as a member and dropped would write over another writer's change.
// A token a route reads off every caller's body is no such condition, and a write back sends it
// there as any caller does: an issue's advances_revision, and an upload's precondition, the
// document token the undo's read returned. Main's routes declare both, so no build drops either.
//
// A write back says it requires its condition (goingCondition.WriteBack): a route refuses one that
// carries no condition the route decides, 500 GOING_CONDITION_MISSING, and writes nothing
// (goingCondition.writeBackRefusal), so plumbing a later change loses never turns an undo into an
// unconditional write. Every other request - the web app's, the CLI's, the bar's own forward
// writes - carries no mark and writes as it always has.

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// codeGoingConditionMissing is a write back that carries no condition its route decides: the
// server's own failure, answered before anything is written.
const codeGoingConditionMissing = "GOING_CONDITION_MISSING"

// goingConditionKey carries a goingCondition on the context of the request it conditions.
type goingConditionKey struct{}

// goingCondition is the compare-and-set one in-process request carries to its route, each field
// read by the route it names, under the lock that route takes for the write. Its fields are
// exported for the request's digest alone (goingSend.digest), which holds them as part of what
// the request sends.
type goingCondition struct {
	// WriteBack marks a request an undo sends: its route refuses it, writing nothing, unless it
	// carries a condition that route decides.
	WriteBack bool
	// Expect is PATCH /api/v1/issues/{key}'s and PATCH /api/v1/asks/{id}'s: each field the write
	// writes and the value it must hold, as the item's own read answers it (rank an issue's rank
	// string), or the write is refused whole, 409 ISSUE_CHANGED or ASK_CHANGED
	// (issueExpectationRefusal, askExpectationRefusal).
	Expect map[string]json.RawMessage
	// ExpectArtifact is an upload's: the id of the document the name must find, or the upload is
	// refused, 409 DOCUMENT_CHANGED, as it is where the name finds none. An undo's write back names
	// it beside the token its read returned, which the upload's body carries as its precondition,
	// as every caller's does, and the write checks under the document's locks (409
	// PRECONDITION_FAILED). Without a precondition, or with ExpectNew or ExpectVersion, it is 400
	// ARTIFACT_INPUT. While empty it is left out of the request's digest (goingSend.digest).
	ExpectArtifact string `json:",omitempty"`
	// ExpectNew is an upload's: the owner must hold no artifact the name finds, or the upload is
	// refused, 409 DOCUMENT_EXISTS.
	ExpectNew bool
	// ExpectVersion is an upload's over a document or a file its owner holds: the artifact the name
	// finds must be this one at this latest version, or the upload is refused, 409
	// DOCUMENT_CHANGED, as it is where the name finds none. With ExpectNew or ExpectArtifact it is
	// 400 ARTIFACT_INPUT. While nil it is left out of the request's digest (goingSend.digest).
	ExpectVersion *goingArtifactVersion `json:",omitempty"`
	// ExpectHolder is a release's: the actor who must hold the claim, or the release is refused,
	// 409 CLAIM_HOLDER_CHANGED.
	ExpectHolder *model.Actor
	// ExpectUnclaimed is a claim's: nobody - the caller too - may hold the issue, or the claim is
	// refused, 409 CLAIM_HOLDER_CHANGED.
	ExpectUnclaimed bool
	// ExpectResolvedAt is a reopen's: the moment of the resolution the thread must hold, as the
	// comment's read answers resolved_at, or the reopen is refused, 409 COMMENT_CHANGED.
	ExpectResolvedAt *string
	// ExpectOpen is a resolve's: the thread must be open, or the resolve is refused, 409
	// COMMENT_CHANGED. While false it is left out of the request's digest (goingSend.digest), so a
	// request that carries none digests as a build without the field records it.
	ExpectOpen bool `json:",omitempty"`
	// ExpectBody is PATCH /api/v1/comments/{id}'s and PATCH /api/v1/messages/{id}'s: the body the
	// post must hold, or the edit is refused, 409 COMMENT_CHANGED or MESSAGE_CHANGED. While nil it
	// is left out of the request's digest (goingSend.digest), as ExpectOpen is.
	ExpectBody *string `json:",omitempty"`
}

// goingArtifactVersion is an artifact and the number of its latest version, as a read found them.
type goingArtifactVersion struct {
	Artifact string
	Version  int
}

// withGoingCondition is ctx carrying condition to the route of the request it is the context of.
func withGoingCondition(ctx context.Context, condition goingCondition) context.Context {
	return context.WithValue(ctx, goingConditionKey{}, condition)
}

// goingConditionOf is the condition the request's context carries, none for every request but one
// the command bar sends in-process with one.
func goingConditionOf(r *http.Request) goingCondition {
	condition, _ := r.Context().Value(goingConditionKey{}).(goingCondition)
	return condition
}

// writeBackRefusal is the refusal of a write back whose condition for its route is missing,
// empty or invalid - decided false - naming what it lacks; nil where decided holds or the request
// is no write back.
func (c goingCondition) writeBackRefusal(decided bool, lacks string) error {
	if !c.WriteBack || decided {
		return nil
	}
	return errorf(http.StatusInternalServerError, codeGoingConditionMissing,
		"this write back carries no %s to decide it by, so nothing was written", lacks)
}

// expects reports whether Expect names every one of fields: a write back of an issue's fields or
// an ask's parts conditions each it writes.
func (c goingCondition) expects(fields []string) bool {
	for _, field := range fields {
		if _, named := c.Expect[field]; !named {
			return false
		}
	}
	return true
}
