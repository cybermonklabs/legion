package intake

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ArchitectCall is what an architect's call asks of the issue it names, as the ownership rule
// reads it on the Dispatch chain (R18 of dispatch://INFRA-3320): op names the call in the
// refusal, and the call is one an architect makes on its own issue and on the issues under it
// (actsOn), one it makes only on an issue under its own (under: rerun_child and
// release_children, which move an issue into Legion's work as the architect above it decides),
// one only a root's own architect makes (root: close_root), or one only the architect of an issue
// that holds its own gate makes (gate: register_gate).
type ArchitectCall struct {
	op   string
	kind architectCallKind
}

type architectCallKind int

const (
	actsOn architectCallKind = iota
	under
	root
	gate
)

// The architect calls the ownership rule judges, each by its kind.
var (
	SignOffCall  = ArchitectCall{op: "sign_off", kind: actsOn}
	BackwardCall = ArchitectCall{op: "request_backward_move", kind: actsOn}
	RetryCall    = ArchitectCall{op: "retry_or_escalate", kind: actsOn}
	PlanCall     = ArchitectCall{op: "register_plan", kind: actsOn}
	StopCall     = ArchitectCall{op: "stop_task", kind: actsOn}
	RerunCall    = ArchitectCall{op: "rerun_child", kind: under}
	ReleaseCall  = ArchitectCall{op: "release_children", kind: under}
	CloseCall    = ArchitectCall{op: "close_root", kind: root}
	GateCall     = ArchitectCall{op: "register_gate", kind: gate}
)

// Roots says whether the call is one only a root's own architect makes (close_root), which the
// rule holds even a call no architect made to (CallRefusal).
func (c ArchitectCall) Roots() bool { return c.kind == root }

// Op is the call's operation, as its refusal names it: sign_off, close_root and the rest.
func (c ArchitectCall) Op() string { return c.op }

// CallRefusal is the ownership rule an architect's call on key is held to, over chains, a read of
// the Dispatch chains of dispatchProject that started from or reached key: the API judges a call
// by it before it builds the fact, and the workflow again where the fact commits (workflow's
// architectCallRefusal and statusCall), so a caller reads the same answer whichever of the two
// refused it. architect is the caller's architect claim token, "" for a call no architect made,
// and caller the key of the caller's own issue where the caller knows it, "" where only the token
// is known: the refusal names the caller by it.
//
// A chain the copy cannot show to its top is refused CHAIN_UNKNOWN first (ChainUnknown): who may
// act on the issue is not known until the daemon has read Dispatch again. Then:
//   - an architect acts on its own issue and on the issues under it on the chain, whatever the
//     status of the issues between them (record.ArchitectActsOn); any other is refused
//     ISSUE_NOT_OWNED, naming the architect claim that acts on the issue now (record.OwningIssue);
//   - rerun_child and release_children take only an issue under the caller's own on the chain
//     (record.ArchitectAbove); any other is refused ISSUE_NOT_OWNED, naming the architect claim
//     above the issue that takes it, where one does;
//   - close_root ends a root (record.Chains.IsRoot), whoever calls, and only its own architect
//     calls it: refused ROOT_REQUIRED, naming the issue above it in Legion's work;
//   - register_gate is taken from the architect of an issue that holds its own gate
//     (record.Chains.HoldsOwnGate), on that issue: refused GATE_ROOT_ONLY, naming the gate root
//     whose architect registers it.
func CallRefusal(ctx context.Context, tx pgx.Tx, store record.Store, dispatchProject string, chains *record.Chains, architect claim.Token, caller, key string, call ArchitectCall) (*Refusal, error) {
	if chain := chains.Chain(key); !chain.Known() {
		return ChainUnknown(key, chain), nil
	}
	project, err := claim.ProjectToken(dispatchProject)
	if err != nil {
		return nil, err
	}
	own, err := claim.NewToken(project, key, claim.RoleArchitect)
	if err != nil {
		return nil, err
	}
	who := string(architect)
	if caller != "" {
		who = fmt.Sprintf("the architect of %s (%s)", caller, architect)
	}
	switch call.kind {
	case root:
		if above := chains.Above(key); len(above) > 0 {
			return &Refusal{Status: 403, Code: "ROOT_REQUIRED", Message: fmt.Sprintf("%s stands under %s in Legion's work on its Dispatch chain. close_root ends a root.",
				key, above[0]) + " " + record.EndChildRule}, nil
		}
		if architect != "" && architect != own {
			return &Refusal{Status: 403, Code: "ROOT_REQUIRED", Message: fmt.Sprintf("close_root ends the root %s; its own architect %s closes it, not %s", key, own, who)}, nil
		}
	case gate:
		if !chains.HoldsOwnGate(key) {
			holder := chains.GateRoot(key)
			return &Refusal{Status: 403, Code: "GATE_ROOT_ONLY", Message: fmt.Sprintf("%s holds no design gate of its own on its Dispatch chain; the design gate it works under is %s's, and its architect %s registers it",
				key, holder, architectOf(project, holder))}, nil
		}
		if architect != own {
			return &Refusal{Status: 403, Code: "GATE_ROOT_ONLY", Message: fmt.Sprintf("the design gate of %s is its own; its own architect %s registers it, not %s", key, own, who)}, nil
		}
	case under:
		if record.ArchitectAbove(project, chains, architect, key) {
			return nil, nil
		}
		walk, err := record.OwnerWalkOver(ctx, store, tx, project, chains, []string{key})
		if err != nil {
			return nil, err
		}
		taker := "no architect takes it there, since no issue above it on that chain is in Legion's work"
		if owner := walk.Owner(key, true); owner != "" && owner != key {
			taker = fmt.Sprintf("the architect of %s (%s) takes it", owner, architectOf(project, owner))
		}
		return &Refusal{Status: 403, Code: "ISSUE_NOT_OWNED", Message: fmt.Sprintf("%s of %s is refused: on its Dispatch chain %s is not under the issue of %s, and %s takes only an issue under its own; %s",
			call.op, key, key, who, call.op, taker)}, nil
	default:
		if record.ArchitectActsOn(project, chains, architect, key) {
			return nil, nil
		}
		walk, err := record.OwnerWalkOver(ctx, store, tx, project, chains, []string{key})
		if err != nil {
			return nil, err
		}
		owner := walk.Owner(key, false)
		return &Refusal{Status: 403, Code: "ISSUE_NOT_OWNED", Message: fmt.Sprintf("%s of %s is refused: on its Dispatch chain %s is neither the issue of %s nor under it; the architect of %s (%s) acts on it",
			call.op, key, key, who, owner, architectOf(project, owner))}, nil
	}
	return nil, nil
}

// architectOf is the architect claim token of key in the claim project project, as a refusal
// names it, or key itself where key makes no token.
func architectOf(project, key string) string {
	token, err := claim.NewToken(project, key, claim.RoleArchitect)
	if err != nil {
		return key
	}
	return string(token)
}
