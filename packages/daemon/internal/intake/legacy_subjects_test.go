package intake

import (
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testwait"
)

// The two forms of an issue event's name (dispatch://INFRA-3862): the old form names the issue's
// key alone, notifications.dispatch.issue.<KEY>.<type>, and the new one the key's project first,
// notifications.dispatch.issue.<PROJECT>.<KEY>.<type>. The daemon reads both before any producer
// moves to the new one, since a daemon that read only the old one would take every renamed event
// for another project's and acknowledge it with no fact.

func projectFormIssueCreatedEnvelope(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/dispatch/issue-created-project-form.json")
	if err != nil {
		t.Fatalf("read the issue.created envelope named in the new form: %v", err)
	}
	return data
}

// An issue event named in the new form decodes to the very fact the same event named in the old
// form does, and only the old form is reported as such (decodedMessage.LegacyName).
func TestAnIssueEventDecodesToOneFactInEitherFormOfItsName(t *testing.T) {
	old, err := decodeMessage("notifications.dispatch.issue.CAPTURE-4.issue.created", "CAPTURE", capturedRepositories, capturedIssueCreatedEnvelope(t))
	if err != nil {
		t.Fatalf("decode the old form: %v", err)
	}
	current, err := decodeMessage("notifications.dispatch.issue.CAPTURE.CAPTURE-4.issue.created", "CAPTURE", capturedRepositories, projectFormIssueCreatedEnvelope(t))
	if err != nil {
		t.Fatalf("decode the new form: %v, want the fact the old form decodes to", err)
	}
	if current.Fact == nil {
		t.Fatal("the new form decoded to no fact, as another project's event would")
	}
	if !reflect.DeepEqual(current.Fact, old.Fact) || current.EventID != old.EventID {
		t.Fatalf("the new form's fact = %#v (%s), want the old form's %#v (%s)", current.Fact, current.EventID, old.Fact, old.EventID)
	}
	if !old.LegacyName || current.LegacyName {
		t.Fatalf("old form reported legacy %v and new form %v, want true and false", old.LegacyName, current.LegacyName)
	}
}

// Another project's issue event is no fact in either form, as it always was in the old one: the
// stream carries every project's events and this daemon acts on its own.
func TestAnotherProjectsIssueEventIsNoFactInEitherForm(t *testing.T) {
	for subject, legacy := range map[string]bool{
		"notifications.dispatch.issue.CAPTURE-4.issue.created":         true,
		"notifications.dispatch.issue.CAPTURE.CAPTURE-4.issue.created": false,
	} {
		decoded, err := decodeMessage(subject, "LEGION", capturedRepositories, capturedIssueCreatedEnvelope(t))
		if err != nil || decoded.Fact != nil || decoded.LegacyName != legacy {
			t.Errorf("decode %s for LEGION = %#v legacy %v, %v; want no fact, legacy %v, no error", subject, decoded.Fact, decoded.LegacyName, err, legacy)
		}
	}
}

// A name of the issue family that carries its key in neither form is poison
// (errIssueNameInNeitherForm), never another project's event acknowledged with no fact: a daemon
// that acknowledged a name it could not read would stop working its project in silence.
func TestAnIssueNameInNeitherFormIsPoison(t *testing.T) {
	for _, subject := range []string{
		"notifications.dispatch.issue.CAPTURE.issue.created",
		"notifications.dispatch.issue.LEGION.CAPTURE-4.issue.created",
		"notifications.dispatch.issue._.CAPTURE-4.issue.created",
		"notifications.dispatch.issue.capture-4.issue.created",
		"notifications.dispatch.issue.CAPTURE.CAPTURE.CAPTURE-4.issue.created",
		"notifications.dispatch.issue.",
	} {
		decoded, err := decodeMessage(subject, "CAPTURE", capturedRepositories, capturedIssueCreatedEnvelope(t))
		if !errors.Is(err, errIssueNameInNeitherForm) {
			t.Errorf("decode %s = %#v, %v; want poison naming a name in neither form", subject, decoded.Fact, err)
		}
	}
}

// Through the Dispatch durable: an issue event named in the new form is applied; one named in the
// old form is applied too, and logged at INFO with its subject and the running count of old-form
// names the consumer has read, the first at once and then at most one line a minute, since every
// daemon hears every project's events; and one whose name is in neither form is logged at ERROR
// with its subject, its stream sequence and the running count of such names, and terminated. It
// leaves the stream as poison does, never acknowledged in silence.
//
// Negative control, run when this was written: make decodeDispatchFact answer a name in neither
// form with no fact and no error, as the merge base did for a key it could not read; the wait for
// the ERROR line times out, the name acknowledged with nothing logged.
func TestTheDispatchDurableReadsBothFormsAndRefusesANameInNeither(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	var elapsed atomic.Int64
	opened := time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC)
	spec.Now = func() time.Time { return opened.Add(time.Duration(elapsed.Load())) }
	stop := startConsume(t, js, spec, pool, writeHandler("applied", nil))
	defer stop()
	dispatch := dispatchConsumerName(spec.Project)
	const oldFormLine = "an issue event named in the old form"

	last := publish(t, js, "notifications.dispatch.issue.CAPTURE.CAPTURE-4.issue.created", projectFormIssueCreatedEnvelope(t))
	assertAcknowledgedThrough(t, stream, dispatch, last)
	if got := writeCount(t, pool); got != 1 {
		t.Fatalf("the new form's event applied %d facts, want 1", got)
	}
	if strings.Contains(logs.String(), oldFormLine) {
		t.Fatalf("the new form was logged as the old one:\n%s", logs)
	}

	last = publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	assertAcknowledgedThrough(t, stream, dispatch, last)
	if got := writeCount(t, pool); got != 2 {
		t.Fatalf("the old form's event applied %d facts in all, want 2", got)
	}
	requireLogLine(t, logs, oldFormLine, "subject=notifications.dispatch.issue.CAPTURE-3.issue.updated", "legacy_count=1")

	// Another project's event in the old form, inside the minute: counted, no second line.
	last = publish(t, js, "notifications.dispatch.issue.LEGION-7.issue.updated", capturedIssueUpdatedEnvelope(t))
	assertAcknowledgedThrough(t, stream, dispatch, last)
	if got := strings.Count(logs.String(), oldFormLine); got != 1 {
		t.Fatalf("%d old-form lines inside one minute, want 1:\n%s", got, logs)
	}
	elapsed.Store(int64(time.Minute))
	last = publish(t, js, "notifications.dispatch.issue.LEGION-8.issue.updated", capturedIssueUpdatedEnvelope(t))
	assertAcknowledgedThrough(t, stream, dispatch, last)
	requireLogLine(t, logs, oldFormLine, "subject=notifications.dispatch.issue.LEGION-8.issue.updated", "legacy_count=3")

	neither := publish(t, js, "notifications.dispatch.issue.CAPTURE.issue.updated", capturedIssueUpdatedEnvelope(t))
	testwait.Eventually(t, "the name in neither form logged at ERROR", func() bool {
		return logLine(logs.String(), "level=ERROR", "poison JetStream message", "subject=notifications.dispatch.issue.CAPTURE.issue.updated") != ""
	})
	requireLogLine(t, logs, "poison JetStream message", "neither_form_count=1", "stream_seq="+strconv.FormatUint(neither.Sequence, 10))
	assertNoAckPending(t, stream, dispatch)
	if got := writeCount(t, pool); got != 2 {
		t.Fatalf("%d facts applied in all, want 2: a name in neither form applies nothing", got)
	}
}

// requireLogLine fails the test unless one line of logs holds every one of terms.
func requireLogLine(t *testing.T, logs *lockedBuffer, terms ...string) {
	t.Helper()
	if logLine(logs.String(), terms...) == "" {
		t.Fatalf("no log line holds %q:\n%s", terms, logs)
	}
}
