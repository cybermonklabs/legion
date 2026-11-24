package policy_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// The people and projects of the project tag's tests. carol owns PRIV and bob is on its access
// list; alice is neither. PUB is a public project, and GONE a key Dispatch holds no project for.
const (
	carol       = "carol@example.com"
	bob         = "bob@example.com"
	alice       = "alice@example.com"
	privProject = "PRIV"
	pubProject  = "PUB"
	goneProject = "GONE"
	projectSvc  = "priv-daemon"
)

// projectSecret is a namespace secret tagged with project, owned by owner at tier.
func projectSecret(name, owner, tier, project string) memory.Secret {
	s := policytest.Secret(name, owner, tier, "v")
	s.Tags[policy.TagProject] = project
	return s
}

// projectLoader is the Loader of a broker that holds a K3 client, so it serves a tagged secret.
func projectLoader(store secretstore.Store) policy.Loader {
	loader := policytest.Loader(store, projectSvc)
	loader.ProjectChecks = true
	return loader
}

// loadProjects loads store through projectLoader, failing the test on an error.
func loadProjects(t *testing.T, store *memory.Store) *policy.Set {
	t.Helper()
	set, err := projectLoader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return set
}

// states is the plain data a caller resolves before it evaluates: PRIV private, PUB public and
// GONE a project Dispatch does not hold.
var states = map[string]policy.ProjectState{
	privProject: policy.ProjectPrivate,
	pubProject:  policy.ProjectPublic,
	goneProject: policy.ProjectMissing,
}

// credential is a requester whose credential names projects, with the states its caller resolved.
// Named and Projects are the same here: every project these rows name is one its person may see,
// which is the ordinary case. The two differ only once a person loses a project, which
// TestListsPrivateReadsTheCredentialsOwnProjects is about.
func credential(operator, service string, projects ...string) policy.Requester {
	return policy.Requester{Operator: operator, Service: service, Named: projects, Projects: projects, ProjectStates: states}
}

// TestAPrivateProjectSecretGoesOnlyToACredentialThatNamesIt is R11 in the policy: a secret tagged
// with a private project is released to a credential that names that project, and refused,
// project-not-named, to one naming another project, to one naming none, and to the dev-box
// session of a person on the project's access list whose credential does not name it. Who the
// person is decides nothing here: naming the project is what the rule reads. A pod of the
// project's service whose credential names it gets the secret too.
func TestAPrivateProjectSecretGoesOnlyToACredentialThatNamesIt(t *testing.T) {
	set := loadProjects(t, memory.New(projectSecret("PRIV_KEY", policy.OwnerShared, policy.TierAgent, privProject)))
	source := policytest.Source("PRIV_KEY")
	released := policy.Decision{Outcome: policy.Automatic, Source: source}
	refused := policy.Decision{Outcome: policy.Deny, Reason: policy.ReasonProjectNotNamed, Source: source}
	for _, c := range []struct {
		credential string
		requester  policy.Requester
		want       policy.Decision
	}{
		{"carol's session naming PRIV", credential(carol, "", privProject), released},
		{"bob's session naming PUB and PRIV", credential(bob, "", privProject, pubProject), released},
		{"the PRIV daemon's pod naming PRIV", credential("", projectSvc, privProject), released},
		{"bob's session naming another project, PUB", credential(bob, "", pubProject), refused},
		{"alice's session naming no project", credential(alice, ""), refused},
		{"carol's dev-box session, carol a member of PRIV, naming no project", credential(carol, ""), refused},
		{"a pod naming no project", credential("", projectSvc), refused},
	} {
		got, err := set.Evaluate("PRIV_KEY", c.requester)
		if err != nil {
			t.Errorf("Evaluate(PRIV_KEY) for %s = %v; want %+v", c.credential, err, c.want)
			continue
		}
		if got != c.want {
			if got.Outcome != policy.Deny && c.want.Outcome == policy.Deny {
				t.Errorf("%s got the private project's secret PRIV_KEY (%+v), but its credential does not name %s; want %+v",
					c.credential, got, privProject, c.want)
				continue
			}
			t.Errorf("Evaluate(PRIV_KEY) for %s = %+v; want %+v", c.credential, got, c.want)
		}
	}
}

// TestAPrivateProjectSecretOwnedByAPersonStillAsksItsOwner is the rule's other half: a credential
// that names the project passes the project rule and nothing more. Owner, tier, the requesters
// tag and a withheld name answer after it exactly as they answer for an untagged secret, so a
// person's human-tier secret of PRIV still asks its owner, and its agent-tier one goes at once to
// its owner's sessions alone.
func TestAPrivateProjectSecretOwnedByAPersonStillAsksItsOwner(t *testing.T) {
	listed := projectSecret("PRIV_LISTED", carol, policy.TierHuman, privProject)
	listed.Tags[policy.TagRequesters] = carol
	set := loadProjects(t, memory.New(
		projectSecret("PRIV_HUMAN", carol, policy.TierHuman, privProject),
		projectSecret("PRIV_AGENT", carol, policy.TierAgent, privProject),
		listed,
	))
	approval := policy.Decision{Outcome: policy.Approval, Approver: carol}
	withheld := credential(carol, "", privProject)
	withheld.Withheld = []string{"PRIV_AGENT"}
	for _, c := range []struct {
		name, credential string
		requester        policy.Requester
		want             policy.Decision
	}{
		{"PRIV_HUMAN", "carol's session naming PRIV", credential(carol, "", privProject), approval},
		{"PRIV_HUMAN", "bob's session naming PRIV", credential(bob, "", privProject), approval},
		{"PRIV_AGENT", "carol's session naming PRIV", credential(carol, "", privProject), policy.Decision{Outcome: policy.Automatic}},
		{"PRIV_AGENT", "bob's session naming PRIV", credential(bob, "", privProject), approval},
		{"PRIV_AGENT", "carol's session naming PRIV, PRIV_AGENT withheld", withheld, approval},
		{"PRIV_LISTED", "bob's session naming PRIV", credential(bob, "", privProject), policy.Decision{Outcome: policy.Deny, Reason: policy.ReasonRequesterNotListed}},
		{"PRIV_LISTED", "carol's session naming PRIV", credential(carol, "", privProject), approval},
		{"PRIV_HUMAN", "carol's session naming no project", credential(carol, ""), policy.Decision{Outcome: policy.Deny, Reason: policy.ReasonProjectNotNamed}},
	} {
		got, err := set.Evaluate(c.name, c.requester)
		c.want.Source = policytest.Source(c.name)
		if err != nil || got != c.want {
			t.Errorf("Evaluate(%s) for %s = %+v, %v; want %+v", c.name, c.credential, got, err, c.want)
		}
	}
}

// TestAPrivateProjectSecretOfAPublicProjectFollowsOwnerAndTier is R9 for a tagged secret: a
// secret whose project is public answers every credential, whatever projects it names or does
// not, exactly as owner and tier answer an untagged one.
func TestAPrivateProjectSecretOfAPublicProjectFollowsOwnerAndTier(t *testing.T) {
	set := loadProjects(t, memory.New(
		projectSecret("PUB_SHARED", policy.OwnerShared, policy.TierAgent, pubProject),
		projectSecret("PUB_PERSON", carol, policy.TierAgent, pubProject),
		projectSecret("PUB_HUMAN", policy.OwnerShared, policy.TierHuman, pubProject),
		projectSecret("PUB_SERVICE", projectSvc, policy.TierAgent, pubProject),
	))
	automatic := policy.Decision{Outcome: policy.Automatic}
	for _, c := range []struct {
		name, credential string
		requester        policy.Requester
		want             policy.Decision
	}{
		{"PUB_SHARED", "alice's session naming no project", credential(alice, ""), automatic},
		{"PUB_SHARED", "bob's session naming PRIV", credential(bob, "", privProject), automatic},
		{"PUB_SHARED", "a pod naming no project", credential("", ""), automatic},
		{"PUB_PERSON", "carol's session naming no project", credential(carol, ""), automatic},
		{"PUB_PERSON", "alice's session naming PUB", credential(alice, "", pubProject), policy.Decision{Outcome: policy.Approval, Approver: carol}},
		{"PUB_HUMAN", "alice's session naming no project", credential(alice, ""), policy.Decision{Outcome: policy.Approval, Approver: record.AnyoneApprover}},
		{"PUB_SERVICE", "the service's pod naming no project", credential("", projectSvc), automatic},
		{"PUB_SERVICE", "alice's session naming PUB", credential(alice, "", pubProject), policy.Decision{Outcome: policy.Deny}},
	} {
		got, err := set.Evaluate(c.name, c.requester)
		c.want.Source = policytest.Source(c.name)
		if err != nil || got != c.want {
			t.Errorf("Evaluate(%s) for %s = %+v, %v; want %+v", c.name, c.credential, got, err, c.want)
		}
	}
}

// TestAPrivateProjectSecretTaggedWithAMissingProjectGoesToNobody is the tag that names a project
// Dispatch does not hold — a typo, a renamed key: it fails closed, denied project-missing to
// every credential, the one naming that key included. The caller is told the same reason as for
// an unnamed private project, so a refusal never says whether a project exists; the broker's own
// log keeps project-missing apart.
func TestAPrivateProjectSecretTaggedWithAMissingProjectGoesToNobody(t *testing.T) {
	set := loadProjects(t, memory.New(projectSecret("GONE_KEY", policy.OwnerShared, policy.TierAgent, goneProject)))
	want := policy.Decision{Outcome: policy.Deny, Reason: policy.ReasonProjectMissing, Source: policytest.Source("GONE_KEY")}
	for who, r := range map[string]policy.Requester{
		"carol's session naming GONE":   credential(carol, "", goneProject),
		"carol's session naming none":   credential(carol, ""),
		"a pod of the service":          credential("", projectSvc, goneProject),
		"bob's session naming PRIV":     credential(bob, "", privProject),
		"alice's session naming PUB":    credential(alice, "", pubProject),
		"carol's session naming both":   credential(carol, "", goneProject, privProject),
		"the service's pod naming GONE": credential("", projectSvc, goneProject),
	} {
		if got, err := set.Evaluate("GONE_KEY", r); err != nil || got != want {
			t.Errorf("Evaluate(GONE_KEY) for %s = %+v, %v; want %+v: a tag naming no project goes to nobody", who, got, err, want)
		}
	}
	if got, want := policy.CallerReason(policy.ReasonProjectMissing), policy.CallerReason(policy.ReasonProjectNotNamed); got != want {
		t.Errorf("a caller is told %q for a missing project and %q for an unnamed private one; want one text, so the refusal does not say whether the project exists", got, want)
	}
	if got := policy.CallerReason(policy.ReasonProjectNotNamed); got != policy.ReasonProjectNotNamed {
		t.Errorf("CallerReason(%s) = %q; want it unchanged", policy.ReasonProjectNotNamed, got)
	}
	if got := policy.CallerReason(policy.ReasonRequesterNotListed); got != policy.ReasonRequesterNotListed {
		t.Errorf("CallerReason(%s) = %q; want every other reason unchanged", policy.ReasonRequesterNotListed, got)
	}
}

// TestAPrivateProjectSecretWithNoStateIsAnError is the rule's own guard: Evaluate does no I/O,
// so a caller that evaluates a tagged secret without having resolved its project's state gets
// ErrNoProjectState rather than a guess — neither a release nor a denial it did not decide.
func TestAPrivateProjectSecretWithNoStateIsAnError(t *testing.T) {
	set := loadProjects(t, memory.New(
		projectSecret("PRIV_KEY", policy.OwnerShared, policy.TierAgent, privProject),
		policytest.Secret("PLAIN_KEY", policy.OwnerShared, policy.TierAgent, "v"),
	))
	for who, r := range map[string]policy.Requester{
		"no states at all":       {Operator: carol, Projects: []string{privProject}},
		"states for another key": {Operator: carol, Projects: []string{privProject}, ProjectStates: map[string]policy.ProjectState{pubProject: policy.ProjectPublic}},
		"the state unknown":      {Operator: carol, Projects: []string{privProject}, ProjectStates: map[string]policy.ProjectState{privProject: policy.ProjectStateUnknown}},
	} {
		if got, err := set.Evaluate("PRIV_KEY", r); !errors.Is(err, policy.ErrNoProjectState) {
			t.Errorf("Evaluate(PRIV_KEY) with %s = %+v, %v; want ErrNoProjectState", who, got, err)
		}
	}
	// An untagged secret reads no state, so a caller that resolved none is answered as today.
	if got, err := set.Evaluate("PLAIN_KEY", policy.Requester{Operator: carol}); err != nil || got.Outcome != policy.Automatic {
		t.Errorf("Evaluate(PLAIN_KEY) with no states = %+v, %v; want automatic", got, err)
	}
}

// TestAPrivateProjectSecretTagMovesTheVersionOnlyWhenPresent pins the digest's bytes: a secret
// with no project tag writes exactly the line it wrote before the tag existed, in each of
// NewSet's three forms, so no deployed version moves; a tagged one ends its line with
// "\tproject=<KEY>" after the admin field, and so moves the version, which ends the grants the
// policy decided before the tag.
func TestAPrivateProjectSecretTagMovesTheVersionOnlyWhenPresent(t *testing.T) {
	const arn = "arn:aws:secretsmanager:local:000000000000:secret:example/agent-secrets/"
	digest := func(lines string) string {
		sum := sha256.Sum256([]byte(lines))
		return hex.EncodeToString(sum[:])
	}
	for name, c := range map[string]struct {
		secret         policy.Secret
		untagged, line string
	}{
		"a plain secret": {
			secret:   policy.Secret{Name: "A_KEY", Owner: carol, Tier: policy.TierAgent, Source: arn + "a-key"},
			untagged: "A_KEY\tcarol@example.com\tagent\t" + arn + "a-key\n",
			line:     "A_KEY\tcarol@example.com\tagent\t" + arn + "a-key\t0s\t\tproject=PRIV\n",
		},
		"a secret with a max-grant": {
			secret:   policy.Secret{Name: "A_KEY", Owner: carol, Tier: policy.TierAgent, Source: arn + "a-key", MaxGrant: time.Hour},
			untagged: "A_KEY\tcarol@example.com\tagent\t" + arn + "a-key\t1h0m0s\t\n",
			line:     "A_KEY\tcarol@example.com\tagent\t" + arn + "a-key\t1h0m0s\t\tproject=PRIV\n",
		},
		// The cases with optional fields before the project field, one in each of NewSet's two
		// forms a tagged secret takes: requesters, scopes and admin are written first, and
		// project after them, so a digest that moved project= ahead of any of them would move
		// every such secret's version.
		"a secret with requesters, scopes, admin and a max-grant": {
			secret:   policy.Secret{Name: "A_KEY", Owner: carol, Tier: policy.TierHuman, Source: arn + "a-key", Requesters: carol, Scopes: "cloud-platform", Admin: true, MaxGrant: time.Hour},
			untagged: "A_KEY\tcarol@example.com\thuman\t" + arn + "a-key\t1h0m0s\t\trequesters=carol@example.com\tscopes=cloud-platform\tadmin=true\n",
			line:     "A_KEY\tcarol@example.com\thuman\t" + arn + "a-key\t1h0m0s\t\trequesters=carol@example.com\tscopes=cloud-platform\tadmin=true\tproject=PRIV\n",
		},
		"a session-default secret with requesters, scopes, admin and a max-grant": {
			secret:   policy.Secret{Name: "A_KEY", Owner: carol, Tier: policy.TierHuman, Source: arn + "a-key", SessionDefault: true, Requesters: carol, Scopes: "cloud-platform", Admin: true, MaxGrant: time.Hour},
			untagged: "A_KEY\tcarol@example.com\thuman\t" + arn + "a-key\t1h0m0s\t\tdefault-grant=session\trequesters=carol@example.com\tscopes=cloud-platform\tadmin=true\n",
			line:     "A_KEY\tcarol@example.com\thuman\t" + arn + "a-key\t1h0m0s\t\tdefault-grant=session\trequesters=carol@example.com\tscopes=cloud-platform\tadmin=true\tproject=PRIV\n",
		},
		"a session-default secret": {
			secret:   policy.Secret{Name: "A_KEY", Owner: carol, Tier: policy.TierAgent, Source: arn + "a-key", SessionDefault: true},
			untagged: "A_KEY\tcarol@example.com\tagent\t" + arn + "a-key\t0s\t\tdefault-grant=session\n",
			line:     "A_KEY\tcarol@example.com\tagent\t" + arn + "a-key\t0s\t\tdefault-grant=session\tproject=PRIV\n",
		},
	} {
		untagged := policy.NewSet(map[string]policy.Secret{c.secret.Name: c.secret})
		if want := digest(c.untagged); untagged.Version != want {
			t.Errorf("%s with no project tag: Version = %s, want %s, the digest of the line it wrote before the tag existed", name, untagged.Version, want)
		}
		tagged := c.secret
		tagged.Project = privProject
		set := policy.NewSet(map[string]policy.Secret{tagged.Name: tagged})
		if want := digest(c.line); set.Version != want {
			t.Errorf("%s tagged PRIV: Version = %s, want %s, the digest of %q", name, set.Version, want, c.line)
		}
		if set.Version == untagged.Version {
			t.Errorf("%s: tagging it PRIV left the version at %s", name, set.Version)
		}
	}
	// The golden namespace loads to the same version whether or not the broker checks projects.
	checked, err := projectLoader(arnSourced{goldenFixture()}).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const golden = "252a3be479774f1163b99a7fbeeec17d523eb5e7048d54f90b8cb5e702fc16ca"
	if checked.Version != golden {
		t.Errorf("the golden namespace under a broker that checks projects has version %s, want %s, TestGoldenDigest's", checked.Version, golden)
	}
}

// TestAPrivateProjectSecretTagIsLoaded is the loader's half: a tag naming a project key is read
// as written into Secret.Project, and the loader refuses, by name, a tag that is no project key
// (project-malformed, the word none and an empty tag among them, since retag --project none
// removes the tag rather than writing it), and any tagged secret on a broker that holds no K3
// client (project-unchecked), which could not tell whether the project is private and would
// otherwise release it to every session.
func TestAPrivateProjectSecretTagIsLoaded(t *testing.T) {
	store := memory.New(
		projectSecret("PRIV_KEY", policy.OwnerShared, policy.TierAgent, privProject),
		projectSecret("LOWER_KEY", policy.OwnerShared, policy.TierAgent, "priv"),
		projectSecret("NONE_KEY", policy.OwnerShared, policy.TierAgent, "none"),
		projectSecret("EMPTY_KEY", policy.OwnerShared, policy.TierAgent, ""),
		projectSecret("LONG_KEY", policy.OwnerShared, policy.TierAgent, "ABCDEFGHIJK"),
		projectSecret("SPACED_KEY", policy.OwnerShared, policy.TierAgent, " PRIV"),
		policytest.Secret("PLAIN_KEY", policy.OwnerShared, policy.TierAgent, "v"),
	)
	set := loadProjects(t, store)
	if got := set.Secrets["PRIV_KEY"].Project; got != privProject {
		t.Errorf("PRIV_KEY's Project = %q, want %q", got, privProject)
	}
	if got := set.Secrets["PLAIN_KEY"].Project; got != "" {
		t.Errorf("PLAIN_KEY's Project = %q, want none", got)
	}
	for _, name := range []string{"LOWER_KEY", "NONE_KEY", "EMPTY_KEY", "LONG_KEY", "SPACED_KEY"} {
		want := policy.Refusal{Reason: policy.ReasonProjectMalformed, Owner: policy.OwnerShared}
		if got := set.Refused[policytest.ID(name)]; got != want {
			t.Errorf("%s refused as %+v, want %+v", name, got, want)
		}
	}
	unchecked, err := policytest.Loader(store, projectSvc).Load(context.Background())
	if err != nil {
		t.Fatalf("Load with no K3 client: %v", err)
	}
	if got, want := unchecked.Refused[policytest.ID("PRIV_KEY")], (policy.Refusal{Reason: policy.ReasonProjectUnchecked, Owner: policy.OwnerShared}); got != want {
		t.Errorf("PRIV_KEY on a broker with no K3 client refused as %+v, want %+v", got, want)
	}
	if _, served := unchecked.Secrets["PLAIN_KEY"]; !served {
		t.Error("PLAIN_KEY is not served by a broker with no K3 client; an untagged secret is served as today")
	}
	lookup, err := projectLoader(store).LoadOne(context.Background(), "PRIV_KEY")
	if err != nil || !lookup.Served || lookup.Secret.Project != privProject {
		t.Errorf("LoadOne(PRIV_KEY) = %+v, %v; want it served with Project %s", lookup, err, privProject)
	}
	for raw, want := range map[string]string{"PRIV": "PRIV", "AB": "AB", "A123456789": "A123456789"} {
		if got, err := policy.ParseProject(raw); err != nil || got != want {
			t.Errorf("ParseProject(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "none", "NONE ", "priv", "P", "1PRIV", "PRIV-1", "A1234567890", "PRIV PUB"} {
		if got, err := policy.ParseProject(raw); !errors.Is(err, policy.ErrProjectMalformed) {
			t.Errorf("ParseProject(%q) = %q, %v; want ErrProjectMalformed", raw, got, err)
		}
	}
}

// TestAMisspelledProjectTagKeyRefusesTheSecret: a tag key that differs from dispatch-project or
// project-channels only in case or in its separators is refused, project-tag-misspelled, by a
// broker that checks projects and by one that does not, through Load and LoadOne alike. Loaded as
// an untagged secret it would go by owner and tier to every session, which is what the owner who
// wrote it meant to stop. A key that is another word, the cost-allocation tag "project" among
// them, is not read at all, as before.
func TestAMisspelledProjectTagKeyRefusesTheSecret(t *testing.T) {
	misspelled := map[string]string{
		"CAPS_KEY":        "Dispatch-Project",
		"UNDERSCORE_KEY":  "dispatch_project",
		"UPPER_KEY":       "DISPATCH-PROJECT",
		"JOINED_KEY":      "dispatchproject",
		"SPACED_KEY":      "dispatch project",
		"CHANNELS_KEY":    "PROJECT-CHANNELS",
		"CHANNELS_US_KEY": "project_channels",
		"CHANNELS_DOT":    "Project.Channels",
	}
	stored := []memory.Secret{}
	for name, key := range misspelled {
		s := policytest.Secret(name, policy.OwnerShared, policy.TierAgent, "v")
		s.Tags[key] = privProject
		stored = append(stored, s)
	}
	other := policytest.Secret("COST_KEY", policy.OwnerShared, policy.TierAgent, "v")
	other.Tags["project"] = "billing"
	other.Tags["dispatch-projects"] = privProject
	stored = append(stored, other)
	store := memory.New(stored...)
	for _, checks := range []bool{true, false} {
		loader := policytest.Loader(store, projectSvc)
		loader.ProjectChecks = checks
		set, err := loader.Load(context.Background())
		if err != nil {
			t.Fatalf("Load (project checks %v): %v", checks, err)
		}
		want := policy.Refusal{Reason: policy.ReasonProjectTagMisspelled, Owner: policy.OwnerShared}
		for name, key := range misspelled {
			if got := set.Refused[policytest.ID(name)]; got != want {
				t.Errorf("project checks %v: %s, tagged %q, refused as %+v, want %+v", checks, name, key, got, want)
			}
			if lookup, err := loader.LoadOne(context.Background(), name); err != nil || lookup.Served || lookup.Reason != policy.ReasonProjectTagMisspelled {
				t.Errorf("project checks %v: LoadOne(%s), tagged %q = %+v, %v; want refused %s", checks, name, key, lookup, err, policy.ReasonProjectTagMisspelled)
			}
		}
		if _, served := set.Secrets["COST_KEY"]; !served {
			t.Errorf("project checks %v: COST_KEY, tagged project and dispatch-projects, is not served (refused: %+v); only a spelling of the two project keys is refused", checks, set.Refused[policytest.ID("COST_KEY")])
		}
	}
}

// TestTheProjectRuleIsOneFunction: policy.ProjectRule is the one statement of the dispatch-project
// rule, which Evaluate and the airlock's connection check (requests' airlockProjectRule) both call.
// It lets through a secret with no tag, one of a public project and one of a private project the
// credential names; it answers project-not-named for a private project the credential does not
// name, project-missing for a key Dispatch holds no project for, and ErrNoProjectState for a
// project the caller resolved no state for. Evaluate's answer for the same secret is a Deny with
// that reason, or the error, in every row, so the two readers cannot drift.
func TestTheProjectRuleIsOneFunction(t *testing.T) {
	const unresolved = "NEW"
	store := memory.New(
		policytest.Secret("PLAIN_KEY", policy.OwnerShared, policy.TierAgent, "v"),
		projectSecret("PUB_KEY", policy.OwnerShared, policy.TierAgent, pubProject),
		projectSecret("PRIV_KEY", policy.OwnerShared, policy.TierAgent, privProject),
		projectSecret("GONE_KEY", policy.OwnerShared, policy.TierAgent, goneProject),
		projectSecret("NEW_KEY", policy.OwnerShared, policy.TierAgent, unresolved),
	)
	set := loadProjects(t, store)
	for _, c := range []struct {
		name, project string
		requester     policy.Requester
		reason        string
		err           error
	}{
		{"PLAIN_KEY", "", credential(alice, ""), "", nil},
		{"PLAIN_KEY", "", policy.Requester{Operator: alice}, "", nil},
		{"PUB_KEY", pubProject, credential(alice, ""), "", nil},
		{"PRIV_KEY", privProject, credential(alice, "", privProject), "", nil},
		{"PRIV_KEY", privProject, credential(alice, "", pubProject), policy.ReasonProjectNotNamed, nil},
		{"PRIV_KEY", privProject, credential(alice, ""), policy.ReasonProjectNotNamed, nil},
		{"PRIV_KEY", privProject, policy.Requester{Operator: alice, Named: []string{privProject}, ProjectStates: states}, policy.ReasonProjectNotNamed, nil},
		{"GONE_KEY", goneProject, credential(alice, "", goneProject), policy.ReasonProjectMissing, nil},
		{"NEW_KEY", unresolved, credential(alice, "", unresolved), "", policy.ErrNoProjectState},
	} {
		reason, err := policy.ProjectRule(c.project, c.requester)
		if reason != c.reason || !errors.Is(err, c.err) || (c.err == nil && err != nil) {
			t.Errorf("ProjectRule(%q, %+v) = %q, %v; want %q, %v", c.project, c.requester, reason, err, c.reason, c.err)
		}
		decision, evalErr := set.Evaluate(c.name, c.requester)
		switch {
		case c.err != nil:
			if !errors.Is(evalErr, c.err) {
				t.Errorf("Evaluate(%s, %+v) = %+v, %v; want %v, as ProjectRule answers", c.name, c.requester, decision, evalErr, c.err)
			}
		case c.reason != "":
			if evalErr != nil || decision.Outcome != policy.Deny || decision.Reason != c.reason {
				t.Errorf("Evaluate(%s, %+v) = %+v, %v; want a Deny, %s, as ProjectRule answers", c.name, c.requester, decision, evalErr, c.reason)
			}
		default:
			if evalErr != nil || decision.Outcome != policy.Automatic {
				t.Errorf("Evaluate(%s, %+v) = %+v, %v; want automatic: the project rule lets it through and the secret is shared agent tier", c.name, c.requester, decision, evalErr)
			}
		}
	}
}
