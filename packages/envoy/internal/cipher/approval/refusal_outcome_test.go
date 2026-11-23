package approval_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/groups/groupstest"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestARefusedDecisionSaysWhyItIsStillPending is round 6's m-f on cybermonklabs/legion#4135: a
// decision the broker refused, of a record still pending that its reader may decide, names the
// reason its line is still pending. Here TEST_SECRET's owner moves from ada to carol after ada's
// page is printed, so the current policy no longer lets ada approve it. The reason travels to the
// page in a fixed code the broker maps to its own sentence, so a forged code, or one outside the
// set, reads as a line with no reason did before, and the address can write no words of its own.
func TestARefusedDecisionSaysWhyItIsStillPending(t *testing.T) {
	r := newRig(t)
	enrollmentID, key := r.benSession(t, "ben-box")
	p := r.request(t, enrollmentID, key, "deploy the billing service", nil, "", "TEST_SECRET")
	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	form := decisionForm(t, b.Page(t, p.recordID).Body, p.recordID)
	if form == nil {
		t.Fatalf("the pending request offers %s no decision form", ada)
	}
	r.retag(t, policytest.Secret("TEST_SECRET", carol, policy.TierHuman, "test-v1"))

	res := b.FromPage(t, "/approve", decision(form, "approve"))
	if res.Status != http.StatusSeeOther {
		t.Fatalf("%s's Approve after the owner moved = %d, want 303 to the record with its outcome:\n%s", ada, res.Status, res.Body)
	}
	to := res.Header.Get("Location")
	if want := "&refused=" + p.recordID + ":policy"; !strings.Contains(to, want) {
		t.Fatalf("the decision redirects to %q; want it to carry %q", to, want)
	}
	r.requirePending(t, p.recordID)
	if after := pageText(t, b.Get(t, to).Body); !strings.Contains(after, "TEST_SECRET: still pending; it was not decided: the current policy no longer lets you approve it.") {
		t.Errorf("the page after the refused Approve does not name the policy:\n%s", after)
	}

	for _, forged := range []string{"bogus", "policy<b>x</b>", ""} {
		page := pageText(t, b.Get(t, "/approve/"+p.recordID+"?decided="+p.recordID+"&refused="+p.recordID+":"+forged).Body)
		if !strings.Contains(page, "TEST_SECRET: still pending; it was not decided.") || strings.Contains(page, "bogus") || strings.Contains(page, "<b>") {
			t.Errorf("the page with the forged code %q does not read as a line with no reason:\n%s", forged, page)
		}
	}
}

// TestARefusalForAnUnreadGroupSaysToApproveOnceItIsRead: ada's approval of a request for her
// secret whose requesters name a Google group the broker has not read in the last two hours is
// refused and leaves the request pending, and the page says why in its own words, not as a policy
// that no longer lets her approve it: the same approval works once the group is read.
func TestARefusalForAnUnreadGroupSaysToApproveOnceItIsRead(t *testing.T) {
	const group = "operators@example.com"
	directory := groupstest.New()
	directory.Set(group, ben)
	listed := policytest.ListedSecret("LISTED_SECRET", ada, policy.TierHuman, record.GroupPrefix+group, "listed-v1")
	r := newRig(t, func(s *rigSetup) {
		s.secrets = append(s.secrets, listed)
		s.groups = directory
	})
	enrollmentID, key := r.benSession(t, "ben-box")
	p := r.request(t, enrollmentID, key, "deploy the billing service", nil, "", "LISTED_SECRET")
	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	form := decisionForm(t, b.Page(t, p.recordID).Body, p.recordID)
	if form == nil {
		t.Fatalf("the pending request offers %s no decision form", ada)
	}
	directory.MarkStale(group)

	res := b.FromPage(t, "/approve", decision(form, "approve"))
	to := res.Header.Get("Location")
	if want := "&refused=" + p.recordID + ":group-unread"; res.Status != http.StatusSeeOther || !strings.Contains(to, want) {
		t.Fatalf("ada's Approve while the group is unread = %d to %q; want 303 carrying %q", res.Status, to, want)
	}
	r.requirePending(t, p.recordID)
	after := pageText(t, b.Get(t, to).Body)
	if !strings.Contains(after, "LISTED_SECRET: still pending; it was not decided: who may ask for it names a Google group the broker has not read in the last two hours. Approve it again once the group is read.") ||
		strings.Contains(after, "no longer lets you approve it") {
		t.Errorf("the page after the refused Approve does not say the group is unread:\n%s", after)
	}
}
