package blockers

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Mirror writes each reading of Dispatch into the daemon's copy of Dispatch's issues, and decides
// nothing: it refuses no fact, ever, so the handlers after it see exactly the facts they would
// have seen without it. It runs before them, inside their transaction, because they read the row
// it writes. Only a reading of Dispatch reaches the copy: an issue event, the release's read of
// the children it adopts, and the periodic turn's own re-read of Dispatch, which rides on the
// controller tick (intake.ControllerTick.Observed) and is written here as an event is, so a
// blocker whose closing event never arrived is repaired before those handlers judge. A reading
// writes only the values it carries, and each value keeps the sequence of the reading that wrote
// it: a reading changes it only at a higher sequence, or where no reading wrote it
// (record.PutDispatchIssue). Each issue's Dispatch claim is mirrored beside its row, behind a
// fence of its own (record.PutDispatchClaim): from an issue event or a view that shows it, and
// from Dispatch's issue.claimed and issue.released events (intake.DispatchClaim), which say
// nothing else of the issue and are kept even when they arrive before the issue's first row.
//
// One path it never sees: an intake.ObservedIssue of an issue already recorded whose event was
// prepared in place of none applies no handler at all (intake.settleObserved sets handlers to
// nil), so its view of the issue reaches no mirror write. Nothing depends on that observation —
// the issue's next event, the periodic turn or the next boot's reconcile writes the row, and until
// then the row holds the last thing Dispatch said.
type Mirror struct {
	store   record.Store
	project string
}

var _ intake.Handler = (*Mirror)(nil)

// NewMirror creates the mirror handler for one Dispatch project: the project of the daemon's own
// events and of its own listings, which is what those observations are written under. A row is
// written under the project of the observation itself (put), not this one, since the periodic
// turn also re-reads blockers of other projects.
func NewMirror(store record.Store, project string) *Mirror {
	return &Mirror{store: store, project: project}
}

// Apply writes the row of a Dispatch issue fact, of every child a release of children adopts, as
// that release's own read showed it, and of every row the periodic turn's re-read observed, the
// claim of a claim event, and does nothing for every other kind of fact — a tick that observed
// nothing, which is almost every turn, included.
func (m *Mirror) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	switch fact := fact.(type) {
	case intake.DispatchIssue:
		observed := intake.IssueStatus{Key: fact.Key, Project: m.project, Status: fact.Status, Blockers: fact.Blockers,
			Seq: fact.Seq, ClaimedBy: fact.ClaimedBy, ClaimKnown: fact.ClaimKnown, Parent: fact.Parent, Labels: fact.Labels, Rank: fact.Rank}
		if err := m.put(ctx, tx, readingOf(observed)); err != nil {
			return intake.Result{}, err
		}
	case intake.DispatchClaim:
		if err := m.store.PutDispatchClaim(ctx, tx, fact.Key, fact.ClaimedBy, fact.Seq); err != nil {
			return intake.Result{}, err
		}
	case intake.ChildAdoption:
		// An adopted root is recorded already, so no observation of it reaches a handler
		// (intake's settleObserved) and nothing else would bring the release's read here. Without
		// it the adoption's own promotion, judging by a row written before the person's edit,
		// starts a root that edit blocked (dispatch://INFRA-714/spec R3).
		for _, child := range fact.Children {
			if child.Seq == 0 {
				continue
			}
			// The release's read carries the child's status, blockers and parent, so its reading
			// carries those and leaves every other value as the copy holds it. The parent is among
			// them because the children-first rule (record.ChildWaitsOf) reads the child under the
			// parent that read showed. The project is this daemon's, since a parent link never
			// crosses a project and the release adopts the children of a root of its own; a row
			// written under no project would drop out of every project's read of its children.
			reading := record.DispatchReading{Key: child.Issue, Project: m.project, Seq: child.Seq,
				Status: record.Carry(child.Status), Parent: record.Carry(child.Parent), Blockers: record.Carry(child.BlockedBy)}
			if err := m.put(ctx, tx, reading); err != nil {
				return intake.Result{}, fmt.Errorf("mirror the release's read of %s: %w", child.Issue, err)
			}
		}
	case intake.ControllerTick:
		for _, observed := range fact.Observed {
			if err := m.put(ctx, tx, readingOf(observed)); err != nil {
				return intake.Result{}, fmt.Errorf("mirror the turn's re-read of %s: %w", observed.Key, err)
			}
		}
	}
	return intake.Result{}, nil
}

// put writes one reading of Dispatch into the copy, under the project the reading carries — the
// project of the read that saw the issue, which for an event or a listing of the daemon's own
// project is that project and for a blocker the turn re-read is whatever project its key names —
// and behind the sequences PutDispatchIssue keeps: the reading changes only the values it
// carries, each only where no reading has written it or an older one did, and one that does not
// show the claim leaves the claim as it stands.
func (m *Mirror) put(ctx context.Context, tx pgx.Tx, reading record.DispatchReading) error {
	return m.store.PutDispatchIssue(ctx, tx, reading)
}

// readingOf is observed as a reading of Dispatch at its sequence: it carries all five values
// observed, the status, the parent, the blockers, the labels and the rank, no parent being one of
// the things it can have seen, so a child a person took out from under its parent is copied as
// the issue Dispatch now shows and ends that parent's children-first wait. A claim observed is the
// claim at that sequence.
func readingOf(observed intake.IssueStatus) record.DispatchReading {
	reading := record.DispatchReading{Key: observed.Key, Project: observed.Project, Seq: observed.Seq,
		Status: record.Carry(observed.Status), Parent: record.Carry(observed.Parent), Blockers: record.Carry(observed.Blockers),
		Labels: record.Carry(observed.Labels), Rank: record.Carry(observed.Rank)}
	if observed.ClaimKnown {
		reading.ClaimedBy, reading.ClaimSeq = observed.ClaimedBy, observed.Seq
	}
	return reading
}

// Observed is one summary of a Dispatch listing of project as an observation of that issue, the
// facts a row of the copy is written from: the boot's reconcile and the periodic turn's listing
// both read the project this way, and both write what it returns. A listing is of one project, so
// project is the project of every summary in it, and the row carries it. A listing carries all
// five values, always shows the claim, and shows the parent of every issue the project has,
// recorded by the daemon or not. Each value it shows replaces one an older reading wrote, so the
// first listing after a boot leaves every issue of the project under the parent of the newest
// reading of it, which is what makes that listing the moment the children-first rule is complete.
func Observed(project string, summary dispatch.IssueSummary) intake.IssueStatus {
	parent := ""
	if summary.Parent != nil {
		parent = *summary.Parent
	}
	return intake.IssueStatus{Key: summary.Key, Project: project, Status: summary.Status, Blockers: summary.BlockedBy,
		Seq: summary.LastSeq, ClaimedBy: summary.ClaimedBy, ClaimKnown: !summary.ClaimUnread, Parent: parent,
		Labels: summary.Labels, Rank: summary.Rank}
}

// Reconcile mirrors the boot listing: every summary Dispatch returned for project, as the same
// reading of all five values a live event is, so a listing older than an event already applied
// changes nothing that event wrote.
func Reconcile(ctx context.Context, tx pgx.Tx, store record.Store, project string, summaries []dispatch.IssueSummary) error {
	mirror := NewMirror(store, project)
	for _, summary := range summaries {
		if err := mirror.put(ctx, tx, readingOf(Observed(project, summary))); err != nil {
			return fmt.Errorf("mirror the boot listing of %s: %w", summary.Key, err)
		}
	}
	return nil
}
