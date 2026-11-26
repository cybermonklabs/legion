package access

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestCredentialCoversDecidesOnlyPrivateProjects (R3): a full member's list decides
// only which private projects its credential reaches, and a restricted member's list narrows every
// project. carol's token that lists PRIV covers PUB and PRIV, and not PRIV2; bob's token that lists
// PRIV covers PRIV and not PUB; the shared token covers PUB and not PRIV. A restricted member's
// token that lists none covers the public projects and no private one, since a private project
// needs a token that lists it; a credential of no person that lists PRIV covers PRIV and PUB, and
// one that lists none covers PUB alone (R3); a browser lists nothing and
// covers every project.
func TestCredentialCoversDecidesOnlyPrivateProjects(t *testing.T) {
	private := map[string]bool{"PUB": false, "PRIV": true, "PRIV2": true}
	for _, test := range []struct {
		name   string
		cred   Credential
		covers []string
	}{
		{"carol's token that lists PRIV", Credential{Listed: []string{"PRIV"}, Person: true}, []string{"PRIV", "PUB"}},
		{"bob's token that lists PRIV", Credential{Listed: []string{"PRIV"}, Person: true, Restricted: true}, []string{"PRIV"}},
		{"the shared token", Credential{}, []string{"PUB"}},
		{"bob's token that lists none", Credential{Person: true, Restricted: true}, []string{"PUB"}},
		{"a token of no person that lists PRIV", Credential{Listed: []string{"PRIV"}}, []string{"PRIV", "PUB"}},
		{"carol's browser", Credential{Person: true, Browser: true}, []string{"PRIV", "PRIV2", "PUB"}},
	} {
		for project, isPrivate := range private {
			if got, want := CredentialCovers(test.cred, project, isPrivate), slices.Contains(test.covers, project); got != want {
				t.Errorf("%s: CredentialCovers(%s, private=%t) = %t, want %t", test.name, project, isPrivate, got, want)
			}
		}
	}
}

// TestACredentialOfNoPersonSeesThePrivateProjectsItLists (R3): a credential that names no person
// sees every public project and the private projects its list names, as Resolve answers it: a
// service-account token that lists PRIV sees PRIV and PUB, and one that lists nothing, the shared
// token, sees PUB alone. These two rows are the rule's, read through Resolve. A job's broker token
// also names no person and lists projects, and no row of this test runs that shape.
func TestACredentialOfNoPersonSeesThePrivateProjectsItLists(t *testing.T) {
	database := seedAccess(t)
	for _, test := range []struct {
		name  string
		actor model.Actor
		sees  []string
	}{
		{"a service-account token that lists PRIV", model.Actor{Service: new("system:serviceaccount:legion:worker"), Credential: &model.Credential{Projects: []string{"PRIV"}}}, []string{"PRIV", "PUB"}},
		{"the shared token, which lists nothing", model.Actor{Credential: &model.Credential{}}, []string{"PUB"}},
	} {
		viewer, err := Resolve(context.Background(), database.Pool, test.actor)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", test.name, err)
		}
		if !slices.Equal(viewer.Sees, test.sees) || !viewer.Open {
			t.Errorf("%s: Resolve = %+v, want an open viewer that sees %v", test.name, viewer, test.sees)
		}
	}
}

// TestTheListRuleIsOneInGoAndSQL: the list rule's Go form (CredentialCovers, through
// Credential.Sees) and its SQL form (VisibleProjectsSQL, through Resolve) answer alike over every
// combination: a full member, a restricted member and no person; a browser and an agent token;
// every list of the four projects, none included; each project public or private, and seen by the
// person or not. full@ is a full member PUBSEEN and PRIVSEEN name; restricted@ is a restricted
// member PUBSEEN and PRIVSEEN name; PUBUNSEEN and PRIVUNSEEN name nobody. Each caller's Sees is
// also held to the rule's stated answer for the combinations the forms could agree on wrongly: a
// full member's token sees the public projects whatever it lists and only the private ones it
// lists; a restricted member's token is held to its list, or with none to the public projects its
// person sees; a credential of no person sees the public projects and the private ones it lists.
// With no private project a full member's list decides nothing it reads (R9): each of its tokens
// sees what its token listing none sees. Resolve still answers a personal token's list as Lists,
// which is new with private projects, and no Lists for a full member's broker token, whose list it
// answered as none before (R9); a restricted member's list holds in every state.
func TestTheListRuleIsOneInGoAndSQL(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	const full, restricted = "full@example.com", "restricted@other.example"
	if _, err := database.Pool.Exec(ctx, `
		insert into people (email, member_kind) values
			('olga@example.com', 'full'), ('full@example.com', 'full'), ('restricted@other.example', 'restricted');
		insert into projects (key, name, default_assignee, visibility) values
			('PUBSEEN', 'Public seen', 'olga@example.com', 'public'),
			('PUBUNSEEN', 'Public unseen', 'olga@example.com', 'public'),
			('PRIVSEEN', 'Private seen', 'olga@example.com', 'private'),
			('PRIVUNSEEN', 'Private unseen', 'olga@example.com', 'private');
		insert into project_access (project_key, principal_kind, principal, added_by) values
			('PUBSEEN', 'person', 'full@example.com', '{"kind":"user","id":"olga@example.com"}'),
			('PUBSEEN', 'person', 'restricted@other.example', '{"kind":"user","id":"olga@example.com"}'),
			('PRIVSEEN', 'person', 'full@example.com', '{"kind":"user","id":"olga@example.com"}'),
			('PRIVSEEN', 'person', 'restricted@other.example', '{"kind":"user","id":"olga@example.com"}');
	`); err != nil {
		t.Fatalf("seed the combinations: %v", err)
	}
	projects := []string{"PRIVSEEN", "PRIVUNSEEN", "PUBSEEN", "PUBUNSEEN"}
	private := map[string]bool{"PRIVSEEN": true, "PRIVUNSEEN": true, "PUBSEEN": false, "PUBUNSEEN": false}
	var lists [][]string
	for mask := range 1 << len(projects) {
		var list []string
		for i, project := range projects {
			if mask&(1<<i) != 0 {
				list = append(list, project)
			}
		}
		lists = append(lists, list)
	}
	type caller struct {
		name       string
		actor      model.Actor
		person     string
		restricted bool
		listed     []string
	}
	var callers []caller
	for _, person := range []string{full, restricted} {
		callers = append(callers, caller{person + "'s browser", cookie(person), person, person == restricted, nil})
		for _, list := range lists {
			callers = append(callers, caller{fmt.Sprintf("%s's token listing %v", person, list), personalToken(person, list...), person, person == restricted, list})
		}
	}
	for _, list := range lists {
		callers = append(callers, caller{fmt.Sprintf("a token of no person listing %v", list),
			model.Actor{Service: new("system:serviceaccount:legion:worker"), Credential: &model.Credential{Projects: list}}, "", false, list})
	}
	personSees := map[string][]string{}
	for _, person := range []string{full, restricted} {
		viewer, err := Resolve(ctx, database.Pool, cookie(person))
		if err != nil {
			t.Fatalf("Resolve %s's browser: %v", person, err)
		}
		personSees[person] = viewer.Sees
	}
	// The rule's stated answer, written apart from both forms.
	want := func(c caller, project string) bool {
		listed := slices.Contains(c.listed, project)
		switch {
		case c.actor.Kind == "user":
			return slices.Contains(personSees[c.person], project)
		case c.person == "":
			return !private[project] || listed
		case !slices.Contains(personSees[c.person], project):
			return false
		case c.restricted && len(c.listed) > 0:
			return listed
		}
		return !private[project] || listed
	}
	compared := 0
	for _, c := range callers {
		viewer, err := Resolve(ctx, database.Pool, c.actor)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", c.name, err)
		}
		if viewer.Open == c.restricted {
			t.Errorf("%s: Resolve answers open %t, want %t: a viewer is open when it is not a restricted member's", c.name, viewer.Open, !c.restricted)
		}
		cred := Credential{Listed: c.listed, Person: c.person != "", Browser: c.actor.Kind == "user", Restricted: c.restricted, PersonSees: personSees[c.person]}
		for _, project := range projects {
			compared++
			goForm, sqlForm := cred.Sees(project, private[project]), slices.Contains(viewer.Sees, project)
			if goForm != sqlForm {
				t.Errorf("%s, %s (private %t): Credential.Sees = %t, VisibleProjectsSQL = %t", c.name, project, private[project], goForm, sqlForm)
			}
			if w := want(c, project); goForm != w || sqlForm != w {
				t.Errorf("%s, %s (private %t): Go %t, SQL %t, the rule says %t", c.name, project, private[project], goForm, sqlForm, w)
			}
		}
	}
	if wantCompared := (2*(1+len(lists)) + len(lists)) * len(projects); compared != wantCompared {
		t.Fatalf("compared %d answers, want %d: not every combination was read", compared, wantCompared)
	}

	if _, err := database.Pool.Exec(ctx, `update projects set visibility = 'public'`); err != nil {
		t.Fatalf("make every project public: %v", err)
	}
	unlisted := map[string]model.Viewer{}
	for _, c := range callers {
		if c.actor.Kind != "user" && len(c.listed) == 0 {
			viewer, err := Resolve(ctx, database.Pool, c.actor)
			if err != nil {
				t.Fatalf("%s: Resolve: %v", c.name, err)
			}
			unlisted[c.person] = viewer
		}
	}
	for _, c := range callers {
		if c.actor.Kind == "user" || len(c.listed) == 0 {
			continue
		}
		viewer, err := Resolve(ctx, database.Pool, c.actor)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", c.name, err)
		}
		switch {
		case !slices.Equal(viewer.Lists, c.listed):
			t.Errorf("with no private project, %s resolves Lists %v, want its list %v: a token that is no broker token's is answered its list in every state", c.name, viewer.Lists, c.listed)
		case !c.restricted && !slices.Equal(viewer.Sees, unlisted[c.person].Sees):
			t.Errorf("with no private project, %s resolves %+v, want what its token listing none sees, %v (R9)", c.name, viewer, unlisted[c.person].Sees)
		}
		if c.person == "" || c.restricted {
			continue
		}
		broker := model.Actor{Kind: "session", ID: "ses_broker", Owner: new(c.person),
			Credential: &model.Credential{Scopes: model.Scopes, Session: "ses_broker", Projects: c.listed}}
		brokered, err := Resolve(ctx, database.Pool, broker)
		if err != nil {
			t.Fatalf("%s's broker token listing %v: Resolve: %v", c.person, c.listed, err)
		}
		if brokered.Lists != nil || !slices.Equal(brokered.Sees, unlisted[c.person].Sees) {
			t.Errorf("with no private project, %s's broker token listing %v resolves %+v, want what its token listing none sees, %v, and no Lists (R9)",
				c.person, c.listed, brokered, unlisted[c.person].Sees)
		}
	}
}
