package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// This file is the inventory of every decision that reads the stored tree today (R15's last
// paragraph, R18, R19 and R26's repair of dispatch://INFRA-3320), written before any of them is
// moved onto the Dispatch chain (dispatch://INFRA-4730's t1). One fixture, chainRows, holds every
// shape the plan names; each row is one issue, what Dispatch and the daemon's record hold of it,
// and the answer the Dispatch chain gives for each decision about it. The words of R15 are read
// as the plan of dispatch://INFRA-4730 defines them ("The model"): an issue is in Legion's work
// when the daemon records it at a phase other than done and Dispatch shows it todo, in_progress,
// testing, needs_review or retro; Above is the issues above it on its chain that are in Legion's
// work, nearest first; a root has none; an issue holds its own gate when it is a root or ran as
// one (holds_gate); its gate root is the nearest issue at or above it in Legion's work that holds
// its own gate. Two rules follow from them, which the table's answers read:
//
//   - a notice about an issue reaches its own architect while that claim holds its role, else the
//     nearest architect of Above whose claim holds its role, else its gate root's, which for an
//     issue not in Legion's work is the nearest issue above it that the daemon records and that
//     holds its own gate, else the issue itself (R18, R22's state tree); one that starts at the
//     issue's parent walks Above alone and ends at the gate root of Above's first issue, and an
//     issue with nothing above it hears its own; a catch-up goes to its own issue's architect
//     alone;
//   - a session's status write on an issue is set back when the session holds a claim of an issue
//     with the same gate root (R26's repair, which reads the gate root where it reads the stored
//     tree today), and, from any other session, set back with its architect told only on an issue
//     that is its own gate root and has started; anything else is a person's move. The move out
//     of the workflow of the architect of an issue above comes first (dispatch://INFRA-3178): the
//     backlog of a session that holds a published architect claim of an issue above it on its
//     chain is a person's move whatever gate root the two stand under, and whatever of the chain
//     is in Legion's work (workflow's architectAbovesMove, decision 24 of dispatch://INFRA-4730).
//
// The decisions are read through the daemon's own code over a migrated store, one database and
// one runtime per project, and a stand-in Dispatch that answers from the fixture:
//
//   - the architect each notice kind reaches (record.NoticeKinds), through the notice router
//     (readNoticeRoute, noticeArchitect) over the outbox's view of the claims;
//   - the admission pass's starts and each issue's place in the state's admission lists after it
//     (active, waiting, held with its reason and holders), read through projection.Project;
//   - the repair of a session's status write (workflow's sessionStatusWrite), by its effect: a
//     reassert row, with or without its architect told;
//   - the answer of each architect operation the table names (release_children, rerun_child,
//     sign_off, close_root and register_gate), through api.Server's routes. park_child is gone
//     with #4573, and the rows that called it name no call in its place.
//
// A decision that changes the store is made in a transaction the test rolls back, or, for an
// architect's call, which commits through the API's own pool, followed by the store put back as
// the fixture left it (keepFixture), so every decision is made over the same fixture.

// The state of a fixture issue's architect claim; "" is an issue whose architect has no claim.
const (
	holdsItsRole        = "holds its role"
	architectFailed     = "failed"
	suspendedByOperator = "suspended by the operator"
	releasedForIdle     = "released for being idle"
)

// The answers of the repair of a session's status write (repairs).
const (
	setBack       = "set back"
	setBackTold   = "set back, its architect told"
	personsMove   = "taken as a person's move"
	outsideWriter = "outside"
)

// The places of an issue in the state's admission lists.
const (
	placeActive  = "active"
	placeWaiting = "waiting"
	placeNone    = "in no list"
)

// The architect operations a row's calls name, and the answer of a call the route took.
const (
	opRelease   = "release_children"
	opRerun     = "rerun_child"
	opSignOff   = "sign_off"
	opCloseRoot = "close_root"
	opRegister  = "register_gate"
	taken       = "taken"
)

// chainSeq is the Dispatch sequence every fixture issue's reading carries, and chainRepo the
// repository each resolves to.
const (
	chainSeq  = 10
	chainRepo = "acme/widgets"
)

// chainProjects is the projects of the fixture, each a daemon of its own: a daemon serves one
// project, and no chain crosses a project.
var chainProjects = []string{"INFRA", "BET", "LINUX", "OSSV"}

// heldPlace is the place of an issue held for reason by the issues by.
func heldPlace(reason record.HeldReason, by ...string) string {
	if len(by) == 0 {
		return "held " + string(reason)
	}
	return "held " + string(reason) + " by " + strings.Join(by, ", ")
}

// chainCall is one architect operation on a row's issue, made by the architect claim of the
// issue from, and the answer the chain gives it: taken, or the refusal's code.
type chainCall struct {
	op, from, want string
}

// chainRow is one issue of the fixture: what Dispatch shows of it, what today's daemon records of
// it, and the answers the Dispatch chain gives.
type chainRow struct {
	key   string
	shape string

	// Dispatch's issue, as a reading wrote it into the copy (seedCopyRow).
	parent    string
	status    string
	labelled  bool
	blockedBy []string

	// The daemon's record of the issue as today's daemon stores it; a zero phase leaves the issue
	// unrecorded. tree is the stored tree.
	phase     phase.Phase
	tree      string
	slot      bool   // holds an admission slot
	gate      bool   // a design gate of its own, approved at its latest version
	lifecycle bool   // an open workflow tree lifecycle under its own key
	architect string // the state of its architect claim, "" for none

	// The answers of the Dispatch chain. notice is the architect a notice about the issue
	// reaches and noticeAbove the one a notice that starts at its parent reaches
	// (record.NoticeStartsAtParent), both "" for an issue the daemon does not record, which has
	// no notice; place is where the state lists it after an admission pass and starts whether
	// that pass starts it; repairs is the answer to a session's write of backlog on it, by the
	// session of the architect claim of the issue each key names, or by a session no claim holds
	// (outsideWriter); calls are the architect operations on it. The repair's judgment of a done
	// and of a write by another role's session is k41's
	// (admit's TestTheRepairTakesTheWriteOfAnArchitectAboveOnTheChain), not this fixture's.
	notice, noticeAbove string
	place               string
	starts              bool
	repairs             map[string]string
	calls               []chainCall
}

// chainRows is the fixture. Every issue an answer names is a row of its own, so the stand-in
// Dispatch and the store hold exactly these issues. The comment above each row names the clause
// of dispatch://INFRA-3320 its answers follow.
var chainRows = []chainRow{
	// A root alone, which a person handed over and its architect's design a person approved: the
	// root holds its slot and stands at admitted with its gate open. R19: a root starts by a
	// person's handover, the label and todo, with its design gate open, no unfinished blocker and
	// a slot, so the pass starts its first phase. R18: its notices reach its own architect; it is a
	// root, so close_root takes it while it has started no phase. R26's repair: its own session is
	// an agent of its gate root, itself, and an outside write on it, its own gate root at work, is
	// set back and told.
	{key: "INFRA-9001", shape: "a root alone at todo with the label and an open gate",
		status: "todo", labelled: true, phase: phase.Admitted, tree: "INFRA-9001", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "INFRA-9001", noticeAbove: "INFRA-9001", place: placeActive, starts: true,
		repairs: map[string]string{"INFRA-9001": setBack, outsideWriter: setBackTold},
		calls: []chainCall{{opSignOff, "INFRA-9001", "SIGNOFF_OUTSIDE_ACCEPTING"}, {opRegister, "INFRA-9001", taken},
			{opCloseRoot, "INFRA-9001", taken}}},

	// A root at work, three deep. R18: its own architect, which holds its role; it acts on its
	// issue and the issues under it; register_gate from an issue that holds its own gate.
	// R10's children-first (kept by R19): a started parent with an open child waits on it.
	{key: "INFRA-9010", shape: "a root A at work with B, C and D under it",
		status: "in_progress", labelled: true, phase: phase.Implementing, tree: "INFRA-9010", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "INFRA-9010", noticeAbove: "INFRA-9010", place: heldPlace(record.HeldAwaitingChildren, "INFRA-9011"),
		repairs: map[string]string{"INFRA-9010": setBack, outsideWriter: setBackTold},
		calls: []chainCall{{opSignOff, "INFRA-9010", "SIGNOFF_OUTSIDE_ACCEPTING"}, {opRegister, "INFRA-9010", taken},
			{opCloseRoot, "INFRA-9010", "CLOSE_AFTER_PHASE_STARTED"}}},
	// B in work under A. R18: its own architect, released for being idle, still holds its role;
	// a notice from its parent reaches A's; B holds no gate of its own. R26's repair: a session of
	// B's claim is an agent of B's gate root A; A's architect owns B, so its backlog is a person's
	// move (dispatch://INFRA-3178); an outside write on an issue that is not its own gate root is a
	// person's move.
	{key: "INFRA-9011", shape: "B in work under A",
		parent: "INFRA-9010", status: "in_progress", phase: phase.Planning, tree: "INFRA-9010", slot: true, architect: releasedForIdle,
		notice: "INFRA-9011", noticeAbove: "INFRA-9010", place: heldPlace(record.HeldAwaitingChildren, "INFRA-9012"),
		repairs: map[string]string{"INFRA-9010": personsMove, "INFRA-9011": setBack, outsideWriter: personsMove},
		calls: []chainCall{{opSignOff, "INFRA-9010", "SIGNOFF_OUTSIDE_ACCEPTING"}, {opSignOff, "INFRA-9011", "SIGNOFF_OUTSIDE_ACCEPTING"},
			{opRegister, "INFRA-9011", "GATE_ROOT_ONLY"}}},
	// C in work under B, its own architect failed. R18: a failed claim does not hold its role, so
	// its notices go to the nearest architect above it in Legion's work that does, B's. R26's
	// repair: B's architect wrote its backlog on C while its claim was published, before its idle
	// release (seed's writtenAt), so the repair, judging the write by the holders published when
	// Dispatch recorded it, takes it as the move of the architect above (decision 24 of
	// dispatch://INFRA-4730), though the claim is released by the time the write is applied.
	{key: "INFRA-9012", shape: "C in work under B, its architect failed",
		parent: "INFRA-9011", status: "testing", phase: phase.Testing, tree: "INFRA-9010", slot: true, architect: architectFailed,
		notice: "INFRA-9011", noticeAbove: "INFRA-9011", place: heldPlace(record.HeldAwaitingChildren, "INFRA-9013"),
		repairs: map[string]string{"INFRA-9011": personsMove, "INFRA-9012": setBack, outsideWriter: personsMove},
		calls:   []chainCall{{opSignOff, "INFRA-9011", "SIGNOFF_OUTSIDE_ACCEPTING"}}},
	// D at todo under C. R19: an issue under a parent in Legion's work starts on todo, whatever its
	// label, once every gate above it is open (A's is). R18: C's architect failed, so B's. R26's
	// repair: B's architect's backlog, written before its idle release, is taken, as on C.
	{key: "INFRA-9013", shape: "D at todo under C, three deep",
		parent: "INFRA-9012", status: "todo", phase: phase.Admitted, tree: "INFRA-9010",
		notice: "INFRA-9011", noticeAbove: "INFRA-9011", place: placeActive, starts: true,
		repairs: map[string]string{"INFRA-9011": personsMove, outsideWriter: personsMove},
		calls:   []chainCall{{opRelease, "INFRA-9011", taken}, {opRelease, "INFRA-9012", taken}}},

	// An issue at work under a parent on the backlog under an issue in work: the state R24
	// refuses, which stands only before R25's fix. R15: the parent on the backlog is not in
	// Legion's work, so Above skips it. R19: work under way goes on.
	{key: "INFRA-9020", shape: "a root at work above a parent on the backlog",
		status: "in_progress", labelled: true, phase: phase.Implementing, tree: "INFRA-9020", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "INFRA-9020", noticeAbove: "INFRA-9020", place: placeActive,
		repairs: map[string]string{"INFRA-9020": setBack, outsideWriter: setBackTold}},
	{key: "INFRA-9021", shape: "a parent on the backlog under an issue in work",
		parent: "INFRA-9020", status: "backlog", phase: phase.Done, tree: "INFRA-9020",
		notice: "INFRA-9020", noticeAbove: "INFRA-9020", place: placeNone,
		calls: []chainCall{{opRelease, "INFRA-9020", taken}}},
	{key: "INFRA-9022", shape: "an issue at work under a parent on the backlog under an issue in work",
		parent: "INFRA-9021", status: "in_progress", phase: phase.Implementing, tree: "INFRA-9020", slot: true,
		notice: "INFRA-9020", noticeAbove: "INFRA-9020", place: placeActive,
		repairs: map[string]string{"INFRA-9020": personsMove, outsideWriter: personsMove},
		calls:   []chainCall{{opSignOff, "INFRA-9020", "SIGNOFF_OUTSIDE_ACCEPTING"}}},

	// Unrecorded children under a parent at work. R19: a label on an issue under a parent in
	// Legion's work changes nothing; release_children of the architect above it releases it, a
	// todo child as well as one on the backlog. R15: an unrecorded issue is in no list.
	{key: "INFRA-9030", shape: "a root at work over unrecorded children",
		status: "in_progress", labelled: true, phase: phase.Implementing, tree: "INFRA-9030", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "INFRA-9030", noticeAbove: "INFRA-9030", place: heldPlace(record.HeldAwaitingChildren, "INFRA-9031", "INFRA-9032", "INFRA-9033"),
		repairs: map[string]string{"INFRA-9030": setBack}},
	{key: "INFRA-9031", shape: "an unrecorded todo child with the label under a parent at work",
		parent: "INFRA-9030", status: "todo", labelled: true, place: placeNone,
		calls: []chainCall{{opRelease, "INFRA-9030", taken}}},
	{key: "INFRA-9032", shape: "an unrecorded todo child without the label under a parent at work",
		parent: "INFRA-9030", status: "todo", place: placeNone,
		calls: []chainCall{{opRelease, "INFRA-9030", taken}}},
	{key: "INFRA-9033", shape: "a labelled issue under a parent at work that Dispatch shows on the backlog",
		parent: "INFRA-9030", status: "backlog", labelled: true, place: placeNone,
		calls: []chainCall{{opRelease, "INFRA-9030", taken}}},

	// INFRA-500's shape: a done parent under an issue in merging. R19 and R21: it is held
	// accepted_ancestor under INFRA-430, which stands in merging; R18: the nearest architect above
	// it in Legion's work is INFRA-430's, which acts on it; R26's repair: INFRA-430's session is
	// an agent of INFRA-500's gate root.
	{key: "INFRA-430", shape: "an issue in merging above a done parent",
		status: "retro", labelled: true, phase: phase.Merging, tree: "INFRA-430", gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "INFRA-430", noticeAbove: "INFRA-430", place: heldPlace(record.HeldAtWork),
		repairs: map[string]string{"INFRA-430": setBack, outsideWriter: setBackTold},
		calls:   []chainCall{{opSignOff, "INFRA-430", "SIGNOFF_OUTSIDE_ACCEPTING"}}},
	{key: "INFRA-205", shape: "INFRA-500's done parent, stored as a closed tree of its own",
		parent: "INFRA-430", status: "done", phase: phase.Done, tree: "INFRA-205",
		notice: "INFRA-430", noticeAbove: "INFRA-430", place: placeNone,
		calls: []chainCall{{opRerun, "INFRA-430", "CHILD_UNDER_ACCEPTED_PARENT"}}},
	{key: "INFRA-500", shape: "INFRA-500: a done parent under an issue in merging",
		parent: "INFRA-205", status: "todo", phase: phase.Admitted, tree: "INFRA-205",
		notice: "INFRA-430", noticeAbove: "INFRA-430", place: heldPlace(record.HeldAcceptedAncestor, "INFRA-430"),
		repairs: map[string]string{"INFRA-430": personsMove, outsideWriter: personsMove},
		calls:   []chainCall{{opSignOff, "INFRA-430", "SIGNOFF_OUTSIDE_ACCEPTING"}}},

	// INFRA-1238's shape: a done parent under an admitted issue. R19 and R21: held ancestry
	// under INFRA-7, which is in Legion's work while INFRA-1238's parent is not. INFRA-7 itself is
	// a root with its label at todo, so the pass starts it (R19); its open child INFRA-14 holds its
	// first phase only (R10).
	{key: "INFRA-7", shape: "an admitted root over a root under it and a done parent",
		status: "todo", labelled: true, phase: phase.Admitted, tree: "INFRA-7",
		notice: "INFRA-7", noticeAbove: "INFRA-7", place: placeActive, starts: true,
		repairs: map[string]string{outsideWriter: personsMove}},
	// A root under a parent at work (the place walk's INFRA-14, accepting under INFRA-7). R15:
	// INFRA-7 is above it in Legion's work, so it is no root, and it keeps its own gate. R18: a
	// notice from its parent reaches INFRA-7's architect; it acts on its own issue and registers
	// its own gate; close_root ends a root, and it is none.
	{key: "INFRA-14", shape: "the place walk's root INFRA-14, accepting under INFRA-7",
		parent: "INFRA-7", status: "needs_review", labelled: true, phase: phase.Accepting, tree: "INFRA-14", gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "INFRA-14", noticeAbove: "INFRA-7", place: heldPlace(record.HeldAtWork),
		repairs: map[string]string{"INFRA-14": setBack, outsideWriter: setBackTold},
		calls:   []chainCall{{opSignOff, "INFRA-14", taken}, {opCloseRoot, "INFRA-14", "ROOT_REQUIRED"}, {opRegister, "INFRA-14", taken}}},
	{key: "INFRA-777", shape: "INFRA-1238's done parent, stored as a closed tree of its own",
		parent: "INFRA-7", status: "done", phase: phase.Done, tree: "INFRA-777",
		notice: "INFRA-7", noticeAbove: "INFRA-7", place: placeNone},
	{key: "INFRA-1238", shape: "INFRA-1238: a done parent under an admitted issue",
		parent: "INFRA-777", status: "todo", phase: phase.Admitted, tree: "INFRA-777",
		notice: "INFRA-7", noticeAbove: "INFRA-7", place: heldPlace(record.HeldAncestry, "INFRA-7"),
		repairs: map[string]string{outsideWriter: personsMove}},

	// INFRA-1416's shape: its only ancestor is done. R15 and R21: it is a root, and with no label
	// it waits for a person's handover (R19).
	{key: "INFRA-1066", shape: "INFRA-1416's done parent, its only ancestor",
		status: "done", phase: phase.Done, tree: "INFRA-1066",
		notice: "INFRA-1066", noticeAbove: "INFRA-1066", place: placeNone},
	{key: "INFRA-1416", shape: "INFRA-1416: its only ancestor done",
		parent: "INFRA-1066", status: "todo", phase: phase.Admitted, tree: "INFRA-1066",
		notice: "INFRA-1416", noticeAbove: "INFRA-1416", place: heldPlace(record.HeldLabelOff),
		repairs: map[string]string{outsideWriter: personsMove}},

	// Two roots of the place walk under INFRA-3189 at work. R15: each is under INFRA-3189 and
	// keeps its own gate, run and claims (R17, R21). R18: INFRA-904's own architect, suspended by
	// the operator, does not hold its role, so its notices reach INFRA-3189's; INFRA-3189's
	// architect acts on both; close_root ends a root only. R26's repair: a session of INFRA-3189's
	// claim is no agent of a gate root other than INFRA-3189, and each runs as its own gate root,
	// but INFRA-3189's architect stands above each on its chain, so its backlog is a person's move
	// (dispatch://INFRA-3178).
	{key: "INFRA-3189", shape: "the parent at work of two roots of the place walk",
		status: "in_progress", labelled: true, phase: phase.Implementing, tree: "INFRA-3189", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "INFRA-3189", noticeAbove: "INFRA-3189", place: heldPlace(record.HeldAwaitingChildren, "INFRA-2969", "INFRA-904"),
		repairs: map[string]string{"INFRA-3189": setBack, outsideWriter: setBackTold},
		calls:   []chainCall{{opCloseRoot, "INFRA-3189", "CLOSE_AFTER_PHASE_STARTED"}}},
	{key: "INFRA-2969", shape: "the place walk's root INFRA-2969, implementing under INFRA-3189",
		parent: "INFRA-3189", status: "in_progress", labelled: true, phase: phase.Implementing, tree: "INFRA-2969", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "INFRA-2969", noticeAbove: "INFRA-3189", place: placeActive,
		repairs: map[string]string{"INFRA-3189": personsMove, "INFRA-2969": setBack},
		calls: []chainCall{{opSignOff, "INFRA-3189", "SIGNOFF_OUTSIDE_ACCEPTING"},
			{opCloseRoot, "INFRA-2969", "ROOT_REQUIRED"}, {opRegister, "INFRA-2969", taken}}},
	{key: "INFRA-904", shape: "the place walk's root INFRA-904, implementing under INFRA-3189, its architect suspended by the operator",
		parent: "INFRA-3189", status: "in_progress", labelled: true, phase: phase.Implementing, tree: "INFRA-904", slot: true, gate: true, lifecycle: true, architect: suspendedByOperator,
		notice: "INFRA-3189", noticeAbove: "INFRA-3189", place: placeActive,
		repairs: map[string]string{"INFRA-3189": personsMove, "INFRA-904": setBack},
		calls:   []chainCall{{opSignOff, "INFRA-904", "SIGNOFF_OUTSIDE_ACCEPTING"}, {opRegister, "INFRA-904", taken}}},

	// INFRA-3206: no Dispatch parent, stored in INFRA-979's tree. R21: it holds no sign-off of
	// INFRA-979 and waits on its own unfinished blocked_by issue; R15: it is a root, so its notices
	// reach its own architect; R26's repair: INFRA-979's session is no agent of it, and no tree
	// runs at it.
	{key: "INFRA-979", shape: "the stored tree INFRA-3206 left, at work",
		status: "in_progress", labelled: true, phase: phase.Implementing, tree: "INFRA-979", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "INFRA-979", noticeAbove: "INFRA-979", place: placeActive,
		repairs: map[string]string{"INFRA-979": setBack}},
	{key: "INFRA-3206", shape: "INFRA-3206: no parent, stored in INFRA-979's tree",
		status: "todo", labelled: true, blockedBy: []string{"INFRA-3207"}, phase: phase.Admitted, tree: "INFRA-979",
		notice: "INFRA-3206", noticeAbove: "INFRA-3206", place: placeWaiting,
		repairs: map[string]string{"INFRA-979": personsMove, outsideWriter: personsMove},
		calls:   []chainCall{{opSignOff, "INFRA-979", "ISSUE_NOT_OWNED"}}},
	{key: "INFRA-3207", shape: "INFRA-3206's unfinished blocker, which the daemon does not record",
		status: "in_progress", place: placeNone},

	// INFRA-533: no Dispatch parent, stored in INFRA-302's tree. R21: it is a root; R19: with
	// its label and todo it starts; R18: INFRA-302's architect does not act on it.
	{key: "INFRA-302", shape: "the stored tree INFRA-533 left, at work",
		status: "in_progress", labelled: true, phase: phase.Implementing, tree: "INFRA-302", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "INFRA-302", noticeAbove: "INFRA-302", place: placeActive,
		repairs: map[string]string{"INFRA-302": setBack}},
	{key: "INFRA-533", shape: "INFRA-533: no parent, stored in INFRA-302's tree",
		status: "todo", labelled: true, phase: phase.Admitted, tree: "INFRA-302",
		notice: "INFRA-533", noticeAbove: "INFRA-533", place: placeActive, starts: true,
		repairs: map[string]string{"INFRA-302": personsMove, outsideWriter: personsMove},
		calls:   []chainCall{{opSignOff, "INFRA-302", "ISSUE_NOT_OWNED"}}},

	// INFRA-840: recorded at phase done while Dispatch shows it in work, stored in INFRA-514's
	// tree, under an issue the daemon does not record. R15 and R21: it is not in Legion's work and
	// holds nothing; its notices reach no architect of a tree it left.
	{key: "INFRA-514", shape: "the closed stored tree INFRA-840 left",
		status: "done", phase: phase.Done, tree: "INFRA-514",
		notice: "INFRA-514", noticeAbove: "INFRA-514", place: placeNone},
	{key: "INFRA-840", shape: "INFRA-840: done in the record, in work in Dispatch, stored in INFRA-514's tree",
		parent: "INFRA-3862", status: "in_progress", phase: phase.Done, tree: "INFRA-514",
		notice: "INFRA-840", noticeAbove: "INFRA-840", place: placeNone,
		repairs: map[string]string{outsideWriter: personsMove}},
	{key: "INFRA-3862", shape: "INFRA-840's parent, which the daemon does not record",
		status: "in_progress", place: placeNone},

	// The BET roots of the place walk under BET-59 at work, BET-683 among them with BET-690 and
	// BET-991 under it. R15: each is under BET-59 (BET-690 and BET-991 through BET-683, which is
	// in Legion's work); R18: a notice walks up to the nearest architect in Legion's work that
	// holds its role, past a failed one and past an issue with none; R19: an issue at todo under a
	// parent in Legion's work starts, once the gates of the gate holders above it are open, and
	// BET-683 holds its own and has registered none.
	{key: "BET-59", shape: "the place walk's parent at work of four roots",
		status: "in_progress", labelled: true, phase: phase.Implementing, tree: "BET-59", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "BET-59", noticeAbove: "BET-59", place: heldPlace(record.HeldAwaitingChildren, "BET-614", "BET-620", "BET-67", "BET-683"),
		repairs: map[string]string{"BET-59": setBack, outsideWriter: setBackTold}},
	{key: "BET-614", shape: "the place walk's root BET-614, implementing under BET-59",
		parent: "BET-59", status: "in_progress", labelled: true, phase: phase.Implementing, tree: "BET-614", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "BET-614", noticeAbove: "BET-59", place: placeActive,
		repairs: map[string]string{"BET-59": personsMove, "BET-614": setBack},
		calls:   []chainCall{{opSignOff, "BET-59", "SIGNOFF_OUTSIDE_ACCEPTING"}}},
	{key: "BET-620", shape: "the place walk's root BET-620, accepting under BET-59, its architect failed",
		parent: "BET-59", status: "needs_review", labelled: true, phase: phase.Accepting, tree: "BET-620", gate: true, lifecycle: true, architect: architectFailed,
		notice: "BET-59", noticeAbove: "BET-59", place: heldPlace(record.HeldAtWork),
		repairs: map[string]string{"BET-59": personsMove, "BET-620": setBack},
		calls:   []chainCall{{opSignOff, "BET-59", taken}, {opSignOff, "BET-620", taken}}},
	{key: "BET-67", shape: "the place walk's root BET-67, admitted at todo under BET-59",
		parent: "BET-59", status: "todo", labelled: true, phase: phase.Admitted, tree: "BET-67",
		notice: "BET-59", noticeAbove: "BET-59", place: placeActive, starts: true,
		repairs: map[string]string{"BET-59": personsMove}},
	{key: "BET-683", shape: "the place walk's root BET-683, admitted at in_progress under BET-59, over BET-690, BET-991 and BET-888",
		parent: "BET-59", status: "in_progress", labelled: true, phase: phase.Admitted, tree: "BET-683",
		notice: "BET-59", noticeAbove: "BET-59", place: heldPlace(record.HeldChildrenFirst, "BET-690", "BET-888", "BET-991"),
		repairs: map[string]string{"BET-59": personsMove}},
	{key: "BET-690", shape: "the place walk's root BET-690, implementing under BET-683",
		parent: "BET-683", status: "in_progress", labelled: true, phase: phase.Implementing, tree: "BET-690", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "BET-690", noticeAbove: "BET-59", place: placeActive,
		repairs: map[string]string{"BET-59": personsMove, "BET-690": setBack},
		calls: []chainCall{{opSignOff, "BET-59", "SIGNOFF_OUTSIDE_ACCEPTING"},
			{opCloseRoot, "BET-690", "ROOT_REQUIRED"}, {opRegister, "BET-690", taken}}},
	{key: "BET-991", shape: "the place walk's root BET-991, admitted at todo under BET-683",
		parent: "BET-683", status: "todo", labelled: true, phase: phase.Admitted, tree: "BET-991",
		notice: "BET-59", noticeAbove: "BET-59", place: heldPlace(record.HeldDesignGate, "BET-683"),
		repairs: map[string]string{outsideWriter: personsMove}},
	// BET-888: under BET-683, stored in BET-1090's tree. R21: it stands under BET-683, and the
	// architect above it acts on it, not BET-1090's.
	{key: "BET-1090", shape: "the stored tree BET-888 left, at work",
		status: "in_progress", labelled: true, phase: phase.Implementing, tree: "BET-1090", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "BET-1090", noticeAbove: "BET-1090", place: placeActive,
		repairs: map[string]string{"BET-1090": setBack, outsideWriter: setBackTold}},
	{key: "BET-888", shape: "BET-888: under BET-683, stored in BET-1090's tree",
		parent: "BET-683", status: "todo", phase: phase.Admitted, tree: "BET-1090",
		notice: "BET-59", noticeAbove: "BET-59", place: heldPlace(record.HeldDesignGate, "BET-683"),
		repairs: map[string]string{"BET-1090": personsMove, "BET-59": personsMove},
		calls:   []chainCall{{opSignOff, "BET-1090", "ISSUE_NOT_OWNED"}, {opSignOff, "BET-59", "SIGNOFF_OUTSIDE_ACCEPTING"}}},
	// The place walk's BET-951, admitted at todo under BET-305 at work. R19: it starts under its
	// parent; R18: BET-305's architect acts on it.
	{key: "BET-305", shape: "the place walk's parent at work of BET-951",
		status: "in_progress", labelled: true, phase: phase.Implementing, tree: "BET-305", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "BET-305", noticeAbove: "BET-305", place: heldPlace(record.HeldAwaitingChildren, "BET-951"),
		repairs: map[string]string{"BET-305": setBack}},
	{key: "BET-951", shape: "the place walk's root BET-951, admitted at todo under BET-305",
		parent: "BET-305", status: "todo", labelled: true, phase: phase.Admitted, tree: "BET-951",
		notice: "BET-305", noticeAbove: "BET-305", place: placeActive, starts: true,
		repairs: map[string]string{"BET-305": personsMove}},

	// The place walk's LINUX-1535 under LINUX-1301, and the two LINUX members whose chain does
	// not reach their stored tree, both at phase done while Dispatch shows them in work. R15 and
	// R21: neither is in Legion's work, so neither holds anything nor is acted on from the tree it
	// left.
	{key: "LINUX-1301", shape: "the place walk's parent at work of LINUX-1535",
		status: "in_progress", labelled: true, phase: phase.Implementing, tree: "LINUX-1301", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "LINUX-1301", noticeAbove: "LINUX-1301", place: heldPlace(record.HeldAwaitingChildren, "LINUX-1535"),
		repairs: map[string]string{"LINUX-1301": setBack}},
	{key: "LINUX-1535", shape: "the place walk's root LINUX-1535, implementing under LINUX-1301",
		parent: "LINUX-1301", status: "in_progress", labelled: true, phase: phase.Implementing, tree: "LINUX-1535", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "LINUX-1535", noticeAbove: "LINUX-1301", place: placeActive,
		repairs: map[string]string{"LINUX-1301": personsMove, "LINUX-1535": setBack},
		calls:   []chainCall{{opSignOff, "LINUX-1301", "SIGNOFF_OUTSIDE_ACCEPTING"}}},
	{key: "LINUX-1270", shape: "the stored tree LINUX-1419 left, at work",
		status: "in_progress", labelled: true, phase: phase.Implementing, tree: "LINUX-1270", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "LINUX-1270", noticeAbove: "LINUX-1270", place: placeActive,
		repairs: map[string]string{"LINUX-1270": setBack}},
	{key: "LINUX-1419", shape: "LINUX-1419: done in the record, todo in Dispatch, stored in LINUX-1270's tree",
		status: "todo", phase: phase.Done, tree: "LINUX-1270",
		notice: "LINUX-1419", noticeAbove: "LINUX-1419", place: placeNone,
		repairs: map[string]string{"LINUX-1270": personsMove},
		calls:   []chainCall{{opSignOff, "LINUX-1270", "ISSUE_NOT_OWNED"}, {opRerun, "LINUX-1270", "ISSUE_NOT_OWNED"}}},
	{key: "LINUX-1", shape: "the closed stored tree LINUX-2 left",
		status: "done", phase: phase.Done, tree: "LINUX-1",
		notice: "LINUX-1", noticeAbove: "LINUX-1", place: placeNone},
	{key: "LINUX-2", shape: "LINUX-2: done in the record, in work in Dispatch, stored in LINUX-1's tree",
		status: "in_progress", phase: phase.Done, tree: "LINUX-1",
		notice: "LINUX-2", noticeAbove: "LINUX-2", place: placeNone,
		repairs: map[string]string{outsideWriter: personsMove}},

	// The place walk's OSSV-135 under OSSV-276. R18: its own architect, released for being idle,
	// holds its role; a notice from its parent reaches OSSV-276's, which acts on it.
	{key: "OSSV-276", shape: "the place walk's parent at work of OSSV-135",
		status: "in_progress", labelled: true, phase: phase.Implementing, tree: "OSSV-276", slot: true, gate: true, lifecycle: true, architect: holdsItsRole,
		notice: "OSSV-276", noticeAbove: "OSSV-276", place: heldPlace(record.HeldAwaitingChildren, "OSSV-135"),
		repairs: map[string]string{"OSSV-276": setBack}},
	{key: "OSSV-135", shape: "the place walk's root OSSV-135, implementing under OSSV-276, its architect released for being idle",
		parent: "OSSV-276", status: "in_progress", labelled: true, phase: phase.Implementing, tree: "OSSV-135", slot: true, gate: true, lifecycle: true, architect: releasedForIdle,
		notice: "OSSV-135", noticeAbove: "OSSV-276", place: placeActive,
		repairs: map[string]string{"OSSV-276": personsMove, "OSSV-135": setBack},
		calls:   []chainCall{{opSignOff, "OSSV-276", "SIGNOFF_OUTSIDE_ACCEPTING"}, {opSignOff, "OSSV-135", "SIGNOFF_OUTSIDE_ACCEPTING"}}},
}

// noticeProbe is one notice the router is asked about, and the decision it answers.
type noticeProbe struct {
	label  string
	notice record.Notice
}

// noticeProbes is a notice of every kind the daemon publishes (record.NoticeKinds), so a kind
// added to that list joins the inventory, and, for a kind whose walk starts elsewhere when the
// notice is about the issue's own architect (record.NoticeStartsAtParent), that notice too.
func noticeProbes() []noticeProbe {
	var probes []noticeProbe
	for _, kind := range record.NoticeKinds() {
		notice := record.Notice{Kind: kind}
		if kind == "catch-up" {
			notice.CatchUp = &record.CatchUp{}
		}
		probes = append(probes, noticeProbe{label: fmt.Sprintf("the architect a %s notice reaches", kind), notice: notice})
		if record.NoticeStartsAtParent(kind, claim.RoleArchitect) != record.NoticeStartsAtParent(kind, "") {
			notice.Role = claim.RoleArchitect
			probes = append(probes, noticeProbe{label: fmt.Sprintf("the architect a %s notice about its own architect reaches", kind), notice: notice})
		}
	}
	return probes
}

// Each decision's label, as the answers and the failures name it.
const (
	placeLabel  = "its place in the admission lists after a pass"
	startsLabel = "whether the admission pass starts it"
)

// repairLabel is the decision of the repair of a session's write of backlog by writer.
func repairLabel(writer string) string {
	if writer == outsideWriter {
		return "the repair of a status write by a session no claim holds"
	}
	return fmt.Sprintf("the repair of a status write by the session of %s's architect", writer)
}

// callLabel is the decision of call's answer.
func callLabel(call chainCall) string {
	return fmt.Sprintf("%s from %s's architect", call.op, call.from)
}

// want is every decision of row and the answer the Dispatch chain gives it.
func (row chainRow) want() map[string]string {
	want := map[string]string{placeLabel: row.place, startsLabel: startsAnswer(row.starts)}
	for _, probe := range noticeProbes() {
		switch {
		case row.notice == "":
			want[probe.label] = "unroutable"
		case probe.notice.CatchUp != nil:
			// A catch-up states what its own issue's architect acts on, so it goes to that
			// architect and to no other (noticeArchitect; R18's catch-up).
			want[probe.label] = row.key
		case record.NoticeStartsAtParent(probe.notice.Kind, probe.notice.Role):
			want[probe.label] = row.noticeAbove
		default:
			want[probe.label] = row.notice
		}
	}
	for writer, answer := range row.repairs {
		want[repairLabel(writer)] = answer
	}
	for _, call := range row.calls {
		want[callLabel(call)] = call.want
	}
	return want
}

// startsAnswer is the answer of whether the admission pass starts an issue.
func startsAnswer(starts bool) string {
	if starts {
		return "starts"
	}
	return "starts nothing"
}

// architectToken is the claim token of key's architect in its project's daemon.
func architectToken(t *testing.T, key string) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(projectOf(key))
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, key, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// sessionOf is the session of key's architect claim, or, for outsideWriter, a session no claim
// holds.
func sessionOf(key string) string {
	if key == outsideWriter {
		return "ses-outside-the-project"
	}
	return "ses-" + strings.ToLower(key) + "-architect"
}

// documentOf is the design document of key, which its gate records and its architect registers.
func documentOf(key string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
}

// seedCopyRow writes the daemon's copy of Dispatch's issue key (dispatch_issues) as a Dispatch
// reading writes it (record.Store.PutDispatchIssue): its parent, its status, the issues it is
// blocked by and the sequence of the reading. A reading always carries a sequence of 1 or more,
// and PutDispatchIssue drops a row at 0, so a sequence below 1 fails the test rather than leaving
// the copy without the row.
func seedCopyRow(t *testing.T, tx pgx.Tx, key, parent, status string, seq int64, blockedBy ...string) {
	t.Helper()
	if seq < 1 {
		t.Fatalf("seed the copy row of %s at sequence %d: a Dispatch reading carries a sequence of 1 or more", key, seq)
	}
	if err := record.NewStore().PutDispatchIssue(context.Background(), tx, record.DispatchReading{
		Key: key, Project: projectOf(key), Seq: seq,
		Status: record.Carry(status), Parent: record.Carry(parent), Blockers: record.Carry(blockedBy),
	}); err != nil {
		t.Fatalf("seed the copy row of %s: %v", key, err)
	}
}

// rolledBack is a pool over one transaction its caller rolls back: each transaction a fact opens
// on it (intake.ApplyFact) is a savepoint of that one, so a decision is made through the daemon's
// own fact path and leaves the store as it found it.
type rolledBack struct{ tx pgx.Tx }

func (p rolledBack) BeginTx(ctx context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return p.tx.Begin(ctx)
}

func (p rolledBack) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return p.tx.Query(ctx, sql, args...)
}

func (p rolledBack) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return p.tx.QueryRow(ctx, sql, args...)
}

// fixtureDispatch is Dispatch as the fixture holds it: every issue of the project and the design
// document of each, approved at its one version.
type fixtureDispatch struct {
	outboxDispatch
	issues    map[string]dispatch.Issue
	documents map[string]string
}

func newFixtureDispatch(rows []chainRow) *fixtureDispatch {
	d := &fixtureDispatch{issues: map[string]dispatch.Issue{}, documents: map[string]string{}}
	for _, row := range rows {
		issue := dispatch.Issue{Key: row.key, Project: projectOf(row.key), Title: row.key, Status: row.status, Rank: rankOf(rows, row.key),
			HandedOver: row.labelled, LastSeq: chainSeq, ResolvedRepo: chainRepo, BlockedBy: row.blockedBy}
		if row.parent != "" {
			issue.Parent = new(row.parent)
		}
		if row.labelled {
			issue.Labels = []string{dispatch.LegionLabel}
		}
		d.issues[row.key] = issue
		d.documents[documentOf(row.key)] = row.key
	}
	return d
}

func (d *fixtureDispatch) ListIssues(_ context.Context, project string, _ []string) ([]dispatch.IssueSummary, error) {
	var listed []dispatch.IssueSummary
	for _, key := range slices.Sorted(maps.Keys(d.issues)) {
		issue := d.issues[key]
		if issue.Project != project {
			continue
		}
		listed = append(listed, dispatch.IssueSummary{Key: issue.Key, Title: issue.Title, Status: issue.Status, Parent: issue.Parent, Rank: issue.Rank,
			HandedOver: issue.HandedOver, LastSeq: issue.LastSeq, ResolvedRepo: issue.ResolvedRepo, BlockedBy: issue.BlockedBy})
	}
	return listed, nil
}

func (d *fixtureDispatch) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	issue, ok := d.issues[key]
	if !ok {
		return dispatch.Issue{}, &dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: key + " is not an issue"}
	}
	return issue, nil
}

func (d *fixtureDispatch) Approval(_ context.Context, artifactID string) (dispatch.Approval, error) {
	issue, ok := d.documents[artifactID]
	if !ok {
		return dispatch.Approval{}, &dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: artifactID + " is not a document"}
	}
	approved := 1
	return dispatch.Approval{IssueKey: issue, State: "approved", LatestVersion: 1, Version: &approved, Seq: chainSeq, Primary: true}, nil
}

// rankOf is key's board rank: its place among rows, the fixture's own order.
func rankOf(rows []chainRow, key string) string {
	return fmt.Sprintf("r%03d", slices.IndexFunc(rows, func(row chainRow) bool { return row.key == key }))
}

// keepFixture copies every table of the store as the fixture left it into a schema of its own,
// and answers the statements that put the store back as it was: every table emptied, children
// before the tables they reference, then filled again from the copy, parents first. An architect's
// call commits through the API's own pool, so a call the daemon takes changes the store, and the
// next call must be decided over the same fixture.
func keepFixture(t *testing.T, pool *store.Pool) string {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `select c.relname from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where n.nspname = 'public' and c.relkind = 'r' order by c.relname`)
	if err != nil {
		t.Fatalf("list the tables of the store: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list the tables of the store: %v", err)
	}
	rows, err = pool.Query(ctx, `select child.relname, parent.relname from pg_constraint k
		join pg_class child on child.oid = k.conrelid join pg_class parent on parent.oid = k.confrelid
		join pg_namespace n on n.oid = child.relnamespace
		where k.contype = 'f' and n.nspname = 'public' and child.oid <> parent.oid`)
	if err != nil {
		t.Fatalf("list the references of the store: %v", err)
	}
	references := map[string][]string{}
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			t.Fatalf("read a reference of the store: %v", err)
		}
		references[child] = append(references[child], parent)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list the references of the store: %v", err)
	}
	var order []string
	placed := map[string]bool{}
	var place func(table string, path []string)
	place = func(table string, path []string) {
		if placed[table] {
			return
		}
		if slices.Contains(path, table) {
			t.Fatalf("the store's references loop through %s: %v", table, path)
		}
		for _, parent := range references[table] {
			place(parent, append(path, table))
		}
		placed[table] = true
		order = append(order, table)
	}
	for _, table := range tables {
		place(table, nil)
	}
	keep := []string{"create schema fixture_kept"}
	var empty, fill []string
	for _, table := range order {
		live, kept := pgx.Identifier{"public", table}.Sanitize(), pgx.Identifier{"fixture_kept", table}.Sanitize()
		keep = append(keep, fmt.Sprintf("create table %s as table %s", kept, live))
		empty = append([]string{"delete from " + live}, empty...)
		fill = append(fill, fmt.Sprintf("insert into %s overriding system value select * from %s", live, kept))
	}
	if _, err := pool.Exec(ctx, strings.Join(keep, ";\n")); err != nil {
		t.Fatalf("keep the fixture: %v", err)
	}
	return strings.Join(append(empty, fill...), ";\n")
}

// wrongTrees sets every stored tree of the store to another issue's tree: each value
// issues.tree holds is mapped to the next one in key order, the last to the first, and that one
// mapping is applied to issues.tree, claims.tree, production_checks.tree,
// children_first_notices.tree, architect_stalled_notices.tree, handovers.tree,
// step_starts_rollback.tree and tree_lifecycles.tree, with claims.tree_epoch following the
// lifecycle its claim's tree now names. Every stored tree is then wrong, and the stored state is
// still one a daemon could read: each claim's tree is still its issue's stored tree.
func wrongTrees(t *testing.T, pool *store.Pool) {
	t.Helper()
	ctx := context.Background()
	var trees int
	if err := pool.QueryRow(ctx, "select count(distinct tree) from issues where tree <> ''").Scan(&trees); err != nil {
		t.Fatalf("count the stored trees: %v", err)
	}
	if trees < 2 {
		t.Fatalf("the store holds %d stored trees, and setting each to another issue's needs two or more", trees)
	}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
create temporary table wrong_tree on commit drop as
	with trees as (select distinct tree from issues where tree <> ''),
	ranked as (select tree, row_number() over (order by tree) as n, count(*) over () as total from trees)
	select a.tree as old, b.tree as new from ranked a join ranked b on b.n = a.n % a.total + 1;
update issues i set tree = w.new from wrong_tree w where i.tree = w.old;
update claims c set tree = w.new from wrong_tree w where c.tree = w.old;
update production_checks p set tree = w.new from wrong_tree w where p.tree = w.old;
update children_first_notices c set tree = w.new from wrong_tree w where c.tree = w.old;
update architect_stalled_notices a set tree = w.new from wrong_tree w where a.tree = w.old;
update handovers h set tree = w.new from wrong_tree w where h.tree = w.old;
update step_starts_rollback s set tree = w.new from wrong_tree w where s.tree = w.old;
update tree_lifecycles set tree = '~' || tree where tree in (select old from wrong_tree);
update tree_lifecycles l set tree = w.new from wrong_tree w where l.tree = '~' || w.old;
update claims c set tree_epoch = coalesce((select l.epoch from tree_lifecycles l where l.project = c.project and l.tree = c.tree), 0)`)
		return err
	}); err != nil {
		t.Fatalf("set every stored tree to another issue's: %v", err)
	}
}

// chainDaemon is one project's daemon over a store of its own seeded with the project's rows,
// wired as the daemon wires it: the Dispatch mirror, the workflow engine and admission as the
// handlers every fact applies through, the outbox's notice router over a supervisor holding the
// store's architect claims, and the API's routes over the same handlers and grants.
type chainDaemon struct {
	t        *testing.T
	project  string
	rows     []chainRow
	pool     *store.Pool
	st       *store.Store
	records  record.Store
	handlers []intake.Handler
	sup      *supervisor
	runner   *outbox
	routes   http.Handler
	grants   *credential.Grants
	restore  string
	tokens   map[claim.Token]string
	// writtenAt is when Dispatch recorded each session write the table makes: after every live
	// claim of the fixture was written, and before a claim released for being idle was released
	// (seed).
	writtenAt time.Time
}

// newChainDaemon is the daemon of project over a store seeded with the fixture's rows of that
// project, with every stored tree another issue's when wrong (wrongTrees), and the copy the store
// is put back from after each architect call (keepFixture).
func newChainDaemon(t *testing.T, project string, wrong bool) *chainDaemon {
	t.Helper()
	var rows []chainRow
	for _, row := range chainRows {
		if projectOf(row.key) == project {
			rows = append(rows, row)
		}
	}
	return newChainDaemonOver(t, project, rows, wrong)
}

// newChainDaemonOver is newChainDaemon over rows, every one of project, which the store and the
// stand-in Dispatch hold exactly.
func newChainDaemonOver(t *testing.T, project string, rows []chainRow, wrong bool) *chainDaemon {
	t.Helper()
	ctx := context.Background()
	token, err := claim.ProjectToken(project)
	if err != nil {
		t.Fatal(err)
	}
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store of %s: %v", project, err)
	}
	t.Cleanup(st.Close)
	d := &chainDaemon{t: t, project: project, rows: rows, pool: pool, st: st, records: record.NewStore(), tokens: map[claim.Token]string{}}
	for _, row := range rows {
		d.tokens[architectToken(t, row.key)] = row.key
	}
	d.seed(token)
	if wrong {
		wrongTrees(t, pool)
	}

	steps := config.DesignGateRootIssues.Steps()
	engine := workflow.New(d.records, workflow.Config{Project: project, Repositories: testRepositories, Steps: steps}, quietLogger())
	admission := admit.New(d.records, engine, len(rows), project, quietLogger())
	d.handlers = []intake.Handler{blockers.NewMirror(d.records, project), engine, admission}
	// The supervisor holds the claims the store holds, as a daemon's boot restores them: with
	// every stored tree wrong, each claim's tree is the wrong one.
	d.sup, _ = newOutboxSupervisor(t, token, t.TempDir())
	claims, err := st.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims of %s: %v", project, err)
	}
	for _, c := range claims {
		if _, _, err := d.sup.Create(ctx, c, ""); err != nil {
			t.Fatalf("supervise the claim %s: %v", c.Token, err)
		}
	}
	d.runner = &outbox{log: quietLogger(), pool: pool, dispatchProject: project, records: d.records, supervisor: d.sup, project: token}
	d.grants = credential.New(nil)
	d.routes = api.NewServer("127.0.0.1", 0, api.Options{
		Steps: steps, Supervisor: d.sup, Project: token, DispatchProject: project, Pool: pool, Handlers: d.handlers,
		Record: d.records, Grants: d.grants, Dispatch: newFixtureDispatch(rows), Log: quietLogger(),
	}).Handler
	d.restore = keepFixture(t, pool)
	return d
}

// seed writes the project's rows as today's daemon stores them: each issue's copy row, the
// record of each recorded issue with its gate and slot, the lifecycle of each tree at work, each
// architect claim on the tree its issue is stored in, and the marks today's daemon keeps by tree.
// Each record whose stored tree is its own key holds its own gate (holds_gate), as the
// migration that adds the column marks every root of the release before
// (20261122154644_issue_holds_gate).
func (d *chainDaemon) seed(token string) {
	t := d.t
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.Transact(ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
		slots := 0
		for _, row := range d.rows {
			seedCopyRow(t, tx, row.key, row.parent, row.status, chainSeq, row.blockedBy...)
			if row.labelled {
				// The copy holds the label as Dispatch shows it (R26 of dispatch://INFRA-3320 reads it
				// on the chain).
				if err := record.NewStore().PutDispatchIssue(ctx, tx, record.DispatchReading{Key: row.key, Project: projectOf(row.key),
					Seq: chainSeq, Labels: record.Carry([]string{dispatch.LegionLabel})}); err != nil {
					return err
				}
			}
			if row.phase == "" {
				continue
			}
			issue := record.Issue{Key: row.key, Tree: row.tree, Project: d.project, Title: row.key, Phase: row.phase, Generation: 1,
				Status: row.status, DispatchStatus: row.status, Rank: rankOf(d.rows, row.key), LastDispatchSeq: chainSeq, HandedOver: row.labelled,
				Repo: chainRepo, DefaultBranch: "main", GitHubRepo: chainRepo, HoldsGate: row.tree == row.key}
			issue.TakenBack = issue.CarriesRun()
			if row.parent != "" {
				issue.Parent = new(row.parent)
				// A root that ran under a parent Dispatch gave it later was told once that it stays
				// its own tree (workflow's tellReparented).
				if row.tree == row.key && issue.CarriesRun() {
					issue.ToldParent = row.parent
				}
			}
			if err := d.records.PutIssue(ctx, tx, issue); err != nil {
				return fmt.Errorf("record %s: %w", row.key, err)
			}
			if row.gate {
				approved := 1
				if err := d.records.PutGate(ctx, tx, record.DesignGate{Issue: row.key, Step: record.DesignStepSpec, ArtifactID: documentOf(row.key),
					LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true}); err != nil {
					return fmt.Errorf("open the gate of %s: %w", row.key, err)
				}
			}
			if row.slot {
				if err := d.records.PutSlot(ctx, tx, record.Slot{Issue: row.key, Index: slots, AdmittedAt: now}); err != nil {
					return fmt.Errorf("give %s its slot: %w", row.key, err)
				}
				slots++
			}
		}
		// The marks today's daemon keeps by tree, each where the fixture's story has one: a
		// parent's children-first wait told, a failed architect's stall told, the closed
		// production check of a done issue, the cancelled hand-over of a closed tree's child, and
		// the rollback witness of a step start.
		_, err := tx.Exec(ctx, `
insert into children_first_notices (issue, tree, generation, told_at)
	select key, tree, 1, now() from issues where key in ('INFRA-7', 'BET-683');
insert into architect_stalled_notices (issue, tree, generation, arm, told_at)
	select key, tree, 1, 'claim-unreachable', now() from issues where key = 'BET-620';
insert into production_checks (issue, project, tree, repo, pr_number, opened_at, bound_at, looks, closed_at, outcome)
	select key, project, tree, repo, 205, now() - interval '2 days', now() - interval '1 day', 1, now() - interval '1 day', 'recorded'
	from issues where key = 'INFRA-205';
insert into handovers (issue, tree, linger_generation, cancelled)
	select key, tree, 1, true from issues where key = 'INFRA-500';
insert into step_starts_rollback (issue, tree, taken_at)
	select key, tree, now() from issues where key = 'INFRA-9010'`)
		return err
	}); err != nil {
		t.Fatalf("seed the fixture of %s: %v", d.project, err)
	}
	epochs := map[string]uint64{}
	for _, row := range d.rows {
		if !row.lifecycle {
			continue
		}
		lifecycle, err := d.st.OpenTreeLifecycle(ctx, token, row.key, treelifecycle.AuthorityWorkflow)
		if err != nil {
			t.Fatalf("open the tree lifecycle of %s: %v", row.key, err)
		}
		epochs[row.key] = lifecycle.Epoch
	}
	// A claim released for being idle is written live first and released after the moment every
	// session write the table makes is written at (writtenAt), as an architect's write is made
	// while it holds its role and its idle release follows. The release keeps the claim's role
	// (supervise.HoldsRole), so its write stamps no leave (claims.unpublished_since) and the claim
	// stays published, and the repair judges the write by the holders published when Dispatch
	// recorded it.
	var released []supervise.Claim
	for _, row := range d.rows {
		if row.architect == "" {
			continue
		}
		c := supervise.Claim{Token: architectToken(t, row.key), Project: token, Tree: row.tree, TreeEpoch: epochs[row.tree], Issue: row.key,
			Role: claim.RoleArchitect, Generation: 1, State: supervise.StateWorking, Session: sessionOf(row.key),
			SessionFile: "/sessions/" + sessionOf(row.key) + ".jsonl", CapabilityHash: []byte("capability of " + row.key)}
		switch row.architect {
		case architectFailed:
			c.State = supervise.StateFailed
		case suspendedByOperator:
			c.State, c.SuspendedBy = supervise.StateSuspended, supervise.SuspendedByOperator
		case releasedForIdle:
			released = append(released, c)
		}
		if err := d.st.PutClaim(ctx, c); err != nil {
			t.Fatalf("seed the architect claim of %s: %v", row.key, err)
		}
		// The claim's session is held as its registration holds it, so a claim that holds its role
		// is one the daemon publishes as its role's holder (record.Store.RoleHolders).
		if _, err := d.st.HoldSessions(ctx, token, c.Token, supervise.RecordedHolds(c.Session, c.SessionFile)); err != nil {
			t.Fatalf("hold the session of the architect claim of %s: %v", row.key, err)
		}
	}
	d.writtenAt = time.Now().UTC()
	for _, c := range released {
		c.State, c.SuspendedBy = supervise.StateSuspended, supervise.SuspendedByReleased
		if err := d.st.PutClaim(ctx, c); err != nil {
			t.Fatalf("release the architect claim of %s for being idle: %v", c.Issue, err)
		}
	}
}

// decide makes every decision of the project's rows over the fixture and writes each answer into
// answers, by row and decision.
func (d *chainDaemon) decide(answers map[string]map[string]string) {
	answer := func(key, label, value string) {
		if answers[key] == nil {
			answers[key] = map[string]string{}
		}
		answers[key][label] = value
	}
	d.notices(answer)
	d.repairs(answer)
	d.pass(answer)
	d.calls(answer)
}

// notices asks the notice router which architect each notice of each row's issue reaches, as the
// outbox asks it when it delivers one, over the claims the supervisor holds: one route read for
// every row's issue (readNoticeRoute), as a ready's release reads it.
func (d *chainDaemon) notices(answer func(key, label, value string)) {
	t := d.t
	ctx := context.Background()
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin the notice reads of %s: %v", d.project, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	keys := make([]string, len(d.rows))
	for i, row := range d.rows {
		keys[i] = row.key
	}
	route, err := d.runner.readNoticeRoute(ctx, tx, d.project, keys)
	if err != nil {
		t.Fatalf("read the notice route of %s: %v", d.project, err)
	}
	for _, row := range d.rows {
		issue, err := route.issue(row.key)
		if err != nil && !errors.Is(err, errNoticeUnroutable) {
			t.Fatalf("read the notice route of %s: %v", row.key, err)
		}
		for _, probe := range noticeProbes() {
			if err != nil {
				answer(row.key, probe.label, "unroutable")
				continue
			}
			token, err := noticeArchitect(route, issue, probe.notice, d.runner.holdsItsRole)
			switch key, ok := d.tokens[token]; {
			case err != nil:
				answer(row.key, probe.label, "unroutable")
			case ok:
				answer(row.key, probe.label, key)
			default:
				answer(row.key, probe.label, string(token))
			}
		}
	}
}

// repairs writes backlog on each row's issue from each session its repairs name, as Dispatch's
// event of that write delivers it, through the handlers, and reads what the repair did: a
// reassert row of the daemon's own status, with or without its architect told.
func (d *chainDaemon) repairs(answer func(key, label, value string)) {
	t := d.t
	ctx := context.Background()
	for _, row := range d.rows {
		for _, writer := range slices.Sorted(maps.Keys(row.repairs)) {
			var labels []string
			if row.labelled {
				labels = []string{dispatch.LegionLabel}
			}
			fact := intake.DispatchIssue{Key: row.key, Seq: chainSeq + 1, Type: "issue.updated", Status: "backlog", Title: row.key, Parent: row.parent,
				Rank: rankOf(d.rows, row.key), HandedOver: row.labelled, Labels: labels, ActorSession: sessionOf(writer), ResolvedRepo: chainRepo, Blockers: row.blockedBy,
				At: d.writtenAt}
			answer(row.key, repairLabel(writer), d.undone(func(tx pgx.Tx) string {
				if _, err := intake.ApplyFact(ctx, rolledBack{tx}, "chain-decisions", "repair:"+row.key+":"+writer, fact, d.handlers...); err != nil {
					return "the fact failed: " + err.Error()
				}
				var reasserts, told int
				if err := tx.QueryRow(ctx, `select
					(select count(*) from outbox where issue = $1 and kind = 'dispatch_status' and payload->>'kind' = 'reassert'),
					(select count(*) from outbox where issue = $1 and kind = 'notice' and payload->>'kind' = 'status-reasserted')`,
					row.key).Scan(&reasserts, &told); err != nil {
					t.Fatalf("read the repair of %s: %v", row.key, err)
				}
				switch {
				case reasserts > 0 && told > 0:
					return setBackTold
				case reasserts > 0:
					return setBack
				case told > 0:
					return "its architect told, and nothing set back"
				}
				return personsMove
			}))
		}
	}
}

// pass runs one admission pass (the controller's tick, on which admission promotes the line)
// and reads, through the state's projection, where each issue stands after it, and through the
// records which issues it started: an issue it gave a slot, or one it moved from admitted into a
// phase that carries a run.
func (d *chainDaemon) pass(answer func(key, label, value string)) {
	t := d.t
	ctx := context.Background()
	claims, err := d.st.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims of %s: %v", d.project, err)
	}
	project := func(tx pgx.Tx) (api.State, error) {
		return projection.Project(ctx, projection.Inputs{Tx: tx, Store: d.records, Project: d.project,
			Steps: config.DesignGateRootIssues.Steps(), Claims: claims, Now: time.Now().UTC()})
	}
	recorded := func(tx pgx.Tx) map[string]record.Issue {
		issues, err := d.records.Issues(ctx, tx)
		if err != nil {
			t.Fatalf("read the records of %s: %v", d.project, err)
		}
		byKey := map[string]record.Issue{}
		for _, issue := range issues {
			byKey[issue.Key] = issue
		}
		return byKey
	}
	d.undone(func(tx pgx.Tx) string {
		before, err := project(tx)
		if err != nil {
			t.Fatalf("project the state of %s before the pass: %v", d.project, err)
		}
		was := recorded(tx)
		_, passErr := intake.ApplyFact(ctx, rolledBack{tx}, "chain-decisions", "pass:"+d.project, intake.ControllerTick{}, d.handlers...)
		after, err := project(tx)
		if passErr == nil && err != nil {
			passErr = err
		}
		is := recorded(tx)
		for _, row := range d.rows {
			if passErr != nil {
				answer(row.key, placeLabel, "the pass failed: "+passErr.Error())
				answer(row.key, startsLabel, "the pass failed: "+passErr.Error())
				continue
			}
			answer(row.key, placeLabel, placeIn(after, row.key))
			slotted := slices.Contains(after.Admission.Active, row.key) && !slices.Contains(before.Admission.Active, row.key)
			entered := was[row.key].Phase == phase.Admitted && is[row.key].CarriesRun()
			answer(row.key, startsLabel, startsAnswer(slotted || entered))
		}
		return ""
	})
}

// placeIn is where the state lists key among its admission lists.
func placeIn(state api.State, key string) string {
	active, waiting := slices.Contains(state.Admission.Active, key), slices.Contains(state.Admission.Waiting, key)
	switch {
	case active && waiting:
		return placeActive + " and " + placeWaiting
	case active:
		return placeActive
	case waiting:
		return placeWaiting
	}
	for _, held := range state.Admission.Held {
		if held.Issue == key {
			return heldPlace(held.Reason, held.HeldBy...)
		}
	}
	return placeNone
}

// calls makes each architect operation each row names through the API's route, with a grant its
// caller's claim mints, and reads the answer: taken, or the refusal's code. The store is put back
// as the fixture left it after each one.
func (d *chainDaemon) calls(answer func(key, label, value string)) {
	t := d.t
	ctx := context.Background()
	for _, row := range d.rows {
		for _, call := range row.calls {
			machine, ok := d.sup.Machine(architectToken(t, call.from))
			if !ok {
				t.Fatalf("the table names a call of %s from %s's architect, which has no claim", row.key, call.from)
			}
			grant, err := d.grants.Mint(machine.Claim())
			if err != nil {
				t.Fatalf("mint the grant of %s's architect: %v", call.from, err)
			}
			var path string
			var body any
			switch call.op {
			case opRelease:
				path, body = "/legion/v1/waves/release", api.WaveReleaseRequest{GrantID: grant.ID, Issues: []string{row.key}}
			case opRerun:
				path, body = "/legion/v1/children/rerun", api.RerunChildRequest{GrantID: grant.ID, Issue: row.key}
			case opSignOff:
				path, body = "/legion/v1/signoff", api.SignOffRequest{GrantID: grant.ID, Issue: row.key}
			case opCloseRoot:
				path, body = "/legion/v1/roots/close", api.RootCloseRequest{GrantID: grant.ID, Issue: row.key, Reason: "The work is no longer needed."}
			case opRegister:
				path, body = "/legion/v1/gates/register", api.GateRegisterRequest{GrantID: grant.ID, Issue: row.key, ArtifactID: documentOf(row.key), Version: 1}
			default:
				t.Fatalf("the table names the operation %q, which the test does not make", call.op)
			}
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			d.routes.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded)))
			answer(row.key, callLabel(call), callAnswer(recorder))
			if _, err := d.pool.Exec(ctx, d.restore); err != nil {
				t.Fatalf("put the store back after %s of %s: %v", call.op, row.key, err)
			}
		}
	}
}

// callAnswer is a route's answer: taken for a success, else the refusal's code.
func callAnswer(recorder *httptest.ResponseRecorder) string {
	if recorder.Code/100 == 2 {
		return taken
	}
	var failure api.Failure
	if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil || failure.Code == "" {
		return fmt.Sprintf("%d %s", recorder.Code, strings.TrimSpace(recorder.Body.String()))
	}
	return failure.Code
}

// undone runs decide in a transaction it then rolls back, and answers decide's answer.
func (d *chainDaemon) undone(decide func(tx pgx.Tx) string) string {
	t := d.t
	ctx := context.Background()
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin a decision of %s: %v", d.project, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return decide(tx)
}

// decideOverTheFixture makes every decision of the fixture, project by project, with each stored
// tree as today's daemon stores it, or, wrong, with every stored tree another issue's
// (wrongTrees), and answers each by row and decision.
func decideOverTheFixture(t *testing.T, wrong bool) map[string]map[string]string {
	t.Helper()
	answers := map[string]map[string]string{}
	for _, project := range chainProjects {
		newChainDaemon(t, project, wrong).decide(answers)
	}
	return answers
}

// describe is how a failure names a row: its key, the tree today's daemon stores it in where it
// has a record, and its shape.
func (row chainRow) describe() string {
	if row.phase == "" {
		return fmt.Sprintf("%s (unrecorded; %s)", row.key, row.shape)
	}
	return fmt.Sprintf("%s (stored tree %s; %s)", row.key, row.tree, row.shape)
}

// Each decision that reads the stored tree today, over a fixture of every shape the plan of
// dispatch://INFRA-4730 names, is the one the Dispatch chain gives under R15 to R21 and R26 of
// dispatch://INFRA-3320: the architect each notice reaches, the admission pass's starts and each
// issue's place after it, the repair of a session's status write, and each architect operation's
// answer. chainRows holds the fixture and the answers, each row commented with its clause; a
// failure names the row, the tree its record is stored in and the decision.
func TestDecisionsFollowTheDispatchChain(t *testing.T) {
	t.Parallel()
	got := decideOverTheFixture(t, false)
	for _, row := range chainRows {
		want := row.want()
		for _, label := range slices.Sorted(maps.Keys(want)) {
			answer, ok := got[row.key][label]
			if !ok {
				answer = "no answer"
			}
			if answer != want[label] {
				t.Errorf("%s: %s = %q, the Dispatch chain gives %q", row.describe(), label, answer, want[label])
			}
		}
	}
}

// Every decision of the fixture of TestDecisionsFollowTheDispatchChain is the same whatever the
// stored trees hold (R15: no decision reads one): the fixture is decided twice, once with each
// stored tree as today's daemon stores it and once with every stored tree column set to another
// issue's tree (wrongTrees), and both runs must give the table's answers. A decision whose answer
// the stored trees change is named first, with both answers.
func TestEveryDecisionIsTheSameWithEveryStoredTreeWrong(t *testing.T) {
	t.Parallel()
	right := decideOverTheFixture(t, false)
	wrong := decideOverTheFixture(t, true)
	for _, row := range chainRows {
		want := row.want()
		for _, label := range slices.Sorted(maps.Keys(want)) {
			stored, other := right[row.key][label], wrong[row.key][label]
			switch {
			case stored != other:
				t.Errorf("%s: %s = %q with each stored tree as the daemon stores it and %q with every stored tree another issue's, so the decision reads the stored tree; the Dispatch chain gives %q",
					row.describe(), label, stored, other, want[label])
			case stored != want[label]:
				t.Errorf("%s: %s = %q in both runs, the Dispatch chain gives %q", row.describe(), label, stored, want[label])
			}
		}
	}
}
