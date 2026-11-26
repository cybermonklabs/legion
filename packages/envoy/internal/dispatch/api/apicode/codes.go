// Package apicode is the one declared list of the codes Dispatch's API refuses a request with.
//
// A Code is a struct with unexported fields, so no package but this one makes a code, and a string
// literal does not convert to one: the API's error writers take a Code, so a refusal whose code is
// not on this list does not build. Each code carries the name and status it answers under /api/v2
// and the name and status it answers under /api/v1, which is the literal and the status that site
// wrote before this list existed (TestV1CodesAreTheCodesOfC8a44f97 holds the v1 pairs to the
// codes of c8a44f97). Most codes answer alike in both versions; the ones that do not are in their
// own blocks below, each saying why.
package apicode

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/sjawhar/envoy/internal/contracts"
)

// Code is one refusal code. Its zero value is no code at all: the API's writers answer it 500
// INTERNAL and log where it was written.
type Code struct {
	// name and status are what /api/v2 answers; name is empty for a code /api/v2 never answers.
	name   string
	status int
	// v1Name and v1Status are what /api/v1 answers; v1Name is empty for a code /api/v1 never
	// answers.
	v1Name   string
	v1Status int
	// fromCall marks the one code whose status is the call's rather than the code's
	// (AnswerNotKept): its statuses are 0, and only writeErrorAt writes it.
	fromCall bool
}

// Name is the code /api/v2 answers, and "" for a code /api/v2 never answers.
func (c Code) Name() string { return c.name }

// Status is the status /api/v2 answers the code with, and 0 for a code /api/v2 never answers or
// one whose status is the call's.
func (c Code) Status() int { return c.status }

// V1Name is the code /api/v1 answers, and "" for a code /api/v1 never answers.
func (c Code) V1Name() string { return c.v1Name }

// V1Status is the status /api/v1 answers the code with, and 0 for a code /api/v1 never answers or
// one whose status is the call's.
func (c Code) V1Status() int { return c.v1Status }

// InV1 reports whether /api/v1 answers the code.
func (c Code) InV1() bool { return c.v1Name != "" }

// InV2 reports whether /api/v2 answers the code.
func (c Code) InV2() bool { return c.name != "" }

// StatusFromCall reports whether the code takes its status from the call that writes it.
func (c Code) StatusFromCall() bool { return c.fromCall }

// IsZero reports whether c is the zero Code, which names no code.
func (c Code) IsZero() bool { return c == Code{} }

// String names the code by both of its pairs, for a test's or a log's message.
func (c Code) String() string {
	if c.IsZero() {
		return "no code"
	}
	pair := func(name string, status int) string {
		switch {
		case name == "":
			return "none"
		case c.fromCall:
			return name + " *"
		}
		return fmt.Sprintf("%s %d", name, status)
	}
	return fmt.Sprintf("v2 %s, v1 %s", pair(c.name, c.status), pair(c.v1Name, c.v1Status))
}

// all is every code this file declares, in declaration order. Each constructor below appends the
// code it makes, so a code cannot be declared without being listed.
var all []Code

// All is every code the API can answer, in declaration order.
func All() []Code { return slices.Clone(all) }

func declare(c Code) Code {
	all = append(all, c)
	return c
}

// both is a code both versions answer with one name and one status.
func both(name string, status int) Code {
	return declare(Code{name: name, status: status, v1Name: name, v1Status: status})
}

// renamed is a code /api/v2 answers under another name than /api/v1 does, at one status.
func renamed(v1Name string, status int, name string) Code {
	return declare(Code{name: name, status: status, v1Name: v1Name, v1Status: status})
}

// v2Only is a code only /api/v2 answers.
func v2Only(name string, status int) Code {
	return declare(Code{name: name, status: status})
}

// v1Only is a code only /api/v1 answers.
func v1Only(name string, status int) Code {
	return declare(Code{v1Name: name, v1Status: status})
}

// statusFromCall is a code both versions answer by one name, with the status its call names.
func statusFromCall(name string) Code {
	return declare(Code{name: name, v1Name: name, fromCall: true})
}

// The codes both versions answer alike: the name and the status each /api/v1 site wrote.
var (
	AcceptAlreadyAccepted      = both("ACCEPT_ALREADY_ACCEPTED", http.StatusConflict)
	AcceptFailed               = both("ACCEPT_FAILED", http.StatusConflict)
	AcceptForbidden            = both("ACCEPT_FORBIDDEN", http.StatusForbidden)
	AcceptNotAsideOrSteer      = both("ACCEPT_NOT_ASIDE_OR_STEER", http.StatusConflict)
	AcceptNotDirect            = both("ACCEPT_NOT_DIRECT", http.StatusConflict)
	AcceptNotRequestedByAuthor = both("ACCEPT_NOT_REQUESTED_BY_AUTHOR", http.StatusConflict)
	AcceptNotRequestedByPerson = both("ACCEPT_NOT_REQUESTED_BY_PERSON", http.StatusConflict)
	AcceptNotWrittenByPerson   = both("ACCEPT_NOT_WRITTEN_BY_PERSON", http.StatusConflict)
	AcceptStale                = both("ACCEPT_STALE", http.StatusConflict)
	AcceptSuperseded           = both("ACCEPT_SUPERSEDED", http.StatusConflict)
	AcceptWrittenByAgent       = both("ACCEPT_WRITTEN_BY_AGENT", http.StatusConflict)
	ActorKind                  = both("ACTOR_KIND", http.StatusBadRequest)
	ActorSessionMismatch       = both("ACTOR_SESSION_MISMATCH", http.StatusForbidden)
	// AdvancesInput refuses an `advances` input no relation can be written from, as
	// BlockedByInput refuses a blocker list beside it: more ids than the cap holds, an id no
	// priority row names, a priority whose state is gone, and a relation whose issue does not
	// exist. Each of the four is a 400 naming the key it refuses.
	AdvancesInput = both("ADVANCES_INPUT", http.StatusBadRequest)
	// AdvancesRevision refuses an `advances` write the revision rule does not let through: the
	// one refusal of that field that is not a 400.
	AdvancesRevision                   = both("ADVANCES_REVISION", http.StatusConflict)
	AgentSecretsAccountTokenUnreadable = both("AGENT_SECRETS_ACCOUNT_TOKEN_UNREADABLE", http.StatusInternalServerError)
	AgentSecretsUnavailable            = both("AGENT_SECRETS_UNAVAILABLE", http.StatusServiceUnavailable)
	AgentTokenInput                    = both("AGENT_TOKEN_INPUT", http.StatusBadRequest)
	AlreadyActioned                    = both("ALREADY_ACTIONED", http.StatusConflict)
	AmbiguousIssueRef                  = both("AMBIGUOUS_ISSUE_REF", http.StatusBadRequest)
	AnchorMissing                      = both("ANCHOR_MISSING", http.StatusConflict)
	AnchorOrphaned                     = both("ANCHOR_ORPHANED", http.StatusConflict)
	ApprovalWaitsOnHuman               = both("APPROVAL_WAITS_ON_HUMAN", http.StatusConflict)
	ApproverMeOnly                     = both("APPROVER_ME_ONLY", http.StatusBadRequest)
	ArtifactAgentOwned                 = both("ARTIFACT_AGENT_OWNED", http.StatusBadRequest)
	ArtifactInput                      = both("ARTIFACT_INPUT", http.StatusBadRequest)
	ArtifactKindMismatch               = both("ARTIFACT_KIND_MISMATCH", http.StatusBadRequest)
	ArtifactLinked                     = both("ARTIFACT_LINKED", http.StatusBadRequest)
	ArtifactNameTaken                  = both("ARTIFACT_NAME_TAKEN", http.StatusConflict)
	ArtifactNotFound                   = both("ARTIFACT_NOT_FOUND", http.StatusNotFound)
	ArtifactNotUntouched               = both("ARTIFACT_NOT_UNTOUCHED", http.StatusConflict)
	ArtifactPrimary                    = both("ARTIFACT_PRIMARY", http.StatusBadRequest)
	ArtifactProjectDocument            = both("ARTIFACT_PROJECT_DOCUMENT", http.StatusBadRequest)
	ArtifactReferenceAmbiguous         = both("ARTIFACT_REFERENCE_AMBIGUOUS", http.StatusBadRequest)
	ArtifactRoleName                   = both("ARTIFACT_ROLE_NAME", http.StatusBadRequest)
	AskAnswerChanged                   = both("ASK_ANSWER_CHANGED", http.StatusConflict)
	AskAnswered                        = both("ASK_ANSWERED", http.StatusConflict)
	AskApprovalReview                  = both("ASK_APPROVAL_REVIEW", http.StatusConflict)
	AskBlockText                       = both("ASK_BLOCK_TEXT", http.StatusBadRequest)
	AskEdited                          = both("ASK_EDITED", http.StatusConflict)
	AskIDInput                         = both("ASK_ID_INPUT", http.StatusBadRequest)
	AskKindFixed                       = both("ASK_KIND_FIXED", http.StatusConflict)
	AskKindInput                       = both("ASK_KIND_INPUT", http.StatusBadRequest)
	AskNotDeclinable                   = both("ASK_NOT_DECLINABLE", http.StatusConflict)
	AskNotOpen                         = both("ASK_NOT_OPEN", http.StatusConflict)
	AskResolved                        = both("ASK_RESOLVED", http.StatusConflict)
	AskRevisionRequired                = both("ASK_REVISION_REQUIRED", http.StatusBadRequest)
	AskStateInvalid                    = both("ASK_STATE_INVALID", http.StatusInternalServerError)
	AssigneeNotAllowed                 = both("ASSIGNEE_NOT_ALLOWED", http.StatusBadRequest)
	AuthorSessionOrProjectConflict     = both("AUTHOR_SESSION_OR_PROJECT_CONFLICT", http.StatusBadRequest)
	AuthorSessionOrProjectRequired     = both("AUTHOR_SESSION_OR_PROJECT_REQUIRED", http.StatusBadRequest)
	BlockedByInput                     = both("BLOCKED_BY_INPUT", http.StatusBadRequest)
	BroadcastEmpty                     = both("BROADCAST_EMPTY", http.StatusBadRequest)
	BroadcastInput                     = both("BROADCAST_INPUT", http.StatusBadRequest)
	BroadcastNotFound                  = both("BROADCAST_NOT_FOUND", http.StatusNotFound)
	BrokerTokenKeyMismatch             = both("BROKER_TOKEN_KEY_MISMATCH", http.StatusUnauthorized)
	BrokerTokenNotActive               = both("BROKER_TOKEN_NOT_ACTIVE", http.StatusUnauthorized)
	BrokerTokenNotASession             = both("BROKER_TOKEN_NOT_A_SESSION", http.StatusUnauthorized)
	BrokerTokenProofInvalid            = both("BROKER_TOKEN_PROOF_INVALID", http.StatusUnauthorized)
	BrokerTokenRateLimited             = both("BROKER_TOKEN_RATE_LIMITED", http.StatusTooManyRequests)
	BrokerUnavailable                  = both("BROKER_UNAVAILABLE", http.StatusServiceUnavailable)
	CheckAmbiguous                     = both("CHECK_AMBIGUOUS", http.StatusConflict)
	CheckContended                     = both("CHECK_CONTENDED", http.StatusConflict)
	CheckProvedByCI                    = both("CHECK_PROVED_BY_CI", http.StatusConflict)
	ClaimContended                     = both("CLAIM_CONTENDED", http.StatusConflict)
	CommentIDInput                     = both("COMMENT_ID_INPUT", http.StatusBadRequest)
	CommentNotFound                    = both("COMMENT_NOT_FOUND", http.StatusNotFound)
	ComponentsInput                    = both("COMPONENTS_INPUT", http.StatusBadRequest)
	DatabaseUnavailable                = both("DATABASE_UNAVAILABLE", http.StatusServiceUnavailable)
	DecisionBlockInBody                = both("DECISION_BLOCK_IN_BODY", http.StatusBadRequest)
	DecisionChanged                    = both("DECISION_CHANGED", http.StatusConflict)
	DeleteContended                    = both("DELETE_CONTENDED", http.StatusConflict)
	DeliveryNotConfigured              = both("DELIVERY_NOT_CONFIGURED", http.StatusNotFound)
	DeliverySettingsAccess             = both("DELIVERY_SETTINGS_ACCESS", http.StatusConflict)
	DeliverySettingsInput              = both("DELIVERY_SETTINGS_INPUT", http.StatusBadRequest)
	DocNewerContent                    = both("DOC_NEWER_CONTENT", http.StatusConflict)
	DocServiceUnavailable              = both("DOC_SERVICE_UNAVAILABLE", http.StatusServiceUnavailable)
	DocumentLive                       = both("DOCUMENT_LIVE", http.StatusConflict)
	DocumentLoads                      = both("DOCUMENT_LOADS", http.StatusConflict)
	DocumentUnloadable                 = both("DOCUMENT_UNLOADABLE", http.StatusConflict)
	DocumentWriterNotArchitect         = both("DOCUMENT_WRITER_NOT_ARCHITECT", http.StatusForbidden)
	DurationInput                      = both("DURATION_INPUT", http.StatusBadRequest)
	EditLostToConcurrentChange         = both("EDIT_LOST_TO_CONCURRENT_CHANGE", http.StatusConflict)
	EditQueueFull                      = both("EDIT_QUEUE_FULL", http.StatusTooManyRequests)
	EnvoyRefilling                     = both("ENVOY_REFILLING", http.StatusServiceUnavailable)
	EnvoyUnavailable                   = both("ENVOY_UNAVAILABLE", http.StatusServiceUnavailable)
	EnvoyUnsubscribeFailed             = both("ENVOY_UNSUBSCRIBE_FAILED", http.StatusBadGateway)
	ExcludeIssueInput                  = both("EXCLUDE_ISSUE_INPUT", http.StatusBadRequest)
	ExternalLinkTaken                  = both("EXTERNAL_LINK_TAKEN", http.StatusConflict)
	ExternalProjectMismatch            = both("EXTERNAL_PROJECT_MISMATCH", http.StatusBadRequest)
	FeatureOff                         = both("FEATURE_OFF", http.StatusNotFound)
	FileMissing                        = both("FILE_MISSING", http.StatusInternalServerError)
	FollowerForbidden                  = both("FOLLOWER_FORBIDDEN", http.StatusForbidden)
	FollowerInput                      = both("FOLLOWER_INPUT", http.StatusBadRequest)
	FollowerNotFound                   = both("FOLLOWER_NOT_FOUND", http.StatusNotFound)
	ForeignChildOpen                   = both("FOREIGN_CHILD_OPEN", http.StatusConflict)
	FrameInput                         = both("FRAME_INPUT", http.StatusBadRequest)
	GitHubAppUnconfigured              = both("GITHUB_APP_UNCONFIGURED", http.StatusServiceUnavailable)
	GitHubUnreadable                   = both("GITHUB_UNREADABLE", http.StatusBadGateway)
	// GoingAskerNamesNobody refuses the POST of a question by a credential that names nobody:
	// the deployment's shared token, which resolves as {}. The stream serves a run to the
	// credential that asked it and to no other, and comparing identities cannot tell two holders
	// of one shared token apart, so such a run would be readable by nobody - the model called,
	// the deployment's paced quota spent, and every reader of it answered 404. It is refused
	// where that is cheap and plain, before anything is recorded.
	GoingAskerNamesNobody = both("GOING_ASKER_NAMES_NOBODY", http.StatusForbidden)
	GoingInput            = both("GOING_INPUT", http.StatusBadRequest)
	// GoingRequestNotFound is the one answer an answer run's stream gives for every run a caller
	// may not read: a run the record does not hold, an id that is not a uuid, and a run that is
	// somebody else's. One answer for all three is what keeps an id from telling a caller
	// anything it did not already know - whether that run exists, and whether it is anyone's.
	GoingRequestNotFound = both("GOING_REQUEST_NOT_FOUND", http.StatusNotFound)
	HandoffForbidden     = both("HANDOFF_FORBIDDEN", http.StatusForbidden)
	HandoffIDInput       = both("HANDOFF_ID_INPUT", http.StatusBadRequest)
	HandoffInput         = both("HANDOFF_INPUT", http.StatusBadRequest)
	HandoffNotFound      = both("HANDOFF_NOT_FOUND", http.StatusNotFound)
	// InternalError is INTERNAL. It is not named after the code alone: the leak guard's
	// private-host rule (packages/daemon/internal/leakguard/private-names.toml, private_host_rule)
	// reads a selector of that name followed by a method call as a host under the internal
	// domain, and refuses the push.
	InternalError        = both("INTERNAL", http.StatusInternalServerError)
	InvalidAdvances      = both("INVALID_ADVANCES", http.StatusBadRequest)
	InvalidAnchor        = both("INVALID_ANCHOR", http.StatusBadRequest)
	InvalidAnswer        = both("INVALID_ANSWER", http.StatusBadRequest)
	InvalidArtifact      = both("INVALID_ARTIFACT", http.StatusBadRequest)
	InvalidAsk           = both("INVALID_ASK", http.StatusBadRequest)
	InvalidAskBlock      = both("INVALID_ASK_BLOCK", http.StatusBadRequest)
	InvalidAskState      = both("INVALID_ASK_STATE", http.StatusBadRequest)
	InvalidAskThread     = both("INVALID_ASK_THREAD", http.StatusBadRequest)
	InvalidAuthor        = both("INVALID_AUTHOR", http.StatusBadRequest)
	InvalidBody          = both("INVALID_BODY", http.StatusBadRequest)
	InvalidCICheck       = both("INVALID_CI_CHECK", http.StatusBadRequest)
	InvalidClosedBefore  = both("INVALID_CLOSED_BEFORE", http.StatusBadRequest)
	InvalidClosedSince   = both("INVALID_CLOSED_SINCE", http.StatusBadRequest)
	InvalidClosedWindow  = both("INVALID_CLOSED_WINDOW", http.StatusBadRequest)
	InvalidComment       = both("INVALID_COMMENT", http.StatusBadRequest)
	InvalidDecision      = both("INVALID_DECISION", http.StatusBadRequest)
	InvalidIssue         = both("INVALID_ISSUE", http.StatusBadRequest)
	InvalidIssueRef      = both("INVALID_ISSUE_REF", http.StatusBadRequest)
	InvalidJSON          = both("INVALID_JSON", http.StatusBadRequest)
	InvalidKeep          = both("INVALID_KEEP", http.StatusBadRequest)
	InvalidKeywordsOnly  = both("INVALID_KEYWORDS_ONLY", http.StatusBadRequest)
	InvalidKind          = both("INVALID_KIND", http.StatusBadRequest)
	InvalidLimit         = both("INVALID_LIMIT", http.StatusBadRequest)
	InvalidMarkdown      = both("INVALID_MARKDOWN", http.StatusBadRequest)
	InvalidMessage       = both("INVALID_MESSAGE", http.StatusBadRequest)
	InvalidNumberProject = both("INVALID_NUMBER_PROJECT", http.StatusBadRequest)
	InvalidOffset        = both("INVALID_OFFSET", http.StatusBadRequest)
	InvalidOp            = both("INVALID_OP", http.StatusBadRequest)
	InvalidPrecondition  = both("INVALID_PRECONDITION", http.StatusBadRequest)
	InvalidPriority      = both("INVALID_PRIORITY", http.StatusBadRequest)
	InvalidProject       = both("INVALID_PROJECT", http.StatusBadRequest)
	InvalidQuery         = both("INVALID_QUERY", http.StatusBadRequest)
	InvalidReference     = both("INVALID_REFERENCE", http.StatusBadRequest)
	InvalidRepository    = both("INVALID_REPOSITORY", http.StatusBadRequest)
	InvalidResolution    = both("INVALID_RESOLUTION", http.StatusBadRequest)
	InvalidReview        = both("INVALID_REVIEW", http.StatusBadRequest)
	InvalidRoleHolder    = both("INVALID_ROLE_HOLDER", http.StatusBadRequest)
	InvalidRouteStatus   = both("INVALID_ROUTE_STATUS", http.StatusBadRequest)
	InvalidSessionID     = both("INVALID_SESSION_ID", http.StatusBadRequest)
	InvalidSince         = both("INVALID_SINCE", http.StatusBadRequest)
	InvalidState         = both("INVALID_STATE", http.StatusBadRequest)
	InvalidStatus        = both("INVALID_STATUS", http.StatusBadRequest)
	InvalidSuggestion    = both("INVALID_SUGGESTION", http.StatusBadRequest)
	InvalidUpdatedSince  = both("INVALID_UPDATED_SINCE", http.StatusBadRequest)
	InvalidURL           = both("INVALID_URL", http.StatusBadRequest)
	InvalidUTF8          = both("INVALID_UTF8", http.StatusBadRequest)
	InvalidVersion       = both("INVALID_VERSION", http.StatusBadRequest)
	IssueChanged         = both("ISSUE_CHANGED", http.StatusConflict)
	IssueClaimed         = both("ISSUE_CLAIMED", http.StatusConflict)
	IssueClosed          = both("ISSUE_CLOSED", http.StatusConflict)
	IssueNotFound        = both("ISSUE_NOT_FOUND", http.StatusNotFound)
	JSONContentType      = both("JSON_CONTENT_TYPE", http.StatusUnsupportedMediaType)
	LabelsInput          = both("LABELS_INPUT", http.StatusBadRequest)
	LegionNotConfigured  = both("LEGION_NOT_CONFIGURED", http.StatusNotFound)
	LegionSettingInput   = both("LEGION_SETTING_INPUT", http.StatusBadRequest)
	// LegionStatusScopeRequired refuses one of R26's three writes of a Legion issue (#4601) to a
	// credential that holds neither the person scope nor legion-status; its challenge names
	// legion-status.
	LegionStatusScopeRequired = both("LEGION_STATUS_SCOPE_REQUIRED", http.StatusForbidden)
	MentionInput              = both("MENTION_INPUT", http.StatusBadRequest)
	MessageIDInput            = both("MESSAGE_ID_INPUT", http.StatusBadRequest)
	MessageInput              = both("MESSAGE_INPUT", http.StatusBadRequest)
	MessageNotFound           = both("MESSAGE_NOT_FOUND", http.StatusNotFound)
	MessageTargetChanged      = both("MESSAGE_TARGET_CHANGED", http.StatusConflict)
	NearCopyCheckIDInput      = both("NEAR_COPY_CHECK_ID_INPUT", http.StatusBadRequest)
	NearCopyCheckNotFound     = both("NEAR_COPY_CHECK_NOT_FOUND", http.StatusNotFound)
	NotAnIssueMessage         = both("NOT_AN_ISSUE_MESSAGE", http.StatusConflict)
	NotAnswerer               = both("NOT_ANSWERER", http.StatusForbidden)
	NotAuthor                 = both("NOT_AUTHOR", http.StatusForbidden)
	NotDocument               = both("NOT_DOCUMENT", http.StatusBadRequest)
	NotFound                  = both("NOT_FOUND", http.StatusNotFound)
	NotPlanDocument           = both("NOT_PLAN_DOCUMENT", http.StatusConflict)
	NotRemovable              = both("NOT_REMOVABLE", http.StatusConflict)
	NotSpecDocument           = both("NOT_SPEC_DOCUMENT", http.StatusConflict)
	NoVerificationDocument    = both("NO_VERIFICATION_DOCUMENT", http.StatusConflict)
	NoVersion                 = both("NO_VERSION", http.StatusConflict)
	NULCharacter              = both("NUL_CHARACTER", http.StatusBadRequest)
	OAuthScopeRequired        = both("OAUTH_SCOPE_REQUIRED", http.StatusForbidden)
	OIDCIssuerUnavailable     = both("OIDC_ISSUER_UNAVAILABLE", http.StatusServiceUnavailable)
	OIDCTokenInvalid          = both("OIDC_TOKEN_INVALID", http.StatusUnauthorized)
	OwnerInvalid              = both("OWNER_INVALID", http.StatusBadRequest)
	// ParentContended is a PATCH whose issue's parent a concurrent reparent moved at every lock
	// attempt. The Legion daemon retries a status write it refuses rather than dropping it
	// (packages/daemon/internal/dispatch/types.go, ParentContended, which PermanentRefusal
	// reads), so the two spellings change together: no mechanism shares a value between the two
	// Go modules.
	ParentContended       = both("PARENT_CONTENDED", http.StatusConflict)
	ParentStatusConflict  = both("PARENT_STATUS_CONFLICT", http.StatusConflict)
	StatusChangeInput     = both("STATUS_CHANGE_INPUT", http.StatusBadRequest)
	BrowserSignInRequired = both("BROWSER_SIGN_IN_REQUIRED", http.StatusForbidden)
	PauseArmed            = both("PAUSE_ARMED", http.StatusConflict)
	PauseNotArmed         = both("PAUSE_NOT_ARMED", http.StatusNotFound)
	PausePointInput       = both("PAUSE_POINT_INPUT", http.StatusBadRequest)
	PlanApprovedByReview  = both("PLAN_APPROVED_BY_REVIEW", http.StatusConflict)
	PossibleDuplicate     = both("POSSIBLE_DUPLICATE", http.StatusConflict)
	PostDeleted           = both("POST_DELETED", http.StatusConflict)
	PreconditionFailed    = both("PRECONDITION_FAILED", http.StatusConflict)
	// PriorityChanged refuses a patch whose expected title or description is not the one the
	// priority stores now: somebody else wrote it after the writer's edit began, and the patch
	// would write over that change unseen (the api package's priorityChangedRefusal).
	PriorityChanged = both("PRIORITY_CHANGED", http.StatusConflict)
	// PriorityExists refuses a create of an id the store already holds, as a project create
	// refuses a key it holds (ProjectExists): a second create would write its own history over
	// the first write's.
	PriorityExists = both("PRIORITY_EXISTS", http.StatusConflict)
	// PriorityInput refuses a body no priority can be written from: a title with nothing in it
	// or one longer than an issue title may be, an id the caller names that is no priority id, a
	// patch that names nothing to write, and a patch whose expected names an attribute the same
	// patch does not write.
	PriorityInput = both("PRIORITY_INPUT", http.StatusBadRequest)
	// PriorityNotFound answers a route addressed to an id no priority row carries: a write of
	// it, and the read of its history.
	PriorityNotFound = both("PRIORITY_NOT_FOUND", http.StatusNotFound)
	ProjectExists    = both("PROJECT_EXISTS", http.StatusConflict)
	ProjectNotFound  = both("PROJECT_NOT_FOUND", http.StatusNotFound)
	// ProjectScope refuses a write the caller's credential does not cover: an object of another
	// project, or one the scope check could not resolve to the caller's own.
	ProjectScope = both("PROJECT_SCOPE", http.StatusForbidden)
	// ProjectScopeDeployment refuses a project-scoped caller a write that belongs to the
	// deployment rather than to any one project.
	ProjectScopeDeployment = both("PROJECT_SCOPE_DEPLOYMENT", http.StatusForbidden)
	// QueuedOnBus is the queued-messages test hook's read on a process that publishes a queued
	// message to NATS, where the stream holds it and no recorder of the process does (#4088).
	QueuedOnBus = both("QUEUED_ON_BUS", http.StatusNotFound)
	// RankInput refuses a move to a place the order does not have, which is the code and the
	// wording the issue board's own drag is refused with (the api package's issue_rank.go).
	RankInput                      = both("RANK_INPUT", http.StatusBadRequest)
	ReasonRequired                 = both("REASON_REQUIRED", http.StatusBadRequest)
	ReferencesTimeout              = both("REFERENCES_TIMEOUT", http.StatusServiceUnavailable)
	ReplaceFields                  = both("REPLACE_FIELDS", http.StatusBadRequest)
	ReplyForbidden                 = both("REPLY_FORBIDDEN", http.StatusForbidden)
	RequestBodyUnreadable          = both("REQUEST_BODY_UNREADABLE", http.StatusBadRequest)
	RequestTooLarge                = both("REQUEST_TOO_LARGE", http.StatusRequestEntityTooLarge)
	ResultCommitUnknown            = both("RESULT_COMMIT_UNKNOWN", http.StatusBadRequest)
	ResultContradicts              = both("RESULT_CONTRADICTS", http.StatusBadRequest)
	ResultIDInput                  = both("RESULT_ID_INPUT", http.StatusBadRequest)
	ResultInput                    = both("RESULT_INPUT", http.StatusBadRequest)
	ResultNotFound                 = both("RESULT_NOT_FOUND", http.StatusNotFound)
	ResultRetracted                = both("RESULT_RETRACTED", http.StatusConflict)
	RetractionForbidden            = both("RETRACTION_FORBIDDEN", http.StatusForbidden)
	RetractionInput                = both("RETRACTION_INPUT", http.StatusBadRequest)
	ReviewVersionMoved             = both("REVIEW_VERSION_MOVED", http.StatusConflict)
	RoleHolderForbidden            = both("ROLE_HOLDER_FORBIDDEN", http.StatusForbidden)
	RoleHolderNotProven            = both("ROLE_HOLDER_NOT_PROVEN", http.StatusForbidden)
	RouteChanged                   = both("ROUTE_CHANGED", http.StatusConflict)
	RouteInvalid                   = both("ROUTE_INVALID", http.StatusBadRequest)
	RunIDInput                     = both("RUN_ID_INPUT", http.StatusBadRequest)
	SearchCoverageTimeout          = both("SEARCH_COVERAGE_TIMEOUT", http.StatusServiceUnavailable)
	SessionGroupIDInput            = both("SESSION_GROUP_ID_INPUT", http.StatusBadRequest)
	SessionGroupInput              = both("SESSION_GROUP_INPUT", http.StatusBadRequest)
	SessionGroupNameIsSessionTitle = both("SESSION_GROUP_NAME_IS_SESSION_TITLE", http.StatusConflict)
	SessionGroupNameTaken          = both("SESSION_GROUP_NAME_TAKEN", http.StatusConflict)
	SessionIDInput                 = both("SESSION_ID_INPUT", http.StatusBadRequest)
	SessionInGroup                 = both("SESSION_IN_GROUP", http.StatusConflict)
	SessionRequired                = both("SESSION_REQUIRED", http.StatusBadRequest)
	// SessionScope refuses a caller whose credential proved one session a write addressed to
	// another session's own rows.
	SessionScope       = both("SESSION_SCOPE", http.StatusForbidden)
	ShuttingDown       = both("SHUTTING_DOWN", http.StatusServiceUnavailable)
	SlowWriteCutOff    = both("SLOW_WRITE_CUT_OFF", http.StatusServiceUnavailable)
	SnoozeInput        = both("SNOOZE_INPUT", http.StatusBadRequest)
	SourceAccess       = both("SOURCE_ACCESS", http.StatusConflict)
	SourceInput        = both("SOURCE_INPUT", http.StatusBadRequest)
	SourceNotFound     = both("SOURCE_NOT_FOUND", http.StatusNotFound)
	StateStale         = both("STATE_STALE", http.StatusConflict)
	StreamUnavailable  = both("STREAM_UNAVAILABLE", http.StatusServiceUnavailable)
	SubscriberNotFound = both("SUBSCRIBER_NOT_FOUND", http.StatusNotFound)
	SummaryInput       = both("SUMMARY_INPUT", http.StatusBadRequest)
	TableWidth         = both("TABLE_WIDTH", http.StatusBadRequest)
	TargetAmbiguous    = both("TARGET_AMBIGUOUS", http.StatusConflict)
	TargetNotFound     = both("TARGET_NOT_FOUND", http.StatusNotFound)
	TargetSpansBlocks  = both("TARGET_SPANS_BLOCKS", http.StatusBadRequest)
	TestHookFailed     = both("TEST_HOOK_FAILED", http.StatusInternalServerError)
	TestHooksOff       = both("TEST_HOOKS_OFF", http.StatusNotFound)
	ThreadForbidden    = both("THREAD_FORBIDDEN", http.StatusForbidden)
	ThreadIDInput      = both("THREAD_ID_INPUT", http.StatusBadRequest)
	TokenIDInput       = both("TOKEN_ID_INPUT", http.StatusBadRequest)
	TokenRevoked       = both("TOKEN_REVOKED", http.StatusUnauthorized)
	TokenScopesInput   = both("TOKEN_SCOPES_INPUT", http.StatusBadRequest)
	TurnRequiresAsk    = both("TURN_REQUIRES_ASK", http.StatusBadRequest)
	Unauthorized       = both("UNAUTHORIZED", http.StatusUnauthorized)
	UnknownCheck       = both("UNKNOWN_CHECK", http.StatusBadRequest)
	WaitInput          = both("WAIT_INPUT", http.StatusBadRequest)
)

// The codes the contracts package names for its clients, whose names are those constants, so a
// client's code and the server's cannot drift apart.
var (
	KeyReused      = both(contracts.IdempotencyKeyReused, http.StatusUnprocessableEntity)
	KeyInFlight    = both(contracts.IdempotencyKeyInFlight, http.StatusConflict)
	KeyCommitted   = both(contracts.IdempotencyKeyCommitted, http.StatusConflict)
	KeyInvalid     = both(contracts.IdempotencyKeyInvalid, http.StatusBadRequest)
	IDAmbiguous    = both(contracts.IDAmbiguous, http.StatusConflict)
	SignInRequired = both(contracts.DispatchSignInRequired, http.StatusForbidden)
	LoginChanged   = both(contracts.DispatchLoginChanged, http.StatusConflict)
	// ReadOnlyToken is a read-only token's call of a route outside the read class
	// (refuseReadOnlyToken).
	ReadOnlyToken = both(contracts.DispatchReadOnlyToken, http.StatusForbidden)
	// ApproveOnlyToken is an approve-only token's call of a route the approve list does not name
	// (refuseApproveOnlyToken), its send of a state of a route that takes several by state that the
	// list does not give it (gateApproveOnlyStates), or its answer of an ask that is no approval
	// request (approveOnlyAnswerRefusal, gateApproveOnlyAskAnswer).
	ApproveOnlyToken   = both(contracts.DispatchApproveOnlyToken, http.StatusForbidden)
	AsOfTokenNotServed = both(contracts.DispatchAsOfTokenNotServed, http.StatusForbidden)
	// The refusals of a write of a step record (steps.go), which the Legion daemon reads to end a
	// row on.
	StepInput      = both(contracts.StepInputCode, http.StatusBadRequest)
	StepWriter     = both(contracts.StepWriterCode, http.StatusForbidden)
	StepOtherIssue = both(contracts.StepOtherIssueCode, http.StatusConflict)
	// The refusals of a write of a plan (plans.go) beside the step write's STEP_WRITER, which the
	// Legion daemon reads to end a row on.
	PlanInput      = both(contracts.PlanInputCode, http.StatusBadRequest)
	PlanOtherIssue = both(contracts.PlanOtherIssueCode, http.StatusConflict)

	// AnswerNotKept answers a repeat of a keyed write whose first answer could not be kept, with
	// the status that first answer had (request_keys.go): its status is the call's, so only
	// writeErrorAt writes it.
	AnswerNotKept = statusFromCall(contracts.IdempotencyAnswerNotKept)
)

// The identity layer's codes, which every authenticated route answers a caller that does not
// resolve with (identity.Refusal): no person, a sign-in issuer that cannot be reached, and any
// other failure of the identity itself.
var (
	NoIdentity        = both("NO_IDENTITY", http.StatusUnauthorized)
	SigninUnavailable = both("SIGNIN_UNAVAILABLE", http.StatusServiceUnavailable)
	IdentityError     = both("IDENTITY_ERROR", http.StatusInternalServerError)
)

// The router's codes, which it writes in front of every route, /api/v2 and /api/v1 alike.
var (
	// OriginRefused is the cookie-origin guard's 403 for a write a session cookie carries from
	// another origin, which answered no code before /api/v2 (R3.2).
	OriginRefused = both("ORIGIN_REFUSED", http.StatusForbidden)
	// HostMismatch is dev sign-in mode's 421 for a request whose Host is not the dashboard's
	// origin, which answers every path.
	HostMismatch = both("HOST_MISMATCH", http.StatusMisdirectedRequest)
)

// The codes /api/v1 wrote at two statuses, or for two conditions /api/v2 tells apart: one
// constant for each, with the v1 name and status its sites wrote, and a v2 name of its own.
var (
	// CapExceeded is a field, a block or a count past its cap (capExceededError,
	// countExceededError, blockBoundError).
	CapExceeded = both("CAP_EXCEEDED", http.StatusBadRequest)
	// CapExceededTooLarge is a body, an upload or a document past its size bound, which /api/v1
	// answered with the field cap's code at 413. /api/v2 answers it as it answers a JSON body past
	// its bound (RequestTooLarge): every 413 of /api/v2 is REQUEST_TOO_LARGE.
	CapExceededTooLarge = renamed("CAP_EXCEEDED", http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE")

	// FileStoreUnavailable is a file store that failed the read or the write asked of it.
	FileStoreUnavailable = both("FILE_STORE_UNAVAILABLE", http.StatusBadGateway)
	// FileStoreUnconfigured is a file whose bytes are in a store this server is not configured to
	// read, which /api/v1 answered under the failing store's code at 503.
	FileStoreUnconfigured = renamed("FILE_STORE_UNAVAILABLE", http.StatusServiceUnavailable, "FILE_STORE_UNCONFIGURED")

	// DocSchema is a document that holds a tree outside the schema.
	DocSchema = both("DOC_SCHEMA", http.StatusConflict)
	// DocSchemaInternal is a tree outside the schema that an operation produced, which is the
	// server's fault: /api/v1 answered it DOC_SCHEMA at 500, and /api/v2 answers it as every
	// other fault of the server.
	DocSchemaInternal = renamed("DOC_SCHEMA", http.StatusInternalServerError, "INTERNAL")

	// ParentInput is a parent the request names that cannot be the issue's parent: the issue
	// itself, an issue that does not exist, or one of another project.
	ParentInput = both("PARENT_INPUT", http.StatusBadRequest)
	// ParentCycle is a parent that would make the issue its own ancestor (validateIssueParent).
	// /api/v2 answers a loop through parents and a loop through dependencies with one code (R7).
	ParentCycle = renamed("PARENT_INPUT", http.StatusConflict, "ISSUE_CYCLE")
	// ParentTooDeep is a parent that would put an issue more links below its root than the cap
	// (refuseParentTooDeep), which /api/v1 answered under the parent loop's code.
	ParentTooDeep = renamed("PARENT_INPUT", http.StatusConflict, "PARENT_TOO_DEEP")
	// DependencyCycle is a blocked_by or parent link that would make an issue wait on itself, or
	// a dependency graph deeper than the walk that looks for one (assertNoWaitPath).
	DependencyCycle = renamed("DEPENDENCY_CYCLE", http.StatusConflict, "ISSUE_CYCLE")

	// BlockNotFound is a {block_id} the path's document does not hold, the 404 idRoutes'
	// BlockNotFound names. /api/v1 answers it as it answers a block an operation names
	// (TargetNotFound); /api/v2 names it as the object its path names
	// (dispatch://INFRA-4071/spec R7).
	BlockNotFound = renamed("TARGET_NOT_FOUND", http.StatusNotFound, "BLOCK_NOT_FOUND")

	// SessionGroupNotFound is a session group's {id} that names no group, and a group gone before
	// the write that read it could make it (delivery.ErrSessionGroupNotFound). /api/v1 answers
	// both as it answers a row nothing holds (NOT_FOUND); /api/v2 names the group, the object its
	// path names (dispatch://INFRA-4071/spec R7).
	SessionGroupNotFound = renamed("NOT_FOUND", http.StatusNotFound, "SESSION_GROUP_NOT_FOUND")

	// AskClosed is a first answer to an ask somebody has already answered (transitionAskTx),
	// which /api/v1 answered under a code of its own. /api/v2 answers it as it answers every other
	// transition of an answered ask (AskAnswered), so one condition takes one code there (R7).
	AskClosed = renamed("ASK_CLOSED", http.StatusConflict, "ASK_ANSWERED")
)

// The decoder's codes that no route of c8a44f97 answered (decode_v2.go). /api/v1 answers them only
// at the /api/v1 path of a row served in both versions, which the decoder reads as it reads the
// row's /api/v2 path (R2 of dispatch://INFRA-4071/spec: a new row is served in both versions by
// one declaration); every other /api/v1 route ignores a key it does not read, as it always did.
var (
	// QueryKeyUnknown is a query key the route does not declare.
	QueryKeyUnknown = both("QUERY_KEY_UNKNOWN", http.StatusBadRequest)
	// StateInvalid is a state body whose `state` names no state its route takes, or none at all:
	// the decoder of a union keyed by `state` names the states it takes.
	StateInvalid = both("STATE_INVALID", http.StatusBadRequest)
)

// The codes only /api/v2 answers.
var (
	// NoRoute is a path under /api/v2 that no route serves.
	NoRoute = v2Only("NO_ROUTE", http.StatusNotFound)
	// PositionInvalid is a position parameter that is not one of the route's position field.
	PositionInvalid = v2Only("POSITION_INVALID", http.StatusBadRequest)

	// The objects a v2 path names that have no not-found code of their own under /api/v1, each
	// the 404 of a path whose object does not exist (dispatch://INFRA-4071/spec R7). The broker's
	// ids answer the broker's own 404 as one of these.
	AskNotFound               = v2Only("ASK_NOT_FOUND", http.StatusNotFound)
	AgentTokenNotFound        = v2Only("AGENT_TOKEN_NOT_FOUND", http.StatusNotFound)
	VersionNotFound           = v2Only("VERSION_NOT_FOUND", http.StatusNotFound)
	DeliveryNotFound          = v2Only("DELIVERY_NOT_FOUND", http.StatusNotFound)
	CredentialRequestNotFound = v2Only("CREDENTIAL_REQUEST_NOT_FOUND", http.StatusNotFound)
	CredentialGrantNotFound   = v2Only("CREDENTIAL_GRANT_NOT_FOUND", http.StatusNotFound)
	MachineLoginNotFound      = v2Only("MACHINE_LOGIN_NOT_FOUND", http.StatusNotFound)
	SigninNotFound            = v2Only("SIGNIN_NOT_FOUND", http.StatusNotFound)
	// ThreadNotFound is a thread mark's {id} that names no ask, root comment or root message
	// (ThreadMissing): /api/v1 wrote nothing for such an id and answered 204.
	ThreadNotFound = v2Only("THREAD_NOT_FOUND", http.StatusNotFound)
	// StepNotFound is a step record's {id} that no record holds, under a write that does not
	// name the run's identity and so cannot make one (StepMissing): /api/v1 answers that write 400
	// STEP_INPUT.
	StepNotFound = v2Only("STEP_NOT_FOUND", http.StatusNotFound)
)

// The codes of the secrets broker's refusals on the routes Dispatch's /api/v2 credential routes
// call (brokerCall in the api package), which /api/v2 answers through its one error writer with
// the broker's message (dispatch://INFRA-4071/spec R7). /api/v1 names none of them: it relays the
// broker's bytes, its code among them, as it always did.
var (
	// AgentSecretsRefusalUndeclared is a refusal the broker answered a call with under a code, or
	// at a status, that the call does not declare: a code a broker newer than this Dispatch added
	// or renamed, or an answer that names no code, such as a gateway's page in front of the
	// broker. /api/v2 answers no code its document does not list, so it cannot pass the code on,
	// nor the status, which a client would read with that status's meaning for an answer Dispatch
	// did not understand. 502 is what a gateway answers for an upstream answer it cannot vouch for,
	// which this is, and the message names the broker's status, code and message, so nothing the
	// broker said is lost.
	AgentSecretsRefusalUndeclared = v2Only("AGENT_SECRETS_REFUSAL_UNDECLARED", http.StatusBadGateway)

	// The broker's own codes, each under the broker's name and at the broker's status. Its
	// INTERNAL, REQUEST_TOO_LARGE and INVALID_DECISION are answered as InternalError,
	// RequestTooLarge and InvalidDecision, which carry that name and status already, and its 404
	// NOT_FOUND as the not-found code of the object the route's path names.
	ApproverAccountInput     = v2Only("APPROVER_ACCOUNT_INPUT", http.StatusBadRequest)
	ApproverIsGroup          = v2Only("APPROVER_IS_GROUP", http.StatusBadRequest)
	ApproverReplaced         = v2Only("APPROVER_REPLACED", http.StatusForbidden)
	ApproverRequired         = v2Only("APPROVER_REQUIRED", http.StatusBadRequest)
	CodeMismatch             = v2Only("CODE_MISMATCH", http.StatusForbidden)
	CodeRequired             = v2Only("CODE_REQUIRED", http.StatusBadRequest)
	CredentialIDInput        = v2Only("CREDENTIAL_ID_INPUT", http.StatusBadRequest)
	DecideOnBrokerPage       = v2Only("DECIDE_ON_BROKER_PAGE", http.StatusForbidden)
	GrantIDInput             = v2Only("GRANT_ID_INPUT", http.StatusBadRequest)
	GrantMoving              = v2Only("GRANT_MOVING", http.StatusConflict)
	InvalidDismissal         = v2Only("INVALID_DISMISSAL", http.StatusBadRequest)
	InvalidLookup            = v2Only("INVALID_LOOKUP", http.StatusBadRequest)
	InvalidRevoke            = v2Only("INVALID_REVOKE", http.StatusBadRequest)
	JudgeDecides             = v2Only("JUDGE_DECIDES", http.StatusForbidden)
	KeyBindsReadToken        = v2Only("KEY_BINDS_READ_TOKEN", http.StatusConflict)
	KeyHoldsLiveCredential   = v2Only("KEY_HOLDS_LIVE_CREDENTIAL", http.StatusConflict)
	MemberKindUncheckable    = v2Only("MEMBER_KIND_UNCHECKABLE", http.StatusServiceUnavailable)
	NoSuchCode               = v2Only("NO_SUCH_CODE", http.StatusNotFound)
	NotApprover              = v2Only("NOT_APPROVER", http.StatusForbidden)
	PersonRequired           = v2Only("PERSON_REQUIRED", http.StatusBadRequest)
	ProjectAccessUnavailable = v2Only("PROJECT_ACCESS_UNAVAILABLE", http.StatusServiceUnavailable)
	RecordIDInput            = v2Only("RECORD_ID_INPUT", http.StatusBadRequest)
	RecordPending            = v2Only("RECORD_PENDING", http.StatusConflict)
	RecordTerminal           = v2Only("RECORD_TERMINAL", http.StatusConflict)
	RequestEnded             = v2Only("REQUEST_ENDED", http.StatusServiceUnavailable)
	ServiceLogsInWithToken   = v2Only("SERVICE_LOGS_IN_WITH_TOKEN", http.StatusBadRequest)
	SigninIDInput            = v2Only("SIGNIN_ID_INPUT", http.StatusBadRequest)
	StateInput               = v2Only("STATE_INPUT", http.StatusBadRequest)

	// The broker's refusals of Dispatch's own call, which it answers on any of those routes
	// before the route runs (accountAuth in its route table), each under the broker's name: those
	// of Dispatch's own ServiceAccount token, and RATE_LIMITED, which spends the bucket of the
	// address Dispatch calls from before any token is verified. The broker refused Dispatch, not
	// the caller, so none is answered at the 401, 403 or 429 the broker wrote, which would tell
	// the caller its own credential failed or that it sent too many.

	// AccountLacksPower is the broker's 403 to Dispatch's account, listed in its
	// BROKER_API_ACCOUNTS without the power the route needs. It is answered 502: the broker
	// Dispatch relays to refused Dispatch itself, as the deployment configures the two, which
	// nothing the caller sends changes; a 403 would tell the caller its own credential may not.
	AccountLacksPower = v2Only("ACCOUNT_LACKS_POWER", http.StatusBadGateway)
	// AccountNotListed is the broker's 403 to a sound token naming an account its
	// BROKER_API_ACCOUNTS does not list, Dispatch's own. It is answered 502 for the reason
	// AccountLacksPower is: the broker refused Dispatch's configuration, not the caller.
	AccountNotListed = v2Only("ACCOUNT_NOT_LISTED", http.StatusBadGateway)
	// AccountTokenExpired is the broker's 401 to Dispatch's own ServiceAccount token, expired. It
	// is answered 502: the token that failed is the one Dispatch presented upstream, and a 401
	// would send the caller to renew a credential of its own that never failed.
	AccountTokenExpired = v2Only("ACCOUNT_TOKEN_EXPIRED", http.StatusBadGateway)
	// AccountTokenInvalid is the broker's 401 to Dispatch's own ServiceAccount token, which does
	// not verify or names no issuer the broker trusts. It is answered 502 for the reason
	// AccountTokenExpired is.
	AccountTokenInvalid = v2Only("ACCOUNT_TOKEN_INVALID", http.StatusBadGateway)
	// AccountTokenTooLongLived is the broker's 401 to Dispatch's own ServiceAccount token, which
	// names no issue time or lives longer than the broker's hour from it. It is answered 502 for
	// the reason AccountTokenExpired is.
	AccountTokenTooLongLived = v2Only("ACCOUNT_TOKEN_TOO_LONG_LIVED", http.StatusBadGateway)
	// AccountTokensOff is the broker's 401 when it trusts no token issuer for its API callers, so
	// no token of Dispatch's authenticates. It is answered 502 for the reason AccountLacksPower
	// is: the broker's configuration refused Dispatch, and a 401 would blame the caller's
	// credential.
	AccountTokensOff = v2Only("ACCOUNT_TOKENS_OFF", http.StatusBadGateway)
	// RateLimited is the broker's 429 to Dispatch's calls, whose requests, those of every caller
	// of Dispatch together, spent the broker's bucket for the address they come from, which the
	// broker spends before it verifies any token. It is answered 503 with the broker's Retry-After
	// where the broker sent one: the service Dispatch relies on cannot take the call for now, and
	// a 429 would tell the caller that it, one of Dispatch's many callers, sent too many.
	RateLimited = v2Only("RATE_LIMITED", http.StatusServiceUnavailable)
)

// The codes only /api/v1 answers.
var (
	// APIV1Ended is every /api/v1 path once the deployment's end of /api/v1 has passed (R3.4).
	APIV1Ended = v1Only("API_V1_ENDED", http.StatusGone)
)
