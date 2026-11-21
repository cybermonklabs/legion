// packages/envoy/internal/cipher/requests/record_policy_read_test.go
// The post-merge review's second finding on cybermonklabs/legion#4135: the record a person reads
// is answered on the policy its person rule checked, never on one read after it.
package requests

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// retagOnAsk is a projectaccess.Access that runs retag the first time it is asked about project,
// before it passes the question on: a policy refresh that lands while a reader of a row is between
// its person rule's read of the policy and the answer it builds.
type retagOnAsk struct {
	inner   projectaccess.Access
	project string
	retag   func()
	once    sync.Once
	fired   atomic.Bool
}

func (r *retagOnAsk) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	if project == r.project {
		r.once.Do(func() {
			r.retag()
			r.fired.Store(true)
		})
	}
	return r.inner.Access(ctx, project, person)
}

func (r *retagOnAsk) MemberKind(ctx context.Context, person string) (string, error) {
	return r.inner.MemberKind(ctx, person)
}

func (r *retagOnAsk) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	return r.inner.States(ctx, projects)
}

// TestARecordIsReadOnThePolicyItsPersonRuleChecked: a shared secret's request from a pod naming no
// person, whose reason names TEAM-1, is read for an outsider, and the policy tags its secret with
// PRIV while the person rule asks Dispatch about TEAM, after the rule read the secret's tags. The
// record the outsider is answered is the one the rule checked: it names no project of PRIV's,
// whose list does not hold them. Read again, the rule reads the tag, hides the record from the
// outsider, and shows a person PRIV lists the secret's project.
func TestARecordIsReadOnThePolicyItsPersonRuleChecked(t *testing.T) {
	m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	pod, podKey := everyonesPod(t, m, "pod-policy-read", "")
	pending, err := m.Create(t.Context(), pod, signRequest(t, m, podKey, "work for "+teamProject+"-1", "SHARED_KEY"), "", nil)
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY) = %+v, %v; want it pending", pending, err)
	}
	front := &retagOnAsk{inner: m.Projects, project: teamProject, retag: func() {
		retag(t, m, taggedSecret("SHARED_KEY", policy.OwnerShared, policy.TierHuman, privProject, "shared-v1"))
	}}
	m.Projects = front

	detail, err := m.ReadRecordFor(t.Context(), *pending.RecordID, outsider)
	if !front.fired.Load() {
		t.Fatal("the person rule asked Dispatch nothing about TEAM, so the policy never changed under it: the test is blind")
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		t.Fatalf("ReadRecordFor(%s) as SHARED_KEY turns PRIV's = %v; want the record its rule checked, or pgx.ErrNoRows", outsider, err)
	case slices.ContainsFunc(detail.SecretProjects, func(p SecretProject) bool { return p.Project == privProject }):
		t.Fatalf("ReadRecordFor(%s) as SHARED_KEY turns PRIV's = projects %v; want none of PRIV's, which nobody asked whether %s may see", outsider, detail.SecretProjects, outsider)
	}

	if _, err := m.ReadRecordFor(t.Context(), *pending.RecordID, outsider); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("ReadRecordFor(%s) once SHARED_KEY is PRIV's = %v; want pgx.ErrNoRows", outsider, err)
	}
	shown, err := m.ReadRecordFor(t.Context(), *pending.RecordID, insider)
	if want := []SecretProject{{Name: "SHARED_KEY", Project: privProject, Private: true}}; err != nil || !slices.Equal(shown.SecretProjects, want) {
		t.Errorf("ReadRecordFor(%s) once SHARED_KEY is PRIV's = projects %v, %v; want %v", insider, shown.SecretProjects, err, want)
	}
}
