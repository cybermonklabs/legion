package enroll

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestEnrollmentOperatorIsThePersonAnEnrollmentActsFor: a person's session answers that person,
// a service's pod, which acts for nobody, answers the empty string, and so does each once ended;
// an id no row has is ErrNoEnrollment, never a person.
func TestEnrollmentOperatorIsThePersonAnEnrollmentActsFor(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	session, err := svc.Create(ctx, mintCredential(t, svc, str(aliceLogin), nil, "devbox"),
		Enrollment{Kind: "session", RuntimeID: "box-alice", Thumbprint: "tp-box-alice"})
	if err != nil {
		t.Fatalf("Create(alice's session): %v", err)
	}
	issuer, key := withPodVerifier(t, svc)
	daemon := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
	pod, err := svc.Create(ctx, daemon, Enrollment{
		Kind: "pod", RuntimeID: "pod-operator-1", Thumbprint: "tp-pod-operator",
		PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-operator-1"),
	})
	if err != nil {
		t.Fatalf("Create(pod): %v", err)
	}
	want := func(enr Enrollment, operator, when string) {
		t.Helper()
		if got, err := svc.EnrollmentOperator(ctx, enr.ID.String()); err != nil || got != operator {
			t.Fatalf("EnrollmentOperator(%s) %s = %q, %v; want %q", enr.RuntimeID, when, got, err, operator)
		}
	}
	want(session, aliceLogin, "live")
	want(pod, "", "live")
	if err := svc.Revoke(ctx, daemon, pod.ID.String(), "test"); err != nil {
		t.Fatalf("Revoke(pod): %v", err)
	}
	want(pod, "", "once ended")

	missing := uuid.NewString()
	if got, err := svc.EnrollmentOperator(ctx, missing); !errors.Is(err, ErrNoEnrollment) || got != "" {
		t.Fatalf("EnrollmentOperator(%s), an id no row has = %q, %v; want ErrNoEnrollment", missing, got, err)
	}
}
