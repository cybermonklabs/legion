package enroll

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// The people and projects of the enrollment's project tests: PRIV is private, owned by carol,
// with bob on its access list; PUB is public; bob is a restricted member, alice and carol full.
const (
	carolLogin = "carol@example.com"
	bobLogin   = "bob@example.com"
	aliceLogin = "alice@example.com"
)

// withProjects gives svc a client of a Dispatch stand-in holding PRIV and PUB, and answers the
// stand-in so a test can count its calls or take it down.
func withProjects(t *testing.T, svc *Service) *projectaccesstest.Server {
	t.Helper()
	dispatch := projectaccesstest.NewServer(t,
		map[string]projectaccesstest.Row{
			"PRIV": {Visibility: "private", Owner: carolLogin, Listed: []string{bobLogin}},
			"PUB":  {Visibility: "public", Owner: carolLogin},
		},
		map[string]string{bobLogin: projectaccess.MemberRestricted, aliceLogin: projectaccess.MemberFull, carolLogin: projectaccess.MemberFull})
	svc.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
	return dispatch
}

// enrolSession enrols a session of person under a credential of their own, naming projects.
func enrolSession(t *testing.T, svc *Service, person, runtimeID string, projects ...string) (Enrollment, error) {
	t.Helper()
	cred := mintCredential(t, svc, str(person), nil, "devbox")
	return svc.Create(context.Background(), cred, Enrollment{Kind: "session", RuntimeID: runtimeID, Thumbprint: "tp-" + runtimeID, Projects: projects})
}

// TestASessionEnrollsWithProjectsItsPersonSees: a session whose person may see every project it
// names enrolls with that list, held sorted whatever order it was named in, and the enrollment's
// own read answers it. A restricted member's session enrolls nothing, whatever it names
// (TestARestrictedMembersSessionEnrollsNothingNamingProjectsOrNot).
func TestASessionEnrollsWithProjectsItsPersonSees(t *testing.T) {
	svc := newService(t)
	withProjects(t, svc)
	for _, c := range []struct {
		person   string
		projects []string
	}{
		{carolLogin, []string{"PUB", "PRIV"}},
		{aliceLogin, []string{"PUB"}},
	} {
		enr, err := enrolSession(t, svc, c.person, "box-"+c.person, c.projects...)
		if err != nil {
			t.Fatalf("%s's session naming %v: %v", c.person, c.projects, err)
		}
		want := slices.Sorted(slices.Values(c.projects))
		if !slices.Equal(enr.Projects, want) {
			t.Errorf("%s's enrollment names %v, want %v", c.person, enr.Projects, want)
		}
		read, err := svc.Get(context.Background(), enr.ID.String())
		if err != nil || !slices.Equal(read.Projects, want) {
			t.Errorf("Get(%s's enrollment) = %v, %v; want the projects %v", c.person, read.Projects, err, want)
		}
	}
}

// TestASessionNamingAProjectItsPersonCannotSeeIsRefused: a project its person may not see, and a
// project Dispatch does not hold, each refuse the whole enrollment with the same refusal, naming
// the project the session asked for and saying nothing of whether it exists; nothing is enrolled.
func TestASessionNamingAProjectItsPersonCannotSeeIsRefused(t *testing.T) {
	svc := newService(t)
	withProjects(t, svc)
	unseen, err := enrolSession(t, svc, aliceLogin, "box-alice", "PRIV", "PUB")
	if !errors.Is(err, ErrProjectNotVisible) || !strings.Contains(err.Error(), "PRIV") {
		t.Fatalf("alice's session naming PRIV and PUB = %+v, %v; want ErrProjectNotVisible naming PRIV", unseen, err)
	}
	_, missing := enrolSession(t, svc, aliceLogin, "box-alice-2", "NOPE")
	if !errors.Is(missing, ErrProjectNotVisible) || missing.Error() != strings.ReplaceAll(err.Error(), "PRIV", "NOPE") {
		t.Fatalf("alice's session naming NOPE = %v; want the refusal %q with NOPE in its place", missing, err)
	}
	var count int
	if err := svc.Store.Pool.QueryRow(context.Background(), `select count(*) from enrollments where runtime_id like 'box-alice%'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("refused enrollments wrote %d rows (%v); want none", count, err)
	}
}

// TestARestrictedMembersSignInSessionMustNameItsProjects: a session under a restricted member's
// sign-in, which checkMember does not hold (the broker checked the sign-in's domain itself), is
// held to #4135's list rule: naming no project it is refused ErrProjectsRequired, since such a
// person sees only the projects that name them, and naming PRIV, whose list names bob, it enrolls
// with that list. While Dispatch refuses the broker's own account, or answers a member kind the
// broker does not know, the empty list is refused ErrProjectsUnchecked, wrapping the cause. A full
// member's session naming none enrolls as before. (A restricted member's machine login and pod
// login enroll nothing at all: TestARestrictedMembersLauncherEnrollsNothing.)
func TestARestrictedMembersSignInSessionMustNameItsProjects(t *testing.T) {
	ctx := context.Background()
	enrolUnder := func(t *testing.T, svc *Service, cred Credential, runtimeID string, projects ...string) (Enrollment, error) {
		t.Helper()
		return svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: runtimeID, Thumbprint: "tp-" + runtimeID, Projects: projects})
	}
	svc := newService(t)
	withProjects(t, svc)
	signIn := personsSignIn(t, svc, bobLogin)
	if _, err := enrolUnder(t, svc, signIn, "box-bob-signin-none"); !errors.Is(err, ErrProjectsRequired) {
		t.Fatalf("a session under bob's sign-in naming no project = %v; want ErrProjectsRequired", err)
	}
	enr, err := enrolUnder(t, svc, signIn, "box-bob-signin-priv", "PRIV")
	if err != nil || !slices.Equal(enr.Projects, []string{"PRIV"}) {
		t.Fatalf("a session under bob's sign-in naming PRIV = projects %v, %v; want it enrolled naming [PRIV]", enr.Projects, err)
	}
	if enr, err := enrolSession(t, svc, aliceLogin, "box-alice-none"); err != nil || enr.Projects != nil {
		t.Fatalf("alice's session naming no project = %+v, %v; want it enrolled naming none", enr, err)
	}

	for _, c := range []struct {
		name  string
		serve func(t *testing.T, svc *Service)
		cause error
	}{
		{"Dispatch refuses the broker's account", func(t *testing.T, svc *Service) {
			dispatch := withProjects(t, svc)
			svc.Projects = projectaccess.New(dispatch.URL, projectaccesstest.StaticAccount("not-the-stand-ins-token"), nil)
		}, dispatchread.ErrBrokerRefused},
		{"Dispatch answers a member kind the broker does not know", func(t *testing.T, svc *Service) {
			dispatch := projectaccesstest.NewServer(t, nil, map[string]string{bobLogin: "guest"})
			svc.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
		}, projectaccess.ErrUnknownMemberKind},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := newService(t)
			c.serve(t, svc)
			signIn := personsSignIn(t, svc, bobLogin)
			if _, err := enrolUnder(t, svc, signIn, "box-bob-signin-unchecked"); !errors.Is(err, ErrProjectsUnchecked) || !errors.Is(err, c.cause) {
				t.Fatalf("a session under bob's sign-in naming no project = %v; want ErrProjectsUnchecked wrapping %v", err, c.cause)
			}
		})
	}
}

// TestAnEmptyListEnrollsWhenK3IsDownOrUnset: a broker with no client of Dispatch's project
// access enrols a session naming no project exactly as before, a restricted member's included, and
// refuses one naming a project, which could not be checked (ErrProjectsUnchecked). With a client,
// a person's machine login enrolls nothing while Dispatch cannot say what kind of member the
// person is (*projectaccess.MemberKindUncheckable, checkMember): Dispatch down, Dispatch refusing
// the broker's own account (a deployment that has not listed the broker), a member kind the broker
// does not know, or a token file it cannot read. A person's sign-in, which checkMember does not
// hold, enrolls a session naming no project while Dispatch is down, unchecked, and is refused one
// naming a project (ErrProjectsUnchecked).
func TestAnEmptyListEnrollsWhenK3IsDownOrUnset(t *testing.T) {
	uncheckable := func(t *testing.T, err error, cause error, what string) {
		t.Helper()
		var refused *projectaccess.MemberKindUncheckable
		if !errors.As(err, &refused) || cause != nil && !errors.Is(err, cause) {
			t.Fatalf("%s = %v; want *projectaccess.MemberKindUncheckable (%v)", what, err, cause)
		}
	}
	t.Run("no client", func(t *testing.T) {
		svc := newService(t)
		if _, err := enrolSession(t, svc, bobLogin, "box-bob-unset"); err != nil {
			t.Fatalf("a session naming no project on a broker with no client: %v", err)
		}
		if _, err := enrolSession(t, svc, carolLogin, "box-carol-unset", "PRIV"); !errors.Is(err, ErrProjectsUnchecked) {
			t.Fatalf("a session naming PRIV on a broker with no client = %v; want ErrProjectsUnchecked", err)
		}
	})
	t.Run("Dispatch down", func(t *testing.T) {
		svc := newService(t)
		dispatch := withProjects(t, svc)
		dispatch.SetDown(true)
		_, err := enrolSession(t, svc, bobLogin, "box-bob-down")
		uncheckable(t, err, nil, "a restricted member's machine-login session naming no project while Dispatch is down")
		signIn := personsSignIn(t, svc, carolLogin)
		if _, err := svc.Create(context.Background(), signIn, Enrollment{Kind: "session", RuntimeID: "box-carol-signin-down", Thumbprint: "tp-box-carol-signin-down"}); err != nil {
			t.Fatalf("a sign-in's session naming no project while Dispatch is down: %v; want it enrolled as before", err)
		}
		if _, err := svc.Create(context.Background(), signIn, Enrollment{Kind: "session", RuntimeID: "box-carol-signin-priv", Thumbprint: "tp-box-carol-signin-priv", Projects: []string{"PRIV"}}); !errors.Is(err, ErrProjectsUnchecked) {
			t.Fatalf("a sign-in's session naming PRIV while Dispatch is down = %v; want ErrProjectsUnchecked", err)
		}
	})
	t.Run("Dispatch refuses the broker's account", func(t *testing.T) {
		svc := newService(t)
		dispatch := withProjects(t, svc)
		svc.Projects = projectaccess.New(dispatch.URL, projectaccesstest.StaticAccount("not-the-stand-ins-token"), nil)
		_, err := enrolSession(t, svc, aliceLogin, "box-alice-refused")
		uncheckable(t, err, dispatchread.ErrBrokerRefused, "a session naming no project while Dispatch refuses the broker's account")
	})
	// Base #4119's cfe6a5f216 adds two answers that are neither an outage nor a refusal of the
	// list: a member kind the broker does not know (projectaccess.ErrUnknownMemberKind), and a
	// token file the broker cannot read, which ErrBrokerRefused now covers. The exchange mints
	// through neither, and no session enrols through them either, naming a project or not.
	t.Run("Dispatch answers a member kind the broker does not know", func(t *testing.T) {
		svc := newService(t)
		dispatch := projectaccesstest.NewServer(t, nil, map[string]string{aliceLogin: "guest"})
		svc.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
		_, err := enrolSession(t, svc, aliceLogin, "box-alice-guest")
		uncheckable(t, err, projectaccess.ErrUnknownMemberKind, "a session naming no project while Dispatch answers an unknown member kind")
	})
	t.Run("the broker cannot read its token file", func(t *testing.T) {
		svc := newService(t)
		dispatch := withProjects(t, svc)
		svc.Projects = projectaccess.New(dispatch.URL, ciphertoken.NewAccountToken(filepath.Join(t.TempDir(), "missing-token")), nil)
		_, err := enrolSession(t, svc, aliceLogin, "box-alice-no-token")
		uncheckable(t, err, dispatchread.ErrBrokerRefused, "a session naming no project while the broker cannot read its token file")
		if calls := dispatch.Calls(); calls != 0 {
			t.Errorf("Dispatch was asked %d times; want none, since there is no token to ask with", calls)
		}
	})
}

// TestAPodNamesNoProjectOfItsOwn: a service's pod's project is the one its operator's entry maps
// its account to, never one it names, so a service's pod enrollment naming projects is refused
// before Dispatch is asked; so is a list that is no list (lower case, a key twice, more than
// sixteen). A person's pod names its projects as its person's session does
// (person_pod_test.go).
func TestAPodNamesNoProjectOfItsOwn(t *testing.T) {
	svc := newService(t)
	dispatch := withProjects(t, svc)
	issuer, key := withPodVerifier(t, svc)
	cred := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
	_, err := svc.Create(context.Background(), cred, Enrollment{
		Kind: "pod", RuntimeID: "pod-uid-p", Thumbprint: "tp-pod-p", Projects: []string{"PUB"},
		PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-uid-p"),
	})
	if !errors.Is(err, ErrProjectsNeedAPerson) {
		t.Fatalf("a pod naming PUB = %v; want ErrProjectsNeedAPerson", err)
	}
	seventeen := make([]string, 17)
	for i := range seventeen {
		seventeen[i] = "P" + string(rune('A'+i)) + "X"
	}
	for _, projects := range [][]string{{"priv"}, {"PRIV", "PRIV"}, seventeen} {
		if _, err := enrolSession(t, svc, carolLogin, "box-carol-"+strings.ToLower(projects[0]), projects...); !errors.Is(err, ErrProjectsInvalid) {
			t.Errorf("a session naming %v = %v; want ErrProjectsInvalid", projects, err)
		}
	}
	if calls := dispatch.Calls(); calls != 0 {
		t.Fatalf("Dispatch was asked %d times; want none, since each refusal comes before the question", calls)
	}
}
