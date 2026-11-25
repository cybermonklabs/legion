package api

// v2_refusals.go is the refusals that the /api/v2 rows of more than one group declare, each a
// condition and its one code (dispatch://INFRA-4071/spec R7), and the families of them that one
// shared write answers, which every row whose handler runs that write declares through one helper.

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var (
	issueClosedRefusal         = apicode.Refusal{Condition: apicode.OwnerIssueClosed, Code: apicode.IssueClosed}
	postDeletedRefusal         = apicode.Refusal{Condition: apicode.PostWithdrawn, Code: apicode.PostDeleted}
	valuePastBoundRefusal      = apicode.Refusal{Condition: apicode.ValuePastBound, Code: apicode.CapExceeded}
	artifactNotDocumentRefusal = apicode.Refusal{Condition: apicode.ArtifactNotDocument, Code: apicode.NotDocument}
	sessionIDMalformedRefusal  = apicode.Refusal{Condition: apicode.SessionIDMalformed, Code: apicode.InvalidSessionID}
	anchorInvalidRefusal       = apicode.Refusal{Condition: apicode.AnchorInvalid, Code: apicode.InvalidAnchor}
	envoyUnreachableRefusal    = apicode.Refusal{Condition: apicode.EnvoyUnreachable, Code: apicode.EnvoyUnavailable}
	envoyRefillingRefusal      = apicode.Refusal{Condition: apicode.EnvoyListRefilling, Code: apicode.EnvoyRefilling}
	// actingSessionMismatchRefusal is actingPerson's refusal of a `session` parameter naming
	// another session than the credential proved, which a row declares where a value of its
	// request, not every request, acts as the person (openAPIOperationCodes declares it for a row
	// whose OAuthScopes hold person, and for a body that names its writer).
	actingSessionMismatchRefusal = apicode.Refusal{Condition: apicode.ActingSessionMismatch, Code: apicode.ActorSessionMismatch}
	assigneeNotSignedInRefusal   = apicode.Refusal{Condition: apicode.AssigneeNotSignedIn, Code: apicode.AssigneeNotAllowed}
	gitHubReadFailedRefusal      = apicode.Refusal{Condition: apicode.GitHubReadFailed, Code: apicode.GitHubUnreadable}
	refPrefixAmbiguousRefusal    = apicode.Refusal{Condition: apicode.RefPrefixAmbiguous, Code: apicode.IDAmbiguous}
	referenceInvalidRefusal      = apicode.Refusal{Condition: apicode.ReferenceQueryInvalid, Code: apicode.InvalidReference}
	issueStatusInvalidRefusal    = apicode.Refusal{Condition: apicode.IssueStatusInvalid, Code: apicode.InvalidStatus}
	rankInvalidRefusal           = apicode.Refusal{Condition: apicode.RankInvalid, Code: apicode.RankInput}
	reasonMissingRefusal         = apicode.Refusal{Condition: apicode.ReasonMissing, Code: apicode.ReasonRequired}
	// queryProjectMalformedRefusal is the refusal of a project filter whose value is no project key.
	queryProjectMalformedRefusal = apicode.Refusal{Condition: apicode.QueryProjectMalformed, Code: apicode.InvalidProject}
)

// envoyListenerRefusals are the refusals of a route that needs the Envoy listener's list of
// agents, after more, the route's own: a Dispatch configured with no listener or whose listener
// did not answer, and a listener that refills its list.
func envoyListenerRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return append(more, envoyUnreachableRefusal, envoyRefillingRefusal)
}

// unsubscribeRefusals are the refusals of the unsubscription of a session from an owner's topics
// (unsubscribeSession), after more, the route's own: the listener's (envoyListenerRefusals) and
// an unsubscription the listener does not take, a topic only a broader one matches, and a session
// that holds none.
func unsubscribeRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return envoyListenerRefusals(append(more,
		apicode.Refusal{Condition: apicode.EnvoyUnsubscribeRefused, Code: apicode.EnvoyUnsubscribeFailed},
		apicode.Refusal{Condition: apicode.TopicNotRemovable, Code: apicode.NotRemovable},
		apicode.Refusal{Condition: apicode.SessionNotSubscribed, Code: apicode.SubscriberNotFound},
	)...)
}

// The refusals of a project document's route given a document that is no project's: an issue's
// document, and a file of an agent's conversation.
var (
	documentOfIssueRefusal        = apicode.Refusal{Condition: apicode.DocumentOfIssue, Code: apicode.ArtifactLinked}
	documentOfConversationRefusal = apicode.Refusal{Condition: apicode.DocumentOfConversation, Code: apicode.ArtifactAgentOwned}
)

// projectDocumentRefusals are the refusals of a route of a project document's comments, asks,
// events or subscribers, after more, the route's own: a document its {id} names that does not
// exist, an issue's document and a file of an agent's conversation, which documentOwnerFromRequest
// answers on every such route.
func projectDocumentRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return append(more, documentMissingRefusal, documentOfIssueRefusal, documentOfConversationRefusal)
}

// The refusals of a document the document service could not serve, which writeHandlerError
// names by documentErrorCode whichever route met it: a room or a service that cannot serve it, a
// stored history that does not load, an element a newer build wrote, and a tree outside the
// schema.
var (
	documentServiceDownRefusal   = apicode.Refusal{Condition: apicode.DocumentServiceDown, Code: apicode.DocServiceUnavailable}
	documentUnloadableRefusal    = apicode.Refusal{Condition: apicode.DocumentHistoryUnloadable, Code: apicode.DocumentUnloadable}
	documentNewerContentRefusal  = apicode.Refusal{Condition: apicode.DocumentNewerContent, Code: apicode.DocNewerContent}
	documentOutsideSchemaRefusal = apicode.Refusal{Condition: apicode.DocumentOutsideSchema, Code: apicode.DocSchema}
)

// documentRefusals are the refusals of a route that opens or reads a document through the
// document service and passes its error on to writeHandlerError, after more, the route's own: the
// four of a document the service could not serve. Every such row declares them through this
// helper, so none leaves one out, but the rebuild (POST /api/v2/artifacts/{id}/rebuilds), which
// lists two of the four itself: it repairs a history that does not load and parses its source
// again, so neither a history that does not load nor a stored tree outside the schema is its
// refusal. A route that reaches the service only through anchorBlock declares none, since
// anchorBlock names the error in the answer's anchor_block_error and answers no refusal.
func documentRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return append(more, documentServiceDownRefusal, documentUnloadableRefusal, documentNewerContentRefusal, documentOutsideSchemaRefusal)
}

// The refusals of what a write puts into a document, and of what it names there, that the
// document service answers on more than one route.
var (
	markdownRefusedRefusal       = apicode.Refusal{Condition: apicode.MarkdownRefused, Code: apicode.InvalidMarkdown}
	askBlockRefusedRefusal       = apicode.Refusal{Condition: apicode.AskBlockRefused, Code: apicode.InvalidAskBlock}
	ciCheckRuleBrokenRefusal     = apicode.Refusal{Condition: apicode.CICheckRuleBroken, Code: apicode.InvalidCICheck}
	documentPastSizeBoundRefusal = apicode.Refusal{Condition: apicode.DocumentPastSizeBound, Code: apicode.CapExceededTooLarge}
	targetMissingRefusal         = apicode.Refusal{Condition: apicode.TargetMissing, Code: apicode.TargetNotFound}
	quoteAmbiguousRefusal        = apicode.Refusal{Condition: apicode.QuoteAmbiguous, Code: apicode.TargetAmbiguous}
	anchorDocumentMissingRefusal = apicode.Refusal{Condition: apicode.AnchorDocumentMissing, Code: apicode.NotFound}
	editLostRefusal              = apicode.Refusal{Condition: apicode.EditLost, Code: apicode.EditLostToConcurrentChange}
)

// markdownWriteRefusals are the refusals of markdown a write puts into a document whole or in
// part, after more, the route's own: markdown that is no Proof document or names a block it may
// not, an ask block that does not read or an open one it changes, a check block breaking a rule
// of a check CI proves, and a document past its size bound. POST /api/v2/issues, which seeds its
// spec straight into the store, and the rebuild, which lists two of documentRefusals' four
// itself, declare them through this helper and not documentContentRefusals; the edit route
// declares its own, since it answers a markdown refusal as INVALID_OP.
func markdownWriteRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return append(more, markdownRefusedRefusal, askBlockRefusedRefusal, ciCheckRuleBrokenRefusal, documentPastSizeBoundRefusal)
}

// documentContentRefusals are the refusals of markdown a write puts into a document through the
// document service (an upload, an accepted suggestion), after more, the route's own: those of the
// markdown (markdownWriteRefusals), with the four of a document the service could not serve.
func documentContentRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return documentRefusals(markdownWriteRefusals(more...)...)
}

// anchorRefusals are the refusals of the anchor a comment or an ask writes on a document
// (resolveAnchor), after more, the route's own: a quote the document does not hold or holds in
// several places, a mark_id it does not hold when the wait ends, and marks past the document's
// size bound, with the four of a document the service could not serve. The ambiguous quote's body
// is anchorRefusalAnswers'.
func anchorRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return documentRefusals(append(more, targetMissingRefusal, quoteAmbiguousRefusal, documentPastSizeBoundRefusal,
		apicode.Refusal{Condition: apicode.AnchorMarkMissing, Code: apicode.AnchorMissing})...)
}

// anchorRefusalAnswers are the refusal bodies richer than {code, error} of a write that anchors
// on a document: 409 TARGET_AMBIGUOUS with the places the quote matched (writeAmbiguousTarget).
func anchorRefusalAnswers() []routeAnswer {
	return []routeAnswer{{Status: http.StatusConflict, Body: ambiguousTargetError{}}}
}

// uploadPreconditionRefusalAnswers are the refusal bodies richer than {code, error} of an upload:
// 409 PRECONDITION_FAILED for a stale document or file token, naming the current version
// (uploadPreconditionBody, fileUploadPreconditionBody).
func uploadPreconditionRefusalAnswers() []routeAnswer {
	return []routeAnswer{{Status: http.StatusConflict, Body: editPreconditionError{}}}
}
