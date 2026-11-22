// person_pod_responsible_test.go pins whose Dispatch Inbox lists a request that a person's pod
// makes for a shared human-tier secret (responsiblePerson). The first step of that order is the
// person the session acts for. A pod acts for its person only while BROKER_POD_OPERATORS maps its
// account to that person (requester, unmappedPersonPod). So the request is listed for that person
// while the setting maps the account. After the setting drops the account, or maps it to another
// person, the pod acts for no person. A person's pod has no claim, so no later step names a
// person either, and the pod's next request is listed for everyone.
package requests

import (
	"context"
	"crypto/ecdsa"
	"testing"
)

// podInboxes are the logins whose PendingForApprover lists these tests read: sami, whose pod asks;
// alice, whose own pod the fixture maps too and to whom one change moves sami's account; bob, who
// has nothing to do with the pod; and dave, whom the stand-in Dispatch names as the assignee of
// every issue.
var podInboxes = []string{samisPod.person, alicesPod.person, bob, dave}

// whoseInbox names the Inbox that lists a record whose notify is notify.
func whoseInbox(notify string) string {
	if notify == "" {
		return "every person's Inbox"
	}
	return notify + "'s Inbox alone"
}

// samisPodWithDispatch is newPersonPodFixture with a stand-in Dispatch that names dave as the
// assignee of every issue. So a claim on the pod would put its request on dave's list. It fails t
// unless the pod's enrollment records sami as its operator and no claim. It answers the Machine,
// the pod's enrollment and key, and the stand-in.
func samisPodWithDispatch(t *testing.T) (*Machine, string, *ecdsa.PrivateKey, *standInAssignees) {
	t.Helper()
	m, pod, key := newPersonPodFixture(t)
	assignees := &standInAssignees{answer: dave}
	m.Assignees = assignees
	requireSamisRow(t, m, pod, "at enrollment")
	return m, pod, key, assignees
}

// requireSamisRow fails t unless enrollment pod records sami as its operator and no claim. when
// says at which step of the test the row is read.
func requireSamisRow(t *testing.T, m *Machine, pod, when string) {
	t.Helper()
	var operator, claim *string
	if err := m.Store.Pool.QueryRow(context.Background(), `select operator, claim from enrollments where id=$1`, pod).Scan(&operator, &claim); err != nil {
		t.Fatalf("read the operator and claim of enrollment %s: %v", pod, err)
	}
	if deref(operator) != samisPod.person || deref(claim) != "" {
		t.Fatalf("%s, enrollment %s records operator %q and claim %q; want operator %s and no claim: the row keeps the person, and only the setting decides whether the pod acts for that person", when, pod, deref(operator), deref(claim), samisPod.person)
	}
}

// requireListedForSami fails t unless record recordID, a request of sami's mapped pod for a shared
// secret, notifies sami and is on sami's PendingForApprover list alone among podInboxes.
func requireListedForSami(t *testing.T, m *Machine, pod, recordID string) {
	t.Helper()
	if got := notifyOf(t, m, recordID); got != samisPod.person {
		t.Errorf("record %s of %s's mapped pod %s notifies %q; want %s, the person the pod acts for: %s would list it", recordID, samisPod.person, pod, got, samisPod.person, whoseInbox(got))
	}
	for _, login := range podInboxes {
		listed, want := listsFor(t, m, login, recordID), login == samisPod.person
		switch {
		case listed && !want:
			t.Errorf("PendingForApprover(%s) lists record %s of %s's mapped pod %s: %s's Inbox would list a request of a pod that acts for %s alone", login, recordID, samisPod.person, pod, login, samisPod.person)
		case !listed && want:
			t.Errorf("PendingForApprover(%s) does not list record %s of %s's mapped pod %s: %s's Inbox would miss a request of the pod that acts for them", login, recordID, samisPod.person, pod, login)
		}
	}
}

// requireListedForEveryone fails t unless record recordID, a request of pod made after change,
// notifies nobody and is on the PendingForApprover list of every login in podInboxes.
func requireListedForEveryone(t *testing.T, m *Machine, pod, recordID, change string) {
	t.Helper()
	if got := notifyOf(t, m, recordID); got != "" {
		t.Errorf("record %s of pod %s, made after %s, notifies %q; want none: %s would list a request of a pod that acts for no person", recordID, pod, change, got, whoseInbox(got))
	}
	for _, login := range podInboxes {
		if !listsFor(t, m, login, recordID) {
			t.Errorf("PendingForApprover(%s) does not list record %s of pod %s, made after %s; want it on every person's list: the pod acts for no person and has no claim", login, recordID, pod, change)
		}
	}
}

// requireNoDispatchRead fails t unless the stand-in Dispatch was asked for no issue.
func requireNoDispatchRead(t *testing.T, assignees *standInAssignees) {
	t.Helper()
	if asked := assignees.keys(); len(asked) != 0 {
		t.Errorf("Dispatch was asked for the assignee of %v; want no read: a person's pod has no claim, and a read would put the request on dave's list", asked)
	}
}

// TestPersonPodSharedRequestIsListedForItsPerson: while BROKER_POD_OPERATORS maps the pod's
// account to sami, the pod asks as sami. Its request for a shared human-tier secret waits on
// anyone and notifies sami. It is on sami's PendingForApprover list, which Dispatch's Inbox reads,
// and not on the list of alice, bob or dave. The pod has no claim, so Dispatch is not read.
func TestPersonPodSharedRequestIsListedForItsPerson(t *testing.T) {
	m, pod, key, assignees := samisPodWithDispatch(t)
	req := askShared(t, m, pod, key)
	requireListedForSami(t, m, pod, *req.RecordID)
	requireNoDispatchRead(t, assignees)
}

// TestPersonPodSharedRequestFollowsTheSetting: the setting drops the pod's account, or maps it to
// another person. The pod's next request for the shared human-tier secret notifies nobody and is
// on every person's PendingForApprover list, because the pod acts for no person and has no claim.
// It is not on sami's list alone, and in the move not on alice's list alone.
//
// Before the change, the pod's first request is listed for sami alone. That makes the listing for
// everyone after the change a change. The pod then cancels the first request, so that Create
// writes a new request and does not join the pending one. After the change, the enrollment row
// still records sami as its operator. So the setting, and nothing else, takes the request off
// sami's list.
func TestPersonPodSharedRequestFollowsTheSetting(t *testing.T) {
	for _, tc := range []struct {
		name string
		// change applies the setting's change to the map.
		change func(map[string]string)
	}{
		{name: "the setting drops the account", change: func(m map[string]string) { delete(m, samisPod.account) }},
		{name: "the setting maps the account to another person", change: func(m map[string]string) { m[samisPod.account] = alicesPod.person }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, pod, key, assignees := samisPodWithDispatch(t)

			first := askShared(t, m, pod, key)
			requireListedForSami(t, m, pod, *first.RecordID)
			if t.Failed() {
				t.Fatalf("before the change, record %s of pod %s is not listed for %s alone: the listing for everyone after the change needs it first", *first.RecordID, pod, samisPod.person)
			}
			if err := m.Cancel(ctx, first.ID, pod); err != nil {
				t.Fatalf("Cancel(request %s, by pod %s that made it) = %v; want it cancelled, so that the pod's next request is a new one", first.ID, pod, err)
			}

			tc.change(m.PodOperators)
			requireSamisRow(t, m, pod, "after "+tc.name)

			again := askShared(t, m, pod, key)
			if again.ID == first.ID || *again.RecordID == *first.RecordID {
				t.Fatalf("Create(SHARED_KEY) after %s = request %s, record %s; want a new request, not request %s", tc.name, again.ID, *again.RecordID, first.ID)
			}
			requireListedForEveryone(t, m, pod, *again.RecordID, tc.name)
			requireNoDispatchRead(t, assignees)
		})
	}
}
