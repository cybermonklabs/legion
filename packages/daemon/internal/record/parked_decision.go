package record

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// RelayOutcome is what RelayParkedDecision did with one decision, which is what its caller logs.
type RelayOutcome int

const (
	// RelayNone is a decision nobody outside its tree needs: the tree runs and hears it itself.
	// Nothing is kept and nobody is told.
	RelayNone RelayOutcome = iota
	// RelayKept is a person's decision on a tree whose root waits for its slot: it is kept for the
	// root's next architect start, which names it (workflow.Engine.ParkedDecisionTask), and the
	// controller is not told, since the tree already runs again.
	RelayKept
	// RelaySent is a person's decision on a parked tree: kept for the root's next start and relayed
	// to the project's controller, which decides whether the tree runs again.
	RelaySent
	// RelayNoPerson is a decision on a parked tree that no person made — a session's, or the
	// daemon's own — which is nobody's to relay.
	RelayNoPerson
	// RelayNoPersonWaiting is a decision no person made on a tree whose root waits for its slot:
	// nothing is kept, since only a person decides, and nobody is told.
	RelayNoPersonWaiting
	// RelayNotParked is a decision on a lingering root someone has since moved to a status of the
	// workflow (admission's releaseUnslotted leaves one at in_progress): it is nobody's parked
	// tree, so nothing is kept and nobody is told.
	RelayNotParked
)

// WaitsForSlot says whether root is a root handed to Legion that waits in the line for its tree
// to be opened, with no architect running for it yet: the one queued-root rule (QueuedRoot),
// read with the root's own design gate and its tree's lifecycle (RootFacts), not lingering, and
// holding no slot of its own. steps are the design-gate steps the deployment gates.
//
// It is that rule and no other reading of it. "Status todo and no slot" used to say the same
// thing and no longer does: a running root can hold no row while it waits on another issue, and a
// root handed over in accepting carries the row it held in the tree it left (Waiting's
// queued-root loop states why), so a person's todo on such a root read here as a root with no
// architect, and every decision a person made on its tree was kept for a start that is never
// queued instead of reaching the architect that is running (RelayParkedDecision, workflow's
// CatchUpTheTreesOwed). The slot is still asked, after the rule rather than instead of it: a
// root at admitted whose gate has never opened is a queued root by its record and holds the
// slot its architect is writing the design in, and that architect hears a decision itself.
func WaitsForSlot(ctx context.Context, tx pgx.Tx, store Store, root Issue, steps []DesignStep) (bool, error) {
	if root.Lingers() || !claim.IsTreeRoot(root.Key, root.Tree) {
		return false, nil
	}
	lifecycle, gate, err := RootFacts(ctx, store, tx, root, steps)
	if err != nil {
		return false, err
	}
	if !QueuedRoot(root, lifecycle, gate) {
		return false, nil
	}
	slots, err := store.Slots(ctx, tx)
	if err != nil {
		return false, err
	}
	return !slices.ContainsFunc(slots, func(slot Slot) bool { return slot.Issue == root.Key }), nil
}

// RelayParkedDecision is the one rule for a decision made where nobody in its tree hears it, which
// every path that finds one follows: the workflow's answer, resolution or reply on an ask of a tree
// nobody runs (workflow's ask_mirror.go), the outbox's spec decision whose notice no architect
// took (the daemon's parkDecision), and the workflow's person's approval of a parked root that a
// later change of its status kept from queuing the root (RelayRefusedHandover). issue is where in
// Dispatch the decision was made, root its tree's root, and nobodyHears says the caller found
// nobody inside the tree to hear it. The rules, in order:
//
//   - A root that waits for its slot (WaitsForSlot) keeps a person's decision for the start its
//     admission will queue, and the controller is told nothing: the tree already runs again.
//   - A tree that runs hears the decision itself (RelayNone), and a lingering root someone has
//     moved back into the workflow is nobody's parked tree (RelayNotParked): either keeps nothing.
//   - Only a person decides (PersonActor): a session's write, the daemon's own included, is no
//     decision to relay (RelayNoPerson).
//   - What is kept is kept under issue, the issue it was decided on, never under a root: the tree
//     that owes the person an answer is the tree that issue stands in whenever a start or a
//     catch-up names it, so a child handed over into a tree of its own, a parked child a person
//     orphans and a queued root adopted into a live tree take what was decided on them with them,
//     with nothing to move. It is kept until the start or catch-up that named it reached an
//     architect of that tree (Store.ForgetKeptDecisions, or the confirmation of the delivery
//     holding it); one that reached none hands it over to a fresh catch-up
//     (workflow.HandOverKept), so it is never left unclaimed while its tree has an architect to
//     tell. The root is read for the two answers that are the root's own: whether it waits for its
//     slot, and the one parked-decision controller notice that names it, the decision, where in
//     Dispatch, who made it, and a spec decision's version and a changes request's reason.
//
// The kept row carries the person's words of the decision (ParkedDecision.Words), since a start
// task names them and its send-time rewrite reads them back from the row. The caller gives them:
// the words the decision's event carried, or a changes request's reason, by the person who
// requested the changes, and for an approval or a carried gate's opening the person and the time
// of the act alone (the daemon's noticeWords), so the notice names when the person made the
// decision; a carried gate's reason is the daemon's own sentence, never a person's, and is given as
// none.
//
// Every write is in the caller's transaction, which the caller keys so that a replay writes none of
// them again.
func RelayParkedDecision(ctx context.Context, tx pgx.Tx, store Store, issue string, root Issue, nobodyHears bool,
	decision ParkedDecision, version int, steps []DesignStep) (RelayOutcome, error) {
	return relayParkedDecision(ctx, tx, store, issue, root, nobodyHears, decision, ParkedDecisionNoticeOf(issue, root.Key, decision, version), steps)
}

// HandoverRefusal is why a person's approval of a parked root's primary document queued nothing
// as their handover of the root (workflow's refuseHandover): the root's status changed after the
// approval. ApprovalSeq is the Dispatch event of the approval, StatusChangedSeq the first later
// event of the root's Dispatch event log that changed its status (intake.DispatchArtifact's
// StatusChangedSeq), and Status the status Dispatch showed the root at when the daemon read the
// approval, after that change.
type HandoverRefusal struct {
	ApprovalSeq      int64
	StatusChangedSeq int64
	Status           string
}

// RelayRefusedHandover is RelayParkedDecision of a person's approval of the parked root's primary
// document that a later change of the root's status kept from queuing the root as their handover
// (refusal): nobody in the parked tree hears it, so the approval is kept for the root's next
// architect start, and the parked-decision notice the controller is sent names why it queued
// nothing (RefusedHandoverNoticeOf).
func RelayRefusedHandover(ctx context.Context, tx pgx.Tx, store Store, issue string, root Issue,
	decision ParkedDecision, version int, refusal HandoverRefusal, steps []DesignStep) (RelayOutcome, error) {
	return relayParkedDecision(ctx, tx, store, issue, root, true, decision, RefusedHandoverNoticeOf(issue, root.Key, decision, version, refusal), steps)
}

// relayParkedDecision is the rule RelayParkedDecision states, which keeps decision and sends the
// controller notice, the parked-decision notice its caller built of decision.
func relayParkedDecision(ctx context.Context, tx pgx.Tx, store Store, issue string, root Issue, nobodyHears bool,
	decision ParkedDecision, notice texts.ParkedDecisionRecord, steps []DesignStep) (RelayOutcome, error) {
	waiting, err := WaitsForSlot(ctx, tx, store, root, steps)
	if err != nil {
		return RelayNone, err
	}
	if !waiting {
		if !nobodyHears {
			return RelayNone, nil
		}
		if root.Lingers() && !OutOfWorkflow(root.Status) {
			return RelayNotParked, nil
		}
	}
	if !PersonActor(decision.Actor) {
		if waiting {
			return RelayNoPersonWaiting, nil
		}
		return RelayNoPerson, nil
	}
	decision.Issue = issue
	if err := store.AddParkedDecision(ctx, tx, decision); err != nil {
		return RelayNone, err
	}
	if waiting {
		return RelayKept, nil
	}
	queued, err := ControllerNoticeOf(notice)
	if err != nil {
		return RelayNone, err
	}
	row, err := NewOutboxRow(issue, queued, decision.At)
	if err != nil {
		return RelayNone, err
	}
	if err := store.Enqueue(ctx, tx, row); err != nil {
		return RelayNone, err
	}
	return RelaySent, nil
}

// ParkedDecisionNoticeOf is the parked-decision notice of decision, made on issue of root's tree:
// what the person did, the tree's root, the decision's reference, who and when (the act's own
// time where its words carry one, else the time the daemon kept it), a spec decision's version,
// and the person's words, cut to texts.OneAct (ActWords.Carry) and named in the words object with
// the person and the time: a changes request's reason, or an ask's answer, resolution or reply as
// its answer with the options it chose. Words that reached the daemon as a payload_omitted
// stand-in, with no text and the mark that more follows, are named in the words object with that
// mark alone, as the kept-decisions task draws them. The reference is where the rest of the words
// is read.
func ParkedDecisionNoticeOf(issue, root string, decision ParkedDecision, version int) texts.ParkedDecisionRecord {
	notice := texts.ParkedDecisionRecord{NoticeHead: texts.NoticeHead{Kind: texts.ParkedDecision, Issue: issue}, Decision: string(decision.Decision),
		Root: root, Ref: decision.Ref, Actor: decision.Actor, At: decision.At, Version: version}
	words := decision.Words
	if words == nil {
		return notice
	}
	if !words.At.IsZero() {
		notice.At = words.At
	}
	notice.Options = words.Options
	if words.Text == "" && !words.More {
		return notice
	}
	carried := words.Carry(texts.OneAct)
	field := "answer"
	if decision.Decision == DecisionDesignChangesRequested {
		field, notice.Reason = "reason", carried.Text
	} else {
		notice.Answer = carried.Text
	}
	by := cmp.Or(words.By, decision.Actor)
	notice.Words = map[string]texts.WordsMeta{field: {By: by, At: notice.At, Rest: carried.Rest, More: carried.More}}
	return notice
}

// RefusedHandoverNoticeOf is the parked-decision notice of decision (ParkedDecisionNoticeOf), a
// person's approval of issue's primary document, the parked root of its own tree, that refusal
// kept from queuing the root as their handover: it names the Dispatch event of the approval, the
// first later event that changed the root's status and the status the root stood at, that the
// approval queued nothing, and issue as the issue whose todo brings the root back.
func RefusedHandoverNoticeOf(issue, root string, decision ParkedDecision, version int, refusal HandoverRefusal) texts.ParkedDecisionRecord {
	notice := ParkedDecisionNoticeOf(issue, root, decision, version)
	notice.ApprovalSeq, notice.StatusChangedSeq, notice.Status = refusal.ApprovalSeq, refusal.StatusChangedSeq, refusal.Status
	notice.NotQueued, notice.SetTodo = true, issue
	return notice
}

// DecisionMark identifies one kept decision an architect's start task or catch-up named: the
// issue it was decided on, its Dispatch reference and the time of the decision the row held then
// (ParkedDecision.At) are the row's identity — a newer decision a person made on the same
// reference since replaces that row with a later time, so the telling leaves it for the next
// start or catch-up to name — and its kind and its author are what the task's text says of it
// (workflow.KeptDecisionsTask), which is rewritten from these marks whenever a held task is
// narrowed at send time (supervise's Deps.RootTask). The issue is also what says which tree the
// decision is owed to now, which is what that narrowing and the telling both read. Options are
// the options a person's answer chose, and Answer the words of the act, which a catch-up names
// as words by the person (Actor), AnswerAt when the person wrote them, More that more follows
// than the act's event carried and Rest the count a text had cut off them already (ActWords,
// ParkedDecision.Mark); a mark an older daemon wrote has none of the five. The words are as the
// row keeps them: a text that carries them cuts them when it is written (Carry). Its JSON is
// texts.DecisionMark's, with the decision a DecisionKind here.
type DecisionMark struct {
	Ref      string       `json:"ref"`
	At       time.Time    `json:"at"`
	Issue    string       `json:"issue,omitempty"`
	Decision DecisionKind `json:"decision,omitempty"`
	Actor    string       `json:"actor,omitempty"`
	Options  []string     `json:"options,omitempty"`
	Answer   string       `json:"answer,omitempty"`
	AnswerAt time.Time    `json:"answer_at,omitzero"`
	More     bool         `json:"more,omitempty"`
	Rest     int          `json:"rest,omitempty"`
}

// Carry is the words of m as a text carries them, bounded to limit (ActWords.Carry).
func (m DecisionMark) Carry(limit int) texts.Words {
	return carryWords(m.Answer, m.Rest, limit, m.More)
}

// KeptApproval is the approval a root's re-admission kept for its next architect start
// (Store.KeepReadmittedApproval): the design-gate step whose document a person approved, the
// version they approved, and the version that document has reached since. The zero value is no
// kept approval. The step is what says which document the task names, since a deployment that
// gates more than one step can keep a pair of any of them. KeptDecisions embeds it, so the pair
// travels to the architect as it was kept, with nothing copied field by field. Its JSON is
// texts.KeptApproval's, with the step a DesignStep here. Document is that document as a
// dispatch:// link, which the catch-up that names the approval gives (workflow's QueueCatchUp);
// the store keeps none, since the root and the step name the document.
type KeptApproval struct {
	Step     DesignStep `json:"step,omitempty"`
	Document string     `json:"document,omitempty"`
	Approved int        `json:"approved,omitempty"`
	Latest   int        `json:"latest,omitempty"`
}

// KeptDecisions is what one architect start task or catch-up named of what its tree is owed
// (workflow.Engine.ParkedDecisionTask): each decision, and the step and the pair of approved and
// latest versions of the document a person approved when it named one. It travels with the task
// (SuperviseRequest.Kept, CatchUp.Kept) and is persisted with the delivery a claim queues it as,
// which is what says which rows that delivery holds. A published catch-up tells exactly what it
// named (Store.ForgetKeptDecisions), and a queued start's task tells it at its delivery's
// confirmation. A start whose task reached no claim, and a catch-up a newer one supersedes, hand
// every row over instead (workflow.HandOverKept): a fresh catch-up names them while the tree each
// is owed to has an architect to tell, and otherwise they wait, KEPT, for the next start or
// ready to name them.
//
// The approval it names is the kept approval itself (KeptApproval), embedded: encoding/json
// writes an embedded struct's own fields as fields of this one, so the stored shape is the
// decisions and the fields of the approval, texts.KeptFacts's shape; a catch-up's adds the
// approval's document, which nothing else writes.
//
// A record written before a deployment could gate more than one step carries no step, and the
// task it is written again from reads it as the spec step, the one step that release gated.
type KeptDecisions struct {
	Decisions []DecisionMark `json:"decisions,omitempty"`
	KeptApproval
}

// KeptToldSource is the processed-event source a catch-up marks, under the dedupe key its publish
// went out on (Notice.Published, which a re-held copy shares with the row it copies, since one key
// is one telling however many rows publish it), once that publish has told the architect what it
// named — before it forgets those rows, in a transaction of its own. Only that mark says the
// architect was told when the forget fails or the daemon dies before its commit: the row's next run
// then forgets what it named rather than hand it over, and no drop of the root's queued catch-ups
// takes the row away before it does (Store.DropCatchUps, whose SQL computes that same key from the
// row it names, its original for a copy). A start needs none: its claim records the row with the
// delivery (supervise.Claim.DeliveredRows).
const KeptToldSource = "kept-told"

func (k KeptDecisions) validate() error {
	for _, decision := range k.Decisions {
		if decision.Ref == "" || decision.At.IsZero() {
			return fmt.Errorf("a kept decision names its Dispatch reference and the time it was kept at")
		}
	}
	if (k.Approved != 0 || k.Latest != 0) && (k.Approved <= 0 || k.Latest <= k.Approved) {
		return fmt.Errorf("a kept approval names a version a person approved below the spec's latest version")
	}
	if len(k.Decisions) == 0 && k.Approved == 0 {
		return fmt.Errorf("a kept-decisions record names nothing")
	}
	return nil
}

// DecisionKind is what a person did that a parked-decision notice relays.
type DecisionKind string

const (
	DecisionDesignApproved         DecisionKind = "design-approved"
	DecisionDesignChangesRequested DecisionKind = "design-changes-requested"
	DecisionCarriedGateOpened      DecisionKind = "carried-gate-opened"
	DecisionAskAnswered            DecisionKind = "ask-answered"
	DecisionAskResolved            DecisionKind = "ask-resolved"
	DecisionAskReply               DecisionKind = "ask-reply"
)

// SpecDecision is the decision a notice of kind reports when kind is one of the three a person's
// spec decision writes (workflow's gate.go), and whether it is. Each of the three names its own
// DecisionKind: the notice kinds and the decisions are two vocabularies, and nothing but this
// mapping ties them. The notice executor relays those three to the controller when nobody in the
// tree will hear them; every other kind is the machinery's, which a stopped tree needs nobody to
// hear.
func SpecDecision(kind NoticeKind) (DecisionKind, bool) {
	switch kind {
	case texts.DesignApproved:
		return DecisionDesignApproved, true
	case texts.DesignChangesRequested:
		return DecisionDesignChangesRequested, true
	case texts.CarriedGateOpened:
		return DecisionCarriedGateOpened, true
	default:
		return "", false
	}
}

func validDecisionKind(kind DecisionKind) bool {
	switch kind {
	case DecisionDesignApproved, DecisionDesignChangesRequested, DecisionCarriedGateOpened,
		DecisionAskAnswered, DecisionAskResolved, DecisionAskReply:
		return true
	default:
		return false
	}
}

// PersonActor says whether actor, as Dispatch names an event's author (`<kind>:<id>`), is a person.
func PersonActor(actor string) bool {
	return strings.HasPrefix(actor, "user:") && len(actor) > len("user:")
}
