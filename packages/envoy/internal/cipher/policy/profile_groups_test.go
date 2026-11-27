package policy_test

// A profile and a Google group meet in three places: a profile's requesters may name a group, a
// profile of a group-owned source is the group's, and a full load names every group to the group
// reader, the profiles' among them.

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/groups/groupstest"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// profiledWithGroups is profiled with fake as the loader's group reader, so every set the policy
// makes live asks fake who is a member of a group, and each full load names its groups to fake.
func profiledWithGroups(t *testing.T, stored []secrets.LocalSecret, fake *groupstest.Directory, profiles ...string) *policy.Current {
	t.Helper()
	loader := policytest.Loader(secrets.NewLocal(stored...), daemonService)
	loader.Profiles = policytest.Profiles(t, policytest.KindsFile(profiles...), daemonService)
	loader.Groups = fake
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("NewCurrent: %v", err)
	}
	return cur
}

// groupListedApp is GROUP_LISTED_APP, an agent-tier GitHub App that the members of approversGroup
// and the daemon service may ask for, for an hour at most.
func groupListedApp() secrets.LocalSecret {
	app := policytest.TypeSecret("GROUP_LISTED_APP", appOwner, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
	app.Tags[policy.TagRequesters] = approversOwner + " " + daemonService
	app.Tags[policy.TagMaxGrant] = "1h"
	return app
}

// evaluated fails t unless set answers r's ask for name with outcome and reason.
func evaluated(t *testing.T, set *policy.Set, name, what string, r policy.Requester, outcome, reason string) {
	t.Helper()
	d, err := set.Evaluate(name, r)
	if err != nil || d.Outcome != outcome || d.Reason != reason {
		t.Errorf("%s: Evaluate(%s) = %+v, %v; want %s %q", what, name, d, err, outcome, reason)
	}
}

// TestAGroupOnlyAProfileNamesIsNamedToTheReaderAtTheLoad: a full load names to the group reader
// every group the profiles name as well as the stored secrets'. TEAM_READ's requesters name
// approversGroup, which its source, with no requesters tag, does not, so only the profile names
// it: the load names it, a member of it is then put to the source's owner as any listed requester
// is, and an outsider is denied requester-not-listed. A load that named only its stored secrets'
// groups would leave the reader never told of the group, which then admits nobody, and the profile
// would serve none of its members.
func TestAGroupOnlyAProfileNamesIsNamedToTheReaderAtTheLoad(t *testing.T) {
	fake := groupstest.New()
	cur := profiledWithGroups(t, []secrets.LocalSecret{appSource()}, fake, readProfile,
		`{"name": "TEAM_READ", "source": "LEGION_IMPLEMENT_APP", "requesters": ["`+approversOwner+`"], `+acmeRead+`}`)
	if got := fake.Wants(); !slices.EqualFunc(got, [][]string{{approversGroup}}, slices.Equal) {
		t.Fatalf("the load named %v to the reader, want [[%s]]", got, approversGroup)
	}
	fake.Set(approversGroup, groupMember)
	set := cur.Get()
	evaluated(t, set, "TEAM_READ", "a member", policy.Requester{Operator: groupMember}, policy.Approval, "")
	evaluated(t, set, "TEAM_READ", "an outsider", policy.Requester{Operator: groupOutsider}, policy.Deny, policy.ReasonRequesterNotListed)
}

// TestAProfileNarrowsAGroupRequesterOnlyToThatGroup: a group entry in a source's requesters is
// matched by the same group entry alone when the profile's list is checked against it (R2: a
// requester the source's requesters does not name is looser). A profile naming that group is
// served, and admits the group's members as its source does, denying an outsider and pausing a
// member while the group is not read; a profile naming a person, though a member today, is refused,
// since the group's members change and the person's entry would not; and so is a profile naming a
// group of a source whose list names only people and services, or another group.
func TestAProfileNarrowsAGroupRequesterOnlyToThatGroup(t *testing.T) {
	stored := append(everySource(), groupListedApp())
	for _, row := range []struct {
		name, profile string
		served        bool
	}{
		{"the source's group", `{"name": "TEAM_READ", "source": "GROUP_LISTED_APP", "requesters": ["` + approversOwner + `"], ` + acmeRead + `}`, true},
		{"a person in the source's group", `{"name": "TEAM_READ", "source": "GROUP_LISTED_APP", "requesters": ["` + groupMember + `"], ` + acmeRead + `}`, false},
		{"another group", `{"name": "TEAM_READ", "source": "GROUP_LISTED_APP", "requesters": ["` + reviewersOwner + `"], ` + acmeRead + `}`, false},
		{"a group on a source that lists none", `{"name": "TEAM_READ", "source": "LISTED_APP", "requesters": ["` + approversOwner + `"], ` + acmeRead + `}`, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := groupstest.New()
			fake.Set(approversGroup, groupMember)
			set := profiledWithGroups(t, stored, fake, readProfile, row.profile).Get()
			if !row.served {
				refusedProfile(t, set, "TEAM_READ", policy.ReasonProfileLooser)
				return
			}
			if got := servedProfile(t, set, "TEAM_READ"); got.Requesters != approversOwner {
				t.Fatalf("TEAM_READ's requesters = %q; want %q", got.Requesters, approversOwner)
			}
			evaluated(t, set, "TEAM_READ", "a member", policy.Requester{Operator: groupMember}, policy.Approval, "")
			evaluated(t, set, "TEAM_READ", "an outsider", policy.Requester{Operator: groupOutsider}, policy.Deny, policy.ReasonRequesterNotListed)
			evaluated(t, set, "TEAM_READ", "the daemon, which the profile leaves out", policy.Requester{Service: daemonService}, policy.Deny, policy.ReasonRequesterNotListed)
			fake.MarkStale(approversGroup)
			evaluated(t, set, "TEAM_READ", "a member, group stale", policy.Requester{Operator: groupMember}, policy.Deny, policy.ReasonRequesterGroupUnread)
		})
	}
}

// TestAProfileOfAGroupOwnedSourceIsTheGroups: a profile's owner is its source's (R2), so a profile
// of a secret a group owns is the group's: the set answers it group-owned, which the release's
// re-check reads to re-check its grants on every use; a member's session gets it at once at agent
// tier while the group is read; an outsider, and a member while the group is not read, are put to
// the group.
func TestAProfileOfAGroupOwnedSourceIsTheGroups(t *testing.T) {
	fake := groupstest.New()
	fake.Set(approversGroup, groupMember)
	team := policytest.Secret("TEAM_KEY", approversOwner, policy.TierAgent, "team-v1")
	set := profiledWithGroups(t, []secrets.LocalSecret{appSource(), team}, fake, readProfile,
		`{"name": "TEAM_PROXY", "source": "TEAM_KEY", `+apiProxy+`}`).Get()
	profile := servedProfile(t, set, "TEAM_PROXY")
	if profile.Owner != approversOwner || !set.GroupOwned("TEAM_PROXY") {
		t.Fatalf("TEAM_PROXY's owner = %q, group-owned %t; want %s, true", profile.Owner, set.GroupOwned("TEAM_PROXY"), approversOwner)
	}
	evaluated(t, set, "TEAM_PROXY", "a member", policy.Requester{Operator: groupMember}, policy.Automatic, "")
	d, err := set.Evaluate("TEAM_PROXY", policy.Requester{Operator: groupOutsider})
	if err != nil || d.Outcome != policy.Approval || d.Approver != approversOwner {
		t.Fatalf("an outsider: Evaluate(TEAM_PROXY) = %+v, %v; want an approval of %s", d, err, approversOwner)
	}
	fake.MarkStale(approversGroup)
	if d, err := set.Evaluate("TEAM_PROXY", policy.Requester{Operator: groupMember}); err != nil || d.Outcome != policy.Approval || d.Approver != approversOwner {
		t.Fatalf("a member, group stale: Evaluate(TEAM_PROXY) = %+v, %v; want an approval of %s", d, err, approversOwner)
	}
}
