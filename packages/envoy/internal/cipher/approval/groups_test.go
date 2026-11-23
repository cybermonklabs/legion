package approval

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/groups/groupstest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
)

// TestTheApprovalPageShowsAMemberTheButtons: a pending request whose approver is a group shows a
// member of the group Approve and Deny, and shows a person outside it none, whose outcome line
// says the request is not theirs to decide and names the group; once the group is stale it shows
// nobody the buttons.
func TestTheApprovalPageShowsAMemberTheButtons(t *testing.T) {
	const group = "approvers@example.com"
	directory := groupstest.New()
	directory.Set(group, "member@example.com")
	p := &Page{cfg: Config{Machine: &requests.Machine{Groups: directory}}}
	now := time.Now()
	detail := requests.RecordDetail{
		RecordID: strings.Repeat("a", 64), Kind: record.KindAgentSecret, State: "pending",
		Approver: record.GroupPrefix + group, Identifiers: []string{"TEAM_KEY"},
		ExpiresAt: now.Add(time.Hour), RequestedAt: now,
	}
	if v := p.recordView(detail, "member@example.com", now); !v.Buttons || !v.MayDecide {
		t.Fatalf("the member's view = %+v; want the buttons", v)
	}
	v := p.recordView(detail, "outsider@example.com", now)
	if v.Buttons || v.MayDecide {
		t.Fatalf("the outsider's view = %+v; want no buttons", v)
	}
	if want := "a member of the group " + group; v.Approver != want {
		t.Fatalf("the page names the approver %q, want %q", v.Approver, want)
	}
	if line := outcome(detail, "outsider@example.com", now, "", directory); !strings.Contains(line, "not yours to decide; only a member of the group "+group+" may.") {
		t.Fatalf("the outsider's outcome line is %q; want it to say the request is not theirs to decide", line)
	}
	if line := outcome(detail, "member@example.com", now, "", directory); strings.Contains(line, "not yours to decide") {
		t.Fatalf("the member's outcome line is %q; want it pending, theirs to decide", line)
	}
	if hint := approverHint(detail.Approver); hint != "" {
		t.Fatalf("approverHint(%q) = %q, want none: a group is no login to sign in as", detail.Approver, hint)
	}
	directory.MarkStale(group)
	if v := p.recordView(detail, "member@example.com", now); v.Buttons {
		t.Fatal("a member of a stale group sees the buttons")
	}
}
