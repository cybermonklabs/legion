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
func GateDocuments(gates map[DesignStep]DesignGate) []DesignGateDocument {
	documents := make([]DesignGateDocument, 0, len(gates))
	for step, gate := range gates {
		documents = append(documents, DesignGateDocument{Step: step, ArtifactID: gate.ArtifactID})
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
// caller touched.
//
// That read of the issue's gates is given back, so a caller that needs the set the write left —
// to decide what the tree does next — takes this one rather than reading the same rows again.
//
// The comparison is of the pairs alone. A write that moves only a version, a review mark or
// ever_opened leaves them as they were and queues nothing: every review and every version of every
// gated tree reaches this helper, and each would otherwise cost Dispatch a write that says what it
// already holds.
//
// The enqueue goes through the store the caller holds, which is what tells the issue's board lane
// on commit that a publication is due (the daemon's projectRecords), and it is here rather than
// inside Postgres.PutGate because PutGate is also what every test seed writes a gate with: an
// enqueue there would queue a publication under each of them.
func PublishChangedGates(ctx context.Context, store Store, tx pgx.Tx, issue string, before map[DesignStep]DesignGate,
	now time.Time) error {
	after, err := store.GatesOf(ctx, tx, issue)
	if err != nil {
		return err
	}
	documents := GateDocuments(after)
	if slices.Equal(documents, GateDocuments(before)) {
		return nil
	}
	return store.EnqueueDesignGates(ctx, tx, DesignGatesPublish{Issue: issue, Gates: documents}, now)
}
