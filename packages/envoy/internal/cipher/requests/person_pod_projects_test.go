// person_pod_projects_test.go drives a private project's secret to a person's pod: a pod whose
// account BROKER_POD_OPERATORS maps to a person, which names its projects when it enrols as that
// person's own session does (enroll.Service.Create). While the setting maps its account to its
// person, the pod names the projects its enrollment recorded, and every release of a tagged
// secret checks them against that person again, as a session's release does. Once the setting no
// longer maps the account, the pod acts for no person and names no list of its own.
package requests

import (
	"crypto/ecdsa"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// personPodNaming is newProjectFixture with samisPod's account mapped to its person, the fixture's
// operator, whom PRIV's access list names. It enrols one pod of that account, whose enrollment
// names projects, and answers the Machine, the pod's enrollment id and its signing key.
func personPodNaming(t *testing.T, podUID string, projects ...string) (*Machine, string, *ecdsa.PrivateKey) {
	t.Helper()
	m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	m.PodOperators = map[string]string{samisPod.account: samisPod.person}
	pod, key := samisPod.enroll(t, m.Store, podUID)
	nameProjects(t, m, pod, projects...)
	return m, pod, key
}

// TestAPersonsPodNamingAPrivateProjectGetsItsSecretWhileItsPersonSeesIt: sami's pod, its
// enrollment naming PRIV, is granted PRIV_KEY at once and released its value, as sami's session
// naming PRIV is. Once PRIV's access list no longer names sami, the next release ends the grant
// for good, as it ends a session's. Sami's pod naming no project is denied PRIV_KEY
// project-not-named, though PRIV's list names sami (control).
func TestAPersonsPodNamingAPrivateProjectGetsItsSecretWhileItsPersonSeesIt(t *testing.T) {
	m, pod, key := personPodNaming(t, "pod-sami-priv", privProject)
	_, grantID := grantOf(t, m, pod, key, "read it", "PRIV_KEY")
	if release, err := m.Values(t.Context(), grantID, pod, nil, ReleaseCall{}); err != nil || release.Values["PRIV_KEY"] != "priv-v1" {
		t.Fatalf("Values(PRIV_KEY) to sami's pod naming PRIV = %v, %v; want its value", release.Values, err)
	}

	answerFrom(t, m, map[string]projectaccesstest.Row{
		privProject: {Visibility: "private", Owner: otherPerson, Listed: []string{insider}},
		teamProject: publicTeam,
	})
	if release, err := m.Values(t.Context(), grantID, pod, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || release.Values != nil {
		t.Fatalf("Values of grant %s once PRIV's list no longer names %s = %v, %v; want ErrGrantNotLive", grantID, samisPod.person, release.Values, err)
	}
	if by, _ := endedForGood(t, m, grantID); by != GrantEndedActor {
		t.Fatalf("grant %s revoked by %q, want %q", grantID, by, GrantEndedActor)
	}

	other, unnamed, unnamedKey := personPodNaming(t, "pod-sami-none")
	request, err := other.Create(t.Context(), unnamed, signRequest(t, other, unnamedKey, "read it", "PRIV_KEY"), "", nil)
	deniedFor(t, "a request for PRIV_KEY from sami's pod naming no project", request, err, "PRIV_KEY", policy.ReasonProjectNotNamed)
}

// TestAnUnmappedPersonsPodNamesNoListOfItsOwn: sami's pod, its enrollment naming PRIV, holds a
// grant of PRIV_KEY. Once BROKER_POD_OPERATORS drops its account, the pod acts for no person, so
// it names what a pod of no person names: its operator entry's project, and here no entry maps
// the account. The next release ends the grant for good.
func TestAnUnmappedPersonsPodNamesNoListOfItsOwn(t *testing.T) {
	m, pod, key := personPodNaming(t, "pod-sami-unmapped", privProject)
	_, grantID := grantOf(t, m, pod, key, "read it", "PRIV_KEY")
	delete(m.PodOperators, samisPod.account)
	if release, err := m.Values(t.Context(), grantID, pod, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || release.Values != nil {
		t.Fatalf("Values of grant %s once the setting drops %s = %v, %v; want ErrGrantNotLive", grantID, samisPod.account, release.Values, err)
	}
	if by, _ := endedForGood(t, m, grantID); by != GrantEndedActor {
		t.Fatalf("grant %s revoked by %q, want %q", grantID, by, GrantEndedActor)
	}
}
