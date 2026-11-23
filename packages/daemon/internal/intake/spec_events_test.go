package intake

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// specEvent is one Dispatch event of key as Dispatch's issue event log serves it, of type with
// payload, at seq.
func specEvent(key, typ string, seq int64, payload string) []byte {
	return fmt.Appendf(nil, `{"id": %d, "issue_key": %q, "seq": %d, "notify": true, "type": %q,
		"actor": {"kind": "user", "id": "alice"}, "created_at": "2026-11-22T10:00:00Z", "payload": %s}`, seq+100, key, seq, typ, payload)
}

// A refused approval of a stale change request (spec.request_stale) is a fact the daemon acts on
// (D8): the issue, the approval request, each stale node with the version the request read and
// the approved spec's current one, and Dispatch's reason. Before this release the decoder took
// no such type and the event was acknowledged with no fact, so the owner never heard of it.
func TestASpecRequestStaleEventIsAFact(t *testing.T) {
	key := fmt.Sprintf("%s-%d", "ACME", 51)
	payload := fmt.Sprintf(`{"issue": %q, "approval_request": "the-request", "reason": "the approved spec changed a-bill-names-its-order after the request was written",
		"statements": [{"id": "a-bill-names-its-order", "base": 1, "current": 2}, {"id": "an-order-is-paid-once", "base": 3, "current": 0, "removed": true}]}`, key)
	got, unread, err := DecodeDispatchEvent("ACME", key, specEvent(key, "spec.request_stale", 7, payload))
	if err != nil || len(unread) != 0 {
		t.Fatalf("decode spec.request_stale = %v, unread %v", err, unread)
	}
	want := SpecRequestStale{Key: key, Seq: 7, ApprovalRequest: "the-request",
		Reason:     "the approved spec changed a-bill-names-its-order after the request was written",
		Statements: []StaleNode{{ID: "a-bill-names-its-order", Base: 1, Current: 2}, {ID: "an-order-is-paid-once", Base: 3, Removed: true}},
		Mark:       record.EventMark{Seq: 7, At: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}, Actor: dispatch.Actor{Kind: "user", ID: "alice"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fact = %#v, want %#v", got, want)
	}
	// An event that names another issue in its payload, or no node, is no stale request.
	for _, bad := range []string{
		fmt.Sprintf(`{"issue": %q, "approval_request": null, "reason": "r", "statements": [{"id": "x", "base": 1, "current": 2}]}`, fmt.Sprintf("%s-%d", "OTHER", 1)),
		fmt.Sprintf(`{"issue": %q, "approval_request": null, "reason": "r", "statements": []}`, key),
	} {
		if _, _, err := DecodeDispatchEvent("ACME", key, specEvent(key, "spec.request_stale", 8, bad)); err == nil {
			t.Errorf("decode of %s = no error, want it refused", bad)
		}
	}
}

// A list of outcomes recorded for a pull request head (spec.outcomes_recorded) is a fact too: it
// is what makes the daemon read the ending condition of a tree again (D10).
func TestASpecOutcomesRecordedEventIsAFact(t *testing.T) {
	key := fmt.Sprintf("%s-%d", "ACME", 52)
	head := strings.Repeat("ab", 20)
	payload := fmt.Sprintf(`{"issue": %q, "record": 7, "pull_request": "acme/widgets#12", "head": %q, "list": "final", "outcomes": 2}`, key, head)
	got, unread, err := DecodeDispatchEvent("ACME", key, specEvent(key, "spec.outcomes_recorded", 9, payload))
	if err != nil || len(unread) != 0 {
		t.Fatalf("decode spec.outcomes_recorded = %v, unread %v", err, unread)
	}
	want := SpecOutcomesRecorded{Key: key, Seq: 9, Record: 7, PullRequest: "acme/widgets#12", Head: head, List: "final"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fact = %#v, want %#v", got, want)
	}
	if _, _, err := DecodeDispatchEvent("ACME", key, specEvent(key, "spec.outcomes_recorded", 10, `{"issue": "`+key+`", "record": 8, "list": "final"}`)); err == nil {
		t.Fatalf("decode of a record with no head = no error, want it refused")
	}
}

// A write of the approved spec of a repository (spec.approved_written) is a fact too: another
// issue's approval or an import moves what every change request of that repository is read
// against, so the daemon reads the spec gates of that repository again. The event's own issue
// must be the one its payload names, and it names the repository; anything else is refused.
func TestASpecApprovedWrittenEventIsAFact(t *testing.T) {
	key := fmt.Sprintf("%s-%d", "ACME", 53)
	payload := fmt.Sprintf(`{"issue": %q, "repo": "acme/widgets", "act": "approval", "nodes": [{"id": "a-second-charge-fails", "kind": "check", "version": 2}]}`, key)
	got, unread, err := DecodeDispatchEvent("ACME", key, specEvent(key, "spec.approved_written", 11, payload))
	if err != nil || len(unread) != 0 {
		t.Fatalf("decode spec.approved_written = %v, unread %v", err, unread)
	}
	want := ApprovedSpecWritten{Key: key, Seq: 11, Repo: "acme/widgets", Act: "approval"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fact = %#v, want %#v", got, want)
	}
	for _, bad := range []string{
		fmt.Sprintf(`{"issue": %q, "repo": "acme/widgets", "act": "approval", "nodes": []}`, fmt.Sprintf("%s-%d", "OTHER", 1)),
		fmt.Sprintf(`{"issue": %q, "repo": "", "act": "import", "nodes": []}`, key),
	} {
		if _, _, err := DecodeDispatchEvent("ACME", key, specEvent(key, "spec.approved_written", 12, bad)); err == nil {
			t.Errorf("decode of %s = no error, want it refused", bad)
		}
	}
}
