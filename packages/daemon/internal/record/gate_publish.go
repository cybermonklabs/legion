package record

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// GateDocuments is the step-to-document pairs of gates, in step order: the set one publication
// carries (DesignGatesPublish). The order is the one a gate runs the steps in (DesignSteps), the
// order every other list of steps in this record takes, so a publication reads as the gate runs.
// An issue whose gates record no document gives an empty set rather than none, which is the set
// that clears what Dispatch holds, and which every path writes the same way, so a cleared issue
// and one published empty again queue the same payload.
//
// Each pair says whether it holds the issue's tree (DesignGateDocument.Holds): whether the steps
// that tree is gated on (TreeSteps over steps, the deployment's list, and gates, the issue's own
// rows) name its step. The rule is the daemon's alone, so Dispatch is told the answer rather than
// left to work it out from the project's list, which does not name a criteria row a root
// registered beside a deployment of spec alone. A caller gives the deployment's steps, never a
// tree's, since this applies TreeSteps itself.
func GateDocuments(steps []DesignStep, gates map[DesignStep]DesignGate) []DesignGateDocument {
	gated := TreeSteps(steps, gates)
	documents := make([]DesignGateDocument, 0, len(gates))
	for step, gate := range gates {
		documents = append(documents, DesignGateDocument{Step: step, ArtifactID: gate.ArtifactID,
			Holds: new(slices.Contains(gated, step))})
	}
	slices.SortFunc(documents, func(a, b DesignGateDocument) int {
		return slices.Index(DesignSteps, a.Step) - slices.Index(DesignSteps, b.Step)
	})
	return documents
}

// PublishChangedGates queues one publication of issue's design-gate documents when the gate write
// the caller has just made moved the issue's step-to-document pairs away from before, the pairs it
// read before that write. Every path that writes a gate calls it, so Dispatch is shown the set the
// record holds whichever path changed it, and the queued set is read after the write, in the
// caller's transaction, so it is the whole set as that write left it rather than the one row the
// caller touched. steps are the deployment's (GateDocuments).
//
// The comparison is of the pairs alone. A write that moves only a version, a review mark or
// ever_opened leaves them as they were and queues nothing: every review and every version of every
// gated tree reaches this helper, and each would otherwise cost Dispatch a write that says what it
// already holds. What each pair says of holding its tree follows from the steps the pairs name and
// the deployment's list, which is the same on both sides, so pairs that stand as they were hold as
// they did.
//
// The enqueue goes through the store the caller holds, which is what tells the issue's board lane
// on commit that a publication is due (the daemon's projectRecords), and it is here rather than
// inside Postgres.PutGate because PutGate is also what every test seed writes a gate with: an
// enqueue there would queue a publication under each of them.
func PublishChangedGates(ctx context.Context, store Store, tx pgx.Tx, steps []DesignStep, issue string,
	before map[DesignStep]DesignGate, now time.Time) error {
	after, err := store.GatesOf(ctx, tx, issue)
	if err != nil {
		return err
	}
	documents := GateDocuments(steps, after)
	if slices.EqualFunc(documents, GateDocuments(steps, before), samePair) {
		return nil
	}
	return store.EnqueueDesignGates(ctx, tx, DesignGatesPublish{Issue: issue, Gates: documents}, now)
}

// samePair is whether two entries of a publication bind the same step to the same document,
// which is all PublishChangedGates compares.
func samePair(a, b DesignGateDocument) bool {
	return a.Step == b.Step && a.ArtifactID == b.ArtifactID
}
