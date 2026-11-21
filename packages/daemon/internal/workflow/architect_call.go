package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// architectCall is what an architect's call asks of the issue it names, as the ownership rule
// reads it on the Dispatch chain (R18 of dispatch://INFRA-3320): op names the call in the
// refusal, and the call is one an architect makes on its own issue and on the issues under it
// (actsOn), one only a root's own architect makes (root: close_root), or one only the architect
// of an issue that holds its own gate makes (gate: register_gate).
type architectCall struct {
	op   string
	kind architectCallKind
}

type architectCallKind int

const (
	actsOn architectCallKind = iota
	root
	gate
)

var (
	signOffCall  = architectCall{op: "sign_off", kind: actsOn}
	backwardCall = architectCall{op: "request_backward_move", kind: actsOn}
	retryCall    = architectCall{op: "retry_or_escalate", kind: actsOn}
	closeCall    = architectCall{op: "close_root", kind: root}
	gateCall     = architectCall{op: "register_gate", kind: gate}
)

// architectCallRefusal is the check of an architect's call where its fact commits (decision 5 of
// dispatch://INFRA-4730): the API judged the call before it built the fact, in a transaction of
// its own, and a change of parent can land between the two, so the workflow reads the Dispatch
// chain of issue again in the fact's own transaction (record.ReadChains) and refuses the call the
// chain no longer allows, with the code that names the architect who may act. architect is the
// caller's architect claim token the fact carries, "" for a fact no architect sent (a phase
// worker's own backward move, the operator's retry, a registration the workflow makes itself),
// which is held to nothing here but close_root's root.
//
// A chain the copy cannot show to its top is refused CHAIN_UNKNOWN first (intake.ChainUnknown):
// who may act on the issue is not known until the daemon has read Dispatch again. Then:
//   - an architect acts on its own issue and on the issues under it on the chain, whatever the
//     status of the issues between them (record.ArchitectActsOn); any other is refused
//     ISSUE_NOT_OWNED, naming the architect that owns the issue now (record.OwnerWalk.Owner);
//   - close_root ends a root (record.Chains.IsRoot), whoever calls, and only its own architect
//     calls it: refused ROOT_REQUIRED, naming the issue above it in Legion's work;
//   - register_gate is taken from the architect of an issue that holds its own gate
//     (record.Chains.HoldsOwnGate), on that issue: refused GATE_ROOT_ONLY, naming the gate root
//     whose architect registers it.
//
// The refusals are the API's own codes and statuses for the same calls, so a caller reads the
// same answer whichever of the two refused it.
func (e *Engine) architectCallRefusal(ctx context.Context, tx pgx.Tx, architect claim.Token, issue record.Issue, call architectCall) (*intake.Refusal, error) {
	if architect == "" && call.kind != root {
		return nil, nil
	}
	chains, err := record.ReadChains(ctx, tx, e.store, issue.Project, []string{issue.Key})
	if err != nil {
		return nil, err
	}
	if chain := chains.Chain(issue.Key); !chain.Known() {
		return intake.ChainUnknown(issue.Key, chain), nil
	}
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return nil, err
	}
	own, err := claim.NewToken(project, issue.Key, claim.RoleArchitect)
	if err != nil {
		return nil, err
	}
	switch call.kind {
	case root:
		if above := chains.Above(issue.Key); len(above) > 0 {
			return &intake.Refusal{Status: 403, Code: "ROOT_REQUIRED", Message: fmt.Sprintf("%s stands under %s in Legion's work on its Dispatch chain; close_root ends a root, and an issue under another leaves with park_child "+parkRule,
				issue.Key, above[0])}, nil
		}
		if architect != "" && architect != own {
			return &intake.Refusal{Status: 403, Code: "ROOT_REQUIRED", Message: fmt.Sprintf("close_root ends the root %s; its own architect closes it, not %s", issue.Key, architect)}, nil
		}
	case gate:
		if !chains.HoldsOwnGate(issue.Key) {
			return &intake.Refusal{Status: 403, Code: "GATE_ROOT_ONLY", Message: fmt.Sprintf("%s holds no design gate of its own on its Dispatch chain; the design gate it works under is %s's, and its architect registers it",
				issue.Key, chains.GateRoot(issue.Key))}, nil
		}
		if architect != own {
			return &intake.Refusal{Status: 403, Code: "GATE_ROOT_ONLY", Message: fmt.Sprintf("the design gate of %s is its own; its own architect registers it, not %s", issue.Key, architect)}, nil
		}
	default:
		if record.ArchitectActsOn(project, chains, architect, issue.Key) {
			return nil, nil
		}
		walk, err := record.OwnerWalkOver(ctx, e.store, tx, project, chains, []string{issue.Key})
		if err != nil {
			return nil, err
		}
		return &intake.Refusal{Status: 403, Code: "ISSUE_NOT_OWNED", Message: fmt.Sprintf("%s of %s is refused: on its Dispatch chain %s is not the issue of %s or under it, and the architect of %s acts on it",
			call.op, issue.Key, issue.Key, architect, walk.Owner(issue.Key, false))}, nil
	}
	return nil, nil
}
