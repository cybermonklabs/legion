package intake

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// DefectReport is a role's report_defect: a defect of the change of Issue, found once the task's
// last step passed (api's reportDefect). Role and By are the grant's: the role that reports, the
// implementer in retro, the merger in merging, or the owning architect, and its claim. Text says
// what is wrong with its evidence, Head is the head of the pull request the role saw it at, and
// Check is the block id of the check it is about, where it names one. The route held Text and
// Head to their form; the workflow judges who may report it and when, under the fact lock
// (workflow's defectReport), records it, and moves no phase.
type DefectReport struct {
	Issue string
	Role  claim.Role
	By    claim.Token
	Text  string
	Head  string
	Check string
}

func (f DefectReport) namedIssue() (string, bool) { return f.Issue, true }

// DefectWithdrawal is the owning architect's withdraw_defect: the reported defect Defect of Issue
// ended with Reason (api's withdrawDefect). By is the architect's claim, which the workflow holds
// to the rule of sign_off under the fact lock (workflow's defectWithdrawal).
type DefectWithdrawal struct {
	Issue  string
	Defect int64
	Reason string
	By     claim.Token
}

func (f DefectWithdrawal) namedIssue() (string, bool) { return f.Issue, true }

// DefectState is what a report or a withdrawal the workflow took did with its defect.
type DefectState string

const (
	// DefectRecorded is a report the workflow recorded as a defect of its own.
	DefectRecorded DefectState = "recorded"
	// DefectSame is a report whose text, trimmed with its white space folded, is an open defect's
	// of the same issue: that defect, and nothing new recorded.
	DefectSame DefectState = "same"
	// DefectJoined is a report naming a check that is already a reason of the issue: that reason,
	// and nothing new recorded.
	DefectJoined DefectState = "joined"
	// DefectWithdrawn is a withdrawal that ended its defect, or found it ended already.
	DefectWithdrawn DefectState = "withdrawn"
)

// DefectAnswer is what the workflow answers a report or a withdrawal it took (Result.Defect): the
// defect's id, what was done with it, and the generation of the change that will carry it, which
// a withdrawal leaves 0.
type DefectAnswer struct {
	Defect     int64
	State      DefectState
	NextChange uint64
}

// DefectNotFound is the 404 DEFECT_NOT_FOUND refusal of a withdrawal of id, which issue holds no
// defect of: the route answers it for an id that is no number (api's withdrawDefect), and the
// workflow for a number the record does not hold on the issue (workflow's defectWithdrawal).
func DefectNotFound(issue, id string) *Refusal {
	return &Refusal{Status: http.StatusNotFound, Code: "DEFECT_NOT_FOUND", Message: fmt.Sprintf(
		"%s holds no defect %s; read its defects in legion state's closeWait or in close-wait.md. This withdrawal changed nothing.", issue, id)}
}
