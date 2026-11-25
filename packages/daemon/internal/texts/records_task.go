package texts

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// This file holds the records of the tasks: the prompts that the daemon sends a session. Render
// writes each of them. A task record holds the words of another author in Words or *Words fields,
// which Render draws as blocks after the fields.

// DesignApprovedWake is the sentence that a resume of a waiting root carries when the root's
// design gate is open. The daemon sends no design-approved notice to a root that waits on a
// person, so this resume is that root's design-approved wake. The architect's skill and the root
// architect's prompt name the sentence by its words, and a sentence a reader's guidance names by
// its words stays word for word (dispatch://INFRA-3686/spec R8).
const DesignApprovedWake = "this resume is your design-approved wake"

// ControllerStartMessage is the controller's first prompt at every start (the daemon's
// controllerKeeper). The controller's skill names it by its words, so it stays word for word
// (dispatch://INFRA-3686/spec R8).
const ControllerStartMessage = "Legion controller start: follow skill://legion-controller's start procedure now (\"What happened before you started\"), then end the turn."

// The words of the act that answered a root's wait (RootResumeRecord.Act), as record.Answer
// names them, and the word of an act that none of them names.
const (
	ActApproved         = "approved"
	ActChangesRequested = "changes_requested"
	ActVersion          = "version"
	ActRetracted        = "retracted"
	ActClosed           = "closed"
	ActAsk              = "ask"
	ActReply            = "reply"
	ActTurn             = "turn"
	ActDeclined         = "declined"
	ActActed            = "acted"
)

// PhaseAssignmentRecord is the task of a phase worker that the daemon starts on a phase
// (workflow's task): the issue's title and the phase, where the worker's work stands (Standing),
// the handoff that ended the phase before it (its phase word and its version, which dispatch
// handoff-read reads), the commit that handoff names, the tester's verdict, and the pull request.
// The text of the block of the worker's task, which the owner of the issue wrote in its plan, goes
// last, in the task_text block, by the architect claim that gave that version of the plan, at the
// time it gave it (TaskText): only a step's worker of an issue with an owner's plan is given one,
// since Legion writes the text of the task default itself.
//
// A move back names the phase it came from (MovedBackFrom), and the words of the role that moved
// it go in the reason block, by that role. A review's rejection names the head it reviewed and
// GitHub's id of the review (ReviewID, absent where the decision an older daemon recorded holds
// none), and the review's body goes in the body block, by its author: where the block names a
// rest, or that more follows, the whole body is that review's on the pull request, the item's
// reference dispatch://INFRA-3686's R1 has a cut text carry. A merger's task at a head that a merge
// forward left after the review, while the review that landing asked of the reviewer stands for
// that head, names the base the merge brought forward (MergedForward) and that head as the one
// whose approval it waits for (ApprovalOwed). A retry names who retried (architect, operator, or a
// todo written on the held issue), and the architect's own words go in the reason block. A retry
// of a review round that nothing on its way ends names the round's cause (StuckRound, a
// ReviewStuckRecord cause), its head, the head the reviewer approved where that is other code
// (ReviewedHead), the review that ends it (EndsRound), and why the daemon set aside a review that
// would otherwise have decided it (SetAside). A round GitHub will not merge from names, as its
// review-stuck notice does, the merge state GitHub reports for the head, each check and each
// commit status of that head that did not pass, each check of that head GitHub skipped (skipped)
// with the field that says once that a skipped check ran nothing on the head (each_skipped_check,
// CheckConclusion), the count of the checks that passed (passed), the count of the commit statuses
// that passed (passed_statuses, absent where none did) and the command that lists them. A stopped
// task started again at its first step names what started it (Restarted: a todo
// written on the held issue, or a later version of the owner's plan), and, for a plan, that
// version (PlanVersion) and the stopped tasks it changed (StoppedTasks).
type PhaseAssignmentRecord struct {
	TaskHead
	Title string      `json:"title"`
	Phase phase.Phase `json:"phase"`
	Standing
	HandoffPhase      string            `json:"handoff_phase,omitempty"`
	HandoffVersion    int               `json:"handoff_version,omitempty"`
	HandoffCommit     string            `json:"handoff_commit,omitempty"`
	TesterVerdict     string            `json:"tester_verdict,omitempty"`
	PullRequest       string            `json:"pull_request,omitempty"`
	MovedBackFrom     phase.Phase       `json:"moved_back_from,omitempty"`
	ReviewedHead      string            `json:"reviewed_head,omitempty"`
	ReviewID          int64             `json:"review_id,omitempty"`
	MergedForward     string            `json:"merged_forward,omitempty"`
	ApprovalOwed      string            `json:"approval_owed,omitempty"`
	Head              string            `json:"head,omitempty"`
	Retried           string            `json:"retried,omitempty"`
	StuckRound        string            `json:"stuck_round,omitempty"`
	EndsRound         string            `json:"ends_round,omitempty"`
	SetAside          string            `json:"set_aside,omitempty"`
	MergeState        string            `json:"merge_state,omitempty"`
	NotPassed         []CheckConclusion `json:"not_passed,omitempty"`
	Skipped           []string          `json:"skipped,omitempty"`
	EachSkippedCheck  string            `json:"each_skipped_check,omitempty"`
	NotPassedStatuses []StatusState     `json:"not_passed_statuses,omitempty"`
	Passed            *int              `json:"passed,omitempty"`
	PassedStatuses    int               `json:"passed_statuses,omitempty"`
	ListPassed        string            `json:"list_passed,omitempty"`
	Restarted         string            `json:"restarted,omitempty"`
	PlanVersion       int               `json:"plan_version,omitempty"`
	StoppedTasks      []string          `json:"stopped_tasks,omitempty"`
	Reason            *Words            `json:"reason"`
	Body              *Words            `json:"body"`
	TaskText          *Words            `json:"task_text"`
}

// Standing is where a phase worker's work stands, which its phase assignment and its resume task
// both name (workflow's standingPart), since a worker resumed in a fresh session has no earlier
// task to read it from. Every start names its working copy: the issue's branch, which its
// workspace's bookmark names (claim.IssueBranch), and the directory the runtime gives the
// workspace, where it gives one. An issue with plan rows also names its one task (the plan task's
// id) and the task's steps, each as its plan writes it, in the order they run, each at its place
// from 1; Legion runs its retro and its merge after them. A step's worker names its place in the
// task (Step, of len(Steps)) and each step before it whose role's completion named a handoff
// record (Reported). The retro and the merge run no step of the task, so their worker names the
// task and its steps alone: the merger reads from them whether the task has a reviewer step. An
// issue with no plan rows names the working copy alone, and a task with no task field is such an
// issue's.
type Standing struct {
	Branch    string       `json:"branch,omitempty"`
	Directory string       `json:"directory,omitempty"`
	Task      string       `json:"task,omitempty"`
	Step      int          `json:"step,omitempty"`
	Steps     []string     `json:"steps,omitempty"`
	Reported  []StepReport `json:"reported,omitempty"`
}

// StepReport is one step before a worker's step that reported: its place in the task, its role
// as the plan writes it, and the handoff record its role's completion of that step named, by its
// phase word and its version (decision 9 of dispatch://INFRA-3686's plan), which dispatch
// handoff-read reads. A step whose role writes no handoff (the owner's acceptance) has none.
type StepReport struct {
	Step           int    `json:"step"`
	Role           string `json:"role"`
	HandoffPhase   string `json:"handoff_phase"`
	HandoffVersion int    `json:"handoff_version"`
}

// PhaseResumeRecord is the task of a worker started for a phase already under way
// (workflow's Engine.ResumeTask): the issue's title, the phase, where the worker's work stands
// (Standing) and, last, the text of its task's block in the task_text block, as a phase
// assignment names them.
type PhaseResumeRecord struct {
	TaskHead
	Title string      `json:"title"`
	Phase phase.Phase `json:"phase"`
	Standing
	TaskText *Words `json:"task_text"`
}

// RootResumeRecord is the task that an answered root's architect resumes with (workflow's
// resumeTask).
//
// The act: what answered the wait (Act, one of the Act words), who acted and when, the ask it
// answered as a dispatch:// link, and the version the act was about. GateOffAtAnswer says that
// the design gate was off when the wait was answered. ClosedBy names who closed the ask. Options
// are the options the answer chose. DispatchWords are the sentences Dispatch wrote for the asking
// agent on a decline.
//
// The gate of the step in question now, wherever that step has a gate, whatever the act: its
// document as a dispatch:// link (Document), its latest version, whether it is open, and whether
// the design gate is armed. A step with no gate, as a question asked before any step of the gate
// was registered has, names no gate state: GateOpen is nil, as main's resume wrote no gate
// sentence for it, and so is GateArmed, but on an approval (ActApproved). An approval of such a
// step names GateArmed all the same, since whether the design gate is armed is what says whether
// a person approved (main's "Your %s %s on %s was approved." under an armed gate, and "The design
// gate on %s is off, so no approval of your %s %s is asked for." under one that is not). A
// document an approval, a changes request or a version answered the wait on is that document too.
// LaterChangesVersion is a changes request made after the wait began that did not answer it, made
// on that document, whose reason goes in the later_reason block with its author and time.
//
// What is still with the person: the asks (dispatch:// links), and ApprovalRequest, the version of
// the document whose approval request is with the person. What waits on the architect: the asks
// (dispatch:// links), and RequestOpen, that the approval request's thread is open.
//
// The words of the act go in blocks: the answer, the reply, the reason of a changes request and
// the note of a decline, each bounded at OneAct, or at EachOfSeveral each where the task also
// carries a later changes request's reason. Where the gate is open, the task keeps
// DesignApprovedWake word for word.
type RootResumeRecord struct {
	TaskHead
	Act                 string    `json:"act"`
	By                  string    `json:"by,omitempty"`
	At                  time.Time `json:"at,omitzero"`
	Ask                 string    `json:"ask,omitempty"`
	Document            string    `json:"document,omitempty"`
	Version             int       `json:"version,omitempty"`
	GateOffAtAnswer     bool      `json:"gate_off_at_answer,omitempty"`
	ClosedBy            string    `json:"closed_by,omitempty"`
	Options             []string  `json:"options,omitempty"`
	DispatchWords       string    `json:"dispatch_words,omitempty"`
	DocumentVersion     int       `json:"document_version,omitempty"`
	GateOpen            *bool     `json:"gate_open,omitempty"`
	GateArmed           *bool     `json:"gate_armed,omitempty"`
	LaterChangesVersion int       `json:"later_changes_version,omitempty"`
	WithPerson          []string  `json:"with_person,omitempty"`
	ApprovalRequest     int       `json:"approval_request,omitempty"`
	WaitingOnYou        []string  `json:"waiting_on_you,omitempty"`
	RequestOpen         bool      `json:"request_open,omitempty"`
	Answer              *Words    `json:"answer"`
	Reply               *Words    `json:"reply"`
	Reason              *Words    `json:"reason"`
	Note                *Words    `json:"note"`
	LaterReason         *Words    `json:"later_reason"`
}

// Kept is DesignApprovedWake where the design gate is open at the resume.
func (r RootResumeRecord) Kept() []string {
	if r.GateOpen == nil || !*r.GateOpen {
		return nil
	}
	return []string{DesignApprovedWake}
}

// The words of what woke a look of a production check (CheckLookRecord.WokenBy).
const (
	WokenByMerge = "merge"
	WokenByBound = "bound"
	WokenByNamed = "named"
	WokenByRetry = "retry"
)

// The rule of a look's verdict, in the terms of dispatch://INFRA-904's R7, which
// dispatch://INFRA-4533's R1 has every look's task state: a requirement of the issue that only the
// deploy can show and that a look observes as not met is a fail, on every look, the last among them
// (CheckLookRecord.NotMetRequirement); one that is still unobserved at the last look is a fail too,
// which every look states, as main's task said it on every look ("or still unobserved when no look
// follows this one ... is a defect") and as INFRA-4533's R3 holds it for each look before the last
// (CheckLookRecord.UnobservedRequirement); the last look's pending is only for an issue with no
// such requirement (CheckLookRecord.Pending); and a last look that nobody reported gives no verdict,
// and the daemon closes the check as not run, which is no pass (CheckLookRecord.Unreported, R7's
// second sentence).
const (
	LookNotMetRequirement     = "fail"
	LookUnobservedRequirement = "fail"
	LastLookPending           = "only_with_no_deploy_only_requirement"
	LastLookUnreported        = "not_run"
)

// CheckLookRecord is the task of a look of a closed issue's production check (workflow's
// checkTask). Its first line is its lead, kept word for word, since the legion tool's
// description names it; the lead names the issue and its title, so they are no fields.
//
// The fields: the look, what woke it (one of the WokenBy words), the cause of a retried look
// that nobody reported, whether it is the last look, the rule of every look's verdict
// (NotMetRequirement and UnobservedRequirement, the Look words) and, beside it on the last look
// alone, the rest of that look's rule (Pending and Unreported, the LastLook words), the pull
// request, the merge commit (absent where the record holds none), and the check's bound with the
// time it comes due (LastLook, the check's bound_at). The last look starts at that time, or, while
// a look is still open then, a look's bound later each time (workflow's checkBound), so LastLook
// is when the bound comes due rather than when the last look starts. The bound is in whole days
// (BoundDays) where it is whole days, and otherwise as Go writes the duration (BoundDuration,
// such as 36h0m0s, 1h30m0s or 0s), exact to the nanosecond, as main named such a bound (its
// boundText): cut to whole days it would be no bound the check has, and a decimal number of hours
// names most bounds under an hour only nearly. The pending summary that named the wake goes in
// the summary block, by the checking role.
type CheckLookRecord struct {
	Issue                 string    `json:"-"`
	Title                 string    `json:"-"`
	Kind                  Kind      `json:"kind"`
	Look                  int       `json:"look"`
	WokenBy               string    `json:"woken_by"`
	RetryOf               string    `json:"retry_of,omitempty"`
	Last                  bool      `json:"last,omitempty"`
	NotMetRequirement     string    `json:"not_met_requirement,omitempty"`
	UnobservedRequirement string    `json:"unobserved_requirement,omitempty"`
	Pending               string    `json:"pending,omitempty"`
	Unreported            string    `json:"unreported,omitempty"`
	PullRequest           string    `json:"pull_request"`
	MergeCommit           string    `json:"merge_commit,omitempty"`
	BoundDays             int       `json:"bound_days,omitempty"`
	BoundDuration         string    `json:"bound_duration,omitempty"`
	LastLook              time.Time `json:"last_look,omitzero"`
	Summary               *Words    `json:"summary"`
}

// Lead is the first line of the look task: Production check of <KEY>: <title>. The title is a
// person's or a session's, and Dispatch keeps a line break inside a title, so the lead writes each
// line break of it as a space (oneLine): the lead stays one line, and no part of a title starts a
// line of the task, such as a second field or the opening tag of a block.
func (r CheckLookRecord) Lead() string {
	return "Production check of " + r.Issue + ": " + oneLine(r.Title) + "."
}

// oneLine is text with each character that breaks a line written as a space: the line feed, the
// carriage return, the vertical tab, the form feed, and the three that TOON has no escape for
// (unescapedBreak). Text with none is returned as it is.
func oneLine(text string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n', r == '\v', r == '\f', r == '\r', unescapedBreak(r):
			return ' '
		}
		return r
	}, text)
}

// The shapes of a root architect's start task (ArchitectStartRecord.Shape), one for each shape of
// workflow's ArchitectStartTask.
const (
	ShapeFreshTree         = "fresh-tree"
	ShapeCarriedPhase      = "carried-phase"
	ShapeGateStanding      = "gate-standing"
	ShapeGateOpen          = "gate-open"
	ShapeEarlierGeneration = "earlier-generation"
)

// ArchitectStartRecord is the first task of a root architect (workflow's ArchitectStartTask),
// in one of its shapes. The kept decisions that a start names go before it, as a task of their own
// (KeptDecisionsRecord).
//
// The facts and the first act that dispatch://INFRA-2614's R3 gives each shape: the shape, the
// generation the tree is open at, whether the root holds an admission slot, the phase the root
// stands in, whether the hand-over that carried a root in brought its design gate (GateCarried)
// and whether its pull request has merged (Merged), whether the daemon starts the worker of that
// phase, whether a member of the tree runs, the gate step by step, the steps the deployment gates,
// the first act as words (FirstAct, such as claim, write_spec, decompose, request_approval and
// register_gate), whether the architect decomposes the tree, what the root's own first phase waits
// for (such as gate, slot, children and blockers), and whether nothing else starts the tree.
type ArchitectStartRecord struct {
	TaskHead
	Shape              string          `json:"shape"`
	Generation         uint64          `json:"generation"`
	HoldsSlot          bool            `json:"holds_slot,omitempty"`
	Phase              phase.Phase     `json:"phase"`
	GateCarried        *bool           `json:"gate_carried,omitempty"`
	Merged             bool            `json:"merged,omitempty"`
	DaemonStartsWorker *bool           `json:"daemon_starts_worker,omitempty"`
	MembersRun         bool            `json:"members_run,omitempty"`
	Gates              []ArchitectGate `json:"gates,omitempty"`
	Gated              []string        `json:"gated,omitempty"`
	FirstAct           []string        `json:"first_act"`
	Decompose          bool            `json:"decompose"`
	FirstPhaseWaitsFor []string        `json:"first_phase_waits_for,omitempty"`
	NothingElseStarts  bool            `json:"nothing_else_starts_it,omitempty"`
}

// The states of a design-gate step in a start task (ArchitectGate.State).
const (
	GateUnregistered     = "unregistered"
	GateRegistered       = "registered"
	GateApproved         = "approved"
	GateChangesRequested = "changes-requested"
	GateUnanswered       = "unanswered"
)

// ArchitectGate is one step of a root's design gate in a start task: the step, its document as a
// dispatch:// link, or as the bare artifact reference (ArtifactReference) where neither the
// daemon's record nor Dispatch, asked when the daemon queued the task or wrote it again at its
// send, named the issue that holds it, the version registered (0 where the step is not
// registered), and its state (one of the Gate words).
type ArchitectGate struct {
	Step     string `json:"step"`
	Document string `json:"document"`
	Version  int    `json:"version"`
	State    string `json:"state"`
}

// NameStartDocuments is task, the text of an architect-start task as Render writes it
// (workflow's ArchitectStartTask), with the document of each gate step that it gives as an
// artifact reference (ArtifactReference) given instead as the document name answers for that step
// and artifact, where it answers one. It reads the gates rows of the task's own record alone: the
// last record of the text, after the kept decisions a task can carry before it, whose blocks hold
// a person's words. A row it names is written as Render writes a row, so a task whose gates give
// no artifact reference, or one name answers nothing for, comes back as it was.
func NameStartDocuments(task string, name func(step, artifact string) (string, bool)) string {
	if !strings.Contains(task, artifactPath) {
		return task
	}
	lines := strings.Split(task, "\n")
	kind := encodeKey("kind") + ": " + encodeStringLiteral(string(ArchitectStart))
	from := -1
	for i, line := range lines {
		if line == kind {
			from = i
		}
	}
	if from < 0 {
		return task
	}
	for i := from + 1; i < len(lines); i++ {
		fields, count, ok := gatesHeader(lines[i])
		if !ok {
			continue
		}
		step, document := slices.Index(fields, "step"), slices.Index(fields, "document")
		for j := i + 1; step >= 0 && document >= 0 && j <= i+count && j < len(lines); j++ {
			lines[j] = nameRowDocument(lines[j], len(fields), step, document, name)
		}
		i += count
	}
	return strings.Join(lines, "\n")
}

// gatesHeader is the field names and the row count of line where it is the header of a gates
// table as formatHeader writes it (gates[2]{step,document,version,state}:).
func gatesHeader(line string) ([]string, int, bool) {
	rest, found := strings.CutPrefix(line, encodeKey("gates")+"[")
	if !found {
		return nil, 0, false
	}
	count, names, found := strings.Cut(rest, "]{")
	if !found {
		return nil, 0, false
	}
	rows, err := strconv.Atoi(count)
	if err != nil {
		return nil, 0, false
	}
	names, found = strings.CutSuffix(names, "}:")
	if !found {
		return nil, 0, false
	}
	return strings.Split(names, toonDelimiter), rows, true
}

// nameRowDocument is line, one row of a gates table width cells wide, with its document cell (at
// document) named as name answers for its step cell (at step) where that cell is an artifact
// reference written unquoted, as Render writes one whose id needs no quote. Any other row comes
// back as it was.
func nameRowDocument(line string, width, step, document int, name func(step, artifact string) (string, bool)) string {
	row, indented := strings.CutPrefix(line, toonIndent)
	if !indented {
		return line
	}
	cells := strings.Split(row, toonDelimiter)
	if len(cells) != width {
		return line
	}
	artifact, bare := ArtifactOfReference(cells[document])
	if !bare || encodeStringLiteral(ArtifactReference(artifact)) != cells[document] || encodeStringLiteral(cells[step]) != cells[step] {
		return line
	}
	named, ok := name(cells[step], artifact)
	if !ok {
		return line
	}
	cells[document] = encodeStringLiteral(named)
	return toonIndent + strings.Join(cells, toonDelimiter)
}

// KeptDecisionsRecord is the task that names what a person decided on a tree while nobody in it
// could hear it (workflow's KeptDecisionsTask): each decision, and the approval that a
// re-admission kept (the step, its document as a dispatch:// link, the version a person approved
// and the version the document has reached since).
type KeptDecisionsRecord struct {
	TaskHead
	Kept             []KeptDecision `json:"kept,omitempty"`
	ApprovalStep     string         `json:"approval_step,omitempty"`
	ApprovalDocument string         `json:"approval_document,omitempty"`
	Approved         int            `json:"approved,omitempty"`
	Latest           int            `json:"latest,omitempty"`
}

// KeptDecision is one decision a kept-decisions task names: its reference (a dispatch:// link),
// the issue it was made on, what the person did (a record.DecisionKind word), who and when, the
// options the answer chose, joined by a comma and a space, and the words of the act in the answer
// block, bounded at OneAct where the task names one decision and at EachOfSeveral each where it
// names several. The block names its path, such as kept[1].answer.
type KeptDecision struct {
	Ref      string    `json:"ref"`
	Issue    string    `json:"issue"`
	Decision string    `json:"decision"`
	By       string    `json:"by"`
	At       time.Time `json:"at"`
	Options  string    `json:"options"`
	Answer   *Words    `json:"answer"`
}

// PodWakeRecord is the task of a role whose released pod a wake brought back (workflow's
// WakeTask). Wake is the wake's word (pods.WakeChecks and the others), and StoppedBy what stopped
// the session the wake brings back: an idle release, which took the pod, or a project's pause,
// which kept the pod, its workspace and the work not pushed, unless a later release of the pod that
// did not stop the session took the pod (StoppedByRelease, StoppedByPause).
//
// A CI settlement names the pull request, the head, the issue's phase, the checks and the commit
// statuses that did not pass, the checks GitHub skipped (skipped) with the field that says once
// that a skipped check ran nothing on the head (each_skipped_check, CheckConclusion), the count of
// the checks that passed (passed), the count of the commit statuses that passed (passed_statuses,
// absent where none did) and the command that lists them. A message to the role's
// topic names the topic and the facts dispatch://INFRA-2846's R7 requires: how long Envoy keeps a
// role message, the field a kept card shows, that a kept card can come before this task, that a
// kept card whose time ended before the session came back does not reach it (ExpiredDoesNotReach),
// that Envoy tells the sender when it drops one, the setting a message that is not kept is sent
// with, and that its sender sends it again. A message to the session names the session and the
// facts dispatch://INFRA-3360's R3 has the woken role's task give: a message that Envoy or
// Dispatch stored for the session reaches it in this session (StoredReachesSession), and a direct
// Envoy message that Envoy refused reaches it only when its sender sends it again
// (RefusedSenderSendsAgain). The backstop names the role it was armed for. A wake of another word
// gives its detail.
type PodWakeRecord struct {
	TaskHead
	Wake                    string            `json:"wake"`
	PullRequest             string            `json:"pull_request,omitempty"`
	Head                    string            `json:"head,omitempty"`
	Phase                   phase.Phase       `json:"phase,omitempty"`
	NotPassed               []CheckConclusion `json:"not_passed,omitempty"`
	Skipped                 []string          `json:"skipped,omitempty"`
	EachSkippedCheck        string            `json:"each_skipped_check,omitempty"`
	NotPassedStatuses       []StatusState     `json:"not_passed_statuses,omitempty"`
	Passed                  *int              `json:"passed,omitempty"`
	PassedStatuses          int               `json:"passed_statuses,omitempty"`
	ListPassed              string            `json:"list_passed,omitempty"`
	Topic                   string            `json:"topic,omitempty"`
	KeptFor                 string            `json:"kept_for,omitempty"`
	KeptCardField           string            `json:"kept_card_field,omitempty"`
	KeptCardMayComeFirst    bool              `json:"kept_card_may_come_first,omitempty"`
	ExpiredDoesNotReach     bool              `json:"expired_does_not_reach,omitempty"`
	SenderToldOfDrop        bool              `json:"sender_told_of_drop,omitempty"`
	NotKeptWith             string            `json:"not_kept_with,omitempty"`
	SenderSendsAgain        bool              `json:"sender_sends_again,omitempty"`
	Session                 string            `json:"session,omitempty"`
	StoredReachesSession    bool              `json:"stored_reaches_session,omitempty"`
	RefusedSenderSendsAgain bool              `json:"refused_sender_sends_again,omitempty"`
	ArmedFor                string            `json:"armed_for,omitempty"`
	Detail                  string            `json:"detail,omitempty"`
	StoppedBy               string            `json:"stopped_by"`
}

// The words of what stopped the session a wake brings back (PodWakeRecord.StoppedBy).
const (
	StoppedByRelease = "release"
	StoppedByPause   = "pause"
)

// ControllerStartRecord is the controller's start message. It has no field: its text is
// ControllerStartMessage, word for word.
type ControllerStartRecord struct{}

// Kept is ControllerStartMessage.
func (ControllerStartRecord) Kept() []string {
	return []string{ControllerStartMessage}
}

// The audiences of news of an approved design (DesignNewsRecord.Audience): the session that holds
// the issue's phase, told what changed for that issue, and the tree root's architect, told of the
// whole tree in one summary.
const (
	NewsToHolder        = "holder"
	NewsToRootArchitect = "root_architect"
)

// The words of what one item of news says changed (DesignNewsItem.Changes).
const (
	// NewsChangesListed is a pair whose changed sections and blocks the rows of sections and blocks
	// that name the item list.
	NewsChangesListed = "listed"
	// NewsChangesNoneNamed is a pair Dispatch read and named no changed section or block of.
	NewsChangesNoneNamed = "none_named"
	// NewsChangesUnavailable is a pair Dispatch could not answer for, so nothing names what
	// changed; the item's reason block says why.
	NewsChangesUnavailable = "unavailable"
	// NewsChangesSameText is a new version that holds the same text as the version taken.
	NewsChangesSameText = "same_text"
	// NewsChangesDecisionBlocksOnly is a new version that differs from the version taken only in
	// decision blocks.
	NewsChangesDecisionBlocksOnly = "decision_blocks_only"
	// NewsChangesNoPair is an item with no earlier version to compare with: none was approved
	// before it (taken 0), or the version taken could not be read (taken null, its reason block
	// saying why).
	NewsChangesNoPair = "no_pair"
)

// The units of a document that a section row of news names (DesignNewsSection.Unit): a `##`
// section, or a run of the text outside every section, by where it stands: before the first
// section, after the `##` section directly before it, or nowhere Dispatch names.
const (
	NewsUnitSection            = "section"
	NewsUnitBeforeFirstSection = "before_first_section"
	NewsUnitAfterSection       = "after_section"
	NewsUnitOutsideSections    = "outside_sections"
)

// NewsBlockOther is the type a block row of news gives a block whose type the daemon does not
// name (DesignNewsBlock.Type), whose type as the document gives it goes in its other_type block.
const NewsBlockOther = "other"

// What a member of a tree is told of an approved version, in its root architect's summary
// (DesignNewsMember.Told): a task tells it now; the news waits until its holder takes the task it
// already holds; the next start of its phase carries it; the root architect answers it itself;
// nothing; or nothing yet.
const (
	NewsToldNow           = "now"
	NewsToldWhenTaskTaken = "when_task_taken"
	NewsToldAtNextStart   = "at_next_start"
	NewsToldByYou         = "by_you"
	NewsToldNothing       = "nothing"
	NewsToldNothingYet    = "not_yet"
)

// Why a member of a tree is told nothing, or nothing yet (DesignNewsMember.Why): no version it is
// owed changes its work; only the root's checks changed; its phase already took the approved
// version; it has not started; it waits on a person; it stands in held, whatever holds it (its
// stopped task, the issues beneath it or its failed worker), and the release that moves it back
// into a phase carries what it is owed then; its work is done; its status takes it out of the
// workflow; its pull request has merged; or the gate closed again before the summary was sent.
const (
	NewsWhyWorkUnchanged  = "work_unchanged"
	NewsWhyRootChecksOnly = "root_checks_only"
	NewsWhyAlreadyTaken   = "already_taken"
	NewsWhyNotStarted     = "not_started"
	NewsWhyWaitsOnPerson  = "waits_on_person"
	NewsWhyStopped        = "stopped"
	NewsWhyDone           = "done"
	NewsWhyOutOfWorkflow  = "out_of_workflow"
	NewsWhyMerged         = "merged"
	NewsWhyGateClosed     = "gate_closed"
)

// DesignNewsRecord is the news of an approved design that a task carries to one audience
// (workflow's RenderNews): a task of its own, or the record after a start's own record, joined to
// it by Joined. A task can carry several, one for each audience it speaks
// to. dispatch://INFRA-175's R2, R3 and R5 have it name the changed sections and checks of each
// document, and, for the root's architect, each issue of the tree with what changed for it; the
// readers' prompts give the answers its Design 7 names.
//
// The issue it speaks of and its tree, its audience (a NewsTo word) and, for the holder, the
// phase the reader holds the issue in. Where the gate's opening is owed to the audience, the gate
// as it stands at the send, with the fields a gate-moved message gives (GateMovedRecord): open or
// closed (Gate, a Gate word), the step, the step's document as a dispatch:// link, the artifact,
// the version, and what the gate does to a merge (Merge, a Merge word); none where it is not owed.
//
// Each item is one step's approved version (Items), each section and each block that changed is a
// row naming the index of its item in Items (Sections, Blocks), and, in the root architect's
// summary, each member of the tree is a row (Members) and each item a member is owed a row naming
// the member and the index of its item (Owed). Dispatch's reason, a section's heading, a block's id
// and a type the daemon does not name are another author's words, each in a block of its row.
type DesignNewsRecord struct {
	TaskHead
	Tree         string              `json:"tree"`
	Audience     string              `json:"audience"`
	Phase        string              `json:"phase,omitempty"`
	Gate         string              `json:"gate,omitempty"`
	GateStep     string              `json:"gate_step,omitempty"`
	GateDocument string              `json:"gate_document,omitempty"`
	GateArtifact string              `json:"gate_artifact,omitempty"`
	GateVersion  int                 `json:"gate_version,omitempty"`
	Merge        string              `json:"merge,omitempty"`
	Items        []DesignNewsItem    `json:"items,omitempty"`
	Sections     []DesignNewsSection `json:"sections,omitempty"`
	Blocks       []DesignNewsBlock   `json:"blocks,omitempty"`
	Members      []DesignNewsMember  `json:"members,omitempty"`
	Owed         []DesignNewsOwed    `json:"owed,omitempty"`
}

// DesignNewsItem is one step's approved version as news names it: the step, its document as a
// dispatch:// link (or the bare artifact reference, ArtifactReference, where no issue that holds
// it is named: the gate row of the step holds another document, or neither the daemon's record
// nor Dispatch names the issue of the row's document, workflow.GateDocuments), the artifact, the
// version a person approved now, and the version the audience took (Taken): 0 where no version
// was approved before it, and null where the review history could not say which version the
// audience took, its reason in the reason block. In the
// root architect's summary, Yours says whether that version is the one the architect took, and
// not the one each issue owed the item took. Changes is what the item says changed (a
// NewsChanges word).
type DesignNewsItem struct {
	Step     string `json:"step"`
	Document string `json:"document"`
	Artifact string `json:"artifact"`
	Version  int    `json:"version"`
	Taken    *int   `json:"taken"`
	Yours    *bool  `json:"yours,omitempty"`
	Changes  string `json:"changes"`
	Reason   *Words `json:"reason"`
}

// DesignNewsSection is one top-level unit of a document that changed, as Dispatch names it: the
// index of its item in Items, the unit (a NewsUnit word), its place among the sections of its
// heading counted from 1 (Nth, 0 for a run outside every section), how it changed (Dispatch's
// word: changed, added, removed or moved), and, for a run after a section, that section's place
// among the sections of its heading (AfterNth, 0 otherwise). The section's heading, and the
// heading of the section a run follows, go in blocks; an empty heading draws none.
type DesignNewsSection struct {
	Item         int    `json:"item"`
	Unit         string `json:"unit"`
	Nth          int    `json:"nth"`
	Change       string `json:"change"`
	AfterNth     int    `json:"after_nth"`
	Heading      *Words `json:"heading"`
	AfterHeading *Words `json:"after_heading"`
}

// DesignNewsBlock is one typed block of a document that changed: the index of its item in Items,
// its type (requirement, check, plan_task, callout, or NewsBlockOther), and how it changed
// (Dispatch's word). Its id goes in the id block, and a type the daemon does not name in the
// other_type block.
type DesignNewsBlock struct {
	Item      int    `json:"item"`
	Type      string `json:"type"`
	Change    string `json:"change"`
	ID        *Words `json:"id"`
	OtherType *Words `json:"other_type"`
}

// DesignNewsMember is one member of a tree in its root architect's summary: its key, its phase,
// how it is told (a NewsTold word) and, where it is told nothing or nothing yet, why (a NewsWhy
// word, empty otherwise), the role that holds its phase where a task tells it (empty for a member
// told nothing, and where its phase names no role), whether the task that tells it gives it the
// gate's opening (Opening: its holder row owes the opening while the standing gate is open), the
// count of the items it is owed that the daemon cannot name yet, whose change sets are not
// recorded, which no row of Owed names (Unnamed), and, for a member in accepting that an
// architect other than the tree root's holds, the issue whose architect that is (ArchitectOf: the
// member itself, or an issue above it; empty otherwise). A member told by a task is owed the
// opening, an item Owed names, an item Unnamed counts, or more than one of them; a member told
// nothing has no Opening and no Unnamed.
type DesignNewsMember struct {
	Issue       string `json:"issue"`
	Phase       string `json:"phase"`
	Told        string `json:"told"`
	Why         string `json:"why"`
	Holder      string `json:"holder"`
	Opening     bool   `json:"opening"`
	Unnamed     int    `json:"unnamed"`
	ArchitectOf string `json:"architect_of"`
}

// DesignNewsOwed is one item a member of a tree is owed, in its root architect's summary: the
// member's key and the index of the item in Items.
type DesignNewsOwed struct {
	Issue string `json:"issue"`
	Item  int    `json:"item"`
}
