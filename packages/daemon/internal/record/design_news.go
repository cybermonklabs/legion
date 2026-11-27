package record

import (
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// DesignAudience is whom a told row speaks for (DesignTold).
type DesignAudience string

const (
	// AudienceHolder is the session holding an issue's phase, whichever session that is: the row
	// is the issue's and not a claim's, so the next holder of the issue inherits what the last one
	// took, and is told what changed since.
	AudienceHolder DesignAudience = "holder"
	// AudienceArchitect is a tree root's architect, for the whole tree.
	AudienceArchitect DesignAudience = "architect"
)

// DesignTold is what one audience took of its tree's approved design: the approved version of each
// gated step's document it took, and whether the gate's opening is owed to it.
//
// A holder row rises only by a credit: a confirmed delivery's, the copy of news its prompt carried,
// counted while the delivery holds that confirmation and folded in once its turn's result says the
// turn ran, or a raise where what it would be told changes nothing of the work
// (DesignChanges.WorkUnchanged: ask blocks alone, or nothing at all), which tells nobody
// anything. A credit reaches only a row that took the version it counts from, and its give-back
// ends it alone (Postgres.TreeTold reads the row as its credits make it). So it says what the
// session holding the issue's phase was given, never what the daemon meant to give it. Its first
// value is the baseline: the approved versions the issue's first working phase of its generation
// started from (workflow's Engine.enter), and for a tree root's architect row those its tree's
// first opening released (workflow's putGateTold), or the gate a hand-over carried to it (admit's
// readmit). The rows go with the issue's generation (Postgres.ClearGeneration), so the next
// generation takes its own baseline.
type DesignTold struct {
	Issue    string
	Audience DesignAudience
	// Versions is the version of each step's document the audience took. A step with no entry is
	// one the record does not know: a holder of work already in flight when this record began, whose
	// tree's gate a changes request had closed, or a step whose row was registered after the
	// baseline. 0 is a step taken while no approved version of its document stood.
	Versions map[DesignStep]int
	// OpeningOwed says the opening sentence of the gate is owed to the audience: the gate opened
	// while it was owed news, so its news carries the opening in place of the role message.
	OpeningOwed bool
	// Opening is the number of the last settlement of the gate's opening on this row
	// (Postgres.SettleOpening, CloseOpenings): each opening that became owed to the audience, each
	// close of the gate and each opened role message the audience was sent takes the next one. An
	// owed opening is named by it, and a delivery carries the number it rendered the opening under
	// (DeliveryOpening.Seq), so its credit tells that opening and no later one.
	Opening int
	// ToldAt is when a write of its versions last touched the row: the baseline or the fill that
	// wrote it, or the last raise, which sets it whether or not a version rose. A fill of a row
	// already there keeps it. It is zero for a row that only the owed opening wrote.
	ToldAt time.Time
}

// Took is the version of step's document the audience took, and whether the record knows it.
func (t DesignTold) Took(step DesignStep) (int, bool) {
	version, known := t.Versions[step]
	return version, known
}

// ToldRow is issue's audience row among told, the zero row, which knows no version and owes no
// opening, where there is none. The table is keyed by issue and audience, so at most one row
// matches.
func ToldRow(told []DesignTold, issue string, audience DesignAudience) DesignTold {
	for _, row := range told {
		if row.Issue == issue && row.Audience == audience {
			return row
		}
	}
	return DesignTold{Issue: issue, Audience: audience}
}

// VersionPair is two versions of one document, the earlier From and the later To: the key of a
// change set (DesignChanges). From 0 is a document with no approved version before To: a step its
// audience took while none stood, or one Dispatch's review history read and found none before To.
// A review history that could not be read names no pair at all (DesignHistoryUnread).
type VersionPair struct {
	ArtifactID string
	From       int
	To         int
}

// DesignHistoryQuestion is one question the reconcile asks Dispatch's review history
// (dispatch.Client.ApprovedVersionBefore): which version of the document ArtifactID a person last
// approved before version To. An owed item whose told row knows no version of its step asks it
// (NewsItem.Question), since that answer is the version its audience is taken to have worked from.
type DesignHistoryQuestion struct {
	ArtifactID string
	To         int
}

// DesignHistoryUnread is a question of Dispatch's review history that the reconcile could not
// answer within its bounded attempts (the daemon's designChanges): Reason names the failed read,
// with Dispatch's error kept as a far side's error text is (the daemon's giveUpReason), and ReadAt
// is when the reconcile gave up on it.
//
// It is a state of its own, and never a version. A read that failed says nothing about the
// document, so the told row that asked keeps no version of the step, and its item names no pair:
// the news says the version its audience took could not be read, and why, never that no version
// was approved before To, which is what a version 0 says. Every later reconcile asks the history
// again, and the read that answers fills the told row and deletes this row in one write
// (Postgres.ClearDesignHistoryUnread), so a failure Dispatch stops giving is never kept as a fact.
// For the same reason the row stands only while no reconcile of the tree that asks it is pending
// (Store.DesignNewsPending): a pending one reads the history again and replaces the row, so no
// start attaches it, and no merger is released on it, until that reconcile has run (the daemon's
// readNewsRecorded). A delivery a start already attached it to carries its reason and time
// (DeliveryNewsItem.HistoryUnread), and each send of that delivery tells it as attached.
type DesignHistoryUnread struct {
	DesignHistoryQuestion
	Reason string
	ReadAt time.Time
}

// DesignChangesState is how a change set was recorded: read from Dispatch, or recorded as one
// Dispatch could not answer.
type DesignChangesState string

const (
	// DesignChangesRead is Dispatch's answer for the pair.
	DesignChangesRead DesignChangesState = "read"
	// DesignChangesUnavailable is a pair Dispatch could not answer: a document Dispatch no longer
	// holds (dispatch.ErrDocumentNotFound), or a bounded run of failed reads, a Dispatch whose
	// every answer was that it has no changes route (dispatch.ErrNoChangesRoute) among them. Its
	// Reason says which.
	DesignChangesUnavailable DesignChangesState = "unavailable"
)

// DesignChanges is what changed in one document between two of its versions, as Dispatch read the
// two (dispatch.ArtifactChanges), or the reason it could not say. Two stored versions of a
// document never change, so a recorded change set never goes stale: keeping it is no copy of a
// value its owner can move, and a pair recorded read is never written again
// (Postgres.PutDesignChanges).
type DesignChanges struct {
	VersionPair
	State DesignChangesState
	// DecisionOnly says the two versions differ only in their ask blocks, so the change
	// tells nobody anything about the work: Sections and Blocks are empty. The name is that of
	// the changes route's field decision_only, which names ask blocks, not `decision` blocks.
	DecisionOnly bool
	// Unchanged says Dispatch found no change at all between the two versions: their stored texts
	// are the same, or what differs is in no section, run or block. It tells nobody anything about
	// the work either. Sections and Blocks are empty, DecisionOnly is false, and an unavailable
	// pair is never unchanged, since nothing was read of it.
	Unchanged bool
	Sections  []SectionChange
	Blocks    []BlockChange
	// Reason is why Dispatch could not answer, for an unavailable pair alone, with Dispatch's error
	// kept as a far side's error text is (the daemon's giveUpReason).
	Reason string
	ReadAt time.Time
}

// WorkUnchanged says whether Dispatch read the pair and found that nothing of the work changed:
// its two versions differ in ask blocks alone (DecisionOnly), or in nothing (Unchanged). It is
// the one test of that rule, read by the reconcile, which raises what a holder took past such a
// pair and sends nothing (the daemon's settlementOf), and by the news, which says such a pair
// changed nothing the issue holds in place of a change list (workflow.RenderNews). An unavailable
// pair is never one, since nothing says what changed.
func (c DesignChanges) WorkUnchanged() bool {
	return c.State == DesignChangesRead && (c.DecisionOnly || c.Unchanged)
}

// SectionChange is one top-level unit of a document that changed between two versions, as
// Dispatch names it (dispatch.SectionChange), and how it changed (`changed`, `added`, `removed`, or
// `moved` for a unit both versions hold with the same text at another place). A `##` section is
// named by its heading and how many sections before it carry that heading, a heading with no
// words counted from 1. The heading "" with occurrence 0 is a run of the text outside every `##`
// section, and Place, AfterHeading and AfterOccurrence say where it stands: above the first
// section (`before_first_section`), or directly after the `##` section the two name
// (`after_section`). A run recorded from a Dispatch that named no place has none, and a `##`
// section never has one.
type SectionChange struct {
	Heading         string `json:"heading"`
	Occurrence      int    `json:"occurrence"`
	Change          string `json:"change"`
	Place           string `json:"place,omitempty"`
	AfterHeading    string `json:"after_heading,omitempty"`
	AfterOccurrence int    `json:"after_occurrence,omitempty"`
}

// BlockChange is one typed block of a document that changed between two versions, by its `#id`,
// with its type and how it changed.
type BlockChange struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Change string `json:"change"`
}

// NewsFacts is what one tree's owed news is read from (OwedNews): the deployment's steps (Steps,
// as `gates.design` names them), the tree root's gate rows by step (Gates), every member of the
// tree with the root among them (Members), the tree's told rows (Told), and the members whose
// pull request has merged (Merged), which the caller reads from each pull request's own state
// (PullRequestMerged) whatever phase the member stands in.
type NewsFacts struct {
	Steps   []DesignStep
	Gates   map[DesignStep]DesignGate
	Members []Issue
	Told    []DesignTold
	Merged  map[string]bool
}

// TreeNews is what one tree owes of its approved design: the members whose holder is owed news,
// in the order the facts list them, what the tree root's architect is owed for the tree (the items
// of Architect, and the opening where ArchitectOpening says so), and why each other member is owed
// nothing (Untold, by the member's key).
type TreeNews struct {
	Members          []MemberNews
	Architect        []NewsItem
	ArchitectOpening bool
	Untold           map[string]UntoldReason
}

// ArchitectOwed says whether the tree root's architect is owed anything: an item, or the opening
// alone.
func (n TreeNews) ArchitectOwed() bool {
	return len(n.Architect) > 0 || n.ArchitectOpening
}

// Member is what the holder of issue's phase is owed, and whether it is owed anything: the one
// walk over Members that every reader of one member's news takes.
func (n TreeNews) Member(issue string) (MemberNews, bool) {
	for _, member := range n.Members {
		if member.Issue == issue {
			return member, true
		}
	}
	return MemberNews{}, false
}

// UntoldReason is why OwedNews owes a member nothing (TreeNews.Untold): the first of these that
// holds, in the order they are declared. It is decided once, there, and read by each reader that
// says why: the root architect's summary, which names it as the member's state (the daemon's
// untoldReason), and the opened message, which adds that nothing the member's issue holds changed
// only for a member whose holder took every approved version that concerns it (workflow's
// openingDelivery). So a new phase with no holder, or a change to stepConcerns, is made here once.
type UntoldReason string

const (
	// UntoldTaken is a member whose holder took every approved version of every step the tree is
	// gated on.
	UntoldTaken UntoldReason = "taken"
	// UntoldNotConcerned is a member whose holder took every approved version that concerns it,
	// and some it has not taken are of a step that does not: criteria, whose checks are the tree
	// root's alone (stepConcerns).
	UntoldNotConcerned UntoldReason = "not-concerned"
	// UntoldMerged is a member whose pull request has merged, in whatever phase it stands, and
	// merged work never moves back.
	UntoldMerged UntoldReason = "merged"
	// UntoldDone is a member whose work is done.
	UntoldDone UntoldReason = "done"
	// UntoldOutOfWorkflow is a member whose status takes it out of the workflow (InWorkflow).
	UntoldOutOfWorkflow UntoldReason = "out-of-workflow"
	// UntoldAdmitted is a member that has not started: its first working entry takes the version
	// approved then as its baseline.
	UntoldAdmitted UntoldReason = "admitted"
	// UntoldAwaitingPerson is a member that waits on a person, whose phase no worker holds.
	UntoldAwaitingPerson UntoldReason = "awaiting-person"
	// UntoldHeld is a member that stands in held, whatever holds it (its stopped task, the issues
	// beneath it or its failed worker): the release that moves it back into a phase carries what it
	// is owed then.
	UntoldHeld UntoldReason = "held"
	// UntoldAwaitingChildren is a parent that stands in awaiting_children while a Dispatch child of
	// it is open: no restart moves it back, and the admission pass's return that moves it back into
	// its phase once no child of it is open (workflow's ReturnFromChildren) carries what it is owed
	// then.
	UntoldAwaitingChildren UntoldReason = "awaiting-children"
	// UntoldHeldBack is a member none of the reasons above explains: its holder has not taken a
	// version that concerns it, and OwedNews holds that news back, because the tree's standing gate
	// is not open or the tree lingers.
	UntoldHeldBack UntoldReason = "held-back"
)

// MemberNews is what the holder of one member's phase is owed: the items it has not taken, none
// where its row owes the opening alone (OwedNews).
type MemberNews struct {
	Issue string
	Phase phase.Phase
	Items []NewsItem
}

// NewsItem is one step whose approved version is news to its audience: the step's document, the
// version the audience took (From, nil where the record does not know it, which the reconcile
// reads from Dispatch's review history, and which stays nil while that history cannot be read),
// and the version the gate stands approved at now (To).
type NewsItem struct {
	Step       DesignStep
	ArtifactID string
	From       *int
	To         int
}

// Pair is the change set the item names, and whether it names one: from From to To. An item whose
// told row knows no version of its step names none, until the reconcile resolves that version into
// the row (Store.FillTold, from Dispatch's review history). While the history cannot be read it
// still names none, and the record says why under its question (Question, DesignHistoryUnread).
func (i NewsItem) Pair() (VersionPair, bool) {
	if i.From == nil {
		return VersionPair{}, false
	}
	return VersionPair{ArtifactID: i.ArtifactID, From: *i.From, To: i.To}, true
}

// Question is the question of Dispatch's review history the item asks while its told row knows no
// version of its step: which version of its document a person approved before To.
func (i NewsItem) Question() DesignHistoryQuestion {
	return DesignHistoryQuestion{ArtifactID: i.ArtifactID, To: i.To}
}

// stepConcerns says whether step concerns issue, a member of tree: spec concerns every member,
// and criteria the tree root alone, since its checks are the root's to prove. It is
// the one statement of that rule, read by what a member is owed and by why it is owed nothing
// (OwedNews, UntoldNotConcerned), which the opened message's clause and the root architect's
// summary read in turn, so a holder is never told that nothing changed while it is owed news.
func stepConcerns(step DesignStep, issue, tree string) bool {
	return step != DesignStepCriteria || claim.IsTreeRoot(issue, tree)
}

// OwedNews is the one rule for what a tree owes of its approved design. A member is owed news of
// a step only when every one of these holds:
//
//   - the tree is gated on the step (TreeSteps): the deployment gates it, or the root registered
//     criteria beside a deployment that gates spec and not criteria;
//   - the tree's standing gate is open (TreeGate.Open), so the first of two `artifact.approved`
//     facts of one answer tells nothing and the second tells once;
//   - the step's approved version is above the version the member's holder took, or the record
//     does not know that version (DesignTold.Took);
//   - the step concerns the member (stepConcerns): spec concerns every member, and criteria the
//     tree root alone, since its checks are the root's to prove;
//   - the member is in the workflow (InWorkflow), its tree does not linger (Issue.Lingers on the
//     tree's root, as TreeLingers reads it), and its phase has a holder, a phase whose work
//     phase.RoleFor names a role for: not awaiting_person, done, held or awaiting_children, which a
//     restart or the return from the children moves back into its phase, and not admitted, which
//     phase.RoleFor names the tree's architect for
//     (dispatch://INFRA-2614) and which comes before any working phase of the member, so its first
//     phase's holder takes its baseline at that phase's start (workflow's Engine.enter);
//   - its pull request has not merged, in whatever phase it stands: merging is the usual case,
//     but a pull request can merge while its issue still stands in reviewing, and merged work
//     never moves back.
//
// A member whose phase has a holder has started its work, so a version the record does not know
// is owed: the holder of work in flight is told what its version cannot be shown to cover. A
// member released after an approval is owed nothing, because its first entry's baseline is that
// approved version.
//
// A member whose holder row owes the opening (DesignTold.OpeningOwed) while the standing gate is
// open and the tree does not linger is owed that opening, with no item where it is owed none, on
// the same rule of its own record. The opening became owed because the holder was owed news when
// the gate opened, and the task that carried that news can be confirmed without it. Either the
// prompt whose turn confirms the task was rendered before the opening became owed (the turn after
// a restart, or the late turn of a prompt whose resend Oh My Pi answered busy), or that prompt
// carried an earlier opening, which a close and a reopening before its turn replaced. So no role
// message told this opening, and a task of its own does (MemberNews with no Items).
//
// Every member owed nothing is named in Untold with the reason, the first that holds of those
// UntoldReason lists: what its holder took, then its own record, then the hold of the standing
// gate or the tree's linger. The hold is read last, so only a member that would be owed news with
// the gate open and the tree running is named for it (UntoldHeldBack).
//
// The root architect's row is owed on its own rule: while the standing gate is open and the tree
// does not linger, each step the tree is gated on whose approved version is above the version
// that row took, or whose version that row does not know, whatever its members are owed, and the
// opening where the row owes it (ArchitectOpening). So a change to the checks alone on a merged
// root, which owes no member, still reaches the architect.
//
// It is pure, and lives here beside the other rules over the record (Waiting, AtWork), so that
// the store's upgrade test can call it and the daemon, the outbox and the send all read the one
// rule.
func OwedNews(facts NewsFacts) TreeNews {
	tree, lingers := "", false
	for _, member := range facts.Members {
		tree = member.Tree
		if claim.IsTreeRoot(member.Key, member.Tree) {
			lingers = member.Lingers()
		}
	}
	if tree == "" {
		return TreeNews{}
	}
	heldBack := lingers || !(TreeGate{Steps: facts.Steps, Rows: facts.Gates}).Open()
	holders := make(map[string]DesignTold, len(facts.Told))
	var architect DesignTold
	for _, told := range facts.Told {
		switch {
		case told.Audience == AudienceHolder:
			holders[told.Issue] = told
		case told.Audience == AudienceArchitect && told.Issue == tree:
			architect = told
		}
	}
	steps := TreeSteps(facts.Steps, facts.Gates)
	var news TreeNews
	for _, member := range facts.Members {
		untaken := owedItems(steps, facts.Gates, holders[member.Key])
		// DeleteFunc rewrites untaken's elements in place, so how many there were is read first.
		owedAll := len(untaken)
		items := slices.DeleteFunc(untaken, func(item NewsItem) bool {
			return !stepConcerns(item.Step, member.Key, member.Tree)
		})
		other := len(items) < owedAll
		owes := len(items) > 0 || (!heldBack && holders[member.Key].OpeningOwed)
		if reason := untold(member, facts.Merged[member.Key], heldBack, owes, other); reason != "" {
			if news.Untold == nil {
				news.Untold = map[string]UntoldReason{}
			}
			news.Untold[member.Key] = reason
			continue
		}
		news.Members = append(news.Members, MemberNews{Issue: member.Key, Phase: member.Phase, Items: items})
	}
	if !heldBack {
		news.Architect = owedItems(steps, facts.Gates, architect)
		news.ArchitectOpening = architect.OpeningOwed
	}
	return news
}

// untold is why OwedNews owes member nothing, the first of the reasons that holds (UntoldReason),
// or "" where member is owed news: owes says its holder has not taken some approved version that
// concerns it, or its row owes the opening while the gate stands open, and other that it has not
// taken one that does not concern it. Every phase no holder works is named before the last case
// (done, admitted, awaiting_person, held, awaiting_children; admitted is the tree architect's,
// which holds no member's phase), so the role check there only keeps a phase the record never
// holds from being owed.
func untold(member Issue, merged, heldBack, owes, other bool) UntoldReason {
	switch {
	case !owes && other:
		return UntoldNotConcerned
	case !owes:
		return UntoldTaken
	case merged:
		return UntoldMerged
	case member.Phase == phase.Done:
		return UntoldDone
	case !InWorkflow(member):
		return UntoldOutOfWorkflow
	case member.Phase == phase.Admitted:
		return UntoldAdmitted
	case member.Phase == phase.AwaitingPerson:
		return UntoldAwaitingPerson
	case member.Phase == phase.Held:
		return UntoldHeld
	case member.Phase == phase.AwaitingChildren:
		return UntoldAwaitingChildren
	case heldBack || phase.RoleFor(member.Phase) == "":
		return UntoldHeldBack
	}
	return ""
}

// owedItems is each of steps whose approved version in gates is news to the audience whose row is
// told: above the version it took, or a version it took that the record does not know. A step the
// gate holds no approval of names nothing. Which of them concern a member is its caller's to weigh
// (stepConcerns); every one concerns the root architect.
func owedItems(steps []DesignStep, gates map[DesignStep]DesignGate, told DesignTold) []NewsItem {
	var items []NewsItem
	for _, step := range steps {
		gate := gates[step]
		if gate.ApprovedVersion == nil {
			continue
		}
		item := NewsItem{Step: step, ArtifactID: gate.ArtifactID, To: *gate.ApprovedVersion}
		if took, known := told.Took(step); known {
			if took >= item.To {
				continue
			}
			item.From = new(took)
		}
		items = append(items, item)
	}
	return items
}
