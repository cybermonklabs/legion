package record

import (
	"encoding/json"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// CloseFacts is what an issue's close is judged on, read in one transaction (workflow's
// closeWait). It holds the issue's own repository and its newest merge, the moment of its newest
// entry into verifying, what still runs beneath and under it, the newest read of Dispatch's
// states of its checks, its production check, its open defects and whether a next change of it
// is open. CloseWaitOf judges them, and nothing else is read: a role's statement, a sign-off or
// a handoff summary is none of them.
type CloseFacts struct {
	// Repo is the issue's own repository as Dispatch resolves it (Issue.Repo): the repository of
	// every basis of a read that names no repository of its own.
	Repo string
	// Merge is the issue's newest merge, from its own row (Issue.Merge); zero before its first
	// merge. A read decides a check only where it holds this merge.
	Merge Merge
	// Entered is the issue's newest entry into verifying, the stored wait's `entered`: a read
	// counts only where it began after it. Zero before the issue's first entry.
	Entered time.Time
	// Beneath is each recorded issue beneath this one that is still in the workflow (OpenBeneath).
	Beneath []string
	// OpenChildren is each child the read's Dispatch issue lists (GET /api/v1/issues/{key}) that
	// ChildOpen reads as open and the record does not hold in the workflow.
	OpenChildren []string
	// Read is the issue's newest read of Dispatch's states, nil before its first read.
	Read *CheckRead
	// Generation is the issue's generation, which the production term's check must carry.
	Generation uint64
	// Production is the issue's production check of any generation, nil where none opened.
	Production *ProductionCheck
	// ProductionOn says that the issue's project runs a production check
	// (Config.ProductionCheckRole != "").
	ProductionOn bool
	// Defects is the issue's open reasons of a next change: unwithdrawn rows that no merged
	// change carried.
	Defects []Defect
	// OpenChange says that a next change of the issue has started and has neither merged nor
	// ended.
	OpenChange bool
}

// CloseWait is what an issue's close waits for. Done is true only where nothing is listed and
// nothing is unread.
type CloseWait struct {
	// Beneath and OpenChildren are CloseFacts' own: each issue beneath, and each open child, that
	// holds the close.
	Beneath, OpenChildren []string
	// Checks is each check whose state at the newest merge is not a pass, with its state and
	// Dispatch's reason, in the order of the Verification document.
	Checks []CheckWait
	// Unapproved says that a check was removed or reworded since the Verification document's
	// approved version. A block that only gains subject, program or both, where the approved
	// block held neither, is not reworded. Any other change of them is.
	Unapproved bool
	// Unread says that the close has no read it can decide on: no read, a failed read, a read
	// before the newest entry into verifying, or a read that names another merge for a check.
	Unread bool
	// Production is what the production term waits for, read only from a check of the issue's
	// generation at its newest merge: "" for nothing, or one of the Production values.
	Production string
	// Defects is the id of each reported defect, or request, that holds the close.
	Defects []string
	// OpenChange says that an open next change of the issue holds the close.
	OpenChange bool
}

// The values of CloseWait.Production. ProductionAwaitingChange is an open next change with no
// production check of its generation yet: no earlier generation's outcome shows as a current
// failure. ProductionOpening is no check of the issue's generation: the next fresh read with
// nothing beneath opens one. ProductionOpen, ProductionNotRun and ProductionDefect are the check of
// the issue's generation at its newest merge, open, closed with no look that could see the change,
// and closed with a fail. ProductionEarlierMerge is a check of the issue's generation opened for
// an earlier merge than its newest.
const (
	ProductionAwaitingChange = "awaiting_change"
	ProductionOpening        = "opening"
	ProductionOpen           = "open"
	ProductionNotRun         = "not_run"
	ProductionDefect         = "defect"
	ProductionEarlierMerge   = "earlier_merge"
)

// CheckState is what the close makes of Dispatch's answer for one check (CloseCheckOf).
type CheckState string

const (
	// CheckStatePass is a pass that stands: Dispatch answers yes.
	CheckStatePass CheckState = "pass"
	// CheckStateFail is Dispatch's computed fail (no, for the reason fail): the one state that
	// starts the next change.
	CheckStateFail CheckState = "fail"
	// CheckStateNoResult is a check no result decides yet (no, for the reason no_result): it waits
	// for evidence.
	CheckStateNoResult CheckState = "no_result"
	// CheckStateUnknown is every other answer: a pass and a fail at one commit, a result Dispatch
	// could not place, a read Dispatch could not make, and an answer the close does not know.
	CheckStateUnknown CheckState = "unknown"
)

// CheckWait is one check of the Verification document as the close reads it: its block id, its
// state, and Dispatch's answer and reason as Dispatch gave them. Result is a fail's deciding
// result (Dispatch's results[0]) and Commit its commit, both "" for every other state. An unknown
// state whose Reason is none of Dispatch's reasons of an unknown answer is one the caller logs,
// with the check and the answer.
type CheckWait struct {
	Check  string
	State  CheckState
	Answer string
	Reason string
	Result string
	Commit string
}

// CheckReadAnswer is what one read of Dispatch's states for an issue's close answered, the layout
// CheckRead.Answer keeps it in. Results is GET /api/v1/issues/{key}/results with no query.
// AtMergeHead is the same route asked at the newest merge's final head (?at=<merge_head>), made
// only where another pull request of the issue is open and no subject of Results names that
// head: the close takes only its before_merge checks' answers. Children is each child of the
// issue as GET /api/v1/issues/{key} lists it. Approval is the Verification document's newest
// approval and what it approved, nil where the issue has no Verification document. Every field
// is in Dispatch's own names (the dispatch package's types), so a stored read decodes as the
// read did.
type CheckReadAnswer struct {
	Results     dispatch.IssueResults  `json:"results"`
	AtMergeHead *dispatch.IssueResults `json:"at_merge_head,omitempty"`
	Children    []dispatch.IssueChild  `json:"children,omitempty"`
	Approval    *CheckApproval         `json:"approval,omitempty"`
}

// CheckApproval is the Verification document's newest approval as the close read takes it: the
// version a person approved, 0 for none, and the blocks of that version (Approved) and of the
// document's latest version (Current), each as Dispatch reads it from that version's own text
// (dispatch.HTTPClient.PlanDocument). The close compares their check blocks alone. Where Version
// is 0 no approved set exists to weaken, and Approved is empty.
type CheckApproval struct {
	Version  int                      `json:"version"`
	Approved []dispatch.DocumentBlock `json:"approved,omitempty"`
	Current  []dispatch.DocumentBlock `json:"current,omitempty"`
}

// checkBlockType is the block type of a check in a document version's blocks.
const checkBlockType = "check"

// The two attributes a check block may gain after its approval without rewording the check,
// where the approved block held neither: what the check reads, and the program it reads.
const (
	checkSubjectAttribute = "subject"
	checkProgramAttribute = "program"
)

// CloseWaitOf is the one predicate of an issue's close: what the close waits for, from the facts
// the workflow read. Each term is read as follows.
//
//   - Beneath and OpenChildren are taken as the facts give them.
//   - The read counts only where it answered and began after the issue's newest entry into
//     verifying. Each check counts only where the read holds the newest merge for its class
//     (checkAtNewestMerge). A check that does not makes the read Unread and is not listed, since
//     its state is of another merge. Each check that counts and does not pass is listed with its
//     state (CloseCheckOf).
//   - Unapproved compares the approved version's check blocks with the current ones
//     (unapprovedSince).
//   - The production term reads only the check of the issue's generation at its newest merge
//     (productionTerm).
//   - Defects is each open defect that is no check's reason and no production check's reason:
//     the read answers a check's reason, and the production term a production reason, so neither
//     holds the close as a defect.
//   - OpenChange is taken as the facts give it.
func CloseWaitOf(f CloseFacts) CloseWait {
	wait := CloseWait{Production: productionTerm(f), OpenChange: f.OpenChange}
	if len(f.Beneath) > 0 {
		wait.Beneath = f.Beneath
	}
	if len(f.OpenChildren) > 0 {
		wait.OpenChildren = f.OpenChildren
	}
	for _, defect := range f.Defects {
		if defect.Open() && defect.Kind != DefectKindCheck && defect.Kind != DefectKindProduction {
			wait.Defects = append(wait.Defects, strconv.FormatInt(defect.ID, 10))
		}
	}
	answer, counts := f.freshAnswer()
	if !counts {
		wait.Unread = true
		return wait
	}
	for _, check := range answer.Results.Checks {
		state, fresh := f.checkAtNewestMerge(check, answer.AtMergeHead)
		switch {
		case !fresh:
			wait.Unread = true
		case state.State != CheckStatePass:
			wait.Checks = append(wait.Checks, state)
		}
	}
	wait.Unapproved = unapprovedSince(answer.Approval)
	return wait
}

// Done says that the close waits for nothing: no term is listed and the read is not unread.
func (w CloseWait) Done() bool {
	return len(w.Beneath) == 0 && len(w.OpenChildren) == 0 && len(w.Checks) == 0 && !w.Unapproved && !w.Unread &&
		w.Production == "" && len(w.Defects) == 0 && !w.OpenChange
}

// CloseCheckOf is the one mapping of Dispatch's computed answer for a check to the close's state
// of it. It reads Dispatch's answer and reason alone, so the rule of a pass is Dispatch's, and
// where Dispatch's contract moves a name, this function alone changes:
//
//   - yes is a pass.
//   - no for the reason fail is a fail, with results[0] as its deciding result and commit as its
//     commit. It starts the next change.
//   - no for the reason no_result is no result yet.
//   - Every other answer is unknown: a pass and a fail at one commit (unknown, pass_and_fail),
//     which the owner is told of at once; a result Dispatch could not place or a read it could not
//     make (unknown, not_placed or read_failed), read again on the backoff; and an answer with no
//     reason, or one the close does not know, which the caller logs.
func CloseCheckOf(check dispatch.CheckBlock) CheckWait {
	stands := check.PassStands
	state := CheckWait{Check: check.BlockID, State: CheckStateUnknown, Answer: stands.Answer, Reason: stands.Reason}
	switch {
	case stands.Answer == dispatch.PassStandsYes:
		state.State = CheckStatePass
	case stands.Answer == dispatch.PassStandsNo && stands.Reason == dispatch.PassStandsReasonFail:
		state.State, state.Commit = CheckStateFail, stands.Commit
		if len(stands.Results) > 0 {
			state.Result = stands.Results[0]
		}
	case stands.Answer == dispatch.PassStandsNo && stands.Reason == dispatch.PassStandsReasonNoResult:
		state.State = CheckStateNoResult
	}
	return state
}

// freshAnswer is the newest read's answer where it counts for the close: the read answered, it
// began after the issue's newest entry into verifying, so a read in flight across an entry is not
// fresh, and its answer decodes. It answers false for every other read and for none.
func (f CloseFacts) freshAnswer() (CheckReadAnswer, bool) {
	read := f.Read
	if read == nil || read.Failure != "" || len(read.Answer) == 0 || !read.ReadAt.After(f.Entered) {
		return CheckReadAnswer{}, false
	}
	var answer CheckReadAnswer
	if err := json.Unmarshal(read.Answer, &answer); err != nil {
		return CheckReadAnswer{}, false
	}
	return answer, true
}

// checkAtNewestMerge is check as the close reads it, and whether the read holds the issue's newest
// merge for it, by the check's class:
//
//   - An after_merge check: its branch basis of the merge's repository on the merge's base branch
//     names merge_commit equal to the merge commit. A basis that names no merge_commit, which
//     Dispatch gives where it cannot learn or order that merge now, is not fresh yet.
//   - A default_branch check, which Dispatch compares with the default branch of each repository
//     alone: where that entry of the merge's repository is the merge's base branch, the same test.
//     Where it is another branch, the issue merged into an integration branch, and the entry names
//     a merge_commit only where a pull request that landed that branch holds the newest merge.
//     That rests on Dispatch's landing rule, so the entry is fresh, and where it names none the
//     work has not reached the default branch and Dispatch's answer is no result.
//   - A before_merge check: its subject whose basis.commit is the newest merge's final head, else
//     the check in the read at that head (atMergeHead), which names that head as its one basis.
//
// A check of another class is not fresh.
func (f CloseFacts) checkAtNewestMerge(check dispatch.CheckBlock, atMergeHead *dispatch.IssueResults) (CheckWait, bool) {
	switch check.When {
	case dispatch.CheckAfterMerge:
		basis, named := f.mergeBranchBasis(check, true)
		return CloseCheckOf(check), named && sameCommit(basis.MergeCommit, f.Merge.SHA)
	case dispatch.CheckDefaultBranch:
		basis, named := f.mergeBranchBasis(check, false)
		if !named {
			return CheckWait{}, false
		}
		if basis.Branch == f.Merge.Base {
			return CloseCheckOf(check), sameCommit(basis.MergeCommit, f.Merge.SHA)
		}
		return CloseCheckOf(check), true
	case dispatch.CheckBeforeMerge:
		return f.checkAtMergeHead(check, atMergeHead)
	}
	return CheckWait{}, false
}

// mergeBranchBasis is the branch basis of check in the repository of the issue's newest merge: on
// the merge's base branch where onBase is set, and otherwise the first, which for a default_branch
// check is the repository's default branch, the one branch Dispatch compares such a check with. A
// basis that names no repository is of the issue's own. It answers false where no such basis is
// named, and where the merge's repository, base branch or commit is not known: no reader invents
// one.
func (f CloseFacts) mergeBranchBasis(check dispatch.CheckBlock, onBase bool) (dispatch.CheckBasis, bool) {
	if f.Merge.Repo == "" || f.Merge.Base == "" || f.Merge.SHA == "" {
		return dispatch.CheckBasis{}, false
	}
	for _, basis := range check.ComparedWith {
		repo := basis.Repo
		if repo == "" {
			repo = f.Repo
		}
		if basis.Kind != dispatch.BasisBranch || !strings.EqualFold(repo, f.Merge.Repo) || onBase && basis.Branch != f.Merge.Base {
			continue
		}
		return basis, true
	}
	return dispatch.CheckBasis{}, false
}

// checkAtMergeHead is a before_merge check as the close reads it: its state at the final head of
// the issue's newest merged pull request, which is the subject of the results read whose
// basis.commit is that head, and where no subject names it, the check in the read at that head,
// whose one basis names it. It answers false where neither read holds that head.
func (f CloseFacts) checkAtMergeHead(check dispatch.CheckBlock, atMergeHead *dispatch.IssueResults) (CheckWait, bool) {
	if f.Merge.Head == "" {
		return CheckWait{}, false
	}
	for _, subject := range check.PassStands.Subjects {
		if sameCommit(subject.Basis.Commit, f.Merge.Head) {
			atHead := check
			atHead.PassStands = dispatch.PassStands{Answer: subject.Answer, Reason: subject.Reason, Results: subject.Results, Commit: subject.Basis.Commit}
			return CloseCheckOf(atHead), true
		}
	}
	if atMergeHead == nil {
		return CheckWait{}, false
	}
	for _, read := range atMergeHead.Checks {
		if read.BlockID != check.BlockID || read.When != dispatch.CheckBeforeMerge {
			continue
		}
		for _, basis := range read.ComparedWith {
			if basis.Kind == dispatch.BasisNamed && sameCommit(basis.Commit, f.Merge.Head) {
				return CloseCheckOf(read), true
			}
		}
	}
	return CheckWait{}, false
}

// productionTerm is what the close waits for of the production check: nothing where the project
// runs none. Otherwise it reads only the check of the issue's generation at the newest merge, so a
// check of generation 0, which a release before opened and which binds no generation, never meets
// it, and neither does a check of an earlier generation, one closed recorded included. Where no
// check of the issue's generation stands, the term is awaiting_change while a next change is open
// and opening otherwise. A check of the generation opened for an earlier merge is earlier_merge.
// The check of the newest merge is met once it closed recorded, and is open, not_run or defect
// before that.
func productionTerm(f CloseFacts) string {
	if !f.ProductionOn {
		return ""
	}
	check := f.Production
	if check == nil || check.Generation == 0 || check.Generation != f.Generation {
		if f.OpenChange {
			return ProductionAwaitingChange
		}
		return ProductionOpening
	}
	if !sameCommit(check.MergeSHA, f.Merge.SHA) {
		return ProductionEarlierMerge
	}
	switch {
	case check.Open():
		return ProductionOpen
	case check.Outcome == CheckRecorded:
		return ""
	case check.Outcome == CheckDefect:
		return ProductionDefect
	}
	return ProductionNotRun
}

// unapprovedSince says whether a check block of the approved version was removed or reworded in
// the current version. A block the current version holds as it was approved is the approved
// check. So is a block whose only difference is the attributes subject and program, where the
// approved block held neither: the comparison takes the two out of both blocks, and reads equal
// blocks as the approved check only where the approved block holds neither. Every other difference
// is a rewording: a changed value, a removed value, and a mark gained where the approved block held
// one of the two. A check added after the approval is no rewording: it holds the close like any
// other check. Where no version is approved, no approved set exists to weaken, and nothing is
// unapproved.
func unapprovedSince(approval *CheckApproval) bool {
	if approval == nil || approval.Version == 0 {
		return false
	}
	current := make(map[string]dispatch.DocumentBlock, len(approval.Current))
	for _, block := range approval.Current {
		if block.Type == checkBlockType {
			current[block.ID] = block
		}
	}
	for _, approved := range approval.Approved {
		if approved.Type != checkBlockType {
			continue
		}
		now, kept := current[approved.ID]
		if !kept || !sameCheckBlock(approved, now) {
			return true
		}
	}
	return false
}

// sameCheckBlock says whether now is the check approved approved: the same text and attributes,
// or, where the approved block holds neither subject nor program, the same once both are taken
// out of each.
func sameCheckBlock(approved, now dispatch.DocumentBlock) bool {
	if approved.Text != now.Text {
		return false
	}
	if maps.Equal(approved.Attributes, now.Attributes) {
		return true
	}
	_, subject := approved.Attributes[checkSubjectAttribute]
	_, program := approved.Attributes[checkProgramAttribute]
	if subject || program {
		return false
	}
	return maps.Equal(withoutMarks(approved.Attributes), withoutMarks(now.Attributes))
}

// withoutMarks is attributes without subject and program.
func withoutMarks(attributes map[string]string) map[string]string {
	kept := maps.Clone(attributes)
	delete(kept, checkSubjectAttribute)
	delete(kept, checkProgramAttribute)
	return kept
}

// sameCommit says whether two commits name one: both known and equal. An unknown commit names
// none, so two unknown commits are not the same merge.
func sameCommit(a, b string) bool { return a != "" && a == b }
