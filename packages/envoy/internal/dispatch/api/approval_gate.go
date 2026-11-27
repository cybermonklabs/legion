package api

import (
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// approvalGateLine is one line of an issue's approval gate, which GET /api/v1/me/inbox carries on
// each entry of an issue and GET /api/v1/issues/{key} on the issue: the gated step the line stands
// for, empty where the line stands for the issue's primary document alone, and that step's
// document with its approval. A step whose document the issue does not hold is a line with no
// document and no approval.
type approvalGateLine struct {
	Step       string                  `json:"step,omitempty"`
	ArtifactID *string                 `json:"artifact_id"`
	Approval   *model.ArtifactApproval `json:"approval"`
}

// approvalGateDocument is one document a gate reads: its id and its approval (approvalOf), or the
// zero value where the issue holds no such document.
type approvalGateDocument struct {
	id       string
	approval *model.ArtifactApproval
}

// line is the gate line of step that this document stands in.
func (document approvalGateDocument) line(step string) approvalGateLine {
	line := approvalGateLine{Step: step}
	if document.id != "" {
		id := document.id
		line.ArtifactID = &id
		line.Approval = document.approval
	}
	return line
}

// approvalGate is the gate of an issue, the one function both reads compute it with. A root issue
// (gateRoot: no parent, or a parent of another project) of a project that gates steps has one line
// for each step, in the order the project publishes them: `spec` names the primary document and
// `criteria` the Verification document. Every other issue has one line with no step, for its
// primary document. steps is the project's stored list, read as the project list reads it
// (gatedStepsWithout), so a stored `decisions` is no line.
func approvalGate(root bool, steps []string, primary, verification approvalGateDocument) []approvalGateLine {
	steps = gatedStepsWithout(steps)
	if !root || len(steps) == 0 {
		return []approvalGateLine{primary.line("")}
	}
	lines := make([]approvalGateLine, 0, len(steps))
	for _, step := range steps {
		var document approvalGateDocument
		switch step {
		case model.GatedStepSpec:
			document = primary
		case model.GatedStepCriteria:
			document = verification
		}
		lines = append(lines, document.line(step))
	}
	return lines
}

// issueApprovalGate is the gate the issue read answers, from what the read loaded: the issue row,
// which names its primary and its Verification document, the project's gated steps, and the
// issue's documents, each with the approval applyApprovals filled. A document the row names that
// the issue's list does not hold - a retired one - is no document of the gate, as the one read
// reads it (meInboxGateReads).
func issueApprovalGate(issue model.Issue, steps []string, artifacts []model.Artifact) []approvalGateLine {
	document := func(id string) approvalGateDocument {
		for index := range artifacts {
			if id != "" && artifacts[index].ID == id {
				return approvalGateDocument{id: id, approval: artifacts[index].Approval}
			}
		}
		return approvalGateDocument{}
	}
	return approvalGate(gateRoot(issue.Project, issue.Parent), steps, document(issue.PrimaryArtifactID), document(issue.VerificationArtifactID))
}

// gateRoot is whether an issue of project with parent (nil for none) is a root of its project, the
// issue that stands in the project's design gate: it has no parent, or its parent is an issue of
// another project, which passes nothing on (a parent of another project ends each of the server's
// parent walks as a root does, refs.IssueRepoTerms and legionWalkSQL among them). An issue key's
// project is the part before its first hyphen, since a key is its project's key, which holds no
// hyphen (contracts.ProjectKeyPattern), a hyphen and a number (createIssue), so the rule reads the
// issue's own row and not its parent's. The one read spells the same rule in SQL
// (meInboxGateReads), so both reads answer one gate.
func gateRoot(project string, parent *string) bool {
	if parent == nil {
		return true
	}
	parentProject, _, _ := strings.Cut(*parent, "-")
	return parentProject != project
}

// issueGatedStepsStatement reads the gated steps of the project of the issue $1 names, as stored;
// scanIssueGatedSteps reads its row. A key no issue carries returns no row, which is what the
// issue read's first batch requires of every statement beside the issue row's.
const issueGatedStepsStatement = `
		select p.gated_steps from issues i join projects p on p.key = i.project_key where i.key = $1
	`

// scanIssueGatedSteps reads the row issueGatedStepsStatement returns, nil where there is none.
func scanIssueGatedSteps(rows pgx.Rows) ([]string, error) {
	var steps []string
	for rows.Next() {
		if err := rows.Scan(&steps); err != nil {
			return nil, err
		}
	}
	return steps, rows.Err()
}
