package api

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// The spec of the approval-first case: A, which alice approves, holds the requirement r1 and a
// decision with no reading; G changes one word of r1, which nobody approves; N undoes G's change
// and adds the agent's reading under the decision, so N differs from A by that reading alone.
const (
	approvalFirstRequirement = "The service ships behind a flag."
	approvalFirstChanged     = "The service ships behind a canary."
	approvalFirstSpec        = "# Plan\n\n:::requirement{#r1}\n" + approvalFirstRequirement + "\n:::\n\n:::decision{#d2}\n" + readingDecisionTwo + "\n:::\n"
)

// TestACarryCommitsApprovalFirstWhileTheRootWaitsAtAnUnapprovedVersion holds R14's order where the
// deployed Legion daemon reads it: alice approves version A of a spec; the agent writes G, a change
// she has not approved, and opens its approval request there, where the root waits; then the agent
// writes N, which undoes G's change and adds one reading under the decision. N carries alice's
// approval of A as a readings carry, and its artifact.approved comes before N's artifact.version,
// in event id and in seq, so the daemon's gate reads the approval before the version and never
// resumes the root on a version it reads as unapproved. The carry answers the request the root
// waits on, in alice's name. G's change is a word of a requirement because R14's approval covers
// the spec query's record, which a prose change leaves as it is, so the approval would stand at G
// and no root would wait there.
func TestACarryCommitsApprovalFirstWhileTheRootWaitsAtAnUnapprovedVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	issue := createInteractionIssue(t, handler, "TEST", "Approval first", approvalFirstSpec)
	spec := issue.PrimaryArtifactID
	approval := reviewDocument(t, handler, spec, "alice", "approved", "")
	reviseDocument(t, handler, spec, approvalFirstRequirement, approvalFirstChanged, approval.Version+1)
	waiting := requestApproval(t, handler, spec)
	if waiting.Approval.Version != approval.Version+1 {
		t.Fatalf("the root's request waits at version %d, want G, version %d", waiting.Approval.Version, approval.Version+1)
	}

	carried := approval.Version + 2
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+spec+"/edits", map[string]any{
		"ops": []map[string]string{
			replaceOp(approvalFirstChanged, approvalFirstRequirement),
			replaceOp(readingDecisionTwo, "::::decision{#d2}\n"+readingDecisionTwo+"\n\n:::reading{#r2}\n"+readingOfTwo+"\n:::\n::::"),
		},
		"actor": sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("write N, undoing G and adding a reading: status=%d body=%s", edited.Code, edited.Body.String())
	}
	waitForArtifactVersion(t, handler, spec, carried)

	var reason *string
	if err := database.Pool.QueryRow(ctx, `
		select r.reason from artifact_reviews r
		join artifact_review_carries c on c.review_id = r.id
		where r.artifact_id = $1 and r.version = $2 and r.state = 'approved'`, spec, carried).Scan(&reason); err != nil {
		t.Fatalf("N, version %d, holds no carried approval: %v; want alice's approval of version %d carried as a readings carry", carried, err, approval.Version)
	}
	if reason == nil || *reason != readingCarrySentence(approval.Version) {
		t.Errorf("the carry to N, version %d, reads %v, want the readings carry %q", carried, reason, readingCarrySentence(approval.Version))
	}
	var approvedID, approvedSeq, versionID, versionSeq int64
	if err := database.Pool.QueryRow(ctx, `
		select approved.id, approved.seq, versioned.id, versioned.seq
		from (select id, seq from events
		      where type = 'artifact.approved' and payload->>'artifact_id' = $1 and (payload->>'version')::int = $2
		      order by id desc limit 1) approved,
		     (select id, seq from events
		      where type = 'artifact.version' and payload->>'artifact_id' = $1
		        and (case jsonb_typeof(payload->'version') when 'object' then payload->'version'->>'number' else payload->>'version' end)::int = $2
		      order by id desc limit 1) versioned`, spec, carried).Scan(&approvedID, &approvedSeq, &versionID, &versionSeq); err != nil {
		t.Fatalf("read the readings carry's artifact.approved and N's artifact.version: %v", err)
	}
	if approvedID >= versionID || approvedSeq >= versionSeq {
		t.Errorf("the readings carry to N appended artifact.approved at id %d, seq %d and N's artifact.version at id %d, seq %d while the root waits at G: want the approval first in both",
			approvedID, approvedSeq, versionID, versionSeq)
	}
	var state string
	if err := database.Pool.QueryRow(ctx, `select state from asks where id = $1`, waiting.ID).Scan(&state); err != nil {
		t.Fatalf("read the root's request %s: %v", waiting.ID, err)
	}
	if state != "answered" {
		t.Errorf("the root's request %s reads %q after the readings carry to N, want it answered in alice's name", waiting.ID, state)
	}
}
