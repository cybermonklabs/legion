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

// CheckWhenBeforeMerge is the `when` a check proved by CI takes, and the default of a check that
// names none (pmdoc/schema/blocks.json).
const CheckWhenBeforeMerge = "before_merge"

// CarriesCICheck reports whether check names a check run in `ci_check`. A blank value names none,
// as the editor hides an empty attribute.
func CarriesCICheck(check model.CheckBlock) bool {
	return strings.TrimSpace(check.CICheck) != ""
}

// CICheckRule is the first of the four rules check breaks, read as a check carrying `ci_check`
// (CarriesCICheck), and "" when it breaks none: it carries no run, its `when` is before_merge
// (blank is that default), it names its negative control, and no result is recorded against it.
// holdsRecordedResult is whether one is (HoldsRecordedResult). Every reader of the rules comes
// here: a document write's refusal (refuseCICheckBlocks), settlement's repair of what a browser
// leaves (reconcileCICheckBlocks) and the results routes' test of a check CI proves.
func CICheckRule(check model.CheckBlock, holdsRecordedResult bool) string {
	switch {
	case strings.TrimSpace(check.Run) != "":
		return ciCheckRuleRun
	case check.When != "" && check.When != CheckWhenBeforeMerge:
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

// checkBlockNodes is every `check` block of tree in document order. A block repeating the id of a
// check block before it is left out, as the block index keeps the first holder of an id. A nil
// tree holds none.
func checkBlockNodes(tree *pmdoc.Node) []*pmdoc.Node {
	var nodes []*pmdoc.Node
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
		nodes = append(nodes, node)
		return true
	})
	return nodes
}

// CheckBlockOf is the check block node holds, with the attributes a check block carries: its id,
// `ci_check`, run, `when` (a blank one read as the default, before_merge), negative control and
// surface. Every reader of a check block's attributes comes here: the four rules (CICheckRule) and
// the results routes.
func CheckBlockOf(node *pmdoc.Node) model.CheckBlock {
	var check model.CheckBlock
	check.BlockID, _ = node.Attrs[pmdoc.BlockIDAttr].(string)
	check.CICheck, _ = node.Attrs["ci_check"].(string)
	check.Run, _ = node.Attrs["run"].(string)
	check.When, _ = node.Attrs["when"].(string)
	check.NegativeControl, _ = node.Attrs["negative_control"].(string)
	check.Surface, _ = node.Attrs["surface"].(string)
	if check.When == "" {
		check.When = CheckWhenBeforeMerge
	}
	return check
}

// refuseCICheckBlocks refuses tree, the tree an agent's or API write to artifactID leaves, when a
// check block the write introduces carries `ci_check` and breaks one of the four rules, naming the
// first such block in document order (ErrInvalidCICheck). A block is introduced when prior, the
// tree before the write, holds no check block under its id, or one whose `ci_check`, run, `when`
// or negative control differ; a nil prior (a new document, or one outside the schema) holds none.
// So an edit elsewhere in the document never meets a block another writer left broken. Whether a
// result is recorded against the block is read in the write's transaction, and only for a block
// that keeps the other three rules. A browser's write never comes here: settlement repairs what
// it leaves.
func refuseCICheckBlocks(ctx context.Context, tx pgx.Tx, artifactID string, tree, prior *pmdoc.Node) error {
	var carrying []model.CheckBlock
	for _, node := range checkBlockNodes(tree) {
		if check := CheckBlockOf(node); CarriesCICheck(check) {
			carrying = append(carrying, check)
		}
	}
	if len(carrying) == 0 {
		return nil
	}
	held := map[string]model.CheckBlock{}
	for _, node := range checkBlockNodes(prior) {
		check := CheckBlockOf(node)
		held[check.BlockID] = check
	}
	for _, check := range carrying {
		if before, ok := held[check.BlockID]; ok && sameCIRuleAttributes(before, check) {
			continue
		}
		rule, err := ciCheckRuleIn(ctx, tx, artifactID, check)
		if err != nil {
			return err
		}
		if rule != "" {
			return &ErrInvalidCICheck{BlockID: check.BlockID, CICheck: check.CICheck, Rule: rule}
		}
	}
	return nil
}

// ciCheckRuleIn is CICheckRule for check, a block of the document artifactID, with whether it
// holds a recorded result read through q, and only when the other three rules leave that to
// decide.
func ciCheckRuleIn(ctx context.Context, q Queryer, artifactID string, check model.CheckBlock) (string, error) {
	if rule := CICheckRule(check, false); rule != "" {
		return rule, nil
	}
	recorded, err := HoldsRecordedResult(ctx, q, artifactID, check.BlockID)
	if err != nil {
		return "", err
	}
	return CICheckRule(check, recorded), nil
}

// reconcileCICheckBlocks holds every check block of tree, the document settlement read, to the four
// rules. A browser's write is never refused, so a check block it leaves carrying a `ci_check` that
// breaks a rule is repaired here: its `ci_check` is deleted, in tree and, through repairLive, in the
// live document, and one block.invalid names the block, the rule it broke and block_type "check",
// which the issue's Activity reads as the refusal. A repaired block carries no `ci_check`, so a
// later settlement repairs it no more. Whether a result is recorded against a block is read in tx,
// the settlement's transaction, and only for a block that keeps the other three rules. actor is
// the settlement's, as its ask events name.
func (r *settlementReconciliation) reconcileCICheckBlocks(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	tree *pmdoc.Node,
	actor model.Actor,
) error {
	for _, node := range checkBlockNodes(tree) {
		check := CheckBlockOf(node)
		if !CarriesCICheck(check) {
			continue
		}
		rule, err := ciCheckRuleIn(ctx, tx, artifactID, check)
		if err != nil {
			return err
		}
		if rule == "" {
			continue
		}
		if r.repair(node, check.BlockID, deleteJudgedCICheck(check)) {
			r.events = append(r.events, documentAskEvent(owner, artifactID, "block.invalid", actor, model.BlockInvalidEventPayload{
				BlockID:     check.BlockID,
				Reason:      rule,
				DisturbedBy: actor,
				BlockType:   "check",
			}))
		}
	}
	return nil
}

// deleteJudgedCICheck deletes the `ci_check` of a check block that still carries the attributes
// judged holds, the block settlement found breaking a rule. A block a peer changed since is left
// as it is: the change schedules a settlement of its own, which judges it again.
func deleteJudgedCICheck(judged model.CheckBlock) func(*pmdoc.Node) bool {
	return func(node *pmdoc.Node) bool {
		if !sameCIRuleAttributes(CheckBlockOf(node), judged) {
			return false
		}
		delete(node.Attrs, "ci_check")
		return true
	}
}

// sameCIRuleAttributes reports whether two check blocks carry the same attributes the four rules
// read.
func sameCIRuleAttributes(a, b model.CheckBlock) bool {
	return a.CICheck == b.CICheck && a.Run == b.Run && a.When == b.When && a.NegativeControl == b.NegativeControl
}
