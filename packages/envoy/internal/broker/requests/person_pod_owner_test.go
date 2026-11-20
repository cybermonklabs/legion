// person_pod_owner_test.go drives whose requests a person's pod reads and cancels
// (Machine.owner). A pod whose account BROKER_POD_OPERATORS maps to a person records that person
// as its owner, as the person's dev box does. So each of the two reads and cancels the requests of
// the other. The pod has that owner only while the setting still maps its account to the person.
// After the setting drops the account, or maps it to another person, the pod reads and cancels its
// own requests alone.
package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/exchange"
)

// samisOwner is the owner that enroll's ownerKey records for each enrollment of a credential whose
// operator is fixtureOperator: sami's box and sami's mapped pod.
var samisOwner = exchange.PersonPrincipal(fixtureOperator)

// samisBoxAndPod is newPersonPodFixture with a box session of sami's beside the pod. personPod.enroll
// and newEnrollment write no owner, so it records samisOwner on both enrollments, as ownerKey writes
// it at enrollment. It then reads both rows back and fails t unless each records samisOwner. That
// is the state in which the pod reads the box's requests, and so the state that makes each refusal
// after a change of the setting reachable. It answers the Machine, the pod's enrollment and key,
// and the box's enrollment and key.
func samisBoxAndPod(t *testing.T) (m *Machine, pod string, podKey *ecdsa.PrivateKey, box string, boxKey *ecdsa.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	m, pod, podKey = newPersonPodFixture(t)
	box, boxKey = newEnrollment(t, m.Store, "session", "box-sami-"+t.Name(), new(fixtureOperator), nil)
	if _, err := m.Store.Pool.Exec(ctx, `update enrollments set owner=$3 where id in ($1,$2)`, pod, box, samisOwner); err != nil {
		t.Fatalf("record owner %s on sami's pod %s and box %s: %v", samisOwner, pod, box, err)
	}
	for _, enr := range []string{pod, box} {
		var owner *string
		if err := m.Store.Pool.QueryRow(ctx, `select owner from enrollments where id=$1`, enr).Scan(&owner); err != nil {
			t.Fatalf("read the owner of enrollment %s: %v", enr, err)
		}
		if owner == nil || *owner != samisOwner {
			t.Fatalf("enrollment %s records owner %v; want %s, the owner that the pod and the box share", enr, owner, samisOwner)
		}
	}
	return m, pod, podKey, box, boxKey
}

// askPending makes enr ask for DEEL_API_KEY, sami's human-tier secret, and fails t unless the
// request is pending. Only a pending request can be cancelled.
func askPending(t *testing.T, m *Machine, enr string, key *ecdsa.PrivateKey) Request {
	t.Helper()
	req, err := m.Create(context.Background(), enr, signRequest(t, m, key, "need it", "DEEL_API_KEY"), "", nil)
	if err != nil || req.State != "pending" {
		t.Fatalf("Create(enrollment %s, DEEL_API_KEY) = %+v, %v; want it pending", enr, req, err)
	}
	return req
}

// enrollmentOwner is EnrollmentOwner(enr), and it fails t on an error.
func enrollmentOwner(t *testing.T, m *Machine, enr string) string {
	t.Helper()
	owner, err := m.EnrollmentOwner(context.Background(), enr)
	if err != nil {
		t.Fatalf("EnrollmentOwner(%s): %v", enr, err)
	}
	return owner
}

// requestOwner is the owner that OwnerAndState reads for request id, and it fails t on an error.
func requestOwner(t *testing.T, m *Machine, id string) string {
	t.Helper()
	owner, _, _, err := m.OwnerAndState(context.Background(), id)
	if err != nil {
		t.Fatalf("OwnerAndState(%s): %v", id, err)
	}
	return owner
}

// requestState reads the state of request id.
func requestState(t *testing.T, m *Machine, id string) string {
	t.Helper()
	var state string
	if err := m.Store.Pool.QueryRow(context.Background(), `select state from requests where id=$1`, id).Scan(&state); err != nil {
		t.Fatalf("read the state of request %s: %v", id, err)
	}
	return state
}

// TestPersonPodSharesItsPersonsRequestsWhileMapped: while BROKER_POD_OPERATORS maps the pod's
// account to sami, the pod has sami's owner. The pod reads the request of sami's box and cancels
// it. Sami's box reads the request of the pod.
func TestPersonPodSharesItsPersonsRequestsWhileMapped(t *testing.T) {
	ctx := context.Background()
	m, pod, podKey, box, boxKey := samisBoxAndPod(t)
	boxReq := askPending(t, m, box, boxKey)
	podReq := askPending(t, m, pod, podKey)

	podOwner := enrollmentOwner(t, m, pod)
	if podOwner != samisOwner {
		t.Fatalf("EnrollmentOwner(sami's mapped pod %s) = %s; want %s, the owner it records", pod, podOwner, samisOwner)
	}
	if got := requestOwner(t, m, boxReq.ID); got != podOwner {
		t.Fatalf("the owner of request %s of sami's box = %s; want the mapped pod's %s, so that the pod reads it", boxReq.ID, got, podOwner)
	}
	if got, boxOwner := requestOwner(t, m, podReq.ID), enrollmentOwner(t, m, box); got != boxOwner {
		t.Fatalf("the owner of request %s of sami's mapped pod = %s; want the owner of sami's box, %s, so that the box reads it", podReq.ID, got, boxOwner)
	}
	if err := m.Cancel(ctx, boxReq.ID, pod); err != nil {
		t.Fatalf("Cancel(request %s of sami's box, by sami's mapped pod %s) = %v; want it cancelled", boxReq.ID, pod, err)
	}
	if state := requestState(t, m, boxReq.ID); state != "cancelled" {
		t.Fatalf("request %s of sami's box is %s after the pod cancelled it; want cancelled", boxReq.ID, state)
	}
}

// TestPersonPodRequestsFollowTheSetting: the setting drops the pod's account, or maps it to another
// person. The pod then has its own enrollment's owner, 'enrollment:<id>', and sami's box keeps
// sami's owner. The pod no longer reads or cancels the request of sami's box. Sami's box no longer
// reads or cancels the request of the pod. Each refused request stays pending. The pod still reads
// and cancels its own request.
//
// Before the change, both enrollments record sami's owner, and the pod has it (samisBoxAndPod).
// That makes each refusal reachable. Once the entry is back, the pod cancels the request of sami's
// box. That shows that the setting, and nothing else, refused the pod.
func TestPersonPodRequestsFollowTheSetting(t *testing.T) {
	for _, tc := range []struct {
		name string
		// change applies the setting's change to the map.
		change func(map[string]string)
	}{
		{name: "the setting drops the account", change: func(m map[string]string) { delete(m, samisPod.account) }},
		{name: "the setting maps the account to another person", change: func(m map[string]string) { m[samisPod.account] = otherPerson }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, pod, podKey, box, boxKey := samisBoxAndPod(t)
			boxReq := askPending(t, m, box, boxKey)
			podReq := askPending(t, m, pod, podKey)
			if got := enrollmentOwner(t, m, pod); got != samisOwner {
				t.Fatalf("before the change, EnrollmentOwner(sami's mapped pod %s) = %s; want %s: each refusal below needs the pod to share sami's requests first", pod, got, samisOwner)
			}
			if got := requestOwner(t, m, boxReq.ID); got != samisOwner {
				t.Fatalf("before the change, the owner of request %s of sami's box = %s; want %s, the pod's owner", boxReq.ID, got, samisOwner)
			}

			tc.change(m.PodOperators)

			own := "enrollment:" + pod
			podOwner := enrollmentOwner(t, m, pod)
			if podOwner != own {
				t.Fatalf("EnrollmentOwner(pod %s) after the change = %s; want %s: the pod acts for no person now, and owner %s would give it sami's requests", pod, podOwner, own, podOwner)
			}
			if got := enrollmentOwner(t, m, box); got != samisOwner {
				t.Fatalf("EnrollmentOwner(sami's box %s) after the change = %s; want %s: the setting maps no box", box, got, samisOwner)
			}

			if got := requestOwner(t, m, boxReq.ID); got == podOwner {
				t.Fatalf("the owner of request %s of sami's box = %s, the pod's owner: the pod would read sami's request", boxReq.ID, got)
			}
			if err := m.Cancel(ctx, boxReq.ID, pod); !errors.Is(err, ErrNotOwner) {
				t.Fatalf("Cancel(request %s of sami's box, by pod %s) after the change = %v; want ErrNotOwner: the pod would end sami's request", boxReq.ID, pod, err)
			}
			if state := requestState(t, m, boxReq.ID); state != "pending" {
				t.Fatalf("request %s of sami's box is %s after the pod's refused cancel; want pending", boxReq.ID, state)
			}

			if got := requestOwner(t, m, podReq.ID); got == samisOwner {
				t.Fatalf("the owner of request %s of pod %s = %s, the owner of sami's box: sami's box would read the pod's request", podReq.ID, pod, got)
			}
			if err := m.Cancel(ctx, podReq.ID, box); !errors.Is(err, ErrNotOwner) {
				t.Fatalf("Cancel(request %s of pod %s, by sami's box %s) after the change = %v; want ErrNotOwner: sami's box would end the pod's request", podReq.ID, pod, box, err)
			}
			if state := requestState(t, m, podReq.ID); state != "pending" {
				t.Fatalf("request %s of pod %s is %s after the box's refused cancel; want pending", podReq.ID, pod, state)
			}

			if got := requestOwner(t, m, podReq.ID); got != podOwner {
				t.Fatalf("the owner of request %s of pod %s = %s; want the pod's own %s, so that the pod reads its own request", podReq.ID, pod, got, podOwner)
			}
			if err := m.Cancel(ctx, podReq.ID, pod); err != nil {
				t.Fatalf("Cancel(request %s, by pod %s that made it) after the change = %v; want it cancelled", podReq.ID, pod, err)
			}
			if state := requestState(t, m, podReq.ID); state != "cancelled" {
				t.Fatalf("request %s of pod %s is %s after the pod cancelled it; want cancelled", podReq.ID, pod, state)
			}

			m.PodOperators[samisPod.account] = samisPod.person
			if err := m.Cancel(ctx, boxReq.ID, pod); err != nil {
				t.Fatalf("Cancel(request %s of sami's box, by pod %s) once the entry is back = %v; want it cancelled: only the setting refused it", boxReq.ID, pod, err)
			}
		})
	}
}
