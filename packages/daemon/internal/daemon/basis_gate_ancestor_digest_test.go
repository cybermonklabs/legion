package daemon

import (
	"encoding/json"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// basisDigestsTaken waits, for each document of an ancestor that a step of key stands on (its
// Basis, open or pending), until the daemon has applied the ancestor's newest event among those it
// always applies as a fact (quiet), and, on a daemon that reads Dispatch's verdict
// (dispatch://INFRA-6065), until Dispatch has filled the digest of every document of the ancestor
// (digestsFilled) and the daemon has applied the document's newest artifact.digest. Such a daemon
// applies an ancestor's digest whichever root stands on the document: a digest saying the person's
// approval no longer stands closes a step open on it (workflow's ancestorVerdictEndsBasis and
// endOpenBasis), and one of a newer version cues a pending step (pendingBasisCue), so without this
// wait a refusal's two snapshots of key can read on either side of it. A daemon that reads no
// verdict decodes no artifact.digest and only acknowledges one, writing no processed row, so the
// digest is waited for only where the daemon's design_gates rows carry digest_owed, which such a
// daemon's schema lacks (digestsTaken reads the same column).
func (r *personHandoverRig) basisDigestsTaken(key string) {
	t := r.t
	t.Helper()
	var bases []record.GateBasis
	for _, gate := range r.gates(key) {
		if gate.Basis != nil && !slices.ContainsFunc(bases, func(basis record.GateBasis) bool { return basis.ArtifactID == gate.Basis.ArtifactID }) {
			bases = append(bases, *gate.Basis)
		}
	}
	if len(bases) == 0 {
		return
	}
	readsVerdict := r.readsDigestVerdict(key)
	for _, basis := range bases {
		if readsVerdict {
			r.digestsFilled(basis.Issue)
		}
		r.quiet(basis.Issue)
		if !readsVerdict {
			continue
		}
		events := r.issueEvents(basis.Issue)
		for i := len(events) - 1; i >= 0; i-- {
			var payload struct {
				ArtifactID string `json:"artifact_id"`
			}
			if events[i].Type == "artifact.digest" && json.Unmarshal(events[i].Payload, &payload) == nil && payload.ArtifactID == basis.ArtifactID {
				r.applied(events[i])
				t.Logf("the daemon applied %s's artifact.digest of version %d (seq %d) of %s, on which a step of %s stands", basis.Issue, events[i].version(),
					events[i].Seq, basis.ArtifactID, key)
				break
			}
		}
	}
}

// readsDigestVerdict says whether the daemon's design_gates rows of key carry digest_owed, the
// column of a daemon that reads Dispatch's verdict on a version (dispatch://INFRA-6065). The rows
// are read whole as JSON, as digestsTaken reads them, since the schema of a daemon that reads no
// verdict has no such column.
func (r *personHandoverRig) readsDigestVerdict(key string) bool {
	t := r.t
	t.Helper()
	var reads bool
	if err := r.pool.QueryRow(t.Context(), `select coalesce(bool_or(to_jsonb(g) ? 'digest_owed'), false) from design_gates g where g.issue = $1`, key).
		Scan(&reads); err != nil {
		t.Fatalf("read whether the design gates of %s carry digest_owed: %v", key, err)
	}
	return reads
}
