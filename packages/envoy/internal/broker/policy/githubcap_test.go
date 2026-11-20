package policy_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// cappedApp is a github-app secret of a placeholder App whose tokens are capped to one repository
// of acme-org, proofs, and checks: write, owned by the legion-daemon service and asked for by the
// OSSV project's pods of that service alone (the shape dispatch://INFRA-3885 needs). The tag keys
// are written out rather than read from the package's constants, so this fixture builds against
// a broker from before the cap and its test there is the rollback's own reading.
func cappedApp() secrets.LocalSecret {
	s := policytest.TypeSecret("CAPPED_APP", "legion-daemon", policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
	s.Tags["github-owners"] = "acme-org"
	s.Tags["github-repositories"] = "proofs"
	s.Tags["github-permissions"] = "checks:write"
	s.Tags["requesters"] = "legion-daemon/OSSV"
	return s
}

// TestACappedSecretListingAProjectsPodsIsServed: CAPPED_APP loads and is served. It uses no API
// a broker from before project requester entries lacks, so it runs at that broker too, and there it
// fails: that broker reads legion-daemon/OSSV as a service no deployment registers and refuses the
// whole secret as requesters-malformed, which is what makes a rollback serve it to nobody rather
// than to every pod of legion-daemon.
func TestACappedSecretListingAProjectsPodsIsServed(t *testing.T) {
	set := policytest.Current(t, secrets.NewLocal(cappedApp()), "legion-daemon").Get()
	if refusal, refused := set.Refused[policytest.ID("CAPPED_APP")]; refused {
		t.Fatalf("CAPPED_APP refused %s, want served", refusal.Reason)
	}
	if _, served := set.Secrets["CAPPED_APP"]; !served {
		t.Fatalf("CAPPED_APP is not served (served: %v)", set.Secrets)
	}
}

// TestALegacyKindBesideTypeIsIgnored: a capped secret written with `kind: github-app-capped` beside
// `type: github-app`, so that a broker reading only `kind` refuses it as type-unknown, is served
// by this broker as the github-app secret its `type` names, with its cap.
func TestALegacyKindBesideTypeIsIgnored(t *testing.T) {
	s := cappedApp()
	s.Tags[policy.LegacyTypeTag] = "github-app-capped"
	set := policytest.Current(t, secrets.NewLocal(s), "legion-daemon").Get()
	served, ok := set.Secrets["CAPPED_APP"]
	if !ok || served.Type() != policy.TypeGitHubApp || served.GitHubPermissions != "checks:write" {
		t.Fatalf("CAPPED_APP = %+v (served %t, refused %v); want the capped github-app secret", served, ok, set.Refused)
	}
	s.Tags = map[string]string{policy.TagOwner: "legion-daemon", policy.TagTier: policy.TierAgent, policy.LegacyTypeTag: "github-app-capped"}
	if refusal := policytest.Current(t, secrets.NewLocal(s), "legion-daemon").Get().Refused[policytest.ID("CAPPED_APP")]; refusal.Reason != policy.ReasonTypeUnknown {
		t.Fatalf("the same secret with no type tag refused %q, want %s: the control that the kind is read", refusal.Reason, policy.ReasonTypeUnknown)
	}
}

// TestAProjectRequesterEntryServesThatProjectsPodsAlone: legion-daemon/OSSV names the pods of the
// legion-daemon service whose project is OSSV and nobody else: an INFRA pod of the same service, a
// pod of it with no project, an OSSV pod of another service, a person's session and a requester
// naming the project with no service are each denied requester-not-listed, before owner and tier
// are read. The secret's cap reaches the minter as one repository and one permission.
func TestAProjectRequesterEntryServesThatProjectsPodsAlone(t *testing.T) {
	set := policytest.Current(t, secrets.NewLocal(cappedApp()), "legion-daemon", "other-service").Get()
	for _, tc := range []struct {
		name string
		r    policy.Requester
		want string
	}{
		{"OSSV's pod", policy.Requester{Service: "legion-daemon", Project: "OSSV"}, policy.Automatic},
		{"INFRA's pod", policy.Requester{Service: "legion-daemon", Project: "INFRA"}, policy.Deny},
		{"a pod with no project", policy.Requester{Service: "legion-daemon"}, policy.Deny},
		{"an OSSV pod of another service", policy.Requester{Service: "other-service", Project: "OSSV"}, policy.Deny},
		{"a person's session", policy.Requester{Operator: "ada@example.com"}, policy.Deny},
		{"a project and no service", policy.Requester{Project: "OSSV"}, policy.Deny},
		{"a lowercase project", policy.Requester{Service: "legion-daemon", Project: "ossv"}, policy.Deny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := set.Evaluate("CAPPED_APP", tc.r)
			if err != nil || d.Outcome != tc.want {
				t.Fatalf("Evaluate = %+v, %v; want %s", d, err, tc.want)
			}
			if tc.want == policy.Deny && d.Reason != policy.ReasonRequesterNotListed {
				t.Fatalf("Evaluate reason = %q, want %q", d.Reason, policy.ReasonRequesterNotListed)
			}
		})
	}
	want := secrets.GitHubCap{Repositories: []string{"proofs"}, Permissions: map[string]string{"checks": "write"}}
	if got := set.Secrets["CAPPED_APP"].GitHubCap(); !reflect.DeepEqual(got, want) {
		t.Fatalf("GitHubCap = %+v, want %+v", got, want)
	}
}

// TestTheLoaderRefusesAMalformedOrMisplacedCap: each malformed cap tag, a cap on a secret that is
// no github-app one, a repository cap under more than one owner, and a project requester entry
// that names no project key or an unregistered service, is refused by name, so no cap the broker
// cannot apply is ever served as though it held.
func TestTheLoaderRefusesAMalformedOrMisplacedCap(t *testing.T) {
	edit := func(name string, change func(*secrets.LocalSecret)) secrets.LocalSecret {
		s := cappedApp()
		s.Name = policytest.ID(name)
		change(&s)
		return s
	}
	cases := map[string]struct {
		secret secrets.LocalSecret
		reason string
	}{
		"REPOS_COMMA":   {edit("REPOS_COMMA", func(s *secrets.LocalSecret) { s.Tags[policy.TagGitHubRepositories] = "a,b" }), policy.ReasonGitHubRepositoriesMalformed},
		"REPOS_OWNER":   {edit("REPOS_OWNER", func(s *secrets.LocalSecret) { s.Tags[policy.TagGitHubRepositories] = "acme-org/proofs" }), policy.ReasonGitHubRepositoriesMalformed},
		"REPOS_EMPTY":   {edit("REPOS_EMPTY", func(s *secrets.LocalSecret) { s.Tags[policy.TagGitHubRepositories] = " " }), policy.ReasonGitHubRepositoriesMalformed},
		"REPOS_TWICE":   {edit("REPOS_TWICE", func(s *secrets.LocalSecret) { s.Tags[policy.TagGitHubRepositories] = "a A" }), policy.ReasonGitHubRepositoriesMalformed},
		"PERMS_LEVEL":   {edit("PERMS_LEVEL", func(s *secrets.LocalSecret) { s.Tags[policy.TagGitHubPermissions] = "checks:admin" }), policy.ReasonGitHubPermissionsMalformed},
		"PERMS_SHAPE":   {edit("PERMS_SHAPE", func(s *secrets.LocalSecret) { s.Tags[policy.TagGitHubPermissions] = "checks" }), policy.ReasonGitHubPermissionsMalformed},
		"PERMS_EMPTY":   {edit("PERMS_EMPTY", func(s *secrets.LocalSecret) { s.Tags[policy.TagGitHubPermissions] = "" }), policy.ReasonGitHubPermissionsMalformed},
		"PERMS_TWICE":   {edit("PERMS_TWICE", func(s *secrets.LocalSecret) { s.Tags[policy.TagGitHubPermissions] = "checks:read checks:write" }), policy.ReasonGitHubPermissionsMalformed},
		"PERMS_UPPER":   {edit("PERMS_UPPER", func(s *secrets.LocalSecret) { s.Tags[policy.TagGitHubPermissions] = "Checks:write" }), policy.ReasonGitHubPermissionsMalformed},
		"CAP_ON_STORED": {edit("CAP_ON_STORED", func(s *secrets.LocalSecret) { delete(s.Tags, policy.TagType); delete(s.Tags, policy.TagGitHubOwners) }), policy.ReasonGitHubCapType},
		"TWO_OWNERS":    {edit("TWO_OWNERS", func(s *secrets.LocalSecret) { s.Tags[policy.TagGitHubOwners] = "acme-org ada" }), policy.ReasonGitHubCapOwners},
		"PERMS_TWO_OWNERS": {edit("PERMS_TWO_OWNERS", func(s *secrets.LocalSecret) {
			delete(s.Tags, policy.TagGitHubRepositories)
			s.Tags[policy.TagGitHubOwners] = "acme-org ada"
		}), policy.ReasonGitHubCapOwners},
		"PROJECT_LOWER":   {edit("PROJECT_LOWER", func(s *secrets.LocalSecret) { s.Tags[policy.TagRequesters] = "legion-daemon/ossv" }), policy.ReasonRequestersMalformed},
		"PROJECT_EMPTY":   {edit("PROJECT_EMPTY", func(s *secrets.LocalSecret) { s.Tags[policy.TagRequesters] = "legion-daemon/" }), policy.ReasonRequestersMalformed},
		"PROJECT_UNKNOWN": {edit("PROJECT_UNKNOWN", func(s *secrets.LocalSecret) { s.Tags[policy.TagRequesters] = "unregistered/OSSV" }), policy.ReasonRequestersMalformed},
		"PROJECT_SLASHES": {edit("PROJECT_SLASHES", func(s *secrets.LocalSecret) { s.Tags[policy.TagRequesters] = "legion-daemon/OSSV/X" }), policy.ReasonRequestersMalformed},
	}
	store := secrets.NewLocal()
	for _, c := range cases {
		store.Put(c.secret)
	}
	set := policytest.Current(t, store, "legion-daemon").Get()
	for name, c := range cases {
		refusal, refused := set.Refused[policytest.ID(name)]
		if !refused || refusal.Reason != c.reason {
			t.Errorf("%s refused %+v (refused %t), want %s", name, refusal, refused, c.reason)
		}
	}
	if len(set.Secrets) != 0 {
		t.Errorf("served %v, want nothing", set.Secrets)
	}
}

// TestACapIsInThePolicyVersion: adding or changing a cap moves Version, so a live grant of the
// secret is checked again against the new cap; a secret with neither tag keeps the digest bytes it
// wrote before the cap existed (the golden digest tests).
func TestACapIsInThePolicyVersion(t *testing.T) {
	versionOf := func(change func(*secrets.LocalSecret)) string {
		s := cappedApp()
		change(&s)
		return policytest.Current(t, secrets.NewLocal(s), "legion-daemon").Get().Version
	}
	capped := versionOf(func(*secrets.LocalSecret) {})
	uncapped := versionOf(func(s *secrets.LocalSecret) {
		delete(s.Tags, policy.TagGitHubRepositories)
		delete(s.Tags, policy.TagGitHubPermissions)
	})
	wider := versionOf(func(s *secrets.LocalSecret) { s.Tags[policy.TagGitHubPermissions] = "checks:write contents:write" })
	if capped == uncapped || capped == wider || uncapped == wider {
		t.Fatalf("versions capped %s, uncapped %s, wider %s; want three different", capped, uncapped, wider)
	}
}

// TestTheCapParsersAnswerOneCanonicalForm: two spellings of one cap are one policy, and each
// refusal is its own named error.
func TestTheCapParsersAnswerOneCanonicalForm(t *testing.T) {
	if got, err := policy.ParseGitHubRepositories("  widgets\tProofs "); err != nil || got != "Proofs widgets" {
		t.Fatalf("ParseGitHubRepositories = %q, %v; want %q", got, err, "Proofs widgets")
	}
	if got, err := policy.ParseGitHubPermissions("pull_requests:read checks:write"); err != nil || got != "checks:write pull_requests:read" {
		t.Fatalf("ParseGitHubPermissions = %q, %v", got, err)
	}
	if got := policy.GitHubPermissionsMap("checks:write pull_requests:read"); !reflect.DeepEqual(got, map[string]string{"checks": "write", "pull_requests": "read"}) {
		t.Fatalf("GitHubPermissionsMap = %v", got)
	}
	for raw, want := range map[string]error{
		"":      policy.ErrGitHubRepositoriesMissing,
		"a,b":   policy.ErrGitHubRepositoriesComma,
		"..":    policy.ErrGitHubRepositoriesEntry,
		"o/r":   policy.ErrGitHubRepositoriesEntry,
		"a b A": policy.ErrGitHubRepositoriesDuplicate,
	} {
		if _, err := policy.ParseGitHubRepositories(raw); !errors.Is(err, want) {
			t.Errorf("ParseGitHubRepositories(%q) = %v, want %v", raw, err, want)
		}
	}
	for raw, want := range map[string]error{
		"":                        policy.ErrGitHubPermissionsMissing,
		"checks:write,a:read":     policy.ErrGitHubPermissionsComma,
		"checks:none":             policy.ErrGitHubPermissionsEntry,
		"checks":                  policy.ErrGitHubPermissionsEntry,
		"_checks:read":            policy.ErrGitHubPermissionsEntry,
		"checks:read checks:read": policy.ErrGitHubPermissionsDuplicate,
	} {
		if _, err := policy.ParseGitHubPermissions(raw); !errors.Is(err, want) {
			t.Errorf("ParseGitHubPermissions(%q) = %v, want %v", raw, err, want)
		}
	}
}
