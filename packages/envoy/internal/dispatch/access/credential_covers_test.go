package access

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestCredentialCoversDecidesOnlyPrivateProjects (R3, plan D8): a list decides only which private
// projects its credential reaches, for every credential, a limited member's as a full member's
// (its person's limit is Sees', not this test's). carol's token that lists PRIV covers PUB and
// PRIV, and not PRIV2; a person's token that lists none covers the public projects and no private
// one, since a private project needs a token that lists it; the shared token covers PUB and not
// PRIV; a credential of no person that lists PRIV covers PRIV and PUB (R3); a browser lists
// nothing and covers every project. A results token's write is held to its one project, private or
// public (ResultsOnly, withinProjectScope's write rule): one of PRIV covers PRIV alone, and one of
// PUB covers PUB alone. The empty project stands for every project and is not private, so each
// credential covers it except a results token's.
func TestCredentialCoversDecidesOnlyPrivateProjects(t *testing.T) {
	private := map[string]bool{"": false, "PUB": false, "PRIV": true, "PRIV2": true}
	for _, test := range []struct {
		name   string
		cred   Credential
		covers []string
	}{
		{"carol's token that lists PRIV", Credential{Listed: []string{"PRIV"}, Person: true}, []string{"", "PRIV", "PUB"}},
		{"the shared token", Credential{}, []string{"", "PUB"}},
		{"a person's token that lists none", Credential{Person: true}, []string{"", "PUB"}},
		{"a token of no person that lists PRIV", Credential{Listed: []string{"PRIV"}}, []string{"", "PRIV", "PUB"}},
		{"carol's browser", Credential{Person: true, Browser: true}, []string{"", "PRIV", "PRIV2", "PUB"}},
		{"a results token of PRIV", Credential{Listed: []string{"PRIV"}, ResultsOnly: true}, []string{"PRIV"}},
		{"a results token of PUB", Credential{Listed: []string{"PUB"}, ResultsOnly: true}, []string{"PUB"}},
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

// TestTheListRuleIsOneInGoAndSQL (plan D8, R1): the list rule is one formula, which its Go form
// (Credential.Sees, through CredentialCovers) and its SQL form (VisibleProjectsSQL, through
// Resolve) both answer: a caller sees project P when its person sees P and P is public, or its
// list names P, or it is the person's browser; a caller of no person sees P when P is public or its
// list names P. A list narrows private projects only, a limited member's as well as a full
// member's. A person sees P when P's owner is the person, P's access list names the person, P's
// list names a group the person is a member of (a limited member too), or P is public and the
// person is a full member, so a limited member sees no public project whose list does not name it,
// whatever its credential lists (the architect's ruling on departure 6).
//
// The callers: the browser of a full member (full@) and of a limited member (restricted@), each's
// personal token and broker token, a broker token of a person no people row holds (newbie@, whom
// the broker signs in as a full member), and a token of no person; each token with every list of
// none, one and two of the five projects. PUBSEEN and PRIVSEEN name full@ and restricted@,
// PRIVGROUP names the group crew, whose members they both are, and PUBUNSEEN and PRIVUNSEEN name
// nobody. What each person sees is stated here, apart from both forms, and both forms are held to
// the formula over it. With no private project no list decides anything a caller reads: each
// caller sees what its person sees, and Resolve still answers Lists as before (R9).
func TestTheListRuleIsOneInGoAndSQL(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	const full, restricted, newbie = "full@example.com", "restricted@other.example", "newbie@example.com"
	if _, err := database.Pool.Exec(ctx, `
		insert into people (email, member_kind) values
			('olga@example.com', 'full'), ('full@example.com', 'full'), ('restricted@other.example', 'restricted');
		insert into projects (key, name, default_assignee, visibility) values
			('PUBSEEN', 'Public seen', 'olga@example.com', 'public'),
			('PUBUNSEEN', 'Public unseen', 'olga@example.com', 'public'),
			('PRIVSEEN', 'Private seen', 'olga@example.com', 'private'),
			('PRIVUNSEEN', 'Private unseen', 'olga@example.com', 'private'),
			('PRIVGROUP', 'Private through a group', 'olga@example.com', 'private');
		insert into access_groups (name, owner) values ('crew', 'olga@example.com');
		insert into access_group_members (group_name, person) values ('crew', 'full@example.com'), ('crew', 'restricted@other.example');
		insert into project_access (project_key, principal_kind, principal, added_by) values
			('PUBSEEN', 'person', 'full@example.com', '{"kind":"user","id":"olga@example.com"}'),
			('PUBSEEN', 'person', 'restricted@other.example', '{"kind":"user","id":"olga@example.com"}'),
			('PRIVSEEN', 'person', 'full@example.com', '{"kind":"user","id":"olga@example.com"}'),
			('PRIVSEEN', 'person', 'restricted@other.example', '{"kind":"user","id":"olga@example.com"}'),
			('PRIVGROUP', 'group', 'crew', '{"kind":"user","id":"olga@example.com"}');
	`); err != nil {
		t.Fatalf("seed the combinations: %v", err)
	}
	projects := []string{"PRIVGROUP", "PRIVSEEN", "PRIVUNSEEN", "PUBSEEN", "PUBUNSEEN"}
	private := map[string]bool{"PRIVGROUP": true, "PRIVSEEN": true, "PRIVUNSEEN": true, "PUBSEEN": false, "PUBUNSEEN": false}
	// What each person sees, by the person rule.
	personSees := map[string][]string{
		full:       {"PRIVGROUP", "PRIVSEEN", "PUBSEEN", "PUBUNSEEN"},
		restricted: {"PRIVGROUP", "PRIVSEEN", "PUBSEEN"},
		newbie:     {"PUBSEEN", "PUBUNSEEN"},
	}
	lists := [][]string{nil}
	for i, first := range projects {
		lists = append(lists, []string{first})
		for _, second := range projects[i+1:] {
			lists = append(lists, []string{first, second})
		}
	}
	type caller struct {
		name                string
		actor               model.Actor
		person              string
		browser, restricted bool
		broker              bool
		listed              []string
	}
	broker := func(person string, list []string) model.Actor {
		session := "ses_broker_" + strings.Split(person, "@")[0]
		return model.Actor{Kind: "session", ID: session, Owner: new(person),
			Credential: &model.Credential{Scopes: model.Scopes, Session: session, Projects: list}}
	}
	var callers []caller
	for _, person := range []string{full, restricted} {
		callers = append(callers, caller{name: person + "'s browser", actor: cookie(person), person: person, browser: true, restricted: person == restricted})
	}
	for _, list := range lists {
		for _, person := range []string{full, restricted} {
			callers = append(callers,
				caller{name: fmt.Sprintf("%s's personal token listing %v", person, list), actor: personalToken(person, list...), person: person, restricted: person == restricted, listed: list},
				caller{name: fmt.Sprintf("%s's broker token listing %v", person, list), actor: broker(person, list), person: person, restricted: person == restricted, broker: true, listed: list})
		}
		callers = append(callers,
			caller{name: fmt.Sprintf("%s's broker token listing %v", newbie, list), actor: broker(newbie, list), person: newbie, broker: true, listed: list},
			caller{name: fmt.Sprintf("a token of no person listing %v", list),
				actor: model.Actor{Service: new("system:serviceaccount:legion:worker"), Credential: &model.Credential{Projects: list}}, listed: list})
	}
	for _, person := range []string{full, restricted} {
		viewer, err := Resolve(ctx, database.Pool, cookie(person))
		if err != nil {
			t.Fatalf("Resolve %s's browser: %v", person, err)
		}
		if !slices.Equal(viewer.Sees, personSees[person]) {
			t.Errorf("%s sees %v, want %v: the person rule admits a limited member through a group as through a direct listing, and a public project only to a full member or a person its list names",
				person, viewer.Sees, personSees[person])
		}
	}
	// The formula, written apart from both forms.
	want := func(c caller, project string) bool {
		if c.person != "" && !slices.Contains(personSees[c.person], project) {
			return false
		}
		return !private[project] || slices.Contains(c.listed, project) || c.browser
	}
	compared := 0
	for _, c := range callers {
		viewer, err := Resolve(ctx, database.Pool, c.actor)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", c.name, err)
		}
		if viewer.Open == c.restricted {
			t.Errorf("%s: Resolve answers open %t, want %t: a viewer is open when it is not a limited member's", c.name, viewer.Open, !c.restricted)
		}
		cred := Credential{Listed: c.listed, Person: c.person != "", Browser: c.browser, PersonSees: personSees[c.person]}
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
		if c.restricted && slices.Contains(viewer.Sees, "PUBUNSEEN") {
			t.Errorf("%s sees PUBUNSEEN, a public project whose access list does not name the limited member: an open viewer's public arm must not pass a limited member's credential", c.name)
		}
	}
	if wantCompared := (2 + 6*len(lists)) * len(projects); compared != wantCompared {
		t.Fatalf("compared %d answers, want %d: not every combination was read", compared, wantCompared)
	}

	if _, err := database.Pool.Exec(ctx, `update projects set visibility = 'public'`); err != nil {
		t.Fatalf("make every project public: %v", err)
	}
	// With no private project a person sees what it saw and each public project a full member
	// sees; no list decides anything.
	publicSees := map[string][]string{
		full:       projects,
		restricted: {"PRIVGROUP", "PRIVSEEN", "PUBSEEN"},
		newbie:     projects,
		"":         projects,
	}
	for _, c := range callers {
		viewer, err := Resolve(ctx, database.Pool, c.actor)
		if err != nil {
			t.Fatalf("with no private project, %s: Resolve: %v", c.name, err)
		}
		if !slices.Equal(viewer.Sees, publicSees[c.person]) {
			t.Errorf("with no private project, %s sees %v, want what its person sees, %v: no list decides anything (R9)", c.name, viewer.Sees, publicSees[c.person])
		}
		cred := Credential{Listed: c.listed, Person: c.person != "", Browser: c.browser, PersonSees: publicSees[c.person]}
		for _, project := range projects {
			if got, w := cred.Sees(project, false), slices.Contains(publicSees[c.person], project); got != w {
				t.Errorf("with no private project, %s, %s: Credential.Sees = %t, want %t", c.name, project, got, w)
			}
		}
		// Lists: every credential's list but a broker token's, which is answered only while some
		// project is private or its person is a limited member, as before projects could be
		// private (R9).
		var wantLists []string
		if !c.browser && len(c.listed) > 0 && (!c.broker || c.restricted) {
			wantLists = c.listed
		}
		if !slices.Equal(viewer.Lists, wantLists) {
			t.Errorf("with no private project, %s resolves Lists %v, want %v", c.name, viewer.Lists, wantLists)
		}
	}
}
