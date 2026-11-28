package intake

//ci:reads packages/contracts/fixtures/hash-gate/** hashGateEvent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// hashGateEvent is the whole Dispatch event one event file of packages/contracts/fixtures/hash-gate,
// the wire contract of Dispatch's hash gate (R1 of dispatch://INFRA-6065), stands for. The file
// holds the event's type and payload alone; the event wraps them in the envelope the issue's event
// log serves and the Envoy envelope carries (id, issue_key, artifact_id, project, seq, type, actor,
// notify, created_at, payload), on ACME-7 at sequence seq and time at, made by actor.
func hashGateEvent(t *testing.T, name string, seq int64, at string, actor dispatch.Actor) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "hash-gate", name))
	if err != nil {
		t.Fatalf("read the hash-gate fixture %s: %v", name, err)
	}
	var file map[string]json.RawMessage
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode the hash-gate fixture %s: %v", name, err)
	}
	var eventType string
	if err := json.Unmarshal(file["type"], &eventType); err != nil || eventType == "" || !isJSONObject(file["payload"]) || len(file) != 2 {
		t.Fatalf("the hash-gate fixture %s is %s, want an event's type and payload alone", name, raw)
	}
	event, err := json.Marshal(map[string]any{
		"id": 9000 + seq, "issue_key": "ACME-7", "artifact_id": nil, "project": "ACME", "seq": seq, "type": eventType,
		"actor": map[string]string{"kind": actor.Kind, "id": actor.ID}, "notify": true, "created_at": at, "payload": file["payload"],
	})
	if err != nil {
		t.Fatalf("encode the event of %s: %v", name, err)
	}
	return event
}

// Intake decodes the hash-gate event fixtures, each in the envelope of its event (hashGateEvent):
// an artifact.version reads digest_owed, true (artifact-version.json) or false
// (artifact-version-filled.json), and its content_sha256, the digest of the version's text; an
// artifact.digest is a DispatchArtifact of its own kind, with its version, its verdict
// (approval_current), the event's mark and actor and its digests, with approved_sha256
// (artifact-digest.json) and without it (that digest with approval_current false and no
// approved-version field, which the fixtures do not hold). Each decodes alike from the event log
// (DecodeDispatchEvent) and from the Envoy envelope on its subject
// (notifications.dispatch.issue.<PROJECT>.<KEY>.<type>). The captured events of a Dispatch that
// sends no digest field decode with none set, and a digest that names no artifact, no positive
// version or no verdict is refused, as the other artifact events are.
func TestHashGateDecodesDigestEvents(t *testing.T) {
	t.Parallel()
	const (
		artifact = "00000000-0000-4000-8000-000000000001"
		// recordDigest is artifact-digest.json's content_sha256, the digest of version 2's record,
		// and its approved_sha256, the approved version's, which matches it; markdownDigest is the
		// content_sha256 of both artifact.version files, the digest of version 2's text.
		recordDigest   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		markdownDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		subjectPrefix  = "notifications.dispatch.issue.ACME.ACME-7."
	)
	mark := func(seq int64, at string) record.EventMark {
		created, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			t.Fatalf("parse the event's created_at %q: %v", at, err)
		}
		return record.EventMark{Seq: seq, At: created}
	}
	architect := dispatch.Actor{Kind: "session", ID: "session-architect"}
	fill := dispatch.Actor{Kind: "system", ID: "digest-fill"}
	digest := hashGateEvent(t, "artifact-digest.json", 41, "2026-11-27T18:00:01.5Z", fill)
	notCurrent := withPayloadFields(t, hashGateEvent(t, "artifact-digest.json", 44, "2026-11-27T18:20:01.5Z", fill), map[string]json.RawMessage{
		"approval_current": json.RawMessage("false"), "approved_version": nil, "approved_sha256": nil,
	})
	for _, tc := range []struct {
		name, eventType string
		event           []byte
		want            DispatchArtifact
	}{
		{name: "a version that owes its digest", eventType: "artifact.version",
			event: hashGateEvent(t, "artifact-version.json", 40, "2026-11-27T18:00:00Z", architect),
			want: DispatchArtifact{Key: "ACME-7", ArtifactID: artifact, Kind: DispatchArtifactVersion, Version: 2,
				Mark: mark(40, "2026-11-27T18:00:00Z"), Actor: architect, DigestOwed: new(true), ContentSHA256: markdownDigest}},
		{name: "a version that owes no digest", eventType: "artifact.version",
			event: hashGateEvent(t, "artifact-version-filled.json", 42, "2026-11-27T18:10:00Z", architect),
			want: DispatchArtifact{Key: "ACME-7", ArtifactID: artifact, Kind: DispatchArtifactVersion, Version: 2,
				Mark: mark(42, "2026-11-27T18:10:00Z"), Actor: architect, DigestOwed: new(false), ContentSHA256: markdownDigest}},
		{name: "a digest that reports the approval current", eventType: "artifact.digest", event: digest,
			want: DispatchArtifact{Key: "ACME-7", ArtifactID: artifact, Kind: DispatchArtifactDigest, Version: 2,
				Mark: mark(41, "2026-11-27T18:00:01.5Z"), Actor: fill, Verdict: new(true), ContentSHA256: recordDigest, ApprovedSHA256: recordDigest}},
		{name: "a digest that reports the approval not current", eventType: "artifact.digest", event: notCurrent,
			want: DispatchArtifact{Key: "ACME-7", ArtifactID: artifact, Kind: DispatchArtifactDigest, Version: 2,
				Mark: mark(44, "2026-11-27T18:20:01.5Z"), Actor: fill, Verdict: new(false), ContentSHA256: recordDigest}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fact, unread, err := DecodeDispatchEvent("ACME", "ACME-7", tc.event)
			if err != nil || len(unread) != 0 {
				t.Fatalf("decode %s from the event log = %v, unread %v; want it decoded whole", tc.event, err, unread)
			}
			if !reflect.DeepEqual(fact, tc.want) {
				t.Fatalf("fact from the event log = %#v, want %#v", fact, tc.want)
			}
			envelope := envelopeJSON(t, fmt.Sprintf("dispatch-%d", tc.want.Mark.Seq), "dispatch", string(tc.event))
			delivered, err := decodeMessage(subjectPrefix+tc.eventType, "ACME", capturedRepositories, envelope)
			if err != nil || delivered.LegacyName || !reflect.DeepEqual(delivered.Fact, tc.want) {
				t.Fatalf("fact from the envelope on %s = %#v (legacy name %t), %v; want %#v", subjectPrefix+tc.eventType, delivered.Fact, delivered.LegacyName, err, tc.want)
			}
		})
	}

	t.Run("the captured artifact events carry no digest field", func(t *testing.T) {
		for _, captured := range []struct{ file, subject string }{
			{"dispatch/artifact-version.json", "notifications.dispatch.issue.CAPTURE-4.artifact.version"},
			{"dispatch/artifact-approved.json", "notifications.dispatch.issue.CAPTURE-4.artifact.approved"},
			{"dispatch/artifact-changes-requested.json", "notifications.dispatch.issue.CAPTURE-3.artifact.changes_requested"},
		} {
			data, err := os.ReadFile("testdata/" + captured.file)
			if err != nil {
				t.Fatalf("read the captured envelope %s: %v", captured.file, err)
			}
			decoded, err := decodeMessage(captured.subject, "CAPTURE", capturedRepositories, data)
			if err != nil {
				t.Fatalf("decode the captured envelope %s: %v", captured.file, err)
			}
			fact, ok := decoded.Fact.(DispatchArtifact)
			if !ok || fact.Kind == DispatchArtifactDigest {
				t.Fatalf("%s decoded to %#v, want a review or version fact", captured.file, decoded.Fact)
			}
			if fact.DigestOwed != nil || fact.Verdict != nil || fact.VerdictMark != (record.EventMark{}) || fact.Latest != 0 ||
				fact.ContentSHA256 != "" || fact.CurrentSHA256 != "" || fact.ApprovedSHA256 != "" {
				t.Fatalf("%s decoded to %#v, want no digest field set from a Dispatch that sends none", captured.file, fact)
			}
		}
	})

	t.Run("a digest Dispatch could not have sent is refused", func(t *testing.T) {
		for _, refused := range []struct {
			name, want string
			// set is written on the digest's payload, a nil value leaving the key out (withPayloadFields).
			set map[string]json.RawMessage
		}{
			{"no artifact_id", "artifact_id", map[string]json.RawMessage{"artifact_id": nil}},
			{"version 0", "version", map[string]json.RawMessage{"version": json.RawMessage("0")}},
			{"no version", "version", map[string]json.RawMessage{"version": nil}},
			{"no approval_current", "approval_current", map[string]json.RawMessage{"approval_current": nil}},
			{"a null approval_current", "approval_current", map[string]json.RawMessage{"approval_current": json.RawMessage("null")}},
		} {
			fact, _, err := DecodeDispatchEvent("ACME", "ACME-7", withPayloadFields(t, digest, refused.set))
			if err == nil || fact != nil || !strings.Contains(err.Error(), refused.want) {
				t.Errorf("a digest with %s decoded to %#v, %v; want it refused, naming %s", refused.name, fact, err, refused.want)
			}
		}
	})
}

// A read of a document's approval records one thing of one Dispatch body, whichever reader made it
// (ReadApproval: register_gate and the gate seed). With a Dispatch that sends digest_owed it records
// the approval Dispatch shows at approval.version, never the latest version, with its reviewer,
// also while the digest is owed (R3 of dispatch://INFRA-6065, "A read"), and its verdict at the
// latest version only where no digest is owed, marked at the floor of the issue's last_seq and the
// review's sequence (the review's alone for a carried copy) and at the read's time, raised to the
// review's. A read of a Dispatch that sends no digest_owed records today's: an approval only of the
// latest version, a changes request of it, or the latest version with no reviewer.
func TestHashGateReadApprovalRecordsOneReading(t *testing.T) {
	t.Parallel()
	person := dispatch.Actor{Kind: "user", ID: "person"}
	reviewedAt := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	readAt := reviewedAt.Add(time.Hour)
	digest := strings.Repeat("a", 64)
	review := record.EventMark{Seq: 5, At: reviewedAt}
	approval := func(state string, version int, owed *bool) dispatch.Approval {
		read := dispatch.Approval{State: state, Version: &version, LatestVersion: 2, By: person, Seq: 5, At: reviewedAt, DigestOwed: owed}
		if owed != nil && !*owed {
			read.CurrentSHA256 = &digest
		}
		return read
	}
	withReason := approval("changes_requested", 2, new(false))
	withReason.Reason = new("name the rollback path")
	for _, tc := range []struct {
		name     string
		approval dispatch.Approval
		carried  bool
		want     ApprovalRead
	}{
		{"hash mode, an approval below the latest version while the digest is owed", approval("approved", 1, new(true)), false,
			ApprovalRead{Kind: DispatchArtifactApproved, Version: 1, Mark: review, Actor: person, DigestOwed: new(true)}},
		{"hash mode, an approval below the latest version that stands", approval("approved", 1, new(false)), false,
			ApprovalRead{Kind: DispatchArtifactApproved, Version: 1, Mark: review, Actor: person, DigestOwed: new(false), CurrentSHA256: digest,
				Verdict: new(true), VerdictMark: record.EventMark{Seq: 7, At: readAt}}},
		{"hash mode, a carried copy's read marks the verdict at the review's sequence", approval("approved", 1, new(false)), true,
			ApprovalRead{Kind: DispatchArtifactApproved, Version: 1, Mark: review, Actor: person, DigestOwed: new(false), CurrentSHA256: digest,
				Verdict: new(true), VerdictMark: record.EventMark{Seq: 5, At: readAt}}},
		{"hash mode, an approval that does not stand", approval("stale", 1, new(false)), false,
			ApprovalRead{Kind: DispatchArtifactVersion, Version: 2, DigestOwed: new(false), CurrentSHA256: digest,
				Verdict: new(false), VerdictMark: record.EventMark{Seq: 7, At: readAt}}},
		{"hash mode, a changes request of the latest version", withReason, false,
			ApprovalRead{Kind: DispatchArtifactChangesRequested, Version: 2, Mark: review, Actor: person, Reason: "name the rollback path",
				DigestOwed: new(false), CurrentSHA256: digest, Verdict: new(false), VerdictMark: record.EventMark{Seq: 7, At: readAt}}},
		{"version mode, an approval of the latest version", approval("approved", 2, nil), false,
			ApprovalRead{Kind: DispatchArtifactApproved, Version: 2, Mark: review, Actor: person}},
		{"version mode, an approval below the latest version", approval("approved", 1, nil), false,
			ApprovalRead{Kind: DispatchArtifactVersion, Version: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReadApproval(tc.approval, 7, readAt, tc.carried); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ReadApproval = %#v, want %#v", got, tc.want)
			}
		})
	}
	t.Run("the verdict's time is raised to a review Dispatch marked after the read", func(t *testing.T) {
		if got := ReadApproval(approval("approved", 1, new(false)), 7, reviewedAt.Add(-time.Minute), false); got.VerdictMark != (record.EventMark{Seq: 7, At: reviewedAt}) {
			t.Fatalf("the verdict's mark = %+v, want sequence 7 at the review's time %s", got.VerdictMark, reviewedAt)
		}
	})
}
