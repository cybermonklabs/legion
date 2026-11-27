package intake

//ci:reads packages/contracts/fixtures/hash-gate/** hashGateEvent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// hashGateEvent is one event file of packages/contracts/fixtures/hash-gate, the wire contract of
// Dispatch's hash gate (R1 of dispatch://INFRA-6065): a whole Dispatch event, as the issue's event
// log serves it and the Envoy envelope carries it.
func hashGateEvent(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "hash-gate", name))
	if err != nil {
		t.Fatalf("read the hash-gate fixture %s: %v", name, err)
	}
	return raw
}

// withPayload is event with its payload changed by change: an event the fixtures hold but for one
// field.
func withPayload(t *testing.T, event []byte, change func(payload map[string]any)) []byte {
	t.Helper()
	var read map[string]any
	if err := json.Unmarshal(event, &read); err != nil {
		t.Fatalf("decode the event: %v", err)
	}
	payload, ok := read["payload"].(map[string]any)
	if !ok {
		t.Fatalf("the event %s has no payload object", event)
	}
	change(payload)
	changed, err := json.Marshal(read)
	if err != nil {
		t.Fatalf("encode the event: %v", err)
	}
	return changed
}

// Intake decodes the hash-gate event fixtures: an artifact.version reads digest_owed, true or
// false, and its content_sha256; an artifact.digest is a DispatchArtifact of its own kind, with
// its version, its verdict (approval_current), the event's mark and actor and its digests, with
// and without approved_sha256. Each decodes alike from the event log (DecodeDispatchEvent) and
// from the Envoy envelope on its subject in the form the fixtures' README names. The captured
// events of a Dispatch that sends no digest field decode with none set, and a digest that names
// no artifact, no positive version or no verdict is refused, as the other artifact events are.
func TestHashGateDecodesDigestEvents(t *testing.T) {
	t.Parallel()
	const (
		artifact      = "e470eab3-ee84-4cbb-a5f9-b8b117610f94"
		version7      = "fcb3d52cac0a068e46478d4d8d59fbdc95a3e46fdc05a121b0eede5bcd633981"
		version8      = "3cd63d61a307828c936c8f57e230973fa538f32a7a23bf04e068b872673202f2"
		version9      = "ff6123270261c2169d3eb9c14ae391fb1f1d869809dfe9815800e07f181696a7"
		version10     = "42d81cd17ff49d3600be047a138cd7b84d3ae9b5b5c92b6d64c66a5832f081d7"
		subjectPrefix = "notifications.dispatch.issue.ACME.ACME-7."
	)
	mark := func(seq int64, at string) record.EventMark {
		created, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			t.Fatalf("parse the fixture's created_at %q: %v", at, err)
		}
		return record.EventMark{Seq: seq, At: created}
	}
	architect := dispatch.Actor{Kind: "session", ID: "session-architect"}
	fill := dispatch.Actor{Kind: "system", ID: "digest-fill"}
	for _, tc := range []struct {
		name, file, eventType string
		want                  DispatchArtifact
	}{
		{name: "a version that owes its digest", file: "artifact-version-digest-owed.json", eventType: "artifact.version",
			want: DispatchArtifact{Key: "ACME-7", ArtifactID: artifact, Kind: DispatchArtifactVersion, Version: 8,
				Mark: mark(40, "2026-11-27T18:00:00Z"), Actor: architect, DigestOwed: new(true), ContentSHA256: version8}},
		{name: "a version that owes no digest", file: "artifact-version-digest-not-owed.json", eventType: "artifact.version",
			want: DispatchArtifact{Key: "ACME-7", ArtifactID: artifact, Kind: DispatchArtifactVersion, Version: 9,
				Mark: mark(42, "2026-11-27T18:10:00Z"), Actor: architect, DigestOwed: new(false), ContentSHA256: version9}},
		{name: "a digest that reports the approval current", file: "artifact-digest-current.json", eventType: "artifact.digest",
			want: DispatchArtifact{Key: "ACME-7", ArtifactID: artifact, Kind: DispatchArtifactDigest, Version: 8,
				Mark: mark(41, "2026-11-27T18:00:01.5Z"), Actor: fill, Verdict: new(true), ContentSHA256: version8, ApprovedSHA256: version7}},
		{name: "a digest that reports the approval not current", file: "artifact-digest-not-current.json", eventType: "artifact.digest",
			want: DispatchArtifact{Key: "ACME-7", ArtifactID: artifact, Kind: DispatchArtifactDigest, Version: 10,
				Mark: mark(44, "2026-11-27T18:20:01.5Z"), Actor: fill, Verdict: new(false), ContentSHA256: version10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := hashGateEvent(t, tc.file)
			fact, unread, err := DecodeDispatchEvent("ACME", "ACME-7", event)
			if err != nil || len(unread) != 0 {
				t.Fatalf("decode %s from the event log = %v, unread %v; want it decoded whole", tc.file, err, unread)
			}
			if !reflect.DeepEqual(fact, tc.want) {
				t.Fatalf("fact from the event log = %#v, want %#v", fact, tc.want)
			}
			delivered, err := decodeMessage(subjectPrefix+tc.eventType, "ACME", capturedRepositories, envelopeJSON(t, "dispatch-"+tc.file, "dispatch", string(event)))
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
		current := hashGateEvent(t, "artifact-digest-current.json")
		for _, refused := range []struct {
			name, want string
			change     func(payload map[string]any)
		}{
			{"no artifact_id", "artifact_id", func(payload map[string]any) { delete(payload, "artifact_id") }},
			{"version 0", "version", func(payload map[string]any) { payload["version"] = 0 }},
			{"no version", "version", func(payload map[string]any) { delete(payload, "version") }},
			{"no approval_current", "approval_current", func(payload map[string]any) { delete(payload, "approval_current") }},
			{"a null approval_current", "approval_current", func(payload map[string]any) { payload["approval_current"] = nil }},
		} {
			fact, _, err := DecodeDispatchEvent("ACME", "ACME-7", withPayload(t, current, refused.change))
			if err == nil || fact != nil || !strings.Contains(err.Error(), refused.want) {
				t.Errorf("a digest with %s decoded to %#v, %v; want it refused, naming %s", refused.name, fact, err, refused.want)
			}
		}
	})
}
