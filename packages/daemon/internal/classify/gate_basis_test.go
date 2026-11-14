package classify

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// basisAt is LEGION-100's approval of version 3 of its spec, registered at registered.
func basisAt(registered time.Time) record.GateBasis {
	return record.GateBasis{Issue: "LEGION-100", ArtifactID: "artifact-100", Version: 3,
		Approval: record.EventMark{Seq: 31, At: registered.Add(-time.Hour)}, Approver: "user:alice",
		Reason: "the root's checks serve r1", Session: "ses_architect_208", RegisteredAt: registered}
}

// A step a basis opened changes as a step a person approved at the version registered changes
// (R3 of dispatch://INFRA-3320): a newer version of the root's own document closes it and falls
// back to the person's own approval it kept, never to the version the basis opened; a person's
// changes request at the version registered or later closes it and clears that approval too; a
// person's approval at the version registered or later opens it on that approval and leaves no
// basis behind. A review of an earlier version, delivered late or twice, and a version the step
// already holds change nothing. The ancestor a basis opened the step on stays rested on whatever
// ends the basis. A step that waits on an ancestor's request ends its wait on the same acts.
func TestApplyDesignGateEventEndsABasisAsAPersonsReviewOfTheRootsDocument(t *testing.T) {
	registered := time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)
	personal := record.EventMark{Seq: 9, At: registered.Add(-48 * time.Hour)}
	review := record.EventMark{Seq: 40, At: registered.Add(time.Hour)}
	earlier := record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 4,
		ApprovedVersion: new(2), ApprovedMark: personal}
	open := record.OpenOnBasis(earlier, basisAt(registered))
	pending := record.PendOnBasis(earlier, basisAt(registered), "ask-1")
	closedOn := func(latest int, approved *int, mark record.EventMark, rests string) record.DesignGate {
		return record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: latest,
			ApprovedVersion: approved, ApprovedMark: mark, RestsOn: rests}
	}
	for _, tc := range []struct {
		name    string
		gate    record.DesignGate
		kind    DesignGateEventKind
		version int
		want    record.DesignGate
	}{
		{"a newer version of an open step", open, DesignGateVersion, 5, closedOn(5, new(2), personal, "LEGION-100")},
		{"the registered version again", open, DesignGateVersion, 4, open},
		{"an earlier version delivered late", open, DesignGateVersion, 3, open},
		{"a person's approval at the registered version", open, DesignGateApproved, 4, closedOn(4, new(4), review, "LEGION-100")},
		{"a person's approval of a later version", open, DesignGateApproved, 5, closedOn(5, new(5), review, "LEGION-100")},
		{"a person's approval of an earlier version", open, DesignGateApproved, 3, open},
		{"a changes request at the registered version", open, DesignGateChangesRequested, 4, closedOn(4, nil, record.EventMark{}, "LEGION-100")},
		{"a changes request of an earlier version", open, DesignGateChangesRequested, 3, open},
		{"a newer version of a pending step", pending, DesignGateVersion, 5, closedOn(5, new(2), personal, "")},
		{"a person's approval of a pending step", pending, DesignGateApproved, 4, closedOn(4, new(4), review, "")},
		{"a changes request on a pending step", pending, DesignGateChangesRequested, 4, closedOn(4, nil, record.EventMark{}, "")},
		{"an earlier version's review of a pending step", pending, DesignGateApproved, 3, pending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mark := review
			if tc.kind == DesignGateVersion {
				mark = record.EventMark{}
			}
			if got := ApplyDesignGateEvent(tc.gate, tc.kind, tc.version, mark); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ApplyDesignGateEvent(%s, %d) = %#v, want %#v", tc.kind, tc.version, got, tc.want)
			}
		})
	}
}

// A person's review of the root's document is read against the person's own reviews of it, never
// against the basis (R2, R3): on a step a basis opened, the newest applied review is the person's
// approval the step kept apart, or the changes request it told, so a late or repeated review of an
// earlier version is stale exactly as it would be with no basis, and a review of the version the
// basis opened is not, since no person reviewed that version. A changes request no person's
// approval followed is still the person's to tell, whatever version the basis opened.
func TestLatestReviewReadsThePersonsOwnReviewsOnABasis(t *testing.T) {
	registered := time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)
	personal := record.EventMark{Seq: 9, At: registered.Add(-48 * time.Hour)}
	kept := record.OpenOnBasis(record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4,
		ApprovedVersion: new(2), ApprovedMark: personal}, basisAt(registered))
	told := record.OpenOnBasis(RecordChangesTold(record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4},
		3, "name the rollback path", record.EventMark{Seq: 12}), basisAt(registered))
	none := record.OpenOnBasis(record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4}, basisAt(registered))

	if mark, version, ok := LatestReview(kept); !ok || version != 2 || mark != personal {
		t.Fatalf("LatestReview of the step that kept a person's approval = %#v, %d, %t; want the approval of version 2", mark, version, ok)
	}
	if mark, version, ok := LatestReview(told); !ok || version != 3 || mark != (record.EventMark{Seq: 12}) {
		t.Fatalf("LatestReview of the step that told a changes request = %#v, %d, %t; want the request of version 3", mark, version, ok)
	}
	if _, _, ok := LatestReview(none); ok {
		t.Fatalf("LatestReview of a step no person reviewed reads a review, want none: the basis's version is no review of the root's")
	}

	for _, tc := range []struct {
		name    string
		gate    record.DesignGate
		version int
		mark    record.EventMark
		stale   bool
	}{
		{"an approval of the kept version delivered after it", kept, 2, record.EventMark{Seq: 8}, true},
		{"the kept approval read again", kept, 2, record.EventMark{Seq: 9}, false},
		{"a review of the version the basis opened", kept, 4, record.EventMark{Seq: 8}, false},
		{"a review of the version the basis opened, no person having reviewed", none, 4, record.EventMark{Seq: 1}, false},
		{"an approval of the told version delivered before the request", told, 3, record.EventMark{Seq: 11}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StaleReview(tc.gate, tc.version, tc.mark); got != tc.stale {
				t.Fatalf("StaleReview(%d, %#v) = %t, want %t", tc.version, tc.mark, got, tc.stale)
			}
		})
	}

	if ApprovalFollows(kept, 3, record.EventMark{Seq: 20}) {
		t.Fatalf("a changes request of version 3 reads as followed by an approval on a step a basis opened at version 4, want it the person's to tell: no person approved version 4")
	}
	if !ApprovalFollows(kept, 1, record.EventMark{Seq: 5}) {
		t.Fatalf("a changes request of version 1 reads as followed by nothing, want it followed by the person's own approval of version 2")
	}
	if ApprovalFollows(none, 3, record.EventMark{Seq: 20}) {
		t.Fatalf("a changes request reads as followed by an approval on a step no person approved")
	}
}
