package requests

import (
	"crypto/ecdsa"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// newAirlockFixture is newFixture with a profiles file, file, whose profiles hold fixture names
// behind the proxy, so the policy folds each profile's fingerprint into what a request records. It
// is a fixture of its own because newFixture's loader has no profiles file at all, which is the
// deployment every other row of this package is about.
func newAirlockFixture(t *testing.T, file string) (m *Machine, enrollmentID string, requesterKey *ecdsa.PrivateKey) {
	t.Helper()
	st := storetest.Open(t)
	local := secrets.NewLocal(fixtureSecrets()...)
	enrollmentID, requesterKey = newEnrollment(t, st, "session", "box-a-"+t.Name(), new(fixtureOperator), nil)
	m = &Machine{
		Store:             st,
		Policy:            policytest.ProfiledCurrent(t, local, file, fixtureService),
		Secrets:           policytest.Store(local),
		PendingTTL:        12 * time.Hour,
		Audience:          testAudience,
		Skew:              time.Minute,
		Replay:            replayer(st),
		ServicesByAccount: map[string]string{fixtureSubject: fixtureService},
	}
	m.Chain = NewChainVerifier(st, testAudience, time.Minute, m.Policy)
	return m, enrollmentID, requesterKey
}

// rollProfiles serves file in m's place as the pod a profiles change rolls in does, with its own
// first load: the broker reads that file once at start, so a changed profile reaches a running
// broker no other way.
func rollProfiles(t *testing.T, m *Machine, file string) {
	t.Helper()
	m.Policy = policytest.ProfiledCurrent(t, fixtureStore(m), file, fixtureService)
}

// proxied is the profiles file of policytest.Kinds holding, for each of names, a profile of that
// stored secret's own name behind kind, which its proxy string names.
func proxied(kind string, names ...string) string {
	profiles := make([]string, len(names))
	for i, name := range names {
		profiles[i] = `{"name": "` + name + `", "proxy": "` + kind + `"}`
	}
	return policytest.KindsFile(profiles...)
}

// proxyTag sets the proxy tag (policy.TagProxy) of the secret m's store holds as name to kind, or
// takes the tag off for "", as its owner's `cipher secret retag --proxy KIND|none` does, and has
// m's policy read the namespace again, so from here on the policy serves the name behind that kind
// of its profiles file, or behind none.
func proxyTag(t *testing.T, m *Machine, name, kind string) {
	t.Helper()
	id := aws.String(policytest.ID(name))
	var err error
	if kind == "" {
		_, err = fixtureStore(m).UntagResource(t.Context(), &secretsmanager.UntagResourceInput{SecretId: id, TagKeys: []string{policy.TagProxy}})
	} else {
		_, err = fixtureStore(m).TagResource(t.Context(), &secretsmanager.TagResourceInput{SecretId: id, Tags: []smtypes.Tag{{Key: aws.String(policy.TagProxy), Value: aws.String(kind)}}})
	}
	if err != nil {
		t.Fatalf("set the %s tag of %s to %q: %v", policy.TagProxy, name, kind, err)
	}
	if err := m.Policy.Refresh(t.Context()); err != nil {
		t.Fatalf("policy refresh after the %s tag of %s: %v", policy.TagProxy, name, err)
	}
	if served, held := m.Policy.Get().Secrets[name]; !held || served.Proxy != kind {
		t.Fatalf("after its %s tag is set to %q, %s is served %v behind %q; refused %+v", policy.TagProxy, kind, name, held, served.Proxy, m.Policy.Get().Refused)
	}
}

// profileDigest is the fingerprint the profile name of file has.
func profileDigest(t *testing.T, file, name string) string {
	t.Helper()
	return policytest.Profiles(t, file, fixtureService).Digest(name)
}

// recordedDigests reads the profile fingerprint request requestID recorded for each name it
// names.
func recordedDigests(t *testing.T, m *Machine, requestID string) map[string]string {
	t.Helper()
	rows, err := m.Store.Pool.Query(t.Context(), `select name, airlock_digest from request_secrets where request_id=$1`, requestID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	recorded := map[string]string{}
	for rows.Next() {
		var name, digest string
		if err := rows.Scan(&name, &digest); err != nil {
			t.Fatal(err)
		}
		recorded[name] = digest
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return recorded
}

// TestARequestRecordsEachNamesProfileDigest: a request freezes the fingerprint of every name it
// names, the profile's for one served as a profile and the empty string for a stored secret
// served by its own name behind no kind, which is what the release compares against.
func TestARequestRecordsEachNamesProfileDigest(t *testing.T) {
	t.Parallel()
	file := proxied("example", "AUTO_TOKEN")
	m, enr, key := newAirlockFixture(t, file)
	request, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read both", "AUTO_TOKEN", "DEEL_API_KEY"), "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	recorded := recordedDigests(t, m, request.ID)
	if want := profileDigest(t, file, "AUTO_TOKEN"); want == "" || recorded["AUTO_TOKEN"] != want {
		t.Errorf("AUTO_TOKEN recorded digest = %q, want its profile's, %q", recorded["AUTO_TOKEN"], want)
	}
	if recorded["DEEL_API_KEY"] != "" {
		t.Errorf("DEEL_API_KEY recorded digest = %q, want none: no profile holds it", recorded["DEEL_API_KEY"])
	}
}

// TestAChangedProfileEndsTheGrantsItAffects: a grant whose released name's profile changed ends
// for good at its next release, naming the secret in its grant.revoked audit row, and putting the
// profile back does not bring it back. A grant of a name whose profile did not change goes on
// releasing, even though the change moved the policy version both grants were decided under.
func TestAChangedProfileEndsTheGrantsItAffects(t *testing.T) {
	t.Parallel()
	before := policytest.KindsFile(`{"name": "AUTO_TOKEN", "proxy": "example"}`, `{"name": "SHARED_KEY", "proxy": "slack"}`)
	changed := policytest.KindsFile(`{"name": "AUTO_TOKEN", "proxy": "github"}`, `{"name": "SHARED_KEY", "proxy": "slack"}`)
	m, enr, key := newAirlockFixture(t, before)
	moved, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read the token", "AUTO_TOKEN"), "")
	if err != nil || moved.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, %v; want a grant", moved, err)
	}
	// SHARED_KEY is human tier, so its grant comes of an approval; its entry never moves.
	pending, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read the shared key", "SHARED_KEY"), "")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY) = %+v, %v; want pending", pending, err)
	}
	kept, err := m.ApplyDecision(t.Context(), *pending.RecordID, true, fixtureOperator)
	if err != nil || kept.GrantID == "" {
		t.Fatalf("ApplyDecision(SHARED_KEY) = %+v, %v", kept, err)
	}

	rollProfiles(t, m, changed)
	if release, err := m.Values(t.Context(), *moved.GrantID, enr, ReleaseCall{}); err == nil || release.Values != nil {
		t.Fatalf("Values after the entry changed = %v, %v; want the grant ended", release.Values, err)
	}
	if release, err := m.Values(t.Context(), kept.GrantID, enr, ReleaseCall{}); err != nil || releasedValue(release, "SHARED_KEY") == "" {
		t.Fatalf("Values of the grant whose entry did not change = %+v, %v; want SHARED_KEY released", release, err)
	}

	rollProfiles(t, m, before)
	if release, err := m.Values(t.Context(), *moved.GrantID, enr, ReleaseCall{}); err == nil || release.Values != nil {
		t.Fatalf("Values once the entry is back = %v, %v; want the grant to stay ended", release.Values, err)
	}
	if by, audits := endedForGood(t, m, *moved.GrantID); by != GrantEndedActor || audits != 1 {
		t.Fatalf("grant revoked_by = %q with %d grant.revoked rows by it; want %s and 1", by, audits, GrantEndedActor)
	}
	var reason string
	if err := m.Store.Pool.QueryRow(t.Context(), `select detail->>'reason' from audit where kind='grant.revoked' and grant_id=$1`, *moved.GrantID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if want := ErrGrantNotLive.Error() + ": AUTO_TOKEN's profile changed since this grant was decided"; reason != want {
		t.Fatalf("grant.revoked reason = %q, want %q", reason, want)
	}
	if by, _ := endedForGood(t, m, kept.GrantID); by != "" {
		t.Fatalf("the grant whose entry did not change was revoked by %q; want it live", by)
	}
}

// TestARequestApprovedAcrossAProfilesChangeIsReleasedUnderTheEntryItWasDecidedUnder: a request
// waits on its person for up to half a day, and the profiles file can roll while it waits. What a
// release compares each name against is the profile that name was decided under, and for an
// approved request the decision is the approval: so the grant a person has just made releases,
// where recording the profile at creation alone ended it on its first release with a reason that
// was not true. A profile that moves after the decision still ends the grant, which is what the
// fingerprint is there for.
func TestARequestApprovedAcrossAProfilesChangeIsReleasedUnderTheEntryItWasDecidedUnder(t *testing.T) {
	t.Parallel()
	one, two, three := proxied("example", "SHARED_KEY"), proxied("slack", "SHARED_KEY"), proxied("github", "SHARED_KEY")
	m, enr, key := newAirlockFixture(t, one)
	// SHARED_KEY is shared and human tier, so its grant comes of a person's approval.
	pending, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read the shared key", "SHARED_KEY"), "")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY) = %+v, %v; want pending", pending, err)
	}

	// The profiles roll while the card sits on the approver's page.
	rollProfiles(t, m, two)
	decided, err := m.ApplyDecision(t.Context(), *pending.RecordID, true, fixtureOperator)
	if err != nil || decided.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v; want a grant", decided, err)
	}
	if got, want := recordedDigests(t, m, pending.ID)["SHARED_KEY"], profileDigest(t, two, "SHARED_KEY"); got != want {
		t.Fatalf("the approved request records the digest %q; want %q, the profile it was decided under", got, want)
	}
	if release, err := m.Values(t.Context(), decided.GrantID, enr, ReleaseCall{}); err != nil || releasedValue(release, "SHARED_KEY") == "" {
		t.Fatalf("Values of the grant the approval made = %+v, %v; want SHARED_KEY released: the approval was given under this entry", release, err)
	}
	if by, _ := endedForGood(t, m, decided.GrantID); by != "" {
		t.Fatalf("the grant the approval made was revoked by %q; want it live", by)
	}

	// A profile that moves after the decision ends the grant it was decided under.
	rollProfiles(t, m, three)
	if release, err := m.Values(t.Context(), decided.GrantID, enr, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || release.Values != nil {
		t.Fatalf("Values after the entry changed = %v, %v; want ErrGrantNotLive and no value", release.Values, err)
	}
	if by, audits := endedForGood(t, m, decided.GrantID); by != GrantEndedActor || audits != 1 {
		t.Fatalf("grant revoked_by = %q with %d grant.revoked rows by it; want %s and 1", by, audits, GrantEndedActor)
	}
}

// TestAGrantDecidedUnderAnotherEntryEndsWhenTheVersionReturnsToTheRequests: an approval records
// the entry it was given under (decidedUnder) and leaves the request's policy version as the
// request was written under, so a profiles file that rolls from one entry to another while the
// card waits, is approved under the second, and then rolls back to the first brings the live
// version back to the request's own. The release compares each name's fingerprint all the same,
// whatever the version says: what the person approved was the second entry, and the first, which
// may be the wider, is not what was granted. The grant ends for good, as any other moved entry
// ends it.
func TestAGrantDecidedUnderAnotherEntryEndsWhenTheVersionReturnsToTheRequests(t *testing.T) {
	t.Parallel()
	one, two := proxied("example", "SHARED_KEY"), proxied("slack", "SHARED_KEY")
	m, enr, key := newAirlockFixture(t, one)
	// SHARED_KEY is shared and human tier, so its grant comes of a person's approval.
	pending, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read the shared key", "SHARED_KEY"), "")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY) = %+v, %v; want pending", pending, err)
	}
	written := m.PolicyVersion()

	rollProfiles(t, m, two)
	decided, err := m.ApplyDecision(t.Context(), *pending.RecordID, true, fixtureOperator)
	if err != nil || decided.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v; want a grant", decided, err)
	}
	rollProfiles(t, m, one)
	if got := m.PolicyVersion(); got != written {
		t.Fatalf("the policy version after the roll back = %q; want the request's own %q, which is the case this row is about", got, written)
	}
	if release, err := m.Values(t.Context(), decided.GrantID, enr, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || release.Values != nil {
		t.Fatalf("Values under the entry the request was made under, not the one it was approved under = %v, %v; want ErrGrantNotLive and no value", release.Values, err)
	}
	if by, audits := endedForGood(t, m, decided.GrantID); by != GrantEndedActor || audits != 1 {
		t.Fatalf("grant revoked_by = %q with %d grant.revoked rows by it; want %s and 1", by, audits, GrantEndedActor)
	}
}
