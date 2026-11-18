package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/kvchunk/kvchunktest"
)

// overlapTopics is a registration of the shape that outgrows one message in practice: a wildcard
// over a project's issues, many issue topics under it, a role lane, and topics no wildcard covers.
func overlapTopics() []string {
	topics := []string{"notifications.dispatch.issue.K-0.>", contracts.RoleTopicPrefix + "legion-controller"}
	for i := range 400 {
		topics = append(topics, fmt.Sprintf("notifications.dispatch.issue.K-0.t%04d-%s", i, strings.Repeat("x", 270)))
	}
	for i := range 50 {
		topics = append(topics, fmt.Sprintf("notifications.team.uncovered-%04d.%s", i, strings.Repeat("y", 270)))
	}
	return topics
}

// requireMatchesEvery fails the test unless view matches every subject registered matches and
// holds no topic beyond registered: an earlier build's subject index over view routes and counts
// the session for exactly the topics it registered.
func requireMatchesEvery(t *testing.T, what string, view, registered []string) {
	t.Helper()
	for _, topic := range registered {
		if !slices.ContainsFunc(view, func(pattern string) bool { return bus.SubjectContains(pattern, topic) }) {
			t.Fatalf("%s holds %d topics, and none of them matches %s, one of the %d registered", what, len(view), topic, len(registered))
		}
	}
	for _, topic := range view {
		if !slices.Contains(registered, topic) {
			t.Fatalf("%s holds %s, which the session never registered", what, topic)
		}
	}
}

// While builds share the interest bucket, a listener of an earlier build reads a chunked record's
// manifest as a record of its own type: every earlier build decodes a value with encoding/json into
// Interest and ignores a member it does not know (origin/main's decodeInterest). So the manifest
// carries the session's id, machine and directory, and the covering topics, which match every
// subject the full list does: that listener keeps serving and counting the session rather than
// evicting it as a record it cannot decode. This build reads the same manifest as the full record.
func TestAnEarlierBuildReadsAChunkedRecordAsTheSameSession(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	registered := overlapTopics()
	if _, err := reg.Upsert(Interest{SessionID: "ses_overlap", MachineID: "m1", Dir: "/overlap"}, registered); err != nil {
		t.Fatalf("store the record: %v", err)
	}
	head, err := rawInterests(t, conn).Get("ses_overlap")
	if err != nil {
		t.Fatalf("read the session's key: %v", err)
	}
	if !kvchunk.IsManifest(head.Value()) {
		t.Fatalf("the session's key holds %.60q, want a manifest", head.Value())
	}
	var earlier Interest
	if err := json.Unmarshal(head.Value(), &earlier); err != nil {
		t.Fatalf("an earlier build's decode of the manifest: %v, want the session", err)
	}
	if earlier.SessionID != "ses_overlap" || earlier.MachineID != "m1" || earlier.Dir != "/overlap" || earlier.UpdatedAt == 0 {
		t.Fatalf("an earlier build reads the manifest as %+v, want ses_overlap on m1 in /overlap with its update time", earlier)
	}
	requireMatchesEvery(t, "an earlier build's view", earlier.Topics, registered)
	if !slices.Contains(earlier.Topics, contracts.RoleTopicPrefix+"legion-controller") {
		t.Fatalf("an earlier build's view holds no role lane: %v", earlier.Topics)
	}
	t.Logf("an earlier build reads %d topics of the %d registered, in a manifest of %d bytes", len(earlier.Topics), len(registered), len(head.Value()))

	row, _, err := reg.RefreshAt(t.Context(), "ses_overlap")
	if err != nil {
		t.Fatalf("this build's read of the manifest: %v", err)
	}
	requireTopics(t, "this build's read", row, sortedTopics(registered))
}

// CoveringTopics drops a topic only when another covers every name form of it, keeps a role lane
// whatever covers it, keeps the first of two topics that cover each other, and keeps a topic no
// other covers.
func TestCoveringTopicsDropsOnlyWhatAnotherTopicCovers(t *testing.T) {
	lane := contracts.RoleTopicPrefix + "legion-controller"
	got := CoveringTopics([]string{
		"notifications.team.a.>", "notifications.team.a.b", "notifications.team.a.b.c",
		"notifications.team.x.*", "notifications.team.x.y", "notifications.team.x.y.z",
		"notifications.>", lane, "notifications.team.a.b",
	})
	if want := []string{"notifications.>", lane}; !slices.Equal(got, want) {
		t.Fatalf("CoveringTopics answered %v, want %v", got, want)
	}
	got = CoveringTopics([]string{"notifications.team.x.*", "notifications.team.x.y", "notifications.team.x.y.z"})
	if want := []string{"notifications.team.x.*", "notifications.team.x.y.z"}; !slices.Equal(got, want) {
		t.Fatalf("CoveringTopics answered %v, want %v: `*` covers one token, not two", got, want)
	}
}

// A view takes the covering topics when they fit, falls back to the consumer filter's reduction
// beside the role lanes when they do not, and is bus.ErrTooLarge when neither fits the room.
func TestAChunkedRecordViewFallsBackToTheReductionThenRefuses(t *testing.T) {
	lane := contracts.RoleTopicPrefix + "legion-controller"
	var topics []string
	for i := range 200 {
		// Two families that overlap without either containing the other, so covering keeps them all
		// and the reduction merges them into one prefix.
		topics = append(topics, fmt.Sprintf("notifications.team.f.%04d.*", i), fmt.Sprintf("notifications.team.f.*.%04d", i))
	}
	topics = append(topics, lane)
	encode := func(topics []string) ([]byte, error) {
		return json.Marshal(Interest{SessionID: "ses_view", Topics: topics})
	}
	covering, _ := encode(CoveringTopics(topics))
	view, err := ChunkedRecordView(len(covering), topics, encode)
	if err != nil || !slices.Equal(view, covering) {
		t.Fatalf("a view with room for the covering topics answered %.80q (%v), want them", view, err)
	}
	view, err = ChunkedRecordView(len(covering)-1, topics, encode)
	var seen Interest
	if err != nil || json.Unmarshal(view, &seen) != nil || !slices.Contains(seen.Topics, lane) || len(seen.Topics) >= len(topics) {
		t.Fatalf("a view with no room for the covering topics answered %.80q (%v), want the reduction beside the role lane", view, err)
	}
	for _, topic := range topics {
		if !slices.ContainsFunc(seen.Topics, func(pattern string) bool { return bus.SubjectContains(pattern, topic) }) {
			t.Fatalf("the reduced view %v matches no topic of %s", seen.Topics, topic)
		}
	}
	if _, err := ChunkedRecordView(10, topics, encode); !errors.Is(err, bus.ErrTooLarge) {
		t.Fatalf("a view with room for nothing answered %v, want bus.ErrTooLarge", err)
	}
}

// A record whose topics no view fits in one message - topics no wildcard among them covers and that
// do not overlap, so neither covering nor the consumer filter's reduction shortens them - is refused
// bus.ErrTooLarge before anything is written: no chunk, no record. Its session consumer's filter
// carries those same subjects past one message, which the registration route refuses first
// (cmd/listener's too_large), so the bound is the one a session consumer is already held to, not a
// new one. The control is the same topics under a covering wildcard, stored in chunks.
func TestARecordWithNoViewThatFitsIsRefusedAndWritesNothing(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	reg := openRegistry(t, conn)
	kv := rawInterests(t, conn)
	var uncovered []string
	for i := range 400 {
		uncovered = append(uncovered, fmt.Sprintf("notifications.team.u%04d.%s", i, strings.Repeat("z", 270)))
	}
	_, err := reg.Upsert(Interest{SessionID: "ses_no_view", MachineID: "m1"}, uncovered)
	if !errors.Is(err, bus.ErrTooLarge) {
		t.Fatalf("a record of %d topics no wildcard covers answered %v, want bus.ErrTooLarge", len(uncovered), err)
	}
	t.Logf("the refusal: %v", err)
	for key, op := range kvchunktest.LiveKeys(t, kv) {
		if op == natsgo.KeyValuePut {
			t.Fatalf("the refused write left %s in the bucket", key)
		}
	}
	if _, err := reg.Upsert(Interest{SessionID: "ses_no_view", MachineID: "m1"}, append([]string{"notifications.team.>"}, uncovered...)); err != nil {
		t.Fatalf("the same topics under a covering wildcard answered %v, want them stored", err)
	}
}
