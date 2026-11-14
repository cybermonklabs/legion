package record

import (
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
// A holder row rises only where a delivery that carried news is confirmed, or where what it would
// be told is decision blocks alone, which tell nobody anything of the work; so it says what the
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
// change set (DesignChanges). From 0 is a document with no approved version before To.
type VersionPair struct {
	ArtifactID string
	From       int
	To         int
}

// DesignChangesState is how a change set was recorded: read from Dispatch, or recorded as one
// Dispatch could not answer.
type DesignChangesState string

const (
	// DesignChangesRead is Dispatch's answer for the pair.
	DesignChangesRead DesignChangesState = "read"
	// DesignChangesUnavailable is a pair Dispatch could not answer: a release without the changes
	// route (dispatch.ErrNoChangesRoute), a document Dispatch no longer holds
	// (dispatch.ErrDocumentNotFound), or a bounded run of failed reads. Its Reason says which.
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
	// DecisionOnly says the two versions differ only in their decision blocks, so the change
	// tells nobody anything about the work: Sections and Blocks are empty.
	DecisionOnly bool
	Sections     []SectionChange
	Blocks       []BlockChange
	// Reason is why Dispatch could not answer, for an unavailable pair alone.
	Reason string
	ReadAt time.Time
}

// SectionChange is one `##` section of a document that changed between two versions, named by its
// heading and how many sections before it carry that heading, and how it changed (`changed`,
// `added` or `removed`).
type SectionChange struct {
	Heading    string `json:"heading"`
	Occurrence int    `json:"occurrence"`
	Change     string `json:"change"`
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
// in the order the facts list them, and what the tree root's architect is owed for the tree.
type TreeNews struct {
	Members   []MemberNews
	Architect []NewsItem
}

// MemberNews is what the holder of one member's phase is owed.
type MemberNews struct {
	Issue string
	Phase phase.Phase
	Items []NewsItem
}

// HeldByArchitect says the member stands in accepting, whose holder is the architect that owns the
// member — the tree's root architect, or a sub-architect running for it — which the daemon
// resolves when it sends.
func (m MemberNews) HeldByArchitect() bool {
	return m.Phase == phase.Accepting
}

// NewsItem is one step whose approved version is news to its audience: the step's document, the
// version the audience took (From, nil where the record does not know it, which the reconcile
// reads from Dispatch's review history), and the version the gate stands approved at now (To).
type NewsItem struct {
	Step       DesignStep
	ArtifactID string
	From       *int
	To         int
}

// Pair is the change set the item names, and whether it names one: from From to To. An item whose
// told row knows no version of its step names none, until the reconcile resolves that version into
// the row (Store.FillTold, from Dispatch's review history).
func (i NewsItem) Pair() (VersionPair, bool) {
	if i.From == nil {
		return VersionPair{}, false
	}
	return VersionPair{ArtifactID: i.ArtifactID, From: *i.From, To: i.To}, true
}

// StepConcerns says whether step concerns issue, a member of tree: spec and decisions concern
// every member, and criteria the tree root alone, since its checks are the root's to prove. It is
// the one statement of that rule, read by what a member is owed (OwedNews), by the clause the
// opened message adds (workflow's openingDelivery) and by why a member is told nothing (the
// daemon's untoldReason), so a holder is never told that nothing changed while it is owed news.
func StepConcerns(step DesignStep, issue, tree string) bool {
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
//   - the step concerns the member (StepConcerns): spec and decisions concern every member, and
//     criteria the tree root alone, since its checks are the root's to prove;
//   - the member is in the workflow (InWorkflow), its tree does not linger (Issue.Lingers on the
//     tree's root, as TreeLingers reads it), and its phase has a holder (phase.RoleFor): not
//     admitted, awaiting_person, done, or held, which a restart moves back into its phase;
//   - its pull request has not merged, in whatever phase it stands: merging is the usual case,
//     but a pull request can merge while its issue still stands in reviewing, and merged work
//     never moves back.
//
// A member whose phase has a holder has started its work, so a version the record does not know
// is owed: the holder of work in flight is told what its version cannot be shown to cover. A
// member released after an approval is owed nothing, because its first entry's baseline is that
// approved version.
//
// The root architect's row is owed on its own rule: while the standing gate is open and the tree
// does not linger, each step the tree is gated on whose approved version is above the version
// that row took, or whose version that row does not know, whatever its members are owed. So a
// change to the checks alone on a merged root, which owes no member, still reaches the architect.
//
// It is pure, and lives here beside the other rules over the record (Waiting, AtWork), so that
// the store's upgrade test can call it and the daemon, the outbox and the send all read the one
// rule.
func OwedNews(facts NewsFacts) TreeNews {
	if gate := (TreeGate{Steps: facts.Steps, Rows: facts.Gates}); !gate.Open() {
		return TreeNews{}
	}
	tree, lingers := "", false
	for _, member := range facts.Members {
		tree = member.Tree
		if claim.IsTreeRoot(member.Key, member.Tree) {
			lingers = member.Lingers()
		}
	}
	if tree == "" || lingers {
		return TreeNews{}
	}
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
		if !InWorkflow(member) || phase.RoleFor(member.Phase) == "" || facts.Merged[member.Key] {
			continue
		}
		items := owedItems(steps, facts.Gates, holders[member.Key], func(step DesignStep) bool {
			return StepConcerns(step, member.Key, member.Tree)
		})
		if len(items) > 0 {
			news.Members = append(news.Members, MemberNews{Issue: member.Key, Phase: member.Phase, Items: items})
		}
	}
	news.Architect = owedItems(steps, facts.Gates, architect, func(DesignStep) bool { return true })
	return news
}

// owedItems is each of steps that concerns the audience whose row is told and whose approved
// version in gates is news to it: above the version it took, or a version it took that the
// record does not know. A step the open gate holds no approval of names nothing.
func owedItems(steps []DesignStep, gates map[DesignStep]DesignGate, told DesignTold, concerns func(DesignStep) bool) []NewsItem {
	var items []NewsItem
	for _, step := range steps {
		gate := gates[step]
		if !concerns(step) || gate.ApprovedVersion == nil {
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
