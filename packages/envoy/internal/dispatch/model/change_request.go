package model

import "time"

// The read of an issue's change request and the end of it (D6, D10 of the work-units plan):
// GET /api/v1/issues/{key}/change-request answers ChangeRequestRead, GET
// /api/v1/issues/{key}/change-request/ending answers ChangeRequestEnding, and POST and GET
// /api/v1/issues/{key}/outcomes write and read SpecOutcomeRecord.

// SpecOutcome is one outcome of a change in its JSON form: its text, and the approved statement
// that requires it or that it is restored for the conclusion's statement, never both. Its line
// form is `(requires <statement>) <text>`, `(restored) <text>` or `<text>`.
type SpecOutcome struct {
	Text      string `json:"text"`
	Statement string `json:"statement,omitempty"`
	Restored  bool   `json:"restored,omitempty"`
}

// SpecOutcomeRecord is one list of outcomes recorded for an issue: the pull request and the head
// it is of, whose list it is (the author's, the reviewer's own, or the reviewer's final list), its
// outcomes, the actor that recorded it and when. Each record is a row of its own; the newest final
// record at a head is that head's final list.
type SpecOutcomeRecord struct {
	ID          int64         `json:"id"`
	Issue       string        `json:"issue"`
	PullRequest string        `json:"pull_request"`
	Head        string        `json:"head"`
	List        string        `json:"list" enum:"author,reviewer,final"`
	Outcomes    []SpecOutcome `json:"outcomes"`
	Actor       Actor         `json:"actor"`
	RecordedAt  time.Time     `json:"recorded_at"`
}

// SpecOutcomesRecordedEventPayload is the payload of spec.outcomes_recorded, appended on the issue
// a list of outcomes was recorded for: the record, its pull request, head and list, and how many
// outcomes it holds. A reader fetches the record for the outcomes.
type SpecOutcomesRecordedEventPayload struct {
	Issue       string `json:"issue"`
	Record      int64  `json:"record"`
	PullRequest string `json:"pull_request"`
	Head        string `json:"head"`
	List        string `json:"list"`
	Outcomes    int    `json:"outcomes"`
}

// ChangeRequestNode is one node a change request proposes, or one check it changes: its id, its
// kind, how it stands to the approved spec, the approved version its writer read (0 for none), the
// approved spec's current version of it (0 where it holds none), the block it is read from,
// `<document>#<block>`, and whether the approved spec already holds it as the block writes it
// (Approved): a stale node whose current approved version a person's approval wrote with this
// very text, such as the issue's own request once a person approved it. Every other node waits for
// a person's approval.
type ChangeRequestNode struct {
	ID       string `json:"id"`
	Kind     string `json:"kind" enum:"statement,component,interface,check"`
	What     string `json:"what" enum:"added,changed,removed,stale"`
	Base     int    `json:"base"`
	Current  int    `json:"current"`
	Block    string `json:"block"`
	Approved bool   `json:"approved"`
}

// ChangeRequestProblem is one fault of a block of a change request: the block, the rule it breaks
// and one sentence on it.
type ChangeRequestProblem struct {
	Block  string `json:"block"`
	Rule   string `json:"rule"`
	Detail string `json:"detail"`
}

// ConclusionAnswer is a person's answer to a conclusion: a comment of the issue anchored to the
// conclusion's block, with its author, body and time.
type ConclusionAnswer struct {
	Comment   string    `json:"comment"`
	Author    Actor     `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// ChangeRequestConclusion is the conclusion block of a version of an issue's spec: its stored
// block id, the approved statement it concludes about and the kind of difference it found (empty
// where it names none), whether it names no statement, its evidence, its outcomes, and each
// answer anchored to it, oldest first.
type ChangeRequestConclusion struct {
	Block       string             `json:"block"`
	Statement   string             `json:"statement"`
	Difference  string             `json:"difference"`
	NoStatement bool               `json:"no_statement"`
	Evidence    []string           `json:"evidence"`
	Outcomes    []SpecOutcome      `json:"outcomes"`
	Answers     []ConclusionAnswer `json:"answers"`
}

// ChangeRequestJudgmentReason is one reason of a conclusion's judgment: its code, one sentence on
// it, and the outcome it names, for omits_outcome and uncovered_outcome alone.
type ChangeRequestJudgmentReason struct {
	Code    string `json:"code" enum:"no_statement_or_kind,statement_and_no_statement,statement_not_approved,no_evidence,omits_outcome,two_conclusions,uncovered_outcome"`
	Detail  string `json:"detail"`
	Outcome string `json:"outcome,omitempty"`
}

// SpecFinalList names the reviewer's final list a judgment read: the record, its pull request and
// head, and when it was recorded.
type SpecFinalList struct {
	ID          int64     `json:"id"`
	PullRequest string    `json:"pull_request"`
	Head        string    `json:"head"`
	RecordedAt  time.Time `json:"recorded_at"`
}

// ChangeRequestRead is the change request of an issue at a version of its spec: whether its
// repository is a spec source of its project, what it proposes against that repository's approved
// spec (statements, components and interfaces) and which approved checks its Verification
// document changes, whether a proposal or a changed check is one the approved spec does not hold as
// written (ProposesUnapproved, ChangesChecksUnapproved: no person's approval wrote it), each fault
// of its blocks, its conclusion, and the judgment of that conclusion against the final list it
// read. ApprovedSpecMark is the approved spec it was read against (specstore.State.Mark, 0 for an
// issue of no spec source): with Version and VerificationVersion it names every input of the
// comparison, since another issue's approval moves the approved spec and leaves this issue's
// documents as they are. A read of a lower mark is of an older approved spec.
type ChangeRequestRead struct {
	Issue                   string                        `json:"issue"`
	Version                 int                           `json:"version"`
	SpecSource              bool                          `json:"spec_source"`
	Repo                    string                        `json:"repo"`
	VerificationVersion     int                           `json:"verification_version"`
	ApprovedSpecMark        int64                         `json:"approved_spec_mark"`
	Proposes                bool                          `json:"proposes"`
	ChangesChecks           bool                          `json:"changes_checks"`
	ProposesUnapproved      bool                          `json:"proposes_unapproved"`
	ChangesChecksUnapproved bool                          `json:"changes_checks_unapproved"`
	Proposals               []ChangeRequestNode           `json:"proposals"`
	ChangedChecks           []ChangeRequestNode           `json:"changed_checks"`
	Problems                []ChangeRequestProblem        `json:"problems"`
	Conclusion              *ChangeRequestConclusion      `json:"conclusion"`
	Judgment                string                        `json:"judgment" enum:"none,incomplete,uncovered,complete"`
	JudgmentReasons         []ChangeRequestJudgmentReason `json:"judgment_reasons"`
	FinalList               *SpecFinalList                `json:"final_list"`
}

// ChangeRequestEndingReason is one reason the change of an issue may not land at a head: its code
// and one sentence on what the owner does.
type ChangeRequestEndingReason struct {
	Code   string `json:"code" enum:"no_final_list_for_head,final_list_of_another_head,no_conclusion,incomplete,uncovered_without_approval,proposes_without_approval,changes_checks_without_approval"`
	Detail string `json:"detail"`
}

// ChangeRequestEnding is whether the change of an issue may land at head (D10): ready exactly when
// no reason holds it, with the judgment of the latest spec's conclusion against the final list at
// head, and whether a person's approval of the latest spec version stands.
type ChangeRequestEnding struct {
	Issue           string                        `json:"issue"`
	Head            string                        `json:"head"`
	Version         int                           `json:"version"`
	State           string                        `json:"state" enum:"ready,waiting"`
	Reasons         []ChangeRequestEndingReason   `json:"reasons"`
	Judgment        string                        `json:"judgment" enum:"none,incomplete,uncovered,complete"`
	JudgmentReasons []ChangeRequestJudgmentReason `json:"judgment_reasons"`
	Approved        bool                          `json:"approved"`
	FinalList       *SpecFinalList                `json:"final_list"`
}
