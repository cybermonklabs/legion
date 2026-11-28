package exchange_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
)

// TestARestrictedMembersMachineLoginIsNamedToItsOneProject: a machine login whose approver
// Dispatch calls a restricted member is decided named, to exactly the one project its trade asked
// for, and its token lives at most RestrictedLifetime, 270 seconds; one asking for no project or
// for two is refused, the refusal naming the rule, the person and how to name the one project.
//
// Control: Machine deciding the person scope for every approver; the first row fails naming the
// scope.
func TestARestrictedMembersMachineLoginIsNamedToItsOneProject(t *testing.T) {
	set := listed(t)
	if exchange.RestrictedLifetime != 270*time.Second {
		t.Fatalf("RestrictedLifetime = %s, want 4m30s: 300 s less the broker's 30 s cache of Dispatch's answer, the bound on how long a person taken off a project's access list keeps a token for it", exchange.RestrictedLifetime)
	}
	claims, err := set.Machine(credentialID, person, "ada-devbox", []string{project}, exchange.ApproverRestricted, nil)
	if err != nil {
		t.Fatalf("Machine(a restricted member, [%s]): %v", project, err)
	}
	want := exchange.Claims{Session: "ada-devbox", Person: person, Scope: exchange.ProjectScopeNamed, Projects: []string{project},
		Principal: personPrincipal, Lifetime: exchange.RestrictedLifetime}
	if !reflect.DeepEqual(claims, want) {
		t.Fatalf("Machine(a restricted member, [%s]) = %+v, want %+v", project, claims, want)
	}
	for _, asked := range [][]string{nil, {project, "OTHER"}} {
		claims, err := set.Machine(credentialID, person, "ada-devbox", asked, exchange.ApproverRestricted, nil)
		if !errors.Is(err, exchange.ErrRestrictedOneProject) {
			t.Fatalf("Machine(a restricted member, %v) = %+v, %v; want ErrRestrictedOneProject", asked, claims, err)
		}
		for _, names := range []string{"exactly one project", person, "AGENT_SECRETS_PROJECTS"} {
			if !strings.Contains(err.Error(), names) {
				t.Errorf("Machine(a restricted member, %v) refused with %q, which does not name %q", asked, err, names)
			}
		}
	}
}

// TestAMachineLoginNotRestrictedActsAsItsPerson is the restricted rows' control: a machine login
// Dispatch calls no restricted member's is decided as it always was, the person scope with the
// list it asked for and the broker's own lifetime. One whose approver's kind nobody decided is
// refused, never read as either.
func TestAMachineLoginNotRestrictedActsAsItsPerson(t *testing.T) {
	set := listed(t)
	for _, asked := range [][]string{nil, {project}, {"OTHER", project}} {
		claims, err := set.Machine(credentialID, person, "ada-devbox", asked, exchange.ApproverUnrestricted, nil)
		if err != nil {
			t.Fatalf("Machine(a member not restricted, %v): %v", asked, err)
		}
		if claims.Scope != exchange.ProjectScopePerson || !reflect.DeepEqual(claims.Projects, asked) || claims.Lifetime != 0 {
			t.Fatalf("Machine(a member not restricted, %v) = %+v, want the person scope, the asked list and no lifetime of its own", asked, claims)
		}
	}
	claims, err := set.Machine(credentialID, person, "ada-devbox", []string{project}, exchange.ApproverUnknown, nil)
	if !errors.Is(err, exchange.ErrApproverKindUnknown) {
		t.Fatalf("Machine(an approver of no kind) = %+v, %v; want ErrApproverKindUnknown", claims, err)
	}
}

// TestARestrictedMembersDispatchSignInIsGivenNoToken: a Dispatch sign-in's claims act in every
// project its person may reach (the consented scope is read under the person scope), which a
// restricted member's token never does, so Machine refuses a restricted member's sign-in
// ErrRestrictedSignIn, whatever projects it asks for and whatever scopes its person approved, the
// empty list included, and the refusal names the person and the browser instead. Control: a member
// who is not restricted is given the consented claims, carrying the approved scopes beside the
// person scope.
//
// The named revert: drop the refusal, and the restricted member's sign-in is held to one project
// in a scope the consented token cannot carry, or minted as a person's.
func TestARestrictedMembersDispatchSignInIsGivenNoToken(t *testing.T) {
	set := listed(t)
	for _, approved := range [][]string{{"person"}, {}} {
		for _, asked := range [][]string{nil, {project}} {
			claims, err := set.Machine(credentialID, person, "ada-devbox", asked, exchange.ApproverRestricted, &approved)
			if !errors.Is(err, exchange.ErrRestrictedSignIn) {
				t.Fatalf("Machine(a restricted member's sign-in approving %v, asking %v) = %+v, %v; want ErrRestrictedSignIn", approved, asked, claims, err)
			}
			for _, names := range []string{person, "restricted member", "Dispatch sign-in", "browser"} {
				if !strings.Contains(err.Error(), names) {
					t.Errorf("the refusal %q does not name %q", err, names)
				}
			}
		}
	}
	approved := []string{"person"}
	claims, err := set.Machine(credentialID, person, "ada-devbox", []string{project}, exchange.ApproverUnrestricted, &approved)
	if err != nil || claims.Scope != exchange.ProjectScopePerson || !reflect.DeepEqual(claims.Consented, approved) {
		t.Fatalf("control: Machine(a full member's sign-in) = %+v, %v; want the person scope consenting %v", claims, err, approved)
	}
}
