package record

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// StandingApproval is a person's approval of a spec that stands for the architect of the tree root
// Root (StandingSpecApproval): version Version of the document ArtifactID, the spec of Spec.
// Carried says Root's spec row is a copy of another root's gate that a hand-over carried to it,
// and Spec is then that other root, or "" where the hand-over's row names none (a row a release
// before HandedOverRoot.FromRoot wrote); otherwise Spec is Root.
type StandingApproval struct {
	Root       string
	Spec       string
	ArtifactID string
	Version    int
	Carried    bool
}

// StandingSpecApproval is the approval that stands for the architect of the issue key, nil for
// none, read in the caller's transaction. steps are the design-gate steps the deployment gates.
//
// An approval stands only where the deployment gates the spec step, since no person approves a
// step it does not gate (`gates.design: off` gates none), and only for a tree root, since no
// person reads a child's spec. Then it stands where the root's spec row records an approved
// version with the place Dispatch recorded that approval at: a registration of an ungated step
// approves its row with no mark, so the mark is what says a person's approval set the version. A
// new version keeps it, a changes request on the root's own spec clears it, and a later approval
// replaces it (classify.ApplyDesignGateEvent). A row that is a copy of another root's gate, carried
// at the root's generation (HandedOverRoot.GateCarriedAt), takes only an approval of that other
// root's spec (workflow's copyTakes), so a changes request there leaves it standing, and a copy
// carried closed with an older approved version on its row names that version. A root with no spec
// row is answered by the record its re-admission left (ClearedSpecApproval), until its next
// registration or a changes request on that document clears it.
func StandingSpecApproval(ctx context.Context, tx pgx.Tx, store Store, steps []DesignStep, key string) (*StandingApproval, error) {
	if !slices.Contains(steps, DesignStepSpec) {
		return nil, nil
	}
	issue, err := store.Issue(ctx, tx, key)
	if err != nil || issue == nil || !claim.IsTreeRoot(issue.Key, issue.Tree) {
		return nil, err
	}
	gates, err := store.GatesOf(ctx, tx, key)
	if err != nil {
		return nil, err
	}
	row := SpecGate(gates)
	if row == nil {
		cleared, err := store.ClearedSpecApproval(ctx, tx, key)
		if err != nil || cleared == nil {
			return nil, err
		}
		return &StandingApproval{Root: key, Spec: key, ArtifactID: cleared.ArtifactID, Version: cleared.Version}, nil
	}
	if !row.PersonApproved() {
		return nil, nil
	}
	standing := &StandingApproval{Root: key, Spec: key, ArtifactID: row.ArtifactID, Version: *row.ApprovedVersion}
	handed, err := store.HandedOverRoot(ctx, tx, key)
	if err != nil {
		return nil, err
	}
	if handed.GateCarriedAt(issue.Generation) {
		standing.Carried, standing.Spec = true, handed.FromRoot
	}
	return standing, nil
}
