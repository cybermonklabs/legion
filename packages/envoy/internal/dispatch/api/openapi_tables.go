// The per-type data the OpenAPI emitter reads: the schema names that differ from their Go type's,
// what each json.RawMessage field's bytes carry, the schema fragments written out where no Go
// type names them, and the multipart body the upload routes take. They stand apart from
// openapi.go so that file is the reflection and the operation building alone, and so a reader
// looking for "what does this field hold" finds the tables without reading the walk.
package api

import (
	"reflect"

	"github.com/sjawhar/envoy/internal/broker/wire"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// schemaNames overrides the schema name a Go type is declared under. It exists so a Go type whose
// name differs from the name its TypeScript consumers import can keep that name: the default is
// the Go type's own name with its first letter upper-cased, and a schema's name is the name
// openapi-typescript renders it as, which is the name every TypeScript consumer imports it by.
// A schema named anything else would rename an export for no reason.
//
// Every row was measured against packages/contracts/src/dispatch-api.ts: the Go type's property
// set equals the property set of the interface it is named for, that interface's `extends` chain
// flattened, so the name it takes means what its consumers already import. A row naming a Go type
// the document does not reflect fails the emit, so the table cannot rot.
var schemaNames = map[reflect.Type]string{
	// GET /api/v1/issues/{key}/summary's answer, against model.IssueSummary, the row GET
	// /api/v1/issues lists. The list row holds the name, since the dashboard reads its issues
	// through it; this is the reference card, which its TypeScript consumers already import as
	// IssueReferenceSummary (packages/contracts/src/dispatch-api.ts).
	reflect.TypeOf(issueSummary{}): "IssueReferenceSummary",
	// The owner of a GET /api/v1/asks/open row, against model.OpenAskOwner, which three routes
	// answer (GET /api/v1/me/answers, /me/replies and /agents/{session_id}/answers) and whose
	// shape is what the TypeScript OpenAskOwner says. This one carries the issue's assignee
	// besides, so the two are not one schema.
	reflect.TypeOf(openAskOwner{}): "OpenAskItemOwner",

	// This package's own shapes, whose Go names say what the handler called them and whose
	// TypeScript names say what a client reads.
	reflect.TypeOf(acceptedComment{}):          "AcceptSuggestionResponse",
	reflect.TypeOf(acceptedDelivery{}):         "AcceptedMessageDelivery",
	reflect.TypeOf(agentResponse{}):            "Agent",
	reflect.TypeOf(userAskSnooze{}):            "AskSnooze",
	reflect.TypeOf(claimAnswer{}):              "ClaimedIssue",
	reflect.TypeOf(commentInput{}):             "CreateCommentInput",
	reflect.TypeOf(createAgentTokenResponse{}): "CreatedAgentToken",
	reflect.TypeOf(documentSummary{}):          "DocumentReferenceSummary",
	reflect.TypeOf(inboxAsk{}):                 "InboxRow",
	reflect.TypeOf(issueDetail{}):              "IssueDetails",
	reflect.TypeOf(rankInput{}):                "IssueRankInput",
	reflect.TypeOf(issueSummaryDocument{}):     "IssueReferenceSummaryDocument",
	reflect.TypeOf(replyRead{}):                "MessageReplyResult",
	reflect.TypeOf(checkResultInput{}):         "RecordCheckResultInput",
	reflect.TypeOf(relatedRequest{}):           "RelatedCheckInput",

	// The members of the collaborators answer, whose Go names say which handler built them and
	// whose names here are the ones main's TypeScript exported and a consumer imports
	// (packages/contracts/.changes/unreleased/pr2234-added-1.yaml announces them).
	// Declared under their Go names they would delete three exports main publishes.
	reflect.TypeOf(collaboratorPerson{}): "IssueCollaboratorPerson",
	reflect.TypeOf(collaboratorAgent{}):  "IssueCollaboratorAgent",
	reflect.TypeOf(collaboratorLegion{}): "IssueCollaboratorLegion",

	// The model and document packages, whose one-word names would take a word of the API's own.
	reflect.TypeOf(model.Mention{}):        "CommentMention",
	reflect.TypeOf(model.NodeParent{}):     "GraphNodeParent",
	reflect.TypeOf(model.SearchArtifact{}): "SearchArtifactRef",
	reflect.TypeOf(docs.RebuildReport{}):   "ArtifactRebuildReport",
	reflect.TypeOf(pmdoc.Candidate{}):      "TargetCandidate",

	// The broker's wire shapes, which the relay rows answer; each is one of Dispatch's
	// credential pages rather than a shape of the broker's own naming.
	reflect.TypeOf(wire.PendingResponse{}):   "CredentialPendingResponse",
	reflect.TypeOf(wire.PendingEntry{}):      "CredentialPendingRow",
	reflect.TypeOf(wire.RecordSessionResp{}): "CredentialSession",

	// The delivery package's views, whose `View` suffix is a Go naming of the read and no part
	// of what a client imports, and the measures its pages carry, whose one-word names
	// (Status, Timing, Cell, Day) belong to delivery alone.
	reflect.TypeOf(delivery.DeliveryComponentView{}):     "DeliveryComponent",
	reflect.TypeOf(measures.DailyPoint{}):                "DeliveryDailyPoint",
	reflect.TypeOf(measures.DeployFlagCounts{}):          "DeliveryDeployFlagCounts",
	reflect.TypeOf(measures.DeployJobTiming{}):           "DeliveryDeployJobTiming",
	reflect.TypeOf(measures.FailingJob{}):                "DeliveryFailingJob",
	reflect.TypeOf(measures.FlagCounts{}):                "DeliveryFlagCounts",
	reflect.TypeOf(measures.JobTiming{}):                 "DeliveryJobTiming",
	reflect.TypeOf(measures.Status{}):                    "DeliveryKpiStatus",
	reflect.TypeOf(measures.LeadTimeCell{}):              "DeliveryLeadTimeCell",
	reflect.TypeOf(measures.LeadTimeDay{}):               "DeliveryLeadTimeDay",
	reflect.TypeOf(measures.LeadTimeTiming{}):            "DeliveryLeadTimeTiming",
	reflect.TypeOf(delivery.DeliveryPRView{}):            "DeliveryPR",
	reflect.TypeOf(delivery.DeliveryRunView{}):           "DeliveryRun",
	reflect.TypeOf(delivery.DeliveryRunDetailView{}):     "DeliveryRunDetail",
	reflect.TypeOf(delivery.DeliveryRunJobView{}):        "DeliveryRunJob",
	reflect.TypeOf(delivery.DeliveryRunJobDetailView{}):  "DeliveryRunJobDetail",
	reflect.TypeOf(delivery.DeliveryRunProductionView{}): "DeliveryRunProduction",
	reflect.TypeOf(measures.Result{}):                    "DeliveryMeasures",
	reflect.TypeOf(delivery.SessionGroup{}):              "DeliverySessionGroup",
	reflect.TypeOf(measures.Spread{}):                    "DeliverySpread",
	reflect.TypeOf(measures.Targets{}):                   "DeliveryTargets",
	reflect.TypeOf(measures.Timing{}):                    "DeliveryTiming",
	reflect.TypeOf(measures.Cell{}):                      "DeliveryTimingCell",
	reflect.TypeOf(measures.Day{}):                       "DeliveryTimingDay",
	reflect.TypeOf(delivery.UnownedIssue{}):              "DeliveryUnownedIssue",
	reflect.TypeOf(delivery.DeliveryWaitingPRView{}):     "DeliveryWaitingPR",
	reflect.TypeOf(itemblocks.Carried{}):                 "CarriedBlock",
}

// rawFieldKeyOf names one field of one Go type: the owner by its reflect.Type rather than by an
// unqualified name, so two packages cannot collide and a renamed type fails to compile here
// rather than at the emit. schemaNames in this file is keyed the same way.
type rawFieldKeyOf struct {
	Owner reflect.Type
	Field string
}

// rawField is what one json.RawMessage field's bytes carry, for rawFieldSchemas. GoType names the
// Go type the bytes hold, which is reflected like any other, so a rename fails to compile rather
// than leaving a reference to a schema nothing declares; Fragment writes the schema out where no
// Go type names it (a login, a timestamp, a bounded integer).
type rawField struct {
	// GoType is a zero value of the Go type the bytes hold; nil where Fragment describes them.
	GoType any
	// Fragment is the schema the bytes hold, where no Go type names it; nil where GoType does.
	Fragment map[string]any
	// Nullable is a field whose bytes may be the JSON literal null as well as a value.
	Nullable bool
}

// rawFieldSchemas names the Go type a json.RawMessage field carries, for the fields whose three
// states (absent, null, a value) are why the handler holds the bytes unparsed.
//
// A field named here is left out of its object's `required` by its json tag alone: absent is one
// of the three states the handler reads, so a document that required every such field would deny
// a write the server takes. The two whose handler does refuse an absent field say so with
// `required:"true"`, the tag jsonschema-go reads, as every other request field does. An entry
// whose key names no field of any emitted type fails the emit, so the table cannot rot.
var rawFieldSchemas = map[rawFieldKeyOf]rawField{
	// parseAnswerPrecondition (ask_transitions.go): absent is no precondition, null the
	// precondition that the value is unset, a string the value it must hold.
	{reflect.TypeOf(answerAskInput{}), "expected_answer_at"}: rawField{Fragment: openAPITimestamp, Nullable: true},
	{reflect.TypeOf(answerAskInput{}), "expected_edited_at"}: rawField{Fragment: openAPITimestamp, Nullable: true},
	// parseIssueAssignee (issue_assignee.go) reads create and update alike: null clears the
	// assignee, a string names a person who has signed in (400 ASSIGNEE_NOT_ALLOWED otherwise).
	{reflect.TypeOf(createIssueInput{}), "assignee"}: rawField{Fragment: openAPIPersonEmail, Nullable: true},
	{reflect.TypeOf(updateIssueInput{}), "assignee"}: rawField{Fragment: openAPIPersonEmail, Nullable: true},
	// parseIssueComponents (issue_components.go) reads create and update alike: null is back to
	// inheriting, an object the mode and what it needs.
	{reflect.TypeOf(createIssueInput{}), "components"}: rawField{GoType: issueComponentsInput{}, Nullable: true},
	{reflect.TypeOf(updateIssueInput{}), "components"}: rawField{GoType: issueComponentsInput{}, Nullable: true},
	// parseIssueParent (issue_parent.go): null is no parent, a string the parent's issue key.
	{reflect.TypeOf(updateIssueInput{}), "parent"}: rawField{Fragment: openAPIIssueKey, Nullable: true},
	// parseIssuePriority (issue_create.go), read by create and update alike.
	{reflect.TypeOf(createIssueInput{}), "priority"}: rawField{Fragment: openAPIIssuePriority, Nullable: true},
	{reflect.TypeOf(updateIssueInput{}), "priority"}: rawField{Fragment: openAPIIssuePriority, Nullable: true},
	// parseIssueRepo (issue_repo.go): null clears the repository, a string is "owner/name".
	{reflect.TypeOf(createIssueInput{}), "repo"}: rawField{Fragment: openAPIRepository, Nullable: true},
	{reflect.TypeOf(updateIssueInput{}), "repo"}: rawField{Fragment: openAPIRepository, Nullable: true},
	// putProjectDefaultAssignee (project_default_assignee.go): null clears the default, a string
	// names a person who has signed in. Absent is refused (400 INVALID_PROJECT), so a PUT of `{}`
	// cannot wipe a default, which is why the handler holds the bytes rather than a *string.
	{reflect.TypeOf(projectDefaultAssigneeInput{}), "assignee"}: rawField{Fragment: openAPIPersonEmail, Nullable: true},
	// An upload refuses `primary` whenever it carries one, JSON or multipart (artifacts.go):
	// primary is fixed at issue creation. The field is described so a client reading the document
	// knows what the bytes would have to be, never because one may send them.
	{reflect.TypeOf(jsonArtifactUpload{}), "primary"}: rawField{Fragment: map[string]any{
		"type": "boolean",
		"description": "refused whenever present, JSON or multipart (400 ARTIFACT_INPUT): " +
			"an artifact is made the issue's spec at issue creation and never by an upload",
	}},
	// createBroadcast refuses `idempotency_key` whenever the body carries one (broadcasts.go):
	// the key is the Idempotency-Key header. The field is described so a client reading the
	// document knows what the bytes would have to be, never because one may send them.
	{reflect.TypeOf(createBroadcastInput{}), "idempotency_key"}: rawField{Fragment: map[string]any{
		"type": "string",
		"description": "refused whenever present (400 INVALID_JSON): a broadcast's idempotency " +
			"key is the Idempotency-Key header and never a body member",
	}},
}

// The schemas rawFieldSchemas writes out, where no Go type names what a field's bytes hold.
var (
	openAPITimestamp = map[string]any{
		"type": "string", "format": "date-time",
		"description": "an RFC3339 timestamp",
	}
	openAPIPersonEmail = map[string]any{
		"type": "string", "format": "email",
		"description": "the lowercase email of a person who has signed in to Dispatch",
	}
	openAPIIssueKey = map[string]any{
		"type":        "string",
		"description": "an issue key, such as CORE-123",
	}
	openAPIIssuePriority = map[string]any{
		"type": "integer", "minimum": 0, "maximum": 3,
		"description": "an issue priority, P0 to P3",
	}
	openAPIRepository = map[string]any{
		"type":        "string",
		"description": `a GitHub repository as "owner/name"`,
	}
)

// openAPIMultipartSchema is the multipart/form-data body the three upload routes take beside
// their JSON one: the fields multipartArtifactUpload reads (artifacts.go, around the
// content-type switch). `primary` and `content` are refused in a multipart body, so neither is
// here; the file is the one part an upload cannot leave out.
var openAPIMultipartSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"file": map[string]any{
			"type": "string", "format": "binary",
			"description": "the uploaded bytes; its kind is read from them, whatever the part declares",
		},
		"name": map[string]any{
			"type":        "string",
			"description": "the artifact's name, trimmed",
		},
		"summary": map[string]any{
			"type":        "string",
			"description": "what this version changed, trimmed",
		},
		"actor": map[string]any{
			"type":        "string",
			"description": "the acting session as one JSON object, as the JSON body's `actor` field",
		},
	},
	"required": []any{"file"},
}
