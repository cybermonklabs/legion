package replay

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// defaultBranch is the base branch a start state records an issue on where the window's merges name
// none for its repository.
const defaultBranch = "main"

// seedSession is the session a claim the start state writes runs where the record shows no session
// running that claim (IssueTrace.sessionAt), which no worker of the replay's own takes.
const seedSession = "replay-seed"

// Seed writes into a replay's database, in tx, the state t's record shows at at (assumption 6): what
// every line and event before at made, and nothing at or after it, which the replay applies itself.
// It writes the Dispatch mirror row of every issue an event before at shows, with the Dispatch
// claim the newest event before at that shows one shows; the record of every
// issue that entered the workflow before at, with its phase (held, or awaiting a person, where the
// trace holds that wait at at), Dispatch status, rank, parent, tree, generation, hold, awaiting and
// whether it is handed to Legion, and a lingering root's linger; the design gate rows of each such
// issue's current run, what each holder of a gated tree's work took of its approved design, as the
// daemon's upgrade to the release that records it writes it (told), and the mirror row of each ask
// on such a root; the mark that the architect owning an issue was told of its children wait, where
// the record told it in its tree's current run (childrenToldMarks); the slots the issues hold, the
// trace's inferred slots among them (assumption 34), and the mark of an issue whose slot went back
// for a wait on another issue; the tree lifecycle of each tree that runs or lingers, and the closed
// lifecycle of each other tree the trace shows promoted before the start; the claims of
// the turns under way of the issues in the workflow, as the outbox's start writes a claim (its
// given entry and a confirmed delivery), and of the architect of each root that waits on a person
// between its turns (waitingArchitects), each running the session the record shows running it;
// seeded live claims retain a death the reach records and hold no start failure after readiness;
// the owed start of each issue whose work came back while it held no slot, marked to wait for one
// and created at the moment its work came back, carrying the resume task engine composes for it
// (workflow.Engine.ResumeTask); and a synthetic open pull request for each issue the trace has seen
// one of. records writes the record's tables and claims the claims.
//
// It names each issue it cannot place, with the reason: one Dispatch shows no event or no
// repository of, one in the workflow whose turn stands in a tree the record shows running no
// lifecycle, one a fact the trace could not place leaves in doubt, and one the daemon's own
// judgement of the line, read back from what Seed wrote, finds held by no word it has
// (record.HeldUnclassified). A fact leaves an issue in doubt where it bears on what Seed writes for
// it (bearsOnStart): its phase and entry, its slot, its hold, its linger. A turn fact bears on none,
// and a fact of a linger only where the linger it closes stands at at.
//
// A turn that stands at at of an issue outside the workflow then — one that left it, or a member
// of a tree that closed — is a production check's, which changes nothing of a replay (assumption
// 30): Seed writes no claim of it and names no issue for it, and SeedAndReport counts it. Seed
// answers the issues it names; SeedAndReport answers its whole report (SeedReport).
func Seed(ctx context.Context, tx pgx.Tx, records record.Store, claims *store.Store, engine *workflow.Engine, t Trace,
	at time.Time,
) ([]Unplaced, error) {
	report, err := SeedAndReport(ctx, tx, records, claims, engine, t, at)
	if err != nil {
		return nil, err
	}
	return report.Unplaced, nil
}

// SeedReport is what a start state reports beside what it writes (SeedAndReport): the issues it
// names, the turns it wrote no claim of, the slots it wrote that the trace inferred, and the
// trace's check of its slots against the record's own count, for which it names no issue.
type SeedReport struct {
	// Unplaced is each issue the start state could not place, once for each reason (Seed).
	Unplaced []Unplaced
	// LeftOut is how many turns standing at the start the start state wrote no claim of, by the
	// number of the assumption each rests on: the turns of the issues outside the workflow, a
	// production check's (assumption 30).
	LeftOut map[int]int
	// InferredSlots is how many of the slots the start state wrote the trace inferred an issue held
	// with no slot line of it (SlotSpan.Inferred, assumption 34).
	InferredSlots int
	// SlotCount is the trace's check of the slots it holds against the slots in use the slot line at
	// the window's start says (Trace.SlotCount), whose difference is no issue's.
	SlotCount SlotCount
	// ChildrenTold is the moment the record told each issue's children wait whose telling the start
	// state marked (childrenToldMarks), by key: the moment that wait began.
	ChildrenTold map[string]time.Time
}

// SeedAndReport writes the start state Seed writes and answers its whole report (SeedReport).
// engine is the engine the replay runs, which composes the resume task of each owed start the
// state seeds (workflow.Engine.ResumeTask), so that task is the text the daemon's own start of a
// worker already in its phase carries.
func SeedAndReport(ctx context.Context, tx pgx.Tx, records record.Store, claims *store.Store, engine *workflow.Engine, t Trace,
	at time.Time,
) (SeedReport, error) {
	token, err := claim.ProjectToken(t.Project)
	if err != nil {
		return SeedReport{}, fmt.Errorf("replay: seed the start state of %s: %w", t.Project, err)
	}
	s := seeder{ctx: ctx, tx: tx, records: records, claims: claims, engine: engine, t: t, at: at, rule: t.ruleAt(at), token: token,
		placed: map[string]record.Issue{}, epochs: map[string]uint64{}, slotted: map[string]struct{}{}, leftOut: map[int]int{},
		childrenTold: map[string]time.Time{}}
	keys := slices.Sorted(maps.Keys(t.Issues))
	for _, key := range keys {
		if err := s.mirror(t.Issues[key]); err != nil {
			return SeedReport{}, err
		}
	}
	for _, key := range keys {
		if err := s.issue(t.Issues[key]); err != nil {
			return SeedReport{}, err
		}
	}
	for _, fn := range []func() error{s.gates, s.told, s.childrenToldMarks, s.asks, s.slots, s.membersOwed, s.lifecycles,
		s.closedLifecycles, s.turns, s.waitingArchitects, s.owedStarts, s.pullRequests, s.unclassified} {
		if err := fn(); err != nil {
			return SeedReport{}, err
		}
	}
	for _, fact := range startDoubts(t, at) {
		if fact.At.Before(at) {
			s.unplaced(fact.Issue, "the trace could not place a fact of it before the window's start: %s", fact.Reason)
		} else {
			s.unplaced(fact.Issue, "the trace could not place a fact of it that stands at the window's start: %s", fact.Reason)
		}
	}
	return SeedReport{Unplaced: s.report, LeftOut: s.leftOut, InferredSlots: s.inferred, SlotCount: t.SlotCount,
		ChildrenTold: s.childrenTold}, nil
}

// startDoubts is each fact of t the trace could not place that leaves in doubt what a start state
// at at writes (bearsOnStart), of an issue the trace holds: the facts a start state names its
// issues for (SeedAndReport), and a record's scorecard counts (unplacedAtStart).
func startDoubts(t Trace, at time.Time) []UnplacedFact {
	var doubts []UnplacedFact
	for _, fact := range t.Unplaced {
		if _, considered := t.Issues[fact.Issue]; considered && bearsOnStart(fact, at) {
			doubts = append(doubts, fact)
		}
	}
	return doubts
}

// bearsOnStart says whether fact, which the trace could not place, leaves in doubt what a start
// state at at writes. A fact of a turn bears on none: it is of a turn that ended at the fact's own
// moment (UnplacedTurn), and the claims written are of the turns standing at at. A fact of a linger,
// the close of one the trace does not hold, bears on the start where that linger stands at at, its
// close at or after it: a linger closed before at leaves its tree closed at at whether the trace
// held its start or not. Every other fact bears on the start where it came before at.
func bearsOnStart(fact UnplacedFact, at time.Time) bool {
	switch fact.Kind {
	case UnplacedTurn:
		return false
	case UnplacedLinger:
		return !fact.At.Before(at)
	}
	return fact.At.Before(at)
}

// seeder is SeedAndReport's state while it writes one start state.
type seeder struct {
	ctx     context.Context
	tx      pgx.Tx
	records record.Store
	claims  *store.Store
	// engine is the engine the replay runs, which composes the resume task of each owed start this
	// state seeds (owedStarts, workflow.Engine.ResumeTask). The one reader that builds a state
	// without writing it (startRecord's caller) needs none and holds nil.
	engine *workflow.Engine
	t      Trace
	at     time.Time
	rule   record.SlotRule
	// token is the project's claim token, which claims and tree lifecycles are keyed by.
	token string
	// placed is the record written for each issue, and epochs the epoch of each tree lifecycle
	// opened, by its root.
	placed map[string]record.Issue
	epochs map[string]uint64
	// slotted is each issue the start state wrote a slot row or the mark of a release for a wait
	// for (slots), which the mark of a return still owed to a tree's members reads (membersOwed).
	slotted map[string]struct{}
	// report is each issue named, leftOut the turns written no claim of by assumption, inferred
	// the slots written that the trace inferred, and childrenTold the children waits whose telling
	// was marked (SeedReport).
	report       []Unplaced
	leftOut      map[int]int
	inferred     int
	childrenTold map[string]time.Time
}

// unplaced names an issue the start state cannot place, once for each reason.
func (s *seeder) unplaced(issue, format string, args ...any) {
	reason := fmt.Sprintf(format, args...)
	if !slices.Contains(s.report, Unplaced{Issue: issue, Reason: reason}) {
		s.report = append(s.report, Unplaced{Issue: issue, Reason: reason})
	}
}

// standsAt says whether a stretch from start to end (zero while it stands) stands at at: it began
// before at and had not ended, an end at at itself being a fact the replay applies.
func standsAt(start, end, at time.Time) bool {
	return start.Before(at) && (end.IsZero() || !end.Before(at))
}

// dispatchBefore is the newest issue event of iss before at, and the highest sequence of any of its
// events before it; nil where none is.
func dispatchBefore(iss *IssueTrace, at time.Time) (*DispatchEvent, int64) {
	var newest *DispatchEvent
	var seq int64
	for i := range iss.Dispatch {
		event := &iss.Dispatch[i]
		if !event.At.Before(at) {
			continue
		}
		seq = max(seq, event.Seq)
		if event.Issue != nil && (newest == nil || event.Seq > newest.Seq) {
			newest = event
		}
	}
	return newest, seq
}

// claimBefore is the newest event of iss before at that shows its Dispatch claim
// (DispatchEvent.ClaimShown), nil where none does: the claim the daemon's mirror held at at, which
// takes each claim an event shows over one an older event showed.
func claimBefore(iss *IssueTrace, at time.Time) *DispatchEvent {
	var newest *DispatchEvent
	for i := range iss.Dispatch {
		event := &iss.Dispatch[i]
		if event.ClaimShown && event.At.Before(at) && (newest == nil || event.Seq > newest.Seq) {
			newest = event
		}
	}
	return newest
}

// mirror writes iss's Dispatch mirror row as its newest issue event before the start shows it, and
// its Dispatch claim as the newest event before the start that shows one shows it (claimBefore),
// through the daemon's own writer of the copy (blockers.Mirror), as the daemon writes a reading of
// an event and a claim event: the copy of Dispatch's issues is written by a reading alone, and the
// record's newest events before the start are the readings the start state stands on. So every
// read of an issue's Dispatch claim (record.Store.DispatchClaim, workflow.Unresumable's at the
// first pass among them) answers from the start as the record's daemon read it then.
func (s *seeder) mirror(iss *IssueTrace) error {
	if event, _ := dispatchBefore(iss, s.at); event != nil {
		shown := event.Issue
		fact := intake.DispatchIssue{Key: iss.Key, Seq: event.Seq, Status: shown.Status, Parent: shown.Parent,
			Blockers: append([]string{}, shown.BlockedBy...), Labels: append([]string{}, shown.Labels...), Rank: shown.Rank}
		if _, err := blockers.NewMirror(s.records, cmp.Or(event.Project, iss.Project)).Apply(s.ctx, s.tx, fact); err != nil {
			return fmt.Errorf("replay: seed the mirror row of %s: %w", iss.Key, err)
		}
	}
	if event := claimBefore(iss, s.at); event != nil {
		claimed := intake.DispatchClaim{Key: iss.Key, Seq: event.Seq, ClaimedBy: event.Claim}
		if _, err := blockers.NewMirror(s.records, iss.Project).Apply(s.ctx, s.tx, claimed); err != nil {
			return fmt.Errorf("replay: seed the Dispatch claim of %s: %w", iss.Key, err)
		}
	}
	return nil
}

// run is iss's latest entry before the start and the visits of that run, nil where it had entered
// none.
func (s *seeder) run(iss *IssueTrace) (*Entry, []Visit) {
	var entry *Entry
	for i := range iss.Entries {
		if iss.Entries[i].At.Before(s.at) {
			entry = &iss.Entries[i]
		}
	}
	if entry == nil {
		return nil, nil
	}
	var visits []Visit
	for _, visit := range iss.Visits {
		if !visit.Enter.Before(entry.At) && visit.Enter.Before(s.at) {
			visits = append(visits, visit)
		}
	}
	return entry, visits
}

// waitAt is iss's wait of kind that stands at the start, nil for none.
func (s *seeder) waitAt(iss *IssueTrace, kind WaitKind) *Wait {
	for i := range iss.Waits {
		if wait := &iss.Waits[i]; wait.Kind == kind && standsAt(wait.Start, wait.End, s.at) {
			return wait
		}
	}
	return nil
}

// visitAt is the visit of visits that stands at the start, nil for none.
func (s *seeder) visitAt(visits []Visit) *Visit {
	for i := range visits {
		if standsAt(visits[i].Enter, visits[i].Exit, s.at) {
			return &visits[i]
		}
	}
	return nil
}

// issue writes iss's record as the start finds it (startRecord), where it had entered the workflow.
func (s *seeder) issue(iss *IssueTrace) error {
	issue, unplaced := s.startRecord(iss)
	if unplaced != "" {
		s.unplaced(iss.Key, "%s", unplaced)
	}
	if issue == nil {
		return nil
	}
	if err := s.records.PutIssue(s.ctx, s.tx, *issue); err != nil {
		return fmt.Errorf("replay: seed the record of %s: %w", iss.Key, err)
	}
	// The hold's beginning is the one column of the hold PutIssue keeps as stored: the move into
	// held writes it beside the record (workflow's claimFailed).
	if issue.Hold != nil && issue.Hold.Since != nil {
		if err := s.records.MarkHoldBegan(s.ctx, s.tx, iss.Key, *issue.Hold.Since); err != nil {
			return fmt.Errorf("replay: seed when the hold of %s began: %w", iss.Key, err)
		}
	}
	s.placed[iss.Key] = *issue
	return nil
}

// startRecord is iss's record as the start finds it: its phase (held, or awaiting a person, where
// the trace holds that wait at the start), Dispatch status, rank, priority, parent, tree,
// generation, hold, awaiting and whether it is handed to Legion, and a lingering root's linger. Its
// priority is its own as its newest Dispatch event before the start shows it, as the daemon's
// intake reads each event's (record.Issue.Priority), or, where the replay's copy of Dispatch shows
// the priority Dispatch resolves in its place (Trace.shownPriorities), the one standing just
// before the start. Nil where it had not entered the workflow, and nil with the reason where it
// had and its record cannot be placed.
func (s *seeder) startRecord(iss *IssueTrace) (*record.Issue, string) {
	entry, visits := s.run(iss)
	if entry == nil {
		return nil, ""
	}
	event, seq := dispatchBefore(iss, s.at)
	switch {
	case event == nil:
		return nil, fmt.Sprintf("it runs in the workflow and the window holds no Dispatch event of it before %s", stamp(s.at))
	case iss.Repo == "":
		return nil, "Dispatch names no repository of it, which the daemon records every issue on"
	}
	shown := event.Issue
	priority := shown.Priority
	if resolved := s.t.shownPriorities; resolved != nil {
		priority = resolved.before(iss.Key, s.at)
	}
	issue := record.Issue{Key: iss.Key, Tree: entry.Tree, Project: s.t.Project, Title: cmp.Or(iss.Title, iss.Key), Phase: phase.Done,
		Generation: entry.Generation, Status: shown.Status, DispatchStatus: shown.Status, Rank: shown.Rank,
		Priority: priority, LastDispatchSeq: seq,
		HandedOver: dispatch.CarriesLegionLabel(shown.Labels), Repo: iss.Repo, GitHubRepo: iss.Repo,
		DefaultBranch: cmp.Or(s.t.Branches[iss.Repo], defaultBranch)}
	if shown.Parent != "" {
		parent := shown.Parent
		issue.Parent = &parent
	}
	for _, visit := range visits {
		if visit.Phase != phase.Admitted {
			issue.TakenBack = true
		}
	}
	if visit := s.visitAt(visits); visit != nil {
		issue.Phase = visit.Phase
		if held := s.waitAt(iss, WaitHeld); held != nil {
			since := held.Start
			issue.Phase, issue.Hold = phase.Held, &record.Hold{From: visit.Phase, Since: &since}
			if held.Reason == string(record.HoldEscalated) {
				issue.Hold.Reason = record.HoldEscalated
			}
		} else if person := s.waitAt(iss, WaitPerson); person != nil && visit.Phase == phase.Admitted {
			issue.Phase = phase.AwaitingPerson
			issue.Awaiting = &record.Awaiting{Ask: person.Ask, GateVersion: person.Version, Artifact: person.Artifact,
				Since: record.EventMark{Seq: person.SinceSeq, At: eventAtSeq(iss, person.SinceSeq, person.Start)}}
		}
	}
	if entry.Tree == iss.Key {
		for _, linger := range iss.Lingers {
			if !linger.At.Before(entry.At) && standsAt(linger.At, linger.Closed, s.at) {
				until := linger.Until
				issue.LingerUntil = &until
			}
		}
	}
	return &issue, ""
}

// startRecords is the record t's start state at at writes for each issue of keys that it places
// (seeder.startRecord), by key: none for an issue that had not entered the workflow by then, or
// whose record it cannot place.
func startRecords(t Trace, at time.Time, keys []string) map[string]record.Issue {
	s := seeder{t: t, at: at}
	placed := make(map[string]record.Issue, len(keys))
	for _, key := range keys {
		if iss, known := t.Issues[key]; known {
			if issue, _ := s.startRecord(iss); issue != nil {
				placed[key] = *issue
			}
		}
	}
	return placed
}

// eventAtSeq is the time of iss's event of sequence seq, or otherwise when where the window holds
// none of that sequence.
func eventAtSeq(iss *IssueTrace, seq int64, otherwise time.Time) time.Time {
	for _, event := range iss.Dispatch {
		if event.Seq == seq {
			return event.At
		}
	}
	return otherwise
}

// placedKeys is the keys of the issues the start state recorded, in order.
func (s *seeder) placedKeys() []string { return slices.Sorted(maps.Keys(s.placed)) }

// inWorkflow says whether a placed record stands in the workflow: a phase short of done.
func inWorkflow(issue record.Issue) bool { return issue.Phase != phase.Done }

// gates writes the design gate rows of each placed issue's current run: each step's document and
// latest version, the version approved with the Dispatch event of its approval, the changes a
// person requested, and whether the gate ever opened, as the gate lines and the verdicts before the
// start leave them.
func (s *seeder) gates() error {
	for _, key := range s.placedKeys() {
		iss := s.t.Issues[key]
		entry, _ := s.run(iss)
		steps := map[record.DesignStep]*record.DesignGate{}
		stepOf := func(artifact string) record.DesignStep {
			for step, gate := range steps {
				if gate.ArtifactID == artifact {
					return step
				}
			}
			return record.DesignStepSpec
		}
		for _, fact := range iss.Gates {
			if fact.At.Before(entry.At) || !fact.At.Before(s.at) {
				continue
			}
			step := fact.Step.OrSpec()
			if fact.Kind != GateRegisteredFact {
				step = stepOf(fact.Artifact)
			}
			gate, known := steps[step]
			if !known {
				if fact.Kind != GateRegisteredFact {
					continue
				}
				gate = &record.DesignGate{Issue: key, Step: step}
				steps[step] = gate
			}
			version := fact.Version
			switch fact.Kind {
			case GateRegisteredFact:
				gate.ArtifactID, gate.LatestVersion = fact.Artifact, max(gate.LatestVersion, version)
				if fact.Open {
					gate.ApprovedVersion, gate.ApprovedMark = &version, s.approvalMark(iss, fact)
					gate.EverOpened = true
				}
				if fact.ChangesRequested {
					gate.ToldChangesVersion = &version
				}
			case GateOpenedFact:
				gate.LatestVersion = max(gate.LatestVersion, version)
				gate.ApprovedVersion, gate.ApprovedMark = &version, s.approvalMark(iss, fact)
				gate.ToldChangesVersion, gate.ToldChangesReason, gate.ToldChangesMark = nil, "", record.EventMark{}
				gate.EverOpened = true
			case GateClosedFact:
				gate.LatestVersion = max(gate.LatestVersion, version)
				if fact.Event == string(intake.DispatchArtifactChangesRequested) {
					gate.ApprovedVersion, gate.ApprovedMark = nil, record.EventMark{}
					gate.ToldChangesVersion = &version
				}
			case GateChangesRequested:
				if version == gate.LatestVersion {
					gate.ToldChangesVersion, gate.ToldChangesMark = &version, record.EventMark{Seq: fact.Seq, At: fact.At}
				}
			}
		}
		for _, step := range slices.Sorted(maps.Keys(steps)) {
			if err := s.records.PutGate(s.ctx, s.tx, *steps[step]); err != nil {
				return fmt.Errorf("replay: seed the %s gate of %s: %w", step, key, err)
			}
		}
	}
	return nil
}

// approvalMark is the Dispatch event a gate's opening at fact stands on: the newest approval of its
// document at or before the fact, or, where a registration opened an ungated step, the issue's
// newest event before it at the registration's own time.
func (s *seeder) approvalMark(iss *IssueTrace, fact GateFact) record.EventMark {
	for _, verdict := range slices.Backward(iss.Gates) {
		if verdict.Kind == GateApproved && verdict.Artifact == fact.Artifact && !verdict.At.After(fact.At) {
			return record.EventMark{Seq: verdict.Seq, At: verdict.At}
		}
	}
	_, seq := dispatchBefore(iss, fact.At.Add(time.Nanosecond))
	return record.EventMark{Seq: seq, At: fact.At}
}

// told writes what each holder of a gated tree's work took of its approved design at the start
// (record.DesignTold), by the rule the daemon's own upgrade to the release that records it seeds a
// database with (internal/store's migration 20261119203047_design_news): a holder row of each
// placed issue whose work has started — its status is none the workflow leaves, and the phase its
// work stands at (record.Issue.WorkingPhase) is none of admitted, awaiting_person and done — and
// whose tree root holds gate rows; and an architect row of each such root of whose tree a holder
// row was written, or which holds an approved version while an issue of its tree stands short of
// done in the workflow. Each row is at the versions the root's gate rows stand approved at
// (record.Store.BaselineTold), the empty set where they hold none.
//
// A window recorded under a release before that one holds no record of what anyone took, and what
// it shows is work in flight on the version approved when it started or since: the start owes
// nobody news of what the record shows taken, as the upgrade owes nobody, and the replay's news is
// of what a person approves inside the window. A later release's window shows what its daemon told
// each holder in no line or event, and its record is read the same way. The work the replay starts
// takes its own baseline at its first entry (workflow's Engine.enter), and a tree's first opening
// its architect's (workflow's putGateTold).
func (s *seeder) told() error {
	gates := map[string]map[record.DesignStep]record.DesignGate{}
	rowsOf := func(tree string) (map[record.DesignStep]record.DesignGate, error) {
		rows, read := gates[tree]
		if read {
			return rows, nil
		}
		rows, err := s.records.GatesOf(s.ctx, s.tx, tree)
		if err != nil {
			return nil, fmt.Errorf("replay: read the gate of %s for what its holders took: %w", tree, err)
		}
		gates[tree] = rows
		return rows, nil
	}
	started := map[string]bool{}
	live := map[string]bool{}
	for _, key := range s.placedKeys() {
		issue := s.placed[key]
		rows, err := rowsOf(issue.Tree)
		if err != nil {
			return err
		}
		if len(rows) == 0 || !record.InWorkflow(issue) {
			continue
		}
		live[issue.Tree] = true
		switch issue.WorkingPhase() {
		case phase.Admitted, phase.AwaitingPerson, phase.Done:
			continue
		}
		if err := s.records.BaselineTold(s.ctx, s.tx, key, record.AudienceHolder, issue.Tree, s.at); err != nil {
			return fmt.Errorf("replay: seed what the holder of %s took: %w", key, err)
		}
		started[issue.Tree] = true
	}
	for _, tree := range slices.Sorted(maps.Keys(gates)) {
		approved := false
		for _, gate := range gates[tree] {
			approved = approved || gate.ApprovedVersion != nil
		}
		if !started[tree] && !(approved && live[tree]) {
			continue
		}
		if err := s.records.BaselineTold(s.ctx, s.tx, tree, record.AudienceArchitect, tree, s.at); err != nil {
			return fmt.Errorf("replay: seed what the architect of %s took: %w", tree, err)
		}
	}
	return nil
}

// childrenToldMarks writes the mark that the architect owning each placed issue in the workflow was
// told of its children wait (record.Store.TellChildrenFirst), at its tree root's generation, where
// the record told it in its tree's current run before the start: a line of a children wait of the
// issue (Wait.Told) in the tree it stands in now, at or after its root's latest entry, a
// re-admission opening a run the daemon tells anew. The daemon tells a wait once for its tree's
// generation (admit's tellChildrenFirst), so without the mark the replay's daemon would tell again,
// at its first pass, a wait the record told before the start: a notice the record never sent and a
// line of a wait the trace would read as a second one. Each moment it marks is reported
// (SeedReport.ChildrenTold).
func (s *seeder) childrenToldMarks() error {
	for _, key := range s.placedKeys() {
		issue, iss := s.placed[key], s.t.Issues[key]
		root, placed := s.placed[issue.Tree]
		if !inWorkflow(issue) || !placed {
			continue
		}
		// A placed root is one the trace holds, placed in its current run.
		run, _ := s.run(s.t.Issues[issue.Tree])
		i := slices.IndexFunc(iss.Waits, func(wait Wait) bool {
			return wait.Kind == WaitChildren && wait.Told && !wait.Start.Before(run.At) && wait.Start.Before(s.at) &&
				treeOf(iss, wait.Start) == issue.Tree
		})
		if i < 0 {
			continue
		}
		told := iss.Waits[i].Start
		if _, err := s.records.TellChildrenFirst(s.ctx, s.tx, key, issue.Tree, root.Generation, told); err != nil {
			return fmt.Errorf("replay: seed the telling of %s's children wait: %w", key, err)
		}
		s.childrenTold[key] = told
	}
	return nil
}

// heldSlot is the slot iss holds at the start, and whether it holds one.
func (s *seeder) heldSlot(iss *IssueTrace) (SlotSpan, bool) {
	for _, span := range iss.Slots {
		if standsAt(span.Start, span.End, s.at) {
			return span, true
		}
	}
	return SlotSpan{}, false
}

// asks writes the mirror row of each ask on a placed root of its own tree as its events before the
// start leave it (workflow's mirrorAsk): its kind and document, open on the person's turn from its
// opening, closed by a person or by someone else with the person's answer or other act on it, and
// whether a session of the root's tree opened it: one the record shows running a claim of a placed
// issue of that tree (actorClaimOf), as the daemon reads the claims of the tree for the session
// (record.Store.SessionClaimsTree). A root that waits on a person's answer waits on such a row.
func (s *seeder) asks() error {
	for _, key := range s.placedKeys() {
		issue, iss := s.placed[key], s.t.Issues[key]
		if issue.Tree != key {
			continue
		}
		rows := map[string]*record.RootAsk{}
		for _, event := range iss.Dispatch {
			if event.Ask == nil || !event.At.Before(s.at) {
				continue
			}
			row, known := rows[event.Ask.ID]
			if !known {
				row = &record.RootAsk{Issue: key, Ask: event.Ask.ID}
				rows[event.Ask.ID] = row
			}
			if event.Ask.Kind == record.AskQuestion || event.Ask.Kind == record.AskApproval {
				row.Kind = event.Ask.Kind
			}
			if event.Ask.Artifact != "" {
				row.Artifact = event.Ask.Artifact
			}
			person := event.Actor.Person()
			switch event.Type {
			case "ask.opened":
				row.Open, row.Seq, row.WaitingOn, row.WaitingOnSeq = true, event.Seq, "human", event.Seq
				if ran, ok := actorClaimOf(event); ok {
					if member, placed := s.placed[ran.Issue]; placed && member.Tree == key {
						row.ByTree = true
					}
				}
			default:
				row.Open, row.Seq, row.ClosedSeq = false, event.Seq, event.Seq
				row.ClosedBy = record.ClosedByOther
				if person {
					row.ClosedBy = record.ClosedByPerson
					if event.Ask.State == "answered" {
						row.PersonAnswerSeq = event.Seq
					} else {
						row.PersonSeq = event.Seq
					}
				}
			}
		}
		for _, ask := range slices.Sorted(maps.Keys(rows)) {
			if err := s.records.PutRootAsk(s.ctx, s.tx, *rows[ask]); err != nil {
				return fmt.Errorf("replay: seed the mirror row of the ask %s of %s: %w", ask, key, err)
			}
		}
	}
	return nil
}

// slots writes the slot of each placed issue that holds one at the start, in the order the slots
// were given out, each with the moment a link or children wait of its holder began where one stands
// beside it, counting each the trace inferred (SeedReport.InferredSlots); and the mark of each issue
// in the workflow whose last slot went back for a wait on another issue and that has taken none
// since, which stands it in the line for a slot of its own again (record.Issue.ReleasedForWaitAt).
func (s *seeder) slots() error {
	type holder struct {
		key  string
		span SlotSpan
	}
	var holders []holder
	for _, key := range s.placedKeys() {
		if span, held := s.heldSlot(s.t.Issues[key]); held && inWorkflow(s.placed[key]) {
			holders = append(holders, holder{key: key, span: span})
		}
	}
	slices.SortStableFunc(holders, func(x, y holder) int { return x.span.Start.Compare(y.span.Start) })
	for index, h := range holders {
		slot := record.Slot{Issue: h.key, Index: index, AdmittedAt: h.span.Start}
		for _, kind := range []WaitKind{WaitLink, WaitChildren} {
			if wait := s.waitAt(s.t.Issues[h.key], kind); wait != nil && (slot.WaitsSince == nil || wait.Start.Before(*slot.WaitsSince)) {
				since := wait.Start
				slot.WaitsSince = &since
			}
		}
		if err := s.records.PutSlot(s.ctx, s.tx, slot); err != nil {
			return fmt.Errorf("replay: seed the slot of %s: %w", h.key, err)
		}
		s.slotted[h.key] = struct{}{}
		if h.span.Inferred {
			s.inferred++
		}
	}
	next := len(holders)
	for _, key := range s.placedKeys() {
		issue, iss := s.placed[key], s.t.Issues[key]
		if _, held := s.heldSlot(iss); held || !inWorkflow(issue) || !s.rule.PhaseKeepsSlot(record.StandsAt(issue)) {
			continue
		}
		var last *SlotSpan
		for i := range iss.Slots {
			if span := &iss.Slots[i]; span.Start.Before(s.at) {
				last = span
			}
		}
		entry, _ := s.run(iss)
		if last == nil || !last.ForWait || last.Start.Before(entry.At) {
			continue
		}
		// The mark is written with the release of a slot row, in one statement (ReleaseSlotForWait),
		// so the row is put first and released at the moment the record shows it went back.
		if err := s.records.PutSlot(s.ctx, s.tx, record.Slot{Issue: key, Index: next, AdmittedAt: last.Start}); err != nil {
			return fmt.Errorf("replay: seed the slot %s gave back for its wait: %w", key, err)
		}
		if err := s.records.ReleaseSlotForWait(s.ctx, s.tx, key, last.End); err != nil {
			return fmt.Errorf("replay: seed the release of %s's slot for its wait: %w", key, err)
		}
		s.slotted[key] = struct{}{}
	}
	return nil
}

// membersOwed marks each re-admitted root whose generation no promotion has opened at the start
// (record.Issue.MembersOwed), so the replay's daemon brings its tree's members back at that
// promotion, or at the first admission pass that finds the tree running, as the daemon of the
// commit it was built from does (admit's returnOwedMembers). A window recorded under a release
// before the mark holds none of it, so the start state writes it as the daemon's upgrade to the
// release that keeps it writes it (store migration 20261123224717_members_owed): on each root of
// its own tree standing in todo at admitted on a generation past the first, with no linger and
// neither a slot nor the mark of a release for a wait (slots). It writes through the store's own
// writer (record.Store.OweMembers), and the placed record carries the mark with it.
func (s *seeder) membersOwed() error {
	for _, key := range s.placedKeys() {
		issue := s.placed[key]
		if _, slotted := s.slotted[key]; slotted || issue.Tree != key || issue.Generation <= 1 || issue.Status != "todo" ||
			issue.Phase != phase.Admitted || issue.LingerUntil != nil {
			continue
		}
		if err := s.records.OweMembers(s.ctx, s.tx, key); err != nil {
			return fmt.Errorf("replay: seed the return owed to the members of %s: %w", key, err)
		}
		issue.MembersOwed = true
		s.placed[key] = issue
	}
	return nil
}

// lifecycles opens, under the workflow's authority, the lifecycle of each tree that runs or
// lingers at the start: each tree whose root the start state recorded in its current run, once
// that run took its first slot or a member of the tree had started (record.Issue.TakenBack, which
// the start state set from the member's run), or the record shows that run running with no slot
// line of it in the log (ranWithNoSlotLine), and whose linger has not closed it.
func (s *seeder) lifecycles() error {
	started := map[string]bool{}
	for _, member := range s.placed {
		if member.TakenBack {
			started[member.Tree] = true
		}
	}
	for _, root := range s.placedKeys() {
		issue := s.placed[root]
		if issue.Tree != root || !inWorkflow(issue) && issue.LingerUntil == nil {
			continue
		}
		iss := s.t.Issues[root]
		entry, _ := s.run(iss)
		opened := started[root] || slices.ContainsFunc(iss.Slots, func(span SlotSpan) bool {
			return span.Start.Before(s.at) && (!span.Start.Before(entry.At) || span.Before)
		}) || ranWithNoSlotLine(iss, entry.At, s.at)
		if !opened {
			continue
		}
		lifecycle, err := s.records.OpenTreeLifecycle(s.ctx, s.tx, s.token, root, treelifecycle.AuthorityWorkflow)
		if err != nil {
			return fmt.Errorf("replay: seed the tree lifecycle of %s: %w", root, err)
		}
		s.epochs[root] = lifecycle.Epoch
	}
	return nil
}

// ranWithNoSlotLine says whether the record shows the run of iss, a root, that it entered at
// entered running before at though no slot line of that run is in the log: its design gate opened
// in the run (a registration that found it open, or an opening line), which is what the daemon
// reads as a run of its own (record's ran, TreeGate.EverOpened), or its architect's claim was
// launched afresh in it (Launch). Either is the daemon's tree at work, whose slot came by a way the
// log holds no line of, so its tree runs a lifecycle as one promoted in the run does.
func ranWithNoSlotLine(iss *IssueTrace, entered, at time.Time) bool {
	inRun := func(moment time.Time) bool { return !moment.Before(entered) && moment.Before(at) }
	return slices.ContainsFunc(iss.Gates, func(fact GateFact) bool {
		return inRun(fact.At) && (fact.Kind == GateOpenedFact || fact.Kind == GateRegisteredFact && fact.Open)
	}) || slices.ContainsFunc(iss.Launches, func(launch Launch) bool {
		return inRun(launch.At) && launch.Role == claim.RoleArchitect && !launch.Resumed
	})
}

// closedLifecycles writes, under the workflow's authority, the lifecycle of each tree whose root
// the start state recorded, that runs no lifecycle at the start (lifecycles) and that the trace
// shows promoted as a tree of its own before it (promotedBefore), with its cleanup reserved and
// confirmed: the row the daemon leaves of a tree it ran and closed, which it never deletes. A
// root's later adoption into another tree reads it (workflow's RanAsRoot), so a root that ran as
// a tree of its own before the window queues as its own tree again in a replay, as it did in the
// record, rather than joining the tree a person moved it under. The row is opened through the
// store's own writer, on its clock, and closed as a tree's cleanup closes it.
func (s *seeder) closedLifecycles() error {
	for _, root := range s.placedKeys() {
		if _, opened := s.epochs[root]; opened || s.placed[root].Tree != root || !promotedBefore(s.t, s.t.Issues[root], s.at) {
			continue
		}
		if _, err := s.records.OpenTreeLifecycle(s.ctx, s.tx, s.token, root, treelifecycle.AuthorityWorkflow); err != nil {
			return fmt.Errorf("replay: seed the closed tree lifecycle of %s: %w", root, err)
		}
		reserved, err := treelifecycle.ReserveCleanup(s.ctx, s.tx, s.token, root, treelifecycle.AuthorityWorkflow)
		if err != nil {
			return fmt.Errorf("replay: seed the closed tree lifecycle of %s: %w", root, err)
		}
		if err := treelifecycle.ConfirmCleanup(s.ctx, s.tx, s.token, root, reserved.Epoch); err != nil {
			return fmt.Errorf("replay: seed the closed tree lifecycle of %s: %w", root, err)
		}
	}
	return nil
}

// promotedBefore says whether t shows iss promoted as a tree of its own before at, which opened
// its tree's lifecycle: a slot it took before at while its entry stood in its own tree (its
// queued-root slot, or one the trace infers it held there), or a linger of its tree, by a `workflow:
// tree lingers` line before at or a `workflow: linger closed the tree` line before at whose linger
// began before the inputs reach (an UnplacedLinger fact). A linger counts where the run it ended
// had entered the workflow before the inputs reach, where the promotion that opened the run is out
// of sight: a root that a person took out of the workflow before any slot of its run also lingers,
// though it opened no lifecycle, and the trace holds that whole run.
func promotedBefore(t Trace, iss *IssueTrace, at time.Time) bool {
	for _, span := range iss.Slots {
		if entry := iss.entryAt(span.Start); span.Start.Before(at) && entry != nil && entry.Tree == iss.Key {
			return true
		}
	}
	for _, linger := range iss.Lingers {
		if entry := iss.entryAt(linger.At); linger.At.Before(at) && (entry == nil || entry.At.Before(t.Start)) {
			return true
		}
	}
	return slices.ContainsFunc(t.Unplaced, func(fact UnplacedFact) bool {
		return fact.Kind == UnplacedLinger && fact.Issue == iss.Key && fact.At.Before(at)
	})
}

// turns writes the claim of each turn under way at the start of a placed issue in the workflow, as
// the outbox's start and the turn's confirmation write it: working in its turn since the turn
// began, bound to its tree's lifecycle, the entry it was given and a delivery confirmed at that
// moment, running the session the record shows running the claim then (seedSession where it shows
// none). A turn whose tree runs no lifecycle the record shows open is one the start state cannot
// place. A turn of an issue outside the workflow at the start is a production check's: it is
// written no claim and counted under assumption 30 (SeedReport.LeftOut), not named.
func (s *seeder) turns() error {
	for _, key := range s.placedKeys() {
		issue, iss := s.placed[key], s.t.Issues[key]
		for _, turn := range iss.Turns {
			if !standsAt(turn.Start, turn.End, s.at) {
				continue
			}
			if !inWorkflow(issue) {
				s.leftOut[assumeNoProductionCheck]++
				continue
			}
			epoch, open := s.epochs[issue.Tree]
			if !open {
				s.unplaced(key, "the turn of its %s from %s stands at %s, and its tree %s runs no lifecycle the record shows open",
					turn.Claim.Role, stamp(turn.Start), stamp(s.at), issue.Tree)
				continue
			}
			var worked phase.Phase
			if record.StandsAt(issue) != phase.Admitted && phaseRole(record.StandsAt(issue)) == turn.Claim.Role {
				worked = record.StandsAt(issue)
			}
			session := cmp.Or(iss.sessionAt(turn.Claim.Role, s.at), seedSession)
			c := supervise.Claim{Token: turn.Claim.Token, Project: s.token, Tree: issue.Tree, Issue: key, Role: turn.Claim.Role,
				Generation: max(turn.Generation, 1), TreeEpoch: epoch, Session: session, State: supervise.StateWorking,
				Handshake: supervise.EntryHandshake{Generation: issue.Generation, QueuedAt: turn.Start, ConfirmedAt: turn.Start},
				LastReady: turn.Start, LastTurnStart: turn.Start}
			s.t.seedDeath(&c, s.at)
			d := supervise.Delivery{ID: "seed:" + string(turn.Claim.Token), Task: "the work the record shows under way at the window's start",
				Phase: worked, Generation: issue.Generation, QueuedAt: turn.Start, DeliveredAt: turn.Start, ConfirmedAt: turn.Start,
				Prompted: true}
			if err := s.claims.PutClaimAndDeliveryTx(s.ctx, s.tx, c, d); err != nil {
				return fmt.Errorf("replay: seed the claim of %s: %w", turn.Claim.Token, err)
			}
		}
	}
	return nil
}

// waitingArchitects writes the claim of the own architect of each placed root that waits on a
// person at the start, with no turn of it under way then, where the record shows the session that
// ran it before the start: idle between its turns, running the latest such session, bound to its
// tree's lifecycle, at the generation and the moments of its last turn. The daemon holds that claim
// through the wait, and its admission pass keeps a waiting root only while the session that holds
// the root's Dispatch claim claims within the tree (workflow.Unresumable), at every pass from the
// first; an ask, a reply or a status write of that session is read by the same comparison. A claim
// the record shows no session of runs no session the daemon could compare, and is left, as the
// claim of every other worker between turns is, to the next start of its role, which the stub
// writes as the daemon's outbox writes it. A root standing at admitted is not one of these: its
// architect's claim beside a phases row that names no entry, as no start state writes one, is
// one admission's sweep raises an entry for (admit's owedOf), work the record's daemon never sent.
func (s *seeder) waitingArchitects() error {
	for _, key := range s.placedKeys() {
		issue, iss := s.placed[key], s.t.Issues[key]
		if !claim.IsTreeRoot(key, issue.Tree) || !issue.AwaitingPerson() {
			continue
		}
		epoch, open := s.epochs[issue.Tree]
		if !open {
			continue
		}
		var last *Turn
		for i := range iss.Turns {
			if turn := &iss.Turns[i]; turn.Claim.Role == claim.RoleArchitect && turn.Start.Before(s.at) {
				last = turn
			}
		}
		if last != nil && standsAt(last.Start, last.End, s.at) {
			continue
		}
		session := ""
		for _, ran := range iss.Sessions {
			if ran.Role == claim.RoleArchitect && ran.First.Before(s.at) {
				session = ran.ID
			}
		}
		if session == "" {
			continue
		}
		token, err := claim.NewToken(s.token, key, claim.RoleArchitect)
		if err != nil {
			return fmt.Errorf("replay: seed the claim of %s's architect: %w", key, err)
		}
		c := supervise.Claim{Token: token, Project: s.token, Tree: issue.Tree, Issue: key, Role: claim.RoleArchitect, Generation: 1,
			TreeEpoch: epoch, Session: session, State: supervise.StateIdle}
		if last != nil {
			c.Generation = max(last.Generation, 1)
			c.LastReady, c.LastTurnStart, c.LastTurnEnd = last.Start, last.Start, last.End
		}
		s.t.seedDeath(&c, s.at)
		if err := s.claims.PutClaimTx(s.ctx, s.tx, c); err != nil {
			return fmt.Errorf("replay: seed the claim of %s: %w", token, err)
		}
	}
	return nil
}

// owedStarts writes the start each placed issue owes the worker of the phase it stands in where
// its work came back to it by a move while it held no slot and no worker of that phase has started:
// a start of that phase's role that waits for the issue's slot (needsSlot), created at the moment
// its work first came back, the move into the first of the visits since then in which no worker
// started, since a later move writes over the owed start and keeps its place (EnqueueStart).
func (s *seeder) owedStarts() error {
	for _, key := range s.placedKeys() {
		issue, iss := s.placed[key], s.t.Issues[key]
		if issue.Phase == phase.Held || issue.Phase == phase.AwaitingPerson || !inWorkflow(issue) {
			continue
		}
		if _, held := s.heldSlot(iss); held {
			continue
		}
		_, visits := s.run(iss)
		at := slices.IndexFunc(visits, func(visit Visit) bool { return standsAt(visit.Enter, visit.Exit, s.at) })
		if at < 0 {
			continue
		}
		// A visit whose worker started did so before it ended, so only the visit standing at the
		// start can have a worker start at or after it.
		unstarted := func(visit Visit) bool {
			return visit.In != nil && s.rule.PhaseKeepsSlot(visit.Phase) && (visit.WorkerStart.IsZero() || !visit.WorkerStart.Before(s.at))
		}
		if !unstarted(visits[at]) {
			continue
		}
		first := at
		for first > 0 && unstarted(visits[first-1]) && visits[first-1].Exit.Equal(visits[first].Enter) {
			first--
		}
		created := visits[first].Enter
		standing := visits[at].Phase
		// The task is the one the daemon's own start of a worker already in its phase carries,
		// composed by the engine the replay runs (workflow.Engine.ResumeTask) rather than written
		// here, so a seeded start and a start the replay makes read alike.
		task, err := s.engine.ResumeTask(s.ctx, s.tx, issue)
		if err != nil {
			return fmt.Errorf("replay: seed the owed start of %s: %w", key, err)
		}
		payload := record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: phaseRole(standing), Task: task,
			Generation: issue.Generation, Phase: standing, NeedsSlot: true}
		row, err := record.NewOutboxRow(key, payload, created)
		if err != nil {
			return fmt.Errorf("replay: seed the owed start of %s: %w", key, err)
		}
		row.CreatedAt = created
		if err := s.records.Enqueue(s.ctx, s.tx, row); err != nil {
			return fmt.Errorf("replay: seed the owed start of %s: %w", key, err)
		}
	}
	return nil
}

// pullRequests writes a synthetic open pull request for each placed issue in the workflow the trace
// has seen one of before the start, on the issue's own repository and branch against its default
// branch, at the number a run gives the issue's record in that repository (pullRequestNumbers),
// its head arrived when the trace first saw the pull request: the record the daemon's guards
// read. An issue that stands at the start in a visit to merging whose merge came
// before it, its close waiting for the issues beneath it (Visit.Merge), holds its pull request
// merged, as the replay's own merge leaves it, so the engine closes it at the last one's leave as
// it closed it in the record.
func (s *seeder) pullRequests() error {
	numbers := newPullRequestNumbers(s.t)
	for _, key := range s.placedKeys() {
		issue, iss := s.placed[key], s.t.Issues[key]
		if !inWorkflow(issue) || iss.PullRequest.IsZero() || !iss.PullRequest.Before(s.at) {
			continue
		}
		number := numbers.of(key, issue.Repo)
		pr := record.PullRequest{Issue: key, Repo: issue.Repo, Number: number, Branch: claim.IssueBranch(key), Base: issue.DefaultBranch,
			HeadUpdatedAt: iss.PullRequest, State: record.PullRequestOpen}
		_, visits := s.run(iss)
		if visit := s.visitAt(visits); visit != nil && visit.Merge != nil && visit.Merge.At.Before(s.at) {
			pr.State, pr.MergeSHA = record.PullRequestMerged, fmt.Sprintf("replay-merge-%s-%d", key, number)
		}
		if err := s.records.PutPullRequest(s.ctx, s.tx, pr); err != nil {
			return fmt.Errorf("replay: seed the pull request of %s: %w", key, err)
		}
	}
	return nil
}

// unclassified reads the line back from what the start state wrote, as the tick of an admission
// pass at the start reads it (blockers.JudgeLine over the project's live issues and slots, under the
// rule and the gate steps the window ran, and record.HeldList over its judgement), and names each
// issue that reading holds by no word it has: a state the record could not have left the daemon
// in, such as a phase held from past the review with no slot.
func (s *seeder) unclassified() error {
	issues, err := s.records.LiveIssues(s.ctx, s.tx, s.t.Project)
	if err != nil {
		return fmt.Errorf("replay: read back the seeded issues of %s: %w", s.t.Project, err)
	}
	slots, err := s.records.Slots(s.ctx, s.tx)
	if err != nil {
		return fmt.Errorf("replay: read back the seeded slots of %s: %w", s.t.Project, err)
	}
	live := make(map[string]bool, len(issues))
	for _, issue := range issues {
		live[issue.Key] = true
	}
	own := slices.DeleteFunc(slots, func(slot record.Slot) bool { return !live[slot.Issue] })
	facts, err := record.ReadLineFactsUnder(s.ctx, s.tx, s.records, s.t.Project, issues, s.t.GateSteps, s.rule)
	if err != nil {
		return fmt.Errorf("replay: read back the seeded line facts of %s: %w", s.t.Project, err)
	}
	_, facts, line, dropped, err := blockers.JudgeLine(s.ctx, s.tx, s.records, s.t.Project, issues, own, facts, nil, s.at)
	if err != nil {
		return fmt.Errorf("replay: judge the seeded line of %s: %w", s.t.Project, err)
	}
	for _, row := range record.HeldList(issues, own, line, dropped, facts) {
		if row.Reason == record.HeldUnclassified {
			issue := s.placed[row.Issue]
			s.unplaced(row.Issue, "at %s it stands in %s with the status %s, and the daemon's judgement of the line holds it by no word (%s)",
				stamp(s.at), issue.Phase, issue.Status, record.HeldUnclassified)
		}
	}
	return nil
}
