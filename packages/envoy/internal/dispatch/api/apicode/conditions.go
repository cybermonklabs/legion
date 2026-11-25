package apicode

// Condition names one condition a request can be refused for. A v2 route declares each refusal
// it answers as a Refusal, a (condition, code) pair, so one condition gets one code on every
// route (dispatch://INFRA-4071/spec R7). Like a Code it is a struct with an unexported field, so
// no package but this one names a condition and a string does not convert to one.
type Condition struct {
	name string
}

// String is the condition's name, for a test's or a log's message.
func (c Condition) String() string { return c.name }

// Refusal is one refusal a v2 route declares: when Condition holds, the route answers Code.
type Refusal struct {
	Condition Condition
	Code      Code
}

// The objects a v2 path names, each missing: the path's {key}, {id}, {block_id}, {number} or
// {attempt} names a row Dispatch stores, and that row does not exist (dispatch://INFRA-4071/spec
// R7). A route answers each with the object's own not-found code.
var (
	IssueMissing             = Condition{"issue missing"}
	ProjectMissing           = Condition{"project missing"}
	DocumentMissing          = Condition{"document missing"}
	AskMissing               = Condition{"ask missing"}
	CommentMissing           = Condition{"comment missing"}
	MessageMissing           = Condition{"message missing"}
	HandoffMissing           = Condition{"handoff missing"}
	BroadcastMissing         = Condition{"broadcast missing"}
	PriorityMissing          = Condition{"priority missing"}
	ResultMissing            = Condition{"result missing"}
	AgentTokenMissing        = Condition{"agent token missing"}
	SessionGroupMissing      = Condition{"session group missing"}
	NearCopyCheckMissing     = Condition{"near-copy check missing"}
	GoingRequestMissing      = Condition{"going request missing"}
	BlockMissing             = Condition{"block missing"}
	VersionMissing           = Condition{"version missing"}
	DeliveryMissing          = Condition{"delivery missing"}
	CredentialRequestMissing = Condition{"credential request missing"}
	CredentialGrantMissing   = Condition{"credential grant missing"}
	MachineLoginMissing      = Condition{"machine login missing"}
	SigninMissing            = Condition{"sign-in missing"}
	// ThreadMissing is the {id} of a thread mark (PATCH and GET /api/v2/me/marks/thread/{id})
	// that names no thread's root: no ask, no root comment and no root message. A thread is one
	// of the three by its root's id, so an id that names none of them names no thread.
	ThreadMissing = Condition{"thread missing"}
	// StepMissing is a step record's {id} (PUT /api/v2/issues/{key}/steps/{id} and its
	// /rating) that no record holds, under a write that does not name the run's identity: the
	// first write of a run makes its record, so only a write that cannot make it meets a missing
	// step.
	StepMissing = Condition{"step missing"}
	// ArchitectureSourceMissing is a repository a route names as one of a project's architecture
	// sources (the {owner}/{repo} of DELETE /api/v2/projects/{key}/architecture-sources/{owner}/{repo},
	// a member of POST /api/v2/projects/{key}/architecture-syncs's repos) that the project has no
	// source from, before the route acts or while it runs.
	ArchitectureSourceMissing = Condition{"architecture source missing"}
	// DeployRunMissing is the {id} of a deploy run (GET /api/v2/deploy-runs/{id}) that names no
	// run of the deployment's deploy repository Dispatch holds.
	DeployRunMissing = Condition{"deploy run missing"}
)

// The conditions of a path value written in no form the object it names takes.
var (
	// RunIDMalformed is the {id} of a deploy run (GET /api/v2/deploy-runs/{id}) that is no
	// positive integer, the form of a GitHub run id.
	RunIDMalformed = Condition{"run id malformed"}
	// SessionIDMalformed is a {session_id} that is no session id: empty, or holding /, ?, #,
	// whitespace, a control character or <>"'`[]| (text.IsSessionID).
	SessionIDMalformed = Condition{"session id malformed"}
	// SessionIDBlank is a {session_id} of a session's conversations that is blank.
	SessionIDBlank = Condition{"session id blank"}
)

// The conditions of what the object a request's path names is, where the route serves another.
var (
	// DocumentOfIssue is an issue's document on a route of a project document's comments, asks,
	// events or subscribers, which an issue's are its own.
	DocumentOfIssue = Condition{"document of an issue"}
	// DocumentOfConversation is a file of an agent's conversation on a route of a project
	// document's comments, asks, events or subscribers, which a conversation has none of.
	DocumentOfConversation = Condition{"file of an agent's conversation"}
	// ProjectDocumentRemoved is a project's document on the removal of an untouched issue document.
	ProjectDocumentRemoved = Condition{"removal of a project document"}
	// SpecRenamed is the title correction of an issue's spec, whose name is fixed.
	SpecRenamed = Condition{"title correction of the spec"}
	// RenamedNotDocument is the title correction of an artifact that is a file or an image.
	RenamedNotDocument = Condition{"title correction of an artifact that is no document"}
	// NotTheSpec is an approval request that puts the Verification document beside a document
	// that is not its issue's spec.
	NotTheSpec = Condition{"verification beside a document that is not the spec"}
	// NotThePlan is a plan review of a document that is not its issue's plan, or a plan an upload
	// stored as a file or an image.
	NotThePlan = Condition{"plan review of a document that is not the plan"}
)

// The conditions of the state the object a request's path names stands in.
var (
	// OwnerIssueClosed is a write a closed issue refuses, into the issue or a row it owns.
	OwnerIssueClosed = Condition{"issue closed"}
	// PostWithdrawn is a write that would put text on a post its author deleted, or under it.
	PostWithdrawn = Condition{"post deleted"}
	// ArtifactTouched is the removal of an issue document that holds writing, a later version, an
	// unsettled edit or anything that names it.
	ArtifactTouched = Condition{"document touched"}
	// NoSettledVersion is a review or an approval request of a document that has no settled
	// version yet.
	NoSettledVersion = Condition{"no settled version"}
	// VerificationDocumentMissing is an approval request with include_verification on an issue that
	// has no Verification document.
	VerificationDocumentMissing = Condition{"no verification document"}
	// ApprovalOfPlan is an approval request of an issue's plan, which a plan review approves.
	ApprovalOfPlan = Condition{"approval request of a plan"}
	// ApprovalWaitsOnPerson is a renewed approval request that would rewrite the card of a request
	// that waits on its person.
	ApprovalWaitsOnPerson = Condition{"approval request rewritten while it waits on its person"}
	// DocumentOpenInRoom is the rebuild of a document a room holds open.
	DocumentOpenInRoom = Condition{"document open in a room"}
	// DocumentHistoryLoads is the rebuild of a document whose history loads.
	DocumentHistoryLoads = Condition{"document history loads"}
	// ReviewedVersionMoved is a review naming the version its caller read, where the review would
	// record another.
	ReviewedVersionMoved = Condition{"reviewed version moved"}
	// SessionNotSubscribed is the unsubscription of a session that holds no topic of the owner.
	SessionNotSubscribed = Condition{"session not subscribed"}
	// TopicNotRemovable is the unsubscription of a session that only a broader topic matches.
	TopicNotRemovable = Condition{"topic not removable"}
)

// The conditions of a request body's members.
var (
	// ValuePastBound is a request value longer, or holding more items, than its bound.
	ValuePastBound = Condition{"value past its bound"}
	// BodyHoldsDecisionBlock is a post's body that holds a decision block.
	BodyHoldsDecisionBlock = Condition{"decision block in a body"}
	// CommentInvalid is a comment's body or thread members the write refuses: no body, a reply
	// that carries an anchor or a suggestion, a reply_to or an ask_id naming nothing of the owner,
	// a turn that is neither human nor agent.
	CommentInvalid = Condition{"comment input invalid"}
	// MentionInvalid is a comment's mentions, a delivery mode, or a mention target the write
	// refuses, or a mention reply's blank body.
	MentionInvalid = Condition{"mentions invalid"}
	// AskIDShort is a comment's ask_id that is no full ask id.
	AskIDShort = Condition{"ask id not a full id"}
	// AnchorInvalid is an anchor that names no document or neither a quote nor a mark, or a
	// document of another owner.
	AnchorInvalid = Condition{"anchor invalid"}
	// TurnWithoutAsk is a comment's turn on a comment that answers no ask.
	TurnWithoutAsk = Condition{"turn without an ask"}
	// AskKindRefused is an ask's kind that is not question.
	AskKindRefused = Condition{"ask kind refused"}
	// AskInvalid is an ask's question, options or urgency the write refuses.
	AskInvalid = Condition{"ask input invalid"}
	// UploadInvalid is an upload with no name, no file or a content type that does not parse, or
	// a new name or key past its bound.
	UploadInvalid = Condition{"upload invalid"}
	// UploadInputRefused is an upload naming primary, giving content beside a file, giving no
	// content under an existing document's name, or putting a document into a conversation.
	UploadInputRefused = Condition{"upload input refused"}
	// UploadKindMismatch is an upload under the name of an artifact of another kind.
	UploadKindMismatch = Condition{"upload of another kind than its artifact"}
	// RoleDocumentName is a file, or a document with no content, under the name of an issue's
	// plan, Verification document or decisions document, or a title giving another document one.
	RoleDocumentName = Condition{"name of a role document"}
	// ArtifactNameInvalid is a title that is empty or past its bound.
	ArtifactNameInvalid = Condition{"artifact name invalid"}
	// ArtifactNameHeld is a title another artifact of the owner holds by name or slug.
	ArtifactNameHeld = Condition{"artifact name taken"}
	// SummaryBlank is an approval request's summary given blank.
	SummaryBlank = Condition{"summary blank"}
	// ReviewInvalid is a review's state or version the write refuses.
	ReviewInvalid = Condition{"review input invalid"}
	// ReasonMissing is a request for changes that gives no reason.
	ReasonMissing = Condition{"reason missing"}
	// VersionSummaryBlank is a named version's summary given blank.
	VersionSummaryBlank = Condition{"version summary blank"}
	// EditOpInvalid is an edit op the document service refuses.
	EditOpInvalid = Condition{"edit op invalid"}
	// ReplaceFieldsInvalid is a replace op that names no with or a member a replace does not take.
	ReplaceFieldsInvalid = Condition{"replace op fields invalid"}
	// PreconditionMalformed is a precondition no write of the route can hold to: one naming blocks
	// on an upload, or one on an upload stored as a file.
	PreconditionMalformed = Condition{"precondition malformed"}
	// SuggestionActionInvalid is an accept of an unanchored suggestion, or an accept or a reject of
	// a comment that holds no suggestion.
	SuggestionActionInvalid = Condition{"suggestion action invalid"}
	// MessageInvalid is a message's body given blank, or an in_reply_to_block the write refuses.
	MessageInvalid = Condition{"message input invalid"}
	// MessageInputRefused is a message's target, delivery mode or in_reply_to the write refuses,
	// or a delivery reply's blank body.
	MessageInputRefused = Condition{"message target or delivery refused"}
	// BroadcastInputInvalid is a broadcast's delivery mode or sessions the write refuses.
	BroadcastInputInvalid = Condition{"broadcast input invalid"}
	// HandoffInputInvalid is a handoff's phase that is no lowercase slug, or a body that is not one
	// JSON object as a string.
	HandoffInputInvalid = Condition{"handoff input invalid"}
	// AgentTokenNameBlank is a personal token's name given blank.
	AgentTokenNameBlank = Condition{"agent token name blank"}
	// TokenScopesInvalid is a personal token's scopes left out of a change of them, or naming a
	// scope the deployment does not know or the caller does not hold.
	TokenScopesInvalid = Condition{"token scopes invalid"}
	// MarkKindUnknown is a mark's kind that is none of the six a mark has.
	MarkKindUnknown = Condition{"mark kind unknown"}
	// MarkStateInvalid is a mark's set or a document mark's read_version the write refuses.
	MarkStateInvalid = Condition{"mark state invalid"}
	// SnoozeInvalid is an ask mark's snoozed_until left out or not in the future.
	SnoozeInvalid = Condition{"snooze invalid"}
	// RoleHoldersInvalid is a role-holder set the write refuses: no holders, a role that is none of
	// the six, a session that is no session id, a role or a session named twice.
	RoleHoldersInvalid = Condition{"role holders invalid"}
)

// The conditions of who the caller is, where the route takes only some callers.
var (
	// ActingSessionMismatch is a request naming, in its body's actor or its session parameter, a
	// session other than the one its credential proved.
	ActingSessionMismatch = Condition{"acting session other than the one the credential proved"}
	// ReplyNotTheRecipient is a delivery reply from a cookie caller, from a session the attempt is
	// not for, or naming neither or both of body and error.
	ReplyNotTheRecipient = Condition{"reply from a caller that may not answer the attempt"}
	// AcceptNotTheRecipient is a delivery accept from a cookie caller or from a session the attempt
	// is not for.
	AcceptNotTheRecipient = Condition{"accept from a caller that may not take the attempt"}
	// HandoffFromCookie is a handoff a cookie caller writes, which an agent session writes alone.
	HandoffFromCookie = Condition{"handoff from a cookie caller"}
	// RoleHolderNotTheDaemon is a role-holder set written by any actor but its project's Legion
	// daemon session.
	RoleHolderNotTheDaemon = Condition{"role holders from another than the project's daemon"}
	// DirectThreadWithoutSession is a direct conversation read by a credential without the person
	// scope that names no session.
	DirectThreadWithoutSession = Condition{"direct conversation read with no session named"}
	// DirectThreadOfAnotherSession is a direct conversation read as a session it is not addressed
	// to and that has not replied in it.
	DirectThreadOfAnotherSession = Condition{"direct conversation read as a session outside it"}
)

// The conditions of the delivery attempt an accept names (PUT
// /api/v2/messages/{id}/deliveries/{attempt}/state), each one of its compare-and-set's tests.
var (
	AcceptOfIndirectMessage      = Condition{"accept of a message not sent directly to the session"}
	AcceptOfMessageNoPersonWrote = Condition{"accept of a message no person wrote"}
	AcceptOfAgentWrittenMessage  = Condition{"accept of a message an agent wrote for its person"}
	AcceptOfOtherMode            = Condition{"accept of a message sent neither as a Send nor as an Aside"}
	AcceptOfOlderAttempt         = Condition{"accept of an attempt that is not the message's latest"}
	AcceptAfterAccept            = Condition{"accept of a message accepted before"}
	AcceptOfAttemptNoPersonAsked = Condition{"accept of an attempt no person requested"}
	AcceptOfAttemptAnotherAsked  = Condition{"accept of an attempt another than the author requested"}
	AcceptOfFailedAttempt        = Condition{"accept of a failed attempt"}
	AcceptOfStaleAttempt         = Condition{"accept of an attempt older than a minute"}
)

// The conditions of a write another write changed the ground under.
var (
	// DocumentPreconditionUnmet is a write's precondition the live document no longer meets, or
	// one on a first upload of a name, which replaces no version.
	DocumentPreconditionUnmet = Condition{"document precondition unmet"}
	// EditLost is an edit, or a suggestion's accept, whose text a concurrent change removed before
	// it could be versioned.
	EditLost = Condition{"edit lost to a concurrent change"}
	// QuoteAmbiguous is a quote an edit or an anchor names that the document holds in several
	// places.
	QuoteAmbiguous = Condition{"quote ambiguous"}
	// RouteMoved is a comment whose issue's delivery route changed while it was being written.
	RouteMoved = Condition{"delivery route changed"}
	// MessageTargetMoved is a message whose target changed while its delivery was being recorded.
	MessageTargetMoved = Condition{"message target changed"}
	// DeleteRacedSend is a delete of a post whose pending send its own sender moved while the
	// delete was being written.
	DeleteRacedSend = Condition{"send moved under a delete"}
	// SuggestionDecided is an accept, a reject or a resolve of a suggestion another caller already
	// accepted or rejected.
	SuggestionDecided = Condition{"suggestion already decided"}
	// MarkStateMoved is a mark write under a token or version the mark no longer stands at.
	MarkStateMoved = Condition{"mark state moved"}
)

// The conditions of what a request names that nothing holds, or that names more than one thing.
var (
	// CorrectionOfDirectMessage is an edit or a delete of a message of a conversation with no issue.
	CorrectionOfDirectMessage = Condition{"correction of a direct message"}
	// BroadcastReachesNobody is a broadcast none of whose sessions can take it.
	BroadcastReachesNobody = Condition{"broadcast reaches no session"}
	// ReferenceNodeMissing is a reference read of a node nothing holds.
	ReferenceNodeMissing = Condition{"reference node missing"}
	// RefPrefixAmbiguous is a reference's id written short that two items or more carry.
	RefPrefixAmbiguous = Condition{"reference prefix ambiguous"}
	// QuestionSourceWithoutDocument is an ask list's source whose text belongs to no document.
	QuestionSourceWithoutDocument = Condition{"question source on no document"}
	// CheckMissing is a result's check that names no check block of the issue's Verification
	// document or of an ancestor's.
	CheckMissing = Condition{"check missing"}
	// CheckRefAmbiguous is a result's check named by a block id that is not unique among them.
	CheckRefAmbiguous = Condition{"check reference ambiguous"}
	// IssueRefAmbiguous is an issue reference that names more than one issue.
	IssueRefAmbiguous = Condition{"issue reference ambiguous"}
	// ResultCommitUnheld is a result's commit that no repository it may be of holds.
	ResultCommitUnheld = Condition{"result commit held by no repository"}
	// GitHubReadFailed is a read of GitHub that did not answer: a rate limit, a 5xx, a transport
	// failure.
	GitHubReadFailed = Condition{"github read failed"}
	// SessionNotFollowing is the unfollow of an ask by a session that does not follow it.
	SessionNotFollowing = Condition{"session not following"}
	// TargetMissing is a quote, a heading or a block that an edit or an anchor names and the
	// document does not hold.
	TargetMissing = Condition{"target missing"}
	// AskBlockMissing is an answer, a resolution, a decline or a reword of a block ask whose block
	// its document no longer holds, which settlement has not retracted the ask for yet.
	AskBlockMissing = Condition{"ask block missing"}
	// AnchorMarkMissing is an anchor's mark_id that its document does not hold when the wait for
	// it ends.
	AnchorMarkMissing = Condition{"anchor mark missing"}
	// AnchorDocumentMissing is an anchor whose artifact names, by id or slug, no document of the
	// issue the write is on.
	AnchorDocumentMissing = Condition{"anchor document missing"}
	// SuggestionMarkGone is an accept or a reject of a suggestion whose mark is no longer in its
	// document.
	SuggestionMarkGone = Condition{"suggestion mark gone"}
)

// The conditions of what a write puts into a document, or of the document it writes into.
var (
	// MarkdownRefused is markdown a write puts into a document that is no Proof document, that
	// repeats a block id, or that leaves an open anchor's quote nowhere its mark may stand, or a
	// block pointer it introduces that names a block the pointer may not name.
	MarkdownRefused = Condition{"markdown refused"}
	// AskBlockRefused is an ask block a write puts into a document that does not read as an ask,
	// or a write that changes or drops the block of an open ask.
	AskBlockRefused = Condition{"ask block refused"}
	// CICheckRuleBroken is a check block a write puts into a document that carries ci_check and
	// breaks one of the rules of a check CI proves.
	CICheckRuleBroken = Condition{"ci check rule broken"}
	// DocumentPastSizeBound is a write that would leave a document, its margin or the marks on its
	// text larger than the server stores.
	DocumentPastSizeBound = Condition{"document past its size bound"}
	// AskBlockUnrepresentable is a reword of an ask that its block in the document cannot carry as
	// written.
	AskBlockUnrepresentable = Condition{"ask block unrepresentable"}
	// AcceptRefused is an accept of a suggestion whose replacement the document cannot hold where
	// the suggestion stands.
	AcceptRefused = Condition{"accept refused"}
	// QuoteSpansBlocks is an edit's quote whose match runs past one textblock.
	QuoteSpansBlocks = Condition{"quote spans blocks"}
	// TableRowsTooWide is table rows an edit inserts that hold more cells than the table is wide.
	TableRowsTooWide = Condition{"table rows too wide"}
	// EditPreconditionMalformed is an edit's precondition that names neither or both of document
	// and blocks, a block without its id or token or twice, or no token for a block or a quote
	// spanning blocks that its operations resolve.
	EditPreconditionMalformed = Condition{"edit precondition malformed"}
	// ConditionalEditsQueued is a conditional edit while the document's queue of them is full.
	ConditionalEditsQueued = Condition{"conditional edits queued"}
)

// The conditions of an issue's write and of the members of an issue's body.
var (
	AdvancesInvalid            = Condition{"advances invalid"}
	AdvancesRevisionMoved      = Condition{"advances revision moved"}
	AssigneeNotSignedIn        = Condition{"assignee nobody signed in as"}
	BlockedByInvalid           = Condition{"blocked_by invalid"}
	ComponentsInvalid          = Condition{"components invalid"}
	IssueInputInvalid          = Condition{"issue input invalid"}
	PriorityInvalid            = Condition{"priority invalid"}
	LabelsInvalid              = Condition{"labels invalid"}
	ParentInvalid              = Condition{"parent invalid"}
	ParentChainTooDeep         = Condition{"parent chain too deep"}
	IssueRefInvalid            = Condition{"issue reference invalid"}
	ExternalOfAnotherProject   = Condition{"external reference linked to another project"}
	TitleNearCopy              = Condition{"title a near copy of another issue's"}
	ParentOutsideCallerProject = Condition{"parent outside the credential's project"}
	ExternalLinkHeld           = Condition{"external link held by another issue"}
	// IssueStatusInvalid is an issue status a patch or a search names that is none of the
	// statuses.
	IssueStatusInvalid         = Condition{"issue status invalid"}
	ExternalURLInvalid         = Condition{"external url invalid"}
	IssueSequenceMoved         = Condition{"issue sequence moved"}
	ParentLockContended        = Condition{"parent lock contended"}
	ParentStatusRuleBroken     = Condition{"parent status would be out of line with child status"}
	StatusChangeOptionsInvalid = Condition{"status change options or fields invalid"}
	StatusCascadeNeedsBrowser  = Condition{"closing descendants requires a signed-in browser"}
	// RankInvalid is a rank a write refuses: an issue's rank, or a priority's, naming no neighbour
	// or one it cannot land beside.
	RankInvalid           = Condition{"rank invalid"}
	IssueRouteInvalid     = Condition{"issue route invalid"}
	AdvancesFilterInvalid = Condition{"advances filter invalid"}
	ClosedWindowInverted  = Condition{"closed window inverted"}
	// ClaimHolderMoved is a claim write whose holder changed twice while it ran.
	ClaimHolderMoved = Condition{"claim holder moved"}
	// IssueHeldByAnother is a claim or a release the caller may not make over the holder.
	IssueHeldByAnother = Condition{"issue held by another"}
	// ForceWithoutPersonScope is a forced claim from a credential that does not hold the person
	// scope.
	ForceWithoutPersonScope = Condition{"forced claim without the person scope"}
	// DecisionTextMoved is a keep of a decision whose text changed since the caller read it.
	DecisionTextMoved = Condition{"decision text moved"}
	// KeepWithoutDigest is a keep of a decision that names no text_sha256.
	KeepWithoutDigest = Condition{"keep without a digest"}
	// ReferencesBudgetSpent is a read of an issue's references that spent its statement budget.
	ReferencesBudgetSpent = Condition{"references read budget spent"}
)

// The conditions of a check result's record and retraction, and of a step's and a plan's write.
var (
	ResultInputInvalid     = Condition{"result input invalid"}
	ResultCountsContradict = Condition{"result counts contradict its verdict"}
	ResultOfCICheck        = Condition{"result of a check CI proves"}
	CheckReworded          = Condition{"check reworded while its result was written"}
	ResultRetractedBefore  = Condition{"result retracted before"}
	RetractionByOther      = Condition{"retraction by a caller the result is not open to"}
	RetractionReasonBlank  = Condition{"retraction reason blank"}
	StepInputInvalid       = Condition{"step input invalid"}
	StepOfAnotherIssue     = Condition{"step of another issue"}
	StepWriterNotTheDaemon = Condition{"step or plan written by another than the project's daemon"}
	PlanInputInvalid       = Condition{"plan input invalid"}
	PlanOfAnotherIssue     = Condition{"plan of another issue"}
)

// The conditions of an ask's write: its reword, its answer, its decline and resolution, its
// followers.
var (
	AskNotOpenToEdit = Condition{"reword of an ask that is not open"}
	AskKindChange    = Condition{"reword of an approval ask"}
	// AskAnsweredBefore is a resolve or a decline of an ask already answered.
	AskAnsweredBefore = Condition{"ask answered before"}
	// AnsweredFirstByAnother is a first answer to an ask somebody answered first.
	AnsweredFirstByAnother  = Condition{"ask answered first by another"}
	AskResolvedBefore       = Condition{"ask resolved before"}
	AnswerMoved             = Condition{"answer changed since it was read"}
	ApprovalAnsweredBefore  = Condition{"answer change of an approval"}
	QuestionEditedSinceRead = Condition{"question edited since it was read"}
	ApprovalDeclined        = Condition{"decline of an approval request"}
	RevisionMissing         = Condition{"answer naming no revision"}
	AnswerInvalid           = Condition{"answer invalid"}
	ResolutionInvalid       = Condition{"resolution invalid"}
	NotTheAnswerer          = Condition{"answer change by another than the answerer"}
	FollowerSessionBlank    = Condition{"follower session blank"}
	FollowOfAnotherSession  = Condition{"follow of another session without the person scope"}
)

// The conditions of what a search, a going request and a related search name.
var (
	// SearchAuthorInvalid is a search's author filter that is no one actor id.
	SearchAuthorInvalid = Condition{"search author invalid"}
	// NumberProjectMalformed is a search's number_project that is no project key.
	NumberProjectMalformed = Condition{"number project malformed"}
	// SearchCoverageTimedOut is a search coverage whose one statement ran past its bound.
	SearchCoverageTimedOut = Condition{"search coverage timed out"}
	// CredentialNamesNobody is a going request from a credential that names nobody, whose run
	// no reader would be served.
	CredentialNamesNobody = Condition{"going credential names nobody"}
	// GoingWordsBlank is a going request whose words are blank.
	GoingWordsBlank = Condition{"going words blank"}
	// BodyProjectMalformed is a project a request's body names that is no project key: POST
	// /api/v2/going-requests.
	BodyProjectMalformed = Condition{"body project malformed"}
	// ExcludeIssueInvalid is an issue a related search excludes that is no issue key of the
	// project, or a list of them longer than the bound.
	ExcludeIssueInvalid = Condition{"excluded issue invalid"}
)

// The conditions of what a session group, a project, an architecture source and a priority write
// refuses.
var (
	// SessionGroupInvalid is a session group's name or sessions the write refuses, or an edit
	// that would leave it empty.
	SessionGroupInvalid = Condition{"session group invalid"}
	// SessionAlreadyGrouped is a session another group holds.
	SessionAlreadyGrouped = Condition{"session already grouped"}
	// SessionGroupNameHeld is a session group's name another group has.
	SessionGroupNameHeld = Condition{"session group name held"}
	// SessionGroupNameTitled is a session group's name that is the title of a session outside
	// the group.
	SessionGroupNameTitled = Condition{"session group name is a session title"}
	// ProjectInputInvalid is a project's key, name, default assignee, gated steps or default
	// repository the write refuses, or a patch that names none of them.
	ProjectInputInvalid = Condition{"project input invalid"}
	// ProjectKeyHeld is a project's key another project has.
	ProjectKeyHeld = Condition{"project key held"}
	// ProjectHasNoSource is a read or a sync of every architecture source of a project that has
	// none.
	ProjectHasNoSource = Condition{"project has no architecture source"}
	// SourceInputInvalid is a repository that is no owner/name, or a blank branch, an
	// architecture source's route names.
	SourceInputInvalid = Condition{"source input invalid"}
	// SourceUnreadable is a repository, or a branch of it, the GitHub App cannot read: no App
	// key, no installation, no contents read, or no such branch.
	SourceUnreadable = Condition{"source unreadable"}
	// PriorityInputInvalid is a priority's title, id or write that names nothing the write
	// refuses.
	PriorityInputInvalid = Condition{"priority input invalid"}
	// PriorityIDHeld is a priority's id the store already holds.
	PriorityIDHeld = Condition{"priority id held"}
)

// The conditions of the GitHub App's installation read and of the secrets broker's relays.
var (
	// RepositoryPathInvalid is a repository path whose owner or name GitHub does not allow.
	RepositoryPathInvalid = Condition{"repository path invalid"}
	// GitHubAppKeyMissing is a read of GitHub's App on a Dispatch that holds no App key.
	GitHubAppKeyMissing = Condition{"github app key missing"}
	// BrokerUnreachable is a credential route's call the secrets broker did not answer.
	BrokerUnreachable = Condition{"secrets broker unreachable"}
	// ServiceAccountTokenUnreadable is a credential route's call that stopped at Dispatch's own
	// ServiceAccount token, which it cannot read.
	ServiceAccountTokenUnreadable = Condition{"service account token unreadable"}
	// BrokerRefusalUndeclared is a credential route's call the secrets broker refused under a
	// code, or at a status, that the call does not declare (brokerCall in the api package).
	BrokerRefusalUndeclared = Condition{"secrets broker refusal undeclared"}
)

// The conditions of the refusals the secrets broker answers on the routes Dispatch's credential
// routes call, one for each code it answers there, each that code relayed from the broker. Its
// 404 NOT_FOUND is the condition of the object the route's path names (CredentialRequestMissing
// and the three beside it).
var (
	BrokerAccountLacksPower        = Condition{"ACCOUNT_LACKS_POWER relayed from the secrets broker"}
	BrokerAccountNotListed         = Condition{"ACCOUNT_NOT_LISTED relayed from the secrets broker"}
	BrokerAccountTokenExpired      = Condition{"ACCOUNT_TOKEN_EXPIRED relayed from the secrets broker"}
	BrokerAccountTokenInvalid      = Condition{"ACCOUNT_TOKEN_INVALID relayed from the secrets broker"}
	BrokerAccountTokenTooLongLived = Condition{"ACCOUNT_TOKEN_TOO_LONG_LIVED relayed from the secrets broker"}
	BrokerAccountTokensOff         = Condition{"ACCOUNT_TOKENS_OFF relayed from the secrets broker"}
	BrokerApproverIsGroup          = Condition{"APPROVER_IS_GROUP relayed from the secrets broker"}
	BrokerApproverRequired         = Condition{"APPROVER_REQUIRED relayed from the secrets broker"}
	BrokerCodeMismatch             = Condition{"CODE_MISMATCH relayed from the secrets broker"}
	BrokerCodeRequired             = Condition{"CODE_REQUIRED relayed from the secrets broker"}
	BrokerCredentialIDInput        = Condition{"CREDENTIAL_ID_INPUT relayed from the secrets broker"}
	BrokerDecideOnBrokerPage       = Condition{"DECIDE_ON_BROKER_PAGE relayed from the secrets broker"}
	BrokerGrantIDInput             = Condition{"GRANT_ID_INPUT relayed from the secrets broker"}
	BrokerGrantMoving              = Condition{"GRANT_MOVING relayed from the secrets broker"}
	BrokerInternal                 = Condition{"INTERNAL relayed from the secrets broker"}
	BrokerInvalidDecision          = Condition{"INVALID_DECISION relayed from the secrets broker"}
	BrokerInvalidDismissal         = Condition{"INVALID_DISMISSAL relayed from the secrets broker"}
	BrokerInvalidLookup            = Condition{"INVALID_LOOKUP relayed from the secrets broker"}
	BrokerInvalidRevoke            = Condition{"INVALID_REVOKE relayed from the secrets broker"}
	BrokerJudgeDecides             = Condition{"JUDGE_DECIDES relayed from the secrets broker"}
	BrokerKeyBindsReadToken        = Condition{"KEY_BINDS_READ_TOKEN relayed from the secrets broker"}
	BrokerKeyHoldsLiveCredential   = Condition{"KEY_HOLDS_LIVE_CREDENTIAL relayed from the secrets broker"}
	BrokerMemberKindUncheckable    = Condition{"MEMBER_KIND_UNCHECKABLE relayed from the secrets broker"}
	BrokerNoSuchCode               = Condition{"NO_SUCH_CODE relayed from the secrets broker"}
	BrokerNotApprover              = Condition{"NOT_APPROVER relayed from the secrets broker"}
	BrokerPersonRequired           = Condition{"PERSON_REQUIRED relayed from the secrets broker"}
	BrokerProjectAccessUnavailable = Condition{"PROJECT_ACCESS_UNAVAILABLE relayed from the secrets broker"}
	BrokerRateLimited              = Condition{"RATE_LIMITED relayed from the secrets broker"}
	BrokerRecordIDInput            = Condition{"RECORD_ID_INPUT relayed from the secrets broker"}
	BrokerRecordPending            = Condition{"RECORD_PENDING relayed from the secrets broker"}
	BrokerRecordTerminal           = Condition{"RECORD_TERMINAL relayed from the secrets broker"}
	BrokerRequestEnded             = Condition{"REQUEST_ENDED relayed from the secrets broker"}
	BrokerRequestTooLarge          = Condition{"REQUEST_TOO_LARGE relayed from the secrets broker"}
	BrokerServiceLogsInWithToken   = Condition{"SERVICE_LOGS_IN_WITH_TOKEN relayed from the secrets broker"}
	BrokerSigninIDInput            = Condition{"SIGNIN_ID_INPUT relayed from the secrets broker"}
	BrokerStateInput               = Condition{"STATE_INPUT relayed from the secrets broker"}
)

// The conditions of a service Dispatch relies on.
var (
	// EnvoyUnreachable is a route that needs the Envoy listener, on a Dispatch configured with
	// none or whose listener did not answer.
	EnvoyUnreachable = Condition{"envoy listener unavailable"}
	// EnvoyListRefilling is a route that needs the listener's agent list while the listener
	// refills it.
	EnvoyListRefilling = Condition{"envoy listener refilling"}
	// EnvoyUnsubscribeRefused is an unsubscription the listener did not take.
	EnvoyUnsubscribeRefused = Condition{"envoy unsubscribe failed"}
	// FileStoreDown is a file the file store did not take or did not return.
	FileStoreDown = Condition{"file store unavailable"}
	// FileBytesMissing is a file version whose bytes the file store does not hold.
	FileBytesMissing = Condition{"file bytes missing"}
	// FileStoreNotConfigured is a file version whose bytes are in a file store this server is
	// not configured to read.
	FileStoreNotConfigured = Condition{"file store not configured"}
	// DocumentServiceDown is a document the document service cannot serve now: its room failed,
	// the service is shutting down, or a rebuild holds it.
	DocumentServiceDown = Condition{"document service down"}
	// DocumentHistoryUnloadable is a document whose stored history does not load, the state a
	// rebuild repairs.
	DocumentHistoryUnloadable = Condition{"document history unloadable"}
	// DocumentNewerContent is a document holding an element that a newer build wrote and this
	// build does not know.
	DocumentNewerContent = Condition{"document holds newer content"}
	// DocumentOutsideSchema is a document whose tree is outside the Proof schema.
	DocumentOutsideSchema = Condition{"document outside the schema"}
)

// The conditions every v2 route can meet in its query or its loops (R4, R5 and R7 of
// dispatch://INFRA-4071/spec).
var (
	// PathUnknown is a path under /api/v2 that no route serves.
	PathUnknown = Condition{"path unknown"}
	// QueryKeyUndeclared is a query key the route does not declare.
	QueryKeyUndeclared = Condition{"query key undeclared"}
	// LimitOutOfBounds is a limit that is no integer within the route's bound.
	LimitOutOfBounds = Condition{"limit out of bounds"}
	// OffsetOutOfBounds is an offset that is no integer within the route's bound.
	OffsetOutOfBounds = Condition{"offset out of bounds"}
	// PositionMalformed is a position parameter that is not one of the route's position field.
	PositionMalformed = Condition{"position malformed"}
	// ParentLoop is a parent that would make an issue its own ancestor.
	ParentLoop = Condition{"parent loop"}
	// DependencyLoop is a dependency that would make an issue wait on itself.
	DependencyLoop = Condition{"dependency loop"}
	// OpenForeignChild is an acceptance write that asked for the open-child check while a child of
	// another project is open (refuseOpenForeignChildren, #4884).
	OpenForeignChild = Condition{"open foreign child"}
)

// The conditions of what a request names outside its path.
var (
	// QueryVersionMissing is a version a request's query names that the document holds no version
	// of: GET /api/v2/artifacts/{id}/changes's version_from and version_to, each answered as GET
	// /api/v1/artifacts/{id}/changes answers its from and to, since the path names no version.
	QueryVersionMissing = Condition{"query version missing"}
	// ArtifactNotDocument is an artifact a route reads as a document that is a file or an image.
	ArtifactNotDocument = Condition{"artifact not a document"}
	// QueryProjectMalformed is a project a request's query names that is no project key: the
	// project filter of GET /api/v2/nodes, /search-results and /related-check-stats.
	QueryProjectMalformed = Condition{"query project malformed"}
	// ReferenceQueryInvalid is a reference read that names no reference, names neither or both of
	// to and from, or names one that is no dispatch:// reference: GET /api/v2/references,
	// /reference-targets and /block-links.
	ReferenceQueryInvalid = Condition{"reference query invalid"}
)

// The conditions of what the caller's credential covers.
var (
	// DeploymentMemberScoped is a body member that belongs to the deployment rather than to a
	// project, written by a credential scoped to one project through a row whose own class is a
	// project's write: PATCH /api/v2/projects/{key}'s default_repository. A row whose whole write
	// belongs to the deployment is refused the same code in front of its handler, from its class.
	DeploymentMemberScoped = Condition{"deployment member written under a project scope"}
)

// The conditions of who wrote what a request's path names.
var (
	// NotTheAuthor is a write only the author of the post or the ask the path names may make,
	// from a caller that is not that author: the edit and the delete of a comment and of an issue
	// message (PATCH and DELETE /api/v2/comments/{id} and /api/v2/messages/{id}), and another
	// session's reword of an ask (PATCH /api/v2/asks/{id}). Whether the caller wrote the post is
	// read from the post's row, so the handler decides it, after the post is found.
	NotTheAuthor = Condition{"caller not the author"}
)

// The conditions of what a deployment configures.
var (
	// BrokerUnconfigured is a request to a route that relays to the secrets broker, on a Dispatch
	// configured with none (DISPATCH_AGENT_SECRETS_URL unset).
	BrokerUnconfigured = Condition{"secrets broker unconfigured"}
	// DeliveryUnconfigured is a request to a route that reads the deployment's delivery settings,
	// on a Dispatch that holds none (no delivery_settings row).
	DeliveryUnconfigured = Condition{"delivery settings unconfigured"}
)
