// Package testdata holds the outbox payloads of the release before the one that starts a fix beside
// the other steps of a task, frozen, so a test of the store can read a queued row as that release
// reads it after a rollback.
//
// Each type is a copy of internal/record's type of the same name at commit
// f98546a6a247e0fab8cdcc291947d7d07d432541, the last commit of that release: SuperviseRequest,
// StepRecord with the PlanRating it embeds, Notice with each type it nests, UpdateAsk, the notice
// kinds of validNoticeKinds and of controllerOnlyNoticeKind, and the supervise ops of
// validSuperviseOp. The copy keeps each member's JSON name and Go type and drops the comments. It
// never follows record: a member record gains is a member this copy refuses, which is the release
// before's refusal.
package testdata

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"
)

// DecodeBeforeFix reads payload, the payload of a queued outbox row of kind, as the release before
// reads it (record's DecodeOutboxPayload at the commit above): strictly, refusing a member the
// payload's type does not have (DisallowUnknownFields) and trailing JSON, and refusing a notice
// kind, a controller notice kind and a supervise op that release does not know. It answers nil
// where that release takes the row. It decodes the supervise, step_record, notice,
// controller_notice and update_ask rows; a kind the release before does not know is refused as that release
// refuses it, and a kind it knows but this copy does not freeze is an error that says so, so a
// test that queues one learns it reads nothing.
func DecodeBeforeFix(kind string, payload []byte) error {
	switch kind {
	case "supervise":
		var value superviseRequest
		if err := strict(payload, &value); err != nil {
			return err
		}
		if !slices.Contains(superviseOps, value.Op) {
			return fmt.Errorf("unknown supervise operation %q", value.Op)
		}
	case "step_record":
		var value stepRecord
		return strict(payload, &value)
	case "notice":
		var value notice
		if err := strict(payload, &value); err != nil {
			return err
		}
		if !slices.Contains(noticeKinds, value.Kind) {
			return fmt.Errorf("unknown notice kind %q", value.Kind)
		}
	case "controller_notice":
		var value notice
		if err := strict(payload, &value); err != nil {
			return err
		}
		if !slices.Contains(noticeKinds, value.Kind) && !slices.Contains(controllerOnlyNoticeKinds, value.Kind) {
			return fmt.Errorf("unknown controller notice kind %q", value.Kind)
		}
	case "update_ask":
		var value updateAsk
		return strict(payload, &value)
	default:
		if slices.Contains(outboxKinds, kind) {
			return fmt.Errorf("this copy freezes no payload of kind %q, which the release before reads", kind)
		}
		return fmt.Errorf("unknown kind %q", kind)
	}
	return nil
}

// strict decodes payload into value as the release before's decodeOutboxJSON does.
func strict(payload []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("payload has trailing JSON")
		}
		return err
	}
	return nil
}

// outboxKinds is every kind the release before writes or reads: the list of its last
// outbox_kind_check, 20261119203047_design_news.
var outboxKinds = []string{
	"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
	"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
	"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
	"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "update_ask",
	"merge_queue_read", "merge_queue_removal", "role_holders_publish", "step_record", "plan_record",
	"conflict_confirm", "merge_forward", "design_changes",
}

// superviseOps is the release before's validSuperviseOp.
var superviseOps = []string{"start", "suspend", "release", "step_stop", "tree_close", "issue_close", "pause_stop"}

// noticeKinds is the release before's validNoticeKinds.
var noticeKinds = []string{
	"phase-finished", "worker-died", "worker-stalled", "worker-silent", "worker-background-stalled", "held",
	"pr-blocked", "pr-merged", "pr-closed-unmerged", "pull-request-missing", "message-refused",
	"design-approved", "design-changes-requested", "carried-gate-opened",
	"child-closed", "child-status", "children-first", "child-release-stalled", "child-not-recorded",
	"catch-up", "checks-red", "review-stuck", "status-reasserted", "volume-near-full",
	"session-lost", "session-forgotten", "session-store-fault", "task-stopped", "plan-not-taken", "retro-held",
}

// controllerOnlyNoticeKinds is the release before's controllerOnlyNoticeKind.
var controllerOnlyNoticeKinds = []string{
	"triage", "slot-free", "todo", "tick", "unslotted", "awaiting-person", "parked-decision",
	"update-stalled", "provider-errors", "tree-stalled", "architect-stalled", "memory-guard-kill",
	"project-paused", "project-resumed",
}

type updateAsk struct {
	Role  string `json:"role"`
	Round string `json:"round"`
	Pod   string `json:"pod,omitempty"`
	Text  string `json:"text"`
}

type superviseRequest struct {
	Op            string         `json:"op"`
	Tree          string         `json:"tree"`
	Role          string         `json:"role"`
	Task          string         `json:"task,omitempty"`
	Kept          *keptDecisions `json:"kept,omitempty"`
	Generation    uint64         `json:"generation"`
	Phase         string         `json:"phase,omitempty"`
	Entry         int64          `json:"entry,omitempty"`
	News          bool           `json:"news,omitempty"`
	ResendEntry   bool           `json:"resendEntry,omitempty"`
	ResumeTask    bool           `json:"resumeTask,omitempty"`
	RoundNamed    bool           `json:"roundNamed,omitempty"`
	RuleResume    bool           `json:"ruleResume,omitempty"`
	NeedsSlot     bool           `json:"needsSlot,omitempty"`
	Relaunch      bool           `json:"relaunch,omitempty"`
	Assignment    string         `json:"assignment,omitempty"`
	FreshSession  bool           `json:"freshSession,omitempty"`
	Linger        uint64         `json:"linger,omitempty"`
	Reason        string         `json:"reason,omitempty"`
	CheckLook     int            `json:"checkLook,omitempty"`
	CheckOpenedAt time.Time      `json:"checkOpenedAt,omitzero"`
	Pause         int64          `json:"pause,omitempty"`
	Issue         string         `json:"issue,omitempty"`
	WakeKind      string         `json:"wakeKind,omitempty"`
}

type keptDecisions struct {
	Decisions []decisionMark `json:"decisions,omitempty"`
	keptApproval
}

type decisionMark struct {
	Ref      string    `json:"ref"`
	At       time.Time `json:"at"`
	Issue    string    `json:"issue,omitempty"`
	Decision string    `json:"decision,omitempty"`
	Actor    string    `json:"actor,omitempty"`
}

type keptApproval struct {
	Step     string `json:"step,omitempty"`
	Approved int    `json:"approved,omitempty"`
	Latest   int    `json:"latest,omitempty"`
}

type stepRecord struct {
	ID         string     `json:"id"`
	Task       string     `json:"task"`
	Step       int        `json:"step"`
	Kind       string     `json:"kind"`
	Role       string     `json:"role"`
	Type       string     `json:"type"`
	Agent      string     `json:"agent"`
	Generation uint64     `json:"generation"`
	Session    *string    `json:"session,omitempty"`
	Model      *string    `json:"model,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	Result     *string    `json:"result,omitempty"`
	ReturnedTo *int       `json:"returned_to,omitempty"`
	Report     *string    `json:"report,omitempty"`
	*planRating
}

type planRating struct {
	Rating int    `json:"rating"`
	Reason string `json:"rating_reason"`
}

// notice is the release before's Notice as its outbox decodes a queued one (its releasedNotice):
// with the key verdict, which it reads and drops.
type notice struct {
	Kind             string          `json:"kind"`
	Role             string          `json:"role,omitempty"`
	Phase            string          `json:"phase,omitempty"`
	Summary          string          `json:"summary,omitempty"`
	Version          int             `json:"version,omitempty"`
	Reason           string          `json:"reason,omitempty"`
	Topic            string          `json:"topic,omitempty"`
	Failure          *noticeFailure  `json:"failure,omitempty"`
	Escalation       string          `json:"escalation,omitempty"`
	StoppedBy        string          `json:"stoppedBy,omitempty"`
	Resends          int             `json:"resends,omitempty"`
	PacedAt          time.Time       `json:"paced_at,omitzero"`
	PacedFrom        time.Time       `json:"paced_from,omitzero"`
	ResendOf         int64           `json:"resend_of,omitempty"`
	CatchUp          *catchUp        `json:"catch_up,omitempty"`
	OpenCapabilities []string        `json:"openCapabilities,omitempty"`
	Blocked          []blockedIssue  `json:"blocked,omitempty"`
	Held             []heldIssue     `json:"held,omitempty"`
	Silent           []silentIssue   `json:"silent,omitempty"`
	Actor            string          `json:"actor,omitempty"`
	Ref              string          `json:"ref,omitempty"`
	Decision         string          `json:"decision,omitempty"`
	Root             string          `json:"root,omitempty"`
	Update           *updateStalled  `json:"update,omitempty"`
	Pause            *projectPause   `json:"pause,omitempty"`
	Volume           *volumeNearFull `json:"volume,omitempty"`
	Verdict          json.RawMessage `json:"verdict"`
}

type noticeFailure struct {
	Cause          string        `json:"cause"`
	Detail         string        `json:"detail,omitempty"`
	LaunchFailures failureBudget `json:"launchFailures"`
	Deaths         failureBudget `json:"deaths"`
	PromptFailures failureBudget `json:"promptFailures"`
	PromptRetires  failureBudget `json:"promptRetires"`
	Death          string        `json:"death,omitempty"`
	StartFailure   string        `json:"startFailure,omitempty"`
}

type failureBudget struct {
	Count int `json:"count"`
	Limit int `json:"limit"`
}

type catchUp struct {
	Generation uint64         `json:"generation"`
	Launch     uint64         `json:"launch"`
	HandedOver bool           `json:"handed_over,omitempty"`
	Carried    bool           `json:"carried,omitempty"`
	Gates      []catchUpGate  `json:"gates"`
	Issues     []catchUpIssue `json:"issues"`
	Decided    string         `json:"decided,omitempty"`
	Kept       *keptDecisions `json:"kept,omitempty"`
}

type catchUpGate struct {
	Step         string `json:"step"`
	Gated        bool   `json:"gated"`
	Unregistered bool   `json:"unregistered,omitempty"`
	Artifact     string `json:"artifact,omitempty"`
	Version      int    `json:"version,omitempty"`
	Approved     int    `json:"approved,omitempty"`
	Open         bool   `json:"open"`
}

type catchUpIssue struct {
	Key    string `json:"key"`
	Parent string `json:"parent,omitempty"`
	Title  string `json:"title"`
	Phase  string `json:"phase"`
	Status string `json:"status"`
}

type blockedIssue struct {
	Issue     string   `json:"issue"`
	BlockedBy []string `json:"blockedBy"`
}

type heldIssue struct {
	Issue  string   `json:"issue"`
	Reason string   `json:"reason"`
	HeldBy []string `json:"heldBy,omitempty"`
}

type silentIssue struct {
	Issue           string    `json:"issue"`
	Role            string    `json:"role"`
	LastStep        time.Time `json:"lastStep"`
	Silent          string    `json:"silent"`
	BackgroundSince time.Time `json:"backgroundSince,omitzero"`
}

type updateStalled struct {
	Round   string          `json:"round"`
	Pod     string          `json:"pod,omitempty"`
	Reasons string          `json:"reasons"`
	Waiting []updateWaiting `json:"waiting"`
}

type updateWaiting struct {
	Claim    string `json:"claim"`
	Role     string `json:"role,omitempty"`
	State    string `json:"state"`
	Answered bool   `json:"answered,omitempty"`
}

type projectPause struct {
	ID     int64      `json:"id"`
	By     pauseActor `json:"by"`
	At     time.Time  `json:"at"`
	Reason string     `json:"reason,omitempty"`
}

type pauseActor struct {
	Kind    string `json:"kind"`
	Session string `json:"session,omitempty"`
}

type volumeNearFull struct {
	PodUID              string          `json:"podUid"`
	Trimmer             string          `json:"trimmer"`
	Episode             int             `json:"episode"`
	RaisedBy            string          `json:"raisedBy"`
	UsedBytes           int64           `json:"usedBytes"`
	FreeBytes           int64           `json:"freeBytes"`
	FreedBytes          int64           `json:"freedBytes"`
	StopReason          string          `json:"stopReason"`
	RefusedReads        []string        `json:"refusedReads,omitempty"`
	RefusedReadsOmitted int             `json:"refusedReadsOmitted,omitempty"`
	Sizes               []volumeSize    `json:"sizes"`
	SizesOmitted        int             `json:"sizesOmitted,omitempty"`
	SizesAt             time.Time       `json:"sizesAt"`
	SizesPartial        bool            `json:"sizesPartial,omitempty"`
	SizesMissing        []volumeMissing `json:"sizesMissing,omitempty"`
	SizesMissingOmitted int             `json:"sizesMissingOmitted,omitempty"`
	Faults              []string        `json:"faults,omitempty"`
	FaultsOmitted       int             `json:"faultsOmitted,omitempty"`
	SwitchedOff         bool            `json:"switchedOff,omitempty"`
}

type volumeSize struct {
	Path          string `json:"path"`
	Bytes         int64  `json:"bytes"`
	BytesOverHour int64  `json:"bytesOverHour"`
	BytesOverDay  int64  `json:"bytesOverDay"`
	Refused       string `json:"refused,omitempty"`
	Refusals      int    `json:"refusals,omitempty"`
}

type volumeMissing struct {
	Path   string `json:"path"`
	Reason string `json:"reason,omitempty"`
}
