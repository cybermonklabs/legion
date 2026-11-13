package docs

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The four rules a `check` block carrying `ci_check` keeps, in the order CICheckRule reads them.
// Such a check is proved by the GitHub check run its `ci_check` names, so no role runs it.
const (
	ciCheckRuleRun             = "a check proved by CI carries no run"
	ciCheckRuleWhen            = "ci_check takes when of before_merge"
	ciCheckRuleNegativeControl = "a check proved by CI names its negative control"
	ciCheckRuleRecorded        = "this check already holds a recorded result, so it does not become CI's; write a new check block under a new id and delete this one"
)

// checkWhenBeforeMerge is the `when` a check proved by CI takes, and the default of a check that
// names none (pmdoc/schema/blocks.json).
const checkWhenBeforeMerge = "before_merge"

// CarriesCICheck reports whether check names a check run in `ci_check`. A blank value names none,
// as the editor hides an empty attribute.
func CarriesCICheck(check model.CheckBlock) bool {
	return strings.TrimSpace(check.CICheck) != ""
}

// CICheckRule is the first of the four rules check breaks, read as a check carrying `ci_check`
// (CarriesCICheck), and "" when it breaks none: it carries no run, its `when` is before_merge
// (blank is that default), it names its negative control, and no result is recorded against it.
// holdsRecordedResult is whether one is (HoldsRecordedResult). Every reader of the rules comes
// here: a document write's refusal (refuseCICheckBlocks) and the results routes' test of a check
// CI proves.
func CICheckRule(check model.CheckBlock, holdsRecordedResult bool) string {
	switch {
	case strings.TrimSpace(check.Run) != "":
		return ciCheckRuleRun
	case check.When != "" && check.When != checkWhenBeforeMerge:
		return ciCheckRuleWhen
	case strings.TrimSpace(check.NegativeControl) == "":
		return ciCheckRuleNegativeControl
	case holdsRecordedResult:
		return ciCheckRuleRecorded
	}
	return ""
}

// HoldsRecordedResult reports whether a result is recorded against the check block blockID of the
// document artifactID, read through q: the pool, or the transaction of the write that asks.
func HoldsRecordedResult(ctx context.Context, q Queryer, artifactID, blockID string) (bool, error) {
	var held bool
	if err := q.QueryRow(ctx, `
		select exists(select 1 from check_results where check_artifact_id = $1::uuid and check_block_id = $2)
	`, artifactID, blockID).Scan(&held); err != nil {
		return false, fmt.Errorf("read whether check %s of document %s holds a result: %w", blockID, artifactID, err)
	}
	return held, nil
}

// ErrInvalidCICheck refuses a write that leaves a check block carrying `ci_check` that breaks one
// of the four rules (CICheckRule). It is the caller's to fix, never a schema refusal.
type ErrInvalidCICheck struct {
	BlockID string
	CICheck string
	Rule    string
}

func (e *ErrInvalidCICheck) Error() string {
	return fmt.Sprintf("check %s (ci_check %q): %s", e.BlockID, e.CICheck, e.Rule)
}

// checkBlockAttributes is every `check` block of tree in document order, with the attributes the
// four rules read. A block repeating the id of a check block before it is left out, as the block
// index keeps the first holder of an id. A nil tree holds none.
func checkBlockAttributes(tree *pmdoc.Node) []model.CheckBlock {
	var checks []model.CheckBlock
	seen := map[string]struct{}{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "check" {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if _, repeated := seen[id]; repeated && id != "" {
			return true
		}
		seen[id] = struct{}{}
		check := model.CheckBlock{BlockID: id}
		check.CICheck, _ = node.Attrs["ci_check"].(string)
		check.Run, _ = node.Attrs["run"].(string)
		check.When, _ = node.Attrs["when"].(string)
		check.NegativeControl, _ = node.Attrs["negative_control"].(string)
		if check.When == "" {
			check.When = checkWhenBeforeMerge
		}
		checks = append(checks, check)
		return true
	})
	return checks
}

// refuseCICheckBlocks refuses tree, the tree an agent's or API write to artifactID leaves, when a
// check block the write introduces carries `ci_check` and breaks one of the four rules, naming the
// first such block in document order (ErrInvalidCICheck). A block is introduced when prior, the
// tree before the write, holds no check block under its id, or one whose `ci_check`, run, `when`
// or negative control differ; a nil prior (a new document, or one outside the schema) holds none.
// So an edit elsewhere in the document never meets a block another writer left broken. Whether a
// result is recorded against the block is read in the write's transaction. A browser's write
// never comes here: settlement repairs what it leaves.
func refuseCICheckBlocks(ctx context.Context, tx pgx.Tx, artifactID string, tree, prior *pmdoc.Node) error {
	held := map[string]model.CheckBlock{}
	for _, check := range checkBlockAttributes(prior) {
		held[check.BlockID] = check
	}
	for _, check := range checkBlockAttributes(tree) {
		if !CarriesCICheck(check) {
			continue
		}
		if before, ok := held[check.BlockID]; ok && sameCIRuleAttributes(before, check) {
			continue
		}
		recorded, err := HoldsRecordedResult(ctx, tx, artifactID, check.BlockID)
		if err != nil {
			return err
		}
		if rule := CICheckRule(check, recorded); rule != "" {
			return &ErrInvalidCICheck{BlockID: check.BlockID, CICheck: check.CICheck, Rule: rule}
		}
	}
	return nil
}

// sameCIRuleAttributes reports whether two check blocks carry the same attributes the four rules
// read.
func sameCIRuleAttributes(a, b model.CheckBlock) bool {
	return a.CICheck == b.CICheck && a.Run == b.Run && a.When == b.When && a.NegativeControl == b.NegativeControl
}
