// groups_project_test.go holds the sites where a Google group's rule and a private Dispatch
// project's rule both decide: a group's secret tagged with a private project is listed, decided,
// and revoked by a member only while the member may also see the project.
package requests

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/groups/groupstest"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// TestAGroupsSecretOfAPrivateProject: PRIV_GROUP_KEY, the group's human-tier secret tagged PRIV,
// is asked for by the operator's session naming PRIV. The group lists insider, on PRIV's list,
// and outsider, who is not. The request is on insider's Inbox and not outsider's; outsider's
// approval finds no such record, insider's grants it; the grant is on insider's grant list and
// not outsider's; and outsider's revoke finds no such grant, while insider's ends it.
func TestAGroupsSecretOfAPrivateProject(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	local := secrets.NewLocal(append(fixtureSecrets(),
		taggedSecret("PRIV_GROUP_KEY", approversForm, policy.TierHuman, privProject, "priv-group-v1"))...)
	enrollmentID, key := newEnrollment(t, st, "session", "box-pg-"+t.Name(), new(fixtureOperator), nil)
	directory := groupstest.New()
	directory.Set(approversGroup, insider, outsider)
	loader := policytest.Loader(local, fixtureService)
	loader.ProjectChecks, loader.Groups = true, directory
	current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("policy.NewCurrent: %v", err)
	}
	m := &Machine{
		Store: st, Policy: current, Secrets: policytest.Store(local), PendingTTL: 12 * time.Hour,
		Audience: testAudience, Skew: time.Minute, Replay: replayer(st), Groups: directory,
		ServicesByAccount: map[string]string{fixtureSubject: fixtureService},
	}
	m.Chain = NewChainVerifier(st, testAudience, time.Minute, m.Policy)
	m.Chain.Groups = directory
	answerFrom(t, m, dispatchRows(publicTeam))
	nameProjects(t, m, enrollmentID, privProject)

	req, err := m.Create(ctx, enrollmentID, signRequest(t, m, key, "PRIV work", "PRIV_GROUP_KEY"), "")
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("the request for PRIV_GROUP_KEY = %+v, %v; want pending on the group", req, err)
	}
	recordID := *req.RecordID
	for login, want := range map[string]bool{insider: true, outsider: false} {
		pending, err := m.PendingForApprover(ctx, login, PendingStates{})
		if err != nil {
			t.Fatalf("PendingForApprover(%s): %v", login, err)
		}
		if got := slices.ContainsFunc(pending, func(p PendingSummary) bool { return p.RecordID == recordID }); got != want {
			t.Errorf("PendingForApprover(%s) lists the PRIV group request: %v, want %v", login, got, want)
		}
	}
	if _, err := m.ApplyDecision(ctx, recordID, true, outsider); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ApplyDecision by a member outside PRIV = %v; want pgx.ErrNoRows, as for an id no record has", err)
	}
	dec, err := m.ApplyDecision(ctx, recordID, true, insider)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision by a member on PRIV's list = %+v, %v; want a grant", dec, err)
	}
	for login, want := range map[string]bool{insider: true, outsider: false} {
		grants, err := m.GrantsForApprover(ctx, login)
		if err != nil {
			t.Fatalf("GrantsForApprover(%s): %v", login, err)
		}
		if got := slices.ContainsFunc(grants, func(g ApproverGrant) bool { return g.GrantID == dec.GrantID }); got != want {
			t.Errorf("GrantsForApprover(%s) lists the PRIV group grant: %v, want %v", login, got, want)
		}
	}
	if err := m.RevokeByApprover(ctx, dec.GrantID, outsider, record.HumanActor(outsider)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("RevokeByApprover by a member outside PRIV = %v; want pgx.ErrNoRows, as for an id no grant has", err)
	}
	if err := m.RevokeByApprover(ctx, dec.GrantID, insider, record.HumanActor(insider)); err != nil {
		t.Fatalf("RevokeByApprover by a member on PRIV's list = %v, want nil", err)
	}
}
