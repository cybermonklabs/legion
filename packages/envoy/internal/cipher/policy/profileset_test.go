package policy_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// The sources of the profile tests: secrets in the store, each owned by appOwner.
const (
	appOwner = "ada@example.com"
	// appSourceName is an agent-tier GitHub App of the accounts acme and ada
	// (policytest.FixtureGitHubOwners), with no requesters tag, so every session and pod may ask.
	appSourceName = "LEGION_IMPLEMENT_APP"
	// daemonService is the one service the profile tests' deployment registers.
	daemonService = "legion-daemon"
	// listedRequesters is the requesters tag of listedApp, as ParseRequesters answers it.
	listedRequesters = "ada@example.com legion-daemon"
)

// readProfile is GITHUB_READ, the profile every test file holds beside the profile it is about:
// reads of acme through appSourceName, as a bare token behind the proxy, under the github kind its
// proxy string names. It passes against its source, so a file whose other profile fails still
// serves it.
const readProfile = `{"name": "GITHUB_READ", "source": "LEGION_IMPLEMENT_APP",
  "github": {"accounts": ["acme"], "permissions": {"contents": "read", "metadata": "read"}, "value": "token"},
  "proxy": "github"}`

// acmeRead is a github block that fits every github-app source here: reads on acme.
const acmeRead = `"github": {"accounts": ["acme"], "permissions": {"metadata": "read"}}`

// apiProxy is a proxy string naming the test kind every source's type takes a way of
// (policytest.Kinds' example).
const apiProxy = `"proxy": "example"`

// appSource is appSourceName holding a value the decoder reads.
func appSource() memory.Secret {
	return policytest.TypeSecret(appSourceName, appOwner, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
}

// humanApp is HUMAN_APP, a human-tier GitHub App of acme and ada.
func humanApp() memory.Secret {
	return policytest.TypeSecret("HUMAN_APP", appOwner, policy.TierHuman, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
}

// listedApp is LISTED_APP, an agent-tier GitHub App that only its owner and the daemon service
// may ask for, for an hour at most.
func listedApp() memory.Secret {
	app := policytest.TypeSecret("LISTED_APP", appOwner, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
	app.Tags[policy.TagRequesters] = listedRequesters
	app.Tags[policy.TagMaxGrant] = "1h"
	return app
}

// robotKey is ROBOT_KEY, a google-service-account secret whose token carries bigquery and
// cloud-platform.
func robotKey() memory.Secret {
	robot := policytest.TypeSecret("ROBOT_KEY", appOwner, policy.TierHuman, policy.TypeGoogleServiceAccount, policytest.TypeValue(policy.TypeGoogleServiceAccount))
	robot.Tags[policy.TagScopes] = "bigquery cloud-platform"
	return robot
}

// everySource is one source of each kind a profile names: the three Apps, a stored value, a second
// stored value whose name a profile may take, a google-login and an azure-login secret, and a
// google-service-account one.
func everySource() []memory.Secret {
	return []memory.Secret{
		appSource(), humanApp(), listedApp(), robotKey(),
		policytest.Secret("PLAIN_KEY", appOwner, policy.TierAgent, "plain-v1"),
		policytest.Secret("TAKEN_KEY", appOwner, policy.TierAgent, "taken-v1"),
		policytest.TypeSecret("LOGIN_KEY", appOwner, policy.TierAgent, policy.TypeGoogleLogin, policytest.TypeValue(policy.TypeGoogleLogin)),
		policytest.TypeSecret("AZURE_KEY", appOwner, policy.TierAgent, policy.TypeAzureLogin, policytest.TypeValue(policy.TypeAzureLogin)),
	}
}

// profiled is the live policy of a store holding stored, under the profiles file holding
// profiles and the test kinds (policytest.KindsFile), with daemonService registered.
func profiled(t *testing.T, stored []memory.Secret, profiles ...string) (*policy.Current, *memory.Store) {
	t.Helper()
	store := memory.New(stored...)
	return policytest.ProfiledCurrent(t, store, policytest.KindsFile(profiles...), daemonService), store
}

// profileKey is the key a profile's refusal has in Set.Refused.
func profileKey(name string) string { return "profile:" + name }

// refusedProfile fails t unless set refuses the profile name, and serves nothing by that name,
// for reason; it answers the refusal.
func refusedProfile(t *testing.T, set *policy.Set, name, reason string) policy.Refusal {
	t.Helper()
	if secret, served := set.Secrets[name]; served {
		t.Fatalf("%s is served (%+v); want it refused %s", name, secret, reason)
	}
	refusal, refused := set.Refused[profileKey(name)]
	if !refused || refusal.Reason != reason {
		t.Fatalf("Refused[%s] = %+v (present %t); want reason %s", profileKey(name), refusal, refused, reason)
	}
	return refusal
}

// servedProfile fails t unless set serves name as the profile of its name, and answers it.
func servedProfile(t *testing.T, set *policy.Set, name string) policy.Secret {
	t.Helper()
	secret, served := set.Secrets[name]
	if !served || secret.Profile == nil || secret.Profile.Name != name {
		t.Fatalf("%s served = %t as %+v, refused %+v; want it served as its profile", name, served, secret, set.Refused[profileKey(name)])
	}
	return secret
}

// TestAPassingProfileIsServedByItsName: a profile that passes against its source is served by its
// own name, as a secret made of its source: the source's value (Source), owner and type, the
// profile's own scope (its accounts, as GitHubOwners), its entry's digest, the profile itself, and
// its source's value version, by which a mint the decoder refuses refuses that source.
func TestAPassingProfileIsServedByItsName(t *testing.T) {
	cur, store := profiled(t, []memory.Secret{appSource()}, readProfile)
	base, err := policytest.Loader(store, daemonService).Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	source := base.Secrets[appSourceName]
	profiles := policytest.Profiles(t, policytest.KindsFile(readProfile), daemonService)
	set := cur.Get()
	got := servedProfile(t, set, "GITHUB_READ")
	if got.Source != source.Source || got.Owner != appOwner || got.Tier != policy.TierAgent || got.Type() != policy.TypeGitHubApp {
		t.Errorf("GITHUB_READ = %+v; want its source's value %s, owner %s, tier agent and type github-app", got, source.Source, appOwner)
	}
	if got.GitHubOwners != "acme" || got.Requesters != "" || got.MaxGrant != 0 {
		t.Errorf("GITHUB_READ's accounts, requesters and maxGrant = %q, %q, %s; want the profile's acme and the source's none", got.GitHubOwners, got.Requesters, got.MaxGrant)
	}
	if got.ProfileDigest == "" || got.ProfileDigest != profiles.Digest("GITHUB_READ") {
		t.Errorf("GITHUB_READ's ProfileDigest = %q; want its entry's, %q", got.ProfileDigest, profiles.Digest("GITHUB_READ"))
	}
	if !reflect.DeepEqual(*got.Profile, *profiles.Lookup("GITHUB_READ")) {
		t.Errorf("GITHUB_READ's profile = %+v; want the file's entry, %+v", got.Profile, profiles.Lookup("GITHUB_READ"))
	}
	if version := set.ValueVersion("GITHUB_READ"); version == "" || version != base.ValueVersion(appSourceName) {
		t.Errorf("ValueVersion(GITHUB_READ) = %q; want its source's, %q", version, base.ValueVersion(appSourceName))
	}
	if refusal, refused := set.Refused[profileKey("GITHUB_READ")]; refused {
		t.Errorf("GITHUB_READ is refused as well: %+v", refusal)
	}
	d, err := set.Evaluate("GITHUB_READ", policy.Requester{Operator: appOwner})
	if err != nil || d.Outcome != policy.Automatic || d.Source != source.Source {
		t.Errorf("Evaluate(GITHUB_READ) for its owner = %+v, %v; want automatic from its source", d, err)
	}
}

// TestAFailedProfileIsRefusedAloneAndTheOthersServe holds one row for each way a profile fails
// against its source (R2): the profile is not served, the policy's refusals name it with the row's
// reason, and GITHUB_READ, the passing profile in the same file, is still served.
func TestAFailedProfileIsRefusedAloneAndTheOthersServe(t *testing.T) {
	for _, row := range []struct {
		name, profile, reason string
	}{
		{"its source is not a stored secret", `{"name": "GHOST_READ", "source": "NO_SUCH_APP", ` + acmeRead + `, ` + apiProxy + `}`, policy.ReasonProfileSourceMissing},
		{"its source is another profile", `{"name": "READ_OF_READ", "source": "GITHUB_READ", ` + acmeRead + `, ` + apiProxy + `}`, policy.ReasonProfileSourceMissing},
		{"its name is another stored secret's", `{"name": "TAKEN_KEY", "source": "LEGION_IMPLEMENT_APP", ` + acmeRead + `}`, policy.ReasonProfileNameTaken},
		{"a github block naming an account its source does not", `{"name": "OTHER_READ", "source": "LEGION_IMPLEMENT_APP", "github": {"accounts": ["other-org"], "permissions": {"metadata": "read"}}}`, policy.ReasonProfileBlockMismatch},
		{"a github block on a stored value", `{"name": "PLAIN_READ", "source": "PLAIN_KEY", ` + acmeRead + `, ` + apiProxy + `}`, policy.ReasonProfileBlockMismatch},
		{"no github block on a github-app source", `{"name": "BARE_APP", "source": "LEGION_IMPLEMENT_APP", ` + apiProxy + `}`, policy.ReasonProfileBlockMismatch},
		{"a google block naming a scope its source does not", `{"name": "ROBOT_ADMIN", "source": "ROBOT_KEY", "google": {"scopes": ["admin.directory.group"]}}`, policy.ReasonProfileBlockMismatch},
		{"a google block on a google-login source", `{"name": "LOGIN_SCOPED", "source": "LOGIN_KEY", "google": {"scopes": ["cloud-platform"]}, ` + apiProxy + `}`, policy.ReasonProfileBlockMismatch},
		{"agent tier on a human source", `{"name": "HUMAN_READ", "source": "HUMAN_APP", "tier": "agent", ` + acmeRead + `}`, policy.ReasonProfileLooser},
		{"a requester its source does not list", `{"name": "LISTED_READ", "source": "LISTED_APP", "requesters": ["bob@example.com"], ` + acmeRead + `}`, policy.ReasonProfileLooser},
		{"a maxGrant above its source's", `{"name": "LISTED_LONG", "source": "LISTED_APP", "maxGrant": "2h", ` + acmeRead + `}`, policy.ReasonProfileLooser},
		{"no proxy on a stored value", `{"name": "PLAIN_KEY"}`, policy.ReasonProfileSourceNotProxied},
		{"no proxy on a google-login source", `{"name": "LOGIN_READ", "source": "LOGIN_KEY"}`, policy.ReasonProfileSourceNotProxied},
		{"no proxy on an azure-login source", `{"name": "AZURE_READ", "source": "AZURE_KEY"}`, policy.ReasonProfileSourceNotProxied},
		{"an allow of its own and no proxy", `{"name": "ALLOWED_READ", "source": "LEGION_IMPLEMENT_APP", ` + acmeRead + `, "allow": []}`, policy.ReasonProfileSourceNotProxied},
	} {
		t.Run(row.name, func(t *testing.T) {
			cur, _ := profiled(t, everySource(), readProfile, row.profile)
			set := cur.Get()
			name := policytest.Profiles(t, policytest.KindsFile(row.profile), daemonService).Names()[0]
			if refusal := refusedProfile(t, set, name, row.reason); refusal.Detail == "" {
				t.Errorf("%s's refusal names no detail: %+v", name, refusal)
			}
			servedProfile(t, set, "GITHUB_READ")
		})
	}
}

// TestAProfileLooserThanItsSourceIsRefused: a profile is no looser than its source in tier,
// requesters or maxGrant. For requesters, a source with no requesters tag serves every session
// and pod, so any list narrows it; a listed source takes a subset of its list; and a profile with
// no list takes the source's.
func TestAProfileLooserThanItsSourceIsRefused(t *testing.T) {
	// outcome is how a file holding readProfile and profile answers profile's name.
	outcome := func(t *testing.T, profile string) *policy.Set {
		t.Helper()
		cur, _ := profiled(t, everySource(), readProfile, profile)
		return cur.Get()
	}
	t.Run("tier", func(t *testing.T) {
		refusedProfile(t, outcome(t, `{"name": "HUMAN_READ", "source": "HUMAN_APP", "tier": "agent", `+acmeRead+`}`), "HUMAN_READ", policy.ReasonProfileLooser)
		if got := servedProfile(t, outcome(t, `{"name": "ASKED_READ", "source": "LEGION_IMPLEMENT_APP", "tier": "human", `+acmeRead+`}`), "ASKED_READ"); got.Tier != policy.TierHuman {
			t.Errorf("a human-tier profile of an agent-tier source has tier %s; want human", got.Tier)
		}
	})
	t.Run("maxGrant", func(t *testing.T) {
		refusedProfile(t, outcome(t, `{"name": "LISTED_LONG", "source": "LISTED_APP", "maxGrant": "2h", `+acmeRead+`}`), "LISTED_LONG", policy.ReasonProfileLooser)
		if got := servedProfile(t, outcome(t, `{"name": "LISTED_SHORT", "source": "LISTED_APP", "maxGrant": "30m", `+acmeRead+`}`), "LISTED_SHORT"); got.MaxGrant != 30*time.Minute {
			t.Errorf("a 30m profile of a 1h source grants at most %s; want 30m", got.MaxGrant)
		}
		if got := servedProfile(t, outcome(t, `{"name": "CAPPED_READ", "source": "LEGION_IMPLEMENT_APP", "maxGrant": "2h", `+acmeRead+`}`), "CAPPED_READ"); got.MaxGrant != 2*time.Hour {
			t.Errorf("a 2h profile of a source with no max-grant grants at most %s; want 2h", got.MaxGrant)
		}
	})
	t.Run("requesters", func(t *testing.T) {
		for _, row := range []struct {
			name, profile, want string
		}{
			{"a list on a source with no requesters tag is served", `{"name": "DAEMON_READ", "source": "LEGION_IMPLEMENT_APP", "requesters": ["legion-daemon"], ` + acmeRead + `}`, "legion-daemon"},
			{"a subset of a listed source's list is served", `{"name": "DAEMON_READ", "source": "LISTED_APP", "requesters": ["legion-daemon"], ` + acmeRead + `}`, "legion-daemon"},
			{"an entry the source's list does not name is refused", `{"name": "DAEMON_READ", "source": "LISTED_APP", "requesters": ["ada@example.com", "bob@example.com"], ` + acmeRead + `}`, ""},
			{"no list takes the source's", `{"name": "DAEMON_READ", "source": "LISTED_APP", ` + acmeRead + `}`, listedRequesters},
		} {
			t.Run(row.name, func(t *testing.T) {
				set := outcome(t, row.profile)
				if row.want == "" {
					refusedProfile(t, set, "DAEMON_READ", policy.ReasonProfileLooser)
					return
				}
				if got := servedProfile(t, set, "DAEMON_READ"); got.Requesters != row.want {
					t.Errorf("DAEMON_READ's requesters = %q; want %q", got.Requesters, row.want)
				}
			})
		}
	})
}

// TestAFailedProfilesNameReleasesNoStoredValue: a profile that holds a stored secret's own name and
// fails leaves that name released by nobody, rather than as the stored value: a request for it is
// refused, with the profile's reason. A stored value whose profile is behind no kind is refused by
// its own id too, so the store's refusals say the secret is served by no name.
func TestAFailedProfilesNameReleasesNoStoredValue(t *testing.T) {
	cur, _ := profiled(t, everySource(), readProfile, `{"name": "PLAIN_KEY"}`)
	set := cur.Get()
	refusedProfile(t, set, "PLAIN_KEY", policy.ReasonProfileSourceNotProxied)
	_, err := set.Evaluate("PLAIN_KEY", policy.Requester{Operator: appOwner})
	if !errors.Is(err, policy.ErrProfileRefused) || !errors.Is(err, policy.ErrUnknownSecret) || !strings.Contains(err.Error(), policy.ReasonProfileSourceNotProxied) {
		t.Fatalf("Evaluate(PLAIN_KEY) = %v; want ErrProfileRefused, a name the policy does not serve, naming %s", err, policy.ReasonProfileSourceNotProxied)
	}
	if refusal := set.Refused[policytest.ID("PLAIN_KEY")]; refusal.Reason != policy.ReasonProfileSourceNotProxied || refusal.Owner != appOwner {
		t.Errorf("the stored PLAIN_KEY's refusal = %+v; want %s with its owner", refusal, policy.ReasonProfileSourceNotProxied)
	}
}

// TestAProfileNamedForAnotherStoredSecretRefusesBoth: a profile whose name a stored secret other
// than its source holds is refused, and so is that stored secret, until one of the two goes.
func TestAProfileNamedForAnotherStoredSecretRefusesBoth(t *testing.T) {
	cur, store := profiled(t, everySource(), readProfile, `{"name": "TAKEN_KEY", "source": "LEGION_IMPLEMENT_APP", `+acmeRead+`}`)
	set := cur.Get()
	refusedProfile(t, set, "TAKEN_KEY", policy.ReasonProfileNameTaken)
	if refusal := set.Refused[policytest.ID("TAKEN_KEY")]; refusal.Reason != policy.ReasonProfileNameTaken || refusal.Owner != appOwner {
		t.Errorf("the stored TAKEN_KEY's refusal = %+v; want %s with its owner", refusal, policy.ReasonProfileNameTaken)
	}
	if _, err := set.Evaluate("TAKEN_KEY", policy.Requester{Operator: appOwner}); !errors.Is(err, policy.ErrProfileRefused) {
		t.Errorf("Evaluate(TAKEN_KEY) = %v; want ErrProfileRefused", err)
	}
	servedProfile(t, set, "GITHUB_READ")

	store.Delete(policytest.ID("TAKEN_KEY"))
	if err := cur.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	set = cur.Get()
	if got := servedProfile(t, set, "TAKEN_KEY"); got.Type() != policy.TypeGitHubApp {
		t.Errorf("TAKEN_KEY once the stored secret went = %+v; want the profile of the App", got)
	}
	if refusal, refused := set.Refused[policytest.ID("TAKEN_KEY")]; refused {
		t.Errorf("the deleted stored TAKEN_KEY is still refused: %+v", refusal)
	}
}

// TestAProfileOfAnAdminSourceIsAdmin: a profile of an admin source is admin, and held to the admin
// tag's rules on its own tags: one that keeps the source's human tier is served as admin, and an
// agent-tier one is refused admin-tier.
func TestAProfileOfAnAdminSourceIsAdmin(t *testing.T) {
	admin := policytest.ListedSecret("ADMIN_PAT", appOwner, policy.TierHuman, appOwner, "pat-v1")
	admin.Tags[policy.TagAdmin] = policy.AdminTrue
	cur, _ := profiled(t, []memory.Secret{appSource(), admin}, readProfile,
		`{"name": "ADMIN_READ", "source": "ADMIN_PAT", `+apiProxy+`}`,
		`{"name": "ADMIN_AGENT", "source": "ADMIN_PAT", "tier": "agent", `+apiProxy+`}`)
	set := cur.Get()
	if got := servedProfile(t, set, "ADMIN_READ"); !got.Admin || got.Tier != policy.TierHuman || got.Requesters != appOwner || got.MaxGrant != time.Hour {
		t.Errorf("ADMIN_READ = %+v; want admin, human tier, its source's requesters and an hour's cap", got)
	}
	refusedProfile(t, set, "ADMIN_AGENT", policy.ReasonAdminTier)
}

// TestWithholdingASourceWithholdsItsProfiles: a session whose operator withheld a source asks for
// each profile of it as for a human-tier secret, and a withhold of another name changes nothing.
func TestWithholdingASourceWithholdsItsProfiles(t *testing.T) {
	cur, _ := profiled(t, everySource(), readProfile)
	set := cur.Get()
	for _, row := range []struct {
		withheld []string
		want     string
	}{
		{nil, policy.Automatic},
		{[]string{"PLAIN_KEY"}, policy.Automatic},
		{[]string{appSourceName}, policy.Approval},
		{[]string{"GITHUB_READ"}, policy.Approval},
	} {
		d, err := set.Evaluate("GITHUB_READ", policy.Requester{Operator: appOwner, Withheld: row.withheld})
		if err != nil || d.Outcome != row.want || (row.want == policy.Approval && d.Approver != appOwner) {
			t.Errorf("Evaluate(GITHUB_READ) withholding %v = %+v, %v; want %s", row.withheld, d, err, row.want)
		}
	}
}

// TestAWithholdDecidesBySource: a withheld name withholds every name whose source is its source,
// in both directions. A name's source is the source of the profile the set serves or refuses by
// that name, and the name itself for any other: a stored secret served by its own name, a source
// asked for by its own name, or a name the policy has never heard of. So a read profile withholds
// its write sibling and the profile that holds its source's name, a refused profile withholds and
// is withheld as its file names it, and a stored secret no profile uses withholds itself alone.
func TestAWithholdDecidesBySource(t *testing.T) {
	cur, _ := profiled(t, everySource(), readProfile,
		`{"name": "GITHUB_WRITE", "source": "LEGION_IMPLEMENT_APP", "github": {"accounts": ["acme"], "permissions": {"contents": "write", "metadata": "read"}}}`,
		`{"name": "LEGION_IMPLEMENT_APP", "github": {"accounts": ["acme"], "permissions": {"contents": "write", "metadata": "read"}}}`,
		`{"name": "GITHUB_WIDE", "source": "LEGION_IMPLEMENT_APP", "github": {"accounts": ["other-org"], "permissions": {"metadata": "read"}}}`,
		`{"name": "PLAIN_READ", "source": "PLAIN_KEY", `+apiProxy+`}`)
	set := cur.Get()
	for _, name := range []string{"GITHUB_READ", "GITHUB_WRITE", appSourceName, "PLAIN_READ"} {
		servedProfile(t, set, name)
	}
	refusedProfile(t, set, "GITHUB_WIDE", policy.ReasonProfileBlockMismatch)
	if secret, served := set.Secrets["TAKEN_KEY"]; !served || secret.Profile != nil {
		t.Fatalf("TAKEN_KEY = %+v (served %t); want it served by its own name, as no profile", secret, served)
	}
	for _, row := range []struct {
		withheld, name string
		want           bool
	}{
		{"GITHUB_READ", "GITHUB_WRITE", true},
		{"GITHUB_READ", appSourceName, true},
		{appSourceName, "GITHUB_READ", true},
		{appSourceName, "GITHUB_WRITE", true},
		{"GITHUB_WIDE", "GITHUB_WRITE", true},
		{"GITHUB_READ", "GITHUB_WIDE", true},
		{"PLAIN_READ", "PLAIN_KEY", true},
		{"PLAIN_KEY", "PLAIN_READ", true},
		{"TAKEN_KEY", "TAKEN_KEY", true},
		{"TAKEN_KEY", "PLAIN_KEY", false},
		{"TAKEN_KEY", "GITHUB_READ", false},
		{"GITHUB_READ", "PLAIN_READ", false},
		{"PLAIN_READ", "GITHUB_WRITE", false},
		{"GONE_KEY", "GONE_KEY", true},
		{"GONE_KEY", "GITHUB_READ", false},
	} {
		if got := set.Withholds(policy.Requester{Withheld: []string{row.withheld}}, row.name); got != row.want {
			t.Errorf("Withholds(%s) withholding %s = %t; want %t", row.name, row.withheld, got, row.want)
		}
	}
	if set.Withholds(policy.Requester{}, "GITHUB_READ") {
		t.Errorf("Withholds(GITHUB_READ) withholding nothing = true; want false")
	}
}

// TestAnEditedProfileEndsItsGrants: a change of one permission of a profile's entry moves the
// policy version and the profile's digest, which a release compares its grant against; a guidance
// tag written on its source, which tells the judge how to decide requests to come, moves neither.
func TestAnEditedProfileEndsItsGrants(t *testing.T) {
	served := func(stored []memory.Secret, profile string) (version, digest string) {
		t.Helper()
		cur, _ := profiled(t, stored, profile)
		set := cur.Get()
		return set.Version, servedProfile(t, set, "GITHUB_READ").ProfileDigest
	}
	version, digest := served(everySource(), readProfile)
	editedVersion, editedDigest := served(everySource(), strings.Replace(readProfile, `"contents": "read"`, `"contents": "write"`, 1))
	if editedVersion == version || editedDigest == digest {
		t.Errorf("an edited permission left the version %t and the digest %t where they were; want both moved", editedVersion == version, editedDigest == digest)
	}
	guided := everySource()
	tagged := 0
	for _, s := range guided {
		if s.Name == policytest.ID("LEGION_IMPLEMENT_APP") {
			s.Tags[policy.TagGuidance] = "Reads of acme's repositories."
			tagged++
		}
	}
	if tagged != 1 {
		t.Fatalf("tagged %d secrets named LEGION_IMPLEMENT_APP, want the one source GITHUB_READ is made from", tagged)
	}
	guidedVersion, guidedDigest := served(guided, readProfile)
	if guidedVersion != version || guidedDigest != digest {
		t.Errorf("a guidance tag on the source moved the version (%t) or the digest (%t); want neither moved", guidedVersion != version, guidedDigest != digest)
	}
}

// TestASourceIsServedOnlyThroughItsProfiles: a source that profiles of other names name is not
// served by its own name, while its profiles pass or fail; a request for it by that name is
// refused, and it is no refusal of its own. The refusal names only the profiles the set serves,
// to ask for in its place, and never a refused one, for which a caller would be refused again;
// a source the set serves none of the profiles of is refused saying that each is refused, and
// naming them.
func TestASourceIsServedOnlyThroughItsProfiles(t *testing.T) {
	cur, _ := profiled(t, everySource(), readProfile,
		`{"name": "GITHUB_WIDE", "source": "LEGION_IMPLEMENT_APP", "github": {"accounts": ["other-org"], "permissions": {"metadata": "read"}}}`,
		`{"name": "LISTED_LONG", "source": "LISTED_APP", "maxGrant": "2h", `+acmeRead+`}`,
		`{"name": "PLAIN_READ", "source": "PLAIN_KEY", `+apiProxy+`}`)
	set := cur.Get()
	// GITHUB_WIDE names an account its source's github-owners does not, and LISTED_LONG a
	// maxGrant above its source's max-grant: each row below is about a profile the set refuses.
	refusedProfile(t, set, "GITHUB_WIDE", policy.ReasonProfileBlockMismatch)
	refusedProfile(t, set, "LISTED_LONG", policy.ReasonProfileLooser)
	for _, row := range []struct {
		source   string
		profiles []string // ProfilesOf: every profile of the source, in the file's order
		says     []string // what the refusal says
		never    []string // what it must not say
	}{
		{appSourceName, []string{"GITHUB_READ", "GITHUB_WIDE"}, []string{"ask for GITHUB_READ"}, []string{"GITHUB_WIDE"}},
		{"LISTED_APP", []string{"LISTED_LONG"}, []string{"each of them is refused", "LISTED_LONG"}, []string{"ask for"}},
		{"PLAIN_KEY", []string{"PLAIN_READ"}, []string{"ask for PLAIN_READ"}, []string{"refused"}},
	} {
		if secret, served := set.Secrets[row.source]; served {
			t.Errorf("%s is served by its own name (%+v); want it served only through %v", row.source, secret, row.profiles)
		}
		if got := set.ProfilesOf(row.source); !slices.Equal(got, row.profiles) {
			t.Errorf("ProfilesOf(%s) = %v; want %v", row.source, got, row.profiles)
		}
		_, err := set.Evaluate(row.source, policy.Requester{Operator: appOwner})
		if !errors.Is(err, policy.ErrServedOnlyAsProfiles) || !errors.Is(err, policy.ErrUnknownSecret) {
			t.Errorf("Evaluate(%s) = %v; want ErrServedOnlyAsProfiles, a name the policy does not serve", row.source, err)
			continue
		}
		for _, said := range row.says {
			if !strings.Contains(err.Error(), said) {
				t.Errorf("Evaluate(%s)'s refusal %q does not say %q", row.source, err, said)
			}
		}
		for _, unsaid := range row.never {
			if strings.Contains(err.Error(), unsaid) {
				t.Errorf("Evaluate(%s)'s refusal %q says %q", row.source, err, unsaid)
			}
		}
		if refusal, refused := set.Refused[policytest.ID(row.source)]; refused {
			t.Errorf("%s is refused %+v; want it served through its profiles, which is no refusal", row.source, refusal)
		}
	}
	servedProfile(t, set, "PLAIN_READ")
}

// TestAProfileOfTheSourcesOwnNameServesThatName: a profile that holds its source's own name serves
// that name as the profile, with the profile's scope and digest, beside any profile of another
// name of the same source.
func TestAProfileOfTheSourcesOwnNameServesThatName(t *testing.T) {
	own := `{"name": "LEGION_IMPLEMENT_APP", ` + acmeRead + `}`
	cur, _ := profiled(t, everySource(), readProfile, own)
	profiles := policytest.Profiles(t, policytest.KindsFile(readProfile, own), daemonService)
	set := cur.Get()
	got := servedProfile(t, set, appSourceName)
	if got.GitHubOwners != "acme" || got.ProfileDigest != profiles.Digest(appSourceName) || got.Type() != policy.TypeGitHubApp {
		t.Errorf("%s = %+v; want the profile's account acme and digest %s", appSourceName, got, profiles.Digest(appSourceName))
	}
	if d, err := set.Evaluate(appSourceName, policy.Requester{Operator: appOwner}); err != nil || d.Outcome != policy.Automatic {
		t.Errorf("Evaluate(%s) = %+v, %v; want automatic", appSourceName, d, err)
	}
	servedProfile(t, set, "GITHUB_READ")
}

// TestAGitHubAppWithNoProfileIsRefusedByName: a github-app secret no profile names is refused by
// its name, with a refusal that says it has no profile, in a deployment with a profiles file and
// in one with none; a stored value no profile names is served as always.
func TestAGitHubAppWithNoProfileIsRefusedByName(t *testing.T) {
	app := policytest.TypeSecret("APP_KEY", appOwner, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
	plain := policytest.Secret("PLAIN_KEY", appOwner, policy.TierAgent, "plain-v1")
	for name, cur := range map[string]*policy.Current{
		"an empty profiles file": policytest.ProfiledCurrent(t, memory.New(app, plain), policytest.ProfilesFile()),
		"no profiles file":       policytest.Current(t, memory.New(app, plain)),
	} {
		t.Run(name, func(t *testing.T) {
			set := cur.Get()
			if secret, served := set.Secrets["APP_KEY"]; served {
				t.Fatalf("APP_KEY is served (%+v); want it refused", secret)
			}
			refusal := set.Refused[policytest.ID("APP_KEY")]
			if refusal.Reason != policy.ReasonGitHubAppNoProfile || refusal.Owner != appOwner || !strings.Contains(refusal.Detail, "no profile") {
				t.Errorf("APP_KEY's refusal = %+v; want %s with its owner, saying it has no profile", refusal, policy.ReasonGitHubAppNoProfile)
			}
			_, err := set.Evaluate("APP_KEY", policy.Requester{Operator: appOwner})
			if !errors.Is(err, policy.ErrProfileRefused) || !strings.Contains(err.Error(), "no profile") {
				t.Errorf("Evaluate(APP_KEY) = %v; want ErrProfileRefused saying it has no profile", err)
			}
			if _, served := set.Secrets["PLAIN_KEY"]; !served {
				t.Errorf("PLAIN_KEY, a stored value no profile names, is not served: %+v", set.Refused[policytest.ID("PLAIN_KEY")])
			}
		})
	}
}

// TestAProfileOfAValueRefusedSourceNamesTheSourcesReason: a source whose value the decoder refuses
// is refused value-malformed, as a source is today, and each profile of it is refused with a
// detail that names that reason.
func TestAProfileOfAValueRefusedSourceNamesTheSourcesReason(t *testing.T) {
	broken := appSource()
	broken.Value = "not an App's value"
	cur, _ := profiled(t, []memory.Secret{broken}, readProfile)
	set := cur.Get()
	if refusal := set.Refused[policytest.ID(appSourceName)]; refusal.Reason != policy.ReasonValueMalformed {
		t.Fatalf("the source's refusal = %+v; want %s", refusal, policy.ReasonValueMalformed)
	}
	if refusal := refusedProfile(t, set, "GITHUB_READ", policy.ReasonProfileSourceMissing); !strings.Contains(refusal.Detail, policy.ReasonValueMalformed) {
		t.Errorf("GITHUB_READ's refusal detail %q does not name its source's reason %s", refusal.Detail, policy.ReasonValueMalformed)
	}
	lk, err := cur.RefreshOne(t.Context(), "GITHUB_READ")
	if err != nil || lk.Served || lk.Reason != policy.ReasonProfileSourceMissing {
		t.Errorf("RefreshOne(GITHUB_READ) = %+v, %v; want it refused %s", lk, err, policy.ReasonProfileSourceMissing)
	}
}

// describedIDs records the secret id of every Describe call made through it.
type describedIDs struct {
	secretstore.Store
	mu  sync.Mutex
	ids []string
}

func (d *describedIDs) Describe(ctx context.Context, id string, opts secretstore.ReadOptions) (secretstore.Listing, error) {
	d.mu.Lock()
	d.ids = append(d.ids, id)
	d.mu.Unlock()
	return d.Store.Describe(ctx, id, opts)
}

func (d *describedIDs) take() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := d.ids
	d.ids = nil
	return ids
}

// TestRereadingAProfileNameRereadsItsSource: a reread of a profile's name reads its source, which
// is the only secret a store holds for it, and answers for the profile as the policy then serves it.
func TestRereadingAProfileNameRereadsItsSource(t *testing.T) {
	store := memory.New(appSource())
	described := &describedIDs{Store: store}
	loader := policytest.Loader(described, daemonService)
	loader.Profiles = policytest.Profiles(t, policytest.KindsFile(readProfile), daemonService)
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	described.take()

	store.Put(policytest.TypeSecret(appSourceName, appOwner, policy.TierHuman, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)))
	lk, err := cur.RefreshOne(t.Context(), "GITHUB_READ")
	if err != nil || !lk.Served || lk.Secret.Name != "GITHUB_READ" || lk.Secret.Tier != policy.TierHuman {
		t.Fatalf("RefreshOne(GITHUB_READ) after its source became human tier = %+v, %v; want the profile served at human tier", lk, err)
	}
	if got := cur.Get().Secrets["GITHUB_READ"].Tier; got != policy.TierHuman {
		t.Errorf("the live GITHUB_READ's tier = %s; want human, as the reread found its source", got)
	}
	if ids := described.take(); !slices.Equal(ids, []string{policytest.ID(appSourceName)}) {
		t.Errorf("the reread described %v; want its source alone, %s", ids, policytest.ID(appSourceName))
	}

	store.Delete(policytest.ID(appSourceName))
	if lk, err := cur.RefreshOne(t.Context(), "GITHUB_READ"); err != nil || lk.Served || lk.Reason != policy.ReasonProfileSourceMissing {
		t.Errorf("RefreshOne(GITHUB_READ) once its source is gone = %+v, %v; want it refused %s", lk, err, policy.ReasonProfileSourceMissing)
	}
}

// TestTheFirstPublishWritesTheProfilesLine: the pod's first publish writes one profiles-applied
// line, with the counts of the profiles it served and refused and the digest of the file it read,
// beside a refusal line for each refused profile; no later publish writes it again.
func TestTheFirstPublishWritesTheProfilesLine(t *testing.T) {
	logged := policytest.CaptureLog(t)
	file := policytest.KindsFile(readProfile, `{"name": "GHOST_READ", "source": "NO_SUCH_APP", `+acmeRead+`, `+apiProxy+`}`)
	store := memory.New(everySource()...)
	cur := policytest.ProfiledCurrent(t, store, file, daemonService)
	sum := sha256.Sum256([]byte(file))
	want := "INFO " + policy.ProfilesAppliedMessage + " served=1 refused=1 file_digest=" + hex.EncodeToString(sum[:]) + "\n"
	if got := logged.String(); strings.Count(got, policy.ProfilesAppliedMessage) != 1 || !strings.Contains(got, want) {
		t.Fatalf("the first publish wrote\n%s\nwant one line %q", got, want)
	}
	refused := "ERROR " + policy.RefusedMessage + " name=" + profileKey("GHOST_READ") + " reason=" + policy.ReasonProfileSourceMissing
	if got := logged.String(); !strings.Contains(got, refused) || !strings.Contains(got, " detail=") {
		t.Errorf("the first publish wrote\n%s\nwant a line %q with its detail", got, refused)
	}

	store.Put(policytest.Secret("PLAIN_KEY", appOwner, policy.TierHuman, "plain-v2"))
	if err := cur.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	store.Put(policytest.Secret("PLAIN_KEY", appOwner, policy.TierAgent, "plain-v3"))
	if _, err := cur.RefreshOne(t.Context(), "PLAIN_KEY"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(logged.String(), policy.ProfilesAppliedMessage); got != 1 {
		t.Errorf("the publishes after the first wrote the profiles line %d times in all; want once", got)
	}
}

// TestEveryPublishedSetHoldsTheProfiles drives each way the live policy is published - a reload, a
// reread, a value read that readValues records and a mint's RefuseValue - and after each the live
// set serves GITHUB_READ, or refuses it with its source's reason.
func TestEveryPublishedSetHoldsTheProfiles(t *testing.T) {
	cur, store := profiled(t, everySource(), readProfile)
	// holds checks the live set after step: GITHUB_READ served when reason is "", else refused with
	// a detail naming reason, its source's.
	holds := func(step, reason string) {
		t.Helper()
		set := cur.Get()
		if reason == "" {
			if _, served := set.Secrets["GITHUB_READ"]; !served {
				t.Errorf("after %s GITHUB_READ is not served: %+v", step, set.Refused[profileKey("GITHUB_READ")])
			}
			return
		}
		if refusal := set.Refused[profileKey("GITHUB_READ")]; refusal.Reason != policy.ReasonProfileSourceMissing || !strings.Contains(refusal.Detail, reason) {
			t.Errorf("after %s GITHUB_READ's refusal = %+v; want %s naming %s", step, refusal, policy.ReasonProfileSourceMissing, reason)
		}
	}

	store.Put(policytest.Secret("PLAIN_KEY", appOwner, policy.TierHuman, "plain-v2"))
	if err := cur.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if cur.Get().Secrets["PLAIN_KEY"].Tier != policy.TierHuman {
		t.Fatal("the reload did not publish PLAIN_KEY's new tier")
	}
	holds("a reload", "")

	store.Put(policytest.Secret("PLAIN_KEY", appOwner, policy.TierAgent, "plain-v3"))
	if _, err := cur.RefreshOne(t.Context(), "PLAIN_KEY"); err != nil {
		t.Fatal(err)
	}
	if cur.Get().Secrets["PLAIN_KEY"].Tier != policy.TierAgent {
		t.Fatal("the reread did not publish PLAIN_KEY's new tier")
	}
	holds("a reread", "")

	cur.RefuseValue(t.Context(), "GITHUB_READ", cur.Get().ValueVersion("GITHUB_READ"), policy.TypeGitHubApp, nil)
	if refusal := cur.Get().Refused[policytest.ID(appSourceName)]; refusal.Reason != policy.ReasonValueMalformed {
		t.Fatalf("RefuseValue(GITHUB_READ) left its source's refusal %+v; want %s", refusal, policy.ReasonValueMalformed)
	}
	holds("a mint's RefuseValue", policy.ReasonValueMalformed)

	// A new value of the source, which the reload lists and its value read then decodes: the
	// refusal RefuseValue made ends at the read, which readValues publishes.
	source := appSource()
	source.Value += " "
	store.Put(source)
	if err := cur.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if refusal, refused := cur.Get().Refused[policytest.ID(appSourceName)]; refused {
		t.Fatalf("the source's new value did not end its refusal: %+v", refusal)
	}
	holds("a value read that readValues records", "")
}

// TestARereadNamesEveryNameServedFromTheValueItReads: a reread of a name reads one stored
// secret, the name itself or the source of the profile that holds it (Current.RefreshOne), and a
// new value of that secret is the value of every name served from it: the source's own name and
// each profile of it (Set.ProfilesOf). The airlock's proxy holds a grant's values by the name the
// grant was released under, so RereadNames is every name whose held values a reread must drop. A
// profile's reread names its source and its siblings, a source's names its profiles, and a name
// no profile is of names itself alone, as does a name the policy has never heard of.
func TestARereadNamesEveryNameServedFromTheValueItReads(t *testing.T) {
	store := memory.New(appSource(),
		policytest.Secret("STORED_TOKEN", appOwner, policy.TierAgent, "stored-v1"),
		policytest.Secret("PLAIN_KEY", appOwner, policy.TierAgent, "plain-v1"))
	cur := policytest.ProfiledCurrent(t, store, policytest.KindsFile(
		`{"name": "STORED_TOKEN_READ", "source": "STORED_TOKEN", `+apiProxy+`}`,
		`{"name": "STORED_TOKEN_SECOND", "source": "STORED_TOKEN", "proxy": "github"}`,
		`{"name": "`+appSourceName+`", `+acmeRead+`}`,
		readProfile,
	), daemonService)
	stored := []string{"STORED_TOKEN", "STORED_TOKEN_READ", "STORED_TOKEN_SECOND"}
	app := []string{"GITHUB_READ", appSourceName}
	for _, row := range []struct {
		name string
		want []string
	}{
		{"STORED_TOKEN", stored},
		{"STORED_TOKEN_READ", stored},
		{"STORED_TOKEN_SECOND", stored},
		{appSourceName, app},
		{"GITHUB_READ", app},
		{"PLAIN_KEY", []string{"PLAIN_KEY"}},
		{"NO_SUCH_KEY", []string{"NO_SUCH_KEY"}},
	} {
		if got := slices.Sorted(slices.Values(cur.RereadNames(row.name))); !slices.Equal(got, row.want) {
			t.Errorf("RereadNames(%s) = %v; want %v", row.name, got, row.want)
		}
	}
}

// TestARereadMovesTheNamesOfAMintedSourceOnlyWhenItMovesItsValueVersion: RefreshOne's Moved is
// the names a reread of a name gave a new value, read against the live set as it stood when the
// reread began. For a type source each served name carries its value version (Set.ValueVersion),
// so a reread of the source or of a profile of it that finds the version the policy held moves
// nothing, and one that finds a new version moves every name RereadNames gives: the source and
// each profile of it. A stored value has no version the policy lists, so its reread is the only
// signal of a new value, and every reread of it moves every name served from it.
func TestARereadMovesTheNamesOfAMintedSourceOnlyWhenItMovesItsValueVersion(t *testing.T) {
	app := []string{"GITHUB_READ", appSourceName}
	stored := []string{"STORED_TOKEN", "STORED_TOKEN_READ", "STORED_TOKEN_SECOND"}
	for _, row := range []struct {
		name string
		// reread is the name reread.
		reread string
		// newValue is whether the store holds a new value of the App before the reread.
		newValue bool
		want     []string
	}{
		{"a minted source whose version did not move", appSourceName, false, nil},
		{"a profile of a minted source whose version did not move", "GITHUB_READ", false, nil},
		{"a minted source whose version moved", appSourceName, true, app},
		{"a profile of a minted source whose version moved", "GITHUB_READ", true, app},
		{"a stored value, which lists no version", "STORED_TOKEN", false, stored},
		{"a profile of a stored value, which lists no version", "STORED_TOKEN_READ", false, stored},
	} {
		t.Run(row.name, func(t *testing.T) {
			store := memory.New(appSource(), policytest.Secret("STORED_TOKEN", appOwner, policy.TierAgent, "stored-v1"))
			cur := policytest.ProfiledCurrent(t, store, policytest.KindsFile(
				`{"name": "STORED_TOKEN_READ", "source": "STORED_TOKEN", `+apiProxy+`}`,
				`{"name": "STORED_TOKEN_SECOND", "source": "STORED_TOKEN", "proxy": "github"}`,
				`{"name": "`+appSourceName+`", `+acmeRead+`}`,
				readProfile,
			), daemonService)
			if row.newValue {
				// A new version of the App's value: a trailing space, which the decoder still reads.
				store.Put(policytest.TypeSecret(appSourceName, appOwner, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)+" "))
			}
			lk, err := cur.RefreshOne(t.Context(), row.reread)
			if err != nil {
				t.Fatalf("RefreshOne(%s): %v", row.reread, err)
			}
			var got []string
			if lk.Moved != nil {
				got = slices.Sorted(slices.Values(lk.Moved))
			}
			if !slices.Equal(got, row.want) {
				t.Errorf("RefreshOne(%s).Moved = %v; want %v", row.reread, got, row.want)
			}
		})
	}
}
