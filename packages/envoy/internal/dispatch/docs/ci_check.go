package docs

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The four rules a `check` block carrying `ci_check` keeps, in the order CICheckRuleIn reads them.
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

// CheckAttributes is what a check block says of how it is proved: the attributes the four rules
// read and the results write path judges, read as the rules read them: `ci_check`, run and
// negative control with the spaces around them trimmed, as the editor cannot show them, and a
// blank `when` as the default, before_merge. A check block as the results read serves it
// (CheckBlockOf) keeps its run and negative control as written.
type CheckAttributes struct {
	When            string
	Run             string
	NegativeControl string
	CICheck         string
}

// CheckAttributesOf is the CheckAttributes the check block node holds.
func CheckAttributesOf(node *pmdoc.Node) CheckAttributes {
	return CheckAttributesOfBlock(CheckBlockOf(node))
}

// CheckAttributesOfBlock is the CheckAttributes of check, a block CheckBlockOf read, which has
// already trimmed its `ci_check` and given a blank `when` the default.
func CheckAttributesOfBlock(check model.CheckBlock) CheckAttributes {
	return CheckAttributes{
		When:            check.When,
		Run:             strings.TrimSpace(check.Run),
		NegativeControl: strings.TrimSpace(check.NegativeControl),
		CICheck:         check.CICheck,
	}
}

// CarriesCICheck reports whether check names a check run in `ci_check`. A blank value names none,
// as the editor hides an empty attribute, and a value of only spaces reads as blank.
func CarriesCICheck(check CheckAttributes) bool {
	return check.CICheck != ""
}

// CICheckRule is the first of the three attribute rules check breaks, read as a check carrying
// `ci_check` (CarriesCICheck), and "" when it breaks none: it carries no run, its `when` is
// before_merge, and it names its negative control. The fourth rule, that no result is recorded
// against it, is read from the database (CICheckRuleIn).
func CICheckRule(check CheckAttributes) string {
	switch {
	case check.Run != "":
		return ciCheckRuleRun
	case check.When != CheckWhenBeforeMerge:
		return ciCheckRuleWhen
	case check.NegativeControl == "":
		return ciCheckRuleNegativeControl
	}
	return ""
}

// CICheckRuleIn is the first of the four rules check, the check block blockID of the document
// artifactID, breaks, read as a check carrying `ci_check` (CarriesCICheck), and "" when it breaks
// none: the three of CICheckRule, then that no result is recorded against it, read through q
// (the pool, or the transaction of the write that asks) only when the other three leave that to
// decide. Every reader of the rules that has no results loaded comes here: a document write's
// refusal (refuseCICheckBlocks), settlement's repair of what a browser leaves
// (reconcileCICheckBlocks) and the refusal of a result against a check CI proves.
func CICheckRuleIn(ctx context.Context, q Queryer, artifactID, blockID string, check CheckAttributes) (string, error) {
	if rule := CICheckRule(check); rule != "" {
		return rule, nil
	}
	var recorded bool
	if err := q.QueryRow(ctx, `
		select exists(select 1 from check_results where check_artifact_id = $1::uuid and check_block_id = $2)
	`, artifactID, blockID).Scan(&recorded); err != nil {
		return "", fmt.Errorf("read whether check %s of document %s holds a result: %w", blockID, artifactID, err)
	}
	if recorded {
		return ciCheckRuleRecorded, nil
	}
	return "", nil
}

// ErrInvalidCICheck refuses a write that leaves a check block carrying `ci_check` that breaks one
// of the four rules (CICheckRuleIn). It is the caller's to fix, never a schema refusal.
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
// `ci_check` (with the spaces around it trimmed, as the editor cannot show them, so it names the
// run it reads as), run, `when` (a blank one read as the default, before_merge), negative control
// and surface. Run, negative control and surface are kept as written. Every reader of a check
// block's attributes comes here: the four rules through CheckAttributesOf, settlement and the
// results routes.
func CheckBlockOf(node *pmdoc.Node) model.CheckBlock {
	var check model.CheckBlock
	check.BlockID, _ = node.Attrs[pmdoc.BlockIDAttr].(string)
	ciCheck, _ := node.Attrs["ci_check"].(string)
	check.CICheck = strings.TrimSpace(ciCheck)
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
	// held is read from prior at the first block carrying ci_check, so a write that leaves none
	// walks prior not at all.
	var held map[string]CheckAttributes
	for _, node := range checkBlockNodes(tree) {
		check := CheckAttributesOf(node)
		if !CarriesCICheck(check) {
			continue
		}
		if held == nil {
			held = map[string]CheckAttributes{}
			for _, before := range checkBlockNodes(prior) {
				id, _ := before.Attrs[pmdoc.BlockIDAttr].(string)
				held[id] = CheckAttributesOf(before)
			}
		}
		blockID, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if before, ok := held[blockID]; ok && before == check {
			continue
		}
		rule, err := CICheckRuleIn(ctx, tx, artifactID, blockID, check)
		if err != nil {
			return err
		}
		if rule != "" {
			return &ErrInvalidCICheck{BlockID: blockID, CICheck: check.CICheck, Rule: rule}
		}
	}
	return nil
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
		check := CheckAttributesOf(node)
		if !CarriesCICheck(check) {
			continue
		}
		blockID, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		rule, err := CICheckRuleIn(ctx, tx, artifactID, blockID, check)
		if err != nil {
			return err
		}
		if rule == "" {
			continue
		}
		if r.repair(node, blockID, deleteJudgedCICheck(check)) {
			r.events = append(r.events, documentAskEvent(owner, artifactID, "block.invalid", actor, model.BlockInvalidEventPayload{
				BlockID:     blockID,
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
func deleteJudgedCICheck(judged CheckAttributes) func(*pmdoc.Node) bool {
	return func(node *pmdoc.Node) bool {
		if CheckAttributesOf(node) != judged {
			return false
		}
		delete(node.Attrs, "ci_check")
		return true
	}
}
