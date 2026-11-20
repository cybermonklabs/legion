package policy_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/groups/groupstest"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// The groups and people of the group tests: example.com names alone, since the repository is
// public.
const (
	approversGroup = "approvers@example.com"
	approversOwner = record.GroupPrefix + approversGroup
	reviewersGroup = "reviewers@example.com"
	reviewersOwner = record.GroupPrefix + reviewersGroup
	groupMember    = "member@example.com"
	groupOutsider  = "outsider@example.com"
)

// loadOnly loads store's namespace once, with the services registered.
func loadOnly(t *testing.T, store *secrets.Local, services ...string) *policy.Set {
	t.Helper()
	set, err := policytest.Loader(store, services...).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return set
}

// TestTheOwnerTagTakesAGroup: an owner tag group:<address> loads, the secret is served with that
// owner, and the set answers it as group-owned; a person's secret is not.
func TestTheOwnerTagTakesAGroup(t *testing.T) {
	set := loadOnly(t, secrets.NewLocal(
		policytest.Secret("TEAM_KEY", approversOwner, policy.TierHuman, "v"),
		policytest.Secret("DEEL_API_KEY", "ada@example.com", policy.TierHuman, "v"),
	))
	secret, served := set.Secrets["TEAM_KEY"]
	if !served || secret.Owner != approversOwner {
		t.Fatalf("TEAM_KEY = %+v, served %v, refused %v; want served with owner %s", secret, served, set.Refused, approversOwner)
	}
	if !set.GroupOwned("TEAM_KEY") || set.GroupOwned("DEEL_API_KEY") || set.GroupOwned("NO_SUCH_KEY") {
		t.Fatalf("GroupOwned: TEAM_KEY %v, DEEL_API_KEY %v, NO_SUCH_KEY %v; want true, false, false",
			set.GroupOwned("TEAM_KEY"), set.GroupOwned("DEEL_API_KEY"), set.GroupOwned("NO_SUCH_KEY"))
	}
	if !policy.ValidGroupOwner(approversOwner) || policy.ValidGroupOwner(approversGroup) {
		t.Fatalf("ValidGroupOwner: %q %v, %q %v; want true, false", approversOwner, policy.ValidGroupOwner(approversOwner), approversGroup, policy.ValidGroupOwner(approversGroup))
	}
}

// TestMalformedGroupEntriesAreRefused: a group entry with no address, an uppercase address, or no
// @ is refused as a malformed owner and as a malformed requester, never read as a person or a
// service.
func TestMalformedGroupEntriesAreRefused(t *testing.T) {
	for _, entry := range []string{"group:", "group:Upper@example.com", "group:nobody"} {
		owned := policytest.Secret("TEAM_KEY", entry, policy.TierHuman, "v")
		listed := policytest.ListedSecret("LISTED_KEY", policy.OwnerShared, policy.TierHuman, entry, "v")
		set := loadOnly(t, secrets.NewLocal(owned, listed), "nobody")
		if got := set.Refused[policytest.ID("TEAM_KEY")].Reason; got != policy.ReasonOwnerTagMalformed {
			t.Errorf("owner %q: refused %q, want %s", entry, got, policy.ReasonOwnerTagMalformed)
		}
		if got := set.Refused[policytest.ID("LISTED_KEY")].Reason; got != policy.ReasonRequestersMalformed {
			t.Errorf("requesters %q: refused %q, want %s", entry, got, policy.ReasonRequestersMalformed)
		}
		if _, err := policy.ParseRequesterShapes(entry); err == nil {
			t.Errorf("ParseRequesterShapes(%q) = nil error, want it refused", entry)
		}
		if policy.ValidGroupOwner(entry) {
			t.Errorf("ValidGroupOwner(%q) = true, want false", entry)
		}
	}
}

// TestSplitRequestersNeverAnswersAGroupAsAPerson: a group entry holds an @, and SplitRequesters
// answers it as a group, never as a person.
func TestSplitRequestersNeverAnswersAGroupAsAPerson(t *testing.T) {
	people, services, groups := policy.SplitRequesters("ada@example.com " + approversOwner + " legion-daemon " + reviewersOwner)
	if !slices.Equal(people, []string{"ada@example.com"}) || !slices.Equal(services, []string{"legion-daemon"}) || !slices.Equal(groups, []string{approversOwner, reviewersOwner}) {
		t.Fatalf("SplitRequesters = people %v, services %v, groups %v; want [ada@example.com], [legion-daemon], [%s %s]", people, services, groups, approversOwner, reviewersOwner)
	}
	if canonical, err := policy.ParseRequesterShapes(approversOwner + " ada@example.com"); err != nil || canonical != "ada@example.com "+approversOwner {
		t.Fatalf("ParseRequesterShapes = %q, %v; want the two entries, sorted", canonical, err)
	}
}

// TestRequesterRuleHoldsAGroupOwnedServiceAccountSecretToItsGroup: a google-service-account secret
// a group owns is valid when its requesters name that group once and no person, beside services at
// human tier; another group, a person, a second group or a service at agent tier is refused, and a
// person owner's list names no group.
func TestRequesterRuleHoldsAGroupOwnedServiceAccountSecretToItsGroup(t *testing.T) {
	owned := func(tier, requesters string) policy.TagSet {
		return policy.TagSet{Type: policy.TypeGoogleServiceAccount, Owner: approversOwner, Tier: tier, Requesters: requesters, MaxGrant: time.Hour}
	}
	for _, c := range []struct {
		name string
		set  policy.TagSet
		want policy.RequesterFault
	}{
		{"its group once", owned(policy.TierHuman, approversOwner), policy.RequesterFaultNone},
		{"its group once, at agent tier", owned(policy.TierAgent, approversOwner), policy.RequesterFaultNone},
		{"its group and a service, at human tier", owned(policy.TierHuman, approversOwner+" legion-daemon"), policy.RequesterFaultNone},
		{"another group", owned(policy.TierHuman, reviewersOwner), policy.RequesterFaultNotTheOwnerGroupOnce},
		{"its group and another", owned(policy.TierHuman, approversOwner+" "+reviewersOwner), policy.RequesterFaultNotTheOwnerGroupOnce},
		{"a person", owned(policy.TierHuman, "ada@example.com"), policy.RequesterFaultNotTheOwnerGroupOnce},
		{"its group and a person", owned(policy.TierHuman, "ada@example.com "+approversOwner), policy.RequesterFaultNotTheOwnerGroupOnce},
		{"no list", owned(policy.TierHuman, ""), policy.RequesterFaultNotTheOwnerGroupOnce},
		{"a service at agent tier", owned(policy.TierAgent, approversOwner+" legion-daemon"), policy.RequesterFaultServiceBelowHumanTier},
		{"a person owner whose list names a group", policy.TagSet{Type: policy.TypeGoogleServiceAccount, Owner: "ada@example.com", Tier: policy.TierHuman, Requesters: "ada@example.com " + approversOwner, MaxGrant: time.Hour}, policy.RequesterFaultNotTheOwnerOnce},
	} {
		if got := policy.RequesterRule(c.set); got != c.want {
			t.Errorf("%s: RequesterRule = %v, want %v", c.name, got, c.want)
		}
	}
	if reason := policy.TagRules(owned(policy.TierHuman, reviewersOwner)); reason != policy.ReasonRequestersNotOwner {
		t.Fatalf("TagRules(another group) = %q, want %s", reason, policy.ReasonRequestersNotOwner)
	}
	set := loadOnly(t, secrets.NewLocal(policytest.TypeSecret("ROBOT_TOKEN", approversOwner, policy.TierHuman, policy.TypeGoogleServiceAccount, policytest.TypeValue(policy.TypeGoogleServiceAccount))))
	if _, served := set.Secrets["ROBOT_TOKEN"]; !served {
		t.Fatalf("a google-service-account secret owned by %s listing its group is refused: %v", approversOwner, set.Refused)
	}
}

// TestEvaluateAdmitsAGroupMember: an agent-tier secret a group owns goes to a member's session at
// once and is an approval request to the group for anyone else, a member it was withheld from
// included, and for anyone once the group is stale; a human-tier one is an approval request to the
// group; a requesters entry naming a group admits a member and refuses a non-member.
func TestEvaluateAdmitsAGroupMember(t *testing.T) {
	loaded := loadOnly(t, secrets.NewLocal(
		policytest.Secret("TEAM_KEY", approversOwner, policy.TierAgent, "v"),
		policytest.Secret("TEAM_ADMIN_KEY", approversOwner, policy.TierHuman, "v"),
		policytest.ListedSecret("LISTED_KEY", policy.OwnerShared, policy.TierAgent, approversOwner, "v"),
	))
	fake := groupstest.New()
	fake.Set(approversGroup, groupMember)
	set := policy.NewSetWithGroups(loaded.Secrets, fake)
	member, outsider := policy.Requester{Operator: groupMember}, policy.Requester{Operator: groupOutsider}
	evaluate := func(name string, r policy.Requester) policy.Decision {
		t.Helper()
		d, err := set.Evaluate(name, r)
		if err != nil {
			t.Fatalf("Evaluate(%s, %+v): %v", name, r, err)
		}
		return d
	}
	approval := policy.Decision{Outcome: policy.Approval, Approver: approversOwner}
	for _, c := range []struct {
		name, secret string
		r            policy.Requester
		want         policy.Decision
	}{
		{"a member, agent tier", "TEAM_KEY", member, policy.Decision{Outcome: policy.Automatic}},
		{"a member in another casing, agent tier", "TEAM_KEY", policy.Requester{Operator: " Member@Example.com "}, policy.Decision{Outcome: policy.Automatic}},
		{"a non-member, agent tier", "TEAM_KEY", outsider, approval},
		{"a member it was withheld from", "TEAM_KEY", policy.Requester{Operator: groupMember, Withheld: []string{"TEAM_KEY"}}, approval},
		{"a pod of no person", "TEAM_KEY", policy.Requester{Service: "legion-daemon"}, approval},
		{"a member, human tier", "TEAM_ADMIN_KEY", member, approval},
		{"a member, listed by group", "LISTED_KEY", member, policy.Decision{Outcome: policy.Automatic}},
		{"a non-member, listed by group", "LISTED_KEY", outsider, policy.Decision{Outcome: policy.Deny, Reason: policy.ReasonRequesterNotListed}},
	} {
		got := evaluate(c.secret, c.r)
		got.Source = ""
		if got != c.want {
			t.Errorf("%s: Evaluate(%s) = %+v, want %+v", c.name, c.secret, got, c.want)
		}
	}
	fake.MarkStale(approversGroup)
	if got := evaluate("TEAM_KEY", member); got.Outcome != policy.Approval || got.Approver != approversOwner {
		t.Errorf("a member of a stale group, agent tier: Evaluate = %+v, want an approval request to %s", got, approversOwner)
	}
	if got := evaluate("LISTED_KEY", member); got.Outcome != policy.Deny || got.Reason != policy.ReasonRequesterNotListed {
		t.Errorf("a member of a stale group, listed by group: Evaluate = %+v, want %s", got, policy.ReasonRequesterNotListed)
	}
	if got, err := policy.NewSet(loaded.Secrets).Evaluate("TEAM_KEY", member); err != nil || got.Outcome != policy.Approval {
		t.Errorf("a set with no group reader: Evaluate(TEAM_KEY, member) = %+v, %v; want an approval request", got, err)
	}
}

// TestALoadNamesItsGroupsAndARereadDoesNot: a full load names every group the served secrets name
// to the reader; a reread that retags a secret to a new group publishes the new owner at once and
// names nothing, so the reader, never told of the new group, admits nobody to it; and the next
// load names it.
func TestALoadNamesItsGroupsAndARereadDoesNot(t *testing.T) {
	const a, b = "a@example.com", "b@example.com"
	store := secrets.NewLocal(policytest.Secret("A_KEY", record.GroupPrefix+a, policy.TierHuman, "v"))
	fake := groupstest.New()
	cur := groupCurrent(t, store, fake)
	if got := fake.Wants(); !slices.EqualFunc(got, [][]string{{a}}, slices.Equal) {
		t.Fatalf("NewCurrent's load named %v to the reader, want [[%s]]", got, a)
	}
	store.Put(policytest.Secret("A_KEY", record.GroupPrefix+b, policy.TierHuman, "v"))
	if _, err := cur.RefreshOne(context.Background(), "A_KEY"); err != nil {
		t.Fatalf("RefreshOne(A_KEY): %v", err)
	}
	if got := cur.Get().Secrets["A_KEY"].Owner; got != record.GroupPrefix+b {
		t.Fatalf("after the reread A_KEY's owner is %q, want %s", got, record.GroupPrefix+b)
	}
	if got := fake.Wants(); len(got) != 1 {
		t.Fatalf("the reread named groups to the reader: %v", got)
	}
	if cur.Get().Groups().HasMember(b, groupMember) || cur.Get().Groups().Fresh(b) {
		t.Fatal("the reader admits a member of a group no load named")
	}
	if err := cur.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := fake.Wants(); !slices.EqualFunc(got, [][]string{{a}, {b}}, slices.Equal) {
		t.Fatalf("the next load named %v to the reader, want [[%s] [%s]]", got, a, b)
	}
}

// TestGroupNamesIsEveryGroupAServedSecretNames: the owner groups and the requesters group entries
// of the served secrets, each once, sorted, as bare addresses; a refused secret's group is not
// named.
func TestGroupNamesIsEveryGroupAServedSecretNames(t *testing.T) {
	refused := policytest.Secret("REFUSED_KEY", record.GroupPrefix+"refused@example.com", policy.TierHuman, "v")
	delete(refused.Tags, policy.TagTier)
	set := loadOnly(t, secrets.NewLocal(
		policytest.Secret("TEAM_KEY", reviewersOwner, policy.TierHuman, "v"),
		policytest.ListedSecret("LISTED_KEY", approversOwner, policy.TierHuman, approversOwner+" "+reviewersOwner+" ada@example.com", "v"),
		policytest.Secret("DEEL_API_KEY", "ada@example.com", policy.TierHuman, "v"),
		refused,
	))
	if got := set.GroupNames(); !slices.Equal(got, []string{approversGroup, reviewersGroup}) {
		t.Fatalf("GroupNames = %v, want [%s %s]", got, approversGroup, reviewersGroup)
	}
}

// groupCurrent is the live policy over store with fake as its loader's group directory.
func groupCurrent(t *testing.T, store *secrets.Local, fake *groupstest.Directory) *policy.Current {
	t.Helper()
	loader := policytest.Loader(store)
	loader.Groups = fake
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("NewCurrent: %v", err)
	}
	return cur
}

// TestEachPublishedSetCarriesTheLoadersDirectory: the set the first load makes live, and the set
// a reread that changes a secret makes live, each carry the loader's group directory, so every
// decision on a set Get answers asks the same reader; a set no Current published carries none.
func TestEachPublishedSetCarriesTheLoadersDirectory(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("DEEL_API_KEY", "ada@example.com", policy.TierAgent, "v1"))
	fake := groupstest.New()
	cur := groupCurrent(t, store, fake)
	if got := cur.Get().Groups(); got != record.Groups(fake) {
		t.Fatalf("the first load's set carries groups %v, want the loader's directory", got)
	}
	store.Put(policytest.Secret("DEEL_API_KEY", "ada@example.com", policy.TierHuman, "v1"))
	if lk, err := cur.RefreshOne(t.Context(), "DEEL_API_KEY"); err != nil || lk.Secret.Tier != policy.TierHuman {
		t.Fatalf("RefreshOne(DEEL_API_KEY) = %+v, %v; want it served at human tier", lk, err)
	}
	if got := cur.Get().Groups(); got != record.Groups(fake) {
		t.Fatalf("the reread's set carries groups %v, want the loader's directory", got)
	}
	if got := policy.NewSet(map[string]policy.Secret{}).Groups(); got != nil {
		t.Fatalf("NewSet(...).Groups() = %v, want nil", got)
	}
	if got := policy.NewSetWithGroups(map[string]policy.Secret{}, fake).Groups(); got != record.Groups(fake) {
		t.Fatalf("NewSetWithGroups(..., fake).Groups() = %v, want fake", got)
	}
}
